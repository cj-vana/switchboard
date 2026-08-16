package delegate

// Named subagent definitions (§13): a markdown file per agent, frontmatter
// naming it, the body its standing instructions. .switchboard/agents/ in the
// repository and ~/.switchboard/agents/ globally, project winning a name
// clash, the same shape custom commands use — a definition is a prompt plus
// two defaults (a rung, a tool grant), so it earns the same trust posture:
// readable without a grant, because nothing executes at read time, and every
// call the agent then makes passes the shared permission engine on its own
// merits.
//
// Discovery is once, at session assembly. The definitions surface in the
// delegate tool's schema, which sits in the frozen zone (§6.1), so a file
// added mid-session is picked up by the next session, not this one.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Agent is one loaded definition.
type Agent struct {
	Name        string
	Description string

	// Tier is the default rung, applied when a call names the agent but no
	// tier. Empty falls to the ladder's bottom, same as a bare delegate.
	Tier string

	// Tools narrows the subagent's registry to these names. Empty means the
	// full core suite. The grant can only narrow — the sub-registry never
	// held delegate or the bridged MCP tools to begin with.
	Tools []string

	// Prompt is the body: standing instructions appended to the subagent's
	// system blocks after the delegate preamble.
	Prompt string

	// FromHome records which directory supplied the file: ~/.switchboard is
	// the user speaking, a repository's .switchboard is whoever was cloned.
	FromHome bool
}

// LoadAgents reads both directories once. validTools is the tool suite a
// subagent can actually hold; a definition granting anything else is skipped
// with a note rather than loaded broken, because a call that fails at
// assembly time would name the wiring, not the typo.
func LoadAgents(workspace string, validTools []string) (agents []Agent, notes []string) {
	valid := map[string]bool{}
	for _, name := range validTools {
		valid[name] = true
	}

	type source struct {
		dir      string
		fromHome bool
	}
	dirs := []source{{filepath.Join(workspace, ".switchboard", "agents"), false}}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, source{filepath.Join(home, ".switchboard", "agents"), true})
	}

	seen := map[string]bool{}
	for _, src := range dirs {
		entries, err := os.ReadDir(src.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(src.dir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				notes = append(notes, fmt.Sprintf("agent %s: %v", path, err))
				continue
			}
			ag, err := parseAgent(e.Name(), string(data), valid)
			if err != nil {
				notes = append(notes, fmt.Sprintf("agent %s: %v", path, err))
				continue
			}
			if seen[ag.Name] {
				continue // project spoke first
			}
			seen[ag.Name] = true
			ag.FromHome = src.fromHome
			agents = append(agents, ag)
		}
	}

	// Sorted so the tool description, which the schema carries into the
	// frozen zone, never depends on directory read order.
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	return agents, notes
}

// parseAgent reads one definition. The frontmatter fields are the four §13
// names — name, description, tier, tools — and anything else is ignored
// rather than an error, so a file carrying another tool's extra keys loads
// without editing.
func parseAgent(filename, content string, valid map[string]bool) (Agent, error) {
	ag := Agent{Name: strings.TrimSuffix(filename, ".md")}
	body := content

	if strings.HasPrefix(content, "---\n") {
		rest := content[4:]
		if end := strings.Index(rest, "\n---"); end >= 0 {
			front := rest[:end]
			body = strings.TrimPrefix(rest[end+4:], "\n")
			for _, line := range strings.Split(front, "\n") {
				key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
				if !ok {
					continue
				}
				value = strings.Trim(strings.TrimSpace(value), `"'`)
				switch strings.TrimSpace(key) {
				case "name":
					if value != "" {
						ag.Name = value
					}
				case "description":
					ag.Description = value
				case "tier":
					ag.Tier = value
				case "tools":
					ag.Tools = splitTools(value)
				}
			}
		}
	}

	for _, name := range ag.Tools {
		if !valid[name] {
			return Agent{}, fmt.Errorf("grants %q, which is not in the subagent suite (%s)",
				name, strings.Join(sortedNames(valid), ", "))
		}
	}
	ag.Prompt = strings.TrimSpace(body)
	if ag.Prompt == "" {
		return Agent{}, fmt.Errorf("has no body; the body is the agent's instructions")
	}
	return ag, nil
}

// splitTools accepts the two list shapes people write: "read, grep" and
// "[read, grep]". Names are lowercased because the suite's are.
func splitTools(value string) []string {
	value = strings.Trim(value, "[]")
	var out []string
	for _, f := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' }) {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
