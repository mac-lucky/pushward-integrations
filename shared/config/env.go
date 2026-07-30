package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// SplitList parses a comma-separated env value, dropping blanks and surrounding
// whitespace. A trailing comma or a wrapped YAML value would otherwise produce an
// empty entry, and an empty repo name fails every poll for the life of the process.
func SplitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// EnvBool applies a boolean environment override to dst, leaving it untouched
// when the variable is unset or empty. An unparseable value is an error rather
// than a silent default: a flag that defaults on would otherwise stay on for
// anyone who wrote "yes" or "enabled" and believed they had turned it off.
func EnvBool(name string, dst *bool) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	*dst = b
	return nil
}

// EnvDuration applies a duration environment override to dst, leaving it
// untouched when the variable is unset or empty. As with EnvBool, a typo in a
// manifest fails loudly at startup instead of quietly keeping the default.
func EnvDuration(name string, dst *time.Duration) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	*dst = d
	return nil
}
