package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"

	"github.com/cjvana/switchboard/internal/provider"
)

// The log is a header line followed by framed records:
//
//	switchboard-session 1\n
//	<8-hex payload length> <8-hex crc32c> <compact json>\n
//
// Length and checksum together let replay tell a torn final write from a clean
// end of file, which is the whole point of the format: a session interrupted by
// a kill signal must resume, not refuse to load. Line framing is safe because
// encoding/json compacts and escapes payloads, so no record body contains a raw
// newline.
const (
	magic         = "switchboard-session"
	SchemaVersion = 1

	frameHeaderLen = 18 // 8 hex + space + 8 hex + space
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// ErrCorruptRecord ends replay. It is not surfaced to the caller as a failure:
// the log truncates at that point and resumes from the last valid state, with
// the loss reported (§10.3).
var ErrCorruptRecord = errors.New("corrupt record")

// ErrSchemaTooNew refuses a log written by a newer binary. A best-effort parse
// would silently drop records it does not understand.
var ErrSchemaTooNew = errors.New("session was written by a newer version of switchboard")

type RecordType string

const (
	RecordSessionStart RecordType = "session_start"
	RecordMessage      RecordType = "message"
	RecordUsage        RecordType = "usage"
	RecordPermission   RecordType = "permission"
	RecordNote         RecordType = "note"
)

type Record struct {
	Seq     int             `json:"seq"`
	At      time.Time       `json:"at"`
	Type    RecordType      `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type SessionStart struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	Target    string `json:"target"`
	Binary    string `json:"binary"`

	// CatalogRevision pins the price and capability data this session started
	// against, so a cost recorded here can be checked later against the data
	// that actually produced it rather than whatever is current (§4).
	CatalogRevision string `json:"catalog_revision,omitempty"`
}

// Usage records one model call. Attempts counts requests actually issued: a
// retry after partial output is billed by most providers, so recording only the
// successful attempt would understate spend (§10.3).
type Usage struct {
	Target   string         `json:"target"`
	Usage    provider.Usage `json:"usage"`
	Duration time.Duration  `json:"duration_ns"`
	Attempts int            `json:"attempts"`

	// CostMicroUSD is what the catalog priced this call at, in millionths of
	// a dollar. It is stored as a plain integer so the session log stays
	// independent of the catalog's types, and micro-USD rather than cents
	// because a single turn routinely costs a fraction of a cent.
	CostMicroUSD int64 `json:"cost_micro_usd,omitempty"`

	// CatalogRevision and PriceConfidence record what produced the number. A
	// cost with neither is not reproducible, and one priced from a surface
	// default is shape rather than fact.
	CatalogRevision string `json:"catalog_revision,omitempty"`
	PriceConfidence string `json:"price_confidence,omitempty"`
}

type Permission struct {
	Tool     string `json:"tool"`
	Mode     string `json:"mode"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

type Note struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

func encodeRecord(rec Record) ([]byte, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("encoding %s record: %w", rec.Type, err)
	}
	frame := make([]byte, 0, frameHeaderLen+len(payload)+1)
	frame = fmt.Appendf(frame, "%08x %08x ", len(payload), crc32.Checksum(payload, crcTable))
	frame = append(frame, payload...)
	return append(frame, '\n'), nil
}

// decodeRecord reads one framed record. It returns ErrCorruptRecord for a torn
// or altered frame and io.EOF at a clean end of log.
func decodeRecord(r *bufio.Reader) (Record, int, error) {
	line, err := r.ReadBytes('\n')
	if len(line) == 0 {
		if err == nil {
			err = io.EOF
		}
		return Record{}, 0, err
	}
	consumed := len(line)

	// A line with no terminator is the tail of a write that did not finish.
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Record{}, consumed, ErrCorruptRecord
		}
		return Record{}, consumed, err
	}
	if len(line) < frameHeaderLen+1 || line[8] != ' ' || line[17] != ' ' {
		return Record{}, consumed, ErrCorruptRecord
	}

	var wantLen, wantCRC uint32
	if _, err := fmt.Sscanf(string(line[:frameHeaderLen-1]), "%08x %08x", &wantLen, &wantCRC); err != nil {
		return Record{}, consumed, ErrCorruptRecord
	}

	payload := line[frameHeaderLen : len(line)-1]
	if uint32(len(payload)) != wantLen || crc32.Checksum(payload, crcTable) != wantCRC {
		return Record{}, consumed, ErrCorruptRecord
	}

	var rec Record
	if err := json.Unmarshal(payload, &rec); err != nil {
		return Record{}, consumed, ErrCorruptRecord
	}
	return rec, consumed, nil
}
