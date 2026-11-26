import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { onlineManager, QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";
import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { heldAnswers } from "../../__tests__/held-answers";
import { heldNotDisabled, pressHeld } from "../../__tests__/held-press";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError, REQUEST_DEADLINE_MS } from "../../api/client";
import { DispatchQueryClient } from "../../api/query-client";
import type { Artifact, ArtifactApproval, ArtifactReview } from "../../api/types";
import { ArtifactHeader } from "../artifacts/ArtifactHeader";
import { emulateChromiumFocusFixup } from "../issue-page/chromium-focus-fixup";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { buildIssuePath, parseIssuePath } from "../refs/routes";
import { SEND_DEADLINE_MS, stoppedWriteNote } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import { ApprovalChip } from "./ApprovalChip";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

function artifact(approval: ArtifactApproval | undefined): Artifact {
  return {
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-1",
    issue_key: "CORE-1",
    kind: "doc",
    name: "Design notes",
    primary: false,
    project: "CORE",
    ref_key: "CORE-1/design-notes",
    session_id: null,
    slug: "design-notes",
    versions: [],
    ...(approval === undefined ? {} : { approval }),
  };
}

function renderChip(
  target: Artifact,
  variant?: "header" | "list" | "pane",
  onReviewed?: () => void
) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <ApprovalChip artifact={target} onReviewed={onReviewed} variant={variant} />
    </QueryClientProvider>
  );
}

test("renders nothing when the artifact carries no approval", () => {
  const view = renderChip(artifact(undefined), "header");
  try {
    expect(view.container.firstChild).toBeNull();
  } finally {
    view.unmount();
  }
});

test("renders nothing for a draft document in every variant", () => {
  for (const variant of ["list", "header"] as const) {
    const view = renderChip(artifact({ latest_version: 1, state: "draft" }), variant);
    try {
      expect(view.container.firstChild).toBeNull();
    } finally {
      view.unmount();
    }
  }
});

test("renders each approval state's label", () => {
  const cases: Array<[ArtifactApproval, string]> = [
    [{ latest_version: 3, state: "awaiting" }, "Awaiting approval"],
    [{ latest_version: 3, state: "approved", version: 3 }, "Approved v3"],
    [{ latest_version: 3, state: "changes_requested", version: 2 }, "Changes requested"],
    [
      // One request covering a spec and its issue's Verification document names both.
      {
        documents: [
          { artifact_id: "artifact-1", name: "spec.md", requested_version: 3, version: 3 },
          { artifact_id: "artifact-2", name: "verification.md", requested_version: 2, version: 2 },
        ],
        latest_version: 3,
        state: "awaiting",
      },
      "Awaiting approval of spec.md v3 and verification.md v2",
    ],
    // A plan is approved by its plan review, never by a person.
    [
      { kind: "plan_review", latest_version: 3, state: "approved", version: 3 },
      "Approved by plan review v3",
    ],
    [
      { kind: "plan_review", latest_version: 4, state: "stale", version: 3 },
      "Approved by plan review v3 · changed since",
    ],
  ];
  for (const [approval, label] of cases) {
    const view = renderChip(artifact(approval), "list");
    try {
      expect(screen.getByRole("button", { name: label })).not.toBeNull();
    } finally {
      view.unmount();
    }
  }
});

test("a stale approval's label and title carry both the approved and latest version", () => {
  const view = renderChip(artifact({ latest_version: 5, state: "stale", version: 2 }), "list");
  try {
    const chip = screen.getByRole("button", { name: "Approved v2 · changed since" });
    expect(chip.title).toContain("version 2");
    expect(chip.title).toContain("is 5");
  } finally {
    view.unmount();
  }
});

// The longest label the chip draws is wider than a phone, and the pill answers that by wrapping
// rather than by hiding any of it: the button carries the whole label as its own text, and no
// `title` stands in for words a reader would otherwise not see (a tooltip is no reading on a
// phone, as the step reason line below the list says too).
test("the longest awaiting label is the chip's whole text, with no title standing in for it", () => {
  const view = renderChip(
    artifact({
      documents: [
        { artifact_id: "artifact-1", name: "spec.md", requested_version: 1, version: 1 },
        { artifact_id: "artifact-2", name: "verification.md", requested_version: 1, version: 1 },
      ],
      latest_version: 1,
      state: "awaiting",
    }),
    "header"
  );
  try {
    const label = "Awaiting approval of spec.md v1 and verification.md v1";
    const chip = screen.getByRole("button", { name: label });
    expect(chip.textContent).toBe(label);
    expect(chip.title).toBe("");
  } finally {
    view.unmount();
  }
});

