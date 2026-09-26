// Package universal maps arbitrary JSON webhooks onto PushWard content. A
// payload is flattened into candidate fields, the field set is fingerprinted
// so a mapping is found again for the next event of the same shape, and a
// deterministic heuristic proposes the first mapping for a shape it has not
// seen.
package universal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"unicode/utf8"
)

// Flatten caps. The relay pod runs with a 64 Mi limit, so a hostile or merely
// huge payload must cost bounded memory: nothing below is proportional to the
// body beyond one token at a time.
const (
	MaxDepth      = 8
	MaxPaths      = 256
	MaxValueRunes = 256
)

// ValueType is the JSON type of a flattened leaf.
type ValueType string

const (
	TypeString ValueType = "string"
	TypeNumber ValueType = "number"
	TypeBool   ValueType = "bool"
	TypeNull   ValueType = "null"
	// TypeEmpty is an empty array. It is kept as a leaf so a list that is
	// sometimes empty does not change the payload's fingerprint.
	TypeEmpty ValueType = "empty"
)

// Field is one leaf of a flattened payload: its normalized path, a sample
// value capped at MaxValueRunes, and its JSON type.
type Field struct {
	Path  string    `json:"path"`
	Value string    `json:"value"`
	Type  ValueType `json:"type"`
}

var errNotContainer = errors.New("universal: payload is not a JSON object or array")

// Flatten walks a JSON document token by token and returns its leaves. Arrays
// contribute only their first element, under "key[]"; keys that look like
// generated ids (uuids, long hex, numbers, dates) become "*", and only the
// first such sibling is walked, so a map keyed by ids flattens like an array.
// Subtrees deeper than MaxDepth are skipped. Once MaxPaths leaves are
// collected the walk stops and truncated is true.
func Flatten(r io.Reader) (fields []Field, truncated bool, err error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, false, fmt.Errorf("universal: decode payload: %w", err)
	}
	f := &flattener{dec: dec, seen: make(map[string]struct{})}
	switch tok {
	case json.Delim('{'):
		err = f.object("", 1)
	case json.Delim('['):
		err = f.array("", 1)
	default:
		return nil, false, errNotContainer
	}
	if errors.Is(err, errFull) {
		return f.fields, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("universal: decode payload: %w", err)
	}
	return f.fields, false, nil
}

// errFull stops the walk once MaxPaths leaves are collected. The rest of the
// body is never read.
var errFull = errors.New("path cap reached")

type flattener struct {
	dec    *json.Decoder
	fields []Field
	seen   map[string]struct{}
}

// value reads the next value, whose path is already known.
func (f *flattener) value(path string, depth int) error {
	tok, err := f.dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case json.Delim:
		if v == '{' {
			return f.object(path, depth+1)
		}
		return f.array(path, depth+1)
	case string:
		return f.add(path, v, TypeString)
	case json.Number:
		return f.add(path, v.String(), TypeNumber)
	case bool:
		if v {
			return f.add(path, "true", TypeBool)
		}
		return f.add(path, "false", TypeBool)
	default:
		return f.add(path, "", TypeNull)
	}
}

// object walks the members of an object whose '{' was just read.
func (f *flattener) object(path string, depth int) error {
	walkedStar := false
	for f.dec.More() {
		tok, err := f.dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		norm := NormalizeKey(key)
		skip := depth > MaxDepth
		if norm == "*" {
			skip = skip || walkedStar
			walkedStar = true
		}
		if skip {
			if err := f.skip(); err != nil {
				return err
			}
			continue
		}
		child := norm
		if path != "" {
			child = path + "." + norm
		}
		if err := f.value(child, depth); err != nil {
			return err
		}
	}
	_, err := f.dec.Token() // '}'
	return err
}

// array walks the first element of an array whose '[' was just read and skips
// the rest. An empty array is recorded as a leaf.
func (f *flattener) array(path string, depth int) error {
	elem := path + "[]"
	first := true
	for f.dec.More() {
		if first && depth <= MaxDepth {
			if err := f.value(elem, depth); err != nil {
				return err
			}
		} else if err := f.skip(); err != nil {
			return err
		}
		first = false
	}
	if first && depth <= MaxDepth {
		if err := f.add(elem, "", TypeEmpty); err != nil {
			return err
		}
	}
	_, err := f.dec.Token() // ']'
	return err
}

// skip consumes one value without recording it.
func (f *flattener) skip() error {
	nest := 0
	for {
		tok, err := f.dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				nest++
			default:
				nest--
			}
		}
		if nest == 0 {
			return nil
		}
	}
}

func (f *flattener) add(path, value string, typ ValueType) error {
	if _, dup := f.seen[path]; dup {
		return nil
	}
	if len(f.fields) >= MaxPaths {
		return errFull
	}
	f.seen[path] = struct{}{}
	f.fields = append(f.fields, Field{Path: path, Value: capRunes(value, MaxValueRunes), Type: typ})
	return nil
}

func capRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

var (
	uuidKey = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	numKey  = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
	hexKey  = regexp.MustCompile(`^[0-9a-fA-F]{8,}$`)
	dateKey = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}([T ][0-9:.]+(Z|[+-][0-9]{2}:?[0-9]{2})?)?$`)
	digit   = regexp.MustCompile(`[0-9]`)
)

// NormalizeKey returns "*" for keys that are generated identifiers rather
// than schema: uuids, pure numbers, dates, and hex strings of 8 or more
// characters that contain a digit (so a plain word like "deadbeef" survives).
func NormalizeKey(key string) string {
	if !utf8.ValidString(key) {
		return "*"
	}
	switch {
	case uuidKey.MatchString(key), numKey.MatchString(key), dateKey.MatchString(key):
		return "*"
	case hexKey.MatchString(key) && digit.MatchString(key):
		return "*"
	}
	return key
}
