import { afterEach, beforeAll, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import {
  SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE,
  SEARCH_OPERATOR_EXCLUDE,
  SEARCH_OPERATOR_PHRASE,
} from "@legion/contracts";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the keys of an IME composition below, which user-event has no form for
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import type { Window as HappyDOMWindow } from "happy-dom";
import { type ReactNode, useState } from "react";
import { Link, MemoryRouter, useLocation, useNavigate } from "react-router-dom";

import { correction, correctionFeed } from "../../__tests__/correction-feed";
import { snippetTree } from "../../__tests__/display-trees";
import { realClock, tick } from "../../__tests__/fake-clock";
import { UNCONFIGURED_CAPABILITY } from "../../__tests__/network";
import { answerRoute } from "../../__tests__/send-answers";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api } from "../../api/client";
import { useEventFeed } from "../../api/event-feed";
import type {
  ArtifactUploadResponse,
  DisplayTree,
  GoingConversation,
  GoingPlanAnswer,
  GoingRun,
  IssueReferenceSummary,
  Message,
  Project,
  SearchResponse,
  SearchResult,
} from "../../api/types";
import { CONTROL } from "../../components/control-names";
import { BLOCK_ID_ATTRIBUTE } from "../doc/block-id";
import { goingState, setLevel, setPlace, useGoingValue } from "../going/going-store";
import { SidePanel } from "../going/SidePanel";
import { issue as issueFixture, specArtifact } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { appKeymap } from "../shell/keymap";
import { ANSWER_PREVIEW_CHARS } from "./AskHitStatus";
import { CONFIGURED_CAPABILITY } from "./going-bar-fixture";
import { NEW_ISSUE_OPTION_ID } from "./NewIssueOffer";
import { firstCriterion } from "./new-issue";
import { SearchPalette } from "./SearchPalette";
import { searchAnswer } from "./search-fixture";
import { optionId } from "./search-model";
import { KEYSTROKE_DEBOUNCE_MS, MEANING_PAUSE_MS } from "./useTieredSearch";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const results: SearchResult[] = [
  {
    artifact: { name: "spec.md", slug: "spec" },
    href: "/issues/LEGION-2/spec?q=astrolabe",
    id: "document-2",
    owner: { key: "LEGION-2", kind: "issue", status: "done", title: "Navigation instruments" },
    kind: "document",
    rank: 3,
    snippet: "The <mark>astrolabe</mark> measures altitude.",
  },
  // An issue hit's text is its title, as the server reads it: the headline of the title alone.
  {
    href: "/issues/LEGION-3",
    id: "LEGION-3",
    owner: { key: "LEGION-3", kind: "issue", status: "todo", title: "Astrolabe research" },
    kind: "issue",
    rank: 2,
    snippet: "<mark>Astrolabe</mark> research",
  },
  {
    href: "/issues/LEGION-2/comments/comment-2",
    id: "comment-2",
    owner: { key: "LEGION-2", kind: "issue", status: "done", title: "Navigation instruments" },
    kind: "comment",
    rank: 1,
    snippet: "Discuss the <mark>astrolabe</mark> diagram.",
  },
];

/** The last argument of every search the bar makes: React Query's own abort signal, which the
 *  bar hands the client so a search it has moved on from is dropped rather than waited for. */
const aborts = { signal: expect.anything() };

/**
 * Every row of this file runs on a held clock.
 *
 * Each keystroke arms two timers of the bar's own (`useTieredSearch.ts`): a 150 ms debounce,
 * after which it searches the words alone, and a 500 ms meaning pause, after which it searches
 * again by meaning and that fused order replaces the list. Which of the two has fired when a
 * row asserts is what the row reads, and on the real clock the machine decides it: a loaded pod
 * crosses the pause inside a press, and the row then reads a list it was not written for. Held,
 * the row decides instead - `searchFor` below moves the clock the debounce's length and stops
 * there, and a row that means the pause to fire moves past 500 ms and says so - so a row reads
 * the same list on a busy pod as on an idle one.
 *
 * The clock is installed here rather than row by row because every row of the file wants it,
 * and `realClock()` below is what brings the real one back on every path out of a row, one that
 * throws included. Each row's user is `userOnFakeClock()`, which this install has to come
 * before: user-event waits between the events of one interaction through `setTimeout`, so a
 * user built any other way never finishes a press under a held clock.
 */
beforeEach(() => {
  jest.useFakeTimers();
});
afterEach(realClock);

/**
 * An issue hit's row is a reference to that issue, which reads the issue's summary to draw it.
 * No test here is about that read, so every key is answered with the issue the row names and no
 * documents.
 */
let getIssueSummary: Mock<typeof api.getIssueSummary>;

beforeEach(() => {
  getIssueSummary = spyOn(api, "getIssueSummary").mockImplementation((key) =>
    Promise.resolve({
      documents: [],
      key,
      project: key.split("-")[0] ?? "",
      status: "todo",
      title: `Instrument ${key}`,
    } satisfies IssueReferenceSummary)
  );
});

afterEach(() => {
  getIssueSummary.mockRestore();
});

function CurrentRoute(): ReactNode {
  const location = useLocation();
  return <output data-testid="current-route">{`${location.pathname}${location.search}`}</output>;
}

function RouteChanger(): ReactNode {
  const navigate = useNavigate();
  return (
    <button onClick={() => navigate("/issues/LEGION-9")} type="button">
      Navigate elsewhere
    </button>
  );
}

/** A route settling itself the way `ProjectPage` does: a `replace` to the address the page
 *  belongs at, which the reader did not ask for. */
function RouteSettler(): ReactNode {
  const navigate = useNavigate();
  return (
    <button onClick={() => navigate("/projects/LEGION/issues", { replace: true })} type="button">
      Settle the route
    </button>
  );
}

/** The reader going back: the browser's Back button, or anything that calls `navigate(-1)`. */
function RouteReturner(): ReactNode {
  const navigate = useNavigate();
  return (
    <button onClick={() => navigate(-1)} type="button">
      Go back
    </button>
  );
}

/** A link whose target is the address already shown. React Router makes that click a `replace`
 *  that moves nothing, and the reader has moved, so the palette closes: the Inbox drawer's owner
 *  links are these (dispatch://INFRA-2452). */
function SameAddressLink({ to }: { to: string }): ReactNode {
  return <Link to={to}>Open the issue</Link>;
}

interface RenderPaletteOptions {
  /** Rendered beside the palette, inside the router: what a navigation row clicks. */
  control?: ReactNode;
  entries?: string[];
  index?: number;
  /** The person the shell read as signed in before the bar opened, as it has on a page. */
  login?: string;
  onClose?: () => void;
  /** The cache the palette reads, for a row that holds it before the palette renders. */
  queryClient?: QueryClient;
}

function renderPalette({
  control,
  entries = ["/"],
  index,
  login,
  onClose = () => {},
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } }),
}: RenderPaletteOptions = {}): { queryClient: QueryClient; unmount: () => void } {
  if (login !== undefined) {
    queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login });
  }
  const view = render(
    <MemoryRouter initialEntries={entries} initialIndex={index}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <CurrentRoute />
          {control}
          <SearchPalette onClose={onClose} open />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, unmount: view.unmount };
}

/** Types `query` into the box and waits for the hits it brings. The 150 ms keystroke debounce
 *  is moved past here, so the search for these words goes out and answers; the 500 ms meaning
 *  pause is left standing, so what a row reads after this is the keyword tier's list. A row
 *  that means the pause to fire moves the clock past it itself. */
async function searchFor(user: UserEvent, query: string): Promise<HTMLInputElement> {
  const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
  await user.clear(input);
  await user.click(input);
  await user.paste(query);
  await tick(150);
  await waitFor(() => expect(screen.getAllByRole("option").length).toBeGreaterThan(0));
  return input;
}

