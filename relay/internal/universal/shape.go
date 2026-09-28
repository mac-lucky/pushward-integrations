package universal

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ShapeVersion is the version of the stored ShapeField and Shape layout.
const ShapeVersion = 1

// ShapeField is a field without its value: everything the heuristic reads
// from a sample, and nothing else. The only value text it keeps is a word
// from the relay's own vocabulary (Vocab or LastWord), so a shape can be
// stored and shown to a model without the payload.
type ShapeField struct {
	Path  string     `json:"p"`
	Type  ValueType  `json:"t"`
	Class ValueClass `json:"c"`
	// Runes and Words count the trimmed sample, capped with it at
	// MaxValueRunes.
	Runes int   `json:"n"`
	Words int   `json:"w"`
	Flags Flags `json:"f,omitempty"`
	// Range buckets a number sample; empty for anything else.
	Range string `json:"r,omitempty"`
	// Vocab is the normalized sample when it is a word the heuristic knows
	// ("critical", "in_progress"). Otherwise LastWord is its last word when
	// that one is ("alert.resolved" gives "resolved", see valueTail).
	Vocab    string `json:"vx,omitempty"`
	LastWord string `json:"vl,omitempty"`
}

// Flags are the facts about a sample the heuristic tests.
type Flags uint16

// The bits are in name order, so they marshal as a sorted list.
const (
	FlagCap     Flags = 1 << iota // first rune upper case, more than one word
	FlagEnum                      // a short code: "firing", "HIGH", "sev1"
	FlagID                        // uuid, hex with a digit, or an integer
	FlagIDToken                   // matches idToken, see scoreCorrelation
	FlagInt                       // a number written without a fraction
	FlagLetters                   // contains a letter
	FlagLower                     // enum with every letter lower case
	FlagNum                       // parses as a finite float
	FlagTime                      // an ISO 8601 date or timestamp
	FlagUpper                     // enum with every letter upper case
	FlagURL                       // http or https URL
)

var flagNames = [...]string{"cap", "enum", "id", "idtok", "int", "letters", "lower", "num", "time", "upper", "url"}

// Has reports whether every flag in g is set.
func (f Flags) Has(g Flags) bool { return f&g == g }

func (f Flags) MarshalJSON() ([]byte, error) {
	b := []byte{'['}
	for i, name := range flagNames {
		if f&(1<<i) == 0 {
			continue
		}
		if len(b) > 1 {
			b = append(b, ',')
		}
		b = strconv.AppendQuote(b, name)
	}
	return append(b, ']'), nil
}

// UnmarshalJSON ignores names it does not know, so a shape stored by a newer
// relay still loads.
func (f *Flags) UnmarshalJSON(b []byte) error {
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return err
	}
	*f = 0
	for _, n := range names {
		for i, name := range flagNames {
			if n == name {
				*f |= 1 << i
			}
		}
	}
	return nil
}

// Range buckets. The edges are the ones the heuristic and the progress scale
// compare against: [0,1] for a ratio, [0,10] for a severity level, [0,100]
// for a percentage.
const (
	RangeNegBig = "neg_big" // < -1e6
	RangeNeg    = "neg"     // [-1e6, 0)
	Range0To1   = "0_1"     // [0, 1]
	Range1To10  = "1_10"    // (1, 10]
	Range10To1h = "10_100"  // (10, 100]
	Range1hTo1M = "100_1e6" // (100, 1e6]
	RangeBig    = "big"     // > 1e6
)

func rangeOf(n float64) string {
	switch {
	case n < -1e6:
		return RangeNegBig
	case n < 0:
		return RangeNeg
	case n <= 1:
		return Range0To1
	case n <= 10:
		return Range1To10
	case n <= 100:
		return Range10To1h
	case n <= 1e6:
		return Range1hTo1M
	}
	return RangeBig
}

// vocab is every value word the heuristic looks up. A sample is kept in a
// shape only when it is one of these.
var vocab = func() map[string]bool {
	v := map[string]bool{}
	for w := range severityValues {
		v[w] = true
	}
	for w := range lifecycleValues {
		v[w] = true
	}
	for _, set := range []map[string]bool{progressStates, alertStates, progressPathWords, alertPathWords} {
		for w := range set {
			v[w] = true
		}
	}
	return v
}()

