package main

// Assembly for skills: discovery is once, here, because the descriptions
// ride the tool's schema into the frozen zone. With nothing discovered the
// tool is not registered at all, and the schemas stay byte-identical to a
// build without the feature — the cache promise the test on this file pins.

import (
	"fmt"

	"github.com/cj-vana/switchboard/internal/skills"
	"github.com/cj-vana/switchboard/internal/tools"
)

func addSkills(registry *tools.Registry, workspace string) ([]skills.Skill, []mcpNote) {
	list, loadNotes := skills.Load(workspace)
	var notes []mcpNote
	for _, n := range loadNotes {
		notes = append(notes, mcpNote{"warn", n})
	}
	if len(list) == 0 {
		return nil, notes
	}
	if err := registry.AddExternal(skills.NewTool(list)); err != nil {
		notes = append(notes, mcpNote{"warn", "skills unavailable: " + err.Error()})
		return nil, notes
	}
	notes = append(notes, mcpNote{"", fmt.Sprintf("skills: %d loaded", len(list))})
	return list, notes
}
