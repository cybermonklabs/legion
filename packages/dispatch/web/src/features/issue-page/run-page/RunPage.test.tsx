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
import { Profiler, type ReactElement } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { graphNode } from "../../../__tests__/graph-fixture";
import { ApiError, api } from "../../../api/client";
import { whoAmIQuery } from "../../../api/queries";
import { queryRetry } from "../../../api/query-client";
import type { GraphEdge, GraphReferences, ReferenceTargets } from "../../../api/types";
import { type PublishedReferences, ReferenceNamesProvider } from "../../refs/reference-names";
import { referencesQuery } from "../../refs/reference-target";
import { parseNodeReference, RUN_PATH, shortForm } from "../../refs/routes";
import { NotFoundPage } from "../../shell/NotFoundPage";
import { userPreferenceStorageKey } from "../../shell/userPreference";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  NODE_REFERENCES_WORDS,
  RUN_PAGE_WORDS,
  STATE_LINES,
  sentenceCase,
  wordsIn,
} from "../page-words";
import { Publisher, publishedNames } from "../projection-fixture";
import { RunPage } from "./RunPage";
import { RunRoute } from "./RunRoute";

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

/** How a case draws the page: the route's `element`, `RunPage` where none is given; the address
 *  it opens at (`path`), the run `RUN`'s where none is given; and what another page publishes to
 *  the page's store of names beside it (`published`), where a case gives it. */
interface PageOptions {
  readonly element?: ReactElement;
  readonly path?: string;
  readonly published?: PublishedReferences;
}

/** The page as `options` draw it (`PageOptions`), reading through `client`. */
function runPage(
  client: QueryClient,
  { element = <RunPage />, path = `/runs/${RUN}`, published }: PageOptions = {}
): ReactElement {
  return (
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>
        {published === undefined ? null : <Publisher published={published} />}
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route element={element} path={RUN_PATH} />
          </Routes>
        </MemoryRouter>
      </ReferenceNamesProvider>
    </QueryClientProvider>
  );
}

/** The page as `options` draw it (`PageOptions`), its one read answered by `answer`, in a client
 *  of its own that waits for no retry. */
function draw(answer: () => Promise<GraphReferences>, options: PageOptions = {}) {
  const getReferences = spyOn(api, "getReferences").mockImplementation(answer);
  const client = new QueryClient({ defaultOptions: { queries: { retryDelay: 0 } } });
  const view = render(runPage(client, options));
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
    client.clear();
  };
  return { client, getReferences, view };
}

/** What a reader sees of `NotFoundPage`: its heading, its one link by its name and address, all
 *  its words, and the title it gives the tab. */
interface NotFoundSeen {
  readonly heading: string;
  readonly href: string;
  readonly link: string;
  readonly text: string;
  readonly title: string;
}

/** `NotFoundPage` drawn on its own, as a reader sees it. */
function notFoundAlone(): NotFoundSeen {
  const alone = render(
    <MemoryRouter>
      <NotFoundPage />
    </MemoryRouter>
  );
  const link = within(alone.container).getByRole("link");
  const drawn = {
    heading: within(alone.container).getByRole("heading").textContent ?? "",
    href: link.getAttribute("href") ?? "",
    link: link.textContent ?? "",
    text: alone.container.textContent ?? "",
    title: document.title,
  };
  alone.unmount();
  return drawn;
}

/** That `container` draws what a reader sees of `NotFoundPage` drawn on its own (`alone`): its one
 *  heading, its one link by its name and address, and its words and no others. Returns the link. */
function expectNotFoundPage(container: HTMLElement, alone: NotFoundSeen): HTMLElement {
  expect(within(container).getByRole("heading")).toHaveAccessibleName(alone.heading);
  const link = within(container).getByRole("link");
  expect(link).toHaveAccessibleName(alone.link);
  expect(link).toHaveAttribute("href", alone.href);
  expect(container.textContent).toBe(alone.text);
  return link;
}

