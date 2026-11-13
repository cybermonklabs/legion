import type { Message } from "../api/types";

/**
 * The marks the server writes on every message that a fixture has no value of its own for: when
 * its author last replaced its body, and when they deleted it with who they are. All three are
 * null until the author corrects the post.
 *
 * Spread it over a fixture, not under it, when the fixture sets one of them itself.
 */
export function messageCorrectionFields(): Pick<
  Message,
  "deleted_at" | "deleted_by" | "edited_at"
> {
  return { deleted_at: null, deleted_by: null, edited_at: null };
}
