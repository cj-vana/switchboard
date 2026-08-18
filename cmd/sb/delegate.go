package main

// Delegate assembly. The tool itself lives in internal/delegate; what
// belongs here is the wiring only a surface has: provider probing, session
// stores, and where the subagent's rails render.

import (
	"context"
	"fmt"
	"sync"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/delegate"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/hooks"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/skills"
	"github.com/switchboard-code/switchboard/internal/tools"
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

// delegateLedgerTracker remembers how much external cost the primary held
// when each sub-session started. Normal settlements charge each successful
// call immediately; reconcile adds only the gap left when a settlement append
// failed after Usage became durable in the sub-session.
type delegateLedgerTracker struct {
	mu       sync.Mutex
	baseline map[string]int64
}

func (d *delegateLedgerTracker) mark(primary, sub *session.Session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.baseline == nil {
		d.baseline = make(map[string]int64)
	}
	d.baseline[sub.ID()] = primary.State().ExternalCostMicroUSD
}

func (d *delegateLedgerTracker) reconcile(primary, sub *session.Session) error {
	d.mu.Lock()
	baseline, ok := d.baseline[sub.ID()]
	delete(d.baseline, sub.ID())
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("delegate %s has no budget baseline", sub.ID())
	}
	recorded := primary.State().ExternalCostMicroUSD - baseline
	missing := sub.State().CostMicroUSD - recorded
	if missing <= 0 {
		return nil
	}
	return primary.AppendBudgetTransfer("delegate-reconcile:"+sub.ID(), missing, 0)
}

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
	skillList []skills.Skill,
) ([]delegate.Agent, []string, error) {
	if len(cfg.Tiers) == 0 {
		return nil, nil, nil // no ladder, nothing to delegate on
	}

	subStore, err := delegateStore()
	if err != nil {
		return nil, nil, fmt.Errorf("delegate needs its session store: %w", err)
	}
	var ledger delegateLedgerTracker

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
		Probe: func(ctx context.Context, tierID string) (config.Tier, provider.Provider, string, error) {
			tier, ok := cfg.Tier(tierID)
			if !ok {
				return config.Tier{}, nil, "", fmt.Errorf("no tier %s", tierID)
			}
			return reg.probeTierFallback(ctx, tier)
		},
		NewSession: func(target provider.RouteTargetID) (*session.Session, error) {
			sess, err := subStore.Create(workspace, target, cat.Revision)
			if err != nil {
				return nil, err
			}
			ledger.mark(primary.Session, sess)
			return sess, nil
		},
		NewLoop: func(tier config.Tier, client provider.Provider, sess *session.Session, obs agent.Observer, named *delegate.Agent) (*agent.Loop, error) {
			subRegistry, err := tools.NewRegistryWithExecution(workspace, primary.Tools.Execution())
			if err != nil {
				return nil, err
			}
			// A subagent searches more than anything else, so it gets the
			// structural tool too. Before Restrict on purpose: a named
			// agent's grant validates against the core suite, so a
			// restricted agent loses astgrep with everything else unnamed,
			// which is right — a grant written on one machine must not
			// depend on another machine's binaries.
			addStructuralSearch(subRegistry)
			// Computer use joins under the astgrep rule: a conditional tool
			// an unrestricted subagent keeps and a restricted one loses with
			// everything else unnamed. Its calls still carry the external
			// effect through the shared engine, so a delegated errand asks
			// the same user the primary would.
			addComputerUse(subRegistry)
			// Skills too, and for the same reason as astgrep's placement: a
			// named agent's grant validates against the core suite, so a
			// restricted agent loses skill with everything else unnamed.
			if modelSkills := skills.ModelVisible(skillList); len(modelSkills) > 0 {
				if err := subRegistry.AddExternal(skills.NewTool(modelSkills)); err != nil {
					return nil, err
				}
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
			wireBudget(sub, budgetGate(budget, cat,
				func() provider.RouteTarget { return sub.Binding().Target },
				func() catalog.Money {
					return catalog.Money(primary.Session.State().AccountedCostMicroUSD())
				},
				func() string { return primary.Session.ID() }).withLedger(
				func() catalog.Money { return catalog.Money(primary.Session.State().RetryReserveMicroUSD) },
				func(amount catalog.Money) (string, error) { return primary.Session.BeginBudgetAttempt(int64(amount)) },
				func(id, outcome string, charge catalog.Money) error {
					return primary.Session.SettleBudgetAttempt(id, outcome, int64(charge))
				}, true))
			return sub, nil
		},
		Finish: func(sess *session.Session) error {
			return ledger.reconcile(primary.Session, sess)
		},
		Forward: subagentForward.get,
	})
	if err != nil {
		return nil, agentNotes, err
	}
	return agents, agentNotes, registry.AddExternal(tool)
}
