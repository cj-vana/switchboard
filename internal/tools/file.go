package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cjvana/switchboard/internal/permission"
)

const maxReadBytes = 256 << 10

type readTool struct{ r *Registry }

func (t *readTool) Name() string { return "read" }

func (t *readTool) Description() string {
	return "Read a UTF-8 text file from the workspace. Returns the file's exact bytes with " +
		"no line numbers or other decoration, so text taken from a read can be pasted " +
		"straight into edit's old_string. Use offset and limit, counted in lines, for " +
		"large files."
}

func (t *readTool) ParallelSafe() bool { return true }

func (t *readTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path, absolute or relative to the workspace root."},
    "offset": {"type": "integer", "description": "First line to return, 1-based. Defaults to the start of the file."},
    "limit": {"type": "integer", "description": "How many lines to return. Defaults to the rest of the file."}
  },
  "required": ["path"]
}`)
}

type readInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func (t *readTool) Plan(input json.RawMessage) (Plan, error) {
	var in readInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("read: %w", err)
	}
	abs, err := t.r.resolve(in.Path)
	if err != nil {
		return Plan{}, err
	}

	return Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectRead, Path: t.r.display(abs)},
		Run: func(context.Context) (Result, error) {
			return t.read(abs, in)
		},
	}, nil
}

func (t *readTool) read(abs string, in readInput) (Result, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return errorf("cannot read %s: %v", t.r.display(abs), err)
	}
	if info.IsDir() {
		return errorf("%s is a directory", t.r.display(abs))
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return errorf("cannot read %s: %v", t.r.display(abs), err)
	}

	// The hash covers the whole file even when only a slice is returned. A
	// partial read still tells the agent what version it saw, and a write must
	// be checked against the file as a whole.
	t.r.versions.record(abs, hashContent(data))

	if len(data) > maxReadBytes && in.Limit == 0 {
		return errorf("%s is %d bytes, over the %d byte limit for a whole-file read; "+
			"use offset and limit", t.r.display(abs), len(data), maxReadBytes)
	}

	content := string(data)
	if in.Offset <= 0 && in.Limit <= 0 {
		if content == "" {
			return Result{Content: fmt.Sprintf("%s is empty", t.r.display(abs))}, nil
		}
		return Result{Content: content}, nil
	}

	lines := strings.Split(content, "\n")
	start := max(in.Offset-1, 0)
	if start >= len(lines) {
		return errorf("%s has %d lines; offset %d is past the end", t.r.display(abs), len(lines), in.Offset)
	}
	end := len(lines)
	if in.Limit > 0 && start+in.Limit < end {
		end = start + in.Limit
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[lines %d-%d of %d]\n", start+1, end, len(lines))
	b.WriteString(strings.Join(lines[start:end], "\n"))
	return Result{Content: b.String()}, nil
}

type writeTool struct{ r *Registry }

func (t *writeTool) Name() string { return "write" }

func (t *writeTool) Description() string {
	return "Write a whole file, creating it or replacing its contents. An existing file must " +
		"have been read first in this session, and the write fails if it changed since " +
		"that read. Prefer edit for changing part of a file."
}

func (t *writeTool) ParallelSafe() bool { return false }

func (t *writeTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path, absolute or relative to the workspace root."},
    "content": {"type": "string", "description": "The complete new contents of the file."}
  },
  "required": ["path", "content"]
}`)
}

type writeInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *writeTool) Plan(input json.RawMessage) (Plan, error) {
	var in writeInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("write: %w", err)
	}
	abs, err := t.r.resolve(in.Path)
	if err != nil {
		return Plan{}, err
	}

	return Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectWrite, Path: t.r.display(abs)},
		Run: func(context.Context) (Result, error) {
			if res, ok := t.r.checkStale(abs, true); !ok {
				return res, nil
			}
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return errorf("cannot create directory for %s: %v", t.r.display(abs), err)
			}
			if err := os.WriteFile(abs, []byte(in.Content), 0o644); err != nil {
				return errorf("cannot write %s: %v", t.r.display(abs), err)
			}
			t.r.versions.record(abs, hashContent([]byte(in.Content)))
			return Result{Content: fmt.Sprintf("wrote %s (%d bytes)", t.r.display(abs), len(in.Content))}, nil
		},
	}, nil
}

