import { describe, expect, test } from "bun:test";
import {
  NATIVE_GPT6_ASTRA_MODEL,
  NATIVE_GPT56_OPT_IN_CONTEXT_WINDOW,
  NATIVE_OPENAI_MODELS,
  nativeDefaultReasoningEffort,
  nativeModelRows,
  nativeOpenAiContextTier,
  nativeOpenAiContextWindow,
  nativeOpenAiMaxInputTokens,
  nativeReasoningEfforts,
  upstreamNativeEntry,
} from "../src/codex/catalog";
import { isGpt56NativeSlug } from "../src/codex/catalog/effort";
import { ACCOUNT_GATED_NATIVE_OPENAI_MODELS } from "../src/codex/catalog/native-models";
import { NEUTRAL_IDENTITY_LINE, neutralizeIdentity } from "../src/adapters/identity";

describe("GPT-6 Astra native port", () => {
  test("lists Astra unconditionally with its shipped native metadata", () => {
    expect(NATIVE_GPT6_ASTRA_MODEL).toBe("gpt-6-astra");
    expect(NATIVE_OPENAI_MODELS).toContain(NATIVE_GPT6_ASTRA_MODEL);
    expect(ACCOUNT_GATED_NATIVE_OPENAI_MODELS.has(NATIVE_GPT6_ASTRA_MODEL)).toBe(false);

    const rows = nativeModelRows({
      disabledModels: [],
      combos: {},
      providerContextCaps: {},
      providers: {},
    });
    expect(rows.some(row => row.slug === NATIVE_GPT6_ASTRA_MODEL)).toBe(true);

    expect(nativeOpenAiContextWindow(NATIVE_GPT6_ASTRA_MODEL)).toBe(272_000);
    expect(nativeOpenAiContextTier(NATIVE_GPT6_ASTRA_MODEL))
      .toEqual({ defaultWindow: 272_000, longWindow: 872_000 });
    expect(nativeReasoningEfforts(NATIVE_GPT6_ASTRA_MODEL))
      .toEqual(["low", "medium", "high", "xhigh", "max", "ultra"]);
    expect(nativeDefaultReasoningEffort(NATIVE_GPT6_ASTRA_MODEL)).toBe("low");
    expect(isGpt56NativeSlug(NATIVE_GPT6_ASTRA_MODEL)).toBe(true);

    expect(upstreamNativeEntry(NATIVE_GPT6_ASTRA_MODEL)).toMatchObject({
      display_name: "GPT-6-Astra",
      description: "Our most capable model for complex, demanding work.",
      context_window: 272_000,
      max_context_window: 872_000,
    });
    expect(upstreamNativeEntry(NATIVE_GPT6_ASTRA_MODEL)?.base_instructions)
      .toContain("You are Codex, an agent based on GPT-6.");
  });

  test("long-window opt-in clamps Astra to its own 872k ceiling", () => {
    const limits = { cap: NATIVE_GPT56_OPT_IN_CONTEXT_WINDOW } as const;
    expect(nativeOpenAiContextWindow(NATIVE_GPT6_ASTRA_MODEL, limits)).toBe(872_000);
    expect(nativeOpenAiMaxInputTokens(NATIVE_GPT6_ASTRA_MODEL, limits)).toBe(872_000);
    expect(nativeOpenAiContextWindow("gpt-5.6-sol", limits)).toBe(922_000);
  });

  test("neutralizes GPT-6 Codex identity on routed prompts", () => {
    expect(neutralizeIdentity("You are Codex, an agent based on GPT-6."))
      .toBe(NEUTRAL_IDENTITY_LINE);
    expect(neutralizeIdentity("You are Codex, a coding agent based on GPT-6.1."))
      .toBe(NEUTRAL_IDENTITY_LINE);
  });
});
