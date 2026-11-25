import { afterEach, beforeEach, expect, jest, spyOn, test } from "bun:test";
import { useQuery } from "@tanstack/react-query";
import { act, cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";

import { realClock, tick } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { renderWithQueries } from "../../__tests__/query-provider";
import { answerRoute } from "../../__tests__/send-answers";
import { ApiError, api, type WriteBounds } from "../../api/client";
import { goingPlanQuery } from "../../api/queries";
import type { GoingPlanAnswer, GoingPlanRow, GoingRun, GoingUndoAnswer } from "../../api/types";
import { borderStrong, linkText, quoteAccentBorder, textMutedOnSurface } from "../../theme/classes";
import { PriorityEditor } from "../issue/PriorityControl";
import { issue as issueFixture } from "../margin/margin-fixture";
import { clockWords } from "../search/blocks/block-cells";
import {
  goingState,
  newConversation,
  openRun,
  resetGoingStore,
  setPlace,
  startRun,
} from "./going-store";
import { PlannedChanges } from "./PlannedChanges";
import { PlanCard, usePlanCardKeys } from "./PlanRows";
import { BoxRow } from "./RunControls";
import { useAsk } from "./useAsk";
import { useOpenPlan } from "./usePlan";

// The plan card of a plan that waits (design v3, state 6; spec R4, R5, R7): its title made from
// its rows and its approval mode linked to its setting; each kind of row in the issue page's words
// - a change of fields, a new issue, a message, a row that takes something out, a row whose item
// changed - the page's own priority menu and label picker to edit a value; why a row waits in
// Auto-review; Approve, which counts the ticked rows that wait, and Discard; and the card's keys.

function row(number: number, extra: Partial<GoingPlanRow>): GoingPlanRow {
  return {
    arguments: {},
    changed: false,
    claim: null,
    class: "field",
    fields: [],
    issue: "",
    item: "",
    needs: [],
    now: null,
    result: null,
    row: number,
    state: "waiting",
    tool: "dispatch_issue_update",
    waits: [],
    ...extra,
  };
}

/** A change of DEMO-10's labels and priority: one label added, and a priority set. */
const labelsAndPriority = row(0, {
  arguments: { issue: "DEMO-10", labels: ["board", "accessibility"], priority: 2 },
  fields: ["labels", "priority"],
  issue: "DEMO-10",
  item: "DEMO-10",
  now: { labels: ["board"], priority: null },
});

function planOf(rows: GoingPlanRow[], extra: Partial<GoingPlanAnswer> = {}): GoingPlanAnswer {
  return {
    mode: "ask",
    plan: 1,
    rows,
    run: "run-1",
    state: "waiting",
    summary: "",
    version: "v1",
    ...extra,
  };
}

const created: GoingRun = {
  context: null,
  continues: null,
  conversation: "run-1",
  created_at: new Date().toISOString(),
  finished_at: null,
  id: "run-1",
  issues: [],
  level: "standard",
  line: null,
  mode: "ask",
  standing_count: 0,
  state: "queued",
  stop_reason: null,
  undoable: false,
  undoable_count: 0,
  undone_at: null,
  undone_count: 0,
  used: { calls: 0, tokens: 0, working_ms: 0 },
  words: "tidy DEMO-10",
};

/** Starts the conversation's run, whose stream carries its plan frame and then stays open while
 *  the plan waits, and shows the conversation over the page. The plan is read as `plan`, and the
 *  run's record is `started`, which says what Undo can take back of it. */
async function openPlan(plan: GoingPlanAnswer, started: GoingRun = created): Promise<() => void> {
  const start = spyOn(api, "createGoingRun").mockResolvedValue(started);
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    onFrame("plan", { plan: 1, state: "waiting" });
    return Promise.withResolvers<void>().promise;
  });
  const read = spyOn(api, "goingPlan").mockImplementation(async () => plan);
  await startRun("tidy DEMO-10", { path: "/projects/DEMO/issues" }, "alice");
  setPlace("over");
  return () => {
    start.mockRestore();
    stream.mockRestore();
    read.mockRestore();
  };
}

function Card(): ReactNode {
  return <PlanCard answering={useOpenPlan()} />;
}

/** The bar's box, as far as the card's keys go: ↑ in it, empty, moves the focus into the card. */
function Box(): ReactNode {
  const keys = usePlanCardKeys();
  const [words, setWords] = useState("");
  return (
    <input
      aria-label="Say what to change"
      onChange={(event) => setWords(event.target.value)}
      onKeyDown={(event) => {
        keys(event);
      }}
      value={words}
    />
  );
}

/** The card's row numbered `number`. */
function drawnRow(number: number): HTMLElement {
  const found = within(screen.getByTestId("going-plan-card-rows"))
    .getAllByTestId("going-plan-row")
    .find((drawn) => drawn.getAttribute("data-row") === String(number));
  if (found === undefined) throw new Error(`the card draws no row ${number}`);
  return found;
}

/** The line of `field` in `drawn`. */
function fieldOf(drawn: HTMLElement, field: string): HTMLElement {
  const found = within(drawn)
    .getAllByTestId("going-plan-field")
    .find((line) => line.getAttribute("data-field") === field);
  if (found === undefined) throw new Error(`the row draws no ${field}`);
  return found;
}

/** The words `element` shows: its text without the options of a menu, which show only once the
 *  menu is opened, with its spaces run together. */
function shownText(element: HTMLElement): string {
  const copy = element.cloneNode(true) as HTMLElement;
  for (const menu of copy.querySelectorAll("select")) menu.remove();
  return (copy.textContent ?? "").replace(/\s+/gu, " ").trim();
}

// Each change row's item is an issue chip, which reads its issue's summary: the one read every
// reference to the issue shares. A message on an issue reads the issue itself, for whom Dispatch
// delivers it to: by default an issue that routes its messages nowhere and that nobody claims.
let summaries: { mockRestore: () => void } | undefined;
let issues: { mockRestore: () => void } | undefined;
// A message on an issue also reads the sessions that follow the issue: by default none. And a
// run's record is read again at each frame after which rows of its plan may have run
// (`rereadRun`): unless a row answers that read itself, Dispatch answers the record the page holds.
let followers: { mockRestore: () => void } | undefined;
let rereads: { mockRestore: () => void } | undefined;
beforeEach(() => {
  summaries = spyOn(api, "getIssueSummary").mockImplementation(async (key: string) => ({
    documents: [],
    key,
    project: "DEMO",
    status: "in_progress",
    title: `The issue ${key}`,
  }));
  issues = spyOn(api, "getIssue").mockImplementation(async (key: string) => ({
    ...issueFixture,
    claim: null,
    key,
    route: null,
  }));
  followers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  rereads = spyOn(api, "goingRun").mockImplementation(async (id) => {
    const held = goingState().runs.get(id)?.record;
    if (held === undefined) throw new Error(`the page holds no run ${id}`);
    return held;
  });
});

afterEach(() => {
  cleanup();
  resetGoingStore();
  summaries?.mockRestore();
  issues?.mockRestore();
  followers?.mockRestore();
  rereads?.mockRestore();
});

