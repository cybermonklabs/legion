import { afterEach, expect, spyOn, test } from "bun:test";
import {
  GOING_ERROR_ANSWER_DID_NOT_ARRIVE,
  GOING_ERROR_ANSWER_FAILED,
  GOING_ERROR_NO_MODEL_CONFIGURED,
  GOING_EVENT_CHANGED,
  GOING_EVENT_CUT,
  GOING_EVENT_END,
  GOING_EVENT_ERROR,
  GOING_EVENT_ITEM,
  GOING_EVENT_NONE,
  GOING_EVENT_TEXT,
  type GraphFrame,
} from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { graphNode } from "../../../__tests__/graph-fixture";
import { ApiError, api } from "../../../api/client";
import { queryRetry } from "../../../api/query-client";
import type { GraphEdge, GraphReferences } from "../../../api/types";
import { RUN_PATH } from "../../refs/routes";
import { NotFoundPage } from "../../shell/NotFoundPage";
import { drawnLine, firstLineOf } from "../issue-tree";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  NODE_REFERENCES_WORDS,
  RUN_PAGE_WORDS,
  STATE_LINES,
  sentenceCase,
  wordsIn,
} from "../page-words";
import { plainWords } from "../tree-drafts";
import { RunPage } from "./RunPage";

const RUN = "3f2b8c1e-5d4a-4b6f-9e2d-1a7c0b9e8f43";
const ASKED_AT = "2026-11-17T09:00:00Z";
const RUN_WORD = sentenceCase(NODE_REFERENCES_WORDS.kinds.interaction);

/** The run's read: its node, carrying `frames` as the server's answer does, and `edges`. */
function answerOf(
  frames: readonly GraphFrame[],
  edges: readonly GraphEdge[] = [],
  question = "When does the release ship?"
): GraphReferences {
  return {
    edges: [...edges],
    node: graphNode({
      frames: [...frames],
      id: RUN,
      kind: "interaction",
      made_at: ASKED_AT,
      ref: `dispatch://interaction/${RUN}`,
      text: question,
    }),
  };
}

/** A frame of the answer. */
function frame(sequence: number, name: string, data: GraphFrame["data"] = null): GraphFrame {
  return { data, name, sequence };
}

let restore: (() => void) | undefined;
afterEach(() => {
  restore?.();
  restore = undefined;
});

/** The page at the run `id`'s address, its one read answered by `answer`, in a client of its own
 *  that waits for no retry. */
function draw(answer: () => Promise<GraphReferences>, id = RUN) {
  const getReferences = spyOn(api, "getReferences").mockImplementation(answer);
  const client = new QueryClient({ defaultOptions: { queries: { retryDelay: 0 } } });
  const view = render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[`/runs/${id}`]}>
        <Routes>
          <Route element={<RunPage />} path={RUN_PATH} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  );
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
    client.clear();
  };
  return { getReferences, view };
}

/** `NotFoundPage` drawn on its own: its markup, the title it gives the tab, and the name of its
 *  one link. */
function notFoundAlone(): {
  readonly link: string;
  readonly markup: string;
  readonly title: string;
} {
  const alone = render(
    <MemoryRouter>
      <NotFoundPage />
    </MemoryRouter>
  );
  const drawn = {
    link: within(alone.container).getByRole("link").textContent ?? "",
    markup: alone.container.innerHTML,
    title: document.title,
  };
  alone.unmount();
  return drawn;
}

/** Every word the run's page says of its own, none of which the not-found page may say. */
const RUN_PAGE_PHRASES = [
  RUN_WORD,
  RUN_PAGE_WORDS.question,
  RUN_PAGE_WORDS.what_changed,
  ANSWER_LINES.loading_older,
  ANSWER_LINES.run_unreadable,
];

test("a 404 draws NotFoundPage", async () => {
  const alone = notFoundAlone();
  for (const refusal of [new ApiError(404, { code: "NOT_FOUND" }), new ApiError(400, {})]) {
    const { getReferences, view } = draw(() => Promise.reject(refusal));
    await waitFor(() => expect(view.container.innerHTML).toBe(alone.markup));
    expect(document.title).toBe(alone.title);
    for (const phrase of RUN_PAGE_PHRASES) {
      expect(view.container.textContent).not.toContain(phrase);
    }
    // A run that is missing is asked for once: a retry finds it no more than the read did.
    expect(getReferences).toHaveBeenCalledTimes(1);
    restore?.();
    restore = undefined;
  }
  // An id that is no lowercase uuid names no run, and nothing is asked for it.
  for (const id of ["not-a-run", RUN.toUpperCase()]) {
    const { getReferences, view } = draw(() => Promise.resolve(answerOf([])), id);
    expect(view.container.innerHTML).toBe(alone.markup);
    expect(getReferences).toHaveBeenCalledTimes(0);
    restore?.();
    restore = undefined;
  }
});

