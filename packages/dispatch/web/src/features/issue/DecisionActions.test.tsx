import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { displayTree } from "../../__tests__/display-trees";
import { api } from "../../api/client";
import type { Artifact, Decision, IssueDecisions } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { DecisionActions } from "./DecisionActions";
import { CONTROL_LABELS } from "./page-words";

/**
 * The two controls a decision's node draws (dispatch://INFRA-1196 R7). The tree is one tab stop,
 * so neither takes the tab order and the row's own keys press them (`useTreeKeymap.ts`); what
 * this file reads is what the controls themselves do, which is what both a pointer and those
 * keys get.
 */

const KEY = "CORE-1";
const REFERENCE = `dispatch://${KEY}/artifact/decisions-md#b-d1`;

const document_: Artifact = {
  created_at: "2026-11-05T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-decisions",
  issue_key: KEY,
  kind: "doc",
  name: "decisions.md",
  primary: false,
  project: "CORE",
  ref_key: "artifact:artifact-decisions",
  session_id: null,
  slug: "decisions-md",
  versions: [],
};

const decision: Decision = {
  asks: [],
  block_id: "d1",
  changed_at: null,
  changed_version: 1,
  heading: "One tree, five presets",
  kept: null,
  kind: "agent",
  links: [],
  ref: REFERENCE,
  replaced: false,
  // The first paragraph below the heading is what a change is anchored to, so a decision with
  // one is a decision whose Change control is drawn.
  text: "One tree, five presets\n\nThe presets are queries over the one tree.",
  text_sha256: "sum",
  unsure: false,
  waiting_review: true,
};

const read: IssueDecisions = {
  decisions: [decision],
  document: document_.id,
  open_questions: [],
};

function renderActions(answer: IssueDecisions = read): { unmount: () => void } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["decisions", KEY], answer);
  const decisions = spyOn(api, "getIssueDecisions").mockResolvedValue(answer);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const view = render(
    <DecisionActions document={document_} isClosed={false} nodeReference={REFERENCE} />,
    { wrapper }
  );
  return {
    unmount: () => {
      view.unmount();
      decisions.mockRestore();
    },
  };
}

// R7, WCAG 2.1.1: a reader who asked to change a decision is there to type. The field takes the
// caret as it mounts, so the caret is in it on the commit that draws the form - no frame of its
// own, and no second form on the page to find it among - whether the Change was a pointer's or
// the row's own key (`useTreeKeymap.ts`'s `tree-change`, which presses this very control).
test("the change form opens with the caret in its field", async () => {
  const user = userEvent.setup();
  const { unmount } = renderActions();
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    const field = screen.getByLabelText(CONTROL_LABELS.your_change);
    expect(field.tagName).toBe("TEXTAREA");
    expect(
      document.activeElement,
      "the form opened with the caret somewhere other than the field a reader opened it to type in"
    ).toBe(field);
  } finally {
    unmount();
  }
});

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The quote the Change form offers for the one decision `fields` describe, which waits for
 *  review so its node draws the Change. */
async function quoteOf(fields: Pick<Decision, "text" | "text_tree">): Promise<string> {
  const user = userEvent.setup();
  const { unmount } = renderActions({ ...read, decisions: [{ ...decision, ...fields }] });
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    return screen.getByTestId("decision-quote").textContent ?? "";
  } finally {
    unmount();
  }
}

// A Change anchors on the words of the decision's first block, which the server finds by
// matching them against the document's own flattened text: that text writes one space wherever
// two text runs are not adjacent in the document's positions (`pmdoc.buildFlattenedText`). The
// end of one list item and the start of the next are four positions apart, so the quote the
// form sends must hold that space too, or `pmdoc.FindQuote` finds nothing to anchor on. The
// words come from the reading the server sent (`Decision.text_tree`), which the test builds
// with the page's own engine (`__tests__/display-trees`).
test("the quote of a first block that is a list holds one space between its items", async () => {
  const text = "## Name them notices\n\n- **alpha**\n- beta\n\nThat is the whole of it.";
  expect(await quoteOf({ text, text_tree: displayTree(text) })).toBe("alpha beta");
});

// The same rule at a hard break: the break is a leaf of its own and takes a position, so the
// two lines it divides are one space apart in the text the server matches a quote against.
test("the quote of a first block with a hard break holds one space between its lines", async () => {
  const text = "## Wrap the lines\n\nline one  \nline two\n\nThat is the whole of it.";
  expect(await quoteOf({ text, text_tree: displayTree(text) })).toBe("line one line two");
});

// An answer with no reading of its text comes from a pod of the build before during a rolling
// deploy, and the Change still has a quote there: the first paragraph's own markdown, which is
// all the browser has.
test("a decision whose answer carried no reading quotes its first paragraph's markdown", async () => {
  const text = "## Keep a log\n\nA log is enough.\n\nUnsure: the index may be slow.";
  expect(await quoteOf({ text, text_tree: undefined })).toBe("A log is enough.");
});
