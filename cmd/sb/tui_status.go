package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// statusLine is the always-on readout §14 requires, drawn as one continuous
// surface across the bottom: routing visible at rest. Left to right — the
// active rung's chip and target, then the session's routing history as one
// dot per landed move, the ladder strip (every rung as a block in its heat
// color, the active one raised), the streaming sparkline while a turn runs,
// mode, effort, spend, context, and the clock. When the terminal narrows,
// the newest luxuries leave first: sparkline, clock, effort, dots.
//
//	▌t3 kimi▐ kimi/coding/kimi-for-coding-highspeed  ·· ▂▂█▂ ▁▃▆▅ ~42 tok/s acceptEdits plan ctx 34% 12:34
func (m *tuiModel) statusLine() string {
	th := m.th
	width := m.width
	rank := m.activeRank()

	chip := m.app.tier.ID
	if m.app.tier.Label != "" {
		chip += " · " + m.app.tier.Label
	}
	chipStyle := th.tierChip
	if rank >= 0 {
		chipStyle = th.rungChip(rank)
	}

	target := ""
	if i := strings.Index(m.tierLine, "  "); i >= 0 {
		target = strings.TrimSpace(m.tierLine[i:])
	}

	// Right-side segments, in display order. Optional ones carry a drop
	// priority: when the bar does not fit, the highest number leaves first.
	type segment struct {
		s    string
		drop int // 0 never leaves
	}
	var segs []segment
	add := func(s string, drop int) {
		if s != "" {
			segs = append(segs, segment{s: s, drop: drop})
		}
	}
	add(m.moveDots(), 2)
	add(m.ladderStrip(), 0)
	add(m.sparkline(), 4)
	// One filled chip on the bar reads as deliberate; three read as a
	// toolbar. The rung chip keeps its fill; mode is its hue as text — except
	// default, whose chip ground is the neutral gray that vanishes as a
	// foreground, so the quiet mode speaks in the quiet color.
	if chipS, ok := th.modeChip[string(m.mode)]; ok && m.mode != "default" {
		modeStyle := lipgloss.NewStyle().Foreground(chipS.GetBackground())
		add(th.onBar(modeStyle).Render(string(m.mode)), 0)
	} else {
		add(th.onBar(th.dim).Render(string(m.mode)), 0)
	}
	if effort := effortOf(m.app.tier.Target); effort != "" {
		add(th.onBar(th.dim).Render("think "+effort), 1)
	}
	add(m.watchChip(), 0)
	if m.updateAvail != "" {
		add(th.onBar(th.warn).Render("↑ "+m.updateAvail), 0)
	}
	costStyle := th.ok
	switch {
	case m.costPct >= 85:
		costStyle = th.err
	case m.costPct >= 60:
		costStyle = th.warn
	}
	add(th.onBar(costStyle).Render(m.costLine), 0)
	add(m.ctxPct(), 0)
	add(m.clock(), 3)
	if m.tr.offset > 0 {
		add(th.onBar(th.dim).Render(fmt.Sprintf("↑%d", m.tr.offset)), 0)
	}

	sep := th.onBar(lipgloss.NewStyle()).Render("  ")
	rightWidth := func() int {
		w := lipgloss.Width(sep)
		for _, s := range segs {
			w += lipgloss.Width(s.s) + lipgloss.Width(sep)
		}
		return w
	}
	makeLeft := func() string {
		return chipStyle.Render(" "+chip+" ") + th.onBar(th.dim).Render(" "+target)
	}
	left := makeLeft()

	// Fit: the target shrinks first — it is the longest thing here and the
	// chip already names the rung — down to a floor that still identifies
	// the model. Only then do the luxuries leave, newest first.
	chipW := lipgloss.Width(chipStyle.Render(" " + chip + " "))
	if avail := width - rightWidth() - chipW - 3; avail < len(target) {
		target = truncate(target, max(avail, 14))
		left = makeLeft()
	}
	for drop := 4; drop >= 1; drop-- {
		if lipgloss.Width(left)+rightWidth() <= width {
			break
		}
		kept := segs[:0]
		for _, s := range segs {
			if s.drop != drop {
				kept = append(kept, s)
			}
		}
		segs = kept
	}
	if avail := width - rightWidth() - chipW - 3; avail < len(target) {
		target = truncate(target, max(avail, 8))
		left = makeLeft()
	}

	var right []string
	for _, s := range segs {
		right = append(right, s.s)
	}
	rightStr := strings.Join(right, sep) + th.onBar(lipgloss.NewStyle()).Render(" ")
	gap := width - lipgloss.Width(left) - lipgloss.Width(rightStr)
	if gap < 1 {
		gap = 1
	}
	return left + th.onBar(lipgloss.NewStyle()).Render(strings.Repeat(" ", gap)) + rightStr
}

