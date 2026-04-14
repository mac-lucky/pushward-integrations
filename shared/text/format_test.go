package text

import "testing"

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		name string
		in   int64
		want string
	}{
		{"zero", 0, "0 B"},
		{"single byte", 1, "1 B"},
		{"just under kb", 1023, "1023 B"},
		{"exactly 1 kb", 1024, "1.0 KB"},
		{"kilobytes", 1536, "1.5 KB"},
		{"just under mb", 1024*1024 - 1, "1024.0 KB"},
		{"exactly 1 mb", 1024 * 1024, "1.0 MB"},
		{"megabytes", 750 * 1024 * 1024, "750.0 MB"},
		{"gigabytes", 1024 * 1024 * 1024, "1.0 GB"},
		{"gigabytes fractional", 1024*1024*1024 + 512*1024*1024, "1.5 GB"},
		{"terabytes", 1024 * 1024 * 1024 * 1024, "1.0 TB"},
		{"terabytes large", 5 * 1024 * 1024 * 1024 * 1024, "5.0 TB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatBytes(tt.in)
			if got != tt.want {
				t.Errorf("FormatBytes(%d) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
