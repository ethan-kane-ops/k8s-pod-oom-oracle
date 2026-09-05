package tui

import "github.com/charmbracelet/lipgloss"

// Colours are declared as adaptive pairs so the dashboard is legible on a light
// terminal as well as a dark one. A node agent gets run over SSH from whatever
// terminal someone happens to have open.
var (
	colourAccent = lipgloss.AdaptiveColor{Light: "#5A3FD6", Dark: "#9D8CFF"}
	colourMuted  = lipgloss.AdaptiveColor{Light: "#6B6B78", Dark: "#8A8A99"}
	colourWarn   = lipgloss.AdaptiveColor{Light: "#B25000", Dark: "#FFA657"}
	colourAlert  = lipgloss.AdaptiveColor{Light: "#B3261E", Dark: "#FF7B72"}
	colourOK     = lipgloss.AdaptiveColor{Light: "#136F3B", Dark: "#7EE787"}
)

var (
	styleTitle = lipgloss.NewStyle().Bold(true).Foreground(colourAccent)
	styleMuted = lipgloss.NewStyle().Foreground(colourMuted)
	styleWarn  = lipgloss.NewStyle().Foreground(colourWarn)
	styleAlert = lipgloss.NewStyle().Foreground(colourAlert).Bold(true)
	styleOK    = lipgloss.NewStyle().Foreground(colourOK)

	// styleSelected marks the highlighted row. It sets a foreground and a bar
	// rather than a background: a background block is the first thing to render
	// wrongly over SSH, in tmux, or through a terminal with a partial palette.
	styleSelected = lipgloss.NewStyle().Bold(true).Foreground(colourAccent)

	stylePane = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colourMuted).
			Padding(0, 1)

	stylePaneFocused = stylePane.BorderForeground(colourAccent)
)
