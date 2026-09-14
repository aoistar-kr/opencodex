import { describe, expect, test } from "bun:test";
import { normalizeRoutedAgentMessages } from "../src/adapters/routed-agent-messages";
import { createResponsesPassthroughAdapter as createResponsesPassthroughAdapterProduction } from "../src/adapters/openai-responses";
import { CODEX_FORWARD_BASE_URL } from "../src/providers/openai-tiers";
import { withTestTranslatorBudget } from "./helpers/translator-budget";

const createResponsesPassthroughAdapter = (...args: Parameters<typeof createResponsesPassthroughAdapterProduction>) =>
  withTestTranslatorBudget(createResponsesPassthroughAdapterProduction(...args));

describe("routed Codex-private history normalization", () => {
  function serializedInput(provider: Record<string, unknown>): Array<Record<string, unknown>> {
    const rawBody = {
      model: "model",
      input: [{
        type: "function_call_output",
        id: "fco_private",
        name: "send_message_to_thread",
        namespace: "codex_app",
        output: "delegated task",
      }],
    };
    const built = createResponsesPassthroughAdapter(provider as never).buildRequest({
      modelId: "model",
      context: { messages: [] },
      stream: true,
      options: {},
      _rawBody: rawBody,
    }, { headers: new Headers() });
    return (JSON.parse(built.body as string) as { input: Array<Record<string, unknown>> }).input;
  }

  test("lowers call-id-less codex_app send_message_to_thread output to a developer message", () => {
    const body = {
      model: "provider-model",
      input: [{
        type: "function_call_output",
        id: "fco_private",
        name: "send_message_to_thread",
        namespace: "codex_app",
        output: "<codex_delegation>[WORK REQUEST] reconcile environment</codex_delegation>",
      }],
    };

    expect(normalizeRoutedAgentMessages(body)).toEqual({
      model: "provider-model",
      input: [{
        type: "message",
        role: "developer",
        content: [{
          type: "input_text",
          text: "Codex app event codex_app/send_message_to_thread\n<codex_delegation>[WORK REQUEST] reconcile environment</codex_delegation>",
        }],
      }],
    });
  });

  test("normalizes sibling automation_update standalone output without a tool-name special case", () => {
    const body = {
      input: [{
        type: "function_call_output",
        name: "automation_update",
        namespace: "codex_app",
        output: [{ type: "output_text", text: "Automation: Daily brief" }],
      }],
    };

    const normalized = normalizeRoutedAgentMessages(body) as { input: Array<Record<string, unknown>> };
    expect(normalized.input[0]).toEqual({
      type: "message",
      role: "developer",
      content: [{
        type: "input_text",
        text: "Codex app event codex_app/automation_update\nAutomation: Daily brief",
      }],
    });
  });

  test("preserves a valid paired function output with call_id", () => {
    const item = {
      type: "function_call_output",
      call_id: "call_real",
      name: "send_message_to_thread",
      namespace: "codex_app",
      output: "ok",
    };
    const body = { input: [item] };
    expect(normalizeRoutedAgentMessages(body)).toBe(body);
    expect(body.input[0]).toBe(item);
  });

  test("does not guess at unrelated malformed public function outputs", () => {
    const body = {
      input: [{ type: "function_call_output", name: "lookup", output: "orphan" }],
    };
    expect(normalizeRoutedAgentMessages(body)).toBe(body);
  });

  test("leaves non-text private output fail-closed instead of dropping content", () => {
    const body = {
      input: [{
        type: "function_call_output",
        name: "send_message_to_thread",
        namespace: "codex_app",
        output: [{ type: "input_image", image_url: "https://example.test/image.png" }],
      }],
    };
    expect(normalizeRoutedAgentMessages(body)).toBe(body);
  });

  test("noncanonical forward gateways are public boundaries and normalize standalone app output", () => {
    const input = serializedInput({
      adapter: "openai-responses",
      baseUrl: "https://gateway.example/v1",
      authMode: "forward",
    });
    expect(input[0]?.type).toBe("message");
    expect(input[0]?.role).toBe("developer");
  });

  test("canonical ChatGPT forward keeps its existing orphan-repair behavior", () => {
    const input = serializedInput({
      adapter: "openai-responses",
      baseUrl: CODEX_FORWARD_BASE_URL,
      authMode: "forward",
    });
    // Canonical forward already runs repairOrphanedInputItems() for a standalone output.
    // This assertion protects that pre-existing behavior from the routed-boundary normalizer.
    expect(input[0]?.type).toBe("message");
  });

  test("explicit Codex-aware loopback retains private standalone app output", () => {
    const input = serializedInput({
      adapter: "openai-responses",
      baseUrl: "http://127.0.0.1:17841/v1",
      allowPrivateNetwork: true,
      preserveCodexPrivateMetadata: true,
    });
    expect(input[0]?.type).toBe("function_call_output");
    expect(input[0]?.namespace).toBe("codex_app");
  });
});
