package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
)

// httpTransport speaks Streamable HTTP: every client message is a POST, and
// the answer arrives either as a single JSON body or as an SSE stream whose
// events are JSON-RPC messages. Both shapes funnel into one channel so the
// client's read loop sees the same thing stdio gives it.
type httpTransport struct {
	url    string
	client *http.Client

	mu                sync.Mutex
	session           string // Mcp-Session-Id, once the server assigns one
	protocol          string // negotiated revision, echoed as a header after initialize
	closing           bool
	nextID            uint64
	active            map[uint64]context.CancelFunc
	toolHeaders       map[string][]toolHeaderBinding
	staticHeaders     map[string]string
	headerEnv         map[string]string
	bearerTokenEnvVar string
	responseSensitive map[int64]responseSensitiveValues

	msgs      chan httpMessage
	done      chan struct{}
	closeOnce sync.Once
	activeWG  sync.WaitGroup
}

type httpMessage struct {
	data      []byte
	err       error
	requestID json.RawMessage
}

type responseSensitiveValues struct {
	secrets     []string
	credentials []string
}

// requestTransportError is a failure of one HTTP response stream, not of the
// endpoint. Client.readLoop uses the ID to unblock only that request; another
// concurrent HTTP call must remain usable.
type requestTransportError struct {
	ID  json.RawMessage
	Err error
}

func (e *requestTransportError) Error() string { return e.Err.Error() }
func (e *requestTransportError) Unwrap() error { return e.Err }

// httpStatusError preserves both the HTTP envelope and any JSON-RPC error
// addressed to the request. Negotiation needs the status to decide whether a
// response is era evidence, while callers still need errors.As to reach the
// typed RPCError and its data.
type httpStatusError struct {
	StatusCode int
	Status     string
	Body       string
	RPC        *RPCError
}

func (e *httpStatusError) Error() string {
	if e.RPC != nil {
		return fmt.Sprintf("mcp server answered %s: %s", e.Status, e.RPC)
	}
	if e.Body == "" {
		return fmt.Sprintf("mcp server answered %s", e.Status)
	}
	return fmt.Sprintf("mcp server answered %s: %s", e.Status, e.Body)
}

func (e *httpStatusError) Unwrap() error {
	if e.RPC == nil {
		return nil
	}
	return e.RPC
}

func startHTTP(spec Spec) (*httpTransport, error) {
	parsed, err := url.Parse(spec.URL)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("mcp server %s: url must be http or https", spec.Name)
	}
	if err := validateHTTPHeaderConfig(spec); err != nil {
		return nil, fmt.Errorf("mcp server %s: %w", spec.Name, err)
	}
	return &httpTransport{
		url: spec.URL,
		// Connect and Call supply the configured startup/tool contexts. The
		// HTTP client adds no hidden cap, so a larger native timeout remains
		// authoritative and an earlier caller deadline still wins.
		client: &http.Client{
			// Configured credentials belong only to the declared endpoint. The
			// default redirect policy can forward custom authentication headers.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		active:            make(map[uint64]context.CancelFunc),
		toolHeaders:       make(map[string][]toolHeaderBinding),
		staticHeaders:     cloneStringMap(spec.Headers),
		headerEnv:         cloneStringMap(spec.HeaderEnv),
		bearerTokenEnvVar: spec.BearerTokenEnvVar,
		responseSensitive: make(map[int64]responseSensitiveValues),
		msgs:              make(chan httpMessage, 16),
		done:              make(chan struct{}),
	}, nil
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for name, value := range source {
		cloned[name] = value
	}
	return cloned
}

