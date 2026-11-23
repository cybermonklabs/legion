/** The text of `message`'s first content block when `message` has `role` and that block has text:
 *  where `withDispatchFirst` and `withSessionAnswers` each look for a copy of their own, and how
 *  `withSessionAnswers` reads the host's own prompts. */
export function leadingText(message: unknown, role: string): string | undefined {
  if (
    typeof message !== "object" ||
    message === null ||
    !("role" in message) ||
    message.role !== role ||
    !("content" in message) ||
    !Array.isArray(message.content)
  ) {
    return undefined;
  }
  const [first]: unknown[] = message.content;
  if (
    typeof first === "object" &&
    first !== null &&
    "text" in first &&
    typeof first.text === "string"
  ) {
    return first.text;
  }
  return undefined;
}

/** A user message of one text block for `first` and one for each of `rest`, in order, stamped 0,
 *  as `withDispatchFirst` adds its skill (one block) and `withSessionAnswers` adds or rebuilds an
 *  answers message: a list and the sentence saying it replaces every earlier one, or the failure
 *  line alone. A message carries at least one block. */
export function userTextMessage(first: string, ...rest: readonly string[]): unknown {
  return {
    role: "user",
    content: [first, ...rest].map((text) => ({ type: "text", text })),
    timestamp: 0,
  };
}
