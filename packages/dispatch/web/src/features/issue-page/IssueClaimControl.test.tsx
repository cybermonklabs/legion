import { expect, jest, type Mock, spyOn, test } from "bun:test";
import { ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS } from "@legion/contracts";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { realClock } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { issueQuery } from "../../api/queries";
import type { ClaimHolderLiveness, Issue, IssueClaim, IssueDetails } from "../../api/types";
import { dangerText, pageFailedText, pageHeaderControl } from "../../theme/classes";
import { runningAgent } from "../credentials/agent-fixture";
import { stateForIssue } from "../issue/pins";
import { MarginProvider } from "../margin/margin-context";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { IssueClaimControl, RELEASE_FAILED } from "./IssueClaimControl";
import {
  claimIssue as base,
  sessionClaim as claim,
  reviewerClaim,
  settleFocus,
} from "./issue-fixtures";
import { NoTooltips } from "./NoTooltips";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import { useIssueHeaderWrites } from "./useIssueHeaderWrites";

/** The claim's row of Details, over the page's own writes, as `IssueMargin.tsx` draws it: on the
 *  cached issue, as the details slot reads it, so a release that lands is what the row draws
 *  next. */
function ClaimRow({ issueKey }: { issueKey: string }): ReactNode {
  const issue = useQuery({ ...issueQuery(issueKey), enabled: false }).data;
  return issue === undefined ? null : <ClaimWrites issue={issue} />;
}

function ClaimWrites({ issue }: { issue: IssueDetails }): ReactNode {
  const writes = useIssueHeaderWrites(issue, stateForIssue(undefined, issue.key));
  return <IssueClaimControl issue={issue} writes={writes} />;
}

/** The claim's row of Details that `claimRow` draws, with what a test drives it by. */
interface ClaimRowView {
  client: QueryClient;
  drawClaim: (next: IssueClaim | null) => void;
  unmount: () => void;
  user: UserEvent;
}

/**
 * The claim's row of Details around one issue. `liveness` is what the issue read says of the
 * claim's holder, which outranks the agent list. Answers, with the row's client and user, its
 * `drawClaim`, which puts another claim, or none, on the cached issue the row is drawn on, as a
 * read of the issue that found it so would.
 *
 * `tooltips` is the surface the chip is drawn on. The issue page raises no tooltip at all
 * (`components/tooltips.tsx`), which is this helper's default and what decides whether each
 * instant is drawn in words; the chip's other hosts, a List row and a Board card, do raise one,
 * and a row that reads the chip's tooltip asks for that surface.
 */
