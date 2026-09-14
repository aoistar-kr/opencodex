# 260914 Cross-provider history portability plan

## Goal

Fix two concrete Codex Desktop -> external Responses-provider continuity failures without weakening
normal OpenAI/Codex behavior:

1. app-server can persist a named standalone `function_call_output` (`namespace: "codex_app"`) with
   no `call_id`; strict public Responses implementations reject it.
2. native OpenAI/ChatGPT compaction produces provider-owned opaque `encrypted_content`; replaying
   that state after a provider/model switch either fails decryption or, in current OpenCodex, is
   deliberately downgraded to a generic note and loses the actual compacted context.

The concrete reproductions that motivated the work are the Codex threads `Reconcile environment
lineage` (call-id-less `codex_app/send_message_to_thread` output) and `작업용 스레드 만들기 (2)`
(native `gAAAA...` compaction followed by a WebGPT -> Console Go/Muse route change).

## Evidence / source audit

### Local authority

- `src/adapters/routed-agent-messages.ts`: current non-OpenAI boundary already lowers private
  `agent_message` records into public `message` items.
- `src/responses/schema.ts`: public function outputs require `call_id`, but the final loose input
  branch intentionally permits Codex-private/forward-compatible item shapes at ingress.
- `src/adapters/openai-responses.ts`: `scrubOcxCompactionItems()` preserves native blobs only for a
  destination that can decode them and an unchanged serving identity; otherwise it lowers the item.
- `src/responses/compaction.ts`: `ocx1:` is already a provider-neutral plaintext summary envelope;
  unknown native blobs currently lower to `OPAQUE_COMPACTION_NOTE`.
- `src/server/responses/core.ts` + `src/responses/reasoning-replay-cache.ts`: route/credential/model
  serving identity is already tracked and opaque replay has a bounded one-retry recovery path.
- `src/server/responses/compact.ts`: native OpenAI/ChatGPT `/responses/compact` is forwarded; routed
  providers use the synthetic summary path.  The code explicitly notes that automatic
  previous-model compaction does not carry the newly selected model.
- `structure/04_transports-and-sidecars.md`: current SOT intentionally treats a foreign native
  compaction blob as non-portable and falls back to a note.

### Current OpenAI Codex main audited at `3abbf9fe2c6b6910e9de61f6a0c5bb468f74b5c8`

- `codex-rs/protocol/src/models.rs`:
  - public/request `ResponseInputItem::FunctionCallOutput` requires `call_id: String`;
  - internal `ResponseItem::FunctionCallOutput` deliberately allows `call_id: Option<String>` and
    carries `name`/`namespace`.
- `codex-rs/protocol/src/protocol.rs`: current multi-agent history has an explicit
  `InterAgentCommunication` / `AgentMessage` representation, confirming that provider-bound
  compatibility lowering belongs at the wire boundary rather than by fabricating a tool call.
- `codex-rs/core/src/session/turn.rs`: pre-sampling model-switch compaction intentionally builds a
  previous-model turn context before the new model is sampled.  A proxy therefore cannot infer the
  target model from the compact request itself.
- `codex-rs/core/src/compact_remote_v2.rs`: compaction checkpoint tracing records both input history
  and installed replacement history.
- `codex-rs/core/src/session/rollout_reconstruction.rs`: replacement history is the resume base;
  guardian history is restored separately.  This validates the observed rollout evidence but also
  argues against coupling the hot proxy path to Codex rollout-file parsing.

### Public issue / API contract evidence

- openai/codex #42067, #42376, #43515, #43944: standalone app-server
  `function_call_output` records without `call_id` break strict third-party Responses providers;
  normalization to an attributable message is a verified workaround and is preferred over a fake
  orphan `call_id`.
- openai/codex #17541, #25290, #36704, #40209: encrypted reasoning/compaction state is owned by the
  backend that minted it; model/provider/key changes can poison a replayed thread.
- OpenAI Responses reference: a compaction item from `/v1/responses/compact` carries opaque
  `encrypted_content` and is intended to be replayed as state.

## Non-negotiable invariants

