package extensions

import (
	"errors"
	"fmt"
)

// ActivationCandidate is proof that one exact installed plugin was freshly
// rediscovered and matched its normalized identity. It does not encode native
// enablement, Switchboard activation, or executable trust. Its fields are
// intentionally private so a caller cannot substitute a plain available
// Plugin by accident.
type ActivationCandidate struct {
	plugin Plugin
}

// newActivationCandidate validates an exact plugin root already published in
// Switchboard's content-addressed cache. It stays package-private so arbitrary
// discovery or native inventory cannot be upgraded into activation proof.
func newActivationCandidate(plugin Plugin) (*ActivationCandidate, error) {
	if err := validateInstallInput(plugin); err != nil {
		return nil, fmt.Errorf("invalid activation candidate: %w", err)
	}
	root, err := openInstallSource(plugin)
	if err != nil {
		return nil, fmt.Errorf("opening activation candidate: %w", err)
	}
	defer root.Close()
	validated, err := discoverExactPlugin(plugin.RealPath, root, plugin)
	if err != nil {
		return nil, fmt.Errorf("validating activation candidate %s: %w", plugin.ID, err)
	}
	return &ActivationCandidate{plugin: clonePlugin(validated)}, nil
}

// Plugin returns the validated plugin record without exposing mutable slices
// held by the capability.
func (candidate *ActivationCandidate) Plugin() Plugin {
	if candidate == nil {
		return Plugin{}
	}
	return clonePlugin(candidate.plugin)
}

func (candidate *ActivationCandidate) currentPlugin() (Plugin, error) {
	if candidate == nil {
		return Plugin{}, errors.New("plugin activation requires an eligibility candidate")
	}
	root, err := openInstallSource(candidate.plugin)
	if err != nil {
		return Plugin{}, fmt.Errorf("opening activation candidate: %w", err)
	}
	defer root.Close()
	plugin, err := discoverExactPlugin(candidate.plugin.RealPath, root, candidate.plugin)
	if err != nil {
		return Plugin{}, fmt.Errorf("activation candidate %s changed: %w", candidate.plugin.ID, err)
	}
	return plugin, nil
}

func clonePlugin(plugin Plugin) Plugin {
	clone := plugin
	clone.Components = append([]Component(nil), plugin.Components...)
	clone.Warnings = append([]Warning(nil), plugin.Warnings...)
	if clone.Components == nil {
		clone.Components = []Component{}
	}
	return clone
}
