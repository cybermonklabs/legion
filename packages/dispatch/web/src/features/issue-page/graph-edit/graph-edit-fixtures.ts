import { QueryClient } from "@tanstack/react-query";

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