test("the header hides Approve for a current approval but offers Request changes", () => {
  const view = renderChip(artifact({ latest_version: 3, state: "approved", version: 3 }), "header");
  try {
    expect(screen.queryByRole("button", { name: /^Approve( v\d+)?$/ })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Request changes" })).not.toBeNull();
  } finally {
    view.unmount();
  }
});

test("the header's Approve reads the latest version for a stale approval", () => {
  const view = renderChip(artifact({ latest_version: 4, state: "stale", version: 2 }), "header");
  try {
    expect(screen.getByRole("button", { name: "Approve v4" })).not.toBeNull();
  } finally {
    view.unmount();
  }
});

test("an unread document draws nothing but in the reading pane's head, which holds its two actions", () => {
  for (const variant of ["list", "header"] as const) {
    const view = render(
      <QueryClientProvider client={new QueryClient()}>
        <ApprovalChip artifact={undefined} variant={variant} />
      </QueryClientProvider>
    );
    try {
      expect(view.container.firstChild).toBeNull();
    } finally {
      view.unmount();
    }
  }
  const view = render(
    <QueryClientProvider client={new QueryClient()}>
      <ApprovalChip
        artifact={undefined}
        shortcutKeys={{ approve: ["a"], requestChanges: ["Shift+R"], showReviews: [] }}
        variant="pane"
      />
    </QueryClientProvider>
  );
  try {
    // Both drawn and held, each in its box with its key, and no state to show yet.
    const buttons = screen.getAllByRole("button");
    expect(buttons.map((button) => button.getAttribute("data-approval-action"))).toEqual([
      "approve",
      "request-changes",
    ]);
    for (const button of buttons) {
      expect(button).toHaveAttribute("aria-disabled", "true");
      expect(button.parentElement?.tagName).toBe("SPAN");
    }
    expect(screen.getByRole("button", { name: "Approve" })).toHaveAttribute(
      "aria-keyshortcuts",
      "A"
    );
    expect(screen.getByRole("button", { name: "Request changes" })).toHaveAttribute(
      "aria-keyshortcuts",
      "Shift+R"
    );
  } finally {
    view.unmount();
  }
});

test("the reading pane's Approve keeps one label for a stale approval", () => {
  const view = renderChip(artifact({ latest_version: 4, state: "stale", version: 2 }), "pane");
  try {
    expect(screen.getByRole("button", { name: "Approve" })).not.toHaveAttribute("aria-disabled");
    expect(screen.queryByRole("button", { name: "Approve v4" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// The keys find the two actions and the badge by their marks: the Inbox's `a` and Shift+R press
// Approve and Request changes, and its command bar's Show the reviews presses the badge by the
// mark the Inbox gives it (`stateMarks`), which no other host's badge carries.
test("the header's actions and its badge carry the marks the keys press them by", () => {
  const awaiting = artifact({ latest_version: 3, state: "awaiting" });
  const view = render(
    <QueryClientProvider client={new QueryClient()}>
      <ApprovalChip
        artifact={awaiting}
        stateMarks={{ "data-inbox-action": "show-reviews" }}
        variant="header"
      />
    </QueryClientProvider>
  );
  try {
    expect(screen.getByRole("button", { name: "Approve" })).toHaveAttribute(
      "data-approval-action",
      "approve"
    );
    expect(screen.getByRole("button", { name: "Request changes" })).toHaveAttribute(
      "data-approval-action",
      "request-changes"
    );
    expect(screen.getByRole("button", { name: "Awaiting approval" })).toHaveAttribute(
      "data-inbox-action",
      "show-reviews"
    );
  } finally {
    view.unmount();
  }
  const unmarked = renderChip(awaiting, "header");
  try {
    expect(screen.getByRole("button", { name: "Awaiting approval" })).not.toHaveAttribute(
      "data-inbox-action"
    );
  } finally {
    unmarked.unmount();
  }
});

test("clicking Approve posts an approved review of the version the page read", async () => {
  const user = userEvent.setup();
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockResolvedValue({
    actor: { id: "alice", kind: "user" },
    artifact_id: "artifact-1",
    ask_id: null,
    content_sha256: null,
    content_sha256_source: null,
    created_at: "2026-09-11T00:00:00Z",
    id: "review-1",
    kind: "person",
    reason: null,
    state: "approved",
    version: 3,
  });
  const view = renderChip(artifact({ latest_version: 3, state: "awaiting" }), "header");
  try {
    await user.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(createArtifactVersionReview).toHaveBeenCalledTimes(1));
    expect(createArtifactVersionReview).toHaveBeenCalledWith(
      "artifact-1",
      3,
      { state: "approved" },
      keyedAttempt
    );
  } finally {
    createArtifactVersionReview.mockRestore();
    view.unmount();
  }
});

// A stale approval's Approve names the latest version, and that is the version it sends.
test("a stale approval's Approve posts its review of the latest version", async () => {
  const user = userEvent.setup();
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockResolvedValue({
    actor: { id: "alice", kind: "user" },
    artifact_id: "artifact-1",
    ask_id: null,
    content_sha256: null,
    content_sha256_source: null,
    created_at: "2026-09-11T00:00:00Z",
    id: "review-1",
    kind: "person",
    reason: null,
    state: "approved",
    version: 4,
  });
  const view = renderChip(artifact({ latest_version: 4, state: "stale", version: 2 }), "header");
  try {
    await user.click(screen.getByRole("button", { name: "Approve v4" }));
    await waitFor(() => expect(createArtifactVersionReview).toHaveBeenCalledTimes(1));
    expect(createArtifactVersionReview).toHaveBeenCalledWith(
      "artifact-1",
      4,
      { state: "approved" },
      keyedAttempt
    );
  } finally {
    createArtifactVersionReview.mockRestore();
    view.unmount();
  }
});

// The document moved after the page read it: Dispatch writes nothing and says which version a
// review would now record. The chip shows those words, and the page reads the document again,
// so the next press reviews what the reader then has in front of them.
test("an Approve whose version moved shows the server's refusal and reads the document again", async () => {
  const user = userEvent.setup();
  const refusal =
    "the review would record version 4 of Design notes, not version 3: read version 4 and review it";
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockRejectedValue(
    new ApiError(409, { code: "REVIEW_VERSION_MOVED", error: refusal })
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["artifact", "artifact-1"], artifact(undefined));
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ApprovalChip
        artifact={artifact({ latest_version: 3, state: "awaiting" })}
        variant="header"
      />
    </QueryClientProvider>
  );
  try {
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent(refusal));
    expect(queryClient.getQueryState(["artifact", "artifact-1"])?.isInvalidated).toBe(true);
    expect(createArtifactVersionReview).toHaveBeenCalledTimes(1);
  } finally {
    createArtifactVersionReview.mockRestore();
    view.unmount();
  }
});

/** An approved review of `version` of the document, as Dispatch answers one it recorded. */
function approvedReview(version: number): ArtifactReview {
  return {
    actor: { id: "alice", kind: "user" },
    artifact_id: "artifact-1",
    ask_id: null,
    content_sha256: null,
    content_sha256_source: null,
    created_at: "2026-09-11T00:00:00Z",
    id: "review-1",
    kind: "person",
    reason: null,
    state: "approved",
    version,
  };
}

/** The chip as both issue pages' header draws it: from the issue's own read, which carries the
 *  issue's documents (`primarySpec`), never from the document's read. Each read of the issue is
 *  answered by `read`. */
function IssueReadChip({
  read,
}: {
  read: () => Promise<{ readonly artifacts: readonly Artifact[] }>;
}): ReactNode {
  const issue = useQuery({ queryFn: read, queryKey: ["issue", "CORE-1"] });
  const spec = issue.data?.artifacts[0];
  return spec === undefined ? null : <ApprovalChip artifact={spec} variant="header" />;
}

/** A client whose reads and writes are not tried again. */
function chipClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
}

// On an issue page the header draws the chip from the issue's read, which a 409 reads again too:
// the next press reviews the version the page then draws, rather than sending the version the
// server refused again and again until something else reads the issue.
test("on an issue page, an Approve whose version moved reads the issue again, and the next press reviews the version it then draws", async () => {
  const user = userEvent.setup();
  const refusal =
    "the review would record version 4 of Design notes, not version 3: read version 4 and review it";
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview")
    .mockRejectedValueOnce(new ApiError(409, { code: "REVIEW_VERSION_MOVED", error: refusal }))
    .mockResolvedValue(approvedReview(4));
  // The version the issue's read answers; the document moves on to 4 once the page has read 3.
  let latest = 3;
  let reads = 0;
  const view = render(
    <QueryClientProvider client={chipClient()}>
      <IssueReadChip
        read={async () => {
          reads += 1;
          return { artifacts: [artifact({ latest_version: latest, state: "awaiting" })] };
        }}
      />
    </QueryClientProvider>
  );
  try {
    const approve = await screen.findByRole("button", { name: "Approve" });
    latest = 4;
    await user.click(approve);
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent(refusal));
    await waitFor(() => expect(reads, "the refusal reads the issue again").toBe(2));
    await user.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(createArtifactVersionReview).toHaveBeenCalledTimes(2));
    expect(createArtifactVersionReview.mock.calls.map(([, version]) => version)).toEqual([3, 4]);
  } finally {
    createArtifactVersionReview.mockRestore();
    view.unmount();
  }
});

// Approve stays held until the reads its review made stale are in, outside `NoTooltips` as
// inside: Dispatch records an approval at the latest version for a review with no request open,
// so a second press - a second `a`, a held key's repeat, a double click - before the issue is read
// again would record a second approval.
test("a second press of Approve before the page reads its approval again sends no second review", async () => {
  const user = userEvent.setup();
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockResolvedValue(
    approvedReview(3)
  );
  const answered = artifact({ latest_version: 3, state: "approved", version: 3 });
  const readAgain = Promise.withResolvers<{ readonly artifacts: readonly Artifact[] }>();
  let reads = 0;
  const view = render(
    <QueryClientProvider client={chipClient()}>
      <IssueReadChip
        read={() => {
          reads += 1;
          return reads === 1
            ? Promise.resolve({ artifacts: [artifact({ latest_version: 3, state: "awaiting" })] })
            : readAgain.promise;
        }}
      />
    </QueryClientProvider>
  );
  try {
    const approve = await screen.findByRole("button", { name: "Approve" });
    await user.click(approve);
    await waitFor(() => expect(createArtifactVersionReview).toHaveBeenCalledTimes(1));
    // The review is recorded, and the issue is being read again.
    await waitFor(() => expect(reads).toBe(2));
    await user.click(approve);
    await user.click(approve);
    await act(settle);
    expect(createArtifactVersionReview).toHaveBeenCalledTimes(1);
    expect(approve).toHaveAttribute("aria-disabled", "true");

    // The read lands with the request answered, and the chip draws the approval in Approve's place.
    await act(async () => readAgain.resolve({ artifacts: [answered] }));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument()
    );
    expect(screen.getByRole("button", { name: "Approved v3" })).toBeInTheDocument();
    expect(createArtifactVersionReview).toHaveBeenCalledTimes(1);
  } finally {
    readAgain.resolve({ artifacts: [answered] });
    createArtifactVersionReview.mockRestore();
    view.unmount();
  }
});

