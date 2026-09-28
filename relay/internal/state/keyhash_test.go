package state_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
)

const rawKey = "hlk_tenant"

var hashedKey = state.HashKey(rawKey)

// old is a writer or reader from before key hashing: the bare store, raw key.
func old(inner state.Store) state.Store { return inner }

func mode(m state.KeyMode) func(state.Store) state.Store {
	return func(inner state.Store) state.Store { return state.KeyHashing(inner, m) }
}

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
	tests := []struct {
		mode   state.KeyMode
		stored string
	}{
		{state.KeyModeCompat, rawKey},
		{state.KeyModeHashed, hashedKey},
		{state.KeyModeStrict, hashedKey},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			ctx := context.Background()
			mem := state.NewMemoryStore()
			s := state.KeyHashing(mem, tt.mode)

			mustSet(t, s, "k", "", `{"v":1}`)
			mustSet(t, s, "g", "a", `{"a":1}`)
			mustSet(t, s, "g", "b", `{"b":1}`)

			for _, r := range mem.Rows() {
				if r.UserKey != tt.stored {
					t.Fatalf("row %s/%s stored under %q, want %q", r.Key, r.SubKey, r.UserKey, tt.stored)
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
		})
	}
}

// Each pair shares one store, as two replicas on different releases share the
// relay_state table during a rollout. The pairs are the ones the rollout puts
// side by side: old with compat, then compat with hashed.
func TestKeyHashing_CompatibleWriterReaderPairs(t *testing.T) {
	pairs := []struct {
		name           string
		writer, reader func(state.Store) state.Store
	}{
		{"old writer, compat reader", old, mode(state.KeyModeCompat)},
		{"compat writer, old reader", mode(state.KeyModeCompat), old},
		{"compat writer, hashed reader", mode(state.KeyModeCompat), mode(state.KeyModeHashed)},
		{"hashed writer, compat reader", mode(state.KeyModeHashed), mode(state.KeyModeCompat)},
	}
	for _, tt := range pairs {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			mem := state.NewMemoryStore()
			w, r := tt.writer(mem), tt.reader(mem)

			mustSet(t, w, "k", "", `{"v":1}`)
			mustSet(t, w, "g", "a", `{"a":1}`)

			if got := mustGet(t, r, "k", ""); got != `{"v":1}` {
				t.Errorf("reader Get = %q", got)
			}
			if !mustExist(t, r, "k", "") {
				t.Error("reader does not see the row")
			}
			if g := mustGroup(t, r, "g"); g["a"] != `{"a":1}` {
				t.Errorf("reader GetGroup = %v", g)
			}

			// The reader overwrites; the writer must see the new value, not a
			// stale twin of the old one.
			mustSet(t, r, "k", "", `{"v":2}`)
			if got := mustGet(t, w, "k", ""); got != `{"v":2}` {
				t.Errorf("writer reads %q after the reader's write, want the new value", got)
			}

			// A delete from either side is a delete for both.
			if err := r.Delete(ctx, "p", rawKey, "k", ""); err != nil {
				t.Fatal(err)
			}
			if mustExist(t, w, "k", "") {
				t.Error("writer still sees a row the reader deleted")
			}
			if err := r.DeleteGroup(ctx, "p", rawKey, "g"); err != nil {
				t.Fatal(err)
			}
			if g := mustGroup(t, w, "g"); len(g) != 0 {
				t.Errorf("writer still sees group %v after the reader deleted it", g)
			}
		})
	}
}

func TestKeyHashing_WriteDeletesTwin(t *testing.T) {
	tests := []struct {
		name  string
		first func(state.Store) state.Store
		then  state.KeyMode
		want  string
	}{
		{"hashed write removes a raw row", old, state.KeyModeHashed, hashedKey},
		{"compat write removes a hashed row", mode(state.KeyModeHashed), state.KeyModeCompat, rawKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem := state.NewMemoryStore()
			mustSet(t, tt.first(mem), "k", "s", `{"v":1}`)
			mustSet(t, state.KeyHashing(mem, tt.then), "k", "s", `{"v":2}`)

			rows := mem.Rows()
			if len(rows) != 1 || rows[0].UserKey != tt.want || string(rows[0].Value) != `{"v":2}` {
				t.Fatalf("rows = %+v, want one %q row holding the new value", rows, tt.want)
			}
		})
	}
}

