# enter-force-submit

Windows-only helper that keeps Enter usable in the Codex desktop composer while a usage
limit has disabled the send button. It never patches the app: no `app.asar` write, no
Codex app config change, no app-server bridge change, no app restart.

## Behavior

A bare Enter is never swallowed. The helper observes the press, lets the app do whatever
it would have done, and submits through the bridge only when the outcome shows the app
did nothing.

```
bare Enter key-down, no modifier held
  -> the key goes to the app untouched; the hook copies the composer snapshot the poller had
     already resolved into the queue and does no UI Automation work itself
  -> off-hook: take that cached pre-enter snapshot (or resolve one when stale/missing), then wait --debounce ms (default 120)
  -> resolve a fresh post-debounce snapshot
  -> the cached snapshot must be no older than 600ms, must have qualified as a hard block, and
     must match the fresh snapshot exactly
  -> resolve one more immediate fresh snapshot; it must still qualify and still match
  -> call the bridge
  -> anything else
       -> do nothing
```

The raw UI Automation value is what gets submitted, with leading, trailing and embedded
whitespace intact. Trimming is used only to decide whether the composer is empty and whether
Chromium is reporting the placeholder, never for the payload.

The comparison is on outcomes, never on draft characters or the keyboard language:

| observed outcome | helper |
| --- | --- |
| normal send: the composer cleared or the draft changed | nothing |
| a composition commit or any other edit changed the draft | nothing |
| a turn began: the send control turned into a stop control | nothing |
| task or focus switch: window, composer identity or task fingerprint changed | nothing |
| the hard block ended: send enabled, limit text gone | nothing |
| ambiguous or unresolvable snapshot | nothing |
| unchanged hard block, draft untouched | force-submit through the bridge |

Shift+Enter is untouched: the hook ignores any Enter with a modifier held and never
consumes a key, so line breaks and normal sends reach the app by construction.

## Conditions for acting

1. the foreground window belongs to a target process and is a Chromium widget;
2. the focused UI Automation element is an `Edit` - the Codex composer is a ProseMirror
   contenteditable, and the page-root `Document` (whose value is the app URL) is not a
   composer;
3. the draft is not empty, and is not Chromium reporting the placeholder as the value;
4. a send button found within four levels of the composer, carrying the app's own label
   (`Send`, `보내기`, ...);
5. the block is established one of two ways. The bridge's own reply carrying an exact nested
   `usage.limitState` of `confirmed` stands on its own, and overrides an enabled send control.
   Otherwise the send button has to report `IsEnabled = false` and a hard-limit indication has to be
   present: the disabled send button's own help text, or one of the exact strings the app renders
   for a hard block (`Messages limit reached`, `You've hit your usage limit for ...`,
   `메시지 한도에 도달...`, `용량 한도에 도달...`) found within `--scan-depth` levels of the composer;
6. snapshots A and B are identical, the bridge is available, and an immediate final
   recheck agrees.

Anything else is ambiguous, and an ambiguous state does nothing.

## Duplicate suppression

The draft is remembered only when the bridge reports success, and a draft equal to it is
never submitted again. The guard is released only against the exact recorded draft: an emptied
composer releases it, and a clear job releases it only while the recorded draft is still the one
that job submitted, so a guard recorded by a later submit is never dropped by mistake. A rejected
bridge call is not recorded, so the draft can be retried.

## Bridge contract

The bridge owns its AF_UNIX socket and this helper never opens it; it runs the bridge exe
as a thin client so a second socket implementation cannot drift from the bridge's own.

- availability: `<bridge> --ocx-status`, polled every `--status-interval` ms (default
  5000). A non-zero exit marks the bridge unavailable and the helper stops acting.
- submit: `<bridge> --ocx-force-submit "<draft>" [--ocx-model-label "<picker label>"]`, one argv element with Windows
  CreateProcess quoting, so spaces, quotes and newlines survive.
- the reply text and exit code are logged. No `threadId` is sent: UI Automation cannot
  read it, so the bridge resolves the active task itself.
- default bridge path:
  `%LOCALAPPDATA%\OpenCodex\codex-appserver-bridge\ocx-codex-appserver-bridge.exe`
  (`--bridge` to change).

## Post-submit clear

