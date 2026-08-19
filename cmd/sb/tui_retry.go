package main

// /retry: take the last turn back and run it again, optionally on another
// rung. It is a composition of things the session model already guarantees,
// not a new mechanism: the turn's file changes revert through the checkpoint
// recorder (a restored file forces a re-read, the /undo contract), the
// conversation goes back by forking at the turn's opening message (the
// original log is read, never written, and stays resumable), and the same
// opening replays byte-for-byte — the recorded message, not a re-expansion,
// so the retried rung reads exactly what the first one read, files as they
// were, gate already passed. That is what makes the rerun a controlled
// comparison instead of a similar question asked twice.
//
// What it does not take back is said out loud: side effects of commands the
// discarded turn ran are outside the checkpoint boundary, the same limit
// /undo states.
//
// The set-aside answer is labelled before the fork: §8.4's user_corrected
// is exactly this event, recorded as a note on the source log, where the
// answer it corrects lives. Routing never reads it — collecting the corpus
// honestly comes before acting on it.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
)

type retryStartMsg struct {
	generation uint64
	opening    provider.Message
	prompt     string // display-only projection; replay always uses opening
	tier       string // empty reruns on the sitting rung
}

func cmdRetry(m *tuiModel, args string) tea.Cmd {
	args = strings.TrimSpace(args)
	var dest config.Tier
	if args != "" {
		t, ok := m.app.config.Tier(args)
		if !ok {
			return noticeCmd("error", "no tier "+args+" is configured; try /tiers")
		}
		dest = t
	}

	state := m.app.loop.Session.State()
	last := lastTurnOpening(state.Messages)
	if last < 0 {
		return noticeCmd("", "nothing to retry; the session has no completed turn")
	}
	opening := provider.CloneMessage(state.Messages[last])
	prompt := opening.AuthoredText()
	if prompt == "" {
		return noticeCmd("error", "the last turn's opening carries no text to replay")
	}

	ctx, generation, sourceID, err := m.startOperation("retry")
	if err != nil {
		return noticeCmd("warn", err.Error())
	}

	// The files first, while the recorder's stack still has the turn on
	// top. A turn that changed nothing has no scope, and an older label on
	// top means the stack has moved past this turn — either way the files
	// stay, and the notice says which world the rerun starts in.
	if rec := m.app.undo; rec != nil {
		if turns := rec.Turns(); len(turns) > 0 && turns[len(turns)-1].Label == checkpointLabel(prompt) {
			if turns[len(turns)-1].Partial {
				var skipped []string
				if details := rec.Details(); len(details) > 0 && details[len(details)-1].Label == turns[len(turns)-1].Label {
					skipped = details[len(details)-1].Skipped
				}
				text := "retry stopped before changing files or running another model: the turn's checkpoint is partial"
				if len(skipped) > 0 {
					text += "; not checkpointed: " + boundedRetryPaths(skipped)
				}
				text += "; use /undo to review and explicitly consume that partial restore"
				_ = m.app.loop.Session.AppendNote("warn", text)
				return func() tea.Msg {
					return noticeMsg{level: "error", text: text, operation: generation, sourceID: sourceID}
				}
			}
			restored, removed, skipped, failed, label, undoErr := rec.Undo()
			changed := append(append([]string(nil), restored...), removed...)
			if len(changed) > 0 {
				m.app.loop.Tools.ForgetVersions(changed)
				invalidateRestoredWorkspace(m)
				m.app.loop.Session.AppendNote("info", fmt.Sprintf("retry: took back the files of %q (%d restored, %d removed)", label, len(restored), len(removed)))
			}
			if undoErr != nil || len(skipped) > 0 || len(failed) > 0 {
				if len(changed) > 0 {
					m.addInfo(fmt.Sprintf("  took back part of the turn's file changes (%d restored, %d removed); what commands did stays done", len(restored), len(removed)))
				}
				text := retryUndoRefusal(len(restored), len(removed), skipped, failed, undoErr)
				_ = m.app.loop.Session.AppendNote("warn", text)
				return func() tea.Msg {
					return noticeMsg{level: "error", text: text, operation: generation, sourceID: sourceID}
				}
			}
			if len(changed) > 0 {
				m.addInfo(fmt.Sprintf("  took back the turn's file changes (%d restored, %d removed); what commands did stays done", len(restored), len(removed)))
			}
		} else {
			m.addInfo("  the turn's files were left as they are (it changed none, or /undo already took them back)")
		}
	}

	destID := m.app.tier.ID
	if dest.ID != "" {
		destID = dest.ID
	}
	sess := m.app.loop.Session
	sess.AppendNote("info", fmt.Sprintf("retry: the last answer was set aside (outcome user_corrected); rerunning on %s", destID))

	id := sess.ID()
	app := m.app
	start := func() tea.Msg {
		return retryStartMsg{opening: provider.CloneMessage(opening), prompt: prompt, tier: dest.ID}
	}
	return func() tea.Msg {
		release, err := pauseAdvisorLedger(ctx, app)
		if err != nil {
			return sessionSwapMsg{err: fmt.Errorf("waiting for the advisor ledger before retry: %w", err), operation: generation, sourceID: sourceID}
		}
		// A retry-specific fork can keep an empty conversation for the first
		// turn, but it always carries the source's budget ledger. Repeated retry
		// therefore cannot make already-sent requests disappear from a ceiling.
		forked, err := app.store.ForkSessionForRetry(sess, last)
		if err != nil {
			return sessionSwapMsg{err: err, release: release, operation: generation, sourceID: sourceID}
		}
		return sessionSwapMsg{sess: forked, tier: app.tier, client: app.loop.Binding().Provider, fresh: last == 0,
			note: fmt.Sprintf("retrying the last turn on %s; the set-aside answer stays in %s, /resume %s returns to it", destID, id, id), andThen: start, release: release,
			continueTurn: true, operation: generation, sourceID: sourceID, preserveRuntimeTarget: true}
	}
}

