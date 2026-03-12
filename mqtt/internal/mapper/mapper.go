package mapper

import (
	"strconv"

	"github.com/mac-lucky/pushward-integrations/mqtt/internal/config"
	"github.com/mac-lucky/pushward-integrations/mqtt/internal/extract"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// Map resolves a ContentMapping against MQTT message data and returns a pushward.Content.
func Map(data map[string]any, mapping config.ContentMapping, template string) pushward.Content {
	c := pushward.Content{
		Template: template,
	}

	if mapping.State != "" {
		c.State = extract.ResolveTemplate(mapping.State, data)
	}
	if mapping.Subtitle != "" {
		c.Subtitle = extract.ResolveTemplate(mapping.Subtitle, data)
	}
	if mapping.Progress != "" {
		s := extract.ResolveTemplate(mapping.Progress, data)
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			c.Progress = f
		}
	}
	if mapping.RemainingTime != "" {
		s := extract.ResolveTemplate(mapping.RemainingTime, data)
		if i, err := strconv.Atoi(s); err == nil {
			c.RemainingTime = &i
		}
	}
	if mapping.CurrentStep != "" {
		s := extract.ResolveTemplate(mapping.CurrentStep, data)
		if i, err := strconv.Atoi(s); err == nil {
			c.CurrentStep = &i
		}
	}
	if mapping.TotalSteps != "" {
		s := extract.ResolveTemplate(mapping.TotalSteps, data)
		if i, err := strconv.Atoi(s); err == nil {
			c.TotalSteps = &i
		}
	}
	if mapping.URL != "" {
		c.URL = extract.ResolveTemplate(mapping.URL, data)
	}
	if mapping.SecondaryURL != "" {
		c.SecondaryURL = extract.ResolveTemplate(mapping.SecondaryURL, data)
	}
	if mapping.Severity != "" {
		c.Severity = extract.ResolveTemplate(mapping.Severity, data)
	}

	c.Icon = resolveFieldOrMap(mapping.Icon, data)
	c.AccentColor = resolveFieldOrMap(mapping.AccentColor, data)

	return c
}

// resolveFieldOrMap resolves a FieldOrMap config: checks the map entries against
// field values in data, and falls back to the default.
func resolveFieldOrMap(fom config.FieldOrMap, data map[string]any) string {
	for field, valueMap := range fom.Map {
		strVal, ok := extract.GetString(data, field)
		if !ok {
			continue
		}
		if mapped, ok := valueMap[strVal]; ok {
			return mapped
		}
	}
	return fom.Default
}
