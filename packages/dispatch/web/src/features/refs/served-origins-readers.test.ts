import { afterEach, beforeAll, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, renderHook, waitFor, within } from "@testing-library/react";
import type { Window as HappyDOMWindow } from "happy-dom";
import { createElement, type ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import { hereHref, setDispatchOrigins } from "../../api/origins";
import type { IssueReferenceSummary } from "../../api/types";
import { joinSplitLinks } from "../issue-page/joined-links";
import { RowText } from "../issue-page/RowText";
import { citedReferences as rowCitedReferences } from "../issue-page/row-references";
import { headRefs } from "./head-refs";
import { citedReferences, warmMarkdownRenderer } from "./markdown-engine";
import { collectReferenceAnchors } from "./RefLink";
import { useWholeRouteRead } from "./reference-target";
import { nodeRouteFromHref, referenceRouteFromHref } from "./routes";
import { readTextReferences } from "./text-references";
import { Unfurl } from "./Unfurl";
import { useTextReferences } from "./useTextReferences";

/**
 * Every reader of the page's origin list takes each address whoami names (INFRA-5491). A
 * deployment serves an address beside its canonical one and still reads links at an address it
 * served before, and `/auth/whoami` names all three, canonical first, which the page holds
 * through `setDispatchOrigins`. A person on the served address reads links people wrote at any
 * of them, so each reader below - every one `grep -rn 'dispatchOrigins\|hereHref' web/src` finds,
 * and the readers that take the list by default (`markdown-engine.ts`, `joined-links.ts`) - reads a
 * link at each as Dispatch's: a reference, or an address it opens on the page's own origin with its
 * path, query and fragment kept. A link at a host the deployment does not name stays the link its
 * author wrote. Each reader is driven as its own test drives it.
 */

/** The test DOM is happy-dom's window (`__tests__/register-dom.ts` registers it), which
 *  TypeScript knows only as the DOM's; each row puts the page on the served address. */
const testWindow = window as unknown as HappyDOMWindow;

const CANONICAL = "https://a.example.test";
const SERVED = "https://b.example.test";
const FORMER = "https://old.example.test";
const OTHER = "https://other.example.test";

/** What whoami names for a deployment that serves `SERVED` and once answered at `FORMER`. */
const NAMED = [CANONICAL, SERVED, FORMER];

// A node's words are drawn from the reading the engine makes of them (`displayTree`).
beforeAll(async () => {
  await warmMarkdownRenderer();
});

let pageBefore = "about:blank";

beforeEach(() => {
  pageBefore = window.location.href;
  testWindow.happyDOM.setURL(`${SERVED}/issues/CORE-2`);
  setDispatchOrigins(NAMED);
});

afterEach(() => {
  testWindow.happyDOM.setURL(pageBefore);
});

/** A project's board at `origin`: an address a reader opens, which cites no reference. */
function board(origin: string): string {
  return `${origin}/projects/CORE?view=board#top`;
}

/** A query client of the row's own, as a fresh page has. */
function newQueryClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

/** `RefLink.tsx`'s link pass over a rendered body that holds a link to CORE-1 and a link to the
 *  board, both at `origin`: the reference the first is drawn as, and where each opens. */
function linkPass(origin: string) {
  const root = document.createElement("div");
  root.innerHTML = `<p><a href="${origin}/issues/CORE-1">${origin}/issues/CORE-1</a> <a href="${board(origin)}">board</a></p>`;
  collectReferenceAnchors(root);
  const [issue, project] = root.querySelectorAll("a");
  return {
    board: project?.getAttribute("href"),
    opens: issue?.getAttribute("href"),
    reference: issue?.getAttribute("data-dispatch-ref"),
  };
}

/** CORE-1's summary read, which titles its card. */
const coreSummary: IssueReferenceSummary = {
  documents: [],
  key: "CORE-1",
  project: "CORE",
  status: "testing",
  title: "Design decision",
};

/** The card `Unfurl` shows under a body that is a link to CORE-1 at `origin`, by where it opens,
 *  and the issues it asked Dispatch about. A body that is no reference shows no card in the
 *  render that draws it, and asks nothing. */
async function unfurlCard(origin: string) {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(coreSummary);
  const view = render(
    createElement(
      QueryClientProvider,
      { client: newQueryClient() },
      createElement(Unfurl, { body: `${origin}/issues/CORE-1` })
    )
  );
  try {
    const cards = view.container.querySelector<HTMLElement>('section[aria-label="References"]');
    const card =
      cards === null ? null : await within(cards).findByRole("link", { name: /Design decision/ });
    return {
      asked: [...new Set(getIssueSummary.mock.calls.map(([key]) => key))],
      card: card?.getAttribute("href") ?? null,
    };
  } finally {
    view.unmount();
    getIssueSummary.mockRestore();
  }
}

/** A comment of CORE-1 as an address writes its id short, and the whole id it names. */
const SHORT_ID = "1a2b3c4d";
const WHOLE_ID = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d";

/** The route `reference-target.ts`'s `useWholeRouteRead` answers for CORE-1's comment written
 *  short, once the graph read answered the comment under a reference written as an address at
 *  `origin`: the read's reference is read against the origins as every address is. */
async function wholeRoute(origin: string) {
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({
      id: WHOLE_ID,
      kind: "comment",
      ref: `${origin}/issues/CORE-1/comments/${WHOLE_ID}`,
    }),
  });
  const queryClient = newQueryClient();
  const hook = renderHook(
    () => useWholeRouteRead({ id: SHORT_ID, key: "CORE-1", kind: "comment" }),
    {
      wrapper: ({ children }: { children: ReactNode }) =>
        createElement(QueryClientProvider, { client: queryClient }, children),
    }
  );
  try {
    await waitFor(() => expect(hook.result.current.resolution.isSuccess).toBe(true));
    return hook.result.current.route;
  } finally {
    hook.unmount();
    getReferences.mockRestore();
  }
}