test("a 404 draws NotFoundPage", async () => {
  const alone = notFoundAlone();
  for (const refusal of [new ApiError(404, { code: "NOT_FOUND" }), new ApiError(400, {})]) {
    const { getReferences, view } = draw(() => Promise.reject(refusal));
    await screen.findByRole("heading", { name: alone.heading });
    expectNotFoundPage(view.container, alone);
    expect(document.title).toBe(alone.title);
    // A run that is missing is asked for once: a retry finds it no more than the read did.
    expect(getReferences).toHaveBeenCalledTimes(1);
    restore?.();
    restore = undefined;
  }
  // An id that is no lowercase uuid names no run, and nothing is asked for it.
  for (const id of ["not-a-run", RUN.toUpperCase()]) {
    const { getReferences, view } = draw(() => Promise.resolve(answerOf([])), {
      path: `/runs/${id}`,
    });
    expectNotFoundPage(view.container, alone);
    expect(getReferences).toHaveBeenCalledTimes(0);
    restore?.();
    restore = undefined;
  }
});

// Decision 533 (root spec v184's R33): a run's address that carries `?issue_page=new` draws the
// run's page for a signed-in reader whose setting is off, as an issue's address that carries it
// draws the new page for them, and nothing is stored. The run's address without it follows the
// setting, and with nobody signed in the parameter opens nothing.
test("a signed-in reader whose setting is off has the run's page at its address with ?issue_page=new, and NotFoundPage at its address without it", async () => {
  const alone = notFoundAlone();
  const stored = userPreferenceStorageKey("alice", "issue.page");
  window.localStorage.removeItem(stored);
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(answerOf([]));
  /** `RunRoute` at `path`, for alice where `signedIn` and for nobody otherwise, in a client that
   *  never reads the sign-in again once it holds an answer. */
  const routeAt = async (path: string, signedIn: boolean) => {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
    });
    if (signedIn) {
      client.setQueryData(whoAmIQuery().queryKey, {
        event_head: "0",
        kind: "user",
        login: "alice",
      });
    } else {
      // The unit network answers the sign-in read with Dispatch's own "no identity"
      // (`network.ts`).
      await client.prefetchQuery(whoAmIQuery());
    }
    const view = render(runPage(client, { element: <RunRoute />, path }));
    return {
      close: () => {
        view.unmount();
        client.clear();
      },
      container: view.container,
    };
  };
  try {
    const opened = await routeAt(`/runs/${RUN}?issue_page=new`, true);
    expect(
      await screen.findByRole("heading", { level: 1, name: RUN_WORD }),
      "the run's page at its address with the parameter"
    ).toBeInTheDocument();
    opened.close();
    expect(window.localStorage.getItem(stored), "what the parameter stored").toBeNull();

    const plain = await routeAt(`/runs/${RUN}`, true);
    expectNotFoundPage(plain.container, alone);
    plain.close();

    const signedOut = await routeAt(`/runs/${RUN}?issue_page=new`, false);
    expectNotFoundPage(signedOut.container, alone);
    signedOut.close();
    expect(getReferences, "the run's reads").toHaveBeenCalledTimes(1);
  } finally {
    getReferences.mockRestore();
    window.localStorage.removeItem(stored);
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

// An item is linked only where the page opens it as the reader expects: a path on Dispatch's
// own origin, or an `http:` or `https:` URL. A `javascript:` href would run a script as the reader
// follows it, and `//host` would open another host, so each draws its title alone.
test("an item links only a path on Dispatch's origin or an http(s) URL", async () => {
  const items = [
    { href: "javascript:alert(1)", title: "Run a script" },
    { href: "//elsewhere.example/plan", title: "Another host" },
    { href: "https://example.com/release", title: "The release notes" },
    { href: "/issues/CORE-9", title: "The release plan" },
  ];
  draw(() =>
    Promise.resolve(answerOf(items.map((item, index) => frame(index + 1, GOING_EVENT_ITEM, item))))
  );
  await screen.findByRole("heading", { level: 1, name: RUN_WORD });
  for (const title of ["Run a script", "Another host"]) {
    expect(screen.getByText(title).closest("a"), `${title} was drawn in a link`).toBeNull();
  }
  expect(screen.getByRole("link", { name: "The release notes" })).toHaveAttribute(
    "href",
    "https://example.com/release"
  );
  expect(screen.getByRole("link", { name: "The release plan" })).toHaveAttribute(
    "href",
    "/issues/CORE-9"
  );
  expect(screen.getAllByRole("link")).toHaveLength(2);
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

  // The read is out: the line and its Retry stay, the Retry busy and holding the focus, and no
  // loading line stands in their place.
  expect(screen.getByRole("alert"), "the line went while its retry was out").toHaveTextContent(
    ANSWER_LINES.run_unreadable
  );
  expect(retry, "Retry went while the read it sent was out").toBeInTheDocument();
  expect(retry).toHaveAttribute("aria-busy", "true");
  expect(retry).toHaveFocus();
  expect(screen.queryByText(ANSWER_LINES.loading_older)).toBeNull();

  // The read answers: the answer replaces the line, and the focus goes to the page's heading.
  await act(async () => {
    landing.resolve(answerOf([]));
  });
  const heading = await screen.findByRole("heading", { level: 1, name: RUN_WORD });
  expect(screen.queryByRole("alert")).toBeNull();
  expect(heading).toHaveFocus();
});

// A Retry whose read fails again leaves its line where it stands: the failure is said again, a
// new alert a screen reader reads out even in the same words, while Retry stays the same button
// and keeps the focus.
test("a Retry whose read fails again says the failure again in the same line and keeps the focus on Retry", async () => {
  const user = userEvent.setup();
  const { getReferences } = draw(() => Promise.reject(new ApiError(500, {})));
  const said = await screen.findByRole("alert");
  expect(said).toHaveTextContent(ANSWER_LINES.run_unreadable);
  const failing = Promise.withResolvers<GraphReferences>();
  getReferences.mockImplementation(() => failing.promise);
  const asked = getReferences.mock.calls.length;
  const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
  act(() => retry.focus());
  await user.keyboard("{Enter}");
  await waitFor(() => expect(getReferences).toHaveBeenCalledTimes(asked + 1));

  // The read is out: the line stands as it was, its Retry busy.
  expect(screen.getByRole("alert"), "the line was drawn anew while its retry was out").toBe(said);
  expect(retry).toHaveAttribute("aria-busy", "true");

  // The read fails again, past each retry a read is given: the failure is a new alert in the
  // same words, and Retry is the same button, no longer busy and still holding the focus.
  await act(async () => {
    failing.reject(new ApiError(500, {}));
  });
  await waitFor(() => expect(retry).not.toHaveAttribute("aria-busy"));
  const again = screen.getByRole("alert");
  expect(again, "the failure was not said again").not.toBe(said);
  expect(said).not.toBeInTheDocument();
  expect(again).toHaveTextContent(ANSWER_LINES.run_unreadable);
  expect(screen.getByRole("button", { name: CONTROL_LABELS.retry }), "Retry was drawn anew").toBe(
    retry
  );
  expect(retry).toHaveFocus();
});

// A Retry whose read the server answers 400 or 404 draws the not-found page in place of the line
// and the Retry the reader's focus is on, and the focus goes to that page's link rather than fall
// to the page's body, where no keyboard action may leave it. The read's failure, which says
// whether the run is missing, is cleared while the read runs again; the line stands meanwhile.
test("a Retry the server answers 400 or 404 hands the focus to the not-found page's link", async () => {
  const alone = notFoundAlone();
  for (const refusal of [new ApiError(404, { code: "NOT_FOUND" }), new ApiError(400, {})]) {
    const user = userEvent.setup();
    const { getReferences, view } = draw(() => Promise.reject(new ApiError(500, {})));
    expect(await screen.findByRole("alert")).toHaveTextContent(ANSWER_LINES.run_unreadable);
    getReferences.mockImplementation(() => Promise.reject(refusal));
    act(() => screen.getByRole("button", { name: CONTROL_LABELS.retry }).focus());
    await user.keyboard("{Enter}");

    await screen.findByRole("heading", { name: alone.heading });
    const link = expectNotFoundPage(view.container, alone);
    expect(
      link,
      `the not-found page of a ${refusal.status} left the focus off its link`
    ).toHaveFocus();
    restore?.();
    restore = undefined;
  }
});

// TanStack clears the failure of a read that holds no answer as the read runs again - on window
// focus, or at a broad refresh of the backlink panels - so a missing run's page drew the run's own
// page, its line saying the read failed, until the read answered 404 again.
test("a missing run's read sent again keeps the not-found page while it is out", async () => {
  const alone = notFoundAlone();
  const { client, getReferences, view } = draw(() =>
    Promise.reject(new ApiError(404, { code: "NOT_FOUND" }))
  );
  const heading = await screen.findByRole("heading", { name: alone.heading });
  expectNotFoundPage(view.container, alone);
  const again = Promise.withResolvers<GraphReferences>();
  getReferences.mockImplementation(() => again.promise);
  const asked = getReferences.mock.calls.length;
  // The prefix this refreshes by stands for the broad refreshes that reach the read, each by the
  // shorter prefix `["references", "to"]` (`referencePanels`, `EVERY_REFERENCE_READ`, a gap in the
  // event feed): the read is sent again.
  act(() => {
    void client.invalidateQueries({
      queryKey: referencesQuery(`dispatch://interaction/${RUN}`).queryKey,
    });
  });
  expect(getReferences).toHaveBeenCalledTimes(asked + 1);

  // The read is out: no line says it failed, and the not-found page stands as it was.
  expect(screen.queryByRole("alert"), "the page said the read failed while it was out").toBeNull();
  expectNotFoundPage(view.container, alone);
  expect(screen.getByRole("heading"), "the not-found page was drawn anew").toBe(heading);

  // The read answers 404 again: the not-found page still stands.
  await act(async () => {
    again.reject(new ApiError(404, { code: "NOT_FOUND" }));
  });
  expect(screen.queryByRole("alert")).toBeNull();
  expectNotFoundPage(view.container, alone);
});

// A page mounted again over a missing run's read, which the cache holds, sends the read again,
// and the result TanStack hands its first render already reads as pending, its failure cleared.
// The page draws the not-found page in its first commit and in each while the read is out.
test("a page mounted again over a missing run's read draws the not-found page at once and keeps it", async () => {
  const alone = notFoundAlone();
  const { client, getReferences, view } = draw(() =>
    Promise.reject(new ApiError(404, { code: "NOT_FOUND" }))
  );
  await screen.findByRole("heading", { name: alone.heading });
  view.unmount();
  const again = Promise.withResolvers<GraphReferences>();
  getReferences.mockImplementation(() => again.promise);
  const asked = getReferences.mock.calls.length;
  const container = document.body.appendChild(document.createElement("div"));
  const drawn: string[] = [];
  const remounted = render(
    <Profiler id="remounted" onRender={() => drawn.push(container.textContent ?? "")}>
      {runPage(client)}
    </Profiler>,
    { container }
  );
  const restoreFirst = restore;
  restore = () => {
    remounted.unmount();
    container.remove();
    restoreFirst?.();
  };
  expect(getReferences, "the page mounted again sent no read").toHaveBeenCalledTimes(asked + 1);

  // The read is out: every commit since the mount drew the not-found page, and no line.
  expect(drawn.length).toBeGreaterThan(0);
  expect(
    drawn.filter((text) => text !== alone.text),
    "a commit drew something other than the not-found page"
  ).toEqual([]);
  expect(screen.queryByRole("alert")).toBeNull();
  expectNotFoundPage(container, alone);

  // The read answers 404 again: the not-found page still stands.
  await act(async () => {
    again.reject(new ApiError(404, { code: "NOT_FOUND" }));
  });
  expect(screen.queryByRole("alert")).toBeNull();
  expectNotFoundPage(container, alone);
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
  const asked = screen.getByRole("region", { name: RUN_PAGE_WORDS.question_heading });
  expect(within(asked).getByText(question).textContent).toBe(question);
  expect(asked.querySelector("strong")).toBeNull();
  expect(within(asked).queryByRole("link")).toBeNull();

  // When it was asked.
  expect(document.querySelector(`time[datetime="${ASKED_AT}"]`)).not.toBeNull();

  // The issue it changed, by its kind's word and its first line - its marks gone, to the end of
  // its first sentence - which links to it.
  const what = screen.getByRole("region", { name: RUN_PAGE_WORDS.what_changed });
  expect(within(what).getByRole("listitem")).toHaveTextContent(
    sentenceCase(NODE_REFERENCES_WORDS.kinds.issue)
  );
  expect(within(what).getByRole("link", { name: "Ship the release on Friday." })).toHaveAttribute(
    "href",
    "/issues/CORE-1"
  );
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
  const sourceOnly: NonNullable<GraphReferences["edges"]>[number] = {
    ...risk,
    excerpt: {
      text: ":::risk{#r1}\n:::",
      text_tree: {
        content: [{ content: [{ text: ":::risk{#r1}\n:::", type: "text" }], type: "paragraph" }],
        type: "doc",
      },
    },
    node: graphNode({
      id: "b752b280-b6fa-4ce0-aae0-3cf81ae5106e",
      kind: "risk",
      ref: "dispatch://CORE-1/spec#b-r1",
    }),
  };
  draw(() => Promise.resolve(answerOf([], [risk, sourceOnly])));
  await screen.findByRole("heading", { level: 1, name: RUN_WORD });
  const what = screen.getByRole("region", { name: RUN_PAGE_WORDS.what_changed });
  const entry = within(what).getByRole("listitem");
  expect(entry.textContent).toBe(excerpt);
  expect(wordsIn(entry.textContent ?? "")).not.toContain("risk");
  expect(within(what).queryByRole("link")).toBeNull();
});

/** An edge from the issue `key`, CORE-1 where none is given, which the run changed, whose excerpt
 *  is `excerpt`. */
function changedIssue(
  excerpt: NonNullable<NonNullable<GraphReferences["edges"]>[number]["excerpt"]>,
  key = "CORE-1"
): NonNullable<GraphReferences["edges"]>[number] {
  return {
    created_at: "2026-11-17T09:00:05Z",
    direction: "in",
    excerpt,
    kind: "changed_by_run",
    node: graphNode({ id: key, issue_key: key, kind: "issue", ref: `dispatch://${key}` }),
    source_seq: null,
  };
}

test("a changed node's line reads three comment names in one batch", async () => {
  const names = ["Warm cache", "Check logs", "Ship build"];
  const targets = names.map((name, at) => ({
    gone: false,
    kind: "comment",
    name,
    ref: `dispatch://CORE-2/comment/44444444-4444-4444-8444-00000000000${at + 1}`,
  }));
  const refs = targets.map(({ ref }) => ref);
  const answer = Promise.withResolvers<ReferenceTargets>();
  const batch = spyOn(api, "getReferenceTargets").mockReturnValue(answer.promise);
  const perTarget = spyOn(api, "getComment").mockRejectedValue(new ApiError(404, {}));
  try {
    const changed = changedIssue({ text: `Close after ${refs.join(" and ")}. Then rest.` });
    // The run's question is drawn as typed and does not join the batch.
    const question = "What about dispatch://CORE-2/comment/44444444-4444-4444-8444-000000000004?";
    draw(() => Promise.resolve(answerOf([], [changed], question)));
    const what = await screen.findByRole("region", { name: RUN_PAGE_WORDS.what_changed });
    expect(perTarget, "individual comment reads while the batch is out").toHaveBeenCalledTimes(0);
    expect(batch.mock.calls).toEqual([[refs]]);
    await act(async () => answer.resolve({ targets }));
    expect(
      await within(what).findByRole("link", { name: `Close after ${names.join(" and ")}.` })
    ).toHaveAttribute("href", "/issues/CORE-1");
    expect(screen.getByText(question)).toBeInTheDocument();
    expect(perTarget, "individual comment reads after the batch answered").toHaveBeenCalledTimes(0);
    expect(batch.mock.calls).toEqual([[refs]]);
  } finally {
    batch.mockRestore();
    perTarget.mockRestore();
  }
});

// A changed node's line is a one-line place (the reading list's `run-page-changed-line`): each
// reference it cites is named as every plain line of the page names one (`referenceStandIns`) -
// an issue by its key, a space and its title, and any other target by the page's name for it,
// once the page's batched read has named it, and as its author wrote it until then - and never by
// its short form.
test("a changed node's line names each reference by its name once read, and as its author wrote it until then", async () => {
  const issue = "dispatch://CORE-2";
  const named = "dispatch://CORE-1/comment/7d1e4c2a-9b3f-4e8d-a6c5-2f0b1d9e7a34";
  // A block's address, which the line holds whole within R1's 100 characters once the comment is
  // named: a comment's address, with its whole id, would be cut off.
  const unnamed = "dispatch://CORE-1/spec#b-r2";
  const route = parseNodeReference(unnamed);
  if (route === undefined) throw new Error(`${unnamed} names no node`);
  const changed = changedIssue({
    text: `Close ${issue} once ${named} and ${unnamed} land. Then rest.`,
  });
  // The page's batched read names the comment, and has been asked for the block and holds no
  // answer for it. No page has named the issue, so the run's one batch asks for it.
  const published = publishedNames(new Map([[named, "Warm the cache first"]]), {
    asked: [unnamed],
  });
  const runAddress = `dispatch://interaction/${RUN}`;
  const answer = Promise.withResolvers<ReferenceTargets>();
  const batch = spyOn(api, "getReferenceTargets").mockReturnValue(answer.promise);
  const perTarget = spyOn(api, "getComment").mockRejectedValue(new ApiError(404, {}));
  try {
    const { getReferences } = draw(() => Promise.resolve(answerOf([], [changed])), {
      published,
    });
    await screen.findByRole("heading", { level: 1, name: RUN_WORD });
    const what = screen.getByRole("region", { name: RUN_PAGE_WORDS.what_changed });
    // While the batch is out, the issue reads as its author wrote it.
    expect(
      await within(what).findByRole("link", {
        name: `Close ${issue} once Warm the cache first and ${unnamed} land.`,
      })
    ).toHaveAttribute("href", "/issues/CORE-1");
    expect(batch.mock.calls).toEqual([[[issue]]]);
    await act(async () =>
      answer.resolve({
        targets: [{ gone: false, kind: "issue", name: "Pin the release date", ref: issue }],
      })
    );
    expect(
      await within(what).findByRole("link", {
        name: `Close CORE-2 Pin the release date once Warm the cache first and ${unnamed} land.`,
      })
    ).toHaveAttribute("href", "/issues/CORE-1");
    expect(what.textContent).not.toContain(shortForm(route));
    expect(batch).toHaveBeenCalledTimes(1);
    expect(perTarget, "a comment read of its own").toHaveBeenCalledTimes(0);
    expect(
      getReferences.mock.calls.filter(([to]) => to !== runAddress),
      "a block read of its own"
    ).toEqual([]);
  } finally {
    batch.mockRestore();
    perTarget.mockRestore();
  }
});

// A key a changed node's words write bare is named as every one-line place names one: by its key
// and its issue's title, where the page published that issue (`knownIssueKeys`). Nothing else on
// the run's page asks for it, so the page's one batch does, as the issue's own reference, and no
// issue is read on its own. A word the text reader reads as a key that names no issue (`UTF-16`)
// is answered gone, and reads as its author wrote it.
test("a changed node's bare issue key joins the one batch and is named by its key and title once read, and a key-shaped word that names no issue reads as written", async () => {
  const answer = Promise.withResolvers<ReferenceTargets>();
  const batch = spyOn(api, "getReferenceTargets").mockReturnValue(answer.promise);
  const issues = spyOn(api, "getIssue").mockRejectedValue(new ApiError(404, {}));
  const summaries = spyOn(api, "getIssueSummary").mockRejectedValue(new ApiError(404, {}));
  try {
    const changed = changedIssue({ text: "Ship CORE-9 before the UTF-16 fix. Then rest." });
    draw(() => Promise.resolve(answerOf([], [changed])));
    const what = await screen.findByRole("region", { name: RUN_PAGE_WORDS.what_changed });
    // While the batch is out, both read as their author wrote them.
    expect(
      await within(what).findByRole("link", { name: "Ship CORE-9 before the UTF-16 fix." })
    ).toHaveAttribute("href", "/issues/CORE-1");
    expect(batch.mock.calls).toEqual([[["dispatch://CORE-9", "dispatch://UTF-16"]]]);
    await act(async () =>
      answer.resolve({
        targets: [
          { gone: false, kind: "issue", name: "Pin the release date", ref: "dispatch://CORE-9" },
          { gone: true, kind: "issue", name: "", ref: "dispatch://UTF-16" },
        ],
      })
    );
    expect(
      await within(what).findByRole("link", {
        name: "Ship CORE-9 Pin the release date before the UTF-16 fix.",
      })
    ).toHaveAttribute("href", "/issues/CORE-1");
    expect(batch).toHaveBeenCalledTimes(1);
    expect(
      issues.mock.calls.length + summaries.mock.calls.length,
      "a read of one issue of its own"
    ).toBe(0);
  } finally {
    batch.mockRestore();
    issues.mockRestore();
    summaries.mockRestore();
  }
});

// Decision 533: a run's row on an issue page that `?issue_page=new` opened links to the run's
// page with the parameter and that issue's key (`run-references.ts`), since the run's own read
// cannot say which issue opened it. The run's page adds the parameter to each link of a changed
// node and of a cited item to an address of that issue that names no `issue_page` of its own,
// after the link's own parameters and before its hash, so the reader goes back to that issue's
// new page. A link to another issue, or to no issue, is drawn as it is, and so is every link at
// an address that names no such issue.
test("a run's page opened from an issue's new page keeps ?issue_page=new on its links to that issue alone, and an address that names no issue keeps it on none", async () => {
  const items = [
    { href: "/issues/CORE-1", title: "The release plan" },
    { href: "/issues/CORE-1/spec?v=3#b-r2", title: "The second rule" },
    { href: "/issues/CORE-1?issue_page=old", title: "The old page" },
    { href: "/issues/CORE-2", title: "The build" },
    { href: "https://example.com/release", title: "The release notes" },
  ];
  const answer = answerOf(
    items.map((item, index) => frame(index + 1, GOING_EVENT_ITEM, item)),
    [
      changedIssue({ text: "Ship the release on Friday." }),
      changedIssue({ text: "Pin the build first." }, "CORE-2"),
    ]
  );
  /** Each link of the run's page at its address with `search`, by its name. */
  const linksAt = async (search: string) => {
    draw(() => Promise.resolve(answer), { path: `/runs/${RUN}${search}` });
    await screen.findByRole("heading", { level: 1, name: RUN_WORD });
    const links = Object.fromEntries(
      screen.getAllByRole("link").map((link) => [link.textContent, link.getAttribute("href")])
    );
    restore?.();
    restore = undefined;
    return links;
  };
  const asGiven = {
    "Pin the build first.": "/issues/CORE-2",
    "Ship the release on Friday.": "/issues/CORE-1",
    ...Object.fromEntries(items.map(({ href, title }) => [title, href])),
  };
  expect(await linksAt("?issue_page=new&issue=CORE-1"), "opened from CORE-1's new page").toEqual({
    ...asGiven,
    "Ship the release on Friday.": "/issues/CORE-1?issue_page=new",
    "The release plan": "/issues/CORE-1?issue_page=new",
    "The second rule": "/issues/CORE-1/spec?v=3&issue_page=new#b-r2",
  });
  for (const search of ["?issue_page=new", "?issue=CORE-1"]) {
    expect(await linksAt(search), `at ${search}`).toEqual(asGiven);
  }
});
