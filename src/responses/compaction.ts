import { createHash } from "node:crypto";

/**
 * Remote compaction v2 support for ROUTED providers.
 *
 * Codex decides "this provider supports remote compaction" by provider name (built-in `OpenAI`),
 * and Design B points that provider at this proxy — so Codex sends remote compaction v2 requests
 * for EVERY routed model. The request is a normal /responses call whose input ends with
 * `{"type":"compaction_trigger"}`; codex-rs `collect_compaction_output` then requires the stream
 * to carry EXACTLY ONE `{"type":"compaction","encrypted_content":...}` output item
 * (compact_remote_v2.rs) or it fatals with "expected exactly one compaction output item".
 *
 * Routed models cannot produce OpenAI's encrypted blob, so the proxy runs the model as a plain
 * summarizer and wraps the summary text in a transparent envelope: `ocx1:` + base64(utf8 summary).
 * Codex stores the item and replays it in later input; the parser decodes our envelope back into
 * plain text for routed models. Real OpenAI-encrypted blobs (no `ocx1:` prefix) are opaque —
 * routed models get a short "history was compacted" note instead.
 */

export const OCX_COMPACTION_PREFIX = "ocx1:";
export const OCX_HYBRID_COMPACTION_PREFIX = "ocx2:";

/** Mirrors codex-rs core/templates/compact/prompt.md (the local-compaction instruction). */
export const COMPACT_PROMPT = `You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.

Include:
- Current progress and key decisions made
- Important context, constraints, or user preferences
- What remains to be done (clear next steps)
- Any critical data, examples, or references needed to continue

Be concise, structured, and focused on helping the next LLM seamlessly continue the work.`;

/** Mirrors codex-rs core/templates/compact/summary_prefix.md (framing for a replayed summary). */
export const SUMMARY_PREFIX = "Another language model started to solve this problem and produced a summary of its thinking process. You also have access to the state of the tools that were used by that language model. Use this to build on the work that has already been done and avoid duplicating work. Here is the summary produced by the other language model, use the information in this summary to assist with your own analysis:";

export const OPAQUE_COMPACTION_NOTE = "[earlier conversation was compacted; the summary is stored in a format this model cannot read]";

/**
 * Item types in the compact wire family. Each carries an `encrypted_content` blob the client
 * replays verbatim on every later turn, and the minting backend verifies it is unmodified.
 *
 * Keep this the only enumeration: a copy that listed just `compaction` let the response-side
 * field backfill synthesize ids into the other two, which the client then replayed as "modified
 * from the compact response".
 */
const COMPACTION_ITEM_TYPES: ReadonlySet<string> = new Set([
  "compaction",
  "compaction_summary",
  "context_compaction",
]);

export function isCompactionItemType(type: unknown): boolean {
  return typeof type === "string" && COMPACTION_ITEM_TYPES.has(type);
}

export function encodeCompactionSummary(summary: string): string {
  return OCX_COMPACTION_PREFIX + Buffer.from(summary, "utf-8").toString("base64");
}

export interface HybridCompactionEnvelope {
  native: string;
  summary: string;
  origin: string | null;
}

export function compactionOriginFingerprint(parts: readonly (string | undefined)[]): string | null {
  if (parts.length !== 5 || parts.some(part => typeof part !== "string" || part.length === 0)) return null;
  return createHash("sha256")
    .update("ocx2-origin-v1\0")
    .update(JSON.stringify(parts))
    .digest("hex");
}

/**
 * Keep the provider-owned blob byte-for-byte while attaching a provider-neutral summary.
 * Only the summary is base64 encoded; re-encoding the already opaque native blob would add ~33%
 * wire/storage overhead for no benefit. Standard base64 never contains `:`, so the first delimiter
 * after the prefix is unambiguous even if the native payload itself contains colons.
 */
export function encodeHybridCompaction(native: string, summary: string, origin?: string | null): string {
  if (native.length === 0 || summary.length === 0) throw new Error("hybrid compaction requires native state and summary");
  const originField = origin && /^[0-9a-f]{64}$/.test(origin) ? origin : "-";
  return `${OCX_HYBRID_COMPACTION_PREFIX}${Buffer.from(summary, "utf-8").toString("base64")}:${originField}:${native}`;
}

export function decodeHybridCompaction(encryptedContent: string): HybridCompactionEnvelope | null {
  if (!encryptedContent.startsWith(OCX_HYBRID_COMPACTION_PREFIX)) return null;
  const payload = encryptedContent.slice(OCX_HYBRID_COMPACTION_PREFIX.length);
  const separator = payload.indexOf(":");
  if (separator <= 0 || separator === payload.length - 1) return null;
  const encodedSummary = payload.slice(0, separator);
  const remainder = payload.slice(separator + 1);
  const originSeparator = remainder.indexOf(":");
  let origin: string | null = null;
  let native = remainder;
  if (originSeparator > 0) {
    const candidate = remainder.slice(0, originSeparator);
    if (candidate === "-" || /^[0-9a-f]{64}$/.test(candidate)) {
      origin = candidate === "-" ? null : candidate;
      native = remainder.slice(originSeparator + 1);
    }
  }
  // Buffer's base64 decoder is deliberately permissive (it ignores junk characters), but this is
  // a proxy-owned wire format. Reject a mutated/noncanonical wrapper instead of silently decoding a
  // different checkpoint. `Buffer.toString("base64")` always emits the standard padded alphabet.
  if (!/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(encodedSummary)) return null;
  try {
    const summary = Buffer.from(encodedSummary, "base64").toString("utf-8");
    if (summary.length === 0 || native.length === 0) return null;
    return { native, summary, origin };
  } catch {
    return null;
  }
}

