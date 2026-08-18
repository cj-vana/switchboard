package session

// Fork branches a session at a turn into a new session sharing history up to
// that point (§12). The copy is the whole mechanism: the source log is read
// and never written, so the original stays exactly what it was, and the
// fork's messages are byte-identical to the source's prefix — which means a
// provider holding that prefix warm serves the fork warm too. This is the
// cache-honest answer to rewinding a conversation: not rewriting sent
// messages (the append-only rule in §6.1), but branching a new log that
// stops where you wish the old one had.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/switchboard-code/switchboard/internal/continuity"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// Fork copies a session's first keepMessages conversation messages, with the
// usage and audit records that accompany them, into a new session. The cut
// must land on a turn boundary: when messages are dropped, the first dropped
// message has to be the user message that opened its turn, because a
// conversation cut mid-turn leaves tool calls without results and every
// request built from it would be malformed (§10.3).
//
// The source is read without the append lock, so a session open in this
// process — the usual case — forks from its durable prefix.
func (s *Store) Fork(id string, keepMessages int) (*Session, error) {
	return s.forkOnto(id, keepMessages, "", false)
}

// ForkOnto is Fork with the new log started against a different target,
// which is what a /race arm needs: the branch shares the source's messages
// but runs its turn on the rung being raced, and a session's start record
// has to name the target that actually served it, because /resume binds
// from that record. An empty target keeps the source's.
func (s *Store) ForkOnto(id string, keepMessages int, target provider.RouteTargetID) (*Session, error) {
	return s.forkOnto(id, keepMessages, target, false)
}

// ForkForRetry is the one branch operation allowed to keep zero messages. A
// first-turn retry needs a fresh conversation prefix but must still inherit
// every budget charge and retry reserve from the set-aside attempt; creating a
// blank session would make repeated first-turn retries erase ceiling spend.
func (s *Store) ForkForRetry(id string, keepMessages int) (*Session, error) {
	return s.forkOnto(id, keepMessages, "", true)
}

// ForkAccountingOnto creates an empty conversation branch on another target
// while carrying the source's full observed cost and retry reserve. Race arms
// use it when the origin has accounting lineage but no messages to copy.
func (s *Store) ForkAccountingOnto(id string, target provider.RouteTargetID) (*Session, error) {
	return s.forkOnto(id, 0, target, true)
}

// ForkSession is Fork against a live source. Holding the source append lock
// from the first byte read through the accounting reconciliation makes the
// branch a single durable snapshot: an asynchronous metered call can be
// wholly before it or wholly after it, never disappear between EOF and the
// later state read.
func (s *Store) ForkSession(source *Session, keepMessages int) (*Session, error) {
	return s.forkSessionOnto(source, keepMessages, "", false)
}

// ForkSessionOnto is the live-source form of ForkOnto.
func (s *Store) ForkSessionOnto(source *Session, keepMessages int, target provider.RouteTargetID) (*Session, error) {
	return s.forkSessionOnto(source, keepMessages, target, false)
}

// ForkSessionForRetry is the live-source form of ForkForRetry.
func (s *Store) ForkSessionForRetry(source *Session, keepMessages int) (*Session, error) {
	return s.forkSessionOnto(source, keepMessages, "", true)
}

// ForkSessionAccountingOnto is the live-source form of ForkAccountingOnto.
func (s *Store) ForkSessionAccountingOnto(source *Session, target provider.RouteTargetID) (*Session, error) {
	return s.forkSessionOnto(source, 0, target, true)
}