func validateHTTPHeaderConfig(spec Spec) error {
	seen := make(map[string]string, len(spec.Headers)+len(spec.HeaderEnv))
	for _, name := range sortedMapKeys(spec.Headers) {
		value := spec.Headers[name]
		if err := validateConfiguredHeaderName(name, seen); err != nil {
			return err
		}
		if !validHTTPHeaderValue(value) {
			return fmt.Errorf("HTTP header %q has an invalid value", name)
		}
	}
	for _, name := range sortedMapKeys(spec.HeaderEnv) {
		envName := spec.HeaderEnv[name]
		if err := validateConfiguredHeaderName(name, seen); err != nil {
			return err
		}
		if !validEnvironmentName(envName) {
			return fmt.Errorf("HTTP header %q has invalid environment variable name %q", name, envName)
		}
	}
	if spec.BearerTokenEnvVar != "" {
		if !validEnvironmentName(spec.BearerTokenEnvVar) {
			return fmt.Errorf("invalid bearer token environment variable name %q", spec.BearerTokenEnvVar)
		}
		if prior, exists := seen[strings.ToLower("Authorization")]; exists {
			return fmt.Errorf("bearer token conflicts with configured HTTP header %q", prior)
		}
	}
	return nil
}

func validateConfiguredHeaderName(name string, seen map[string]string) error {
	if !validHTTPToken(name) {
		return fmt.Errorf("invalid HTTP header name %q", name)
	}
	if reservedHTTPHeader(name) {
		return fmt.Errorf("HTTP header %q is reserved by the MCP transport", name)
	}
	folded := strings.ToLower(name)
	if prior, exists := seen[folded]; exists {
		return fmt.Errorf("HTTP header %q duplicates %q case-insensitively", name, prior)
	}
	seen[folded] = name
	return nil
}

func reservedHTTPHeader(name string) bool {
	folded := strings.ToLower(name)
	if strings.HasPrefix(folded, "mcp-param-") {
		return true
	}
	switch folded {
	case "accept", "connection", "content-length", "content-type", "host",
		"mcp-method", "mcp-name", "mcp-protocol-version", "mcp-session-id",
		"proxy-connection", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func validHTTPHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b == '\t' || b >= 0x20 && b != 0x7f {
			continue
		}
		return false
	}
	return true
}

func (t *httpTransport) applyConfiguredHeaders(req *http.Request) ([]string, []string, error) {
	secrets := endpointSecretValues(t.url)
	credentials := endpointCredentialValues(t.url)
	staticNames := sortedMapKeys(t.staticHeaders)
	for _, name := range staticNames {
		value := t.staticHeaders[name]
		req.Header.Set(name, value)
		if value != "" {
			secrets = append(secrets, value)
			if sensitiveCredentialName(name) {
				credentials = append(credentials, value)
			}
		}
	}
	for _, name := range sortedMapKeys(t.headerEnv) {
		envName := t.headerEnv[name]
		value, ok := os.LookupEnv(envName)
		if !ok {
			return nil, nil, fmt.Errorf("HTTP header %q requires environment variable %q", name, envName)
		}
		if !validHTTPHeaderValue(value) {
			return nil, nil, fmt.Errorf("environment variable %q has an invalid HTTP header value", envName)
		}
		req.Header.Set(name, value)
		if value != "" {
			secrets = append(secrets, value)
			if sensitiveCredentialName(name) || sensitiveEnvName(envName) {
				credentials = append(credentials, value)
			}
		}
	}
	if t.bearerTokenEnvVar != "" {
		value, ok := os.LookupEnv(t.bearerTokenEnvVar)
		if !ok || value == "" {
			return nil, nil, fmt.Errorf("bearer token environment variable %q is not set", t.bearerTokenEnvVar)
		}
		if !validHTTPHeaderValue(value) {
			return nil, nil, fmt.Errorf("bearer token environment variable %q has an invalid value", t.bearerTokenEnvVar)
		}
		req.Header.Set("Authorization", "Bearer "+value)
		secrets = append(secrets, value)
		credentials = append(credentials, value)
	}
	return secrets, credentials, nil
}

