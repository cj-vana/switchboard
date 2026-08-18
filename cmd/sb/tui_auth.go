package main

// /login and /logout: credential entry without leaving the TUI. The flow is
// the same storage path as `sb auth login` — the OS store via the credential
// Writer — so a key entered here and a key piped in a script land in the same
// place and resolve through the same chain. The TUI adds what the CLI cannot:
// a picker that shows, per reference, where a credential would come from right
// now, so "why is this not working" is answered before a request fails.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
)

// credentialRefs lists every provider/surface pair worth offering a key for:
// the ladder's own targets first, then everything the catalog knows. The
// catalog entries matter on a fresh machine, where the user wants to store a
// key before any tier is bound to use it.
func credentialRefs(cfg *config.Config, cat *catalog.Catalog) []credential.Ref {
	var refs []credential.Ref
	seen := map[string]bool{}
	add := func(r credential.Ref) {
		if !seen[r.String()] {
			seen[r.String()] = true
			refs = append(refs, r)
		}
	}
	for _, r := range refsInUse(cfg) {
		add(r)
	}
	var rest []credential.Ref
	for _, info := range cat.Surfaces() {
		r := credential.Ref{Provider: info.Provider, Account: info.Surface}
		if !seen[r.String()] {
			seen[r.String()] = true
			rest = append(rest, r)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].String() < rest[j].String() })
	return append(refs, rest...)
}

// pickerMsg carries an assembled picker back from a command that had to do
// its gathering off the UI goroutine: credential standing is a helper exec
// away, a model list is a server round trip away.
type pickerMsg struct {
	title  string
	items  []pickerItem
	action func(id string) tea.Cmd
}

func cmdLogin(m *tuiModel, args string) tea.Cmd {
	if args != "" {
		ref, err := parseRef(args)
		if err != nil {
			return noticeCmd("error", err.Error())
		}
		return openSecretCmd(m, ref)
	}

	cfg, cat := m.app.config, m.app.catalog
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		refs := credentialRefs(cfg, cat)
		items := make([]pickerItem, 0, len(refs))
		for _, ref := range refs {
			items = append(items, pickerItem{
				id:    ref.String(),
				label: ref.String(),
				desc:  credentialStanding(ctx, cfg, ref),
			})
		}
		return pickerMsg{
			title: "store a credential",
			items: items,
			action: func(refStr string) tea.Cmd {
				ref, err := parseRef(refStr)
				if err != nil {
					return noticeCmd("error", err.Error())
				}
				return openSecretCmd(m, ref)
			},
		}
	}
}

// credentialStanding says where a reference's credential comes from today,
// and never what it is. The resolver's sources describe themselves; "not
// found" names the first place a key would be looked for, which is the next
// action, not just the diagnosis.
func credentialStanding(ctx context.Context, cfg *config.Config, ref credential.Ref) string {
	if ref.Provider == "ollama" {
		return "local server, no key needed"
	}
	secret, err := credential.Chain(cfg.AuthFor(ref.Provider)).Get(ctx, ref)
	if err != nil {
		var notFound *credential.NotFoundError
		if errors.As(err, &notFound) {
			return "not set"
		}
		return "error: " + err.Error()
	}
	return "from " + string(secret.Source) + " (" + secret.Detail + ")"
}

// openSecretCmd resolves to a message rather than mutating m.dlg here: the
// picker that invoked us is mid-update, and its closing sets m.dlg to nil
// after this returns.
func openSecretCmd(m *tuiModel, ref credential.Ref) tea.Cmd {
	store := credential.NewOSStore()
	writer, ok := any(store).(credential.Writer)
	if !ok {
		return noticeCmd("error", store.Name()+" cannot store credentials on this platform; "+
			"set "+credential.EnvNames(ref)[0]+" in the environment instead")
	}
	return func() tea.Msg {
		return secretPromptMsg{ref: ref, writer: writer, storeName: store.Name()}
	}
}

type secretPromptMsg struct {
	ref       credential.Ref
	writer    credential.Writer
	storeName string

	// then, when set, runs after a successful store: the flow that opened
	// the prompt gets to continue (setup reopens its checklist).
	then tea.Cmd
}

