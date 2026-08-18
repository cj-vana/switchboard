package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type observedHTTPRequest struct {
	Header http.Header
	Body   []byte
}

func decodeObservedRequest(t *testing.T, observed observedHTTPRequest) (string, map[string]json.RawMessage) {
	t.Helper()
	var request struct {
		Method string                     `json:"method"`
		Params map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(observed.Body, &request); err != nil {
		t.Fatalf("request body = %s: %v", observed.Body, err)
	}
	return request.Method, request.Params
}

func decodeMeta(t *testing.T, params map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(params["_meta"], &meta); err != nil {
		t.Fatalf("params._meta = %s: %v", params["_meta"], err)
	}
	return meta
}

func TestHTTPModernEnvelopeAndHeaders(t *testing.T) {
	requests := make(chan observedHTTPRequest, 4)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- observedHTTPRequest{Header: r.Header.Clone(), Body: body}

		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &request)
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "server/discover":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"modern-test","version":"1.2.3"}}}}`, request.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","tools":[{"name":"echo","inputSchema":{"type":"object"}}]}}`, request.ID)
		case "tools/call":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","content":[{"type":"text","text":"ok"}]}}`, request.ID)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, Spec{Name: "modern", URL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Call(ctx, "echo", json.RawMessage(`{"text":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	if got := c.ServerLine(); !strings.Contains(got, "modern-test 1.2.3") || !strings.Contains(got, modernProtocolVersion) {
		t.Errorf("ServerLine() = %q", got)
	}

	var observed []observedHTTPRequest
	for range 3 {
		select {
		case request := <-requests:
			observed = append(observed, request)
		case <-time.After(time.Second):
			t.Fatal("server did not receive the expected request")
		}
	}
	wantMethods := []string{"server/discover", "tools/list", "tools/call"}
	for i, request := range observed {
		method, params := decodeObservedRequest(t, request)
		if method != wantMethods[i] {
			t.Fatalf("request %d method = %q, want %q", i, method, wantMethods[i])
		}
		meta := decodeMeta(t, params)
		var version string
		if err := json.Unmarshal(meta["io.modelcontextprotocol/protocolVersion"], &version); err != nil || version != modernProtocolVersion {
			t.Errorf("request %s protocol metadata = %q, %v", method, version, err)
		}
		var capabilities map[string]json.RawMessage
		if err := json.Unmarshal(meta["io.modelcontextprotocol/clientCapabilities"], &capabilities); err != nil || capabilities == nil {
			t.Errorf("request %s client capabilities = %s, %v", method, meta["io.modelcontextprotocol/clientCapabilities"], err)
		}
		var info implementation
		if err := json.Unmarshal(meta["io.modelcontextprotocol/clientInfo"], &info); err != nil || info.Name != "switchboard" {
			t.Errorf("request %s client info = %+v, %v", method, info, err)
		}
		if got := request.Header.Get("MCP-Protocol-Version"); got != version {
			t.Errorf("request %s protocol header = %q, body = %q", method, got, version)
		}
		if got := request.Header.Get("Mcp-Method"); got != method {
			t.Errorf("request %s method header = %q", method, got)
		}
		if request.Header.Get("Mcp-Session-Id") != "" {
			t.Errorf("modern request %s carried a session id", method)
		}
		if !strings.Contains(request.Header.Get("Accept"), "application/json") || !strings.Contains(request.Header.Get("Accept"), "text/event-stream") {
			t.Errorf("request %s Accept = %q", method, request.Header.Get("Accept"))
		}
		wantName := ""
		if method == "tools/call" {
			wantName = "echo"
		}
		if got := request.Header.Get("Mcp-Name"); got != wantName {
			t.Errorf("request %s Mcp-Name = %q, want %q", method, got, wantName)
		}
	}
}