/** Decode a provider-neutral envelope; returns null for real native encrypted blobs or garbage. */
export function decodeCompactionSummary(encryptedContent: string): string | null {
  const hybrid = decodeHybridCompaction(encryptedContent);
  if (hybrid) return hybrid.summary;
  if (!encryptedContent.startsWith(OCX_COMPACTION_PREFIX)) return null;
  try {
    return Buffer.from(encryptedContent.slice(OCX_COMPACTION_PREFIX.length), "base64").toString("utf-8");
  } catch {
    return null;
  }
}

/** Render a replayed compaction item as plain user-visible text for a routed model. */
export function compactionItemToText(encryptedContent: string | undefined): string {
  const decoded = typeof encryptedContent === "string" ? decodeCompactionSummary(encryptedContent) : null;
  return decoded ? `${SUMMARY_PREFIX}\n\n${decoded}` : OPAQUE_COMPACTION_NOTE;
}

/**
 * Remote compaction v1 (`POST /responses/compact`, unary) — codex-rs installs the returned
 * `{"output":[ResponseItem...]}` as the REPLACEMENT history (compact_remote.rs
 * process_compacted_history). Mirror codex-rs local `build_compacted_history`: recent real user
 * messages within a token budget, then one user message `SUMMARY_PREFIX\n<summary>`. Plain user
 * message items parse as real user messages on the codex side (event_mapping parse_user_message);
 * contextual wrappers are filtered there, and v2-style `compaction` items are NOT expected here.
 */

/** codex-rs compact.rs COMPACT_USER_MESSAGE_MAX_TOKENS = 20k tokens (~4 chars/token). */
const COMPACT_V1_RETAINED_CHAR_BUDGET = 20_000 * 4;

/** Extract plain-text user messages from a Responses `input` array (for v1 compact retention). */
export function extractCompactUserMessages(input: unknown): string[] {
  if (!Array.isArray(input)) return [];
  const out: string[] = [];
  for (const item of input) {
    if (!item || typeof item !== "object" || Array.isArray(item)) continue;
    const rec = item as { type?: string; role?: string; content?: unknown };
    if (rec.type !== undefined && rec.type !== "message") continue;
    if (rec.role !== "user") continue;
    let text = "";
    if (typeof rec.content === "string") text = rec.content;
    else if (Array.isArray(rec.content)) {
      text = rec.content
        .map(b => {
          if (!b || typeof b !== "object") return "";
          const block = b as { type?: string; text?: string };
          return (block.type === "input_text" || block.type === "text") && typeof block.text === "string" ? block.text : "";
        })
        .join("");
    }
    if (text.trim().length > 0) out.push(text);
  }
  return out;
}

function compactUserMessageItem(text: string): Record<string, unknown> {
  return { type: "message", role: "user", content: [{ type: "input_text", text }] };
}

/** Build the v1 compact `output` array: retained recent user messages + the summary message. */
export function buildCompactV1Output(userMessages: string[], summary: string): Record<string, unknown>[] {
  const selected: string[] = [];
  let remaining = COMPACT_V1_RETAINED_CHAR_BUDGET;
  for (let i = userMessages.length - 1; i >= 0 && remaining > 0; i--) {
    const msg = userMessages[i];
    if (msg.length <= remaining) {
      selected.push(msg);
      remaining -= msg.length;
    } else {
      // Budget partially covers this older message: keep its tail (most recent context) and stop.
      let tailStart = msg.length - remaining;
      // Never start the retained tail on a lone LOW surrogate: the pair's
      // other half would be lost and encoding substitutes U+FFFD.
      if (tailStart > 0 && tailStart < msg.length) {
        const first = msg.charCodeAt(tailStart);
        if (first >= 0xdc00 && first <= 0xdfff) tailStart += 1;
      }
      selected.push(msg.slice(tailStart));
      break;
    }
  }
  selected.reverse();
  // codex-rs compact.rs uses "{SUMMARY_PREFIX}\n{summary}" (single newline) and detects stored
  // summaries by that exact prefix — keep the same shape.
  const summaryText = summary.trim().length > 0 ? `${SUMMARY_PREFIX}\n${summary}` : "(no summary available)";
  return [...selected.map(compactUserMessageItem), compactUserMessageItem(summaryText)];
}
