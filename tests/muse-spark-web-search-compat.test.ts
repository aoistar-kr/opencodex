import { describe, expect, test } from "bun:test";
import { createResponsesPassthroughAdapter as createResponsesPassthroughAdapterProduction } from "../src/adapters/openai-responses";
import { getProviderRegistryEntry } from "../src/providers/registry";
import type { OcxProviderConfig } from "../src/types";
import { withTestTranslatorBudget } from "./helpers/translator-budget";

const createResponsesPassthroughAdapter = (...args: Parameters<typeof createResponsesPassthroughAdapterProduction>) =>
  withTestTranslatorBudget(createResponsesPassthroughAdapterProduction(...args));

const PROVIDER = {
  adapter: "openai-responses",
  baseUrl: "https://opencode.ai/zen/v1",
  apiKey: "test-key",
} as unknown as OcxProviderConfig;

/** A Codex web_search declaration exactly as `hosted_spec.rs` emits it for TextAndImage. */
function webSearchTool(): Record<string, unknown> {
  return {
    type: "web_search",
    search_content_types: ["text", "image"],
    search_context_size: "medium",
  };
}

function build(modelId: string, rawBody: Record<string, unknown>): Record<string, unknown> {
  const request = createResponsesPassthroughAdapter(PROVIDER).buildRequest({
    modelId,
    context: { messages: [] },
    stream: true,
    options: {},
    _rawBody: { model: modelId, input: "ping", ...rawBody },
  }, { headers: new Headers() });
  return JSON.parse(request.body) as Record<string, unknown>;
}

const toolsOf = (body: Record<string, unknown>) => body.tools as Array<Record<string, unknown>>;

describe("OpenCode Go Responses boundary", () => {
  test("does not rewrite explicit hosted web_search fields by Muse model id", () => {
    for (const model of ["muse-spark-1.2-contributor", "muse-spark-1.3-contributor"]) {
      const body = build(model, { tools: [webSearchTool()] });
      const tool = toolsOf(body)[0]!;
      expect(tool.type).toBe("web_search");
      expect(tool.search_context_size).toBe("medium");
      expect(tool.search_content_types).toEqual(["text", "image"]);
    }
  });

  test("web_search_preview keeps the field, because the gateway accepts it there", () => {
    const body = build("muse-spark-1.2-contributor", {
      tools: [{ ...webSearchTool(), type: "web_search_preview" }],
    });
    const tool = toolsOf(body)[0]!;
    expect(tool.type).toBe("web_search_preview");
    expect(tool.search_content_types).toEqual(["text", "image"]);
  });

  test("another model on the same provider is untouched", () => {
    const body = build("gpt-5.6-luna", { tools: [webSearchTool()] });
    expect(toolsOf(body)[0]!.search_content_types).toEqual(["text", "image"]);
  });

  test("a private additional_tools declaration is promoted without a Muse-only web_search rewrite", () => {
    const body = build("muse-spark-1.2-contributor", {
      input: [{ type: "additional_tools", tools: [webSearchTool()] }],
    });
    const input = body.input as Array<Record<string, unknown>>;
    expect(input.some(item => item.type === "additional_tools")).toBe(false);
    const promoted = toolsOf(body).find(tool => tool.type === "web_search")!;
    expect(promoted.type).toBe("web_search");
    expect(promoted.search_content_types).toEqual(["text", "image"]);
  });

  test("the registry routes only the named exact models to Responses", () => {
    const defaults = getProviderRegistryEntry("opencode-go")?.modelWireDefaults ?? {};
    expect(defaults["muse-spark-1.2-contributor"]).toBe("openai-responses");
    // An exact-model allowlist, not a family rule: a sibling must not be dragged along.
    expect(defaults["muse-spark-1.2"]).toBeUndefined();
  });

  test("promotes replayed additional_tools for Muse 1.3 instead of forwarding the private input item", () => {
    const body = build("muse-spark-1.3-contributor", {
      input: [
        {
          type: "additional_tools",
          role: "developer",
          tools: [{
            type: "function",
            name: "noop",
            description: "",
            parameters: { type: "object", properties: {} },
          }],
        },
        { type: "message", role: "user", content: [{ type: "input_text", text: "ping" }] },
      ],
    });
    const input = body.input as Array<Record<string, unknown>>;
    expect(input.some(item => item.type === "additional_tools")).toBe(false);
    expect(toolsOf(body)).toContainEqual(expect.objectContaining({ type: "function", name: "noop" }));
  });

  test("promotes additional_tools for a sibling Responses model too", () => {
    const body = build("gpt-5.6-luna", {
      input: [{
        type: "additional_tools",
        role: "developer",
        tools: [{ type: "function", name: "noop", parameters: { type: "object", properties: {} } }],
      }],
    });
    const input = body.input as Array<Record<string, unknown>>;
    expect(input.some(item => item.type === "additional_tools")).toBe(false);
    expect(toolsOf(body)).toContainEqual(expect.objectContaining({ type: "function", name: "noop" }));
  });
});