func endpointSecretValues(endpoint string) []string {
	secrets := []string{endpoint}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return secrets
	}
	if parsed.User != nil {
		if username := parsed.User.Username(); username != "" {
			secrets = append(secrets, username)
		}
		if password, exists := parsed.User.Password(); exists && password != "" {
			secrets = append(secrets, password)
		}
	}
	for _, values := range parsed.Query() {
		for _, value := range values {
			if value != "" {
				secrets = append(secrets, value)
			}
		}
	}
	return secrets
}

func endpointCredentialValues(endpoint string) []string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil
	}
	var credentials []string
	if parsed.User != nil {
		if username := parsed.User.Username(); username != "" {
			credentials = append(credentials, username)
		}
		if password, exists := parsed.User.Password(); exists && password != "" {
			credentials = append(credentials, password)
		}
	}
	for name, values := range parsed.Query() {
		if !sensitiveCredentialName(name) {
			continue
		}
		for _, value := range values {
			if value != "" {
				credentials = append(credentials, value)
			}
		}
	}
	return credentials
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func redactSecrets(text string, secrets []string) string {
	if text == "" || len(secrets) == 0 {
		return text
	}
	unique := make(map[string]struct{}, len(secrets))
	filtered := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if _, exists := unique[secret]; exists {
			continue
		}
		unique[secret] = struct{}{}
		filtered = append(filtered, secret)
	}
	sort.Slice(filtered, func(i, j int) bool { return len(filtered[i]) > len(filtered[j]) })
	for _, secret := range filtered {
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	return text
}

// setProtocol records the negotiated revision; the spec wants it echoed on
// every request that follows initialize.
func (t *httpTransport) setProtocol(v string) {
	t.mu.Lock()
	t.protocol = v
	t.mu.Unlock()
}

func (t *httpTransport) setToolHeaders(tool string, bindings []toolHeaderBinding) {
	t.mu.Lock()
	if t.toolHeaders == nil {
		t.toolHeaders = make(map[string][]toolHeaderBinding)
	}
	t.toolHeaders[tool] = append([]toolHeaderBinding(nil), bindings...)
	t.mu.Unlock()
}

type outboundHTTPMessage struct {
	ID        json.RawMessage
	Method    string
	Name      string
	Protocol  string
	Response  bool
	Arguments json.RawMessage
}

type toolHeaderBinding struct {
	name      string
	path      []string
	valueType string
}