test("a change request whose version moved shows the server's refusal in its box", async () => {
  const user = userEvent.setup();
  const refusal =
    "the review would record version 4 of Design notes, not version 3: read version 4 and review it";
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockRejectedValue(
    new ApiError(409, { code: "REVIEW_VERSION_MOVED", error: refusal })
  );
  const view = renderChip(artifact({ latest_version: 3, state: "awaiting" }), "header");
  try {
    await user.click(screen.getByRole("button", { name: "Request changes" }));
    const dialog = screen.getByRole("dialog", { name: "Request changes" });
    await user.click(within(dialog).getByLabelText("Reason"));
    await user.paste("Needs another pass");
    await user.click(within(dialog).getByRole("button", { name: "Request changes" }));
    await waitFor(() => expect(within(dialog).getByRole("alert")).toHaveTextContent(refusal));
    // One refusal, in the box the reader is in: the header shows none of its own beside it.
    expect(screen.getAllByRole("alert")).toHaveLength(1);
  } finally {
    createArtifactVersionReview.mockRestore();
    view.unmount();
  }
});

// While a review the reader sent is out, the controls that would send another are held
// (`held-press.ts`), never `disabled`, on every surface: a `disabled` control drops the focus the
// reader's press left on it to the page's body, and the Inbox's reading column then loses the
// place it hands that focus on from. Approve keeps the focus and sends nothing more, and Request
// changes is held beside it. In the Request changes dialog, the reason box, Cancel and the
// dialog's own Request changes are held while the change request is out.
test("while its review is out, Approve, Request changes and the change request dialog's controls are held, never disabled", async () => {
  const user = userEvent.setup();
  const written = Promise.withResolvers<ArtifactReview>();
  // The review goes to the version the page read (`createArtifactVersionReview`).
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockImplementation(
    () => written.promise
  );
  const approving = renderChip(artifact({ latest_version: 3, state: "awaiting" }), "header");
  try {
    const approve = screen.getByRole("button", { name: "Approve" }) as HTMLButtonElement;
    await pressHeld(user, approve, {
      name: "Approve",
      sent: () => createArtifactVersionReview.mock.calls.length,
    });
    const requestChanges = screen.getByRole("button", { name: "Request changes" });
    expect(heldNotDisabled(requestChanges as HTMLButtonElement, "Request changes")).toBe(true);
  } finally {
    approving.unmount();
  }
  const changing = renderChip(artifact({ latest_version: 3, state: "awaiting" }), "header");
  try {
    await user.click(screen.getByRole("button", { name: "Request changes" }));
    const dialog = within(screen.getByRole("dialog", { name: "Request changes" }));
    const box = dialog.getByLabelText("Reason") as HTMLTextAreaElement;
    await user.click(box);
    await user.paste("Needs another pass");
    await pressHeld(user, dialog.getByRole("button", { name: "Request changes" }), {
      name: "the dialog's Request changes",
      sent: () => createArtifactVersionReview.mock.calls.length - 1,
    });
    expect(heldNotDisabled(box, "the reason box")).toBe(true);
    const cancel = dialog.getByRole("button", { name: "Cancel" }) as HTMLButtonElement;
    expect(heldNotDisabled(cancel, "Cancel")).toBe(true);
  } finally {
    written.reject(new Error("the connection dropped"));
    createArtifactVersionReview.mockRestore();
    changing.unmount();
  }
});