test("a change row names its item and each field in the page's words, the values as the page shows them", async () => {
  const restore = await openPlan(planOf([labelsAndPriority]));
  try {
    renderWithQueries(<Card />);
    const card = await screen.findByTestId("going-plan");
    expect(within(card).getByTestId("going-plan-title")).toHaveTextContent("Change DEMO-10");
    const mode = within(card).getByTestId("going-plan-mode");
    expect(mode).toHaveTextContent("Ask");
    expect(mode).toHaveAttribute("href", "/settings#command-bar-heading");

    const drawn = drawnRow(0);
    // The item is the issue's chip, as an answer names it: its key, its status and its title.
    const chip = within(within(drawn).getByTestId("going-plan-item")).getByTestId("item-chip");
    expect(chip).toHaveTextContent("DEMO-10");
    await waitFor(() => expect(chip).toHaveTextContent("DEMO-10In progressThe issue DEMO-10"));
    // The chip takes the room the row has: no width of its own cuts its title, and the fields
    // go under it where the row has no room for both.
    expect(within(drawn).getByTestId("going-plan-item").className).not.toMatch(/max-w-/u);
    const priority = fieldOf(drawn, "priority");
    expect(priority).toHaveTextContent(/^Priority: No priority → P2/u);
    const now = within(priority).getByTestId("going-plan-now");
    expect(now).toHaveTextContent("No priority");
    // The value now is the bar's one grey, never the colour of a link.
    expect(now.className).toContain(textMutedOnSurface);
    expect(now.className).not.toContain(linkText);
    // The new priority is the badge the issue page draws a priority in, the same one everywhere
    // the bar shows one (review a5).
    const issuePage = renderWithQueries(
      <PriorityEditor
        issueKey="DEMO-10"
        write={{
          failed: false,
          pending: false,
          retry: undefined,
          shown: 2,
          stopped: undefined,
          submit: () => {},
        }}
      />
    );
    const pageBadge = within(issuePage.container).getByText("P2", { selector: "span" });
    expect(within(within(priority).getByTestId("going-plan-next")).getByText("P2").className).toBe(
      pageBadge.className
    );
    issuePage.unmount();
    const labels = fieldOf(drawn, "labels");
    expect(labels).toHaveTextContent(/^Labels: board →/u);
    // The label added is the one marked in green; the label kept is the page's plain chip.
    const added = labels.querySelector('[data-mark="added"]');
    expect(added?.textContent).toBe("added accessibility");
    expect(labels.querySelector('[data-mark="removed"]')).toBeNull();
    // No raw field name and no JSON.
    expect(drawn.textContent ?? "").not.toMatch(/[[\]{}"]|priority none|labels \[/u);
    expect(drawn).not.toHaveAttribute("data-removes");
  } finally {
    restore();
  }
});

// A change of a document's text names the issue whose document it changes by that issue's chip,
// as a change row names its item: in place of the issue's key where the row's words write it,
// and after them where they do not (design v3, state 6).
test("a document row names the issue whose document it changes by its chip", async () => {
  const restore = await openPlan(
    planOf([
      row(0, {
        arguments: {
          artifact: "spec",
          body: "The dome turns by motor once it has one.",
          issue: "DEMO-15",
          quote: "The dome turns by hand.",
          replace_with: "The dome turns by motor.",
        },
        class: "suggestion",
        issue: "DEMO-15",
        item: "8c1f0a52-4d1e-4f7b-9a43-0d6c1b2e7f10",
        tool: "dispatch_suggest",
      }),
      row(1, {
        arguments: { content: "The split moves DEMO-3.\n", issue: "DEMO-16", name: "design.md" },
        class: "create_text",
        issue: "DEMO-16",
        item: "design.md",
        tool: "dispatch_artifact",
      }),
    ])
  );
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    const suggestion = within(drawnRow(0)).getByTestId("going-plan-document");
    const chip = within(suggestion).getByTestId("item-chip");
    await waitFor(() => expect(chip).toHaveTextContent("DEMO-15In progressThe issue DEMO-15"));
    expect(suggestion).toHaveTextContent(
      /^Suggest a change to the spec of DEMO-15In progressThe issue DEMO-15$/u
    );
    const upload = within(drawnRow(1)).getByTestId("going-plan-document");
    await waitFor(() =>
      expect(upload).toHaveTextContent(
        /^Upload design\.md to DEMO-16In progressThe issue DEMO-16$/u
      )
    );
    expect(within(upload).getAllByTestId("item-chip")).toHaveLength(1);
  } finally {
    restore();
  }
});

test("the priority menu and the label picker edit a value, the row says Edited, and Approve carries the values", async () => {
  const user = userEvent.setup();
  const restore = await openPlan(planOf([labelsAndPriority]));
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const approve = answerRoute<
    (run: string, body: unknown, bounds: WriteBounds) => Promise<GoingPlanAnswer>
  >("POST", "/api/v1/going/runs/*/plan/approve");
  approve.mockImplementation(async () => planOf([labelsAndPriority], { state: "running" }));
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    expect(within(drawnRow(0)).queryByTestId("going-plan-edited")).toBeNull();

    // Each line has one Edit, whatever its field; it swaps the value for the issue page's own
    // control, here its priority menu.
    expect(screen.queryByRole("combobox", { name: "Priority of DEMO-10" })).toBeNull();
    await user.click(
      within(fieldOf(drawnRow(0), "priority")).getByRole("button", { name: "Edit priority" })
    );
    await user.selectOptions(screen.getByRole("combobox", { name: "Priority of DEMO-10" }), "1");
    const priority = fieldOf(drawnRow(0), "priority");
    expect(within(priority).getByTestId("going-plan-edited")).toHaveTextContent("Edited");
    expect(priority).toHaveTextContent(/^Priority: No priority → P1/u);
    // The pick puts the value back in its words.
    expect(screen.queryByRole("combobox", { name: "Priority of DEMO-10" })).toBeNull();

    // The labels' Edit opens the issue page's label picker over them.
    await user.click(
      within(fieldOf(drawnRow(0), "labels")).getByRole("button", { name: "Edit labels" })
    );
    const search = await screen.findByRole("combobox", { name: "Search or create label" });
    await user.type(search, "urgent{Enter}");
    await user.keyboard("{Escape}");
    const labels = await waitFor(() => {
      const line = fieldOf(drawnRow(0), "labels");
      expect(within(line).getByTestId("going-plan-edited")).toBeInTheDocument();
      return line;
    });
    expect(
      [...labels.querySelectorAll('[data-mark="added"]')].map((chip) => chip.textContent)
    ).toEqual(["added accessibility", "added urgent"]);

    await user.click(screen.getByTestId("going-plan-approve"));
    await waitFor(() =>
      expect(approve).toHaveBeenCalledWith(
        "run-1",
        {
          plan: 1,
          rows: [
            {
              row: 0,
              tick: true,
              values: { labels: ["board", "accessibility", "urgent"], priority: 1 },
            },
          ],
          version: "v1",
        },
        keyedAttempt
      )
    );
  } finally {
    listIssues.mockRestore();
    restore();
  }
});

/** The card under the bar's box, whose Enter in the empty box answers the plan as the bar's box
 *  does (`useAsk`); the box says whether its last Enter took its words, as the bar reads it. */
function AskedCard(): ReactNode {
  const asking = useAsk({ path: "/projects/DEMO/issues" });
  const [took, setTook] = useState<boolean>();
  return (
    <>
      <PlanCard answering={asking.answering} />
      <input
        aria-label="Say what to change"
        data-took={took === undefined ? undefined : String(took)}
        onKeyDown={(event) => {
          if (event.key === "Enter") setTook(asking.enter(""));
        }}
      />
    </>
  );
}

// Enter in the empty box stands for the card's Approve, ↵ drawn on it, and keeps its guard: with
// no row ticked it approves nothing, as Approve is off, and while the first approval is out a
// second Enter sends none, which Dispatch would refuse as a plan that no longer waits.
test("Enter approves only where Approve is on: not with no row ticked, and not again while the approval is out", async () => {
  const user = userEvent.setup();
  const restore = await openPlan(planOf([labelsAndPriority]));
  const approve = answerRoute<
    (run: string, body: unknown, bounds: WriteBounds) => Promise<GoingPlanAnswer>
  >("POST", "/api/v1/going/runs/*/plan/approve");
  const approved = Promise.withResolvers<GoingPlanAnswer>();
  approve.mockImplementation(() => approved.promise);
  try {
    renderWithQueries(<AskedCard />);
    await screen.findByTestId("going-plan-card-rows");
    const box = screen.getByRole("textbox", { name: "Say what to change" });
    await user.click(within(drawnRow(0)).getByRole("checkbox"));
    expect(screen.getByTestId("going-plan-approve")).toBeDisabled();
    await user.type(box, "{Enter}");
    expect(box).toHaveAttribute("data-took", "false");

    await user.click(within(drawnRow(0)).getByRole("checkbox"));
    await user.type(box, "{Enter}");
    expect(box).toHaveAttribute("data-took", "true");
    // The first approval is the one Enter sent with the row ticked: the Enter before it sent none.
    await waitFor(() => expect(approve).toHaveBeenCalledTimes(1));
    expect(approve.mock.calls[0]?.[1]).toEqual({
      plan: 1,
      rows: [{ row: 0, tick: true }],
      version: "v1",
    });
    await waitFor(() => expect(screen.getByTestId("going-plan-approve")).toBeDisabled());
    await user.type(box, "{Enter}");
    expect(box).toHaveAttribute("data-took", "false");

    approved.resolve(planOf([{ ...labelsAndPriority, state: "running" }], { state: "running" }));
    await waitFor(() => expect(screen.queryByTestId("going-plan-approve")).toBeNull());
    expect(approve).toHaveBeenCalledTimes(1);
  } finally {
    restore();
  }
});

test("a row that adds a label shows the labels now with the one it adds, and takes nothing out", async () => {
  const restore = await openPlan(
    planOf([
      row(0, {
        arguments: { issue: "DEMO-10", labels_add: ["accessibility"], priority: 2 },
        fields: ["labels", "priority"],
        issue: "DEMO-10",
        item: "DEMO-10",
        now: { labels: ["board"], priority: null },
      }),
    ])
  );
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    const labels = fieldOf(drawnRow(0), "labels");
    expect(labels).toHaveTextContent(/^Labels: board → board\s*added accessibility/u);
    expect(within(labels).getByTestId("going-plan-now")).toHaveTextContent(/^board$/u);
    expect(
      [...labels.querySelectorAll('[data-mark="added"]')].map((chip) => chip.textContent)
    ).toEqual(["added accessibility"]);
    expect(labels.querySelector('[data-mark="removed"]')).toBeNull();
    expect(drawnRow(0)).not.toHaveAttribute("data-removes");
    expect(within(drawnRow(0)).queryByTestId("going-plan-removes")).toBeNull();
  } finally {
    restore();
  }
});

