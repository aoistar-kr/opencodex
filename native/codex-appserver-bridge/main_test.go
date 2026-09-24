package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

const (
	staleThreadID    = "01a075db-9341-70d2-b8a6-e62a00e65378"
	liveThreadID     = "01a0d41f-9c48-70b4-9b31-6d2bb4d0c1a4"
	otherThreadID    = "01a0d777-2222-7000-8000-000000000000"
	subagentThreadID = "01a0d888-3333-7000-8000-000000000000"
	unknownThreadID  = "01a0d999-4444-7000-8000-000000000000"
)

// newTestBridge builds a bridge whose backend stdin is a pipe the test drives.
// Nothing else is started: no downstream CLI, no endpoint, no goroutines.
func newTestBridge(t *testing.T) (*bridge, *bufio.Reader) {
	t.Helper()
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
	})
	b := &bridge{
		stdin:             writer,
		knownThreads:      map[string]bool{},
		backgroundThreads: map[string]bool{},
		foregroundThreads: map[string]bool{},
		pendingThread:     map[string]bool{},
		pendingOps:        map[string]threadOp{},
		turnTemplates:     map[string]map[string]any{},
		injected:          map[string]chan injectedResult{},
		connected:         true,
	}
	return b, bufio.NewReader(reader)
}

// wireLine encodes one JSON-RPC line the way the proxy sees it.
func wireLine(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	line, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal wire message: %v", err)
	}
	return append(line, '\n')
}

// appRequest feeds one outbound app-server request (app -> server).
func appRequest(t *testing.T, b *bridge, requestID, method, threadID string) {
	t.Helper()
	appRequestID(t, b, requestID, method, threadID)
}

func appRequestID(t *testing.T, b *bridge, requestID any, method, threadID string) {
	t.Helper()
	b.observeWire(wireLine(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  method,
		"params":  map[string]any{"threadId": threadID},
	}), true)
}

// backendResult feeds one successful response (server -> app).
func backendResult(t *testing.T, b *bridge, requestID string, result any) {
	t.Helper()
	backendResultID(t, b, requestID, result)
}

func backendResultID(t *testing.T, b *bridge, requestID any, result any) {
	t.Helper()
	b.observeWire(wireLine(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"result":  result,
	}), false)
}

// backendError feeds one failed response (server -> app).
func backendError(t *testing.T, b *bridge, requestID, message string) {
	t.Helper()
	b.observeWire(wireLine(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"error":   map[string]any{"code": -32600, "message": message},
	}), false)
}

func threadResult(threadID string) map[string]any {
	return map[string]any{"thread": map[string]any{"id": threadID}}
}

func threadStarted(t *testing.T, b *bridge, thread map[string]any) {
	t.Helper()
	b.observeWire(wireLine(t, map[string]any{
		"jsonrpc": "2.0",
		"method":  "thread/started",
		"params":  map[string]any{"thread": thread},
	}), false)
}

func activeThread(b *bridge) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeThreadID
}

type readOutcome struct {
	line string
	err  error
}

func readLineWithin(reader *bufio.Reader, timeout time.Duration) (string, error) {
	reads := make(chan readOutcome, 1)
	go func() {
		line, err := reader.ReadString('\n')
		reads <- readOutcome{line: line, err: err}
	}()
	select {
	case outcome := <-reads:
		return outcome.line, outcome.err
	case <-time.After(timeout):
		return "", fmt.Errorf("no backend write within %s", timeout)
	}
}

// injectAndAnswer runs force-submit, reads the request it wrote to the backend,
// answers it with the given result, and returns both the request and the reply.
func injectAndAnswer(t *testing.T, b *bridge, backend *bufio.Reader, command ipcCommand, result any) (map[string]any, ipcReply) {
	t.Helper()
	replies := make(chan ipcReply, 1)
	go func() { replies <- b.forceSubmit(command) }()

	line, err := readLineWithin(backend, 5*time.Second)
	if err != nil {
		t.Fatalf("force-submit wrote nothing to the backend: %v", err)
	}
	var injected map[string]any
	if err := json.Unmarshal([]byte(line), &injected); err != nil {
		t.Fatalf("injected request is not JSON: %v (%q)", err, line)
	}
	requestID, _ := injected["id"].(string)
	if requestID == "" {
		t.Fatalf("injected request has no id: %q", line)
	}
	backendResult(t, b, requestID, result)

	select {
	case reply := <-replies:
		return injected, reply
	case <-time.After(5 * time.Second):
		t.Fatal("force-submit did not answer after the backend answered")
		return nil, ipcReply{}
	}
}

