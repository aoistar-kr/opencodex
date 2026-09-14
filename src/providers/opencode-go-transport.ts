import type { OcxProviderConfig } from "../types";

export const OPENCODE_GO_BASE_URL = "https://opencode.ai/zen/go/v1";
export const OPENCODE_GO_SESSION_HEADER = "x-opencode-session";
export const OPENCODE_GO_USER_AGENT = "opencodex";
export const OPENCODE_GO_SESSION_REQUIRED_MESSAGE =
  "OpenCode Go requires a stable per-conversation session identity; provide thread-id, session_id/session-id, x-codex-parent-thread-id, or x-opencode-session.";

function normalizedEndpoint(value: string): string {
  return value.trim().replace(/\/+$/, "").toLowerCase();
}

function headerKey(headers: Record<string, string> | undefined, name: string): string | undefined {
  const target = name.toLowerCase();
  return Object.keys(headers ?? {}).find(key => key.toLowerCase() === target);
}

function deleteHeaderCaseInsensitively(headers: Record<string, string>, name: string): void {
  const target = name.toLowerCase();
  for (const key of Object.keys(headers)) {
    if (key.toLowerCase() === target) delete headers[key];
  }
}

/**
 * OpenCode Go is a fixed key-auth destination. Match by destination rather than configured
 * provider name so a renamed preset still receives the protocol-required session header.
 */
export function isOpenCodeGoTransport(provider: Pick<OcxProviderConfig, "baseUrl" | "authMode">): boolean {
  return (provider.authMode ?? "key") === "key"
    && normalizedEndpoint(provider.baseUrl) === normalizedEndpoint(OPENCODE_GO_BASE_URL);
}

/**
 * Attach OpenCode Go's required stable per-conversation affinity header without persisting it.
 * The caller supplies an already opaque/stable conversation id; static user config is never used
 * as the session authority because one constant header across conversations defeats the contract.
 *
 * Also identify this proxy truthfully when the operator did not provide a User-Agent. OpenCode Go
 * explicitly asks third-party clients not to send a generic runtime user agent (for example the
 * Node/Bun fetch default).
 */
export function withOpenCodeGoSession(
  provider: OcxProviderConfig,
  conversationId: string | undefined,
): OcxProviderConfig {
  if (!isOpenCodeGoTransport(provider)) return provider;
  const headers = { ...(provider.headers ?? {}) };
  deleteHeaderCaseInsensitively(headers, OPENCODE_GO_SESSION_HEADER);
  if (!headerKey(headers, "user-agent")) headers["User-Agent"] = OPENCODE_GO_USER_AGENT;
  const session = conversationId?.trim();
  if (session) headers[OPENCODE_GO_SESSION_HEADER] = session;
  return { ...provider, headers };
}
