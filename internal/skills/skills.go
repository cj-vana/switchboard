package skills

// Skills: standing instructions the model pulls in when the task matches,
// instead of instructions that ride every request. A skill is a markdown
// file with a name and a one-line description; the descriptions travel in
// the skill tool's own description, the bodies stay on disk until asked
// for, and the pull is a tool call the transcript shows. The §13 porting
// story applies unchanged: packs written for the neighboring tools use the
// same directory-plus-SKILL.md shape and load by copying.
//
// The trust posture is the named agents' (§13): both directories load
// without a grant because nothing executes at read time — a skill is a
// prompt, and whatever it persuades the model to do passes the permission
// engine on its own merits. Discovery is once, at session assembly, sorted
// by name, because the descriptions ride the tool schema into the frozen
// zone (§6.1).

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill is one loaded definition.
type Skill struct {
	Name        string
	Description string

	// Body is the instructions, served when the model asks.
	Body string

	// Dir is where supporting files beside the skill live: the skill's own
	// directory for the <name>/SKILL.md shape, the skills directory for a
	// flat <name>.md. The tool serves those files from here, so a pack that
	// references its own references/ works wherever it was copied from.
	Dir string

	// FromHome records which tree supplied the file: ~/.switchboard is the
	// user speaking, a repository's .switchboard is whoever was cloned.
	FromHome bool
}

// Load reads both skill directories once: .switchboard/skills/ in the
// workspace and ~/.switchboard/skills/, project winning a name clash. Two
// file shapes load: <name>.md directly in the directory, and the
// <name>/SKILL.md layout the neighboring tools converged on, so their packs
// port by copying the folder.
func Load(workspace string) (list []Skill, notes []string) {
	type source struct {
		dir      string
		fromHome bool
	}
	dirs := []source{{filepath.Join(workspace, ".switchboard", "skills"), false}}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, source{filepath.Join(home, ".switchboard", "skills"), true})
	}

	seen := map[string]bool{}
	for _, src := range dirs {
		entries, err := os.ReadDir(src.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			var path, fallback, dir string
			switch {
			case e.IsDir():
				path = filepath.Join(src.dir, e.Name(), "SKILL.md")
				fallback = e.Name()
				dir = filepath.Join(src.dir, e.Name())
				if _, err := os.Stat(path); err != nil {
					continue // a directory without a SKILL.md is not a skill
				}
			case strings.HasSuffix(e.Name(), ".md"):
				path = filepath.Join(src.dir, e.Name())
				fallback = strings.TrimSuffix(e.Name(), ".md")
				dir = src.dir
			default:
				continue
			}

			data, err := os.ReadFile(path)
			if err != nil {
				notes = append(notes, fmt.Sprintf("skill %s: %v", path, err))
				continue
			}
			sk, err := parse(fallback, string(data))
			if err != nil {
				notes = append(notes, fmt.Sprintf("skill %s: %v", path, err))
				continue
			}
			if seen[sk.Name] {
				continue // project spoke first
			}
			seen[sk.Name] = true
			sk.Dir = dir
			sk.FromHome = src.fromHome
			list = append(list, sk)
		}
	}

	// Sorted so the tool description, which the schema carries into the
	// frozen zone, never depends on directory read order.
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, notes
}

// parse reads one definition. Only name and description are meaningful;
// anything else in the frontmatter is ignored rather than an error, so a
// file carrying another tool's keys loads without editing.
func parse(fallbackName, content string) (Skill, error) {
	sk := Skill{Name: fallbackName}
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
						sk.Name = value
					}
				case "description":
					sk.Description = value
				}
			}
		}
	}

	sk.Body = strings.TrimSpace(body)
	if sk.Body == "" {
		return Skill{}, fmt.Errorf("has no body; the body is the skill's instructions")
	}
	if sk.Description == "" {
		return Skill{}, fmt.Errorf("has no description; the description is how the model decides when to use it")
	}
	return sk, nil
}
