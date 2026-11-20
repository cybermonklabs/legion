import { afterEach, beforeEach, expect, setSystemTime, spyOn, test } from "bun:test";
import type { LegionRole } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, IssueCollaboratorAgent, IssueCollaborators } from "../../api/types";
import { collaboratorSections } from "../issue/CollaboratorsTab";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { IssueCollaboratorList } from "./IssueCollaboratorList";
import { PROPERTY_LABELS } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS, EMPTY_STATE_LINES, STATE_LINES } from "./page-words";

// The list says how long ago each one last wrote here and when each Legion agent started, and a
// live agent's last heartbeat is dated now (`live` below). Each case runs at one fixed midday, so
// none reads the wall clock. `setSystemTime` holds `Date` alone; the timers the cases wait on
// (React Query's, `waitFor`'s) run on the real clock as before.
const NOON = new Date(2026, 10, 19, 12, 0, 0, 0);
beforeEach(() => setSystemTime(NOON));
afterEach(() => setSystemTime());

function legionAgent(
  sessionId: string,
  role: "implementer" | "planner" | "reviewer" | "tester",
  startedAt: string,
  replacedBy: string | null = null
): IssueCollaboratorAgent {
  return {
    kind: "session",
    last_at: null,
    legion: {
      replaced_at: replacedBy === null ? null : "2026-10-26T05:00:00Z",
      replaced_by: replacedBy,
      role,
      role_token: `legion-p-core-1-${role}`,
      started_at: startedAt,
    },
    reasons: [],
    session_id: sessionId,
    title: `Legion ${role} · ${sessionId}`,
  };
}

function otherAgent(sessionId: string, lastAt: string | null): IssueCollaboratorAgent {
  return {
    kind: "session",
    last_at: lastAt,
    legion: null,
    reasons: ["posted"],
    session_id: sessionId,
    title: sessionId,
  };
}

function live(sessionId: string): Agent {
  return {
    capabilities: ["steer"],
    dir: "/w",
    host: "host",
    last_activity: null,
    last_seen: Date.now(),
    machine_id: "host",
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    state: "live",
    title: sessionId,
  };
}

/** The list under the control that opens it, as Details' People section draws the two. Its
 *  first read fails with `firstFailure` where one is given. */
function renderCollaborators(
  collaborators: IssueCollaborators,
  agents: readonly Agent[] = [],
  firstFailure?: Error
) {
  const read = spyOn(api, "getIssueCollaborators").mockResolvedValue(collaborators);
  if (firstFailure !== undefined) read.mockRejectedValueOnce(firstFailure);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([...agents]);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const owner = createRef<HTMLButtonElement>();
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <button ref={owner} type="button">
          {PROPERTY_LABELS.collaborators}
        </button>
        <IssueCollaboratorList issueKey="CORE-1" owner={owner} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    client,
    owner,
    read,
    restore: () => {
      view.unmount();
      read.mockRestore();
      listAgents.mockRestore();
    },
  };
}

/** Each row's own agent, in order: the name `Row` marks `data-collaborator`, never the one a
 *  "replaced by" line names inside the same row. */
function sessionsIn(container: HTMLElement): string[] {
  return [...container.querySelectorAll("[data-collaborator] [data-agent-name]")].map(
    (node) => node.getAttribute("data-agent-name") ?? ""
  );
}

