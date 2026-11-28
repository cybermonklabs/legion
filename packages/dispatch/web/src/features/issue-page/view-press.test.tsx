import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { DESKTOP_WIDTH, PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { artifact, issueDetails, KEY, SPEC_DOCUMENT } from "./decisions-filter/decisions-fixtures";
import { IssueOutlinePage } from "./IssueOutlinePage";
import { presetPath } from "./issue-address";
import * as issueTreeModule from "./issue-tree";
import { TWO_THREADS } from "./outline-fixtures";
import { CONTROL_LABELS, PRESET_NAMES, type Preset } from "./page-words";
import { type TasksByHand, tasksByHand } from "./slices-fixtures";
import { FAILED_READ_RETRY } from "./slots/failed-read-line";
import { AppProviders, commentNodes, inDocument, stubReads, treeOf } from "./slots/stage-fixtures";
import { coverageRead } from "./spec-filter/spec-fixtures";
import { treeNode } from "./tree-read/tree-fixtures";
import type { TreeAnswer } from "./tree-read/tree-read";

/**
 * A press on a view whose rows need a read (R2's three limits; the plan's state table "A press on
 * a view whose rows need a read", one case for each of its eight rows). The page's whole root is
 * drawn at an issue's address (`IssueOutlinePage`), under what the app provides above it, with the
 * api client's tree reads held by the case: each is a promise the case answers or refuses when it
 * chooses, so a case reads the page in the press's commit, before any read the press sent has
 * answered, and in the commit after the answer.
 *
 * The press's commit is what the document holds once `act` has run the press's click and every
 * render it asked for, with no read answered in between: React commits a press's update in the
 * task of its event, before the browser paints that frame. A read sent from the press's handler is
 * sent before that commit, so the request log notes, for each tree read, which view control read
 * as current as the read was asked for (`asked`): the control of the view the press left, where
 * the handler sent it.
 *
 * The view's place shows that it loads while its read is out: the view's tree reads busy
 * (`aria-busy="true"`), as the load rows read it (`e2e/issue-page-load.ts`' `VIEW_LOADS_SELECTOR`),
 * and as Plan's empty tree reads while a read of it is out.
 */

/** The mark of a view's place while its read is out, as the load rows read it. */
const VIEW_LOADS = '[data-issue-page] [aria-busy="true"]';

/** A statement of the Spec view, drawn from its view read. */
const SPEC_ROW = "[data-spec-statement]";

/** A row of the outline: Conversation's, and Graph's. */
const OUTLINE_ROW = "[data-issue-body] [data-node-row]";

/** Which read a call of the tree route is: a view's by its name, else the outline's. */
type TreeReadName = "conversation" | "graph" | "plan" | "spec" | "verification";

/** One call of the tree route: which read, and the view control that read as current as it was
 *  asked for (`asked`), and the call's own abort signal. */
interface AskedRead {
  readonly asked: string | undefined;
  readonly read: TreeReadName;
  readonly signal: AbortSignal | undefined;
}

/** Each tree read's answer a case holds: one promise per call, answered or refused by the case,
 *  in the order the calls were made. */
interface HeldRead {
  readonly answer: (with_: TreeAnswer) => Promise<void>;
  readonly refuse: () => Promise<void>;
}

/** The two statements of the spec, as the Spec view's read answers them: the view's own nodes
 *  (`role: "query"`), each a block of the spec. */
const SPEC_VIEW: TreeAnswer = treeOf(
  ["r1", "r2"].map((id, position) => ({
    ...treeNode(id, "requirement", KEY, {
      ...inDocument(SPEC_DOCUMENT, position),
      text: `**${id.toUpperCase()}.** The page draws ${id}.`,
    }),
    role: "query" as const,
  }))
);

/** Conversation's read and Graph's: the issue's two threads. */
const OUTLINE: TreeAnswer = treeOf(commentNodes(TWO_THREADS));

/** The issue the cases draw: the fixtures' one, which holds its spec, with a plan as well, so the
 *  Plan view waits on its own view read, which a load of Conversation sends with it. */
const PLAN_DOCUMENT = artifact("artifact-plan", "plan-md");
function plannedIssue(): IssueDetails {
  const issue = issueDetails();
  return {
    ...issue,
    artifacts: [...(issue.artifacts ?? []), PLAN_DOCUMENT],
    plan_artifact_id: PLAN_DOCUMENT.id,
    queries: [
      {
        artifact_id: PLAN_DOCUMENT.id,
        query: "plan",
        subtree: `issue:${KEY}`,
        takes: ["plan_task", "plan_role"],
      },
    ],
  };
}

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
let tasks: TasksByHand;
/** Every call of the tree route since the case began, in order. */
let log: AskedRead[] = [];
/** The calls still held, by read, each the first not answered yet first. */
let held: Map<TreeReadName, HeldRead[]> = new Map();

/** The control of the view row that reads as current now, by its name. */
function currentControl(): string | undefined {
  const pressed = document.querySelector(
    '[data-testid="issue-presets"] button[aria-pressed="true"] [data-preset-name]'
  );
  const named =
    pressed ?? document.querySelector('[data-testid="issue-presets"] [data-preset-name]');
  return named?.textContent ?? undefined;
}

/** The read a call of the tree route makes, by its query. */
function readOf(query: {
  readonly kind?: readonly string[];
  readonly view?: string;
}): TreeReadName {
  if (query.view !== undefined) return query.view as TreeReadName;
  return query.kind === undefined ? "graph" : "conversation";
}

/** The calls of the tree route for `read`, in order. */
function sent(read: TreeReadName): AskedRead[] {
  return log.filter((call) => call.read === read);
}

/** Answers or refuses the first held call of `read`, inside `act`, so the commit it brings is
 *  made before the case reads the page. */
function heldCall(read: TreeReadName): HeldRead {
  const calls = held.get(read) ?? [];
  const call = calls.shift();
  if (call === undefined) throw new Error(`no call of the ${read} read is held`);
  return call;
}

beforeEach(() => {
  log = [];
  held = new Map();
  tasks = tasksByHand();
  stubs = [
    ...stubReads(),
    spyOn(api, "getIssue").mockResolvedValue(plannedIssue()),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getIssueCoverage").mockResolvedValue(coverageRead(["r1", "r2"])),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
    // Each tree read is held until the case answers it; the request log notes which view control
    // read as current as it was asked for.
    spyOn(api, "getIssueTree").mockImplementation((_key, query = {}, options = {}) => {
      const read = readOf(query);
      log.push({ asked: currentControl(), read, signal: options.signal });
      const answer = Promise.withResolvers<TreeAnswer>();
      const calls = held.get(read) ?? [];
      calls.push({
        answer: async (with_) => {
          await act(async () => answer.resolve(with_));
          await tasks.runAll();
        },
        refuse: () =>
          act(async () =>
            answer.reject(new ApiError(503, { code: "UNAVAILABLE", error: "refused" }))
          ),
      });
      held.set(read, calls);
      return answer.promise;
    }),
  ];
});
afterEach(() => {
  cleanup();
  tasks.restore();
  restoreViewport();
  for (const stub of stubs.reverse()) stub.mockRestore();
});

/** The page's root at `preset`'s address of the fixtures' issue, at `width`, under what the app
 *  provides above it, with a client as the page's tests take one: no read tried again, and none
 *  read again by its age. */
function drawPage(
  preset: Preset,
  width = DESKTOP_WIDTH
): { client: QueryClient; view: RenderResult } {
  restoreViewport = stubViewport(width);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    <MemoryRouter initialEntries={[presetPath(KEY, preset)]}>
      <AppProviders client={client}>
        <IssueOutlinePage />
      </AppProviders>
    </MemoryRouter>
  );
  return { client, view };
}

