// Package supervisor — the context guard.
//
// An agent's context only grows. Past a threshold (context_guard.tokens,
// default 120000) the guard intervenes:
//
//   - A child agent is first asked, by a queued system message, to write a
//     short handoff note to .sprawl/agents/<name>/handoff.md and end its turn.
//     At the next turn end that finds it fully quiescent (the idle reaper's own
//     predicate, minus the idle-age term) it is stopped and woken in a FRESH
//     session whose first turn is its original spawn prompt plus that note.
//     Its worktree and branch are not touched.
//   - weave is only asked to call `handoff` at its next safe point. A forced
//     restart of weave would drop prompts the user has queued in the TUI.
//
// "Context size" is input + cache_read + cache_creation tokens of the LAST
// main-chain assistant frame seen before the turn ended. It is deliberately
// not a sum over the turn's frames: each frame re-sends the whole prefix, so a
// sum over-counts by roughly the number of model calls in the turn.
package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmotles/sprawl/internal/agentloop"
	"github.com/dmotles/sprawl/internal/agentops"
	"github.com/dmotles/sprawl/internal/messages"
	"github.com/dmotles/sprawl/internal/protocol"
	runtimepkg "github.com/dmotles/sprawl/internal/runtime"
	"github.com/dmotles/sprawl/internal/state"
)

// contextGuardSender is the From recorded on the guard's queued messages, so
// the recipient can tell it came from sprawl rather than from a peer.
const contextGuardSender = "sprawl-context-guard"

// contextGuardNoteFile is the handoff note's file name inside the agent's
// state directory (.sprawl/agents/<name>/).
const contextGuardNoteFile = "handoff.md"

// contextGuardNoteMaxBytes caps how much of the note is carried into the fresh
// session. The agent is asked for at most 1k characters; the cap is generous
// so a slightly long note survives, and bounded so a runaway one cannot
// rebuild the context the restart exists to shed.
const contextGuardNoteMaxBytes = 4096

// contextGuardSettle is how long one restart attempt keeps re-checking for
// quiescence after a turn end. The turn-completed event is published before
// the runtime clears its in-turn phase, so the first check usually sees
// in_turn; a short poll absorbs that. Anything still blocking after this is
// retried at the agent's next turn end.
var contextGuardSettle = newAtomicDuration(15 * time.Second)

// contextGuardPoll is the re-check interval inside one settle window.
var contextGuardPoll = newAtomicDuration(250 * time.Millisecond)

// contextGuardStopBudget bounds the stop + fresh wake of one restart.
const contextGuardStopBudget = 30 * time.Second

// contextSizeOf returns the context size carried by one protocol frame, and
// false when the frame is not a main-chain assistant frame with usage.
// Sidechain frames (parent_tool_use_id set) are a different conversation and
// must not be read as the agent's own context.
func contextSizeOf(msg *protocol.Message) (int, bool) {
	if msg == nil || msg.Type != "assistant" || len(msg.Raw) == 0 {
		return 0, false
	}
	var am protocol.AssistantMessage
	if err := json.Unmarshal(msg.Raw, &am); err != nil {
		return 0, false
	}
	if am.ParentToolUseID != nil {
		return 0, false
	}
	u, _, err := am.ParseUsage()
	if err != nil || u == nil {
		return 0, false
	}
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens, true
}

// contextTurnObserver receives one call per completed turn with the context
// size of the turn's last main-chain assistant frame.
type contextTurnObserver func(agent, sessionID string, size int)

// contextWatch is the per-runtime state the subscriber keeps. Factored out of
// the goroutine so the last-frame rule is testable without a bus.
type contextWatch struct {
	size      int
	sessionID string
}

// observe folds one runtime event in. It returns (size, sessionID, true) when
// the event is a completed turn with a measured context size.
func (w *contextWatch) observe(ev runtimepkg.RuntimeEvent) (int, string, bool) {
	switch ev.Type {
	case runtimepkg.EventProtocolMessage:
		if n, ok := contextSizeOf(ev.Message); ok {
			// Overwrite, never add: the last frame IS the context size.
			w.size = n
			if ev.Message.SessionID != "" {
				w.sessionID = ev.Message.SessionID
			}
		}
	case runtimepkg.EventTurnCompleted:
		if w.size > 0 {
			return w.size, w.sessionID, true
		}
	}
	return 0, "", false
}