// What the page makes of one answer in the order the server sends it (the contract's: the
// registry's agents by role in workflow order and then newest first, the rest newest first):
// Legion's agents split into a section per role, a replaced one naming its successor; then every
// other agent; then people with their reasons.
test("the collaborators split Legion agents into role sections and keep the server's order", async () => {
  const tab = renderCollaborators(
    {
      agents: [
        legionAgent("planner", "planner", "2026-10-26T00:00:00Z"),
        legionAgent("tester", "tester", "2026-10-26T03:00:00Z"),
        legionAgent("new-reviewer", "reviewer", "2026-10-26T05:00:00Z"),
        legionAgent("old-reviewer", "reviewer", "2026-10-26T01:00:00Z", "new-reviewer"),
        otherAgent("newer", "2026-10-26T02:00:00Z"),
        otherAgent("older", "2026-10-26T01:00:00Z"),
        otherAgent("quiet", null),
      ],
      issue_key: "CORE-1",
      legion_registry: "ok",
      people: [
        { kind: "user", last_at: "2026-10-26T02:00:00Z", login: "bob", reasons: ["posted"] },
        { kind: "user", last_at: null, login: "alice", reasons: ["assignee", "claimed"] },
      ],
    },
    [live("tester")]
  );

  try {
    const legion = await screen.findByRole("region", { name: CONTROL_LABELS.collaborators_legion });
    expect(
      within(legion)
        .getAllByRole("heading", { level: 3 })
        .map((heading) => heading.textContent)
    ).toEqual(["planner", "tester", "reviewer"]);
    expect(sessionsIn(legion)).toEqual(["planner", "tester", "new-reviewer", "old-reviewer"]);
    const replaced = legion.querySelector('[data-replaced-by="new-reviewer"]');
    expect(replaced?.textContent).toBe("replaced by Legion reviewer · new-reviewer");
    expect(replaced?.querySelector("a")?.getAttribute("href")).toBe("/agents/new-reviewer/live");
    // Each agent is the live link, with the registry's state: the tester runs, the rest do not.
    expect(
      within(legion)
        .getByRole("link", { name: "Legion reviewer · old-reviewer" })
        .getAttribute("href")
    ).toBe("/agents/old-reviewer/live");
    await waitFor(() =>
      expect(
        legion.querySelector('[data-agent-name="tester"]')?.getAttribute("data-agent-state")
      ).toBe("running")
    );
    expect(
      legion
        .querySelector('[data-collaborator] [data-agent-name="old-reviewer"]')
        ?.getAttribute("data-agent-state")
    ).toBe("not-running");

    const others = screen.getByRole("region", { name: CONTROL_LABELS.collaborators_others });
    expect(sessionsIn(others)).toEqual(["newer", "older", "quiet"]);

    const people = screen.getByRole("region", { name: CONTROL_LABELS.collaborators_people });
    expect(
      within(people)
        .getAllByRole("listitem")
        .map((row) => row.textContent)
    ).toEqual([expect.stringContaining("bobwrote here"), "aliceassigned, claimed it"]);
    expect(within(people).queryByRole("link")).toBeNull();
    expect(screen.queryByText(EMPTY_STATE_LINES.no_collaborators)).toBeNull();
  } finally {
    tab.restore();
  }
});

test("the collaborators say when Legion's record could not be read, and when nobody worked the issue", async () => {
  const unavailable = renderCollaborators({
    agents: [otherAgent("writer", "2026-10-26T01:00:00Z")],
    issue_key: "CORE-1",
    legion_registry: "unavailable",
    people: [],
  });
  try {
    await screen.findByText(STATE_LINES.legion_record_unreadable);
    expect(screen.queryByRole("region", { name: CONTROL_LABELS.collaborators_legion })).toBeNull();
    expect(
      sessionsIn(screen.getByRole("region", { name: CONTROL_LABELS.collaborators_others }))
    ).toEqual(["writer"]);

    // A read that fails once the list holds an answer keeps that answer: the failure and its
    // Retry are drawn above the list it held.
    unavailable.read.mockRejectedValueOnce(new Error("the collaborators' read failed"));
    await act(() =>
      unavailable.client.invalidateQueries({ queryKey: ["issue", "CORE-1", "collaborators"] })
    );
    const failure = await screen.findByRole("alert");
    expect(failure.textContent).toBe(ANSWER_LINES.collaborators_unreadable);
    const held = screen.queryByRole("region", { name: CONTROL_LABELS.collaborators_others });
    expect(
      held === null ? [] : sessionsIn(held),
      "a failed refetch blanked the held collaborators"
    ).toEqual(["writer"]);
    expect(
      held === null ? 0 : failure.compareDocumentPosition(held) & Node.DOCUMENT_POSITION_FOLLOWING,
      "the failure is not drawn above the list it held"
    ).not.toBe(0);
    // Its Retry goes with its line once the read lands, and the focus goes to the control that
    // opens the list rather than to the page.
    await userEvent.click(screen.getByRole("button", { name: CONTROL_LABELS.retry }));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect(document.activeElement, "the focus fell to the page with Retry").toBe(
      unavailable.owner.current
    );
  } finally {
    unavailable.restore();
  }

  const empty = renderCollaborators({
    agents: [],
    issue_key: "CORE-1",
    legion_registry: "ok",
    people: [],
  });
  try {
    await screen.findByText(EMPTY_STATE_LINES.no_collaborators);
    expect(screen.queryByText(STATE_LINES.legion_record_unreadable)).toBeNull();
    expect(empty.read).toHaveBeenCalledWith("CORE-1");
  } finally {
    empty.restore();
  }
});

