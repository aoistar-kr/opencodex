function nonEmptyString(value: unknown): value is string {
  return typeof value === "string" && value.trim().length > 0;
}

function standaloneFunctionOutputText(output: unknown): string | null {
  if (typeof output === "string") return output;
  if (!Array.isArray(output)) return null;
  const text: string[] = [];
  for (const part of output) {
    if (!part || typeof part !== "object" || Array.isArray(part)) return null;
    const record = part as Record<string, unknown>;
    if (!["input_text", "output_text", "text"].includes(String(record.type))) return null;
    if (typeof record.text !== "string") return null;
    text.push(record.text);
  }
  return text.join("\n");
}

function normalizeStandaloneCodexAppOutput(item: Record<string, unknown>): Record<string, unknown> | null {
  if (item.type !== "function_call_output") return null;
  if (nonEmptyString(item.call_id)) return null;
  if (item.namespace !== "codex_app" || !nonEmptyString(item.name)) return null;
  const output = standaloneFunctionOutputText(item.output);
  if (output === null) return null;
  return {
    type: "message",
    role: "developer",
    content: [{
      type: "input_text",
      text: `Codex app event ${item.namespace}/${item.name}\n${output}`,
    }],
  };
}

/**
 * Codex persists a few private history items that are valid inside app-server but not on the
 * public Responses wire. Public/third-party destinations must receive ordinary messages instead:
 *
 * - `agent_message` is the private multi-agent record.
 * - a named `codex_app` `function_call_output` may intentionally have no `call_id`; public
 *   Responses requires one, and inventing a fake orphan id would corrupt tool semantics.
 *
 * Convert only the text/public-message-compatible subset and leave ciphertext/unknown parts
 * untouched so the encrypted-task recovery path keeps its fail-closed authority.
 */
export function normalizeRoutedAgentMessages(body: unknown): unknown {
  if (!body || typeof body !== "object" || Array.isArray(body)) return body;
  const record = body as Record<string, unknown>;
  if (!Array.isArray(record.input)) return body;
  let changed = false;
  const input = record.input.map((item: unknown) => {
    if (!item || typeof item !== "object" || Array.isArray(item)) return item;
    const message = item as Record<string, unknown>;
    const standaloneOutput = normalizeStandaloneCodexAppOutput(message);
    if (standaloneOutput) {
      changed = true;
      return standaloneOutput;
    }
    if (message.type !== "agent_message" || !Array.isArray(message.content) || message.content.length === 0) return item;
    if (!message.content.every(part => part && typeof part === "object"
      && ["input_text", "input_image", "input_file"].includes((part as Record<string, unknown>).type as string))) return item;
    const identities = Object.fromEntries(["author", "recipient"]
      .filter(key => typeof message[key] === "string")
      .map(key => [key, message[key]]));
    changed = true;
    return {
      type: "message",
      role: "user",
      content: [
        ...(Object.keys(identities).length
          ? [{ type: "input_text", text: `Agent message ${JSON.stringify(identities)}` }]
          : []),
        ...message.content,
      ],
    };
  });
  return changed ? { ...record, input } : body;
}
