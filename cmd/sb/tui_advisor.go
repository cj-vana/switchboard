package main

// /advisor: a second model watching the session, wired per §9.2 — advice,
// never edits, bounded per turn. Advice renders in the transcript the moment
// it arrives and reaches the model at the next safe seam: mid-turn through
// the loop's injection point, or folded into the next prompt if the turn
// already ended.

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/advisor"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/provider"
)

type adviceMsg struct{ text string }

type advisorReadyMsg struct {
	adv  *advisor.Advisor
	tier config.Tier
	err  error
}

const advisorUsage = "usage: /advisor [on|off|status]"

func cmdAdvisor(m *tuiModel, args string) tea.Cmd {
	switch strings.TrimSpace(args) {
	case "", "status":
		if m.app.advisor == nil {
			return noticeCmd("", "advisor is off; /advisor on binds "+describeAdvisorChoice(m.app)+"")
		}
		return noticeCmd("", "advisor is on, using "+string(m.app.advisor.Target().ID()))
	case "on":
		if m.app.advisor != nil {
			return noticeCmd("", "advisor is already on, using "+string(m.app.advisor.Target().ID()))
		}
		return startAdvisor(m.app)
	case "off":
		if m.app.advisor == nil {
			return noticeCmd("", "advisor is already off")
		}
		m.app.advisor = nil
		m.app.loop.Observer = m.app.watcher
		return noticeCmd("", "advisor is off")
	default:
		return noticeCmd("error", advisorUsage)
	}
}

// advisorTier resolves which model advises. The slots table wins, because
// "the advisor is a slot" is the configuration model this whole tool is
// built on; absent a binding, §9.2's default applies: one rung above the
// primary, or the top rung when the primary is already there.
func advisorTier(app *tuiApp) (config.Tier, error) {
	if ref, ok := app.config.Slots["advisor"]; ok {
		if t, found := app.config.Tier(ref); found {
			return t, nil
		}
		target, err := config.ParseTarget(ref, "", "")
		if err != nil {
			return config.Tier{}, err
		}
		return config.Tier{ID: "-advisor", Label: "advisor", Target: target}, nil
	}

	tiers := app.config.Tiers
	rank := app.rankOf(app.tier)
	if rank < 0 || len(tiers) == 0 {
		return app.tier, nil
	}
	if rank+1 < len(tiers) {
		return tiers[rank+1], nil
	}
	return tiers[rank], nil
}

func describeAdvisorChoice(app *tuiApp) string {
	t, err := advisorTier(app)
	if err != nil {
		return "the [slots] advisor entry, which does not parse: " + err.Error()
	}
	return string(t.Target.ID())
}

func startAdvisor(app *tuiApp) tea.Cmd {
	tier, err := advisorTier(app)
	if err != nil {
		return noticeCmd("error", err.Error())
	}
	return func() tea.Msg {
		probed, client, err := app.providers.probeTier(context.Background(), tier)
		if err != nil {
			return advisorReadyMsg{err: err}
		}
		adv := advisor.New(app.watcher, client, probed.Target, func(text string) {
			app.p.Send(adviceMsg{text: text})
		})
		return advisorReadyMsg{adv: adv, tier: probed}
	}
}

func (m *tuiModel) onAdvisorReady(msg advisorReadyMsg) {
	if msg.err != nil {
		m.addNotice("error", "advisor could not start: "+msg.err.Error())
		return
	}
	m.app.advisor = msg.adv
	m.app.loop.Observer = msg.adv
	m.addNotice("advisor", "watching this session with "+string(msg.adv.Target().ID())+
		"; advice appears here and is passed to the model")
}

// adviceContext folds advice that arrived after a turn ended into the next
// prompt, the same seam the ! output uses and for the same reason: one user
// message per turn.
func (m *tuiModel) adviceContext(prompt string) string {
	if m.app.advisor == nil {
		return prompt
	}
	msgs := m.app.advisor.Drain()
	if len(msgs) == 0 {
		return prompt
	}
	var b strings.Builder
	for _, msg := range msgs {
		for _, block := range msg.Content {
			if t, ok := block.(provider.Text); ok {
				b.WriteString(t.Text + "\n\n")
			}
		}
	}
	b.WriteString(prompt)
	return b.String()
}
