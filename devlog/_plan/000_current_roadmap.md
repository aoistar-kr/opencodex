# OpenCodex Current Roadmap

Last reconciled: 2026-09-09

This file is the planning authority index for `devlog/_plan/`.

The directory contains a mixture of live plans, partially completed programmes, design records,
merge/release trains, investigations, and historical execution logs. A directory being under
`_plan` does **not** by itself mean that its instructions are still executable.

## Planning authority order

When documents disagree, use this order:

1. current repository/source state;
2. `structure/` source-of-truth documents;
3. this roadmap index;
4. the currently active plan listed below;
5. residual/deferred plans after a fresh tree audit;
6. historical `_plan` documents.

Do not implement an old dated plan verbatim just because it still lives under `_plan`.

## 1. RECENTLY CLOSED — NO REMAINING 260904/260905 RELEASE BLOCKER

### `260904_opencode_go_codex_harness_parity`

Status: **closed under the operator-accepted focused release + installed-runtime gate**.

Current source already contains the core architecture the plan originally proposed:

- curated OpenCode Go compatibility manifest: 27 models total;
- deterministic Responses / Chat / Anthropic wire ownership;
- live-roster filtering against the curated set;
- fail-soft catalog behavior when discovery itself is unavailable;
- installed-Codex `standalone_web_search` feature probing and marker-owned config injection;
- OpenCode Go hosted-search metadata removal when Codex owns standalone search;
- retirement of Muse-only compatibility behavior that is no longer protocol-authoritative;
- stable per-conversation `x-opencode-session` on every OpenCode Go inference route, using an opaque
  digest and failing closed before upstream when no stable identity exists.

Closeout evidence:

1. focused release gate **277 pass / 1 skip / 0 fail**;
2. typecheck and `git diff --check` **PASS**;
3. selective installed deployment completed and the proxy restarted healthy;
4. installed Responses / Chat / Anthropic / WebSocket OpenCode Go smokes passed;
5. installed stable-session fail-closed canary passed (no identity → 400; explicit identity → 200);
6. standalone Codex web search executed live; Go-specific implicit-tool authority remains separately
   proven by focused E2E, and the production picker path now reaches routed OpenCode Go models
   directly without an overlay override.

Canonical plan: `260904_opencode_go_codex_harness_parity/010_plan.md`.

The abandoned strict V4 cycle remains diagnostic history and is not rewritten as a complete 16-shard
pass. The release gate was explicitly moved to bounded focused + installed-runtime validation after
non-reproducible Windows runner/teardown failures were isolated.

### `260905_local_codex_overlay_and_reserve_mux`

Status: **closed and retired from production; historical lifecycle implementation remains a receipt**.

Closeout evidence:

- repository-owned create/current/repair/start/stop/remove lifecycle exists with owner/home validation,
  predecessor CAS, stale-path-only repair, and foreign-task fail-closed semantics;
- real GUID-isolated Task Scheduler E2E passed clean create, idempotent ensure, start×2 with one Run,
  stop, remove, stale-path repair, and markerless-foreign preservation;
- live E2E exposed and fixed Task Scheduler `<LogonTrigger />` canonicalization plus Windows PowerShell
  5.1 UTF-8-BOM/`WindowsBase` compatibility;
- the lifecycle and per-conversation routing work was completed and validated at the time of closeout;
- on 2026-09-09 the operator retired the external overlay and reserve-mux control surfaces so the
  official Codex model picker is the sole model-selection UI;
- `codexOverlayRoute`, `codexOverlayEffort`, and `codexReserveRoute` were removed from active config/
  management/data-plane code, `src/overlay/` was retired, tray/service overlay hooks were removed,
  and the old markerless `OpenCodexOverlay` task plus active overlay assets/state were explicitly removed;
- the Windows service wrapper was repaired without the old `system overlay-task start` line; proxy health
  remained 200, and a direct production request using the picker-visible
  `opencode-go/muse-spark-1.3-contributor` slug completed with HTTP 200 / `OK`.

