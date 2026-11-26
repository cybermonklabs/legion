import { beforeAll, expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../../__tests__/display-trees";
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
import { DecisionsFilterBody } from "./DecisionsFilterBody";
import {
  decision,
  decisionNode,
  decisionsRead,
  issueDetails,
  issueTree,
  KEY,
  READER,
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

test("a decision's row holds its informs line while the statements it shaped are still to be named", async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(
    ["decisions", KEY],
    decisionsRead([decision("first", { links: [shaped(specRef("s1"))] })])
  );
  client.setQueryData(["asks", KEY, "thread"], []);
  client.setQueryData(["comments", KEY], []);
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
    expect(held?.className, "the held line holds no line's height").toContain("min-h-5");
    expect(held?.getAttribute("aria-hidden"), "the held line is heard").toBe("true");
    expect(
      held?.querySelector("[data-decision-statement]"),
      "the held line names a statement"
    ).toBeNull();

    await act(async () => view.rerender(body(client, namesAnswered())));
    const informs = row?.querySelector("[data-decision-informs]") ?? null;
    expect(informs, "the names drew the informs line again as another element").toBe(held);
    expect(
      informs?.querySelector("[data-decision-statement]")?.textContent,
      "the informs line does not name the statement once the names came"
    ).toBe("The page draws s1.");
    expect(informs?.getAttribute("aria-hidden"), "the named line is hidden").toBeNull();
  } finally {
    view.unmount();
  }
});
