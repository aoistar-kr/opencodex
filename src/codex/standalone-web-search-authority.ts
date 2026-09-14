import { readFileSync } from "node:fs";
import { join } from "node:path";
import { getCodexHome } from "./paths";

/** Codex's first-party standalone search is emitted as this flattened client-tool name. */
export const CODEX_STANDALONE_WEB_RUN_WIRE_NAME = "web__run";

// Keep this exact rather than accepting arbitrary `codex_*` values. `originator` is caller
// metadata, so it is supporting evidence only; the active Codex feature setting below is the
// local authority that the operator/client actually enabled standalone search.
const CODEX_STANDALONE_SEARCH_ORIGINATORS = new Set([
  "codex_cli_rs",
  "codex_exec",
  "codex_app",
  "codex_work_desktop",
  "Codex Desktop",
]);

export interface CodexStandaloneWebSearchAuthorityDeps {
  readConfig?: () => string;
}

function standaloneWebSearchEnabled(content: string): boolean {
  try {
    const parsed = Bun.TOML.parse(content) as Record<string, unknown>;
    const features = parsed.features;
    return typeof features === "object"
      && features !== null
      && !Array.isArray(features)
      && (features as Record<string, unknown>).standalone_web_search === true;
  } catch {
    return false;
  }
}

/**
 * Codex 0.152+ owns `web.run` out of band: it is intentionally absent from Responses `tools[]`.
 * Routed providers therefore need one narrow exception to the request-declared-tool guard.
 *
 * Fail closed unless all three facts agree: this is OpenCode Go (whose catalog contract delegates
 * search to Codex), the caller identifies as a known Codex surface, and the active Codex config
 * explicitly enables the standalone feature. No other undeclared tool is authorized here.
 */
export function codexStandaloneWebRunAuthorized(
  providerName: string,
  headers: Headers,
  deps: CodexStandaloneWebSearchAuthorityDeps = {},
): boolean {
  if (providerName !== "opencode-go") return false;
  if (!CODEX_STANDALONE_SEARCH_ORIGINATORS.has(headers.get("originator") ?? "")) return false;
  try {
    const content = (deps.readConfig ?? (() => readFileSync(join(getCodexHome(), "config.toml"), "utf8")))();
    return standaloneWebSearchEnabled(content);
  } catch {
    return false;
  }
}
