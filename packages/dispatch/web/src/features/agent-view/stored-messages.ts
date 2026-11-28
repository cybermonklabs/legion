import type { MessageRead, MessageWithTrees } from "../../api/types";

/** Every message of a session's stored thread reads, flat: each root and the replies under it,
 *  in the order the reads hold them. A read from a Dispatch that answers no replies carries
 *  none, which is its root alone. */
export function storedMessages(stored: readonly MessageRead[]): MessageWithTrees[] {
  return stored.flatMap((read) => [read.message, ...(read.replies ?? [])]);
}