type editTool struct{ r *Registry }

func (t *editTool) Name() string { return "edit" }

func (t *editTool) Description() string {
	return "Replace an exact string in a file. old_string must appear exactly once unless " +
		"replace_all is set, and must match the file byte for byte including indentation. " +
		"The file must have been read first in this session, and the edit fails if it " +
		"changed since that read."
}

func (t *editTool) ParallelSafe() bool { return false }

func (t *editTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path, absolute or relative to the workspace root."},
    "old_string": {"type": "string", "description": "Exact text to replace, including surrounding context to make it unique."},
    "new_string": {"type": "string", "description": "Replacement text. Use an empty string to delete."},
    "replace_all": {"type": "boolean", "description": "Replace every occurrence instead of requiring exactly one."}
  },
  "required": ["path", "old_string", "new_string"]
}`)
}

type editInput struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (t *editTool) Plan(input json.RawMessage) (Plan, error) {
	var in editInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("edit: %w", err)
	}
	if in.OldString == "" {
		return Plan{}, fmt.Errorf("edit: old_string is empty; use write to create a file")
	}
	if in.OldString == in.NewString {
		return Plan{}, fmt.Errorf("edit: old_string and new_string are identical")
	}
	abs, err := t.r.resolve(in.Path)
	if err != nil {
		return Plan{}, err
	}

	return Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectWrite, Path: t.r.display(abs)},
		Run: func(context.Context) (Result, error) {
			return t.edit(abs, in)
		},
	}, nil
}

func (t *editTool) edit(abs string, in editInput) (Result, error) {
	if res, ok := t.r.checkStale(abs, false); !ok {
		return res, nil
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return errorf("cannot read %s: %v", t.r.display(abs), err)
	}
	content := string(data)

	count := strings.Count(content, in.OldString)
	switch {
	case count == 0:
		return errorf("old_string was not found in %s. Read the file again: it must match "+
			"byte for byte, including indentation and line endings.", t.r.display(abs))
	case count > 1 && !in.ReplaceAll:
		return errorf("old_string appears %d times in %s. Add surrounding context to make it "+
			"unique, or set replace_all.", count, t.r.display(abs))
	}

	updated := content
	if in.ReplaceAll {
		updated = strings.ReplaceAll(content, in.OldString, in.NewString)
	} else {
		updated = strings.Replace(content, in.OldString, in.NewString, 1)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return errorf("cannot stat %s: %v", t.r.display(abs), err)
	}
	if err := os.WriteFile(abs, []byte(updated), info.Mode().Perm()); err != nil {
		return errorf("cannot write %s: %v", t.r.display(abs), err)
	}
	t.r.versions.record(abs, hashContent([]byte(updated)))

	replaced := 1
	if in.ReplaceAll {
		replaced = count
	}
	return Result{Content: fmt.Sprintf("edited %s (%d replacement(s))", t.r.display(abs), replaced)}, nil
}

// checkStale enforces the read-before-write contract. It returns false along
// with the message to hand back to the model.
//
// Line-number addressing is excluded from this suite for the same reason this
// check exists: anything that touches the workspace concurrently invalidates
// the agent's picture of the file, and a positional edit corrupts it silently
// where a content check refuses (§10.2).
func (r *Registry) checkStale(abs string, allowMissing bool) (Result, bool) {
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) && allowMissing {
			return Result{}, true
		}
		res, _ := errorf("cannot read %s: %v", r.display(abs), err)
		return res, false
	}

	current := hashContent(data)
	recorded, ok := r.versions.get(abs)
	if !ok {
		res, _ := errorf("%s exists but has not been read in this session. Read it first so "+
			"the change is made against its current contents.", r.display(abs))
		return res, false
	}
	if recorded != current {
		// Drop the stale version so the next attempt cannot succeed without a
		// fresh read.
		r.versions.forget(abs)
		res, _ := errorf("%s changed since it was read. Read it again before writing.", r.display(abs))
		return res, false
	}
	return Result{}, true
}