/** The view row's control of `preset`, from 1280 px up, once the row is drawn. */
async function control(view: RenderResult, preset: Preset): Promise<HTMLElement> {
  const toolbar = await view.findByRole("toolbar", { name: CONTROL_LABELS.presets });
  return within(toolbar).getByRole("button", { name: PRESET_NAMES[preset] });
}

/** Presses `button` and runs every render the press asks for, with no read answered between. */
function press(button: HTMLElement): void {
  act(() => button.click());
}

/** Conversation's load with its outline read answered and its rows drawn; Plan's read, sent with
 *  the load, is still held. */
async function conversationLoaded(view: RenderResult): Promise<void> {
  await waitFor(() =>
    expect(sent("conversation"), "the load sent no Conversation read").toHaveLength(1)
  );
  await heldCall("conversation").answer(OUTLINE);
  await waitFor(() =>
    expect(
      view.container.querySelector(OUTLINE_ROW),
      "Conversation drew no row once its read answered"
    ).not.toBeNull()
  );
}

// Row 1. A view whose read answered earlier in the load draws its first screen from that answer in
// the press's commit, and the press sends nothing.
test("a press on a view whose read is held draws its first screen in the press's commit and sends nothing", async () => {
  const { view } = drawPage("conversation");
  await conversationLoaded(view);
  press(await control(view, "spec"));
  await heldCall("spec").answer(SPEC_VIEW);
  await waitFor(() =>
    expect(view.container.querySelector(SPEC_ROW), "Spec drew no statement").not.toBeNull()
  );
  press(await control(view, "conversation"));
  await waitFor(() => expect(view.container.querySelector(OUTLINE_ROW)).not.toBeNull());
  const before = log.length;

  press(await control(view, "spec"));
  expect(currentControl(), "the press left Spec's control off in its commit").toBe("Spec");
  expect(
    view.container.querySelectorAll(SPEC_ROW).length,
    "the press drew no statement of the held answer in its commit"
  ).toBe(2);
  expect(
    view.container.querySelector(VIEW_LOADS),
    "the view's place reads as loading with its read held"
  ).toBeNull();
  expect(log.slice(before), "the press on a view whose read is held sent a read").toEqual([]);
});

