// Command ocx-codex-appserver-bridge is a transparent stdio proxy in front of
// the Codex app-server CLI.
//
// On Windows the Codex desktop app always starts its app-server as a child
// process and talks newline-delimited JSON-RPC over the child's stdin/stdout.
// Pointing CODEX_CLI_PATH at this executable inserts the bridge in that path
// without touching app.asar: the bridge spawns the real Codex CLI with the same
// arguments, relays every line in both directions unchanged, and exposes a
// user-only local socket that accepts a force-submit command. The force-submit
// injects a turn/start request for the thread the app is currently using, which
// is what lets the composer submit while the app itself has its send button
// disabled.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	stateDirName   = "codex-appserver-bridge"
	socketName     = "bridge.sock"
	descriptorName = "bridge.json"

	// The installer records the CLI that CODEX_CLI_PATH pointed at before it was
	// repointed at the bridge. See resolveDownstreamCLI.
	downstreamFileName = "downstream-cli.txt"

	forceSubmitTimeout = 10 * time.Second
	maxIPCCommandBytes = 8 << 20

	// Structured usage signal, read from the app-server's own rate-limit
	// traffic. See the "Structured usage signal" section below.
	usageUpdatedMethod   = "account/rateLimits/updated"
	usageRateLimitMethod = "account/rateLimits/read"
	usageSnapshotTTL     = 10 * time.Minute
	usageTurnTTL         = 30 * time.Minute
	usageFreshWindow     = 2 * time.Second
	usageReadTimeout     = 4 * time.Second
)

// debugEnabled traces every parsed line on stderr. Off unless asked for, because
// stderr is forwarded straight into the app's own log.
var debugEnabled = strings.TrimSpace(os.Getenv("OCX_CODEX_BRIDGE_DEBUG")) != ""

// localConn is one accepted client on the platform's local endpoint.
type localConn interface {
	io.Reader
	io.Writer
	io.Closer
}

// localListener hands out one localConn per accepted client.
type localListener interface {
	Accept() (localConn, error)
	Close() error
}

func main() {
	args := os.Args[1:]
	switch {
	case len(args) > 0 && args[0] == "--ocx-endpoint":
		fmt.Println(endpointName())
		return
	case len(args) > 0 && args[0] == "--ocx-status":
		os.Exit(runClient(ipcCommand{Type: "status"}))
	case len(args) > 0 && args[0] == "--ocx-usage":
		os.Exit(runClient(ipcCommand{Type: "usage"}))
	case len(args) > 0 && args[0] == "--ocx-force-submit":
		text, threadID, modelLabel := parseForceSubmitOptions(args[1:])
		os.Exit(runClient(ipcCommand{Type: "force-submit", Text: text, ThreadID: threadID, ModelLabel: modelLabel}))
	}
	os.Exit(runProxy(args))
}

// parseForceSubmitArgs splits the force-submit arguments into the message text
// and the optional --ocx-thread-id flag, which names the thread to submit into
// instead of the tracked selection. The flag is read anywhere in the argument
// list; a message that needs the literal text "--ocx-thread-id" can put it after
// the flag and its value.
func parseForceSubmitArgs(args []string) (string, string) {
	text, threadID, _ := parseForceSubmitOptions(args)
	return text, threadID
}

func parseForceSubmitOptions(args []string) (string, string, string) {
	threadID := ""
	modelLabel := ""
	text := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] == "--ocx-thread-id" {
			// The flag and its value never reach the message text. A caller that
			// built the command line from an empty thread id leaves the flag with
			// no value; consuming it keeps that from polluting the prompt.
			if index+1 < len(args) {
				threadID = args[index+1]
				index++
			}
			continue
		}
		if args[index] == "--ocx-model-label" {
			if index+1 < len(args) {
				modelLabel = args[index+1]
				index++
			}
			continue
		}
		text = append(text, args[index])
	}
	return strings.Join(text, " "), threadID, modelLabel
}

// stateDir is the user-only directory that holds the socket and descriptor.
// %LOCALAPPDATA% already grants access to the owning user only, so nothing here
// needs its own ACL.
func stateDir() string {
	if dir := strings.TrimSpace(os.Getenv("OCX_CODEX_BRIDGE_DIR")); dir != "" {
		return dir
	}
	if runtime.GOOS == "windows" {
		if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
			return filepath.Join(local, "OpenCodex", stateDirName)
		}
	}
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		return filepath.Join(runtimeDir, "opencodex", stateDirName)
	}
	return filepath.Join(os.TempDir(), "opencodex-"+stateDirName)
}

// resolveDownstreamCLI finds the executable the bridge should chain to.
//
// The app spawns the bridge because CODEX_CLI_PATH points at it, so the previous
// value of that variable cannot be read back from inside the bridge: the
// installer has to record it. Anything already wrapping the real CLI has to be
// preserved, which is why the installer-provided variable wins over the bundled
// binary. The order is:
//
//  1. OCX_CODEX_DOWNSTREAM_CLI - the CLI that was in CODEX_CLI_PATH before
//  2. OCX_CODEX_REAL_CLI - the earlier name for the same thing
//  3. the state file the installer writes next to the socket
//  4. the CLI the app itself relocates into %LOCALAPPDATA%/OpenAI/Codex/bin
func resolveDownstreamCLI() (string, string, error) {
	if p := strings.TrimSpace(os.Getenv("OCX_CODEX_DOWNSTREAM_CLI")); p != "" {
		if !isFile(p) {
			return "", "", fmt.Errorf("OCX_CODEX_DOWNSTREAM_CLI does not name a file: %s", p)
		}
		return p, "OCX_CODEX_DOWNSTREAM_CLI", nil
	}
	if p := strings.TrimSpace(os.Getenv("OCX_CODEX_REAL_CLI")); p != "" {
		if !isFile(p) {
			return "", "", fmt.Errorf("OCX_CODEX_REAL_CLI does not name a file: %s", p)
		}
		return p, "OCX_CODEX_REAL_CLI", nil
	}
	stateFile := filepath.Join(stateDir(), downstreamFileName)
	if data, err := os.ReadFile(stateFile); err == nil {
		if p := strings.TrimSpace(string(data)); p != "" && isFile(p) {
			return p, stateFile, nil
		}
	}
	exeName := "codex"
	if runtime.GOOS == "windows" {
		exeName = "codex.exe"
	}
	local := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if local != "" {
		base := filepath.Join(local, "OpenAI", "Codex", "bin")
		direct := filepath.Join(base, exeName)
		if isFile(direct) {
			return direct, "bundled", nil
		}
		if entries, err := os.ReadDir(base); err == nil {
			newest := ""
			var newestMod time.Time
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				candidate := filepath.Join(base, entry.Name(), exeName)
				info, err := os.Stat(candidate)
				if err != nil || !info.Mode().IsRegular() {
					continue
				}
				if newest == "" || info.ModTime().After(newestMod) {
					newest, newestMod = candidate, info.ModTime()
				}
			}
			if newest != "" {
				return newest, "bundled", nil
			}
		}
	}
	return "", "", fmt.Errorf("could not locate a downstream Codex CLI; set OCX_CODEX_DOWNSTREAM_CLI to its full path")
}

// isSelf guards against a CODEX_CLI_PATH that still names the bridge, which
// would make it spawn itself without bound.
func isSelf(path string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.EqualFold(filepath.Clean(mustAbs(path)), filepath.Clean(mustAbs(self)))
}

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

