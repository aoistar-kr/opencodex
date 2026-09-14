import type { ResponsesTerminalStatus } from "../../bridge";
import {
  COMPACT_PROMPT,
  decodeCompactionSummary,
  encodeHybridCompaction,
} from "../../responses/compaction";
import type { OcxProviderConfig } from "../../types";
import { CODEX_FORWARD_BASE_URL, isCanonicalOpenAiForwardProvider } from "../../providers/openai-tiers";
import { openaiResponsesUrl } from "../../adapters/openai-responses-url";
import { consumeForInspection } from "../relay";
import { fetchWithHeaderTimeout, providerFetch } from "./fetch-helpers";

export type CompletedResponsesSnapshot = {
  id?: unknown;
  output?: unknown;
  status?: unknown;
  error?: unknown;
  [key: string]: unknown;
};

export interface NativeShadowTransport {
  providerName: string;
  provider: OcxProviderConfig;
  headers: Headers;
  model: string;
  connectMs: number;
  /** Exact already-routed Responses URL when the caller has one. */
  url?: string;
}

function assistantOutputText(snapshot: CompletedResponsesSnapshot): string | null {
  if (!Array.isArray(snapshot.output)) return null;
  const chunks: string[] = [];
  for (const item of snapshot.output) {
    if (!item || typeof item !== "object" || Array.isArray(item)) continue;
    const message = item as { type?: unknown; role?: unknown; content?: unknown };
    if (message.type !== "message" || message.role !== "assistant" || !Array.isArray(message.content)) continue;
    for (const part of message.content) {
      if (!part || typeof part !== "object" || Array.isArray(part)) continue;
      const content = part as { type?: unknown; text?: unknown };
      if ((content.type === "output_text" || content.type === "text") && typeof content.text === "string") {
        chunks.push(content.text);
      }
    }
  }
  const text = chunks.join("").trim();
  return text.length > 0 ? text : null;
}

export function nativeCompactionItemFromSnapshot(snapshot: unknown): Record<string, unknown> | null {
  if (!snapshot || typeof snapshot !== "object" || Array.isArray(snapshot)) return null;
  const output = (snapshot as { output?: unknown }).output;
  if (!Array.isArray(output)) return null;
  const items = output.filter((item): item is Record<string, unknown> => (
    !!item
    && typeof item === "object"
    && !Array.isArray(item)
    && (item as { type?: unknown }).type === "compaction"
    && typeof (item as { encrypted_content?: unknown }).encrypted_content === "string"
  ));
  if (items.length !== 1) return null;
  const encrypted = items[0]!.encrypted_content as string;
  // `ocx1`/`ocx2` already carry portable text. Only backend-owned opaque state needs a shadow.
  return decodeCompactionSummary(encrypted) === null ? items[0]! : null;
}

async function completedSnapshotFromResponse(
  response: Response,
  signal: AbortSignal,
): Promise<CompletedResponsesSnapshot | null> {
  if (!response.ok) {
    await response.body?.cancel().catch(() => undefined);
    return null;
  }
  if (response.headers.get("content-type")?.toLowerCase().includes("text/event-stream")) {
    if (!response.body) return null;
    const terminal = { status: "incomplete" as ResponsesTerminalStatus };
    let completed: CompletedResponsesSnapshot | undefined;
    await new Promise<void>(resolve => {
      consumeForInspection(
        response.body!,
        status => { terminal.status = status; },
        signal,
        resolve,
        undefined,
        undefined,
        value => { completed = value as CompletedResponsesSnapshot; },
      );
    });
    return !signal.aborted && terminal.status === "completed" && completed ? completed : null;
  }
  try {
    const json = await response.json() as CompletedResponsesSnapshot;
    return json.status === undefined || json.status === "completed" ? json : null;
  } catch {
    return null;
  }
}

function responsesUrlForNativeShadow(transport: NativeShadowTransport): string {
  if (transport.url) return transport.url;
  const provider = transport.provider;
  if (provider.authMode === "forward") {
    const base = isCanonicalOpenAiForwardProvider(provider)
      ? CODEX_FORWARD_BASE_URL
      : provider.baseUrl.replace(/\/+$/, "");
    return `${base}/responses`;
  }
  if (provider.responsesPath === undefined) return openaiResponsesUrl(provider.baseUrl);
  return `${provider.baseUrl.replace(/\/$/, "")}${provider.responsesPath}`;
}

async function renderPortableSummary(args: {
  signal: AbortSignal;
  transport: NativeShadowTransport;
  compactionItem: Record<string, unknown>;
}): Promise<string | null> {
  if (args.signal.aborted) return null;
  const stateItem = { ...args.compactionItem };
  delete stateItem.id;
  const body = {
    model: args.transport.model,
    stream: true,
    store: false,
    tools: [],
    input: [
      stateItem,
      {
        type: "message",
        role: "user",
        content: [{ type: "input_text", text: COMPACT_PROMPT }],
      },
    ],
  };
  try {
    const response = await fetchWithHeaderTimeout(
      responsesUrlForNativeShadow(args.transport),
      {
        method: "POST",
        headers: args.transport.headers,
        body: JSON.stringify(body),
      },
      args.signal,
      args.transport.connectMs,
      false,
      providerFetch(args.transport.provider, undefined, {
        providerName: args.transport.providerName,
        modelId: args.transport.model,
      }),
      args.transport.provider.authMode === "forward",
    );
    const completed = await completedSnapshotFromResponse(response, args.signal);
    return completed ? assistantOutputText(completed) : null;
  } catch {
    return null;
  }
}

/**
 * Attach portable plaintext to one backend-owned native compaction item. This is deliberately
 * fail-soft: a shadow failure returns the ORIGINAL snapshot byte-semantically at the object level.
 */
export async function addPortableShadowToSnapshot(args: {
  snapshot: CompletedResponsesSnapshot;
  signal: AbortSignal;
  transport: NativeShadowTransport;
  origin?: string | null;
}): Promise<CompletedResponsesSnapshot> {
  const nativeItem = nativeCompactionItemFromSnapshot(args.snapshot);
  if (!nativeItem || args.signal.aborted || !Array.isArray(args.snapshot.output)) return args.snapshot;
  const summary = await renderPortableSummary({
    signal: args.signal,
    transport: args.transport,
    compactionItem: nativeItem,
  });
  if (!summary || summary.trim().length === 0) return args.snapshot;
  const native = typeof nativeItem.encrypted_content === "string" ? nativeItem.encrypted_content : "";
  if (native.length === 0 || decodeCompactionSummary(native) !== null) return args.snapshot;
  let wrapped: Record<string, unknown>;
  try {
    wrapped = { ...nativeItem, encrypted_content: encodeHybridCompaction(native, summary, args.origin) };
  } catch {
    return args.snapshot;
  }
  return {
    ...args.snapshot,
    output: args.snapshot.output.map(item => item === nativeItem ? wrapped : item),
  };
}
