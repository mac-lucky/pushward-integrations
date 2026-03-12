package extract

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ApplyTransform applies a single transform to a value.
// Transform format: "name" or "name:arg1:arg2".
func ApplyTransform(value any, transform string) (any, error) {
	parts := strings.SplitN(transform, ":", 2)
	name := parts[0]
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	switch name {
	case "div":
		return applyDiv(value, args)
	case "mul":
		return applyMul(value, args)
	case "format":
		return applyFormat(value, args)
	case "scale":
		return applyScale(value, args)
	case "default":
		return applyDefault(value, args)
	case "upper":
		return applyUpper(value)
	case "lower":
		return applyLower(value)
	default:
		return nil, fmt.Errorf("unknown transform: %s", name)
	}
}

func toFloat64(v any) (float64, bool) {
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

func applyDiv(value any, args string) (any, error) {
	if args == "" {
		return nil, fmt.Errorf("div requires an argument")
	}
	divisor, err := strconv.ParseFloat(args, 64)
	if err != nil {
		return nil, fmt.Errorf("div: invalid divisor: %s", args)
	}
	if divisor == 0 {
		return nil, fmt.Errorf("div: division by zero")
	}
	f, ok := toFloat64(value)
	if !ok {
		return nil, fmt.Errorf("div: cannot convert value to float64")
	}
	return f / divisor, nil
}

func applyMul(value any, args string) (any, error) {
	if args == "" {
		return nil, fmt.Errorf("mul requires an argument")
	}
	multiplier, err := strconv.ParseFloat(args, 64)
	if err != nil {
		return nil, fmt.Errorf("mul: invalid multiplier: %s", args)
	}
	f, ok := toFloat64(value)
	if !ok {
		return nil, fmt.Errorf("mul: cannot convert value to float64")
	}
	return f * multiplier, nil
}

func applyFormat(value any, args string) (any, error) {
	if args == "" {
		return nil, fmt.Errorf("format requires a format string")
	}
	f, ok := toFloat64(value)
	if ok {
		return fmt.Sprintf(args, f), nil
	}
	return fmt.Sprintf(args, value), nil
}

func applyScale(value any, args string) (any, error) {
	parts := strings.SplitN(args, ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("scale requires min:max arguments")
	}
	min, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return nil, fmt.Errorf("scale: invalid min: %s", parts[0])
	}
	max, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return nil, fmt.Errorf("scale: invalid max: %s", parts[1])
	}
	if max == min {
		return nil, fmt.Errorf("scale: min and max cannot be equal")
	}
	f, ok := toFloat64(value)
	if !ok {
		return nil, fmt.Errorf("scale: cannot convert value to float64")
	}
	scaled := (f - min) / (max - min)
	if scaled < 0 {
		scaled = 0
	}
	if scaled > 1 {
		scaled = 1
	}
	return scaled, nil
}

func applyDefault(value any, args string) (any, error) {
	if value == nil || value == "" {
		return args, nil
	}
	return value, nil
}

func applyUpper(value any) (any, error) {
	s, ok := value.(string)
	if !ok {
		s = fmt.Sprintf("%v", value)
	}
	return strings.ToUpper(s), nil
}

func applyLower(value any) (any, error) {
	s, ok := value.(string)
	if !ok {
		s = fmt.Sprintf("%v", value)
	}
	return strings.ToLower(s), nil
}

var templatePattern = regexp.MustCompile(`\{([^}]+)\}`)

// ResolveTemplate scans a template string for {field | transform | ...} patterns,
// resolves each field from data, applies transforms in order, and substitutes back.
// Missing fields resolve to empty string.
func ResolveTemplate(tmpl string, data map[string]any) string {
	return templatePattern.ReplaceAllStringFunc(tmpl, func(match string) string {
		// Strip { and }
		inner := match[1 : len(match)-1]

		// Split on " | " (with spaces around pipe)
		segments := strings.Split(inner, " | ")
		fieldPath := strings.TrimSpace(segments[0])

		val, ok := Get(data, fieldPath)
		if !ok {
			// Field not found — still run transforms (e.g. default) with nil
			val = nil
		}

		// Apply transforms in order
		for _, seg := range segments[1:] {
			transform := strings.TrimSpace(seg)
			var err error
			val, err = ApplyTransform(val, transform)
			if err != nil {
				return ""
			}
		}

		if val == nil {
			return ""
		}

		switch v := val.(type) {
		case string:
			return v
		case float64:
			if v == float64(int64(v)) {
				return strconv.FormatInt(int64(v), 10)
			}
			return strconv.FormatFloat(v, 'f', -1, 64)
		default:
			return fmt.Sprintf("%v", v)
		}
	})
}
