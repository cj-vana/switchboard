package main

// Delegate assembly. The tool itself lives in internal/delegate; what
// belongs here is the wiring only a surface has: provider probing, session
// stores, and where the subagent's rails render.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/delegate"
	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/hooks"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/tools"
)

// delegateForward late-binds where subagent activity renders. The delegate
// tool is registered before either surface exists, and the forwarding target
// is the surface's raw observer, deliberately not the watcher: a subagent's
// error spikes are its own, and feeding them to the primary's escalation
// policy would move the primary on evidence from a different context.
type delegateForward struct {
	mu  sync.Mutex
	obs agent.Observer
}

func (d *delegateForward) set(obs agent.Observer) {
	d.mu.Lock()
	d.obs = obs
	d.mu.Unlock()
}

func (d *delegateForward) get() agent.Observer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.obs
}

var subagentForward = &delegateForward{}

// registerDelegate adds the delegate tool to the primary registry. The
// subagent gets a fresh registry — core tools, no delegate (depth one), no
// MCP — the shared permission engine and asker, the same hooks, and its own
// session in a store /resume never lists.
func registerDelegate(
	registry *tools.Registry,
	cfg *config.Config,
	cat *catalog.Catalog,
	reg *providers,
	primary *agent.Loop,
	hookSet *hooks.Set,
	capability execution.Capability,
	workspace string,
) error {
	if len(cfg.Tiers) == 0 {
		return nil // no ladder, nothing to delegate on
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("delegate needs a home directory for its session store: %w", err)
	}
	subStore, err := session.NewStore(filepath.Join(home, ".switchboard", "delegates"))
	if err != nil {
		return err
	}

	tool, err := delegate.New(delegate.Config{
		Tiers: cfg.Tiers,
		Probe: func(ctx context.Context, tierID string) (config.Tier, provider.Provider, error) {
			tier, ok := cfg.Tier(tierID)
			if !ok {
				return config.Tier{}, nil, fmt.Errorf("no tier %s", tierID)
			}
			return reg.probeTier(ctx, tier)
		},
		NewSession: func(target provider.RouteTargetID) (*session.Session, error) {
			return subStore.Create(workspace, target, cat.Revision)
		},
		NewLoop: func(tier config.Tier, client provider.Provider, sess *session.Session, obs agent.Observer) (*agent.Loop, error) {
			subRegistry, err := tools.NewRegistry(workspace, capability)
			if err != nil {
				return nil, err
			}
			// The mode is read at call time from the shared engine, so a
			// session switched to plan mode delegates plan-mode subagents.
			system := agent.SystemPrompt(workspace, primary.Perms.Mode(), capability)
			system = append(system, provider.Text{Text: delegate.Preamble})
			return &agent.Loop{
				Provider:      client,
				Target:        tier.Target,
				Tools:         subRegistry,
				Perms:         primary.Perms,
				Asker:         primary.Asker,
				Session:       sess,
				Catalog:       cat,
				Cache:         cacheFor(tier.Target, cat),
				System:        system,
				Observer:      obs,
				MaxToolRounds: delegate.MaxRounds,
				Hooks:         hookSet,
			}, nil
		},
		Forward: subagentForward.get,
	})
	if err != nil {
		return err
	}
	return registry.AddExternal(tool)
}
