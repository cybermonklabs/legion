import { expect, spyOn, test } from "bun:test";
import { ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { api } from "../../api/client";
import type { Agent, ClaimHolderLiveness, Issue, IssueClaim, IssueDetails } from "../../api/types";
import { dangerText, pageFailedText, pageHeaderControl } from "../../theme/classes";
import { stateForIssue } from "../issue/pins";
import { MarginProvider } from "../margin/margin-context";
import { IssueClaimControl } from "./IssueClaimControl";
import { NoTooltips } from "./NoTooltips";
import { CONTROL_LABELS } from "./page-words";
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

/** The claim's row of Details, over the page's own writes, as `IssueMargin.tsx` draws it. */
function ClaimRow({ issue }: { issue: IssueDetails }): ReactNode {
  const writes = useIssueHeaderWrites(issue, stateForIssue(undefined, issue.key));
  return <IssueClaimControl issue={issue} writes={writes} />;
}

/**
 * The claim's row of Details around one issue (dispatch://INFRA-1196 R13). `liveness` is what
 * the issue read says of the claim's holder, which outranks the agent list.
 *
 * `tooltips` is the surface the chip is drawn on. The issue page raises no tooltip at all
 * (dispatch://INFRA-1196 R21, `components/tooltips.tsx`), which is this helper's default and
 * what decides whether each instant is drawn in words; the chip's other hosts, a List row and
 * a Board card, do raise one, and a row that reads the chip's `title` asks for that surface.
 */
function claimRow(
  issue: Issue,
  liveness: ClaimHolderLiveness = {},
  { tooltips = false }: { tooltips?: boolean } = {}
): { unmount: () => void; user: UserEvent } {
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
  const wrapper = ({ children }: { children: ReactNode }) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MarginProvider>{tooltips ? children : <NoTooltips>{children}</NoTooltips>}</MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const user = userEvent.setup();
  const view = render(<ClaimRow issue={details} />, { wrapper });
  return {
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
    // page draws none at all (dispatch://INFRA-1196 R21, `components/tooltips.tsx`).
    const claimed = await screen.findByTestId("issue-claim");
    // The row's own label says "Claimed by" (`IssueMargin.tsx`), so the chip opens with the
    // holder (dispatch://INFRA-1196 R13).
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
    expect(alert).toHaveTextContent("Could not release this claim.");
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

// A holder that has missed its heartbeats has not ended: it is still working the issue, and the
// chip that called it "not running" sent the next agent to take work that was being done
// (dispatch://INFRA-392). The chip has to tell the two apart, say since when it has been quiet
// (R5), and say how long the wait is.
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
// the chip draws it (dispatch://INFRA-1196 R21).
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
