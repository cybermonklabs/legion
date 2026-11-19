import type { Artifact } from "../../api/types";

// The roots a Dispatch document's room holds, named as the server names them
// (packages/envoy/internal/dispatch/docs/tree.go): a rename on one side alone would leave the
// browser reading an empty root. They live in a module of their own because the pending sync is a
// lazy chunk of its own: importing them from editor.ts would tie that chunk to the editor's module
// and the shell chunk behind it.

/** The editor's tree in a room that is its own document, and an issue room's own nodes. */
export const DEFAULT_FRAGMENT_NAME = "prosemirror";
/** The margin records the server projects. */
export const MARKS_MAP_NAME = "marks";

/** The editor's tree of `artifact`'s room: the fragment the artifact read names
 *  (`Artifact.fragment`), and `DEFAULT_FRAGMENT_NAME` where it names none, as a server of the
 *  release before the issue room (INFRA-1253) answers. */
export function fragmentOf(artifact: Artifact): string {
  return artifact.fragment ?? DEFAULT_FRAGMENT_NAME;
}

/** Whether an answer about `artifact` says which fragment holds it: it carries `fragment`, as
 *  every artifact a server with issue rooms answers does. */
export function carriesFragment(artifact: Artifact): boolean {
  return artifact.fragment !== undefined;
}
