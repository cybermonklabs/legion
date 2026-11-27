/**
 * The messages of a session's live context (`liveContext` in
 * `@legion/pi-shared/delivery-pictures`: the branch from its latest compaction's kept entry, or
 * after a later `/clear`), oldest first, each as Oh My Pi handed it to the extensions'
 * `message_end` when the session ran it: what the agent stream keeps of a session a process takes
 * up (`AgentStreamPublisher.restore`). The rule that no body before that cut is read rests on the
 * caller passing the live context alone: on a whole branch, `restore` would read every message's
 * slot, and each card's details.
 *
 * A `message` entry holds that message itself, the host's own object, which `restore` copies for
 * each message it keeps. A `custom_message` entry is how the host writes a custom message, an
 * Envoy card among them, and is read back as the host's own `customMessageEntryMessage` rebuilds
 * it (`packages/tui/src/chat/transcript-entry.ts` at the pinned tag): `role: "custom"`, its type,
 * content, display, details and tag, stamped with the entry's time, which the host writes from the
 * message's own (`appendCustomMessageEntry`). Its content and details are read from the entry when
 * the stream asks for them, and only then: a card's details for its slot, its content for the
 * slot of a card that names no id, and both when `restore` copies a card it keeps. Any other entry
 * (a compaction, a branch summary, a model change, an extension's record) holds no message
 * `message_end` is handed, and is passed over.
 */
export function transcriptMessages(live: readonly unknown[]): unknown[] {
  const messages: unknown[] = [];
  for (const entry of live) {
    if (typeof entry !== "object" || entry === null || !("type" in entry)) continue;
    if (entry.type === "message") {
      if ("message" in entry) messages.push(entry.message);
      continue;
    }
    if (entry.type !== "custom_message") continue;
    const timestamp =
      "timestamp" in entry && typeof entry.timestamp === "string"
        ? Date.parse(entry.timestamp)
        : Number.NaN;
    if (!Number.isFinite(timestamp)) continue;
    messages.push({
      customType: "customType" in entry ? entry.customType : undefined,
      display: "display" in entry ? entry.display : undefined,
      role: "custom",
      timestamp,
      ...("tag" in entry && typeof entry.tag === "string" && { tag: entry.tag }),
      get content(): unknown {
        return Reflect.get(entry, "content");
      },
      get details(): unknown {
        return Reflect.get(entry, "details");
      },
    });
  }
  return messages;
}