func TestHTTPMirrorsAnnotatedToolParametersAndSkipsInvalidTools(t *testing.T) {
	callHeaders := make(chan http.Header, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &request)
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "server/discover":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}}`, request.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","tools":[{"name":"annotated","inputSchema":{"type":"object","properties":{"tenant":{"type":"string","x-mcp-header":"Tenant"},"options":{"type":"object","properties":{"active":{"type":"boolean","x-mcp-header":"Active"},"count":{"type":"integer","x-mcp-header":"Count"},"note":{"type":"string","x-mcp-header":"Note"}}}}}},{"name":"invalid","inputSchema":{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string","x-mcp-header":"Row"}}}}}}}]}}`, request.ID)
		case "tools/call":
			callHeaders <- r.Header.Clone()
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","content":[{"type":"text","text":"ok"}]}}`, request.ID)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	var logsMu sync.Mutex
	var logs []string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, Spec{Name: "headers", URL: srv.URL}, func(_, text string) {
		logsMu.Lock()
		logs = append(logs, text)
		logsMu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools := c.Tools()
	if len(tools) != 1 || tools[0].Name != "annotated" {
		t.Fatalf("tools = %+v, want only the valid annotated tool", tools)
	}
	logsMu.Lock()
	joinedLogs := strings.Join(logs, "\n")
	logsMu.Unlock()
	if !strings.Contains(joinedLogs, "tool invalid skipped") || !strings.Contains(joinedLogs, "items") {
		t.Errorf("invalid tool warning = %q", joinedLogs)
	}

	args := json.RawMessage(`{"tenant":"Hello, 世界","options":{"active":false,"count":42.0,"note":null}}`)
	result, err := c.Call(ctx, "annotated", args)
	if err != nil || result.Content != "ok" {
		t.Fatalf("Call result = %+v, error = %v", result, err)
	}
	headers := <-callHeaders
	if got, want := headers.Get("Mcp-Param-Tenant"), encodeMCPHeaderValue("Hello, 世界"); got != want {
		t.Errorf("tenant header = %q, want %q", got, want)
	}
	if got := headers.Get("Mcp-Param-Active"); got != "false" {
		t.Errorf("active header = %q, want false", got)
	}
	if got := headers.Get("Mcp-Param-Count"); got != "42" {
		t.Errorf("count header = %q, want 42", got)
	}
	if got := headers.Get("Mcp-Param-Note"); got != "" {
		t.Errorf("null note produced header %q", got)
	}
}

