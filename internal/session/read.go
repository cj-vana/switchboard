package session

// Read-only replay, for surfaces that summarize sessions rather than append
// to them. `sb cost` reads every log a workspace has recorded, and taking
// the append lock for that would make the summary fail whenever a session
// is open — which is exactly when someone asks what things are costing.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cj-vana/switchboard/internal/provider"
)

// checkHeader validates a log's magic line and schema before any records are
// read, the same refusal an appending open makes.
func checkHeader(r *bufio.Reader, path string) error {
	header, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading session header: %w", err)
	}
	var gotMagic string
	var version int
	if _, err := fmt.Sscanf(strings.TrimSpace(header), "%s %d", &gotMagic, &version); err != nil || gotMagic != magic {
		return fmt.Errorf("%s is not a switchboard session log", path)
	}
	if version > SchemaVersion {
		return fmt.Errorf("%w: log is schema %d, this binary understands %d", ErrSchemaTooNew, version, SchemaVersion)
	}
	return nil
}

// ReadState replays a log without opening it for appending. A corrupt tail
// ends the replay where an appending open would have truncated it, but the
// file is left alone: a reader repairs nothing.
func ReadState(path string) (State, error) {
	f, err := os.Open(path)
	if err != nil {
		return State{}, err
	}
	defer f.Close()
	r := bufio.NewReader(f)

	if err := checkHeader(r, path); err != nil {
		return State{}, err
	}

	replay := &Session{}
	for {
		rec, _, err := decodeRecord(r)
		if errors.Is(err, io.EOF) || errors.Is(err, ErrCorruptRecord) {
			break
		}
		if err != nil {
			return State{}, err
		}
		if err := replay.apply(rec); err != nil {
			return State{}, err
		}
	}
	return replay.state, nil
}

// ReadRaces collects a log's race verdicts, read-only, same posture as
// ReadState: `sb races` sums the paired evidence a workspace has gathered,
// and holding the append lock for a summary would make the question
// unanswerable exactly while a session is racing.
func ReadRaces(path string) ([]Race, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)

	if err := checkHeader(r, path); err != nil {
		return nil, err
	}

	var out []Race
	for {
		rec, _, err := decodeRecord(r)
		if errors.Is(err, io.EOF) || errors.Is(err, ErrCorruptRecord) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if rec.Type != RecordRace {
			continue
		}
		var race Race
		if err := json.Unmarshal(rec.Payload, &race); err != nil {
			return nil, err
		}
		out = append(out, race)
	}
}

// ReadUsages collects a log's per-call usage records, read-only. The replayed
// State sums them, and a sum is the wrong shape for counterfactual pricing:
// catalog prices are banded by the size of one call, so repricing a session on
// another rung has to see each call, not the total the calls added up to.
func ReadUsages(path string) ([]Usage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)

	if err := checkHeader(r, path); err != nil {
		return nil, err
	}

	var out []Usage
	for {
		rec, _, err := decodeRecord(r)
		if errors.Is(err, io.EOF) || errors.Is(err, ErrCorruptRecord) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if rec.Type != RecordUsage {
			continue
		}
		var u Usage
		if err := json.Unmarshal(rec.Payload, &u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
}

// ReadOpening returns the first words the user sent, for listings that need
// to say what a session was about without replaying what it became. It stops
// at the first user message that carries text, so labelling a directory of
// long sessions reads a few records from the head of each log, not the logs.
// A session with no user turn yet answers "", not an error: the caller has an
// id to show, and an empty log is a session, not a failure.
func ReadOpening(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReader(f)

	if err := checkHeader(r, path); err != nil {
		return "", err
	}

	for {
		rec, _, err := decodeRecord(r)
		if errors.Is(err, io.EOF) || errors.Is(err, ErrCorruptRecord) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if rec.Type != RecordMessage {
			continue
		}
		var m provider.Message
		if err := json.Unmarshal(rec.Payload, &m); err != nil {
			return "", err
		}
		// A user-role message whose blocks are all tool results renders no
		// text and is not the user speaking; keep looking.
		if m.Role == provider.RoleUser {
			if text := strings.TrimSpace(m.Text()); text != "" {
				return text, nil
			}
		}
	}
}
