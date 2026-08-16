package mcp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// httpTransport speaks Streamable HTTP: every client message is a POST, and
// the answer arrives either as a single JSON body or as an SSE stream whose
// events are JSON-RPC messages. Both shapes funnel into one channel so the
// client's read loop sees the same thing stdio gives it.
type httpTransport struct {
	url    string
	client *http.Client

	mu       sync.Mutex
	session  string // Mcp-Session-Id, once the server assigns one
	protocol string // negotiated revision, echoed as a header after initialize

	msgs   chan []byte
	closed chan struct{}
	once   sync.Once
}

func startHTTP(spec Spec) (*httpTransport, error) {
	if !strings.HasPrefix(spec.URL, "http://") && !strings.HasPrefix(spec.URL, "https://") {
		return nil, fmt.Errorf("mcp server %s: url must be http or https", spec.Name)
	}
	return &httpTransport{
		url: spec.URL,
		// The timeout bounds one tool call end to end. Five minutes is
		// generous for a tool and finite for a hang; a caller that wants less
		// cancels its own context and the pending response is discarded.
		client: &http.Client{Timeout: 5 * time.Minute},
		msgs:   make(chan []byte, 16),
		closed: make(chan struct{}),
	}, nil
}

// setProtocol records the negotiated revision; the spec wants it echoed on
// every request that follows initialize.
func (t *httpTransport) setProtocol(v string) {
	t.mu.Lock()
	t.protocol = v
	t.mu.Unlock()
}

func (t *httpTransport) Send(msg []byte) error {
	req, err := http.NewRequest(http.MethodPost, t.url, bytes.NewReader(msg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	t.mu.Lock()
	if t.session != "" {
		req.Header.Set("Mcp-Session-Id", t.session)
	}
	if t.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", t.protocol)
	}
	t.mu.Unlock()

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.session = sid
		t.mu.Unlock()
	}

	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		resp.Body.Close()
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		resp.Body.Close()
		return fmt.Errorf("mcp server answered %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxLine))
		if err != nil {
			return err
		}
		t.push(body)
		return nil
	case strings.HasPrefix(ct, "text/event-stream"):
		// The stream may carry several messages before the response to this
		// request; read it out in the background so Send returns and the
		// read loop dispatches whatever arrives.
		go t.readSSE(resp.Body)
		return nil
	default:
		resp.Body.Close()
		return fmt.Errorf("mcp server answered with unexpected content type %q", ct)
	}
}

func (t *httpTransport) readSSE(body io.ReadCloser) {
	defer body.Close()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), maxLine)

	var data []string
	flush := func() {
		if len(data) == 0 {
			return
		}
		t.push([]byte(strings.Join(data, "\n")))
		data = nil
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		// id:, event:, retry:, and comments carry nothing this client uses.
	}
	flush()
}

func (t *httpTransport) push(msg []byte) {
	select {
	case t.msgs <- msg:
	case <-t.closed:
	}
}

func (t *httpTransport) Recv() ([]byte, error) {
	select {
	case msg := <-t.msgs:
		return msg, nil
	case <-t.closed:
		return nil, errors.New("transport closed")
	}
}

func (t *httpTransport) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}