test("a row that closes or removes something is marked in red, and a removed label is struck out", async () => {
  const restore = await openPlan(
    planOf([
      row(0, {
        arguments: { issue: "DEMO-1", labels: ["board"] },
        fields: ["labels"],
        issue: "DEMO-1",
        item: "DEMO-1",
        now: { labels: ["board", "a11y"] },
      }),
      row(1, {
        arguments: { issue: "DEMO-2", reason: "Done.", status: "done" },
        fields: ["status"],
        issue: "DEMO-2",
        item: "DEMO-2",
        now: { status: "in_progress" },
      }),
      row(2, {
        arguments: { issue: "DEMO-3", release: true },
        class: "claim",
        fields: ["claim"],
        issue: "DEMO-3",
        item: "DEMO-3",
        now: { id: "alice", kind: "user" },
        tool: "dispatch_claim",
      }),
      row(3, {
        arguments: { issue: "DEMO-4", status: "in_progress" },
        fields: ["status"],
        issue: "DEMO-4",
        item: "DEMO-4",
        now: { status: "todo" },
      }),
    ])
  );
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    expect(drawnRow(0)).toHaveAttribute("data-removes", "true");
    // The words name each value the row takes out, as the struck-out chip shows it.
    expect(within(drawnRow(0)).getByTestId("going-plan-removes")).toHaveTextContent(
      /^Removes a11y$/u
    );
    expect(drawnRow(0).querySelector('[data-mark="removed"]')?.textContent).toBe("removed a11y");
    expect(drawnRow(1)).toHaveAttribute("data-removes", "true");
    // A close says nothing more than its red mark and its status line: no lone "Closes" under
    // them (review a1).
    expect(within(drawnRow(1)).queryByTestId("going-plan-removes")).toBeNull();
    // The new status reads as its word, as a priority does, with one Edit that opens the issue
    // page's own status menu, set to the status the row writes.
    expect(within(fieldOf(drawnRow(1), "status")).getByTestId("going-plan-next")).toHaveTextContent(
      /^Done$/u
    );
    expect(screen.queryByRole("combobox", { name: "Status of DEMO-2" })).toBeNull();
    await userEvent
      .setup()
      .click(within(fieldOf(drawnRow(1), "status")).getByRole("button", { name: "Edit status" }));
    expect(screen.getByRole("combobox", { name: "Status of DEMO-2" })).toHaveValue("done");
    expect(drawnRow(2)).toHaveAttribute("data-removes", "true");
    expect(fieldOf(drawnRow(2), "claim")).toHaveTextContent(/^Claimed by: alice → Nobody$/u);
    expect(drawnRow(3)).not.toHaveAttribute("data-removes");
    expect(screen.getByTestId("going-plan-title")).toHaveTextContent(
      "Change 3 issues and close DEMO-2"
    );
  } finally {
    restore();
  }
});

test("a new issue row shows New, its title, its fields on one line and its spec in sentences, and a later row names it with (new)", async () => {
  const spec =
    "# Split\n\nThe dome splits in two. Each half turns alone. The motor stays. Nothing else moves.";
  const parentOf = (number: number, key: string, parent: string) =>
    row(number, {
      arguments: { issue: key, parent },
      fields: ["parent"],
      issue: key,
      item: key,
      needs: parent.startsWith("$") ? [Number(parent.slice(1))] : [],
      now: { parent: "DEMO-1" },
    });
  const restore = await openPlan(
    planOf([
      row(0, {
        arguments: {
          labels: ["board"],
          parent: "DEMO-12",
          priority: 2,
          project: "DEMO",
          spec,
          title: "North half",
        },
        class: "create_text",
        tool: "dispatch_issue",
      }),
      row(1, {
        arguments: { project: "DEMO", title: "South half" },
        class: "create_plain",
        tool: "dispatch_issue",
      }),
      parentOf(2, "DEMO-3", "$0"),
      parentOf(3, "DEMO-4", "$1"),
      parentOf(4, "DEMO-5", "DEMO-12"),
    ])
  );
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    expect(screen.getByTestId("going-plan-title")).toHaveTextContent("File 2 issues and move 3");
    const made = drawnRow(0);
    expect(made).toHaveAttribute("data-kind", "new_issue");
    expect(made).toHaveTextContent(/^New/u);
    // A new issue has no key yet: it keeps its New chip, and no issue's chip.
    expect(within(made).queryByTestId("item-chip")).toBeNull();
    expect(within(fieldOf(made, "title")).getByTestId("going-plan-next")).toHaveTextContent(
      "North half"
    );
    expect(shownText(within(made).getByTestId("going-plan-new-issue-fields"))).toBe(
      "DEMO · under DEMO-12 · P2 · board"
    );
    expect(within(made).getByTestId("going-plan-spec-sentences")).toHaveTextContent(
      "The dome splits in two. Each half turns alone. The motor stays."
    );
    const whole = within(made).getByTestId("going-plan-spec");
    expect(whole).not.toHaveAttribute("open");
    expect(whole).toHaveTextContent("Nothing else moves.");
    expect(within(fieldOf(drawnRow(2), "parent")).getByTestId("going-plan-next")).toHaveTextContent(
      "North half (new)"
    );
  } finally {
    restore();
  }
});

// A message on an issue's conversation names whom Dispatch delivers it to, in the form a message
// to a session and a broadcast have (review m5): the session or the role the issue sends its
// messages to, the session that holds its claim and the sessions that follow it, read from the
// issue and its subscribers (`eventDestinations`, outbox/publisher.go) - one by its name, more by
// their count with the sessions under it, and "no session" where the page read them all and the
// server finds none either. Where the page cannot read who follows the issue, or the server finds
// a session the page does not know of, the row says whom Dispatch tells of it in words.
test("message rows say whom they reach and how, show the whole text, and say they cannot be undone", async () => {
  const read = spyOn(api, "getIssue").mockImplementation(async (key: string) => {
    const unrouted = { ...issueFixture, claim: null, key, route: null, route_holder: null };
    if (key === "DEMO-2") {
      return {
        ...unrouted,
        claim: { actor: { id: "s-9", kind: "session" }, at: new Date().toISOString() },
        route: "role:planner",
      };
    }
    return key === "DEMO-3" || key === "DEMO-4" ? { ...unrouted, route: "session:s-1" } : unrouted;
  });
  const followed = spyOn(api, "getIssueSubscribers").mockImplementation(async (key: string) => {
    if (key === "DEMO-4" || key === "DEMO-6") {
      throw new ApiError(503, { code: "ENVOY_UNAVAILABLE", error: "the listener did not answer" });
    }
    // The session DEMO-3 routes to follows it too, and is one session of the three.
    return key === "DEMO-3"
      ? ["s-1", "s-2", "s-3"].map((session) => ({
          last_seen: 0,
          live: true,
          removable: true,
          session_id: session,
          title: session,
          topics: [],
        }))
      : [];
  });
  const onIssue = (number: number, issue: string, body: string, waits = ["message"]) =>
    row(number, {
      arguments: { body, issue },
      class: "message",
      issue,
      item: issue,
      tool: "dispatch_message",
      waits,
    });
  const restore = await openPlan(
    planOf([
      row(0, {
        arguments: {
          body: "The rota is in the spec now.",
          delivery: "btw",
          session: "019a1c01-0d7b-7a2a-8e93-4a5b6c7d8e01",
        },
        class: "message",
        issue: "DEMO-15",
        tool: "dispatch_send_to_agent",
        waits: ["message"],
      }),
      row(1, {
        arguments: {
          body: "The deploy is paused until 14:00.",
          delivery: "aside",
          session_ids: ["s-1", "s-2", "s-3", "s-4"],
        },
        class: "broadcast",
        tool: "dispatch_broadcast",
        waits: ["broadcast"],
      }),
      onIssue(2, "DEMO-1", "Please review the split."),
      onIssue(3, "DEMO-2", "The split is yours."),
      onIssue(4, "DEMO-3", "The split is done."),
      onIssue(5, "DEMO-4", "The split waits."),
      // The server finds a session the event reaches, as a reply reaches the session that wrote
      // the message it answers, which the page does not read.
      onIssue(6, "DEMO-5", "Thanks, that answers it.", ["message", "delivers_to_session"]),
      onIssue(7, "DEMO-6", "The split is late."),
    ])
  );
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    expect(screen.getByTestId("going-plan-title")).toHaveTextContent("Send 8 messages");
    const toSession = drawnRow(0);
    // The issue a message is on is that issue's chip, as an answer names it.
    const toSessionLine = within(toSession).getByTestId("going-plan-message");
    const chip = within(toSessionLine).getByTestId("item-chip");
    await waitFor(() => expect(chip).toHaveTextContent("DEMO-15In progressThe issue DEMO-15"));
    expect(toSessionLine).toHaveTextContent(
      /^Message to .+ on DEMO-15In progressThe issue DEMO-15 · as a BTW$/u
    );
    expect(within(toSession).getByTestId("going-plan-quote")).toHaveTextContent(
      "The rota is in the spec now."
    );
    const broadcast = drawnRow(1);
    expect(within(broadcast).getByTestId("going-plan-message")).toHaveTextContent(
      "Message to 4 sessions · as an aside"
    );
    expect(
      within(within(broadcast).getByTestId("going-plan-sessions")).getAllByRole("listitem")
    ).toHaveLength(4);
    // No route, no claim and nobody who follows it, and the server finds no session either: the
    // row says so in the same form, and names no delivery, as a message on an issue has none.
    await waitFor(() => expect(followed).toHaveBeenCalledWith("DEMO-1"));
    await waitFor(() =>
      expect(within(drawnRow(2)).getByTestId("going-plan-message")).toHaveTextContent(
        /^Message to no session on DEMO-1In progressThe issue DEMO-1$/u
      )
    );
    expect(within(drawnRow(2)).queryByTestId("going-plan-sessions")).toBeNull();
    // The role it routes to, and the session that holds its claim.
    await waitFor(() =>
      expect(within(drawnRow(3)).getByTestId("going-plan-message")).toHaveTextContent(
        /^Message to the planner and .+ on DEMO-2/u
      )
    );
    // Three sessions in all - the one it routes to among those that follow it - by their count,
    // with the sessions under it, as a broadcast names them.
    await waitFor(() =>
      expect(within(drawnRow(4)).getByTestId("going-plan-message")).toHaveTextContent(
        /^Message to 3 sessions on DEMO-3/u
      )
    );
    expect(
      within(within(drawnRow(4)).getByTestId("going-plan-sessions")).getAllByRole("listitem")
    ).toHaveLength(3);
    // Who follows DEMO-4 cannot be read: the session it routes to, then the rest in words.
    await waitFor(() =>
      expect(within(drawnRow(5)).getByTestId("going-plan-message")).toHaveTextContent(
        /^Message to .+ and the agents that follow DEMO-4/u
      )
    );
    // The page finds nobody, but the server finds a session: whom, the page does not know.
    await waitFor(() =>
      expect(within(drawnRow(6)).getByTestId("going-plan-message")).toHaveTextContent(
        /^Message to the agents that follow DEMO-5In progressThe issue DEMO-5$/u
      )
    );
    // Nothing routes or claims DEMO-6, and who follows it cannot be read.
    await waitFor(() =>
      expect(within(drawnRow(7)).getByTestId("going-plan-message")).toHaveTextContent(
        /^Message to the agents that follow DEMO-6In progressThe issue DEMO-6$/u
      )
    );
    for (const number of [0, 1, 2, 3, 4, 5, 6, 7]) {
      expect(within(drawnRow(number)).getByTestId("going-plan-final")).toHaveTextContent(
        "Cannot be undone once sent"
      );
      // In Ask every row waits, and no row says why.
      expect(within(drawnRow(number)).queryByTestId("going-plan-row-state")).toBeNull();
    }
  } finally {
    restore();
    read.mockRestore();
    followed.mockRestore();
  }
});