// injectAndError is injectAndAnswer with a failed backend response.
func injectAndError(t *testing.T, b *bridge, backend *bufio.Reader, command ipcCommand, message string) (map[string]any, ipcReply) {
	t.Helper()
	replies := make(chan ipcReply, 1)
	go func() { replies <- b.forceSubmit(command) }()

	line, err := readLineWithin(backend, 5*time.Second)
	if err != nil {
		t.Fatalf("force-submit wrote nothing to the backend: %v", err)
	}
	var injected map[string]any
	if err := json.Unmarshal([]byte(line), &injected); err != nil {
		t.Fatalf("injected request is not JSON: %v (%q)", err, line)
	}
	requestID, _ := injected["id"].(string)
	if requestID == "" {
		t.Fatalf("injected request has no id: %q", line)
	}
	backendError(t, b, requestID, message)

	select {
	case reply := <-replies:
		return injected, reply
	case <-time.After(5 * time.Second):
		t.Fatal("force-submit did not answer after the backend answered")
		return nil, ipcReply{}
	}
}

// TestStaleReadThenValidReadOnlySelectsTheAnsweredThread is the production
// regression. After an app restart the app rehydrates its persisted selection
// with a read for a thread this app-server session has never seen. Selecting the
// request made force-submit inject a turn for that stale id and the backend
// rejected it with "thread not found". The thread may only be selected once the
// matching response comes back without an error.
func TestStaleReadThenValidReadOnlySelectsTheAnsweredThread(t *testing.T) {
	b, backend := newTestBridge(t)

	appRequest(t, b, "app:thread/read:1", "thread/read", staleThreadID)
	if got := activeThread(b); got != "" {
		t.Fatalf("a read request must not select the thread it names; active=%q", got)
	}
	if reply := b.forceSubmit(ipcCommand{Text: "hello"}); reply.OK || reply.Error != "no-active-thread" {
		t.Fatalf("an unconfirmed read must not be submittable; reply=%+v", reply)
	}

	backendError(t, b, "app:thread/read:1", "thread not found: "+staleThreadID)
	if got := activeThread(b); got != "" {
		t.Fatalf("a failed read must not select its thread; active=%q", got)
	}
	if reply := b.forceSubmit(ipcCommand{Text: "hello"}); reply.Error != "no-active-thread" {
		t.Fatalf("a failed read must leave the bridge refusing; reply=%+v", reply)
	}

	appRequest(t, b, "app:thread/read:2", "thread/read", liveThreadID)
	if got := activeThread(b); got != "" {
		t.Fatalf("selection must wait for the response; active=%q", got)
	}
	backendResult(t, b, "app:thread/read:2", threadResult(liveThreadID))
	if got := activeThread(b); got != liveThreadID {
		t.Fatalf("the answered read should select %q; active=%q", liveThreadID, got)
	}

	// End to end: the injected turn has to name the confirmed thread.
	injected, reply := injectAndAnswer(t, b, backend, ipcCommand{Text: "hello"},
		map[string]any{"turn": map[string]any{"id": "turn-1"}})
	if injected["method"] != "turn/start" {
		t.Fatalf("injected method = %v, want turn/start", injected["method"])
	}
	params, _ := injected["params"].(map[string]any)
	if got, _ := params["threadId"].(string); got != liveThreadID {
		t.Fatalf("injected threadId = %q, want %q", got, liveThreadID)
	}
	if !reply.OK || reply.ThreadID != liveThreadID || reply.TurnID != "turn-1" {
		t.Fatalf("force-submit reply = %+v", reply)
	}
}