// moveDots is the session's routing history at a glance: one dot per landed
// switch, each in the rung it landed on, newest last. The dots have to agree
// with /why about how much the session moved; both are fed by every rebind.
func (m *tuiModel) moveDots() string {
	if len(m.moves) == 0 {
		return ""
	}
	moves := m.moves
	if len(moves) > 8 {
		moves = moves[len(moves)-8:]
	}
	var b strings.Builder
	for _, rank := range moves {
		b.WriteString(m.th.onBar(m.th.rung(rank)).Render("•"))
	}
	return b.String()
}

// ladderStrip draws the whole ladder as one block per rung in its heat
// color, the active rung raised to a full block: position and depth in four
// cells. Each block is state, not decoration — its color is a rung, its
// height is whether work runs there now.
func (m *tuiModel) ladderStrip() string {
	tiers := m.app.config.Tiers
	if len(tiers) < 2 || len(tiers) > 8 {
		return ""
	}
	rank := m.activeRank()
	var b strings.Builder
	for i := range tiers {
		block := "▂"
		if i == rank {
			block = "█"
		}
		b.WriteString(m.th.onBar(m.th.rung(i)).Render(block))
	}
	return b.String()
}

// sparkline is the stream's pulse while a turn runs: recent tokens-per-second
// samples as a tiny bar chart in the active rung's heat, with the newest
// estimate spelled out. The ~ is honest — the rate is chars over four, not a
// count the provider reported.
func (m *tuiModel) sparkline() string {
	if !m.busy || len(m.samples) == 0 {
		return ""
	}
	peak := 1.0
	for _, s := range m.samples {
		if s > peak {
			peak = s
		}
	}
	ramp := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, s := range m.samples {
		i := int(s / peak * float64(len(ramp)-1))
		b.WriteRune(ramp[max(i, 0)])
	}
	style := m.th.dim
	if rank := m.activeRank(); rank >= 0 {
		style = m.th.rung(rank)
	}
	last := m.samples[len(m.samples)-1]
	return m.th.onBar(style).Render(b.String()) +
		m.th.onBar(m.th.dim).Render(fmt.Sprintf(" ~%.0f tok/s", last))
}

// ctxPct is the context occupancy as text, colored by how close it is to the
// wall: fine, warm, and about to be a problem. The rail above the bar draws
// the same number as a line.
func (m *tuiModel) ctxPct() string {
	pct, ok := m.ctxPercent()
	if !ok {
		// An unknown window is not the same as an empty one, and it is the
		// state that matters most: auto-compaction cannot fire against a
		// window nobody has stated, so a session on this target runs until
		// the server refuses. Saying nothing here is what made that silent.
		if m.ctxWindow <= 0 && m.app.config.CompactAuto {
			return m.th.onBar(m.th.faint).Render("ctx ") +
				m.th.onBar(m.th.warn).Render("?")
		}
		return ""
	}
	style := m.th.accent
	switch {
	case pct >= 85:
		style = m.th.err
	case pct >= 60:
		style = m.th.warn
	}
	// A tilde where the provider reported nothing: the number is this
	// build's own count, and the estimator is measured to run low.
	shown := fmt.Sprintf("%d%%", pct)
	if m.callEstimated {
		shown = "~" + shown
	}
	return m.th.onBar(m.th.faint).Render("ctx ") + m.th.onBar(style).Render(shown)
}

func (m *tuiModel) ctxPercent() (int, bool) {
	if m.ctxWindow <= 0 || m.callTokens <= 0 {
		return 0, false
	}
	pct := m.callTokens * 100 / m.ctxWindow
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

// ctxRail is the thin line riding the top edge of the status bar: the
// context window's fill drawn across the whole width in the active rung's
// heat. The same fact as the ctx percentage, shaped so a glance while
// scrolled into work still catches it.
func (m *tuiModel) ctxRail() string {
	width := max(m.width, 1)
	pct, _ := m.ctxPercent()
	fill := width * pct / 100
	style := m.th.barFill
	if rank := m.activeRank(); rank >= 0 {
		style = m.th.rung(rank)
	}
	switch {
	case pct >= 85:
		style = m.th.err
	case pct >= 60:
		style = m.th.warn
	}
	return style.Render(strings.Repeat("▁", fill)) +
		m.th.barEmpty.Render(strings.Repeat("▁", width-fill))
}

// clock is how long this session has been open, mm:ss and then h:mm:ss: the
// quiet fact that anchors the day the way a wall clock does.
func (m *tuiModel) clock() string {
	if m.sessionAt.IsZero() {
		return ""
	}
	d := time.Since(m.sessionAt).Round(time.Second)
	h, rem := d/time.Hour, d%time.Hour
	mins, secs := rem/time.Minute, (rem%time.Minute)/time.Second
	text := fmt.Sprintf("%d:%02d", mins, secs)
	if h > 0 {
		text = fmt.Sprintf("%d:%02d:%02d", h, mins, secs)
	}
	return m.th.onBar(m.th.faint).Render(text)
}

// effortOf reports the reasoning effort riding on a target, or "".
func effortOf(t provider.RouteTarget) string {
	if t.Params.Reasoning == nil || !t.Params.Reasoning.Enabled {
		return ""
	}
	return t.Params.Reasoning.Effort
}
