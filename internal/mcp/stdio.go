package mcp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// maxLine bounds one JSON-RPC message. A server that emits more than this in
// a single line is treated as broken rather than buffered without limit.
const maxLine = 32 << 20

// stdioTransport is a child process speaking newline-delimited JSON-RPC on
// its pipes. Stderr is drained into a bounded tail for diagnostics: a server
// that logs there must not block on a full pipe, and the last lines are the
// ones that explain a death.
type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	writeMu sync.Mutex

	stderrMu sync.Mutex
	stderr   []string
}

func startStdio(spec Spec) (*stdioTransport, error) {
	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Env = serverEnv(spec.Env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting mcp server %s: %w", spec.Name, err)
	}

	t := &stdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 64<<10),
	}
	go t.drainStderr(stderr)
	return t, nil
}

// serverEnv is the parent environment minus switchboard's own model
// credentials. The server gets PATH, HOME, and everything else it needs to
// run; it does not get the keys this program uses to reach model providers,
// because those were entrusted to switchboard, not to whatever a config file
// asked it to spawn. A server that needs a secret gets it explicitly through
// its env table.
func serverEnv(extra map[string]string) []string {
	blocked := func(name string) bool {
		if strings.HasPrefix(name, "SB_") && strings.HasSuffix(name, "_API_KEY") {
			return true
		}
		switch name {
		case "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "KIMI_API_KEY":
			return true
		}
		return false
	}

	var env []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if ok && blocked(name) {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func (t *stdioTransport) Send(msg []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.stdin.Write(append(msg, '\n')); err != nil {
		return fmt.Errorf("writing to mcp server: %w", err)
	}
	return nil
}

func (t *stdioTransport) Recv() ([]byte, error) {
	var buf bytes.Buffer
	for {
		chunk, err := t.stdout.ReadSlice('\n')
		buf.Write(chunk)
		if err == nil {
			line := bytes.TrimSpace(buf.Bytes())
			if len(line) == 0 {
				buf.Reset()
				continue
			}
			return append([]byte(nil), line...), nil
		}
		if err == bufio.ErrBufferFull {
			if buf.Len() > maxLine {
				return nil, fmt.Errorf("mcp message exceeds %d bytes", maxLine)
			}
			continue
		}
		if tail := t.stderrTail(); tail != "" && err == io.EOF {
			return nil, fmt.Errorf("%w; server said: %s", err, tail)
		}
		return nil, err
	}
}

// Close ends the conversation politely and then definitively: stdin closes,
// which is the protocol's shutdown signal, and a server still alive shortly
// after is killed. The wait prevents zombie accumulation either way.
func (t *stdioTransport) Close() error {
	t.stdin.Close()

	done := make(chan error, 1)
	go func() { done <- t.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		_ = t.cmd.Process.Kill()
		return <-done
	}
}

func (t *stdioTransport) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 8<<10), 64<<10)
	for sc.Scan() {
		t.stderrMu.Lock()
		t.stderr = append(t.stderr, sc.Text())
		if len(t.stderr) > 20 {
			t.stderr = t.stderr[len(t.stderr)-20:]
		}
		t.stderrMu.Unlock()
	}
}

func (t *stdioTransport) stderrTail() string {
	t.stderrMu.Lock()
	defer t.stderrMu.Unlock()
	if len(t.stderr) == 0 {
		return ""
	}
	n := len(t.stderr)
	if n > 3 {
		n = 3
	}
	return strings.Join(t.stderr[len(t.stderr)-n:], " | ")
}