func (t *httpTransport) Send(ctx context.Context, msg []byte) error {
	metadata, err := parseOutboundHTTPMessage(msg)
	if err != nil {
		return fmt.Errorf("building MCP HTTP request: %w", err)
	}

	reqCtx, finish, err := t.beginRequest(ctx)
	if err != nil {
		return err
	}
	owned := true
	defer func() {
		if owned {
			finish()
		}
	}()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, t.url, bytes.NewReader(msg))
	if err != nil {
		return errors.New("building MCP HTTP request failed")
	}
	requestSecrets, requestCredentials, err := t.applyConfiguredHeaders(req)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	t.mu.Lock()
	negotiated := t.protocol
	session := t.session
	bindings := append([]toolHeaderBinding(nil), t.toolHeaders[metadata.Name]...)
	t.mu.Unlock()
	if metadata.Response && negotiated == modernProtocolVersion {
		return errors.New("MCP 2026-07-28 HTTP does not permit client response POSTs")
	}
	modern := metadata.Protocol != ""
	if modern {
		protocol := metadata.Protocol
		if protocol == "" {
			protocol = negotiated
		}
		req.Header.Set("MCP-Protocol-Version", protocol)
		req.Header.Set("Mcp-Method", metadata.Method)
		if metadata.Name != "" {
			req.Header.Set("Mcp-Name", encodeMCPHeaderValue(metadata.Name))
		}
		if metadata.Method == "tools/call" {
			parameterHeaders, err := mirroredToolHeaders(bindings, metadata.Arguments)
			if err != nil {
				return fmt.Errorf("building MCP headers for tool %s: %w", metadata.Name, err)
			}
			for name, value := range parameterHeaders {
				req.Header.Set("Mcp-Param-"+name, value)
				if value != "" {
					requestSecrets = append(requestSecrets, value)
				}
			}
		}
	} else {
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		if negotiated != "" {
			req.Header.Set("MCP-Protocol-Version", negotiated)
		}
	}

	resp, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("sending MCP HTTP request failed")
	}

	if !modern {
		sid := resp.Header.Get("Mcp-Session-Id")
		t.mu.Lock()
		if sid != "" {
			t.session = sid
		}
		t.mu.Unlock()
	}

	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		resp.Body.Close()
		if !metadata.Response && len(metadata.ID) != 0 {
			return errors.New("MCP HTTP request was accepted without a response; asynchronous server push is unsupported")
		}
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		body, readErr := readBounded(resp.Body, 64<<10)
		resp.Body.Close()
		if readErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("reading MCP HTTP error response failed")
		}
		return &httpStatusError{
			StatusCode: resp.StatusCode,
			Status:     redactSecrets(resp.Status, requestSecrets),
			Body:       sanitizeHTTPErrorBody(body, requestSecrets),
			RPC:        sanitizeRPCError(parseHTTPRPCError(body, metadata.ID), requestSecrets),
		}
	}

	ct := resp.Header.Get("Content-Type")
	mediaType, _, mediaErr := mime.ParseMediaType(ct)
	if mediaErr != nil {
		resp.Body.Close()
		return fmt.Errorf("mcp server answered with unexpected content type %q", redactSecrets(ct, requestSecrets))
	}
	switch mediaType {
	case "application/json":
		defer resp.Body.Close()
		body, err := readBounded(resp.Body, maxLine)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("reading MCP HTTP response failed")
		}
		if len(metadata.ID) != 0 && !matchesJSONRPCID(body, metadata.ID) {
			return errors.New("MCP HTTP JSON body did not contain the matching JSON-RPC response")
		}
		t.rememberResponseSensitive(metadata.ID, requestSecrets, requestCredentials)
		t.push(sanitizeRPCEnvelope(body, requestSecrets))
		return nil
	case "text/event-stream":
		// The stream may carry several messages before the response to this
		// request; read it out in the background so Send returns and the
		// read loop dispatches whatever arrives.
		owned = false
		go t.readSSE(resp.Body, finish, reqCtx, metadata.ID, requestSecrets, requestCredentials)
		return nil
	default:
		resp.Body.Close()
		return fmt.Errorf("mcp server answered with unexpected content type %q", redactSecrets(ct, requestSecrets))
	}
}

func sanitizeRPCError(rpcErr *RPCError, secrets []string) *RPCError {
	if rpcErr == nil || len(secrets) == 0 {
		return rpcErr
	}
	cloned := *rpcErr
	cloned.secrets = append(append([]string(nil), rpcErr.secrets...), secrets...)
	cloned.Message = redactSecrets(cloned.Message, secrets)
	sourceData := rpcErr.rawData
	if len(sourceData) == 0 {
		sourceData = rpcErr.Data
	}
	cloned.rawData = append(json.RawMessage(nil), sourceData...)
	cloned.Data = append(json.RawMessage(nil), sourceData...)
	if len(sourceData) > 0 {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(sourceData))
		decoder.UseNumber()
		if decoder.Decode(&value) == nil {
			value = redactJSONValue(value, secrets)
			if encoded, err := json.Marshal(value); err == nil {
				cloned.Data = encoded
			}
		}
	}
	return &cloned
}

func sanitizeHTTPErrorBody(body []byte, secrets []string) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return ""
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if decoder.Decode(&value) == nil {
		if encoded, err := json.Marshal(redactJSONValue(value, secrets)); err == nil {
			return string(encoded)
		}
	}
	return redactSecrets(string(trimmed), secrets)
}

