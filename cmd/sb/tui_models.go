package main

// /models: discover what can run and bind it to the ladder, without leaving
// the TUI. Discovery has two sources with different freshness: the local
// Ollama server answers for what is pulled right now, and the catalog answers
// for what a key would unlock. Binding goes through config.BindTier and Save,
// so what this writes is exactly what the next launch loads.

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider/openaicompat"
)

// modelChoice is one bindable target, or one surface to open. The ref/surface
// split mirrors what BindTier validates, and effortLevels comes from the
// catalog because only it knows whether a level is a real request parameter or
// a string the adapter would reject.
type modelChoice struct {
	ref          string // provider/model; empty while the model is still unknown
	provider     string
	surface      string
	desc         string
	effortLevels []string

	// browse marks a surface rather than a model. The catalog knows a
	// surface exists and what it costs; only its server knows which models
	// it serves today, so the pick opens a question rather than answering
	// one.
	browse bool
}

const removeRungID = "\x00remove"

// gatherModelChoices assembles everything bindable: live local models, then
// the catalog's priced entries, then every serving surface the catalog knows
// the mechanics of. The third group is the one that makes the ladder
// buildable at all for plan-metered and compatible endpoints, where the models
// are the account's rather than the catalog's and cannot be enumerated here.
// Shared by /models and first-run setup, which are the same question asked at
// different moments.
func gatherModelChoices(ctx context.Context, reg *providers, cat *catalog.Catalog, cfg *config.Config) ([]pickerItem, map[string]modelChoice) {
	choices := map[string]modelChoice{}
	var items []pickerItem
	add := func(id string, c modelChoice, label string) {
		if _, dup := choices[id]; dup {
			return
		}
		choices[id] = c
		items = append(items, pickerItem{id: id, label: label, desc: c.desc})
	}

	local, localErr := reg.localServer().Models(ctx)
	if localErr == nil {
		sort.Strings(local)
		for _, name := range local {
			c := modelChoice{ref: "ollama/" + name, provider: "ollama", surface: "local", desc: "pulled locally"}
			add(c.ref+" "+c.surface, c, c.ref)
		}
	}
	for _, info := range cat.Entries() {
		c := modelChoice{
			ref:          info.Provider + "/" + info.ProviderModelID,
			provider:     info.Provider,
			surface:      info.Surface,
			desc:         catalogDesc(info),
			effortLevels: info.EffortLevels,
		}
		add(c.ref+" "+c.surface, c, c.ref)
	}
	for _, c := range browsableSurfaces(cat, cfg, localErr) {
		add(browsePrefix+c.provider+"/"+c.surface, c, c.provider+"/"+c.surface+"…")
	}
	return items, choices
}

// browsableSurfaces is every surface worth opening, in the order a first run
// should read them: the local server, then what the catalog describes, then
// the uncharacterized compatible endpoint that stands for everything else.
func browsableSurfaces(cat *catalog.Catalog, cfg *config.Config, localErr error) []modelChoice {
	var out []modelChoice
	seen := map[string]bool{}
	add := func(c modelChoice) {
		key := c.provider + "/" + c.surface
		if seen[key] {
			return
		}
		seen[key] = true
		c.browse = true
		out = append(out, c)
	}

	if localErr != nil {
		// The list above is empty because nothing answered. The row that
		// says so is also the row that fixes it, since a server on another
		// host is the ordinary reason.
		add(modelChoice{
			provider: "ollama", surface: "local",
			desc: "server not answering; set its address or start it",
		})
	}
	for _, info := range cat.Surfaces() {
		add(modelChoice{
			provider:     info.Provider,
			surface:      info.Surface,
			desc:         catalogDesc(info) + " · ask this server what it serves",
			effortLevels: info.EffortLevels,
		})
	}
	// Not in the catalog, and it cannot be: the generic profile is the floor
	// of assumed capability for a server nobody has characterized, so there is
	// no price sheet to publish for it and pretending otherwise would price
	// every request at zero.
	add(modelChoice{
		provider: openaicompat.Name, surface: genericCompat,
		desc: "any OpenAI-compatible server · " + orNone(cfg.ProviderForTarget(openaicompat.Name, genericCompat).BaseURL),
	})
	return out
}

func cmdModels(m *tuiModel, args string) tea.Cmd {
	reg, cat, cfg := m.app.providers, m.app.catalog, m.app.config
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		items, choices := gatherModelChoices(ctx, reg, cat, cfg)
		if len(items) == 0 {
			// The compatible-endpoint row is unconditional, so this is
			// unreachable rather than a state worth diagnosing. It stays as
			// the guard that keeps an empty picker from ever being drawn.
			return noticeMsg{level: "error", text: "nothing to offer; bind a rung by hand in " + cfg.Path}
		}
		if len(cfg.Tiers) > 0 {
			items = append(items, pickerItem{id: removeRungID, label: "remove a rung…", desc: "drop a tier from the ladder"})
		}

		return pickerMsg{
			title: "bind a model to a tier",
			items: items,
			action: func(id string) tea.Cmd {
				if id == removeRungID {
					return removeRungCmd(m)
				}
				choice := choices[id]
				if choice.browse {
					return browseSurfaceCmd(reg, cfg, choice,
						func(c modelChoice) tea.Cmd { return chooseTierCmd(m, c) })
				}
				return chooseTierCmd(m, choice)
			},
		}
	}
}