// childEnv rebuilds the environment the app would have handed the downstream CLI
// if the bridge were not installed.
//
// Two entries have to be repaired. CODEX_CLI_PATH currently names the bridge,
// because that is how the app was pointed here; a downstream wrapper that reads
// it would spawn the bridge again. It is set back to the downstream CLI, which is
// exactly what that wrapper saw before the bridge existed. PATH is prefixed with
// the downstream CLI's directory, which is what the app's own resolution would
// have added.
//
// OCX_CODEX_DOWNSTREAM_CLI is left untouched: the downstream chain needs it to
// keep resolving the same CLI on any further hop.
func childEnv(downstream string) []string {
	env := os.Environ()
	dir := filepath.Dir(downstream)
	hasPath := false
	hasCLIPath := false
	for i, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		switch {
		case strings.EqualFold(name, "CODEX_CLI_PATH"):
			hasCLIPath = true
			env[i] = name + "=" + downstream
		case strings.EqualFold(name, "PATH"):
			hasPath = true
			if dir != "" && dir != "." {
				env[i] = name + "=" + dir + string(os.PathListSeparator) + value
			}
		}
	}
	if !hasCLIPath {
		env = append(env, "CODEX_CLI_PATH="+downstream)
	}
	if !hasPath && dir != "" && dir != "." {
		env = append(env, "PATH="+dir)
	}
	return env
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func runProxy(args []string) int {
	downstream, downstreamSource, err := resolveDownstreamCLI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: %v\n", err)
		return 127
	}
	if isSelf(downstream) {
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: refusing to chain to itself (%s); set OCX_CODEX_DOWNSTREAM_CLI to the CLI that CODEX_CLI_PATH used to name\n", downstream)
		return 127
	}

	cmd := exec.Command(downstream, args...)
	cmd.Env = childEnv(downstream)
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: stdin pipe: %v\n", err)
		return 127
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: stdout pipe: %v\n", err)
		return 127
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: stderr pipe: %v\n", err)
		return 127
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: could not start %s: %v\n", downstream, err)
		return 127
	}
	// The app terminates this process with TerminateProcess when it closes or
	// restarts the connection, which would otherwise orphan the real app-server.
	if err := attachKillOnCloseJob(cmd.Process.Pid); err != nil {
		// Continuing here would leave an app-server behind on every app restart,
		// holding the state database. Refuse to run without the guarantee.
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: cannot guarantee the app-server is reaped with this process: %v\n", err)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 127
	}

	b := &bridge{
		stdin:             stdin,
		knownThreads:      map[string]bool{},
		backgroundThreads: map[string]bool{},
		foregroundThreads: map[string]bool{},
		pendingThread:     map[string]bool{},
		pendingOps:        map[string]threadOp{},
		turnTemplates:     map[string]map[string]any{},
		modelCatalog:      map[string]modelCatalogEntry{},
		pendingModelLists: map[string]bool{},
		injected:          map[string]chan injectedResult{},
		pendingUsage:      map[string]chan injectedResult{},
		appServerPid:      cmd.Process.Pid,
		downstreamCLIPath: downstream,
		downstreamSource:  downstreamSource,
		startedAt:         time.Now(),
		connected:         true,
	}

	go func() { _, _ = io.Copy(os.Stderr, stderr) }()
	go b.serveIPC()

	backendDone := make(chan struct{})
	go func() {
		pumpLines(stdout, b.onBackendLine)
		b.markDisconnected()
		close(backendDone)
	}()
	go func() {
		pumpLines(os.Stdin, b.onAppLine)
		// The app closed our stdin; the app-server treats EOF as shutdown.
		_ = stdin.Close()
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		_ = cmd.Process.Kill()
	}()

	// Wait for the backend stdout to drain before reaping, so the app never
	// sees a truncated final line.
	<-backendDone
	waitErr := cmd.Wait()

	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	if waitErr != nil {
		return 1
	}
	return 0
}

// pumpLines hands each raw line (including its terminator) to handle. Lines can
// be megabytes, so this deliberately avoids bufio.Scanner's token limit.
func pumpLines(r io.Reader, handle func([]byte)) {
	reader := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			handle(line)
		}
		if err != nil {
			return
		}
	}
}

type injectedResult struct {
	result json.RawMessage
	errMsg json.RawMessage
}

// threadOp is one outbound request that named a thread. The request alone is
// not evidence that the app-server knows the thread, so the entry is held until
// the matching response arrives: only a successful response promotes it.
type threadOp struct {
	threadID string
	// seq orders selection-bearing reads. A response older than the current
	// selection is ignored, so a slow hydration read cannot move the selection
	// back to a thread the operator already left. Zero means "not a selection".
	seq int
	// selects is true for reads and resumes, which are selection signals, and
	// false for turns, which only make a thread known.
	selects bool
	// accepts is true only for an app-originated turn/start. Injected turns are
	// written directly to the backend and never create a threadOp.
	accepts bool
	// acceptSeq orders app-originated turn/start requests independently of
	// selection reads. A late response cannot replace a newer accepted root.
	acceptSeq int
}

type bridge struct {
	stdin   io.WriteCloser
	stdinMu sync.Mutex

	mu                sync.Mutex
	activeThreadID    string
	acceptedThreadID  string
	knownThreads      map[string]bool
	backgroundThreads map[string]bool
	foregroundThreads map[string]bool
	pendingThread     map[string]bool
	pendingOps        map[string]threadOp
	turnTemplates     map[string]map[string]any
	modelCatalog      map[string]modelCatalogEntry
	pendingModelLists map[string]bool
	lastTurnID        string
	injected          map[string]chan injectedResult
	// pendingUsage holds account/rateLimits/read requests the bridge is still
	// waiting on. The value is nil for a read the app issued, and a waiter for
	// a read the bridge issued itself.
	pendingUsage      map[string]chan injectedResult
	usageReadInFlight bool
	usageState        usageSnapshot
	usageBlock        usageBlock
	usageTurnBlock    usageBlock
	seq               int
	appTurnSeq        int
	acceptedSeq       int
	readSeq           int
	selectionSeq      int
	injectedTurns     int
	appServerPid      int
	downstreamCLIPath string
	downstreamSource  string
	startedAt         time.Time
	connected         bool
}

func (b *bridge) markDisconnected() {
	b.mu.Lock()
	b.connected = false
	for requestID := range b.pendingThread {
		delete(b.pendingThread, requestID)
	}
	for requestID := range b.pendingOps {
		delete(b.pendingOps, requestID)
	}
	for requestID := range b.pendingModelLists {
		delete(b.pendingModelLists, requestID)
	}
	usageWaiters := make([]chan injectedResult, 0, len(b.pendingUsage))
	for requestID := range b.pendingUsage {
		// A nil entry is an app-issued read: it has no waiter to wake.
		waiter := b.pendingUsage[requestID]
		if waiter != nil {
			usageWaiters = append(usageWaiters, waiter)
		}
		delete(b.pendingUsage, requestID)
	}
	b.usageReadInFlight = false
	// The cached usage belongs to the connection that is gone. Keeping it would report a
	// limit the next session has not confirmed.
	b.usageState = usageSnapshot{}
	b.usageBlock = usageBlock{}
	b.usageTurnBlock = usageBlock{}
	waiters := make([]chan injectedResult, 0, len(b.injected))
	for requestID, waiter := range b.injected {
		waiters = append(waiters, waiter)
		delete(b.injected, requestID)
	}
	b.mu.Unlock()

	errMsg := json.RawMessage(`{"code":-32000,"message":"backend disconnected"}`)
	for _, waiter := range append(waiters, usageWaiters...) {
		waiter <- injectedResult{errMsg: errMsg}
	}
}

func (b *bridge) onAppLine(raw []byte) {
	b.observeWire(raw, true)
	if err := b.writeBackend(raw); err != nil {
		b.rollbackAppPending(raw)
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: app-server stdin: %v\n", err)
	}
}

func (b *bridge) rollbackAppPending(raw []byte) {
	var message wireMessage
	if json.Unmarshal(bytes.TrimSpace(raw), &message) != nil {
		return
	}
	requestID := idKey(message.ID)
	if requestID == "" {
		return
	}
	b.mu.Lock()
	delete(b.pendingThread, requestID)
	delete(b.pendingModelLists, requestID)
	delete(b.pendingOps, requestID)
	delete(b.pendingUsage, requestID)
	b.mu.Unlock()
}

func (b *bridge) onBackendLine(raw []byte) {
	b.observeWire(raw, false)
	if _, err := os.Stdout.Write(raw); err != nil {
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: stdout: %v\n", err)
	}
}

func (b *bridge) writeBackend(line []byte) error {
	b.stdinMu.Lock()
	defer b.stdinMu.Unlock()
	_, err := b.stdin.Write(line)
	return err
}

type wireMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// modelCatalogEntry mirrors the fields the desktop model picker receives from
// model/list. turn/start expects the entry's model slug (with id as a legacy
// fallback), not the display label.
type modelCatalogEntry struct {
	ID                        string   `json:"id"`
	Model                     string   `json:"model"`
	DisplayName               string   `json:"displayName"`
	SupportedReasoningEfforts []string `json:"supportedReasoningEfforts"`
	DefaultReasoningEffort    string   `json:"defaultReasoningEffort"`
}

