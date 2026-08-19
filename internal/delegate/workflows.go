package delegate

// Workflows: a multi-agent script written to a file and run by name.
//
// The shape is deliberately not a tool. A model that could invoke a workflow
// would need its stages, its rungs, and its fan-out in the frozen zone, paid
// for on every cold cache of every session, to describe work the user already
// decided on when they wrote the file. So a workflow is a slash command, its
// definitions never enter a tool description, and the whole feature costs the
// cached prefix nothing.
//
// What it buys over typing the same delegate calls by hand is order and
// carry: stages run in sequence, tasks inside a stage run together, and a
// stage can be handed what the last one answered. That is the part a model
// improvises badly and a file states exactly.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// The caps are validated at load rather than at run, so a definition that
// cannot execute fails when it is read and names its reason, instead of
// failing halfway through a run that has already spent money.
//
// Four matches DefaultMaxParallel: the declared width equals the width that
// can actually execute, so a stage of four is a stage of four rather than a
// queue of four pretending to be parallel.
const (
	MaxWorkflowStages    = 4
	MaxTasksPerStage     = 4
	MaxTasksPerWorkflow  = 8
	MaxCarriedAnswerRune = 1200
)

// Workflow is one loaded definition.
type Workflow struct {
	Name        string
	Description string
	Stages      []Stage

	// FromHome records which directory supplied it, the same trust statement
	// agents and custom commands carry: ~/.switchboard is the user speaking,
	// a repository's .switchboard is whoever was cloned.
	FromHome bool
	Path     string
}

// Stage is a set of tasks that run together, before the next stage starts.
type Stage struct {
	Name  string
	Tasks []WorkflowTask

	// Carry prepends the previous stage's answers to every task in this one.
	// Off by default: a stage that does not need the last one's output should
	// not pay for it in context, and most second stages do not.
	Carry bool
}

// WorkflowTask is one errand in a stage.
type WorkflowTask struct {
	Task  string
	Tier  string
	Agent string
}

type workflowFile struct {
	Description string `toml:"description"`
	Stage       []struct {
		Name  string `toml:"name"`
		Carry bool   `toml:"carry"`
		Task  []struct {
			Task  string `toml:"task"`
			Tier  string `toml:"tier"`
			Agent string `toml:"agent"`
		} `toml:"task"`
	} `toml:"stage"`
}

// LoadWorkflows reads both directories once, project first, mirroring how
// agents and custom commands resolve a name clash. A broken definition is
// skipped with a note rather than failing the session: one unparseable file
// should not cost the user the workflows that do parse.
func LoadWorkflows(workspace string) (workflows []Workflow, notes []string) {
	type source struct {
		dir      string
		fromHome bool
	}
	dirs := []source{{filepath.Join(workspace, ".switchboard", "workflows"), false}}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, source{filepath.Join(home, ".switchboard", "workflows"), true})
	}

	seen := map[string]bool{}
	for _, src := range dirs {
		entries, err := os.ReadDir(src.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
				continue
			}
			path := filepath.Join(src.dir, e.Name())
			name := strings.TrimSuffix(e.Name(), ".toml")
			if seen[name] {
				continue // project spoke first
			}
			wf, err := parseWorkflow(name, path)
			if err != nil {
				notes = append(notes, fmt.Sprintf("workflow %s: %v", path, err))
				continue
			}
			seen[name] = true
			wf.FromHome = src.fromHome
			workflows = append(workflows, wf)
		}
	}
	sort.Slice(workflows, func(i, j int) bool { return workflows[i].Name < workflows[j].Name })
	return workflows, notes
}

func parseWorkflow(name, path string) (Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Workflow{}, err
	}
	var f workflowFile
	meta, err := toml.Decode(string(data), &f)
	if err != nil {
		return Workflow{}, err
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		// The same posture the config file takes: a misspelled key silently
		// ignored is a setting the author believes is in effect.
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return Workflow{}, fmt.Errorf("unrecognized settings: %s", strings.Join(keys, ", "))
	}

	wf := Workflow{Name: name, Description: f.Description, Path: path}
	if len(f.Stage) == 0 {
		return Workflow{}, fmt.Errorf("has no stages; a [[stage]] section declares one")
	}
	if len(f.Stage) > MaxWorkflowStages {
		return Workflow{}, fmt.Errorf("has %d stages, more than the %d ceiling", len(f.Stage), MaxWorkflowStages)
	}
	total := 0
	for i, stage := range f.Stage {
		label := stage.Name
		if label == "" {
			label = fmt.Sprintf("stage %d", i+1)
		}
		if len(stage.Task) == 0 {
			return Workflow{}, fmt.Errorf("%s has no tasks", label)
		}
		if len(stage.Task) > MaxTasksPerStage {
			return Workflow{}, fmt.Errorf("%s has %d tasks, more than the %d that can run at once",
				label, len(stage.Task), MaxTasksPerStage)
		}
		if i == 0 && stage.Carry {
			return Workflow{}, fmt.Errorf("%s carries, but nothing ran before it", label)
		}
		out := Stage{Name: label, Carry: stage.Carry}
		for j, task := range stage.Task {
			if strings.TrimSpace(task.Task) == "" {
				return Workflow{}, fmt.Errorf("%s task %d has no task text", label, j+1)
			}
			out.Tasks = append(out.Tasks, WorkflowTask{Task: task.Task, Tier: task.Tier, Agent: task.Agent})
			total++
		}
		wf.Stages = append(wf.Stages, out)
	}
	if total > MaxTasksPerWorkflow {
		return Workflow{}, fmt.Errorf("has %d tasks, more than the %d ceiling", total, MaxTasksPerWorkflow)
	}
	return wf, nil
}

// Carry folds a stage's answers into the next stage's task text. Each answer
// is truncated, because a stage that fans out to four and carries everything
// hands the next stage four transcripts, and the next stage has to read them
// on every one of its own calls.
func Carry(answers []string, task string) string {
	if len(answers) == 0 {
		return task
	}
	var b strings.Builder
	b.WriteString("Results from the previous stage:\n\n")
	for i, answer := range answers {
		if runes := []rune(answer); len(runes) > MaxCarriedAnswerRune {
			answer = string(runes[:MaxCarriedAnswerRune]) + "\n[truncated]"
		}
		fmt.Fprintf(&b, "--- result %d ---\n%s\n\n", i+1, answer)
	}
	b.WriteString("Your task:\n")
	b.WriteString(task)
	return b.String()
}
