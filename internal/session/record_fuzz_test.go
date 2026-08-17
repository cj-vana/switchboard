package session

import (
	"bufio"
	"bytes"
	"testing"
)

// The log is the source of truth and replay is the recovery path, so the
// decoder must refuse arbitrary bytes with an error, never a panic: a
// crash-recovered file's tail is arbitrary bytes by definition.
func FuzzDecodeRecord(f *testing.F) {
	f.Add([]byte("{}\n"))
	f.Add([]byte("garbage with no frame"))
	f.Add([]byte("\x00\x00\x00\xff"))
	f.Add([]byte(`{"seq":1,"type":"message","payload":{"role":"user"}}` + "\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bufio.NewReader(bytes.NewReader(data))
		for i := 0; i < 64; i++ {
			if _, _, err := decodeRecord(r); err != nil {
				return
			}
		}
	})
}
