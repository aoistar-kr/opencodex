import { describe, expect, test } from "bun:test";
import {
  OPENCODE_GO_ANTHROPIC_MODELS,
  OPENCODE_GO_CHAT_MODELS,
  OPENCODE_GO_CURATED_MODELS,
  OPENCODE_GO_RESPONSES_MODELS,
  isCuratedOpenCodeGoModel,
} from "../src/providers/opencode-go";
import { resolveWireProtocolOverride } from "../src/server/adapter-resolve";
import type { OcxProviderConfig } from "../src/types";

function provider(): OcxProviderConfig {
  return {
    adapter: "openai-chat",
    baseUrl: "https://opencode.ai/zen/go/v1",
    authMode: "key",
    apiKey: "test-key",
  } as OcxProviderConfig;
}

describe("OpenCode Go curated transport manifest", () => {
  test("pins the complete official 2026-09-04 curated roster without duplicates", () => {
    expect(OPENCODE_GO_RESPONSES_MODELS).toHaveLength(4);
    expect(OPENCODE_GO_CHAT_MODELS).toHaveLength(15);
    expect(OPENCODE_GO_ANTHROPIC_MODELS).toHaveLength(8);
    expect(OPENCODE_GO_CURATED_MODELS).toHaveLength(27);
    expect(new Set(OPENCODE_GO_CURATED_MODELS).size).toBe(27);
  });

  test("routes every curated model over the endpoint family OpenCode documents", () => {
    const base = provider();
    for (const model of OPENCODE_GO_RESPONSES_MODELS) {
      expect(resolveWireProtocolOverride("opencode-go", model, base, "responses").adapter)
        .toBe("openai-responses");
    }
    for (const model of OPENCODE_GO_CHAT_MODELS) {
      expect(resolveWireProtocolOverride("opencode-go", model, base, "responses").adapter)
        .toBe("openai-chat");
    }
    for (const model of OPENCODE_GO_ANTHROPIC_MODELS) {
      expect(resolveWireProtocolOverride("opencode-go", model, base, "responses").adapter)
        .toBe("anthropic");
    }
  });

  test("does not promote live-gateway extras into the curated contract", () => {
    for (const id of [
      "glm-5",
      "grok-4.5",
      "hy3-preview",
      "kimi-k2.5",
      "mimo-v2-omni",
      "mimo-v2-pro",
      "qwen3.5-plus",
    ]) {
      expect(isCuratedOpenCodeGoModel(id)).toBe(false);
    }
  });
});