/** An edit or a delete of a comment on `issue`, whose author is `author`, as the plan read it:
 *  its body while it stands, the delete's mark and no body once it is deleted (`deleted`). */
function postRow(
  number: number,
  tool: "dispatch_edit_comment" | "dispatch_delete_comment",
  issue: string,
  author: string,
  read: { readonly body: string } | { readonly deleted: true },
  args: Record<string, unknown> = {}
): GoingPlanRow {
  const comment = `c-${number}`;
  const deleted = "deleted" in read;
  return row(number, {
    arguments: { comment, ...args },
    class: "text",
    fields: ["body"],
    issue,
    item: comment,
    now: deleted
      ? {
          author: { id: author, kind: "user" },
          deleted_at: "2026-11-24T15:00:00Z",
          deleted_by: { id: author, kind: "user" },
        }
      : { author: { id: author, kind: "user" }, body: read.body },
    tool,
    ...(deleted ? { deleted: true } : {}),
  });
}

// An edit and a delete of a comment (R4, R6): the row names what it does to whose post on which
// issue, by the issue's chip; an edit's body is drawn once, as an inline diff with the words it
// changes marked and no "Body:" label (review a3), with Edit, as every change of a field has; a
// delete's body shows whole, struck through under a grey bar, and the row says that a delete
// cannot be undone. A comment deleted after the plan read it shows no word of it - nor the words
// the edit would put there - says it was deleted, and has no tick: Approve leaves it out.
test("post rows name the author and the issue, show an edit's body as an inline diff and a delete's struck through, and a post deleted since shows no body and no tick", async () => {
  const user = userEvent.setup();
  const restore = await openPlan(
    planOf([
      postRow(
        0,
        "dispatch_edit_comment",
        "DEMO-15",
        "alice",
        { body: "The window is 24 hours." },
        { body: "The window is 72 hours." }
      ),
      postRow(1, "dispatch_delete_comment", "DEMO-16", "bob", { body: "Ship it Friday." }),
      postRow(
        2,
        "dispatch_edit_comment",
        "DEMO-17",
        "carol",
        { deleted: true },
        { body: "Ship it Monday, not Friday." }
      ),
    ])
  );
  const approve = answerRoute<
    (run: string, body: unknown, bounds: WriteBounds) => Promise<GoingPlanAnswer>
  >("POST", "/api/v1/going/runs/*/plan/approve");
  approve.mockImplementation(async () => planOf([], { state: "running" }));
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    expect(screen.getByTestId("going-plan-title")).toHaveTextContent(
      "Edit 2 comments and delete 1"
    );

    const edit = drawnRow(0);
    expect(edit).toHaveAttribute("data-kind", "post");
    const editLine = within(edit).getByTestId("going-plan-post");
    await waitFor(() =>
      expect(editLine).toHaveTextContent(
        /^Edit the comment by alice on DEMO-15In progressThe issue DEMO-15$/u
      )
    );
    expect(within(editLine).getByTestId("item-chip")).toHaveTextContent("DEMO-15");
    const body = fieldOf(edit, "body");
    expect(body.textContent ?? "").not.toMatch(/Body/u);
    // The body once, the one word it changes struck through and the one it writes marked.
    expect(within(body).getByTestId("going-plan-body-diff")).toHaveTextContent(
      /^The window is\s*2472\s*hours\.$/u
    );
    expect(body.querySelector("del")?.textContent).toBe("24");
    expect(body.querySelector("ins")?.textContent).toBe("72");
    expect(within(body).getByRole("button", { name: "Edit body" })).toBeInTheDocument();
    expect(
      within(edit).getByRole("checkbox", { name: "Edit the comment on DEMO-15" })
    ).toBeChecked();
    expect(within(edit).queryByTestId("going-plan-final")).toBeNull();

    const remove = drawnRow(1);
    await waitFor(() =>
      expect(within(remove).getByTestId("going-plan-post")).toHaveTextContent(
        /^Delete the comment by bob on DEMO-16In progressThe issue DEMO-16$/u
      )
    );
    const quote = within(remove).getByTestId("going-plan-quote");
    expect(quote.querySelector("del")?.textContent).toBe("Ship it Friday.");
    // A deleted body's bar is grey: the blue bar is the look of words that go out.
    expect(quote.className).toContain(borderStrong);
    expect(quote.className).not.toContain(quoteAccentBorder);
    // Its own words and its red mark say it removes, so no "Removes" stands alone under them.
    expect(within(remove).queryByTestId("going-plan-removes")).toBeNull();
    expect(within(remove).getByTestId("going-plan-final")).toHaveTextContent(
      /^Cannot be undone once deleted$/u
    );
    expect(remove).toHaveAttribute("data-removes", "true");
    expect(within(remove).queryByTestId("going-plan-field")).toBeNull();

    const gone = drawnRow(2);
    await waitFor(() =>
      expect(within(gone).getByTestId("going-plan-post")).toHaveTextContent(
        /^Edit the comment by carol on DEMO-17In progressThe issue DEMO-17$/u
      )
    );
    expect(within(gone).getByTestId("going-plan-post-deleted")).toHaveTextContent(
      /^This comment was deleted\.$/u
    );
    expect(within(gone).queryByRole("checkbox")).toBeNull();
    expect(within(gone).queryByTestId("going-plan-field")).toBeNull();
    expect(within(gone).queryByTestId("going-plan-quote")).toBeNull();
    expect(within(gone).queryByTestId("going-plan-removes")).toBeNull();
    expect(gone.textContent ?? "").not.toMatch(/Monday|Friday|Body/u);

    const approveButton = screen.getByTestId("going-plan-approve");
    expect(approveButton).toHaveTextContent("Approve 2 changes");
    await user.click(approveButton);
    await waitFor(() =>
      expect(approve).toHaveBeenCalledWith(
        "run-1",
        {
          plan: 1,
          rows: [
            { row: 0, tick: true },
            { row: 1, tick: true },
            { row: 2, tick: false },
          ],
          version: "v1",
        },
        keyedAttempt
      )
    );
  } finally {
    restore();
  }
});

// A comment the signed-in person wrote reads "your comment", never "the comment by <their own
// login>" (review a3). A body too long for one line reads as the first sentences of what the
// edit writes, with its diff folded under them, as a document's text change does (R4).
test("an edit of the signed-in person's own comment says your comment, and a long body folds its diff", async () => {
  const signedIn = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const before = "The window is 24 hours. It starts when the plan is approved. ".repeat(4).trim();
  const after = before.replace("24 hours", "72 hours");
  const restore = await openPlan(
    planOf([
      postRow(0, "dispatch_edit_comment", "DEMO-15", "alice", { body: before }, { body: after }),
      postRow(1, "dispatch_edit_comment", "DEMO-16", "bob", { body: "Old." }, { body: "New." }),
    ])
  );
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    await waitFor(() =>
      expect(within(drawnRow(0)).getByTestId("going-plan-post")).toHaveTextContent(
        /^Edit your comment on DEMO-15/u
      )
    );
    expect(within(drawnRow(0)).getByTestId("going-plan-post").textContent).not.toMatch(
      /alice|by /u
    );
    expect(
      within(drawnRow(0)).getByRole("checkbox", { name: "Edit the comment on DEMO-15" })
    ).toBeChecked();
    // A comment of another person's still names them.
    expect(within(drawnRow(1)).getByTestId("going-plan-post")).toHaveTextContent(
      /^Edit the comment by bob on DEMO-16/u
    );

    const long = fieldOf(drawnRow(0), "body");
    expect(within(long).getByTestId("going-plan-sentences")).toHaveTextContent(
      /^The window is 72 hours\./u
    );
    const folded = within(long).getByTestId("going-plan-diff");
    expect(folded.tagName).toBe("DETAILS");
    expect(folded).not.toHaveAttribute("open");
    expect(folded.querySelector("del")?.textContent).toBe("24");
    expect(folded.querySelector("ins")?.textContent).toBe("72");
  } finally {
    restore();
    signedIn.mockRestore();
  }
});

