# OpenCode Go → Codex Harness Parity Plan

Status: COMPLETE — OPERATOR-ACCEPTED RELEASE / INSTALLED-RUNTIME GATE

Last reconciled: 2026-09-09

Planning authority index: `devlog/_plan/000_current_roadmap.md`

## Goal

Make OpenCode Go models behave in Codex through the same upstream transport choices that the
OpenCode agent uses, while keeping execution capabilities owned by the Codex harness wherever
Codex already provides them.

Priority order:

1. Correctness, semantic preservation, and complete functionality.
2. Failure isolation and deterministic behavior.
3. Performance, simplicity, and maintenance cost.

## Current implementation status

The current dirty tree has already crossed most of the implementation phases originally described by
this plan. Treat the checklist below as authoritative over the older future-tense wording later in the
document.

- **P1 curated manifest — IMPLEMENTED.** `src/providers/opencode-go.ts` owns the exact curated
  OpenCode Go set: 4 Responses + 15 Chat + 8 Anthropic = 27 models.
- **P2 catalog intersection — IMPLEMENTED.** Successful live discovery is filtered to curated ids;
  non-curated raw rows do not become picker rows; discovery failure retains bounded curated fail-soft
  behavior rather than treating the raw roster as compatibility authority.
- **P3 transport selection — IMPLEMENTED.** Responses defaults derive from the curated Responses set,
  Anthropic hard pins derive from the curated Anthropic set, and Chat remains provider-default.
- **P4 Codex-owned standalone search — IMPLEMENTED / LIVE E2E PASS.** The injector probes the installed
  Codex feature registry, adds only marker-owned `standalone_web_search` capability when supported,
  preserves explicit user values, and removes the owned setting on restore. Codex 0.153.4 exposes
  standalone `web.run` out of band rather than serializing it in Responses `tools[]`; OpenCodex admits
  only the exact flattened `web__run` name when the resolved provider is `opencode-go`, `originator`
  matches an exact known Codex surface, and active `[features].standalone_web_search = true`. Every
  other undeclared tool remains fail-closed. Fresh-thread live search canaries passed on representative
  Responses (`grok-4.6`), Chat (`glm-5.3-flash`), and Anthropic (`qwen3.8-flash`) routes.
- **P5 hotfix retirement — IMPLEMENTED / FINAL AUDIT PASS.** Muse-specific hosted-search stripping was
  removed; public Responses `additional_tools` handling is generic rather than model-specific. Final
  dirty-diff audit found no remaining OpenCode Go/Muse compatibility branch that overlaps the curated
  transport map or Codex-owned standalone search; retained catalog/search branches cover distinct
  construction/ownership paths rather than double-normalizing one request.
- **OpenCode Go session contract — IMPLEMENTED / FOCUSED PASS.** Every inference route to the fixed Go
  destination requires a stable per-conversation identity. Responses, native Chat, translated
  Anthropic Messages, and Responses WebSocket carry that identity into the same route-time authority;
  the upstream receives only a 32-character SHA-256 digest as `x-opencode-session`, never the caller's
  raw thread/session value. An explicit inbound `x-opencode-session` is accepted as an identity source
  and normalized the same way. A Go turn with no stable identity fails closed with HTTP 400 before any
  upstream request. The transport identifies itself as `User-Agent: opencodex` unless the operator
  supplied a user agent.
- **P6 validation — COMPLETE UNDER THE FINAL RELEASE GATE.** The operator explicitly retired repeated
  full strict-shard reruns as the release blocker after Windows-only teardown/runner flakes had already
  been narrowed with focused reruns. The accepted closeout gate is therefore the final focused release
  matrix, typecheck/diff-check, selective installed deployment, and installed-runtime protocol smoke.
  This document does **not** relabel the abandoned 16-shard strict cycle as fully green.

### Final closeout receipts — 2026-09-09

- Final focused release gate before selective deployment: **277 passed / 1 skipped / 0 failed**.
- Final static gates: typecheck **PASS** and `git diff --check` **PASS**.
- Selective installed deployment completed with repo/install SHA equality verified for the intended
  runtime delta before restart; the proxy was then restarted in place and returned healthy.
- Installed Responses OpenCode Go smoke: **HTTP 200 / `response.completed` / `OK`**.
- Installed Chat OpenCode Go smoke: **HTTP 200 / `OK`**.
- Installed Anthropic Messages OpenCode Go smoke: **HTTP 200 / `OK`**.
- Installed WebSocket Responses smoke in an isolated `websockets:true` home: **`response.completed` /
  `OK`**. Production `websockets:false` was preserved rather than changed for the canary.
