import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { AnswerAskInput, Artifact, AskRead, IssueDetails } from "../../api/types";
import { AskCard } from "../inbox/AskCard";
import type { OwnedAsk } from "../inbox/ask-name";
import { DESKTOP_WIDTH, PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { ApprovalChip } from "./ApprovalChip";
import { ApprovalSheet } from "./ApprovalSheet";
import { changeRequest, ISSUE, noSourceRequest } from "./change-request-fixture";

// R22: "Before the person approves on a phone, Dispatch shows a sheet that restates what the
// approval covers." The sheet lists each added, changed and removed node with its text - a removed
// node with the approved text it removes, a changed one beside the approved text it replaces - and
// the digest the approval pins. On a phone, Approve opens it for an issue whose repository is a
// spec source, and the approval is sent only from it; an issue of no spec source, and every wider
// screen, approve as they always have.

let restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores) restore();
  restores = [];
});

function host(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>{node}</QueryClientProvider>
    </MemoryRouter>
  );
}

test("the sheet lists each added, changed and removed node with its text, and the digests the approval pins", async () => {
  const read = spyOn(api, "getChangeRequest").mockResolvedValue(changeRequest());
  restores.push(() => read.mockRestore());
  const view = host(
    <ApprovalSheet
      coversVerification
      issueKey={ISSUE}
      onApprove={() => {}}
      onClose={() => {}}
      version={3}
    />
  );
  try {
    const sheet = await screen.findByRole("dialog", { name: "What this approval covers" });
    await within(sheet).findByText("A refund returns the payment of an order.");
    expect(read).toHaveBeenCalledWith(ISSUE, 3);
    const rows = within(sheet).getAllByRole("listitem");
    expect(rows.map((row) => [row.dataset.nodeId, row.dataset.what])).toEqual([
      ["an-order-is-paid-once", "changed"],
      ["a-refund-returns-the-payment", "added"],
      ["a-bill-names-its-order", "removed"],
      ["a-second-payment-is-refused", "changed"],
    ]);
    const [changed, added, removed, check] = rows;
    // A changed node shows the text the approval writes and the approved text it replaces.
    expect(within(changed as HTMLElement).getByTestId("approval-sheet-text").textContent).toBe(
      "An order is paid once, and a second payment is refused."
    );
    expect(within(changed as HTMLElement).getByTestId("approval-sheet-approved").textContent).toBe(
      "An order is paid once."
    );
    expect(within(changed as HTMLElement).getByText("under billing-works")).toBeTruthy();
    expect(within(added as HTMLElement).getByTestId("approval-sheet-text").textContent).toBe(
      "A refund returns the payment of an order."
    );
    expect(within(added as HTMLElement).queryByTestId("approval-sheet-approved")).toBeNull();
    // A removed node shows the approved text the approval removes, not the issue's own words.
    expect(within(removed as HTMLElement).getByTestId("approval-sheet-text").textContent).toBe(
      "A bill names its order."
    );
    expect(within(removed as HTMLElement).queryByText("This statement goes.")).toBeNull();
    expect(within(check as HTMLElement).getByTestId("approval-sheet-text").textContent).toBe(
      "Claim: A second payment is refused at once.\n\nProcedure: The test pays one order two times."
    );
    expect(within(sheet).getByTestId("approval-sheet-digest").textContent).toBe("a".repeat(64));
    expect(within(sheet).getByTestId("approval-sheet-verification-digest").textContent).toBe(
      "b".repeat(64)
    );
  } finally {
    view.unmount();
  }
});

