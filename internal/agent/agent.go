// Package agent runs the request/tool-call loop.
//
// It knows nothing about terminals. Output reaches the user through an
// Observer and permission prompts through an Asker, so the same loop drives the
// REPL, a headless run, and eventually an SDK consumer (design principle 1).
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/cjvana/switchboard/internal/permission"
	"github.com/cjvana/switchboard/internal/provider"
	"github.com/cjvana/switchboard/internal/session"
	"github.com/cjvana/switchboard/internal/tools"
)

const (
	// DefaultMaxToolRounds bounds one user turn. Loop detection and escalation
	// are phase 2 work; until then this is the backstop that keeps a model
	// stuck in a retry cycle from running until the budget is gone.
	DefaultMaxToolRounds = 40

	// DefaultMaxAttempts bounds retries of a single model call.
	DefaultMaxAttempts = 3

	retryBaseDelay = 500 * time.Millisecond
)

// ErrRoundLimit reports that a turn hit its tool-round cap with the model still
// asking for more calls.
var ErrRoundLimit = errors.New("turn exceeded its tool-round limit")

type Loop struct {
	Provider provider.Provider
	Target   provider.RouteTarget
	Tools    *tools.Registry
	Perms    *permission.Engine
	Asker    permission.Asker
	Session  *session.Session
	Observer Observer

	System        []provider.Block
	MaxToolRounds int
	MaxAttempts   int
}

func (l *Loop) observer() Observer {
	if l.Observer == nil {
		return NopObserver{}
	}
	return l.Observer
}

// Turn runs one user message to completion.
//
// It returns an error only when the turn could not be carried out: a protocol
// failure, an exhausted retry budget, or cancellation. A tool that fails, times
// out, or is denied is not an error here, because the model is expected to see
// that result and decide what to do about it.
func (l *Loop) Turn(ctx context.Context, input string) error {
	if err := l.Session.AppendMessage(provider.UserText(input)); err != nil {
		return err
	}

	maxRounds := orDefault(l.MaxToolRounds, DefaultMaxToolRounds)
	for range maxRounds {
		msg, stop, usage, attempts, err := l.callModel(ctx)
		if err != nil {
			// Content that did arrive is recorded as an interrupted turn, so the
			// session shows what happened instead of a gap. Adapters drop
			// incomplete messages when building the next request, which is what
			// makes re-issuing safe.
			if len(msg.Content) > 0 {
				msg.Incomplete = true
				if appendErr := l.Session.AppendMessage(msg); appendErr != nil {
					return appendErr
				}
			}
			return err
		}

		if err := l.Session.AppendMessage(msg); err != nil {
			return err
		}
		record := session.Usage{
			Target:   string(l.Target.ID()),
			Usage:    usage,
			Attempts: attempts,
		}
		if err := l.Session.AppendUsage(record); err != nil {
			return err
		}
		l.observer().TurnUsage(record)

		uses := msg.ToolUses()
		if stop != provider.StopToolUse || len(uses) == 0 {
			return nil
		}

		results, runErr := l.runTools(ctx, uses)
		// Results are appended even when the turn is being abandoned. An
		// assistant message whose tool calls have no matching results is a
		// malformed conversation, and every later request built from this
		// session would carry that damage forward.
		if len(results) > 0 {
			if err := l.Session.AppendMessage(provider.Message{
				Role:    provider.RoleTool,
				Content: results,
			}); err != nil {
				return err
			}
		}
		if runErr != nil {
			return runErr
		}
	}

	msg := fmt.Sprintf("turn stopped at the %d tool-round limit", maxRounds)
	l.observer().Notice("warn", msg)
	l.Session.AppendNote("warn", msg)
	return ErrRoundLimit
}