func TestToolHeaderSchemaValidation(t *testing.T) {
	tests := []struct {
		name   string
		schema string
	}{
		{name: "root annotation", schema: `{"type":"object","x-mcp-header":"Root"}`},
		{name: "invalid token", schema: `{"type":"object","properties":{"x":{"type":"string","x-mcp-header":"Bad Header"}}}`},
		{name: "number", schema: `{"type":"object","properties":{"x":{"type":"number","x-mcp-header":"X"}}}`},
		{name: "case insensitive duplicate", schema: `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"Tenant"},"b":{"type":"string","x-mcp-header":"tenant"}}}`},
		{name: "array path", schema: `{"type":"object","properties":{"a":{"type":"array","items":{"type":"string","x-mcp-header":"A"}}}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseToolHeaderBindings(json.RawMessage(test.schema)); err == nil {
				t.Fatal("invalid x-mcp-header schema was accepted")
			}
		})
	}
}

func TestMirroredIntegerRejectsUnsafeRange(t *testing.T) {
	bindings := []toolHeaderBinding{{name: "Count", path: []string{"count"}, valueType: "integer"}}
	_, err := mirroredToolHeaders(bindings, json.RawMessage(`{"count":9007199254740992}`))
	if err == nil || !strings.Contains(err.Error(), "safe range") {
		t.Fatalf("mirroredToolHeaders error = %v, want safe-range rejection", err)
	}
}

func TestHTTPModernResponsePostIsExplicitlyRejected(t *testing.T) {
	var count atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	transport, err := startHTTP(Spec{Name: "response", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	transport.setProtocol(modernProtocolVersion)
	message := []byte(`{"jsonrpc":"2.0","id":7,"result":{}}`)
	err = transport.Send(context.Background(), message)
	if err == nil || !strings.Contains(err.Error(), "does not permit client response POSTs") {
		t.Fatalf("Send error = %v, want explicit modern HTTP response rejection", err)
	}
	if count.Load() != 0 {
		t.Fatalf("server received %d invalid response POSTs", count.Load())
	}
}

func TestHTTPLegacyResponsePostRetainsSessionHeaders(t *testing.T) {
	observed := make(chan observedHTTPRequest, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		observed <- observedHTTPRequest{Header: r.Header.Clone(), Body: body}
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	transport, err := startHTTP(Spec{Name: "legacy-response", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	transport.setProtocol(legacyProtocolVersion)
	transport.mu.Lock()
	transport.session = "session-123"
	transport.mu.Unlock()
	message := []byte(`{"jsonrpc":"2.0","id":7,"result":{}}`)
	if err := transport.Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	request := <-observed
	if got := request.Header.Get("MCP-Protocol-Version"); got != legacyProtocolVersion {
		t.Errorf("protocol header = %q, want %q", got, legacyProtocolVersion)
	}
	if got := request.Header.Get("Mcp-Session-Id"); got != "session-123" {
		t.Errorf("session header = %q, want session-123", got)
	}
	if got := request.Header.Get("Mcp-Method"); got != "" {
		t.Errorf("legacy response carried modern method header %q", got)
	}
}

func TestModernRequestMetadataMergesExistingMeta(t *testing.T) {
	raw, err := withModernMetadata(json.RawMessage(`{"cursor":"next","_meta":{"progressToken":"p-1","com.example/custom":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatal(err)
	}
	if string(params["cursor"]) != `"next"` {
		t.Errorf("cursor was not preserved: %s", params["cursor"])
	}
	meta := decodeMeta(t, params)
	if string(meta["progressToken"]) != `"p-1"` || string(meta["com.example/custom"]) != "true" {
		t.Errorf("caller metadata was not preserved: %s", params["_meta"])
	}
	for _, key := range []string{
		"io.modelcontextprotocol/protocolVersion",
		"io.modelcontextprotocol/clientInfo",
		"io.modelcontextprotocol/clientCapabilities",
	} {
		if len(meta[key]) == 0 {
			t.Errorf("required metadata %q is absent", key)
		}
	}
}

func TestMCPNameHeaderEncoding(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "plain", value: "echo", want: "echo"},
		{name: "unicode", value: "Hello, 世界", want: "=?base64?" + base64.StdEncoding.EncodeToString([]byte("Hello, 世界")) + "?="},
		{name: "padding", value: " padded ", want: "=?base64?" + base64.StdEncoding.EncodeToString([]byte(" padded ")) + "?="},
		{name: "interior tab", value: "hello\tworld", want: "=?base64?" + base64.StdEncoding.EncodeToString([]byte("hello\tworld")) + "?="},
		{name: "sentinel", value: "=?base64?literal?=", want: "=?base64?" + base64.StdEncoding.EncodeToString([]byte("=?base64?literal?=")) + "?="},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := encodeMCPHeaderValue(test.value); got != test.want {
				t.Errorf("encodeMCPHeaderValue(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

func TestHTTPModernProbeFallsBackOnUnrecognized400(t *testing.T) {
	requests := make(chan observedHTTPRequest, 5)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- observedHTTPRequest{Header: r.Header.Clone(), Body: body}
		var request struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &request)
		switch request.Method {
		case "server/discover":
			http.Error(w, "legacy server", http.StatusBadRequest)
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "legacy-session")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"legacy-test","version":"1"}}}`, *request.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, *request.ID)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, Spec{Name: "legacy", URL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var observed []observedHTTPRequest
	for range 4 {
		select {
		case request := <-requests:
			observed = append(observed, request)
		case <-time.After(time.Second):
			t.Fatal("legacy transcript was incomplete")
		}
	}
	wantMethods := []string{"server/discover", "initialize", "notifications/initialized", "tools/list"}
	for i, request := range observed {
		method, params := decodeObservedRequest(t, request)
		if method != wantMethods[i] {
			t.Fatalf("request %d = %q, want %q", i, method, wantMethods[i])
		}
		if i == 0 {
			if request.Header.Get("Mcp-Method") != "server/discover" || len(params["_meta"]) == 0 {
				t.Errorf("modern probe lacks its metadata: headers=%v body=%s", request.Header, request.Body)
			}
			continue
		}
		if len(params["_meta"]) != 0 || request.Header.Get("Mcp-Method") != "" {
			t.Errorf("legacy request %s retained modern metadata: headers=%v body=%s", method, request.Header, request.Body)
		}
		if i >= 2 {
			if request.Header.Get("Mcp-Session-Id") != "legacy-session" || request.Header.Get("MCP-Protocol-Version") != legacyProtocolVersion {
				t.Errorf("legacy request %s lacks negotiated headers: %v", method, request.Header)
			}
		}
	}
}

func TestHTTPRecognizedModernErrorDoesNotFallbackAndPreservesData(t *testing.T) {
	var count atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		body, _ := io.ReadAll(r.Body)
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &request)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32021,"message":"roots required","data":{"requiredCapabilities":["roots"]}}}`, request.ID)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Connect(ctx, Spec{Name: "modern-error", URL: srv.URL}, nil)
	if err == nil {
		t.Fatal("recognized modern error unexpectedly fell back")
	}
	if count.Load() != 1 {
		t.Fatalf("server received %d requests, want only the modern probe", count.Load())
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32021 {
		t.Fatalf("error = %v, want typed -32021 RPCError", err)
	}
	var data struct {
		Required []string `json:"requiredCapabilities"`
	}
	if json.Unmarshal(rpcErr.Data, &data) != nil || len(data.Required) != 1 || data.Required[0] != "roots" {
		t.Errorf("RPC error data = %s", rpcErr.Data)
	}
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusBadRequest {
		t.Errorf("error lost its HTTP status: %v", err)
	}
}

func TestHTTPServerFailureDoesNotFallback(t *testing.T) {
	var count atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Connect(ctx, Spec{Name: "unavailable", URL: srv.URL}, nil)
	if err == nil {
		t.Fatal("server failure unexpectedly triggered a legacy fallback")
	}
	if count.Load() != 1 {
		t.Fatalf("server received %d requests, want only the modern probe", count.Load())
	}
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Connect error = %v, want typed 503 status", err)
	}
}