test("a sheet whose approval names the spec alone shows the spec's digest alone", async () => {
  const read = spyOn(api, "getChangeRequest").mockResolvedValue(changeRequest());
  restores.push(() => read.mockRestore());
  const view = host(
    <ApprovalSheet
      coversVerification={false}
      issueKey={ISSUE}
      onApprove={() => {}}
      onClose={() => {}}
      version={3}
    />
  );
  try {
    const sheet = await screen.findByRole("dialog", { name: "What this approval covers" });
    expect((await within(sheet).findByTestId("approval-sheet-digest")).textContent).toBe(
      "a".repeat(64)
    );
    expect(within(sheet).queryByTestId("approval-sheet-verification-digest")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("Approve approves and Cancel closes, each once", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "getChangeRequest").mockResolvedValue(changeRequest());
  restores.push(() => read.mockRestore());
  const presses: string[] = [];
  const view = host(
    <ApprovalSheet
      coversVerification
      issueKey={ISSUE}
      onApprove={() => presses.push("approve")}
      onClose={() => presses.push("close")}
      version={3}
    />
  );
  try {
    const sheet = await screen.findByRole("dialog", { name: "What this approval covers" });
    await within(sheet).findByTestId("approval-sheet-digest");
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    await user.click(within(sheet).getByRole("button", { name: "Approve" }));
    expect(presses).toEqual(["close", "approve"]);
  } finally {
    view.unmount();
  }
});

test("a sheet whose read fails says so with a Retry, and Approve sends nothing until the read is in", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "getChangeRequest")
    .mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "boom" }))
    .mockResolvedValue(changeRequest());
  restores.push(() => read.mockRestore());
  const presses: string[] = [];
  const view = host(
    <ApprovalSheet
      coversVerification
      issueKey={ISSUE}
      onApprove={() => presses.push("approve")}
      onClose={() => {}}
      version={3}
    />
  );
  try {
    const sheet = await screen.findByRole("dialog", { name: "What this approval covers" });
    const alert = await within(sheet).findByRole("alert");
    expect(alert.textContent).toContain("Could not read what this approval covers.");
    const approve = within(sheet).getByRole("button", { name: "Approve" });
    expect(approve).toHaveAttribute("disabled");
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    expect((await within(sheet).findByTestId("approval-sheet-digest")).textContent).toBe(
      "a".repeat(64)
    );
    await user.click(within(sheet).getByRole("button", { name: "Approve" }));
    expect(presses).toEqual(["approve"]);
  } finally {
    view.unmount();
  }
});

// An approval counts only for the texts its request names (R22), so the sheet restates the change
// request at the spec version and the Verification version the request names, never at another
// version: a Verification document that moved past the version its request names is said so, and
// Approve sends nothing, since Dispatch would refuse the approval.
test("a sheet whose Verification document moved past the version its request names says so, and Approve sends nothing", async () => {
  const read = spyOn(api, "getChangeRequest").mockResolvedValue(changeRequest());
  restores.push(() => read.mockRestore());
  const presses: string[] = [];
  const view = host(
    <ApprovalSheet
      coversVerification
      issueKey={ISSUE}
      onApprove={() => presses.push("approve")}
      onClose={() => {}}
      verificationVersion={1}
      version={3}
    />
  );
  try {
    const sheet = await screen.findByRole("dialog", { name: "What this approval covers" });
    const alert = await within(sheet).findByRole("alert");
    expect(alert.textContent).toContain("version 1 of the Verification document");
    expect(alert.textContent).toContain("now at version 2");
    expect(within(sheet).queryByTestId("approval-sheet-verification-digest")).toBeNull();
    expect(within(sheet).getByRole("button", { name: "Approve" })).toHaveAttribute("disabled");
    expect(presses).toEqual([]);
  } finally {
    view.unmount();
  }
});

/** The issue `ISSUE` as its read answers it, its spec the document `spec-artifact`. */
function issueRead(): IssueDetails {
  return { key: ISSUE, primary_artifact_id: "spec-artifact" } as IssueDetails;
}

/** An open approval request of version 3 of the issue's spec and version 2 of its Verification
 *  document, as the Inbox lists it. */