- Isolated stable-session canary on the installed package: identity absent → **HTTP 400 before
  upstream**; explicit `x-opencode-session` → **HTTP 200 / `OK`**.
- A fresh production Codex thread executed standalone `web_search` successfully. Because the user's
  active production overlay route intentionally overrides the requested model, that production canary
  is recorded only as Codex-owned search execution evidence; the OpenCode-Go-specific `web__run`
  authority remains pinned by the focused E2E/authorization tests rather than overstating the live
  route.
- The repeated strict V4 cycle is retained as diagnostic evidence, not claimed as a final all-shard
  pass: shard 1 exact rerun **870 pass / 12 skip / 0 fail**, shard 2 **970 / 3 / 0**, and shard-3
  failures were narrowed by focused reruns to non-reproducible Windows teardown/fixture behavior before
  the operator moved the release gate to installed-runtime validation.

Adjacent local work such as the `gpt-reserve` mux and Windows overlay is intentionally tracked in
`devlog/_plan/260905_local_codex_overlay_and_reserve_mux/010_plan.md`; it is not a parity completion
criterion for this unit.

The implementation must not guess model behavior from family names or from the raw OpenCode Go
`/models` roster. The OpenCode curated compatibility mapping is the protocol authority; live model
discovery is availability evidence only.

## Non-goals

- Do not build a second general capability framework.
- Do not run model capability probes on every startup or request.
- Do not duplicate adapter factories or transport logic.
- Do not preserve recent hand-written OpenCode Go compatibility hotfixes when the new canonical
  route makes them unnecessary.
- Do not alter unrelated dirty-tree work.

## Source-of-truth model

Two independent facts are combined:

1. **Curated compatibility**: models that OpenCode explicitly supports and the upstream protocol
   OpenCode uses for each model.
2. **Live availability**: models currently advertised by OpenCode Go `/models`.

Effective picker membership is the intersection of those facts. A raw live row outside the curated
set is not promoted into the Codex catalog merely because the gateway advertised it.

The runtime does not infer capabilities from successful/failed probes. Live probes belong in
release/regression validation, not in production route selection.

## Transport architecture

Reuse the existing OpenCodex transport primitives. No new routing framework is required.

### OpenAI Chat Completions

The `opencode-go` provider remains `openai-chat` by default. Models documented by OpenCode on the
Chat endpoint inherit that provider-wide adapter.

### OpenAI Responses

Models documented by OpenCode on `/responses` use the existing exact-model `modelWireDefaults`
mechanism. Explicit user `modelAdapters` overrides retain their existing precedence where the
current resolver permits them.

### Anthropic Messages

Models documented by OpenCode on `/messages` use the existing provider/model hard-pin mechanism in
`src/types/wire.ts`. These are transport facts, not optional user preferences, because the selected
OpenCode Go endpoint speaks the Anthropic wire for those exact models.

No adapter implementation is copied from OpenCode. OpenCode supplies the transport choice; the
existing OpenCodex adapter owns translation, streaming, tool calls, cancellation, errors, images,
reasoning replay, and result reconstruction for that wire.

## Catalog policy

OpenCode Go `/models` is not a compatibility authority.

The OpenCode Go provider contributes a curated supported-model set derived from the official
OpenCode Go mapping. Live discovery may:

- confirm a curated model is currently available;
- hide a curated model that is definitely absent from a successful live roster;
- preserve the existing fail-soft/offline behavior when live discovery itself is unavailable.

Live discovery must not add arbitrary non-curated OpenCode Go ids to Codex pickers. Known raw rows
that are unsupported, unavailable, or on a different undocumented transport therefore disappear
without a growing compatibility-exclusion blacklist.

## Codex tool ownership

Execution capabilities should stay in the Codex harness when Codex already owns them:

- shell / exec;
- filesystem and patch execution;
- MCP and deferred tool discovery;
- local/first-party standalone search when the installed Codex build supports it;
- other Codex-owned extension tools already dispatched by the client.

The provider model chooses a tool; Codex remains the execution and approval authority.

### Standalone web search migration

Do not infer standalone-search support from a hard-coded Codex version. The current implementation
reads the installed runtime's feature registry and fails closed when the feature is absent, removed,
malformed, or the probe fails.

