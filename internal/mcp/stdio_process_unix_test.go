//go:build unix

package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStdioCloseTerminatesDescendants(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant-pid")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, Spec{
		Name:    "descendant-helper",
		Command: os.Args[0],
		Env: map[string]string{
			"SB_MCP_STDIO_HELPER":        "1",
			"SB_MCP_DESCENDANT_PID_FILE": pidFile,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		_ = c.Close()
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		_ = c.Close()
		t.Fatal(err)
	}
	transport := c.transport.(*stdioTransport)
	rootGroup, err := syscall.Getpgid(transport.cmd.Process.Pid)
	if err != nil {
		_ = c.Close()
		t.Fatal(err)
	}
	descendantGroup, err := syscall.Getpgid(pid)
	if err != nil {
		_ = c.Close()
		t.Fatal(err)
	}
	if descendantGroup != rootGroup {
		_ = c.Close()
		t.Fatalf("descendant process group = %d, want server group %d", descendantGroup, rootGroup)
	}
	if err := c.Close(); err != nil && !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("Close error = %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant pid %d survived stdio Close (last probe error: %v)", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