function approvalAsk(): OwnedAsk {
  return {
    anchor: null,
    answer: null,
    approval: {
      artifact_id: "spec-artifact",
      documents: [
        { artifact_id: "spec-artifact", name: "spec.md", requested_version: 3, version: 3 },
        {
          artifact_id: "verification-artifact",
          name: "verification.md",
          requested_version: 2,
          version: 2,
        },
      ],
      name: "spec.md",
      requested_version: 3,
      version: 3,
    },
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "session-1", kind: "session" },
    block_id: null,
    created_at: "2026-11-22T09:00:00Z",
    edited_at: null,
    id: "ask-approval",
    issue_key: ISSUE,
    kind: "approval",
    multiple: false,
    opened_event_id: 1,
    options: [{ label: "Approve" }, { label: "Request changes" }],
    question: "Approve spec.md (version 3)?",
    state: "open",
    urgency: "med",
    waiting_on: "human",
  } as OwnedAsk;
}

function thread(ask: OwnedAsk): () => Promise<AskRead> {
  return async () => ({
    answers: [],
    ask,
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
  });
}

/** Renders the approval request's Inbox card at `width`, reading `read` for its change request,
 *  and answers the sent answers. */
function renderCardAt(width: number, read: () => Promise<unknown>) {
  restores.push(stubViewport(width));
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issueRead());
  const getChangeRequest = spyOn(api, "getChangeRequest").mockImplementation(
    read as typeof api.getChangeRequest
  );
  restores.push(
    () => getIssue.mockRestore(),
    () => getChangeRequest.mockRestore()
  );
  const sent: AnswerAskInput[] = [];
  const ask = approvalAsk();
  const view = host(
    <AskCard
      answerAsk={async (_id, input) => {
        sent.push(input);
        return {
          ...ask,
          answer: {
            at: "2026-11-22T09:05:00Z",
            selected: input.selected ?? null,
            text: null,
            user: "alice",
          },
          state: "answered",
        };
      }}
      ask={ask}
      getAskThread={thread(ask)}
    />
  );
  return { getChangeRequest, getIssue, sent, view };
}

