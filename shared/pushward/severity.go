package pushward

const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

// SeverityColor returns the accent color for a Grafana alert severity level.
func SeverityColor(severity string) string {
	switch severity {
	case "critical":
		return ColorRed
	case "warning":
		return ColorOrange
	case "info":
		return ColorBlue
	default:
		return ColorOrange
	}
}

// SeverityIcon returns the SF Symbol icon name for a Grafana alert severity level.
// The defaultIcon is used for the "warning" and unknown severity levels.
func SeverityIcon(severity, defaultIcon string) string {
	switch severity {
	case "critical":
		return "exclamationmark.octagon.fill"
	case "info":
		return "info.circle.fill"
	default:
		return defaultIcon
	}
}