Codex 0.153.4 does not serialize standalone `web.run` into Responses `tools[]`. A routed model can
therefore return the flattened client-tool name `web__run` even though the ordinary declared-tool set
does not contain it. OpenCodex treats that exact name as an implicit Codex-owned tool only when all
three facts agree: the resolved provider is `opencode-go`, the caller uses an exact known Codex
`originator`, and active Codex config has `[features].standalone_web_search = true`. This authorization
does not add `web__run` to the upstream model tool catalog and does not authorize any other undeclared
tool. Provider/originator/config mismatch and config-read/parse failure remain fail-closed.

Migration gate:

1. **Done:** inject only the minimum marker-owned provider/feature capability required by the installed
   Codex runtime.
2. **Done in config/integration coverage:** prove feature ownership, explicit-user-value preservation,
   fail-closed behavior, idempotent reinjection, and cleanup.
3. **Done:** prove a fresh Codex thread exposes standalone `web.run` through the OpenCodex custom
   provider without serializing it into the routed model's ordinary tool catalog.
4. **Done:** prove end-to-end search call → search result → model continuation on representative
   Responses, Chat, and Anthropic OpenCode Go routes.
5. OpenCode Go does **not** fall through to model-hosted search when standalone capability is absent.
   The production contract is fail-closed for this provider rather than silently changing search
   ownership.

The final production path must fail closed on capability absence; it must not silently fall back to
hosted provider search or convert a cached/no-network request into live external search.

## Recent custom-patch retirement

Recent hand-written OpenCode Go fixes are not compatibility requirements. Once the canonical route
and tool ownership are verified, remove the overlapping custom patches rather than layering the new
system on top of them.

Candidate retirement set includes the recent OpenCode Go/Muse-specific work such as:

- exact Muse wire patches superseded by the curated transport map;
- Muse-only `additional_tools` behavior added solely for the wrong upstream contract; retain the
  generic public-Responses promotion path when it is still protocol-correct for non-canonical public
  Responses providers;
- Muse-only hosted web-search field stripping that is no longer needed on the chosen execution path;
- exact-model web-search conversion workarounds superseded by Codex-owned search;
- other newly-added OpenCode Go exact-model branches whose only purpose was to compensate for an
  incorrect transport choice.

Do **not** remove pre-existing generic OpenCodex protocol bridges or provider-scoped fixes that are
still required by their canonical transport, including generic Responses↔Chat/Anthropic translation,
tool replay normalization, reasoning replay, cancellation, and transport error handling.

Before deletion, identify the recent custom changes from the current dirty-tree diff so unrelated
user work is preserved.

## Implementation phases

### P1 — Curated OpenCode Go manifest — IMPLEMENTED

- Extract the current official OpenCode Go model/endpoint map from upstream OpenCode source/docs.
- Encode one canonical OpenCode Go supported-model manifest in the provider-registry derivation path.
- Keep the representation intentionally small: exact model id + required upstream adapter/wire, plus
  only metadata already needed by existing catalog derivation.
- Add focused tests that pin representative and complete membership/wire mapping.

Graduation: every curated model maps deterministically to one existing adapter; no family-name
heuristics are used.

### P2 — Catalog intersection — IMPLEMENTED

- Change OpenCode Go live discovery augmentation so live rows outside the curated supported set do
  not enter routed picker/catalog output.
- Preserve safe offline/failure semantics: a failed discovery must not erase the static curated
  fallback solely because the network probe failed.
- Remove redundant compatibility exclusions that become unnecessary only when tests prove the new
  intersection owns them.

Graduation: a successful live roster can remove unavailable curated rows but cannot invent a new
supported OpenCode Go row.

### P3 — Correct transport selection — IMPLEMENTED

- Populate Responses exact-model defaults from the curated mapping.
- Populate Anthropic hard pins from the curated mapping.
- Leave Chat models on the provider default.
- Verify inbound Responses → upstream Chat, Responses, and Anthropic paths separately.
- Preserve user override semantics where they are intentionally supported; immutable upstream-wire
  facts remain hard-pinned.

Graduation: representative live requests reach the same endpoint family that OpenCode itself uses.

### P4 — Codex-owned tool canary — IMPLEMENTED / LIVE CANARY PASS

- Extend the generated `[model_providers.opencodex]` capability only as required by the installed
  Codex standalone-search contract.
