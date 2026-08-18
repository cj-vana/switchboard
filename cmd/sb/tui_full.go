package main

import tea "github.com/charmbracelet/bubbletea"

// fullscreen is a panel that temporarily owns the whole terminal. It gets
// first claim on input and rendering while open; returning close from key
// hands control back to the transcript. A key may also return a command so a
// panel can load or apply work without blocking Bubble Tea's update loop.
type fullscreen interface {
	key(tea.KeyMsg) (close bool, cmd tea.Cmd)
	mouse(tea.MouseMsg)
	view(width, height int, th *theme) string
}