// callModel issues one model call, retrying transient failures. It returns
// whatever content arrived even when it ends in an error, so the caller can
// record a partial turn.
func (l *Loop) callModel(ctx context.Context) (provider.Message, provider.StopReason, provider.Usage, int, error) {
	maxAttempts := orDefault(l.MaxAttempts, DefaultMaxAttempts)

	req := provider.Request{
		System:   l.System,
		Tools:    l.Tools.Definitions(),
		Messages: l.Session.State().Messages,
	}

	var lastMsg provider.Message
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		msg, stop, usage, err := l.streamOnce(ctx, req)
		if err == nil {
			return msg, stop, usage, attempt, nil
		}
		lastMsg, lastErr = msg, err

		if ctx.Err() != nil {
			return msg, stop, usage, attempt, ctx.Err()
		}
		if !retryable(err) || attempt == maxAttempts {
			return msg, stop, usage, attempt, err
		}

		// A dropped stream is re-issued from the last committed message rather
		// than resumed. Ollama exposes no continuation handle, and treating a
		// partial response as committed would mean guessing what the server
		// had already produced (§10.3).
		l.observer().Notice("warn", fmt.Sprintf("attempt %d of %d failed (%v), retrying", attempt, maxAttempts, err))
		if err := sleep(ctx, backoff(attempt)); err != nil {
			return msg, stop, usage, attempt, err
		}
	}
	return lastMsg, "", provider.Usage{}, maxAttempts, lastErr
}

func (l *Loop) streamOnce(ctx context.Context, req provider.Request) (provider.Message, provider.StopReason, provider.Usage, error) {
	var b messageBuilder
	var stop provider.StopReason
	var usage provider.Usage

	stream, err := l.Provider.Stream(ctx, l.Target, req)
	if err != nil {
		return b.message(), stop, usage, err
	}
	defer stream.Close()

	for {
		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return b.message(), stop, usage, nil
		}
		if err != nil {
			return b.message(), stop, usage, err
		}

		switch ev.Type {
		case provider.EventThinkingDelta:
			b.delta(ev.Index, provider.KindThinking, ev.Text)
			l.observer().ThinkingDelta(ev.Text)
		case provider.EventTextDelta:
			b.delta(ev.Index, provider.KindText, ev.Text)
			l.observer().TextDelta(ev.Text)
		case provider.EventToolUse:
			b.toolUse(ev.Index, *ev.ToolUse)
		case provider.EventDone:
			stop = ev.StopReason
			usage = ev.Usage
		}
	}
}

type toolJob struct {
	use    provider.ToolUse
	plan   tools.Plan
	result *tools.Result
	ready  bool
}

// runTools resolves permission for every call and then executes. Results come
// back in call order regardless of how they were scheduled.
func (l *Loop) runTools(ctx context.Context, uses []provider.ToolUse) ([]provider.Block, error) {
	jobs := make([]*toolJob, len(uses))
	for i, use := range uses {
		j := &toolJob{use: use}
		jobs[i] = j

		tool, ok := l.Tools.Get(use.Name)
		if !ok {
			j.fail("no tool named %q is available", use.Name)
			continue
		}

		plan, err := tool.Plan(use.Input)
		if err != nil {
			// Bad arguments go back to the model as a tool error so it can
			// correct them. Only protocol-level damage aborts a turn (§10.3).
			j.fail("%s", err.Error())
			continue
		}
		j.plan = plan

		approved, outcome, err := l.Perms.Resolve(ctx, l.Asker, plan.Request)
		if err != nil {
			return resultBlocks(jobs, "the permission prompt failed"), err
		}
		l.Session.AppendPermission(session.Permission{
			Tool:     use.Name,
			Mode:     string(l.Perms.Mode()),
			Decision: string(outcome.Decision),
			Reason:   outcome.Reason,
		})
		if !approved {
			j.fail("the user did not approve this call: %s", outcome.Reason)
			continue
		}
		j.ready = true
	}

	var pending []*toolJob
	parallel := true
	for _, j := range jobs {
		if !j.ready {
			continue
		}
		pending = append(pending, j)
		if tool, ok := l.Tools.Get(j.use.Name); !ok || !tool.ParallelSafe() {
			parallel = false
		}
	}

	// A mixed batch runs serially rather than partitioned, because reordering
	// a write around a read changes what the read returns.
	if parallel && len(pending) > 1 {
		var wg sync.WaitGroup
		for _, j := range pending {
			wg.Add(1)
			go func(j *toolJob) {
				defer wg.Done()
				l.execute(ctx, j)
			}(j)
		}
		wg.Wait()
	} else {
		for _, j := range pending {
			if ctx.Err() != nil {
				break
			}
			l.execute(ctx, j)
		}
	}

	if ctx.Err() != nil {
		return resultBlocks(jobs, "cancelled before this call ran"), ctx.Err()
	}
	return resultBlocks(jobs, "did not run"), nil
}