// The reviewer's case, and why a press made anew never reaches a stopped press: Approve goes late
// and the reader stops it, a change request lands, and the reader approves again. The second
// Approve is a second action, not the first one made again - a press sent under the stopped
// press's key would be answered by Dispatch with what that press did, and the approval the person
// meant now would be recorded nowhere, with the chip saying it was made. So it goes out under a
// key of its own, though it carries what the stopped press carried.
test("approving again after a stopped approval and a change request goes under its own key", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockRejectedValue(
    new NetworkError(
      "/api/v1/artifacts/artifact-1/versions/3/reviews",
      new TypeError("Failed to fetch")
    )
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ApprovalChip
        artifact={artifact({ latest_version: 3, state: "awaiting" })}
        variant="header"
      />
    </QueryClientProvider>
  );

  try {
    await user.click(screen.getByRole("button", { name: "Approve" }));
    await stopTheLateWrite(queryClient);
    const stoppedKey = createArtifactVersionReview.mock.calls[0]?.[3]?.key;
    const stopped = createArtifactVersionReview.mock.calls.length;

    // The change request lands. Its answer is held, as the next Approve's is, and given inside
    // `act` after its press, as docs/testing.md ("The act rule of a Dispatch unit test") says.
    const review: ArtifactReview = {
      actor: { id: "alice", kind: "user" },
      artifact_id: "artifact-1",
      ask_id: null,
      content_sha256: null,
      content_sha256_source: null,
      created_at: "2026-09-11T00:00:00Z",
      id: "review-2",
      kind: "person",
      reason: "Needs another pass",
      state: "changes_requested",
      version: 3,
    };
    const changesAnswer = Promise.withResolvers<ArtifactReview>();
    createArtifactVersionReview.mockReturnValue(changesAnswer.promise);
    await user.click(screen.getByRole("button", { name: "Request changes" }));
    const dialog = screen.getByRole("dialog", { name: "Request changes" });
    await user.click(within(dialog).getByLabelText("Reason"));
    await user.paste("Needs another pass");
    await user.click(within(dialog).getByRole("button", { name: "Request changes" }));
    await act(async () => {
      changesAnswer.resolve(review);
    });
    await tick(1_000);
    expect(createArtifactVersionReview).toHaveBeenCalledTimes(stopped + 1);

    // Approve again: the same values the stopped press carried, and a press of its own.
    const approvalAnswer = Promise.withResolvers<ArtifactReview>();
    createArtifactVersionReview.mockReturnValue(approvalAnswer.promise);
    await user.click(screen.getByRole("button", { name: "Approve" }));
    await act(async () => {
      approvalAnswer.resolve(review);
    });
    await tick(1_000);

    expect(createArtifactVersionReview).toHaveBeenCalledTimes(stopped + 2);
    const again = createArtifactVersionReview.mock.calls.at(-1);
    expect(again?.[1]).toBe(3);
    expect(again?.[2]).toEqual({ state: "approved" });
    expect(again?.[3].key).not.toBe(stoppedKey);
  } finally {
    createArtifactVersionReview.mockRestore();
    view.unmount();
    realClock();
  }
});

