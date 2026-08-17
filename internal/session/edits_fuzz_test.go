package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// Blame replays what this reader returns, and a session log on disk is
// bytes the product does not control: a crash truncates it, a hand can
// tamper with it. The reader must come back with an error or a short
// read, never a panic — the decoder already holds that bar, and the
// state machine above it has to hold it too.
func FuzzReadFileEditRecords(f *testing.F) {
	frame := func(seq int, typ RecordType, payload string) []byte {
		rec := Record{Seq: seq, At: time.Unix(0, 0).UTC(), Type: typ, Payload: json.RawMessage(payload)}
		data, err := encodeRecord(rec)
		if err != nil {
			f.Fatal(err)
		}
		return data
	}
	var valid bytes.Buffer
	valid.Write(frame(1, RecordSessionStart, `{"id":"s1","workspace":"/ws"}`))
	valid.Write(frame(2, RecordMessage, `{"role":"user","content":[{"kind":"text","data":{"text":"go"}}]}`))
	valid.Write(frame(3, RecordMessage, `{"role":"assistant","content":[{"kind":"tool_use","data":{"id":"w1","name":"write","input":{"path":"a.go","content":"x"}}}]}`))
	valid.Write(frame(4, RecordUsage, `{"target":"ollama/local/q"}`))
	valid.Write(frame(5, RecordMessage, `{"role":"tool","content":[{"kind":"tool_result","data":{"tool_use_id":"w1","name":"write","content":"ok"}}]}`))
	valid.Write(frame(6, RecordRoute, `{"turn_depth":0,"tier":"t1","target":"ollama/local/q"}`))

	f.Add(valid.Bytes())
	f.Add(frame(1, RecordMessage, `{"role":"assistant"}`))
	f.Add([]byte("not a frame at all"))
	f.Add([]byte("\x00\x00\x00\xff"))
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bufio.NewReader(bytes.NewReader(data))
		_, _ = readFileEditRecords(r)
	})
}
