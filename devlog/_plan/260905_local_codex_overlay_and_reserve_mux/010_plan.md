# Local Codex Overlay + Reserve Mux Completion Plan

Status: CLOSED / HISTORICAL — FEATURE RETIRED FROM PRODUCTION ON 2026-09-09

Last reconciled: 2026-09-09

> Retirement note: this document is retained as the implementation/E2E receipt for the former local
> Overlay + Reserve Mux feature. The active product no longer uses these selection surfaces. Model
> selection is now authoritative through the official Codex model picker, while OpenCodex continues
> to provide the routed catalog and transport for picker-visible provider/model slugs.

## Goal

Finish the local Codex model-selection UX already present in the dirty tree without introducing a
second routing authority or a fragile Windows startup path.

The feature has two related but separate surfaces:

1. **Reserve mux** — when Codex Desktop emits the bare `gpt-reserve` fallback, OpenCodex may map that
   exact fallback to one configured routed model while leaving every other request unchanged.
2. **External overlay** — a Windows overlay attached to the ChatGPT/Codex window lets the operator
   select the current routed model and reasoning effort, persisted through the existing management API
   and applied by the normal Responses routing path.

Correctness and lifecycle ownership matter more than startup cleverness.

## Current implementation state

### Implemented

- `src/router.ts`
  - exact bare `gpt-reserve` mux trigger;
  - explicit provider/model override through `codexReserveRoute`;
  - native OpenAI behavior preserved when no override exists.
- `src/types/config.ts` / `src/config.ts`
  - `codexReserveRoute`;
  - `codexOverlayRoute`;
  - `codexOverlayEffort`.
- `src/server/management/config-routes.ts`
  - settings GET/PUT for reserve and overlay state;
  - route validation through the actual router;
  - lightweight overlay-state read path;
  - active Desktop thread-title resolution into per-conversation overlay state.
- `src/server/responses/core.ts`
  - per-conversation overlay route override on primary non-compaction turns;
  - per-conversation overlay reasoning-effort override before normal effort policy.
- `src/tray/windows-tray.ps1`
  - reserve-model menu and authenticated settings writes.
- `src/overlay/windows-overlay.ps1`
  - top-level ChatGPT/Codex window tracking;
  - bounded helper UIAutomation probing;
  - model picker and reasoning effort controls;
  - synchronous settings persistence before dispatch;
  - single-instance/stop-event mechanics;
  - denylist surface policy: Settings and ordinary WebChat hidden, Codex and ChatGPT/Work visible;
  - active thread-title change detection and state refresh;
  - Windows PowerShell 5.1 compatibility through UTF-8 BOM + `WindowsBase` reference.
- `src/overlay/session-routing.ts` / `src/overlay/codex-thread-resolver.ts`
  - opaque per-conversation route/effort bindings instead of one global data-plane override;
  - exact active Desktop thread resolution through the visible Work/Codex thread title and
    `state_5.sqlite`, failing closed on ambiguous duplicate names;
  - immediate exact-thread binding when the overlay can resolve the current thread, while preserving
    the next-main-turn pending path only as a fail-safe fallback;
  - persisted bindings reload after proxy restart.
- `src/overlay/windows-task.ts`
  - canonical `OpenCodexOverlay/v1` owner marker and exact `OPENCODEX_HOME` binding;
  - create/current/stale-path-repair/start/stop/remove lifecycle;
  - predecessor-CAS registration/removal and fail-closed foreign/unknown handling;
  - Task Scheduler canonicalization support for a self-closing `<LogonTrigger />`.
- `src/service.ts` / `src/cli/system-command.ts` / `src/cli/index.ts`
  - service wrapper calls the repository lifecycle owner instead of raw `schtasks /run`.
- focused reserve/overlay-state regression coverage exists in `tests/codex-reserve-mux.test.ts`.

### Final live receipts — 2026-09-09

- Installed lifecycle implementation was exercised against the real Windows Task Scheduler using a
  unique `OpenCodexOverlay-E2E-<guid>` task name and a temporary `OPENCODEX_HOME`; the fixed production
  task was not mutated.
- Clean create → **`created`**, live classification → **`current`**.
- Second ensure → **`current`** with no registration rewrite.
- Start #1 → exactly **1** isolated Run process.
- Start #2 with `IgnoreNew` → still exactly **1** isolated Run process.
- Stop → **`stopped`**, isolated Run count → **0**.
- Remove → **`removed`**, live query → absent.
- Injected stale action/working path → **`owned-stale-path`**; ensure → **`repaired`**; live
  classification → **`current`**; remove → **`removed`**.
- Markerless foreign GUID registration classified **`foreign`**. `ensure`, `start`, `stop`, and
  `remove` all refused it without mutation. Captured registration hash
  `a26d1ca233299eff6a9177ff8c24b0473488f7be2b8166d0aec804718a63c665` remained unchanged until a
  separate exact-predecessor CAS cleanup removed only the test registration.
- The live E2E exposed and closed two Windows-only defects before acceptance: Task Scheduler rewrites an
  empty enabled logon trigger to `<LogonTrigger />`, and the canonical Windows PowerShell 5.1 action
  requires both a UTF-8 BOM and a `WindowsBase` reference. A direct PS5 canary then stayed alive and
  exited cleanly through `-Mode Stop` before the final Scheduler matrix passed.
- Session-scoped overlay display was verified live across two real Work threads: switching between
  bound threads returned different route/effort state and switching back restored the original state.