// The run's read keeps its own retry rule in a client whose default is another: a missing run
// (400 or 404) is asked for once, and any other failure as often as `queryRetry` asks every read
// again (`main.tsx`).
test("a missing run is asked for once, and any other failure as every read is", async () => {
  const failure = new ApiError(500, {});
  let asked = 1;
  while (queryRetry(asked - 1, failure)) asked += 1;
  for (const [refusal, times] of [
    [new ApiError(404, { code: "NOT_FOUND" }), 1],
    [new ApiError(400, {}), 1],
    [failure, asked],
  ] as const) {
    const { getReferences } = draw(() => Promise.reject(refusal));
    // The loading line stands while the read is asked again, and goes once it has failed.
    await waitFor(() => expect(screen.queryByText(ANSWER_LINES.loading_older)).toBeNull());
    expect(getReferences, `the asks of a ${refusal.status}`).toHaveBeenCalledTimes(times);
    restore?.();
    restore = undefined;
  }
});

test("no frame draws its event name or error code", async () => {
  const codes = [
    GOING_ERROR_NO_MODEL_CONFIGURED,
    GOING_ERROR_ANSWER_DID_NOT_ARRIVE,
    GOING_ERROR_ANSWER_FAILED,
    "SOMETHING_ELSE",
  ];
  const frames = [
    frame(1, GOING_EVENT_TEXT, { text: "The release ships on Friday\\." }),
    frame(2, GOING_EVENT_ITEM, {
      href: "/issues/CORE-9",
      id: "CORE-9",
      kind: "issue",
      title: "The release plan",
    }),
    frame(3, GOING_EVENT_CUT, { words: 120 }),
    frame(4, GOING_EVENT_CHANGED, { dropped: 2 }),
    frame(5, GOING_EVENT_NONE),
    ...codes.map((code, index) => frame(6 + index, GOING_EVENT_ERROR, { code })),
    frame(10, GOING_EVENT_END, { items: 1, words: 120 }),
  ];
  const { getReferences, view } = draw(() => Promise.resolve(answerOf(frames)));
  await screen.findByRole("heading", { level: 1, name: RUN_WORD });
  expect(getReferences.mock.calls).toEqual([
    [`dispatch://interaction/${RUN}`, { kind: "changed_by_run" }],
  ]);
  // The item is its title, linked, and neither its kind nor its id.
  expect(screen.getByRole("link", { name: "The release plan" })).toHaveAttribute(
    "href",
    "/issues/CORE-9"
  );

  const text = view.container.textContent ?? "";
  expect(wordsIn(text)).not.toContain("issue");
  for (const token of ["CORE-9", ...codes, "changed_by_run", "interaction", RUN, "dispatch://"]) {
    expect(text).not.toContain(token);
  }
  // Each frame by its sentence: `ANSWER_FAILED` and the code the page does not know both say
  // the answer failed.
  for (const line of [
    `${STATE_LINES.run_cut.before}120${STATE_LINES.run_cut.after}`,
    `${STATE_LINES.run_changed.before}2${STATE_LINES.run_changed.after}`,
    STATE_LINES.run_no_model,
    STATE_LINES.run_did_not_arrive,
  ]) {
    expect(screen.getByText(line)).toBeInTheDocument();
  }
  expect(screen.getAllByText(STATE_LINES.run_failed)).toHaveLength(2);
});

test("a failed read other than 404 draws its one sentence with Retry", async () => {
  const user = userEvent.setup();
  const { getReferences } = draw(() => Promise.reject(new ApiError(500, {})));
  expect(await screen.findByRole("alert")).toHaveTextContent(ANSWER_LINES.run_unreadable);
  expect(screen.queryByRole("heading", { name: RUN_WORD })).toBeNull();
  const asked = getReferences.mock.calls.length;
  await user.click(screen.getByRole("button", { name: CONTROL_LABELS.retry }));
  await waitFor(() => expect(getReferences.mock.calls.length).toBeGreaterThan(asked));
});

// A retry of a read that holds no answer drops the read's failure as it starts, so the line and
// the Retry the reader's focus is on went with it, and the focus fell to the page's body, where
// no keyboard action may leave it.
test("a Retry the reader pressed keeps its line until the read lands, then hands the focus to the heading", async () => {
  const user = userEvent.setup();
  const { getReferences } = draw(() => Promise.reject(new ApiError(500, {})));
  expect(await screen.findByRole("alert")).toHaveTextContent(ANSWER_LINES.run_unreadable);
  const landing = Promise.withResolvers<GraphReferences>();
  getReferences.mockImplementation(() => landing.promise);
  const asked = getReferences.mock.calls.length;
  const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
  act(() => retry.focus());
  await user.keyboard("{Enter}");
  await waitFor(() => expect(getReferences).toHaveBeenCalledTimes(asked + 1));

  // The read is out: the line and its Retry stay, the Retry disabled and holding the focus, and
  // no loading line stands in their place.
  expect(screen.getByRole("alert"), "the line went while its retry was out").toHaveTextContent(
    ANSWER_LINES.run_unreadable
  );
  expect(retry, "Retry went while the read it sent was out").toBeInTheDocument();
  expect(retry, "Retry is not disabled while the read it sent is out").toBeDisabled();
  expect(document.activeElement, "Retry lost the focus while its read was out").toBe(retry);
  expect(screen.queryByText(ANSWER_LINES.loading_older)).toBeNull();

  // The read answers: the answer replaces the line, and the focus goes to the page's heading.
  await act(async () => {
    landing.resolve(answerOf([]));
  });
  const heading = await screen.findByRole("heading", { level: 1, name: RUN_WORD });
  expect(screen.queryByRole("alert")).toBeNull();
  expect(document.activeElement, "the answer left the focus off the page's heading").toBe(heading);
});