// ShapesOf returns the shape of every field, in order.
func ShapesOf(fields []Field) []ShapeField {
	out := make([]ShapeField, len(fields))
	for i := range fields {
		out[i] = shapeOf(&fields[i])
	}
	return out
}

func shapeOf(f *Field) ShapeField {
	v := strings.TrimSpace(f.Value)
	s := ShapeField{
		Path:  f.Path,
		Type:  f.Type,
		Class: ClassOf(f.Path, *f),
		// A link may run to MaxURLRunes; its shape counts as far as any
		// other value's does.
		Runes: min(utf8.RuneCountInString(v), MaxValueRunes),
		Words: countWords(v),
	}
	if strings.IndexFunc(v, unicode.IsLetter) >= 0 {
		s.Flags |= FlagLetters
	}
	switch f.Type {
	case TypeNumber:
		n, err := strconv.ParseFloat(v, 64)
		if err == nil {
			s.Flags |= FlagNum
			s.Range = rangeOf(n)
		}
		if intValue.MatchString(v) {
			s.Flags |= FlagInt
			if err == nil {
				s.Flags |= FlagID
			}
		}
	case TypeString:
		lv := strings.ToLower(v)
		isURL := strings.HasPrefix(lv, "http://") || strings.HasPrefix(lv, "https://")
		isTime := timeValue.MatchString(v)
		if isURL {
			s.Flags |= FlagURL
		}
		if isTime {
			s.Flags |= FlagTime
		}
		if uuidValue.MatchString(v) || (hexValue.MatchString(v) && strings.ContainsAny(v, "0123456789")) || intValue.MatchString(v) {
			s.Flags |= FlagID
		}
		if enumValue.MatchString(v) && !isURL && !isTime {
			s.Flags |= FlagEnum | caseFlag(v)
		}
		if idToken.MatchString(v) {
			s.Flags |= FlagIDToken
		}
		if n, err := strconv.ParseFloat(v, 64); err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) {
			s.Flags |= FlagNum
		}
		if r, _ := utf8.DecodeRuneInString(v); unicode.IsUpper(r) && s.Words > 1 {
			s.Flags |= FlagCap
		}
		// A secret keeps nothing, not even a word: "open" may be a password.
		if s.Class == ClassSecret {
			break
		}
		if n := normValue(v); vocab[n] {
			s.Vocab = n
		} else if w := valueTail(v); vocab[w] {
			s.LastWord = w
		}
	}
	return s
}

// caseFlag tells an all upper case enum ("CRITICAL") from an all lower case
// one; a mixed one ("Critical") gets neither.
func caseFlag(v string) Flags {
	upper, lower := false, false
	for _, r := range v {
		upper = upper || unicode.IsUpper(r)
		lower = lower || unicode.IsLower(r)
	}
	switch {
	case upper && !lower:
		return FlagUpper
	case lower && !upper:
		return FlagLower
	}
	return 0
}

// countWords is len(strings.Fields(s)) without the slice.
func countWords(s string) int {
	n, in := 0, false
	for _, r := range s {
		if unicode.IsSpace(r) {
			in = false
		} else if !in {
			in, n = true, n+1
		}
	}
	return n
}

// PathTokens are the words the heuristic reads from a path: the field's own
// key, every parent key, and the nearest parent on its own. Array markers and
// "*" are dropped.
type PathTokens struct {
	Key        []string `json:"k"`
	Parents    []string `json:"p"`
	LastParent []string `json:"lp"`
}

// TokensOf splits a path the way the heuristic does.
func TokensOf(path string) PathTokens {
	segs := segments(path)
	t := PathTokens{Key: []string{}, Parents: []string{}, LastParent: []string{}}
	if len(segs) == 0 {
		return t
	}
	t.Key = tokens(segs[len(segs)-1])
	for _, p := range segs[:len(segs)-1] {
		t.Parents = append(t.Parents, tokens(p)...)
	}
	if len(segs) > 1 {
		t.LastParent = tokens(segs[len(segs)-2])
	}
	return t
}
