package skills

// The skill tool. Its description enumerates what loaded — name and
// one-line description each — so knowing what exists costs the frozen zone
// a few lines, and a body costs tokens only in the sessions that ask for
// it. Serving is read-effect: the bodies were read at assembly, and a
// supporting file is served from the skill's own directory and nowhere
// else, so a pack can carry references beside its SKILL.md — including
// packs living under ~/.switchboard, which the workspace-rooted read tool
// could not reach.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/tools"
)

type skillTool struct {
	byName map[string]Skill
	names  []string
}

// NewTool builds the tool over the loaded set. The caller decided the set at
// session assembly; with no skills it should register nothing, keeping the
// schemas byte-identical to a build without the feature — that absence is
// the cache promise, and the test that pins it lives beside this package's.
func NewTool(list []Skill) tools.Tool {
	t := &skillTool{byName: map[string]Skill{}}
	for _, sk := range list {
		t.byName[sk.Name] = sk
		t.names = append(t.names, sk.Name)
	}
	return t
}

func (t *skillTool) Name() string { return "skill" }

func (t *skillTool) Description() string {
	var b strings.Builder
	b.WriteString("Load a skill: standing instructions for a kind of task, written by the user. " +
		"When a task matches a skill's description, call this before doing the work and follow " +
		"what it says. The body may reference supporting files beside it; pass file to fetch one. " +
		"Available skills:\n")
	for _, name := range t.names {
		b.WriteString("- " + name + ": " + t.byName[name].Description + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ParallelSafe: serving is memory and read-only files, no shared state.
func (t *skillTool) ParallelSafe() bool { return true }

func (t *skillTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "The skill to load, from the available list."},
    "file": {"type": "string", "description": "A supporting file the skill references, relative to the skill's own directory, e.g. references/style.md. Omit for the skill itself."}
  },
  "required": ["name"]
}`)
}

type skillInput struct {
	Name string `json:"name"`
	File string `json:"file"`
}

func (t *skillTool) Plan(input json.RawMessage) (tools.Plan, error) {
	var in skillInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Plan{}, fmt.Errorf("skill: %w", err)
	}
	sk, ok := t.byName[in.Name]
	if !ok {
		return tools.Plan{}, fmt.Errorf("skill: no skill named %q; the available ones are listed in this tool's description", in.Name)
	}

	detail := in.Name
	if in.File != "" {
		detail += " " + in.File
	}
	return tools.Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectRead, Detail: detail},
		Run: func(ctx context.Context) (tools.Result, error) {
			if in.File != "" {
				return serveFile(sk, in.File)
			}
			return tools.Result{Content: fmt.Sprintf("Skill %s, from %s:\n\n%s", sk.Name, sk.Dir, sk.Body)}, nil
		},
	}, nil
}

// serveFile answers with a file from the skill's directory and refuses
// everything else: the skill named its own references, not the filesystem.
// The comparison runs on resolved paths so a symlink cannot carry the read
// outside the directory the skill actually occupies.
func serveFile(sk Skill, rel string) (tools.Result, error) {
	if filepath.IsAbs(rel) {
		return tools.Result{Content: "skill files are relative to the skill's directory; " + rel + " is absolute", IsError: true}, nil
	}
	root, err := filepath.EvalSymlinks(sk.Dir)
	if err != nil {
		return tools.Result{Content: err.Error(), IsError: true}, nil
	}
	joined := filepath.Join(root, rel)
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		if os.IsNotExist(err) {
			return tools.Result{Content: rel + " does not exist in skill " + sk.Name + "'s directory", IsError: true}, nil
		}
		return tools.Result{Content: err.Error(), IsError: true}, nil
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return tools.Result{Content: rel + " leaves skill " + sk.Name + "'s directory, which this tool does not serve", IsError: true}, nil
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return tools.Result{Content: err.Error(), IsError: true}, nil
	}
	return tools.Result{Content: string(data)}, nil
}