test("in Auto-review a row that passed review says it ran, and a row that waits says why in words", async () => {
  const restore = await openPlan(
    planOf(
      [
        row(0, {
          arguments: { issue: "DEMO-1", priority: 1 },
          fields: ["priority"],
          issue: "DEMO-1",
          item: "DEMO-1",
          now: { priority: 3 },
          state: "done",
        }),
        row(1, {
          arguments: { issue: "DEMO-2", status: "in_progress" },
          fields: ["status"],
          issue: "DEMO-2",
          item: "DEMO-2",
          now: { status: "todo" },
          waits: ["claimed_by_session"],
        }),
        row(2, {
          arguments: { body: "Looks right.", issue: "DEMO-3" },
          class: "text",
          issue: "DEMO-3",
          item: "DEMO-3",
          tool: "dispatch_comment",
        }),
      ],
      { mode: "auto_review" }
    )
  );
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    expect(screen.getByTestId("going-plan-mode")).toHaveTextContent("Auto-review");
    expect(within(drawnRow(0)).getByTestId("going-plan-row-state")).toHaveTextContent(
      "Ran · passed review"
    );
    expect(fieldOf(drawnRow(0), "priority")).toHaveTextContent(/^Priority: P3 → P1$/u);
    expect(within(drawnRow(1)).getByTestId("going-plan-row-state")).toHaveTextContent(
      "Waits for you: an agent session holds this issue's claim"
    );
    expect(within(drawnRow(2)).getByTestId("going-plan-row-state")).toHaveTextContent(
      "Waits for you: it changes text, which Auto-review never runs"
    );
    expect(within(drawnRow(2)).getByTestId("going-plan-quote")).toHaveTextContent("Looks right.");
    // Approve counts the rows that wait, not the row that ran.
    expect(screen.getByTestId("going-plan-approve")).toHaveTextContent("Approve 2 changes");
  } finally {
    restore();
  }
});

test("unticking a row unticks the row that needs it, which says why, Approve counts what is left, and Discard rejects", async () => {
  const user = userEvent.setup();
  const restore = await openPlan(
    planOf([
      row(0, {
        arguments: { parent: "DEMO-1", project: "DEMO", title: "North half" },
        class: "create_plain",
        tool: "dispatch_issue",
      }),
      row(1, {
        arguments: { issue: "DEMO-3", parent: "$0" },
        fields: ["parent"],
        issue: "DEMO-3",
        item: "DEMO-3",
        needs: [0],
        now: { parent: "DEMO-1" },
      }),
      { ...labelsAndPriority, row: 2 },
    ])
  );
  const reject = answerRoute<
    (run: string, body: unknown, bounds: WriteBounds) => Promise<GoingPlanAnswer>
  >("POST", "/api/v1/going/runs/*/plan/reject");
  reject.mockImplementation(async () => planOf([], { state: "rejected" }));
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    const approve = screen.getByTestId("going-plan-approve");
    expect(approve).toHaveTextContent("Approve 3 changes");
    expect(within(drawnRow(1)).queryByTestId("going-plan-needs")).toBeNull();

    await user.click(screen.getByRole("checkbox", { name: "File North half" }));
    expect(drawnRow(1)).toHaveAttribute("data-ticked", "false");
    expect(within(drawnRow(1)).getByTestId("going-plan-needs")).toHaveTextContent(
      "Needs the change above"
    );
    expect(within(drawnRow(0)).queryByTestId("going-plan-needs")).toBeNull();
    expect(approve).toHaveTextContent("Approve 1 change");

    expect(screen.getByTestId("going-plan-reject")).toHaveTextContent("Discard");
    await user.click(screen.getByTestId("going-plan-reject"));
    await waitFor(() => expect(reject).toHaveBeenCalledWith("run-1", { plan: 1 }, keyedAttempt));
  } finally {
    restore();
  }
});

test("a row whose item changed since the plan showed is amber, says its value now, and waits to be ticked again", async () => {
  const user = userEvent.setup();
  const restore = await openPlan(
    planOf([
      row(0, {
        arguments: { issue: "DEMO-3", title: "Raise the dome" },
        changed: true,
        fields: ["title"],
        issue: "DEMO-3",
        item: "DEMO-3",
        now: { title: "Bob's dome" },
      }),
      row(1, { ...labelsAndPriority, needs: [0], row: 1 }),
    ])
  );
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    const changed = drawnRow(0);
    expect(changed).toHaveAttribute("data-changed", "true");
    expect(within(changed).getByTestId("going-plan-changed")).toHaveTextContent(
      "Changed since the plan: now Bob's dome"
    );
    expect(fieldOf(changed, "title")).toHaveTextContent(/^Title: Bob's dome → Raise the dome/u);
    // It waits to be ticked again, and so does the row that needs it.
    await waitFor(() => expect(drawnRow(0)).toHaveAttribute("data-ticked", "false"));
    expect(drawnRow(1)).toHaveAttribute("data-ticked", "false");
    expect(screen.getByTestId("going-plan-approve")).toHaveTextContent("Approve 0 changes");
    expect(screen.getByTestId("going-plan-approve")).toBeDisabled();

    await user.click(within(drawnRow(1)).getByRole("checkbox"));
    expect(drawnRow(0)).toHaveAttribute("data-ticked", "true");
    expect(drawnRow(1)).toHaveAttribute("data-ticked", "true");
    expect(screen.getByTestId("going-plan-approve")).toHaveTextContent("Approve 2 changes");
  } finally {
    restore();
  }
});

test("the card notes the changes the page shows, and Approve counts them", async () => {
  const restore = await openPlan(
    planOf([
      row(0, {
        arguments: { issue: "DEMO-1", title: "North" },
        fields: ["title"],
        issue: "DEMO-1",
        item: "DEMO-1",
        now: { title: "Dome" },
      }),
      row(1, {
        arguments: { issue: "DEMO-3", title: "South" },
        fields: ["title"],
        issue: "DEMO-3",
        item: "DEMO-3",
        now: { title: "Dome" },
      }),
      row(2, {
        arguments: { body: "Split.", issue: "DEMO-1" },
        class: "message",
        issue: "DEMO-1",
        item: "DEMO-1",
        tool: "dispatch_message",
      }),
    ])
  );
  try {
    renderWithQueries(
      <>
        <PlannedChanges
          surface={{ issues: ["DEMO-1", "DEMO-3"], kind: "issues", project: "DEMO" }}
        />
        <Card />
      </>
    );
    expect(await screen.findByTestId("going-plan-on-page-note")).toHaveTextContent(
      "2 changes show on this page"
    );
    const card = screen.getByTestId("going-plan");
    expect(within(card).getByTestId("going-plan-approve")).toHaveTextContent("Approve 3 changes");
    // The page's rows have the card's look, in the Planned outline.
    const page = screen.getByTestId("going-plan-on-page");
    expect(within(page).getAllByTestId("going-plan-row")).toHaveLength(2);
    expect(
      fieldOf(within(page).getAllByTestId("going-plan-row")[0] as HTMLElement, "title")
    ).toHaveTextContent(/^Title: Dome → North/u);
  } finally {
    restore();
  }
});

test("↑ in the empty box moves into the card, where ↑ ↓ move, Space ticks, E edits, and Esc or a letter go back to the box", async () => {
  const user = userEvent.setup();
  const restore = await openPlan(
    planOf([
      row(0, {
        arguments: { issue: "DEMO-1", title: "North" },
        fields: ["title"],
        issue: "DEMO-1",
        item: "DEMO-1",
        now: { title: "Dome" },
      }),
      { ...labelsAndPriority, row: 1 },
      row(2, {
        arguments: { body: "Split.", issue: "DEMO-1" },
        class: "message",
        issue: "DEMO-1",
        item: "DEMO-1",
        tool: "dispatch_message",
      }),
    ])
  );
  try {
    renderWithQueries(
      <>
        <Card />
        <Box />
      </>
    );
    await screen.findByTestId("going-plan-card-rows");
    const box = screen.getByRole("textbox", { name: "Say what to change" });
    await user.click(box);
    await user.keyboard("{ArrowUp}");
    // The row nearest the box takes the focus.
    expect(drawnRow(2)).toHaveFocus();
    await user.keyboard("{ArrowUp}{ArrowUp}{ArrowUp}");
    expect(drawnRow(0)).toHaveFocus();
    await user.keyboard("{ArrowDown}");
    expect(drawnRow(1)).toHaveFocus();
    await user.keyboard(" ");
    expect(drawnRow(1)).toHaveAttribute("data-ticked", "false");
    await user.keyboard(" ");
    expect(drawnRow(1)).toHaveAttribute("data-ticked", "true");
    await user.keyboard("{Escape}");
    expect(box).toHaveFocus();

    // E opens the editor of the selected row's value: the title's box, with the focus in it.
    await user.keyboard("{ArrowUp}{ArrowUp}{ArrowUp}");
    expect(drawnRow(0)).toHaveFocus();
    await user.keyboard("e");
    expect(screen.getByRole("textbox", { name: "New title" })).toHaveFocus();
    await user.keyboard("{Escape}");

    // A letter puts the focus back in the box, the letter typed into it.
    await user.click(box);
    await user.keyboard("{ArrowUp}");
    expect(drawnRow(2)).toHaveFocus();
    await user.keyboard("k");
    expect(box).toHaveFocus();
    expect(box).toHaveValue("k");
    // With words in the box, ↑ is the box's own.
    await user.keyboard("{ArrowUp}");
    expect(box).toHaveFocus();
  } finally {
    restore();
  }
});

