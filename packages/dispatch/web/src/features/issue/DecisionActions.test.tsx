import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { Artifact, Decision, IssueDecisions } from "../../api/types";
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

function renderActions(): { unmount: () => void } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["decisions", KEY], read);
  const decisions = spyOn(api, "getIssueDecisions").mockResolvedValue(read);
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
