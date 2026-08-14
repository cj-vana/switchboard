package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// statusLine is the always-on readout §14 requires: routing visible at rest —
// tier, target, mode, cost, and how full the context window is — plus whatever
// is transiently true (an update, a scrolled-back viewport).
//
// Left:  ▍t2 light▍ ollama/local/qwen3.5:9b-mlx
// Right: ↑ v0.3.0 · bypass · $0.0123 · ctx ▓▓▓▓░░░░░░ 34%
func (m *tuiModel) statusLine() string {
	th := m.th
	width := m.width

	tierID, tierLabel, target := m.app.tier.ID, m.app.tier.Label, ""
	if i := strings.Index(m.tierLine, "  "); i >= 0 {
		target = strings.TrimSpace(m.tierLine[i:])
	}
	chip := tierID
	if tierLabel != "" {
		chip += " " + tierLabel
	}
	left := th.tierChip.Render(" "+chip+" ") + " " + th.dim.Render(target)

	var right []string
	if m.updateAvail != "" {
		right = append(right, th.warn.Render("↑ "+m.updateAvail))
	}
	if chip, ok := th.modeChip[string(m.mode)]; ok {
		right = append(right, chip.Render(" "+string(m.mode)+" "))
	} else {
		right = append(right, th.dim.Render(string(m.mode)))
	}
	right = append(right, th.ok.Render(m.costLine))
	if g := m.ctxGauge(); g != "" {
		right = append(right, g)
	}
	if m.tr.offset > 0 {
		right = append(right, th.faint.Render(fmt.Sprintf("↑ %d lines", m.tr.offset)))
	}

	leftW := lipgloss.Width(left)
	rightStr := strings.Join(right, th.faint.Render(" · "))
	rightW := lipgloss.Width(rightStr)

	gap := width - leftW - rightW - 1
	if gap < 1 {
		// Shrink from the left: the target name is the longest thing here.
		target = truncate(target, max(8, width-rightW-12))
		left = th.tierChip.Render(" "+chip+" ") + " " + th.dim.Render(target)
		leftW = lipgloss.Width(left)
		gap = width - leftW - rightW - 1
		if gap < 1 {
			gap = 1
		}
	}
	return left + strings.Repeat(" ", gap) + rightStr
}

// ctxGauge draws the context-window fill as a ten-cell bar, colored by how
// close it is to the wall: fine, warm, and about to be a problem.
func (m *tuiModel) ctxGauge() string {
	if m.ctxWindow <= 0 || m.callTokens <= 0 {
		return ""
	}
	pct := m.callTokens * 100 / m.ctxWindow
	if pct > 100 {
		pct = 100
	}
	fill := pct / 10
	bar := strings.Repeat("▓", fill) + strings.Repeat("░", 10-fill)

	style := m.th.barFill
	switch {
	case pct >= 85:
		style = m.th.err
	case pct >= 60:
		style = m.th.warn
	}
	return m.th.dim.Render("ctx ") + style.Render(bar) + m.th.dim.Render(fmt.Sprintf(" %d%%", pct))
}
