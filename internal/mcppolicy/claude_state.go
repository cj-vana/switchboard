package mcppolicy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxClaudeProjectAliasChecks = 64

// claudeProjectState contains only the two deny surfaces used by Claude Code
// for the active workspace. The rest of .claude.json can include credentials,
// trust decisions, and server definitions and is deliberately never retained.
type claudeProjectState struct {
	disabledNonProject []string
	disabledProject    []string
}

func parseClaudeProjectState(data []byte, workspace string) (claudeProjectState, error) {
	root, err := decodeUniqueJSONObject(data)
	if err != nil {
		return claudeProjectState{}, fmt.Errorf("invalid Claude state JSON")
	}
	rawProjects, exists := root["projects"]
	if !exists {
		return claudeProjectState{}, nil
	}
	projects, err := decodeRawObject(rawProjects)
	if err != nil || len(projects) > MaxPolicyEntries {
		return claudeProjectState{}, fmt.Errorf("invalid Claude projects state")
	}
	matching, err := matchingClaudeWorkspaceKeys(projects, workspace)
	if err != nil {
		return claudeProjectState{}, err
	}
	result := claudeProjectState{}
	for _, key := range matching {
		project, objectErr := decodeRawObject(projects[key])
		if objectErr != nil {
			return claudeProjectState{}, fmt.Errorf("invalid matching Claude project state")
		}
		nonProject, listErr := stateDisabledNames(project, "disabledMcpServers")
		if listErr != nil {
			return claudeProjectState{}, listErr
		}
		projectOnly, listErr := stateDisabledNames(project, "disabledMcpjsonServers")
		if listErr != nil {
			return claudeProjectState{}, listErr
		}
		result.disabledNonProject = appendUniqueStrings(result.disabledNonProject, nonProject...)
		result.disabledProject = appendUniqueStrings(result.disabledProject, projectOnly...)
	}
	return result, nil
}

func decodeRawObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("not an object")
	}
	return object, nil
}

func stateDisabledNames(project map[string]json.RawMessage, field string) ([]string, error) {
	raw, exists := project[field]
	if !exists {
		return nil, nil
	}
	values, ok := rawStringSlice(raw)
	if !ok || len(values) > MaxPolicyValues {
		return nil, fmt.Errorf("invalid Claude disabled-server state")
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || containsControl(value) {
			return nil, fmt.Errorf("invalid Claude disabled-server state")
		}
	}
	return uniqueStrings(values), nil
}

func containsControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

// Exact keys are the normal path and avoid filesystem work. Alias matching is
// deliberately bounded because stale project maps may contain arbitrary paths
// that trigger slow mounts or other filesystem side effects when inspected.
func matchingClaudeWorkspaceKeys(projects map[string]json.RawMessage, workspace string) ([]string, error) {
	keys := make([]string, 0, len(projects))
	for key := range projects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	cleanWorkspace := filepath.Clean(workspace)
	var exact []string
	for _, key := range keys {
		if filepath.IsAbs(key) && filepath.Clean(key) == cleanWorkspace {
			exact = append(exact, key)
		}
	}
	if len(exact) != 0 {
		return exact, nil
	}
	absolute := make([]string, 0, len(keys))
	for _, key := range keys {
		if filepath.IsAbs(key) {
			absolute = append(absolute, key)
		}
	}
	if len(absolute) > maxClaudeProjectAliasChecks {
		return nil, fmt.Errorf("Claude project alias budget exceeded")
	}
	matched := make([]string, 0, 1)
	for _, key := range absolute {
		if sameClaudeWorkspace(key, cleanWorkspace) {
			matched = append(matched, key)
		}
	}
	return matched, nil
}

func sameClaudeWorkspace(project, workspace string) bool {
	projectInfo, projectErr := os.Stat(project)
	workspaceInfo, workspaceErr := os.Stat(workspace)
	if projectErr == nil && workspaceErr == nil {
		return os.SameFile(projectInfo, workspaceInfo)
	}
	project = filepath.Clean(project)
	if real, err := filepath.EvalSymlinks(project); err == nil {
		project = filepath.Clean(real)
	}
	return project == workspace
}
