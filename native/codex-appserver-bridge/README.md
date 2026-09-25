# Codex app-server bridge

A transparent stdio proxy that sits between the Codex desktop app and whatever
Codex CLI the app would otherwise spawn, plus a local named pipe that can submit
a turn directly. It exists so the composer can still send a message when the app
has disabled its own send button at the account usage limit, without modifying
`app.asar` or the MSIX install.

## How it attaches

On Windows the desktop app always starts its app-server as a child process and
speaks newline-delimited JSON-RPC 2.0 over that child's stdin and stdout. The
websocket daemon path is gated on `process.platform !== "win32"`, so it
never applies here. The CLI path is resolved in this order:

1. `hostConfig.codex_cli_command`
2. `CODEX_CLI_PATH` when it is a bare command name
3. the bundled `codex.exe`

Setting `CODEX_CLI_PATH` to a full path is honoured by the bundled
resolver and reports `source: "override"`, which is what lets the bridge
attach without touching the archive. The app then spawns the bridge with

~~~
-c features.code_mode_host=true app-server --analytics-default-enabled
~~~

and the bridge spawns its downstream CLI with those same arguments, the same
working directory, and the app's environment.

## Chaining to an existing wrapper

`CODEX_CLI_PATH` cannot be read back from inside the bridge - the app
already points it at the bridge. The installer therefore records the previous
value, and the bridge resolves its downstream in this order:

1. `OCX_CODEX_DOWNSTREAM_CLI` - full path to the CLI that
   `CODEX_CLI_PATH` named before the bridge was installed. This is how an
   existing wrapper such as `codex-webgpt-proxy.exe` stays in the chain.
2. `OCX_CODEX_REAL_CLI` - the earlier name for the same thing.
3. `%LOCALAPPDATA%\OpenCodex\codex-appserver-bridge\downstream-cli.txt` -
   a single-line state file the installer may write instead of using an
   environment variable.
4. the CLI the app itself relocates into
   `%LOCALAPPDATA%\OpenAI\Codex\bin\codex.exe`, falling back to the
   newest versioned copy under that directory.

The bridge refuses to chain to itself, and it repairs two environment entries for
the child so the downstream sees what it would have seen without the bridge:

- `CODEX_CLI_PATH` is set back to the downstream CLI. It currently names
  the bridge, because that is how the app was pointed here; a downstream wrapper
  that reads it would otherwise spawn the bridge again.
- `PATH` is prefixed with the downstream CLI's directory, which is what
  the app's own resolution would have added.

`OCX_CODEX_DOWNSTREAM_CLI` is left untouched, so any further hop in the
chain still resolves the same CLI.

### The existing WebGPT wrapper does not recurse

Verified against the installed `codex-webgpt-proxy.exe` by reading its
embedded source. It does not use `CODEX_CLI_PATH` to find the original
Codex CLI. It takes `CODEX_WEBGPT_REAL_CODEX` as an explicit override, and
otherwise scans `%LOCALAPPDATA%\OpenAI\Codex\bin\<version>\codex.exe`,
skipping any candidate that resolves to itself, then validates each with
`--version` while explicitly clearing `CODEX_CLI_PATH` for that
probe. The bridge also lives outside that managed root, so it is not a candidate
either way. Chaining through it was exercised end to end and the process count
stayed flat.

## Activation

~~~powershell
powershell -ExecutionPolicy Bypass -File build.ps1
setx OCX_CODEX_DOWNSTREAM_CLI "<the current value of CODEX_CLI_PATH>"
setx CODEX_CLI_PATH "%LOCALAPPDATA%\OpenCodex\codex-appserver-bridge\ocx-codex-appserver-bridge.exe"
~~~

Then restart the Codex app once. A freshly written user environment variable only
reaches a process whose launcher has refreshed its environment, so sign out and
back in, restart Explorer, or launch Codex from a shell that already has the
variables set.

Other environment overrides:

| Variable | Meaning |
| --- | --- |
| `OCX_CODEX_BRIDGE_ENDPOINT` | Exact local endpoint name. |
| `OCX_CODEX_BRIDGE_DIR` | Directory for the endpoint descriptor and the downstream state file. |
| `OCX_CODEX_BRIDGE_DEBUG` | Trace every parsed line to stderr. |

## Endpoint

~~~
\\.\pipe\opencodex-codex-appserver-bridge-<user name>
~~~

A Windows named pipe with an explicit, protected DACL naming exactly three
trustees: the current user's SID, SYSTEM, and Administrators. The bridge reads its
own SID from the process token and passes the descriptor to the pipe, rather than
letting the pipe inherit whatever the creating token's default DACL happens to
be. Inheritance is disabled, so nothing else is reachable. There is no separate
token. The user name in the pipe name keeps two signed-in users apart, because
the pipe namespace is machine-wide.

With `OCX_CODEX_BRIDGE_DEBUG` set, the resolved DACL is printed at
startup, for example
`D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;S-1-5-21-...-1001)`.