// The v2 schema advertises supportedReasoningEfforts as objects with a
// reasoningEffort member, while older app-server builds returned strings.
// Accept both wire shapes so a model/list response always populates the cache.
func (entry *modelCatalogEntry) UnmarshalJSON(raw []byte) error {
	var payload struct {
		ID                        string          `json:"id"`
		Model                     string          `json:"model"`
		DisplayName               string          `json:"displayName"`
		SupportedReasoningEfforts json.RawMessage `json:"supportedReasoningEfforts"`
		DefaultReasoningEffort    string          `json:"defaultReasoningEffort"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	entry.ID, entry.Model, entry.DisplayName, entry.DefaultReasoningEffort = payload.ID, payload.Model, payload.DisplayName, payload.DefaultReasoningEffort
	entry.SupportedReasoningEfforts = nil
	var stringsOnly []string
	if json.Unmarshal(payload.SupportedReasoningEfforts, &stringsOnly) == nil {
		entry.SupportedReasoningEfforts = stringsOnly
		return nil
	}
	var options []struct {
		ReasoningEffort string `json:"reasoningEffort"`
	}
	if json.Unmarshal(payload.SupportedReasoningEfforts, &options) == nil {
		for _, option := range options {
			if value := strings.TrimSpace(option.ReasoningEffort); value != "" {
				entry.SupportedReasoningEfforts = append(entry.SupportedReasoningEfforts, value)
			}
		}
	}
	return nil
}

func (b *bridge) observeWire(raw []byte, fromApp bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return
	}
	var message wireMessage
	if json.Unmarshal(trimmed, &message) != nil {
		return
	}
	if debugEnabled {
		direction := "app->server"
		if !fromApp {
			direction = "server->app"
		}
		fmt.Fprintf(os.Stderr, "ocx-bridge debug: %s method=%q id=%q\n", direction, message.Method, idKey(message.ID))
	}
	if fromApp {
		b.observeAppMessage(&message)
		return
	}
	b.observeBackendMessage(&message)
}

// observeAppMessage tracks which thread the UI has selected.
//
// Only thread-specific reads, resumes, and new-thread creation count. A
// turn/start is deliberately not a selection signal: the app issues turns for
// subagent threads too, so treating one as a selection would submit the
// operator's message into a thread the operator is not looking at. When no
// selection has been observed the bridge reports no-active-thread instead of
// guessing.
//
// A request only names a thread; it is not proof the app-server knows it. After
// a restart the app rehydrates its last selection from its own persisted state,
// so the first read can name a thread this app-server session has never seen.
// Promoting on the request made that stale id the selection, and force-submit
// then injected a turn the backend rejected with "thread not found". The thread
// is held as a candidate keyed by request id and promoted only when the matching
// response comes back without an error.
func (b *bridge) observeAppMessage(message *wireMessage) {
	if message.Method == "" {
		return
	}
	requestID := idKey(message.ID)
	threadID := paramThreadID(message.Params)

	b.mu.Lock()
	defer b.mu.Unlock()
	// An injected id owns its response slot until completion. Even if a broken
	// client later reuses that exact string id, its request must not create an
	// app operation that the injected response could accidentally confirm.
	if requestID != "" {
		if _, injectedID := b.injected[requestID]; injectedID {
			return
		}
		// A usage read the bridge issued owns its id the same way: an app request that
		// reused it must not be able to consume the injected response.
		if _, usageID := b.pendingUsage[requestID]; usageID {
			return
		}
	}
	switch message.Method {
	case "model/list":
		if requestID != "" {
			if b.pendingModelLists == nil {
				b.pendingModelLists = map[string]bool{}
			}
			b.pendingModelLists[requestID] = true
		}
	case "thread/resume", "thread/read", "thread/items/list", "thread/turns/list":
		if threadID == "" || requestID == "" {
			return
		}
		b.readSeq++
		b.pendingOps[requestID] = threadOp{threadID: threadID, seq: b.readSeq, selects: true}
	case "thread/start", "thread/startAeon", "thread/fork":
		// The new thread id only exists in the response. thread/prewarm is a
		// different method and is intentionally not tracked: a prewarmed thread
		// is not the thread the operator selected.
		if requestID != "" {
			b.pendingThread[requestID] = true
		}
	case "thread/unsubscribe":
		if threadID != "" && threadID == b.activeThreadID {
			b.activeThreadID = ""
		}
	case "turn/start", "turn/steer", "turn/addUserMessage":
		if threadID == "" {
			return
		}
		// A turn shows the app is driving the thread, but the id is still only
		// confirmed once the app-server answers the request.
		if requestID != "" {
			acceptSeq := 0
			if message.Method == "turn/start" {
				b.appTurnSeq++
				acceptSeq = b.appTurnSeq
			}
			b.pendingOps[requestID] = threadOp{
				threadID:  threadID,
				accepts:   message.Method == "turn/start",
				acceptSeq: acceptSeq,
			}
		}
		// Only the selected thread's parameters may be reused for an injection.
		if message.Method == "turn/start" && threadID == b.activeThreadID {
			if template := turnTemplate(message.Params); template != nil {
				b.turnTemplates[threadID] = template
			}
		}
	case usageRateLimitMethod:
		// The app polls its own usage. Tracking the request lets its response be
		// parsed without the bridge injecting a read of its own.
		if requestID == "" {
			return
		}
		if b.pendingUsage == nil {
			b.pendingUsage = map[string]chan injectedResult{}
		}
		b.pendingUsage[requestID] = nil
	}
}

// confirmSelectionLocked records a thread the app-server answered a read for.
// The answer is what proves the thread exists in this session, and the request
// order is what keeps a late answer from moving the pointer back. A thread the
// app has already revealed as a subagent or ephemeral thread is never
// selectable.
func (b *bridge) confirmSelectionLocked(op threadOp) {
	b.knownThreads[op.threadID] = true
	if op.seq <= b.selectionSeq || b.backgroundThreads[op.threadID] {
		return
	}
	b.selectionSeq = op.seq
	b.activeThreadID = op.threadID
}

func (b *bridge) observeBackendMessage(message *wireMessage) {
	if message.Method != "" {
		b.observeNotification(message)
		return
	}
	requestID := idKey(message.ID)
	if requestID == "" {
		return
	}

	b.mu.Lock()
	threadResponse := b.pendingThread[requestID]
	delete(b.pendingThread, requestID)
	modelListResponse := b.pendingModelLists[requestID]
	delete(b.pendingModelLists, requestID)
	op, hasOp := b.pendingOps[requestID]
	delete(b.pendingOps, requestID)
	waiter := b.injected[requestID]
	delete(b.injected, requestID)
	usageWaiter, isUsageRead := b.pendingUsage[requestID]
	delete(b.pendingUsage, requestID)
	// Only the bridge's own read owns the in-flight flag. The app polls the same
	// method with its own ids, and its answer arriving mid-read must not let
	// usage() start a second read on top of the one already running.
	if isUsageRead && usageWaiter != nil {
		b.usageReadInFlight = false
	}
	if waiter != nil {
		// The injected request owns this canonical id. It can never confirm an
		// app-originated operation, even if corrupt state somehow contains both.
		threadResponse = false
		hasOp = false
		isUsageRead = false
		usageWaiter = nil
	}
	b.mu.Unlock()

	if errorMessage(message.Error) != "" {
		// The request named a thread this app-server session could not resolve.
		// Nothing in a failed response may become the selection or a known
		// thread, which is the whole point of waiting for the answer.
		if hasOp && op.accepts && missingThreadError(errorMessage(message.Error)) {
			b.evictThread(op.threadID)
		}
		// A turn/start rejected because the account is out of usage is direct
		// evidence, but only the structured codexErrorInfo field counts: a free
		// text message is not a signal.
		if hasOp && op.accepts && strings.EqualFold(errorCodexErrorInfo(message.Error), "usageLimitExceeded") {
			b.noteTurnUsageBlock(op.threadID)
		}
		if waiter != nil {
			waiter <- injectedResult{result: message.Result, errMsg: message.Error}
		}
		if usageWaiter != nil {
			usageWaiter <- injectedResult{result: message.Result, errMsg: message.Error}
		}
		return
	}
	if modelListResponse && len(message.Result) > 0 {
		b.applyModelList(message.Result)
	}

	if isUsageRead && len(message.Result) > 0 {
		b.applyUsageRead(message.Result)
	}
	if usageWaiter != nil {
		// Wake the bridge's own read with the answer so usage() reports the read
		// instead of answering usage-timeout after the cache was already updated.
		usageWaiter <- injectedResult{result: message.Result, errMsg: message.Error}
	}
	if threadResponse && len(message.Result) > 0 {
		var payload struct {
			Thread threadRef `json:"thread"`
		}
		if json.Unmarshal(message.Result, &payload) == nil && payload.Thread.ID != "" {
			b.adoptThread(payload.Thread)
		}
	}
	if hasOp {
		b.confirmOp(op, message.Result)
	}
	if waiter != nil {
		waiter <- injectedResult{result: message.Result, errMsg: message.Error}
	}
}

// confirmOp applies a successful response to an outbound request that named a
// thread. Reading the id off the request was only a candidate; the answer is the
// confirmation.
func (b *bridge) confirmOp(op threadOp, result json.RawMessage) {
	if !op.selects {
		b.mu.Lock()
		b.knownThreads[op.threadID] = true
		if op.accepts && op.acceptSeq > b.acceptedSeq &&
			b.foregroundThreads[op.threadID] && !b.backgroundThreads[op.threadID] {
			b.acceptedThreadID = op.threadID
			b.acceptedSeq = op.acceptSeq
		}
		b.mu.Unlock()
		return
	}
	// thread/read and thread/resume echo the thread they returned. If the backend
	// answers with a different thread than the request named, the answer wins.
	var payload struct {
		Thread threadRef `json:"thread"`
	}
	if len(result) > 0 && json.Unmarshal(result, &payload) == nil && payload.Thread.ID != "" {
		if payload.Thread.ID != op.threadID {
			b.adoptThread(payload.Thread)
			return
		}
		b.mu.Lock()
		b.classifyThreadLocked(payload.Thread)
		b.confirmSelectionLocked(op)
		b.mu.Unlock()
		return
	}
	b.mu.Lock()
	b.confirmSelectionLocked(op)
	b.mu.Unlock()
}

func (b *bridge) observeNotification(message *wireMessage) {
	switch message.Method {
	case "thread/started":
		var payload struct {
			Thread threadRef `json:"thread"`
		}
		if json.Unmarshal(message.Params, &payload) == nil && payload.Thread.ID != "" {
			b.adoptThread(payload.Thread)
		}
	case "turn/started":
		var payload struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(message.Params, &payload) != nil || payload.ThreadID == "" {
			return
		}
		b.mu.Lock()
		b.knownThreads[payload.ThreadID] = true
		// A turn does not select a thread. Subagent turns are indistinguishable
		// from the operator's own here, so the pointer only moves on a selection
		// signal.
		if payload.Turn.ID != "" {
			b.lastTurnID = payload.Turn.ID
		}
		b.mu.Unlock()
	case usageUpdatedMethod:
		b.applyUsageUpdate(message.Params)
	case "turn/completed":
		b.observeTurnCompleted(message.Params)
	}
}

type threadRef struct {
	ID        string `json:"id"`
	Ephemeral bool   `json:"ephemeral"`
	// source is a string in some app-server responses and an object in others.
	// Decoding it as a struct here would make the whole thread object fail to
	// parse, which silently drops the thread id.
	Source json.RawMessage `json:"source"`
}

// isBackground mirrors the app's own subagent and ephemeral thread predicates.
func (t threadRef) isBackground() bool {
	if t.Ephemeral {
		return true
	}
	if len(t.Source) == 0 {
		return false
	}
	var sourceName string
	if json.Unmarshal(t.Source, &sourceName) == nil {
		normalized := strings.ToLower(strings.ReplaceAll(sourceName, "_", ""))
		return normalized == "subagent"
	}
	var source struct {
		SubAgent *struct {
			ThreadSpawn *struct {
				ParentThreadID string `json:"parent_thread_id"`
			} `json:"thread_spawn"`
		} `json:"subAgent"`
		Subagent *struct {
			ThreadSpawn *struct {
				ParentThreadID string `json:"parent_thread_id"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	}
	if json.Unmarshal(t.Source, &source) != nil {
		return false
	}
	return (source.SubAgent != nil && source.SubAgent.ThreadSpawn != nil && source.SubAgent.ThreadSpawn.ParentThreadID != "") ||
		(source.Subagent != nil && source.Subagent.ThreadSpawn != nil && source.Subagent.ThreadSpawn.ParentThreadID != "")
}