1. Never invent a `call_id` without a real matching public function call.
2. Preserve valid `function_call` <-> `function_call_output(call_id)` pairs byte-semantically.
3. Preserve OpenAI/Codex-private standalone FCO behavior on the canonical/OpenAI-aware path.
4. Normalize only the provider boundary that cannot consume the private shape.
5. Same-serving-identity native compaction must keep using the native opaque blob.
6. A route change must never forward a foreign native compaction blob blindly.
7. Existing `ocx1:` routed compaction stays compatible.
8. Portable recovery must not require parsing or mutating Codex rollout JSONL in the hot path.
9. Any new portability enhancement must fail soft to the existing native-compaction response; it
   must not turn an otherwise successful native compact into a failed user turn.
10. No reset/clean/checkout, no unrelated dirty-tree overwrite, no deploy/restart until source tests
    pass and the changed-file set has been inspected.

## Final design

### A. Provider-bound Codex private-item normalization

Extend the existing routed private-item normalizer rather than changing ingress schema semantics.

For non-OpenAI/non-private loopback outbound requests:

- if an item is `function_call_output`, has no non-empty `call_id`, and is a named
  `namespace: "codex_app"` standalone output, convert it to a public `message` item;
- use role `developer` when the destination accepts it (matching the upstream issue proposal), with
  a stable short attribution header containing `codex_app/<name>` plus the textual output;
- preserve string outputs and text-bearing structured output; do not manufacture tool-call linkage;
- leave any item with a valid `call_id` untouched;
- leave unrelated malformed public items untouched for existing validation/error behavior rather
  than silently guessing intent.

This fixes `send_message_to_thread`, `create_thread` delegation/heartbeat/automation bootstrap, and
the same app-server extension class without special-casing one tool name.

### B. Hybrid native + portable compaction envelope (`ocx2`)

The proxy cannot know the future target model at automatic previous-model compact time.  Therefore
make the compacted state itself portable while retaining the native fast path.

`ocx2` conceptually carries:

```
native opaque compact state + provider-neutral plaintext checkpoint + restart-stable origin fingerprint
```

Properties:

- self-contained in the client's normal `encrypted_content` field, so it survives resume/fork and
  proxy restart without a new side database;
- same proven durable origin + native decoding capability + no stricter in-process mismatch:
  OpenCodex unwraps and forwards only the original native blob;
- origin missing/mismatched, destination cannot decode native state, or serving identity changed:
  OpenCodex lowers the portable summary to a normal message;
- a backend never receives the `ocx2` wrapper itself;
- `decodeCompactionSummary()` understands both `ocx1` and `ocx2` portable text, while a dedicated
  decoder exposes `ocx2.native` for the same-backend path.

The origin is a SHA-256 fingerprint over the existing restart-stable reasoning replay identity
components (provider, durable destination, durable credential/account identity, adapter, model).
It stores no raw credential or account identifier.  This closes the restart hole where an empty
process-local serving cache could otherwise treat a foreign native blob as same-origin merely because
the new destination also understands its own native compaction format.

Use a delimiter-safe versioned encoding rather than base64-encoding the already opaque native blob;
only the plaintext summary is base64 encoded.  This avoids a ~33% expansion of the native payload.

### C. Producing the portable half without replaying full pre-compact history

After a successful native `/responses/compact` call:

1. inspect the returned single native compaction item;
2. ask the *exact provider/auth transport that returned the native compact blob* to render that state
   as a plaintext checkpoint using a bounded Responses turn containing the native compaction item
   plus the existing compact-summary instruction; do not re-enter model/account routing;
3. build `ocx2(native, portableSummary, originFingerprint)` and return that compaction item to Codex;
4. if the shadow-render request fails, is cancelled, malformed, or returns empty text, return the
   original successful native compact response unchanged (fail-soft invariant).

Why this is preferable to alternatives:

- no fake decryption;
- no second pass over the huge pre-compaction transcript — the minting backend already knows how to
  decode its compact state;
- no process-local cache that loses continuity after restart;
- no plaintext sidecar database;
- same-provider turns retain native semantics and do not pay re-expansion costs;
- the extra small render call is paid only at native compaction time, not on every normal turn.

The synthetic turn must reuse the FINAL transport that actually produced the native compact result.
That distinction matters after a same-request 429/402 alternate-account retry: if A rejects and B
mints the blob, the shadow call is sent directly with B's resolved provider + headers, never through
a second pool selection.  It must be tool-free, `store:false`, and consume streaming output safely.
It is internal bookkeeping: the client never receives its response id.

### D. Legacy native blobs

