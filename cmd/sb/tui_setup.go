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

	"github.com/cj-vana/switchboard/internal/credential"
)

const (
	setupDoneID  = "\x00done"
	setupCodexID = "\x00codex"
	setupLocalID = "\x00ollama"
)

// codexHelper reads the access token Codex CLI keeps refreshed. Wiring it is
// the user's choice made explicitly here, never something detection does on
// its own: whose token it is stays visible.
var codexHelper = []string{"sh", "-c",
	`python3 -c "import json,os;print(json.load(open(os.path.expanduser('~/.codex/auth.json')))['tokens']['access_token'])"`}

func cmdSetup(m *tuiModel, _ string) tea.Cmd {
	return setupChecklist(m)
}

func setupChecklist(m *tuiModel) tea.Cmd {
	app := m.app
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		var items []pickerItem

		if names, err := app.providers.ollama.Models(ctx); err == nil {
			items = append(items, pickerItem{
				id: setupLocalID, label: "ollama/local", current: true,
				desc: fmt.Sprintf("running, %d models pulled", len(names)),
			})
		} else {
			items = append(items, pickerItem{
				id: setupLocalID, label: "ollama/local",
				desc: "server not answering; start ollama to use local models",
			})
		}

		for _, ref := range credentialRefs(app.config, app.catalog) {
			if ref.Provider == "ollama" {
				continue // covered by the liveness row above
			}
			standing := credentialStanding(ctx, app.config, ref)
			items = append(items, pickerItem{
				id:      ref.String(),
				label:   ref.String(),
				desc:    standing,
				current: standing != "not set",
			})
		}

		if codexLoginAvailable(app) {
			items = append(items, pickerItem{
				id:    setupCodexID,
				label: "use your Codex CLI login",
				desc:  "~/.codex/auth.json found; wire it as openai's credential helper",
			})
		}
		items = append(items, pickerItem{id: setupDoneID, label: "done", desc: "bind rungs with /models"})

		return pickerMsg{
			title: "connect providers",
			items: items,
			action: func(id string) tea.Cmd {
				switch id {
				case setupDoneID:
					return noticeCmd("", "setup closed; /models binds what you connected, /setup returns here")
				case setupLocalID:
					return noticeCmd("", "local models need a running Ollama server: https://ollama.com, then `ollama pull <model>`")
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

func codexLoginAvailable(app *tuiApp) bool {
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
	return len(app.config.AuthFor("openai").Helper) == 0
}

func wireCodexHelper(m *tuiModel) tea.Cmd {
	cfg := m.app.config
	return func() tea.Msg {
		settings := cfg.AuthFor("openai")
		settings.Helper = codexHelper
		if cfg.Auth == nil {
			cfg.Auth = map[string]credential.Settings{}
		}
		cfg.Auth["openai"] = settings
		if err := cfg.Save(); err != nil {
			return noticeMsg{level: "error", text: "wiring the codex login failed: " + err.Error()}
		}
		note := noticeMsg{text: "openai now authenticates with your Codex CLI login; when its token expires, running codex once refreshes it"}
		return note
	}
}