// Design v3, state 6, "Editing a value": a status and an assignee are edited with the issue
// page's own controls, the status menu and the assignee picker, and Approve carries the values.
test("the status menu and the assignee picker edit a row's values, and Approve carries them", async () => {
  const user = userEvent.setup();
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
  ]);
  const handOver = row(0, {
    arguments: { assignee: "alice", issue: "DEMO-5", status: "in_progress" },
    fields: ["assignee", "status"],
    issue: "DEMO-5",
    item: "DEMO-5",
    now: { assignee: null, status: "todo" },
  });
  const restore = await openPlan(planOf([handOver]));
  const approve = answerRoute<
    (run: string, body: unknown, bounds: WriteBounds) => Promise<GoingPlanAnswer>
  >("POST", "/api/v1/going/runs/*/plan/approve");
  approve.mockImplementation(async () => planOf([handOver], { state: "running" }));
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    // Each line's one Edit opens the issue page's own control for its field.
    await user.click(
      within(fieldOf(drawnRow(0), "status")).getByRole("button", { name: "Edit status" })
    );
    const status = screen.getByRole("combobox", { name: "Status of DEMO-5" });
    expect(status).toHaveValue("in_progress");
    await user.selectOptions(status, "testing");
    expect(
      within(fieldOf(drawnRow(0), "status")).getByTestId("going-plan-edited")
    ).toBeInTheDocument();
    await user.click(
      within(fieldOf(drawnRow(0), "assignee")).getByRole("button", { name: "Edit assignee" })
    );
    const assignee = screen.getByRole("combobox", { name: "Assignee of DEMO-5" });
    expect(assignee).toHaveValue("alice");
    // The picker reads who has signed in once the person reaches for it, as on the issue page.
    await user.hover(assignee);
    await waitFor(() => expect(within(assignee).getAllByRole("option")).toHaveLength(3));
    await user.selectOptions(assignee, "bob");
    expect(
      within(fieldOf(drawnRow(0), "assignee")).getByTestId("going-plan-edited")
    ).toBeInTheDocument();
    await user.click(screen.getByTestId("going-plan-approve"));
    await waitFor(() =>
      expect(approve).toHaveBeenCalledWith(
        "run-1",
        {
          plan: 1,
          rows: [{ row: 0, tick: true, values: { assignee: "bob", status: "testing" } }],
          version: "v1",
        },
        keyedAttempt
      )
    );
  } finally {
    listUsers.mockRestore();
    restore();
  }
});

/** A plan of three rows standing in `states`: a priority, a parent that needs the priority's
 *  row, and a message; the first failed with its reason where it failed, and the message queued
 *  where it ran. */
function sentRows(states: readonly GoingPlanRow["state"][]): GoingPlanRow[] {
  return [
    row(0, {
      arguments: { issue: "DEMO-1", priority: 1 },
      fields: ["priority"],
      issue: "DEMO-1",
      item: "DEMO-1",
      now: { priority: 3 },
      result:
        states[0] === "failed"
          ? { code: "FORBIDDEN", delivery: null, message: "you have no access", status: 403 }
          : null,
      state: states[0],
    }),
    row(1, {
      arguments: { issue: "DEMO-2", parent: "DEMO-1" },
      fields: ["parent"],
      issue: "DEMO-2",
      item: "DEMO-2",
      needs: [0],
      now: { parent: null },
      state: states[1],
    }),
    row(2, {
      arguments: { body: "Please look.", issue: "DEMO-1" },
      class: "message",
      issue: "DEMO-1",
      item: "DEMO-1",
      result:
        states[2] === "done" ? { code: "", delivery: "queued", message: "", status: 201 } : null,
      state: states[2],
      tool: "dispatch_message",
    }),
  ];
}

/** Each mark the card's rows draw. */
function marks(): readonly (string | null)[] {
  return screen.getAllByTestId("going-plan-row-mark").map((mark) => mark.getAttribute("data-mark"));
}

// Design v2, state 7: after approval each row ticks in place - a spinner while it runs, a ring
// while it waits its turn - and the title says which change runs.
test("while an approved plan runs, its title says Applying 1 of 3 and its rows tick in place", async () => {
  const restore = await openPlan(
    planOf(sentRows(["running", "approved", "approved"]), { state: "running" })
  );
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    expect(screen.getByTestId("going-plan-title")).toHaveTextContent("Applying 1 of 3…");
    expect(marks()).toEqual(["running", "turn", "turn"]);
    expect(screen.queryByTestId("going-plan-approve")).toBeNull();
    expect(screen.queryByTestId("going-plan-mode")).toBeNull();
    expect(screen.queryByTestId("going-undo-button")).toBeNull();
  } finally {
    restore();
  }
});

// Design v3, state 7: "Applied 3 changes · 16:42", the time the last row ran, which the run's end
// comes after while it writes its answer.
test("once its rows ran, the title says when, while the run still works", async () => {
  let answer = planOf([{ ...labelsAndPriority, state: "running" }], { state: "running" });
  let send: (name: string, data: unknown) => void = () => {};
  const start = spyOn(api, "createGoingRun").mockResolvedValue(created);
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    send = onFrame;
    onFrame("plan", { plan: 1, state: "running" });
    return Promise.withResolvers<void>().promise;
  });
  const read = spyOn(api, "goingPlan").mockImplementation(async () => answer);
  try {
    await startRun("tidy DEMO-10", { path: "/projects/DEMO/issues" }, "alice");
    setPlace("over");
    renderWithQueries(<Card />);
    const title = await screen.findByTestId("going-plan-title");
    await waitFor(() => expect(title).toHaveTextContent("Applying 1 of 1…"));

    answer = planOf([{ ...labelsAndPriority, state: "done" }], { state: "done" });
    const before = Date.now();
    act(() => send("plan", { plan: 1, state: "done" }));
    await waitFor(() => expect(title).toHaveTextContent(/^Applied 1 change · /u));
    const after = Date.now();
    expect([before, after].map((at) => `Applied 1 change · ${clockWords(at)}`)).toContain(
      title.textContent ?? ""
    );
  } finally {
    start.mockRestore();
    stream.mockRestore();
    read.mockRestore();
  }
});

// Design v2, state 7 (R4, R7): a failed row says why with "Say what to do instead", which puts
// the cursor in the box; the rows that needed it say they did not run; a message row says what
// became of it.
test("once it ran, a failed row says why and Say what to do instead puts the cursor in the box", async () => {
  const user = userEvent.setup();
  const restore = await openPlan(
    planOf(sentRows(["failed", "skipped", "done"]), { state: "done" })
  );
  try {
    renderWithQueries(<CardAndBox />);
    await screen.findByTestId("going-plan-card-rows");
    expect(screen.getByTestId("going-plan-title")).toHaveTextContent("Applied 1 of 3 changes");
    expect(marks()).toEqual(["failed", "not_run", "done"]);
    expect(within(drawnRow(0)).getByTestId("going-plan-row-state")).toHaveTextContent(
      "you have no access Say what to do instead"
    );
    expect(within(drawnRow(1)).getByTestId("going-plan-row-state")).toHaveTextContent(
      "Not run: it needs the change above"
    );
    expect(within(drawnRow(2)).getByTestId("going-plan-row-state")).toHaveTextContent(
      "Queued: Dispatch delivers it when the session can take it"
    );
    await user.click(within(drawnRow(0)).getByTestId("going-plan-fix"));
    expect(screen.getByRole("textbox", { name: "Ask the command bar" })).toHaveFocus();
  } finally {
    restore();
  }
});

