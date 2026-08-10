package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestFormatBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input uint64
		want  string
	}{
		{"zero", 0, "0 B"},
		{"bytes", 512, "512 B"},
		{"kilobytes", 1024, "1.0 KB"},
		{"kilobytes fractional", 1536, "1.5 KB"},
		{"megabytes", 1024 * 1024, "1.0 MB"},
		{"gigabytes", 2 * 1024 * 1024 * 1024, "2.0 GB"},
		{"terabytes", 2 * 1024 * 1024 * 1024 * 1024, "2.0 TB"},
		{"terabytes fractional", uint64(1.5 * 1024 * 1024 * 1024 * 1024), "1.5 TB"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatBytes(tc.input)
			if got != tc.want {
				t.Errorf("formatBytes(%d) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		seconds uint64
		want    string
	}{
		{"zero minutes", 0, "0m"},
		{"minutes only", 300, "5m"},
		{"hours and minutes", 3661, "1h 1m"},
		{"days hours minutes", 30*86400 + 5*3600 + 42*60, "30d 5h 42m"},
		{"one day", 86400, "1d 0h 0m"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatDuration(tc.seconds)
			if got != tc.want {
				t.Errorf("formatDuration(%d) = %q, want %q", tc.seconds, got, tc.want)
			}
		})
	}
}

func TestFormatPercent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input float64
		want  string
	}{
		{"zero", 0.0, "0.0%"},
		{"whole number", 47.0, "47.0%"},
		{"one decimal", 47.1, "47.1%"},
		{"rounds down", 47.14, "47.1%"},
		{"rounds up", 47.25, "47.2%"},
		{"100 percent", 100.0, "100.0%"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatPercent(tc.input)
			if got != tc.want {
				t.Errorf("formatPercent(%f) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestFormatBar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		percent float64
		width   int
		want    string
	}{
		{"zero percent", 0, 10, "░░░░░░░░░░"},
		{"100 percent", 100, 10, "██████████"},
		{"50 percent", 50, 10, "█████░░░░░"},
		{"25 percent", 25, 20, "█████░░░░░░░░░░░░░░░"},
		{"clamp above 100", 150, 5, "█████"},
		{"clamp below 0", -10, 5, "░░░░░"},
		{"zero width", 50, 0, ""},
		{"20 wide 32 percent", 32, 20, "██████░░░░░░░░░░░░░░"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatBar(tc.percent, tc.width)
			if got != tc.want {
				t.Errorf("formatBar(%.1f, %d) = %q, want %q", tc.percent, tc.width, got, tc.want)
			}
		})
	}
}

func TestFormatHours(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		hours uint64
		want  string
	}{
		{"zero hours", 0, "0 hours"},
		{"less than a day", 12, "0 days"},
		{"exactly one day", 24, "1 days"},
		{"one year", 365 * 24, "1 years, 0 days"},
		{"one year and some days", 365*24 + 270*24, "1 years, 270 days"},
		{"29 years 270 days", (29*365 + 270) * 24, "29 years, 270 days"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatHours(tc.hours)
			if got != tc.want {
				t.Errorf("formatHours(%d) = %q, want %q", tc.hours, got, tc.want)
			}
		})
	}
}

func TestFormatDataUnits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		units uint64
		want  string
	}{
		{"zero units", 0, "0 B"},
		// 1 unit = 512,000 bytes = 500 KB
		{"one unit", 1, "500.0 KB"},
		// 2048 units = 1,048,576,000 bytes = 1000.0 MB (just under 1 GiB threshold)
		{"large units in MB range", 2048, "1000.0 MB"},
		// 4096 units = 2,097,152,000 bytes ≈ 1.95 GB
		{"large units in GB range", 4096, "2.0 GB"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatDataUnits(tc.units)
			if got != tc.want {
				t.Errorf("formatDataUnits(%d) = %q, want %q", tc.units, got, tc.want)
			}
		})
	}
}

func TestFormatTable(t *testing.T) {
	t.Parallel()

	t.Run("basic table with two rows", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		headers := []string{"Name", "Value"}
		rows := [][]string{
			{"foo", "123"},
			{"longerkey", "456"},
		}
		formatTable(&buf, headers, rows)
		output := buf.String()

		if !strings.Contains(output, "Name") {
			t.Error("output missing header 'Name'")
		}
		if !strings.Contains(output, "longerkey") {
			t.Error("output missing 'longerkey'")
		}
		if !strings.Contains(output, "456") {
			t.Error("output missing value '456'")
		}
	})

	t.Run("column widths align to widest value", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		headers := []string{"Col"}
		rows := [][]string{
			{"short"},
			{"a longer value"},
		}
		formatTable(&buf, headers, rows)
		lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
		// Each data line should be at least as wide as the longest value
		for _, line := range lines {
			if len(line) < len("a longer value") {
				t.Errorf("line too short: %q", line)
			}
		}
	})

	t.Run("empty rows produces only header", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		formatTable(&buf, []string{"H1", "H2"}, nil)
		output := buf.String()
		if !strings.Contains(output, "H1") || !strings.Contains(output, "H2") {
			t.Error("output missing headers for empty row table")
		}
	})
}

func TestFormatNumber(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input uint64
		want  string
	}{
		{"zero", 0, "0"},
		{"less than 1000", 999, "999"},
		{"exactly 1000", 1000, "1,000"},
		{"millions", 1234567, "1,234,567"},
		{"large number", 4567890, "4,567,890"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatNumber(tc.input)
			if got != tc.want {
				t.Errorf("formatNumber(%d) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
