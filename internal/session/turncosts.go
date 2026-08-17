package session

// Read-only per-turn metering, for the surface that answers "which asks
// cost the most". The log already interleaves each turn's opening with
// the usage records its calls produced; this reader just folds them back
// onto the turn, the same walk the edit reader makes.

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/cj-vana/switchboard/internal/provider"
)

// TurnCost is one user turn's metering: what its calls consumed, summed
// from the usage records that rode between its opening and the next.
type TurnCost struct {
	Turn         int
	Prompt       string
	Calls        int
	Usage        provider.Usage
	CostMicroUSD int64
}

// ReadTurnCosts replays a log and returns each turn's summed metering, in
// turn order. Usage recorded before any turn opened — nothing the loop
// writes today — is dropped rather than invented a home.
func ReadTurnCosts(path string) ([]TurnCost, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	if err := checkHeader(r, path); err != nil {
		return nil, err
	}

	var out []TurnCost
	for {
		rec, _, err := decodeRecord(r)
		if errors.Is(err, io.EOF) || errors.Is(err, ErrCorruptRecord) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		switch rec.Type {
		case RecordMessage:
			var m provider.Message
			if err := json.Unmarshal(rec.Payload, &m); err != nil {
				return nil, err
			}
			if OpensTurn(m) {
				out = append(out, TurnCost{Turn: len(out) + 1, Prompt: strings.TrimSpace(m.Text())})
			}
		case RecordUsage:
			if len(out) == 0 {
				continue
			}
			var u Usage
			if err := json.Unmarshal(rec.Payload, &u); err != nil {
				return nil, err
			}
			cur := &out[len(out)-1]
			cur.Calls++
			cur.Usage = cur.Usage.Add(u.Usage)
			cur.CostMicroUSD += u.CostMicroUSD
		}
	}
}