test("renders grouped results with marked snippets and dims a done issue", async () => {
  // Each snippet is Markdown (`MarkdownPreview`), empty until the lazy Markdown engine loads.
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  const view = renderPalette();

  try {
    await searchFor(user, "astrolabe");
    await waitFor(() =>
      expect(screen.getAllByText("astrolabe", { selector: "mark" }).length).toBeGreaterThan(0)
    );

    // The owner rows name their groups and are `aria-hidden`, so a reader hears each owner
    // once; they are found by their label attribute rather than by role.
    const doneGroup = document.querySelector('[aria-label="LEGION-2: Navigation instruments"]');
    expect(screen.getAllByRole("group")).toHaveLength(2);
    expect(screen.getAllByRole("option")).toHaveLength(3);
    expect(screen.getAllByText("astrolabe", { selector: "mark" })).not.toHaveLength(0);
    expect(doneGroup).toHaveAttribute("data-status", "done");
    // A hit's snippet stands under its kind, its match marked (review N2).
    const owned = within(screen.getByRole("group", { name: "LEGION-2: Navigation instruments" }));
    const snippet = within(owned.getAllByRole("option")[0] as HTMLElement).getByTestId(
      "search-hit-snippet"
    );
    expect(snippet).toHaveTextContent("The astrolabe measures altitude.");
    expect(within(snippet).getByText("astrolabe", { selector: "mark" })).toBeTruthy();
    expect(screen.queryByText(/^Showing the best/)).not.toBeInTheDocument();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// A document hit whose headline window began in one Proof directive and ends inside the next
// one's attributes (review N2, 13-filing-line): the server's reader reads neither cut piece as a
// directive, so its tree holds them as paragraph text, a soft break between them a space. The
// snippet line draws that tree with no fence and no attribute syntax: `table. A page is ready
// once its plan loads`, its highlight kept, and its link as the words around it (review A2): no
// anchor, no link look (`data-link-look="text"`, which styles.css's link look skips) and no tab
// stop inside the row, whose own Enter and click are its one action, so a click on the link's
// words opens the hit.
test("a document hit's snippet shows no directive fence or attributes a cut window left, its highlight kept and its link as text", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(
    searchAnswer([
      {
        artifact: { name: "plan.md", slug: "plan" },
        href: "/issues/LEGION-2/artifact/plan?q=plan",
        id: "document-plan",
        kind: "document",
        owner: { key: "LEGION-2", kind: "issue", status: "in_progress", title: "Local testing" },
        rank: 1,
        snippet:
          'table.\n:::\n:::plan_task{#t1 serves="A page is ready once its <mark>plan</mark> loads',
        snippet_tree: {
          content: [
            {
              content: [
                { text: 'table. ::: :::plan_task{#t1 serves="', type: "text" },
                {
                  marks: [{ attrs: { href: "https://example.com/pages" }, type: "link" }],
                  text: "A page",
                  type: "text",
                },
                { text: " is ready once its ", type: "text" },
                { marks: [{ type: "searchHighlight" }], text: "plan", type: "text" },
                { text: " loads", type: "text" },
              ],
              type: "paragraph",
            },
          ],
          type: "doc",
        },
      },
    ])
  );
  const view = renderPalette();
  try {
    const input = await searchFor(user, "plan");
    const option = within(screen.getByRole("group", { name: "LEGION-2: Local testing" })).getByRole(
      "option"
    );
    const snippet = within(option).getByTestId("search-hit-snippet");
    const body = snippet.querySelector<HTMLElement>("[data-markdown-preview]");
    if (body === null) throw new Error("the search snippet is not a MarkdownPreview");
    await waitFor(() =>
      expect(body.textContent?.replace(/\s+/gu, " ").trim()).toBe(
        "table. A page is ready once its plan loads"
      )
    );
    expect(body.textContent).not.toMatch(/:::|plan_task|serves=|[{}"]/u);
    expect(within(body).getByText("plan", { selector: "mark" })).toBeTruthy();
    expect(option.querySelector("a")).toBeNull();
    const link = within(body).getByText("A page");
    expect(link.tagName).toBe("SPAN");
    expect(link).toHaveAttribute("data-markdown-link", "");
    expect(link).toHaveAttribute("data-link-look", "text");
    expect(link).not.toHaveAttribute("tabindex");
    // Tab walks the dialog's own controls round to the box and never stops in the row.
    expect(document.activeElement).toBe(input);
    const stops: (Element | null)[] = [];
    do {
      await user.tab();
      stops.push(document.activeElement);
    } while (document.activeElement !== input && stops.length < 50);
    expect(document.activeElement).toBe(input);
    expect(
      stops.flatMap((stop) => (stop !== null && option.contains(stop) ? [stop.outerHTML] : []))
    ).toEqual([]);
    await user.click(link);
    expect(screen.getByTestId("current-route").textContent).toBe(
      "/issues/LEGION-2/artifact/plan?q=plan"
    );
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// The server says where its headline window cut the text a hit's snippet was read out of, on the
// snippet tree's doc (review R4, 13-filing-line): a line whose window began after a word of the
// text begins with "…", and one whose window stopped before a word of it ends with "…". The plan
// the visual review's bar found is that tree as the server sends it: its window is cut at both
// ends though it holds ten runs between whitespace, since ts_headline counts the parser's tokens,
// and its line begins with "…" where it read "table." with nothing to say the line began inside
// the text. A window whose start the server cut and that began on a sentence's tail starts at the
// next sentence's first word, after "…". A tree that says its window held the whole text draws
// neither and keeps every word, though it starts on a small letter, ends a sentence a few words
// in and holds two dozen words.
test("a snippet begins and ends with … where the server says its window cut the text, and a whole one draws neither though it starts on a small letter", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const plan: DisplayTree = {
    attrs: { cutEnd: true, cutStart: true },
    content: [
      { content: [{ text: "table. :::", type: "text" }], type: "paragraph" },
      {
        content: [
          {
            text: ':::plan_task{#t1 serves="dispatch://CORE-1/artifact/requirements-md#b-r2" ',
            type: "text",
          },
          { marks: [{ type: "searchHighlight" }], text: "needs", type: "text" },
          {
            text: '="dispatch://CORE-1/artifact/plan-md#b-t0"} Draw each page and read',
            type: "text",
          },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  const whole =
    "each page is read against the design. It is drawn once the next one is ready so that no page of the set is left unread";
  const tail = "table. A page is read once it is drawn";
  const end = "Draw each page and read it against the design before the next one";
  const hit = (id: string, snippet: string, tree: DisplayTree): SearchResult => ({
    artifact: { name: "plan.md", slug: "plan" },
    href: `/issues/LEGION-2/artifact/plan?q=page&hit=${id}`,
    id,
    kind: "document",
    owner: { key: "LEGION-2", kind: "issue", status: "in_progress", title: "Local testing" },
    rank: 1,
    snippet,
    snippet_tree: tree,
  });
  const search = spyOn(api, "search").mockResolvedValue(
    searchAnswer([
      hit(
        "document-plan",
        "table.\n:::\n\n:::plan_task{#t1 serves=&#34;dispatch://CORE-1/artifact/requirements-md#b-r2&#34; <mark>needs</mark>=&#34;dispatch://CORE-1/artifact/plan-md#b-t0&#34;}\nDraw each page and read",
        plan
      ),
      hit("document-whole", whole, snippetTree(whole)),
      hit("document-tail", tail, { ...snippetTree(tail), attrs: { cutStart: true } }),
      hit("document-end", end, { ...snippetTree(end), attrs: { cutEnd: true } }),
    ])
  );
  // The plan's directive attributes name two blocks, which its line draws as references, each
  // reading its block's words. No row here is about those words, so the reads are held open and
  // the line draws each reference as the address it is.
  const getReferences = spyOn(api, "getReferences").mockImplementation(() => new Promise(() => {}));
  const view = renderPalette();
  try {
    await searchFor(user, "page");
    const [planHit, wholeHit, tailHit, endHit] = within(
      screen.getByRole("group", { name: "LEGION-2: Local testing" })
    ).getAllByRole("option");
    if (
      planHit === undefined ||
      wholeHit === undefined ||
      tailHit === undefined ||
      endHit === undefined
    ) {
      throw new Error("the bar lists no four document hits");
    }
    const drawn = (option: HTMLElement): string =>
      within(option)
        .getByTestId("search-hit-snippet")
        .querySelector("[data-markdown-preview]")
        ?.textContent?.replace(/\s+/gu, " ")
        .trim() ?? "";
    await waitFor(() => expect(drawn(planHit)).toStartWith("…table. "));
    expect(drawn(planHit)).toEndWith(" Draw each page and read…");
    await waitFor(() => expect(drawn(wholeHit)).toBe(whole));
    await waitFor(() => expect(drawn(tailHit)).toBe("…A page is read once it is drawn"));
    await waitFor(() => expect(drawn(endHit)).toBe(`${end}…`));
  } finally {
    search.mockRestore();
    getReferences.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a best match whose snippet starts past the title's first words shows the whole title, once, with the word that matched marked", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  // The server's headline of "API: rate limit headers" for `headers` starts at "rate".
  const search = spyOn(api, "search").mockResolvedValue(
    searchAnswer([
      {
        href: "/issues/LEGION-30",
        id: "LEGION-30",
        kind: "issue",
        owner: {
          key: "LEGION-30",
          kind: "issue",
          status: "in_progress",
          title: "API: rate limit headers",
        },
        rank: 1,
        snippet: "rate limit <mark>headers</mark>",
      },
    ])
  );
  const view = renderPalette();

  try {
    // The key names the issue, so its hit is the best match, above the line on top, in the one
    // row look that names its item (R2): a hit under its owner's row leaves the title to that row.
    await searchFor(user, "LEGION-30");
    const option = within(screen.getByRole("group", { name: "Best match" })).getByRole("option");
    expect(option).toHaveTextContent("LEGION-30");
    expect(option).toHaveTextContent("API: rate limit headers");
    expect(option.textContent?.match(/rate limit headers/gu)).toHaveLength(1);
    const marked = option.querySelectorAll("mark[data-search-highlight]");
    expect(Array.from(marked, (mark) => mark.textContent)).toEqual(["headers"]);
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("says how many matches a query has when the page holds fewer", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue({
    ...searchAnswer(results),
    reachable: 300,
    total: 1328,
  });
  const view = renderPalette();

  try {
    await searchFor(user, "astrolabe");
    expect(screen.getAllByRole("option")).toHaveLength(3);
    expect(screen.getByText("Showing the best 3 of 1328 matches")).toBeDefined();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// A comment or a message hit prints the post, so one its author edited says so beside its kind,
// as its byline does; one nobody edited carries no mark.
test("an edited post's hit carries the edited mark, and a post nobody edited does not", async () => {
  const user = userOnFakeClock();
  const edited: SearchResult = {
    edited_at: "2026-10-01T00:00:00Z",
    href: "/issues/LEGION-2/comments/comment-3",
    id: "comment-3",
    kind: "comment",
    owner: { key: "LEGION-2", kind: "issue", status: "todo", title: "Navigation instruments" },
    rank: 2,
    snippet: "The <mark>astrolabe</mark> was recalibrated.",
  };
  const standing = results[2] as SearchResult;
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([edited, standing]));
  const view = renderPalette();

  try {
    await searchFor(user, "astrolabe");
    const option = (result: SearchResult) => document.getElementById(optionId(result));
    await waitFor(() => expect(option(edited)?.textContent).toContain("comment · edited"));
    expect(option(standing)?.textContent).toContain("comment");
    expect(option(standing)?.textContent).not.toContain("edited");
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// A deleted post is no hit once its delete commits, which takes its windows with its body; the
// mark is the backstop for a hit of one all the same, whose snippet is empty. The hit draws the
// deleted line where the snippet would be, naming who deleted it and when, and no edited mark
// beside its kind; a post nobody deleted draws its snippet and no deleted line.
test("a deleted post's hit draws the deleted line where its snippet would be", async () => {
  // The standing hit's snippet is Markdown (`MarkdownPreview`), empty until the lazy engine loads,
  // and a wait on this row's held clock can spend its budget in fake time before the engine has
  // loaded, so the row loads it first.
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const deleted: SearchResult = {
    deleted_at: "2026-10-02T00:00:00Z",
    deleted_by: { id: "alice", kind: "user" },
    edited_at: "2026-10-01T00:00:00Z",
    href: "/issues/LEGION-2/comments/comment-4",
    id: "comment-4",
    kind: "comment",
    owner: { key: "LEGION-2", kind: "issue", status: "todo", title: "Navigation instruments" },
    rank: 2,
    snippet: "",
  };
  const standing = results[2] as SearchResult;
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([deleted, standing]));
  const view = renderPalette();

  try {
    await searchFor(user, "astrolabe");
    const option = (result: SearchResult) => document.getElementById(optionId(result));
    const line = () => option(deleted)?.querySelector("[data-post-deleted]");
    await waitFor(() => expect(line()?.textContent).toContain("Deleted by alice"));
    expect(line()?.querySelector("time")).toHaveAttribute("datetime", "2026-10-02T00:00:00Z");
    expect(option(deleted)?.textContent).not.toContain("edited");
    expect(option(standing)?.querySelector("[data-post-deleted]")).toBeNull();
    expect(option(standing)?.textContent).toContain("Discuss the astrolabe diagram.");
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

/** The page's event feed, mounted beside the palette as the shell mounts it. */
function EventFeed(): ReactNode {
  useEventFeed();
  return null;
}

// R2 and R3 of dispatch://INFRA-1523, and the architect's ruling of 2026-11-20 00:23Z. The bar
// says a search failed only while it holds no answer, so a refetch that fails behind a held one
// leaves its hits on screen: a post's correction writes its own facts into every answer the cache
// holds (`markCorrectedPost`). The post's hit carries the edited mark once its author edits it,
// and once they delete it the deleted line, with no word of its snippet, in both tiers' answers.
test("a post's hit whose refetch fails after its edit and delete carries the edited mark, then the deleted line", async () => {
  // Each snippet is Markdown (`MarkdownPreview`), empty until the lazy engine loads, and a wait on
  // this row's held clock can spend its budget in fake time before the engine has loaded, so the
  // row loads it first.
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const id = "f6a7b8c9-d0e1-4f2a-8b3c-4d5e6f7a8b9c";
  const hit: SearchResult = {
    href: `/issues/LEGION-2/comments/${id}`,
    id,
    kind: "comment",
    owner: { key: "LEGION-2", kind: "issue", status: "todo", title: "Navigation instruments" },
    rank: 2,
    snippet: "The <mark>astrolabe</mark> was recalibrated.",
  };
  const standing = results[2] as SearchResult;
  let failing = false;
  const search = spyOn(api, "search").mockImplementation(async () => {
    if (failing) throw new Error("503");
    return searchAnswer([hit, standing]);
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const feed = correctionFeed(queryClient, { fakeClock: true });
  const view = renderPalette({ control: <EventFeed />, queryClient });
  const option = (result: SearchResult) => document.getElementById(optionId(result));

  try {
    await searchFor(user, "astrolabe");
    // The pause's search answers too, so the bar holds both tiers' answers and draws the second.
    await tick(MEANING_PAUSE_MS);
    await waitFor(() => expect(option(hit)?.textContent).toContain("recalibrated"));
    feed.start();
    failing = true;

    await feed.deliver(correction("comment", id, true), [["search"]]);
    expect(option(hit)?.textContent).toContain("comment · edited");

    await feed.deliver(correction("comment", id, false), [["search"]]);
    expect(option(hit)?.querySelector("[data-post-deleted]")?.textContent).toContain(
      "Deleted by alice"
    );
    expect(option(hit)?.textContent).not.toContain("recalibrated");
    expect(option(standing)?.textContent).toContain("Discuss the astrolabe diagram.");
  } finally {
    search.mockRestore();
    view.unmount();
    feed.restore();
    queryClient.clear();
  }
});

test("renders document-owned results under the document and opens their discussion", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(
    searchAnswer([
      {
        artifact: { name: "Navigation design", slug: "navigation-design" },
        href: "/projects/CORE/documents/navigation-design?comment=comment-2",
        id: "comment-2",
        kind: "comment",
        owner: {
          artifact_id: "document-2",
          kind: "document",
          name: "Navigation design",
          project: "CORE",
          slug: "navigation-design",
        },
        rank: 1,
        snippet: "Discuss the <mark>astrolabe</mark> diagram.",
      },
    ])
  );
  const view = renderPalette();

  try {
    await searchFor(user, "astrolabe");

    const documentGroup = document.querySelector('[aria-label="CORE: Navigation design"]');
    expect(documentGroup).not.toBeNull();
    expect(screen.getByRole("group", { name: "CORE: Navigation design" })).toBeTruthy();
    await user.click(screen.getByRole("option"));
    expect(screen.getByTestId("current-route").textContent).toBe(
      "/projects/CORE/documents/navigation-design?comment=comment-2"
    );
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("an ask hit says where it stands and shows its latest answer", async () => {
  // Each snippet and answer is Markdown (`MarkdownPreview`), empty until the lazy engine loads.
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const answer = {
    at: "2026-10-01T00:00:00Z",
    selected: ["Brass", "Steel"],
    text: "Keep **both** frames.",
    user: "alice",
  };
  const resolution = {
    actor: { id: "bob", kind: "user" as const },
    at: "2026-10-02T00:00:00Z",
    kind: "resolved" as const,
    reason: "Settled in the spec.",
  };
  const askHit = (id: string, ask: SearchResult["ask"]): SearchResult => ({
    ask,
    href: `/issues/LEGION-5/asks/${id}`,
    id,
    kind: "ask",
    owner: { key: "LEGION-5", kind: "issue", status: "todo", title: "Orrery" },
    rank: 1,
    snippet: `Which <mark>orrery</mark> frame, ${id}?`,
  });
  const search = spyOn(api, "search").mockResolvedValue(
    searchAnswer([
      askHit("open", { state: "open" }),
      askHit("answered", { answer: { ...answer, selected: [], text: null }, state: "answered" }),
      askHit("closed", { resolution, state: "resolved" }),
      askHit("closed-answered", { answer, resolution, state: "resolved" }),
    ])
  );
  const view = renderPalette();

  try {
    await searchFor(user, "orrery");
    const option = (id: string) =>
      screen.getAllByRole("option").find((row) => row.textContent?.includes(`frame, ${id}?`));
    await waitFor(() => expect(option("closed-answered")).toHaveTextContent("both"));

    expect(option("open")).toHaveTextContent("Open");
    expect(option("answered")).toHaveTextContent("Answered");
    expect(option("answered")).toHaveTextContent("alice · ");
    expect(option("answered")).toHaveTextContent("No option chosen");
    expect(option("closed")).toHaveTextContent("Closed, no answer");
    expect(option("closed")).toHaveTextContent("Resolved by bob · ");
    expect(option("closed")).not.toHaveTextContent("alice");
    expect(option("closed-answered")).toHaveTextContent("Closed after an answer");
    expect(option("closed-answered")).toHaveTextContent("Resolved by bob · ");
    expect(option("closed-answered")).toHaveTextContent("Chose Brass, Steel");
    expect(screen.getAllByRole("option", { name: /^ask / })).toHaveLength(4);
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// The answer's three-line clamp is CSS only: whatever text reaches the option is its accessible
// name, so a long answer is cut to its start before it renders.
test("an ask hit shows only the start of a long answer, cut at a word with an ellipsis", async () => {
  await warmMarkdownRenderer();
  const user = userOnFakeClock();
  const text = `Keep **both** frames. ${"The meridian ring stays level. ".repeat(80)}zenithward`;
  const search = spyOn(api, "search").mockResolvedValue(
    searchAnswer([
      {
        ask: {
          answer: { at: "2026-10-01T00:00:00Z", selected: [], text, user: "alice" },
          state: "answered",
        },
        href: "/issues/LEGION-5/asks/long",
        id: "long",
        kind: "ask",
        owner: { key: "LEGION-5", kind: "issue", status: "todo", title: "Orrery" },
        rank: 1,
        snippet: "Which <mark>orrery</mark> frame?",
      },
    ])
  );
  const view = renderPalette();

  try {
    await searchFor(user, "orrery");
    const option = screen.getByRole("option");
    await waitFor(() => expect(option.querySelector("strong")?.textContent).toBe("both"));

    // The cut is on the tree now, not on the markdown, so the 280 characters
    // (ANSWER_PREVIEW_CHARS) are the words a reader sees rather than the source: the four
    // characters of `**both**`'s markers are no longer spent, and the cut lands two words
    // further in. A deliberate change of this task; the bound it is held to is the same one.
    const shown = option.textContent ?? "";
    const shownAnswer = shown.slice(shown.indexOf("Keep both frames."));
    expect(ANSWER_PREVIEW_CHARS).toBe(280);
    expect(text.length).toBeGreaterThan(2000);
    expect(shown).not.toContain("zenithward");
    expect(shownAnswer).toBe(
      `Keep both frames. ${"The meridian ring stays level. ".repeat(8)}The meridian…`
    );
    expect(shownAnswer.length).toBeLessThanOrEqual(ANSWER_PREVIEW_CHARS + 1);
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// A hit's href is the server's, so a hit in a document whose name is past the bound text is held
// to (2683 characters, which a new upload's reference key still holds) is a hover-card trigger
// for that document all the same, an issue's and a project's alike. The hrefs are read against
// the page's origin, which happy-dom's `about:blank` does not have.
test("a hit in a long-named document previews that document", async () => {
  const user = userOnFakeClock();
  const slug = "a".repeat(2683);
  const search = spyOn(api, "search").mockResolvedValue(
    searchAnswer([
      {
        artifact: { name: "Long notes", slug },
        href: `/issues/LEGION-2/artifacts/${slug}`,
        id: "document-2",
        kind: "document",
        owner: { key: "LEGION-2", kind: "issue", status: "todo", title: "Navigation instruments" },
        rank: 2,
        snippet: "The <mark>astrolabe</mark> measures altitude.",
      },
      {
        artifact: { name: "Long handbook", slug },
        href: `/projects/CORE/documents/${slug}`,
        id: "document-3",
        kind: "document",
        owner: {
          artifact_id: "document-3",
          kind: "document",
          name: "Long handbook",
          project: "CORE",
          slug,
        },
        rank: 1,
        snippet: "Keep the <mark>astrolabe</mark> dry.",
      },
    ])
  );
  // The bar reads the viewer, to list what that person opened lately, the project list, to offer
  // a project row for every project the words name, and what this Dispatch can do, to know
  // whether it answers. This row's page has an origin of its own - the hrefs are read against
  // it - and it is not the suite's, so the shell's own answers to those three routes do not
  // resolve against it: each is answered here at the api client, as the shell answers it, signed
  // out, with no project and with nothing configured. None adds a row, so the two document hits
  // below are the whole list.
  const whoAmI = spyOn(api, "whoAmI").mockRejectedValue(
    new ApiError(401, { code: "NO_IDENTITY", error: "no identity" })
  );
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([]);
  const goingCapability = spyOn(api, "goingCapability").mockResolvedValue(UNCONFIGURED_CAPABILITY);
  const testWindow = window as unknown as HappyDOMWindow;
  const pageBefore = window.location.href;
  testWindow.happyDOM.setURL("https://dispatch.test/issues/LEGION-2");
  const view = renderPalette();

  try {
    await searchFor(user, "astrolabe");
    expect(
      screen.getAllByRole("option").map((option) => option.getAttribute("data-dispatch-ref"))
    ).toEqual([`dispatch://LEGION-2/artifact/${slug}`, `dispatch://CORE/artifact/${slug}`]);
  } finally {
    search.mockRestore();
    whoAmI.mockRestore();
    listProjects.mockRestore();
    goingCapability.mockRestore();
    view.unmount();
    view.queryClient.clear();
    testWindow.happyDOM.setURL(pageBefore);
  }
});

test("does not query below two characters", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([]));
  const view = renderPalette();

  try {
    const input = screen.getByRole("combobox", { name: "Search" });
    await user.clear(input);
    await user.click(input);
    await user.paste("a");
    // Past both of the bar's timers, by the bar's own numbers, so a search of one character
    // would have gone out by now if the bar made one - and so this row goes on reaching both
    // tiers if either number ever moves.
    await tick(KEYSTROKE_DEBOUNCE_MS + MEANING_PAUSE_MS);

    expect(search).not.toHaveBeenCalled();
    expect(screen.getByText("Type at least 2 characters")).not.toBeNull();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("names the limit instead of querying over it, and queries at it", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([]));
  const view = renderPalette();

  try {
    const input = screen.getByRole("combobox", { name: "Search" });
    await user.clear(input);
    await user.click(input);
    await user.paste("x".repeat(SEARCH_QUERY_MAX + 1));

    expect(
      screen.getByText(
        `Search with a short phrase: ${SEARCH_QUERY_MAX + 1} characters is over the ${SEARCH_QUERY_MAX}-character limit`
      )
    ).not.toBeNull();
    // Past both of the bar's timers, by the bar's own numbers: a query over the limit must
    // reach neither the keystroke tier nor the meaning tier, and an advance short of the pause
    // would leave the second half of that unchecked.
    await tick(KEYSTROKE_DEBOUNCE_MS + MEANING_PAUSE_MS);
    expect(search).not.toHaveBeenCalled();

    await user.clear(input);
    await user.click(input);
    await user.paste("x".repeat(SEARCH_QUERY_MAX));
    await waitFor(() =>
      expect(search).toHaveBeenCalledWith(
        "x".repeat(SEARCH_QUERY_MAX),
        { keywords_only: true },
        aborts
      )
    );
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("arrow keys move aria-activedescendant and Enter navigates to the active href", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  const view = renderPalette();

  try {
    const input = await searchFor(user, "astrolabe");
    // Named rather than indexed with `?.`: `toHaveAttribute` given an `undefined` value checks
    // that the attribute is present and not what it holds, so an option the palette failed to
    // list would pass the two rows below rather than failing them.
    const [first, second] = screen.getAllByRole("option");
    if (first === undefined || second === undefined)
      throw new Error("the palette listed fewer than two options");
    expect(input).toHaveAttribute("aria-activedescendant", first.id);

    await user.keyboard("{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", second.id);

    await user.keyboard("{ArrowUp}");
    await user.keyboard("{Enter}");

    expect(screen.getByTestId("current-route").textContent).toBe(
      "/issues/LEGION-2/spec?q=astrolabe"
    );
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// An Enter that confirms an input method's conversion is the input method's: Chromium and Firefox
// mark it `isComposing`, and Safari sends it after the composition's end, unmarked, with keyCode
// 229. It opens no hit; a plain Enter after it opens the highlighted one.
test("an Enter that confirms an input method's conversion opens no hit, and a plain Enter opens the highlighted one", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  const view = renderPalette();
  try {
    const input = await searchFor(user, "astrolabe");
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(input, { isComposing: true, key: "Enter" });
    fireEvent.keyDown(input, { isComposing: false, key: "Enter", keyCode: 229 });
    expect(screen.getByTestId("current-route").textContent).toBe("/");
    expect(screen.getByRole("combobox", { name: "Search" })).toBe(input);

    await user.keyboard("{Enter}");
    expect(screen.getByTestId("current-route").textContent).toBe(
      "/issues/LEGION-2/spec?q=astrolabe"
    );
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// A key that is part of an input method's composition is the composition's, and its default
// stays the browser's: the arrows move no highlight, Enter opens no row, and Backspace on the
// empty box widens no chip. The bar is opened over an issue page, where the plain Backspace on
// the empty box does widen the chip to the project; the page has no writing cursor in a block,
// so Tab narrows nothing there with or without a composition.
test("arrow keys, Enter and Backspace inside an IME composition move no highlight, route or chip", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  // The bar opened over an issue page may read that issue. No agent holds LEGION-2, and the hits
  // are the whole list.
  const getIssue = spyOn(api, "getIssue").mockResolvedValue({
    ...issueFixture,
    artifacts: [{ ...specArtifact, issue_key: "LEGION-2", project: "LEGION" }],
    key: "LEGION-2",
    number: 2,
    project: "LEGION",
    status: "done",
    title: "Navigation instruments",
  });
  const view = renderPalette({ entries: ["/issues/LEGION-2/spec"] });
  const box = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
  const chip = () => screen.getByTestId("going-box-chip").getAttribute("title");

  try {
    // Backspace widens the chip on an empty box alone, so it is pressed before the words are
    // typed. `fireEvent` answers false for a key whose default was cancelled, which the bar does
    // to each key it spends. user-event has no form for this: it cannot mark a key as an IME
    // composition.
    expect(chip()).toContain("Acting on LEGION-2");
    expect(fireEvent.keyDown(box, { isComposing: true, key: "Backspace" })).toBe(true);
    expect(chip()).toContain("Acting on LEGION-2");

    await searchFor(user, "astrolabe");
    // Two rows at least, so an arrow the bar took would move the highlight off the first.
    const [first, second] = screen.getAllByRole("option");
    if (first === undefined || second === undefined)
      throw new Error("the palette listed fewer than two options");
    expect(box).toHaveAttribute("aria-activedescendant", first.id);
    for (const key of ["ArrowDown", "ArrowUp", "Enter"]) {
      // user-event has no form for this: it cannot mark a key as an IME composition.
      expect(fireEvent.keyDown(box, { isComposing: true, key }), `${key} composing`).toBe(true);
      expect(box, `${key} composing`).toHaveAttribute("aria-activedescendant", first.id);
    }
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/LEGION-2/spec");

    // The plain Backspace on the emptied box is the bar's: it widens the chip the composing one
    // left alone.
    await user.clear(box);
    await user.keyboard("{Backspace}");
    expect(chip()).toContain("Acting on the project LEGION");
  } finally {
    search.mockRestore();
    getIssue.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// A Tab that is part of an input method's composition is the composition's too: it narrows no
// chip, and its default stays the browser's. The bar is opened with the writing cursor in a block
// of a document behind it, which is where a plain Tab narrows the chip to that block. It is opened
// over an issue page, so the chip names the issue before any key is pressed.
test("Tab inside an IME composition moves no chip, where a plain Tab would", async () => {
  const user = userOnFakeClock();
  // The bar reads the writing cursor on the render that opens it (`blockAtCursor`), so the cursor
  // is put in a block of a writing surface before the bar is drawn.
  const editable = document.createElement("div");
  editable.setAttribute("contenteditable", "true");
  const block = document.createElement("p");
  block.setAttribute(BLOCK_ID_ATTRIBUTE, "b-1");
  block.textContent = "The sextant sights the horizon.";
  editable.append(block);
  document.body.append(editable);
  editable.focus();
  const caret = document.createRange();
  caret.setStart(block.firstChild ?? block, 0);
  caret.collapse(true);
  window.getSelection()?.removeAllRanges();
  window.getSelection()?.addRange(caret);
  const view = renderPalette({ entries: ["/issues/LEGION-2/spec"] });
  const box = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
  const chip = () => screen.getByTestId("going-box-chip").getAttribute("title");

  try {
    // A composition goes on in the box the reader types into, so the box holds the focus first:
    // the bar takes the focus on itself as it opens and hands it to the box a frame later, and
    // until then a Tab is the dialog's own, to its first control (`useDialog`).
    await user.click(box);
    // `fireEvent` answers false for a key whose default was cancelled, which the bar does to
    // each key it spends. user-event has no form for this: it cannot mark a key as an IME
    // composition.
    expect(chip()).toContain("Acting on LEGION-2");
    expect(fireEvent.keyDown(box, { isComposing: true, key: "Tab" })).toBe(true);
    expect(chip()).toContain("Acting on LEGION-2");

    // The plain Tab is the bar's: it narrows the chip to the block the composing one left alone.
    await user.keyboard("{Tab}");
    expect(chip()).toContain("Acting on the block “The sextant sights the horizon.”");
  } finally {
    view.unmount();
    view.queryClient.clear();
    editable.remove();
    window.getSelection()?.removeAllRanges();
  }
});

// Safari sends the key that ends an input method's composition after the composition's end,
// unmarked - `isComposing` false - with keyCode 229 (WebKit bug 165004): that key is the
// composition's as well. In the bar's box an arrow moves no highlight and Enter opens no row, and
// in the filter panel's Author box Enter takes no author. The plain Enter after it takes the
// author.
test("keys that end an IME composition in Safari, at keyCode 229, move no highlight, open no row and take no author", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  const view = renderPalette();

  try {
    const box = await searchFor(user, "astrolabe");
    // Two rows at least, so an arrow the bar took would move the highlight off the first.
    const [first, second] = screen.getAllByRole("option");
    if (first === undefined || second === undefined)
      throw new Error("the palette listed fewer than two options");
    expect(box).toHaveAttribute("aria-activedescendant", first.id);
    for (const key of ["ArrowDown", "ArrowUp", "Enter"]) {
      // `fireEvent` answers false for a key whose default was cancelled, which the bar does to
      // each key it spends. user-event has no form for this: it cannot mark a key as an IME
      // composition.
      const kept = fireEvent.keyDown(box, { isComposing: false, key, keyCode: 229 });
      expect(kept, `${key} at keyCode 229`).toBe(true);
      expect(box, `${key} at keyCode 229`).toHaveAttribute("aria-activedescendant", first.id);
    }
    expect(screen.getByTestId("current-route").textContent).toBe("/");

    await openFilters(user);
    const author = screen.getByRole("searchbox", { name: "Author" });
    await user.click(author);
    await user.paste("alice");
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(author, { isComposing: false, key: "Enter", keyCode: 229 });
    expect(
      screen.queryByRole("button", { name: "Remove Author alice filter" }),
      "an Enter at keyCode 229 took the author"
    ).toBeNull();

    await user.keyboard("{Enter}");
    expect(
      await screen.findByRole("button", { name: "Remove Author alice filter" })
    ).not.toBeNull();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

function issueHit(key: string): SearchResult {
  return {
    href: `/issues/${key}`,
    id: key,
    kind: "issue",
    owner: { key, kind: "issue", status: "todo", title: `Instrument ${key}` },
    rank: 1,
    snippet: `The <mark>astrolabe</mark> of ${key}.`,
  };
}

test("a refetch that re-ranks the hits leaves the highlight on the hit the reader sees", async () => {
  const user = userOnFakeClock();
  const [first, second] = [issueHit("LEGION-3"), issueHit("LEGION-4")];
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([first, second]));
  const view = renderPalette();

  try {
    const input = await searchFor(user, "astrolabe");
    const shown = input.getAttribute("aria-activedescendant");
    expect(shown).toBe(screen.getAllByRole("option")[0]?.id ?? "");

    // A focus refetch, or the refresh after a gap in the live socket, answers the same query
    // re-ranked.
    search.mockResolvedValue(searchAnswer([second, first]));
    await act(async () => {
      await view.queryClient.refetchQueries();
    });
    await waitFor(() => expect(screen.getAllByRole("option")[0]?.id).not.toBe(shown));
    expect(input).toHaveAttribute("aria-activedescendant", shown);
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

/** The bar's two searches of one query, each answering its own list: `words` for the keystroke's
 *  search, which asks for the keyword leg alone, and `fused` for the search the pause makes,
 *  which asks for the whole of it. */
function bothTiers(
  words: SearchResult[],
  fused: SearchResult[]
): (query: string, options?: { keywords_only?: boolean }) => Promise<SearchResponse> {
  return (_query, options = {}) =>
    Promise.resolve(searchAnswer(options.keywords_only === true ? words : fused));
}

test("a pause searches by meaning too, and its order re-ranks the list under the highlight", async () => {
  const user = userOnFakeClock();
  const [first, second] = [issueHit("LEGION-3"), issueHit("LEGION-4")];
  const search = spyOn(api, "search").mockImplementation(
    bothTiers([first, second], [second, first])
  );
  const view = renderPalette();

  try {
    const input = await searchFor(user, "astrolabe");
    expect(optionIds()).toEqual([optionId(first), optionId(second)]);
    expect(input).toHaveAttribute("aria-activedescendant", optionId(first));
    expect(search.mock.calls).toHaveLength(1);
    expect(search.mock.calls[0]?.[1]).toEqual({ keywords_only: true });

    // The pause asks the same query again, without the flag, and the fused order it answers
    // replaces the list. The highlighted row is still in that list, so it keeps the highlight
    // and Enter still opens the row the reader is looking at. The clock crosses the 500 ms
    // pause here deliberately: this row is about what firing it does.
    await tick(500);
    await waitFor(() => expect(search.mock.calls).toHaveLength(2), { timeout: 3000 });
    expect(search.mock.calls[1]?.[0]).toBe("astrolabe");
    expect(search.mock.calls[1]?.[1]).toEqual({});
    await waitFor(() => expect(optionIds()).toEqual([optionId(second), optionId(first)]));
    expect(input).toHaveAttribute("aria-activedescendant", optionId(first));

    await user.keyboard("{Enter}");
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/LEGION-3");
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a re-rank that drops the highlighted hit leaves no row highlighted, and Enter opens nothing", async () => {
  const user = userOnFakeClock();
  const [first, second] = [issueHit("LEGION-3"), issueHit("LEGION-4")];
  const search = spyOn(api, "search").mockImplementation(bothTiers([first, second], [first]));
  const view = renderPalette();

  try {
    const input = await searchFor(user, "astrolabe");
    await user.keyboard("{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", optionId(second));

    // The pause's order drops the row the reader had arrowed to. Nothing takes its place: the
    // head would be a row they never chose, and the next Enter would open it. The clock
    // crosses the 500 ms pause here deliberately: this row is about what firing it does.
    await tick(500);
    await waitFor(() => expect(optionIds()).toEqual([optionId(first)]), { timeout: 3000 });
    expect(input.getAttribute("aria-activedescendant")).toBeNull();
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("current-route").textContent).toBe("/");

    // The reader's own arrow says which row, and Enter then opens that one.
    await user.keyboard("{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", optionId(first));
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/LEGION-3");
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a first answer with no hits and a second that carries one leave that hit highlighted, and Enter opens it", async () => {
  const user = userOnFakeClock();
  const [only] = [issueHit("LEGION-3")];
  const search = spyOn(api, "search").mockImplementation(bothTiers([], [only]));
  const view = renderPalette();

  try {
    // The keystroke's search answers this query with no hit, so the list holds nothing to
    // highlight. The clock moves the debounce's length alone, short of the pause.
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    await user.click(input);
    await user.paste("astrolabe");
    await tick(150);
    expect(search.mock.calls).toHaveLength(1);
    expect(search.mock.calls[0]?.[1]).toEqual({ keywords_only: true });
    expect(screen.getByText(/No results for/)).toBeInTheDocument();
    expect(optionIds()).not.toContain(optionId(only));
    expect(input.getAttribute("aria-activedescendant")).toBeNull();

    // The pause's search answers the same query with its one hit. No row was highlighted, so
    // no row the reader held has left: the hit is the query's first and takes the highlight.
    // The clock crosses the 500 ms pause here deliberately: this row is about what firing it
    // does.
    await tick(500);
    await waitFor(() => expect(optionIds()).toContain(optionId(only)), { timeout: 3000 });
    expect(input).toHaveAttribute("aria-activedescendant", optionId(only));
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/LEGION-3");
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// A filter chip is another search under another key, so the answer a chip's change brings is
// that search's first hits, never the words' hits re-ranked, though the words are the same: an
// answer the bar already holds for it, painted at once, still sends the highlight to its head.
test("a chip taken off, whose search the bar already holds an answer for, highlights that answer's first hit", async () => {
  const user = userOnFakeClock();
  const [first, second, other] = [issueHit("LEGION-3"), issueHit("LEGION-4"), issueHit("LEGION-5")];
  const search = spyOn(api, "search").mockImplementation((_query, request) =>
    Promise.resolve(searchAnswer(request?.status === "in_progress" ? [other] : [first, second]))
  );
  const view = renderPalette();

  try {
    const input = await searchFor(user, "astrolabe");
    expect(input).toHaveAttribute("aria-activedescendant", optionId(first));

    // The chip's search answers its one hit, which takes the highlight.
    await user.click(screen.getByRole("button", { name: "Filters" }));
    await user.click(screen.getByRole("button", { name: "Status" }));
    await user.click(screen.getByRole("option", { name: "In progress" }));
    await waitFor(() => expect(optionIds()).toEqual([optionId(other)]));
    expect(input).toHaveAttribute("aria-activedescendant", optionId(other));

    // Taken off, the chip leaves the words' own search, whose answer the bar holds: it paints at
    // once, and its first hit takes the highlight, so Enter opens it.
    await user.click(screen.getByRole("button", { name: "Remove Status In progress filter" }));
    await waitFor(() => expect(optionIds()).toEqual([optionId(first), optionId(second)]));
    expect(input).toHaveAttribute("aria-activedescendant", optionId(first));
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a background refresh that fails keeps the hits the query has, and the highlight on its hit", async () => {
  const user = userOnFakeClock();
  const [first, second] = [issueHit("LEGION-3"), issueHit("LEGION-4")];
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([first, second]));
  const view = renderPalette();

  try {
    const input = await searchFor(user, "astrolabe");
    await user.keyboard("{ArrowDown}");
    const arrowed = input.getAttribute("aria-activedescendant");
    expect(arrowed).toBe(screen.getAllByRole("option")[1]?.id ?? "");

    // The tab-return refresh fails; TanStack keeps the answer it already has.
    search.mockRejectedValue(new Error("503"));
    await act(async () => {
      await view.queryClient.refetchQueries();
    });
    await waitFor(() =>
      expect(
        view.queryClient.getQueryState(["search", "astrolabe", { keywords_only: true }])?.status
      ).toBe("error")
    );
    expect(screen.getAllByRole("option")).toHaveLength(2);
    expect(screen.queryByText("Search failed.")).not.toBeInTheDocument();
    expect(input).toHaveAttribute("aria-activedescendant", arrowed);
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// The shell's part in a chosen action: the palette closes, and the row runs once it has.
function PaletteHost(): ReactNode {
  const [open, setOpen] = useState(true);
  return <SearchPalette onClose={() => setOpen(false)} open={open} />;
}

function renderPaletteHost(): { queryClient: QueryClient; unmount: () => void } {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <CurrentRoute />
          <PaletteHost />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, unmount: view.unmount };
}

const CLOSE_ROW = "search-option-action-global-close";

// `Close issue` stands for any row whose label holds a search word somewhere past its start.
function registerClose(ran: string[]): () => void {
  return appKeymap.register("global", [
    { id: "close", keys: [], label: "Close issue", run: () => ran.push("close") },
  ]);
}

function optionIds(): string[] {
  return screen.queryAllByRole("option").map((option) => option.id);
}

test("a query found inside an action's label lists the action below the hits, and Enter opens the first hit", async () => {
  const user = userOnFakeClock();
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const hit = issueHit("LEGION-3");
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([hit]));
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    await user.clear(input);
    await user.click(input);
    await user.paste("issue");
    // The keystroke debounce, after which the search for these words goes out and answers.
    await tick(150);
    await waitFor(() => expect(optionIds()).toEqual([optionId(hit), CLOSE_ROW]));
    expect(input).toHaveAttribute("aria-activedescendant", optionId(hit));

    await user.keyboard("{Enter}");
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/LEGION-3");
    expect(ran).toEqual([]);
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("while the search is in flight no action found inside its label is highlighted, so Enter runs nothing", async () => {
  const user = userOnFakeClock();
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const hit = issueHit("LEGION-3");
  const answer = Promise.withResolvers<SearchResponse>();
  const search = spyOn(api, "search").mockReturnValue(answer.promise);
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    await user.clear(input);
    await user.click(input);
    await user.paste("issue");
    // The keystroke debounce, after which the search goes out; this one never answers.
    await tick(150);
    await waitFor(() => expect(search).toHaveBeenCalled());
    expect(optionIds()).toEqual([CLOSE_ROW]);
    expect(input.getAttribute("aria-activedescendant")).toBeNull();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("dialog", { name: "Search" })).toBeTruthy();
    expect(ran).toEqual([]);

    // The hits arriving take the head; the action row was never adopted while they were out.
    await act(async () => answer.resolve(searchAnswer([hit])));
    await waitFor(() => expect(input).toHaveAttribute("aria-activedescendant", optionId(hit)));
    expect(optionIds()).toEqual([optionId(hit), CLOSE_ROW]);
    // Enter then opens the hit the highlight landed on, rather than the action row below it.
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/LEGION-3");
    expect(ran).toEqual([]);
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a search that finds nothing still leaves an action found inside its label unhighlighted", async () => {
  const user = userOnFakeClock();
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([]));
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    await user.clear(input);
    await user.click(input);
    await user.paste("issue");
    // The keystroke debounce, after which the search goes out and answers with nothing.
    await tick(150);
    await screen.findByText('No results for "issue"');
    expect(optionIds()).toEqual([CLOSE_ROW]);
    expect(input.getAttribute("aria-activedescendant")).toBeNull();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("dialog", { name: "Search" })).toBeTruthy();
    expect(ran).toEqual([]);
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("an arrow pressed before the search answers reaches no action found inside its label", async () => {
  const user = userOnFakeClock();
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const hit = issueHit("LEGION-3");
  const answer = Promise.withResolvers<SearchResponse>();
  const search = spyOn(api, "search").mockReturnValue(answer.promise);
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    await user.clear(input);
    await user.click(input);
    await user.paste("issue");
    // Down and Enter in one burst, inside the debounce and again once the request is out.
    await user.keyboard("{ArrowDown}");
    await user.keyboard("{Enter}");
    expect(input.getAttribute("aria-activedescendant")).toBeNull();
    // Past the debounce now, so the request for these words is out and still unanswered.
    await tick(150);
    await waitFor(() => expect(search).toHaveBeenCalled());
    await user.keyboard("{ArrowDown}");
    await user.keyboard("{ArrowUp}");
    expect(input.getAttribute("aria-activedescendant")).toBeNull();
    await user.keyboard("{Enter}");
    expect(ran).toEqual([]);

    // The hits arrive above it and take the head; Enter opens the first one.
    await act(async () => answer.resolve(searchAnswer([hit])));
    await waitFor(() => expect(optionIds()).toEqual([optionId(hit), CLOSE_ROW]));
    expect(input).toHaveAttribute("aria-activedescendant", optionId(hit));
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/LEGION-3");
    expect(ran).toEqual([]);
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("once the search has answered with no hit, Down takes the first action found inside its label", async () => {
  const user = userOnFakeClock();
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([]));
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    await user.clear(input);
    await user.click(input);
    await user.paste("issue");
    // The keystroke debounce, after which the search goes out and answers with nothing.
    await tick(150);
    await screen.findByText('No results for "issue"');
    await user.keyboard("{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", CLOSE_ROW);
    await user.keyboard("{Enter}");
    await waitFor(() => expect(ran).toEqual(["close"]));
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("while the search is out the arrows walk only the actions the query starts, which keep the highlight as hits arrive", async () => {
  const user = userOnFakeClock();
  const ran: string[] = [];
  const unregister = appKeymap.register("global", [
    { id: "close", keys: [], label: "Close issue", run: () => ran.push("close") },
    { id: "report", keys: [], label: "Issue the report", run: () => ran.push("report") },
    { id: "triage", keys: [], label: "Issue triage", run: () => ran.push("triage") },
  ]);
  const hit = issueHit("LEGION-3");
  const answer = Promise.withResolvers<SearchResponse>();
  const search = spyOn(api, "search").mockReturnValue(answer.promise);
  const view = renderPaletteHost();
  const [report, triage] = [
    "search-option-action-global-report",
    "search-option-action-global-triage",
  ];

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    await user.clear(input);
    await user.click(input);
    await user.paste("issue");
    // The keystroke debounce, after which the search goes out; this one never answers.
    await tick(150);
    await waitFor(() => expect(search).toHaveBeenCalled());
    expect(optionIds()).toEqual([report, triage, CLOSE_ROW]);
    expect(input).toHaveAttribute("aria-activedescendant", report);
    await user.keyboard("{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", triage);
    await user.keyboard("{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", report);
    await user.keyboard("{ArrowUp}");
    expect(input).toHaveAttribute("aria-activedescendant", triage);

    await act(async () => answer.resolve(searchAnswer([hit])));
    await waitFor(() => expect(optionIds()).toEqual([report, triage, optionId(hit), CLOSE_ROW]));
    expect(input).toHaveAttribute("aria-activedescendant", triage);
    await user.keyboard("{Enter}");
    await waitFor(() => expect(ran).toEqual(["triage"]));
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("once the search has failed, Down takes the first action found inside its label", async () => {
  const user = userOnFakeClock();
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const search = spyOn(api, "search").mockRejectedValue(new Error("503"));
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    await user.clear(input);
    await user.click(input);
    await user.paste("issue");
    // The keystroke debounce, after which the search goes out and fails.
    await tick(150);
    expect(await screen.findByRole("alert")).toHaveTextContent("Search failed.");
    await user.keyboard("{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", CLOSE_ROW);
    await user.keyboard("{Enter}");
    await waitFor(() => expect(ran).toEqual(["close"]));
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a background refetch keeps the actions the query starts and the hits walkable, and a highlight already among the More actions moving", async () => {
  const user = userOnFakeClock();
  const ran: string[] = [];
  const unregister = appKeymap.register("global", [
    { id: "close", keys: [], label: "Close issue", run: () => ran.push("close") },
    { id: "report", keys: [], label: "Issue the report", run: () => ran.push("report") },
  ]);
  const hit = issueHit("LEGION-3");
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([hit]));
  const view = renderPaletteHost();
  const report = "search-option-action-global-report";
  const refetchHeld = async (calls: number) => {
    const answer = Promise.withResolvers<SearchResponse>();
    search.mockReturnValue(answer.promise);
    act(() => {
      void view.queryClient.refetchQueries();
    });
    await waitFor(() => expect(search).toHaveBeenCalledTimes(calls));
    return answer;
  };

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    await user.clear(input);
    await user.click(input);
    await user.paste("issue");
    // The palette's 150 ms keystroke debounce, which the search for these words waits out.
    await tick(150);
    await waitFor(() => expect(optionIds()).toEqual([report, optionId(hit), CLOSE_ROW]));
    expect(input).toHaveAttribute("aria-activedescendant", report);

    // A focus or staleTime refetch is out with the hits still shown: the arrows walk the
    // leading row and the hits, and stop short of the More actions.
    const first = await refetchHeld(2);
    await user.keyboard("{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", optionId(hit));
    await user.keyboard("{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", report);
    await user.keyboard("{ArrowUp}");
    expect(input).toHaveAttribute("aria-activedescendant", optionId(hit));
    await act(async () => first.resolve(searchAnswer([hit])));
    await waitFor(() => expect(view.queryClient.isFetching()).toBe(0));
    await user.keyboard("{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", CLOSE_ROW);

    // Moved there after an answer, a highlight among the More actions keeps moving over the
    // whole list through the next refetch.
    const second = await refetchHeld(3);
    await user.keyboard("{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", report);
    await act(async () => second.resolve(searchAnswer([hit])));
    expect(ran).toEqual([]);
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a query that starts an action's label lists it above the hits, and Enter runs it", async () => {
  const user = userOnFakeClock();
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const hit = issueHit("LEGION-3");
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([hit]));
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    await user.clear(input);
    await user.click(input);
    await user.paste("clo");
    // The keystroke debounce, after which the search for these words goes out and answers.
    await tick(150);
    await waitFor(() => expect(optionIds()).toEqual([CLOSE_ROW, optionId(hit)]));
    expect(input).toHaveAttribute("aria-activedescendant", CLOSE_ROW);

    await user.keyboard("{Enter}");
    await waitFor(() => expect(ran).toEqual(["close"]));
    expect(screen.getByTestId("current-route").textContent).toBe("/");
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

// On a Dispatch with no deciding model the line on top is always Ask and never File (R2), so the
// bar offers words its search matched nothing with as a new issue, in the New issue group at the
// foot of the list, under the hits and the More actions, as main draws it (design v3, state 13);
// pressing it files the issue as main's offer does. With a deciding model configured, the line's
// File is the way to file, and the bar offers no group, whether or not that model answers.
const NEW_ISSUE_WORDS = "sextant rota";
const ROTA_ROW = "search-option-action-global-rota";

test("with no deciding model, words that match nothing offer a New issue group under the More actions, and choosing it files the issue as main's offer does", async () => {
  const user = userOnFakeClock();
  const unregister = appKeymap.register("global", [
    { id: "rota", keys: [], label: "Print the sextant rota", run: () => {} },
  ]);
  const made = { ...issueFixture, key: "CORE-7", number: 7, title: NEW_ISSUE_WORDS };
  const spies = {
    capability: spyOn(api, "goingCapability").mockResolvedValue(UNCONFIGURED_CAPABILITY),
    createIssue: spyOn(api, "createIssue").mockResolvedValue(made),
    createMessage: spyOn(api, "createMessage").mockResolvedValue({} as Message),
    patchIssue: spyOn(api, "patchIssue").mockResolvedValue({ ...made, previous: {} }),
    search: spyOn(api, "search").mockResolvedValue(searchAnswer([])),
    settings: spyOn(api, "getNewIssueSettings").mockResolvedValue({
      label: "from-the-bar",
      session: "sess-1",
    }),
    uploadArtifact: spyOn(api, "uploadArtifact").mockResolvedValue({} as ArtifactUploadResponse),
  };
  const view = renderPalette({ entries: ["/projects/CORE/issues"] });

  try {
    const input = await searchFor(user, NEW_ISSUE_WORDS);
    await screen.findByText(`No results for "${NEW_ISSUE_WORDS}"`);
    const group = await screen.findByRole("group", { name: "New issue" });
    expect(within(group).getByRole("option")).toHaveTextContent(
      `Make a new issue from “${NEW_ISSUE_WORDS}”`
    );
    // The group is the list's last, under the More action the words hold further in; the
    // highlight never moves to a write by itself, and nothing is written before the press.
    expect(optionIds()).toEqual([ROTA_ROW, NEW_ISSUE_OPTION_ID]);
    expect(input.getAttribute("aria-activedescendant")).toBeNull();
    expect(spies.createIssue).not.toHaveBeenCalled();

    await user.keyboard("{ArrowDown}{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", NEW_ISSUE_OPTION_ID);
    expect(screen.getByTestId("search-keyboard-hints")).toHaveTextContent("↵ Make · Esc Close");
    await user.keyboard("{Enter}");

    // Main's filing: the create, labelled; the words as the issue's first criterion; its route
    // to the session the deployment names, and its first message there; then the issue opens.
    await waitFor(() => expect(spies.createMessage).toHaveBeenCalled());
    expect(spies.createIssue.mock.calls[0]?.[0]).toEqual({
      labels: ["from-the-bar"],
      project: "CORE",
      title: NEW_ISSUE_WORDS,
    });
    expect(spies.uploadArtifact.mock.calls[0]?.slice(0, 2)).toEqual([
      { issue: "CORE-7" },
      { content: firstCriterion(NEW_ISSUE_WORDS), name: "verification.md" },
    ]);
    expect(spies.patchIssue.mock.calls[0]?.slice(0, 2)).toEqual([
      "CORE-7",
      { route: "session:sess-1" },
    ]);
    expect(spies.createMessage.mock.calls[0]?.slice(0, 2)).toEqual([
      "CORE-7",
      { body: NEW_ISSUE_WORDS, delivery: "aside", target: "session:sess-1" },
    ]);
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-7")
    );
  } finally {
    for (const spy of Object.values(spies)) spy.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("with a deciding model configured, words that match nothing offer no New issue group", async () => {
  const user = userOnFakeClock();
  const spies = [
    spyOn(api, "goingCapability").mockResolvedValue(CONFIGURED_CAPABILITY),
    spyOn(api, "createGoingRequest").mockImplementation(
      () => Promise.withResolvers<never>().promise
    ),
    spyOn(api, "search").mockResolvedValue(searchAnswer([])),
  ];
  const settings = spyOn(api, "getNewIssueSettings").mockResolvedValue({
    label: "from-the-bar",
    session: "sess-1",
  });
  const view = renderPalette({ entries: ["/projects/CORE/issues"] });

  try {
    await searchFor(user, NEW_ISSUE_WORDS);
    await screen.findByText(`No results for "${NEW_ISSUE_WORDS}"`);
    // The capability is read: the line on top is drawn, which only a Dispatch that answers draws.
    expect(document.getElementById("search-option-line")).not.toBeNull();
    expect(screen.queryByRole("group", { name: "New issue" })).toBeNull();
    expect(optionIds()).not.toContain(NEW_ISSUE_OPTION_ID);
    expect(settings).not.toHaveBeenCalled();
  } finally {
    for (const spy of [...spies, settings]) spy.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("Escape calls onClose", async () => {
  const user = userOnFakeClock();
  let closed = 0;
  const view = renderPalette({
    onClose: () => {
      closed += 1;
    },
  });

  try {
    // Escape is the dialog's own keymap binding, registered at the document, so the key
    // reaches it from whatever holds focus inside the open palette.
    await user.keyboard("{Escape}");
    expect(closed).toBe(1);
  } finally {
    view.unmount();
    view.queryClient.clear();
  }
});

test("Escape closes the palette even when pressed before focus has moved into it", async () => {
  const user = userOnFakeClock();
  let closed = 0;
  const view = renderPalette({
    onClose: () => {
      closed += 1;
    },
  });

  try {
    // The dialog moves focus inside itself one animation frame after opening; a key pressed
    // in that frame lands on whatever was focused before (the document editor, the body).
    // user-event sends its key to whatever holds focus, so the focus the dialog took is given
    // up here: the press has to reach the dialog from outside it, as a press in that frame does.
    (document.activeElement as HTMLElement | null)?.blur();
    await user.keyboard("{Escape}");
    expect(closed).toBe(1);
  } finally {
    view.unmount();
    view.queryClient.clear();
  }
});

/** The four rows below drive one navigation each at an open palette and count the closes. */
test("closes the palette when navigation changes", async () => {
  const user = userOnFakeClock();
  let closed = 0;
  const view = renderPalette({
    control: <RouteChanger />,
    onClose: () => {
      closed += 1;
    },
  });

  try {
    await user.click(screen.getByRole("button", { name: "Navigate elsewhere" }));
    await waitFor(() => expect(closed).toBe(1));
  } finally {
    view.unmount();
    view.queryClient.clear();
  }
});

// A bare project path redirects to its board once its architecture sources have loaded, and a
// `replace` mints a fresh `location.key` like any other navigation. The reader opened the bar
// over that page and is still on it, so the bar stays open (dispatch://INFRA-2452).
test("leaves the palette open when the route settles itself with a replace", async () => {
  const user = userOnFakeClock();
  let closed = 0;
  const view = renderPalette({
    control: <RouteSettler />,
    entries: ["/projects/LEGION"],
    onClose: () => {
      closed += 1;
    },
  });

  try {
    await user.click(screen.getByRole("button", { name: "Settle the route" }));
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/projects/LEGION/issues")
    );
    expect(closed).toBe(0);
    expect(screen.getByRole("combobox", { name: "Search" })).not.toBeNull();
  } finally {
    view.unmount();
    view.queryClient.clear();
  }
});

// A pop is the reader leaving the page the bar was opened over, as a push is, so it closes the
// bar; only a `replace` that moves the address is the page correcting itself under a reader who
// has not moved (dispatch://INFRA-2452).
test("closes the palette when the reader goes back", async () => {
  const user = userOnFakeClock();
  let closed = 0;
  const view = renderPalette({
    control: <RouteReturner />,
    entries: ["/projects/LEGION", "/issues/LEGION-9"],
    index: 1,
    onClose: () => {
      closed += 1;
    },
  });

  try {
    await user.click(screen.getByRole("button", { name: "Go back" }));
    await waitFor(() =>
      expect(screen.getByTestId("current-route").textContent).toBe("/projects/LEGION")
    );
    expect(closed).toBe(1);
  } finally {
    view.unmount();
    view.queryClient.clear();
  }
});

// React Router makes a `<Link>` click a `replace` whenever the link's target is the address
// already shown, and that replace moves nothing. The reader followed a link, so the bar closes -
// the Inbox drawer's owner link to the issue being read is this link (dispatch://INFRA-2452).
test("closes the palette when a link points at the address already shown", async () => {
  const user = userOnFakeClock();
  let closed = 0;
  const view = renderPalette({
    control: <SameAddressLink to="/issues/CORE-1" />,
    entries: ["/issues/CORE-1"],
    onClose: () => {
      closed += 1;
    },
  });

  try {
    await user.click(screen.getByRole("link", { name: "Open the issue" }));
    await waitFor(() => expect(closed).toBe(1));
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/CORE-1");
  } finally {
    view.unmount();
    view.queryClient.clear();
  }
});

test("shows a retryable failure when the search request fails", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockRejectedValue(new Error("500"));
  const view = renderPalette();

  try {
    const input = screen.getByRole("combobox", { name: "Search" });
    await user.clear(input);
    await user.click(input);
    await user.paste("astrolabe");
    // The keystroke debounce, after which the search goes out and fails.
    await tick(150);
    expect(await screen.findByRole("alert")).toHaveTextContent("Search failed.");
    expect(screen.getByRole("button", { name: "Retry" })).not.toBeNull();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// The filters (Project, Status, Author, Updated since) and the search syntax help are behind one
// Filters button at the box's right (design v3, state 1): they do not show otherwise, "/" in the
// empty box opens them, and "/" in a box with words is a character.
test("the filters and the syntax help are behind Filters, and / in the empty box opens them", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  const view = renderPalette();
  try {
    // The bar's first reads (the person, the conversations, the projects) land before the keys:
    // an answer that lands between two of them redraws the bar outside the keys' act.
    await waitFor(() => expect(view.queryClient.isFetching()).toBe(0));
    const box = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    const button = screen.getByRole("button", { name: "Filters" });
    expect(button).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("dialog", { name: "Filters" })).toBeNull();
    expect(screen.queryByTestId("search-operators")).toBeNull();

    await user.click(box);
    await user.keyboard("/");
    expect(box).toHaveValue("");
    expect(button).toHaveAttribute("aria-expanded", "true");
    const panel = screen.getByRole("dialog", { name: "Filters" });
    expect(within(panel).getByRole("button", { name: "Project" })).toBeInTheDocument();
    expect(within(panel).getByTestId("search-operators")).toBeInTheDocument();

    await user.click(button);
    expect(screen.queryByRole("dialog", { name: "Filters" })).toBeNull();
    await user.click(box);
    await user.keyboard("a/b");
    expect(box).toHaveValue("a/b");
    expect(screen.queryByRole("dialog", { name: "Filters" })).toBeNull();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// The three forms are offered whatever the query holds; which of them this query was read as
// using is the answer's own reading of it, so the bar and the search never disagree.
test("the operator forms are offered under Filters, and the answer says which the query uses", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue({
    ...searchAnswer(results),
    query: { broadened: false, operators: [SEARCH_OPERATOR_PHRASE, SEARCH_OPERATOR_EXCLUDE] },
  });
  const view = renderPalette();

  try {
    await searchFor(user, '"brass astrolabe" -steel');
    await user.click(screen.getByRole("button", { name: "Filters" }));
    const forms = screen.getByRole("list", { name: "What the words can do" });
    const inUse = (name: string) =>
      forms.querySelector(`[data-operator="${name}"]`)?.getAttribute("data-in-use");

    expect(forms.textContent).toContain('"two words"');
    expect(forms.textContent).toContain("this OR that");
    expect(forms.textContent).toContain("-word");
    await waitFor(() => expect(inUse("phrase")).toBe("true"));
    expect(inUse("exclude")).toBe("true");
    expect(inUse("or")).toBe("false");
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// The bar says nothing about how the deployment is set up (design v3, rule 8): a search that ran
// on words alone lists its hits and says nothing of it.
test("a search that ran on words alone shows its hits and says nothing of meaning search", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue({
    ...searchAnswer(results),
    degraded: SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE,
  });
  const view = renderPalette();

  try {
    await searchFor(user, "astrolabe");
    expect(screen.getAllByRole("option")).toHaveLength(3);
    expect(screen.queryByTestId("search-degraded")).toBeNull();
    expect(screen.queryByText(/meaning search/)).toBeNull();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

/** Opens the one panel of the bar's control row, which all four narrowings are added from, from
 *  the Filters button at the box's right, whose name counts the narrowings that are on. A
 *  narrowing already on is a chip in the row instead, so its control is not in the panel. */
async function openFilters(user: UserEvent): Promise<void> {
  await user.click(screen.getByRole("button", { name: /^Filters/ }));
}

// The dialog's own strip of controls is the control row itself, so the bar's box, the panel the
// narrowings are added from and the chips of what is on are one row and not two. The box is the
// shared search box, which stays the same element under a conversation, and its frame holds the
// row's one panel trigger, the Filters button, at its right (design v3, state 1); the chips stand
// in the row under the box, outside its frame.
test("the bar's controls are one control row: the box with Filters in its frame, and the chips under it", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  const view = renderPalette();

  try {
    const input = await searchFor(user, "astrolabe");
    const dialog = screen.getByRole("dialog", { name: "Search" });
    const rows = dialog.querySelectorAll(`[data-control="${CONTROL.controlRow}"]`);
    const triggers = dialog.querySelectorAll(`[data-control="${CONTROL.panelTrigger}"]`);
    const filters = screen.getByRole("button", { name: "Filters" });

    expect(rows).toHaveLength(1);
    // The query box is in that row, and it is the dialog's one text box: nothing between the
    // dialog's top and its first hit is a search box, a panel trigger or a chip of its own.
    const inputs = dialog.querySelectorAll("input");
    expect(inputs).toHaveLength(1);
    expect(inputs[0]).toBe(input);
    expect(input.getAttribute("data-control")).toBe(CONTROL.searchBox);
    expect(rows[0]?.contains(input)).toBe(true);
    expect(triggers).toHaveLength(1);
    expect(triggers[0]).toBe(filters);
    expect(filters.parentElement).toBe(input.parentElement);

    await openFilters(user);
    await user.click(screen.getByRole("button", { name: "Status" }));
    await user.click(screen.getByRole("option", { name: "In progress" }));
    const chip = await screen.findByRole("button", { name: "Remove Status In progress filter" });
    expect(chip.getAttribute("data-control")).toBe(CONTROL.chip);
    expect(chip.closest(`[data-control="${CONTROL.controlRow}"]`)).toBe(rows[0] ?? null);
    expect(input.parentElement?.contains(chip)).toBe(false);
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a chip narrows the search, reads back in the person's words, and comes off again", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  const view = renderPalette();

  try {
    await searchFor(user, "astrolabe");
    expect(search).toHaveBeenLastCalledWith("astrolabe", { keywords_only: true }, aborts);

    await openFilters(user);
    await user.click(screen.getByRole("button", { name: "Status" }));
    await user.click(screen.getByRole("option", { name: "In progress" }));
    await waitFor(() =>
      expect(search).toHaveBeenLastCalledWith(
        "astrolabe",
        { keywords_only: true, status: "in_progress" },
        aborts
      )
    );

    const chip = screen.getByRole("button", { name: "Remove Status In progress filter" });
    expect(chip.textContent).toContain("Status In progress");
    expect(chip.textContent).not.toContain("status:in_progress");
    // The button says how many filters narrow the search, so none narrows it unseen.
    expect(screen.getByRole("button", { name: "Filters · 1" })).toBeInTheDocument();
    // The chip is the shared one, in the shared row: what the bar narrows by is said where
    // every other page says it.
    expect(chip.getAttribute("data-control")).toBe(CONTROL.chip);
    expect(chip.closest(`[data-control="${CONTROL.controlRow}"]`)).not.toBeNull();

    await user.click(chip);
    await waitFor(() => expect(screen.queryByText("Status In progress")).toBeNull());
    // Removing the chip brings that narrowing's own control back into the panel.
    await openFilters(user);
    expect(screen.getByRole("button", { name: "Status" })).not.toBeNull();
    expect(screen.getByRole("button", { name: "Filters" })).toBeInTheDocument();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a date typed in Updated since narrows the search only once its year is whole", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  const view = renderPalette();

  try {
    await searchFor(user, "astrolabe");
    await openFilters(user);
    const box = screen.getByLabelText("Updated since");
    // An engine reports a valid date from the year's first typed digit, as Chromium does with
    // `0002-11-22`. user-event cannot type into a date box's parts, so the value the engine
    // reports is set as it would report it.
    fireEvent.change(box, { target: { value: "0002-11-22" } });
    expect(screen.getByLabelText("Updated since")).toBe(box);
    expect(screen.queryByRole("button", { name: /^Remove Updated since/ })).toBeNull();
    // The same box once the year's last digit is typed.
    fireEvent.change(box, { target: { value: "2026-11-22" } });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /^Remove Updated since/ })).not.toBeNull()
    );
    expect(screen.queryByLabelText("Updated since")).toBeNull();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a project picked in the panel narrows the search and reads back as its chip", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      gated_steps: [],
      key: "INFRA",
      name: "Infra",
      open_asks: 0,
      proven_sessions: false,
    },
  ]);
  const view = renderPalette();

  try {
    await searchFor(user, "astrolabe");
    await openFilters(user);
    await user.click(await screen.findByRole("button", { name: "Project" }));
    await user.click(await screen.findByRole("option", { name: "INFRA" }));

    await waitFor(() =>
      expect(search).toHaveBeenLastCalledWith(
        "astrolabe",
        { keywords_only: true, project: "INFRA" },
        aborts
      )
    );
    expect(screen.getByRole("button", { name: "Remove Project INFRA filter" })).not.toBeNull();
  } finally {
    listProjects.mockRestore();
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

/** The text boxes in the page now: a list's own box is one that appears when the list opens. */
function textBoxes(): Element[] {
  return [
    ...document.querySelectorAll('input[type="search"], input[type="text"], input:not([type])'),
  ];
}

/** Every element whose own name or text is `name`, as a button or an option: what a reader
 *  picks from a list, however the list draws it. */
function picks(name: RegExp): HTMLElement[] {
  return [
    ...screen.queryAllByRole("button", { name }),
    ...screen.queryAllByRole("option", { name }),
  ];
}

// Every list of options a reader picks from holds a text box that narrows the options, and the
// bar's Project and Status lists are such lists. Each one, opened from the panel,
// holds a text box of its own, and a word typed into it leaves only the options that match.
test("the bar's Project and Status lists each hold a text box that narrows their options", async () => {
  const user = userOnFakeClock();
  const listProjects = spyOn(api, "listProjects").mockResolvedValue(
    ["INFRA", "CORE"].map((key) => ({
      created_at: "2026-09-01T00:00:00Z",
      gated_steps: [],
      key,
      name: key === "INFRA" ? "Infra" : "Core",
      open_asks: 0,
      proven_sessions: false,
    }))
  );
  const view = renderPalette();

  try {
    const lists = [
      { menu: "Project", word: "COR", kept: /^CORE\b/, gone: /^INFRA\b/ },
      { menu: "Status", word: "progress", kept: /^In progress\b/, gone: /^Todo\b/ },
    ];
    for (const { menu, word, kept, gone } of lists) {
      await openFilters(user);
      const before = new Set(textBoxes());
      await user.click(await screen.findByRole("button", { name: menu }));
      await waitFor(() => expect(picks(gone).length).toBeGreaterThan(0));
      const own = textBoxes().filter((box) => !before.has(box));
      expect(own, `the ${menu} list holds no text box of its own`).toHaveLength(1);
      await user.type(own[0] as HTMLElement, word);
      await waitFor(() =>
        expect(picks(gone), `"${word}" leaves ${gone} in the ${menu} list`).toHaveLength(0)
      );
      expect(picks(kept).length, `"${word}" takes ${kept} out of the ${menu} list`).toBeGreaterThan(
        0
      );
      await user.keyboard("{Escape}");
      await user.keyboard("{Escape}");
    }
  } finally {
    listProjects.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// The Project list says whether the bar's read of the projects is still out or failed, rather
// than "No projects yet", as the Agents composer's Issue list does.
test("the bar's Project list says its projects are loading, then that they could not be loaded, and never that there are none", async () => {
  const user = userOnFakeClock();
  const read = Promise.withResolvers<Project[]>();
  const listProjects = spyOn(api, "listProjects").mockReturnValue(read.promise);
  const view = renderPalette();

  try {
    await openFilters(user);
    await user.click(await screen.findByRole("button", { name: "Project" }));
    const list = await screen.findByRole("listbox", { name: "Project options" });
    const panel = list.parentElement as HTMLElement;
    expect(list).toHaveTextContent("Loading projects…");
    expect(within(panel).queryByText("No projects yet")).toBeNull();

    await act(async () => {
      read.reject(new Error("projects unreachable"));
      await read.promise.catch(() => undefined);
    });
    expect(await within(panel).findByText("Could not load projects.")).toBeTruthy();
    expect(within(panel).queryByText("Loading projects…")).toBeNull();
    expect(within(panel).queryByText("No projects yet")).toBeNull();
  } finally {
    listProjects.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// Shift+Tab out of the Project list closes that list and gives the focus back to its Project
// trigger in the Filter panel. From there Escape closes the panel, with the focus on the bar's
// Filter trigger, and the next Escape closes the bar, so the bar can be closed from the keyboard.
test("after Shift+Tab out of the Project list, Escape closes the panel, and the next Escape the bar", async () => {
  const user = userOnFakeClock();
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([
    {
      created_at: "2026-09-01T00:00:00Z",
      gated_steps: [],
      key: "INFRA",
      name: "Infra",
      open_asks: 0,
      proven_sessions: false,
    },
  ]);
  let closed = 0;
  const view = renderPalette({
    onClose: () => {
      closed += 1;
    },
  });

  try {
    await openFilters(user);
    await user.click(await screen.findByRole("button", { name: "Project" }));
    const box = await screen.findByRole("combobox", { name: "Search projects" });
    act(() => box.focus());
    await user.tab({ shift: true });
    expect(screen.queryByRole("listbox", { name: "Project options" })).toBeNull();
    expect(screen.getByRole("button", { name: "Project" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "Filters" })).toBeNull();
    expect(screen.getByRole("button", { name: "Filters" })).toHaveFocus();
    expect(closed).toBe(0);
    await user.keyboard("{Escape}");
    expect(closed).toBe(1);
  } finally {
    listProjects.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// An item's author is a person's login or a session id, and no list holds both, so the author
// is typed. Enter takes it, and a login still half-typed searches nothing.
test("an author typed in the panel is taken on Enter, narrows the search, and comes off again", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  const view = renderPalette();

  try {
    await searchFor(user, "astrolabe");
    await openFilters(user);
    const author = screen.getByRole("searchbox", { name: "Author" });
    await user.click(author);
    await user.paste("ali");
    expect(search).toHaveBeenLastCalledWith("astrolabe", { keywords_only: true }, aborts);

    await user.paste("ce");
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(search).toHaveBeenLastCalledWith(
        "astrolabe",
        { author: "alice", keywords_only: true },
        aborts
      )
    );

    const chip = screen.getByRole("button", { name: "Remove Author alice filter" });
    expect(chip.textContent).toContain("Author alice");

    await user.click(chip);
    await waitFor(() =>
      expect(search).toHaveBeenLastCalledWith("astrolabe", { keywords_only: true }, aborts)
    );
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// Shutting the panel is leaving the box: the press that takes the box away is the reader
// walking off it, and what they typed is taken then rather than dropped.
test("an author left in the box is taken when the panel shuts", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  const view = renderPalette();

  try {
    const input = await searchFor(user, "astrolabe");
    await openFilters(user);
    await user.click(screen.getByRole("searchbox", { name: "Author" }));
    await user.paste("alice");

    await user.click(input);
    await waitFor(() =>
      expect(search).toHaveBeenLastCalledWith(
        "astrolabe",
        { author: "alice", keywords_only: true },
        aborts
      )
    );
    expect(screen.getByRole("button", { name: "Remove Author alice filter" })).not.toBeNull();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// A search made from an issue or a project page searches everything, as a search made from
// anywhere does. The page's project rides under `number_project`, which the route reads a bare
// issue number in and narrows nothing by, so the bar shows no chip claiming a narrowing.
test("the page's project is the search's context, not a chip narrowing it", async () => {
  const user = userOnFakeClock();
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  // The bar opened over an issue page may read that issue. LEGION-2 is the page's issue and the
  // document hit's own, and the chips of its control row are the whole of what this row reads.
  const getIssue = spyOn(api, "getIssue").mockResolvedValue({
    ...issueFixture,
    artifacts: [{ ...specArtifact, issue_key: "LEGION-2", project: "LEGION" }],
    key: "LEGION-2",
    number: 2,
    project: "LEGION",
    status: "done",
    title: "Navigation instruments",
  });
  const view = renderPalette({ entries: ["/issues/LEGION-2/spec"] });

  try {
    await searchFor(user, "astrolabe");

    expect(search).toHaveBeenLastCalledWith(
      "astrolabe",
      { keywords_only: true, number_project: "LEGION" },
      aborts
    );
    expect(screen.queryByRole("button", { name: /^Remove Project/ })).toBeNull();
    await openFilters(user);
    expect(screen.getByRole("button", { name: "Project" })).not.toBeNull();
  } finally {
    search.mockRestore();
    getIssue.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// The bar before Enter (design v3, state 1): with the box empty it lists Continue on a
// conversation of the last hour, then Recent, at most 5 items, then the page's actions, as main
// lists them; the chip sits in the box on its left, and Backspace in the empty box widens it; a
// key or an exact title ranks above the line on top, so Enter opens it (R2); and the foot holds
// the keys of the highlighted row alone.

/** A conversation History lists, its one run asked `minutesAgo`. */
function conversation(id: string, words: string, minutesAgo: number): GoingConversation {
  const at = new Date(Date.now() - minutesAgo * 60_000).toISOString();
  return {
    id,
    newest_at: at,
    runs: [{ ...runRecord(`${id}-run`, words), conversation: id, created_at: at, state: "done" }],
  };
}

/** The person's recent items, as the browser keeps them for alice. */
function keepRecents(count: number): { label: string; path: string }[] {
  const items = Array.from({ length: count }, (_, index) => ({
    label: `CORE-${index + 1} Item ${index + 1}`,
    path: `/issues/CORE-${index + 1}`,
  }));
  window.localStorage.setItem("dispatch.search.recents:alice", JSON.stringify(items));
  return items;
}

test("the empty box lists Continue on the last hour's conversation, then at most 5 Recent items, then the page's actions, and Enter continues", async () => {
  const user = userOnFakeClock();
  const unregister = registerClose([]);
  const items = keepRecents(7);
  const history = spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: [conversation("c-2", WORDS, 10)],
    total: 1,
  });
  // The conversation's run is read from its stream once it is open; it answers nothing here.
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation(
    () => new Promise<void>(() => {})
  );
  const view = renderPalette({ login: "alice" });

  try {
    const box = screen.getByRole("combobox", { name: "Search" });
    await waitFor(() => expect(optionIds()[0]).toBe("search-option-continue"));
    // Continue names the conversation by its title: its question with a capital.
    expect(
      screen.getByRole("option", { name: "Continue: Who looks after the comet" })
    ).toBeTruthy();
    const recent = within(screen.getByRole("group", { name: "Recent" })).getAllByRole("option");
    // Each recent issue reads as a best match does, as Raycast gives every list of items one row
    // look (review a12): its kind's icon, its key, its title - the issue's own once its summary
    // is read - and its status at the row's end.
    await waitFor(() =>
      expect(recent.map((row) => row.textContent)).toEqual(
        items.slice(0, 5).map((item) => {
          const key = item.label.split(" ")[0] ?? "";
          return `Issue${key}Instrument ${key}Todo`;
        })
      )
    );
    for (const row of recent) {
      expect(row.querySelector("svg")).not.toBeNull();
      expect(within(row).getByTestId("search-hit-status")).toHaveTextContent("Todo");
    }
    expect(
      screen.getByRole("option", { name: "Continue: Who looks after the comet" })
    ).toBeTruthy();
    expect(document.getElementById("search-option-continue")?.querySelector("svg")).not.toBeNull();
    // The page's actions follow Continue and Recent, as main lists them (design v3, state 1).
    expect(
      within(screen.getByRole("group", { name: "Actions" }))
        .getAllByRole("option")
        .map((option) => option.id)
    ).toEqual([CLOSE_ROW]);
    expect(optionIds()).toEqual([
      "search-option-continue",
      ...items.slice(0, 5).map((item) => `search-option-recent-${item.path}`),
      CLOSE_ROW,
    ]);
    expect(box).toHaveAttribute("aria-activedescendant", "search-option-continue");
    expect(screen.getByTestId("search-keyboard-hints")).toHaveTextContent("↵ Continue · Esc Close");
    // Nothing asks the person to type: the empty box says nothing under its rows.
    expect(screen.queryByText("Type at least 2 characters")).toBeNull();
    // The search has no header: its box is on top, History and the move are in its foot, and Esc
    // in the foot's hints is its one Close (design v3, state 1).
    expect(screen.queryByTestId("going-header")).toBeNull();
    const foot = screen.getByTestId("search-foot");
    expect(within(foot).getByRole("button", { name: "History" })).toBeInTheDocument();
    expect(within(foot).getByRole("button", { name: "Keep at the side" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Close" })).toBeNull();
    const dialog = screen.getByRole("dialog", { name: "Search" });
    expect(dialog.firstElementChild?.contains(box)).toBe(true);

    await user.click(box);
    await user.keyboard("{Enter}");
    expect(goingState().conversation).toEqual(["c-2-run"]);
    expect(screen.getByRole("dialog", { name: "Search" })).toHaveAttribute(
      "data-state",
      "conversation"
    );
  } finally {
    history.mockRestore();
    stream.mockRestore();
    unregister();
    window.localStorage.removeItem("dispatch.search.recents:alice");
    view.unmount();
    view.queryClient.clear();
  }
});

test("a conversation older than an hour gets no Continue, and Recent heads the list", async () => {
  keepRecents(2);
  const history = spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 50,
    offset: 0,
    rows: [conversation("c-1", WORDS, 61)],
    total: 1,
  });
  const view = renderPalette({ login: "alice" });

  try {
    await waitFor(() => expect(history).toHaveBeenCalled());
    await tick(0);
    expect(optionIds()).toEqual([
      "search-option-recent-/issues/CORE-1",
      "search-option-recent-/issues/CORE-2",
    ]);
    expect(screen.getByTestId("search-keyboard-hints")).toHaveTextContent("↵ Open · Esc Close");
  } finally {
    history.mockRestore();
    window.localStorage.removeItem("dispatch.search.recents:alice");
    view.unmount();
    view.queryClient.clear();
  }
});

test("the chip sits in the box on its left, and Backspace in the empty box widens it to the whole project", async () => {
  const user = userOnFakeClock();
  const view = renderPalette({ entries: ["/issues/LEGION-2"] });

  try {
    const box = screen.getByRole("combobox", { name: "Search" });
    const chip = screen.getByTestId("going-box-chip");
    expect(chip).toHaveTextContent("LEGION-2");
    expect(chip).toHaveAttribute(
      "title",
      "Acting on LEGION-2 · Backspace in the empty box: the whole project"
    );
    // The chip and the Filters button are inside the box's frame, on either side of the words,
    // and no line above the box says what the bar acts on.
    expect(box.parentElement?.firstElementChild).toBe(chip);
    expect(box.parentElement?.lastElementChild).toBe(
      screen.getByRole("button", { name: "Filters" })
    );
    expect(screen.queryByTestId("search-scope")).toBeNull();

    await user.click(box);
    await user.keyboard("{Backspace}");
    expect(screen.getByTestId("going-box-chip")).toHaveTextContent("the whole project");
    expect(screen.getByTestId("going-box-chip")).toHaveAttribute(
      "title",
      "Acting on the project LEGION"
    );
  } finally {
    view.unmount();
    view.queryClient.clear();
  }
});

test("an issue's key or its exact title ranks above the line on top, so Enter opens it", async () => {
  const user = userOnFakeClock();
  const hit = issueHit("LEGION-3");
  const other = issueHit("LEGION-4");
  const spies = [
    spyOn(api, "goingCapability").mockResolvedValue(CONFIGURED_CAPABILITY),
    spyOn(api, "createGoingRequest").mockImplementation(
      () => Promise.withResolvers<never>().promise
    ),
    spyOn(api, "search").mockResolvedValue(searchAnswer([hit, other])),
  ];
  const view = renderPalette();

  try {
    const input = await searchFor(user, "legion-3");
    await waitFor(() =>
      expect(optionIds()).toEqual([optionId(hit), "search-option-line", optionId(other)])
    );
    expect(input).toHaveAttribute("aria-activedescendant", optionId(hit));
    expect(screen.getByTestId("search-keyboard-hints")).toHaveTextContent("↵ Open · Esc Close");
    // The words name their hit, so no model is asked of them: the line reads Ask for good.
    expect(document.getElementById("search-option-line")).toHaveAttribute("data-line", "ask");
    expect(screen.queryByTestId("search-line-deciding")).toBeNull();

    // An exact title: the line on top shows before the hits, and the hit takes the head as it
    // arrives, since the person has not moved the highlight.
    await searchFor(user, "Instrument LEGION-3");
    await waitFor(() => expect(optionIds()[0]).toBe(optionId(hit)));
    expect(input).toHaveAttribute("aria-activedescendant", optionId(hit));
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/LEGION-3");
  } finally {
    for (const spy of spies) spy.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// The arrows keep the highlight where the reader put it for the words they arrowed on, and no
// longer: new words are a new list, so a best match that arrives after the line on top takes the
// head again, and Enter opens it.
test("after the arrows, new words let a best match take the head again as it arrives", async () => {
  const user = userOnFakeClock();
  const hit = issueHit("LEGION-3");
  const other = issueHit("LEGION-4");
  const spies = [
    spyOn(api, "goingCapability").mockResolvedValue(CONFIGURED_CAPABILITY),
    spyOn(api, "createGoingRequest").mockImplementation(
      () => Promise.withResolvers<never>().promise
    ),
    spyOn(api, "search").mockResolvedValue(searchAnswer([hit, other])),
  ];
  const view = renderPalette();

  try {
    const input = await searchFor(user, "instrument");
    await waitFor(() => expect(optionIds()).toContain(optionId(other)));
    await user.keyboard("{ArrowDown}");
    expect(input).toHaveAttribute("aria-activedescendant", optionIds()[1] ?? "");

    await searchFor(user, "Instrument LEGION-3");
    await waitFor(() => expect(optionIds()[0]).toBe(optionId(hit)));
    expect(input).toHaveAttribute("aria-activedescendant", optionId(hit));
    await user.keyboard("{Enter}");
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/LEGION-3");
  } finally {
    for (const spy of spies) spy.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// The bar after Enter (design v2, state 2; spec R1, R3, R10): the search parts leave it and it is
// the conversation, the question at its top with "Thinking…" and its seconds from the moment of
// Enter, and the box under it reading what Enter does now, with the chip, the level and Stop;
// Esc in the empty box stops the run; New brings the search back. Words sent while the run works
// wait under it (state 3's last bullet): × drops them, and the run's end sends them as the next
// turn, or as the revision of the plan it ended waiting on.
//
// Enter on the line Ask hands the run to the panel at the side (the last rows of this file), so
// the palette's own harness here, which keeps the dialog open where the shell would close it, reads
// the conversation the bar holds while it is over the page: the one a run started there begins,
// as Do's does, and the one Ctrl-K brings back from the panel.

const WORDS = "who looks after the comet";

function runRecord(id: string, words: string): GoingRun {
  return {
    context: null,
    continues: null,
    conversation: "c-1",
    created_at: new Date().toISOString(),
    finished_at: null,
    id,
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
    words,
  };
}

function waitingPlan(rows: GoingPlanAnswer["rows"] = []): GoingPlanAnswer {
  return {
    mode: "ask",
    plan: 1,
    rows,
    run: "r-1",
    state: "waiting",
    summary: "Watch the comet.",
    version: "v1",
  };
}

/** The api of a Dispatch whose bar answers: every start answered with the next of `starts`
 *  (`r-1`, `r-2`, … by default), each run's stream held open for the row to send frames down,
 *  Stop answered, and the search and the pause's short answer quiet. */
function answerTheBar(...starts: (GoingRun | Promise<GoingRun>)[]) {
  let made = 0;
  const start = spyOn(api, "createGoingRun").mockImplementation((input) => {
    made += 1;
    return Promise.resolve(starts[made - 1] ?? runRecord(`r-${made}`, input.words));
  });
  const frames = new Map<string, (name: string, data: unknown) => void>();
  const stop = spyOn(api, "stopGoingRun").mockImplementation(async (id) => ({
    ...runRecord(id, WORDS),
    state: "stopped",
  }));
  const spies: readonly { readonly mockRestore: () => void }[] = [
    start,
    stop,
    spyOn(api, "goingCapability").mockResolvedValue(CONFIGURED_CAPABILITY),
    spyOn(api, "search").mockResolvedValue(searchAnswer([])),
    spyOn(api, "createGoingRequest").mockImplementation(
      () => Promise.withResolvers<never>().promise
    ),
    spyOn(api, "streamGoingRunEvents").mockImplementation((id, onFrame) => {
      frames.set(id, onFrame);
      return Promise.withResolvers<void>().promise;
    }),
  ];
  return {
    restore: () => {
      for (const spy of spies) spy.mockRestore();
    },
    /** Hands run `id`'s stream a frame. */
    send: (id: string, name: string, data: unknown) => act(() => frames.get(id)?.(name, data)),
    start,
    stop,
  };
}

/** Types `words` in the bar's box and presses Enter on the line on top, which starts a run; the
 *  box is answered as it was before Enter. */
async function enterInTheBar(user: UserEvent, words: string): Promise<HTMLInputElement> {
  const box = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
  await user.click(box);
  await user.paste(words);
  await waitFor(() => expect(document.getElementById("search-option-line")).not.toBeNull());
  await user.keyboard("{Enter}");
  return box;
}

/** The bar's box under the conversation. */
function talkBox(): HTMLInputElement {
  return screen.getByRole<HTMLInputElement>("textbox", { name: "Ask the command bar" });
}

/** Opens the bar over the page, as Ctrl-K does, and renders it there. */
function openTheBar(options: RenderPaletteOptions = {}) {
  setPlace("over");
  return renderPalette({ entries: ["/projects/LEGION/issues"], ...options });
}

test("Enter turns the bar into the conversation at once: the search parts leave, the question heads it with Thinking and its seconds, and the box under it reads Ask a follow-up…", async () => {
  const user = userOnFakeClock();
  const { promise: created, resolve: create } = Promise.withResolvers<GoingRun>();
  const bar = answerTheBar(created);
  const view = openTheBar();
  try {
    expect(screen.getByRole("combobox", { name: "Search" })).toHaveAttribute(
      "placeholder",
      "Search or ask…"
    );
    const box = await enterInTheBar(user, WORDS);

    const dialog = screen.getByRole("dialog", { name: "Search" });
    expect(dialog).toHaveAttribute("data-state", "conversation");
    expect(dialog.className).toContain("h-[80vh]");
    expect(dialog.className).toContain("xl:max-w-[720px]");
    // The dialog is the box a table opened whole covers (design v2, state 5, "Show all").
    expect(dialog.className.split(" ")).toContain("relative");
    // The list, the Filters button, the message under the list and the list's keys are gone.
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(screen.queryByRole("button", { name: /^Filters/ })).toBeNull();
    expect(screen.queryByRole("dialog", { name: "Filters" })).toBeNull();
    expect(screen.queryByTestId("search-operators")).toBeNull();
    expect(screen.queryByText("Type at least 2 characters")).toBeNull();
    expect(screen.queryByTestId("search-keyboard-hints")).toBeNull();
    // Before Dispatch has answered the start, the question heads the conversation, with
    // Thinking from the moment of Enter, and titles it.
    expect(screen.getByTestId("going-run-words")).toHaveTextContent(WORDS);
    expect(screen.getByTestId("going-thinking")).toHaveTextContent("Thinking… 0 s");
    expect(screen.getByTestId("going-title")).toHaveTextContent("Who looks after the comet");
    // The box is the one it was, still focused, now under the conversation, with the chip on its
    // left and the level beside it.
    expect(talkBox()).toBe(box);
    expect(box).toHaveFocus();
    expect(box).toHaveAttribute("placeholder", "Ask a follow-up…");
    expect(box).toHaveValue("");
    const conversation = screen.getByTestId("going-conversation-view");
    expect(conversation.compareDocumentPosition(box) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(
      0
    );
    expect(screen.getByTestId("going-box-chip")).toHaveTextContent("the project LEGION");
    expect(screen.getByTestId("going-level")).toBeInTheDocument();

    await tick(1000);
    expect(screen.getByTestId("going-thinking")).toHaveTextContent("Thinking… 1 s");

    // Dispatch answers the start: the run's own turn goes on counting from Enter, and Stop sits
    // at the box's right while it works.
    await act(async () => create(runRecord("r-1", WORDS)));
    await waitFor(() => expect(screen.getByTestId("going-run")).toBeInTheDocument());
    expect(screen.queryByTestId("going-run-starting")).toBeNull();
    expect(screen.getByTestId("going-thinking")).toHaveTextContent("Thinking… 1 s");
    expect(screen.getByTestId("going-stop")).toBeInTheDocument();

    // The run's first step ends the Thinking line.
    bar.send("r-1", "call", { kind: "read", label: "Reading LEGION-3", step: 2, tool: "read" });
    expect(screen.queryByTestId("going-thinking")).toBeNull();
    expect(screen.getByTestId("going-step")).toHaveTextContent("Reading LEGION-3…");
  } finally {
    bar.restore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("Esc in the empty box while the run works stops it, and the bar stays open", async () => {
  const user = userOnFakeClock();
  const bar = answerTheBar();
  let closed = 0;
  const view = openTheBar({ onClose: () => (closed += 1) });
  try {
    await enterInTheBar(user, WORDS);
    await waitFor(() => expect(screen.getByTestId("going-run")).toBeInTheDocument());
    await user.keyboard("{Escape}");
    await waitFor(() => expect(bar.stop).toHaveBeenCalledTimes(1));
    expect(bar.stop.mock.calls[0]?.[0]).toBe("r-1");
    expect(closed).toBe(0);
  } finally {
    bar.restore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("New brings the search back, in the same box", async () => {
  const user = userOnFakeClock();
  const bar = answerTheBar();
  const view = openTheBar();
  try {
    const box = await enterInTheBar(user, WORDS);
    await waitFor(() => expect(screen.getByTestId("going-run")).toBeInTheDocument());
    await user.click(screen.getByTestId("going-new"));

    expect(screen.getByRole("dialog", { name: "Search" })).toHaveAttribute("data-state", "search");
    expect(screen.queryByTestId("going-run")).toBeNull();
    const search = screen.getByRole("combobox", { name: "Search" });
    expect(search).toBe(box);
    expect(search).toHaveAttribute("placeholder", "Search or ask…");
    // The cursor is back in the box, so typing lands in it (review n2).
    expect(search).toHaveFocus();
    expect(screen.getByRole("button", { name: "Filters" })).toBeInTheDocument();
  } finally {
    bar.restore();
    view.unmount();
    view.queryClient.clear();
  }
});

/** Starts a run from the bar and sends words while it works: they wait under it. */
async function queueWords(user: UserEvent, words: string): Promise<void> {
  await enterInTheBar(user, WORDS);
  await waitFor(() => expect(screen.getByTestId("going-run")).toBeInTheDocument());
  await user.type(talkBox(), `${words}{Enter}`);
}

test("words sent while the run works wait under it, sending when it ends, and × drops them", async () => {
  const user = userOnFakeClock();
  const bar = answerTheBar();
  const view = openTheBar();
  try {
    await queueWords(user, "and its moons");
    // No second conversation starts beside the run: the words wait at the foot.
    expect(bar.start).toHaveBeenCalledTimes(1);
    expect(screen.getAllByTestId("going-run")).toHaveLength(1);
    expect(talkBox()).toHaveValue("");
    const queued = screen.getByTestId("going-queued");
    expect(within(queued).getByTestId("going-queued-words")).toHaveTextContent("and its moons");
    expect(queued).toHaveTextContent("Sends when this run ends");

    await user.click(within(queued).getByRole("button", { name: "Drop these words" }));
    expect(screen.queryByTestId("going-queued")).toBeNull();
    bar.send("r-1", "reply", { labels: [], text: "Nobody yet.", tools: [] });
    bar.send("r-1", "end", { offers: [], reason: "", state: "done" });
    await tick(0);
    expect(bar.start).toHaveBeenCalledTimes(1);
  } finally {
    bar.restore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("words that wait go as the next turn of the conversation when the run ends with an answer", async () => {
  const user = userOnFakeClock();
  const bar = answerTheBar();
  const view = openTheBar();
  try {
    await queueWords(user, "and its moons");
    bar.send("r-1", "reply", { labels: [], text: "Nobody yet.", tools: [] });
    bar.send("r-1", "end", { offers: [], reason: "", state: "done" });

    await waitFor(() => expect(bar.start).toHaveBeenCalledTimes(2));
    expect(bar.start.mock.calls[1]?.[0]).toMatchObject({
      continues: "r-1",
      words: "and its moons",
    });
    expect(goingState().queued).toBeUndefined();
    await waitFor(() => expect(screen.getAllByTestId("going-run")).toHaveLength(2));
    expect(screen.getAllByTestId("going-run-words").map((words) => words.textContent)).toEqual([
      WORDS,
      "and its moons",
    ]);
  } finally {
    bar.restore();
    view.unmount();
    view.queryClient.clear();
  }
});

// An Enter that confirms an input method's conversion in the box under the conversation is the
// input method's, marked `isComposing` or, from Safari, sent unmarked with keyCode 229: it sends
// nothing and leaves the words in the box. A plain Enter after it sends them as the next turn.
test("an Enter that confirms an input method's conversion in the box under the conversation sends nothing, and a plain Enter sends the words", async () => {
  const user = userOnFakeClock();
  const bar = answerTheBar();
  const view = openTheBar();
  try {
    await enterInTheBar(user, WORDS);
    await waitFor(() => expect(screen.getByTestId("going-run")).toBeInTheDocument());
    bar.send("r-1", "reply", { labels: [], text: "Nobody yet.", tools: [] });
    bar.send("r-1", "end", { offers: [], reason: "", state: "done" });
    const box = talkBox();
    await user.type(box, "and its moons");
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(box, { isComposing: true, key: "Enter" });
    fireEvent.keyDown(box, { isComposing: false, key: "Enter", keyCode: 229 });
    expect(box).toHaveValue("and its moons");
    expect(bar.start).toHaveBeenCalledTimes(1);
    expect(goingState().starting).toBeUndefined();

    await user.keyboard("{Enter}");
    await waitFor(() => expect(bar.start).toHaveBeenCalledTimes(2));
    expect(bar.start.mock.calls[1]?.[0]).toMatchObject({
      continues: "r-1",
      words: "and its moons",
    });
    expect(box).toHaveValue("");
  } finally {
    bar.restore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("words that wait revise the plan the run ends waiting on", async () => {
  const user = userOnFakeClock();
  const bar = answerTheBar();
  const read = spyOn(api, "goingPlan").mockResolvedValue(waitingPlan());
  const revise = answerRoute<(run: string, body: unknown) => Promise<GoingPlanAnswer>>(
    "POST",
    "/api/v1/going/runs/*/plan/revise"
  );
  revise.mockImplementation(async () => ({ ...waitingPlan(), plan: 2 }));
  const view = openTheBar();
  try {
    await queueWords(user, "keep the dome whole");
    bar.send("r-1", "plan", { plan: 1, state: "waiting" });
    bar.send("r-1", "end", { offers: [], reason: "", state: "waiting" });

    await waitFor(() => expect(revise).toHaveBeenCalledTimes(1));
    expect(revise.mock.calls[0]?.[0]).toBe("r-1");
    expect(revise.mock.calls[0]?.[1]).toEqual({ plan: 1, words: "keep the dome whole" });
    expect(bar.start).toHaveBeenCalledTimes(1);
    expect(goingState().queued).toBeUndefined();
  } finally {
    read.mockRestore();
    bar.restore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("while a plan waits the box reads Say what to change…, the hint under it says Enter approves or sends the words, and ↑ in the empty box moves into the card", async () => {
  const user = userOnFakeClock();
  const bar = answerTheBar();
  const read = spyOn(api, "goingPlan").mockResolvedValue(
    waitingPlan([
      {
        arguments: { issue: "LEGION-3", priority: 2 },
        changed: false,
        claim: null,
        class: "field",
        fields: ["priority"],
        issue: "LEGION-3",
        item: "LEGION-3",
        needs: [],
        now: { priority: null },
        result: null,
        row: 0,
        state: "waiting",
        tool: "dispatch_issue_update",
        waits: [],
      },
    ])
  );
  const view = openTheBar();
  try {
    await enterInTheBar(user, WORDS);
    await waitFor(() => expect(screen.getByTestId("going-run")).toBeInTheDocument());
    bar.send("r-1", "plan", { plan: 1, state: "waiting" });
    bar.send("r-1", "end", { offers: [], reason: "", state: "waiting" });
    await waitFor(() => expect(screen.getByTestId("going-plan")).toBeInTheDocument());

    const box = talkBox();
    expect(box).toHaveAttribute("placeholder", "Say what to change…");
    expect(screen.getByTestId("going-box-hint")).toHaveTextContent("↵ Approve");
    await user.type(box, "keep it");
    expect(screen.getByTestId("going-box-hint")).toHaveTextContent(
      "↵ sends your words instead of approving"
    );
    await user.clear(box);
    await user.keyboard("{ArrowUp}");
    expect(document.activeElement?.getAttribute("data-testid")).toBe("going-plan-row");
  } finally {
    read.mockRestore();
    bar.restore();
    view.unmount();
    view.queryClient.clear();
  }
});

// A plan of a withdrawn run waits for nobody (`planWaits`), and its read can come before the
// run's record says the run is withdrawn. While the record still says it waits, Enter in the box
// neither approves the plan nor sends the words in the box: the box reads Dispatch withdrew this
// plan, the hint under it says Enter does nothing, and Enter keeps the words in the box.
test("while the run waits on a plan Dispatch withdrew, the box and the hint under it say so, and Enter keeps the words", async () => {
  const user = userOnFakeClock();
  const bar = answerTheBar();
  const read = spyOn(api, "goingPlan").mockResolvedValue({ ...waitingPlan(), withdrawn: true });
  const revise = answerRoute<(run: string, body: unknown) => Promise<GoingPlanAnswer>>(
    "POST",
    "/api/v1/going/runs/*/plan/revise"
  );
  const view = openTheBar();
  try {
    await enterInTheBar(user, WORDS);
    await waitFor(() => expect(screen.getByTestId("going-run")).toBeInTheDocument());
    bar.send("r-1", "plan", { plan: 1, state: "waiting" });
    bar.send("r-1", "end", { offers: [], reason: "", state: "waiting" });
    await waitFor(() => expect(screen.getByTestId("going-plan")).toBeInTheDocument());

    const box = talkBox();
    await waitFor(() => expect(box).toHaveAttribute("placeholder", "Dispatch withdrew this plan"));
    expect(screen.getByTestId("going-box-hint")).toHaveTextContent(
      "↵ does nothing: Dispatch withdrew this plan"
    );
    await user.type(box, "keep it{Enter}");
    expect(box).toHaveValue("keep it");
    expect(screen.getByTestId("going-box-hint")).toHaveTextContent(
      "↵ does nothing: Dispatch withdrew this plan"
    );
    expect(revise).not.toHaveBeenCalled();
    expect(bar.start).toHaveBeenCalledTimes(1);
  } finally {
    read.mockRestore();
    bar.restore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("an answer whose frame says it waits for the person makes the box read Answer…, and the answer end Waits for your answer", async () => {
  const user = userOnFakeClock();
  const bar = answerTheBar();
  const view = openTheBar();
  try {
    await enterInTheBar(user, "hand LEGION-3 to Legion");
    await waitFor(() => expect(screen.getByTestId("going-run")).toBeInTheDocument());
    bar.send("r-1", "reply", {
      labels: [],
      text: "Which changes hand it over: a label or a status",
      tools: [],
      waits_for_person: true,
    });
    bar.send("r-1", "end", { offers: [], reason: "", state: "done" });
    expect(talkBox()).toHaveAttribute("placeholder", "Answer…");
    expect(screen.getByTestId("going-run-state")).toHaveTextContent("Waits for your answer");
  } finally {
    bar.restore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("an answer that ends in a question mark but whose frame does not say it waits ends Done, and the box reads Ask a follow-up…", async () => {
  const user = userOnFakeClock();
  const bar = answerTheBar();
  const view = openTheBar();
  try {
    await enterInTheBar(user, "how many moons does Mars have");
    await waitFor(() => expect(screen.getByTestId("going-run")).toBeInTheDocument());
    bar.send("r-1", "reply", {
      labels: [],
      text: "Two. Did you mean its natural moons?",
      tools: [],
    });
    bar.send("r-1", "end", { offers: [], reason: "", state: "done" });
    expect(talkBox()).toHaveAttribute("placeholder", "Ask a follow-up…");
    expect(screen.getByTestId("going-run-state")).toHaveTextContent("Done");
  } finally {
    bar.restore();
    view.unmount();
    view.queryClient.clear();
  }
});

// Enter on the line Ask (and the card's Open in panel, which AnswerPanel.test.tsx holds) is one
// action that hands the run to the panel at the side: one run, of the bar's words, level and
// context; the bar closes; and the focus is in the panel's box once the dialog has given it back.
// The rows above keep the palette open as their own harness, since the shell, not the palette,
// closes it; these two draw the bar and the panel as `app.tsx` does, the bar open only while the
// conversation is over the page.

/** The shell's two parts of the conversation: the bar, open while the conversation is over the
 *  page, and the panel, drawn while it is at the side. */
function ConversationShell(): ReactNode {
  const over = useGoingValue((going) => going.place === "over");
  return (
    <>
      <SearchPalette onClose={() => setPlace("closed")} open={over} />
      <SidePanel />
    </>
  );
}

function renderShell(entries: string[]): { queryClient: QueryClient; unmount: () => void } {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={entries}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <ConversationShell />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, unmount: view.unmount };
}

test("Enter on the line Ask starts one run with the bar's words, level and context, closes the bar, and puts the focus in the panel's box", async () => {
  const user = userOnFakeClock();
  const bar = answerTheBar();
  act(() => {
    setLevel("deep");
    setPlace("over");
  });
  const view = renderShell(["/projects/LEGION/issues"]);
  try {
    await enterInTheBar(user, WORDS);
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull());

    expect(bar.start).toHaveBeenCalledTimes(1);
    expect(bar.start.mock.calls[0]?.[0]).toEqual({
      context: { block: "", issue: "", path: "/projects/LEGION/issues", project: "LEGION" },
      level: "deep",
      line: "ask",
      words: WORDS,
    });
    expect(goingState().place).toBe("side");
    const panel = screen.getByTestId("going-side-panel");
    await waitFor(() =>
      expect(within(panel).getByRole("textbox", { name: "Ask the command bar" })).toHaveFocus()
    );
    // The one run is the panel's conversation, the question at its head.
    await waitFor(() =>
      expect(within(panel).getByTestId("going-run-words")).toHaveTextContent(WORDS)
    );
    expect(within(panel).getAllByTestId("going-run")).toHaveLength(1);
  } finally {
    bar.restore();
    view.unmount();
    view.queryClient.clear();
  }
});

// A start Dispatch refuses shows its reason in the panel the bar handed over to, and no
// conversation: the refused words are no run, so there is nothing to show twice.
test("a start Dispatch refuses leaves the panel with its reason and no conversation", async () => {
  const user = userOnFakeClock();
  const bar = answerTheBar();
  bar.start.mockRejectedValue(new Error("Dispatch refused the run."));
  act(() => setPlace("over"));
  const view = renderShell(["/projects/LEGION/issues"]);
  try {
    await enterInTheBar(user, WORDS);
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull());
    await waitFor(() => expect(goingState().refusal).toBeDefined());
    expect(goingState().place).toBe("side");
    expect(goingState().conversation).toBeUndefined();
    expect(goingState().starting).toBeUndefined();
    expect(screen.queryByTestId("going-run")).toBeNull();
    expect(within(screen.getByTestId("going-side-panel")).getByRole("alert")).toBeInTheDocument();
    expect(bar.start).toHaveBeenCalledTimes(1);
  } finally {
    bar.restore();
    view.unmount();
    view.queryClient.clear();
  }
});