func redactJSONValue(value any, secrets []string) any {
	switch value := value.(type) {
	case string:
		return redactSecrets(value, secrets)
	case []any:
		for i := range value {
			value[i] = redactJSONValue(value[i], secrets)
		}
		return value
	case map[string]any:
		redacted := make(map[string]any, len(value))
		for key, child := range value {
			redacted[redactSecrets(key, secrets)] = redactJSONValue(child, secrets)
		}
		return redacted
	case json.Number:
		if redacted := redactSecrets(value.String(), secrets); redacted != value.String() {
			return redacted
		}
		return value
	case bool:
		text := fmt.Sprint(value)
		if redacted := redactSecrets(text, secrets); redacted != text {
			return redacted
		}
		return value
	case nil:
		if redacted := redactSecrets("null", secrets); redacted != "null" {
			return redacted
		}
		return nil
	default:
		return value
	}
}

func (t *httpTransport) beginRequest(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	reqCtx, cancel := context.WithCancel(ctx)
	t.mu.Lock()
	if t.closing {
		t.mu.Unlock()
		cancel()
		return nil, nil, errors.New("transport closed")
	}
	t.nextID++
	id := t.nextID
	t.active[id] = cancel
	t.activeWG.Add(1)
	t.mu.Unlock()

	var once sync.Once
	finish := func() {
		once.Do(func() {
			cancel()
			t.mu.Lock()
			delete(t.active, id)
			t.mu.Unlock()
			t.activeWG.Done()
		})
	}
	return reqCtx, finish, nil
}

func (t *httpTransport) readSSE(body io.ReadCloser, finish func(), reqCtx context.Context, requestID json.RawMessage, secrets, credentials []string) {
	t.readSSEWithSensitive(body, finish, reqCtx, requestID, maxLine, secrets, credentials)
}

func (t *httpTransport) readSSEWithLimit(body io.ReadCloser, finish func(), reqCtx context.Context, requestID json.RawMessage, limit int) {
	t.readSSEWithSensitive(body, finish, reqCtx, requestID, limit, nil, nil)
}

func (t *httpTransport) readSSEWithSensitive(body io.ReadCloser, finish func(), reqCtx context.Context, requestID json.RawMessage, limit int, secrets, credentials []string) {
	defer finish()
	defer body.Close()
	sc := bufio.NewScanner(body)
	initial := 64 << 10
	if limit < initial {
		initial = limit
	}
	if initial < 1 {
		initial = 1
	}
	sc.Buffer(make([]byte, initial), limit)

	var data []string
	dataBytes := 0
	dataLines := 0
	lineLimit := limit
	if lineLimit > 64<<10 {
		lineLimit = 64 << 10
	}
	matched := false
	flush := func() bool {
		if len(data) == 0 {
			return false
		}
		message := sanitizeRPCEnvelope([]byte(strings.Join(data, "\n")), secrets)
		if matchesJSONRPCID(message, requestID) {
			matched = true
			t.rememberResponseSensitive(requestID, secrets, credentials)
		}
		t.push(message)
		data = nil
		dataBytes = 0
		dataLines = 0
		return matched
	}
	var overflowErr error
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if flush() {
				return
			}
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if dataLines >= lineLimit {
				overflowErr = fmt.Errorf("MCP SSE event exceeds %d data lines", lineLimit)
				break
			}
			separator := 0
			if len(data) > 0 {
				separator = 1
			}
			if dataBytes > limit-len(value)-separator {
				overflowErr = fmt.Errorf("MCP SSE event exceeds %d bytes", limit)
				break
			}
			data = append(data, value)
			dataBytes += len(value) + separator
			dataLines++
		}
		// id:, event:, retry:, and comments carry nothing this client uses.
		if overflowErr != nil {
			break
		}
	}
	if overflowErr != nil {
		if len(requestID) != 0 && reqCtx.Err() == nil {
			t.pushError(requestID, overflowErr)
		}
		return
	}
	if flush() {
		return
	}
	if len(requestID) == 0 || matched || reqCtx.Err() != nil {
		return
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			t.pushError(requestID, errors.New("reading MCP SSE response: token too long"))
		} else {
			t.pushError(requestID, errors.New("reading MCP SSE response failed"))
		}
		return
	}
	t.pushError(requestID, errors.New("MCP SSE stream closed before its JSON-RPC response"))
}