// secretDialog takes one secret without echoing it. The value lives in the
// textinput until submit or escape and is never written to the transcript,
// the session log, or the config file; its only destination is the OS store.
type secretDialog struct {
	ref       credential.Ref
	storeName string
	input     textinput.Model
	submit    func(value string) tea.Cmd
}

func newSecretDialog(ref credential.Ref, storeName string, submit func(string) tea.Cmd) *secretDialog {
	ti := textinput.New()
	ti.Prompt = ""
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	ti.Focus()
	return &secretDialog{ref: ref, storeName: storeName, input: ti, submit: submit}
}

func (d *secretDialog) update(key tea.KeyMsg, th *theme) (bool, tea.Cmd) {
	switch key.String() {
	case "esc":
		d.input.Reset()
		return true, nil
	case "enter":
		value := strings.TrimSpace(d.input.Value())
		d.input.Reset()
		if value == "" {
			return true, noticeCmd("", "nothing entered, nothing stored")
		}
		return true, d.submit(value)
	}
	var cmd tea.Cmd
	d.input, cmd = d.input.Update(key)
	return false, cmd
}

func (d *secretDialog) view(width int, th *theme) string {
	var b strings.Builder
	b.WriteString(th.bold.Render(" credential for "+d.ref.String()) + "\n")
	b.WriteString(th.dim.Render(" paste or type; input is hidden; stored in the "+d.storeName) + "\n\n")
	b.WriteString(" " + d.input.View() + "\n")
	b.WriteString(th.faint.Render(" enter store · empty entry skips · esc cancel"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor(th)).
		Padding(0, 1).
		Width(max(width-4, 40))
	return box.Render(b.String())
}

// storeSecretCmd writes the credential off the UI goroutine and reports where
// it landed, never what it was.
func storeSecretCmd(ref credential.Ref, writer credential.Writer, storeName, value string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := writer.Set(ctx, ref, value); err != nil {
			return noticeMsg{level: "error", text: "storing " + ref.String() + " failed: " + err.Error()}
		}
		return noticeMsg{text: "stored " + ref.String() + " in the " + storeName}
	}
}

func cmdLogout(m *tuiModel, args string) tea.Cmd {
	if args != "" {
		ref, err := parseRef(args)
		if err != nil {
			return noticeCmd("error", err.Error())
		}
		return removeSecretCmd(ref)
	}

	cfg, cat := m.app.config, m.app.catalog
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		// Only references the OS store actually holds: a logout picker
		// listing providers with nothing stored would offer removals that
		// cannot happen.
		store := credential.NewOSStore()
		var items []pickerItem
		for _, ref := range credentialRefs(cfg, cat) {
			if _, err := store.Get(ctx, ref); err == nil {
				items = append(items, pickerItem{id: ref.String(), label: ref.String(), desc: "stored in the " + store.Name()})
			}
		}
		if len(items) == 0 {
			return noticeMsg{text: "the " + store.Name() + " holds no switchboard credentials"}
		}
		return pickerMsg{
			title: "remove a credential",
			items: items,
			action: func(refStr string) tea.Cmd {
				ref, err := parseRef(refStr)
				if err != nil {
					return noticeCmd("error", err.Error())
				}
				return removeSecretCmd(ref)
			},
		}
	}
}

func removeSecretCmd(ref credential.Ref) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		store := credential.NewOSStore()
		writer, ok := any(store).(credential.Writer)
		if !ok {
			return noticeMsg{level: "error", text: store.Name() + " stores nothing on this platform"}
		}
		if err := writer.Delete(ctx, ref); err != nil {
			if errors.Is(err, credential.ErrNotFound) {
				return noticeMsg{level: "error", text: "no stored credential for " + ref.String()}
			}
			return noticeMsg{level: "error", text: err.Error()}
		}
		text := "removed " + ref.String() + " from the " + store.Name()
		if leftover := environmentStillSupplies(ctx, ref); leftover != "" {
			text += "; note that " + leftover + " is still set in this environment and takes precedence"
		}
		return noticeMsg{text: text}
	}
}
