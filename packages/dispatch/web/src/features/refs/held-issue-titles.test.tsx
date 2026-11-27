import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type RenderResult, render, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { api } from "../../api/client";
import type { IssueReferenceSummary } from "../../api/types";
import { RowText } from "../issue-page/RowText";
import { HeldIssueTitles } from "./held-issue-titles";
import { MarkdownBody } from "./MarkdownBody";
import { warmMarkdownRenderer } from "./markdown-engine";
import {
  HeldReferenceNames,
  NOTHING_PUBLISHED,
  PublishedReferenceNames,
  ReferenceNamesProvider,
} from "./reference-names";

/**
 * A reference to an issue whose title the page already holds (`HeldIssueTitles`) is drawn with
 * its key and that title from the first drawing, as its summary will draw it, so the line it
 * stands in does not grow when the summary answers (R3; the clock rows' Row 2 at 1440 px, where a
 * comment naming a child issue grew a line). With no titles held - every page but the new issue
 * page - the reference reads as its author wrote it until the summary answers, as before.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

const spies: { mockRestore(): void }[] = [];
afterEach(() => {
  for (const spy of spies.splice(0)) spy.mockRestore();
});

/** The summary read held unanswered, so a case reads the reference before it answers. */
function summaryHeld(): void {
  const { promise } = Promise.withResolvers<IssueReferenceSummary>();
  spies.push(spyOn(api, "getIssueSummary").mockReturnValue(promise));
}

function drawn(children: ReactNode): RenderResult {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/issues/CORE-1"]}>{children}</MemoryRouter>
    </QueryClientProvider>
  );
}

test("a reference to an issue the page holds the title of reads as its key and title before its summary answers", async () => {
  summaryHeld();
  const view = drawn(
    <HeldIssueTitles.Provider value={new Map([["CORE-17", "Child 17: the filter pages"]])}>
      <MarkdownBody markdown="As it does on dispatch://CORE-17." />
    </HeldIssueTitles.Provider>
  );
  try {
    await within(view.container).findByRole("link", { name: "CORE-17 Child 17: the filter pages" });
  } finally {
    view.unmount();
  }
});

test("with no titles held, a reference reads as its author wrote it until its summary answers", async () => {
  summaryHeld();
  const view = drawn(<MarkdownBody markdown="As it does on dispatch://CORE-17." />);
  try {
    const link = await within(view.container).findByRole("link");
    expect(link.textContent).toBe("dispatch://CORE-17");
  } finally {
    view.unmount();
  }
});

const CHILD = "dispatch://CORE-17";
const ROW_WORDS = `As it does on ${CHILD}.`;

/** A row's words that cite CORE-17, on a page that asked its batched read of names for it and
 *  holds no answer yet, so the row waits on that read rather than reading the issue itself. */
function rowAsked(): ReactNode {
  return (
    <PublishedReferenceNames published={{ ...NOTHING_PUBLISHED, asked: new Set([CHILD]) }}>
      <RowText markdown={ROW_WORDS} tree={displayTree(ROW_WORDS)} />
    </PublishedReferenceNames>
  );
}

test("a row's reference to an issue the page holds the title of reads as its key and title before the page's read of names answers", () => {
  summaryHeld();
  const view = drawn(
    <ReferenceNamesProvider>
      <HeldReferenceNames held={new Map([[CHILD, "Child 17: the filter pages"]])}>
        {rowAsked()}
      </HeldReferenceNames>
    </ReferenceNamesProvider>
  );
  try {
    const reference = view.container.querySelector(`a[data-row-reference="${CHILD}"]`);
    expect(reference?.textContent).toBe("CORE-17 Child 17: the filter pages");
  } finally {
    view.unmount();
  }
});

test("with no name held, a row's reference the page asked for reads as its author wrote it until the page's read answers", () => {
  summaryHeld();
  const view = drawn(<ReferenceNamesProvider>{rowAsked()}</ReferenceNamesProvider>);
  try {
    expect(view.container.textContent).toContain(ROW_WORDS);
    expect(view.container.textContent).not.toContain("Child 17");
  } finally {
    view.unmount();
  }
});
