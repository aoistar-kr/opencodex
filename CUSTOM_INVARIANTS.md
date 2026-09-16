# Custom invariants

This fork must fail closed during upstream integration if any of these contracts regress.

- **Responses passthrough and private metadata:** non-canonical providers must not receive caller/runtime credentials or Codex-private metadata; the explicit Codex-aware loopback path may preserve the required provenance. (`tests/responses/openai-responses-passthrough.test.ts`)
- **Compaction routing and portability:** routed/native compact behavior, account selection, terminal handling, and native compaction portability must remain compatible. (`tests/responses/responses-compaction-routing.test.ts`, `tests/responses/native-compaction-portability.test.ts`)
- **Tool identity compatibility:** namespace/custom-tool identity and original function declaration repair must survive provider translation. (`tests/responses/namespace-tool-compat.test.ts`, `tests/responses/responses-function-tool-repair.test.ts`)
- **Standalone web search authority:** `web.run` remains owned by the Codex harness rather than being silently rewritten into a provider-specific substitute. (`tests/standalone-web-search-authority.test.ts`)
- **OpenCode Go transport:** the curated transport manifest and session-header behavior remain intact. (`tests/opencode-go-manifest.test.ts`, `tests/opencode-go-transport.test.ts`)
- **Cross-provider agents:** routed private history remains normalized and the keep-native-v1 compatibility path remains available. (`tests/adapters/routed-agent-messages.test.ts`, `tests/codex-integration/multi-agent-keep-native-v1.test.ts`)
- **Exact reasoning effort:** provider-specific reasoning-effort mapping must not silently drift. (`tests/codex-integration/reasoning-effort.test.ts`)

`bun run test:custom-invariants` is a mandatory CI gate in `.github/workflows/upstream-sync.yml`. Removing the script or any listed test therefore fails upstream integration before deployment.