func sanitizeRPCEnvelope(message []byte, secrets []string) []byte {
	if len(secrets) == 0 {
		return message
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(message, &envelope) != nil {
		return message
	}
	// Server requests and notifications only reach a fixed refusal path or
	// logs, so scrub their params here. Successful result data remains intact
	// until Client.Call, which redacts the narrower credential-bearing subset
	// without treating every ordinary configured header as secret tool data.
	if len(envelope["method"]) > 0 {
		if rawParams := envelope["params"]; len(rawParams) > 0 {
			if encoded, ok := sanitizeJSON(rawParams, secrets); ok {
				envelope["params"] = encoded
			}
		}
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return message
	}
	return encoded
}

func sanitizeJSON(raw []byte, secrets []string) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	encoded, err := json.Marshal(redactJSONValue(value, secrets))
	return encoded, err == nil
}

func (t *httpTransport) push(msg []byte) {
	select {
	case t.msgs <- httpMessage{data: msg}:
	case <-t.done:
	}
}

func (t *httpTransport) pushError(requestID json.RawMessage, err error) {
	select {
	case t.msgs <- httpMessage{err: err, requestID: append(json.RawMessage(nil), requestID...)}:
	case <-t.done:
	}
}

func (t *httpTransport) rememberResponseSensitive(requestID json.RawMessage, secrets, credentials []string) {
	var id int64
	if len(requestID) == 0 || json.Unmarshal(requestID, &id) != nil {
		return
	}
	t.mu.Lock()
	if t.responseSensitive == nil {
		t.responseSensitive = make(map[int64]responseSensitiveValues)
	}
	t.responseSensitive[id] = responseSensitiveValues{
		secrets:     append([]string(nil), secrets...),
		credentials: append([]string(nil), credentials...),
	}
	t.mu.Unlock()
}

func (t *httpTransport) takeResponseSensitive(id int64) ([]string, []string) {
	t.mu.Lock()
	values := t.responseSensitive[id]
	delete(t.responseSensitive, id)
	t.mu.Unlock()
	return values.secrets, values.credentials
}

func (t *httpTransport) Recv() ([]byte, error) {
	select {
	case msg := <-t.msgs:
		if msg.err != nil {
			return nil, &requestTransportError{ID: msg.requestID, Err: msg.err}
		}
		return msg.data, nil
	case <-t.done:
		return nil, errors.New("transport closed")
	}
}

func matchesJSONRPCID(message, requestID json.RawMessage) bool {
	if len(requestID) == 0 {
		return false
	}
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(message, &envelope) != nil || len(envelope.ID) == 0 || envelope.Method != "" {
		return false
	}
	if len(envelope.Result) == 0 && len(envelope.Error) == 0 {
		return false
	}
	return bytes.Equal(bytes.TrimSpace(envelope.ID), bytes.TrimSpace(requestID))
}

func (t *httpTransport) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closing = true
		close(t.done)
		cancels := make([]context.CancelFunc, 0, len(t.active))
		for _, cancel := range t.active {
			cancels = append(cancels, cancel)
		}
		t.mu.Unlock()

		for _, cancel := range cancels {
			cancel()
		}
		t.client.CloseIdleConnections()
		t.activeWG.Wait()
	})
	return nil
}

