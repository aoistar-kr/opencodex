import { expect, test } from "bun:test";
import { createResponsesPassthroughAdapter as createResponsesPassthroughAdapterProduction } from "../../src/adapters/openai-responses/passthrough";
import { withTestTranslatorBudget } from "../helpers/translator-budget";

const createResponsesPassthroughAdapter = (...args: Parameters<typeof createResponsesPassthroughAdapterProduction>) =>
  withTestTranslatorBudget(createResponsesPassthroughAdapterProduction(...args));

test("explicit Codex-aware loopback preserves unstored user item ids", () => {
  const request = createResponsesPassthroughAdapter({
    adapter: "openai-responses",
    baseUrl: "http://127.0.0.1:17841/v1",
    allowPrivateNetwork: true,
    preserveCodexPrivateMetadata: true,
  }).buildRequest({
    modelId: "chatgpt-web/high",
    context: { messages: [] },
    stream: true,
    options: {},
    _rawBody: {
      model: "chatgpt-web/high",
      store: false,
      input: [{
        id: "msg_current_user",
        type: "message",
        role: "user",
        content: [{ type: "input_text", text: "ping" }],
      }],
    },
  }, { headers: new Headers({
    "x-codex-turn-metadata": JSON.stringify({ thread_id: "thread-live-1", turn_id: "turn-live-1" }),
  }) });
  const body = JSON.parse(request.body) as { input: Array<{ id?: string }> };

  expect(body.input[0]?.id).toBe("msg_current_user");
});
