package forgejo

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"
)

// minValidTime is the floor below which a decoded timestamp is treated as
// absent. Forgejo serialises an unset time as the unix epoch rather than null,
// so a literal reading gives an unstarted job a start of 1970. Fed to the live
// -progress anchor that is a 55-year animation window; fed to the pill weights,
// a 55-year step. Anything this old is a sentinel, not a measurement.
var minValidTime = time.Date(1972, 1, 1, 0, 0, 0, 0, time.UTC)

// flexTime decodes a Forgejo timestamp tolerantly. A single malformed value
// anywhere in a page would otherwise fail the whole decode and drop the poll, so
// null, "", a bare unix number and an RFC3339 string all decode, and anything
// unparseable or older than minValidTime yields the zero time. It never returns
// an error.
//
// Zero is the "unknown" the shared ladder already understands: it declines to
// anchor and floors the weight rather than inventing either.
type flexTime struct{ t time.Time }

func (f *flexTime) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return nil
	}

	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil || s == "" {
			return nil
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil
		}
		f.set(t)
		return nil
	}

	// Some Gitea-lineage endpoints hand back a bare unix second count.
	secs, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return nil
	}
	f.set(time.Unix(secs, 0).UTC())
	return nil
}

func (f *flexTime) set(t time.Time) {
	if t.Before(minValidTime) {
		return
	}
	f.t = t
}

func (f flexTime) Time() time.Time { return f.t }

func (f flexTime) IsZero() bool { return f.t.IsZero() }
