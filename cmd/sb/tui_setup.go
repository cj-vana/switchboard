package main

// /setup: every provider this build can reach, one checklist, connect them
// all without leaving the TUI. Each row shows its live standing — the local
// server's model count, where each credential would resolve from — and
// picking a row does the one thing that row needs: a masked key prompt, or
// wiring a login that already exists on this machine. The checklist reopens
// after every action, so connecting three providers is three picks, not
// three commands.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/ollama"
	"github.com/switchboard-code/switchboard/internal/provider/openaicompat"
)

const (
	setupDoneID   = "\x00done"
	setupCodexID  = "\x00codex"
	setupLocalID  = "\x00ollama"
	setupCompatID = "\x00compat"
)

// codexHelper reads the access token Codex CLI keeps refreshed. Wiring it is
// the user's choice made explicitly here, never something detection does on
// its own: whose token it is stays visible.
var codexHelper = []string{"sh", "-c",
	`python3 -c "import json,os;print(json.load(open(os.path.expanduser('~/.codex/auth.json')))['tokens']['access_token'])"`}

// locallyMetered reports whether a reference names a surface the catalog says
// consumes nothing scarce, which is also the set of surfaces that issue no
// credentials.
func locallyMetered(cat *catalog.Catalog, ref credential.Ref) bool {
	info, _, ok := cat.Lookup(provider.RouteTarget{Provider: ref.Provider, Surface: ref.Account})
	return ok && info.Metering == catalog.Local
}

func cmdSetup(m *tuiModel, _ string) tea.Cmd {
	return setupChecklist(m)
}

// setupItems is the checklist itself, shared by /setup and the first-run
// wizard: same rows, same standings, different framing around them. The
// caller supplies the exit row.
func setupItems(ctx context.Context, reg *providers, cat *catalog.Catalog, cfg *config.Config) []pickerItem {
	var items []pickerItem

	host := reg.localServer().BaseURL()
	if names, err := reg.localServer().Models(ctx); err == nil {
		items = append(items, pickerItem{
			id: setupLocalID, label: "ollama/local", current: true,
			desc: fmt.Sprintf("running at %s, %d models pulled", host, len(names)),
		})
	} else {
		items = append(items, pickerItem{
			id: setupLocalID, label: "ollama/local",
			desc: "server not answering at " + host + "; start ollama, or set its address here",
		})
	}

	// The compatible endpoint is configuration rather than a credential: until
	// it has an address there is no server to have a standing with, which is
	// why it needs a row of its own rather than a key prompt.
	compat := cfg.ProviderForTarget(openaicompat.Name, genericCompat).BaseURL
	items = append(items, pickerItem{
		id:      setupCompatID,
		label:   openaicompat.Name + "/" + genericCompat,
		desc:    "any OpenAI-compatible server · " + orNone(compat),
		current: compat != "",
	})

	for _, ref := range credentialRefs(cfg, cat) {
		if ref.Provider == "ollama" {
			continue // covered by the liveness row above
		}
		if locallyMetered(cat, ref) {
			// Nothing meters it and nothing issues a key for it, so a
			// credential row would be a prompt with no correct answer. /login
			// still lists every reference, for the endpoint that turns out to
			// want one anyway.
			continue
		}
		standing := credentialStanding(ctx, cfg, ref)
		items = append(items, pickerItem{
			id:      ref.String(),
			label:   ref.String(),
			desc:    standing,
			current: standing != "not set",
		})
	}

	if codexLoginAvailable(cfg) {
		items = append(items, pickerItem{
			id:    setupCodexID,
			label: "use your Codex CLI login",
			desc:  "~/.codex/auth.json found; wire it as openai's credential helper",
		})
	}
	return items
}

func setupChecklist(m *tuiModel) tea.Cmd {
	app := m.app
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		items := append(setupItems(ctx, app.providers, app.catalog, app.config),
			pickerItem{id: setupDoneID, label: "done", desc: "bind rungs with /models"})

		return pickerMsg{
			title: "connect providers",
			items: items,
			action: func(id string) tea.Cmd {
				switch id {
				case setupDoneID:
					return noticeCmd("", "setup closed; /models binds what you connected, /setup returns here")
				case setupLocalID:
					return askAddressCmd(app.providers, app.config, ollama.Name, ollama.SurfaceLocal,
						func() tea.Cmd { return setupChecklist(m) })
				case setupCompatID:
					return askAddressCmd(app.providers, app.config, openaicompat.Name, genericCompat,
						func() tea.Cmd { return setupChecklist(m) })
				case setupCodexID:
					return tea.Sequence(wireCodexHelper(m), setupChecklist(m))
				}
				ref, err := parseRef(id)
				if err != nil {
					return noticeCmd("error", err.Error())
				}
				return setupSecretCmd(m, ref)
			},
		}
	}
}

// setupSecretCmd is openSecretCmd with a return ticket: after the store, the
// checklist reopens with the row's standing refreshed.
func setupSecretCmd(m *tuiModel, ref credential.Ref) tea.Cmd {
	store := credential.NewOSStore()
	writer, ok := any(store).(credential.Writer)
	if !ok {
		return noticeCmd("error", store.Name()+" cannot store credentials on this platform; "+
			"set "+credential.EnvNames(ref)[0]+" in the environment instead")
	}
	return func() tea.Msg {
		return secretPromptMsg{ref: ref, writer: writer, storeName: store.Name(), then: setupChecklist(m)}
	}
}

func codexLoginAvailable(cfg *config.Config) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		return false
	}
	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &auth) != nil || auth.Tokens.AccessToken == "" {
		return false
	}
	// Already wired is not an offer worth repeating.
	return len(cfg.AuthFor("openai").Helper) == 0
}

// wireCodex writes the helper into the config. Both entrances — /setup and
// the first-run wizard — go through here, so the wiring is one behavior.
func wireCodex(cfg *config.Config) error {
	settings := cfg.AuthFor("openai")
	settings.Helper = codexHelper
	if cfg.Auth == nil {
		cfg.Auth = map[string]credential.Settings{}
	}
	cfg.Auth["openai"] = settings
	return cfg.Save()
}

func wireCodexHelper(m *tuiModel) tea.Cmd {
	cfg := m.app.config
	return func() tea.Msg {
		if err := wireCodex(cfg); err != nil {
			return noticeMsg{level: "error", text: "wiring the codex login failed: " + err.Error()}
		}
		return noticeMsg{text: "openai now authenticates with your Codex CLI login; when its token expires, running codex once refreshes it"}
	}
}
