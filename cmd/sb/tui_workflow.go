package main

// /workflow: a multi-agent script, written to a file and run by name.
//
// It is a slash command rather than a tool on purpose. A model that could
// invoke a workflow would need its stages, rungs, and fan-out carried in the
// frozen zone, paid for on every cold cache of every session, to describe work
// the user already decided when they wrote the file. As a command it costs the
// cached prefix nothing at all.
//
// It runs on the exclusive operation lane, the same one /compact and /learn
// use, so the model is not executing while a workflow does. That is what keeps
// the loop's tool-result barrier untouched: no workflow task is ever a tool
// call waiting to return.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/delegate"
)

func cmdWorkflow(m *tuiModel, args string) tea.Cmd {
	verb, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	rest = strings.TrimSpace(rest)

	switch verb {
	case "", "list":
		return workflowList(m)
	case "show":
		return workflowShow(m, rest)
	case "run":
		return workflowRun(m, rest)
	}
	return noticeCmd("warn", "usage: /workflow [list] · /workflow show <name> · /workflow run <name> [arguments]")
}

func workflowList(m *tuiModel) tea.Cmd {
	if len(subagentWorkflows) == 0 {
		m.addInfo("no workflows defined\n" +
			"  a workflow is stages of subagent tasks in a file, run in order:\n" +
			"  .switchboard/workflows/<name>.toml, or ~/.switchboard/workflows/<name>.toml\n\n" +
			"    [[stage]]\n" +
			"    name = \"survey\"\n" +
			"    [[stage.task]]\n" +
			"    task = \"List every call site of X with file:line.\"\n\n" +
			"    [[stage]]\n" +
			"    name = \"propose\"\n" +
			"    carry = true\n" +
			"    [[stage.task]]\n" +
			"    tier = \"t2\"\n" +
			"    task = \"Given the survey, propose the minimal edit.\"\n\n" +
			"  at most " + fmt.Sprint(delegate.MaxWorkflowStages) + " stages, " +
			fmt.Sprint(delegate.MaxTasksPerStage) + " tasks per stage, " +
			fmt.Sprint(delegate.MaxTasksPerWorkflow) + " tasks in all")
		return nil
	}
	var b strings.Builder
	b.WriteString("workflows\n")
	for _, wf := range subagentWorkflows {
		where := "project"
		if wf.FromHome {
			where = "user"
		}
		fmt.Fprintf(&b, "\n  %-16s %s\n", workspaceSanitize(wf.Name), workspaceSanitize(wf.Description))
		fmt.Fprintf(&b, "    %d stage(s) · %s · %s\n", len(wf.Stages), where, m.app.displayPath(wf.Path))
	}
	b.WriteString("\n  /workflow show <name> reads one · /workflow run <name> runs it")
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

func workflowShow(m *tuiModel, name string) tea.Cmd {
	wf, ok := findWorkflow(name)
	if !ok {
		return noticeCmd("warn", "no workflow "+workspaceSanitize(name)+"; /workflow list shows what is defined")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n", workspaceSanitize(wf.Name), workspaceSanitize(wf.Description))
	for i, stage := range wf.Stages {
		carried := ""
		if stage.Carry {
			carried = " · carries the previous stage's answers"
		}
		fmt.Fprintf(&b, "\n  %d. %s%s\n", i+1, workspaceSanitize(stage.Name), carried)
		for _, task := range stage.Tasks {
			where := task.Tier
			if task.Agent != "" {
				where = task.Agent
				if task.Tier != "" {
					where += " on " + task.Tier
				}
			}
			if where == "" {
				where = "the bottom rung"
			}
			fmt.Fprintf(&b, "     [%s] %s\n", workspaceSanitize(where), workspaceSanitize(firstLine(task.Task)))
		}
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

func findWorkflow(name string) (delegate.Workflow, bool) {
	for _, wf := range subagentWorkflows {
		if wf.Name == name {
			return wf, true
		}
	}
	return delegate.Workflow{}, false
}

// workflowDoneMsg carries a finished run back to the UI goroutine. It rides
// the operation lane's own completion path, so a run whose session was swapped
// underneath it is discarded rather than applied to the wrong transcript.
type workflowDoneMsg struct {
	generation uint64
	sourceID   string
	name       string
	result     delegate.WorkflowResult
}

func workflowRun(m *tuiModel, args string) tea.Cmd {
	name, arguments, _ := strings.Cut(args, " ")
	arguments = strings.TrimSpace(arguments)
	wf, ok := findWorkflow(name)
	if !ok {
		return noticeCmd("warn", "no workflow "+workspaceSanitize(name)+"; /workflow list shows what is defined")
	}
	runner := subagentRunner.get()
	if runner == nil {
		return noticeCmd("error", "the subagent runner is not assembled; delegation is unavailable in this session")
	}

	ctx, generation, sourceID, err := m.startOperation("workflow " + wf.Name)
	if err != nil {
		return noticeCmd("warn", err.Error())
	}
	m.addInfo("running workflow " + wf.Name + "\n  " + fmt.Sprint(len(wf.Stages)) +
		" stage(s) · /tasks watches them · /tasks steer <id> corrects one · esc cancels the run")

	program := m.app.p
	return func() tea.Msg {
		result := runner.RunWorkflow(ctx, wf, arguments, func(text string) {
			if program != nil {
				program.Send(noticeMsg{text: "  " + text})
			}
		})
		return workflowDoneMsg{generation: generation, sourceID: sourceID, name: wf.Name, result: result}
	}
}

// onWorkflowDone renders a finished run and releases the lane.
func (m *tuiModel) onWorkflowDone(msg workflowDoneMsg) tea.Cmd {
	if !m.finishOperation(msg.generation, false) {
		// The session was swapped or the operation was already released; the
		// answers belong to a transcript that is no longer here.
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "workflow %s\n", workspaceSanitize(msg.name))
	for _, stage := range msg.result.Stages {
		fmt.Fprintf(&b, "\n  %s — %d answered, %d failed\n",
			workspaceSanitize(stage.Stage), len(stage.Answers), len(stage.Failed))
		for _, answer := range stage.Answers {
			fmt.Fprintf(&b, "\n%s\n", workspaceSanitize(answer))
		}
		for _, failed := range stage.Failed {
			fmt.Fprintf(&b, "\n  failed: %s\n", workspaceSanitize(firstLine(failed)))
		}
	}
	switch {
	case msg.result.Canceled:
		// The finished stages are kept. The work was done and paid for, and
		// discarding it would make cancelling more expensive than waiting.
		b.WriteString("\n  cancelled; the stages above finished before it stopped")
	case msg.result.Err != nil:
		fmt.Fprintf(&b, "\n  stopped: %s", workspaceSanitize(msg.result.Err.Error()))
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	m.refreshCost(m.app.loop.Session.State())
	return nil
}