func (l *Loop) execute(ctx context.Context, j *toolJob) {
	l.observer().ToolStart(j.use.Name, j.plan.Request)
	started := time.Now()

	res, err := j.plan.Run(ctx)
	if err != nil {
		res = tools.Result{Content: err.Error(), IsError: true}
	}
	j.result = &res
	l.observer().ToolEnd(j.use.Name, res, time.Since(started))
}

func (j *toolJob) fail(format string, args ...any) {
	j.result = &tools.Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// resultBlocks assembles results in call order, filling any gap so that every
// tool call has exactly one result.
func resultBlocks(jobs []*toolJob, unfilled string) []provider.Block {
	out := make([]provider.Block, 0, len(jobs))
	for _, j := range jobs {
		res := tools.Result{Content: unfilled, IsError: true}
		if j.result != nil {
			res = *j.result
		}
		out = append(out, provider.ToolResult{
			ToolUseID: j.use.ID,
			Name:      j.use.Name,
			Content:   res.Content,
			IsError:   res.IsError,
		})
	}
	return out
}

// messageBuilder reassembles streamed events into canonical blocks, keyed by
// the block index the adapter assigned and emitted in arrival order.
type messageBuilder struct {
	byIndex map[int]*blockAccum
	order   []int
}

type blockAccum struct {
	kind provider.BlockKind
	text strings.Builder
	use  provider.ToolUse
}

func (b *messageBuilder) accum(index int, kind provider.BlockKind) *blockAccum {
	if b.byIndex == nil {
		b.byIndex = map[int]*blockAccum{}
	}
	a, ok := b.byIndex[index]
	if !ok {
		a = &blockAccum{kind: kind}
		b.byIndex[index] = a
		b.order = append(b.order, index)
	}
	return a
}

func (b *messageBuilder) delta(index int, kind provider.BlockKind, text string) {
	b.accum(index, kind).text.WriteString(text)
}

func (b *messageBuilder) toolUse(index int, use provider.ToolUse) {
	b.accum(index, provider.KindToolUse).use = use
}

func (b *messageBuilder) message() provider.Message {
	msg := provider.Message{Role: provider.RoleAssistant}
	for _, i := range b.order {
		a := b.byIndex[i]
		switch a.kind {
		case provider.KindThinking:
			msg.Content = append(msg.Content, provider.Thinking{Text: a.text.String()})
		case provider.KindText:
			msg.Content = append(msg.Content, provider.Text{Text: a.text.String()})
		case provider.KindToolUse:
			msg.Content = append(msg.Content, a.use)
		}
	}
	return msg
}

func retryable(err error) bool {
	var apiErr *provider.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	// A dropped stream is worth another try. Malformed content is not, because
	// re-issuing produces the same shape and burns the attempt budget.
	return errors.Is(err, provider.ErrStreamIncomplete)
}

// backoff grows exponentially with jitter, so several clients failing against
// one server do not resynchronize their retries.
func backoff(attempt int) time.Duration {
	base := retryBaseDelay << (attempt - 1)
	return base + time.Duration(rand.Int64N(int64(base/2)))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func orDefault(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}