// TestExplicitThreadIDMustBeConfirmed covers the client contract: an explicit id
// never falls back to the tracked selection, and it is refused until this
// app-server session has answered for it. A turn confirms a thread without
// selecting it.
func TestExplicitThreadIDMustBeConfirmed(t *testing.T) {
	b, backend := newTestBridge(t)

	reply := b.forceSubmit(ipcCommand{Text: "hello", ThreadID: staleThreadID})
	if reply.OK || reply.Error != "no-active-thread" || reply.ThreadID != "" {
		t.Fatalf("an unconfirmed explicit id must be refused without a fallback; reply=%+v", reply)
	}

	appRequest(t, b, "app:turn/start:1", "turn/start", otherThreadID)
	if reply := b.forceSubmit(ipcCommand{Text: "hello", ThreadID: otherThreadID}); reply.Error != "no-active-thread" {
		t.Fatalf("a turn request alone must not confirm a thread; reply=%+v", reply)
	}
	backendResult(t, b, "app:turn/start:1", map[string]any{"turn": map[string]any{"id": "turn-9"}})
	if got := activeThread(b); got != "" {
		t.Fatalf("a turn must not become the selection; active=%q", got)
	}

	_, accepted := injectAndAnswer(t, b, backend, ipcCommand{Text: "hello", ThreadID: otherThreadID},
		map[string]any{"turn": map[string]any{"id": "turn-10"}})
	if !accepted.OK || accepted.ThreadID != otherThreadID {
		t.Fatalf("the answered turn should confirm %q; reply=%+v", otherThreadID, accepted)
	}
}

func TestBackendAcceptedCandidateRequiresSuccessfulForegroundAppTurn(t *testing.T) {
	b, backend := newTestBridge(t)

	threadStarted(t, b, map[string]any{"id": unknownThreadID, "source": "unknown"})
	appRequest(t, b, "app:turn/start:unknown", "turn/start", unknownThreadID)
	backendResult(t, b, "app:turn/start:unknown", map[string]any{"turn": map[string]any{"id": "turn-unknown"}})
	if got := b.status(ipcCommand{}).AcceptedThreadID; got != "" {
		t.Fatalf("unknown source became accepted candidate %q", got)
	}

	// Real Codex Desktop session metadata uses source=vscode for a user-owned
	// root thread; user and cli are the other explicit foreground allowlist rows.
	threadStarted(t, b, map[string]any{"id": liveThreadID, "source": "vscode"})
	appRequest(t, b, "app:turn/start:root", "turn/start", liveThreadID)
	backendResult(t, b, "app:turn/start:root", map[string]any{"turn": map[string]any{"id": "turn-root"}})
	if got := b.status(ipcCommand{}).AcceptedThreadID; got != liveThreadID {
		t.Fatalf("successful foreground app turn accepted = %q, want %q", got, liveThreadID)
	}

	// A successful injected turn is not an app-originated acceptance signal.
	threadStarted(t, b, map[string]any{"id": otherThreadID, "source": "user"})
	_, reply := injectAndAnswer(t, b, backend, ipcCommand{Text: "injected", ThreadID: otherThreadID},
		map[string]any{"turn": map[string]any{"id": "turn-injected"}})
	if !reply.OK {
		t.Fatalf("injected control turn failed: %+v", reply)
	}
	if got := b.status(ipcCommand{}).AcceptedThreadID; got != liveThreadID {
		t.Fatalf("injected turn changed accepted candidate to %q", got)
	}

	// A subagent app turn can succeed, but its source metadata prevents it from
	// replacing the root candidate.
	threadStarted(t, b, map[string]any{
		"id": subagentThreadID,
		"source": map[string]any{
			"subAgent": map[string]any{
				"thread_spawn": map[string]any{"parent_thread_id": liveThreadID},
			},
		},
	})
	appRequest(t, b, "app:turn/start:subagent", "turn/start", subagentThreadID)
	backendResult(t, b, "app:turn/start:subagent", map[string]any{"turn": map[string]any{"id": "turn-subagent"}})
	if got := b.status(ipcCommand{}).AcceptedThreadID; got != liveThreadID {
		t.Fatalf("subagent turn changed accepted candidate to %q", got)
	}

	// If the downstream later says the accepted root no longer exists, the
	// candidate is retired with the rest of that thread's bridge state.
	appRequest(t, b, "app:turn/start:missing", "turn/start", liveThreadID)
	backendError(t, b, "app:turn/start:missing", "thread "+liveThreadID+" not found")
	if got := b.status(ipcCommand{}).AcceptedThreadID; got != "" {
		t.Fatalf("missing accepted thread remained exposed as %q", got)
	}
}

