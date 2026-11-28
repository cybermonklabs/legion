import type { ComponentProps, ReactNode } from "react";

import { MarkdownBody } from "./MarkdownBody";
import { MarkdownPreview } from "./MarkdownPreview";
import { DeletedBody } from "./PostMarks";
import type { PostDeletion } from "./post-marks";

/**
 * One post's body: the Markdown its author wrote, or the deleted line in its place. Every
 * surface that prints a comment's or an issue message's whole body goes through here rather
 * than through `MarkdownBody` itself, so the mark is written once. The body is drawn by the one
 * renderer, on the surface its caller names (`surface`), which it hands on as its own: the
 * reading list's walk reads each mount of this as a mount of the renderer
 * (`surface-lists.test.ts`), so every caller names its row of `DISPATCH_READING_SURFACES`.
 */
export function PostBody({
  deleted,
  surface,
  ...body
}: ComponentProps<typeof MarkdownBody> & { deleted: PostDeletion | null }): ReactNode {
  return deleted === null ? (
    <MarkdownBody {...body} surface={surface} />
  ) : (
    <DeletedBody deleted={deleted} onRendered={body.onRendered} />
  );
}

/** `PostBody` for the surfaces that clamp one rendered line of a post (`MarkdownPreview`): a
 *  hover card, an unfurl card, a reply quote, the margin's collapsed root. It hands its caller's
 *  `surface` on to the preview as `PostBody` does. */
export function PostPreview({
  deleted,
  surface,
  ...preview
}: ComponentProps<typeof MarkdownPreview> & { deleted: PostDeletion | null }): ReactNode {
  return deleted === null ? (
    <MarkdownPreview {...preview} surface={surface} />
  ) : (
    <DeletedBody className={preview.className} deleted={deleted} />
  );
}
