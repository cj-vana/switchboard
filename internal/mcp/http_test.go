package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
		case "server/discover":
			// This fixture is intentionally a 2025-era server. An
			// unrecognized 400 is the Streamable HTTP downgrade signal.
			http.Error(w, "initialize first", http.StatusBadRequest)
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

func TestHTTPConfiguredHeadersAreAppliedAtRequestTime(t *testing.T) {
	const headerEnv = "SB_MCP_TEST_HEADER"
	const bearerEnv = "SB_MCP_TEST_BEARER"
	const ordinaryStatic = "ordinary-static-header"
	t.Setenv(headerEnv, "first-header-secret")
	t.Setenv(bearerEnv, "first-bearer-secret")
	observed := make(chan http.Header, 2)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "server/discover":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}}}}`, req.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"resultType":"complete","tools":[{"name":"echo","inputSchema":{"type":"object"}}]}}`, req.ID)
		case "tools/call":
			observed <- r.Header.Clone()
			text := strings.Join([]string{
				"ordinary tool data",
				ordinaryStatic,
				r.Header.Get("X-Monkey"),
				r.Header.Get("X-Hockey-Team"),
				r.Header.Get("X-API-Key"),
				strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
			}, " | ")
			encoded, _ := json.Marshal(text)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"resultType":"complete","content":[{"type":"text","text":%s}]}}`, req.ID, encoded)
		}
	}))
	t.Cleanup(srv.Close)

	spec := Spec{
		Name: "configured-headers",
		URL:  srv.URL,
		Headers: map[string]string{
			"X-Static":      ordinaryStatic,
			"X-Monkey":      "banana",
			"X-Hockey-Team": "avalanche",
		},
		HeaderEnv:         map[string]string{"X-API-Key": headerEnv},
		BearerTokenEnvVar: bearerEnv,
	}
	c, err := Connect(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	t.Setenv(headerEnv, "second-header-secret")
	t.Setenv(bearerEnv, "second-bearer-secret")
	result, err := c.Call(context.Background(), "echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "ordinary tool data | ordinary-static-header | banana | avalanche | [redacted] | [redacted]" {
		t.Fatalf("successful tool result did not redact request credentials: %+v", result)
	}
	headers := <-observed
	if got := headers.Get("X-Static"); got != ordinaryStatic {
		t.Errorf("X-Static = %q", got)
	}
	if got := headers.Get("X-API-Key"); got != "second-header-secret" {
		t.Errorf("X-API-Key = %q; env was not re-read", got)
	}
	if got := headers.Get("Authorization"); got != "Bearer second-bearer-secret" {
		t.Errorf("Authorization = %q; bearer env was not re-read", got)
	}
}

func TestHTTPHeaderConfigurationValidation(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
	}{
		{"reserved", Spec{Headers: map[string]string{"Content-Type": "text/plain"}}},
		{"protocol parameter", Spec{Headers: map[string]string{"Mcp-Param-Token": "x"}}},
		{"case duplicate", Spec{Headers: map[string]string{"X-Key": "a", "x-key": "b"}}},
		{"cross-map duplicate", Spec{Headers: map[string]string{"X-Key": "a"}, HeaderEnv: map[string]string{"x-KEY": "VALUE"}}},
		{"bearer conflict", Spec{Headers: map[string]string{"Authorization": "token"}, BearerTokenEnvVar: "TOKEN"}},
		{"invalid static value", Spec{Headers: map[string]string{"X-Key": "secret\r\nInjected: true"}}},
		{"invalid env name", Spec{HeaderEnv: map[string]string{"X-Key": "NOT-AN-ENV"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.spec.Name = "invalid"
			test.spec.URL = "https://example.invalid/mcp"
			if err := test.spec.validate(); err == nil {
				t.Fatal("invalid header configuration was accepted")
			}
		})
	}
}

func TestHTTPHeaderEnvironmentFailuresDoNotLeakValues(t *testing.T) {
	const envName = "SB_MCP_BAD_HEADER"
	t.Setenv(envName, "secret-value\r\nInjected: true")
	transport, err := startHTTP(Spec{
		Name:      "bad-header",
		URL:       "https://example.invalid/mcp",
		HeaderEnv: map[string]string{"X-Key": envName},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	err = transport.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err == nil || !strings.Contains(err.Error(), envName) {
		t.Fatalf("Send error = %v, want named environment failure", err)
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("Send error leaked environment value: %v", err)
	}
}

func TestHTTPMissingHeaderEnvironmentFailsBeforeNetwork(t *testing.T) {
	const envName = "SB_MCP_TEST_DEFINITELY_MISSING_HEADER"
	old, existed := os.LookupEnv(envName)
	if err := os.Unsetenv(envName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(envName, old)
		}
	})
	transport, err := startHTTP(Spec{
		Name:      "missing-header",
		URL:       "https://example.invalid/mcp",
		HeaderEnv: map[string]string{"X-Key": envName},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	err = transport.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err == nil || !strings.Contains(err.Error(), envName) {
		t.Fatalf("Send error = %v, want missing environment name", err)
	}
}

func TestHTTPDoesNotForwardConfiguredHeadersAcrossRedirects(t *testing.T) {
	reached := make(chan http.Header, 1)
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached <- http.Header{}
	}))
	t.Cleanup(destination.Close)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	_, err := Connect(context.Background(), Spec{
		Name:    "redirect",
		URL:     source.URL,
		Headers: map[string]string{"X-API-Key": "redirect-secret"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("Connect error = %v, want unfollowed redirect", err)
	}
	select {
	case <-reached:
		t.Fatal("redirect destination received a request")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHTTPRPCErrorsRedactConfiguredSecrets(t *testing.T) {
	for _, mediaType := range []string{"application/json", "text/event-stream"} {
		t.Run(mediaType, func(t *testing.T) {
			const secret = "987654321"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var req struct {
					ID int64 `json:"id"`
				}
				_ = json.Unmarshal(body, &req)
				w.Header().Set("Content-Type", mediaType)
				message := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32021,"message":"refused %s","data":{%q:"x","echo":%s}}}`, req.ID, secret, secret, secret)
				if mediaType == "text/event-stream" {
					fmt.Fprintf(w, "data: %s\n\n", message)
				} else {
					_, _ = io.WriteString(w, message)
				}
			}))
			t.Cleanup(srv.Close)
			_, err := Connect(context.Background(), Spec{
				Name:    "redaction",
				URL:     srv.URL,
				Headers: map[string]string{"X-Secret": secret},
			}, nil)
			if err == nil {
				t.Fatal("Connect unexpectedly succeeded")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked configured secret: %v", err)
			}
			var rpcErr *RPCError
			if !errors.As(err, &rpcErr) {
				t.Fatalf("error lost typed RPC error: %v", err)
			}
			if strings.Contains(string(rpcErr.Data), secret) {
				t.Fatalf("typed RPC data leaked configured secret: %s", rpcErr.Data)
			}
		})
	}
}