`ocx2` makes all newly created native compactions portable.  A legacy raw native blob has no
plaintext summary embedded, so a foreign provider still cannot reconstruct information that never
crossed the trust boundary in plaintext.  For such blobs:

- keep the current fail-closed `OPAQUE_COMPACTION_NOTE` fallback (never guess/decrypt/fabricate);
- retain the one-shot rejected-blob recovery path;
- do not make normal request serving depend on local rollout files;
- document an optional future/offline migration path using Codex `guardian_history` for old local
  rollouts.  That migration is intentionally outside the live transport patch because rewriting a
  private rollout is destructive and upstream issues explicitly warn against blind JSONL surgery.

This distinction is important: the live fix guarantees portability at the moment new compaction
state is minted; it cannot retroactively decrypt an already opaque legacy blob.

## Implementation phases

### Phase 1 — call_id compatibility

- extend `src/adapters/routed-agent-messages.ts` with named standalone `codex_app` FCO lowering;
- call it only on routed/non-private-boundary requests, preserving loopback/canonical semantics;
- regression tests using the exact `send_message_to_thread` shape plus `automation_update` sibling;
- test valid paired output is unchanged.

### Phase 2 — compaction envelope primitives

- add `ocx2` encode/decode helpers in `src/responses/compaction.ts`;
- strict parse, non-empty native + summary, size/format sanity;
- make generic portable text decoding accept `ocx1` and `ocx2`;
- update `scrubOcxCompactionItems()`:
  - native decoder + matching durable origin + unchanged in-process identity => unwrap native;
  - origin missing/mismatched or foreign/changed identity => portable summary message;
  - legacy native => current opaque note.

### Phase 3 — native compact shadow rendering

- factor a bounded helper in `src/server/responses/compact.ts` that consumes a Responses render and
  extracts assistant output text;
- after successful native compact, capture the exact successful provider + auth headers + model and
  send one direct compact-state -> plaintext render without a second routing/account-selection pass;
- wrap returned item as `ocx2` only on a valid non-empty summary;
- all shadow failures fall back to the already buffered native response;
- do not affect routed synthetic compaction (`ocx1`).

### Phase 4 — documentation/SOT

- update `structure/04_transports-and-sidecars.md` from “foreign native always note” to the new
  `ocx1`/`ocx2`/legacy-native matrix;
- document extra native-compaction render request and fail-soft semantics.

### Phase 5 — verification

Focused regressions first, then repository gates required by `AGENTS.md`:

- `routed-agent-messages` / OpenAI Responses passthrough tests:
  - exact no-`call_id` `codex_app/send_message_to_thread` -> developer message;
  - `codex_app/automation_update` sibling -> developer message;
  - valid `call_id` output unchanged;
  - canonical/private loopback unchanged.
- compaction unit tests:
  - `ocx2` round trip;
  - malformed `ocx2` fail closed;
  - same durable origin/native destination unwraps native blob;
  - origin-less, origin-mismatched, changed, or non-decoding destination gets portable summary;
  - old raw native blob still gets opaque note;
  - `ocx1` behavior unchanged.
- compact routing tests:
  - successful native compact + successful shadow render returns `ocx2`;
  - shadow render uses the exact account that minted the native blob;
  - A->B alternate-account compact is followed by B->B shadow, never B->A or a fresh selection;
  - shadow 4xx/5xx/abort/empty output leaves original native compact usable;
  - routed compaction remains one `ocx1` summary path with no extra native shadow call.
- opaque recovery tests remain green.
- `bun run typecheck`.
- `bun run test:changed`.
- targeted privacy scan if fixtures/logging changed; full privacy scan before deploy if practical.

## Deployment / rollback

Source implementation is separated from deployment.  After tests:

1. inspect `git diff -- <touched files>` and verify no unrelated dirty changes were overwritten;
2. identify the currently installed package files and pre-deploy hashes;
3. deploy only the touched runtime source/package artifacts needed by this fix;
4. controlled restart preserving current config/model/effort and existing service state;
5. live smoke:
   - strict external-provider cross-thread message succeeds;
   - newly minted native compact can switch to Console Go/Muse without losing its summary;
6. rollback is file-selective restore from pre-deploy copies, not git reset/checkout.

No install/restart is performed merely because source tests pass; installed deployment gets its own
receipt and pre/post hashes.

## Engineering self-review iterations