// With the list held, a failed read of it keeps its line and its Retry drawn while that Retry
// reads it again, so the Retry is never disabled then: Chromium would drop its focus to the page
// (`chromium-focus-fixup.ts`), and a read that failed again would leave it there.
test("the collaborators' Retry pressed by keyboard over the list held keeps the focus when its read fails again", async () => {
  const list = renderCollaborators({
    agents: [otherAgent("writer", "2026-10-26T01:00:00Z")],
    issue_key: "CORE-1",
    legion_registry: "ok",
    people: [],
  });
  const restoreFocus = emulateChromiumFocusFixup();
  const key = ["issue", "CORE-1", "collaborators"];
  try {
    await screen.findByRole("region", { name: CONTROL_LABELS.collaborators_others });
    list.read.mockRejectedValueOnce(new Error("the collaborators' read failed"));
    await act(() => list.client.invalidateQueries({ queryKey: key }));
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    // The read the Retry sends is held out until the case fails it.
    const again = Promise.withResolvers<IssueCollaborators>();
    list.read.mockImplementationOnce(() => again.promise);

    retry.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(list.read).toHaveBeenCalledTimes(3));
    expect(
      document.activeElement,
      "the collaborators' Retry left the focus on the page while it read"
    ).toBe(retry);

    again.reject(new Error("the collaborators' read failed again"));
    await waitFor(() => expect(list.client.getQueryState(key)?.fetchStatus).toBe("idle"));
    expect(screen.getByRole("alert").textContent).toBe(ANSWER_LINES.collaborators_unreadable);
    expect(screen.getByRole("button", { name: CONTROL_LABELS.retry })).toBe(retry);
    expect(
      document.activeElement,
      "the collaborators' Retry whose read failed again left the focus on the page"
    ).toBe(retry);
    expect(
      sessionsIn(screen.getByRole("region", { name: CONTROL_LABELS.collaborators_others }))
    ).toEqual(["writer"]);
  } finally {
    restoreFocus();
    list.restore();
  }
});

// The collaborators' Retry showed nothing while the read it sent was out, and a read that failed
// again was said on the same node in the same words, so a screen reader said nothing of it and
// the reader could not tell the press did anything. Retry reads busy while its read is out,
// enabled and holding the focus, and each failure is drawn afresh while Retry stays the same
// button.
test("the collaborators' Retry reads busy while its read is out, and a read that fails again is said again", async () => {
  const list = renderCollaborators({
    agents: [otherAgent("writer", "2026-10-26T01:00:00Z")],
    issue_key: "CORE-1",
    legion_registry: "ok",
    people: [],
  });
  const restoreFocus = emulateChromiumFocusFixup();
  const key = ["issue", "CORE-1", "collaborators"];
  try {
    await screen.findByRole("region", { name: CONTROL_LABELS.collaborators_others });
    list.read.mockRejectedValueOnce(new Error("the collaborators' read failed"));
    await act(() => list.client.invalidateQueries({ queryKey: key }));
    const failed = await screen.findByRole("alert");
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
    // The read the Retry sends is held out until the case fails it.
    const again = Promise.withResolvers<IssueCollaborators>();
    list.read.mockImplementationOnce(() => again.promise);

    act(() => retry.focus());
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(list.read).toHaveBeenCalledTimes(3));
    await waitFor(() =>
      expect(retry, "Retry does not read busy while the read it sent again is out").toHaveAttribute(
        "aria-busy",
        "true"
      )
    );
    expect(retry, "Retry turned disabled while the read it sent again is out").toBeEnabled();
    expect(document.activeElement, "Retry lost the focus once it read busy").toBe(retry);

    await act(async () => again.reject(new Error("the collaborators' read failed")));
    await waitFor(() =>
      expect(retry, "Retry still reads busy after the read failed again").not.toHaveAttribute(
        "aria-busy"
      )
    );
    const failedAgain = screen.getByRole("alert");
    expect(failedAgain.textContent).toBe(ANSWER_LINES.collaborators_unreadable);
    expect(
      failedAgain,
      "the second failure was not drawn afresh, so a screen reader would not say it"
    ).not.toBe(failed);
    expect(
      screen.getByRole("button", { name: CONTROL_LABELS.retry }),
      "the line came back with another Retry"
    ).toBe(retry);
    expect(document.activeElement, "Retry lost the focus when the read failed again").toBe(retry);
  } finally {
    restoreFocus();
    list.restore();
  }
});

