import { afterEach, describe, expect, test } from "bun:test";
import { decodeHybridCompaction } from "../../src/responses/compaction";
import { addPortableShadowToSnapshot } from "../../src/server/responses/native-compaction-portability";

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
});

function completedSse(text: string): Response {
  const response = {
    id: "resp_shadow",
    status: "completed",
    output: [{
      type: "message",
      role: "assistant",
      content: [{ type: "output_text", text }],
    }],
  };
  const body = [
    `event: response.completed\ndata: ${JSON.stringify({ type: "response.completed", response })}`,
    "data: [DONE]",
    "",
  ].join("\n\n");
  return new Response(body, { headers: { "content-type": "text/event-stream" } });
}

describe("native compaction portability shadow", () => {
  const transport = {
    providerName: "fixture",
    provider: {
      adapter: "openai-responses" as const,
      baseUrl: "https://provider.example/v1",
      authMode: "key" as const,
      apiKey: "fixture-key",
    },
    headers: new Headers({
      "content-type": "application/json",
      authorization: "Bearer fixture-key",
    }),
    model: "fixture-model",
    connectMs: 5_000,
    url: "https://provider.example/v1/responses",
  };

  test("wraps one native compaction item with the backend-rendered portable checkpoint", async () => {
    const sends: Array<Record<string, unknown>> = [];
    globalThis.fetch = (async (_input, init) => {
      sends.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
      return completedSse("portable checkpoint");
    }) as typeof fetch;

    const snapshot = {
      id: "resp_compact",
      status: "completed",
      output: [{ type: "compaction", encrypted_content: "gAAAAA-native" }],
    };
    const portable = await addPortableShadowToSnapshot({
      snapshot,
      signal: new AbortController().signal,
      origin: "a".repeat(64),
      transport,
    });

    expect(sends).toHaveLength(1);
    expect(sends[0]).toMatchObject({
      model: "fixture-model",
      stream: true,
      store: false,
      tools: [],
    });
    expect((sends[0]!.input as Array<Record<string, unknown>>)[0]).toEqual({
      type: "compaction",
      encrypted_content: "gAAAAA-native",
    });
    const wrapped = (portable.output as Array<{ encrypted_content?: string }>)[0]!.encrypted_content!;
    expect(decodeHybridCompaction(wrapped)).toEqual({
      native: "gAAAAA-native",
      summary: "portable checkpoint",
      origin: "a".repeat(64),
    });
  });

  test("shadow failure is fail-soft and preserves the original native snapshot", async () => {
    globalThis.fetch = (async () => Response.json(
      { error: { message: "shadow unavailable" } },
      { status: 503 },
    )) as typeof fetch;
    const snapshot = {
      id: "resp_compact",
      status: "completed",
      output: [{ type: "compaction", encrypted_content: "gAAAAA-native" }],
    };

    expect(await addPortableShadowToSnapshot({
      snapshot,
      signal: new AbortController().signal,
      origin: "b".repeat(64),
      transport,
    })).toBe(snapshot);
  });
});