function claimRow(
  issue: Issue,
  liveness: ClaimHolderLiveness = {},
  { tooltips = false }: { tooltips?: boolean } = {}
): ClaimRowView {
  const details: IssueDetails = {
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    holder_state: null,
    last_seen: null,
    takeable_at: null,
    ...issue,
    ...liveness,
    artifacts: [],
    children: [],
    design_gates: [],
    open_asks: [],
    progress: { tasks: null, children: null },
    referenced_by_count: 0,
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(issueQuery(details.key).queryKey, details);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MarginProvider>{tooltips ? children : <NoTooltips>{children}</NoTooltips>}</MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const user = userEvent.setup();
  const view = render(<ClaimRow issueKey={details.key} />, { wrapper });
  return {
    client,
    drawClaim: (next) => {
      const read = client.getQueryData<IssueDetails>(issueQuery(details.key).queryKey);
      if (read === undefined) throw new Error("the row reads no cached issue");
      act(() => client.setQueryData(issueQuery(details.key).queryKey, { ...read, claim: next }));
    },
    unmount: () => view.unmount(),
    user,
  };
}

test("a claimed issue names its holder and releases the claim from Details", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "session-one", title: "Implementer" }),
  ]);
  const release = spyOn(api, "releaseIssueClaim").mockResolvedValue({ ...base, claim: null });
  const view = claimRow({ ...base, claim });
  try {
    // The chip says who holds the issue and since when in words, not in a tooltip: the issue
    // page draws none at all (`components/tooltips.tsx`).
    const claimed = await screen.findByTestId("issue-claim");
    // The row's own label says "Claimed by" (`IssueMargin.tsx`), so the chip opens with the
    // holder.
    expect(claimed.textContent).not.toContain("Claimed by");
    expect(claimed.textContent).toContain("Implementer");
    expect(claimed.querySelector("time")?.textContent ?? "").toMatch(/\(.+\)/);
    // The holder is drawn by `AgentName`: its name links to its live conversation, and the
    // registry lists it, so it reads as running and the chip adds nothing.
    const chip = screen.getByTestId("issue-claim");
    expect(within(chip).getByRole("link", { name: "Implementer" })).toHaveAttribute(
      "href",
      "/agents/session-one/live"
    );
    await waitFor(() =>
      expect(
        chip.querySelector('[data-agent-name="session-one"]')?.getAttribute("data-agent-state")
      ).toBe("running")
    );
    expect(screen.queryByText("not running")).not.toBeInTheDocument();
    await view.user.click(screen.getByRole("button", { name: "Release the claim on CORE-1" }));
    await waitFor(() => expect(release).toHaveBeenCalledWith("CORE-1", keyedAttempt));
  } finally {
    view.unmount();
    release.mockRestore();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// A claim whose session the registry no longer lists is the one any agent may take, and the
// Unclaimed filter counts it as free: the chip has to say so, or a reader sees held work.
test("a holder the registry no longer lists reads as not running", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "session-two", title: "Someone else" }),
  ]);
  const view = claimRow({ ...base, claim });
  try {
    expect(await screen.findByText("not running")).toBeDefined();
    // "not running" is drawn, not hovered: the chip carries no tooltip.
    expect(screen.queryByTitle(/no longer running/)).toBeNull();
    const chip = screen.getByTestId("issue-claim");
    expect(
      chip.querySelector('[data-agent-name="session-one"]')?.getAttribute("data-agent-state")
    ).toBe("not-running");
    expect(within(chip).getByRole("img", { name: "Not running" })).toBeDefined();
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// Releasing can fail — an expired session, a 404, a dropped connection — and a button that
// silently returns to "Release" tells a human their click did nothing.
test("a release that fails says so and offers a retry", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "session-one", title: "Implementer" }),
  ]);
  const release = spyOn(api, "releaseIssueClaim").mockRejectedValue(new Error("network error"));
  const view = claimRow({ ...base, claim });
  try {
    await view.user.click(
      await screen.findByRole("button", { name: "Release the claim on CORE-1" })
    );
    const alert = await screen.findByRole("alert");
    // A release that got no answer from Dispatch has no reason to say: the line is its words.
    expect(alert.textContent).toBe(RELEASE_FAILED);
    await view.user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(release).toHaveBeenCalledTimes(2));
  } finally {
    view.unmount();
    release.mockRestore();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// The failure is said by Details' own line (`details-error.tsx`): in the page's colour for
// what failed, not the app's danger colour, with the page's Retry beside it.
test("a failed release is said in the page's colours, with Retry", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "session-one", title: "Implementer" }),
  ]);
  const release = spyOn(api, "releaseIssueClaim").mockRejectedValue(new Error("network error"));
  const view = claimRow({ ...base, claim });
  try {
    await view.user.click(
      await screen.findByRole("button", { name: "Release the claim on CORE-1" })
    );
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(RELEASE_FAILED);
    expect(alert, "the failure is not in the page's colour for what failed").toHaveClass(
      pageFailedText
    );
    for (const token of dangerText.split(" ")) {
      expect(alert, "the failure is in the app's danger colour").not.toHaveClass(token);
    }
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
    expect(retry, "Retry is not drawn as the page's control").toHaveClass(pageHeaderControl);
    await view.user.click(retry);
    await waitFor(() => expect(release).toHaveBeenCalledTimes(2));
  } finally {
    view.unmount();
    release.mockRestore();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// A release Dispatch refused said only that it failed, so a reader was not told why. The line
// says Dispatch's reason beside its words, as the Parent row says a refused parent.
test("a release Dispatch refuses says Dispatch's reason beside its words", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "session-one", title: "Implementer" }),
  ]);
  const reason = "bob claimed this issue at 2026-09-24T06:00:00Z; ask bob to release it";
  const release = spyOn(api, "releaseIssueClaim").mockRejectedValue(
    new ApiError(409, { code: "ISSUE_CLAIMED", error: reason })
  );
  const view = claimRow({ ...base, claim });
  try {
    await view.user.click(
      await screen.findByRole("button", { name: "Release the claim on CORE-1" })
    );
    const alert = await screen.findByRole("alert");
    expect(alert, "the refusal does not say the release failed").toHaveTextContent(RELEASE_FAILED);
    expect(alert, "the refusal does not say Dispatch's reason").toHaveTextContent(reason);
  } finally {
    view.unmount();
    release.mockRestore();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// A release the reader stopped may have been made: the row says so in the header's words, and
// that its Retry sends the same change again - a `DELETE` sends no key - rather than that the
// release failed. The line is about that press while the release its Retry sent is out too, so
// it keeps those words then rather than turn to the failure's.
test("a release the reader stopped says in Details that it may have been made, not that it failed", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "session-one", title: "Implementer" }),
  ]);
  const release = spyOn(api, "releaseIssueClaim").mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1/claim", new TypeError("Failed to fetch"))
  );
  const view = claimRow({ ...base, claim });
  const stopped = stoppedWriteNote("the release of this claim", "made", {
    method: "DELETE",
    stopped: new SendStoppedError(),
  });
  try {
    const control = await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(control);
    await stopTheLateWrite(view.client);
    expect(screen.getByRole("alert").textContent).toBe(stopped);

    const retried = Promise.withResolvers<Issue>();
    release.mockImplementationOnce(() => retried.promise);
    const made = release.mock.calls.length;
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.retry }));
    await waitFor(() => expect(release).toHaveBeenCalledTimes(made + 1));
    await waitFor(() => expect(control).toHaveTextContent(ANSWER_LINES.saving_release));
    expect(
      screen.getByRole("alert").textContent,
      "the line turned to a failure while the release its Retry sent is out"
    ).toBe(stopped);

    await act(async () => retried.resolve({ ...base, claim: null }));
    expect(await screen.findByText(CONTROL_LABELS.none)).toBeDefined();
  } finally {
    realClock();
    view.unmount();
    view.client.clear();
    release.mockRestore();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// Release turned disabled while its release was out, which lost the focus of a reader who
// pressed it by keyboard, since Chromium moves the focus of a control that turns disabled to the
// page's body (`emulateChromiumFocusFixup` does that here). Release stays enabled and refuses a
// press with `aria-disabled` while its release is out, as the page's controls do while they
// save, and a press then sends nothing.
test("Release keeps the focus while its release is out, refuses a press, and a second press sends none", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "session-one", title: "Implementer" }),
  ]);
  const released = Promise.withResolvers<Issue>();
  const release = spyOn(api, "releaseIssueClaim").mockImplementation(() => released.promise);
  const view = claimRow({ ...base, claim });
  const stopBlurring = emulateChromiumFocusFixup();
  try {
    const button = await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    act(() => button.focus());
    await view.user.keyboard("{Enter}");
    await waitFor(() => expect(release).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(button, "Release does not say the release is out").toHaveTextContent(
        ANSWER_LINES.saving_release
      )
    );
    expect(document.activeElement, "Release lost the focus while its release is out").toBe(button);
    expect(button, "Release does not refuse a press while its release is out").toHaveAttribute(
      "aria-disabled",
      "true"
    );
    await view.user.keyboard("{Enter}");
    expect(
      release,
      "a press of Release while its release is out sent another"
    ).toHaveBeenCalledTimes(1);

    await act(async () => released.resolve({ ...base, claim: null }));
    expect(await screen.findByText(CONTROL_LABELS.none)).toBeDefined();
  } finally {
    stopBlurring();
    view.unmount();
    release.mockRestore();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// The failure's line was drawn only while the release stood failed, so a press of its Retry took
// the line, and the Retry with it, away while the release it sent again was out: the focus of a
// reader who pressed Retry by keyboard went to the page's body, and stayed there when that
// release failed again and the line came back. The line stays, with its Retry holding the focus,
// until the release is answered; a second failure is drawn afresh, so a screen reader says it
// again; and once the release lands and the row reads None, the focus goes to that None rather
// than to the page.
test("the failed release's Retry keeps the focus while the release is out and fails again, and hands it to None once the release lands", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "session-one", title: "Implementer" }),
  ]);
  const release = spyOn(api, "releaseIssueClaim").mockRejectedValueOnce(new Error("network error"));
  const view = claimRow({ ...base, claim });
  const stopBlurring = emulateChromiumFocusFixup();
  const retryOfRelease = () => screen.queryByRole("button", { name: CONTROL_LABELS.retry });
  const releaseControl = () => screen.getByRole("button", { name: "Release the claim on CORE-1" });
  try {
    await view.user.click(
      await screen.findByRole("button", { name: "Release the claim on CORE-1" })
    );
    const failed = await screen.findByRole("alert");
    const retry = retryOfRelease();
    if (retry === null) throw new Error("the failed release offers no Retry");

    const retried = Promise.withResolvers<Issue>();
    release.mockImplementationOnce(() => retried.promise);
    act(() => retry.focus());
    await view.user.keyboard("{Enter}");
    await waitFor(() => expect(release).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(releaseControl()).toHaveTextContent(ANSWER_LINES.saving_release));
    expect(
      retryOfRelease(),
      "the line and its Retry went while the release they sent again is out"
    ).toBe(retry);
    expect(
      document.activeElement,
      "Retry lost the focus while the release it sent again is out"
    ).toBe(retry);
    expect(
      retry,
      "Retry does not refuse a press while the release it sent again is out"
    ).toHaveAttribute("aria-disabled", "true");
    await view.user.keyboard("{Enter}");
    expect(release, "a press of Retry while its release is out sent another").toHaveBeenCalledTimes(
      2
    );

    await act(async () => retried.reject(new Error("network error again")));
    await waitFor(() => expect(releaseControl()).toHaveTextContent(CONTROL_LABELS.release_claim));
    const failedAgain = screen.getByRole("alert");
    expect(failedAgain).toHaveTextContent(RELEASE_FAILED);
    expect(
      failedAgain,
      "the second failure was not drawn afresh, so a screen reader would not say it"
    ).not.toBe(failed);
    expect(retryOfRelease(), "the line came back with another Retry").toBe(retry);
    expect(document.activeElement, "Retry lost the focus when the release failed again").toBe(
      retry
    );
    expect(retry, "Retry still refuses a press after the release failed again").not.toHaveAttribute(
      "aria-disabled",
      "true"
    );

    const landed = Promise.withResolvers<Issue>();
    release.mockImplementationOnce(() => landed.promise);
    await view.user.keyboard("{Enter}");
    await waitFor(() => expect(release).toHaveBeenCalledTimes(3));
    await act(async () => landed.resolve({ ...base, claim: null }));
    const none = await screen.findByText(CONTROL_LABELS.none);
    expect(
      document.activeElement,
      "the release landed, and the focus fell to the page rather than to the row's None"
    ).toBe(none);
  } finally {
    stopBlurring();
    view.unmount();
    release.mockRestore();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// A release that lands with no answer to give (Dispatch answers its key `KEY_COMMITTED`) takes
// the failure's line away while the row still draws the claim, until the issue is read again: the
// focus Retry held goes to Release beside it, and from there to the row's None once the read
// says nobody holds the issue, rather than to the page either time.
test("a release that lands with no answer hands Retry's focus to Release, and Release's to None once the claim is read gone", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "session-one", title: "Implementer" }),
  ]);
  const release = spyOn(api, "releaseIssueClaim").mockRejectedValueOnce(new Error("network error"));
  const view = claimRow({ ...base, claim });
  const stopBlurring = emulateChromiumFocusFixup();
  try {
    const control = await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    await view.user.click(control);
    await screen.findByRole("alert");
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });

    release.mockRejectedValueOnce(
      new ApiError(409, { code: "KEY_COMMITTED", error: "already made" })
    );
    act(() => retry.focus());
    await view.user.keyboard("{Enter}");
    await waitFor(() =>
      expect(
        screen.queryByRole("alert"),
        "the line is still drawn after the release landed"
      ).toBeNull()
    );
    expect(
      document.activeElement,
      "the release landed with no answer, and the focus fell to the page rather than to Release"
    ).toBe(control);

    view.drawClaim(null);
    const none = await screen.findByText(CONTROL_LABELS.none);
    expect(
      document.activeElement,
      "the claim was read gone, and the focus fell to the page rather than to the row's None"
    ).toBe(none);
  } finally {
    stopBlurring();
    view.unmount();
    release.mockRestore();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

/**
 * The claim's row of Details on `claim`, with the registry listing both `claim`'s session and
 * `reviewerClaim`'s, and the first release failing. Answers the release spy, the row
 * (`claimRow`), and `restore`, which takes the row off the page and puts the spies back.
 */
function twoSessionClaimRow(): {
  release: Mock<typeof api.releaseIssueClaim>;
  restore: () => void;
  view: ClaimRowView;
} {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "session-one", title: "Implementer" }),
    runningAgent({ session_id: "session-two", title: "Reviewer" }),
  ]);
  const release = spyOn(api, "releaseIssueClaim").mockRejectedValueOnce(new Error("network error"));
  const view = claimRow({ ...base, claim });
  return {
    release,
    restore: () => {
      view.unmount();
      release.mockRestore();
      agents.mockRestore();
      subscribers.mockRestore();
    },
    view,
  };
}