// Row 2. A view whose read no one has sent: the press's handler sends it, before the commit, and
// in the press's commit the control reads as current and the view's place shows that it loads.
test("a press on a view whose read is not sent sends it from the handler, and its commit shows the view loading", async () => {
  const { view } = drawPage("conversation");
  await conversationLoaded(view);
  expect(sent("spec"), "a load of Conversation sent Spec's read").toEqual([]);

  press(await control(view, "spec"));
  expect(
    sent("spec").map((call) => call.asked),
    "Spec's read was not sent from the press's handler, before the commit that turned Spec on"
  ).toEqual(["Conversation"]);
  expect(currentControl(), "the press left Spec's control off in its commit").toBe("Spec");
  expect(
    view.container.querySelector(VIEW_LOADS),
    "the view's place does not show that it loads in the press's commit"
  ).not.toBeNull();
});

// Row 3. Plan's read goes out with every load of Conversation: a press on Plan while it is out
// sends no second request, and its commit shows the view loading.
test("a press on a view whose read is in flight sends no second request, and its commit shows the view loading", async () => {
  const { view } = drawPage("conversation");
  await conversationLoaded(view);
  await waitFor(() => expect(sent("plan"), "the load sent no Plan read").toHaveLength(1));

  press(await control(view, "plan"));
  expect(currentControl(), "the press left Plan's control off in its commit").toBe("Plan");
  expect(
    view.container.querySelector(VIEW_LOADS),
    "the view's place does not show that it loads in the press's commit"
  ).not.toBeNull();
  await act(async () => {});
  expect(sent("plan"), "the press sent Plan's read a second time").toHaveLength(1);
});

// Row 4. A view whose read was refused: the press's commit draws the failed read's line with its
// Retry, as at the base, and no read is sent until Retry.
test("a press on a view whose read was refused draws its failed read's line and sends nothing until Retry", async () => {
  const { view } = drawPage("conversation");
  await conversationLoaded(view);
  press(await control(view, "spec"));
  await heldCall("spec").refuse();
  await waitFor(() =>
    expect(
      view.container.querySelector(FAILED_READ_RETRY),
      "Spec drew no failed read's line once its read was refused"
    ).not.toBeNull()
  );
  press(await control(view, "conversation"));
  await waitFor(() => expect(view.container.querySelector(OUTLINE_ROW)).not.toBeNull());

  press(await control(view, "spec"));
  expect(currentControl(), "the press left Spec's control off in its commit").toBe("Spec");
  expect(
    view.container.querySelector(FAILED_READ_RETRY),
    "the press's commit drew no failed read's line with its Retry"
  ).not.toBeNull();
  await act(async () => {});
  expect(sent("spec"), "the press read a refused view again before Retry").toHaveLength(1);

  const retry = view.container.querySelector<HTMLElement>(FAILED_READ_RETRY);
  if (retry === null) throw new Error("the line holds no Retry");
  act(() => retry.click());
  await waitFor(() => expect(sent("spec"), "Retry sent no read of the view").toHaveLength(2));
});

