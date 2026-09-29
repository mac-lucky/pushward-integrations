package state_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
)

const rawKey = "hlk_tenant"

var hashedKey = state.HashKey(rawKey)

func mustSet(t *testing.T, s state.Store, key, subKey, value string) {
	t.Helper()
	if err := s.Set(context.Background(), "p", rawKey, key, subKey, json.RawMessage(value), time.Hour); err != nil {
		t.Fatalf("Set(%s/%s): %v", key, subKey, err)
	}
}

func mustGet(t *testing.T, s state.Store, key, subKey string) string {
	t.Helper()
	v, err := s.Get(context.Background(), "p", rawKey, key, subKey)
	if err != nil {
		t.Fatalf("Get(%s/%s): %v", key, subKey, err)
	}
	return string(v)
}

func mustExist(t *testing.T, s state.Store, key, subKey string) bool {
	t.Helper()
	ok, err := s.Exists(context.Background(), "p", rawKey, key, subKey)
	if err != nil {
		t.Fatalf("Exists(%s/%s): %v", key, subKey, err)
	}
	return ok
}

func mustGroup(t *testing.T, s state.Store, key string) map[string]string {
	t.Helper()
	g, err := s.GetGroup(context.Background(), "p", rawKey, key)
	if err != nil {
		t.Fatalf("GetGroup(%s): %v", key, err)
	}
	out := make(map[string]string, len(g))
	for k, v := range g {
		out[k] = string(v)
	}
	return out
}

// userKeys lists the userKey column of every stored row.
func userKeys(m *state.MemoryStore) []string {
	var keys []string
	for _, r := range m.Rows() {
		keys = append(keys, r.UserKey)
	}
	return keys
}

func TestKeyHashing_Ops(t *testing.T) {
	ctx := context.Background()
	mem := state.NewMemoryStore()
	s := state.KeyHashing(mem)

	mustSet(t, s, "k", "", `{"v":1}`)
	mustSet(t, s, "g", "a", `{"a":1}`)
	mustSet(t, s, "g", "b", `{"b":1}`)

	for _, r := range mem.Rows() {
		if r.UserKey != hashedKey {
			t.Fatalf("row %s/%s stored under %q, want %q", r.Key, r.SubKey, r.UserKey, hashedKey)
		}
	}
	if got := mustGet(t, s, "k", ""); got != `{"v":1}` {
		t.Errorf("Get = %s", got)
	}
	if got := mustGet(t, s, "missing", ""); got != "" {
		t.Errorf("Get(missing) = %s, want nothing", got)
	}
	if !mustExist(t, s, "k", "") || mustExist(t, s, "missing", "") {
		t.Error("Exists disagrees with what was written")
	}
	if g := mustGroup(t, s, "g"); len(g) != 2 || g["a"] != `{"a":1}` || g["b"] != `{"b":1}` {
		t.Errorf("GetGroup = %v", g)
	}

	if err := s.Delete(ctx, "p", rawKey, "k", ""); err != nil {
		t.Fatal(err)
	}
	if mustExist(t, s, "k", "") {
		t.Error("row survived Delete")
	}
	if err := s.DeleteGroup(ctx, "p", rawKey, "g"); err != nil {
		t.Fatal(err)
	}
	if n := len(mem.Rows()); n != 0 {
		t.Errorf("%d rows left after deleting everything", n)
	}
}

// A row stored under the raw key, as 0.14 and 0.15 in compat mode wrote them,
// is neither read nor removed. It stays until its TTL runs out.
func TestKeyHashing_IgnoresRawRows(t *testing.T) {
	ctx := context.Background()
	mem := state.NewMemoryStore()
	mustSet(t, mem, "k", "", `"raw"`)
	mustSet(t, mem, "g", "a", `"raw-a"`)
	s := state.KeyHashing(mem)

	if got := mustGet(t, s, "k", ""); got != "" {
		t.Errorf("Get read the raw row: %s", got)
	}
	if mustExist(t, s, "k", "") {
		t.Error("Exists saw the raw row")
	}
	if g := mustGroup(t, s, "g"); len(g) != 0 {
		t.Errorf("GetGroup read the raw rows: %v", g)
	}

	// A write under the same key leaves the raw row alone, and a group read
	// returns only what was written hashed.
	mustSet(t, s, "k", "", `"hashed"`)
	mustSet(t, s, "g", "b", `"hashed-b"`)
	if got := mustGet(t, s, "k", ""); got != `"hashed"` {
		t.Errorf("Get = %s, want the hashed row", got)
	}
	if g := mustGroup(t, s, "g"); len(g) != 1 || g["b"] != `"hashed-b"` {
		t.Errorf("GetGroup = %v, want only the hashed sub-key", g)
	}

	if err := s.Delete(ctx, "p", rawKey, "k", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGroup(ctx, "p", rawKey, "g"); err != nil {
		t.Fatal(err)
	}
	if got := userKeys(mem); len(got) != 2 || got[0] != rawKey || got[1] != rawKey {
		t.Errorf("userKeys after deleting through KeyHashing = %v, want the two raw rows", got)
	}
}

func TestHashKey(t *testing.T) {
	h := state.HashKey(rawKey)
	if len(h) != 64 || strings.Contains(h, "hlk_") {
		t.Errorf("HashKey = %q, want 64 hex chars", h)
	}
	if state.HashKey("hlk_other") == h {
		t.Error("different keys hashed alike")
	}
}
