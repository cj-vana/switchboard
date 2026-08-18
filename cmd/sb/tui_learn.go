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
// (§6.1). The pack lands in the standard workspace .agents/skills/ tree and is
// offered when the next Switchboard run assembles its frozen tool registry.
//
// The file is durable and may be committed, so it passes the credential
// scan before it touches disk — the race record's posture: a derived
// artifact redacts unconditionally, because a key that survived into a
// skill pack would hand itself to every future session and every clone.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

const learnSystem = `You are distilling a coding session into a reusable skill: standing instructions a model doing a similar task in this repository will pull in later. Write the repeatable method, not the story of this run. First line: one sentence saying when to use the skill, written so a model can match a task against it; no heading, no quotes. Then a blank line, then the instructions: the procedure that worked, in order; exact commands with their flags; files and locations that matter and why; the pitfalls this session actually hit and how each was resolved. Leave out what was specific to this one task (the particular bug, the particular values) and keep what the next task will need again. Plain markdown, no preamble, no title.`

const learnUsage = "/learn <name> distills this session into .agents/skills/<name>/SKILL.md; the name is lowercase words joined by hyphens, e.g. /learn release-checklist"

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

	dest := filepath.Join(m.app.workspace, ".agents", "skills", name, "SKILL.md")
	if _, err := os.Stat(dest); err == nil {
		return noticeCmd("error", "a skill named "+name+" already exists at "+dest+"; pick another name, or delete it first")
	}

	distiller, fromSlot, err := summarizerFor(m.app)
	if err != nil {
		return noticeCmd("error", err.Error())
	}
	opCtx, generation, sourceID, err := m.startOperation("learn")
	if err != nil {
		return noticeCmd("warn", err.Error())
	}

	line := "learning: distilling " + itoa(len(state.Messages)) + " messages on " + distiller.Target.Display()
	if fromSlot {
		line += " (the summarizer slot)"
	}
	m.addInfo(line + "…")

	app := m.app
	sourceSess := m.app.loop.Session
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(opCtx, 5*time.Minute)
		defer cancel()
		finishNotice := func(level, text string) noticeMsg {
			return noticeMsg{level: level, text: text, operation: generation, sourceID: sourceID}
		}

		client, target := app.loop.Binding().Provider, app.tier.Target
		if fromSlot {
			probed, slotClient, perr := app.providers.probeTier(ctx, distiller)
			if perr != nil {
				return finishNotice("error", "summarizer slot "+distiller.Target.Display()+
					" is unreachable, nothing written: "+perr.Error())
			}
			client, target = slotClient, probed.Target
		}

		req := distillRequest(state.Messages)
		finish, err := beginMeteredCall(app.budget, app.catalog, sourceSess, target, req, session.UsagePurposeLearn)
		if err != nil {
			return finishNotice("error", "learn stopped before distilling, nothing written: "+err.Error())
		}
		generated, usage, providerDone, callErr := distillRequestCall(ctx, client, target, req)
		meterOutcome := callErr
		if providerDone {
			meterOutcome = nil
		}
		meterErr := finish(usage, meterOutcome)
		if err := errors.Join(callErr, meterErr); err != nil {
			return finishNotice("error", "learn failed, nothing written: "+err.Error())
		}

		provenance := fmt.Sprintf(
			"Provenance: distilled from session %s on %s, %d messages, written by %s. "+
				"When this method stops matching the repository, delete the pack and /learn a fresh one; the session remains the evidence.",
			state.ID, time.Now().Format("2006-01-02"), len(state.Messages), target.Display())
		content, redacted, err := composeSkill(name, generated, provenance)
		if err != nil {
			return finishNotice("error", "learn failed, nothing written: "+err.Error())
		}
		if err := ctx.Err(); err != nil {
			return finishNotice("", "")
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return finishNotice("error", "learn failed: "+err.Error())
		}
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			return finishNotice("error", "learn failed: "+err.Error())
		}

		text := "skill " + name + " saved to " + dest + "; it is offered on the next Switchboard run"
		if redacted > 0 {
			text += fmt.Sprintf(" (%d credential-shaped strings were redacted on the way)", redacted)
		}
		return finishNotice("", text)
	}
}

// distill is summarize's sibling: one request outside the loop, no tools, the
// conversation and an instruction to extract the method from it.
func distill(ctx context.Context, client provider.Provider, target provider.RouteTarget, messages []provider.Message) (string, error) {
	text, _, _, err := distillRequestCall(ctx, client, target, distillRequest(messages))
	return text, err
}

func distillRequest(messages []provider.Message) provider.Request {
	return provider.Request{
		System:   []provider.Block{provider.Text{Text: learnSystem}},
		Messages: append(append([]provider.Message{}, messages...), provider.UserText("Distill this session into a skill, per your instructions.")),
	}
}

func distillRequestCall(ctx context.Context, client provider.Provider, target provider.RouteTarget, req provider.Request) (string, provider.Usage, bool, error) {
	stream, err := client.Stream(ctx, target, req)
	if err != nil {
		return "", provider.Usage{}, false, err
	}
	defer stream.Close()

	var b strings.Builder
	for {
		ev, err := stream.Next()
		if err != nil {
			return "", provider.Usage{}, false, err
		}
		switch ev.Type {
		case provider.EventTextDelta:
			b.WriteString(ev.Text)
		case provider.EventDone:
			if s := strings.TrimSpace(b.String()); s != "" {
				return s, ev.Usage, true, nil
			}
			return "", ev.Usage, true, fmt.Errorf("the distiller returned nothing")
		}
	}
}

// composeSkill turns the distiller's output into a SKILL.md: first line
// becomes the frontmatter description, the rest the body, and the whole file
// passes the credential scan before anything reaches disk. The redaction is
// unconditional, never a prompt, because the file outlives every chance to
// ask.
//
// The provenance paragraph exists so the pack can be deleted safely later.
// Instruction files grow without bound precisely because the reason an
// instruction exists is lost the day it is written, and deleting one whose
// rationale is gone feels like risking a regression; a pack that names the
// session it came from can be judged against that session and dropped when
// the method stops matching the repository. It rides the body rather than
// the frontmatter, because the neighboring tools' parsers ignore unknown
// frontmatter keys and this line is written for readers, not parsers.
func composeSkill(name, generated, provenance string) (content string, redacted int, err error) {
	desc, body, _ := strings.Cut(strings.TrimSpace(generated), "\n")
	// The parser reads the description to the end of its line, so it is cut
	// at the distiller's first newline; a wrapped tail is not lost, it opens
	// the body. The collapse keeps stray whitespace out of the frontmatter.
	desc = strings.Join(strings.Fields(desc), " ")
	body = strings.TrimSpace(body)
	if desc == "" || body == "" {
		return "", 0, fmt.Errorf("the distiller returned no usable description and body")
	}

	content = "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n" + body + "\n"
	if provenance != "" {
		content += "\n" + provenance + "\n"
	}
	if leaks := credential.ScanPrompt(content); len(leaks) > 0 {
		content = credential.Redact(content, leaks)
		redacted = len(leaks)
	}
	return content, redacted, nil
}
