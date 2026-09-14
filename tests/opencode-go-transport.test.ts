import { describe, expect, test } from "bun:test";
import {
  OPENCODE_GO_SESSION_HEADER,
  OPENCODE_GO_USER_AGENT,
  isOpenCodeGoTransport,
  withOpenCodeGoSession,
} from "../src/providers/opencode-go-transport";
import type { OcxProviderConfig } from "../src/types";

function goProvider(headers?: Record<string, string>): OcxProviderConfig {
  return {
    adapter: "openai-chat",
    baseUrl: "https://opencode.ai/zen/go/v1/",
    authMode: "key",
    apiKey: "test-key",
    ...(headers ? { headers } : {}),
  } as OcxProviderConfig;
}

describe("OpenCode Go transport session header", () => {
  test("matches the fixed Go destination independent of provider naming", () => {
    expect(isOpenCodeGoTransport(goProvider())).toBe(true);
    expect(isOpenCodeGoTransport({ ...goProvider(), baseUrl: "https://example.test/v1" })).toBe(false);
    expect(isOpenCodeGoTransport({ ...goProvider(), authMode: "oauth" })).toBe(false);
  });

  test("injects the stable session id and identifies opencodex", () => {
    const resolved = withOpenCodeGoSession(goProvider(), "0123456789abcdef0123456789abcdef");
    const headers = new Headers(resolved.headers);
    expect(headers.get(OPENCODE_GO_SESSION_HEADER)).toBe("0123456789abcdef0123456789abcdef");
    expect(headers.get("user-agent")).toBe(OPENCODE_GO_USER_AGENT);
  });

  test("replaces a static session header case-insensitively but preserves an explicit user agent", () => {
    const resolved = withOpenCodeGoSession(goProvider({
      "X-OpenCode-Session": "wrong-global-constant",
      "user-agent": "my-coding-agent/1.0",
    }), "conversation-specific");
    const headers = new Headers(resolved.headers);
    expect(headers.get(OPENCODE_GO_SESSION_HEADER)).toBe("conversation-specific");
    expect(headers.get("user-agent")).toBe("my-coding-agent/1.0");
  });

  test("collapses duplicate differently-cased session headers to one authoritative value", () => {
    const resolved = withOpenCodeGoSession(goProvider({
      "x-opencode-session": "wrong-lowercase-value",
      "X-OpenCode-Session": "wrong-mixed-case-value",
    }), "conversation-specific");
    const sessionKeys = Object.keys(resolved.headers ?? {})
      .filter(key => key.toLowerCase() === OPENCODE_GO_SESSION_HEADER);
    expect(sessionKeys).toEqual([OPENCODE_GO_SESSION_HEADER]);
    expect(new Headers(resolved.headers).get(OPENCODE_GO_SESSION_HEADER)).toBe("conversation-specific");
  });

  test("does not mutate unrelated providers, but still identifies opencodex when no session is available", () => {
    const other = { ...goProvider(), baseUrl: "https://example.test/v1" };
    expect(withOpenCodeGoSession(other, "conversation-specific")).toBe(other);

    const resolved = withOpenCodeGoSession(goProvider({ "X-OpenCode-Session": "wrong-global-constant" }), undefined);
    const headers = new Headers(resolved.headers);
    expect(headers.get(OPENCODE_GO_SESSION_HEADER)).toBeNull();
    expect(headers.get("user-agent")).toBe(OPENCODE_GO_USER_AGENT);
  });
});