Canonical plan: `260905_local_codex_overlay_and_reserve_mux/010_plan.md`.

## 2. PARTIALLY IMPLEMENTED / RESIDUAL ONLY

These plans contain useful remaining intent, but the current tree has moved far beyond the baseline
they describe. They are **not executable as written**. Before resuming one, first rewrite its
current-state section against the live tree and delete or retire already-landed phases.

### `260814_usage_memory_roadmap`

Status: **partially implemented, stale baseline**.

Examples already present in the current tree include pre-dispatch input admission and later transport/
memory hardening. The original P1 segmented-writer / SQLite-projector chain is not represented by the
current `src/usage/` layout and therefore remains at most a future redesign candidate, not an active
stack to execute blindly.

### `260817_windows_stability_program`

Status: **partially implemented, residual programme**.

Its own execution record says phases 010/020/030/031 shipped while later phases remained open. Many
subsequent Windows trains changed the same surfaces, so any remaining 040-090 work must be re-audited
against current CI/service/update behavior before implementation.

## 3. FUTURE / DEFERRED / BLOCKED

### `800_agent-fabric`

Status: **deferred; FAB-01 production authority not granted**.

FAB-00 research and spikes produced useful architecture evidence, but `170_fab01_authority_or_block.md`
still explicitly withholds FAB-01 authority. No production Fabric task kernel, handoff supervisor, or
Task Inspector should be started from this programme until its acceptance/governance prerequisites are
re-run against the current repository and explicit execution authority is granted.

### `260801_monorepo_git_blobless_strategy`

Status: **low-priority operational proposal**.

Research is complete. The remaining work is optional contributor/CI documentation and checkout
optimization, not a product-runtime priority.

## 4. HISTORICAL / IMPLEMENTED / SUPERSEDED

Every other dated directory currently under `devlog/_plan/` is **non-authoritative by default**.

This bucket includes completed implementation records, issue/PR merge trains, release-readiness trains,
probe campaigns, one-off hotfix plans, and designs whose target behavior now exists in the current tree
or was superseded by later work. Examples confirmed during this reconciliation:

- `260824_model_ux_aliases_and_defaults`: the old doc says design-only, but the current tree now has
  provider aliases, model aliases, new-model policy state, and shipped latest/core presets;
- `260826_config_rebase_provenance`: current source contains `src/config/rebase-provenance.ts` and
  persisted `configRebaseProvenance` handling;
- `260826_pre_substrate_adoption`: current transition state supports `adoption-pending`;
- `260826_session_lane_bounds`: current lifecycle code and recall harness contain bounded session lanes;
- `260827_kiro_builder_id_profile`: current source contains the Builder ID service-profile fallback and
  dedicated regression tests;
- `260813_openai_chat_baseurl_normalize`: current source contains `openai-chat-url.ts` plus tests;
- `260818_megafile_split_program`: the provider-name leaf extraction described by the plan is present;
- `260813_bun_canary_dogfood`: its premise was waiting for Bun 1.4; the repository now uses Bun 1.4.0,
  so the canary migration plan is obsolete even though its memory-audit ideas remain historical context.

If a historical plan appears to contain an attractive unfinished item, create a new dated plan from the
current tree instead of reviving the stale instructions in place.

## Execution order from this point

1. `260904_opencode_go_codex_harness_parity` is closed.
2. `260905_local_codex_overlay_and_reserve_mux` is closed; do not revive its old lifecycle-gap wording.
3. Any next maintenance train should start from a fresh audit of Usage/Memory or Windows Stability.
4. Agent Fabric remains deferred until its explicit authority gate is satisfied.

## Promotion rule

When an active plan completes:

- update its status and receipts;
- move it to the appropriate finished/historical location if the repository convention requires it;
- update this index in the same change;
- never leave a completed plan labelled `IMPLEMENTATION STARTED` or `proposed` when the current tree
  has already crossed that boundary.
