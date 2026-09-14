/**
 * OpenCode Go's curated model/transport contract.
 *
 * Authority: anomalyco/opencode `packages/web/src/content/docs/go.mdx`, Endpoints table.
 * Snapshot verified 2026-09-04. The Go `/models` endpoint is availability evidence only;
 * it may advertise rows outside this tested/recommended set.
 */
export const OPENCODE_GO_RESPONSES_MODELS = [
  "grok-4.6",
  "gpt-5.6-luna",
  "muse-spark-1.3-contributor",
  "muse-spark-1.2-contributor",
] as const;

export const OPENCODE_GO_CHAT_MODELS = [
  "glm-5.3-flash",
  "glm-5.3",
  "glm-5.2",
  "glm-5.1",
  "kimi-k3",
  "kimi-k2.7-code",
  "kimi-k2.6",
  "longcat-2.0",
  "deepseek-v4-pro",
  "deepseek-v4-flash",
  "deepseek-v4-flash-vision-exp",
  "mimo-v2.5",
  "mimo-v2.5-pro",
  "hy4-preview",
  "hy3",
] as const;

export const OPENCODE_GO_ANTHROPIC_MODELS = [
  "minimax-m3",
  "minimax-m2.7",
  "minimax-m2.5",
  "qwen3.8-max",
  "qwen3.8-flash",
  "qwen3.7-max",
  "qwen3.7-plus",
  "qwen3.6-plus",
] as const;

export const OPENCODE_GO_CURATED_MODELS = [
  ...OPENCODE_GO_RESPONSES_MODELS,
  ...OPENCODE_GO_CHAT_MODELS,
  ...OPENCODE_GO_ANTHROPIC_MODELS,
] as const;

const OPENCODE_GO_CURATED_MODEL_SET: ReadonlySet<string> = new Set(OPENCODE_GO_CURATED_MODELS);

export function isCuratedOpenCodeGoModel(modelId: string): boolean {
  return OPENCODE_GO_CURATED_MODEL_SET.has(modelId);
}
