import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { saveConfig } from "../src/config";
import { startServer } from "../src/server";
import type { OcxConfig } from "../src/types";
import { installIsolatedCodexHome, type IsolatedCodexHome } from "./helpers/isolated-codex-home";
import { removeTreeWithRetry } from "./helpers/remove-tree";
import { normalizeLogConversationId } from "../src/server/request-log-conversation";

const CHAT_ENDPOINT = "https://opencode.ai/zen/go/v1/chat/completions";

let testDir = "";
let previousHome: string | undefined;
let isolatedCodexHome: IsolatedCodexHome | null = null;
let originalFetch: typeof fetch;

beforeEach(() => {
  originalFetch = globalThis.fetch;
  previousHome = process.env.OPENCODEX_HOME;
  isolatedCodexHome = installIsolatedCodexHome("ocx-opencode-go-goal-codex-");
  testDir = mkdtempSync(join(tmpdir(), "ocx-opencode-go-goal-"));
  process.env.OPENCODEX_HOME = testDir;
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  if (previousHome === undefined) delete process.env.OPENCODEX_HOME;
  else process.env.OPENCODEX_HOME = previousHome;
  isolatedCodexHome?.restore();
  isolatedCodexHome = null;
  if (testDir) removeTreeWithRetry(testDir);
});

function config(): OcxConfig {
  return {
    port: 0,
    hostname: "127.0.0.1",
    defaultProvider: "opencode-go",
    providers: {
      "opencode-go": {
        adapter: "openai-chat",
        baseUrl: "https://opencode.ai/zen/go/v1",
        apiKey: "test-key",
        models: ["deepseek-v4-flash"],
      },
    },
  } as OcxConfig;
}

function goalRequestBody(): Record<string, unknown> {
  return {
    model: "opencode-go/deepseek-v4-flash",
    input: "Create a goal",
    stream: true,
    store: false,
    tools: [{
      type: "namespace",
      name: "functions",
      description: "Client functions",
      tools: [{
        type: "function",
        name: "update_plan",
        description: "Update the current goal plan",
        parameters: {
          type: "object",
          properties: {
            explanation: { type: "string" },
            plan: {
              type: "array",
              items: {
                type: "object",
                properties: {
                  step: { type: "string" },
                  status: { type: "string", enum: ["pending", "in_progress", "completed"] },
                },
                required: ["step", "status"],
                additionalProperties: false,
              },
            },
          },
          required: ["plan"],
          additionalProperties: false,
        },
        strict: false,
      }],
    }],
  };
}

function toolCallFrame(argumentsDelta: string): string {
  return `data: ${JSON.stringify({
    id: "chatcmpl_goal",
    object: "chat.completion.chunk",
    model: "deepseek-v4-flash",
    choices: [{
      index: 0,
      delta: {
        tool_calls: [{
          index: 0,
          id: "call_update_plan",
          type: "function",
          function: { name: "update_plan", arguments: argumentsDelta },
        }],
      },
      finish_reason: null,
    }],
  })}\n\n`;
}

function webRunFrame(argumentsDelta: string): string {
  return `data: ${JSON.stringify({
    id: "chatcmpl_web",
    object: "chat.completion.chunk",
    model: "deepseek-v4-flash",
    choices: [{
      index: 0,
      delta: {
        tool_calls: [{
          index: 0,
          id: "call_web_run",
          type: "function",
          function: { name: "web__run", arguments: argumentsDelta },
        }],
      },
      finish_reason: null,
    }],
  })}\n\n`;
}

function setStandaloneWebSearch(enabled: boolean): void {
  if (!isolatedCodexHome) throw new Error("isolated Codex home not installed");
  writeFileSync(
    join(isolatedCodexHome.path, "config.toml"),
    `model_catalog_json = "opencodex-catalog.json"\n\n[features]\nstandalone_web_search = ${enabled ? "true" : "false"}\n`,
    "utf8",
  );
}

async function runGoal(
  upstreamBody: string,
  requestHeaders: Record<string, string> = {},
  includeStableIdentity = true,
): Promise<{
  responseText: string;
  outboundBody: Record<string, unknown>;
  outboundHeaders: Headers;
}> {
  let outboundBody: Record<string, unknown> | undefined;
  let outboundHeaders: Headers | undefined;
  globalThis.fetch = (async (input, init) => {
    const url = input instanceof Request ? input.url : String(input);
    if (url !== CHAT_ENDPOINT) return originalFetch(input, init);
    outboundBody = JSON.parse(String(init?.body)) as Record<string, unknown>;
    outboundHeaders = new Headers(init?.headers);
    return new Response(upstreamBody, { headers: { "content-type": "text/event-stream" } });
  }) as typeof fetch;

  saveConfig(config());
  const server = startServer(0);
  try {
    const response = await originalFetch(new URL("/v1/responses", server.url), {
      method: "POST",
      headers: {
        "content-type": "application/json",
        ...(includeStableIdentity ? { "thread-id": "opencode-go-goal-test-thread" } : {}),
        ...requestHeaders,
      },
      body: JSON.stringify(goalRequestBody()),
    });
    expect(response.status).toBe(200);
    const responseText = await response.text();
    expect(outboundBody).toBeDefined();
    expect(outboundHeaders).toBeDefined();
    return { responseText, outboundBody: outboundBody!, outboundHeaders: outboundHeaders! };
  } finally {
    await server.stop(true);
  }
}

