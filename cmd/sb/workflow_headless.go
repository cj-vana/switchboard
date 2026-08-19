package main

// Running a workflow without a terminal.
//
// The stages were decided when the file was written, so a workflow asks
// nothing of a person while it runs — which makes it the one thing on this
// surface that is genuinely scriptable. It is also the only way to exercise
// the real path against a real ladder without a TUI, which is the reason it
// exists at all: a feature whose only entrance is an interactive one is a
// feature nobody can test.

import (
	"context"
	"fmt"
	"strings"
)

func runHeadlessWorkflow(ctx context.Context, out *renderer, args string) error {
	// The renderer buffers, and this path returns straight to main rather than
	// through the REPL's own teardown, so every exit has to flush or the run
	// produces its answers and prints none of them.
	defer out.flush()

	name, arguments, _ := strings.Cut(strings.TrimSpace(args), " ")
	arguments = strings.TrimSpace(arguments)

	workflow, ok := findWorkflow(name)
	if !ok {
		var names []string
		for _, w := range subagentWorkflows {
			names = append(names, w.Name)
		}
		if len(names) == 0 {
			return fmt.Errorf("no workflows are defined; write one to .switchboard/workflows/<name>.toml")
		}
		return fmt.Errorf("no workflow %q; this workspace has %s", name, strings.Join(names, ", "))
	}
	runner := subagentRunner.get()
	if runner == nil {
		return fmt.Errorf("the subagent runner is not assembled; delegation is unavailable")
	}

	out.line(out.style(dim, fmt.Sprintf("workflow %s · %d stage(s)", workflow.Name, len(workflow.Stages))))
	result := runner.RunWorkflow(ctx, workflow, arguments, func(text string) {
		out.line(out.style(dim, "  "+text))
	})

	for _, stage := range result.Stages {
		out.line("")
		out.line(out.style(bold, stage.Stage) +
			out.style(dim, fmt.Sprintf("  %d answered, %d failed", len(stage.Answers), len(stage.Failed))))
		for _, answer := range stage.Answers {
			out.line(answer)
		}
		for _, failed := range stage.Failed {
			out.line(out.style(dim, "  failed: "+firstLine(failed)))
		}
	}
	switch {
	case result.Canceled:
		return fmt.Errorf("workflow %s was cancelled after %d stage(s)", workflow.Name, len(result.Stages))
	case result.Err != nil:
		return fmt.Errorf("workflow %s stopped: %w", workflow.Name, result.Err)
	}
	return nil
}
