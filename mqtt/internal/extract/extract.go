package extract

import (
	"fmt"
	"strconv"
	"strings"
)

// Get walks a nested map using dot-notation (e.g. "print.status.percent")
// and returns the value found, or (nil, false) if any key is missing.
func Get(data map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var current any = data
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// GetString extracts a value and coerces it to string.
func GetString(data map[string]any, path string) (string, bool) {
	v, ok := Get(data, path)
	if !ok {
		return "", false
	}
	switch val := v.(type) {
	case string:
		return val, true
	case float64:
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10), true
		}
		return strconv.FormatFloat(val, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(val), true
	case nil:
		return "", false
	default:
		return fmt.Sprintf("%v", val), true
	}
}

// GetFloat64 extracts a value and coerces it to float64.
func GetFloat64(data map[string]any, path string) (float64, bool) {
	v, ok := Get(data, path)
	if !ok {
		return 0, false
	}
	switch val := v.(type) {
	case float64:
		return val, true
	case int:
		return float64(val), true
	case string:
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// GetInt extracts a value and coerces it to int.
func GetInt(data map[string]any, path string) (int, bool) {
	v, ok := Get(data, path)
	if !ok {
		return 0, false
	}
	switch val := v.(type) {
	case float64:
		return int(val), true
	case int:
		return val, true
	case string:
		i, err := strconv.Atoi(val)
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}
