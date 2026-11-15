package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmotles/sprawl/internal/agentloop"
	"github.com/dmotles/sprawl/internal/protocol"
	runtimepkg "github.com/dmotles/sprawl/internal/runtime"
	"github.com/dmotles/sprawl/internal/state"
)

// assistantFrame builds a main-chain (or, with parent set, sidechain)
// assistant frame carrying usage, in the wire shape the CLI emits.
func assistantFrame(sessionID string, input, cacheRead, cacheCreate, output int, parent string) *protocol.Message {
	parentJSON := "null"
	if parent != "" {
		parentJSON = fmt.Sprintf("%q", parent)
	}
	raw := fmt.Sprintf(`{"type":"assistant","session_id":%q,"parent_tool_use_id":%s,"message":{"model":"m","usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":%d}}}`,
		sessionID, parentJSON, input, cacheRead, cacheCreate, output)
	return &protocol.Message{Type: "assistant", SessionID: sessionID, Raw: []byte(raw)}
}

func TestContextSizeOf_IsInputPlusBothCacheTerms(t *testing.T) {
	got, ok := contextSizeOf(assistantFrame("s", 7, 90000, 3000, 55555, ""))
	if !ok {
		t.Fatal("contextSizeOf reported no usage for a main-chain assistant frame with usage")
	}
	if want := 7 + 90000 + 3000; got != want {
		t.Errorf("contextSizeOf = %d, want %d (input + cache_read + cache_creation; output excluded)", got, want)
	}
	if _, ok := contextSizeOf(assistantFrame("s", 1, 2, 3, 4, "toolu_1")); ok {
		t.Error("contextSizeOf accepted a sidechain frame; a sidechain's usage is not the agent's context")
	}
	if _, ok := contextSizeOf(&protocol.Message{Type: "user", Raw: []byte(`{"type":"user"}`)}); ok {
		t.Error("contextSizeOf accepted a non-assistant frame")
	}
}

// The context size at a turn end is the LAST main-chain frame, not the sum of
// the turn's frames. Summing three 50k frames would report 150k and fire a
// 120k guard on an agent whose context is 50k.
func TestContextWatch_ReportsLastFrameNotSum(t *testing.T) {
	var w contextWatch
	events := []runtimepkg.RuntimeEvent{
		{Type: runtimepkg.EventProtocolMessage, Message: assistantFrame("s1", 1, 49000, 999, 10, "")},
		{Type: runtimepkg.EventProtocolMessage, Message: assistantFrame("s1", 1, 50000, 999, 10, "")},
		// A sidechain frame with a huge context must not replace the main one.
		{Type: runtimepkg.EventProtocolMessage, Message: assistantFrame("s1", 1, 900000, 0, 10, "toolu_x")},
		{Type: runtimepkg.EventProtocolMessage, Message: assistantFrame("s1", 1, 51000, 999, 10, "")},
	}
	for _, ev := range events {
		if _, _, fired := w.observe(ev); fired {
			t.Fatalf("observe reported a turn end on a protocol frame")
		}
	}
	size, sid, ok := w.observe(runtimepkg.RuntimeEvent{Type: runtimepkg.EventTurnCompleted})
	if !ok {
		t.Fatal("observe did not report the completed turn")
	}
	if want := 1 + 51000 + 999; size != want {
		t.Errorf("turn-end context size = %d, want %d (the last main-chain frame, not a sum)", size, want)
	}
	if sid != "s1" {
		t.Errorf("sessionID = %q, want s1", sid)
	}
}

func TestContextWatch_NoUsageNoReport(t *testing.T) {
	var w contextWatch
	if _, _, ok := w.observe(runtimepkg.RuntimeEvent{Type: runtimepkg.EventTurnCompleted}); ok {
		t.Error("observe reported a size for a turn with no measured frame")
	}
}

