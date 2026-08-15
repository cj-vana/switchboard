package main

// First-run setup. An empty ladder on an interactive terminal is not an
// error, it is the beginning: instead of printing TOML to copy into a file,
// the TUI walks the same picker components /models and /login use — find a
// model, take a key if that model needs one, bind t1 — and then the ordinary
// startup proceeds against the config this just wrote.

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/credential"
)

// errSetupCancelled distinguishes "the user backed out" from a setup that
// failed; the caller turns it into guidance rather than a stack of context.
var errSetupCancelled = errors.New(
	"setup cancelled; run sb again to retry, or write a ladder into the config by hand")

// runOnboarding runs the wizard as its own inline Bubble Tea program, so the
// few lines it prints stay in the scrollback as a record of what was set up.
func runOnboarding(reg *providers, cat *catalog.Catalog, cfg *config.Config) error {
	m := &onboardModel{reg: reg, cat: cat, cfg: cfg, th: themeFor(detectDark())}
	if _, err := tea.NewProgram(m).Run(); err != nil {
		return err
	}
	if m.cancelled {
		return errSetupCancelled
	}
	return m.err
}

type onboardChoicesMsg struct {
	items   []pickerItem
	choices map[string]modelChoice
}
type onboardPickedMsg struct{ id string }
type onboardKeyStoredMsg struct{ note string }
type onboardBoundMsg struct{ err error }

type onboardModel struct {
	reg *providers
	cat *catalog.Catalog
	cfg *config.Config
	th  *theme

	dlg    dialog
	lines  []string
	choice modelChoice

	cancelled bool
	quitting  bool
	err       error
}

func (m *onboardModel) Init() tea.Cmd {
	reg, cat := m.reg, m.cat
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		items, choices := gatherModelChoices(ctx, reg, cat)
		return onboardChoicesMsg{items: items, choices: choices}
	}
}

func (m *onboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case onboardChoicesMsg:
		if len(msg.items) == 0 {
			m.err = errors.New("no models to offer: the Ollama server did not answer and the catalog is empty.\n" +
				"Start Ollama and `ollama pull` a model, then run sb again")
			m.quitting = true
			return m, tea.Quit
		}
		choices := msg.choices
		m.dlg = &pickerDialog{
			title: "switchboard setup — pick the model t1 starts on",
			items: msg.items,
			onPick: func(id string) tea.Cmd {
				m.choice = choices[id]
				return func() tea.Msg { return onboardPickedMsg{id: id} }
			},
		}
		return m, nil

	case onboardPickedMsg:
		return m, m.afterPick()

	case secretPromptMsg:
		m.dlg = newSecretDialog(msg.ref, msg.storeName, func(value string) tea.Cmd {
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := msg.writer.Set(ctx, msg.ref, value); err != nil {
					return onboardKeyStoredMsg{note: "storing the key failed: " + err.Error()}
				}
				return onboardKeyStoredMsg{note: "stored " + msg.ref.String() + " in the " + msg.storeName}
			}
		})
		return m, nil

	case onboardKeyStoredMsg:
		m.lines = append(m.lines, msg.note)
		return m, m.effortOrBind()

	case pickerMsg:
		m.dlg = &pickerDialog{title: msg.title, items: msg.items, onPick: msg.action}
		return m, nil

	case onboardBoundMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.lines = append(m.lines,
				"t1 now runs "+m.choice.ref,
				"saved to "+m.cfg.Path,
				"add more rungs any time with /models")
		}
		m.quitting = true
		return m, tea.Quit

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.cancelled = true
			m.quitting = true
			return m, tea.Quit
		}
		if m.dlg != nil {
			done, cmd := m.dlg.update(msg, m.th)
			if done {
				m.dlg = nil
				if cmd == nil {
					// Every dialog here resolves to a command; a bare close
					// is the user backing out.
					m.cancelled = true
					m.quitting = true
					return m, tea.Quit
				}
			}
			return m, cmd
		}
	}
	return m, nil
}

// afterPick decides whether the chosen model needs a credential before it can
// answer. Ollama never does; anything else is asked for only when the chain
// finds nothing, because a key in the environment already works.
func (m *onboardModel) afterPick() tea.Cmd {
	choice, cfg := m.choice, m.cfg
	if strings.HasPrefix(choice.ref, "ollama/") {
		return m.effortOrBind()
	}
	providerName, _, _ := strings.Cut(choice.ref, "/")
	ref := credential.Ref{Provider: providerName, Account: choice.surface}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := credential.Chain(cfg.AuthFor(providerName)).Get(ctx, ref); err == nil {
			return onboardKeyStoredMsg{note: ref.String() + " already has a credential"}
		}
		store := credential.NewOSStore()
		writer, ok := any(store).(credential.Writer)
		if !ok {
			return onboardKeyStoredMsg{note: "no OS credential store here; set " +
				credential.EnvNames(ref)[0] + " before the first request"}
		}
		return secretPromptMsg{ref: ref, writer: writer, storeName: store.Name()}
	}
}

func (m *onboardModel) effortOrBind() tea.Cmd {
	choice := m.choice
	if len(choice.effortLevels) == 0 {
		return m.bind("")
	}
	return func() tea.Msg {
		items := []pickerItem{{id: "", label: "default", desc: "let the provider decide"}}
		for _, level := range choice.effortLevels {
			items = append(items, pickerItem{id: level, label: level})
		}
		return pickerMsg{
			title:  "reasoning effort for " + choice.ref,
			items:  items,
			action: func(effort string) tea.Cmd { return m.bind(effort) },
		}
	}
}

func (m *onboardModel) bind(effort string) tea.Cmd {
	choice, cfg := m.choice, m.cfg
	return func() tea.Msg {
		if err := cfg.BindTier("t1", "", choice.ref, choice.surface, effort); err != nil {
			return onboardBoundMsg{err: err}
		}
		return onboardBoundMsg{err: cfg.Save()}
	}
}

func (m *onboardModel) View() string {
	if m.quitting {
		var b strings.Builder
		for _, l := range m.lines {
			b.WriteString(l + "\n")
		}
		return b.String()
	}

	var b strings.Builder
	b.WriteString(m.th.bold.Render("switchboard") + m.th.dim.Render("  first run — nothing is configured yet") + "\n\n")
	for _, l := range m.lines {
		b.WriteString(m.th.dim.Render("  "+l) + "\n")
	}
	if m.dlg != nil {
		b.WriteString(m.dlg.view(76, m.th))
	} else {
		b.WriteString(m.th.dim.Render("  looking for models…"))
	}
	return b.String()
}
