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
    const first = deriveOpenCodeGoSessionId(lane, "openai-chat");
    expect(first).toBe(deriveOpenCodeGoSessionId(lane, "openai-chat"));
    expect(first).toMatch(/^ocx_[a-f0-9]{32}$/);
    expect(first).not.toContain(lane);
    expect(deriveOpenCodeGoSessionId("other-thread", "openai-chat")).not.toBe(first);
    expect(deriveOpenCodeGoSessionId(lane, "anthropic")).not.toBe(first);
  });

  test("injects affinity only for the registered OpenCode Go destination", () => {
    const destination = goProvider();
    const resolved = resolveOpenCodeGoTransport(destination, "conversation-specific", destination);
    expect(new Headers(resolved.headers).get(OPENCODE_GO_SESSION_HEADER))
      .toBe(deriveOpenCodeGoSessionId("conversation-specific", "openai-chat"));

    const other = { ...goProvider(), baseUrl: "https://example.test/v1" };
    expect(resolveOpenCodeGoTransport(other, "conversation-specific", other)).toBe(other);
  });

  test("preserves an explicit session header case-insensitively", () => {
    const provider = goProvider({ "X-OpenCode-Session": "operator-owned" });
    const resolved = resolveOpenCodeGoTransport(provider, "conversation-specific", provider);
    expect(resolved).toBe(provider);
    expect(new Headers(resolved.headers).get(OPENCODE_GO_SESSION_HEADER)).toBe("operator-owned");
  });

  test("fails closed to the unchanged provider when no conversation lane is available", () => {
    const provider = goProvider();
    expect(resolveOpenCodeGoTransport(provider, undefined, provider)).toBe(provider);
    expect(new Headers(provider.headers).get(OPENCODE_GO_SESSION_HEADER)).toBeNull();
  });
});
