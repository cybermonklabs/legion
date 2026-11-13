import type { DispatchEvent, DisplayTree } from "../../api/types";

/** The tree of one stored text an event's payload carries, by that text's path in the envelope
 *  (`payload.body`, `payload.question`, `payload.resolution.reason`). A payload keeps the shape
 *  every reader of the message bus knows, so its trees ride beside it in `trees` rather than on
 *  it (`api/event_trees.go`); a frame from a Dispatch older than the map carries none, and the
 *  text then renders as the markdown its author wrote, parsed.
 *
 *  It sits here rather than beside either of its callers because both an event's body
 *  (`EventBody.tsx`) and the line that quotes one (`activity-model.ts`,
 *  `issue/event-description.ts`, which draw no element of their own and so must not import a
 *  component) read a tree by the same paths. */
export function payloadTree(event: DispatchEvent, path: string): DisplayTree | undefined {
  return event.trees?.[path];
}
