package presets

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
)

// These tests read payloads that are not vendored here: the classifier
// checkout's corpus, its copy of the Zulip webhook fixtures and its OOD set.
// Each skips when its directory is absent. PRESETS_CLASSIFIER_DIR points at
// another checkout; set is true when it did, so a bad path is an error
// rather than a skip.
func classifierDir(t *testing.T, sub ...string) string {
	t.Helper()
	root, set := os.Getenv("PRESETS_CLASSIFIER_DIR"), true
	if root == "" {
		root, set = filepath.Join("..", "..", "..", "..", "..", "pushward-classifier"), false
	}
	dir := filepath.Join(append([]string{root}, sub...)...)
	if _, err := os.Stat(dir); err != nil { // #nosec G703 -- a test reading a directory it names
		if set {
			t.Fatalf("PRESETS_CLASSIFIER_DIR: %v", err)
		}
		t.Skipf("%s not found", dir)
	}
	return dir
}

func flattenJSON(t *testing.T, raw []byte) []universal.Field {
	t.Helper()
	fields, _, err := universal.Flatten(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	return fields
}

// owns reports whether service is one of p's vendor names, compared without
// case, dashes or underscores.
func owns(p Preset, service string) bool {
	norm := func(s string) string {
		return strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(s))
	}
	return slices.ContainsFunc(p.sources, func(s string) bool { return norm(s) == norm(service) })
}

// TestCorpusFalsePositives posts every corpus payload with no source: none
// of another service may match a preset. The corpus leaves out every OOD
// service, so nearly every match would be a false one.
func TestCorpusFalsePositives(t *testing.T) {
	path := filepath.Join(classifierDir(t, "corpus"), "labelled.jsonl")
	f, err := os.Open(path) // #nosec G304 -- a fixed path under the classifier checkout
	if err != nil {
		t.Skipf("%s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	n, bad := 0, 0
	for sc.Scan() {
		var rec struct {
			ID      string          `json:"id"`
			Service string          `json:"service"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || len(rec.Payload) == 0 {
			continue
		}
		n++
		p, _, ok := Match("", flattenJSON(t, rec.Payload))
		if ok && !owns(p, rec.Service) {
			bad++
			t.Errorf("%s (%s) matches %s", rec.ID, rec.Service, p.ID)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d corpus payloads, %d false positives", n, bad)
}

// TestZulipFixtures posts the Zulip project's fixtures (Apache-2.0) for every
// vendor a preset names. A fixture may match only a preset of its own vendor,
// and a match must title the event. The share each vendor's presets cover is
// logged: Zulip keeps fixtures for events no preset is meant to take.
func TestZulipFixtures(t *testing.T) {
	root := filepath.Join(classifierDir(t, "corpus", "raw", "zulip"), "zerver", "webhooks")
	vendors := map[string][]Preset{}
	for _, p := range all {
		for _, s := range p.sources {
			vendors[s] = append(vendors[s], p)
		}
	}
	names := make([]string, 0, len(vendors))
	for s := range vendors {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, source := range names {
		files, _ := filepath.Glob(filepath.Join(root, strings.ReplaceAll(source, "-", "_"), "fixtures", "*.json"))
		if len(files) == 0 {
			continue
		}
		matched := 0
		for _, file := range files {
			raw, err := os.ReadFile(file) // #nosec G304 -- fixtures under the classifier checkout
			if err != nil {
				t.Fatal(err)
			}
			fields := flattenJSON(t, raw)
			p, m, ok := Match("", fields)
			if !ok {
				continue
			}
			if !owns(p, source) {
				t.Errorf("zulip %s/%s matches %s", source, filepath.Base(file), p.ID)
				continue
			}
			matched++
			if ev := universal.Apply(m, fields, source); ev.Title == "" {
				t.Errorf("zulip %s/%s: %s gives no title", source, filepath.Base(file), p.ID)
			}
		}
		t.Logf("zulip %-16s %3d/%3d fixtures matched", source, matched, len(files))
	}
}

// TestOODAgreement logs, per OOD payload a preset takes, which roles land on
// a path the gold labels accept. The OOD set is the classifier's held-out
// check: presets are written from vendor docs, never from it, so nothing is
// asserted here.
func TestOODAgreement(t *testing.T) {
	root := classifierDir(t, "dataset", "ood")
	files, _ := filepath.Glob(filepath.Join(root, "*", "*.json"))
	roles := []universal.Role{
		universal.RoleTitle, universal.RoleBody, universal.RoleURL,
		universal.RoleCorrelation, universal.RoleSeverity, universal.RoleLifecycle,
	}
	hit, total := map[universal.Role]int{}, map[universal.Role]int{}
	taken := 0
	for _, file := range files {
		raw, err := os.ReadFile(file) // #nosec G304 -- payloads under the classifier checkout
		if err != nil {
			t.Fatal(err)
		}
		var ex struct {
			Source  string                             `json:"source"`
			Payload json.RawMessage                    `json:"payload"`
			Labels  map[universal.Role]json.RawMessage `json:"labels"`
		}
		if json.Unmarshal(raw, &ex) != nil {
			continue
		}
		p, m, ok := Match("", flattenJSON(t, ex.Payload))
		if !ok {
			continue
		}
		taken++
		var line []string
		for _, r := range roles {
			var gold []string
			if json.Unmarshal(ex.Labels[r], &gold) != nil {
				continue
			}
			got := m.Paths[r]
			if len(gold) == 0 && got == "" {
				continue
			}
			total[r]++
			if slices.Contains(gold, got) {
				hit[r]++
				continue
			}
			line = append(line, string(r))
		}
		t.Logf("%-40s %-24s misses %v", strings.TrimSuffix(filepath.Base(filepath.Dir(file))+"/"+filepath.Base(file), ".json"), p.ID, line)
	}
	for _, r := range roles {
		if total[r] > 0 {
			t.Logf("%-12s %d/%d agree with gold", r, hit[r], total[r])
		}
	}
	t.Logf("%d of %d OOD payloads taken by a preset", taken, len(files))
}

// TestZulipGiteaIsNotGitHub posts the Zulip project's real Gitea and Gogs
// deliveries, which share GitHub's layout, with no source: none may match a
// preset. The relay's own Gitea fixtures are trimmed too far to show that.
func TestZulipGiteaIsNotGitHub(t *testing.T) {
	root := filepath.Join(classifierDir(t, "corpus", "raw", "zulip"), "zerver", "webhooks")
	n := 0
	for _, vendor := range []string{"gitea", "gogs"} {
		files, _ := filepath.Glob(filepath.Join(root, vendor, "fixtures", "*.json"))
		for _, file := range files {
			raw, err := os.ReadFile(file) // #nosec G304 -- fixtures under the classifier checkout
			if err != nil {
				t.Fatal(err)
			}
			n++
			if p, _, ok := Match("", flattenJSON(t, raw)); ok {
				t.Errorf("zulip %s/%s matches %s", vendor, filepath.Base(file), p.ID)
			}
		}
	}
	t.Logf("%d gitea and gogs fixtures, none matched", n)
}