// runContextWatchSubscriber watches one runtime's bus and reports each
// completed turn's context size to observe. A nil observer still drains the
// bus. The returned function unsubscribes and waits for the goroutine.
func runContextWatchSubscriber(bus *runtimepkg.EventBus, agent string, observe contextTurnObserver, name string) func() {
	ch, unsub := bus.SubscribeNamed(name, 64)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		var w contextWatch
		for ev := range ch {
			size, sid, ok := w.observe(ev)
			if ok && observe != nil {
				observe(agent, sid, size)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			unsub()
			<-doneCh
		})
	}
}

// contextGuardEntry is the per-agent guard state. It is keyed to one session:
// a new session id (a fresh restart, a weave handoff) starts it over.
type contextGuardEntry struct {
	sessionID  string
	asked      bool
	restarting bool
}

// contextGuard holds the threshold and per-agent state for Real.
type contextGuard struct {
	tokens atomic.Int64

	mu      sync.Mutex
	entries map[string]*contextGuardEntry
	closed  bool
	done    chan struct{}
	wg      sync.WaitGroup
}

func newContextGuard(tokens int) *contextGuard {
	g := &contextGuard{entries: make(map[string]*contextGuardEntry), done: make(chan struct{})}
	g.tokens.Store(int64(tokens))
	return g
}

// entryLocked returns the agent's entry for sessionID, resetting it when the
// session changed. Caller holds g.mu.
func (g *contextGuard) entryLocked(agent, sessionID string) *contextGuardEntry {
	e, ok := g.entries[agent]
	if !ok || (sessionID != "" && e.sessionID != sessionID) {
		e = &contextGuardEntry{sessionID: sessionID}
		g.entries[agent] = e
	}
	return e
}

// goAsync runs fn on a tracked goroutine unless the guard is closed.
func (g *contextGuard) goAsync(fn func()) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		fn()
	}()
	return true
}

// stop refuses new work, wakes any settle wait, and waits for in-flight
// restarts to finish. Idempotent.
func (g *contextGuard) stop() {
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		close(g.done)
	}
	g.mu.Unlock()
	g.wg.Wait()
}

// contextGuardNotePath is where an agent writes its handoff note.
func contextGuardNotePath(sprawlRoot, agent string) string {
	return filepath.Join(state.AgentsDir(sprawlRoot), agent, contextGuardNoteFile)
}

// contextGuardNoteRequest is the message asking a child for its handoff note.
func contextGuardNoteRequest(notePath string, size, threshold int) string {
	return fmt.Sprintf("Context guard: your context is %d tokens (limit %d). "+
		"Write a handoff note of at most 1000 characters to %s: what is done, what is next, open decisions. "+
		"Commit any work you want kept, then end your turn. "+
		"sprawl will restart you in a fresh session with your original task and this note; your worktree and branch stay as they are.",
		size, threshold, notePath)
}

// contextGuardWeaveRequest is the message asking weave to hand off.
func contextGuardWeaveRequest(size, threshold int) string {
	return fmt.Sprintf("Context guard: your context is %d tokens (limit %d). "+
		"At your next safe point (no user prompt waiting on you, no reply owed to a child), call the handoff tool so the session restarts fresh.",
		size, threshold)
}

