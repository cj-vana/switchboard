package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as the MCP server the stdio tests spawn: the test binary
// re-executes itself with SB_MCP_STDIO_HELPER set and becomes a real child
// process speaking real JSON-RPC over real pipes. The transport is exercised
// against an actual subprocess, not a description of one, per the testdata
// rule: wire behavior gets captured where it happens.
func TestMain(m *testing.M) {
	if os.Getenv("SB_MCP_STDIO_HELPER") == "1" {
		runHelperServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runHelperServer() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64<<10), 1<<20)
	out := bufio.NewWriter(os.Stdout)
	reply := func(id int64, result string) {
		fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%d,"result":%s}`+"\n", id, result)
		out.Flush()
	}
	for in.Scan() {
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			reply(*req.ID, `{"protocolVersion":"2025-06-18","serverInfo":{"name":"helper","version":"0.0"}}`)
		case "tools/list":
			reply(*req.ID, `{"tools":[{"name":"env","description":"reports selected environment variables","inputSchema":{"type":"object"}}]}`)
		case "tools/call":
			seen := fmt.Sprintf("anthropic=%q sb=%q extra=%q",
				os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("SB_OLLAMA_API_KEY"), os.Getenv("SB_TEST_EXTRA"))
			body, _ := json.Marshal(seen)
			reply(*req.ID, fmt.Sprintf(`{"content":[{"type":"text","text":%s}]}`, body))
		}
	}
}

func TestStdioEndToEndFiltersCredentials(t *testing.T) {
	// These are the parent's model credentials; the child must not see them.
	t.Setenv("ANTHROPIC_API_KEY", "parent-anthropic-secret")
	t.Setenv("SB_OLLAMA_API_KEY", "parent-sb-secret")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := Connect(ctx, Spec{
		Name:    "helper",
		Command: os.Args[0],
		Env: map[string]string{
			"SB_MCP_STDIO_HELPER": "1",
			"SB_TEST_EXTRA":       "explicitly-granted",
		},
	}, func(string, string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	tools := c.Tools()
	if len(tools) != 1 || tools[0].Name != "env" {
		t.Fatalf("tools = %+v", tools)
	}

	res, err := c.Call(ctx, "env", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "parent-anthropic-secret") || strings.Contains(res.Content, "parent-sb-secret") {
		t.Errorf("the server saw a model credential: %s", res.Content)
	}
	if !strings.Contains(res.Content, `anthropic=""`) || !strings.Contains(res.Content, `sb=""`) {
		t.Errorf("blocked variables should read empty in the child: %s", res.Content)
	}
	if !strings.Contains(res.Content, `extra="explicitly-granted"`) {
		t.Errorf("an explicitly configured variable must pass through: %s", res.Content)
	}
}

func TestStdioCloseTerminatesTheServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := Connect(ctx, Spec{
		Name:    "helper",
		Command: os.Args[0],
		Env:     map[string]string{"SB_MCP_STDIO_HELPER": "1"},
	}, func(string, string) {})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return; the child was not reaped")
	}
}

func TestConnectRejectsAMissingCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Connect(ctx, Spec{Name: "ghost", Command: "definitely-not-a-real-binary-xyz"}, nil)
	if err == nil {
		t.Fatal("connecting to a nonexistent command must fail, not hang")
	}
}
