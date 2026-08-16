package main

// /learn: distill the session that just did something into the skill that
// does it again. A session that worked out a procedure — the flags that
// build this repo, the order its services restart in, the pitfall that ate
// an hour — holds knowledge worth more than its transcript, and a skill
// pack is exactly the shape this program already serves it back in (§13).
// The distillation is a one-shot request outside the loop, /compact's
// mechanism reused whole: the summarizer slot writes it when bound, the
// current tier otherwise, no tools attached, nothing appended to the
// session.
//
// What it writes cannot register mid-session, and the command says so
// rather than pretending: skill discovery is once, at session assembly,
// because the descriptions ride the tool schema into the frozen zone
// (§6.1). The pack lands in the workspace's .switchboard/skills/ and is
// offered when the next session assembles.
//
// The file is durable and may be committed, so it passes the credential
// scan before it touches disk — the race record's posture: a derived
// artifact redacts unconditionally, because a key that survived into a
// skill pack would hand itself to every future session and every clone.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/credential"
	"github.com/cj-vana/switchboard/internal/provider"
)

const learnSystem = `You are distilling a coding session into a reusable skill: standing instructions a model doing a similar task in this repository will pull in later. Write the repeatable method, not the story of this run. First line: one sentence saying when to use the skill, written so a model can match a task against it; no heading, no quotes. Then a blank line, then the instructions: the procedure that worked, in order; exact commands with their flags; files and locations that matter and why; the pitfalls this session actually hit and how each was resolved. Leave out what was specific to this one task (the particular bug, the particular values) and keep what the next task will need again. Plain markdown, no preamble, no title.`

const learnUsage = "/learn <name> distills this session into .switchboard/skills/<name>/SKILL.md; the name is lowercase words joined by hyphens, e.g. /learn release-checklist"

// skillNamePattern is deliberately narrow: the name becomes a directory and
// a tool-visible identifier, and validating is honester than transforming —
// a name silently rewritten is a name the user did not choose.
var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func cmdLearn(m *tuiModel, args string) tea.Cmd {
	name := strings.TrimSpace(args)
	if name == "" {
		return noticeCmd("", learnUsage)
	}
	if !skillNamePattern.MatchString(name) {
		return noticeCmd("error", "skill names are lowercase words joined by hyphens; "+learnUsage)
	}

	state := m.app.loop.Session.State()
	if len(state.Messages) == 0 {
		return noticeCmd("", "nothing to learn from yet; the session is empty")
	}

	dest := filepath.Join(m.app.workspace, ".switchboard", "skills", name, "SKILL.md")
	if _, err := os.Stat(dest); err == nil {
		return noticeCmd("error", "a skill named "+name+" already exists at "+dest+"; pick another name, or delete it first")
	}

	distiller, fromSlot, err := summarizerFor(m.app)
	if err != nil {
		return noticeCmd("error", err.Error())
	}

	line := "learning: distilling " + itoa(len(state.Messages)) + " messages on " + string(distiller.Target.ID())
	if fromSlot {
		line += " (the summarizer slot)"
	}
	m.addInfo(line + "…")

	app := m.app
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		client, target := app.loop.Provider, app.tier.Target
		if fromSlot {
			probed, slotClient, perr := app.providers.probeTier(ctx, distiller)
			if perr != nil {
				return noticeMsg{level: "error", text: "summarizer slot " + string(distiller.Target.ID()) +
					" is unreachable, nothing written: " + perr.Error()}
			}
			client, target = slotClient, probed.Target
		}

		generated, err := distill(ctx, client, target, state.Messages)
		if err != nil {
			return noticeMsg{level: "error", text: "learn failed, nothing written: " + err.Error()}
		}

		content, redacted, err := composeSkill(name, generated)
		if err != nil {
			return noticeMsg{level: "error", text: "learn failed, nothing written: " + err.Error()}
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return noticeMsg{level: "error", text: "learn failed: " + err.Error()}
		}
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			return noticeMsg{level: "error", text: "learn failed: " + err.Error()}
		}

		text := "skill " + name + " saved to " + dest + "; it is offered when the next session assembles its tools"
		if redacted > 0 {
			text += fmt.Sprintf(" (%d credential-shaped strings were redacted on the way)", redacted)
		}
		return noticeMsg{level: "", text: text}
	}
}

// distill is summarize's sibling: one request outside the loop, no tools, the
// conversation and an instruction to extract the method from it.
func distill(ctx context.Context, client provider.Provider, target provider.RouteTarget, messages []provider.Message) (string, error) {
	req := provider.Request{
		System:   []provider.Block{provider.Text{Text: learnSystem}},
		Messages: append(append([]provider.Message{}, messages...), provider.UserText("Distill this session into a skill, per your instructions.")),
	}

	stream, err := client.Stream(ctx, target, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var b strings.Builder
	for {
		ev, err := stream.Next()
		if err != nil {
			return "", err
		}
		switch ev.Type {
		case provider.EventTextDelta:
			b.WriteString(ev.Text)
		case provider.EventDone:
			if s := strings.TrimSpace(b.String()); s != "" {
				return s, nil
			}
			return "", fmt.Errorf("the distiller returned nothing")
		}
	}
}

// composeSkill turns the distiller's output into a SKILL.md: first line
// becomes the frontmatter description, the rest the body, and the whole file
// passes the credential scan before anything reaches disk. The redaction is
// unconditional, never a prompt, because the file outlives every chance to
// ask.
func composeSkill(name, generated string) (content string, redacted int, err error) {
	desc, body, _ := strings.Cut(strings.TrimSpace(generated), "\n")
	// The parser reads the description to the end of its line, so it must
	// hold no newlines; collapsing whitespace also unwraps a distiller that
	// wrapped its first sentence.
	desc = strings.Join(strings.Fields(desc), " ")
	body = strings.TrimSpace(body)
	if desc == "" || body == "" {
		return "", 0, fmt.Errorf("the distiller returned no usable description and body")
	}

	content = "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n" + body + "\n"
	if leaks := credential.ScanPrompt(content); len(leaks) > 0 {
		content = credential.Redact(content, leaks)
		redacted = len(leaks)
	}
	return content, redacted, nil
}
