//go:build windows

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestStdioCloseTerminatesImmediateWindowsDescendant(t *testing.T) {
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
	pid, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
	if err != nil {
		_ = c.Close()
		t.Fatal(err)
	}
	if err := c.Close(); err != nil && !strings.Contains(err.Error(), "exit status") {
		t.Fatalf("Close error = %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			return
		}
		state, waitErr := windows.WaitForSingleObject(process, 0)
		_ = windows.CloseHandle(process)
		if waitErr == nil && state == windows.WAIT_OBJECT_0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("immediate descendant pid %d survived kill-on-close job", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