func TestAcceptedCandidateDoesNotMoveBackOnReverseResponses(t *testing.T) {
	b, _ := newTestBridge(t)
	threadStarted(t, b, map[string]any{"id": liveThreadID, "source": "vscode"})
	threadStarted(t, b, map[string]any{"id": otherThreadID, "source": "user"})

	appRequest(t, b, "app:turn:start:older", "turn/start", liveThreadID)
	appRequest(t, b, "app:turn:start:newer", "turn/start", otherThreadID)
	backendResult(t, b, "app:turn:start:newer", map[string]any{"turn": map[string]any{"id": "newer"}})
	backendResult(t, b, "app:turn:start:older", map[string]any{"turn": map[string]any{"id": "older"}})

	if got := b.status(ipcCommand{}).AcceptedThreadID; got != otherThreadID {
		t.Fatalf("late older response moved accepted candidate to %q, want %q", got, otherThreadID)
	}
}

func TestCanonicalIDsAndInjectedCollisionIsolation(t *testing.T) {
	if idKey(json.RawMessage(`1`)) == idKey(json.RawMessage(`"1"`)) {
		t.Fatal("numeric id 1 and string id \"1\" canonicalized to the same key")
	}

	b, backend := newTestBridge(t)
	threadStarted(t, b, map[string]any{"id": liveThreadID, "source": "vscode"})
	appRequestID(t, b, 1, "turn/start", liveThreadID)
	appRequestID(t, b, "1", "turn/start", liveThreadID)
	backendResultID(t, b, "1", map[string]any{"turn": map[string]any{"id": "string"}})
	b.mu.Lock()
	_, numericStillPending := b.pendingOps["n:1"]
	b.mu.Unlock()
	if !numericStillPending {
		t.Fatal("string response consumed numeric request id 1")
	}
	backendResultID(t, b, 1, map[string]any{"turn": map[string]any{"id": "number"}})

	collisionID := fmt.Sprintf("ocx-force-submit-%d-1", os.Getpid())
	b.mu.Lock()
	b.pendingOps["s:"+collisionID] = threadOp{threadID: unknownThreadID, accepts: true, acceptSeq: 999}
	b.mu.Unlock()
	injected, reply := injectAndAnswer(t, b, backend, ipcCommand{Text: "collision", ThreadID: liveThreadID},
		map[string]any{"turn": map[string]any{"id": "injected"}})
	if !reply.OK || injected["id"] == collisionID {
		t.Fatalf("injected id did not avoid app pending collision; id=%v reply=%+v", injected["id"], reply)
	}
	b.mu.Lock()
	_, appOpSurvived := b.pendingOps["s:"+collisionID]
	b.mu.Unlock()
	if !appOpSurvived {
		t.Fatal("injected response consumed or confirmed the colliding app operation")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("forced write failure") }
func (failingWriter) Close() error              { return nil }

func TestPendingStateRollsBackOnWriteFailureAndDisconnect(t *testing.T) {
	b, _ := newTestBridge(t)
	b.stdin = failingWriter{}
	raw := wireLine(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      "write-fails",
		"method":  "turn/start",
		"params":  map[string]any{"threadId": liveThreadID},
	})
	b.onAppLine(raw)
	b.mu.Lock()
	remainingAfterWrite := len(b.pendingOps) + len(b.pendingThread)
	b.mu.Unlock()
	if remainingAfterWrite != 0 {
		t.Fatalf("write failure left %d pending entries", remainingAfterWrite)
	}

	waiter := make(chan injectedResult, 1)
	b.mu.Lock()
	b.pendingOps["s:op"] = threadOp{threadID: liveThreadID}
	b.pendingThread["s:thread"] = true
	b.injected["s:injected"] = waiter
	b.mu.Unlock()
	b.markDisconnected()
	b.mu.Lock()
	remainingAfterDisconnect := len(b.pendingOps) + len(b.pendingThread) + len(b.injected)
	connected := b.connected
	b.mu.Unlock()
	if connected || remainingAfterDisconnect != 0 {
		t.Fatalf("disconnect left connected=%v pending=%d", connected, remainingAfterDisconnect)
	}
	select {
	case result := <-waiter:
		if got := errorMessage(result.errMsg); got != "backend disconnected (code -32000)" {
			t.Fatalf("disconnect waiter error = %q", got)
		}
	default:
		t.Fatal("disconnect did not release injected waiter")
	}
}