Go's AF_UNIX support on Windows binds but cannot connect across processes, which
is why this is a pipe rather than a socket. The Unix build still uses an AF_UNIX
socket under the state directory.

`bridge.json` in the state directory carries `endpoint`,
`pid`, `appServerPid`, `downstreamCli`,
`downstreamCliSource`, and `startedAt`, so a caller can discover the
endpoint and confirm which downstream was resolved instead of guessing.

## Message contract

One JSON object per line, UTF-8, terminated by a newline. Each command gets
exactly one reply on the same connection.

Commands:

~~~json
{"type":"force-submit","id":"1","text":"hello","threadId":"optional-override"}
{"type":"status","id":"2"}
~~~

`text` must be non-empty. `threadId` is optional; without it the
bridge uses the thread it has been tracking. Either way the id has to be one this
app-server session has confirmed by answering a request that named it, otherwise
the reply is `no-active-thread` and nothing is injected. A bare
`{"text":"..."}` with no `type` is treated as a force-submit.

Replies:

~~~json
{"type":"result","command":"force-submit","id":"1","ok":true,"threadId":"...","requestId":"ocx-force-submit-1234-1","turnId":"..."}
{"type":"result","command":"force-submit","id":"1","ok":false,"error":"backend-rejected","message":"...","requestId":"ocx-force-submit-1234-1"}
{"type":"result","command":"status","id":"2","ok":true,"threadId":"...","lastTurnId":"...","connected":true,"appServerPid":456,"injectedTurns":0,"knownThreads":2,"uptimeMs":1234,"downstreamCli":"..."}
~~~

Error codes: `no-active-thread`, `backend-not-connected`,
`write-failed`, `backend-rejected`, `invalid-command`.

A force-submit reply is held until the app-server answers the injected
`turn/start`, so `ok:true` means the turn was accepted and
`turnId` is the real turn id. If the answer does not arrive within ten
seconds the reply is still `ok:true` with a message saying the response was
not observed.

The same executable is also a client, so a keyboard helper needs no socket code:

~~~
ocx-codex-appserver-bridge.exe --ocx-force-submit "text"
ocx-codex-appserver-bridge.exe --ocx-force-submit "text" --ocx-thread-id <thread id>
ocx-codex-appserver-bridge.exe --ocx-force-submit "text" --ocx-model-label "GPT-5.6 Sol Medium"
ocx-codex-appserver-bridge.exe --ocx-status
ocx-codex-appserver-bridge.exe --ocx-endpoint
~~~

`--ocx-thread-id` names the thread to submit into instead of the tracked
selection, so it is the flag a caller uses when it can read the visible task
itself. The flag and its value are stripped from the message text wherever they
sit in the argument list, and an explicit id never falls back to the tracked
selection.

It prints the reply JSON and exits 0 on `ok:true`, 1 when the bridge
reports a failure, and 2 when the bridge cannot be reached.

## What is forwarded and what is injected

Every line in both directions is forwarded byte for byte, including the response
to an injected request. The app matches responses by id and logs
`response_orphaned` for an id it does not own, which is harmless.

An injected request is:

~~~json
{"jsonrpc":"2.0","id":"ocx-force-submit-<pid>-<n>","method":"turn/start","params":{...}}
~~~

The params start from the app's own last `turn/start` params for that
thread, so the model, effort, working directory, approval policy, personality,
`additionalContext`, and `responsesapiClientMetadata` are exactly
what the app would have sent. `input` is then replaced with
`[{"type":"text","text":"...","text_elements":[]}]`, and
`expectedTurnId` is dropped so a stale turn guard cannot reject the
injection.

The template is applied only when the target is the selected thread. An explicit
`threadId` that is not the selection gets no template, so one thread's
model, effort, or sandbox policy can never leak into another thread's turn.

## Turn settings after a restart, with no template

The template only exists once the app has sent a `turn/start` in this
process. After an app restart the bridge starts with none. That is not a problem,
because the app-server resolves an omitted parameter from thread state, and that
was verified rather than assumed.

Probe: against an isolated `CODEX_HOME`, a thread was created with
`model: "gpt-5"`, `cwd`, `approvalPolicy: "never"`, and a
thread-level `config.model_reasoning_effort: "high"` while the app-server's
global default was `low`. A `turn/start` was then sent with only
`threadId` and `input`. The app-server's own
`turn_context` record for that turn came back as:

~~~json
{"cwd":"C:\\Users\\qkrgu","approval_policy":"never","sandbox_policy":{"type":"read-only"},
 "permission_profile":{"type":"managed", "...": "..."},
 "model":"gpt-5","personality":"pragmatic","effort":"high",
 "collaboration_mode":{"mode":"default","settings":{"model":"gpt-5","reasoning_effort":"high"}}}
~~~

So cwd, approval policy, sandbox policy, permission profile, model, reasoning
effort, and personality all come from the thread, not from the turn parameters and
not from the global default. The effort is the decisive field: the global default
was `low` and the turn ran at `high`.