// Row 5. The read a press sent answers: the commit after the answer draws the view's first screen,
// and the place's loading mark goes in that commit; no frame shows the rows and the mark together.
test("the read a press sent answers: one commit draws the view's first screen and takes the loading mark off", async () => {
  const { view } = drawPage("conversation");
  await conversationLoaded(view);
  press(await control(view, "spec"));
  expect(view.container.querySelector(VIEW_LOADS), "the press drew no loading mark").not.toBeNull();
  // Each change of the document is read as it lands: a frame is painted only between tasks, so a
  // look at each change sees any state a frame could show.
  const both: number[] = [];
  const watch = new MutationObserver(() => {
    const rows = view.container.querySelectorAll(SPEC_ROW).length;
    if (rows > 0 && view.container.querySelector(VIEW_LOADS) !== null) both.push(rows);
  });
  watch.observe(view.container, { attributes: true, childList: true, subtree: true });
  try {
    await heldCall("spec").answer(SPEC_VIEW);
    expect(
      view.container.querySelectorAll(SPEC_ROW).length,
      "the commit after the answer drew no statement"
    ).toBe(2);
    expect(
      view.container.querySelector(VIEW_LOADS),
      "the loading mark stayed once the view drew its first screen"
    ).toBeNull();
    expect(both, "a state held the view's rows and its loading mark together").toEqual([]);
  } finally {
    watch.disconnect();
  }
});

// Row 6. A press on another view while the read the first press sent is out: the second control
// reads as current in its own press's commit, the first read goes on and its answer is kept for
// its view, and the first view draws no row.
test("a press on another view while the first press's read is out turns the second on, and the first read's answer is kept", async () => {
  const { client, view } = drawPage("conversation");
  await conversationLoaded(view);
  press(await control(view, "spec"));
  await waitFor(() => expect(sent("spec")).toHaveLength(1));

  press(await control(view, "verification"));
  expect(currentControl(), "the second press left Verification's control off in its commit").toBe(
    "Verification"
  );
  expect(sent("spec")[0]?.signal?.aborted ?? false, "the second press called off Spec's read").toBe(
    false
  );
  await heldCall("spec").answer(SPEC_VIEW);
  expect(
    client.getQueryData(["issue-tree", KEY, "spec"]),
    "Spec's answer was not kept for its view"
  ).toBeDefined();
  expect(view.container.querySelector(SPEC_ROW), "Spec drew a row under Verification").toBeNull();
  expect(currentControl(), "Spec's answer turned Verification off").toBe("Verification");
});

