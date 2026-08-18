package main

// Assembly for the structural search tool: the lookup happens here because
// which binaries a machine has is a surface concern, not the tool suite's.

import (
	"os/exec"

	"github.com/switchboard-code/switchboard/internal/tools"
)

// addStructuralSearch registers astgrep when the machine has the binary.
// Absent is absent, not broken: the model simply never sees the tool, the
// way delegate never appears without a ladder.
func addStructuralSearch(registry *tools.Registry) {
	binary, err := exec.LookPath("ast-grep")
	if err != nil {
		return
	}
	// The only registration failure is a name collision, which cannot
	// happen against the fixed core suite; ignoring it keeps a missing
	// nicety from becoming a startup failure.
	_ = registry.AddExternal(tools.NewAstGrep(registry, binary))
}
