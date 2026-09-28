package universalhook_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"
	"unicode"

	"github.com/mac-lucky/pushward-integrations/relay/internal/relaytest"
)

// Scorecard grades. A role is n/a when the dedicated handler shows nothing
// for it, so there is nothing to match.
const (
	gradeExact   = "exact"
	gradePartial = "partial"
	gradeMiss    = "miss"
	gradeNA      = "n/a"
)

var roles = []string{"kind", "title", "body", "url", "lifecycle", "severity"}

// floors are the lowest credit per role the universal route may score over
// the fixtures: the measured value less 0.05, rounded down to 0.05, so a
// single fixture moving cannot trip one. Credit is exact plus half of
// partial, over the scored fixtures. Raise a floor when the heuristic
// improves; lowering one needs a reason.
var floors = map[string]float64{
	"kind":      0.25,
	"title":     0.35,
	"body":      0.15,
	"url":       0.80,
	"lifecycle": 0.10,
	"severity":  0.30,
}

type scored struct {
	Fixture   string            `json:"fixture"`
	Provider  string            `json:"provider"`
	Route     string            `json:"route"`
	Dedicated relaytest.Outcome `json:"dedicated"`
	Universal relaytest.Outcome `json:"universal"`
	Score     map[string]string `json:"score"`
	dir       string
}

// TestUniversalScorecard answers how well the universal route stands in for
// the dedicated ones: every provider fixture goes through its own route and
// through POST /universal?source=<dir>, and what the user would see of each
// is compared role by role.
func TestUniversalScorecard(t *testing.T) {
	var (
		mu      sync.Mutex
		results []scored
	)
	t.Run("fixtures", func(t *testing.T) {
		for _, rt := range relaytest.Routes() {
			names, err := filepath.Glob(filepath.Join(relaytest.Testdata, rt.Dir, "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range names {
				t.Run(rt.Dir+"/"+filepath.Base(name), func(t *testing.T) {
					t.Parallel()
					ded := relaytest.RunDedicated(t, rt, name)
					uni := relaytest.RunDedicated(t, relaytest.Universal(rt.Dir, rt.Dir), name)
					s := scored{
						Fixture:   ded.Fixture,
						Provider:  rt.Provider,
						Route:     rt.Path,
						Dedicated: relaytest.OutcomeOf(ded.Calls),
						Universal: relaytest.OutcomeOf(uni.Calls),
						dir:       rt.Dir,
					}
					s.Score = grades(s.Dedicated, s.Universal)
					mu.Lock()
					results = append(results, s)
					mu.Unlock()
				})
			}
		}
	})
	if t.Failed() {
		return
	}
	slices.SortFunc(results, func(a, b scored) int { return strings.Compare(a.Fixture, b.Fixture) })

	t.Log("\n" + table(results))
	for _, role := range roles {
		credit, n := creditOf(results, role)
		if n == 0 {
			t.Errorf("%s: no fixture scored", role)
			continue
		}
		if credit < floors[role]-1e-9 {
			t.Errorf("%s: credit %.3f is below its floor %.2f", role, credit, floors[role])
		} else if next := math.Floor((credit-0.05)*20) / 20; next > floors[role]+1e-9 {
			t.Logf("%s: credit %.3f; the floor can go up to %.2f", role, credit, next)
		}
	}

	if out := os.Getenv("UNIVERSAL_SCORECARD_OUT"); out != "" {
		writeJSONL(t, out, results)
	}
}

func grades(ded, uni relaytest.Outcome) map[string]string {
	return map[string]string{
		"kind":      gradeEnum(ded.Kind, uni.Kind),
		"title":     gradeText(ded.Title, uni.Title),
		"body":      gradeText(ded.Body, uni.Body),
		"url":       gradeText(ded.URL, uni.URL),
		"lifecycle": gradeEnum(ded.Lifecycle, uni.Lifecycle),
		"severity":  gradeEnum(ded.Severity, uni.Severity),
	}
}

func gradeEnum(want, got string) string {
	switch {
	case want == "":
		return gradeNA
	case got == want:
		return gradeExact
	}
	return gradeMiss
}

// gradeText is exact on equal text and partial when one contains the other or
// their words overlap by half (Jaccard), ignoring case.
func gradeText(want, got string) string {
	switch {
	case want == "":
		return gradeNA
	case got == want:
		return gradeExact
	case got == "":
		return gradeMiss
	}
	w, g := strings.ToLower(want), strings.ToLower(got)
	if strings.Contains(w, g) || strings.Contains(g, w) || jaccard(words(w), words(g)) >= 0.5 {
		return gradePartial
	}
	return gradeMiss
}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		out[w] = true
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// creditOf is exact plus half of partial over the fixtures scored for role.
func creditOf(results []scored, role string) (float64, int) {
	var credit float64
	n := 0
	for _, r := range results {
		switch r.Score[role] {
		case gradeExact:
			credit++
		case gradePartial:
			credit += 0.5
		case gradeNA:
			continue
		}
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return credit / float64(n), n
}

// table is one row per fixture directory, each role as exact/partial/miss
// counts, then the totals and the credit per role.
func table(results []scored) string {
	var dirs []string
	byDir := map[string][]scored{}
	for _, r := range results {
		if _, ok := byDir[r.dir]; !ok {
			dirs = append(dirs, r.dir)
		}
		byDir[r.dir] = append(byDir[r.dir], r)
	}
	slices.Sort(dirs)

	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "fixtures\tn\t%s\n", strings.Join(roles, "\t"))
	row := func(name string, rs []scored) {
		cells := []string{name, fmt.Sprint(len(rs))}
		for _, role := range roles {
			cells = append(cells, counts(rs, role))
		}
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	for _, d := range dirs {
		row(d, byDir[d])
	}
	row("total", results)
	cells := []string{"credit", ""}
	for _, role := range roles {
		c, _ := creditOf(results, role)
		cells = append(cells, fmt.Sprintf("%.3f", c))
	}
	_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
	_ = tw.Flush()
	return "exact/partial/miss per role, n/a left out\n" + b.String()
}

func counts(rs []scored, role string) string {
	var e, p, m int
	for _, r := range rs {
		switch r.Score[role] {
		case gradeExact:
			e++
		case gradePartial:
			p++
		case gradeMiss:
			m++
		}
	}
	if e+p+m == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d/%d", e, p, m)
}

func writeJSONL(t *testing.T, out string, results []scored) {
	t.Helper()
	f, err := os.Create(out) // #nosec G304 G703 -- path chosen by whoever runs the test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, r := range results {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d records to %s", len(results), out)
}