// TestAuxiliaryReadFailureKeepsTheSelection covers the other half of the rule: a
// thread that is already confirmed stays selected when a later read fails,
// because that failure says nothing about the answer that confirmed it. A
// brand-new thread answers thread/turns/list with "not materialized yet", and
// clearing the selection there would break the composer for a live thread.
func TestAuxiliaryReadFailureKeepsTheSelection(t *testing.T) {
	b, _ := newTestBridge(t)

	appRequest(t, b, "app:thread/turns/list:1", "thread/turns/list", liveThreadID)
	backendResult(t, b, "app:thread/turns/list:1", map[string]any{"data": []any{}, "nextCursor": nil})
	if got := activeThread(b); got != liveThreadID {
		t.Fatalf("a successful turns/list should select %q; active=%q", liveThreadID, got)
	}

	appRequest(t, b, "app:thread/turns/list:2", "thread/turns/list", liveThreadID)
	backendError(t, b, "app:thread/turns/list:2", "thread "+liveThreadID+" is not materialized yet")
	if got := activeThread(b); got != liveThreadID {
		t.Fatalf("a failed auxiliary read must not clear a confirmed selection; active=%q", got)
	}

	appRequest(t, b, "app:thread/read:3", "thread/read", staleThreadID)
	backendError(t, b, "app:thread/read:3", "thread not found: "+staleThreadID)
	if got := activeThread(b); got != liveThreadID {
		t.Fatalf("a failed read of another thread must not clear the selection; active=%q", got)
	}
}

// TestLateReadResponseDoesNotMoveTheSelectionBack keeps a slow hydration read
// from overriding the thread the operator selected afterwards.
func TestLateReadResponseDoesNotMoveTheSelectionBack(t *testing.T) {
	b, _ := newTestBridge(t)

	appRequest(t, b, "app:thread/read:1", "thread/read", liveThreadID)
	appRequest(t, b, "app:thread/read:2", "thread/read", otherThreadID)
	backendResult(t, b, "app:thread/read:2", threadResult(otherThreadID))
	if got := activeThread(b); got != otherThreadID {
		t.Fatalf("the newer read should select %q; active=%q", otherThreadID, got)
	}

	backendResult(t, b, "app:thread/read:1", threadResult(liveThreadID))
	if got := activeThread(b); got != otherThreadID {
		t.Fatalf("a late older response must not move the selection; active=%q", got)
	}

	// A read issued after that selection still wins.
	appRequest(t, b, "app:thread/read:3", "thread/read", liveThreadID)
	backendResult(t, b, "app:thread/read:3", threadResult(liveThreadID))
	if got := activeThread(b); got != liveThreadID {
		t.Fatalf("a read newer than the selection should select %q; active=%q", liveThreadID, got)
	}
}

func TestLateSameIDReadReclassifiesAcceptedThreadWithoutBypassingReadOrder(t *testing.T) {
	b, _ := newTestBridge(t)
	threadStarted(t, b, map[string]any{"id": liveThreadID, "source": "vscode"})
	appRequest(t, b, "app:turn:start", "turn/start", liveThreadID)
	backendResult(t, b, "app:turn:start", map[string]any{"turn": map[string]any{"id": "turn-root"}})
	if got := b.status(ipcCommand{}).AcceptedThreadID; got != liveThreadID {
		t.Fatalf("accepted candidate = %q, want %q", got, liveThreadID)
	}

	appRequest(t, b, "app:thread/read:older", "thread/read", liveThreadID)
	appRequest(t, b, "app:thread/read:newer", "thread/read", liveThreadID)
	backendResult(t, b, "app:thread/read:newer", map[string]any{
		"thread": map[string]any{"id": liveThreadID, "source": "vscode"},
	})
	backendResult(t, b, "app:thread/read:older", map[string]any{
		"thread": map[string]any{"id": liveThreadID, "ephemeral": true},
	})

	status := b.status(ipcCommand{})
	if status.AcceptedThreadID != "" || status.ThreadID != "" {
		t.Fatalf("late same-id ephemeral metadata was ignored; status=%+v", status)
	}
	b.mu.Lock()
	background := b.backgroundThreads[liveThreadID]
	b.mu.Unlock()
	if !background {
		t.Fatal("same-id read did not reclassify the thread as background")
	}
}