function outboundToolNames(body: Record<string, unknown>): string[] {
  const tools = body.tools as Array<{ function?: { name?: string } }> | undefined;
  return tools?.flatMap(tool => tool.function?.name ? [tool.function.name] : []) ?? [];
}

describe("opencode-go /goal streaming (#2260)", () => {
  test("adds the stable OpenCode Go session header and truthful proxy user agent", async () => {
    const terminal = `data: ${JSON.stringify({
      choices: [{ index: 0, delta: {}, finish_reason: "stop" }],
    })}\n\ndata: [DONE]\n\n`;

    const threadId = "codex-thread-session-header-proof";
    const { outboundHeaders } = await runGoal(terminal, { "thread-id": threadId });

    expect(outboundHeaders.get("x-opencode-session")).toBe(normalizeLogConversationId(threadId));
    expect(outboundHeaders.get("user-agent")).toBe("opencodex");
  });

  test("refuses a Go inference turn before upstream when no stable session identity exists", async () => {
    let upstreamCalled = false;
    globalThis.fetch = (async (input, init) => {
      const url = input instanceof Request ? input.url : String(input);
      if (url === CHAT_ENDPOINT) upstreamCalled = true;
      return originalFetch(input, init);
    }) as typeof fetch;

    saveConfig(config());
    const server = startServer(0);
    try {
      const response = await originalFetch(new URL("/v1/responses", server.url), {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(goalRequestBody()),
      });
      expect(response.status).toBe(400);
      const text = await response.text();
      expect(text).toContain("stable per-conversation session identity");
      expect(upstreamCalled).toBe(false);
    } finally {
      await server.stop(true);
    }
  });

  test("accepts an explicit inbound x-opencode-session as stable identity without forwarding its raw value", async () => {
    const terminal = `data: ${JSON.stringify({
      choices: [{ index: 0, delta: {}, finish_reason: "stop" }],
    })}\n\ndata: [DONE]\n\n`;
    const rawSession = "caller-owned-stable-conversation";
    const { outboundHeaders } = await runGoal(
      terminal,
      { "x-opencode-session": rawSession },
      false,
    );
    expect(outboundHeaders.get("x-opencode-session")).toBe(normalizeLogConversationId(rawSession));
    expect(outboundHeaders.get("x-opencode-session")).not.toBe(rawSession);
  });

  test("native Chat Go path also fails closed before upstream when session identity is absent", async () => {
    let upstreamCalled = false;
    globalThis.fetch = (async (input, init) => {
      const url = input instanceof Request ? input.url : String(input);
      if (url === CHAT_ENDPOINT) upstreamCalled = true;
      return originalFetch(input, init);
    }) as typeof fetch;

    saveConfig(config());
    const server = startServer(0);
    try {
      const response = await originalFetch(new URL("/v1/chat/completions", server.url), {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          model: "opencode-go/deepseek-v4-flash",
          messages: [{ role: "user", content: "hello" }],
          stream: false,
        }),
      });
      expect(response.status).toBe(400);
      const text = await response.text();
      expect(text).toContain("stable per-conversation session identity");
      expect(upstreamCalled).toBe(false);
    } finally {
      await server.stop(true);
    }
  });

  test("native Chat Go path sends the hashed session and proxy user agent", async () => {
    let outboundHeaders: Headers | undefined;
    globalThis.fetch = (async (input, init) => {
      const url = input instanceof Request ? input.url : String(input);
      if (url !== CHAT_ENDPOINT) return originalFetch(input, init);
      outboundHeaders = new Headers(init?.headers);
      return Response.json({
        id: "chatcmpl_session",
        object: "chat.completion",
        model: "deepseek-v4-flash",
        choices: [{ index: 0, message: { role: "assistant", content: "ok" }, finish_reason: "stop" }],
      });
    }) as typeof fetch;

    saveConfig(config());
    const server = startServer(0);
    const rawSession = "direct-chat-session";
    try {
      const response = await originalFetch(new URL("/v1/chat/completions", server.url), {
        method: "POST",
        headers: { "content-type": "application/json", "x-opencode-session": rawSession },
        body: JSON.stringify({
          model: "opencode-go/deepseek-v4-flash",
          messages: [{ role: "user", content: "hello" }],
          stream: false,
        }),
      });
      expect(response.status).toBe(200);
      expect(outboundHeaders?.get("x-opencode-session")).toBe(normalizeLogConversationId(rawSession));
      expect(outboundHeaders?.get("user-agent")).toBe("opencodex");
    } finally {
      await server.stop(true);
    }
  });

  test("Anthropic inbound carries explicit x-opencode-session through internal replay and hashes it for Go", async () => {
    let outboundHeaders: Headers | undefined;
    globalThis.fetch = (async (input, init) => {
      const url = input instanceof Request ? input.url : String(input);
      if (url !== CHAT_ENDPOINT) return originalFetch(input, init);
      outboundHeaders = new Headers(init?.headers);
      return new Response(
        `data: ${JSON.stringify({
          id: "chatcmpl_claude_session",
          object: "chat.completion.chunk",
          model: "deepseek-v4-flash",
          choices: [{ index: 0, delta: { content: "ok" }, finish_reason: null }],
        })}\n\ndata: ${JSON.stringify({
          id: "chatcmpl_claude_session",
          object: "chat.completion.chunk",
          model: "deepseek-v4-flash",
          choices: [{ index: 0, delta: {}, finish_reason: "stop" }],
        })}\n\ndata: [DONE]\n\n`,
        { headers: { "content-type": "text/event-stream" } },
      );
    }) as typeof fetch;

    saveConfig(config());
    const server = startServer(0);
    const rawSession = "anthropic-explicit-session";
    try {
      const response = await originalFetch(new URL("/v1/messages", server.url), {
        method: "POST",
        headers: { "content-type": "application/json", "x-opencode-session": rawSession },
        body: JSON.stringify({
          model: "opencode-go/deepseek-v4-flash",
          max_tokens: 64,
          messages: [{ role: "user", content: "hello" }],
          stream: false,
        }),
      });
      expect(response.status).toBe(200);
      expect(outboundHeaders?.get("x-opencode-session")).toBe(normalizeLogConversationId(rawSession));
      expect(outboundHeaders?.get("user-agent")).toBe("opencodex");
    } finally {
      await server.stop(true);
    }
  });

  test("Codex 0.147 functions namespace authorizes a returned update_plan call", async () => {
    const args = JSON.stringify({
      explanation: "Start the goal",
      plan: [{ step: "Inspect", status: "in_progress" }],
    });
    const terminal = `data: ${JSON.stringify({
      choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }],
    })}\n\ndata: [DONE]\n\n`;

    const { responseText, outboundBody } = await runGoal(toolCallFrame(args) + terminal);

    expect(outboundToolNames(outboundBody)).toContain("update_plan");
    expect(responseText).toContain('"type":"function_call"');
    expect(responseText).toContain('"name":"update_plan"');
    expect(responseText).toContain("event: response.completed");
    expect(responseText).not.toContain("undeclared client tool");
  });

  test("EOF after a complete update_plan call synthesizes a clean tool-call terminal", async () => {
    const args = JSON.stringify({
      explanation: "Start the goal",
      plan: [{ step: "Inspect", status: "in_progress" }],
    });

    const { responseText } = await runGoal(toolCallFrame(args));

    expect(responseText).toContain('"type":"function_call"');
    expect(responseText).toContain('"name":"update_plan"');
    expect(responseText).toContain("response.function_call_arguments.done");
    expect(responseText).toContain("event: response.completed");
    expect(responseText).not.toContain("possible truncation");
  });

  test("EOF with incomplete update_plan arguments still fails closed", async () => {
    const { responseText } = await runGoal(toolCallFrame('{"plan":['));

    expect(responseText).toContain("event: response.failed");
    expect(responseText).toContain("possible truncation");
    expect(responseText).not.toContain("response.function_call_arguments.done");
    expect(responseText).not.toContain("event: response.completed");
  });

  test("Codex standalone web.run is authorized out-of-band without adding it to the upstream tool catalog", async () => {
    setStandaloneWebSearch(true);
    const terminal = `data: ${JSON.stringify({
      choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }],
    })}\n\ndata: [DONE]\n\n`;

    const { responseText, outboundBody } = await runGoal(
      webRunFrame('{"query":"OpenAI Codex CLI documentation"}') + terminal,
      { originator: "codex_exec" },
    );

    expect(outboundToolNames(outboundBody)).not.toContain("web__run");
    expect(responseText).toContain('"type":"function_call"');
    expect(responseText).toContain('"name":"web__run"');
    expect(responseText).toContain("event: response.completed");
    expect(responseText).not.toContain("undeclared client tool");
  });

  test("standalone web.run stays undeclared without a Codex originator", async () => {
    setStandaloneWebSearch(true);
    const terminal = `data: ${JSON.stringify({
      choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }],
    })}\n\ndata: [DONE]\n\n`;

    const { responseText } = await runGoal(
      webRunFrame('{"query":"OpenAI Codex CLI documentation"}') + terminal,
    );

    expect(responseText).toContain("event: response.failed");
    expect(responseText).toContain("undeclared client tool");
    expect(responseText).not.toContain("event: response.completed");
  });

  test("standalone web.run stays undeclared when the Codex feature is disabled", async () => {
    setStandaloneWebSearch(false);
    const terminal = `data: ${JSON.stringify({
      choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }],
    })}\n\ndata: [DONE]\n\n`;

    const { responseText } = await runGoal(
      webRunFrame('{"query":"OpenAI Codex CLI documentation"}') + terminal,
      { originator: "codex_exec" },
    );

    expect(responseText).toContain("event: response.failed");
    expect(responseText).toContain("undeclared client tool");
    expect(responseText).not.toContain("event: response.completed");
  });
});
