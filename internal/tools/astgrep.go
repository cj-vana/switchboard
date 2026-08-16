package tools

// Structural search over syntax trees, backed by the user's own ast-grep
// binary. Text grep answers "where does this string appear" and drags back
// every comment and prose hit that shares the words; this answers "where
// does this shape appear" — a call with these arguments, a declaration of
// this kind — because the pattern matches whole syntax nodes. The binary is
// found once at session assembly and the tool is absent rather than broken
// when the machine lacks one.
//
// The §11 posture decides how it runs. Inside demonstrated confinement the
// call carries the read effect and runs wrapped — the same value that
// proved containment is the value that applies it, per the execution
// package's contract. Without confinement the call carries the execute
// effect and is approved like any other subprocess, because a binary the
// sandbox never held is a binary the user vouches for per call. It is
// never read-effect unwrapped.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
)

// astGrepMaxMatches bounds the formatted result the way grep's own cap
// does: a pattern loose enough to hit more has told the model to tighten
// the pattern, not to read more output.
const astGrepMaxMatches = 100

type astGrepTool struct {
	r      *Registry
	binary string
}

// NewAstGrep wires the tool to a resolved binary path. The caller looked
// the binary up at session assembly, so presence is decided once and the
// frozen zone never changes shape mid-session.
func NewAstGrep(r *Registry, binary string) Tool {
	return &astGrepTool{r: r, binary: binary}
}

func (t *astGrepTool) Name() string { return "astgrep" }

func (t *astGrepTool) Description() string {
	return "Search code structurally, by syntax tree rather than text. The pattern is code " +
		"with metavariables: $NAME matches one node, $$$NAME matches a list, so " +
		"fmt.Errorf($MSG, $$$ARGS) finds every such call whatever its spacing, and " +
		"comments or strings that merely mention the words never match. Prefer this " +
		"over grep when the target is a code shape; prefer grep for plain text. " +
		"lang names the pattern's language explicitly (go, javascript, typescript, " +
		"python, rust, ...) and is otherwise inferred per file extension. A pattern " +
		"that is one bare node can parse as the wrong kind — a Go one-argument call " +
		"parses as a type conversion, for instance, and then matches nothing. Give " +
		"such a pattern real surroundings and name the node you want with selector: " +
		"pattern 'x := fmt.Println($A)' with selector call_expression matches the " +
		"call alone."
}

// ParallelSafe: the binary only reads, and each invocation is its own
// process, so concurrent calls cannot interleave state.
func (t *astGrepTool) ParallelSafe() bool { return true }

func (t *astGrepTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "An ast-grep pattern: code with $VAR and $$$VARS metavariables."},
    "lang": {"type": "string", "description": "Language the pattern is written in, e.g. go. Inferred from file extensions when omitted."},
    "selector": {"type": "string", "description": "Tree-sitter node kind to match within the pattern, e.g. call_expression. Use with a pattern that carries surrounding context."},
    "path": {"type": "string", "description": "File or directory to search, relative to the workspace root. Defaults to the whole workspace."}
  },
  "required": ["pattern"]
}`)
}

type astGrepInput struct {
	Pattern  string `json:"pattern"`
	Lang     string `json:"lang"`
	Selector string `json:"selector"`
	Path     string `json:"path"`
}

// astGrepMatch is the slice of ast-grep's --json=compact output this tool
// reads. Captured against ast-grep 0.45.1; unknown fields are ignored so a
// newer binary that adds fields still parses, and a shape change that
// breaks these fields fails loudly in the JSON decoder rather than
// misreporting positions.
type astGrepMatch struct {
	File  string `json:"file"`
	Text  string `json:"text"`
	Range struct {
		Start struct {
			Line int `json:"line"` // zero-based
		} `json:"start"`
	} `json:"range"`
}

func (t *astGrepTool) Plan(input json.RawMessage) (Plan, error) {
	var in astGrepInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("astgrep: %w", err)
	}
	if strings.TrimSpace(in.Pattern) == "" {
		return Plan{}, fmt.Errorf("astgrep: pattern is required")
	}
	if in.Path == "" {
		in.Path = "."
	}
	abs, err := t.r.resolve(in.Path)
	if err != nil {
		return Plan{}, err
	}

	confine := t.r.capability.Confinement()
	effect := permission.EffectRead
	if confine == nil {
		effect = permission.EffectExecute
	}
	return Plan{
		Request: permission.Request{
			Tool:   t.Name(),
			Effect: effect,
			Path:   t.r.display(abs),
			Detail: in.Pattern,
		},
		Run: func(ctx context.Context) (Result, error) {
			return t.run(ctx, in, abs, confine)
		},
	}, nil
}

func (t *astGrepTool) run(ctx context.Context, in astGrepInput, abs string, confine *execution.Confinement) (Result, error) {
	argv := []string{t.binary, "run", "--pattern", in.Pattern, "--json=compact"}
	if in.Lang != "" {
		argv = append(argv, "--lang", in.Lang)
	}
	if in.Selector != "" {
		argv = append(argv, "--selector", in.Selector)
	}
	argv = append(argv, abs)

	res, err := execution.Run(ctx, execution.Command{
		Argv:    argv,
		Dir:     t.r.root,
		Confine: confine,
	})
	if err != nil {
		return Result{}, err
	}
	if res.TimedOut {
		return errorf("astgrep timed out; narrow the path or the pattern")
	}
	// Exit 1 is grep's own convention, matches-found-none, and the JSON
	// still arrives; only higher codes are failures.
	if res.ExitCode > 1 {
		return errorf("ast-grep failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Output))
	}
	if res.Truncated {
		// A truncated stream is not parseable JSON, and guessing at the cut
		// would misreport positions (§10.3): say so instead.
		return errorf("the match list was too large to read back; narrow the path or the pattern")
	}

	// The runner combines stdout and stderr, so the binary's warnings sit
	// beside the JSON. Compact output is one line starting with a bracket;
	// everything else is advice worth passing along on a miss, because
	// "your pattern did not parse cleanly" is the difference between
	// tightening a pattern and abandoning the tool.
	var jsonLine string
	var notes []string
	for line := range strings.Lines(res.Output) {
		line = strings.TrimRight(line, "\n")
		switch {
		case jsonLine == "" && strings.HasPrefix(line, "["):
			jsonLine = line
		case strings.TrimSpace(line) != "":
			notes = append(notes, line)
		}
	}
	if jsonLine == "" {
		return errorf("could not read ast-grep's output: %s", strings.TrimSpace(res.Output))
	}
	var matches []astGrepMatch
	if err := json.Unmarshal([]byte(jsonLine), &matches); err != nil {
		return errorf("could not read ast-grep's output: %v", err)
	}
	if len(matches) == 0 {
		msg := fmt.Sprintf("no structural matches for %s", in.Pattern)
		if len(notes) > 0 {
			msg += "\n" + strings.Join(notes, "\n")
		}
		return Result{Content: msg}, nil
	}

	var b strings.Builder
	shown := matches
	if len(shown) > astGrepMaxMatches {
		shown = shown[:astGrepMaxMatches]
	}
	for _, m := range shown {
		text := m.Text
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[:i] + " …"
		}
		fmt.Fprintf(&b, "%s:%d: %s\n", t.r.display(m.File), m.Range.Start.Line+1, text)
	}
	if len(matches) > astGrepMaxMatches {
		fmt.Fprintf(&b, "[%d more matches not shown; narrow the path or the pattern]\n", len(matches)-astGrepMaxMatches)
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}, nil
}
