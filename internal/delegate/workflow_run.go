package delegate

// Executing a workflow.
//
// Stages run in order and their tasks run together, which is the whole
// contract: a stage is the barrier. The runner owns the goroutines and joins
// every stage before starting the next, so nothing outlives the call and
// TaskManager's promise that it starts no goroutines of its own stays true.

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// StageResult is what one stage produced, in task order.
type StageResult struct {
	Stage   string
	Answers []string
	Failed  []string
}

// WorkflowResult is the whole run, including a run that stopped early. A
// cancelled workflow returns the stages that finished rather than nothing:
// the work was done and paid for, and discarding it would make ctrl+c more
// expensive than waiting.
type WorkflowResult struct {
	Stages   []StageResult
	Canceled bool
	Err      error
}

// RunWorkflow executes every stage in order. progress is called as tasks
// start and finish so a surface can say what is happening; it may be nil.
func (r *Runner) RunWorkflow(ctx context.Context, wf Workflow, arguments string, progress func(string)) WorkflowResult {
	var out WorkflowResult
	say := func(text string) {
		if progress != nil {
			progress(text)
		}
	}

	var previous []string
	for _, stage := range wf.Stages {
		if err := ctx.Err(); err != nil {
			out.Canceled = true
			out.Err = err
			return out
		}
		say(fmt.Sprintf("stage %s: %d task(s)", stage.Name, len(stage.Tasks)))

		answers := make([]string, len(stage.Tasks))
		failures := make([]string, len(stage.Tasks))
		var wg sync.WaitGroup
		for i, task := range stage.Tasks {
			wg.Add(1)
			go func(i int, task WorkflowTask) {
				defer wg.Done()
				text := expandArguments(task.Task, arguments)
				if stage.Carry {
					text = Carry(previous, text)
				}
				spec, named, err := r.Resolve(RunSpec{
					Task: text, Tier: task.Tier, AgentName: task.Agent, Name: stage.Name,
				})
				if err != nil {
					failures[i] = err.Error()
					return
				}
				res, err := r.Run(ctx, spec, named, r.Reserve(spec))
				switch {
				case err != nil:
					failures[i] = err.Error()
				case res.IsError:
					failures[i] = res.Content
				default:
					answers[i] = res.Content
				}
			}(i, task)
		}
		wg.Wait()

		result := StageResult{Stage: stage.Name}
		for i := range stage.Tasks {
			if failures[i] != "" {
				result.Failed = append(result.Failed, failures[i])
				continue
			}
			result.Answers = append(result.Answers, answers[i])
		}
		out.Stages = append(out.Stages, result)

		if err := ctx.Err(); err != nil {
			out.Canceled = true
			out.Err = err
			return out
		}
		// A stage that produced nothing stops the run. Carrying an empty set
		// into the next stage would run it against instructions that promise
		// results and deliver none, which spends a rung to produce confusion.
		if len(result.Answers) == 0 {
			out.Err = fmt.Errorf("stage %s produced no answers", stage.Name)
			return out
		}
		previous = result.Answers
	}
	return out
}

// expandArguments substitutes what the user typed after the workflow name,
// the same two forms custom commands use.
func expandArguments(task, arguments string) string {
	task = strings.ReplaceAll(task, "$ARGUMENTS", arguments)
	fields := strings.Fields(arguments)
	for i := 9; i >= 1; i-- {
		value := ""
		if i <= len(fields) {
			value = fields[i-1]
		}
		task = strings.ReplaceAll(task, fmt.Sprintf("$%d", i), value)
	}
	return task
}
