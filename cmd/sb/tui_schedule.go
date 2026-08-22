package main

// /every, /at, and /schedule: the user's own reminders and recurring prompts.
// The ledger (internal/schedule) persists per workspace and fires only while
// sb runs — there is deliberately no daemon — so an entry whose moment passed
// while the process was down fires once at the next startup, and a recurring
// one reschedules from then rather than catching up every tick it missed.
//
// A fired entry opens an ordinary user turn through enqueue, prefixed
// "[scheduled sN] " so the transcript and the model can tell it from typed
// text. It is a turn's opening, never the injection seam: /retry's opening
// detection depends on that distinction. The content was approved when the
// entry was armed — the secret gate runs at creation — so the schedule side
// adds no gate of its own at fire time beyond the outbound scan every send
// passes.

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/schedule"
)

// schedulePoll is how often the ledger is asked what is due: finer than the
// minute floor entries arm with, coarser than anything a reminder can
// meaningfully keep.
const schedulePoll = 5 * time.Second

type scheduleTickMsg struct{}

func scheduleTick() tea.Cmd {
	return tea.Tick(schedulePoll, func(time.Time) tea.Msg { return scheduleTickMsg{} })
}

// fireScheduled drains what is due into ordinary turns, one entry per tick.
// The tick is re-armed unconditionally: a busy turn delays a fire into the
// queue rather than dropping it, and a clock that stopped because one fire
// failed would silently disarm every later entry. One per tick is what keeps
// a multi-due moment — startup catch-up above all — from stacking fires on
// top of a dialog or a turn that has not started yet; the rest stay armed
// for the next tick.
func (m *tuiModel) fireScheduled() tea.Cmd {
	tick := scheduleTick()
	if m.app.schedules == nil {
		return tick
	}
	// A dialog owns the input zone — a permission ask, a picker, the secret
	// gate itself — and firing under one would either replace it or start a
	// turn the user cannot see begin. The entry stays armed; the next tick
	// takes it.
	if m.dlg != nil {
		return tick
	}
	due, err := m.app.schedules.TakeDue(time.Now(), 1)
	if err != nil {
		m.addNotice("warn", "the schedule ledger did not save; a fired entry may repeat at next launch: "+err.Error())
	}
	if len(due) == 0 {
		return tick
	}
	e := due[0]
	return tea.Batch(tick, m.enqueue("[scheduled "+e.ID+"] "+e.Prompt, ""))
}

func cmdEvery(m *tuiModel, args string) tea.Cmd {
	spec, prompt, ok := splitScheduleSpec(args)
	if !ok {
		return noticeCmd("warn", "usage: /every <interval> <prompt>, e.g. /every 30m run the tests")
	}
	d, err := time.ParseDuration(spec)
	if err != nil {
		return noticeCmd("warn", "/every takes an interval first, like 30m or 2h")
	}
	if d < schedule.MinEvery {
		return noticeCmd("warn", "the shortest interval is "+schedule.MinEvery.String()+"; anything tighter is a loop, not a reminder")
	}
	return m.armSchedule(schedule.Entry{Every: d, Prompt: prompt})
}

func cmdAt(m *tuiModel, args string) tea.Cmd {
	spec, prompt, ok := splitScheduleSpec(args)
	if !ok {
		return noticeCmd("warn", "usage: /at <HH:MM> <prompt>, e.g. /at 14:30 check the deploy")
	}
	if _, err := time.Parse("15:04", spec); err != nil {
		return noticeCmd("warn", "/at takes a 24-hour local clock time first, like 14:30")
	}
	return m.armSchedule(schedule.Entry{At: spec, Prompt: prompt})
}

// armSchedule holds a new entry behind the storage form of the secret gate:
// the armed prompt sits at rest in the ledger and refires later, so "send as
// typed" is not an answer — redact arms the redacted copy, anything else
// stores nothing.
func (m *tuiModel) armSchedule(e schedule.Entry) tea.Cmd {
	if m.app.schedules == nil {
		return noticeCmd("warn", "schedules are unavailable"+m.app.schedulesErr)
	}
	if leaks := credential.ScanPrompt(e.Prompt); len(leaks) > 0 {
		return m.openSecretGateForStorage(leaks, e.Prompt, func(p string) tea.Cmd {
			e.Prompt = p
			return m.commitSchedule(e)
		})
	}
	return m.commitSchedule(e)
}

func (m *tuiModel) commitSchedule(e schedule.Entry) tea.Cmd {
	added, err := m.app.schedules.Add(e)
	if err != nil {
		return noticeCmd("warn", err.Error())
	}
	return noticeCmd("", "armed "+added.ID+": "+scheduleLine(added, time.Now()))
}

func cmdSchedule(m *tuiModel, args string) tea.Cmd {
	if m.app.schedules == nil {
		return noticeCmd("warn", "schedules are unavailable"+m.app.schedulesErr)
	}
	fields := strings.Fields(args)
	if len(fields) > 0 {
		if len(fields) != 2 || fields[0] != "cancel" {
			return noticeCmd("warn", "usage: /schedule [cancel <id>]")
		}
		if m.app.schedules.Cancel(fields[1]) {
			return noticeCmd("", "cancelled "+fields[1]+"; the rest of the schedule is untouched")
		}
		return noticeCmd("warn", "no scheduled entry "+fields[1]+"; /schedule lists them")
	}
	entries := m.app.schedules.List()
	if len(entries) == 0 {
		return noticeCmd("", "nothing scheduled; /every <interval> <prompt> recurs, /at <HH:MM> <prompt> fires once")
	}
	var b strings.Builder
	now := time.Now()
	for _, e := range entries {
		b.WriteString(scheduleLine(e, now) + "\n")
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}
