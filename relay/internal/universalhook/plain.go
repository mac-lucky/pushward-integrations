package universalhook

import (
	"encoding/json"
	"strings"

	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// maxNotificationBytes caps a notification request whose body the relay
// writes itself. The server refuses a push whose APNs payload passes 4096
// bytes, and it adds its own envelope around what the relay sends.
const maxNotificationBytes = 3 << 10

// Detail lines stand in for a body the mapping did not find.
const (
	maxDetailLines   = 4
	detailLabelRunes = 40
	detailValueRunes = 60
)

// readable are the value classes a detail line shows. Ids, times and links
// say nothing on a lock screen, and Display would only mask a secret or an
// email address.
var readable = map[universal.ValueClass]bool{
	universal.ClassText:   true,
	universal.ClassEnum:   true,
	universal.ClassNumber: true,
	universal.ClassBool:   true,
}

// fallbackBody is the body of a notification with nothing to say beyond its
// title: the server needs one, and repeating the title says nothing.
func fallbackBody(source string) string {
	if t := humanize(source); t != "" {
		return "Event from " + t
	}
	return "Webhook event"
}

// sourceTitle titles a notification whose payload has no title: the source
// in words ("my-app" gives "My app"), or "Webhook" when there is none.
func sourceTitle(source string) string {
	if t := humanize(source); t != "" {
		return t
	}
	return "Webhook"
}

// humanize turns the last key of a path, or a source, into words with a
// capital first letter: "disk_used", "diskUsed" and "disk-used" all give
// "Disk used".
func humanize(path string) string {
	return text.Capitalize(strings.Join(universal.TokensOf(path).Key, " "))
}

// detailLines lists up to maxDetailLines "label: value" lines from the
// readable fields m does not map: those that read best as a body first, then
// the rest in payload order. shapes must be ShapesOf(fields). The label is the
// field's key in words, and the value goes through Display, so nothing secret
// reaches the lock screen. A line that would take the lines past budget bytes
// of JSON is left out, since escaped characters count several times over. It
// returns "" when no line fits.
func detailLines(fields []universal.Field, shapes []universal.ShapeField, m universal.Mapping, budget int) string {
	index := make(map[string]int, len(shapes))
	for i := len(shapes) - 1; i >= 0; i-- {
		index[shapes[i].Path] = i
	}
	done := make(map[string]bool, len(shapes))
	for _, p := range m.Paths {
		done[p] = true
	}
	ranked := universal.RankShapes(shapes, universal.RoleBody)
	order := make([]string, 0, len(ranked)+len(shapes))
	for _, c := range ranked {
		order = append(order, c.Path)
	}
	for _, s := range shapes {
		order = append(order, s.Path)
	}

	lines := make([]string, 0, maxDetailLines)
	used := 0
	for _, path := range order {
		if len(lines) == maxDetailLines {
			break
		}
		if done[path] {
			continue
		}
		done[path] = true
		i := index[path]
		if !readable[shapes[i].Class] {
			continue
		}
		// One line each: a value's own line breaks would pass the line cap,
		// and could pass off the rest of it as a labelled line.
		v := strings.Join(strings.Fields(universal.Display(path, fields[i], detailValueRunes)), " ")
		if v == "" {
			continue
		}
		line := label(path) + ": " + v
		cost := jsonLen(line)
		if len(lines) > 0 {
			cost += jsonLen("\n")
		}
		if used+cost > budget {
			continue
		}
		lines = append(lines, line)
		used += cost
	}
	return strings.Join(lines, "\n")
}

func label(path string) string {
	if l := humanize(path); l != "" {
		return text.Truncate(l, detailLabelRunes)
	}
	return "Value"
}

// jsonLen is the length of s as a JSON string, without the quotes.
func jsonLen(s string) int {
	b, _ := json.Marshal(s)
	return len(b) - 2
}
