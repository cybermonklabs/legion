import { beforeAll, expect, spyOn, test } from "bun:test";
import type { AskThreadPage, CommentThreadPage, MessageThreadPage } from "@legion/contracts";
import { QueryClient } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../../api/client";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import type { PublishedReferences } from "../../refs/reference-names";
import { NoTooltips } from "../NoTooltips";
import { Publisher, publishedNames } from "../projection-fixture";
import { AppProviders } from "../slots/stage-fixtures";
import asksThreadBody from "./fixtures/asks-thread.json";
import commentsInBody from "./fixtures/comments-in.json";
import commentsInBeforeBody from "./fixtures/comments-in-before.json";
import messagesInBody from "./fixtures/messages-in.json";
import { ThreadRow } from "./ThreadRow";
import { type ThreadRecord, threadRecords } from "./thread-records";
import { threadRow } from "./thread-row";

/**
 * A row's first line (R7): the page's one line of its record's root, through the projection the
 * page draws a node's first line with (`node-line.tsx`), over the server's answers captured at
 * the stack base (`fixtures/README.md`).
 */

const KEY = "PDFD720CB-1";
const RECORDS: readonly ThreadRecord[] = [
  ...((commentsInBody as unknown as CommentThreadPage).threads ?? []),
  ...((commentsInBeforeBody as unknown as CommentThreadPage).threads ?? []),
  ...((messagesInBody as unknown as MessageThreadPage).threads ?? []),
  ...((asksThreadBody as unknown as AskThreadPage).threads ?? []),
].flatMap(threadRecords);

/** The record whose id begins with `prefix`, as the fixtures' README names them. */
function record(prefix: string): ThreadRecord {
  const found = RECORDS.find((one) => one.id.startsWith(prefix));
  if (found === undefined) throw new Error(`No page of the fixtures holds the record ${prefix}.`);
  return found;
}

/** The first line the row of `shown` draws, once the projection has answered. */
async function firstLineOf(shown: ThreadRecord, published?: PublishedReferences): Promise<string> {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <AppProviders client={client}>
        {published === undefined ? null : <Publisher published={published} />}
        <NoTooltips>
          <ol>
            <ThreadRow
              href="/"
              id={shown.id}
              issueKey={KEY}
              mark={undefined}
              record={shown}
              text={threadRow(shown, { author: (actor) => actor.id, project: "PDFD720CB" })}
            />
          </ol>
        </NoTooltips>
      </AppProviders>
    </MemoryRouter>
  );
  try {
    const line = () => view.container.querySelector("[data-thread-first-line]")?.textContent ?? "";
    await waitFor(() => expect(line()).not.toBe(""));
    return line();
  } finally {
    view.unmount();
  }
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

test("a row's first line is its root's first block of words, to its sentence's end", async () => {
  expect(await firstLineOf(record("18ddeb81"))).toBe("The list reads three routes.");
  expect(await firstLineOf(record("0d7bfef6"))).toBe("The row draws its first line.");
  expect(await firstLineOf(record("b5979723"))).toBe("Filler comment 38 on the list.");
  expect(await firstLineOf(record("4b7efdcf"))).toBe(
    "Should Waiting on me leave out the daemon's threads?"
  );
});

test("a first line longer than the row is cut at a word with an ellipsis", async () => {
  const root = record("a59f2ee7").root;
  const whole = "body" in root ? (root.body.split("\n")[0] ?? "") : "";
  const drawn = await firstLineOf(record("a59f2ee7"));
  expect(drawn.endsWith("…")).toBe(true);
  const stem = drawn.slice(0, -1);
  expect(stem.length).toBeLessThanOrEqual(100);
  expect(whole.startsWith(stem)).toBe(true);
  expect(whole.charAt(stem.length)).toBe(" ");
});

test("a row's first line reads a reference in its root's words as its target's name", async () => {
  const cited = "dispatch://CORE-2/spec#b-r1";
  const name = "the queue's drain";
  const references = spyOn(api, "getReferences").mockImplementation(() => new Promise(() => {}));
  const base = record("b5979723");
  const shown = {
    ...base,
    root: {
      ...base.root,
      body: `Ask about ${cited} **first**.\n\nThen the rest.`,
      body_tree: undefined,
    },
  } as ThreadRecord;
  try {
    expect(await firstLineOf(shown, publishedNames(new Map([[cited, name]])))).toBe(
      `Ask about ${name} first.`
    );
  } finally {
    references.mockRestore();
  }
});