func TestBuildContextGuardInjection_CarriesTaskAndNote(t *testing.T) {
	got := buildContextGuardInjection("alice", "ORIGINAL-TASK: fix the widget", "DONE: a. NEXT: b.", "/x/handoff.md", 130000, 120000)
	for _, want := range []string{"ORIGINAL-TASK: fix the widget", "DONE: a. NEXT: b.", "130000", "120000", "alice"} {
		if !strings.Contains(got, want) {
			t.Errorf("injection missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "No handoff note was written") {
		t.Errorf("injection claims no note was written when one was:\n%s", got)
	}

	got = buildContextGuardInjection("alice", "ORIGINAL-TASK", "  \n", "/x/handoff.md", 130000, 120000)
	if !strings.Contains(got, "ORIGINAL-TASK") || !strings.Contains(got, "No handoff note was written (/x/handoff.md") {
		t.Errorf("injection without a note must still carry the task and say no note was written:\n%s", got)
	}
}

// newContextGuardFixture is the idle-reclaim fixture (a registered, started,
// fully quiescent "alice") with a recorded spawn prompt and a short settle
// window.
func newContextGuardFixture(t *testing.T) (*Real, string, *AgentRuntime, *reclaimTestHandle) {
	t.Helper()
	r, tmpDir, rt, handle := newReclaimFixture(t)
	st, err := state.LoadAgent(tmpDir, "alice")
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	st.Prompt = "ORIGINAL-TASK: build the frobnicator"
	saveTestAgent(t, tmpDir, st)
	prevSettle, prevPoll := contextGuardSettle.get(), contextGuardPoll.get()
	contextGuardSettle.set(300 * time.Millisecond)
	contextGuardPoll.set(20 * time.Millisecond)
	t.Cleanup(func() {
		contextGuardSettle.set(prevSettle)
		contextGuardPoll.set(prevPoll)
	})
	return r, tmpDir, rt, handle
}

func lastSpec(t *testing.T, s *runtimeTestStarter) RuntimeStartSpec {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.specs) == 0 {
		t.Fatal("starter received no specs")
	}
	return s.specs[len(s.specs)-1]
}

func specCount(s *runtimeTestStarter) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.specs)
}

// Positive control for the non-quiescent tests below: a quiescent agent IS
// restarted, in a fresh session, with its original task and its note.
func TestContextGuardRestart_Quiescent_RestartsFreshWithTaskAndNote(t *testing.T) {
	r, tmpDir, rt, handle := newContextGuardFixture(t)
	serveHealthFrames(t, handle)
	notePath := contextGuardNotePath(tmpDir, "alice")
	if err := os.MkdirAll(filepath.Dir(notePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notePath, []byte("DONE: parser. NEXT: tests. OPEN: naming."), 0o644); err != nil {
		t.Fatal(err)
	}
	before := specCount(handle.starter)

	restarted, blocker := r.tryContextGuardRestart(rt, 130000, 120000)

	if !restarted {
		t.Fatalf("tryContextGuardRestart = (false, %q) for a quiescent agent, want a restart", blocker)
	}
	if got := handle.stopCalls.Load(); got != 1 {
		t.Errorf("stopCalls = %d, want 1", got)
	}
	if got := specCount(handle.starter); got != before+1 {
		t.Fatalf("starter got %d new specs, want exactly 1 (fresh only, no resume attempt)", got-before)
	}
	spec := lastSpec(t, handle.starter)
	if spec.Resume {
		t.Error("restart spec has Resume=true; the context guard must start a FRESH session")
	}
	if spec.SessionID == "" || spec.SessionID == "sess-alice" {
		t.Errorf("restart spec SessionID = %q, want a newly minted id", spec.SessionID)
	}
	for _, want := range []string{"ORIGINAL-TASK: build the frobnicator", "DONE: parser. NEXT: tests. OPEN: naming."} {
		if !strings.Contains(spec.RestartInjection, want) {
			t.Errorf("RestartInjection missing %q:\n%s", want, spec.RestartInjection)
		}
	}
	if _, err := os.Stat(notePath); !os.IsNotExist(err) {
		t.Errorf("handoff note still at %s after being consumed (err=%v); a later cycle would reuse it", notePath, err)
	}
	if !rt.SubprocessAlive() {
		t.Error("agent is not running after the restart")
	}
}

func TestContextGuardRestart_NoNote_SaysSoAndStillCarriesTask(t *testing.T) {
	r, _, rt, handle := newContextGuardFixture(t)
	serveHealthFrames(t, handle)

	if restarted, blocker := r.tryContextGuardRestart(rt, 130000, 120000); !restarted {
		t.Fatalf("tryContextGuardRestart = (false, %q), want a restart", blocker)
	}
	inj := lastSpec(t, handle.starter).RestartInjection
	if !strings.Contains(inj, "ORIGINAL-TASK: build the frobnicator") || !strings.Contains(inj, "No handoff note was written") {
		t.Errorf("RestartInjection must carry the task and say no note was written:\n%s", inj)
	}
}

// No restart while the agent is not quiescent: each case spoils exactly one
// term of the fixture the positive control above restarts.
func TestContextGuardRestart_NonQuiescent_IsNotRestarted(t *testing.T) {
	cases := []struct {
		name    string
		spoil   func(t *testing.T, tmpDir string, h *reclaimTestHandle)
		blocker string
	}{
		{"in_turn", func(_ *testing.T, _ string, h *reclaimTestHandle) { h.inTurn.Store(true) }, "in_turn"},
		{"pending_queue", func(t *testing.T, tmpDir string, _ *reclaimTestHandle) {
			if _, err := agentloop.Enqueue(tmpDir, "alice", agentloop.Entry{Class: agentloop.ClassAsync, From: "weave", Body: "queued"}); err != nil {
				t.Fatal(err)
			}
		}, "pending_queue"},
		{"work_outstanding", func(t *testing.T, _ string, h *reclaimTestHandle) {
			feedBackgroundTasks(t, h.runtimeTestSession, protocol.BackgroundTask{TaskID: "bg-1"})
		}, "work_outstanding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, tmpDir, rt, handle := newContextGuardFixture(t)
			tc.spoil(t, tmpDir, handle)
			before := specCount(handle.starter)

			restarted, blocker := r.tryContextGuardRestart(rt, 130000, 120000)

			if restarted {
				t.Fatal("agent was restarted while not quiescent")
			}
			if blocker != tc.blocker {
				t.Errorf("blocker = %q, want %q", blocker, tc.blocker)
			}
			if got := handle.stopCalls.Load(); got != 0 {
				t.Errorf("stopCalls = %d, want 0", got)
			}
			if got := specCount(handle.starter); got != before {
				t.Errorf("starter got %d new specs, want 0", got-before)
			}
		})
	}
}

