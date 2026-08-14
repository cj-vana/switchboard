package eval

import (
	"encoding/json"
	"os"
	"sync"
)

// Journal writes each attempt as it finishes.
//
// A long corpus run is hours of billable work, and a harness that holds its
// results in memory until the end loses all of them to one timeout. That is not
// hypothetical: a three hour run against twenty tasks died on its deadline and
// left nothing but a stack trace, because the test framework buffers its own
// log until the test returns and a panic never returns.
//
// So results are durable at the moment they exist. A run that dies half way
// through is half a measurement, which is worth considerably more than none.
type Journal struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

func NewJournal(path string) (*Journal, error) {
	// Append, so resuming a killed run adds to the record rather than replacing
	// what survived it.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Journal{file: f, enc: json.NewEncoder(f)}, nil
}

// Append records one attempt and syncs it. The sync is the point: without it
// the last minutes of a run sit in a buffer the kill signal discards.
func (j *Journal) Append(r Run) error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	if err := j.enc.Encode(r); err != nil {
		return err
	}
	return j.file.Sync()
}

func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	return j.file.Close()
}

// ReadJournal recovers the runs a previous invocation completed, so a killed run
// can be reported on or continued rather than repeated.
func ReadJournal(path string) ([]Run, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Run
	dec := json.NewDecoder(f)
	for {
		var r Run
		if err := dec.Decode(&r); err != nil {
			// A truncated final line is what a killed run leaves behind. The
			// records before it are still good, and discarding them because the
			// last one is short would repeat the mistake this file prevents.
			return out, nil
		}
		out = append(out, r)
	}
}

// Done reports which attempts a journal already holds, keyed the way a run is
// identified, so a resumed run can skip them.
func Done(runs []Run) map[string]bool {
	out := map[string]bool{}
	for _, r := range runs {
		out[r.Arm+"\x00"+r.TaskID+"\x00"+string(rune('0'+r.Seed))] = true
	}
	return out
}