// A change request the reader stopped may have been made: the dialog says so, and that Request
// changes is a press of its own, rather than a failure it cannot know of.
test("a change request the reader stopped says it may have been made, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockRejectedValue(
    new NetworkError(
      "/api/v1/artifacts/artifact-1/versions/3/reviews",
      new TypeError("Failed to fetch")
    )
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ApprovalChip
        artifact={artifact({ latest_version: 3, state: "awaiting" })}
        variant="header"
      />
    </QueryClientProvider>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Request changes" }));
    const dialog = screen.getByRole("dialog", { name: "Request changes" });
    await user.click(within(dialog).getByLabelText("Reason"));
    await user.paste("Needs another pass");
    await user.click(within(dialog).getByRole("button", { name: "Request changes" }));
    await stopTheLateWrite(queryClient);
    expect(within(dialog).getByRole("alert").textContent).toBe(
      stoppedWriteNote("your review", "made", { anew: "Request changes sends a new review." })
    );
  } finally {
    createArtifactVersionReview.mockRestore();
    view.unmount();
    queryClient.clear();
    realClock();
  }
});

// Offline, a review is tried under its key until Dispatch answers it: each try fails at the
// network, and the next waits out its backoff. At its deadline (`SEND_DEADLINE_MS`) the write goes
// late, still tried and now stoppable at the shell's indicator, and the chip's hold ends there, so
// a review that never answers holds neither action for good, as the Legion daemons form's hold
// ends (`LegionDaemonsSection`). Approve keeps the focus while it is held and once it is let go,
// under `emulateChromiumFocusFixup` throughout.
test("offline, Approve is held until its review's deadline, let go there, and keeps the focus throughout", async () => {
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockRejectedValue(
    new NetworkError(
      "/api/v1/artifacts/artifact-1/versions/3/reviews",
      new TypeError("Failed to fetch")
    )
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ApprovalChip
        artifact={artifact({ latest_version: 3, state: "awaiting" })}
        variant="header"
      />
    </QueryClientProvider>
  );
  const approve = screen.getByRole("button", { name: "Approve" }) as HTMLButtonElement;
  const online = onlineManager.isOnline();
  jest.useFakeTimers();
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    onlineManager.setOnline(false);
    await pressHeld(userOnFakeClock(), approve, {
      name: "Approve",
      sent: () => createArtifactVersionReview.mock.calls.length,
    });

    // A second short of the deadline the review is still within it, and Approve still held.
    await tick(SEND_DEADLINE_MS - 1_000);
    expect(writesOf(queryClient).rows()[0]?.status).toBe("sending");
    expect(heldNotDisabled(approve, "Approve")).toBe(true);
    expect(approve).toHaveFocus();

    // Past it the review is late and still tried, and Approve is let go with the focus on it.
    await tick(1_000);
    expect(writesOf(queryClient).rows()[0]?.status).toBe("late");
    expect(heldNotDisabled(approve, "Approve")).toBe(false);
    expect(approve).toHaveFocus();
  } finally {
    view.unmount();
    queryClient.clear();
    onlineManager.setOnline(online);
    stopFocusFixup();
    realClock();
    createArtifactVersionReview.mockRestore();
  }
});

// A review that lands just as the browser goes offline: the reads it made stale are asked again,
// and the network pauses them until it returns, with no deadline of their own running: the app's
// client holds a read of an idle query until its paused fetch answers (`DispatchQueryClient`). The
// chip holds its actions through those reads only up to the request deadline
// (`REQUEST_DEADLINE_MS`), and lets them go there, with the focus on the Approve the reader pressed.
test("offline, Approve is held through the reads its landed review made stale only until the request deadline", async () => {
  const answer = Promise.withResolvers<ArtifactReview>();
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockReturnValue(
    answer.promise
  );
  const queryClient = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <IssueReadChip
        read={() =>
          Promise.resolve({ artifacts: [artifact({ latest_version: 3, state: "awaiting" })] })
        }
      />
    </QueryClientProvider>
  );
  const approve = (await screen.findByRole("button", { name: "Approve" })) as HTMLButtonElement;
  const online = onlineManager.isOnline();
  jest.useFakeTimers();
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    await pressHeld(userOnFakeClock(), approve, {
      name: "Approve",
      sent: () => createArtifactVersionReview.mock.calls.length,
    });
    onlineManager.setOnline(false);
    await act(async () => answer.resolve(approvedReview(3)));

    // A second short of the request deadline the issue's read is still paused, and Approve held.
    await tick(REQUEST_DEADLINE_MS - 1_000);
    expect(heldNotDisabled(approve, "Approve")).toBe(true);
    expect(approve).toHaveFocus();

    // At it the chip lets Approve go, with the focus on it.
    await tick(1_000);
    expect(heldNotDisabled(approve, "Approve")).toBe(false);
    expect(approve).toHaveFocus();
  } finally {
    view.unmount();
    queryClient.clear();
    onlineManager.setOnline(online);
    stopFocusFixup();
    realClock();
    createArtifactVersionReview.mockRestore();
  }
});