test("on a phone, the Inbox card's Approve of a spec-source issue opens the sheet, and only the sheet's Approve sends the answer", async () => {
  const user = userEvent.setup();
  const { getChangeRequest, sent, view } = renderCardAt(PHONE_WIDTH, async () => changeRequest());
  try {
    const card = view.getByTestId("ask-ask-approval");
    await user.click(await within(card).findByRole("radio", { name: "Approve" }));
    await user.click(within(card).getByRole("button", { name: "Answer" }));
    const sheet = await screen.findByRole("dialog", { name: "What this approval covers" });
    await within(sheet).findByTestId("approval-sheet-digest");
    expect(getChangeRequest).toHaveBeenCalledWith(ISSUE, 3);
    expect(sent).toEqual([]);
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog", { name: "What this approval covers" })).toBeNull();
    expect(sent).toEqual([]);

    await user.click(within(card).getByRole("button", { name: "Answer" }));
    const again = await screen.findByRole("dialog", { name: "What this approval covers" });
    await user.click(await within(again).findByRole("button", { name: "Approve" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]?.selected).toEqual(["Approve"]);
    expect(screen.queryByRole("dialog", { name: "What this approval covers" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("on a phone, the Inbox card's Approve of an issue of no spec source sends the answer with no sheet", async () => {
  const user = userEvent.setup();
  const { sent, view } = renderCardAt(PHONE_WIDTH, async () => noSourceRequest());
  try {
    const card = view.getByTestId("ask-ask-approval");
    await user.click(await within(card).findByRole("radio", { name: "Approve" }));
    await user.click(within(card).getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(screen.queryByRole("dialog", { name: "What this approval covers" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("on a wide screen, the Inbox card's Approve sends the answer with no sheet and reads no change request", async () => {
  const user = userEvent.setup();
  const { getChangeRequest, sent, view } = renderCardAt(DESKTOP_WIDTH, async () => changeRequest());
  try {
    const card = view.getByTestId("ask-ask-approval");
    await user.click(await within(card).findByRole("radio", { name: "Approve" }));
    await user.click(within(card).getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(screen.queryByRole("dialog", { name: "What this approval covers" })).toBeNull();
    expect(getChangeRequest).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});

test("on a phone, a card whose issue cannot be read opens the sheet with a Retry, and reads no other document's version", async () => {
  const user = userEvent.setup();
  const { getChangeRequest, getIssue, sent, view } = renderCardAt(PHONE_WIDTH, async () =>
    changeRequest()
  );
  getIssue.mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "boom" }));
  try {
    const card = view.getByTestId("ask-ask-approval");
    await user.click(await within(card).findByRole("radio", { name: "Approve" }));
    await user.click(within(card).getByRole("button", { name: "Answer" }));
    const sheet = await screen.findByRole("dialog", { name: "What this approval covers" });
    const alert = await within(sheet).findByRole("alert");
    expect(alert.textContent).toContain("Could not read what this approval covers.");
    expect(getChangeRequest).not.toHaveBeenCalled();
    expect(within(sheet).getByRole("button", { name: "Approve" })).toHaveAttribute("disabled");
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    const again = await screen.findByRole("dialog", { name: "What this approval covers" });
    await within(again).findByTestId("approval-sheet-digest");
    expect(getChangeRequest).toHaveBeenCalled();
    for (const call of getChangeRequest.mock.calls) expect(call).toEqual([ISSUE, 3]);
    expect(sent).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("on a phone, Request changes sends with no sheet: only an approval writes the approved spec", async () => {
  const user = userEvent.setup();
  const { sent, view } = renderCardAt(PHONE_WIDTH, async () => changeRequest());
  try {
    const card = view.getByTestId("ask-ask-approval");
    await user.click(await within(card).findByRole("radio", { name: "Request changes" }));
    await user.click(within(card).getByLabelText("Reason (required)"));
    await user.paste("Name the refund's limit.");
    await user.click(within(card).getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]?.selected).toEqual(["Request changes"]);
    expect(screen.queryByRole("dialog", { name: "What this approval covers" })).toBeNull();
  } finally {
    view.unmount();
  }
});

/** The issue's spec, awaiting the approval request above. */
function specArtifact(): Artifact {
  return {
    approval: {
      documents: approvalAsk().approval?.documents ?? [],
      latest_version: 3,
      state: "awaiting",
    },
    created_at: "2026-11-22T08:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "spec-artifact",
    issue_key: ISSUE,
    kind: "doc",
    name: "spec.md",
    primary: true,
    project: "CORE",
    ref_key: `${ISSUE}/spec`,
    session_id: null,
    slug: "spec-md",
    versions: [],
  };
}

test("on a phone, the document header's Approve of a spec-source issue opens the sheet, and only the sheet's Approve records the review", async () => {
  const user = userEvent.setup();
  restores.push(stubViewport(PHONE_WIDTH));
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issueRead());
  const getChangeRequest = spyOn(api, "getChangeRequest").mockResolvedValue(changeRequest());
  const createArtifactReview = spyOn(api, "createArtifactReview").mockResolvedValue({
    actor: { id: "alice", kind: "user" },
    artifact_id: "spec-artifact",
    ask_id: "ask-approval",
    content_sha256: "a".repeat(64),
    content_sha256_source: "insert",
    created_at: "2026-11-22T09:05:00Z",
    id: "review-1",
    kind: "person",
    reason: null,
    state: "approved",
    version: 3,
  });
  restores.push(
    () => getIssue.mockRestore(),
    () => getChangeRequest.mockRestore(),
    () => createArtifactReview.mockRestore()
  );
  const view = host(<ApprovalChip artifact={specArtifact()} variant="header" />);
  try {
    await user.click(screen.getByRole("button", { name: "Approve" }));
    const sheet = await screen.findByRole("dialog", { name: "What this approval covers" });
    await within(sheet).findByTestId("approval-sheet-digest");
    expect(createArtifactReview).not.toHaveBeenCalled();
    await user.click(within(sheet).getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(1));
  } finally {
    view.unmount();
  }
});
