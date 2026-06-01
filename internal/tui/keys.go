package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap содержит все биндинги хоткеев.
type keyMap struct {
	Quit     key.Binding
	Refresh  key.Binding
	Help     key.Binding
	Models   key.Binding
	Tail     key.Binding
	Filter   key.Binding
	Tab      key.Binding
	Enter    key.Binding
	Esc      key.Binding
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Home     key.Binding
	End      key.Binding
}

var keys = keyMap{
	Quit: key.NewBinding(
		key.WithKeys("q", "f10"),
		key.WithHelp("q/F10", "quit"),
	),
	Refresh: key.NewBinding(
		key.WithKeys("f5"),
		key.WithHelp("F5", "refresh"),
	),
	Help: key.NewBinding(
		key.WithKeys("f1"),
		key.WithHelp("F1", "help"),
	),
	Models: key.NewBinding(
		key.WithKeys("m"),
		key.WithHelp("m", "models on selected server"),
	),
	Tail: key.NewBinding(
		key.WithKeys("t"),
		key.WithHelp("t", "toggle generation tail (streaming)"),
	),
	Filter: key.NewBinding(
		key.WithKeys("/"),
		key.WithHelp("/", "filter"),
	),
	Tab: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("Tab", "switch pane"),
	),
	Enter: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("Enter", "details"),
	),
	Esc: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("Esc", "close/cancel"),
	),
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "down"),
	),
	PageUp: key.NewBinding(
		key.WithKeys("pgup"),
		key.WithHelp("PgUp", "page up"),
	),
	PageDown: key.NewBinding(
		key.WithKeys("pgdown"),
		key.WithHelp("PgDn", "page down"),
	),
	Home: key.NewBinding(
		key.WithKeys("home"),
		key.WithHelp("Home", "top"),
	),
	End: key.NewBinding(
		key.WithKeys("end"),
		key.WithHelp("End", "bottom"),
	),
}
