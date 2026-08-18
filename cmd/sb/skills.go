package main

// Assembly for skills: discovery is once, here, because the descriptions
// ride the tool's schema into the frozen zone. With nothing discovered the
// tool is not registered at all, and the schemas stay byte-identical to a
// build without the feature — the cache promise the test on this file pins.

import (
	"fmt"
	"sort"

	"github.com/switchboard-code/switchboard/internal/skills"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func addSkills(registry *tools.Registry, workspace string, additionalRoots ...skills.AdditionalRoot) ([]skills.Skill, []mcpNote) {
	inventory, loadNotes := skills.Load(workspace)
	pluginSkills, pluginNotes := skills.LoadAdditional(additionalRoots)
	inventory = append(inventory, pluginSkills...)
	sort.Slice(inventory, func(i, j int) bool { return inventory[i].Key() < inventory[j].Key() })
	loadNotes = append(loadNotes, pluginNotes...)
	var notes []mcpNote
	for _, n := range loadNotes {
		notes = append(notes, mcpNote{"warn", n})
	}
	visible := skills.ModelVisible(inventory)
	if len(visible) == 0 {
		if len(inventory) > 0 {
			notes = append(notes, mcpNote{"", fmt.Sprintf("skills: %d discovered, none model-visible", len(inventory))})
		}
		return inventory, notes
	}
	if err := registry.AddExternal(skills.NewTool(visible)); err != nil {
		notes = append(notes, mcpNote{"warn", "skills unavailable: " + err.Error()})
		return inventory, notes
	}
	notes = append(notes, mcpNote{"", fmt.Sprintf("skills: %d discovered, %d model-visible", len(inventory), len(visible))})
	return inventory, notes
}