func TestHTTPInvalidSuccessBodyFailsPromptlyWithoutFallback(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed", body: `not-json`},
		{name: "wrong response id", body: `{"jsonrpc":"2.0","id":999,"result":{}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var count atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.body)
			})
			srv := httptest.NewServer(handler)
			t.Cleanup(srv.Close)

			_, err := Connect(context.Background(), Spec{Name: "invalid-success", URL: srv.URL}, nil)
			if err == nil || !strings.Contains(err.Error(), "matching JSON-RPC response") {
				t.Fatalf("Connect error = %v, want explicit mismatched success response", err)
			}
			if count.Load() != 1 {
				t.Fatalf("server received %d requests, want no legacy fallback", count.Load())
			}
		})
	}
}

func TestHTTPAcceptedRequestIsExplicitlyUnsupported(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	transport, err := startHTTP(Spec{Name: "async", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	message := []byte(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	err = transport.Send(context.Background(), message)
	if err == nil || !strings.Contains(err.Error(), "asynchronous server push is unsupported") {
		t.Fatalf("Send error = %v, want explicit asynchronous-response error", err)
	}
}

func TestHTTPUnsupportedVersionExplicitlyNegotiatesLegacy(t *testing.T) {
	methods := make(chan string, 5)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &request)
		methods <- request.Method
		switch request.Method {
		case "server/discover":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32022,"message":"unsupported","data":{"requested":"2026-07-28","supported":["2099-01-01","2025-06-18"]}}}`, *request.ID)
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"dual"}}}`, *request.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, *request.ID)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, Spec{Name: "dual", URL: srv.URL, Headers: map[string]string{"X-Secret": "2025"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i, want := range []string{"server/discover", "initialize", "notifications/initialized", "tools/list"} {
		select {
		case got := <-methods:
			if got != want {
				t.Fatalf("method %d = %q, want %q", i, got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("negotiated legacy transcript was incomplete")
		}
	}
}

func TestHTTPUnsupportedVersionNegotiatesAdvertisedOlderLegacy(t *testing.T) {
	requested := make(chan string, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &request)
		switch request.Method {
		case "server/discover":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32022,"message":"unsupported","data":{"requested":"2026-07-28","supported":["2025-03-26"]}}}`, *request.ID)
		case "initialize":
			requested <- request.Params.ProtocolVersion
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-03-26","serverInfo":{"name":"older"}}}`, *request.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, *request.ID)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, Spec{Name: "older", URL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.protocol != olderLegacyVersion {
		t.Fatalf("negotiated protocol = %q, want %q", c.protocol, olderLegacyVersion)
	}
	select {
	case got := <-requested:
		if got != olderLegacyVersion {
			t.Fatalf("initialize requested %q, want %q", got, olderLegacyVersion)
		}
	case <-time.After(time.Second):
		t.Fatal("legacy initialize was not sent")
	}
}

