package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

// Palette.
var (
	colAccent = lipgloss.Color("#FF4500") // DayZ orange
	colDim    = lipgloss.Color("#555555")
	colGreen  = lipgloss.Color("#22C55E")
	colRed    = lipgloss.Color("#EF4444")
	colYellow = lipgloss.Color("#EAB308")
	colBlue   = lipgloss.Color("#3B82F6")
	colText   = lipgloss.Color("#E2E8F0")
)

// Styles.
var (
	titleStyle = lipgloss.NewStyle().
			Foreground(colAccent).
			Bold(true)

	subtitleStyle = lipgloss.NewStyle().
			Foreground(colText).
			Bold(true)

	dimStyle = lipgloss.NewStyle().Foreground(colDim)

	greenStyle  = lipgloss.NewStyle().Foreground(colGreen)
	redStyle    = lipgloss.NewStyle().Foreground(colRed)
	yellowStyle = lipgloss.NewStyle().Foreground(colYellow)
	blueStyle   = lipgloss.NewStyle().Foreground(colBlue)

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(colAccent).
			Foreground(colAccent)

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colDim).
			Padding(0, 1)

	errorBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colRed).
			Foreground(colRed).
			Padding(0, 2)

	helpStyle = lipgloss.NewStyle().Foreground(colDim)

	checkboxOn  = greenStyle.Render("[x]")
	checkboxOff = dimStyle.Render("[ ]")
)

// boolIcon returns a coloured check or cross.
func boolIcon(b bool) string {
	if b {
		return greenStyle.Render("✓")
	}
	return redStyle.Render("✗")
}

// timeEmoji returns ☀ for daytime hours, ☾ for night.
func timeEmoji(t string) string {
	if len(t) < 2 {
		return "?"
	}
	var hour int
	_, _ = fmt.Sscan(t[:2], &hour)
	if hour >= 6 && hour <= 18 {
		return "☀"
	}
	return "☾"
}

// envIcon returns the OS icon for a server's environment field.
func envIcon(env string) string {
	if env == "l" {
		return "🐧"
	}
	return "🪟"
}

// truncate shortens s to n runes, appending "…" if truncated.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

// formatBytes converts bytes to a human-readable string.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
