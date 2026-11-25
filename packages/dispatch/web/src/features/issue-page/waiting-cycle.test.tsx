import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, type RenderResult, render, screen, waitFor } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { graphNode } from "../../__tests__/graph-fixture";
import { answerRequests } from "../../__tests__/network";
import {
  artifactRecordHandler,
  decisionsAndReadMarkHandlers,
} from "../../__tests__/proof-document";
import { api } from "../../api/client";
import type { GraphReferences, IssueAskRow, IssueDetails, IssueReferences } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { answeredAsk } from "../inbox/ask-card-fixture";
import { parseIssuePath } from "../refs/routes";
import { APPROVAL_CHIP } from "./approval-chip-box";
import {
  artifact,
  DECISIONS_DOCUMENT,
  decision,
  decisionNode,
  decisionsRead,
  issueDetails,
  KEY,
  READER,
  shaped,
  specRef,
} from "./decisions-filter/decisions-fixtures";
import { presetPath } from "./issue-address";
import { HeaderSlot } from "./slots/header";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import { AppProviders, focusedRow, stubReads } from "./slots/stage-fixtures";
import { issueAsksQuery } from "./useIssueTree";

/**
 * The count of what waits on the reader (part N's R5, the plan's ST-3), one case per row: the
 * header's own count, pressed by the pointer or by Enter, over the page's own stage, whose
 * Decisions view walks what waits, with the header's approval control as a stop of the cycle
 * after the last decision where an open approval request waits on the reader.
 */

/** The spec, whose approval request the header draws a control for, and a document of no role. */
const SPEC = { ...artifact("artifact-spec", "spec", true), approval: awaiting() };
const NOTES = artifact("artifact-notes", "notes-md");

/** An approval that waits for a review of the document's first version. */
function awaiting(): NonNullable<IssueDetails["artifacts"]>[number]["approval"] {
  return { latest_version: 1, state: "awaiting" };
}

/** One open question of the issue whose turn is the reader's, as the asks read answers it. */
function question(id: string, words: string): IssueAskRow {
  return {
    ...answeredAsk({
      answer: null,
      id,
      issue_key: KEY,
      question: words,
      state: "open",
      waiting_on_person: READER,
    }),
    last_reply: null,
  };
}

/** The issue's two questions that wait on the reader. */
const QUESTIONS: readonly IssueAskRow[] = [
  question("q-first", "Which width does the header condense at?"),
  question("q-second", "Does the rail stay on a phone?"),
];

/** An open approval request of `document` whose turn is the reader's. */
function approvalOf(document: string): IssueAskRow {
  return {
    ...question("ask-approval", "Approve the document?"),
    artifact_id: document,
    kind: "approval",
  };
}

/** The issue with its spec, its decisions document and a document of no role, and the open asks
 *  the header counts: `asks`, the two questions and an approval request of the spec unless
 *  given. */
function issueWith(asks: readonly IssueAskRow[] = [...QUESTIONS, approvalOf(SPEC.id)]) {
  return {
    ...issueDetails(),
    artifacts: [SPEC, DECISIONS_DOCUMENT, NOTES],
    open_asks: [...asks],
  } satisfies IssueDetails;
}

/** Two decisions of agents, each tied to the spec's statement and waiting for the reader's
 *  review: the review list. */
const REVIEW = decisionsRead([
  decision("d-older", { changed_at: "2026-11-10T12:00:00Z", links: [shaped(specRef("r1"))] }),
  decision("d-newer", { changed_at: "2026-11-11T12:00:00Z", links: [shaped(specRef("r1"))] }),
]);

/** Where the router is. */
function Where(): ReactNode {
  const { pathname, search } = useLocation();
  return <output data-testid="where">{`${pathname}${search}`}</output>;
}

/** The header over the stage at the address the router holds, under the page's own providers,
 *  as the page's root draws them, for `issue`. */
function Page({ issue }: { issue: IssueDetails }): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <ProvidersSlot issue={issue} route={route}>
      <HeaderSlot issue={issue} route={route} />
      <StageSlot issue={issue} route={route} />
      <Where />
    </ProvidersSlot>
  );
}

let restoreReads: { mockRestore(): void }[] = [];

beforeEach(() => {
  restoreReads = [
    ...stubReads(),
    spyOn(api, "getIssueCoverage").mockResolvedValue({
      gaps: [],
      has_plan: false,
      issue: KEY,
      requirements: [
        {
          block_id: "r1",
          checks: 0,
          gaps: [],
          plan_tasks: 0,
          ref: specRef("r1"),
          text: "The page draws the header.",
        },
      ],
      spec_artifact_id: SPEC.id,
    }),
    spyOn(api, "getIssueDecisions").mockResolvedValue(REVIEW),
    spyOn(api, "listIssueAsks").mockResolvedValue([...QUESTIONS]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "getArtifactText").mockResolvedValue({
      markdown: "The notes",
      token: "sha256:notes",
      version: null,
    }),
  ];
  answerRequests(
    artifactRecordHandler((id) => issueWith().artifacts.find((each) => each.id === id)),
    http.get("/api/v1/artifacts/:id/blocks", () => Response.json([])),
    http.get("/api/v1/issues/:key/references", () =>
      Response.json({
        linked_from: [],
        links: [],
        members: [],
        truncated: false,
      } satisfies IssueReferences)
    ),
    http.get("/api/v1/references", ({ request }) =>
      Response.json({
        edges: [],
        node: graphNode({
          id: new URL(request.url).searchParams.get("to") ?? "",
          kind: "artifact",
        }),
      } satisfies GraphReferences)
    ),
    ...decisionsAndReadMarkHandlers()
  );
});