// A second press of Retry made before React draws the read the first one sent found the render's
// `isFetching` still false, so it read the list again: a refetch of a read that holds an answer
// ends the request that is out and sends another. A press while the read is out joins it.
test("two presses of the collaborators' Retry before a render send one read", async () => {
  const collaborators: IssueCollaborators = {
    agents: [otherAgent("writer", "2026-10-26T01:00:00Z")],
    issue_key: "CORE-1",
    legion_registry: "ok",
    people: [],
  };
  const list = renderCollaborators(collaborators);
  const key = ["issue", "CORE-1", "collaborators"];
  try {
    await screen.findByRole("region", { name: CONTROL_LABELS.collaborators_others });
    list.read.mockRejectedValueOnce(new Error("the collaborators' read failed"));
    await act(() => list.client.invalidateQueries({ queryKey: key }));
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    // Every read sent from here is held out until the case lets it land.
    const again = Promise.withResolvers<IssueCollaborators>();
    list.read.mockImplementation(() => again.promise);

    // Both presses in one task inside one `act`, so React draws nothing between them.
    act(() => {
      retry.click();
      retry.click();
    });
    expect(
      list.read,
      "the second press ended the read the first one sent and sent another"
    ).toHaveBeenCalledTimes(3);
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));

    await act(async () => again.resolve(collaborators));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect(list.read).toHaveBeenCalledTimes(3);
  } finally {
    list.restore();
  }
});

// A first read of the list that failed holds no answer, and a fetch of such a read clears its
// failure as it starts, so the press of its Retry took the line, and the Retry with it, away while
// the read it sent was out: the focus of a reader who pressed Retry by keyboard went with it. The
// line stays, its Retry busy and holding the focus, through a read that fails again, and once a
// read answers the focus goes to the control that opens the list.
test("a first read of the collaborators that failed keeps its line and its Retry, with the focus, while Retry reads again", async () => {
  const collaborators: IssueCollaborators = {
    agents: [otherAgent("writer", "2026-10-26T01:00:00Z")],
    issue_key: "CORE-1",
    legion_registry: "ok",
    people: [],
  };
  const list = renderCollaborators(collaborators, [], new Error("the collaborators' read failed"));
  const restoreFocus = emulateChromiumFocusFixup();
  try {
    const failed = await screen.findByRole("alert");
    expect(failed.textContent).toBe(ANSWER_LINES.collaborators_unreadable);
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
    // The read the Retry sends is held out until the case fails it.
    const again = Promise.withResolvers<IssueCollaborators>();
    list.read.mockImplementationOnce(() => again.promise);

    act(() => retry.focus());
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(list.read).toHaveBeenCalledTimes(2));
    await waitFor(() => {
      expect(
        retry,
        "the line and its Retry went while the read Retry sent again is out"
      ).toBeInTheDocument();
      expect(retry, "Retry does not read busy while the read it sent again is out").toHaveAttribute(
        "aria-busy",
        "true"
      );
    });
    expect(retry, "Retry turned disabled while the read it sent again is out").toBeEnabled();
    expect(document.activeElement, "Retry lost the focus while the read it sent again is out").toBe(
      retry
    );
    expect(
      screen.queryByText(ANSWER_LINES.loading_collaborators),
      "the list says it is loading beside the failure its Retry reads again"
    ).toBeNull();

    await act(async () => again.reject(new Error("the collaborators' read failed")));
    await waitFor(() =>
      expect(retry, "Retry still reads busy after the read failed again").not.toHaveAttribute(
        "aria-busy"
      )
    );
    const failedAgain = screen.getByRole("alert");
    expect(failedAgain.textContent).toBe(ANSWER_LINES.collaborators_unreadable);
    expect(
      failedAgain,
      "the second failure was not drawn afresh, so a screen reader would not say it"
    ).not.toBe(failed);
    expect(
      screen.getByRole("button", { name: CONTROL_LABELS.retry }),
      "the line came back with another Retry"
    ).toBe(retry);
    expect(document.activeElement, "Retry lost the focus when the read failed again").toBe(retry);

    await userEvent.keyboard("{Enter}");
    await screen.findByRole("region", { name: CONTROL_LABELS.collaborators_others });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(
      document.activeElement,
      "the list answered, and the focus fell to the page rather than to the control that opens it"
    ).toBe(list.owner.current);
  } finally {
    restoreFocus();
    list.restore();
  }
});

