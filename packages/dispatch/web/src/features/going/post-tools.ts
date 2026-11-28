/** A tool a command-bar plan's row edits or deletes a comment or an issue message with: the kind
 *  of post it acts on, and whether it deletes the post rather than edit its body. */
export interface PostTool {
  readonly kind: "comment" | "message";
  readonly deletes: boolean;
}

/** The tools that edit or delete a post, by name (going_observe.go `goingPostTools`). A row of one
 *  reads the post's author and its body as its value now, and once the post is deleted, the
 *  delete's mark and no body (`goingPostNow`). */
const POST_TOOLS: Readonly<Record<string, PostTool>> = {
  dispatch_delete_comment: { deletes: true, kind: "comment" },
  dispatch_delete_message: { deletes: true, kind: "message" },
  dispatch_edit_comment: { deletes: false, kind: "comment" },
  dispatch_edit_message: { deletes: false, kind: "message" },
};

/** The post tool `tool` is, or undefined for a tool that edits or deletes no post. */
export function postToolOf(tool: string): PostTool | undefined {
  return Object.hasOwn(POST_TOOLS, tool) ? POST_TOOLS[tool] : undefined;
}