func TestKeyHashing_GetGroupMergesTwins(t *testing.T) {
	mem := state.NewMemoryStore()
	// A group half-written by each release: sub-key b exists under both keys.
	mustSet(t, mem, "g", "a", `"raw-a"`)
	mustSet(t, mem, "g", "b", `"raw-b"`)
	if err := mem.Set(context.Background(), "p", hashedKey, "g", "b", json.RawMessage(`"hashed-b"`), time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := mem.Set(context.Background(), "p", hashedKey, "g", "c", json.RawMessage(`"hashed-c"`), time.Hour); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		mode state.KeyMode
		want map[string]string
	}{
		{state.KeyModeHashed, map[string]string{"a": `"raw-a"`, "b": `"hashed-b"`, "c": `"hashed-c"`}},
		{state.KeyModeCompat, map[string]string{"a": `"raw-a"`, "b": `"raw-b"`, "c": `"hashed-c"`}},
		{state.KeyModeStrict, map[string]string{"b": `"hashed-b"`, "c": `"hashed-c"`}},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			got := mustGroup(t, state.KeyHashing(mem, tt.mode), "g")
			if len(got) != len(tt.want) {
				t.Fatalf("GetGroup = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("GetGroup[%s] = %s, want %s", k, got[k], v)
				}
			}
		})
	}
}

// Strict is for state that never had a raw key: a raw row is neither read nor
// cleaned up.
func TestKeyHashing_StrictIgnoresRawRows(t *testing.T) {
	mem := state.NewMemoryStore()
	mustSet(t, mem, "k", "", `"raw"`)
	s := state.KeyHashing(mem, state.KeyModeStrict)

	if got := mustGet(t, s, "k", ""); got != "" {
		t.Errorf("strict Get read the raw row: %s", got)
	}
	if mustExist(t, s, "k", "") {
		t.Error("strict Exists saw the raw row")
	}
	mustSet(t, s, "k", "", `"hashed"`)
	if got := userKeys(mem); len(got) != 2 {
		t.Errorf("strict write touched the raw row: userKeys = %v", got)
	}
}

// failingDeletes is a MemoryStore whose deletes fail for one userKey.
type failingDeletes struct {
	*state.MemoryStore
	userKey string
}

var errDelete = errors.New("delete failed")

func (f failingDeletes) Delete(ctx context.Context, provider, userKey, key, subKey string) error {
	if userKey == f.userKey {
		return errDelete
	}
	return f.MemoryStore.Delete(ctx, provider, userKey, key, subKey)
}

func (f failingDeletes) DeleteGroup(ctx context.Context, provider, userKey, key string) error {
	if userKey == f.userKey {
		return errDelete
	}
	return f.MemoryStore.DeleteGroup(ctx, provider, userKey, key)
}

func TestKeyHashing_TwinDeleteFailure(t *testing.T) {
	ctx := context.Background()
	mem := state.NewMemoryStore()
	s := state.KeyHashing(failingDeletes{MemoryStore: mem, userKey: rawKey}, state.KeyModeHashed)

	// Set: the new row is still written and read back in this mode; the stale
	// raw twin stays behind until its TTL.
	mustSet(t, mem, "k", "", `"raw"`)
	mustSet(t, s, "k", "", `"hashed"`)
	if got := mustGet(t, s, "k", ""); got != `"hashed"` {
		t.Errorf("Get = %s, want the new value over the surviving twin", got)
	}

	// Delete: the surviving twin would read back as live state, so the error
	// reaches the caller.
	if err := s.Delete(ctx, "p", rawKey, "k", ""); !errors.Is(err, errDelete) {
		t.Errorf("Delete error = %v, want the twin's failure", err)
	}
	if err := s.DeleteGroup(ctx, "p", rawKey, "k"); !errors.Is(err, errDelete) {
		t.Errorf("DeleteGroup error = %v, want the twin's failure", err)
	}
}

// recording is a MemoryStore that logs the order of writes and deletes.
type recording struct {
	*state.MemoryStore
	ops []string
}

func (r *recording) Set(ctx context.Context, provider, userKey, key, subKey string, value json.RawMessage, ttl time.Duration) error {
	r.ops = append(r.ops, "set "+userKey)
	return r.MemoryStore.Set(ctx, provider, userKey, key, subKey, value, ttl)
}

func (r *recording) Delete(ctx context.Context, provider, userKey, key, subKey string) error {
	r.ops = append(r.ops, "delete "+userKey)
	return r.MemoryStore.Delete(ctx, provider, userKey, key, subKey)
}

// Set removes the twin before writing: with a compat and a hashed replica
// racing on one row, write-then-delete could lose both copies, while this order
// can only leave a duplicate.
func TestKeyHashing_SetDeletesTwinFirst(t *testing.T) {
	tests := []struct {
		mode state.KeyMode
		want []string
	}{
		{state.KeyModeCompat, []string{"delete " + hashedKey, "set " + rawKey}},
		{state.KeyModeHashed, []string{"delete " + rawKey, "set " + hashedKey}},
		{state.KeyModeStrict, []string{"set " + hashedKey}},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			rec := &recording{MemoryStore: state.NewMemoryStore()}
			mustSet(t, state.KeyHashing(rec, tt.mode), "k", "", `{}`)
			if strings.Join(rec.ops, ", ") != strings.Join(tt.want, ", ") {
				t.Errorf("ops = %v, want %v", rec.ops, tt.want)
			}
		})
	}
}

func TestKeyHashing_UnknownModePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("KeyHashing accepted an unknown mode")
		}
	}()
	state.KeyHashing(state.NewMemoryStore(), "")
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