// A review that lands while the reads it made stale go unanswered: the chip holds its actions
// through them up to the request deadline, lets them go there, with the focus on the Approve the
// reader pressed, and says why beside them, in the words of the error such a read ends with. The
// line goes once those reads settle.
test("a landed review whose reads have not answered by the request deadline lets Approve go there, and the chip says why until they settle", async () => {
  const answer = Promise.withResolvers<ArtifactReview>();
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockReturnValue(
    answer.promise
  );
  const readAgain = Promise.withResolvers<{ readonly artifacts: readonly Artifact[] }>();
  let reads = 0;
  const view = render(
    <QueryClientProvider client={chipClient()}>
      <IssueReadChip
        read={() => {
          reads += 1;
          return reads === 1
            ? Promise.resolve({ artifacts: [artifact({ latest_version: 3, state: "awaiting" })] })
            : readAgain.promise;
        }}
      />
    </QueryClientProvider>
  );
  const approve = (await screen.findByRole("button", { name: "Approve" })) as HTMLButtonElement;
  const why = "The new approval is not read yet. Dispatch did not answer within 30 seconds.";
  jest.useFakeTimers();
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    await pressHeld(userOnFakeClock(), approve, {
      name: "Approve",
      sent: () => createArtifactVersionReview.mock.calls.length,
    });
    await act(async () => answer.resolve(approvedReview(3)));
    await tick(0);
    expect(reads).toBe(2);

    // A second short of the request deadline the issue's read is still out, Approve held, and
    // nothing said.
    await tick(REQUEST_DEADLINE_MS - 1_000);
    expect(heldNotDisabled(approve, "Approve")).toBe(true);
    expect(screen.queryByText(why)).toBeNull();

    // At it the chip lets Approve go, with the focus on it, and says why.
    await tick(1_000);
    expect(heldNotDisabled(approve, "Approve")).toBe(false);
    expect(approve).toHaveFocus();
    expect(screen.getByRole("alert").textContent).toBe(why);

    // The read answers with the request still waiting, and the line goes.
    await act(async () =>
      readAgain.resolve({ artifacts: [artifact({ latest_version: 3, state: "awaiting" })] })
    );
    await tick(0);
    expect(screen.queryByText(why)).toBeNull();
    expect(screen.getByRole("button", { name: "Approve" })).toBe(approve);
    expect(createArtifactVersionReview).toHaveBeenCalledTimes(1);
  } finally {
    readAgain.resolve({ artifacts: [artifact({ latest_version: 3, state: "awaiting" })] });
    view.unmount();
    stopFocusFixup();
    realClock();
    createArtifactVersionReview.mockRestore();
  }
});

// The line a landed review's reads the bound ended draws is that review's own: the next review
// takes it away as it lands, though the earlier reads are still out, and that review's reads
// answering say nothing more. Dispatch holds the first review's read of the issue until the row
// answers it, so the next review's read, which waits for that one (`DispatchQueryClient`),
// answers after it.
test("the line a review's reads the bound ended draws goes as the next review lands, whose reads answer", async () => {
  // Each review's answer, held until the row gives it, so what it sets off runs in `act`.
  const { answers, next } = heldAnswers<ArtifactReview>();
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockImplementation(
    next
  );
  const waiting = { artifacts: [artifact({ latest_version: 3, state: "awaiting" })] };
  const readAgain = Promise.withResolvers<{ readonly artifacts: readonly Artifact[] }>();
  let reads = 0;
  const queryClient = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <IssueReadChip
        read={() => {
          reads += 1;
          return reads === 2 ? readAgain.promise : Promise.resolve(waiting);
        }}
      />
    </QueryClientProvider>
  );
  const approve = (await screen.findByRole("button", { name: "Approve" })) as HTMLButtonElement;
  const why = "The new approval is not read yet. Dispatch did not answer within 30 seconds.";
  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    await user.click(approve);
    await tick(0);
    await act(async () => answers[0]?.(approvedReview(3)));
    await tick(0);
    expect(reads).toBe(2);
    await tick(REQUEST_DEADLINE_MS);
    expect(heldNotDisabled(approve, "Approve")).toBe(false);
    expect(screen.getByRole("alert").textContent).toBe(why);

    await user.click(approve);
    await tick(0);
    expect(createArtifactVersionReview).toHaveBeenCalledTimes(2);
    await act(async () => answers[1]?.(approvedReview(3)));
    await tick(0);
    expect(heldNotDisabled(approve, "Approve")).toBe(true);
    expect(
      screen.queryByText(why),
      "the earlier review's reads kept their line past the next review"
    ).toBeNull();

    await act(async () => readAgain.resolve(waiting));
    await tick(0);
    expect(heldNotDisabled(approve, "Approve")).toBe(false);
    expect(screen.queryByRole("alert")).toBeNull();
  } finally {
    view.unmount();
    queryClient.clear();
    realClock();
    createArtifactVersionReview.mockRestore();
  }
});