When the bridge reports an accepted submit the helper does not assume the app emptied its own
composer. It queues the submitted composer identity and the exact raw draft, and a dedicated
worker brings the composer back to empty with real key input: a tagged `Ctrl+A` then `Delete` via
`SendInput`. The clipboard, `ValuePattern.SetValue`, page script and app files are never used.

The clear runs only when the composer is still the exact one that was submitted - same foreground
window and process, same focused (composer) runtime id, same task fingerprint, byte-for-byte the
same draft - and no modifier is held. Any mismatch declines and the draft stays recorded, so it is
not resubmitted. That identity and the exact raw draft are re-read once more in the narrowest window
before the keystrokes, and the focus, foreground and modifier state is confirmed again immediately
before the batch; nothing is ever refocused. After sending it polls every 100ms for up to 600ms for
an empty/placeholder composer; it never retries the keystrokes. `SendInput` counts as success only
when every event of the batch was accepted: a partially accepted batch is not replayed, and one
tagged best-effort compensation batch then releases `Ctrl`, `A` and `Delete` so no key stays held
down. On success the duplicate guard is released for that exact draft; on failure the guard is kept.
The injected keys carry a private `dwExtraInfo` tag and the hook ignores any event bearing it.

## Structured usage signal

The helper also reads the bridge's own `--ocx-status` reply. The account usage there is nested, and
only an exact `usage.limitState` of `confirmed` resolves the hard limit:

```
{"usage":{"limitState":"confirmed"}}
```

That confirms the hard limit and lets the expensive limit-text scan be skipped. It also overrides an
enabled send control, because the app can leave its Send button enabled while its own backend
refuses the turn; requiring the disabled control there would leave the helper unable to act at all.
Every other condition still has to hold: a focused `Edit`, a nonempty non-placeholder draft, a send
control that was found, and matching before/after snapshots. The confirmed flag is part of that
snapshot comparison, so a signal that drops between the two reads declines the submit instead of
acting on a stale confirmation.

An absent `usage` object, a `limitState` of `advisory`, `unknown` or anything else, a stray top-level
`limitState`, a reply that is not JSON, an older bridge, a custom provider or an unreachable
app-server all leave the signal unknown. An unknown or advisory signal overrides nothing: the
disabled control plus a live limit indicator are still required, so a missing signal can never widen
what the helper acts on.

## Build and run

```powershell
./build.ps1                              # bin\enter-force-submit.exe via the built-in csc.exe
./run.ps1                                # build if needed, then start hidden
./run.ps1 -Probe                         # dump the live UI Automation tree and exit
./run.ps1 -ProbeEnter                    # live snapshot + the decision sequence, then exit
./run.ps1 -DryRun -Foreground -VerboseLog   # observe and log, never submit
Stop-Process -Name enter-force-submit
```

Log: `%LOCALAPPDATA%\OpenCodex\enter-force-submit\helper.log`.

## Probes

`--probe` prints the live UI Automation tree for the target windows. `--probe-enter` prints a
live snapshot taken through the same capture the observer uses, then a read-only composer
exposure report: the raw draft with its raw and trimmed lengths, every button within four levels
and whether it carries a send label, the nearby text matching a limit phrase, and a plain verdict
on whether the hard block is active right now. It then runs the decision core over the outcomes
above and marks each case as-expected or UNEXPECTED.

When the composer holds no nonempty draft, the app renders no send button, and the report says so
rather than guessing - type a draft and rerun it to read the real send button. When the hard block
is not active the report prints `hard block : NOT ACTIVE` with the reason. Neither probe
installs a hook, types anything, or submits anything.

## Limits

- The send button is matched by exact accessible label within four levels of the
  composer. A substring rule was tried and matched an unrelated control
  ("Force submit review" contains "submit"), so a renamed control now means no match and
  the helper stands down rather than acting on the wrong control. `--send-label` extends
  the label list.
- The limit text scan skips levels holding more than 400 text nodes so it never walks the
  conversation, and defaults to two levels. `--scan-depth` widens it. Broad phrases such as
  `usage limit` were removed from the pattern list because ordinary conversation text contains
  them: a text match must not be able to stand in for a real block.
- The task fingerprint is the nearest document ancestor's accessible name, which is weak
  task evidence; it is used only to detect that something changed.
- One low-level hook exists per desktop: a second instance detects the named mutex and
  exits instead of installing a competing hook.
- An outcome a UIA snapshot cannot distinguish is treated as ambiguous and ignored, so the
  helper can decline to submit. That is the intended failure direction.