- Final preservation receipt: production config SHA256
  `839376ADF7C8A8B7E4B6552164535C59CE36E4A28152DC01D80AE1A0D719A282`; existing fixed markerless
  `OpenCodexOverlay` XML SHA256
  `588992DFC18280B4E605D3195A76FCBCC8B568D773A926C4862308F394A8EC88`; zero E2E task remnants;
  proxy PID `32636` health **200**; production overlay Run count **1**.
- Final focused lifecycle/surface tests, typecheck, and `git diff --check` are green.

### Post-closeout runtime hotfix — 2026-09-09

- A live user interaction exposed an additional overlay-only fault while changing reasoning effort:
  `HttpWebRequest.GetResponse()` could exceed the old 1.6-second write timeout, and
  `Save-OverlaySelection` logged the failure and rethrew it from the WinForms event callback. The
  resulting unhandled callback exception produced a Microsoft .NET dialog even though the proxy itself
  remained healthy.
- The overlay now gives user-driven `/api/settings` writes a 3-second budget, never rethrows transport
  failures into the WinForms event loop, and performs a bounded 1.2-second current-thread
  `/api/overlay-state` reconciliation because a timed-out client response may still follow a committed
  server write. If reconciliation also fails, the previous visible route/effort is restored.
- Reasoning-effort and model-click handlers have an additional top-level exception boundary so future
  callback faults are logged instead of becoming process-wide WinForms unhandled exceptions. Model
  checkmarks are derived from the committed/reconciled route rather than the attempted click.
- Regression coverage for timeout containment is green (**4/4** overlay surface-policy tests),
  typecheck and `git diff --check` pass, and the canonical Windows PowerShell 5.1 Probe canary exits 0.
- The hotfix was copied to both the installed package and the active legacy overlay asset; repo,
  installed, and legacy script SHA256 are
  `A9CB1E1247FDA826C588B8D0943DA66CEF6E06353F0C6F6E788D8E7FAEAA2A02`, with UTF-8 BOM preserved.
  Only the overlay was restarted: old PID `18276` exited and one new Run process (`1396`) started;
  proxy health remained **200**. The fixed markerless task XML SHA256 remained
  `588992DFC18280B4E605D3195A76FCBCC8B568D773A926C4862308F394A8EC88`.

The production fixed-name `OpenCodexOverlay` remains a pre-existing **markerless foreign** task by
design. The new owner refuses to take it over or delete it. Canonical lifecycle implementation and
isolated live E2E are complete; adopting the canonical owner at the fixed production name would require
an explicit migration decision for that legacy task and is intentionally outside this closeout.

## Invariants

1. `gpt-reserve` is the only bare model id affected by the reserve mux.
2. Unset reserve override means native reserve behavior, not an implicit routed default.
3. Overlay selection is persisted before the next request can depend on it.
4. Compaction/internal turns are not accidentally rerouted by an overlay UI choice.
5. One installed OpenCodex instance owns at most one overlay scheduled task and one overlay process.
6. Install/update/repair is idempotent.
7. Stop/uninstall leaves no active overlay process and no stale scheduled task owned by OpenCodex.
8. Task launch must use the installed script path, not a development checkout path.
9. User-owned unrelated Scheduled Tasks are never modified.
10. Failure to start the overlay must not prevent the proxy/service from starting.

## P1 — Scheduled Task lifecycle owner

Add one canonical lifecycle owner for `OpenCodexOverlay`.

Required operations:

- install/create;
- update/repair when executable/script path changes;
- validate task action, arguments, working path, and ownership markers;
- start on demand;
- stop;
- uninstall/remove only when ownership is proven.

Prefer the repository's existing trusted Windows elevation / ScheduledTasks primitives rather than
building a second ad-hoc PowerShell elevation path.

Graduation:

- a clean install creates exactly one correct task;
- a second install/repair is a no-op except for genuine drift;
- a poisoned/foreign task with the same name is refused or handled without destructive takeover.

## P2 — Service and tray integration

- Replace the current unverified fire-and-forget assumption with a lifecycle call whose failure is
  observable but non-fatal to the proxy.
- Keep service startup ordering deterministic: overlay launch must not race config installation.
- Ensure tray/manual selection and overlay selection converge on the same management settings.
- Ensure shutdown/uninstall signals the overlay stop event before removing owned task state.

Graduation: repeated service start/stop cycles never multiply overlay processes or task entries.

## P3 — Windows live E2E — COMPLETE

Run a bounded local lifecycle matrix:

1. clean install/repair;
2. service start;
3. verify one overlay process/task;
4. select a routed model and effort;
5. verify management state changed;
6. send one representative request and verify route/effort ownership at the proxy;
7. restart service and verify one overlay instance returns;
8. stop service and verify overlay exits;
9. uninstall/cleanup and verify the owned task is gone.

Also test a stale-task-path repair case and a foreign-task collision case.

## P4 — Documentation and final gate

- Add the overlay/reserve lifecycle to `structure/` only after the task owner is implemented and
  verified; do not document an installer that does not exist.
- Keep reserve mux and overlay semantics distinct in docs even though they share settings plumbing.
- Run focused tests, typecheck, and the changed-test gate appropriate to the final diff.

## Completion criteria

Complete only when:

- reserve mux regression coverage is green;
- overlay route/effort state is green;
- Scheduled Task install/update/repair/start/stop/uninstall is repository-owned and idempotent;
- a clean Windows lifecycle smoke proves no duplicate overlay process/task;
- failure of the overlay remains non-fatal to the proxy;
- final focused tests, typecheck, and changed-test validation are green.

Closeout note: the accepted final validation for this unit is focused lifecycle/surface coverage +
typecheck/diff-check + real Task Scheduler E2E. The broader repeated strict changed-test cycle was not
restarted solely for this Windows lifecycle closeout after the operator moved release acceptance to
bounded real-runtime gates.
