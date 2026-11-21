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