// Dispatch releases whichever claim the issue holds, so a release is about the claim it was
// pressed on. After a release that failed, the claim released elsewhere and the issue claimed
// again by another session, the row drew the old failure beside the new claim, with a Retry that
// would release that one. The failure goes with its claim: the new claim reads no failure and
// offers no Retry, and a Release of it draws no line of the old failure while it is out.
test("a failed release is forgotten once its claim is gone, so a new claim reads no failure and offers no Retry", async () => {
  const { release, restore, view } = twoSessionClaimRow();
  try {
    await view.user.click(
      await screen.findByRole("button", { name: "Release the claim on CORE-1" })
    );
    await screen.findByRole("alert");

    view.drawClaim(null);
    await screen.findByText(CONTROL_LABELS.none);
    view.drawClaim(reviewerClaim);
    const releaseNew = await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    expect(
      screen.queryByRole("alert"),
      "the new claim reads the failure of the release of the claim before it"
    ).toBeNull();
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.retry }),
      "the new claim offers a Retry that would release it"
    ).toBeNull();

    const out = Promise.withResolvers<Issue>();
    release.mockImplementationOnce(() => out.promise);
    await view.user.click(releaseNew);
    await waitFor(() => expect(releaseNew).toHaveTextContent(ANSWER_LINES.saving_release));
    expect(
      screen.queryByRole("alert"),
      "a Release of the new claim drew the old failure while it is out"
    ).toBeNull();
    await act(async () => out.resolve({ ...base, claim: null }));
    expect(await screen.findByText(CONTROL_LABELS.none)).toBeDefined();
  } finally {
    restore();
  }
});

