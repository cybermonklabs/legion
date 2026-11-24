import { expect, test } from "bun:test";
import { render } from "@testing-library/react";
import type { ReactNode } from "react";

import {
  PublishedReferenceNames,
  type PublishedReferences,
  ReferenceNamesProvider,
  useKnownReferences,
  usePublishReferenceNames,
} from "./reference-names";

/**
 * The names store (`reference-names.tsx`) carries each issue's status a page's batched read
 * answered beside its name, which the mark beside a reference to the issue draws (INFRA-4009 R2).
 * Every page on screen publishes its own read, and the store hands readers one map of them all:
 * where two publishers answer one issue, the one that published last stands, since a publisher
 * publishes again each time its read answers anew and so holds the newest answer.
 */

/** Publishes `published` for as long as it is drawn, as a page publishes its batched read. */
function Publisher({ published }: { published: PublishedReferences }): ReactNode {
  usePublishReferenceNames(published);
  return null;
}

/** What a page publishes of its batched read: each issue named by `title` and answered with its
 *  status, made anew on every call, as the page's own read makes its parts anew each time it
 *  answers. A page whose read named no issue leaves `statuses` out. */
function publication(statuses: Readonly<Record<string, string>> | undefined): PublishedReferences {
  const references = Object.keys(statuses ?? {});
  return {
    ambiguous: new Set(),
    asked: new Set(references),
    gone: new Set(),
    readings: new Map(references.map((reference) => [reference, { words: "title" }])),
    ...(statuses === undefined ? {} : { statuses: new Map(Object.entries(statuses)) }),
    unnamed: new Set(),
  };
}

/** What every reader below last read: the statuses and the names, as plain entries. */
interface Read {
  statuses: [string, string][];
  names: [string, string][];
}

/** Reads the store's statuses and names into `read` on every render, as a surface does. */
function Reader({ read }: { read: Read }): ReactNode {
  const { names, statuses } = useKnownReferences();
  read.statuses = [...statuses].sort(([left], [right]) => left.localeCompare(right));
  read.names = [...names].sort(([left], [right]) => left.localeCompare(right));
  return null;
}

/** The store with each of `publications` published in order, the first drawn first, and a reader
 *  of it. */
function store(publications: readonly (PublishedReferences | undefined)[], read: Read): ReactNode {
  return (
    <ReferenceNamesProvider>
      {publications.map((published, at) =>
        published === undefined ? null : (
          // A publisher's place is its identity, so one that leaves keeps the others' keys.
          // biome-ignore lint/suspicious/noArrayIndexKey: each slot is one publisher throughout.
          <Publisher key={at} published={published} />
        )
      )}
      <Reader read={read} />
    </ReferenceNamesProvider>
  );
}

test("two publishers' statuses merge, the last to publish standing where both answer one issue", () => {
  const read: Read = { names: [], statuses: [] };
  const first = publication({ "dispatch://CORE-1": "in_progress", "dispatch://CORE-2": "todo" });
  const second = publication({ "dispatch://CORE-1": "done", "dispatch://CORE-3": "testing" });
  const view = render(store([first, second], read));
  try {
    // The second published after the first, so its answer for CORE-1 stands.
    expect(read.statuses).toEqual([
      ["dispatch://CORE-1", "done"],
      ["dispatch://CORE-2", "todo"],
      ["dispatch://CORE-3", "testing"],
    ]);
    expect(read.names.map(([reference]) => reference)).toEqual([
      "dispatch://CORE-1",
      "dispatch://CORE-2",
      "dispatch://CORE-3",
    ]);

    // The first's read answers anew: it published last now, so its answer stands.
    const answered = publication({
      "dispatch://CORE-1": "needs_review",
      "dispatch://CORE-2": "todo",
    });
    view.rerender(store([answered, second], read));
    expect(read.statuses).toEqual([
      ["dispatch://CORE-1", "needs_review"],
      ["dispatch://CORE-2", "todo"],
      ["dispatch://CORE-3", "testing"],
    ]);

    // A page whose read names no issue publishes no statuses, and leaves the others' standing.
    view.rerender(store([answered, second, publication(undefined)], read));
    expect(read.statuses).toEqual([
      ["dispatch://CORE-1", "needs_review"],
      ["dispatch://CORE-2", "todo"],
      ["dispatch://CORE-3", "testing"],
    ]);

    // A publisher that leaves takes its own statuses with it, and no other's.
    view.rerender(store([answered, undefined, publication(undefined)], read));
    expect(read.statuses).toEqual([
      ["dispatch://CORE-1", "needs_review"],
      ["dispatch://CORE-2", "todo"],
    ]);
    view.rerender(store([], read));
    expect(read.statuses).toEqual([]);
  } finally {
    view.unmount();
  }
});

// A page's own subtree reads the page's value in the render that draws it
// (`PublishedReferenceNames`), and a status, which names nothing without its name, is the store's
// alone, as a name is: the subtree reads the statuses the store merged.
test("a reader under a page's own subtree reads the statuses the store holds", () => {
  const read: Read = { names: [], statuses: [] };
  const page = publication({ "dispatch://CORE-1": "in_progress" });
  const other = publication({ "dispatch://CORE-2": "done" });
  const view = render(
    <ReferenceNamesProvider>
      <Publisher published={page} />
      <Publisher published={other} />
      <PublishedReferenceNames published={page}>
        <Reader read={read} />
      </PublishedReferenceNames>
    </ReferenceNamesProvider>
  );
  try {
    expect(read.statuses).toEqual([
      ["dispatch://CORE-1", "in_progress"],
      ["dispatch://CORE-2", "done"],
    ]);
  } finally {
    view.unmount();
  }
});