// TestParseForceSubmitArgs pins the CLI contract the caller script relies on:
// the flag and its value leave the message text, wherever they sit.
func TestParseForceSubmitArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		text     string
		threadID string
	}{
		{"no flag", []string{"hello", "world"}, "hello world", ""},
		{"flag last", []string{"hello", "world", "--ocx-thread-id", staleThreadID}, "hello world", staleThreadID},
		{"flag first", []string{"--ocx-thread-id", staleThreadID, "hello", "world"}, "hello world", staleThreadID},
		{"flag without value", []string{"hello", "--ocx-thread-id"}, "hello", ""},
		{"single token", []string{"hello"}, "hello", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			text, threadID := parseForceSubmitArgs(testCase.args)
			if text != testCase.text || threadID != testCase.threadID {
				t.Fatalf("parseForceSubmitArgs(%q) = (%q, %q), want (%q, %q)",
					testCase.args, text, threadID, testCase.text, testCase.threadID)
			}
		})
	}
}

// TestRejectedTurnEvictsTheDeadThreadAndBlocksFurtherSubmits covers the recovery
// path for a confirmed thread the backend then refuses. When an injected turn
// comes back with the backend saying it cannot resolve the thread, that id is
// dead for this app-server session. The bridge has to forget it and drop the
// selection, so the next submission is refused here instead of injecting another
// turn the backend would reject the same way.
func TestRejectedTurnEvictsTheDeadThreadAndBlocksFurtherSubmits(t *testing.T) {
	b, backend := newTestBridge(t)

	appRequest(t, b, "app:thread/read:1", "thread/read", liveThreadID)
	backendResult(t, b, "app:thread/read:1", threadResult(liveThreadID))
	if got := activeThread(b); got != liveThreadID {
		t.Fatalf("the answered read should select %q; active=%q", liveThreadID, got)
	}

	_, rejected := injectAndError(t, b, backend, ipcCommand{Text: "hello"},
		"thread "+liveThreadID+" not found")
	if rejected.OK || rejected.Error != "backend-rejected" {
		t.Fatalf("a turn the backend cannot resolve must report backend-rejected; reply=%+v", rejected)
	}

	if got := activeThread(b); got != "" {
		t.Fatalf("the rejected thread must be evicted and the selection cleared; active=%q", got)
	}
	b.mu.Lock()
	stillKnown := b.knownThreads[liveThreadID]
	b.mu.Unlock()
	if stillKnown {
		t.Fatalf("the rejected thread must no longer count as known")
	}

	// Implicit submit: the tracked selection is gone, so it is refused locally.
	if reply := b.forceSubmit(ipcCommand{Text: "again"}); reply.OK || reply.Error != "no-active-thread" {
		t.Fatalf("the next implicit submit must be refused locally; reply=%+v", reply)
	}
	// Explicit submit: the id was evicted, so it is refused with no fallback.
	if reply := b.forceSubmit(ipcCommand{Text: "again", ThreadID: liveThreadID}); reply.OK ||
		reply.Error != "no-active-thread" || reply.ThreadID != "" {
		t.Fatalf("the next explicit submit must be refused locally; reply=%+v", reply)
	}

	// Neither refusal may have written a turn/start to the backend.
	if line, err := readLineWithin(backend, 300*time.Millisecond); err == nil {
		t.Fatalf("a locally-refused submit must not reach the backend; wrote %q", line)
	}
}
