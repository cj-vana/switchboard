package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/switchboard-code/switchboard/internal/provider"
)

const evaluationIdentityVersion = "eval-v1"

type snapshotIdentity struct {
	Target   provider.RouteTargetID
	Snapshot string
}

type armTargetIdentity struct {
	Arm     string
	Targets []provider.RouteTargetID
}

type identityRecord struct {
	Version         string
	HarnessCommit   string
	CatalogRevision string
	PromptVersion   string
	Tasks           []string
	Arms            []string
	ArmTargets      []armTargetIdentity
	Targets         []provider.RouteTargetID
	Replicates      []int
	Workers         int
	Snapshots       []snapshotIdentity
}

// evaluationID is a deterministic binding between journal rows and the
// configuration that produced them. It deliberately includes the intended
// arms and replicates, not only what happened to finish, so an entirely absent
// arm or replicate cannot disappear when the journal is reported later.
func evaluationID(
	tasks []Task,
	arms []string,
	armTargets map[string][]provider.RouteTargetID,
	targets []provider.RouteTargetID,
	replicates []int,
	workers int,
	pins Pins,
) string {
	taskSet := map[string]bool{}
	for _, task := range tasks {
		if task.Provenance == HandWritten && task.ID != "" {
			taskSet[task.ID] = true
		}
	}
	armSet := stringSet(arms)
	targetSet := map[provider.RouteTargetID]bool{}
	for _, target := range targets {
		if target != "" {
			targetSet[target] = true
		}
	}
	replicateSet := map[int]bool{}
	for _, replicate := range replicates {
		replicateSet[replicate] = true
	}

	normalizedTargets := sortedTargets(targetSet)
	record := identityRecord{
		Version:         evaluationIdentityVersion,
		HarnessCommit:   pins.HarnessCommit,
		CatalogRevision: pins.CatalogRevision,
		PromptVersion:   pins.PromptVersion,
		Tasks:           sortedStrings(taskSet),
		Arms:            sortedStrings(armSet),
		ArmTargets:      make([]armTargetIdentity, 0, len(armTargets)),
		Targets:         normalizedTargets,
		Replicates:      sortedInts(replicateSet),
		Workers:         workers,
		Snapshots:       make([]snapshotIdentity, 0, len(normalizedTargets)),
	}
	for arm, configured := range armTargets {
		set := map[provider.RouteTargetID]bool{}
		for _, target := range configured {
			if target != "" {
				set[target] = true
			}
		}
		record.ArmTargets = append(record.ArmTargets, armTargetIdentity{
			Arm: arm, Targets: sortedTargets(set),
		})
	}
	sort.Slice(record.ArmTargets, func(i, j int) bool {
		return record.ArmTargets[i].Arm < record.ArmTargets[j].Arm
	})
	for _, target := range normalizedTargets {
		record.Snapshots = append(record.Snapshots, snapshotIdentity{
			Target: target, Snapshot: pins.Snapshots[target],
		})
	}
	sort.Slice(record.Snapshots, func(i, j int) bool {
		return record.Snapshots[i].Target < record.Snapshots[j].Target
	})

	raw, err := json.Marshal(record)
	if err != nil {
		panic(err) // identityRecord contains only JSON-native values
	}
	sum := sha256.Sum256(raw)
	return evaluationIdentityVersion + ":" + hex.EncodeToString(sum[:])
}