// Design v2, state 7 (R6): the card keeps one Undo once a row ran; after it, the title says what
// was reverted, a value someone else changed since stays and its row names it as the page shows
// it, and a sent message's row reads "Sent, not undone".
test("Undo on the card reverts the run, names a value someone else changed, and a message sent, not undone", async () => {
  const user = userEvent.setup();
  const ran = planOf(
    [
      row(0, {
        arguments: { issue: "DEMO-1", priority: 1 },
        fields: ["priority"],
        issue: "DEMO-1",
        item: "DEMO-1",
        now: { priority: 3 },
        state: "done",
      }),
      row(1, {
        arguments: { issue: "DEMO-3", title: "Raise the dome" },
        fields: ["title"],
        issue: "DEMO-3",
        item: "DEMO-3",
        now: { title: "Dome" },
        state: "done",
      }),
      row(2, {
        arguments: { body: "Done.", issue: "DEMO-1" },
        class: "message",
        issue: "DEMO-1",
        item: "DEMO-1",
        result: { code: "", delivery: "sent", message: "", status: 201 },
        state: "done",
        tool: "dispatch_message",
      }),
    ],
    { state: "done" }
  );
  // Dispatch's record of the run: two changes Undo can take back, the message none.
  const counted = { ...created, undoable: true, undoable_count: 2 };
  const restore = await openPlan(ran, counted);
  const reread = spyOn(api, "goingRun").mockResolvedValue(counted);
  const undo = answerRoute<
    (run: string, body: undefined, bounds: WriteBounds) => Promise<GoingUndoAnswer>
  >("POST", "/api/v1/going/runs/*/undo");
  undo.mockImplementation(async () => ({
    lines: [
      {
        fields: [],
        item: "DEMO-1",
        outcome: { code: "", message: "sent, not undone", ok: true, status: 200 },
        plan: 1,
        row: 2,
        tool: "dispatch_message",
        undo: "not_undone",
      },
      {
        fields: [
          {
            by: { id: "bob", kind: "user" },
            field: "title",
            message: `holds "Bob's dome" now, which user bob wrote after this run, so it is kept`,
            now: "Bob's dome",
            ok: false,
          },
        ],
        item: "DEMO-3",
        outcome: { code: "GOING_UNDO_CHANGED", message: "title: holds …", ok: false, status: 409 },
        plan: 1,
        row: 1,
        tool: "dispatch_issue_update",
        undo: "write_back",
      },
      {
        fields: [{ field: "priority", message: "written back", ok: true }],
        item: "DEMO-1",
        outcome: { code: "", message: "written back", ok: true, status: 200 },
        plan: 1,
        row: 0,
        tool: "dispatch_issue_update",
        undo: "write_back",
      },
    ],
  }));
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    expect(screen.getByTestId("going-plan-title")).toHaveTextContent("Applied 3 changes");
    // The message row reads in the applied card as it reads while the plan waits: nothing routes,
    // claims or follows DEMO-1, so it reaches no session, and its result says where it went
    // rather than "Sent" under "Message to no session" (review a8).
    await waitFor(() =>
      expect(within(drawnRow(2)).getByTestId("going-plan-message")).toHaveTextContent(
        /^Message to no session on DEMO-1In progressThe issue DEMO-1$/u
      )
    );
    // The issue's chip stands in the line above, so the result names no issue again (review N1).
    await waitFor(() =>
      expect(within(drawnRow(2)).getByTestId("going-plan-row-state")).toHaveTextContent(
        /^Posted in the issue's conversation$/u
      )
    );
    expect(screen.getByTestId("going-undo-button")).toHaveTextContent(/^Undo 2 changes$/u);
    await user.click(screen.getByTestId("going-undo-button"));
    await waitFor(() => expect(undo).toHaveBeenCalledWith("run-1", undefined, keyedAttempt));
    await waitFor(() =>
      expect(screen.getByTestId("going-plan-title")).toHaveTextContent(
        "Undone · 1 of 2 changes reverted"
      )
    );
    expect(within(drawnRow(0)).getByTestId("going-undo-line")).toHaveTextContent("Reverted");
    expect(within(drawnRow(1)).getByTestId("going-undo-line").textContent).toBe(
      "Title stays Bob's dome: bob changed it after this run"
    );
    expect(within(drawnRow(2)).getByTestId("going-undo-line")).toHaveTextContent(
      "Sent, not undone"
    );
    expect(screen.queryByTestId("going-undo-button")).toBeNull();
    // The change Undo took back reads reverted: no check, its line as the plan card drew it,
    // plain "old → new" with no strike (review N3), and "Reverted" under it, above. The change
    // someone else kept, and the message sent, keep their check.
    expect(marks()).toEqual(["reverted", "done", "done"]);
    const priority = fieldOf(drawnRow(0), "priority");
    expect(shownText(priority)).toBe("Priority: P3 → P1");
    expect(priority.querySelector("del")).toBeNull();
    expect(shownText(fieldOf(drawnRow(1), "title"))).toBe("Title: Dome → Raise the dome");
    expect(screen.getByTestId("going-plan").textContent ?? "").not.toMatch(/[{}"]|GOING_/u);
  } finally {
    reread.mockRestore();
    restore();
  }
});

// A message that reached a session says "Sent", as before: "Posted in the issue's conversation"
// is for the message that reached no session, whose line above says so (review a8).
test("a sent message that reached a session reads Sent, not Posted in the issue's conversation", async () => {
  const reaching = spyOn(api, "getIssueSubscribers").mockResolvedValue([
    {
      last_seen: 0,
      live: true,
      removable: true,
      session_id: "s-planner",
      title: "Planner",
      topics: [],
    },
  ]);
  const restore = await openPlan(
    planOf(
      [
        row(0, {
          arguments: { body: "Done.", issue: "DEMO-1" },
          class: "message",
          issue: "DEMO-1",
          item: "DEMO-1",
          result: { code: "", delivery: "sent", message: "", status: 201 },
          state: "done",
          tool: "dispatch_message",
        }),
      ],
      { state: "done" }
    )
  );
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    await waitFor(() =>
      expect(within(drawnRow(0)).getByTestId("going-plan-message")).toHaveTextContent(
        /^Message to .+ on DEMO-1/u
      )
    );
    expect(within(drawnRow(0)).getByTestId("going-plan-message").textContent).not.toMatch(
      /no session/u
    );
    expect(within(drawnRow(0)).getByTestId("going-plan-row-state")).toHaveTextContent(/^Sent$/u);
  } finally {
    restore();
    reaching.mockRestore();
  }
});

// Review A1: the pinned Approve bar hides the rows scrolled under it, and Enter approves all of
// them. While rows stand under the bar, its top edge says so - how many, and which of those
// nothing takes back, each named by its kind - and its "3 more ↓" is a button, a press of which
// scrolls to the first of them; the line goes once none is under the bar (the view's scroll, at
// its end). Review B1: the press scrolls the conversation that holds the card, and never the
// frame around it, whose header and box stay where they are.
test("rows under the pinned Approve bar are counted on it with those that cannot be undone named, its button scrolls the conversation and not the frame, and the line goes once none is hidden", async () => {
  const user = userEvent.setup();
  // The bar's top edge, and each row as a browser lays them out: row n from 100n to 100(n+1),
  // and every other box, the conversation's scroll among them, at the window's top.
  let edge = 250;
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    const { row: number, testid } = this.dataset;
    const [top, bottom] =
      testid === "going-plan-actions"
        ? [edge, edge + 60]
        : testid === "going-plan-row"
          ? [Number(number) * 100, (Number(number) + 1) * 100]
          : [0, 0];
    return {
      bottom,
      height: bottom - top,
      left: 0,
      right: 700,
      toJSON: () => ({}),
      top,
      width: 700,
      x: 0,
      y: top,
    };
  });
  // A scroll into view moves every ancestor that can scroll, the frame's own hidden overflow
  // among them: the press uses none.
  const intoView = spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => undefined);
  const restore = await openPlan(
    planOf([
      row(0, { arguments: { issue: "DEMO-1", priority: 1 }, fields: ["priority"], item: "DEMO-1" }),
      row(1, { arguments: { issue: "DEMO-2", priority: 1 }, fields: ["priority"], item: "DEMO-2" }),
      row(2, { arguments: { issue: "DEMO-3", priority: 1 }, fields: ["priority"], item: "DEMO-3" }),
      postRow(3, "dispatch_delete_comment", "DEMO-16", "bob", { body: "Ship it Friday." }),
      row(4, {
        arguments: { body: "Hello.", issue: "DEMO-1" },
        class: "message",
        issue: "DEMO-1",
        item: "DEMO-1",
        tool: "dispatch_message",
        waits: ["message"],
      }),
    ])
  );
  try {
    // The card in a conversation that scrolls, inside a frame that hides what overflows it, as
    // the bar's dialog and the panel hold it.
    renderWithQueries(
      <div data-testid="frame" style={{ overflow: "hidden" }}>
        <div data-testid="conversation" style={{ overflowY: "auto" }}>
          <Card />
        </div>
      </div>
    );
    await screen.findByTestId("going-plan-card-rows");
    const more = await screen.findByTestId("going-plan-more");
    // Rows 2, 3 and 4 stand under the bar; the delete and the message cannot be undone.
    expect(more).toHaveTextContent(
      "3 more ↓ · includes a comment's deletion and a message that cannot be undone"
    );
    expect(screen.getByTestId("going-plan-actions").className).toContain("shadow-");
    await user.click(within(more).getByRole("button", { name: "3 more" }));
    // The conversation scrolled the first row under the bar, row 2, to its top; the frame did
    // not move.
    expect(screen.getByTestId("conversation").scrollTop).toBe(200);
    expect(screen.getByTestId("frame").scrollTop).toBe(0);
    expect(intoView).not.toHaveBeenCalled();

    // A row that is unticked is not run, so it is not one the line warns of.
    await user.click(within(drawnRow(4)).getByRole("checkbox"));
    expect(screen.getByTestId("going-plan-more")).toHaveTextContent(
      "3 more ↓ · includes a comment's deletion that cannot be undone"
    );

    // The view scrolled to its end: no row stands under the bar, and the line and shadow go.
    edge = 1000;
    act(() => {
      document.dispatchEvent(new Event("scroll"));
    });
    await waitFor(() => expect(screen.queryByTestId("going-plan-more")).toBeNull());
    expect(screen.getByTestId("going-plan-actions").className).not.toContain("shadow-");
  } finally {
    restore();
    intoView.mockRestore();
    boxes.mockRestore();
  }
});

