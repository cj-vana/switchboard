package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/tools"
)

// nameCharset is what every provider accepts in a tool name. MCP servers may
// use anything; a character outside this set is mapped to an underscore
// before the name crosses a wire.
var nameCharset = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// maxToolName is the tightest documented provider limit.
const maxToolName = 64

func sanitize(s string) string {
	return nameCharset.ReplaceAllString(s, "_")
}

// Namespaced renders the registry name for one server's tool. The mcp__
// prefix keeps the external suite visibly separate from the built-ins and
// unable to collide with them.
func Namespaced(server, tool string) string {
	return "mcp__" + sanitize(server) + "__" + sanitize(tool)
}

// BridgedTools wraps every discovered tool for the registry. A tool whose
// namespaced name would not survive the providers' constraints is skipped
// with a notice rather than silently renamed into a collision.
func (c *Client) BridgedTools() []tools.Tool {
	var out []tools.Tool
	seen := map[string]bool{}
	for _, info := range c.tools {
		name := Namespaced(c.spec.Name, info.Name)
		if len(name) > maxToolName {
			c.logf("warn", fmt.Sprintf("mcp %s: tool %s skipped: namespaced name exceeds %d characters", c.spec.Name, info.Name, maxToolName))
			continue
		}
		if seen[name] {
			c.logf("warn", fmt.Sprintf("mcp %s: tool %s skipped: name collides after sanitizing", c.spec.Name, info.Name))
			continue
		}
		seen[name] = true
		out = append(out, &bridgedTool{client: c, info: info, name: name})
	}
	return out
}

// AllowRules translates the spec's allow list into permission rules, so a
// tool the user named in config runs without a prompt. The rule names the
// namespaced form: what the user allowed is this server's tool, not any
// tool that happens to share the short name.
func (c *Client) AllowRules() []permission.Rule {
	var rules []permission.Rule
	for _, tool := range c.spec.Allow {
		rules = append(rules, permission.Rule{
			Decision: permission.Allow,
			Tool:     Namespaced(c.spec.Name, tool),
			Effect:   permission.EffectExternal,
		})
	}
	return rules
}

type bridgedTool struct {
	client *Client
	info   ToolInfo
	name   string
}

func (t *bridgedTool) Name() string { return t.name }

func (t *bridgedTool) Description() string {
	desc := strings.TrimSpace(t.info.Description)
	if desc == "" {
		desc = "No description provided."
	}
	return fmt.Sprintf("[%s MCP] %s", t.client.spec.Name, desc)
}

// ParallelSafe is false for every bridged tool: this client cannot know what
// a server-side tool touches, and two opaque effects in flight at once is a
// race nobody can reason about afterward.
func (t *bridgedTool) ParallelSafe() bool { return false }

func (t *bridgedTool) Schema() json.RawMessage {
	if len(t.info.InputSchema) > 0 && json.Valid(t.info.InputSchema) {
		return t.info.InputSchema
	}
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *bridgedTool) Plan(input json.RawMessage) (tools.Plan, error) {
	if len(input) > 0 && !json.Valid(input) {
		return tools.Plan{}, fmt.Errorf("%s: arguments are not valid JSON", t.name)
	}

	// The external effect is the honest classification: whatever this tool
	// does happens outside the workspace boundary and outside any sandbox
	// this host verified, in a process the permission engine cannot see into.
	// Detail is display only — the dialog shows the arguments, while the
	// remembered answer covers the tool, because a user approving an MCP tool
	// is approving the tool, not one byte-exact invocation.
	return tools.Plan{
		Request: permission.Request{
			Tool:   t.name,
			Effect: permission.EffectExternal,
			Detail: fmt.Sprintf("%s (%s server)", compactJSON(input), t.client.spec.Name),
		},
		Run: func(ctx context.Context) (tools.Result, error) {
			res, err := t.client.Call(ctx, t.info.Name, input)
			if err != nil {
				if ctx.Err() != nil {
					return tools.Result{}, ctx.Err()
				}
				return tools.Result{Content: err.Error(), IsError: true}, nil
			}
			return tools.Result{Content: res.Content, IsError: res.IsError}, nil
		},
	}, nil
}

// compactJSON renders arguments for the permission dialog: one line, bounded.
func compactJSON(raw json.RawMessage) string {
	var buf strings.Builder
	compact := json.RawMessage(raw)
	if len(compact) == 0 {
		return "{}"
	}
	var tmp any
	if err := json.Unmarshal(compact, &tmp); err == nil {
		if b, err := json.Marshal(tmp); err == nil {
			compact = b
		}
	}
	s := string(compact)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	buf.WriteString(s)
	return buf.String()
}

// SortTools orders bridged tools by name so the registry's frozen-zone
// ordering does not depend on server enumeration order.
func SortTools(ts []tools.Tool) {
	slices.SortFunc(ts, func(a, b tools.Tool) int { return strings.Compare(a.Name(), b.Name()) })
}
