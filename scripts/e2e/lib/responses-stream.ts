// The OpenAI Responses API's streamed answer as Codex CLI reads a model turn from it:
// `response.created`, one `response.output_item.done` for each output item, then
// `response.completed` with the usage Codex requires of it (measured on Codex CLI 0.154.0). Two
// stand-ins answer Codex with it and keep no copy of their own: lib/messages-standin.ts, the model
// a proof's Codex sessions call, and the stand-in model of
// packages/claude-envoy/tests/codex-real.test.ts. It imports nothing, so that package's own
// typecheck takes it as it is.

/** One output item of a Responses turn: an assistant message or a function call. */
export type ResponsesItem = Readonly<Record<string, unknown>>;

/** An assistant message item of one `output_text`. */
export function responsesMessage(id: string, text: string): ResponsesItem {
  return {
    id,
    type: "message",
    role: "assistant",
    status: "completed",
    content: [{ type: "output_text", text, annotations: [] }],
  };
}

/** A call of tool `name` with its arguments, a JSON text, as Codex reads a function call. */
export function responsesCall(
  id: string,
  callId: string,
  name: string,
  args: string
): ResponsesItem {
  return { id, type: "function_call", call_id: callId, name, arguments: args };
}

/** The turn `id` answering with `items`, as the Server-Sent Events a streamed request reads. */
export function responsesStream(id: string, items: readonly ResponsesItem[]): string {
  const events: (readonly [string, Readonly<Record<string, unknown>>])[] = [
    ["response.created", { response: { id } }],
    ...items.map((item) => ["response.output_item.done", { item }] as const),
    [
      "response.completed",
      {
        response: {
          id,
          usage: {
            input_tokens: 1,
            input_tokens_details: { cached_tokens: 0 },
            output_tokens: 1,
            output_tokens_details: { reasoning_tokens: 0 },
            total_tokens: 2,
          },
        },
      },
    ],
  ];
  return events
    .map(([type, payload]) => `event: ${type}\ndata: ${JSON.stringify({ type, ...payload })}\n\n`)
    .join("");
}
