import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { commandInvocation } from "../../lib/win-exec";
import { resolveCodexRuntime } from "../runtime";

export interface StandaloneWebSearchCapabilityProbeDeps {
  resolveRuntime?: () => { command: string; version: string | null };
  runFeaturesList?: (command: string) => string;
  now?: () => number;
}

const STANDALONE_WEB_SEARCH_CAPABILITY_TTL_MS = 5 * 60_000;
let capabilityMemo: { key: string; observedAt: number; supported: boolean } | null = null;

export function codexFeatureRegistrySupports(output: string, feature: string): boolean {
  for (const rawLine of String(output).split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line) continue;
    const match = /^(\S+)\s+(.+?)\s+(true|false)$/i.exec(line);
    if (!match || match[1] !== feature) continue;
    return match[2]!.trim().toLowerCase() !== "removed";
  }
  return false;
}

function runCodexFeaturesListReadOnly(command: string): string {
  let probeHome: string | undefined;
  try {
    probeHome = mkdtempSync(join(tmpdir(), "ocx-codex-feature-probe-"));
    const inv = commandInvocation(command, ["features", "list"]);
    return execFileSync(inv.file, inv.args, {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
      timeout: 5_000,
      windowsHide: true,
      env: { ...process.env, CODEX_HOME: probeHome },
      ...inv.options,
    });
  } finally {
    if (probeHome) {
      try { rmSync(probeHome, { recursive: true, force: true }); } catch { /* best effort */ }
    }
  }
}

export function probeInstalledCodexStandaloneWebSearch(
  deps: StandaloneWebSearchCapabilityProbeDeps = {},
): boolean {
  const now = (deps.now ?? Date.now)();
  const cacheable = !deps.resolveRuntime && !deps.runFeaturesList && !deps.now;
  let cacheKey: string | null = null;
  try {
    const runtime = deps.resolveRuntime
      ? deps.resolveRuntime()
      : resolveCodexRuntime({ discoverAlternatives: false }).runtime;
    if (!runtime.command) return false;
    const key = `${runtime.command}\u0000${runtime.version ?? ""}`;
    cacheKey = key;
    if (cacheable && capabilityMemo?.key === key
      && now - capabilityMemo.observedAt < STANDALONE_WEB_SEARCH_CAPABILITY_TTL_MS) {
      return capabilityMemo.supported;
    }
    const output = (deps.runFeaturesList ?? runCodexFeaturesListReadOnly)(runtime.command);
    const supported = codexFeatureRegistrySupports(output, "standalone_web_search");
    if (cacheable) capabilityMemo = { key, observedAt: now, supported };
    return supported;
  } catch {
    if (cacheable && cacheKey) capabilityMemo = { key: cacheKey, observedAt: now, supported: false };
    return false;
  }
}

export function resetStandaloneWebSearchCapabilityProbeForTests(): void {
  capabilityMemo = null;
}

export const MANAGED_STANDALONE_WEB_SEARCH_MARKER =
  "# Managed by opencodex: Codex standalone web search";
export const MANAGED_STANDALONE_WEB_SEARCH_TABLE_MARKER =
  "# Managed by opencodex: Codex standalone web search features table";

const FEATURES_TABLE_HEADER = /^\s*\[(["']?)\s*features\s*\1\]\s*(?:#.*)?$/;
const STANDALONE_WEB_SEARCH_KEY =
  /^\s*(?:"standalone_web_search"|'standalone_web_search'|standalone_web_search)\s*=/;
const STANDALONE_WEB_SEARCH_BOOLEAN =
  /^\s*(?:"standalone_web_search"|'standalone_web_search'|standalone_web_search)\s*=\s*(true|false)\s*(?:#.*)?$/;

export function standaloneWebSearchFeatureSetting(content: string): boolean | undefined {
  const lines = content.split("\n");
  const featuresStart = lines.findIndex(line => FEATURES_TABLE_HEADER.test(line));
  if (featuresStart === -1) return undefined;
  const nextTable = lines.findIndex((line, index) => index > featuresStart && /^\s*\[/.test(line));
  const featuresEnd = nextTable === -1 ? lines.length : nextTable;
  for (let i = featuresStart + 1; i < featuresEnd; i += 1) {
    const match = STANDALONE_WEB_SEARCH_BOOLEAN.exec(lines[i]!);
    if (match) return match[1] === "true";
  }
  return undefined;
}

export function ensureManagedStandaloneWebSearchFeature(content: string): string {
  const lines = content.split("\n");
  const featuresStart = lines.findIndex(line => FEATURES_TABLE_HEADER.test(line));
  if (featuresStart === -1) {
    return content.trimEnd()
      + `\n\n${MANAGED_STANDALONE_WEB_SEARCH_TABLE_MARKER}\n[features]\n`
      + `${MANAGED_STANDALONE_WEB_SEARCH_MARKER}\nstandalone_web_search = true\n`;
  }
  const nextTable = lines.findIndex((line, index) => index > featuresStart && /^\s*\[/.test(line));
  const featuresEnd = nextTable === -1 ? lines.length : nextTable;
  for (let i = featuresStart + 1; i < featuresEnd; i += 1) {
    if (!STANDALONE_WEB_SEARCH_KEY.test(lines[i]!)) continue;
    if (i > 0 && lines[i - 1] === MANAGED_STANDALONE_WEB_SEARCH_MARKER) {
      if (STANDALONE_WEB_SEARCH_BOOLEAN.exec(lines[i]!)?.[1] === "true") return lines.join("\n");
      lines.splice(i - 1, 1);
      if (featuresStart > 0 && lines[featuresStart - 1] === MANAGED_STANDALONE_WEB_SEARCH_TABLE_MARKER) {
        lines.splice(featuresStart - 1, 1);
      }
    }
    return lines.join("\n");
  }
  let insertAt = featuresEnd;
  while (insertAt > featuresStart + 1 && lines[insertAt - 1]!.trim() === "") insertAt -= 1;
  lines.splice(insertAt, 0, MANAGED_STANDALONE_WEB_SEARCH_MARKER, "standalone_web_search = true");
  return lines.join("\n");
}

export function stripManagedStandaloneWebSearchFeature(content: string): string {
  const lines = content.split("\n");
  for (let i = lines.length - 1; i >= 0; i -= 1) {
    if (lines[i] !== MANAGED_STANDALONE_WEB_SEARCH_MARKER) continue;
    const next = lines[i + 1];
    if (next !== undefined && STANDALONE_WEB_SEARCH_BOOLEAN.exec(next)?.[1] === "true") lines.splice(i, 2);
    else lines.splice(i, 1);
  }
  for (let i = lines.length - 1; i >= 0; i -= 1) {
    if (lines[i] !== MANAGED_STANDALONE_WEB_SEARCH_TABLE_MARKER) continue;
    const headerIndex = i + 1;
    if (headerIndex >= lines.length || !FEATURES_TABLE_HEADER.test(lines[headerIndex]!)) {
      lines.splice(i, 1);
      continue;
    }
    const nextTable = lines.findIndex((line, index) => index > headerIndex && /^\s*\[/.test(line));
    const end = nextTable === -1 ? lines.length : nextTable;
    const sectionHasUserContent = lines.slice(headerIndex + 1, end).some(line => line.trim() !== "");
    if (sectionHasUserContent) lines.splice(i, 1);
    else lines.splice(i, end - i);
  }
  return lines.join("\n");
}