test("Request changes requires a reason before it can submit, then posts it", async () => {
  const user = userEvent.setup();
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockResolvedValue({
    actor: { id: "alice", kind: "user" },
    artifact_id: "artifact-1",
    ask_id: null,
    content_sha256: null,
    content_sha256_source: null,
    created_at: "2026-09-11T00:00:00Z",
    id: "review-1",
    kind: "person",
    reason: "Needs another pass",
    state: "changes_requested",
    version: 3,
  });
  const view = renderChip(artifact({ latest_version: 3, state: "awaiting" }), "header");
  try {
    await user.click(screen.getByRole("button", { name: "Request changes" }));
    const dialog = screen.getByRole("dialog", { name: "Request changes" });
    const submit = within(dialog).getByRole("button", { name: "Request changes" });
    expect(submit.hasAttribute("disabled")).toBe(true);

    await user.click(within(dialog).getByLabelText("Reason"));
    await user.paste("Needs another pass");
    expect(submit).not.toHaveAttribute("disabled");

    await user.click(submit);
    await waitFor(() => expect(createArtifactVersionReview).toHaveBeenCalledTimes(1));
    expect(createArtifactVersionReview).toHaveBeenCalledWith(
      "artifact-1",
      3,
      {
        reason: "Needs another pass",
        state: "changes_requested",
      },
      keyedAttempt
    );
  } finally {
    createArtifactVersionReview.mockRestore();
    view.unmount();
  }
});

// A reason Dispatch refuses for its length is refused the same way every time, and the server's
// words name the block and the bound: the dialog shows them in place of wording of its own.
test("a refused reason shows the server's message", async () => {
  const user = userEvent.setup();
  const refusal =
    "reason block 1 (paragraph) is 1 characters over the 32000-character block limit (32001/32000)";
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockRejectedValue(
    new ApiError(400, { code: "CAP_EXCEEDED", error: refusal })
  );
  const view = renderChip(artifact({ latest_version: 3, state: "awaiting" }), "header");
  try {
    await user.click(screen.getByRole("button", { name: "Request changes" }));
    const dialog = screen.getByRole("dialog", { name: "Request changes" });
    await user.click(within(dialog).getByLabelText("Reason"));
    await user.paste("x".repeat(32_001));
    await user.click(within(dialog).getByRole("button", { name: "Request changes" }));

    await waitFor(() => expect(screen.getByRole("alert").textContent).toBe(refusal));
  } finally {
    createArtifactVersionReview.mockRestore();
    view.unmount();
  }
});

// Dispatch made the review and lost its answer (`409 KEY_COMMITTED`): a landing, not a refusal.
// The dialog closes with no refusal, the reviews are read again, and a second press is a fresh
// open of the dialog rather than a second review of the reason already sent.
test("a change request made with its answer lost closes the dialog, refuses nothing and reads again", async () => {
  const user = userEvent.setup();
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "made; the answer was lost" })
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  queryClient.setQueryData(["artifact-reviews", "artifact-1"], []);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ApprovalChip
        artifact={artifact({ latest_version: 3, state: "awaiting" })}
        variant="header"
      />
    </QueryClientProvider>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Request changes" }));
    const dialog = screen.getByRole("dialog", { name: "Request changes" });
    await user.click(within(dialog).getByLabelText("Reason"));
    await user.paste("Needs another pass");
    await user.click(within(dialog).getByRole("button", { name: "Request changes" }));
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Request changes" })).not.toBeInTheDocument()
    );
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(queryClient.getQueryState(["artifact-reviews", "artifact-1"])?.isInvalidated).toBe(true);
    expect(createArtifactVersionReview).toHaveBeenCalledTimes(1);
  } finally {
    createArtifactVersionReview.mockRestore();
    view.unmount();
  }
});

test("clicking the chip opens a popover listing every review", async () => {
  const reviews: ArtifactReview[] = [
    {
      actor: { id: "alice", kind: "user" },
      artifact_id: "artifact-1",
      ask_id: null,
      content_sha256: null,
      content_sha256_source: null,
      created_at: "2026-09-10T00:00:00Z",
      id: "review-1",
      kind: "person",
      reason: null,
      state: "approved",
      version: 2,
    },
    {
      actor: { id: "bob", kind: "user" },
      artifact_id: "artifact-1",
      ask_id: null,
      content_sha256: null,
      content_sha256_source: null,
      created_at: "2026-09-11T00:00:00Z",
      id: "review-2",
      kind: "person",
      reason: "Please tighten the intro.",
      state: "changes_requested",
      version: 3,
    },
  ];
  const user = userEvent.setup();
  const listArtifactReviews = spyOn(api, "listArtifactReviews").mockResolvedValue(reviews);
  const view = renderChip(
    artifact({ latest_version: 3, state: "changes_requested", version: 3 }),
    "list"
  );
  try {
    await user.click(screen.getByRole("button", { name: "Changes requested" }));
    // The dialog is drawn at once, saying `Loading reviews…`, so its rows are waited for.
    const dialog = await screen.findByRole("dialog", { name: "Reviews" });
    expect(await within(dialog).findByText(/alice approved v2/)).not.toBeNull();
    expect(within(dialog).getByText(/bob requested changes on v3/)).not.toBeNull();
    expect(
      await within(dialog).findByText((text) => text.includes("Please tighten the intro."))
    ).not.toBeNull();
  } finally {
    listArtifactReviews.mockRestore();
    view.unmount();
  }
});