afterEach(() => {
  for (const read of restoreReads) read.mockRestore();
});

/** The page at `address` for `issue`, with its query client. */
function drawPage(
  address: string,
  issue: IssueDetails = issueWith()
): RenderResult & { readonly client: QueryClient } {
  // The header reads the issue itself, for its primary document, whose approval control it draws.
  restoreReads.push(spyOn(api, "getIssue").mockResolvedValue(issue));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[address]}>
      <AppProviders client={client}>
        <DocumentRuntime.Provider value={fakeDocumentRuntime({ text: "The notes" }).runtime}>
          <Page issue={issue} />
        </DocumentRuntime.Provider>
      </AppProviders>
    </MemoryRouter>
  );
  return { ...view, client };
}

/** The address the router is at. */
function address(): string {
  return screen.getByTestId("where").textContent ?? "";
}

/** The header's count of what waits on the reader, once it is drawn. */
function count(): Promise<HTMLElement> {
  return screen.findByTestId("issue-whose-turn");
}

/** Presses the count as the reader does: by the pointer, or by Enter with the focus on it. */
async function pressCount(user: UserEvent, by: "pointer" | "Enter"): Promise<void> {
  const control = await count();
  if (by === "pointer") {
    await user.click(control);
    return;
  }
  await act(async () => control.focus());
  await user.keyboard("{Enter}");
}

/** The header's approval control of the document `artifactId`. */
function chipOf(artifactId: string): HTMLElement | null {
  return document.querySelector<HTMLElement>(
    `${APPROVAL_CHIP}[data-approval-artifact="${artifactId}"]`
  );
}

/** Each row the Decisions view draws in `part`, by its node id, once it draws one. */
async function rowsOf(view: RenderResult, part: string): Promise<string[]> {
  return waitFor(() => {
    const rows = [
      ...view.container.querySelectorAll<HTMLElement>(
        `[data-decisions-part="${part}"] [data-node-row]`
      ),
    ].map((row) => row.dataset.nodeId ?? "");
    if (rows.length === 0) throw new Error(`the Decisions view draws no row in ${part}`);
    return rows;
  });
}

/** Waits for the focus to rest on the row of `node`. */
async function focusOn(node: string, what: string): Promise<void> {
  await waitFor(() => expect(focusedRow(), what).toBe(node));
}

/** Waits for the focus to rest on the header's approval control of the document `artifactId`. */
async function focusOnChip(artifactId: string, what: string): Promise<void> {
  await waitFor(() => {
    const chip = chipOf(artifactId);
    expect(chip, "the header draws no approval control of the document").not.toBeNull();
    expect(document.activeElement, what).toBe(chip);
  });
}

const DECISIONS = presetPath(KEY, "decisions");
const OLDER = decisionNode("d-older").id;
const NEWER = decisionNode("d-newer").id;

test("questions and decisions wait, and an approval request: the first press turns the Decisions view on with the focus on the first waiting question", async () => {
  const user = userEvent.setup();
  const view = drawPage(`/issues/${KEY}`);
  try {
    await pressCount(user, "pointer");
    await waitFor(() => expect(address()).toBe(DECISIONS));
    const [first] = await rowsOf(view, "open_questions");
    expect(first).toBe("q-first");
    await focusOn("q-first", "the first press left the focus off the first waiting question");
  } finally {
    view.unmount();
  }
});

test("after the last question: the next press puts the focus on the first decision waiting for review", async () => {
  const user = userEvent.setup();
  const view = drawPage(`/issues/${KEY}`);
  try {
    await pressCount(user, "Enter");
    await focusOn("q-first", "the first press");
    await pressCount(user, "Enter");
    await focusOn("q-second", "the second press");
    const [firstDecision] = await rowsOf(view, "review");
    expect(firstDecision).toBe(NEWER);
    await pressCount(user, "Enter");
    await focusOn(NEWER, "the press after the last question");
  } finally {
    view.unmount();
  }
});

test("after the last decision: the next press puts the focus on the header's approval control of the waiting request", async () => {
  const user = userEvent.setup();
  const view = drawPage(`/issues/${KEY}`);
  try {
    for (const [node, by] of [
      ["q-first", "pointer"],
      ["q-second", "Enter"],
      [NEWER, "pointer"],
      [OLDER, "Enter"],
    ] as const) {
      await pressCount(user, by);
      await focusOn(node, `the press that goes to ${node}`);
    }
    await pressCount(user, "pointer");
    await focusOnChip(SPEC.id, "the press after the last decision left the approval control");
    expect(address(), "the approval stop left the Decisions view").toBe(DECISIONS);
  } finally {
    view.unmount();
  }
});

