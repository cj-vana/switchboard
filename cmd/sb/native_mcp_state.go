package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/switchboard-code/switchboard/internal/mcpnative"
)

const (
	nativeMCPStateFileName = "native-mcp.json"
	maxNativeMCPStateBytes = 1 << 20
)

// nativeMCPActivationState is Switchboard's independent decision about exact
// native Codex and Claude MCP definitions. The random key makes the persisted
// definition digest safe to render or back up without turning it into an
// oracle for short secrets. Native enabled/trusted state is never imported.
type nativeMCPActivationState struct {
	path string

	mu       sync.Mutex
	key      []byte
	records  map[string]nativeMCPActivationRecord
	poisoned error
}

type nativeMCPActivationStatus struct {
	Enabled bool
	Changed bool
}

// nativeMCPActivationRecord extends the original on-disk identity with the
// required-startup bit that was in force when the definition was approved.
// Required is a pointer so records written by older Switchboard releases stay
// distinguishable from an explicitly optional server. Unknown legacy records
// remain fail-closed if their authoritative Codex snapshot is unavailable.
type nativeMCPActivationRecord struct {
	ID        string `json:"id"`
	RealPath  string `json:"real_path"`
	TrustRoot string `json:"trust_root,omitempty"`
	Digest    string `json:"digest"`
	Required  *bool  `json:"required,omitempty"`
}

// nativeMCPActivationReference is the non-secret recovery view of a saved
// decision. It intentionally omits both the activation HMAC and its key.
type nativeMCPActivationReference struct {
	ID            string
	RealPath      string
	TrustRoot     string
	Required      bool
	RequiredKnown bool
	RecoveryToken string
}

func (record nativeMCPActivationRecord) identity() mcpnative.ActivationIdentity {
	return mcpnative.ActivationIdentity{
		ID: record.ID, RealPath: record.RealPath, TrustRoot: record.TrustRoot, Digest: record.Digest,
	}
}

func nativeMCPActivationRecordFromIdentity(identity mcpnative.ActivationIdentity, required *bool) nativeMCPActivationRecord {
	record := nativeMCPActivationRecord{
		ID: identity.ID, RealPath: identity.RealPath, TrustRoot: identity.TrustRoot, Digest: identity.Digest,
	}
	if required != nil {
		value := *required
		record.Required = &value
	}
	return record
}

func openNativeMCPActivationState() (*nativeMCPActivationState, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("no home directory for native MCP state: %w", err)
	}
	return openNativeMCPActivationStateFile(filepath.Join(home, ".switchboard", nativeMCPStateFileName))
}