// A host that selected this approval request - the Inbox's reading pane - learns from the chip
// that the reader reviewed it here, so it can move on to the next item.
test("a review the server records tells the host, once, that the document was reviewed", async () => {
  const user = userEvent.setup();
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockResolvedValue({
    actor: { id: "alice", kind: "user" },
    artifact_id: "artifact-1",
    ask_id: null,
    content_sha256: null,
    content_sha256_source: null,
    created_at: "2026-09-11T00:00:00Z",
    id: "review-1",
    kind: "person",
    reason: null,
    state: "approved",
    version: 3,
  });
  let reviewed = 0;
  const view = renderChip(artifact({ latest_version: 3, state: "awaiting" }), "header", () => {
    reviewed += 1;
  });
  try {
    expect(reviewed).toBe(0);
    await user.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(reviewed).toBe(1));
    expect(createArtifactVersionReview).toHaveBeenCalledTimes(1);
  } finally {
    createArtifactVersionReview.mockRestore();
    view.unmount();
  }
});

/** The address of `target`'s own page on its issue. */
function pathOf(target: Artifact): string {
  return buildIssuePath({ key: "CORE-1", kind: "artifact", slug: target.slug });
}

/** The reader going back: the browser's Back button, or anything that calls `navigate(-1)`. */
function GoBack(): ReactNode {
  const navigate = useNavigate();
  return (
    <button onClick={() => navigate(-1)} type="button">
      Go back
    </button>
  );
}

/** The header of whichever of `documents` the route names: as on an issue's page, one header is
 *  drawn for one document after another as the route moves. */
function HeaderOfRoute({ documents }: { readonly documents: readonly Artifact[] }): ReactNode {
  const route = parseIssuePath(useLocation().pathname);
  const shown =
    route?.kind === "artifact" ? documents.find(({ slug }) => slug === route.slug) : undefined;
  return shown === undefined ? null : (
    <ArtifactHeader
      artifact={shown}
      highlight={false}
      showVersionPicker={false}
      version={undefined}
    >
      {null}
    </ArtifactHeader>
  );
}

// A change request belongs to the document it was made on: the dialog holds the reason typed for
// that document and what became of its request. Here Dispatch refused the request, the dialog
// said to try again, and the reader went back to another document of the issue with it open. The
// header there has no dialog open, and a change request there carries the reason typed for that
// document: the dialog left open would post the first document's reason on this one.
test("a change request refused on one document posts none of its reason on the document the reader goes back to", async () => {
  const user = userEvent.setup();
  const designNotes = artifact({ latest_version: 3, state: "awaiting" });
  const releaseNotes: Artifact = {
    ...designNotes,
    id: "artifact-2",
    name: "Release notes",
    ref_key: "CORE-1/release-notes",
    slug: "release-notes",
  };
  const createArtifactVersionReview = spyOn(api, "createArtifactVersionReview").mockRejectedValue(
    new ApiError(403, { code: "FORBIDDEN", error: "not a reviewer of CORE-1" })
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter initialEntries={[pathOf(releaseNotes), pathOf(designNotes)]} initialIndex={1}>
      <QueryClientProvider client={queryClient}>
        <HeaderOfRoute documents={[designNotes, releaseNotes]} />
        <GoBack />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByRole("heading", { name: "Design notes" });
    await user.click(screen.getByRole("button", { name: "Request changes" }));
    const refused = screen.getByRole("dialog", { name: "Request changes" });
    await user.click(within(refused).getByLabelText("Reason"));
    await user.paste("The intro names the wrong service");
    await user.click(within(refused).getByRole("button", { name: "Request changes" }));
    expect(await within(refused).findByRole("alert")).toHaveTextContent(
      "Could not request changes. Try again."
    );

    await user.click(screen.getByRole("button", { name: "Go back" }));
    await screen.findByRole("heading", { name: "Release notes" });
    expect(screen.queryByRole("dialog", { name: "Request changes" })).toBeNull();

    createArtifactVersionReview.mockResolvedValue({
      actor: { id: "alice", kind: "user" },
      artifact_id: "artifact-2",
      ask_id: null,
      content_sha256: null,
      content_sha256_source: null,
      created_at: "2026-09-11T00:00:00Z",
      id: "review-1",
      kind: "person",
      reason: "The dates are missing",
      state: "changes_requested",
      version: 3,
    });
    await user.click(screen.getByRole("button", { name: "Request changes" }));
    const dialog = screen.getByRole("dialog", { name: "Request changes" });
    expect(within(dialog).getByLabelText("Reason")).toHaveValue("");
    await user.click(within(dialog).getByLabelText("Reason"));
    await user.paste("The dates are missing");
    await user.click(within(dialog).getByRole("button", { name: "Request changes" }));

    await waitFor(() => expect(createArtifactVersionReview).toHaveBeenCalledTimes(2));
    expect(createArtifactVersionReview).toHaveBeenLastCalledWith(
      "artifact-2",
      3,
      { reason: "The dates are missing", state: "changes_requested" },
      keyedAttempt
    );
  } finally {
    createArtifactVersionReview.mockRestore();
    view.unmount();
  }
});