// isForegroundSource is intentionally an allowlist. Actual user-owned Codex
// sessions use vscode (Desktop), cli, or user. Missing, object, and unknown
// values are not evidence that a thread is a root thread.
func (t threadRef) isForegroundSource() bool {
	if t.Ephemeral || len(bytes.TrimSpace(t.Source)) == 0 {
		return false
	}
	var sourceName string
	if json.Unmarshal(t.Source, &sourceName) != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(sourceName)) {
	case "user", "vscode", "cli":
		return true
	default:
		return false
	}
}

func (b *bridge) adoptThread(thread threadRef) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adoptThreadLocked(thread)
}

// adoptThreadLocked records a thread the app-server itself revealed, through a
// thread/start, thread/startAeon or thread/fork response or a thread/started
// notification. The backend creating or reporting the thread is confirmation
// that it exists, so it becomes known and, unless it is a background thread, the
// selection. Reads already in flight were issued before this and must not move
// the pointer back, which is what selectionSeq records.
func (b *bridge) adoptThreadLocked(thread threadRef) {
	b.knownThreads[thread.ID] = true
	b.selectionSeq = b.readSeq
	b.classifyThreadLocked(thread)
	if b.backgroundThreads[thread.ID] {
		return
	}
	b.activeThreadID = thread.ID
}

// classifyThreadLocked applies source/ephemeral metadata without changing the
// read ordering. This is also used for same-id read/resume responses: their
// metadata may retract an accepted/selected thread, but their selection still
// has to pass confirmSelectionLocked's sequence guard.
func (b *bridge) classifyThreadLocked(thread threadRef) {
	if !thread.Ephemeral && len(bytes.TrimSpace(thread.Source)) == 0 {
		return
	}
	if thread.isBackground() {
		b.backgroundThreads[thread.ID] = true
		delete(b.foregroundThreads, thread.ID)
		if b.acceptedThreadID == thread.ID {
			b.acceptedThreadID = ""
		}
		// A hydration read can reach a subagent thread before its own
		// thread/started reveals what it is; retract the selection it may have
		// caused.
		if b.activeThreadID == thread.ID {
			b.activeThreadID = ""
		}
		return
	}
	delete(b.backgroundThreads, thread.ID)
	if thread.isForegroundSource() {
		b.foregroundThreads[thread.ID] = true
	} else {
		delete(b.foregroundThreads, thread.ID)
		if b.acceptedThreadID == thread.ID {
			b.acceptedThreadID = ""
		}
	}
}

func idKey(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return ""
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return "s:" + typed
	case json.Number:
		return "n:" + typed.String()
	default:
		return ""
	}
}

func paramThreadID(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var payload struct {
		ThreadID string `json:"threadId"`
	}
	if json.Unmarshal(params, &payload) != nil {
		return ""
	}
	return payload.ThreadID
}

// turnTemplate keeps the app's own turn/start parameters for a thread so the
// injected turn inherits the model, effort, cwd, approval policy, and context
// the app would have sent. The user message and the turn guard are dropped.
func turnTemplate(params json.RawMessage) map[string]any {
	var template map[string]any
	if json.Unmarshal(params, &template) != nil {
		return nil
	}
	delete(template, "input")
	delete(template, "expectedTurnId")
	delete(template, "threadId")
	// A tool-result continuation is mutually exclusive with a new non-empty input.
	// Force submit always creates a fresh user-text turn, so never inherit it.
	delete(template, "toolOutput")
	// The app keys its optimistic user message on this id. Reusing the id of a
	// message the app already sent would make it attribute this turn's user item
	// to that older message.
	delete(template, "clientUserMessageId")
	if len(template) == 0 {
		return nil
	}
	return template
}

func (b *bridge) applyModelList(result json.RawMessage) {
	var payload struct {
		Data []modelCatalogEntry `json:"data"`
	}
	if json.Unmarshal(result, &payload) != nil {
		return
	}
	b.mu.Lock()
	catalog := make(map[string]modelCatalogEntry, len(b.modelCatalog)+len(payload.Data))
	for key, entry := range b.modelCatalog {
		catalog[key] = entry
	}
	for _, entry := range payload.Data {
		if strings.TrimSpace(entry.ID) == "" && strings.TrimSpace(entry.Model) == "" && strings.TrimSpace(entry.DisplayName) == "" {
			continue
		}
		// Keep every useful lookup key, while retaining the exact fields from
		// the app-server response for the turn/start override.
		for _, key := range []string{entry.ID, entry.Model, entry.DisplayName} {
			if key = strings.TrimSpace(key); key != "" {
				catalog[key] = entry
			}
		}
	}
	b.modelCatalog = catalog
	b.mu.Unlock()
}

type resolvedModelLabel struct {
	model  string
	effort string
}

func effortForEntry(entry modelCatalogEntry, value string) string {
	for _, effort := range entry.SupportedReasoningEfforts {
		if strings.EqualFold(strings.TrimSpace(effort), strings.TrimSpace(value)) {
			return strings.TrimSpace(effort)
		}
	}
	return ""
}