func TestHTTPConnectCancellationBeforeResponseHeaders(t *testing.T) {
	started := make(chan struct{})
	serverCanceled := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var once sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		once.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
			close(serverCanceled)
		case <-release:
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := Connect(ctx, Spec{Name: "blocked", URL: srv.URL}, nil)
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Connect error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Connect did not stop after cancellation")
	}
	select {
	case <-serverCanceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP server did not observe request cancellation")
	}
}

func TestHTTPCallCancellationClosesSSE(t *testing.T) {
	streamOpen := make(chan struct{})
	serverCanceled := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &request)
		if request.Method == "tools/call" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(streamOpen)
			select {
			case <-r.Context().Done():
				close(serverCanceled)
			case <-release:
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if request.Method == "server/discover" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}}`, request.ID)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","tools":[{"name":"wait","inputSchema":{"type":"object"}}]}}`, request.ID)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	connectCtx, stopConnect := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopConnect()
	c, err := Connect(connectCtx, Spec{Name: "sse", URL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	callCtx, cancelCall := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := c.Call(callCtx, "wait", nil)
		result <- err
	}()
	<-streamOpen
	cancelCall()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Call error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Call did not stop after cancellation")
	}
	select {
	case <-serverCanceled:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not observe cancellation")
	}
}

func TestHTTPMatchingSSEResponseClosesItsStream(t *testing.T) {
	serverCanceled := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &request)
		if request.Method == "tools/call" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"resultType\":\"complete\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}\n\n", request.ID)
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				close(serverCanceled)
			case <-release:
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if request.Method == "server/discover" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}}`, request.ID)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","tools":[{"name":"stream","inputSchema":{"type":"object"}}]}}`, request.ID)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, Spec{Name: "matching-sse", URL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	result, err := c.Call(context.Background(), "stream", nil)
	if err != nil || result.Content != "ok" {
		t.Fatalf("Call result = %+v, error = %v", result, err)
	}
	select {
	case <-serverCanceled:
	case <-time.After(time.Second):
		t.Fatal("matching SSE response left its HTTP stream open")
	}
}

