package relaytest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// Call is one PushWard API call as the exporter writes it.
type Call struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// Calls converts recorded calls, bodies as sent.
func Calls(calls []testutil.APICall) []Call {
	out := make([]Call, 0, len(calls))
	for _, c := range calls {
		out = append(out, Call{Method: c.Method, Path: c.Path, Body: c.Body})
	}
	return out
}

// NormalizeCalls converts recorded calls with the fields that depend on the
// clock dropped, so two runs of one fixture compare equal: content.fired_at
// and content.end_date, and the action URLs of a notification, whose tokens
// carry an expiry. Bodies come out with their keys sorted.
func NormalizeCalls(calls []testutil.APICall) []Call {
	out := Calls(calls)
	for i := range out {
		out[i].Body = normalizeBody(out[i].Body)
	}
	return out
}

func normalizeBody(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return raw
	}
	if content, ok := body["content"].(map[string]any); ok {
		delete(content, "fired_at")
		delete(content, "end_date")
	}
	if actions, ok := body["actions"].([]any); ok {
		for _, a := range actions {
			if m, ok := a.(map[string]any); ok {
				delete(m, "url")
			}
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return b
}

// Outcome is what a user sees of one event, read off the calls a handler
// made. Kind is alert for an alert-template card, progress for any other
// card, notification when there are only notifications, and empty when the
// handler made no call. Title is the card's name, else the notification's
// title; Body is the card's first state line, else the notification's body.
// Lifecycle is ended or ongoing for a card and empty otherwise. The universal
// route's mapping review is not part of the event and is left out.
type Outcome struct {
	Kind      string `json:"kind,omitempty"`
	Title     string `json:"title,omitempty"`
	Body      string `json:"body,omitempty"`
	URL       string `json:"url,omitempty"`
	Lifecycle string `json:"lifecycle,omitempty"`
	Severity  string `json:"severity,omitempty"`
}

// reviewThread is the thread universalhook sends its mapping reviews on.
const reviewThread = "universal-review"

// OutcomeOf reads the Outcome off calls.
func OutcomeOf(calls []testutil.APICall) Outcome {
	var (
		o                                Outcome
		names, states, urls              []string
		notifTitles, notifBodies, nurls  []string
		activity, notified, alert, ended bool
	)
	for _, c := range calls {
		var b struct {
			Name     string `json:"name"`
			State    string `json:"state"`
			Title    string `json:"title"`
			Body     string `json:"body"`
			URL      string `json:"url"`
			ThreadID string `json:"thread_id"`
			Content  struct {
				Template string `json:"template"`
				State    string `json:"state"`
				URL      string `json:"url"`
				Severity string `json:"severity"`
			} `json:"content"`
		}
		_ = json.Unmarshal(c.Body, &b)
		switch {
		case c.Method == http.MethodPost && c.Path == "/activities":
			activity = true
			names = appendNonEmpty(names, b.Name)
		case c.Method == http.MethodPatch && strings.HasPrefix(c.Path, "/activities/"):
			activity = true
			alert = alert || b.Content.Template == "alert"
			states = appendNonEmpty(states, b.Content.State)
			urls = appendNonEmpty(urls, b.Content.URL)
			if b.Content.Severity != "" {
				o.Severity = b.Content.Severity
			}
			ended = ended || b.State == "ended"
		case c.Method == http.MethodPost && c.Path == "/notifications" && b.ThreadID != reviewThread:
			notified = true
			notifTitles = appendNonEmpty(notifTitles, b.Title)
			notifBodies = appendNonEmpty(notifBodies, b.Body)
			nurls = appendNonEmpty(nurls, b.URL)
		}
	}
	switch {
	case alert:
		o.Kind = "alert"
	case activity:
		o.Kind = "progress"
	case notified:
		o.Kind = "notification"
	}
	if activity {
		o.Lifecycle = "ongoing"
		if ended {
			o.Lifecycle = "ended"
		}
	}
	o.Title = first(names, notifTitles)
	o.Body = first(states, notifBodies)
	o.URL = first(urls, nurls)
	return o
}

func appendNonEmpty(s []string, v string) []string {
	if v == "" {
		return s
	}
	return append(s, v)
}

func first(lists ...[]string) string {
	for _, l := range lists {
		if len(l) > 0 {
			return l[0]
		}
	}
	return ""
}