// The failure's line stays while the release its Retry sent again is out (`useSaidFailure`), and
// the write reports no failure then, so a failure was forgotten only once it stood: the claim
// released elsewhere and the issue claimed again while that Retry was out, the row drew the old
// failure beside the new claim, with a Retry that would release it. The line goes with its claim
// whatever the write's state.
test("a failed release whose Retry is out draws no failure beside a new claim", async () => {
  const { release, restore, view } = twoSessionClaimRow();
  try {
    await view.user.click(
      await screen.findByRole("button", { name: "Release the claim on CORE-1" })
    );
    await screen.findByRole("alert");
    const retried = Promise.withResolvers<Issue>();
    release.mockImplementationOnce(() => retried.promise);
    await view.user.click(screen.getByRole("button", { name: CONTROL_LABELS.retry }));
    await waitFor(() => expect(release).toHaveBeenCalledTimes(2));

    view.drawClaim(null);
    await screen.findByText(CONTROL_LABELS.none);
    view.drawClaim(reviewerClaim);
    const releaseNew = await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    expect(
      screen.queryByRole("alert"),
      "the new claim reads the failure of the release of the claim before it while its Retry is out"
    ).toBeNull();
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.retry }),
      "the new claim offers a Retry that would release it"
    ).toBeNull();

    await act(async () => retried.reject(new Error("network error again")));
    await waitFor(() => expect(releaseNew).toHaveTextContent(CONTROL_LABELS.release_claim));
    expect(
      screen.queryByRole("alert"),
      "the failure of the Retry of the claim before it is said beside the new claim"
    ).toBeNull();
  } finally {
    restore();
  }
});