// How someone worked the issue reaches the page as the server's
// token - `assignee`, `claimed`, `posted`, `answered` - and the list draws the page's word for
// it, for a person and for an agent alike, so no reader is shown the token itself.
test("a collaborator's reasons are the page's words, never their tokens", async () => {
  const words = CONTROL_LABELS.collaborator_reasons;
  // A word that is its own token would let a list that draws the token pass this case.
  expect([words.claimed, words.posted]).not.toEqual(["claimed", "posted"]);
  const list = renderCollaborators({
    agents: [otherAgent("writer", null)],
    issue_key: "CORE-1",
    legion_registry: "ok",
    people: [{ kind: "user", last_at: null, login: "carol", reasons: ["claimed", "posted"] }],
  });
  /** The line under each row's name that says how that one worked the issue. */
  const reasonsIn = (region: HTMLElement) =>
    [...region.querySelectorAll("[data-collaborator]")].map(
      (name) => name.nextElementSibling?.textContent ?? ""
    );
  try {
    const people = await screen.findByRole("region", { name: CONTROL_LABELS.collaborators_people });
    expect(reasonsIn(people)).toEqual([`${words.claimed}, ${words.posted}`]);
    expect(
      reasonsIn(screen.getByRole("region", { name: CONTROL_LABELS.collaborators_others }))
    ).toEqual([words.posted]);
    expect(reasonsIn(people).flatMap((line) => line.split(", "))).not.toContain("posted");
  } finally {
    list.restore();
  }
});

// The grouping itself, without a DOM: each section holds the server's list in the order it sent
// it, and a role nobody worked has no section at all.
test("collaboratorSections groups the server's list into the three sections the panel draws", () => {
  const sections = collaboratorSections({
    agents: [
      legionAgent("new-planner", "planner", "2026-10-26T02:00:00Z"),
      legionAgent("old-planner", "planner", "2026-10-26T00:00:00Z", "new-planner"),
      legionAgent("tester", "tester", "2026-10-26T03:00:00Z"),
      otherAgent("loud", "2026-10-26T04:00:00Z"),
      otherAgent("quiet", null),
    ],
    issue_key: "CORE-1",
    legion_registry: "ok",
    people: [{ kind: "user", last_at: null, login: "alice", reasons: ["assignee"] }],
  });

  expect(
    sections.legion.map(({ agents, role }) => [role, agents.map((agent) => agent.session_id)])
  ).toEqual([
    ["planner", ["new-planner", "old-planner"]],
    ["tester", ["tester"]],
  ]);
  expect(sections.others.map((agent) => agent.session_id)).toEqual(["loud", "quiet"]);
  expect(sections.people.map((person) => person.login)).toEqual(["alice"]);
});

// What the page does with a registry this build does not understand: a role it has no section
// for, and a start time no reader here parses. Both come from the Legion daemon through the
// listener, so neither is this app's to guarantee, and neither may lose a row.
test("collaboratorSections keeps an agent whose role or timestamps it cannot read", () => {
  const skewed = {
    ...legionAgent("future-role", "tester", "2026-10-26T00:00:00Z"),
    legion: {
      replaced_at: null,
      replaced_by: null,
      role: "archivist" as LegionRole,
      role_token: "legion-archivist",
      started_at: "2026-10-26T00:00:00Z",
    },
  };
  const sections = collaboratorSections({
    agents: [
      skewed,
      legionAgent("early", "tester", "2026-10-26T01:00:00Z"),
      { ...legionAgent("unreadable", "tester", "yesterday"), last_at: null },
    ],
    issue_key: "CORE-1",
    legion_registry: "ok",
    people: [],
  });

  // The unknown role is listed rather than dropped, and the session whose start this build
  // cannot read keeps the place the server gave it.
  expect(sections.others.map((agent) => agent.session_id)).toEqual(["future-role"]);
  expect(
    sections.legion.map(({ agents, role }) => [role, agents.map((a) => a.session_id)])
  ).toEqual([["tester", ["early", "unreadable"]]]);
});
