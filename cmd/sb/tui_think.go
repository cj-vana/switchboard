package main

// /think: reasoning effort for the model that is running, changed without
// leaving the session. The change is session-scoped by design — a binding
// that should survive a restart is made in /models, where effort is part of
// the rung — and it is visible twice the moment it lands: the target ID
// grows a +think suffix and the status bar says so.

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/provider"
)

var thinkLevels = []pickerItem{
	{id: "default", label: "default", desc: "let the provider decide"},
	{id: "low", label: "low"},
	{id: "medium", label: "medium"},
	{id: "high", label: "high"},
	{id: "max", label: "max"},
}

func cmdThink(m *tuiModel, args string) tea.Cmd {
	if args != "" {
		return m.applyThink(args)
	}
	items := make([]pickerItem, len(thinkLevels))
	copy(items, thinkLevels)
	current := effortOf(m.app.tier.Target)
	for i := range items {
		items[i].current = items[i].id == current || (current == "" && items[i].id == "default")
	}
	m.dlg = &pickerDialog{
		title:  "reasoning effort for " + string(m.app.tier.Target.ID()),
		items:  items,
		onPick: func(level string) tea.Cmd { return m.applyThink(level) },
	}
	return nil
}

func (m *tuiModel) applyThink(level string) tea.Cmd {
	switch level {
	case "default", "off":
		level = ""
	case "low", "medium", "high", "max":
	default:
		return noticeCmd("error", "effort is low, medium, high, max, or default")
	}

	tier := m.app.tier
	if level == "" {
		tier.Target.Params.Reasoning = nil
	} else {
		tier.Target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: level}
	}

	// Same client, new inference parameters. bind rebuilds the cache
	// controller too, which matters: effort changes cache identity, and a
	// tracker carried across that boundary would attribute one prefix's
	// cache to another (§6).
	m.app.bind(tier, m.app.loop.Provider, true)
	m.tierLine = m.app.tierLine()
	m.refreshCtxWindow()

	if level == "" {
		return noticeCmd("", "reasoning effort is the provider's default for this session; a target that cannot reason will say so on the next turn")
	}
	return noticeCmd("", "reasoning effort is "+level+" for this session; /models rebinds a rung if it should persist")
}
