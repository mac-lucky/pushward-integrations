package ranker

import (
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"strconv"
	"strings"

	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
)

// FeaturesVersion names the feature set below. Weights trained on another
// version do not load; bump it with any change to a feature's name or value.
const FeaturesVersion = 1

// TopK is how many of the heuristic's candidates per role are offered to the
// models, before none.
const TopK = 8

// Feature is one named model input. Most are indicators with value 1.
type Feature struct {
	Name  string
	Value float64
}

// Features is a feature list in the order the models sum it. It marshals as a
// JSON object in that order.
type Features []Feature

func (fs Features) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 24*len(fs)+2)
	b = append(b, '{')
	for i, f := range fs {
		if !finite(f.Value) {
			return nil, fmt.Errorf("feature %q is %v", f.Name, f.Value)
		}
		name, err := json.Marshal(f.Name)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, name...)
		b = append(b, ':')
		b = strconv.AppendFloat(b, f.Value, 'g', -1, 64)
	}
	return append(b, '}'), nil
}

// Option is one answer offered for a role: a field, or none when Path is
// empty.
type Option struct {
	Path     string   `json:"path"`
	Features Features `json:"f"`
}

// Extraction is everything the models read from one payload. It is computed
// from shapes alone, so a stored shape gets the features its payload did.
type Extraction struct {
	// Proposal is the heuristic's own proposal.
	Proposal universal.Proposal
	// Ranked holds RankShapes for every role, and Options the models'
	// choices built from its first TopK entries plus none, both in Roles
	// order.
	Ranked  [][]universal.Candidate
	Options [][]Option
	// Kind feeds the kind model.
	Kind Features
}

// Extract runs the heuristic over shapes and builds the models' inputs.
//
// A field option carries:
//   - its shape: t:<type>, c:<class>, n:<runes> and w:<words> as log2
//     buckets, f:<flag> per flag, r:<range>, vx and vl when the sample is a
//     vocabulary word or ends in one;
//   - where it sits: depth, order (index over field count), in_array;
//   - the words of its key (k:), of every parent (p:) and of the nearest
//     parent (lp:);
//   - the heuristic's view: h:score, h:gap (to the best candidate) and
//     h:rank0 to h:rank3 (3 and below share one);
//   - claimed:<role> for every other role the heuristic gave this field.
//
// The none option carries none, none:best (the best candidate's score, -5
// without one) and none:n (log1p of the options offered). The kind features
// are the heuristic's kind (hk:), has:<role> per role it filled, and the
// states its value tables map to (severity_values:<state>,
// lifecycle_values:<state>).
func Extract(shapes []universal.ShapeField) *Extraction {
	p, ranked := universal.ProposeRanked(shapes)
	ex := &Extraction{Proposal: p, Ranked: ranked, Options: make([][]Option, len(universal.Roles))}
	index := make(map[string]int, len(shapes))
	for i := range shapes {
		if _, dup := index[shapes[i].Path]; !dup {
			index[shapes[i].Path] = i
		}
	}
	static := make([]Features, len(shapes))
	for ri, r := range universal.Roles {
		cands := ranked[ri][:min(len(ranked[ri]), TopK)]
		opts := make([]Option, 0, len(cands)+1)
		best := -5.0
		if len(cands) > 0 {
			best = cands[0].Score
		}
		for rank, c := range cands {
			i := index[c.Path]
			if static[i] == nil {
				static[i] = fieldFeatures(&shapes[i], i, len(shapes))
			}
			fs := make(Features, len(static[i]), len(static[i])+6)
			copy(fs, static[i])
			fs = append(fs,
				Feature{"h:score", c.Score},
				Feature{"h:gap", best - c.Score},
				Feature{rankNames[min(rank, len(rankNames)-1)], 1},
			)
			for _, other := range universal.Roles {
				if other != r && p.Path(other) == c.Path {
					fs = append(fs, Feature{"claimed:" + string(other), 1})
				}
			}
			opts = append(opts, Option{Path: c.Path, Features: fs})
		}
		opts = append(opts, Option{Features: Features{
			{"none", 1},
			{"none:best", best},
			{"none:n", math.Log1p(float64(len(cands)))},
		}})
		ex.Options[ri] = opts
	}
	ex.Kind = kindFeatures(&p)
	return ex
}

