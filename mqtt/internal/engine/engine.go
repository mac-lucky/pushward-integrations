package engine

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mac-lucky/pushward-integrations/mqtt/internal/config"
	"github.com/mac-lucky/pushward-integrations/mqtt/internal/tracker"
)

// RuleEntry pairs a config rule with its tracker.
type RuleEntry struct {
	rule    *config.RuleConfig
	tracker *tracker.Tracker
}

// Engine routes incoming MQTT messages to the appropriate rule tracker.
type Engine struct {
	rules []*RuleEntry
}

// New creates an engine with trackers for each rule.
func New(rules []*RuleEntry) *Engine {
	return &Engine{rules: rules}
}

// NewRuleEntry creates a rule entry for the engine.
func NewRuleEntry(rule *config.RuleConfig, tr *tracker.Tracker) *RuleEntry {
	return &RuleEntry{rule: rule, tracker: tr}
}

// Topics returns the unique set of MQTT topics to subscribe to.
func (e *Engine) Topics() []string {
	seen := make(map[string]bool)
	var topics []string
	for _, re := range e.rules {
		if !seen[re.rule.Topic] {
			seen[re.rule.Topic] = true
			topics = append(topics, re.rule.Topic)
		}
	}
	return topics
}

// Route processes an incoming MQTT message by matching it to rules and dispatching.
func (e *Engine) Route(topic string, payload []byte) {
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		slog.Debug("ignoring non-JSON MQTT message", "topic", topic, "error", err)
		return
	}

	for _, re := range e.rules {
		if !topicMatches(re.rule.Topic, topic) {
			continue
		}

		// Inject virtual fields
		data["_topic"] = topic
		segments := strings.Split(topic, "/")
		for i, seg := range segments {
			data[fmt.Sprintf("_topic_segment:%d", i)] = seg
		}

		re.tracker.HandleMessage(data)
	}
}

// Stop stops all trackers.
func (e *Engine) Stop() {
	for _, re := range e.rules {
		re.tracker.Stop()
	}
}

// topicMatches checks if an actual topic matches a subscription topic pattern.
// Supports MQTT wildcards: + (single level) and # (multi level).
func topicMatches(pattern, topic string) bool {
	patternParts := strings.Split(pattern, "/")
	topicParts := strings.Split(topic, "/")

	for i, pp := range patternParts {
		if pp == "#" {
			// # must match at least one level
			return i < len(topicParts)
		}
		if i >= len(topicParts) {
			return false
		}
		if pp == "+" {
			continue
		}
		if pp != topicParts[i] {
			return false
		}
	}

	return len(patternParts) == len(topicParts)
}