func parseOutboundHTTPMessage(msg []byte) (outboundHTTPMessage, error) {
	var envelope struct {
		JSONRPC string                     `json:"jsonrpc"`
		ID      json.RawMessage            `json:"id"`
		Method  string                     `json:"method"`
		Params  map[string]json.RawMessage `json:"params"`
		Result  json.RawMessage            `json:"result"`
		Error   json.RawMessage            `json:"error"`
	}
	if err := json.Unmarshal(msg, &envelope); err != nil {
		return outboundHTTPMessage{}, err
	}
	if envelope.JSONRPC != "2.0" {
		return outboundHTTPMessage{}, errors.New("message is not JSON-RPC 2.0")
	}
	out := outboundHTTPMessage{ID: envelope.ID, Method: envelope.Method}
	if envelope.Method == "" {
		if len(envelope.ID) == 0 || len(envelope.Result) == 0 && len(envelope.Error) == 0 {
			return outboundHTTPMessage{}, errors.New("message is not a JSON-RPC request, notification, or response")
		}
		out.Response = true
		return out, nil
	}
	if raw := envelope.Params["_meta"]; len(raw) > 0 {
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(raw, &meta); err != nil {
			return outboundHTTPMessage{}, fmt.Errorf("invalid params._meta: %w", err)
		}
		if rawVersion := meta["io.modelcontextprotocol/protocolVersion"]; len(rawVersion) > 0 {
			if err := json.Unmarshal(rawVersion, &out.Protocol); err != nil {
				return outboundHTTPMessage{}, errors.New("params._meta protocol version is not a string")
			}
		}
	}

	var source string
	switch envelope.Method {
	case "tools/call", "prompts/get":
		source = "name"
	case "resources/read":
		source = "uri"
	}
	if source != "" {
		raw := envelope.Params[source]
		if len(raw) == 0 || json.Unmarshal(raw, &out.Name) != nil || out.Name == "" {
			return outboundHTTPMessage{}, fmt.Errorf("%s params.%s must be a non-empty string", envelope.Method, source)
		}
	}
	if envelope.Method == "tools/call" {
		out.Arguments = append(json.RawMessage(nil), envelope.Params["arguments"]...)
	}
	return out, nil
}

func encodeMCPHeaderValue(value string) string {
	safe := value != "" && strings.Trim(value, " \t") == value
	if strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") {
		safe = false
	}
	for i := 0; safe && i < len(value); i++ {
		b := value[i]
		if b < 0x20 || b > 0x7e {
			safe = false
		}
	}
	if safe {
		return value
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
}

func parseToolHeaderBindings(schema json.RawMessage) ([]toolHeaderBinding, error) {
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil || root == nil {
		if err == nil {
			err = errors.New("inputSchema must be a JSON object")
		}
		return nil, err
	}
	var bindings []toolHeaderBinding
	seen := make(map[string]string)
	if err := collectToolHeaderBindings(root, nil, false, seen, &bindings); err != nil {
		return nil, err
	}
	return bindings, nil
}

func collectToolHeaderBindings(node map[string]any, path []string, property bool, seen map[string]string, bindings *[]toolHeaderBinding) error {
	if annotation, exists := node["x-mcp-header"]; exists {
		if !property {
			return errors.New("x-mcp-header is not on a property reachable only through properties")
		}
		name, ok := annotation.(string)
		if !ok || name == "" || !validHTTPToken(name) {
			return fmt.Errorf("x-mcp-header at %s is not a non-empty HTTP token", strings.Join(path, "."))
		}
		valueType, ok := node["type"].(string)
		if !ok || valueType != "string" && valueType != "integer" && valueType != "boolean" {
			return fmt.Errorf("x-mcp-header %q at %s requires type string, integer, or boolean", name, strings.Join(path, "."))
		}
		folded := strings.ToLower(name)
		if prior, duplicate := seen[folded]; duplicate {
			return fmt.Errorf("x-mcp-header %q at %s duplicates %s case-insensitively", name, strings.Join(path, "."), prior)
		}
		seen[folded] = strings.Join(path, ".")
		*bindings = append(*bindings, toolHeaderBinding{
			name:      name,
			path:      append([]string(nil), path...),
			valueType: valueType,
		})
	}

	if properties, ok := node["properties"].(map[string]any); ok {
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child, ok := properties[name].(map[string]any)
			if !ok {
				continue
			}
			childPath := append(append([]string(nil), path...), name)
			if err := collectToolHeaderBindings(child, childPath, true, seen, bindings); err != nil {
				return err
			}
		}
	}

	// These keywords introduce schemas without following a properties-only
	// path. Any annotation below one invalidates the tool definition.
	for _, keyword := range []string{
		"$defs", "additionalProperties", "allOf", "anyOf", "contains",
		"definitions", "dependentSchemas", "else", "if", "items", "not",
		"oneOf", "patternProperties", "prefixItems", "propertyNames", "then",
		"unevaluatedItems", "unevaluatedProperties",
	} {
		if containsToolHeaderAnnotation(node[keyword]) {
			return fmt.Errorf("x-mcp-header appears below %s instead of a properties-only path", keyword)
		}
	}
	return nil
}

