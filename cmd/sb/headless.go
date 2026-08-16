package main

// The headless surface: what `sb -p` becomes when a script is driving it.
// Piped stdin rides into the prompt as an attachment, and -output json turns
// the run's outcome into one machine-readable line on stdout while the
// transcript keeps rendering on stderr. Both exist for the same reason the
// REPL survived the TUI: a tool that can only be used by a person at a
// terminal cannot be a step in anything larger.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/credential"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
)

// attachPipedInput carries piped stdin into the prompt the way an @path
// mention carries a file: the prompt stays what the user said, and the
// attachment follows, labelled, so the model knows why it is there.
func attachPipedInput(prompt string, data []byte) string {
	text := strings.TrimRight(string(data), "\n")
	if strings.TrimSpace(text) == "" {
		return prompt
	}
	return prompt + "\n\nContents of standard input (piped in):\n```\n" + text + "\n```"
}

// refuseLeakedSecrets is the headless half of the outbound credential gate.
// The TUI can hold the prompt and ask; a scripted run cannot, and the safe
// answer to "no one to ask" is no — a diff piped through -p is exactly how
// a .env line reaches a provider and the session log by accident. The
// findings name their kind and prefix only: an error message quoting the
// key would be this gate committing the leak it exists to stop.
func refuseLeakedSecrets(prompt string, allow bool) error {
	if allow {
		return nil
	}
	leaks := credential.ScanPrompt(prompt)
	if len(leaks) == 0 {
		return nil
	}
	kinds := make([]string, len(leaks))
	for i, l := range leaks {
		kinds[i] = l.String()
	}
	return fmt.Errorf("the prompt contains %s; nothing was sent. Redact the input, or pass -allow-secrets to send it deliberately",
		strings.Join(kinds, ", "))
}

// headlessReport is the JSON a -output json run prints: the result, what it
// consumed, and what that consumption was priced at. One object, one line,
// stdout; everything else the run had to say went to stderr as it happened.
type headlessReport struct {
	Result  string `json:"result"`
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`

	Session string `json:"session"`
	Tier    string `json:"tier"`
	Target  string `json:"target"`

	Calls int            `json:"calls"`
	Usage provider.Usage `json:"usage"`
	Cost  headlessCost   `json:"cost"`
}

// headlessCost keeps the three meterings apart in the scripting surface for
// the same reason /cost keeps them apart on screen (§4): a script that reads
// a collapsed dollar figure learns the wrong thing about a local or plan
// session, and scripts propagate what they learn.
type headlessCost struct {
	Metering string `json:"metering"`
	Note     string `json:"note"`

	// EstimatedUSD is present only when the metering is per-token and the
	// catalog priced the session. It is an estimate against the named
	// revision, never a substitute for the provider's invoice (§15).
	EstimatedUSD    *catalog.Money `json:"estimated_usd,omitempty"`
	CatalogRevision string         `json:"catalog_revision,omitempty"`
}

// buildHeadlessReport derives the report from what the session recorded. The
// tier is the one the run ended on, because escalation may have moved it, and
// a report naming the starting rung would misattribute the spend.
func buildHeadlessReport(state session.State, cat *catalog.Catalog, tier config.Tier, turnErr error) headlessReport {
	rep := headlessReport{
		Result:  lastAssistantText(state.Messages),
		Outcome: "completed",
		Session: state.ID,
		Tier:    tier.ID,
		Target:  string(tier.Target.ID()),
		Calls:   state.Calls,
		Usage:   state.Usage,
		Cost:    headlessCost{Metering: "unknown"},
	}
	switch {
	case errors.Is(turnErr, context.Canceled):
		rep.Outcome = "cancelled"
		rep.Error = turnErr.Error()
	case turnErr != nil:
		rep.Outcome = "error"
		rep.Error = turnErr.Error()
	}

	// The branches mirror summaryLines exactly: the wording a person reads in
	// /cost and the fields a script reads here must not drift apart.
	info, _, ok := cat.Lookup(tier.Target)
	switch {
	case !ok:
		rep.Cost.Note = "no catalog entry for this target, so nothing was priced"
	case info.Metering == catalog.Local:
		rep.Cost.Metering = string(catalog.Local)
		rep.Cost.Note = "runs locally, so there is nothing to bill"
	case info.Metering == catalog.Plan:
		rep.Cost.Metering = string(catalog.Plan)
		rep.Cost.Note = "billed as a plan; quota, not dollars, is what this consumed"
	case info.Free():
		rep.Cost.Metering = string(info.Metering)
		rep.Cost.Note = "no per-token cost recorded for this target"
	default:
		cost := catalog.Money(state.CostMicroUSD)
		rep.Cost.Metering = string(info.Metering)
		rep.Cost.Note = "estimated against catalog " + state.CatalogRevision + ", not the provider's invoice"
		rep.Cost.EstimatedUSD = &cost
		rep.Cost.CatalogRevision = state.CatalogRevision
	}
	return rep
}

// lastAssistantText is the run's result: the text of the last assistant
// message that said anything. A turn that ended mid-stream still reports what
// it produced, because a partial answer a script can read beats a silent one.
func lastAssistantText(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != provider.RoleAssistant {
			continue
		}
		var parts []string
		for _, b := range msgs[i].Content {
			if t, isText := b.(provider.Text); isText && strings.TrimSpace(t.Text) != "" {
				parts = append(parts, t.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n\n")
		}
	}
	return ""
}

func writeHeadlessReport(w io.Writer, rep headlessReport) error {
	enc := json.NewEncoder(w)
	return enc.Encode(rep)
}
