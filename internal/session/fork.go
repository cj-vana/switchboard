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

	"github.com/cj-vana/switchboard/internal/provider"
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
	if keepMessages < 1 {
		return nil, fmt.Errorf("a fork keeping no messages is an empty session; /clear is how those start")
	}
	matches, err := filepath.Glob(filepath.Join(s.root, "*", id+".log"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("session %s not found", id)
	}

	f, err := os.Open(matches[0])
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
		return nil, fmt.Errorf("%s is not a switchboard session log", matches[0])
	}
	if version > SchemaVersion {
		return nil, fmt.Errorf("%w: log is schema %d, this binary understands %d", ErrSchemaTooNew, version, SchemaVersion)
	}

	var start SessionStart
	var kept []Record
	messages := 0
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
		if rec.Type == RecordMessage {
			if messages == keepMessages {
				var m provider.Message
				if err := json.Unmarshal(rec.Payload, &m); err != nil {
					return nil, err
				}
				if m.Role != provider.RoleUser {
					return nil, fmt.Errorf(
						"the cut falls inside a turn: message %d is %s, and a turn is dropped whole or kept whole",
						keepMessages, m.Role)
				}
				break
			}
			messages++
		}
		kept = append(kept, rec)
	}
	if start.ID == "" {
		return nil, fmt.Errorf("session %s has no start record", id)
	}
	if messages < keepMessages {
		return nil, fmt.Errorf("session %s holds %d messages, cannot keep %d", id, messages, keepMessages)
	}

	fork, err := s.Create(start.Workspace, provider.RouteTargetID(start.Target), start.CatalogRevision)
	if err != nil {
		return nil, err
	}
	for _, rec := range kept {
		if err := fork.appendCopied(rec); err != nil {
			fork.Close()
			return nil, err
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

// appendCopied writes a record carried over by a fork, keeping the source's
// timestamp: the moment the turn happened is a fact about the turn, not
// about the copy.
func (s *Session) appendCopied(rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	copied := Record{Seq: s.seq, At: rec.At, Type: rec.Type, Payload: rec.Payload}
	frame, err := encodeRecord(copied)
	if err != nil {
		return err
	}
	if _, err := s.f.Write(frame); err != nil {
		return fmt.Errorf("appending to forked log: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	return s.apply(copied)
}