func splitOpenCodeGoLabel(raw string) (string, string) {
	for _, effort := range []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		for _, suffix := range []string{" " + effort, " (" + effort + ")", " [" + effort + "]"} {
			if len(raw) > len(suffix) && strings.EqualFold(raw[len(raw)-len(suffix):], suffix) {
				return strings.TrimSpace(raw[:len(raw)-len(suffix)]), effort
			}
		}
	}
	return raw, ""
}

// resolveModelLabel uses the longest displayName prefix so labels such as
// "GPT (high)" resolve before a shorter "GPT" entry. Only a known
// reasoning effort may follow the prefix; an unknown label is rejected rather
// than silently reusing the stale turn template.
func (b *bridge) resolveModelLabelLocked(label string) (resolvedModelLabel, error) {
	raw := strings.TrimSpace(label)
	if raw == "" {
		return resolvedModelLabel{}, nil
	}
	// Custom routed slugs are already the actual wire model. Preserve them
	// instead of rewriting an opencode-go/... slug through an alias entry or
	// stale catalog. The desktop may append a canonical effort to the picker
	// label; strip only that known suffix and keep the slug itself byte-for-byte.
	if strings.HasPrefix(raw, "opencode-go/") {
		model, effort := splitOpenCodeGoLabel(raw)
		return resolvedModelLabel{model: model, effort: effort}, nil
	}
	entries := make([]modelCatalogEntry, 0, len(b.modelCatalog))
	seen := map[string]bool{}
	for _, entry := range b.modelCatalog {
		key := entry.ID + "\x00" + entry.Model + "\x00" + entry.DisplayName
		if !seen[key] {
			entries = append(entries, entry)
			seen[key] = true
		}
	}
	bestLen := -1
	var best modelCatalogEntry
	bestEffort := ""
	for _, entry := range entries {
		name := strings.TrimSpace(entry.DisplayName)
		if name == "" {
			continue
		}
		if strings.EqualFold(raw, name) || strings.EqualFold(raw, strings.TrimSpace(entry.Model)) || strings.EqualFold(raw, strings.TrimSpace(entry.ID)) {
			if len(name) > bestLen {
				best, bestLen, bestEffort = entry, len(name), effortForEntry(entry, entry.DefaultReasoningEffort)
			}
			continue
		}
		if len(raw) <= len(name) || !strings.EqualFold(raw[:len(name)], name) {
			continue
		}
		suffix := strings.TrimSpace(raw[len(name):])
		suffix = strings.Trim(suffix, "()[]{}")
		suffix = strings.TrimSpace(strings.TrimLeft(suffix, "-:|/·, 	"))
		if effort := effortForEntry(entry, suffix); effort != "" && len(name) > bestLen {
			best, bestLen, bestEffort = entry, len(name), effort
		}
	}
	if bestLen < 0 {
		return resolvedModelLabel{}, fmt.Errorf("unknown model picker label %q", raw)
	}
	model := strings.TrimSpace(best.Model)
	if model == "" {
		model = strings.TrimSpace(best.ID)
	}
	if model == "" {
		return resolvedModelLabel{}, fmt.Errorf("model picker label %q has no wire model", raw)
	}
	return resolvedModelLabel{model: model, effort: bestEffort}, nil
}

func (b *bridge) resolveModelLabel(label string) (resolvedModelLabel, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.resolveModelLabelLocked(label)
}

func (b *bridge) buildTurnStartParams(threadID, text, modelLabel string) (map[string]any, error) {
	params := map[string]any{}
	// The template is only ever the selected thread's own parameters. Applying
	// one thread's model, effort, or sandbox policy to another is exactly the
	// leak this guard exists to prevent.
	if threadID == b.activeThreadID {
		for key, value := range b.turnTemplates[threadID] {
			params[key] = value
		}
	}
	params["threadId"] = threadID
	params["input"] = []any{map[string]any{
		"type":          "text",
		"text":          text,
		"text_elements": []any{},
	}}
	if strings.TrimSpace(modelLabel) != "" {
		resolved, err := b.resolveModelLabelLocked(modelLabel)
		if err != nil {
			return nil, err
		}
		params["model"] = resolved.model
		if resolved.effort != "" {
			params["effort"] = resolved.effort
		} else {
			// Never carry the previous model's effort into a newly selected model.
			// Omitting the key lets the app-server apply that model's own default.
			delete(params, "effort")
		}
	}
	return params, nil
}

type ipcCommand struct {
	Type       string `json:"type"`
	ID         string `json:"id,omitempty"`
	Text       string `json:"text,omitempty"`
	ThreadID   string `json:"threadId,omitempty"`
	ModelLabel string `json:"modelLabel,omitempty"`
}

type ipcReply struct {
	Type             string      `json:"type"`
	Command          string      `json:"command,omitempty"`
	ID               string      `json:"id,omitempty"`
	OK               bool        `json:"ok"`
	Error            string      `json:"error,omitempty"`
	Message          string      `json:"message,omitempty"`
	ThreadID         string      `json:"threadId,omitempty"`
	AcceptedThreadID string      `json:"backendAcceptedThreadId,omitempty"`
	RequestID        string      `json:"requestId,omitempty"`
	TurnID           string      `json:"turnId,omitempty"`
	LastTurnID       string      `json:"lastTurnId,omitempty"`
	Connected        *bool       `json:"connected,omitempty"`
	AppServerPid     int         `json:"appServerPid,omitempty"`
	InjectedTurns    int         `json:"injectedTurns,omitempty"`
	KnownThreads     int         `json:"knownThreads,omitempty"`
	UptimeMs         int64       `json:"uptimeMs,omitempty"`
	DownstreamCli    string      `json:"downstreamCli,omitempty"`
	DownstreamSrc    string      `json:"downstreamCliSource,omitempty"`
	Usage            *usageReply `json:"usage,omitempty"`
}

func (b *bridge) serveIPC() {
	endpoint := endpointName()
	listener, err := listenLocal(endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: force-submit endpoint unavailable: %v\n", err)
		return
	}
	defer listener.Close()
	writeDescriptor(endpoint, b)

	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go b.handleIPCConn(conn)
	}
}

func (b *bridge) handleIPCConn(conn localConn) {
	defer conn.Close()
	encoder := json.NewEncoder(conn)
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64<<10), maxIPCCommandBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var command ipcCommand
		if err := json.Unmarshal([]byte(line), &command); err != nil {
			_ = encoder.Encode(ipcReply{
				Type:    "result",
				OK:      false,
				Error:   "invalid-command",
				Message: err.Error(),
			})
			continue
		}
		_ = encoder.Encode(b.dispatch(command))
	}
}

func (b *bridge) dispatch(command ipcCommand) ipcReply {
	switch command.Type {
	case "", "force-submit":
		return b.forceSubmit(command)
	case "status":
		return b.status(command)
	case "usage":
		return b.usage(command)
	default:
		return ipcReply{
			Type:    "result",
			Command: command.Type,
			ID:      command.ID,
			OK:      false,
			Error:   "invalid-command",
			Message: "unsupported command type",
		}
	}
}

