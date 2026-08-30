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

// EnvDurationPtr applies a duration environment override to an optional field.
// Unlike EnvDuration it can distinguish "unset" (nil, so the server default
// applies) from an explicit zero, which for dismissal_ttl means "remove the
// card immediately".
func EnvDurationPtr(name string, dst **time.Duration) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	*dst = &d
	return nil
}

// EnvInt applies an integer environment override to dst, leaving it untouched
// when the variable is unset or empty.
//
// strconv rejects trailing garbage ("5x", "0x10"); fmt.Sscanf would silently
// accept it and truncate to a wrong value. The wrapped error already quotes the
// offending input, so the message here only needs to name the variable.
func EnvInt(name string, dst *int) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	*dst = n
	return nil
}

// EnvInt64 is EnvInt for a 64-bit field, which is the width a value has to be
// when it crosses a wire that specifies one.
func EnvInt64(name string, dst *int64) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	*dst = n
	return nil
}

// EnvFloat64 applies a floating-point environment override to dst, leaving it
// untouched when the variable is unset or empty. Range is the caller's business:
// this only reports what could not be parsed at all.
func EnvFloat64(name string, dst *float64) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	*dst = f
	return nil
}