- Add focused config-injection tests proving ownership and cleanup are reversible.
- Enable the under-development feature only through an explicit OpenCodex-managed path that can be
  disabled if the installed Codex build does not support it.
- Admit only the exact implicit `web__run` client-tool name under OpenCode Go + exact Codex originator
  + active standalone-feature proof; never widen the general undeclared-tool guard.
- Run fresh-thread canaries for Responses, Chat, and Anthropic OpenCode Go representatives.

Graduation: search is executed by Codex and returns a usable result to the routed model without the
provider needing native hosted-search semantics.

### P5 — Retire overlapping recent hotfixes — IMPLEMENTED / FINAL AUDIT PASS

- Diff the current dirty tree against the installed/upstream baseline.
- Remove only recent OpenCode Go-specific hotfixes superseded by P1–P4.
- Keep generic adapter compatibility code still required by the correct wire.
- Re-run focused regression tests after each logical removal group.

Graduation: no double-normalization path remains for OpenCode Go.

### P6 — Validation matrix — COMPLETE UNDER FINAL RELEASE GATE

Protocol-level fixtures:

- Responses representative: basic stream, reasoning, ordinary function tool, tool-result replay.
- Chat representative: same contract through Responses→Chat translation.
- Anthropic representative: same contract through Responses→Anthropic translation.
- catalog membership and successful-live-roster intersection;
- failed/malformed discovery fail-soft behavior;
- standalone search injection/cleanup, exact implicit `web__run` authorization/fail-closed scope, and
  E2E canary where the local Codex build supports it.

Live smoke tests for every curated OpenCode Go model should be small and bounded:

- basic generation;
- ordinary tool call or an explicit unsupported classification;
- reasoning level when advertised;
- image only where advertised;
- standalone search contract once P4 is enabled.

Release/runtime routing must not depend on these live probes.

## Invariants

1. No raw `/models` row becomes supported without curated compatibility evidence.
2. One exact model resolves to one upstream transport before adapter execution.
3. Adapter construction remains owned by `src/adapters/registry.ts`.
4. OpenCode compatibility data does not import Compatibility Lab into the core request path.
5. Codex client tools remain client-executed; the proxy must not execute shell/filesystem actions
   merely because a routed model generated the tool name.
6. Standalone search is advertised, and implicit `web__run` is admitted, only when the proven
   OpenCode Go + exact Codex originator + active-feature contract is satisfied; no other undeclared
   tool gains authority.
7. Search policy must not widen cached/offline intent into live external access.
8. Existing unrelated dirty-tree edits are preserved.
9. Recent local OpenCode Go hotfixes may be deleted once superseded; generic protocol bridges may not.
10. No OpenCode Go inference request may leave the proxy without a stable per-conversation
    `x-opencode-session`; raw caller identities are not forwarded, and identity absence fails closed.
11. Every behavior change receives focused tests before broader changed-test/typecheck validation.

## Validation commands

Implementation-time minimum:

```text
bun test tests/<focused-provider-or-adapter-tests>.test.ts
bun run typecheck
```

For the broader multi-file integration:

```text
bun run test:changed
```

On Windows, when the wrapper/runtime budget makes the unsharded form impractical, use the same
comparison commit through sequential non-overlapping shards and do not bypass the test-home lock:

```text
bun scripts/test.ts --changed=dev --parallel=2 --shard=1/8
...
bun scripts/test.ts --changed=dev --parallel=2 --shard=8/8
```

Run one shard at a time.

Do not run the repository-wide full suite until PR-ready or explicitly requested.

## Completion criteria

The work is complete when:

- OpenCode Go picker membership is curated rather than raw-roster driven;
- every curated model uses the same protocol family OpenCode uses;
- Chat/Responses/Anthropic tool and continuation paths pass focused regression coverage;
- Codex-owned standalone search is advertised for OpenCode Go only when the installed runtime proves
  the capability; exact implicit `web__run` authorization is bounded to OpenCode Go + exact Codex
  originator + active feature, capability/authority absence fails closed, and no hosted-search fallback
  or general undeclared-tool widening occurs;
- every OpenCode Go inference wire sends one opaque stable `x-opencode-session`, including translated
  Anthropic and WebSocket turns, while identity-less requests are rejected before upstream dispatch;
- recent overlapping local hotfixes are removed;
- focused release tests, typecheck/diff-check, and installed-runtime protocol smokes are green. The
  final full `test:changed` strict cycle was explicitly superseded as the release acceptance gate and is
  not falsely recorded as fully green.