func (b *bridge) forceSubmit(command ipcCommand) ipcReply {
	reply := ipcReply{Type: "result", Command: "force-submit", ID: command.ID}
	if strings.TrimSpace(command.Text) == "" {
		reply.Error = "invalid-command"
		reply.Message = "text is empty"
		return reply
	}

	b.mu.Lock()
	threadID := command.ThreadID
	explicit := threadID != ""
	if threadID == "" {
		threadID = b.activeThreadID
	}
	if !b.connected {
		b.mu.Unlock()
		reply.Error = "backend-not-connected"
		reply.Message = "the app-server connection has closed"
		return reply
	}
	if threadID == "" {
		b.mu.Unlock()
		reply.Error = "no-active-thread"
		reply.Message = "no thread observed yet; pass threadId explicitly"
		return reply
	}
	// The id has to be one this app-server session has actually answered for.
	// After a restart the obvious source of an id is the app's persisted
	// selection, which the backend may reject as "thread not found".
	if !b.knownThreads[threadID] {
		b.mu.Unlock()
		reply.Error = "no-active-thread"
		if explicit {
			reply.Message = fmt.Sprintf("threadId %s has not been confirmed by this app-server session", threadID)
		} else {
			reply.Message = fmt.Sprintf("thread %s has not been confirmed by this app-server session", threadID)
		}
		return reply
	}
	var requestID, requestKey string
	for {
		b.seq++
		requestID = fmt.Sprintf("ocx-force-submit-%d-%d", os.Getpid(), b.seq)
		requestKey = "s:" + requestID
		_, pendingOp := b.pendingOps[requestKey]
		_, pendingThread := b.pendingThread[requestKey]
		_, pendingInjection := b.injected[requestKey]
		if !pendingOp && !pendingThread && !pendingInjection {
			break
		}
	}
	params, modelErr := b.buildTurnStartParams(threadID, command.Text, command.ModelLabel)
	if modelErr != nil {
		b.mu.Unlock()
		reply.Error = "unknown-model-label"
		reply.Message = modelErr.Error()
		return reply
	}
	waiter := make(chan injectedResult, 1)
	b.injected[requestKey] = waiter
	b.mu.Unlock()

	reply.ThreadID = threadID
	reply.RequestID = requestID

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  "turn/start",
		"params":  params,
	})
	if err == nil {
		err = b.writeBackend(append(payload, '\n'))
	}
	if err != nil {
		b.forgetInjected(requestKey)
		reply.Error = "write-failed"
		reply.Message = err.Error()
		return reply
	}
	b.mu.Lock()
	b.injectedTurns++
	b.mu.Unlock()

	select {
	case result := <-waiter:
		if errorText := errorMessage(result.errMsg); errorText != "" {
			reply.Error = "backend-rejected"
			reply.Message = errorText
			// The backend refusing the turn because it cannot resolve the thread
			// means the id is dead in this session. Leaving it known would make
			// every later submission reuse it and fail the same way.
			if missingThreadError(errorText) {
				b.evictThread(threadID)
			}
			return reply
		}
		reply.OK = true
		reply.TurnID = turnIDFromResult(result.result)
		return reply
	case <-time.After(forceSubmitTimeout):
		b.forgetInjected(requestKey)
		reply.OK = true
		reply.Message = "accepted; response not observed before timeout"
		return reply
	}
}

func (b *bridge) forgetInjected(requestKey string) {
	b.mu.Lock()
	delete(b.injected, requestKey)
	b.mu.Unlock()
}

// missingThreadError reports whether a backend reply says the target thread
// cannot be turned: the id resolves to no rollout this app-server session can
// start a turn in. Any other failure is a real turn error and leaves the
// selection alone.
func missingThreadError(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range []string{"not found", "no rollout", "not materialized"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// evictThread forgets a thread the backend says it cannot start a turn in, and
// clears the selection when it pointed at that thread, so the next submission
// cannot inject into the same dead id.
func (b *bridge) evictThread(threadID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.knownThreads, threadID)
	delete(b.foregroundThreads, threadID)
	if b.activeThreadID == threadID {
		b.activeThreadID = ""
	}
	if b.acceptedThreadID == threadID {
		b.acceptedThreadID = ""
	}
}

// ---------------------------------------------------------------------------------------
// Structured usage signal
//
// The helper in front of this socket must never invent a usage block, so the bridge reports
// only what the app-server itself said. A hard block is exactly one of the official
// structured signals:
//
//   - a rateLimits snapshot whose top-level rateLimitReachedType is set,
//   - a rateLimits snapshot with spendControlReached true,
//   - a full account/rateLimits/read whose response-level ordinaryUsageAllowed is false,
//   - the operator's own foreground root turn reporting codexErrorInfo
//     usageLimitExceeded, in turn/completed or in a failed turn/start.
//
// usedPercent and credits are carried as advisory context and never confirm a block on their
// own: the protocol says a client must not infer recovery from percentages or reset times. A
// shape this bridge does not recognise leaves the state unknown, which keeps the helper's own
// UI inspection in charge for every provider the bridge has no structured signal for.
// account/rateLimits/updated is a sparse rolling update and carries no ordinaryUsageAllowed,
// so that flag is only ever set by an authoritative read.
// ---------------------------------------------------------------------------------------

// usageSnapshot is the merged view of every structured usage message observed so far. A full
// account/rateLimits/read replaces the baseline, because it is the authoritative document: a
// window the read no longer mentions is gone. account/rateLimits/updated is sparse, so it only
// replaces the keys it actually carried.
type usageSnapshot struct {
	observedAt int64
	source     string
	accountID  string
	// ordinaryAllows is the response-level ordinaryUsageAllowed from the last full
	// read. It sits next to rateLimits, not inside it, and a sparse update never
	// carries one, so only an authoritative read ever sets or clears it.
	ordinaryAllows *bool
	top            map[string]json.RawMessage
	nested         map[string]map[string]json.RawMessage
}

// usageBlock records one confirmed hard block and the evidence that produced it.
type usageBlock struct {
	at        int64
	source    string
	reason    string
	accountID string
}

// usageReply is the additive usage section of a reply. limitState is the only field the
// helper acts on: confirmed is a hard block, advisory is context, unknown is nothing.
type usageReply struct {
	LimitState      string             `json:"limitState"`
	Source          string             `json:"source,omitempty"`
	Reason          string             `json:"reason,omitempty"`
	ObservedAt      int64              `json:"observedAt,omitempty"`
	ObservedAgeMs   int64              `json:"observedAgeMs,omitempty"`
	AccountID       string             `json:"accountId,omitempty"`
	LimitID         string             `json:"limitId,omitempty"`
	NormalModelSlug string             `json:"normalModelSlug,omitempty"`
	PlanType        string             `json:"planType,omitempty"`
	Primary         *usageWindowReply  `json:"primary,omitempty"`
	Secondary       *usageWindowReply  `json:"secondary,omitempty"`
	Credits         *usageCreditsReply `json:"credits,omitempty"`
}

type usageWindowReply struct {
	UsedPercent        *float64 `json:"usedPercent,omitempty"`
	WindowDurationMins *int64   `json:"windowDurationMins,omitempty"`
	ResetsAt           *int64   `json:"resetsAt,omitempty"`
}

type usageCreditsReply struct {
	HasCredits *bool  `json:"hasCredits,omitempty"`
	Unlimited  *bool  `json:"unlimited,omitempty"`
	Balance    string `json:"balance,omitempty"`
}

func nowMillis() int64 { return time.Now().UnixMilli() }

func jsonObjectField(raw map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	value, ok := raw[key]
	if !ok {
		return nil, false
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal(value, &nested) != nil || nested == nil {
		return nil, false
	}
	return nested, true
}

func jsonBoolField(raw map[string]json.RawMessage, key string) (bool, bool) {
	value, ok := raw[key]
	if !ok {
		return false, false
	}
	var flag bool
	if json.Unmarshal(value, &flag) != nil {
		return false, false
	}
	return flag, true
}

func jsonNumberField(raw map[string]json.RawMessage, key string) (float64, bool) {
	value, ok := raw[key]
	if !ok {
		return 0, false
	}
	var number float64
	if json.Unmarshal(value, &number) != nil {
		return 0, false
	}
	return number, true
}

func jsonIntField(raw map[string]json.RawMessage, key string) (int64, bool) {
	number, ok := jsonNumberField(raw, key)
	if !ok {
		return 0, false
	}
	return int64(number), true
}

func jsonTextField(raw map[string]json.RawMessage, key string) string {
	value, ok := raw[key]
	if !ok {
		return ""
	}
	var text string
	if json.Unmarshal(value, &text) != nil {
		return ""
	}
	return strings.TrimSpace(text)
}

// jsonSetField reports a key present with a value other than null. A null field means "not
// reported", which is what keeps a limit that has since reset from reading as a block.
func jsonSetField(raw map[string]json.RawMessage, key string) bool {
	value, ok := raw[key]
	if !ok {
		return false
	}
	trimmed := strings.TrimSpace(string(value))
	return trimmed != "" && trimmed != "null"
}

func mergeUsageFields(dst, src map[string]json.RawMessage) map[string]json.RawMessage {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]json.RawMessage, len(src))
	}
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

// usageNestedObjects are the snapshot members the bridge tracks per key. The schema also
// defines individual_limit, a SpendControlLimitSnapshot the bridge does not track here.
var usageNestedObjects = []string{"primary", "secondary", "credits"}

// merge replaces only the keys the incoming object actually carried. A sparse update needs
// this: replacing the whole document with one would erase the windows it did not mention.
func (s *usageSnapshot) merge(incoming map[string]json.RawMessage) {
	s.top = mergeUsageFields(s.top, incoming)
	for _, key := range usageNestedObjects {
		child, ok := jsonObjectField(incoming, key)
		if !ok {
			continue
		}
		if s.nested == nil {
			s.nested = map[string]map[string]json.RawMessage{}
		}
		s.nested[key] = mergeUsageFields(s.nested[key], child)
	}
}