// A Retry whose read the server answers 404 draws the not-found page in place of the line and
// the Retry the reader's focus is on, and the focus goes to that page's link rather than fall
// to the page's body, where no keyboard action may leave it.
test("a Retry the server answers 404 hands the focus to the not-found page's link", async () => {
  const alone = notFoundAlone();
  const user = userEvent.setup();
  const { getReferences, view } = draw(() => Promise.reject(new ApiError(500, {})));
  expect(await screen.findByRole("alert")).toHaveTextContent(ANSWER_LINES.run_unreadable);
  getReferences.mockImplementation(() => Promise.reject(new ApiError(404, { code: "NOT_FOUND" })));
  act(() => screen.getByRole("button", { name: CONTROL_LABELS.retry }).focus());
  await user.keyboard("{Enter}");

  await waitFor(() => expect(view.container.innerHTML).toContain(alone.markup));
  for (const phrase of RUN_PAGE_PHRASES) {
    expect(view.container.textContent).not.toContain(phrase);
  }
  const link = within(view.container).getByRole("link");
  expect(link).toHaveAccessibleName(alone.link);
  expect(document.activeElement, "the not-found page left the focus off its link").toBe(link);
});

test("the answered page draws the question as typed, the time and what it changed", async () => {
  const question = "**not bold** [x](y)";
  const excerpt = "Ship the **release** on Friday. Then rest.";
  const changed: GraphEdge = {
    created_at: "2026-11-17T09:00:05Z",
    direction: "in",
    excerpt: { text: excerpt },
    kind: "changed_by_run",
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: "dispatch://CORE-1" }),
    source_seq: null,
  };
  const frames = [
    frame(1, GOING_EVENT_TEXT, { text: "Done\\." }),
    frame(2, GOING_EVENT_END, { items: 0, words: 1 }),
  ];
  draw(() => Promise.resolve(answerOf(frames, [changed], question)));
  await screen.findByRole("heading", { level: 1, name: RUN_WORD });
  expect(document.title).toBe(`${RUN_WORD} · Dispatch`);

  // The question, character for character: no strong text and no link made of it.
  const asked = screen.getByRole("region", { name: RUN_PAGE_WORDS.question });
  expect(within(asked).getByText(question).textContent).toBe(question);
  expect(asked.querySelector("strong")).toBeNull();
  expect(within(asked).queryByRole("link")).toBeNull();

  // When it was asked.
  expect(document.querySelector(`time[datetime="${ASKED_AT}"]`)).not.toBeNull();

  // The issue it changed, by its kind's word and its first line, which links to it.
  const what = screen.getByRole("region", { name: RUN_PAGE_WORDS.what_changed });
  expect(within(what).getByRole("listitem")).toHaveTextContent(
    sentenceCase(NODE_REFERENCES_WORDS.kinds.issue)
  );
  expect(
    within(what).getByRole("link", { name: drawnLine(firstLineOf(plainWords(excerpt))) })
  ).toHaveAttribute("href", "/issues/CORE-1");
});

// A node of a kind the page has no word for is named by its line alone: no schema token
// stands where its kind's word would.
test("a changed node of a kind the page has no word for is named by its line alone", async () => {
  const excerpt = "Drop the release if the checks fail.";
  const risk: GraphEdge = {
    created_at: "2026-11-17T09:00:05Z",
    direction: "in",
    excerpt: { text: excerpt },
    kind: "changed_by_run",
    node: graphNode({ id: "4d1e2f3a-6b5c-4d7e-8f90-a1b2c3d4e5f6", kind: "risk" }),
    source_seq: null,
  };
  draw(() => Promise.resolve(answerOf([], [risk])));
  await screen.findByRole("heading", { level: 1, name: RUN_WORD });
  const what = screen.getByRole("region", { name: RUN_PAGE_WORDS.what_changed });
  const entry = within(what).getByRole("listitem");
  expect(entry.textContent).toBe(drawnLine(firstLineOf(plainWords(excerpt))));
  expect(wordsIn(entry.textContent ?? "")).not.toContain("risk");
});
