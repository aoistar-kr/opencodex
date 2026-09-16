import { describe, expect, test } from "bun:test";
import {
  OPENCODE_GO_SESSION_HEADER,
  deriveOpenCodeGoSessionId,
  resolveOpenCodeGoTransport,
} from "../src/providers/opencode-go-transport";
import type { OcxProviderConfig } from "../src/types";

function goProvider(headers?: Record<string, string>): OcxProviderConfig {
  return {
    adapter: "openai-chat",
    baseUrl: "https://opencode.ai/zen/go/v1",
    authMode: "key",
    apiKey: "test-key",
    ...(headers ? { headers } : {}),
  } as OcxProviderConfig;
}

describe("OpenCode Go transport session header", () => {
  test("derives a stable opaque provider-scoped session id", () => {
    const lane = "thread-0123456789abcdef";
    const first = deriveOpenCodeGoSessionId(lane);
    expect(first).toBe(deriveOpenCodeGoSessionId(lane));
    expect(first).toMatch(/^ocx_[a-f0-9]{32}$/);
    expect(first).not.toContain(lane);
    expect(deriveOpenCodeGoSessionId("other-thread")).not.toBe(first);
  });

  test("injects affinity only for the registered OpenCode Go destination", () => {
    const resolved = resolveOpenCodeGoTransport(goProvider(), "conversation-specific");
    expect(new Headers(resolved.headers).get(OPENCODE_GO_SESSION_HEADER))
      .toBe(deriveOpenCodeGoSessionId("conversation-specific"));

    const other = { ...goProvider(), baseUrl: "https://example.test/v1" };
    expect(resolveOpenCodeGoTransport(other, "conversation-specific")).toBe(other);
  });

  test("preserves an explicit session header case-insensitively", () => {
    const provider = goProvider({ "X-OpenCode-Session": "operator-owned" });
    const resolved = resolveOpenCodeGoTransport(provider, "conversation-specific");
    expect(resolved).toBe(provider);
    expect(new Headers(resolved.headers).get(OPENCODE_GO_SESSION_HEADER)).toBe("operator-owned");
  });

  test("fails closed to the unchanged provider when no conversation lane is available", () => {
    const provider = goProvider();
    expect(resolveOpenCodeGoTransport(provider, undefined)).toBe(provider);
    expect(new Headers(provider.headers).get(OPENCODE_GO_SESSION_HEADER)).toBeNull();
  });
});