// catalogDesc is the one line that has to say what choosing this costs. The
// three zero-cost meterings are deliberately kept distinct (§4): free because
// local, free because a plan pays, and free-for-now are different promises.
func catalogDesc(info catalog.ModelInfo) string {
	name := info.DisplayName
	if name == "" {
		name = info.ProviderModelID
	}
	switch info.Metering {
	case catalog.Local:
		return name + " · local"
	case catalog.Plan:
		return name + " · plan-metered"
	}
	if info.Free() {
		return name + " · free"
	}
	if band, ok := info.Band(0); ok {
		return name + " · " + band.InputPerMTok.String() + "/" + band.OutputPerMTok.String() + " per MTok in/out"
	}
	return name
}

// chooseTierCmd is stage two: which rung takes the model. Existing rungs
// rebind in place and keep their labels; one new rung past the top is always
// offered, which is how a ladder grows and how the first tier gets made.
func chooseTierCmd(m *tuiModel, choice modelChoice) tea.Cmd {
	cfg := m.app.config
	return func() tea.Msg {
		var items []pickerItem
		for _, t := range cfg.Tiers {
			items = append(items, pickerItem{
				id:    t.ID,
				label: t.ID,
				desc:  "rebind; now " + t.Target.Display(),
			})
		}
		// One past the highest rung, not the count plus one: the ladder can
		// have gaps (t1, t3) and a "new" rung must not collide with t3.
		next := "t" + strconv.Itoa(highestRung(cfg)+1)
		items = append(items, pickerItem{id: next, label: next, desc: "new rung at the top"})

		return pickerMsg{
			title: "which tier runs " + choice.ref,
			items: items,
			action: func(tierID string) tea.Cmd {
				if len(choice.effortLevels) > 0 {
					return chooseEffortCmd(m, choice, tierID)
				}
				return bindCmd(m, choice, tierID, "")
			},
		}
	}
}

func chooseEffortCmd(m *tuiModel, choice modelChoice, tierID string) tea.Cmd {
	return func() tea.Msg {
		items := []pickerItem{{id: "", label: "default", desc: "let the provider decide"}}
		for _, level := range choice.effortLevels {
			items = append(items, pickerItem{id: level, label: level})
		}
		return pickerMsg{
			title: "reasoning effort for " + choice.ref + " on " + tierID,
			items: items,
			action: func(effort string) tea.Cmd {
				return bindCmd(m, choice, tierID, effort)
			},
		}
	}
}

func bindCmd(m *tuiModel, choice modelChoice, tierID, effort string) tea.Cmd {
	cfg := m.app.config
	label := ""
	if existing, ok := cfg.Tier(tierID); ok {
		label = existing.Label
	}
	if err := cfg.BindTier(tierID, label, choice.ref, choice.surface, effort); err != nil {
		return noticeCmd("error", err.Error())
	}
	if err := cfg.Save(); err != nil {
		return noticeCmd("error", "binding "+tierID+" failed to save: "+err.Error())
	}
	return noticeCmd("", tierID+" now runs "+choice.ref+"; /"+tierID+" switches to it")
}

func removeRungCmd(m *tuiModel) tea.Cmd {
	cfg := m.app.config
	return func() tea.Msg {
		var items []pickerItem
		for _, t := range cfg.Tiers {
			desc := t.Target.Display()
			if t.ID == m.app.tier.ID {
				desc += " · active now"
			}
			items = append(items, pickerItem{id: t.ID, label: t.ID, desc: desc})
		}
		return pickerMsg{
			title: "remove which rung",
			items: items,
			action: func(tierID string) tea.Cmd {
				if tierID == m.app.tier.ID {
					return noticeCmd("error", tierID+" is the active tier; switch off it before removing it")
				}
				if !cfg.RemoveTier(tierID) {
					return noticeCmd("error", "no rung named "+tierID)
				}
				if err := cfg.Save(); err != nil {
					return noticeCmd("error", "removing "+tierID+" failed to save: "+err.Error())
				}
				return noticeCmd("", tierID+" removed from the ladder")
			},
		}
	}
}

func highestRung(cfg *config.Config) int {
	high := 0
	for _, t := range cfg.Tiers {
		n, err := strconv.Atoi(strings.TrimPrefix(t.ID, "t"))
		if err == nil && n > high {
			high = n
		}
	}
	return high
}