What cannot be recovered is the two per-turn extras the app attaches itself:
`additionalContext` and `responsesapiClientMetadata`. They are not
thread state and appear in no thread read. Without a template an injected turn
simply has no extra app context, which is a fidelity gap rather than a failure.

For the same reason the bridge does not synthesize a template from
`thread/read` or `thread/resume` responses. `thread/read`
returns only the thread object, with no model, effort, approval policy, or
personality at all. `thread/resume` does return them, but they reflect the
resume request's own nulls rather than the thread's stored settings, so a template
built from it would silently downgrade the turn. Neither is authoritative, and
omitting the fields is.

Request ids use the `ocx-force-submit-` prefix, which cannot collide with
the app's `<method>:<uuid>` ids.

## Selected thread

The bridge tracks the thread the UI has selected, and only from selection signals
the app-server has actually answered:

- a successful response to `thread/resume`, `thread/read`,
  `thread/items/list`, or `thread/turns/list`, which each name the
  thread they read
- the response to `thread/start`, `thread/startAeon`, or
  `thread/fork`, which creates the task the operator is now in
- `thread/unsubscribe` clears the pointer when it names the current thread

The request alone is not a signal. After a restart the app rehydrates its last
selection from its own persisted state, so the first read it issues can name a
thread this app-server session has never seen, and the backend answers that with
`thread not found`. The bridge holds the thread as a candidate keyed by request
id and promotes it only when the matching response comes back without an error.
A failed response promotes nothing, so a force-submit in that state answers
`no-active-thread` instead of injecting a turn the backend rejects. A late
response for a read issued before the current selection is ignored, so a slow
hydration read cannot move the pointer to a thread the operator already left.

A `turn/start` is deliberately not a selection signal. The app issues
turns for subagent threads too, so treating a turn as a selection would submit the
operator's message into a thread they are not looking at. When no selection has
been observed, a force-submit returns `no-active-thread` instead of
guessing; pass `threadId` explicitly to submit anyway.

Threads the app itself treats as background are never selectable: `ephemeral:
true` or `source.subAgent.thread_spawn.parent_thread_id` set. If a
hydration read reaches a subagent thread before its own `thread/started`
reveals what it is, the selection is retracted at that point.

`thread/prewarm` is a separate method and is not tracked: a prewarmed
thread is not a thread the operator selected.

This is still a protocol-level view, so a caller that can read the visible task
should pass `threadId` explicitly.

Note that the thread object's `source` field is a string in some responses
and an object in others, so it is decoded leniently. Decoding it strictly makes
the whole thread object fail to parse and silently drops the thread id.

## Lifetime

The app stops the bridge with `TerminateProcess`, which runs no cleanup
code here, so the downstream CLI is placed in a Windows job object with
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`. Without that the app-server would be
orphaned on every app restart and would keep the state database busy.

If the job assignment fails the bridge kills the child it just started and exits
without serving. Running without the guarantee is worse than not running: every
app restart would leave another app-server holding the state database.

## Verification status

Verified against the installed app's own app-server, driven directly over stdio
with no app restart:

- line-for-line pass-through in both directions, including stderr
- the named-pipe endpoint, the descriptor, and the client flags
- the downstream child receiving `CODEX_CLI_PATH` set to itself and
  `OCX_CODEX_DOWNSTREAM_CLI` preserved, with the same arguments
- chaining through the installed WebGPT wrapper with no recursion
- a `turn/start` alone leaving the selection empty, and a force-submit in
  that state answering `no-active-thread` rather than guessing
- `thread/start` and then `thread/read` tracked as the selected
  thread
- the restart regression, as a protocol test: a `thread/read` request
  alone selects nothing, the matching `thread not found` response selects
  nothing, a later read whose response succeeds selects that thread, and a
  force-submit then injects into it; an explicit `--ocx-thread-id` this session
  has not answered for is refused instead of injected. `go test ./...` in this
  directory covers that along with the late-response and auxiliary-read cases.
- the pipe DACL resolving to the current user's SID plus SYSTEM and Administrators
- an injected `turn/start` accepted by the app-server, answered with a
  real `turnId`, with `turn/started`, `item/started`,
  `item/completed`, and `thread/status/changed` forwarded to the
  caller unchanged
- the forced text arriving as a `userMessage` item in the thread, which is
  the same item type the app renders
- an injected turn with no template inheriting cwd, approval policy, sandbox
  policy, permission profile, model, reasoning effort, and personality from the
  thread, confirmed from the app-server's own `turn_context` record

Not verified: the app's own rendering of the injected turn, and the assistant
reply for it. Both need the app running with the bridge active, which requires the
activation step and one app restart. In the isolated test home the turn reached
the model call and failed there with `401 Unauthorized`, because that home
has no login; the protocol path up to and including turn creation and the user
message item is what was exercised.