// Row 7. A press back to Conversation draws Conversation's first screen in the press's commit from
// the tree the owner built (`issue-tree-owner.tsx`): no tree is built again and no read is sent.
// From Spec, whose owner holds Conversation's tree, and from Graph, whose owner held Graph's.
test("a press back to Conversation draws its first screen in the press's commit, builds no tree and sends nothing", async () => {
  const { view } = drawPage("conversation");
  await conversationLoaded(view);
  press(await control(view, "graph"));
  await heldCall("graph").answer(OUTLINE);
  await waitFor(() =>
    expect(view.container.querySelector(OUTLINE_ROW), "Graph drew no row").not.toBeNull()
  );
  // Graph's read is sent once, from the press's handler: the body of the Conversation the press
  // left watched that read, and leaving gave up no read in flight for Graph's readers to send again.
  expect(
    sent("graph").map((call) => call.asked),
    "the press on Graph did not send Graph's read once, from its handler"
  ).toEqual(["Conversation"]);
  press(await control(view, "spec"));
  await heldCall("spec").answer(SPEC_VIEW);
  await waitFor(() => expect(view.container.querySelector(SPEC_ROW)).not.toBeNull());

  for (const from of ["spec", "graph"] as const) {
    if (from === "graph") {
      press(await control(view, "graph"));
      await waitFor(() => expect(view.container.querySelector(OUTLINE_ROW)).not.toBeNull());
    }
    const before = log.length;
    const builds = spyOn(issueTreeModule, "buildIssueTree");
    try {
      press(await control(view, "conversation"));
      expect(currentControl(), `the press from ${from} left Conversation's control off`).toBe(
        "Conversation"
      );
      expect(
        view.container.querySelector(OUTLINE_ROW),
        `the press from ${from} drew no row of Conversation in its commit`
      ).not.toBeNull();
      expect(builds, `the press from ${from} built the tree again`).not.toHaveBeenCalled();
      await act(async () => {});
      expect(log.slice(before), `the press from ${from} sent a read`).toEqual([]);
    } finally {
      builds.mockRestore();
    }
  }
});

// Row 8. A press while the load's reads are still out: the control reads as current in the press's
// commit, and the view draws once the reads it needs answer, by rows 1 to 5.
test("a press during the load turns the view on in its commit, and the view draws once its reads answer", async () => {
  const { view } = drawPage("conversation");
  await waitFor(() => expect(sent("conversation")).toHaveLength(1));
  const toolbar = await view.findByRole("toolbar", { name: CONTROL_LABELS.presets });
  expect(
    view.container.querySelector(OUTLINE_ROW),
    "Conversation drew a row before its read answered"
  ).toBeNull();

  press(within(toolbar).getByRole("button", { name: PRESET_NAMES.spec }));
  expect(currentControl(), "the press during the load left Spec's control off").toBe("Spec");
  expect(
    view.container.querySelector(VIEW_LOADS),
    "the view's place does not show that it loads in the press's commit"
  ).not.toBeNull();
  expect(
    sent("spec").map((call) => call.asked),
    "Spec's read was not sent from the press's handler"
  ).toEqual(["Conversation"]);
  await heldCall("spec").answer(SPEC_VIEW);
  await heldCall("conversation").answer(OUTLINE);
  await waitFor(() =>
    expect(
      view.container.querySelectorAll(SPEC_ROW).length,
      "Spec drew no statement once its reads answered"
    ).toBe(2)
  );
  expect(view.container.querySelector(VIEW_LOADS), "the loading mark stayed").toBeNull();
});

// At 390 px the view row is one control, whose press opens its menu in the press's commit; a press
// on a view in it is a press on that view, as above.
test("at 390 px a press on the one control opens its menu in its commit, and a view pressed in it sends its read from the handler", async () => {
  const { view } = drawPage("conversation", PHONE_WIDTH);
  await conversationLoaded(view);
  const presets = await view.findByTestId("issue-presets");
  const chooser = presets.querySelector<HTMLElement>('button[aria-haspopup="menu"]');
  if (chooser === null) throw new Error("the row drew no one control");

  press(chooser);
  expect(
    presets.querySelector('[role="menu"]'),
    "the press on the one control opened no menu in its commit"
  ).not.toBeNull();
  // Each item is named by its name and the signal it draws, so it is found by its name's own words.
  const spec = [...presets.querySelectorAll<HTMLElement>('[role="menuitemradio"]')].find(
    (item) => item.querySelector("span[id]")?.textContent === PRESET_NAMES.spec
  );
  if (spec === undefined) throw new Error("the menu holds no Spec");
  press(spec);
  expect(
    sent("spec").map((call) => call.asked),
    "Spec's read was not sent from the press's handler"
  ).toEqual(["Conversation"]);
  expect(
    presets.querySelector('button[aria-haspopup="menu"] [data-preset-name]')?.textContent,
    "the one control does not name Spec in the press's commit"
  ).toBe("Spec");
  expect(
    view.container.querySelector(VIEW_LOADS),
    "the view's place does not show that it loads in the press's commit"
  ).not.toBeNull();
});