func TestHTTPPrematureSSEEOFFailsPendingCall(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &request)
		if request.Method == "tools/call" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if request.Method == "server/discover" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}}`, request.ID)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","tools":[{"name":"broken","inputSchema":{"type":"object"}}]}}`, request.ID)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, Spec{Name: "premature-eof", URL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	result := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "broken", nil)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "stream closed before") {
			t.Fatalf("Call error = %v, want premature SSE EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Call remained pending after its SSE stream closed")
	}
}

func TestHTTPPrematureSSEEOFFailsOnlyItsOwnConcurrentCall(t *testing.T) {
	goodStarted := make(chan struct{})
	releaseGood := make(chan struct{})
	var startOnce sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &request)
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "server/discover":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}}`, request.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","tools":[{"name":"bad","inputSchema":{"type":"object"}},{"name":"good","inputSchema":{"type":"object"}}]}}`, request.ID)
		case "tools/call":
			if request.Params.Name == "bad" {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				return
			}
			startOnce.Do(func() { close(goodStarted) })
			<-releaseGood
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","content":[{"type":"text","text":"ok"}]}}`, request.ID)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, Spec{Name: "concurrent-eof", URL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	goodResult := make(chan error, 1)
	go func() {
		result, err := c.Call(context.Background(), "good", nil)
		if err == nil && result.Content != "ok" {
			err = fmt.Errorf("good call content = %q", result.Content)
		}
		goodResult <- err
	}()
	select {
	case <-goodStarted:
	case <-time.After(time.Second):
		t.Fatal("good concurrent call did not start")
	}

	if _, err := c.Call(context.Background(), "bad", nil); err == nil || !strings.Contains(err.Error(), "stream closed before") {
		t.Fatalf("bad Call error = %v, want premature SSE EOF", err)
	}
	select {
	case err := <-goodResult:
		t.Fatalf("bad SSE stream terminated the good concurrent call early: %v", err)
	default:
	}
	if err := c.Err(); err != nil {
		t.Fatalf("request-scoped SSE failure killed the client: %v", err)
	}
	close(releaseGood)
	select {
	case err := <-goodResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("good concurrent call did not complete")
	}
}

func TestHTTPSSEOversizeFailsPendingRequest(t *testing.T) {
	transport := &httpTransport{
		msgs: make(chan httpMessage, 1),
		done: make(chan struct{}),
	}
	body := io.NopCloser(strings.NewReader("data: " + strings.Repeat("x", 128) + "\n\n"))
	transport.readSSEWithLimit(body, func() {}, context.Background(), json.RawMessage(`1`), 64)
	_, err := transport.Recv()
	if err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("Recv error = %v, want bounded SSE scanner failure", err)
	}
}

func TestHTTPSSEMultilineAggregateIsBounded(t *testing.T) {
	transport := &httpTransport{
		msgs: make(chan httpMessage, 1),
		done: make(chan struct{}),
	}
	body := io.NopCloser(strings.NewReader("data: " + strings.Repeat("a", 40) + "\ndata: " + strings.Repeat("b", 40) + "\n\n"))
	transport.readSSEWithLimit(body, func() {}, context.Background(), json.RawMessage(`1`), 64)
	_, err := transport.Recv()
	if err == nil || !strings.Contains(err.Error(), "event exceeds 64 bytes") {
		t.Fatalf("Recv error = %v, want aggregate SSE event limit", err)
	}
}

func TestHTTPSSEEventLineCountIsBounded(t *testing.T) {
	transport := &httpTransport{
		msgs: make(chan httpMessage, 1),
		done: make(chan struct{}),
	}
	body := io.NopCloser(strings.NewReader(strings.Repeat("data:\n", 65) + "\n"))
	transport.readSSEWithLimit(body, func() {}, context.Background(), json.RawMessage(`1`), 64)
	_, err := transport.Recv()
	if err == nil || !strings.Contains(err.Error(), "event exceeds 64 data lines") {
		t.Fatalf("Recv error = %v, want bounded SSE event line count", err)
	}
}