// replace makes the incoming document the new baseline. Only a full read may do this, so a
// window that read left out of its answer stops being reported.
func (s *usageSnapshot) replace(incoming map[string]json.RawMessage) {
	s.top = make(map[string]json.RawMessage, len(incoming))
	for key, value := range incoming {
		s.top[key] = value
	}
	s.nested = map[string]map[string]json.RawMessage{}
	for _, key := range usageNestedObjects {
		if child, ok := jsonObjectField(incoming, key); ok {
			s.nested[key] = child
		}
	}
}

func (s usageSnapshot) present() bool { return len(s.top) > 0 || len(s.nested) > 0 }

// reachedTypeSource names the member that carries rateLimitReachedType, or "" when none
// does. Only the snapshot itself carries it: the RateLimitWindow members under primary and
// secondary do not, and individual_limit is a SpendControlLimitSnapshot without one. The
// app-server serializes an unset variant as null, so presence alone is not enough.
func (s usageSnapshot) reachedTypeSource() string {
	if jsonSetField(s.top, "rateLimitReachedType") {
		return "rateLimitReachedType"
	}
	return ""
}

// blockEvidence reports the official hard-block signal this snapshot carries, or "".
func (s usageSnapshot) blockEvidence() string {
	if source := s.reachedTypeSource(); source != "" {
		return source + " is set"
	}
	if reached, ok := jsonBoolField(s.top, "spendControlReached"); ok && reached {
		return "spendControlReached is true"
	}
	if spend, ok := jsonObjectField(s.top, "spendControl"); ok {
		if reached, ok := jsonBoolField(spend, "reached"); ok && reached {
			return "spendControl.reached is true"
		}
	}
	if s.ordinaryAllows != nil && !*s.ordinaryAllows {
		return "ordinaryUsageAllowed is false"
	}
	return ""
}

// observeUsageSnapshot folds one structured usage object into the cache and records the hard
// block it carries. full marks the authoritative account/rateLimits/read; allowed is the
// response-level ordinaryUsageAllowed, which only a read carries. Absence of evidence is never
// evidence: a snapshot with no block signal leaves an earlier confirmation alone until its TTL
// runs out.
func (b *bridge) observeUsageSnapshot(snapshot map[string]json.RawMessage, source, accountID string, full bool, allowed *bool) {
	if len(snapshot) == 0 && allowed == nil {
		return
	}
	at := nowMillis()
	if accountID == "" {
		accountID = jsonTextField(snapshot, "accountId")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(snapshot) > 0 {
		if full {
			b.usageState.replace(snapshot)
		} else {
			b.usageState.merge(snapshot)
		}
		b.usageState.observedAt = at
		b.usageState.source = source
		if accountID != "" {
			b.usageState.accountID = accountID
		}
	}
	if allowed != nil {
		b.usageState.ordinaryAllows = allowed
	}
	if evidence := b.usageState.blockEvidence(); evidence != "" {
		b.usageBlock = usageBlock{at: at, source: source, reason: evidence, accountID: accountID}
		return
	}
	// An explicit "ordinary usage is allowed" retracts the snapshot's own confirmation, and
	// only for the account it answered for. The turn signal is direct evidence from the
	// operator's own turn and is left to its own TTL.
	if allowed != nil && *allowed && sameUsageScope(b.usageBlock.accountID, accountID) {
		b.usageBlock = usageBlock{}
	}
}

// sameUsageScope reports whether a confirmation and a fresh read describe the same account. An
// unnamed account on either side cannot be narrowed, so the answer still counts.
func sameUsageScope(confirmed, read string) bool {
	return confirmed == "" || read == "" || confirmed == read
}

// applyUsageRead consumes the app-server's answer to account/rateLimits/read, whether the
// app issued it or the bridge did. ordinaryUsageAllowed sits at the response level, next to
// rateLimits, so it is read there and not from inside the snapshot.
func (b *bridge) applyUsageRead(result json.RawMessage) {
	payload := map[string]json.RawMessage{}
	if json.Unmarshal(result, &payload) != nil {
		return
	}
	snapshot, ok := jsonObjectField(payload, "rateLimits")
	if !ok {
		// No rateLimits container: a provider that has none, or a shape this bridge does
		// not know. Either way the state stays as it is and the helper keeps its own scan.
		return
	}
	var allowed *bool
	if value, ok := jsonBoolField(payload, "ordinaryUsageAllowed"); ok {
		allowed = &value
	}
	b.observeUsageSnapshot(snapshot, usageRateLimitMethod, jsonTextField(payload, "accountId"), true, allowed)
}

// applyUsageUpdate consumes account/rateLimits/updated, which carries only what changed.
func (b *bridge) applyUsageUpdate(params json.RawMessage) {
	payload := map[string]json.RawMessage{}
	if json.Unmarshal(params, &payload) != nil {
		return
	}
	snapshot := payload
	if nested, ok := jsonObjectField(payload, "rateLimits"); ok {
		snapshot = nested
	}
	b.observeUsageSnapshot(snapshot, usageUpdatedMethod, jsonTextField(payload, "accountId"), false, nil)
}

// observeTurnCompleted records the strongest signal there is: the operator's own foreground
// root turn came back failed because the account hit its usage limit. A subagent turn, and
// any thread that is not the accepted or active foreground root, is ignored.
func (b *bridge) observeTurnCompleted(params json.RawMessage) {
	var payload struct {
		ThreadID       string          `json:"threadId"`
		CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
		Turn           struct {
			CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
			Error          struct {
				CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
			} `json:"error"`
		} `json:"turn"`
	}
	if json.Unmarshal(params, &payload) != nil || payload.ThreadID == "" {
		return
	}
	name := codexErrorName(payload.Turn.Error.CodexErrorInfo)
	if name == "" {
		name = codexErrorName(payload.Turn.CodexErrorInfo)
	}
	if name == "" {
		name = codexErrorName(payload.CodexErrorInfo)
	}
	if !strings.EqualFold(name, "usageLimitExceeded") {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.backgroundThreads[payload.ThreadID] {
		return
	}
	root := payload.ThreadID == b.acceptedThreadID ||
		(payload.ThreadID == b.activeThreadID && b.foregroundThreads[payload.ThreadID])
	if !root {
		return
	}
	b.usageTurnBlock = usageBlock{
		at:     nowMillis(),
		source: "turn/completed",
		reason: "the foreground root turn ended with codexErrorInfo usageLimitExceeded",
	}
}

// codexErrorName normalizes codexErrorInfo, which is a bare string in some app-server builds
// and a tagged object ({"type": ...} or {"kind": ...}) in others.
func codexErrorName(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var tagged map[string]json.RawMessage
	if json.Unmarshal(raw, &tagged) != nil {
		return ""
	}
	for _, key := range []string{"type", "kind", "name", "codexErrorInfo"} {
		if value := jsonTextField(tagged, key); value != "" {
			return value
		}
	}
	return ""
}

// errorCodexErrorInfo extracts the structured codexErrorInfo name from a JSON-RPC error
// object. The app-server reports it beside the message or under data; a free-text error
// message is not a signal.
func errorCodexErrorInfo(raw json.RawMessage) string {
	var payload struct {
		CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
		Data           struct {
			CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
		} `json:"data"`
	}
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	if name := codexErrorName(payload.CodexErrorInfo); name != "" {
		return name
	}
	return codexErrorName(payload.Data.CodexErrorInfo)
}

// noteTurnUsageBlock records a turn/start the app-server rejected with codexErrorInfo
// usageLimitExceeded. Only the operator's own foreground root thread counts: a subagent or
// background thread hitting the same limit says nothing about the composer.
func (b *bridge) noteTurnUsageBlock(threadID string) {
	if threadID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.backgroundThreads[threadID] {
		return
	}
	if threadID != b.acceptedThreadID &&
		!(threadID == b.activeThreadID && b.foregroundThreads[threadID]) {
		return
	}
	b.usageTurnBlock = usageBlock{
		at:        nowMillis(),
		source:    "turn/start",
		reason:    "a foreground root turn/start was rejected with codexErrorInfo usageLimitExceeded",
		accountID: b.usageState.accountID,
	}
}

// activeUsageBlockLocked returns the newest confirmation still inside its TTL. The turn
// signal wins because it is direct evidence from the operator's own turn. Both expire so a
// limit that has since reset cannot keep the helper's shortcut alive.
func (b *bridge) activeUsageBlockLocked(at int64) *usageBlock {
	if block := b.usageTurnBlock; block.at != 0 && at-block.at <= usageTurnTTL.Milliseconds() {
		return &block
	}
	if block := b.usageBlock; block.at != 0 && at-block.at <= usageSnapshotTTL.Milliseconds() {
		return &block
	}
	return nil
}

// usageReplyLocked reports the cached structured state. It never blocks and never injects,
// because the helper reads it through --ocx-status and a status poll has to stay cheap; the
// explicit --ocx-usage call is the one that may ask the app-server for a fresh read.
func (b *bridge) usageReplyLocked(now time.Time) *usageReply {
	at := now.UnixMilli()
	reply := &usageReply{LimitState: "unknown"}
	snapshot := b.usageState
	if snapshot.present() {
		reply.ObservedAt = snapshot.observedAt
		reply.ObservedAgeMs = at - snapshot.observedAt
		reply.Source = snapshot.source
		reply.AccountID = snapshot.accountID
		reply.LimitID = jsonTextField(snapshot.top, "limitId")
		reply.NormalModelSlug = jsonTextField(snapshot.top, "normalModelSlug")
		reply.PlanType = jsonTextField(snapshot.top, "planType")
		reply.Primary = usageWindowFrom(snapshot.nested["primary"])
		reply.Secondary = usageWindowFrom(snapshot.nested["secondary"])
		reply.Credits = usageCreditsFrom(snapshot.nested["credits"])
	}
	if block := b.activeUsageBlockLocked(at); block != nil {
		reply.LimitState = "confirmed"
		reply.Source = block.source
		reply.Reason = block.reason
		return reply
	}
	if snapshot.present() {
		// Advisory only: a percentage or a credit balance never proves the app-server is
		// refusing turns, so the helper keeps its own UI inspection alongside this.
		reply.LimitState = "advisory"
		reply.Reason = "a usage snapshot is cached but carries no hard-block signal"
	}
	return reply
}

func usageWindowFrom(fields map[string]json.RawMessage) *usageWindowReply {
	if len(fields) == 0 {
		return nil
	}
	reply := &usageWindowReply{}
	if value, ok := jsonNumberField(fields, "usedPercent"); ok {
		reply.UsedPercent = &value
	}
	if value, ok := jsonIntField(fields, "windowDurationMins"); ok {
		reply.WindowDurationMins = &value
	}
	if value, ok := jsonIntField(fields, "resetsAt"); ok {
		reply.ResetsAt = &value
	}
	return reply
}

func usageCreditsFrom(fields map[string]json.RawMessage) *usageCreditsReply {
	if len(fields) == 0 {
		return nil
	}
	reply := &usageCreditsReply{Balance: jsonTextField(fields, "balance")}
	if value, ok := jsonBoolField(fields, "hasCredits"); ok {
		reply.HasCredits = &value
	}
	if value, ok := jsonBoolField(fields, "unlimited"); ok {
		reply.Unlimited = &value
	}
	return reply
}

// nextUsageRequestLocked picks an id no pending request owns. A response is matched by
// canonical id, so reusing an id that is still in flight would answer the wrong slot.
func (b *bridge) nextUsageRequestLocked() (string, string) {
	for {
		b.seq++
		requestID := fmt.Sprintf("ocx-usage-%d-%d", os.Getpid(), b.seq)
		requestKey := "s:" + requestID
		_, pendingOp := b.pendingOps[requestKey]
		_, pendingThread := b.pendingThread[requestKey]
		_, pendingInjection := b.injected[requestKey]
		_, pendingUsage := b.pendingUsage[requestKey]
		if !pendingOp && !pendingThread && !pendingInjection && !pendingUsage {
			return requestID, requestKey
		}
	}
}

// usage answers the explicit --ocx-usage call. When the cached snapshot is stale and no read
// is in flight it asks the app-server for its own usage once, then answers from the cache. A
// provider with no rate limits, or an app-server that stays silent, leaves the reply at
// unknown, which is the fail-open direction: the helper's own inspection decides then.
func (b *bridge) usage(command ipcCommand) ipcReply {
	reply := ipcReply{Type: "result", Command: "usage", ID: command.ID}
	now := time.Now()
	b.mu.Lock()
	if !b.connected {
		reply.Usage = b.usageReplyLocked(now)
		b.mu.Unlock()
		reply.Error = "backend-not-connected"
		reply.Message = "the app-server connection has closed"
		return reply
	}
	fresh := b.usageState.observedAt != 0 && now.UnixMilli()-b.usageState.observedAt < usageFreshWindow.Milliseconds()
	if fresh || b.usageReadInFlight {
		reply.OK = true
		reply.Usage = b.usageReplyLocked(now)
		b.mu.Unlock()
		return reply
	}
	requestID, requestKey := b.nextUsageRequestLocked()
	waiter := make(chan injectedResult, 1)
	if b.pendingUsage == nil {
		b.pendingUsage = map[string]chan injectedResult{}
	}
	b.pendingUsage[requestKey] = waiter
	b.usageReadInFlight = true
	b.mu.Unlock()

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  usageRateLimitMethod,
		"params":  map[string]any{},
	})
	if err == nil {
		err = b.writeBackend(append(payload, '\n'))
	}
	if err != nil {
		b.mu.Lock()
		delete(b.pendingUsage, requestKey)
		b.usageReadInFlight = false
		reply.Usage = b.usageReplyLocked(time.Now())
		b.mu.Unlock()
		reply.Error = "write-failed"
		reply.Message = err.Error()
		return reply
	}

	select {
	case result := <-waiter:
		b.mu.Lock()
		reply.Usage = b.usageReplyLocked(time.Now())
		b.mu.Unlock()
		if errorText := errorMessage(result.errMsg); errorText != "" {
			reply.Error = "backend-rejected"
			reply.Message = errorText
			return reply
		}
		reply.OK = true
		return reply
	case <-time.After(usageReadTimeout):
		// The read may still land and update the cache; dropping the waiter only means this
		// caller answers from the cache as it stands now.
		b.mu.Lock()
		delete(b.pendingUsage, requestKey)
		b.usageReadInFlight = false
		reply.Usage = b.usageReplyLocked(time.Now())
		b.mu.Unlock()
		reply.Error = "usage-timeout"
		reply.Message = "the app-server did not answer the usage read in time"
		return reply
	}
}

