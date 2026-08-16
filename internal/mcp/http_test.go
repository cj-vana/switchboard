package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// streamableServer implements enough of the Streamable HTTP transport to
// exercise both response shapes: initialize answers as plain JSON with a
// session id, tools/list answers as an SSE stream, tools/call echoes and
// asserts the session and protocol headers arrived.
func streamableServer(t *testing.T) (*httptest.Server, *sync.Map) {
	t.Helper()
	seen := &sync.Map{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ID == nil { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "session-123")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-03-26","serverInfo":{"name":"httpfake","version":"2"}}}`, *req.ID)
		case "tools/list":
			seen.Store("list-session", r.Header.Get("Mcp-Session-Id"))
			seen.Store("list-protocol", r.Header.Get("MCP-Protocol-Version"))
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"tools\":[{\"name\":\"probe\",\"inputSchema\":{\"type\":\"object\"}}]}}\n\n", *req.ID)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"probed"}]}}`, *req.ID)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, seen
}

func TestStreamableHTTPHandshakeAndCall(t *testing.T) {
	srv, seen := streamableServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := Connect(ctx, Spec{Name: "httpfake", URL: srv.URL}, func(string, string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if got := c.Tools(); len(got) != 1 || got[0].Name != "probe" {
		t.Fatalf("tools = %+v, want the probe tool from the SSE response", got)
	}

	// The session id the server assigned on initialize must ride every
	// later request, and the negotiated protocol must be echoed back.
	if sid, _ := seen.Load("list-session"); sid != "session-123" {
		t.Errorf("tools/list carried session %q, want session-123", sid)
	}
	if proto, _ := seen.Load("list-protocol"); proto != "2025-03-26" {
		t.Errorf("tools/list carried protocol %q, want the negotiated 2025-03-26", proto)
	}

	res, err := c.Call(ctx, "probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "probed" {
		t.Errorf("call = %+v", res)
	}
}

func TestHTTPRefusesNonHTTPURL(t *testing.T) {
	_, err := startHTTP(Spec{Name: "x", URL: "ftp://nope"})
	if err == nil {
		t.Fatal("a non-http url must be refused")
	}
}

func TestHTTPErrorStatusSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "who are you", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Connect(ctx, Spec{Name: "denied", URL: srv.URL}, func(string, string) {})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want the 401 surfaced", err)
	}
}
