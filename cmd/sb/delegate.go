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
	"github.com/cj-vana/switchboard/internal/checkpoint"
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
// session in a store /resume never lists. It returns the named agent
// definitions it discovered and any notes their loading produced, for
// /agents to show.
func registerDelegate(
	registry *tools.Registry,
	cfg *config.Config,
	cat *catalog.Catalog,
	reg *providers,
	primary *agent.Loop,
	hookSet *hooks.Set,
	capability execution.Capability,
	workspace string,
	undoRec *checkpoint.Recorder,
	budget *budgetState,
) ([]delegate.Agent, []string, error) {
	if len(cfg.Tiers) == 0 {
		return nil, nil, nil // no ladder, nothing to delegate on
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, fmt.Errorf("delegate needs a home directory for its session store: %w", err)
	}
	subStore, err := session.NewStore(filepath.Join(home, ".switchboard", "delegates"))
	if err != nil {
		return nil, nil, err
	}

	// A definition naming a rung this ladder does not have still loads — it
	// was probably written for a taller ladder — but runs on the default
	// rung, and the note says so rather than letting every call error.
	agents, agentNotes := delegate.LoadAgents(workspace, tools.CoreNames())
	for i := range agents {
		if agents[i].Tier == "" {
			continue
		}
		if _, ok := cfg.Tier(agents[i].Tier); !ok {
			agentNotes = append(agentNotes, fmt.Sprintf(
				"agent %s names tier %s, which is not on the ladder; it will run on the default rung",
				agents[i].Name, agents[i].Tier))
			agents[i].Tier = ""
		}
	}

	tool, err := delegate.New(delegate.Config{
		Tiers:  cfg.Tiers,
		Agents: agents,
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
		NewLoop: func(tier config.Tier, client provider.Provider, sess *session.Session, obs agent.Observer, named *delegate.Agent) (*agent.Loop, error) {
			subRegistry, err := tools.NewRegistry(workspace, capability)
			if err != nil {
				return nil, err
			}
			// A named agent's grant narrows the suite before the first
			// request; the grant was validated at load, so an error here is
			// wiring, not a typo.
			if named != nil && len(named.Tools) > 0 {
				if err := subRegistry.Restrict(named.Tools); err != nil {
					return nil, err
				}
			}
			// The sub-registry shares the primary recorder and the sub-loop
			// opens no scope of its own, so a delegate's edits file under
			// the turn that delegated and one /undo takes back both.
			subRegistry.SetCheckpoints(undoRec)
			// The mode is read at call time from the shared engine, so a
			// session switched to plan mode delegates plan-mode subagents.
			system := agent.SystemPrompt(workspace, primary.Perms.Mode(), capability)
			system = append(system, provider.Text{Text: delegate.Preamble})
			if named != nil {
				system = append(system, provider.Text{Text: named.Prompt})
			}
			sub := &agent.Loop{
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
			}
			// The errand runs under the same ceiling as the session that
			// spawned it, counting what both logs have priced so far. A
			// delegated task is not a way around /budget.
			sub.Budget = budgetGate(budget, cat,
				func() provider.RouteTarget { return sub.Target },
				func() catalog.Money {
					return catalog.Money(primary.Session.State().CostMicroUSD + sess.State().CostMicroUSD)
				})
			return sub, nil
		},
		Forward: subagentForward.get,
	})
	if err != nil {
		return nil, agentNotes, err
	}
	return agents, agentNotes, registry.AddExternal(tool)
}