func (b *bridge) status(command ipcCommand) ipcReply {
	b.mu.Lock()
	defer b.mu.Unlock()
	connected := b.connected
	return ipcReply{
		Type:             "result",
		Command:          "status",
		ID:               command.ID,
		OK:               true,
		ThreadID:         b.activeThreadID,
		AcceptedThreadID: b.acceptedThreadID,
		LastTurnID:       b.lastTurnID,
		Connected:        &connected,
		AppServerPid:     b.appServerPid,
		InjectedTurns:    b.injectedTurns,
		KnownThreads:     len(b.knownThreads),
		UptimeMs:         time.Since(b.startedAt).Milliseconds(),
		DownstreamCli:    b.downstreamCLIPath,
		DownstreamSrc:    b.downstreamSource,
		Usage:            b.usageReplyLocked(time.Now()),
	}
}

func errorMessage(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return strings.TrimSpace(string(raw))
	}
	if payload.Message == "" {
		return strings.TrimSpace(string(raw))
	}
	return fmt.Sprintf("%s (code %d)", payload.Message, payload.Code)
}

func turnIDFromResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var payload struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	return payload.Turn.ID
}

func writeDescriptor(endpoint string, b *bridge) {
	payload := map[string]any{
		"endpoint":            endpoint,
		"pid":                 os.Getpid(),
		"appServerPid":        b.appServerPid,
		"downstreamCli":       b.downstreamCLIPath,
		"downstreamCliSource": b.downstreamSource,
		"startedAt":           b.startedAt.UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	descriptor := filepath.Join(stateDir(), descriptorName)
	temporary := descriptor + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return
	}
	_ = os.Rename(temporary, descriptor)
}

// runClient is the same force-submit path for callers that would rather shell
// out than speak the socket protocol themselves.
func runClient(command ipcCommand) int {
	endpoint := endpointName()
	conn, err := dialLocal(endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: cannot reach the bridge at %s: %v\n", endpoint, err)
		return 2
	}
	defer conn.Close()
	// Nothing here can outlive the server's own reply deadline by much; this only
	// guards against a server that died without closing the connection.
	expired := make(chan struct{})
	defer close(expired)
	go func() {
		select {
		case <-expired:
		case <-time.After(forceSubmitTimeout + 10*time.Second):
			_ = conn.Close()
		}
	}()

	payload, err := json.Marshal(command)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: %v\n", err)
		return 2
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: %v\n", err)
		return 2
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if len(line) == 0 {
		fmt.Fprintf(os.Stderr, "ocx-codex-appserver-bridge: no reply from the bridge: %v\n", err)
		return 2
	}
	_, _ = os.Stdout.Write(line)
	var reply struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(line, &reply) != nil || !reply.OK {
		return 1
	}
	return 0
}
