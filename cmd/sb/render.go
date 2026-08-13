package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cjvana/switchboard/internal/permission"
	"github.com/cjvana/switchboard/internal/session"
	"github.com/cjvana/switchboard/internal/tools"
)

// renderer writes a turn to a terminal. It tracks what kind of output it last
// wrote so that model text, tool lines, and notices stay visually separate
// without the loop having to know about any of it.
type renderer struct {
	w     *bufio.Writer
	color bool

	lastKind  string
	atLineTop bool
}

func newRenderer(f *os.File) *renderer {
	return &renderer{
		w:         bufio.NewWriter(f),
		color:     useColor(f),
		atLineTop: true,
	}
}

func useColor(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

const (
	reset = "\x1b[0m"
	dim   = "\x1b[2m"
	bold  = "\x1b[1m"
	red   = "\x1b[31m"
)

func (r *renderer) style(code, s string) string {
	if !r.color {
		return s
	}
	return code + s + reset
}

func (r *renderer) flush() { r.w.Flush() }

// section separates a new kind of output from whatever came before it.
func (r *renderer) section(kind string) {
	if r.lastKind == kind {
		return
	}
	if !r.atLineTop {
		r.w.WriteByte('\n')
		r.atLineTop = true
	}
	if r.lastKind != "" {
		r.w.WriteByte('\n')
	}
	r.lastKind = kind
}

func (r *renderer) write(s string) {
	if s == "" {
		return
	}
	r.w.WriteString(s)
	r.atLineTop = strings.HasSuffix(s, "\n")
}

func (r *renderer) line(s string) {
	if !r.atLineTop {
		r.w.WriteByte('\n')
	}
	r.w.WriteString(s)
	r.w.WriteByte('\n')
	r.atLineTop = true
}

func (r *renderer) ThinkingDelta(text string) {
	r.section("thinking")
	r.write(r.style(dim, text))
	r.flush()
}

func (r *renderer) TextDelta(text string) {
	r.section("text")
	r.write(text)
	r.flush()
}

func (r *renderer) ToolStart(name string, req permission.Request) {
	r.section("tool")
	r.line(r.style(bold, name) + " " + r.style(dim, describeRequest(req)))
	r.flush()
}

func (r *renderer) ToolEnd(name string, res tools.Result, took time.Duration) {
	status := "ok in " + formatDuration(took)
	if res.IsError {
		status = "failed in " + formatDuration(took)
	}

	detail := firstLine(res.Content)
	if detail != "" {
		status += ": " + detail
	}
	if res.IsError {
		r.line("  " + r.style(red, status))
	} else {
		r.line("  " + r.style(dim, status))
	}
	r.flush()
}

func (r *renderer) Notice(level, text string) {
	r.section("notice")
	prefix := "  note: "
	if level == "warn" || level == "error" {
		prefix = "  " + level + ": "
	}
	r.line(r.style(dim, prefix+text))
	r.flush()
}

func (r *renderer) TurnUsage(session.Usage) {}

// endTurn closes out whatever the turn was writing so the next prompt starts
// on a clean line.
func (r *renderer) endTurn() {
	if !r.atLineTop {
		r.w.WriteByte('\n')
		r.atLineTop = true
	}
	r.lastKind = ""
	r.flush()
}

func describeRequest(req permission.Request) string {
	if req.Effect == permission.EffectExecute {
		return tools.Describe(req.Argv, req.Shell)
	}
	return req.Path
}

// formatDuration keeps a fast tool from reporting "0s", which reads as a
// failure to measure rather than as speed.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return d.Round(10 * time.Microsecond).String()
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	default:
		return d.Round(10 * time.Millisecond).String()
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	line, _, _ := strings.Cut(s, "\n")
	const width = 100
	if len(line) > width {
		return line[:width] + "..."
	}
	return line
}

// terminalAsker resolves a permission Ask against the same stdin the REPL
// reads. The loop is not reading input while a tool is pending, so there is no
// contention.
type terminalAsker struct {
	in  *bufio.Reader
	out *renderer
}

func (a *terminalAsker) Ask(_ context.Context, req permission.Request, out permission.Outcome) (permission.Response, error) {
	r := a.out
	r.section("prompt")
	r.line(r.style(bold, "approve "+req.Tool) + " " + describeRequest(req))
	r.line(r.style(dim, "  "+out.Reason))

	// Design principle 4: a prompt is not containment, and the moment the user
	// approves is the moment that has to be plain.
	if out.SandboxAbsent {
		r.line(r.style(dim, "  this command is not sandboxed and can do anything your account can"))
	}
	r.line("  [y] once   [a] always, this exact command   [n] no")
	r.w.WriteString("  > ")
	r.atLineTop = false
	r.flush()

	answer, err := a.in.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			// No one is there to answer, so nothing is approved.
			r.line("")
			return permission.Response{}, nil
		}
		return permission.Response{}, err
	}

	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return permission.Response{Approved: true}, nil
	case "a", "always":
		return permission.Response{Approved: true, Remember: true}, nil
	default:
		return permission.Response{}, nil
	}
}
