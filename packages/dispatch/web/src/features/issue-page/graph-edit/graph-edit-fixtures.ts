import { QueryClient } from "@tanstack/react-query";
import * as Y from "yjs";

import type { EditorHandle } from "../../doc/editor";
import { KEY } from "../decisions-filter/decisions-fixtures";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import recorded from "./__fixtures__/graph-edit.json";

/** The instant each recorded time is read as. */
const RECORDED_AT = "2026-01-01T00:00:00Z";

/**
 * The server's own answers the graph-edit cases read, `__fixtures__/graph-edit.json`, with each
 * time as one RFC 3339 instant. The Go test that records the file writes every time as `<time>`,
 * so the file is byte for byte what each run records (`graph_edit_web_fixtures_test.go`, which CI
 * compares with the file), and the file is never edited by hand; the page parses each time it
 * draws or orders by (`lib/timestamps.ts`), which a placeholder is not. Only a value that is a
 * whole `<time>` is read so: a time inside a refusal's words stays as recorded.
 */
export const fixture: typeof recorded = JSON.parse(
  JSON.stringify(recorded).replaceAll('"<time>"', JSON.stringify(RECORDED_AT))
);

/** The fixture's stable name of the issue, which no route of the page takes for a key. */
export const NAMED = "<issue>";

/** `value` with the fixture's name of an issue, `from` (this issue's by default), written as the
 *  key the routes take: a name for a name, no other change to what the server answered. */
export function renamed<T>(value: T, from = NAMED): T {
  return JSON.parse(JSON.stringify(value).replaceAll(from, KEY)) as T;
}

/** The node of read id `id` in `held`, which the fixture's read holds. */
export function nodeOf(held: TreeAnswer, id: string): TreeAnswerNode {
  const found = held.nodes?.find((each) => each.id === id);
  if (found === undefined) throw new Error(`the fixture's read holds no node ${id}`);
  return found;
}

/** A client signed in as alice, which retries no failed read and no failed write. */
export function signedIn(): QueryClient {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  return client;
}

/** A block of a document as the room holds it, in ProseMirror's JSON (`IssueTree.test.tsx`'s
 *  `LiveBlock`): its type, its attributes - its `blockId` among them - and what it holds. */
export interface LiveBlock {
  readonly attrs?: Readonly<Record<string, string | number>>;
  readonly content?: readonly (LiveBlock | { readonly text: string; readonly type: "text" })[];
  readonly type: string;
}

/** `block` as the room holds it: a Yjs element of its type and attributes, holding its words and
 *  its blocks, as y-prosemirror writes a document there. */
export function roomBlock(block: LiveBlock): Y.XmlElement {
  const element = new Y.XmlElement(block.type);
  const attributes = element as Y.XmlElement<Record<string, string | number>>;
  for (const [name, value] of Object.entries(block.attrs ?? {})) {
    attributes.setAttribute(name, value);
  }
  element.insert(
    0,
    (block.content ?? []).map((child) =>
      "text" in child ? new Y.XmlText(child.text) : roomBlock(child)
    )
  );
  return element;
}

/** A paragraph of `words`, whose `blockId` is `blockId` where it has one. */
export function paragraphOf(words: string, blockId?: string): LiveBlock {
  return {
    ...(blockId === undefined ? {} : { attrs: { blockId } }),
    content: [{ text: words, type: "text" }],
    type: "paragraph",
  };
}

/** The room's copy of a document and the browser's, each update to either relayed to the other,
 *  and the row's editor of it, once made (`IssueTree.test.tsx`'s `LiveRoom`). */
export interface LiveRoom {
  readonly browser: Y.Doc;
  editor?: EditorHandle;
  readonly server: Y.Doc;
}

/** A room whose two copies hold nothing yet, and no editor of it. */
export function liveRoom(): LiveRoom {
  const browser = new Y.Doc();
  const server = new Y.Doc();
  for (const [from, to] of [
    [server, browser],
    [browser, server],
  ] as const) {
    from.on("update", (update: Uint8Array, origin: unknown) => {
      if (origin !== "relay") Y.applyUpdate(to, update, "relay");
    });
  }
  return { browser, server };
}

/** Each block of the document `room` holds, as the room holds it: its type, its block id and its
 *  words. Strings, never nodes, are what a case compares. */
export function fragmentOf(room: LiveRoom | undefined): string[] {
  // `Y.XmlText.toString()` writes formatting marks (`proofAuthored`) as XML tags; its delta holds
  // the block's plain words.
  const textOf = (text: Y.XmlText): string =>
    (text.toDelta() as readonly { readonly insert?: unknown }[])
      .map((op) => (typeof op.insert === "string" ? op.insert : ""))
      .join("");
  const wordsIn = (element: Y.XmlElement): string =>
    element
      .toArray()
      .map((child) =>
        child instanceof Y.XmlText
          ? textOf(child)
          : child instanceof Y.XmlElement
            ? wordsIn(child)
            : ""
      )
      .join("");
  return (room?.server.getXmlFragment("prosemirror").toArray() ?? []).map((block) =>
    block instanceof Y.XmlElement
      ? `${block.nodeName} ${String(block.getAttribute("blockId"))}: ${wordsIn(block)}`
      : "text"
  );
}