/** The references `text-references.ts`'s `readTextReferences` reads in a text holding a link to
 *  CORE-1 at `origin`, against the origins it reads by default. */
function readInText(origin: string) {
  const { references } = readTextReferences(`See ${origin}/issues/CORE-1 here.`);
  return references.map(({ reference }) => reference);
}

/** The references `useTextReferences.ts` says a text holding a link to CORE-1 at `origin`
 *  cites, which the link pass over its rendered body links. */
function citedByTheHook(origin: string) {
  const queryClient = newQueryClient();
  const hook = renderHook(() => useTextReferences(`See ${origin}/issues/CORE-1 here.`), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client: queryClient }, children),
  });
  try {
    return [...hook.result.current.cited];
  } finally {
    hook.unmount();
  }
}

/** Where the router stands, drawn so a row reads it. */
function Place(): ReactNode {
  const at = useLocation();
  return createElement("output", { "data-place": "" }, `${at.pathname}${at.search}${at.hash}`);
}

/** A plain click on a link to the board at `origin` in a node's words, drawn from their reading
 *  at `/issues/CORE-1` as `RowText.tsx` draws them: whether the browser was left to follow it,
 *  and where the router stands after. The words are drawn while whoami has named no origin yet,
 *  so the link pass (`collectReferenceAnchors`) leaves the link at the address its author wrote,
 *  and whoami's origins are named before the click: the click alone decides where the link opens.
 *  The unit suite's document loads no page, so the browser's own follow is stopped once the click
 *  has been read. */
async function rowClick(origin: string) {
  const text = `See [the board](${board(origin)}).`;
  const tree = displayTree(text);
  if (tree === undefined) throw new Error("the engine refused the node's words");
  setDispatchOrigins([]);
  const view = render(
    createElement(
      QueryClientProvider,
      { client: newQueryClient() },
      createElement(
        MemoryRouter,
        { initialEntries: ["/issues/CORE-1"] },
        createElement(RowText, { markdown: text, tree }),
        createElement(Place)
      )
    )
  );
  let left = false;
  const stop = (event: Event) => {
    left = !event.defaultPrevented;
    event.preventDefault();
  };
  try {
    const link = await waitFor(() => {
      const found = view.container.querySelector("a");
      expect(found?.textContent).toBe("the board");
      return found;
    });
    expect(link?.getAttribute("href")).toBe(board(origin));
    setDispatchOrigins(NAMED);
    document.addEventListener("click", stop);
    act(() => {
      link?.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
    });
    return {
      at: view.container.querySelector("[data-place]")?.textContent,
      leftToTheBrowser: left,
    };
  } finally {
    document.removeEventListener("click", stop);
    view.unmount();
  }
}