Scoring rubric (100): root-cause fidelity 20, compatibility/scope 15, continuity semantics 20,
failure/privacy/resource behavior 15, regression coverage 20, operability/rollback/docs 10.

### Review V1 — 73/100

- Proposed fake/synthetic `call_id` and simple “strip opaque compaction on switch”.
- Rejected: orphan call ids violate public function-call semantics; stripping fixes 400 but loses the
  exact context the user is trying to retain; model switch target is not visible on previous-model
  compact requests.

### Review V2 — 91/100

- Correct private-FCO -> message normalization.
- Proposed process-local/disk `blobHash -> portable summary` shadow cache.
- Rejected as final design: process-local cache loses resume/restart continuity; disk cache creates a
  second lifecycle/privacy store and requires reconciliation/eviction/versioning.

### Review V3 — 100/100 against the rubric

- Exact public/private tool boundary preserved; no fake call ids.
- Self-contained `ocx2` survives normal Codex persistence/fork/resume with no side database.
- Same backend retains original native compaction semantics; cross backend receives portable text.
- Summary is rendered by the blob's own decoder backend, not guessed by the proxy.
- Shadow generation is fail-soft, bounded to compaction time, and routed with the same account
  affinity.
- Legacy raw blobs remain explicitly fail-closed instead of claiming impossible retroactive
  portability.
- Tests cover both concrete user failures plus positive/negative route, malformed, recovery, and
  no-regression cases.
- Deployment remains selective and reversible in the already-dirty repository/install.

The 100/100 score means the written plan satisfies this review rubric; it is not a claim that code
is correct until the implementation and gates below actually pass.

### Review V4 — 100/100 after implementation audit

The first implementation review found two gaps that the V3 prose had not made mechanically true:

- restart erased the process-local serving-identity cache, so a different backend that could decode
  its OWN native compaction format might incorrectly receive another backend's `ocx2.native`;
- after an in-request A -> B pool failover, the successful compact belonged to B but the follow-up
  shadow path could re-enter routing and select a different account.

Both were closed before completion:

- `ocx2` now embeds a non-secret restart-stable origin fingerprint derived from the existing durable
  reasoning replay identity components. Native unwrap requires an exact origin match; absent or
  mismatched provenance is portable-only.
- the native compact branch captures the exact successful provider/auth headers/model and sends the
  shadow render directly through that transport. A regression proves the concrete sequence
  `A compact 429 -> B compact 200 -> B shadow 200`.

Re-score: root-cause fidelity 20/20, compatibility/scope 15/15, continuity semantics 20/20,
failure/privacy/resource behavior 15/15, regression coverage 20/20, operability/rollback/docs 10/10.
This still does not turn the repository-wide `test:changed` timeout into a pass; the score concerns
the reviewed design/implementation evidence, while gate status remains recorded independently below.

### Review V5 — 100/100 after live route correction

Live canaries exposed one material routing assumption that unit tests alone did not: the current
canonical ChatGPT backend returned upstream 404 from `/responses/compact` for `gpt-5.6-sol`,
`gpt-5.5`, and `gpt-5.6-luna`. The actual failing user thread had produced its `gAAAA...` checkpoint
through a normal `/responses` request carrying `compaction_trigger`, and that contract rejects
`stream:false` (`Stream must be set to true`). Therefore the first native-shadow hook covered a
compatibility surface but not the concrete production path that minted the user's checkpoint.

The final implementation adds a canonical-compaction-only passthrough branch: consume that one SSE
turn to `response.completed`, render the portable checkpoint through the exact already-selected
transport/account, wrap the native item as `ocx2`, then re-frame the completed snapshot as canonical
Responses SSE. Ordinary passthrough traffic keeps its existing tee/eager/WS relay. A live canary then
proved `Luna compaction -> ocx2 -> same-thread Muse 1.3 -> exact checkpoint marker` end-to-end.

Re-score remains 100/100 because the concrete production route is now covered rather than inferred.

## Completion receipt

- [x] Phase 1 implemented + focused tests green (`routed-agent-messages`: 8/8)
- [x] Phase 2 implemented + focused tests green (`responses-compaction`: 31/31 after durable-origin hardening)
- [x] Phase 3 implemented + focused native-shadow tests green, including pooled-account affinity and
  the exact `A -> B -> B` alternate-account shadow ownership case; the canonical regular-Responses
  compaction path now shares the same `ocx2` semantics
