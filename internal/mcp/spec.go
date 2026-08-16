package mcp

import (
	"fmt"
	"os"
	"sort"

	"github.com/BurntSushi/toml"
)

// SpecFileName is the file consulted in ~/.switchboard and in a workspace's
// .switchboard directory. It is its own file rather than a table in
// config.toml because config.toml is regenerated from typed state on every
// TUI settings change, and a hand-maintained server list does not belong in
// a file the program rewrites.
const SpecFileName = "mcp.toml"

type specEntry struct {
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
	URL     string            `toml:"url"`
	Allow   []string          `toml:"allow"`
}

// LoadSpecs reads one server file. A missing file is an empty list, not an
// error: most machines and most repositories have none.
func LoadSpecs(path string) ([]Spec, error) {
	var file struct {
		MCP map[string]specEntry `toml:"mcp"`
	}
	if _, err := toml.DecodeFile(path, &file); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	names := make([]string, 0, len(file.MCP))
	for name := range file.MCP {
		names = append(names, name)
	}
	sort.Strings(names)

	specs := make([]Spec, 0, len(names))
	for _, name := range names {
		e := file.MCP[name]
		s := Spec{
			Name:    name,
			Command: e.Command,
			Args:    e.Args,
			Env:     e.Env,
			URL:     e.URL,
			Allow:   e.Allow,
		}
		if err := s.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		specs = append(specs, s)
	}
	return specs, nil
}