// buildContextGuardInjection is the first turn of the fresh session: the
// original spawn prompt plus the handoff note, or an explicit line saying no
// note was written.
func buildContextGuardInjection(agent, prompt, note, notePath string, size, threshold int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[sprawl context guard] Your previous session reached %d tokens of context (limit %d), so you were restarted in a fresh session. "+
		"You are still %s, in the same worktree and on the same branch; check git log and git status for work already done.\n\n", size, threshold, agent)
	b.WriteString("## Original task\n\n")
	if strings.TrimSpace(prompt) == "" {
		b.WriteString("(no spawn prompt is recorded for this agent)\n")
	} else {
		b.WriteString(strings.TrimRight(prompt, "\n"))
		b.WriteString("\n")
	}
	b.WriteString("\n## Handoff note from your previous session\n\n")
	if strings.TrimSpace(note) == "" {
		fmt.Fprintf(&b, "No handoff note was written (%s was missing or empty). Reconstruct where you were from git log, git status and your worktree before continuing.\n", notePath)
	} else {
		b.WriteString(strings.TrimRight(note, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// readContextGuardNote reads the handoff note, capped at
// contextGuardNoteMaxBytes. A missing file is an empty note.
func readContextGuardNote(path string) string {
	f, err := os.Open(path) //nolint:gosec // path is derived from the sprawl root and a validated agent name
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, contextGuardNoteMaxBytes+1)
	n, _ := f.Read(buf)
	if n > contextGuardNoteMaxBytes {
		return string(buf[:contextGuardNoteMaxBytes]) + "\n[note truncated]"
	}
	return string(buf[:n])
}

// onContextTurn is the observer installed on every child runtime and on
// weave's. It runs on a bus subscriber goroutine, so everything that can block
// is handed to a tracked goroutine.
func (r *Real) onContextTurn(agent, sessionID string, size int) {
	g := r.ctxGuard
	if g == nil || agent == "" {
		return
	}
	threshold := int(g.tokens.Load())
	if threshold <= 0 || size < threshold {
		return
	}
	isRoot := agent == r.callerName

	g.mu.Lock()
	e := g.entryLocked(agent, sessionID)
	switch {
	case !e.asked:
		e.asked = true
		g.mu.Unlock()
		action := "ask for a handoff note, then restart fresh"
		if isRoot {
			action = "ask weave to call handoff"
		}
		slog.Default().Info("context guard: fired",
			slog.String("agent", agent),
			slog.Int("context_tokens", size),
			slog.Int("threshold", threshold),
			slog.Bool("root", isRoot),
			slog.String("action", action),
		)
		body := contextGuardWeaveRequest(size, threshold)
		if !isRoot {
			notePath := contextGuardNotePath(r.sprawlRoot, agent)
			// A note left over from an earlier cycle would be carried into the
			// next session as if it were current.
			_ = os.Remove(notePath)
			body = contextGuardNoteRequest(notePath, size, threshold)
		}
		g.goAsync(func() {
			if err := r.injectSystemMessage(agent, body); err != nil {
				slog.Default().Warn("context guard: could not queue the request",
					slog.String("agent", agent), slog.Any("err", err))
			}
		})
		return
	case isRoot || e.restarting:
		// weave is asked once per session and never restarted here.
		g.mu.Unlock()
		return
	}
	e.restarting = true
	g.mu.Unlock()

	started := g.goAsync(func() {
		defer func() {
			g.mu.Lock()
			e.restarting = false
			g.mu.Unlock()
		}()
		r.contextGuardRestart(agent, size, threshold)
	})
	if !started {
		g.mu.Lock()
		e.restarting = false
		g.mu.Unlock()
	}
}

// injectSystemMessage queues body for agent the way SendMessage does (maildir
// envelope, then queue entry, then a poke), as sprawl rather than as a peer.
func (r *Real) injectSystemMessage(agent, body string) error {
	gate := r.reclaimGate(agent)
	gate.Lock()
	defer gate.Unlock()
	shortID, err := messages.Send(r.sprawlRoot, contextGuardSender, agent, "", body)
	if err != nil {
		return fmt.Errorf("writing the message envelope: %w", err)
	}
	if _, err := agentloop.Enqueue(r.sprawlRoot, agent, agentloop.Entry{
		ShortID: shortID,
		Class:   agentloop.ClassAsync,
		From:    contextGuardSender,
		Body:    body,
	}); err != nil {
		return fmt.Errorf("enqueueing: %w", err)
	}
	if rt, ok := r.startedRuntime(agent); ok {
		// The entry is durable; a failed poke is retried at the next drain.
		_ = rt.WakeForDelivery()
	}
	return nil
}

// quiescenceInputsFor is idleInputsFor with the idle-age term reduced to "has
// been active at least once": the guard restarts at the first quiescent point,
// not after an idle period. Every other term (in turn, outstanding work,
// pending queue, in-flight system mail, pending question, root) is unchanged.
func (r *Real) quiescenceInputsFor(rt *AgentRuntime) idleInputs {
	in := r.idleInputsFor(rt)
	in.Threshold = time.Nanosecond
	return in
}

// contextGuardRestart polls for quiescence for one settle window and restarts
// the agent when it finds it. It gives up quietly (to be retried at the next
// turn end) if the agent stays busy.
func (r *Real) contextGuardRestart(agent string, size, threshold int) {
	if r.runtimeRegistry == nil || r.ctxGuard == nil {
		return
	}
	rt, ok := r.runtimeRegistry.Get(agent)
	if !ok {
		return
	}
	deadline := time.Now().Add(contextGuardSettle.get())
	for {
		restarted, blocker := r.tryContextGuardRestart(rt, size, threshold)
		if restarted || blocker == "not_root" {
			return
		}
		if !time.Now().Before(deadline) {
			slog.Default().Info("context guard: restart deferred to the next turn end",
				slog.String("agent", agent), slog.String("blocker", blocker))
			return
		}
		select {
		case <-r.ctxGuard.done:
			return
		case <-time.After(contextGuardPoll.get()):
		}
	}
}

// tryContextGuardRestart makes one attempt. It returns whether the agent was
// restarted and, if not, the term that blocked it.
//
// The reclaim gate is held from the final quiescence check through the stop
// AND the fresh wake. Releasing it between the two would let a SendMessage see
// the agent resting idle and wake it by RESUMING the old session, which is the
// one outcome this guard exists to prevent.
func (r *Real) tryContextGuardRestart(rt *AgentRuntime, size, threshold int) (bool, string) {
	name := rt.Name()
	gate := r.reclaimGate(name)

	gate.Lock()
	a := assessIdle(r.quiescenceInputsFor(rt))
	gate.Unlock()
	if !a.Reap {
		return false, a.Blocker
	}

	var held bool
	blocker := ""
	guard := func() (bool, func()) {
		gate.Lock()
		if again := assessIdle(r.quiescenceInputsFor(rt)); !again.Reap {
			blocker = again.Blocker
			gate.Unlock()
			return false, nil
		}
		held = true
		return true, nil // released below, after the wake
	}
	ctx, cancel := context.WithTimeout(context.Background(), contextGuardStopBudget)
	defer cancel()
	stopped, stopErr := rt.StopAfterTurnIf(ctx, contextGuardStopBudget, stopReasonContextGuard, guard)
	if held {
		defer gate.Unlock()
	}
	if !stopped && stopErr == nil {
		return false, blocker
	}
	if stopErr != nil {
		slog.Default().Warn("context guard: stop failed; agent left as-is",
			slog.String("agent", name), slog.Any("err", stopErr))
		return false, "stop_failed"
	}

	notePath := contextGuardNotePath(r.sprawlRoot, name)
	note := readContextGuardNote(notePath)
	prompt := ""
	if st, err := state.LoadAgent(r.sprawlRoot, name); err == nil {
		prompt = st.Prompt
		if err := agentops.AssertNotOnMain(st.Worktree, name); err != nil {
			slog.Default().Warn("context guard: agent left stopped, refusing to wake it onto main",
				slog.String("agent", name), slog.Any("err", err))
			return false, "on_main"
		}
	}
	injection := buildContextGuardInjection(name, prompt, note, notePath, size, threshold)

	r.questions.cancelByAgent(name, "agent restarting for the context guard")
	wakeCtx, wakeCancel := context.WithTimeout(context.Background(), contextGuardStopBudget)
	defer wakeCancel()
	res, err := rt.WakeFresh(wakeCtx, injection)
	if err != nil {
		// The agent rests idle; the next message wakes it (by resume).
		slog.Default().Warn("context guard: fresh restart failed; agent left idle",
			slog.String("agent", name), slog.Any("err", err))
		return false, "wake_failed"
	}
	if note != "" {
		_ = os.Rename(notePath, notePath+".prev")
	}
	// The new session starts the guard over, even if its frames never carry
	// a session id to key the reset on.
	if g := r.ctxGuard; g != nil {
		g.mu.Lock()
		delete(g.entries, name)
		g.mu.Unlock()
	}
	slog.Default().Info("context guard: restarted in a fresh session",
		slog.String("agent", name),
		slog.Int("context_tokens", size),
		slog.Int("threshold", threshold),
		slog.String("mode", res.Mode),
		slog.Bool("handoff_note", note != ""),
	)
	if emit := r.wokenEmitter; emit != nil {
		emit(name)
	}
	// Mail that arrived while the agent was down is in pending/; poke so the
	// new session drains it rather than waiting for unrelated traffic.
	if pending, lErr := agentloop.ListPending(r.sprawlRoot, name); lErr != nil || len(pending) > 0 {
		_ = rt.WakeForDelivery()
	}
	return true, ""
}