/** The words of each anchor `joined-links.ts`'s `joinSplitLinks` leaves of a link to CORE-1 at
 *  `origin` that the painter drew as three pieces, the middle one bold. A link the reference pass
 *  reads keeps its three anchors, which that pass's portals draw into; any other is joined into
 *  one. */
function joinedPieces(origin: string): string[] {
  const address = `${origin}/issues/CORE-1`;
  const body = document.createElement("div");
  body.innerHTML =
    `<p><a href="${address}">the </a><strong><a href="${address}">issue</a></strong>` +
    `<a href="${address}"> page</a></p>`;
  joinSplitLinks(body);
  return [...body.querySelectorAll("a")].map((anchor) => anchor.textContent ?? "");
}

/** Each reader of the list: its name, how a link at one origin is read through it, what it reads
 *  a link at each address whoami names as, and what it reads one at `OTHER` as. */
const readers: readonly (readonly [string, (origin: string) => unknown, unknown, unknown])[] = [
  [
    "RefLink.tsx's link pass",
    linkPass,
    { board: board(SERVED), opens: "/issues/CORE-1", reference: "dispatch://CORE-1" },
    { board: board(OTHER), opens: `${OTHER}/issues/CORE-1`, reference: null },
  ],
  [
    "Unfurl.tsx's cards",
    unfurlCard,
    { asked: ["CORE-1"], card: "/issues/CORE-1" },
    { asked: [], card: null },
  ],
  [
    "reference-target.ts's useWholeRouteRead",
    wholeRoute,
    { id: WHOLE_ID, key: "CORE-1", kind: "comment" },
    undefined,
  ],
  [
    "routes.ts's referenceRouteFromHref",
    (origin: string) => referenceRouteFromHref(`${origin}/issues/CORE-1`),
    { key: "CORE-1", kind: "issue" },
    undefined,
  ],
  [
    "routes.ts's nodeRouteFromHref",
    (origin: string) => nodeRouteFromHref(`${origin}/issues/CORE-1/spec`),
    { key: "CORE-1", kind: "spec" },
    undefined,
  ],
  [
    "head-refs.ts's headRefs, which moves a cut inside a reference back to its start",
    (origin: string) => headRefs(`see ${origin}/issues/CORE-1 now`, 12, "…"),
    "see…",
    "see https:/…",
  ],
  [
    "markdown-engine.ts's citedReferences",
    (origin: string) => [...citedReferences(`See ${origin}/issues/CORE-1 here.`, undefined)],
    ["dispatch://CORE-1"],
    [],
  ],
  [
    "row-references.ts's citedReferences",
    (origin: string) => rowCitedReferences(`See ${origin}/issues/CORE-1 here.`),
    ["dispatch://CORE-1"],
    [],
  ],
  ["text-references.ts's readTextReferences", readInText, ["dispatch://CORE-1"], []],
  ["useTextReferences.ts's useTextReferences", citedByTheHook, ["dispatch://CORE-1"], []],
  [
    "origins.ts's hereHref",
    (origin: string) => hereHref(board(origin)),
    board(SERVED),
    board(OTHER),
  ],
  [
    "RowText.tsx's click in a node's words",
    rowClick,
    { at: "/projects/CORE?view=board#top", leftToTheBrowser: false },
    { at: "/issues/CORE-1", leftToTheBrowser: true },
  ],
  [
    "joined-links.ts's joinSplitLinks, which leaves a link the reference pass reads in its pieces",
    joinedPieces,
    ["the ", "issue", " page"],
    ["the issue page"],
  ],
];

test.each(readers)(
  "%s reads a link at each address whoami names as Dispatch's, and one at another host as the link it is",
  async (_reader, read, atDispatch, elsewhere) => {
    for (const origin of NAMED) {
      expect(await read(origin), `a link at ${origin}`).toEqual(atDispatch);
    }
    expect(await read(OTHER), `a link at ${OTHER}`).toEqual(elsewhere);
  }
);

// The issue page keeps each text's reading (`row-references.ts`); one made against the lists of
// an older whoami answer is read again once a newer answer names others.
test("a row's words read before whoami names a former address are read again once it does", () => {
  const text = `See ${FORMER}/issues/CORE-1 here.`;
  setDispatchOrigins([CANONICAL, SERVED]);
  expect(rowCitedReferences(text)).toEqual([]);

  setDispatchOrigins(NAMED);

  expect(rowCitedReferences(text)).toEqual(["dispatch://CORE-1"]);
});