/** A project's default repository set, which ran: a setting, which Undo leaves as it is. */
const repositorySet = row(1, {
  arguments: { project: "DEMO", repo: "acme/infra" },
  class: "setting",
  item: "DEMO",
  result: { code: "", delivery: null, message: "", status: 200 },
  state: "done",
  tool: "dispatch_set_default_repository",
});

// What the card's Undo says its press takes back is Dispatch's count of it (class search, entry
// 55), never the plan's rows that ran: a setting stays as it is, so a plan that updated an issue
// and set a default repository offered "Undo 2 changes" for a press that takes back one.
test("the card's Undo counts the changes Dispatch says it can take back, not the rows that ran", async () => {
  const restore = await openPlan(
    planOf([{ ...labelsAndPriority, state: "done" }, repositorySet], { state: "done" }),
    { ...created, undoable: true, undoable_count: 1 }
  );
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    expect(screen.getByTestId("going-plan-title")).toHaveTextContent("Applied 2 changes");
    expect(screen.getByTestId("going-undo-button")).toHaveTextContent(/^Undo 1 change$/u);
  } finally {
    restore();
  }
});

// A plan whose one row that ran is such a setting offered "Undo 1 change", though Dispatch's
// record says nothing of it is left to take back, and its press read "Undone · nothing to revert".
test("a plan whose changes Undo leaves as they are offers no Undo", async () => {
  const restore = await openPlan(planOf([repositorySet], { state: "done" }));
  try {
    renderWithQueries(<Card />);
    await screen.findByTestId("going-plan-card-rows");
    expect(screen.getByTestId("going-plan-title")).toHaveTextContent("Applied 1 change");
    expect(screen.queryByTestId("going-undo-button")).toBeNull();
  } finally {
    restore();
  }
});

/** A start of run `id`, a conversation of its own, of `words`. */
function startedAs(id: string, words: string): GoingRun {
  return { ...created, conversation: id, id, words };
}

/** A change of DEMO-11's priority, which the second run's plan proposes. */
const raiseDemo11 = row(0, {
  arguments: { issue: "DEMO-11", priority: 1 },
  fields: ["priority"],
  issue: "DEMO-11",
  item: "DEMO-11",
  now: { priority: null },
});

/** Answers the starts of `run-1` and then `run-2`, each its own conversation, whose streams carry
 *  a plan frame and stay open, and each run's plan as `plans` says. `counts` is what each run's
 *  record says Undo can take back of it. */
function answerTwoRuns(
  plans: (run: string) => GoingPlanAnswer,
  counts: Partial<GoingRun> = {}
): () => void {
  const start = spyOn(api, "createGoingRun")
    .mockResolvedValueOnce({ ...created, ...counts })
    .mockResolvedValueOnce({ ...startedAs("run-2", "raise DEMO-11"), ...counts });
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    onFrame("plan", { plan: 1, state: "waiting" });
    return Promise.withResolvers<void>().promise;
  });
  const read = spyOn(api, "goingPlan").mockImplementation(async (run) => plans(run));
  return () => {
    start.mockRestore();
    stream.mockRestore();
    read.mockRestore();
  };
}

/** Starts `run-2` in a conversation of its own, as New and then Enter in the bar do. */
async function startTheSecond(): Promise<void> {
  await act(async () => {
    newConversation();
    await startRun("raise DEMO-11", { path: "/projects/DEMO/issues" }, "alice");
  });
}

/** The state of the plan of `run` as the page holds it, read from nowhere. */
function HeldPlan({ run }: { readonly run: string }): ReactNode {
  const held = useQuery({ ...goingPlanQuery(run), enabled: false }).data;
  return <output data-testid={`held-plan-${run}`}>{held?.state}</output>;
}

// An answer to a plan is of the run it was pressed on: an approval that lands after the person
// started another conversation settles into that run's plan, while the card of the run open keeps
// its own plan, whose Approve was its own to press all along.
test("an approval that lands after another run opened settles into the plan it approved, and the open card keeps its own", async () => {
  const user = userEvent.setup();
  const first = planOf([labelsAndPriority]);
  const restore = answerTwoRuns((run) =>
    run === "run-1" ? first : planOf([raiseDemo11], { run: "run-2" })
  );
  const approve = answerRoute<
    (run: string, body: unknown, bounds: WriteBounds) => Promise<GoingPlanAnswer>
  >("POST", "/api/v1/going/runs/*/plan/approve");
  const approved = Promise.withResolvers<GoingPlanAnswer>();
  approve.mockImplementationOnce(() => approved.promise);
  try {
    await startRun("tidy DEMO-10", { path: "/projects/DEMO/issues" }, "alice");
    setPlace("over");
    renderWithQueries(
      <>
        <Card />
        <HeldPlan run="run-1" />
      </>
    );
    await user.click(await screen.findByTestId("going-plan-approve"));
    await waitFor(() =>
      expect(approve).toHaveBeenCalledWith("run-1", expect.anything(), keyedAttempt)
    );
    await startTheSecond();
    await waitFor(() =>
      expect(screen.getByTestId("going-plan-title")).toHaveTextContent("Change DEMO-11")
    );
    expect(screen.getByTestId("going-plan-approve")).toBeEnabled();

    approved.resolve({ ...first, rows: [{ ...labelsAndPriority, state: "done" }], state: "done" });
    await waitFor(() => expect(screen.getByTestId("held-plan-run-1")).toHaveTextContent("done"));
    expect(screen.getByTestId("going-plan-title")).toHaveTextContent("Change DEMO-11");
    expect(screen.getByTestId("going-plan-approve")).toBeEnabled();
  } finally {
    restore();
  }
});

// One run's Undo on a card is that run's: the card of the conversation open draws another run's
// plan once another opens, and an Undo out meanwhile, its answer and a refusal of it, stay with
// the run pressed.
test("a card's Undo out when another run opens keeps its answer and its refusal that run's", async () => {
  const user = userEvent.setup();
  const ran = (run: string) =>
    planOf([{ ...labelsAndPriority, state: "done" }], { run, state: "done" });
  const counts = { undoable: true, undoable_count: 1 };
  const restore = answerTwoRuns(ran, counts);
  const reread = spyOn(api, "goingRun").mockImplementation(async (id) => ({
    ...created,
    ...counts,
    id,
  }));
  const undo = answerRoute<
    (run: string, body: undefined, bounds: WriteBounds) => Promise<GoingUndoAnswer>
  >("POST", "/api/v1/going/runs/*/undo");
  const answered = Promise.withResolvers<GoingUndoAnswer>();
  undo.mockImplementationOnce(() => answered.promise);
  undo.mockImplementationOnce(async () => {
    throw new ApiError(409, { code: "GOING_UNDO_UNKNOWN", error: "nothing was undone" });
  });
  try {
    await startRun("tidy DEMO-10", { path: "/projects/DEMO/issues" }, "alice");
    setPlace("over");
    renderWithQueries(<Card />);
    await user.click(await screen.findByTestId("going-undo-button"));
    await waitFor(() => expect(undo).toHaveBeenCalledWith("run-1", undefined, keyedAttempt));
    await startTheSecond();
    await waitFor(() => expect(screen.getByTestId("going-undo-button")).toBeEnabled());

    answered.resolve({ lines: [] });
    await waitFor(() => expect(goingState().undone.has("run-1")).toBe(true));
    expect(goingState().undone.has("run-2")).toBe(false);
    expect(screen.getByTestId("going-undo-button")).toBeEnabled();

    await user.click(screen.getByTestId("going-undo-button"));
    expect(await screen.findByRole("alert")).toHaveTextContent("nothing was undone");
    act(() => openRun("run-1"));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  } finally {
    reread.mockRestore();
    restore();
  }
});

// Design v3, state 7: the time a plan's rows ran is the time the card first drew them run, taken
// again for each plan it draws: the card of the conversation open draws the next run's plan once
// another conversation starts, and that plan's time is its own.
test("the time a plan's rows ran is taken again for the next plan the card draws", async () => {
  jest.useFakeTimers();
  const restore = answerTwoRuns((run) =>
    planOf([{ ...labelsAndPriority, state: "done" }], { run, state: "done" })
  );
  try {
    await startRun("tidy DEMO-10", { path: "/projects/DEMO/issues" }, "alice");
    setPlace("over");
    renderWithQueries(<Card />);
    // The card draws nothing while the next plan is read, so its title is read afresh each time.
    const title = () => screen.getByTestId("going-plan-title").textContent;
    await waitFor(() => expect(title()).toMatch(/^Applied 1 change · /u));
    const first = title();

    await tick(120_000, 120_000);
    const before = Date.now();
    await startTheSecond();
    await waitFor(() => expect(title()).not.toBe(first));
    const after = Date.now();
    expect([before, after].map((at) => `Applied 1 change · ${clockWords(at)}`)).toContain(
      title() ?? ""
    );
  } finally {
    restore();
    realClock();
  }
});

/** The plan card above the conversation's box row, whose box "Say what to do instead" puts the
 *  cursor in. */
function CardAndBox(): ReactNode {
  return (
    <>
      <Card />
      <BoxRow
        box={<input aria-label="Ask the command bar" type="text" />}
        hint={undefined}
        onStop={() => {}}
      />
    </>
  );
}