// A release is about the claim it was pressed on, which need not be the one the row was first
// drawn with: once the issue is claimed again while the row is drawn, a Release of the new claim
// that fails says so beside it.
test("a release that fails on a claim drawn after the row was says so beside it", async () => {
  const { restore, view } = twoSessionClaimRow();
  try {
    await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    view.drawClaim(reviewerClaim);
    await waitFor(() => expect(screen.getByTestId("issue-claim")).toHaveTextContent("Reviewer"));
    await view.user.click(screen.getByRole("button", { name: "Release the claim on CORE-1" }));
    await waitFor(() =>
      expect(
        screen.queryByRole("alert"),
        "a failed release of the claim drawn after the row was says nothing"
      ).not.toBeNull()
    );
    expect(screen.getByRole("alert")).toHaveTextContent(RELEASE_FAILED);
    expect(screen.getByRole("button", { name: CONTROL_LABELS.retry })).toBeDefined();
  } finally {
    restore();
  }
});

// The reader released the claim by keyboard, so the row handed the focus to its None. Another
// session then claimed the issue, and the row drew the new claim in None's place. That claim is no
// press of the reader's, so the focus stays on the page's body, as for every change the reader did
// not make, and nothing in the row takes it.
test("a claim made elsewhere while the focus is on the row's None leaves the focus on the page's body", async () => {
  const { release, restore, view } = twoSessionClaimRow();
  const stopBlurring = emulateChromiumFocusFixup();
  try {
    release.mockReset();
    release.mockResolvedValueOnce({ ...base, claim: null });
    const control = await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    act(() => control.focus());
    await view.user.keyboard("{Enter}");
    const none = await screen.findByText(CONTROL_LABELS.none);
    await waitFor(() =>
      expect(document.activeElement, "the release landed, and the focus is not on None").toBe(none)
    );

    view.drawClaim(reviewerClaim);
    const releaseNew = await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    await settleFocus();
    expect(
      document.activeElement,
      "another session claimed the issue while the focus was on the row's None, and the focus did not stay on the page's body"
    ).toBe(document.body);
    expect(releaseNew, "the new claim's Release took the focus").not.toHaveFocus();
  } finally {
    stopBlurring();
    restore();
  }
});

// Another session released the claim while the focus was on Release, and the row handed that
// focus to its None, as it does once the reader's own release lands. That release is no press of
// the reader's, so the focus stays on the page's body, and nothing in the row takes it.
test("a release made elsewhere while the focus is on Release leaves the focus on the page's body", async () => {
  const { restore, view } = twoSessionClaimRow();
  const stopBlurring = emulateChromiumFocusFixup();
  try {
    const control = await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    act(() => control.focus());

    view.drawClaim(null);
    const none = await screen.findByText(CONTROL_LABELS.none);
    await settleFocus();
    expect(
      document.activeElement,
      "another session released the claim while the focus was on Release, and the focus did not stay on the page's body"
    ).toBe(document.body);
    expect(none, "the row's None took the focus").not.toHaveFocus();
  } finally {
    stopBlurring();
    restore();
  }
});

// A release of the reader's that failed took nothing away, so the press that sent it answers no
// later loss: once another session releases the claim, the focus Release held stays on the page's
// body.
test("a release of the reader's that failed hands nothing on once the claim is released elsewhere", async () => {
  const { restore, view } = twoSessionClaimRow();
  const stopBlurring = emulateChromiumFocusFixup();
  try {
    const control = await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    act(() => control.focus());
    await view.user.keyboard("{Enter}");
    await screen.findByRole("alert");
    await waitFor(() => expect(control).toHaveTextContent(CONTROL_LABELS.release_claim));
    expect(document.activeElement, "Release lost the focus as its release failed").toBe(control);

    view.drawClaim(null);
    const none = await screen.findByText(CONTROL_LABELS.none);
    await settleFocus();
    expect(
      document.activeElement,
      "another session released the claim after the reader's release failed, and the focus did not stay on the page's body"
    ).toBe(document.body);
    expect(none, "the row's None took the focus").not.toHaveFocus();
  } finally {
    stopBlurring();
    restore();
  }
});