var rankNames = [...]string{"h:rank0", "h:rank1", "h:rank2", "h:rank3"}

// flagFeatures names each flag here rather than through Flags.MarshalJSON,
// so a renamed flag cannot silently rename a feature.
var flagFeatures = [...]struct {
	flag universal.Flags
	name string
}{
	{universal.FlagCap, "f:cap"},
	{universal.FlagEnum, "f:enum"},
	{universal.FlagID, "f:id"},
	{universal.FlagIDToken, "f:idtok"},
	{universal.FlagInt, "f:int"},
	{universal.FlagLetters, "f:letters"},
	{universal.FlagLower, "f:lower"},
	{universal.FlagNum, "f:num"},
	{universal.FlagTime, "f:time"},
	{universal.FlagUpper, "f:upper"},
	{universal.FlagURL, "f:url"},
}

// fieldFeatures are the features of a field that do not depend on the role.
func fieldFeatures(s *universal.ShapeField, i, n int) Features {
	fs := make(Features, 0, 24)
	fs = append(fs,
		Feature{"t:" + string(s.Type), 1},
		Feature{"c:" + string(s.Class), 1},
		Feature{"n:" + strconv.Itoa(bits.Len(uint(max(s.Runes, 0)))), 1},
		// Words are not capped the way runes are; 32 and up share a bucket.
		Feature{"w:" + strconv.Itoa(bits.Len(uint(min(max(s.Words, 0), 32)))), 1},
	)
	for _, f := range flagFeatures {
		if s.Flags.Has(f.flag) {
			fs = append(fs, Feature{f.name, 1})
		}
	}
	if s.Range != "" {
		fs = append(fs, Feature{"r:" + s.Range, 1})
	}
	if s.Vocab != "" {
		fs = append(fs, Feature{"vx", 1})
	}
	if s.LastWord != "" {
		fs = append(fs, Feature{"vl", 1})
	}
	fs = append(fs,
		Feature{"depth", float64(len(universal.Segments(s.Path)))},
		Feature{"order", float64(i) / float64(max(n, 1))},
	)
	if strings.Contains(s.Path, "[]") {
		fs = append(fs, Feature{"in_array", 1})
	}
	t := universal.TokensOf(s.Path)
	fs = appendWords(fs, "k:", t.Key)
	fs = appendWords(fs, "p:", t.Parents)
	fs = appendWords(fs, "lp:", t.LastParent)
	return fs
}

// appendWords adds prefix+word once per distinct word: "data.data.id" has
// the parent word data once.
func appendWords(fs Features, prefix string, words []string) Features {
	for i, w := range words {
		if !slices.Contains(words[:i], w) {
			fs = append(fs, Feature{prefix + w, 1})
		}
	}
	return fs
}

func kindFeatures(p *universal.Proposal) Features {
	fs := Features{{"hk:" + string(p.Kind), 1}}
	for _, r := range universal.Roles {
		if p.Path(r) != "" {
			fs = append(fs, Feature{"has:" + string(r), 1})
		}
	}
	fs = appendStates(fs, "severity_values:", p.SeverityValues)
	return appendStates(fs, "lifecycle_values:", p.LifecycleValues)
}

// appendStates adds one feature per distinct state a value table maps to,
// in sorted order.
func appendStates(fs Features, prefix string, table map[string]string) Features {
	states := make([]string, 0, len(table))
	for _, s := range table {
		if !slices.Contains(states, s) {
			states = append(states, s)
		}
	}
	slices.Sort(states)
	for _, s := range states {
		fs = append(fs, Feature{prefix + s, 1})
	}
	return fs
}
