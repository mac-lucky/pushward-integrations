package universal

import "testing"

func fingerprint(t *testing.T, source, body string) string {
	t.Helper()
	fields, _ := flatten(t, body)
	return Fingerprint(source, fields)
}

func TestFingerprintStable(t *testing.T) {
	base := `{"status": "firing", "title": "disk full", "alerts": [{"id": "a"}], "note": "x", "tags": ["a"]}`
	same := map[string]string{
		"key order":      `{"alerts": [{"id": "a"}], "tags": ["a"], "note": "x", "title": "disk full", "status": "firing"}`,
		"array length":   `{"status": "firing", "title": "disk full", "alerts": [{"id": "a"}, {"id": "b"}, {"id": "c"}], "note": "x", "tags": ["a", "b"]}`,
		"null flip":      `{"status": "firing", "title": "disk full", "alerts": [{"id": "a"}], "note": null, "tags": ["a"]}`,
		"empty list":     `{"status": "firing", "title": "disk full", "alerts": [{"id": "a"}], "note": "x", "tags": []}`,
		"other values":   `{"status": "resolved", "title": "cpu high", "alerts": [{"id": 7}], "note": true, "tags": [3]}`,
		"late element":   `{"status": "firing", "title": "disk full", "alerts": [{"id": "a"}, {"id": "b", "new": 1}], "note": "x", "tags": ["a"]}`,
		"whitespace":     `{"status":"firing","title":"disk full","alerts":[{"id":"a"}],"note":"x","tags":["a"]}`,
		"duplicate keys": `{"status": "firing", "status": "resolved", "title": "disk full", "alerts": [{"id": "a"}], "note": "x", "tags": ["a"]}`,
	}
	want := fingerprint(t, "hc", base)
	for name, body := range same {
		if got := fingerprint(t, "hc", body); got != want {
			t.Errorf("%s changed the fingerprint", name)
		}
	}

	differ := map[string]string{
		"new field":     `{"status": "firing", "title": "disk full", "alerts": [{"id": "a"}], "note": "x", "tags": ["a"], "url": "u"}`,
		"missing field": `{"status": "firing", "title": "disk full", "alerts": [{"id": "a"}], "tags": ["a"]}`,
		"renamed key":   `{"state": "firing", "title": "disk full", "alerts": [{"id": "a"}], "note": "x", "tags": ["a"]}`,
	}
	for name, body := range differ {
		if got := fingerprint(t, "hc", body); got == want {
			t.Errorf("%s kept the fingerprint", name)
		}
	}
	if fingerprint(t, "other", base) == want {
		t.Error("source is not part of the fingerprint")
	}
}

// A source name must not be able to pose as a path: "ab" + ["c"] and
// "a" + ["bc"] are different shapes.
func TestFingerprintNoConcatenationCollision(t *testing.T) {
	a := Fingerprint("ab", []Field{{Path: "c"}})
	b := Fingerprint("a", []Field{{Path: "bc"}})
	if a == b {
		t.Error("source and path boundaries collide")
	}
}