// A press belongs to the element it was made on. The reader pressed Release by pointer and moved
// the focus away before the release landed, so the claim went with no focus to hand on. A read of
// the issue that Dispatch answered before it had the release then drew that claim again, and
// another session released it while the focus was on Release: that release is no press of the
// reader's, so the focus stays on the page's body rather than go to the row's None.
test("a release whose claim went without the focus answers no later release made elsewhere", async () => {
  const { release, restore, view } = twoSessionClaimRow();
  const stopBlurring = emulateChromiumFocusFixup();
  try {
    const out = Promise.withResolvers<Issue>();
    release.mockReset();
    release.mockImplementationOnce(() => out.promise);
    const control = await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    await view.user.click(control);
    await waitFor(() => expect(control).toHaveTextContent(ANSWER_LINES.saving_release));
    act(() => control.blur());
    await act(async () => out.resolve({ ...base, claim: null }));
    await screen.findByText(CONTROL_LABELS.none);

    view.drawClaim(claim);
    const again = await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    act(() => again.focus());
    view.drawClaim(null);
    const none = await screen.findByText(CONTROL_LABELS.none);
    await settleFocus();
    expect(
      document.activeElement,
      "another session released the claim drawn again, and the focus did not stay on the page's body"
    ).toBe(document.body);
    expect(none, "the row's None took the focus").not.toHaveFocus();
  } finally {
    stopBlurring();
    restore();
  }
});

// The reader pressed the failed release's Retry, and while the release it sent again was out the
// issue was claimed again in another session, which takes the line, and Retry with it, away. That
// claim is no press of the reader's, so the focus stays on the page's body rather than go to the
// new claim's Release. Nor does the Retry's release move the focus once it lands: Dispatch
// releases whichever claim the issue holds, so it takes away the new claim, not the one Retry was
// pressed on.
test("the failed release's Retry, the claim taken again elsewhere while its release is out, hands nothing on then or once it lands", async () => {
  const { release, restore, view } = twoSessionClaimRow();
  const stopBlurring = emulateChromiumFocusFixup();
  try {
    await view.user.click(
      await screen.findByRole("button", { name: "Release the claim on CORE-1" })
    );
    await screen.findByRole("alert");
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
    const out = Promise.withResolvers<Issue>();
    release.mockImplementationOnce(() => out.promise);
    act(() => retry.focus());
    await view.user.keyboard("{Enter}");
    await waitFor(() => expect(release).toHaveBeenCalledTimes(2));

    view.drawClaim(reviewerClaim);
    await waitFor(() =>
      expect(retry.isConnected, "Retry is still drawn beside the new claim").toBe(false)
    );
    const releaseNew = screen.getByRole("button", { name: "Release the claim on CORE-1" });
    await settleFocus();
    expect(
      document.activeElement,
      "another session claimed the issue again while the focus was on Retry, and the focus did not stay on the page's body"
    ).toBe(document.body);
    expect(releaseNew, "the new claim's Release took the focus").not.toHaveFocus();

    act(() => releaseNew.focus());
    await act(async () => out.resolve({ ...base, claim: null }));
    const none = await screen.findByText(CONTROL_LABELS.none);
    await settleFocus();
    expect(
      document.activeElement,
      "the Retry's release took the new claim away, and the focus did not stay on the page's body"
    ).toBe(document.body);
    expect(none, "the row's None took the focus").not.toHaveFocus();
  } finally {
    stopBlurring();
    restore();
  }
});

// The reader pressed Release, and before the release was answered another session released the
// claim and claimed the issue again, which the row read at once. Dispatch releases whichever claim
// the issue holds, so the reader's release, once it lands, takes the new claim away, and Release
// with it while Release holds the focus. That claim is not the one the reader pressed Release on,
// so nothing hands the focus on then: it stays on the page's body.
test("a release whose claim was taken again elsewhere before it landed moves no focus once it lands", async () => {
  const { release, restore, view } = twoSessionClaimRow();
  const stopBlurring = emulateChromiumFocusFixup();
  try {
    const out = Promise.withResolvers<Issue>();
    release.mockReset();
    release.mockImplementationOnce(() => out.promise);
    const control = await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    act(() => control.focus());
    await view.user.keyboard("{Enter}");
    await waitFor(() => expect(control).toHaveTextContent(ANSWER_LINES.saving_release));

    view.drawClaim(reviewerClaim);
    await waitFor(() => expect(screen.getByTestId("issue-claim")).toHaveTextContent("Reviewer"));
    expect(document.activeElement, "Release lost the focus as the new claim was drawn").toBe(
      control
    );

    await act(async () => out.resolve({ ...base, claim: null }));
    const none = await screen.findByText(CONTROL_LABELS.none);
    await settleFocus();
    expect(
      document.activeElement,
      "the reader's release took away the claim made after they pressed it, and the focus did not stay on the page's body"
    ).toBe(document.body);
    expect(none, "the row's None took the focus").not.toHaveFocus();
  } finally {
    stopBlurring();
    restore();
  }
});