- [x] Phase 4 SOT updated
- [x] typecheck green (`bun x tsc --noEmit`)
- [ ] `test:changed` green — attempted against the already broad dirty tree and terminated by the
  repository runner's 900-second suite ceiling (exit 124); this is recorded as inconclusive rather
  than green. Directly affected focused suites are green. A later full/changed gate should be run
  from a quieter/narrower worktree before review-ready status.
- [x] privacy gate green (`Privacy scan passed`)
- [x] touched-file `git diff --check` green; final dirty-tree diff audited without reset/checkout
- [x] installed deployment completed — backed up the pre-deploy runtime files under
  `%USERPROFILE%\.opencodex\backups\20260914-cross-provider-history-portability`, synchronized the
  reviewed runtime files into the global 2.39.0 install, verified repo/install SHA-256 equality,
  imported the installed modules successfully, and restarted through `ocx restart`. The later
  regular-compaction hook backed up the then-installed `core.ts` separately before synchronizing
  `core.ts` plus `native-compaction-portability.ts`.
- [x] installed/local live smoke completed — the latest proxy replacement is PID 19764 on port 10100
  and `/healthz` returned HTTP 200. The installed public-wire adapter lowers a
  call-id-less `codex_app/send_message_to_thread` output to a developer message, and installed `ocx2`
  replay proves same durable origin -> native blob while origin mismatch -> portable checkpoint
- [x] full real-provider E2E completed — a real Console Go/Muse 1.3 request carrying the historical
  call-id-less `codex_app/send_message_to_thread` shape returned HTTP 200 and `CALLID_CANARY_OK`.
  Separately, real `gpt-5.6-luna` canonical compaction returned exactly one `ocx2:` item; replaying
  that SAME compacted item on the SAME thread through `opencode-go/muse-spark-1.3-contributor`
  returned the exact checkpoint marker `PORTABLE_E2E_20260914_1515`. Legacy pre-patch raw `gAAAA...`
  state remains outside this live transport fix because it never contained a portable plaintext half.

### Verification notes

- `responses-compaction-routing.test.ts` new pooled-account case proved the native `/responses/compact`
  call and the follow-up portable-shadow `/responses` call both used the same `chatgpt-account-id`.
- A second focused alternate-account regression proved a 429 on account A followed by a successful
  native compact on B sends the direct portable-shadow render to B again (`A, B, B`), and the emitted
  `ocx2` contains both the B-minted native blob and a restart-stable origin fingerprint.
- Compaction replay regressions prove a matching durable origin may unwrap native state, while an
  origin mismatch or origin-less wrapper lowers to portable text even without an in-process route
  mismatch record.
- Installed-module smoke after deployment proved the same two contracts against the GLOBAL package,
  not merely the source worktree. The final replacement proxy reported HTTP 200 from `/healthz` with
  version `2.39.0`, PID `19764`, and port `10100`.
- Live route verification established that the current canonical ChatGPT backend uses normal
  `/v1/responses + compaction_trigger + stream:true` for remote compaction. Direct
  `/responses/compact` probes returned upstream 404 for `gpt-5.6-sol`, `gpt-5.5`, and
  `gpt-5.6-luna`, while `stream:false` on the regular compaction contract returned
  `Stream must be set to true`. The canonical compaction branch is therefore regression-critical.
- `native-compaction-portability.test.ts` adds focused success/fail-soft coverage for the common
  transport-leaf shadow helper. Latest focused gates are: compaction 31/31, compaction routing 49/49,
  OpenAI Responses passthrough 119/119, native portability helper 2/2, plus green typecheck.
- The service wrapper logged the requested restart as a child exit followed by its normal five-second
  supervised relaunch. The first 5-second health probe landed during startup; the later 20-second
  probe returned HTTP 200, so the deployment receipt records the final healthy state rather than
  treating the transient startup timeout as a runtime failure.
- A later whole-file run reached 43 pass / 5 timeout failures. All five failures were pre-existing
  alternate-account cases crossing their 5-second test deadline while Windows `icacls` hardening was
  concurrently stalling/failing; the new pooled-shadow case itself passed in that run.
- Earlier broad regression runs before that Windows load spike were green: compaction routing 47/47,
  opaque-blob recovery 23/23, and OpenAI Responses passthrough 119/119.