func (s *Store) forkSessionOnto(source *Session, keepMessages int, target provider.RouteTargetID, allowEmpty bool) (*Session, error) {
	if source == nil {
		return nil, fmt.Errorf("cannot fork a nil session")
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.state.raceBranchPending {
		return nil, fmt.Errorf("%w: origin session %s", ErrRaceBranchPending, source.state.raceBranchOrigin)
	}
	state := source.state
	state.Messages = provider.CloneMessages(source.state.Messages)
	return s.forkPathOnto(source.state.ID, source.path, keepMessages, target, allowEmpty, &state)
}

func (s *Store) forkOnto(id string, keepMessages int, target provider.RouteTargetID, allowEmpty bool) (*Session, error) {
	if keepMessages < 0 || (keepMessages == 0 && !allowEmpty) {
		return nil, fmt.Errorf("a fork keeping no messages is an empty session; /clear is how those start")
	}
	matches, err := filepath.Glob(filepath.Join(s.root, "*", id+".log"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("session %s not found", id)
	}
	return s.forkPathOnto(id, matches[0], keepMessages, target, allowEmpty, nil)
}

func (s *Store) forkPathOnto(id, path string, keepMessages int, target provider.RouteTargetID, allowEmpty bool, sourceState *State) (*Session, error) {
	if keepMessages < 0 || (keepMessages == 0 && !allowEmpty) {
		return nil, fmt.Errorf("a fork keeping no messages is an empty session; /clear is how those start")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)

	header, err := r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading session header: %w", err)
	}
	var gotMagic string
	var version int
	if _, err := fmt.Sscanf(header, "%s %d", &gotMagic, &version); err != nil || gotMagic != magic {
		return nil, fmt.Errorf("%s is not a switchboard session log", path)
	}
	if version > SchemaVersion {
		return nil, fmt.Errorf("%w: log is schema %d, this binary understands %d", ErrSchemaTooNew, version, SchemaVersion)
	}

	explicitRetarget := target != ""
	var start SessionStart
	var kept []Record
	messages := 0
	pastCut := false
	raceBranchPending := false
	for {
		rec, _, err := decodeRecord(r)
		if errors.Is(err, io.EOF) || errors.Is(err, ErrCorruptRecord) {
			// A corrupt tail is what replay would have truncated; the fork
			// carries the valid prefix, same as a resume would.
			break
		}
		if err != nil {
			return nil, err
		}
		if rec.Type == RecordSessionStart {
			if err := json.Unmarshal(rec.Payload, &start); err != nil {
				return nil, err
			}
			continue
		}
		if rec.Type == RecordRaceBranch {
			var branch RaceBranch
			if err := json.Unmarshal(rec.Payload, &branch); err != nil {
				return nil, err
			}
			raceBranchPending = !branch.Finalized
			// Branch lifecycle belongs to this physical log. Copying a finalized
			// marker makes the child look like the old race branch and prevents it
			// from participating in a later independent race.
			continue
		}
		if explicitRetarget && rec.Type == RecordRuntimeBinding {
			// An Onto fork's SessionStart names its new target. Carrying the
			// source's moving binding would overwrite that target during replay
			// and can also inherit a user pin into an automatic race arm.
			continue
		}
		if rec.Type == RecordContinuity {
			capsule, err := continuity.DecodeStored(rec.Payload)
			if err != nil {
				return nil, err
			}
			// Keep every capsule derived from the exact prefix. This includes a
			// basis-zero capsule referenced by a first opening: retry must replay
			// that opening byte-for-byte, and its durable reference is valid only
			// while the same capsule remains current in the fork.
			if pastCut || capsule.BasisMessages > keepMessages {
				continue
			}
			kept = append(kept, rec)
			continue
		}
		if message, isMessage, err := conversationMessage(rec); err != nil {
			return nil, err
		} else if isMessage {
			if !pastCut && messages == keepMessages {
				if message.Role != provider.RoleUser {
					return nil, fmt.Errorf(
						"the cut falls inside a turn: message %d is %s, and a turn is dropped whole or kept whole",
						keepMessages, message.Role)
				}
				pastCut = true
			}
			if !pastCut {
				messages++
				kept = append(kept, rec)
			}
			continue
		}
		if !pastCut || carriesBudgetAccounting(rec.Type) {
			kept = append(kept, rec)
		}
	}
	if start.ID == "" {
		return nil, fmt.Errorf("session %s has no start record", id)
	}
	if raceBranchPending {
		return nil, fmt.Errorf("%w: session %s", ErrRaceBranchPending, id)
	}
	if messages < keepMessages {
		return nil, fmt.Errorf("session %s holds %d messages, cannot keep %d", id, messages, keepMessages)
	}

	if target == "" {
		target = provider.RouteTargetID(start.Target)
	}
	fork, err := s.Create(start.Workspace, target, start.CatalogRevision)
	if err != nil {
		return nil, err
	}
	for _, rec := range kept {
		if err := fork.appendCopied(rec); err != nil {
			fork.Close()
			return nil, err
		}
	}
	if allowEmpty {
		// Retry discards conversation and its Usage records, not the bill for
		// requests already sent. Carry the dropped observed cost as an external
		// charge so repeated retries cannot reset a hard ceiling, while keeping
		// token/call telemetry honest for the new branch.
		var snapshot State
		if sourceState != nil {
			snapshot = *sourceState
		} else {
			var stateErr error
			snapshot, stateErr = ReadState(path)
			if stateErr != nil {
				fork.Close()
				return nil, stateErr
			}
		}
		droppedCost := snapshot.AccountedCostMicroUSD() - fork.State().AccountedCostMicroUSD()
		if droppedCost > 0 {
			if err := fork.AppendBudgetTransfer("retry:"+id, droppedCost, 0); err != nil {
				fork.Close()
				return nil, err
			}
		}
	}
	// Provenance rides the log, so an exported or audited fork names where
	// its history came from.
	if err := fork.AppendNote("info", fmt.Sprintf("forked from %s, keeping %d messages", id, keepMessages)); err != nil {
		fork.Close()
		return nil, err
	}
	return fork, nil
}

// carriesBudgetAccounting names records that survive a conversation rewind.
// A retry can discard messages and tool work, but it cannot un-send provider
// requests or un-spend delegated/raced calls already admitted by this ledger.
func carriesBudgetAccounting(t RecordType) bool {
	switch t {
	case RecordRetryReserve, RecordBudgetAttempt, RecordBudgetSettle, RecordBudgetTransfer:
		return true
	default:
		return false
	}
}

// appendCopied writes a record carried over by a fork, keeping the source's
// timestamp: the moment the turn happened is a fact about the turn, not
// about the copy.
func (s *Session) appendCopied(rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return fmt.Errorf("%w: %v", ErrSessionPoisoned, s.poisoned)
	}
	s.seq++
	copied := Record{Seq: s.seq, At: rec.At, Type: rec.Type, Payload: rec.Payload}
	frame, err := encodeRecord(copied)
	if err != nil {
		return err
	}
	if err := s.writeFrame(frame); err != nil {
		return fmt.Errorf("appending to forked log: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		s.poisoned = fmt.Errorf("syncing copied record %d: %w", s.seq, err)
		return fmt.Errorf("%w: %v", ErrSessionPoisoned, s.poisoned)
	}
	return s.apply(copied)
}
