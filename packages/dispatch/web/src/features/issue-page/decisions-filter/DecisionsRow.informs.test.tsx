import { beforeAll, expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../../__tests__/display-trees";
import { issueCoverageQuery } from "../../../api/queries";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { NOTHING_PUBLISHED, type PublishedReferences } from "../../refs/reference-names";
import { artifactsBySlug } from "../issue-artifacts";
import { type TreeNode, targetNaming } from "../issue-tree";
import { NoTooltips } from "../NoTooltips";
import { NO_FAILED_READS } from "../outline-reads";
import { EMPTY_STATE_LINES } from "../page-words";
import { presetView } from "../presets";
import { blockReads, Publisher } from "../projection-fixture";
import { AppProviders } from "../slots/stage-fixtures";
import { coverageRead } from "../spec-filter/spec-fixtures";
import { DecisionsFilterBody } from "./DecisionsFilterBody";
import {
  decision,
  decisionNode,
  decisionsRead,
  issueDetails,
  issueTree,
  KEY,
  READER,
  SPEC_DOCUMENT,
  shaped,
  specRef,
  statement,
} from "./decisions-fixtures";

/**
 * A row of the Decisions view and its "informs" line (`DecisionsRow.tsx`), the moves the CI legs
 * read at `da175e7a`: on a load of the Decisions view the rows of the two decisions that shape the
 * spec's statements each grew 20 px, one line, and the second moved down 20 px, in a frame no
 * read had answered since the frame before, at 1440 and at 390 px. The line that grows is the
 * "informs" line, which names each statement the decision shaped by the line the page's names
 * give it (`KnownReferences.lines`): the row drew it only once the names had come, a commit after
 * the rows. The row holds that line's place from its first drawing while a statement it shaped is
 * still to be named, and names the statements in that same line.
 *
 * At the head after that, the row of `spec#b-d1` shrank from 212 to 206 px at 1440 px as its
 * statement's name came, in each of five loads (Job `e2e-rows-hg9qc`): the probe of Job
 * `e2e-rows-dlw7l` read the informs line go from 26 to 20 px in the frame its name was drawn,
 * and its word "informs" move 6 px up. The line set its word and its statements' lines by their
 * baselines, and a line still to be named holds no text to give one: its baseline was taken at
 * its foot, 20 px down, 6 px under where a named line's text has its own, and the word was set
 * that much lower, which made the line 6 px taller until the name came.
 *
 * A unit test's DOM lays nothing out, so the place is read by the element that holds it and its
 * classes, and the names by the element they are drawn in.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The issue's tree: the spec's statement, and the decision that shaped it, which waits for the
 *  reader's review, so the view draws it in its first part. */
const ROOT: TreeNode = issueTree([statement("s1"), decisionNode("first")]);

/** What the page publishes of its names read once it has answered, for the statement: named by
 *  the reading of its words the read sends for a block, as the page names it (`targetNaming`). */
function namesAnswered(): PublishedReferences {
  const named = [statement("s1")].map((node) => ({ ...node, tree: displayTree(node.text) }));
  return { ...targetNaming(blockReads(issueDetails(), named)), asked: new Set([specRef("s1")]) };
}

/** What the page publishes while that read is out: the statement asked for, and no name yet. */
const NAMES_OUT: PublishedReferences = { ...NOTHING_PUBLISHED, asked: new Set([specRef("s1")]) };

/** The Decisions view's body over `ROOT`, with `published` published to the names store and
 *  handed to the body, as the view's slot does. */
function body(client: QueryClient, published: PublishedReferences) {
  const issue = issueDetails();
  return (
    <MemoryRouter>
      <AppProviders client={client}>
        <Publisher published={published} />
        <NoTooltips>
          <DecisionsFilterBody
            artifacts={artifactsBySlug(issue)}
            emptyLine={EMPTY_STATE_LINES.decisions}
            holdsEscape={false}
            isClosed={false}
            issue={issue}
            namesLine={NO_FAILED_READS}
            published={published}
            reader={READER}
            root={ROOT}
            view={presetView(ROOT, "decisions", "", new Set()).root}
          />
        </NoTooltips>
      </AppProviders>
    </MemoryRouter>
  );
}

/** A client that holds every read the view draws its entries from, answered: the decisions read
 *  with the decision that shaped the statement, and no ask, comment, block or covered statement. */
function answeredReads(): QueryClient {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(
    ["decisions", KEY],
    decisionsRead([decision("first", { links: [shaped(specRef("s1"))] })])
  );
  client.setQueryData(["asks", KEY, "thread"], []);
  client.setQueryData(["comments", KEY], []);
  // The spec's blocks, the one document of the issue its decisions stand in, which the view
  // reads (`decisions-entries.ts`): none, so the row makes no request for them.
  client.setQueryData(["artifact", SPEC_DOCUMENT.id, "blocks"], []);
  // The coverage read, which part T's Decisions view waits on before it draws its entries
  // (`decisions-entries.ts`'s `entriesHeld`): one that holds no statement.
  client.setQueryData(issueCoverageQuery(KEY).queryKey, coverageRead([]));
  return client;
}

test("a decision's row holds its informs line while the statements it shaped are still to be named", async () => {
  const client = answeredReads();
  const view = render(body(client, NAMES_OUT));
  try {
    const row = view.container.querySelector<HTMLElement>(
      `[data-node-row][data-node-id="${decisionNode("first").id}"]`
    );
    expect(row, "the view drew no row of the decision").not.toBeNull();
    const held = row?.querySelector("[data-decision-informs]") ?? null;
    expect(
      held,
      "the row held no place for its informs line while the statement was still to be named"
    ).not.toBeNull();
    // One line, 20 px, for the statement from the first drawing, with no name in it yet
    // (`DecisionsRow.tsx`, the informs line).
    const line = held?.querySelector("[data-decision-statement]") ?? null;
    expect(line, "the held line holds no line for the statement").not.toBeNull();
    expect(line?.className, "the statement's line holds no line's height").toContain("h-5");
    expect(line?.textContent, "the held line names the statement before its name came").toBe("");

    await act(async () => view.rerender(body(client, namesAnswered())));
    const informs = row?.querySelector("[data-decision-informs]") ?? null;
    expect(informs, "the names drew the informs line again as another element").toBe(held);
    expect(
      informs?.querySelector("[data-decision-statement]"),
      "the names drew the statement's line again as another element"
    ).toBe(line);
    expect(
      line?.textContent,
      "the informs line does not name the statement once the names came"
    ).toBe("The page draws s1.");
  } finally {
    view.unmount();
  }
});

test("a decision's informs line sets its word and its statements' lines by their tops, so a line still to be named is as tall as a named one", async () => {
  const client = answeredReads();
  const view = render(body(client, NAMES_OUT));
  try {
    const informs = view.container.querySelector<HTMLElement>(
      `[data-node-row][data-node-id="${decisionNode("first").id}"] [data-decision-informs]`
    );
    expect(informs, "the row held no place for its informs line").not.toBeNull();
    // The classes that set a part of the line by its baseline, on the line or inside it, which a
    // line still to be named holds none of.
    const alignments = (): string[] =>
      [informs, ...(informs?.querySelectorAll("*") ?? [])].flatMap((element) =>
        (element?.getAttribute("class") ?? "")
          .split(/\s+/)
          .filter((name) => /^(items|self)-baseline/.test(name))
      );
    expect(
      alignments(),
      "the informs line sets its parts by their baselines while its statement is still to be named"
    ).toEqual([]);
    expect(informs?.className, "the informs line sets its parts by no top").toContain(
      "items-start"
    );

    await act(async () => view.rerender(body(client, namesAnswered())));
    expect(
      informs?.textContent,
      "the informs line does not name the statement once the names came"
    ).toContain("The page draws s1.");
    expect(
      alignments(),
      "the informs line sets its parts by their baselines once its statement is named"
    ).toEqual([]);
    expect(informs?.className, "the names changed how the informs line sets its parts").toContain(
      "items-start"
    );
  } finally {
    view.unmount();
  }
});