// The full child sequence through the observer: below threshold nothing
// happens; the first turn end over it queues a note request and does NOT stop
// the agent; the next turn end over it restarts the agent fresh.
func TestOnContextTurn_AsksFirstThenRestarts(t *testing.T) {
	r, tmpDir, rt, handle := newContextGuardFixture(t)
	serveHealthFrames(t, handle)
	r.ctxGuard.tokens.Store(120000)

	r.onContextTurn("alice", "sess-alice", 119999)
	time.Sleep(50 * time.Millisecond)
	if p, _ := agentloop.ListPending(tmpDir, "alice"); len(p) != 0 {
		t.Fatalf("below threshold queued %d entries, want 0", len(p))
	}

	r.onContextTurn("alice", "sess-alice", 120001)
	waitFor(t, func() bool {
		p, _ := agentloop.ListPending(tmpDir, "alice")
		return len(p) == 1
	}, "note request queued")
	p, _ := agentloop.ListPending(tmpDir, "alice")
	if p[0].From != contextGuardSender || !strings.Contains(p[0].Body, contextGuardNotePath(tmpDir, "alice")) {
		t.Errorf("queued request = from %q body %q; want from %q naming the note path", p[0].From, p[0].Body, contextGuardSender)
	}
	if got := handle.stopCalls.Load(); got != 0 {
		t.Fatalf("stopCalls = %d after the first over-threshold turn, want 0 (ask first, restart later)", got)
	}

	// The agent "reads" its mail, as the drain would.
	for _, e := range p {
		if err := agentloop.MarkDelivered(tmpDir, "alice", e.ID); err != nil {
			t.Fatal(err)
		}
	}
	r.onContextTurn("alice", "sess-alice", 125000)
	waitFor(t, func() bool { return handle.stopCalls.Load() == 1 && rt.SubprocessAlive() }, "fresh restart")
	r.ctxGuard.stop()
	if spec := lastSpec(t, handle.starter); spec.Resume || !strings.Contains(spec.RestartInjection, "ORIGINAL-TASK") {
		t.Errorf("restart spec Resume=%v injection=%q; want a fresh session carrying the task", spec.Resume, spec.RestartInjection)
	}
}

func TestOnContextTurn_DisabledDoesNothing(t *testing.T) {
	r, tmpDir, _, handle := newContextGuardFixture(t)
	r.ctxGuard.tokens.Store(0)
	r.onContextTurn("alice", "sess-alice", 10_000_000)
	r.onContextTurn("alice", "sess-alice", 10_000_000)
	r.ctxGuard.stop()
	if p, _ := agentloop.ListPending(tmpDir, "alice"); len(p) != 0 {
		t.Errorf("disabled guard queued %d entries", len(p))
	}
	if got := handle.stopCalls.Load(); got != 0 {
		t.Errorf("disabled guard stopped the agent (%d)", got)
	}
}

// weave is asked to call handoff, once per session, and is never stopped.
func TestOnContextTurn_WeaveIsAskedOnceAndNeverRestarted(t *testing.T) {
	r, tmpDir := newFakeReal(t)
	saveTestAgent(t, tmpDir, &state.AgentState{Name: "weave", Type: "root", Status: state.StatusActive})
	r.ctxGuard.tokens.Store(1000)

	for i := 0; i < 3; i++ {
		r.onContextTurn("weave", "weave-sess", 5000)
	}
	r.ctxGuard.stop()

	p, err := agentloop.ListPending(tmpDir, "weave")
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 1 {
		t.Fatalf("weave got %d queued requests over three turns, want 1", len(p))
	}
	if !strings.Contains(p[0].Body, "handoff") {
		t.Errorf("weave request does not mention handoff: %q", p[0].Body)
	}
}