func TestHTTPResultDerivedErrorsRedactConfiguredSecrets(t *testing.T) {
	const secret = "result-derived-secret"
	for _, result := range []string{
		fmt.Sprintf(`{"resultType":%q,"supportedVersions":["2026-07-28"]}`, secret),
		fmt.Sprintf(`{"resultType":"complete","supportedVersions":[%q]}`, secret),
	} {
		t.Run(result, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var req struct {
					ID int64 `json:"id"`
				}
				_ = json.Unmarshal(body, &req)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, result)
			}))
			t.Cleanup(srv.Close)
			_, err := Connect(context.Background(), Spec{
				Name:    "result-redaction",
				URL:     srv.URL,
				Headers: map[string]string{"X-Secret": secret},
			}, nil)
			if err == nil {
				t.Fatal("Connect unexpectedly succeeded")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("derived error leaked configured secret: %v", err)
			}
		})
	}
}

func TestHTTPSSENotificationsUsePerRequestSecretSnapshot(t *testing.T) {
	const envName = "SB_MCP_ROTATING_HEADER"
	const requestSecret = "per-request-notification-secret"
	t.Setenv(envName, "initial-secret")
	callStarted := make(chan struct{})
	release := make(chan struct{})
	logs := make(chan string, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "server/discover":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"resultType":"complete","supportedVersions":["2026-07-28"]}}`, req.ID)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"resultType":"complete","tools":[{"name":"echo","inputSchema":{"type":"object"}}]}}`, req.ID)
		case "tools/call":
			close(callStarted)
			<-release
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":%q}}\n\n", requestSecret)
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"resultType\":\"complete\",\"content\":[]}}\n\n", req.ID)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := Connect(context.Background(), Spec{
		Name:      "notification-redaction",
		URL:       srv.URL,
		HeaderEnv: map[string]string{"X-Secret": envName},
	}, func(_ string, text string) { logs <- text })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	t.Setenv(envName, requestSecret)
	callDone := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "echo", nil)
		callDone <- err
	}()
	<-callStarted
	t.Setenv(envName, "rotated-after-request")
	close(release)
	if err := <-callDone; err != nil {
		t.Fatal(err)
	}
	select {
	case logged := <-logs:
		if strings.Contains(logged, requestSecret) {
			t.Fatalf("notification log leaked request-time secret: %s", logged)
		}
	case <-time.After(time.Second):
		t.Fatal("server notification was not logged")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type errorBody struct{ err error }

func (b errorBody) Read([]byte) (int, error) { return 0, b.err }
func (b errorBody) Close() error             { return nil }

func TestHTTPTransportAndBodyErrorsAreOpaque(t *testing.T) {
	const secret = "transport-secret"
	message := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)

	transport, err := startHTTP(Spec{Name: "opaque", URL: "https://example.invalid/mcp", Headers: map[string]string{"X-Key": secret}})
	if err != nil {
		t.Fatal(err)
	}
	transport.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("failed for %s with %s", request.URL, secret)
	})
	if err := transport.Send(context.Background(), message); err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "example.invalid") {
		t.Fatalf("transport error was not opaque: %v", err)
	}
	_ = transport.Close()

	transport, err = startHTTP(Spec{Name: "opaque", URL: "https://example.invalid/mcp", Headers: map[string]string{"X-Key": secret}})
	if err != nil {
		t.Fatal(err)
	}
	transport.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       errorBody{err: errors.New("read failed with " + secret)},
		}, nil
	})
	if err := transport.Send(context.Background(), message); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("body read error was not opaque: %v", err)
	}
	_ = transport.Close()
}

func TestHTTPStatusErrorsRedactEndpointCredentials(t *testing.T) {
	const password = "url-password-secret"
	const querySecret = "url-query-secret"
	transport, err := startHTTP(Spec{
		Name: "url-secret",
		URL:  "https://user:" + password + "@example.invalid/mcp?token=" + querySecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	transport.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := "refused password=" + password + " query=" + querySecret
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 " + password,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	err = transport.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err == nil {
		t.Fatal("Send unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), querySecret) {
		t.Fatalf("status error leaked endpoint credentials: %v", err)
	}
	_ = transport.Close()
}

func TestHTTPStartupTimeoutBoundsDiscovery(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	started := time.Now()
	_, err := Connect(context.Background(), Spec{
		Name:           "startup-timeout",
		URL:            srv.URL,
		StartupTimeout: 50 * time.Millisecond,
	}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Connect error = %v, want deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("startup timeout took %v", elapsed)
	}
}