func TestHTTPCloseCancelsActiveSSEAndWaitsForReader(t *testing.T) {
	streamOpen := make(chan struct{})
	serverCanceled := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(streamOpen)
		select {
		case <-r.Context().Done():
			close(serverCanceled)
		case <-release:
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	transport, err := startHTTP(Spec{Name: "sse-close", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	message := []byte(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	if err := transport.Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	select {
	case <-streamOpen:
	case <-time.After(time.Second):
		t.Fatal("SSE stream did not open")
	}

	closed := make(chan struct{})
	go func() {
		_ = transport.Close()
		close(closed)
	}()
	select {
	case <-serverCanceled:
	case <-time.After(time.Second):
		t.Fatal("transport Close did not cancel the active SSE request")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("transport Close did not wait for the SSE reader to stop")
	}
}

type blockingSendTransport struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (t *blockingSendTransport) Send(ctx context.Context, _ []byte) error {
	t.once.Do(func() { close(t.started) })
	<-ctx.Done()
	return ctx.Err()
}

func (t *blockingSendTransport) Recv() ([]byte, error) {
	<-t.closed
	return nil, errors.New("closed")
}

func (t *blockingSendTransport) Close() error {
	select {
	case <-t.closed:
	default:
		close(t.closed)
	}
	return nil
}

func TestCallCancellationDuringSendForgetsPending(t *testing.T) {
	transport := &blockingSendTransport{started: make(chan struct{}), closed: make(chan struct{})}
	c := &Client{
		spec:      Spec{Name: "blocked"},
		logf:      func(string, string) {},
		transport: transport,
		pending:   map[int64]chan rpcResponse{},
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, "wait", nil)
		result <- err
	}()
	<-transport.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v, want context.Canceled", err)
	}
	c.mu.Lock()
	pending := len(c.pending)
	c.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending calls = %d after canceled Send", pending)
	}
}

func TestStdioProbeClassification(t *testing.T) {
	tests := []struct {
		name       string
		response   string
		wantLegacy bool
		wantCode   int
	}{
		{name: "method not found", response: `{"code":-32601,"message":"unknown"}`, wantLegacy: true},
		{name: "invalid params", response: `{"code":-32602,"message":"initialize first"}`, wantLegacy: true},
		{name: "modern capability error", response: `{"code":-32021,"message":"roots required","data":{"requiredCapabilities":["roots"]}}`, wantCode: -32021},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wire := newFakeTransport()
			client := newClient(Spec{Name: "probe"}, wire, func(string, string) {})
			t.Cleanup(func() { client.Close() })
			go func() {
				raw := <-wire.toServer
				var request struct {
					ID     int64                      `json:"id"`
					Method string                     `json:"method"`
					Params map[string]json.RawMessage `json:"params"`
				}
				_ = json.Unmarshal(raw, &request)
				wire.fromServer <- []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":%s}`, request.ID, test.response))
			}()

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			decision, err := client.probeModern(ctx, transportStdio)
			if test.wantLegacy {
				if err != nil || decision.era != eraLegacy {
					t.Fatalf("decision = %+v, err = %v, want legacy", decision, err)
				}
				return
			}
			var rpcErr *RPCError
			if !errors.As(err, &rpcErr) || rpcErr.Code != test.wantCode {
				t.Fatalf("error = %v, want RPC code %d", err, test.wantCode)
			}
		})
	}
}

func TestUnsupportedResultTypeIsExplicit(t *testing.T) {
	c, _ := connectFake(t, echoTool, func(string, json.RawMessage) string {
		return `{"resultType":"input_required","inputRequests":{"confirm":{"type":"elicitation"}}}`
	})
	_, err := c.Call(context.Background(), "echo", nil)
	var unsupported *UnsupportedResultTypeError
	if !errors.As(err, &unsupported) || unsupported.ResultType != "input_required" {
		t.Fatalf("Call error = %v, want explicit input_required error", err)
	}
}