test("on the approval control: the next press puts the focus on the first waiting question again", async () => {
  const user = userEvent.setup();
  const view = drawPage(`/issues/${KEY}`);
  try {
    for (const node of ["q-first", "q-second", NEWER, OLDER]) {
      await pressCount(user, "Enter");
      await focusOn(node, `the press that goes to ${node}`);
    }
    await pressCount(user, "Enter");
    await focusOnChip(SPEC.id, "the press after the last decision");
    await pressCount(user, "Enter");
    await focusOn("q-first", "the press after the approval control did not start the cycle again");
    // And round again, by the pointer, to the approval control.
    for (const node of ["q-second", NEWER, OLDER]) {
      await pressCount(user, "pointer");
      await focusOn(node, `the second round's press that goes to ${node}`);
    }
    await pressCount(user, "pointer");
    await focusOnChip(SPEC.id, "the second round's press after the last decision");
  } finally {
    view.unmount();
  }
});

test("only an approval request waits: each press puts the focus on the approval control", async () => {
  const user = userEvent.setup();
  restoreReads.push(
    spyOn(api, "getIssueDecisions").mockResolvedValue(decisionsRead([])),
    spyOn(api, "listIssueAsks").mockResolvedValue([])
  );
  const view = drawPage(`/issues/${KEY}`, issueWith([approvalOf(SPEC.id)]));
  try {
    await waitFor(async () => expect((await count()).textContent).toContain("(1)"));
    for (const by of ["pointer", "Enter"] as const) {
      await pressCount(user, by);
      await focusOnChip(SPEC.id, `a press by ${by} left the approval control`);
      expect(address(), "a press with only an approval waiting turned a view on").toBe(
        `/issues/${KEY}`
      );
    }
  } finally {
    view.unmount();
  }
});

test("a document open: the first press closes it, and the first waiting item takes the focus", async () => {
  const user = userEvent.setup();
  const view = drawPage(`/issues/${KEY}/artifacts/${NOTES.slug}`);
  try {
    expect(await screen.findByRole("heading", { name: NOTES.name })).toBeVisible();
    await pressCount(user, "pointer");
    await waitFor(() => expect(address()).toBe(DECISIONS));
    expect(screen.queryByRole("heading", { name: NOTES.name })).toBeNull();
    await focusOn("q-first", "the press from the document left the focus off the first item");
  } finally {
    view.unmount();
  }
});

test("an item answered between presses: the next press goes to the next item still waiting, by the order at the press", async () => {
  const user = userEvent.setup();
  const view = drawPage(`/issues/${KEY}`);
  try {
    await pressCount(user, "pointer");
    await focusOn("q-first", "the first press");
    await pressCount(user, "pointer");
    await focusOn("q-second", "the second press");
    // The reader answers the question they are on, elsewhere: it waits on them no more.
    await act(async () =>
      view.client.setQueryData(issueAsksQuery(KEY).queryKey, [
        QUESTIONS[0],
        { ...QUESTIONS[1], state: "answered" },
      ])
    );
    await waitFor(() =>
      expect(
        [
          ...view.container.querySelectorAll<HTMLElement>(
            '[data-decisions-part="open_questions"] [data-node-row]'
          ),
        ].map((row) => row.dataset.nodeId),
        "the answer did not reach the Decisions view"
      ).toEqual(["q-first"])
    );
    await pressCount(user, "Enter");
    await focusOn(NEWER, "the press after an answered question went back to the first question");
  } finally {
    view.unmount();
  }
});

test("the reads still out: a press turns the Decisions view on, and the focus lands once the reads hold", async () => {
  const user = userEvent.setup();
  const asksOut = Promise.withResolvers<IssueAskRow[]>();
  restoreReads.push(spyOn(api, "listIssueAsks").mockReturnValue(asksOut.promise));
  const view = drawPage(`/issues/${KEY}`);
  try {
    await pressCount(user, "pointer");
    await waitFor(() => expect(address()).toBe(DECISIONS));
    // The decisions are in and drawn, the questions not yet.
    await rowsOf(view, "review");
    expect(focusedRow(), "the press landed before the questions were in").toBeUndefined();
    await act(async () => asksOut.resolve([...QUESTIONS]));
    await focusOn("q-first", "the press did not land on the first waiting question");
  } finally {
    view.unmount();
  }
});

test("an approval with no control in the header: after the last decision the next press goes to the first waiting question", async () => {
  const user = userEvent.setup();
  // The request is for a document of no role, which the header draws no approval control for.
  const view = drawPage(`/issues/${KEY}`, issueWith([...QUESTIONS, approvalOf(NOTES.id)]));
  try {
    for (const node of ["q-first", "q-second", NEWER, OLDER]) {
      await pressCount(user, "pointer");
      await focusOn(node, `the press that goes to ${node}`);
    }
    expect(chipOf(NOTES.id)).toBeNull();
    await pressCount(user, "pointer");
    await focusOn("q-first", "the request with no control in the header was a stop");
  } finally {
    view.unmount();
  }
});