func containsToolHeaderAnnotation(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		if _, ok := value["x-mcp-header"]; ok {
			return true
		}
		for _, child := range value {
			if containsToolHeaderAnnotation(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsToolHeaderAnnotation(child) {
				return true
			}
		}
	}
	return false
}

func validHTTPToken(value string) bool {
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' {
			continue
		}
		switch b {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		}
		return false
	}
	return value != ""
}

func mirroredToolHeaders(bindings []toolHeaderBinding, arguments json.RawMessage) (map[string]string, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &root); err != nil || root == nil {
		if err == nil {
			err = errors.New("arguments must be a JSON object")
		}
		return nil, err
	}
	headers := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		raw, present, err := rawAtPropertyPath(root, binding.path)
		if err != nil {
			return nil, fmt.Errorf("parameter %s: %w", strings.Join(binding.path, "."), err)
		}
		if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		value, err := formatMirroredValue(raw, binding.valueType)
		if err != nil {
			return nil, fmt.Errorf("parameter %s: %w", strings.Join(binding.path, "."), err)
		}
		headers[binding.name] = value
	}
	return headers, nil
}

func rawAtPropertyPath(root map[string]json.RawMessage, path []string) (json.RawMessage, bool, error) {
	current := root
	for i, segment := range path {
		raw, ok := current[segment]
		if !ok {
			return nil, false, nil
		}
		if i == len(path)-1 {
			return raw, true, nil
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, false, nil
		}
		var child map[string]json.RawMessage
		if err := json.Unmarshal(raw, &child); err != nil || child == nil {
			return nil, false, nil
		}
		current = child
	}
	return nil, false, nil
}

func formatMirroredValue(raw json.RawMessage, valueType string) (string, error) {
	switch valueType {
	case "string":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", errors.New("value is not a string")
		}
		return encodeMCPHeaderValue(value), nil
	case "boolean":
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", errors.New("value is not a boolean")
		}
		if value {
			return "true", nil
		}
		return "false", nil
	case "integer":
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return "", errors.New("value is not an integer")
		}
		number, ok := value.(json.Number)
		if !ok {
			return "", errors.New("value is not an integer")
		}
		rational, ok := new(big.Rat).SetString(number.String())
		if !ok || !rational.IsInt() {
			return "", errors.New("value is not an integer")
		}
		const maxSafeInteger = int64(1<<53 - 1)
		absolute := new(big.Int).Abs(new(big.Int).Set(rational.Num()))
		if absolute.Cmp(big.NewInt(maxSafeInteger)) > 0 {
			return "", errors.New("integer exceeds the JavaScript safe range")
		}
		return rational.Num().String(), nil
	default:
		return "", fmt.Errorf("unsupported mirrored value type %q", valueType)
	}
}

func parseHTTPRPCError(body, requestID json.RawMessage) *RPCError {
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *RPCError       `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.JSONRPC != "2.0" || envelope.Error == nil {
		return nil
	}
	if len(requestID) == 0 || !bytes.Equal(bytes.TrimSpace(envelope.ID), bytes.TrimSpace(requestID)) {
		return nil
	}
	return envelope.Error
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("mcp HTTP body exceeds %d bytes", limit)
	}
	return body, nil
}