// retryStart launches the replay once the swap has landed. A named rung goes
// through the /tN machinery — probe first, one turn there, then restore —
// because a retry elsewhere is exactly a one-shot override with a recorded
// prompt.
func (m *tuiModel) retryStart(msg retryStartMsg) tea.Cmd {
	// Production continuations arrive with planning ownership claimed by
	// onSessionSwap. The zero-generation branch keeps direct legacy/test calls
	// fail-closed without weakening that owned boundary.
	if msg.generation == 0 {
		if !m.busy {
			return noticeCmd("warn", "retry continuation arrived without turn ownership")
		}
		return noticeCmd("warn", "a turn started before the retry could; /retry again when it finishes")
	}
	if msg.generation != m.turnGeneration || !m.turnPlanning || m.turnCtx == nil {
		return nil
	}
	refuse := func(text string) tea.Cmd {
		m.finishPlanning()
		m.addNotice("error", text)
		return m.nextQueuedTurn()
	}
	opening, err := stampRecordedTurnOpening(m.app.loop.Session, msg.opening)
	if err != nil {
		return refuse("retry refused: " + err.Error())
	}
	prompt := opening.AuthoredText()
	if prompt == "" {
		return refuse("retry refused: the recorded opening carries no authored text")
	}
	m.addUser(prompt)
	if msg.tier != "" && msg.tier != m.app.tier.ID {
		tier, ok := m.app.config.Tier(msg.tier)
		if !ok {
			return refuse("no tier " + msg.tier + " is configured; try /tiers")
		}
		app := m.app
		ctx, generation := m.turnCtx, msg.generation
		sticky := app.sticky
		return func() tea.Msg {
			result := overrideProbeMsg{generation: generation, opening: opening, prompt: prompt}
			plan := prospectiveTurnPlan(app.loop, sticky, opening, app.workspace)
			result.plan = plan
			rank := app.rankOf(tier)
			if rank < 0 {
				result.err = fmt.Errorf("the requested tier %s is not on the configured ladder", tier.ID)
				return result
			}
			probed, client, note, err := app.providers.probeTierFallbackFeasible(ctx, tier, func(candidate config.Tier) error {
				return checkTurnFeasible(app.loop, app.catalog, app.providers, app.budget, app.config.Destinations, candidate, rank, plan, opening)
			})
			if err != nil {
				result.err = fmt.Errorf("the requested tier %s cannot serve the turn: %w", tier.ID, err)
				return result
			}
			if err := ctx.Err(); err != nil {
				result.err = err
				return result
			}
			retargetTurnPlan(&plan, app.loop, app.catalog, app.caches, probed, rank, opening)
			result.plan = plan
			result.tier, result.client, result.note = probed, client, note
			return result
		}
	}
	m.turnPlanning = false
	m.beginTurn(prompt)
	m.launchModelTurn(opening)
	return m.spin.Tick
}

// lastTurnOpening finds the user message that opened the final turn. Not
// every user-role message opens one: advice and watch reports inject as
// user-role mid-turn, and the log marks them Injected for exactly this
// reader. A user message behind a tool-result tail is otherwise a real
// opening — a cancelled or round-limited turn ends on its results, and the
// next prompt follows them — except in a log written before the marker
// existed, where an injection is only recognizable by the label it leads
// with.
func lastTurnOpening(messages []provider.Message) int {
	last := -1
	for idx, msg := range messages {
		if msg.Role != provider.RoleUser || msg.Injected {
			continue
		}
		if idx > 0 && messages[idx-1].Role == provider.RoleTool && injectionShaped(msg) {
			continue
		}
		last = idx
	}
	return last
}

// injectionShaped recognizes an unmarked injection by the label its text
// leads with. Watch folds ride behind the typed prompt, so an opening never
// leads with "[watch]"; an advice fold does lead with "[advisor]", and an
// opening carrying one behind a tool-result tail — an interrupted turn,
// then a prompt typed over pending advice, in a log too old to carry the
// marker — is misread as an injection. That corner falls back to an earlier
// opening, and the source session /retry never writes is the recovery.
func injectionShaped(msg provider.Message) bool {
	text := msg.AuthoredText()
	return strings.HasPrefix(text, "[advisor]") || strings.HasPrefix(text, "[watch]")
}

func retryUndoRefusal(restored, removed int, skipped, failed []string, undoErr error) string {
	parts := []string{fmt.Sprintf("retry stopped before another model ran: file restore was incomplete (%d restored, %d removed)", restored, removed)}
	if undoErr != nil {
		parts = append(parts, "undo error: "+undoErr.Error())
	}
	if len(skipped) > 0 {
		parts = append(parts, "not checkpointed: "+boundedRetryPaths(skipped))
	}
	if len(failed) > 0 {
		parts = append(parts, "not restored or not fully verified: "+boundedRetryPaths(failed))
	}
	return strings.Join(parts, "; ")
}

func boundedRetryPaths(paths []string) string {
	const limit = 4
	shown := paths
	if len(shown) > limit {
		shown = shown[:limit]
	}
	text := strings.Join(shown, ", ")
	if hidden := len(paths) - len(shown); hidden > 0 {
		text += fmt.Sprintf(" (+%d more)", hidden)
	}
	return text
}

// checkpointLabel mirrors what the recorder files a turn under, so the
// stack-top comparison compares like with like.
func checkpointLabel(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}
