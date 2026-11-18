import { expect, spyOn, test } from "bun:test";
import { ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS } from "@legion/contracts";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { ApiError, api } from "../../api/client";
import { issueQuery } from "../../api/queries";
import type { Agent, ClaimHolderLiveness, Issue, IssueClaim, IssueDetails } from "../../api/types";
import { dangerText, pageFailedText, pageHeaderControl } from "../../theme/classes";
import { stateForIssue } from "../issue/pins";
import { MarginProvider } from "../margin/margin-context";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { IssueClaimControl } from "./IssueClaimControl";
import { NoTooltips } from "./NoTooltips";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import { useIssueHeaderWrites } from "./useIssueHeaderWrites";

const claim: IssueClaim = {
  actor: { kind: "session", id: "session-one", origin: { session_title: "Implementer" } },
  at: "2026-09-24T06:00:00Z",
};

const base: Issue = {
  advances: [],
  advances_revision: 0,
  assignee: "alice",
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  closed_at: null,
  created_at: "2026-09-24T05:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  parent: null,
  blocked_by: [],
  plan_artifact_id: "",
  decisions_artifact_id: "",
  primary_artifact_id: "artifact-1",
  verification_artifact_id: "",
  priority: null,
  project: "CORE",
  rank: "U",
  route: null,
  repo: null,
  resolved_repo: null,
  status: "in_progress",
  title: "Claim the issue before working it",
  updated_at: "2026-09-24T06:00:00Z",
};

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

/**
 * The claim's row of Details around one issue. `liveness` is what the issue read says of the
 * claim's holder, which outranks the agent list.
 *
 * `tooltips` is the surface the chip is drawn on. The issue page raises no tooltip at all
 * (`components/tooltips.tsx`), which is this helper's default and what decides whether each
 * instant is drawn in words; the chip's other hosts, a List row and a Board card, do raise one,
 * and a row that reads the chip's `title` asks for that surface.
 */
function claimRow(
  issue: Issue,
  liveness: ClaimHolderLiveness = {},
  { tooltips = false }: { tooltips?: boolean } = {}
): { client: QueryClient; unmount: () => void; user: UserEvent } {
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
    unmount: () => view.unmount(),
    user,
  };
}

/** One live session, as `GET /api/v1/agents` lists it. */
function running(sessionID: string, title: string): Agent {
  return {
    session_id: sessionID,
    title,
    dir: "/w",
    host: "host",
    machine_id: "host",
    roles: [],
    capabilities: [],
    last_seen: 1,
    open_asks: 0,
    last_activity: null,
    state: "live",
  };
}

/** The same session after it has missed its heartbeats: the listener still lists it, and says
 *  so on the row (`state`), with the heartbeat it was last seen at. */
function unresponsive(sessionID: string, title: string, lastSeen: number): Agent {
  return { ...running(sessionID, title), last_seen: lastSeen, state: "unresponsive" };
}

test("a claimed issue names its holder and releases the claim from Details", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    running("session-one", "Implementer"),
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
    running("session-two", "Someone else"),
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
    running("session-one", "Implementer"),
  ]);
  const release = spyOn(api, "releaseIssueClaim").mockRejectedValue(new Error("network error"));
  const view = claimRow({ ...base, claim });
  try {
    await view.user.click(
      await screen.findByRole("button", { name: "Release the claim on CORE-1" })
    );
    const alert = await screen.findByRole("alert");
    // A release that got no answer from Dispatch has no reason to say: the line is its words.
    expect(alert.textContent).toBe("Could not release this claim.");
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
    running("session-one", "Implementer"),
  ]);
  const release = spyOn(api, "releaseIssueClaim").mockRejectedValue(new Error("network error"));
  const view = claimRow({ ...base, claim });
  try {
    await view.user.click(
      await screen.findByRole("button", { name: "Release the claim on CORE-1" })
    );
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not release this claim.");
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
    running("session-one", "Implementer"),
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
    expect(alert, "the refusal does not say the release failed").toHaveTextContent(
      "Could not release this claim."
    );
    expect(alert, "the refusal does not say Dispatch's reason").toHaveTextContent(reason);
  } finally {
    view.unmount();
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
    running("session-one", "Implementer"),
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
    running("session-one", "Implementer"),
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
    expect(failedAgain).toHaveTextContent("Could not release this claim.");
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
    running("session-one", "Implementer"),
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

    const read = view.client.getQueryData<IssueDetails>(issueQuery("CORE-1").queryKey);
    if (read === undefined) throw new Error("the row reads no cached issue");
    act(() => view.client.setQueryData(issueQuery("CORE-1").queryKey, { ...read, claim: null }));
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

test("the claim shows the holder's live title, not the one stamped on the claim", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockResolvedValue([
    {
      session_id: "session-one",
      title: "Live registry title",
      dir: "/w",
      host: "host",
      machine_id: "host",
      roles: [],
      capabilities: [],
      last_seen: 1,
      open_asks: 0,
      last_activity: null,
      state: "live",
    },
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
    unresponsive("session-one", "Implementer", lastSeen),
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
    unresponsive("session-one", "Implementer", lastSeen),
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
    unresponsive("session-one", "Implementer", lastSeen),
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
test("a host that raises a tooltip keeps the unread registry in the title and draws no word", async () => {
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const agents = spyOn(api, "listAgents").mockRejectedValue(new Error("Envoy unavailable"));
  const view = claimRow({ ...base, claim }, {}, { tooltips: true });
  try {
    await waitFor(() =>
      expect(screen.getByTestId("issue-claim").getAttribute("title")).toContain(
        "Couldn't check whether that session is still running"
      )
    );
    expect(screen.queryByTestId("issue-claim-unchecked")).toBeNull();
  } finally {
    view.unmount();
    agents.mockRestore();
    subscribers.mockRestore();
  }
});
