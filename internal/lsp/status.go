package lsp

import (
	"path/filepath"
	"strings"
)

// ServerState describes the lazy runtime without causing it to start.
type ServerState string

const (
	ServerConfigured ServerState = "configured"
	ServerStarting   ServerState = "starting"
	ServerRunning    ServerState = "running"
	ServerClosed     ServerState = "closed"
)

// ServerStatus is a non-starting snapshot of a Server. Capabilities are only
// populated while the runtime is running. LastError records the latest failed
// startup attempt; failures remain retryable.
type ServerStatus struct {
	State        ServerState
	Executable   string
	Capabilities Capabilities
	LastError    string
}

// Status reports the lazy runtime state. It never invokes the executable,
// initializes a ProblemStore, or waits for an in-progress startup.
func (s *Server) Status() ServerStatus {
	s.mu.Lock()
	status := ServerStatus{State: ServerConfigured, LastError: s.lastError}
	if len(s.Argv) > 0 {
		status.Executable = filepath.Base(s.Argv[0])
	}
	switch {
	case s.closed:
		status.State = ServerClosed
	case s.client != nil:
		status.State = ServerRunning
		status.Capabilities = s.client.Capabilities()
	case s.starting != nil:
		status.State = ServerStarting
	}
	s.mu.Unlock()
	return status
}

func boundedStatusError(err error) string {
	const maxRunes = 512
	message := strings.TrimSpace(err.Error())
	runes := []rune(message)
	if len(runes) <= maxRunes {
		return message
	}
	return string(runes[:maxRunes]) + "…"
}
