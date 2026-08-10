// Package cli formats system monitoring data for terminal output.
package cli

import (
	"fmt"
	"io"
	"strings"
)

const (
	nvmeDataUnitBytes = 512_000
)

// formatBytes converts a byte count to a human-readable string.
func formatBytes(n uint64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
		tb = gb * 1024
	)
	switch {
	case n == 0:
		return "0 B"
	case n >= tb:
		return fmt.Sprintf("%.1f TB", float64(n)/float64(tb))
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// formatDuration converts a duration in seconds to a human-readable string.
func formatDuration(seconds uint64) string {
	const (
		minute = 60
		hour   = minute * 60
		day    = hour * 24
	)
	days := seconds / day
	hours := (seconds % day) / hour
	mins := (seconds % hour) / minute

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// formatPercent formats a float64 as a percentage string.
func formatPercent(v float64) string {
	return fmt.Sprintf("%.1f%%", v)
}

// formatBar renders an ASCII progress bar of the given width.
// filled blocks use '█', empty blocks use '░'.
func formatBar(percent float64, width int) string {
	if width <= 0 {
		return ""
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	filled := int(percent/100.0*float64(width) + 0.5)
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// formatHours converts hours to a human-readable years/days string.
func formatHours(hours uint64) string {
	const (
		hoursPerDay  = 24
		daysPerYear  = 365
		hoursPerYear = hoursPerDay * daysPerYear
	)
	if hours == 0 {
		return "0 hours"
	}
	years := hours / hoursPerYear
	days := (hours % hoursPerYear) / hoursPerDay
	if years > 0 {
		return fmt.Sprintf("%d years, %d days", years, days)
	}
	return fmt.Sprintf("%d days", days)
}

// formatDataUnits converts NVMe data units (each = 512,000 bytes) to a
// human-readable byte string.
func formatDataUnits(units uint64) string {
	return formatBytes(units * nvmeDataUnitBytes)
}

// formatTable writes a simple column-aligned table to w.
// Column widths are determined by the widest value in each column.
func formatTable(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	// header row
	for i, h := range headers {
		if i > 0 {
			fmt.Fprint(w, "  ")
		}
		fmt.Fprintf(w, "%-*s", widths[i], h)
	}
	fmt.Fprintln(w)

	// separator
	for i, w2 := range widths {
		if i > 0 {
			fmt.Fprint(w, "  ")
		}
		fmt.Fprint(w, strings.Repeat("-", w2))
	}
	fmt.Fprintln(w)

	// data rows
	for _, row := range rows {
		for i, cell := range row {
			if i >= len(widths) {
				break
			}
			if i > 0 {
				fmt.Fprint(w, "  ")
			}
			fmt.Fprintf(w, "%-*s", widths[i], cell)
		}
		fmt.Fprintln(w)
	}
}
