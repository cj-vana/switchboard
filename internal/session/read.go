package session

// Read-only replay, for surfaces that summarize sessions rather than append
// to them. `sb cost` reads every log a workspace has recorded, and taking
// the append lock for that would make the summary fail whenever a session
// is open — which is exactly when someone asks what things are costing.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

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

	header, err := r.ReadString('\n')
	if err != nil {
		return State{}, fmt.Errorf("reading session header: %w", err)
	}
	var gotMagic string
	var version int
	if _, err := fmt.Sscanf(strings.TrimSpace(header), "%s %d", &gotMagic, &version); err != nil || gotMagic != magic {
		return State{}, fmt.Errorf("%s is not a switchboard session log", path)
	}
	if version > SchemaVersion {
		return State{}, fmt.Errorf("%w: log is schema %d, this binary understands %d", ErrSchemaTooNew, version, SchemaVersion)
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