// The reader pressed Release by keyboard, and while the release is out a read of the issue finds
// the claim gone: the release's own event, which can arrive before its answer. That reached what
// the press asked for, so the row takes it for the press's answer, and the focus Release held goes
// to the row's None rather than to the page's body.
test("a claim read gone while the reader's release is out hands Release's focus to the row's None", async () => {
  const { release, restore, view } = twoSessionClaimRow();
  const stopBlurring = emulateChromiumFocusFixup();
  try {
    release.mockReset();
    // The reader's release is held out for the whole case.
    release.mockImplementationOnce(() => new Promise(() => undefined));
    const control = await screen.findByRole("button", { name: "Release the claim on CORE-1" });
    act(() => control.focus());
    await view.user.keyboard("{Enter}");
    await waitFor(() => expect(control).toHaveTextContent(ANSWER_LINES.saving_release));

    view.drawClaim(null);
    const none = await screen.findByText(CONTROL_LABELS.none);
    await waitFor(() =>
      expect(
        document.activeElement,
        "the claim was read gone while the reader's release was out, and the focus did not go to the row's None"
      ).toBe(none)
    );
  } finally {
    stopBlurring();
    restore();
  }
});

test("the claim shows the holder's live title, not the one stamped on the claim", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({ session_id: "session-one", title: "Live registry title" }),
  ]);
  const view = claimRow({ ...base, claim });
  try {
    expect(await screen.findByText("Live registry title")).toBeDefined();
    expect(screen.queryByText("Implementer")).not.toBeInTheDocument();
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

test("an unclaimed issue reads None and offers nothing to release", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const view = claimRow(base);
  try {
    expect(await screen.findByText(CONTROL_LABELS.none)).toBeDefined();
    expect(screen.queryByTitle(/Claimed by/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Release the claim/ })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// A holder that has missed its heartbeats has not ended: it is still working the issue, and a
// chip that called it "not running" once sent the next agent to take work that was being done.
// The chip has to tell the two apart, say since when it has been quiet, and say how long the
// wait is.
test("a holder that missed its heartbeats reads as unresponsive since its last heartbeat, not gone, and names when the claim comes free", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  // Six minutes of silence: inside the claim's grace, so the server would still refuse a take.
  const lastSeen = Date.now() - 6 * 60_000;
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({
      session_id: "session-one",
      title: "Implementer",
      last_seen: lastSeen,
      state: "unresponsive",
    }),
  ]);
  // Drawn as Details draws it, inside `NoTooltips`: the chip says in its words what a tooltip
  // would have held, and carries no `title`, since the issue page raises no tooltip.
  const view = claimRow({ ...base, claim });
  try {
    const stalled = await screen.findByTestId("issue-claim-unresponsive");
    expect(screen.queryByTestId("issue-claim-lapsed")).not.toBeInTheDocument();
    expect(screen.queryByText("not running")).not.toBeInTheDocument();
    // The state and the moment it started, which is what tells a reader whether to wait.
    expect(stalled).toHaveTextContent(
      `${CONTROL_LABELS.unresponsive} ${CONTROL_LABELS.unresponsive_since}`
    );
    expect(
      within(stalled)
        .getByTestId("issue-claim-unresponsive-since")
        .querySelector("time")
        ?.getAttribute("datetime")
    ).toBe(new Date(lastSeen).toISOString());
    // The wait the chip names is the sum the server refuses by, not a guess of its own, and the
    // issue is not free until then.
    const takeableAt = lastSeen + ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS;
    const free = within(stalled).getByTestId("issue-claim-takeable");
    expect(free.textContent).toBe(
      `, ${CONTROL_LABELS.takeable} ${new Date(takeableAt).toLocaleString(undefined, {
        dateStyle: "short",
        timeStyle: "short",
      })}`
    );
    expect(free.querySelector("time")?.getAttribute("datetime")).toBe(
      new Date(takeableAt).toISOString()
    );
    const chip = screen.getByTestId("issue-claim");
    expect(
      chip.querySelector('[data-agent-name="session-one"]')?.getAttribute("data-agent-state")
    ).toBe("unresponsive");
    // That the holder missed its heartbeats without ending, which a tooltip held, is the name of
    // the holder's dot.
    expect(
      within(chip).getByRole("img", {
        name: /^Unresponsive · .*missed its heartbeats without ending/u,
      })
    ).toBeDefined();
    expect(chip.getAttribute("title"), "the chip in Details carries a tooltip").toBeNull();
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// Past the grace the server grants the next agent that asks the issue, so a chip that still read
// as held would hide pickable work - the fault the Unclaimed filter had too. The holder has still
// not ended, so it is drawn as unresponsive and not as gone.
test("an unresponsive holder past its takeable moment reads takeable now, still unresponsive and never not running", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const lastSeen = Date.now() - (ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS + 60_000);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({
      session_id: "session-one",
      title: "Implementer",
      last_seen: lastSeen,
      state: "unresponsive",
    }),
  ]);
  // Drawn as Details draws it, inside `NoTooltips`: the chip says the page's words and carries
  // no `title`, since the issue page raises no tooltip.
  const view = claimRow({ ...base, claim });
  try {
    const stalled = await screen.findByTestId("issue-claim-unresponsive");
    expect(stalled).toHaveTextContent("unresponsive since");
    expect(within(stalled).getByTestId("issue-claim-takeable").textContent).toBe(
      `, ${CONTROL_LABELS.takeable_now}`
    );
    expect(screen.queryByTestId("issue-claim-lapsed")).not.toBeInTheDocument();
    expect(screen.queryByText("not running")).not.toBeInTheDocument();
    const chip = screen.getByTestId("issue-claim");
    expect(chip.getAttribute("title")).toBeNull();
    expect(
      chip.querySelector('[data-agent-name="session-one"]')?.getAttribute("data-agent-state")
    ).toBe("unresponsive");
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// The chip has to read as a sentence. It drew the relative age after "free for another agent
// from", which made ", free for another agent from in 3 minutes (11/11/26, 12:05)", and inside
// the last minute of the wait ", free for another agent from just now" - free, said of an issue
// the server still refuses to hand over.
test("the moment the issue comes free is drawn as a date and a time, so the chip reads as a sentence", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  // Half a minute of the grace left: the relative form of that moment is "just now", and the
  // server still refuses the take.
  const lastSeen = Date.now() - ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS + 30_000;
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    runningAgent({
      session_id: "session-one",
      title: "Implementer",
      last_seen: lastSeen,
      state: "unresponsive",
    }),
  ]);
  const view = claimRow({ ...base, claim });
  try {
    const stalled = await screen.findByTestId("issue-claim-unresponsive");
    const free = within(stalled).getByTestId("issue-claim-takeable");
    const takeableAt = lastSeen + ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS;
    expect(free.textContent).toBe(
      `, ${CONTROL_LABELS.takeable} ${new Date(takeableAt).toLocaleString(undefined, {
        dateStyle: "short",
        timeStyle: "short",
      })}`
    );
    expect(free.querySelector("time")?.getAttribute("datetime")).toBe(
      new Date(takeableAt).toISOString()
    );
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// The issue read judged the holder against its own record, by id; the agent list is a cache
// that can be missing a session it still holds. When the two disagree the read wins, or the
// page would call a stalled holder gone the moment the list dropped it.
test("the issue read's own holder_state outranks an agent list that does not name the holder", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([]);
  const lastSeen = new Date(Date.now() - 6 * 60_000).toISOString();
  const takeableAt = new Date(Date.now() + 14 * 60_000).toISOString();
  const liveness: ClaimHolderLiveness = {
    holder_state: "unresponsive",
    last_seen: lastSeen,
    takeable_at: takeableAt,
  };
  // Drawn as Details draws it, inside `NoTooltips`: the moment the claim comes free is the
  // chip's own words, and the chip carries no `title`.
  const view = claimRow({ ...base, claim }, liveness);
  try {
    const stalled = await screen.findByTestId("issue-claim-unresponsive");
    expect(screen.queryByTestId("issue-claim-lapsed")).not.toBeInTheDocument();
    // Both moments on the chip are the read's own, not the registry's.
    expect(
      within(stalled)
        .getByTestId("issue-claim-unresponsive-since")
        .querySelector("time")
        ?.getAttribute("datetime")
    ).toBe(lastSeen);
    const free = within(stalled).getByTestId("issue-claim-takeable");
    expect(free.querySelector("time")?.getAttribute("datetime")).toBe(takeableAt);
    expect(free.textContent).toBe(
      `, ${CONTROL_LABELS.takeable} ${new Date(takeableAt).toLocaleString(undefined, {
        dateStyle: "short",
        timeStyle: "short",
      })}`
    );
    expect(
      screen.getByTestId("issue-claim").getAttribute("title"),
      "the chip in Details carries a tooltip"
    ).toBeNull();
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// A registry that could not be read judges nothing, so the chip may not pass the claim off as
// held and may not call it gone either. On the issue page no tooltip carries that refusal, so
// the chip draws it.
test("a claim whose registry could not be read says the running state is unknown", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockRejectedValue(new Error("Envoy unavailable"));
  const view = claimRow({ ...base, claim });
  try {
    const unjudged = await screen.findByTestId("issue-claim-unchecked");
    expect(unjudged.textContent).toBe("running state unknown");
    expect(screen.queryByTestId("issue-claim-lapsed")).toBeNull();
    expect(screen.queryByText("not running")).toBeNull();
    // Drawn, not hovered: this surface raises no tooltip at all.
    expect(screen.getByTestId("issue-claim").getAttribute("title")).toBeNull();
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});

// Where a tooltip does carry it - a List row, a Board card - the chip says it there and draws
// no word of its own, which would say the same thing twice and widen a chip those hosts have
// no room for.
test("a host that raises a tooltip keeps the unread registry in the tooltip and draws no word", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockRejectedValue(new Error("Envoy unavailable"));
  const view = claimRow({ ...base, claim }, {}, { tooltips: true });
  try {
    const chip = screen.getByTestId("issue-claim");
    await view.user.hover(chip);
    await waitFor(() =>
      expect(screen.getByRole("tooltip").textContent).toContain(
        "Couldn't check whether that session is still running"
      )
    );
    expect(chip.getAttribute("title")).toBeNull();
    expect(screen.queryByTestId("issue-claim-unchecked")).toBeNull();
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});