func openNativeMCPActivationStateFile(path string) (*nativeMCPActivationState, error) {
	state := &nativeMCPActivationState{path: path, records: make(map[string]nativeMCPActivationRecord)}
	pathInfo, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if os.IsNotExist(err) {
		return state, nil
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("reading %s: native MCP state must not be a symbolic link", path)
	}
	f, err := openNativeMCPStateDataFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if !os.SameFile(pathInfo, openedInfo) {
		return nil, fmt.Errorf("reading %s: native MCP state changed while it was opened", path)
	}
	if !openedInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("reading %s: native MCP state is not a regular file", path)
	}
	if runtime.GOOS != "windows" && openedInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("reading %s: native MCP state permissions are %04o, want 0600", path, openedInfo.Mode().Perm())
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxNativeMCPStateBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(raw) > maxNativeMCPStateBytes {
		return nil, fmt.Errorf("reading %s: native MCP state exceeds %d bytes", path, maxNativeMCPStateBytes)
	}
	if err := rejectNativeMCPDuplicateJSONKeys(raw); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var file struct {
		Version     int                         `json:"version"`
		Key         string                      `json:"key"`
		Activations []nativeMCPActivationRecord `json:"activations"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := requireNativeMCPJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if file.Version != 1 {
		return nil, fmt.Errorf("reading %s: unsupported native MCP state version %d", path, file.Version)
	}
	key, err := base64.RawStdEncoding.DecodeString(file.Key)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("reading %s: native MCP activation key is invalid", path)
	}
	state.key = append([]byte(nil), key...)
	for _, activation := range file.Activations {
		identity := activation.identity()
		if err := validateNativeMCPActivation(identity); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		recordKey := nativeMCPActivationKey(identity)
		if _, duplicate := state.records[recordKey]; duplicate {
			return nil, fmt.Errorf("reading %s: duplicate native MCP activation for %s", path, activation.ID)
		}
		state.records[recordKey] = activation
	}
	return state, nil
}

func (s *nativeMCPActivationState) NativeMCPActivated(request mcpnative.ActivationRequest) bool {
	return s.status(request).Enabled
}

func (s *nativeMCPActivationState) hasDialect(dialect mcpnative.Dialect, workspaces ...string) bool {
	if s == nil {
		return false
	}
	workspaceRoot, _ := canonicalNativeMCPWorkspace(firstNativeMCPWorkspace(workspaces))
	prefix := string(dialect) + ":"
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return false
	}
	for _, record := range s.records {
		if strings.HasPrefix(record.ID, prefix) && nativeMCPActivationApplies(record, workspaceRoot) {
			return true
		}
	}
	return false
}

// references returns a stable, non-secret view suitable for recovery UI and
// exact disable operations after the native definition has disappeared.
func (s *nativeMCPActivationState) references(workspaces ...string) []nativeMCPActivationReference {
	if s == nil {
		return nil
	}
	workspaceRoot, _ := canonicalNativeMCPWorkspace(firstNativeMCPWorkspace(workspaces))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return nil
	}
	references := make([]nativeMCPActivationReference, 0, len(s.records))
	for _, record := range s.records {
		if !nativeMCPActivationApplies(record, workspaceRoot) {
			continue
		}
		reference := nativeMCPActivationReference{
			ID: record.ID, RealPath: record.RealPath, TrustRoot: record.TrustRoot,
		}
		if record.Required != nil {
			reference.Required = *record.Required
			reference.RequiredKnown = true
		}
		reference.RecoveryToken = nativeMCPActivationRecoveryToken(s.key, reference)
		references = append(references, reference)
	}
	sort.Slice(references, func(i, j int) bool {
		if references[i].ID != references[j].ID {
			return references[i].ID < references[j].ID
		}
		if references[i].RealPath != references[j].RealPath {
			return references[i].RealPath < references[j].RealPath
		}
		return references[i].TrustRoot < references[j].TrustRoot
	})
	return references
}

// nativeMCPActivationRecoveryToken is a domain-separated, opaque selector
// over the complete saved source identity. It lets the CLI distinguish two
// project activations that naturally share an ID and config path without
// printing the workspace trust root, definition digest, or state key.
func nativeMCPActivationRecoveryToken(key []byte, reference nativeMCPActivationReference) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("switchboard/native-mcp-recovery-selector/v1\x00"))
	for _, value := range []string{reference.ID, reference.RealPath, reference.TrustRoot} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write([]byte(value))
	}
	return "saved:" + hex.EncodeToString(mac.Sum(nil))
}

// snapshotFailureRequired reports whether losing an authoritative snapshot
// must abort startup. Explicitly optional records may stay off with a warning;
// required and legacy-unknown records preserve fail-closed startup semantics.
func (s *nativeMCPActivationState) snapshotFailureRequired(dialect mcpnative.Dialect, workspaces ...string) bool {
	prefix := string(dialect) + ":"
	if s == nil {
		return false
	}
	workspaceRoot, _ := canonicalNativeMCPWorkspace(firstNativeMCPWorkspace(workspaces))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return false
	}
	for _, record := range s.records {
		if strings.HasPrefix(record.ID, prefix) && nativeMCPActivationApplies(record, workspaceRoot) &&
			(record.Required == nil || *record.Required) {
			return true
		}
	}
	return false
}

func (s *nativeMCPActivationState) status(request mcpnative.ActivationRequest) nativeMCPActivationStatus {
	if s == nil {
		return nativeMCPActivationStatus{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked(request)
}

func (s *nativeMCPActivationState) statusLocked(request mcpnative.ActivationRequest) nativeMCPActivationStatus {
	if s.poisoned != nil || len(s.key) != 32 {
		return nativeMCPActivationStatus{}
	}
	want, err := request.Identity(s.key)
	if err != nil {
		return nativeMCPActivationStatus{}
	}
	got, exists := s.records[nativeMCPActivationKey(want)]
	if !exists {
		return nativeMCPActivationStatus{}
	}
	equal := subtle.ConstantTimeCompare([]byte(got.Digest), []byte(want.Digest)) == 1
	return nativeMCPActivationStatus{Enabled: equal, Changed: !equal}
}

// canonicalNativeMCPWorkspace resolves the same physical checkout identity
// used by project-scoped native discovery. An unresolved workspace cannot
// make any project activation applicable; global records remain independent.
func canonicalNativeMCPWorkspace(workspace string) (string, bool) {
	if strings.TrimSpace(workspace) == "" {
		return "", true
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", false
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return filepath.Clean(real), true
}

func firstNativeMCPWorkspace(workspaces []string) string {
	if len(workspaces) == 0 {
		return ""
	}
	return workspaces[0]
}

func nativeMCPActivationApplies(record nativeMCPActivationRecord, workspaceRoot string) bool {
	if record.TrustRoot == "" {
		return true
	}
	if workspaceRoot == "" {
		return false
	}
	recordRoot, ok := canonicalNativeMCPWorkspace(record.TrustRoot)
	return ok && recordRoot == workspaceRoot
}

func (s *nativeMCPActivationState) enable(request mcpnative.ActivationRequest) error {
	return s.enableRecordContext(context.Background(), request, nil)
}

func (s *nativeMCPActivationState) enableWithRequired(request mcpnative.ActivationRequest, required bool) error {
	return s.enableRecordContext(context.Background(), request, &required)
}

func (s *nativeMCPActivationState) enableWithRequiredContext(ctx context.Context, request mcpnative.ActivationRequest, required bool) error {
	return s.enableRecordContext(ctx, request, &required)
}

func (s *nativeMCPActivationState) enableRecordContext(ctx context.Context, request mcpnative.ActivationRequest, required *bool) error {
	if s == nil {
		return errors.New("native MCP activation state is unavailable")
	}
	return s.mutate(ctx, func(latest *nativeMCPActivationState) (bool, error) {
		if len(latest.key) == 0 {
			latest.key = make([]byte, 32)
			if _, err := rand.Read(latest.key); err != nil {
				return false, fmt.Errorf("creating native MCP activation key: %w", err)
			}
		}
		identity, err := request.Identity(latest.key)
		if err != nil {
			return false, err
		}
		if err := validateNativeMCPActivation(identity); err != nil {
			return false, err
		}
		recordKey := nativeMCPActivationKey(identity)
		before, existed := latest.records[recordKey]
		effectiveRequired := required
		if effectiveRequired == nil && existed {
			effectiveRequired = before.Required
		}
		record := nativeMCPActivationRecordFromIdentity(identity, effectiveRequired)
		if existed && nativeMCPActivationRecordsEqual(before, record) {
			return false, nil
		}
		latest.records[recordKey] = record
		return true, nil
	})
}

func (s *nativeMCPActivationState) disable(request mcpnative.ActivationRequest) error {
	return s.disableReferenceContext(context.Background(), nativeMCPActivationReference{
		ID: request.ID, RealPath: request.RealPath, TrustRoot: request.TrustRoot,
	})
}

func (s *nativeMCPActivationState) disableReference(reference nativeMCPActivationReference) error {
	return s.disableReferenceContext(context.Background(), reference)
}

func (s *nativeMCPActivationState) disableReferenceContext(ctx context.Context, reference nativeMCPActivationReference) error {
	if s == nil {
		return errors.New("native MCP activation state is unavailable")
	}
	identity := mcpnative.ActivationIdentity{
		ID: reference.ID, RealPath: reference.RealPath, TrustRoot: reference.TrustRoot,
	}
	if identity.ID == "" || !filepath.IsAbs(identity.RealPath) ||
		(identity.TrustRoot != "" && !filepath.IsAbs(identity.TrustRoot)) {
		return errors.New("native MCP activation reference is invalid")
	}
	recordKey := nativeMCPActivationKey(identity)
	return s.mutate(ctx, func(latest *nativeMCPActivationState) (bool, error) {
		if _, exists := latest.records[recordKey]; !exists {
			return false, nil
		}
		delete(latest.records, recordKey)
		return true, nil
	})
}

func nativeMCPActivationRecordsEqual(left, right nativeMCPActivationRecord) bool {
	if left.ID != right.ID || left.RealPath != right.RealPath || left.TrustRoot != right.TrustRoot || left.Digest != right.Digest {
		return false
	}
	if left.Required == nil || right.Required == nil {
		return left.Required == nil && right.Required == nil
	}
	return *left.Required == *right.Required
}

// mutate reloads the latest complete ledger while holding the sidecar lock,
// applies one requested record change, and publishes atomically. Any failure
// before the latest state can be established, during publication, or while
// releasing authority poisons this handle so cached decisions fail closed.
func (s *nativeMCPActivationState) mutate(ctx context.Context, apply func(*nativeMCPActivationState) (bool, error)) (resultErr error) {
	if s == nil {
		return errors.New("native MCP activation state is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return fmt.Errorf("native MCP activation state is unavailable after an earlier I/O failure: %w", s.poisoned)
	}

	lock, err := acquireNativeMCPStateFileLock(ctx, s.path)
	if err != nil {
		return s.poisonLocked(err)
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			poisonErr := s.poisonLocked(fmt.Errorf("releasing native MCP activation state lock: %w", closeErr))
			resultErr = errors.Join(resultErr, poisonErr)
		}
	}()
	if err := ctx.Err(); err != nil {
		return s.poisonLocked(err)
	}

	latest, err := openNativeMCPActivationStateFile(s.path)
	if err != nil {
		return s.poisonLocked(fmt.Errorf("reloading native MCP activation state: %w", err))
	}
	// Adopt the validated baseline before user/request validation. A canceled
	// or invalid mutation may not commit, but this handle must still stop
	// serving authority another process already removed.
	s.adoptLocked(latest)
	if err := ctx.Err(); err != nil {
		return err
	}
	changed, err := apply(latest)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if changed {
		if err := latest.saveLockedContext(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return s.poisonLocked(fmt.Errorf("saving native MCP activation state: %w", err))
		}
	}
	s.adoptLocked(latest)
	return nil
}

func (s *nativeMCPActivationState) adoptLocked(latest *nativeMCPActivationState) {
	s.key = append(s.key[:0], latest.key...)
	s.records = make(map[string]nativeMCPActivationRecord, len(latest.records))
	for key, record := range latest.records {
		if record.Required != nil {
			value := *record.Required
			record.Required = &value
		}
		s.records[key] = record
	}
	s.poisoned = nil
}

func (s *nativeMCPActivationState) poisonLocked(err error) error {
	if err == nil {
		err = errors.New("native MCP activation state I/O is ambiguous")
	}
	s.key = nil
	s.records = make(map[string]nativeMCPActivationRecord)
	s.poisoned = err
	return err
}

func nativeMCPActivationKey(identity mcpnative.ActivationIdentity) string {
	return strings.Join([]string{identity.ID, filepath.Clean(identity.RealPath), filepath.Clean(identity.TrustRoot)}, "\x00")
}

func validateNativeMCPActivation(identity mcpnative.ActivationIdentity) error {
	if identity.ID == "" || strings.ContainsAny(identity.ID, "\x00\r\n") {
		return errors.New("native MCP activation has an invalid ID")
	}
	if !strings.HasPrefix(identity.ID, "codex:") && !strings.HasPrefix(identity.ID, "claude:") {
		return fmt.Errorf("native MCP activation %s has an unsupported dialect", identity.ID)
	}
	if !filepath.IsAbs(identity.RealPath) {
		return fmt.Errorf("native MCP activation %s has a non-absolute config path", identity.ID)
	}
	if strings.ContainsAny(identity.RealPath, "\x00\r\n") {
		return fmt.Errorf("native MCP activation %s has an invalid config path", identity.ID)
	}
	if identity.TrustRoot != "" && !filepath.IsAbs(identity.TrustRoot) {
		return fmt.Errorf("native MCP activation %s has a non-absolute trust root", identity.ID)
	}
	if strings.ContainsAny(identity.TrustRoot, "\x00\r\n") {
		return fmt.Errorf("native MCP activation %s has an invalid trust root", identity.ID)
	}
	digest, err := hex.DecodeString(identity.Digest)
	if err != nil || len(digest) != 32 || identity.Digest != strings.ToLower(identity.Digest) {
		return fmt.Errorf("native MCP activation %s has an invalid digest", identity.ID)
	}
	return nil
}

func (s *nativeMCPActivationState) saveLocked() error {
	return s.saveLockedContext(context.Background())
}

func (s *nativeMCPActivationState) saveLockedContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(s.key) != 32 {
		return errors.New("native MCP activation key is unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(s.path), err)
	}
	file := struct {
		Version     int                         `json:"version"`
		Key         string                      `json:"key"`
		Activations []nativeMCPActivationRecord `json:"activations"`
	}{Version: 1, Key: base64.RawStdEncoding.EncodeToString(s.key)}
	for _, activation := range s.records {
		file.Activations = append(file.Activations, activation)
	}
	sort.Slice(file.Activations, func(i, j int) bool {
		left, right := file.Activations[i], file.Activations[j]
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		if left.TrustRoot != right.TrustRoot {
			return left.TrustRoot < right.TrustRoot
		}
		return left.RealPath < right.RealPath
	})
	tmp, err := os.CreateTemp(filepath.Dir(s.path), nativeMCPStateFileName+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(file); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return replaceNativeMCPStateFile(tmp.Name(), s.path)
}

func requireNativeMCPJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("unexpected JSON value after native MCP state")
}

func rejectNativeMCPDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("invalid JSON object key")
				}
				if seen[key] {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				seen[key] = true
				if err := value(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := value(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("invalid JSON delimiter")
		}
	}
	if err := value(); err != nil {
		return err
	}
	return nil
}
