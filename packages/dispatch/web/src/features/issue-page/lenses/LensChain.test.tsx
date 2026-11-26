import { beforeAll, describe, expect, test } from "bun:test";
import type { LensChain as ChainRead, LensRead, NodeLenses } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { type ReactNode, useState } from "react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { answerRequests } from "../../../__tests__/network";
import { pageKindBullet, pageKindTag } from "../../../theme/classes";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import {
  PublishedReferenceNames,
  type PublishedReferences,
  ReferenceNamesProvider,
  usePublishReferenceNames,
} from "../../refs/reference-names";
import { node } from "../decisions-filter/decisions-fixtures";
import { buildIssueTree, type TreeNode } from "../issue-tree";
import { NodeRow } from "../NodeRow";
import { NoTooltips } from "../NoTooltips";
import {
  authorKind,
  CONTROL_LABELS,
  LENS_ANSWERS,
  LENS_WORDS,
  NODE_TYPE_NAMES,
} from "../page-words";
import { publishedNames } from "../projection-fixture";
import { issueDetails, statementRef } from "../spec-filter/spec-fixtures";
import { blockNodeId } from "../tree-drafts";
import { treeAnswer, treeNode, treeTuple } from "../tree-read/tree-fixtures";
import { LensLines } from "./LensLines";
import { kindWords, type LensFacts, type LensPlace } from "./lens-drafts";
import { lensChainQuery } from "./lens-reads";

/**
 * What a lens's row draws of its chain, the lines a lens, a risk, a constrained statement and a
 * constraint draw under their words, and the colour their rows are drawn in.
 *
 * The chain is read through the page's own client (`lens-reads.ts`, `api.getLenses`), answered at
 * `fetch` by the suite's network (`answerRequests`), so a request the row sends is one the test
 * counts. A failed read is not retried, as in the page's other rendering tests. A fact line's words
 * and a chain entry's are drawn through the plain-text projection, which answers in the render that
 * draws them once the engine has loaded, so every case runs with it loaded.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

const KEY = "CORE-1";

const TRADE_OFF = "dispatch://lens/0c6f1d2e-1111-4a6b-9c1d-5e2f3a4b5c61";
const PREMORTEM = "dispatch://lens/0c6f1d2e-2222-4a6b-9c1d-5e2f3a4b5c62";
const FOLLOW_UP = "dispatch://lens/0c6f1d2e-3333-4a6b-9c1d-5e2f3a4b5c63";
const UNNAMED = "dispatch://lens/0c6f1d2e-4444-4a6b-9c1d-5e2f3a4b5c64";
const FURTHER = "dispatch://lens/0c6f1d2e-5555-4a6b-9c1d-5e2f3a4b5c65";
const ELSEWHERE = "dispatch://lens/0c6f1d2e-6666-4a6b-9c1d-5e2f3a4b5c66";
const BEYOND = "dispatch://lens/0c6f1d2e-7777-4a6b-9c1d-5e2f3a4b5c67";

/** The statement both lenses read: its words, longer than one line, which the chain names it by,
 *  whole. */
const STATEMENT = statementRef("r1");
const STATEMENT_WORDS =
  "Store the notices for a day in the store the listener writes, and drop each one that is older than a day when the next notice of its issue arrives";

function facts(partial: Partial<LensFacts>): LensFacts {
  return { constrainedBy: [], preventedBy: [], triggered: [], triggeredBy: [], ...partial };
}

/** Every lens of the tree the pre-mortem stands in, by address. */
const PLACES: ReadonlyMap<string, LensPlace> = new Map([
  [TRADE_OFF, { id: "lens/1111", kindWords: "trade-off analysis", onWords: STATEMENT_WORDS }],
  [PREMORTEM, { id: "lens/2222", kindWords: "pre-mortem", onWords: STATEMENT_WORDS }],
]);

/** The pre-mortem on the statement, triggered by the trade-off analysis. */
function premortem(lenses: ReadonlyMap<string, LensPlace> = PLACES): TreeNode {
  return node({
    id: "lens/2222",
    lens: facts({
      kind: "pre-mortem",
      lenses,
      triggeredBy: [{ address: TRADE_OFF, id: "lens/1111", words: "Trade-off analysis" }],
    }),
    reference: PREMORTEM,
    text: "Ship it behind a flag",
    type: "lens",
  });
}

/** One lens as the lens read answers it, of the kind whose words are `words`. */
function lensRead(address: string, words: string | null): LensRead {
  return {
    address,
    author: { id: "alice", kind: "person" },
    created_at: "2026-11-16T09:00:00Z",
    findings: [],
    kind: words === null ? null : { id: `kind-${address.slice(-2)}`, words },
    on: STATEMENT,
    risks: [],
    state: "live",
    triggered: [],
    triggered_by: [],
    verdict: null,
    visibility: "project",
  };
}

function chain(lenses: readonly LensRead[], stoppedAt: string | null = null): ChainRead {
  return { lenses: [...lenses], stopped_at: stoppedAt };
}

/** The lens read of the pre-mortem, holding `chains`. */
function lensesOf(chains: readonly ChainRead[]): NodeLenses {
  return {
    chains: [...chains],
    lens: lensRead(PREMORTEM, "pre-mortem"),
    lenses: [],
    on: { address: PREMORTEM, kind: "lens", state: "live", visibility: "project" },
  };
}

/** Answers every lens read with what `answer` gives, and keeps the `on` of each request in the
 *  order they came. */
function answerLensReads(answer: () => Response | Promise<Response>): string[] {
  const asked: string[] = [];
  answerRequests(
    http.get("/api/v1/lenses", ({ request }) => {
      asked.push(new URL(request.url).searchParams.get("on") ?? "");
      return answer();
    })
  );
  return asked;
}

/** Who made a node the model holds no author for, as the tree names one. */
const NOBODY_RECORDED = {
  kind: authorKind(undefined),
  name: CONTROL_LABELS.unknown_author,
  reader: false,
};

/** One row the way the tree draws it: unfocused until a focus lands inside it, unless it starts
 *  focused. */
function LensRow({ drawn, focused: start = false }: { drawn: TreeNode; focused?: boolean }) {
  const [focused, setFocused] = useState(start);
  return (
    <NodeRow
      author={NOBODY_RECORDED}
      context={false}
      focused={focused}
      isClosed={false}
      level={1}
      node={drawn}
      onFocus={() => setFocused(true)}
      onToggle={() => {}}
      ownAnswer={false}
      rowKey="row"
      tabStop={false}
    />
  );
}

/** A client whose failed read is not retried, as the page's other rendering tests use. */
function testClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

/** `page` on this issue's route, so a chip's link takes the route's key: at Conversation's
 *  address unless the case names another (`path`). */
function renderOnIssue(
  page: ReactNode,
  queryClient = testClient(),
  path = `/issues/${KEY}`
): RenderResult {
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <NoTooltips>
          <Routes>
            <Route element={page} path="/issues/:key/*" />
          </Routes>
        </NoTooltips>
      </MemoryRouter>
    </QueryClientProvider>
  );
}

/** Publishes `published` to the names store while it is drawn, and hands it to what it draws in
 *  the render that draws it, as the page publishes its one batched read and hands it its rows
 *  (`NodeRow.drawn.test.tsx`'s `Named`). Drawn inside the names store (`ReferenceNamesProvider`). */
function Named({
  children,
  published,
}: {
  children: ReactNode;
  published: PublishedReferences;
}): ReactNode {
  usePublishReferenceNames(published);
  return <PublishedReferenceNames published={published}>{children}</PublishedReferenceNames>;
}

function showChain(view: RenderResult): HTMLButtonElement | null {
  return view.container.querySelector<HTMLButtonElement>("[data-lens-show-chain]");
}

/** The control, once the row draws it. */
async function drawnControl(view: RenderResult): Promise<HTMLButtonElement> {
  return waitFor(() => {
    const control = showChain(view);
    if (control === null) throw new Error("the lens draws no control for its chain");
    return control;
  });
}

/** What the chain's read says, where it says something. */
function chainStatus(view: RenderResult): string | null {
  return view.container.querySelector('[data-lens-chain] [role="status"]')?.textContent ?? null;
}

function chainAddresses(view: RenderResult): (string | null)[] {
  return [...view.container.querySelectorAll("[data-lens-chain-entry]")].map((entry) =>
    entry.getAttribute("data-lens-address")
  );
}

/** What a reader reads of each entry of the chain, and whose words each kind's words are. */
function chainEntries(view: RenderResult) {
  return [...view.container.querySelectorAll("[data-lens-chain-entry]")].map((entry) => {
    const kind = entry.querySelector("[data-lens-chain-kind]");
    return {
      address: entry.getAttribute("data-lens-address"),
      kind: kind?.textContent ?? null,
      line: entry.querySelector("[data-lens-chain-line]")?.textContent ?? null,
      marked: kind?.hasAttribute("data-author-text") ? "author" : "page",
    };
  });
}

/** Lets whatever a press or a focus would send go out and be answered. */
async function letRequestsSettle(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, 50);
  await act(() => promise);
}

// The lens read is sent at a press of the control alone, never at the focus, which `j`
// gives every row, nor at drawing the row, which draws the control with or without the focus;
// once in a load, so a second press, and a press on the row focused again, draw the held answer
// and send nothing.
test("the chain is read at a press and not at the focus", async () => {
  const user = userEvent.setup();
  const out = Promise.withResolvers<void>();
  const asked = answerLensReads(async () => {
    await out.promise;
    return Response.json(lensesOf([chain([lensRead(TRADE_OFF, null), lensRead(PREMORTEM, null)])]));
  });
  const queryClient = testClient();
  const first = renderOnIssue(<LensRow drawn={premortem()} />, queryClient);
  try {
    // The row draws the control before it has the focus.
    const control = await drawnControl(first);
    expect(control.textContent).toBe(LENS_WORDS.show_chain);
    expect(control.hasAttribute("data-page-chrome")).toBe(true);
    expect(control.tabIndex, "the control is in the tab order").toBe(-1);
    await letRequestsSettle();
    expect(asked, "drawing the row sent the lens read").toEqual([]);
    act(() => first.container.querySelector<HTMLElement>("[data-node-row]")?.focus());
    await letRequestsSettle();
    expect(asked, "the focus sent the lens read").toEqual([]);
    expect(showChain(first), "the focus changed the control").toBe(control);
    expect(chainStatus(first)).toBeNull();

    await user.click(control);
    expect(chainStatus(first)).toBe(LENS_ANSWERS.loading_chain);
    await waitFor(() => expect(asked).toEqual([PREMORTEM]));
    out.resolve();
    await waitFor(() => expect(chainAddresses(first)).toEqual([TRADE_OFF, PREMORTEM]));
    expect(chainStatus(first)).toBeNull();
    expect(showChain(first), "the control went with the answer").toBe(control);

    await user.click(control);
    await letRequestsSettle();
    expect(asked, "a second press sent the lens read again").toEqual([PREMORTEM]);
    expect(chainAddresses(first)).toEqual([TRADE_OFF, PREMORTEM]);
  } finally {
    out.resolve();
    first.unmount();
  }

  // The row loses the focus and has it again, in the same load.
  const again = renderOnIssue(<LensRow drawn={premortem()} focused />, queryClient);
  try {
    const control = await drawnControl(again);
    expect(chainAddresses(again), "the held answer is drawn before the press").toEqual([]);
    await user.click(control);
    expect(chainStatus(again)).toBeNull();
    expect(chainAddresses(again)).toEqual([TRADE_OFF, PREMORTEM]);
    await letRequestsSettle();
    expect(asked, "a press on the row focused again sent the lens read").toEqual([PREMORTEM]);
  } finally {
    again.unmount();
  }

  // A lens with no trigger chip has no chain to show.
  const alone = node({
    id: "lens/3333",
    lens: facts({ kind: "second opinion", lenses: PLACES }),
    reference: FOLLOW_UP,
    type: "lens",
  });
  const unchained = renderOnIssue(<LensRow drawn={alone} focused />, queryClient);
  try {
    expect(unchained.container.querySelector("[data-node-row]")).not.toBeNull();
    expect(showChain(unchained)).toBeNull();
  } finally {
    unchained.unmount();
  }
});

// The answer holds the chain this lens stands in and the chains of the lenses invoked on
// it, which are its rows; only the first is drawn, in the answer's order, each lens by its
// kind's words as held - the tree's, else the read's, else the page's word - and by the whole
// words of the row it stands under where the tree drew it.
test("only the chain that holds the lens is drawn", async () => {
  const user = userEvent.setup();
  const asked = answerLensReads(() =>
    Response.json(
      lensesOf([
        chain([lensRead(ELSEWHERE, "second opinion"), lensRead(BEYOND, "second opinion")]),
        chain([
          lensRead(TRADE_OFF, "trade-off analysis, as read"),
          lensRead(PREMORTEM, "pre-mortem"),
          lensRead(FOLLOW_UP, "second opinion"),
          lensRead(UNNAMED, null),
        ]),
      ])
    )
  );
  const view = renderOnIssue(<LensRow drawn={premortem()} focused />);
  try {
    await user.click(await drawnControl(view));
    await waitFor(() => expect(chainAddresses(view)).toHaveLength(4));
    expect(asked).toEqual([PREMORTEM]);
    expect(chainEntries(view)).toEqual([
      { address: TRADE_OFF, kind: "trade-off analysis", line: STATEMENT_WORDS, marked: "author" },
      { address: PREMORTEM, kind: "pre-mortem", line: STATEMENT_WORDS, marked: "author" },
      { address: FOLLOW_UP, kind: "second opinion", line: null, marked: "author" },
      { address: UNNAMED, kind: NODE_TYPE_NAMES.lens, line: null, marked: "page" },
    ]);
    expect(
      view.container.querySelector(`[data-lens-address="${ELSEWHERE}"]`),
      "the chain of a lens invoked on this one is drawn"
    ).toBeNull();
    expect(view.container.querySelector("[data-lens-chain-more]")).toBeNull();
  } finally {
    view.unmount();
  }
});

// R22 over the one reference framework: a chain entry names the row its lens stands under by that
// row's whole words, read through the plain-text projection as every one-line view reads them,
// so a `dispatch://` address in them reads as the name the page published for its target, never
// as the address, and the line makes no read of its own for a target the page named.
test("a chain entry reads a dispatch:// reference the page published by its target's name", async () => {
  const user = userEvent.setup();
  const address = "dispatch://CORE-1/spec#b-r1";
  answerLensReads(() =>
    Response.json(lensesOf([chain([lensRead(TRADE_OFF, null), lensRead(PREMORTEM, null)])]))
  );
  const words = `Store the notices as ${address} says`;
  const places: ReadonlyMap<string, LensPlace> = new Map([
    [TRADE_OFF, { id: "lens/1111", kindWords: "trade-off analysis", onWords: words }],
    [PREMORTEM, { id: "lens/2222", kindWords: "pre-mortem", onWords: words }],
  ]);
  const queryClient = testClient();
  const view = renderOnIssue(
    <ReferenceNamesProvider>
      <Named published={publishedNames(new Map([[address, "Draw the rows"]]))}>
        <LensRow drawn={premortem(places)} focused />
      </Named>
    </ReferenceNamesProvider>,
    queryClient
  );
  try {
    await user.click(await drawnControl(view));
    await waitFor(() =>
      expect(chainEntries(view).map(({ line }) => line)).toEqual([
        "Store the notices as Draw the rows says",
        "Store the notices as Draw the rows says",
      ])
    );
    expect(
      queryClient
        .getQueryCache()
        .getAll()
        .map((query) => query.queryKey)
        .filter((key) => JSON.stringify(key).includes("spec#b-r1")),
      "the line read a target the page named"
    ).toEqual([]);
  } finally {
    view.unmount();
  }
});

// A chain the read cut at its depth names the lens it stopped at, last, by a chip of its
// kind's words, whole as the tag is, linked to its row where the tree drew one.
test("a stopped_at ends the chain with More lenses past this one", async () => {
  const user = userEvent.setup();
  answerLensReads(() =>
    Response.json(
      lensesOf([chain([lensRead(TRADE_OFF, null), lensRead(PREMORTEM, null)], FURTHER)])
    )
  );
  const drawnEnd = async (lenses: ReadonlyMap<string, LensPlace>) => {
    const view = renderOnIssue(<LensRow drawn={premortem(lenses)} focused />);
    try {
      await user.click(await drawnControl(view));
      await waitFor(() => expect(chainAddresses(view)).toEqual([TRADE_OFF, PREMORTEM]));
      const last = view.container.querySelector("[data-lens-chain]")?.lastElementChild;
      const label = last?.firstElementChild;
      const name = last?.querySelector("[data-node-chip] [data-chip-name]");
      return {
        href: last?.querySelector("[data-node-chip] a")?.getAttribute("href") ?? null,
        label: label?.textContent,
        labelMarked: label?.hasAttribute("data-page-chrome"),
        more: last?.hasAttribute("data-lens-chain-more"),
        words: name?.textContent,
        wordsMarked: name?.hasAttribute("data-author-text") ? "author" : "page",
      };
    } finally {
      view.unmount();
    }
  };

  const furtherKind = "risk review of the notice store's retention";
  const further = new Map(PLACES).set(FURTHER, {
    id: "lens/5555",
    kindWords: furtherKind,
    onWords: STATEMENT_WORDS,
  });
  expect(await drawnEnd(further)).toEqual({
    href: "/issues/CORE-1?node=lens%2F5555",
    label: LENS_WORDS.more_lenses,
    labelMarked: true,
    more: true,
    words: kindWords(furtherKind),
    wordsMarked: "author",
  });
  // A lens the tree drew no row for is named by the page's word for a lens, marked as the
  // page's, with no link.
  expect(await drawnEnd(PLACES)).toEqual({
    href: null,
    label: LENS_WORDS.more_lenses,
    labelMarked: true,
    more: true,
    words: kindWords(NODE_TYPE_NAMES.lens),
    wordsMarked: "page",
  });
});

// A read that fails says so in the page's words, and a lens that is no longer there says
// what an invocation's 404 says. Every attempt is answered.
test("a failed or 404 chain read says so", async () => {
  const user = userEvent.setup();
  const said = async (status: number, error: string) => {
    const asked = answerLensReads(() => Response.json({ code: "X", error }, { status }));
    const view = renderOnIssue(<LensRow drawn={premortem()} focused />);
    try {
      await user.click(await drawnControl(view));
      await waitFor(() => expect(chainStatus(view)).not.toBe(LENS_ANSWERS.loading_chain));
      expect(asked.length).toBeGreaterThan(0);
      expect(chainAddresses(view)).toEqual([]);
      expect(showChain(view), "the control went with the failure").not.toBeNull();
      return {
        marked: view.container
          .querySelector('[data-lens-chain] [role="status"]')
          ?.hasAttribute("data-page-chrome"),
        status: chainStatus(view),
      };
    } finally {
      view.unmount();
    }
  };
  expect(await said(500, "the read failed")).toEqual({
    marked: true,
    status: LENS_ANSWERS.chain_failed,
  });
  expect(await said(404, "no lens at that address")).toEqual({
    marked: true,
    status: LENS_ANSWERS.gone,
  });
});

// A chain once drawn stays drawn when its read is sent again and fails, as it is when something
// refreshes every read the page holds (`write-store.ts` does for a landing no surface waits on):
// a failed refresh over a held answer blanks nothing, and the failure is said only where no
// answer is held.
test("a chain drawn stays drawn when a read again fails", async () => {
  const user = userEvent.setup();
  let failing = false;
  const asked = answerLensReads(() =>
    failing
      ? Response.json({ code: "UNAVAILABLE", error: "the store is down" }, { status: 500 })
      : Response.json(lensesOf([chain([lensRead(TRADE_OFF, null), lensRead(PREMORTEM, null)])]))
  );
  const queryClient = testClient();
  const view = renderOnIssue(<LensRow drawn={premortem()} focused />, queryClient);
  try {
    await user.click(await drawnControl(view));
    await waitFor(() => expect(chainAddresses(view)).toEqual([TRADE_OFF, PREMORTEM]));

    failing = true;
    await act(() => queryClient.invalidateQueries());
    expect(asked, "the chain was not read again").toEqual([PREMORTEM, PREMORTEM]);
    expect(
      queryClient.getQueryState(lensChainQuery(PREMORTEM, true).queryKey)?.status,
      "the read sent again did not fail"
    ).toBe("error");
    expect(chainAddresses(view), "the failure took the chain away").toEqual([TRADE_OFF, PREMORTEM]);
    expect(chainStatus(view)).toBeNull();
    expect(view.container.textContent).not.toContain(LENS_ANSWERS.chain_failed);
  } finally {
    view.unmount();
  }
});

/**
 * What a reader reads of each line `drawn` draws at `path`, this issue's Conversation unless the
 * case names another of its views: what the line is, its label in the page's words, the author's
 * words it holds, and each chip's words and where its link goes.
 */
function linesDrawn(drawn: TreeNode, path = `/issues/${KEY}`) {
  const view = render(
    <QueryClientProvider client={testClient()}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route element={<LensLines node={drawn} tabStops={false} />} path="/issues/:key/*" />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    return [...view.container.querySelectorAll("[data-lens-line]")].map((line) => {
      const parts = [...line.children];
      return {
        chips: [...line.querySelectorAll("[data-node-chip]")].map((chip) => ({
          href: chip.querySelector("a")?.getAttribute("href") ?? null,
          words: chip.querySelector("[data-chip-name][data-author-text]")?.textContent ?? null,
        })),
        label: parts.find((part) => part.hasAttribute("data-page-chrome"))?.textContent ?? null,
        line: line.getAttribute("data-lens-line"),
        words: parts
          .filter((part) => part.hasAttribute("data-author-text"))
          .map((part) => part.textContent),
      };
    });
  } finally {
    view.unmount();
  }
}

const CHECK = "dispatch://CORE-1/verification#b-c1";
const ISSUE_CONSTRAINT = "dispatch://constraint/9a1b2c3d-0000-4e5f-8a9b-0c1d2e3f4a50";
const CATALOGUE_CONSTRAINT = "dispatch://lens-kinds/pre-mortem/sources/owasp-asvs";

function risk(partial: Partial<LensFacts>): TreeNode {
  return node({
    id: "lens/2222#b-risk-1",
    lens: facts({ earlyWarning: "Notices pile up past a day", ...partial }),
    text: "The store keeps notices for a week",
    type: "risk",
  });
}

/** The address as it stands, its path and its query, drawn where a case reads where a press
 *  went. */
function AddressDrawn(): ReactNode {
  const { pathname, search } = useLocation();
  return <span data-address={`${pathname}${search}`} />;
}

describe("the lines under a row", () => {
  // Each chip names its lens, linked to its row where the tree drew one.
  test("a lens draws the lenses that triggered it and the lenses it triggered", () => {
    const drawn = node({
      id: "lens/2222",
      lens: facts({
        kind: "pre-mortem",
        triggered: [{ address: FOLLOW_UP, words: "Second opinion" }],
        triggeredBy: [{ address: TRADE_OFF, id: "lens/1111", words: "Trade-off analysis" }],
      }),
      reference: PREMORTEM,
      type: "lens",
    });
    expect(linesDrawn(drawn)).toEqual([
      {
        chips: [{ href: "/issues/CORE-1?node=lens%2F1111", words: "Trade-off analysis" }],
        label: LENS_WORDS.triggered_by,
        line: "triggered_by",
        words: [],
      },
      {
        chips: [{ href: null, words: "Second opinion" }],
        label: LENS_WORDS.triggered,
        line: "triggered",
        words: [],
      },
    ]);
  });

  // A risk's early warning, then the checks that prevent it, or the one line saying none
  // does, and never both.
  test("a risk names the checks that prevent it, or says that none does", () => {
    const prevented = risk({
      preventedBy: [
        { address: CHECK, id: "verification-md#b-c1", words: "Notices older than a day…" },
      ],
    });
    const warning = {
      chips: [],
      label: LENS_WORDS.early_warning,
      line: "early_warning",
      words: ["Notices pile up past a day"],
    };
    expect(linesDrawn(prevented)).toEqual([
      warning,
      {
        chips: [
          {
            href: "/issues/CORE-1?node=verification-md%23b-c1",
            words: "Notices older than a day…",
          },
        ],
        label: LENS_WORDS.prevented_by,
        line: "prevented_by",
        words: [],
      },
    ]);
    expect(linesDrawn(risk({}))).toEqual([
      warning,
      {
        chips: [],
        label: LENS_WORDS.prevented_by_nothing,
        line: "prevented_by_nothing",
        words: [],
      },
    ]);
  });

  // A press on a chip goes to its target's address and focuses the target's row where
  // the tree draws it, at every press: the address puts the focus on its node once, and no more
  // after the reader's first act, so a second press on the same chip, after the reader went back
  // to the risk, focuses the check's row all the same. The chip names the check by its id in the
  // tree read, and the row carries the id the tree draws a typed block by. In Conversation and in
  // Graph alike, the link is the open view's own address of the row (R7).
  test("a chip focuses its row at every press", async () => {
    const user = userEvent.setup();
    const checkId = blockNodeId("verification-md", "c1");
    const prevented = risk({
      preventedBy: [{ address: CHECK, id: "c1", words: "Notices older than a day…" }],
    });
    const check = node({
      id: checkId,
      readId: "c1",
      reference: CHECK,
      text: "Notices older than a day are gone",
      type: "check",
    });
    for (const path of [`/issues/${KEY}`, `/issues/${KEY}/graph`]) {
      const view = renderOnIssue(
        <>
          <div aria-label={CONTROL_LABELS.tree} role="tree">
            <LensRow drawn={prevented} />
            <LensRow drawn={check} />
          </div>
          <AddressDrawn />
        </>,
        testClient(),
        path
      );
      const rowOf = (id: string) =>
        view.container.querySelector<HTMLElement>(
          `[data-node-row][data-node-id="${CSS.escape(id)}"]`
        );
      const chip = () => {
        const link = rowOf(prevented.id)?.querySelector<HTMLElement>("[data-node-chip] a");
        if (link === null || link === undefined) throw new Error("the risk draws no chip's link");
        return link;
      };
      const address = () =>
        view.container.querySelector("[data-address]")?.getAttribute("data-address");
      try {
        await user.click(chip());
        expect(document.activeElement, `the press at ${path} left the focus`).toBe(rowOf(checkId));
        expect(address()).toBe(`${path}?node=c1`);

        // The reader goes back to the risk, and `f` steps to its chip.
        act(() => rowOf(prevented.id)?.focus());
        act(() => chip().focus());
        await user.keyboard("{Enter}");
        expect(document.activeElement, `Enter on the chip at ${path} left the focus`).toBe(
          rowOf(checkId)
        );
        expect(address()).toBe(`${path}?node=c1`);
      } finally {
        view.unmount();
      }
    }
  });

  // A statement names each constraint it is held to; one of the issue's is a row and its
  // chip a link, and one of the catalogue has no row, so its chip is words with no link. Where
  // the open view draws the issue's constraint's row, the chip links it there; where only Graph
  // draws it (`graphOnly`), as Conversation's answer and the Spec view's give it, the chip links
  // Graph's row of it from every view (R7).
  test("a statement names its constraints, the issue's linked and the catalogue's as words", () => {
    const statement = (graphOnly: boolean) =>
      node({
        id: "spec#b-r5",
        lens: facts({
          constrainedBy: [
            {
              address: ISSUE_CONSTRAINT,
              id: "constraint/9a1b",
              words: "Retention under 24 hours",
              ...(graphOnly ? { graphOnly: true } : {}),
            },
            { address: CATALOGUE_CONSTRAINT, words: "OWASP ASVS 4.0 V8.3" },
          ],
        }),
        reference: "dispatch://CORE-1/spec#b-r5",
        text: "Notices expire",
        type: "requirement",
      });
    const line = (href: string) => [
      {
        chips: [
          { href, words: "Retention under 24 hours" },
          { href: null, words: "OWASP ASVS 4.0 V8.3" },
        ],
        label: LENS_WORDS.constrained_by,
        line: "constrained_by",
        words: [],
      },
    ];
    const inConversation = "/issues/CORE-1?node=constraint%2F9a1b";
    const inGraph = "/issues/CORE-1/graph?node=constraint%2F9a1b";
    expect(linesDrawn(statement(false))).toEqual(line(inConversation));
    for (const path of [`/issues/${KEY}`, `/issues/${KEY}/spec`, `/issues/${KEY}/graph`]) {
      expect(linesDrawn(statement(true), path), `at ${path}`).toEqual(line(inGraph));
    }
    expect(linesDrawn(statement(false), `/issues/${KEY}/graph`)).toEqual(line(inGraph));
  });

  // In Conversation a press on the chip of a constraint only Graph draws opens Graph at its row,
  // and focuses no row Conversation draws there: Graph's address puts the focus on the row as
  // Graph opens.
  test("a constraint's chip in Conversation opens Graph at its row", async () => {
    const user = userEvent.setup();
    const constraintId = "constraint/9a1b";
    const statement = node({
      id: "spec#b-r5",
      lens: facts({
        constrainedBy: [
          {
            address: ISSUE_CONSTRAINT,
            graphOnly: true,
            id: constraintId,
            words: "Retention under 24 hours",
          },
        ],
      }),
      reference: "dispatch://CORE-1/spec#b-r5",
      text: "Notices expire",
      type: "requirement",
    });
    const constraint = node({
      id: constraintId,
      readId: constraintId,
      reference: ISSUE_CONSTRAINT,
      text: "Retention under 24 hours",
      type: "constraint",
    });
    const view = renderOnIssue(
      <>
        <div aria-label={CONTROL_LABELS.tree} role="tree">
          <LensRow drawn={statement} />
          <LensRow drawn={constraint} />
        </div>
        <AddressDrawn />
      </>
    );
    try {
      const link = view.container.querySelector<HTMLElement>(
        `[data-node-row][data-node-id="${CSS.escape(statement.id)}"] [data-node-chip] a`
      );
      if (link === null) throw new Error("the statement draws no chip's link");
      await user.click(link);
      expect(
        view.container.querySelector("[data-address]")?.getAttribute("data-address"),
        "the press did not open Graph"
      ).toBe("/issues/CORE-1/graph?node=constraint%2F9a1b");
      expect(
        document.activeElement?.getAttribute("data-node-id"),
        "the press focused a row Conversation draws"
      ).not.toBe(constraintId);
    } finally {
      view.unmount();
    }
  });

  // The plan's decision on a constraint's row: a standard's line holds its reference, and a
  // measurement's says where it was taken.
  test("a standard draws its reference and a measurement where it was taken", () => {
    const standard = node({
      id: "constraint/9a1b",
      lens: facts({ reference: "ISO/IEC 27001 A.8.10" }),
      text: "Information deletion",
      type: "constraint",
    });
    const measurement = node({
      id: "constraint/9a1c",
      lens: facts({ takenWhere: "the staging cluster" }),
      text: "p99 latency: 180 ms",
      type: "constraint",
    });
    expect(linesDrawn(standard)).toEqual([
      { chips: [], label: null, line: "reference", words: ["ISO/IEC 27001 A.8.10"] },
    ]);
    expect(linesDrawn(measurement)).toEqual([
      { chips: [], label: LENS_WORDS.taken_at, line: "taken_at", words: ["the staging cluster"] },
    ]);
  });

  // A lens draws no line for its kind, which its tag carries whole; a risk draws its
  // prevention after its early warning, whether or not its row has the focus.
  test("a lens draws no line for its kind and a risk draws its prevention", () => {
    const lens = node({
      id: "lens/5555",
      lens: facts({ kind: "risk review of the notice store's retention" }),
      reference: FURTHER,
      type: "lens",
    });
    expect(linesDrawn(lens)).toEqual([]);

    const prevents = risk({ prevention: "Expire notices every hour" });
    expect(linesDrawn(prevents).map(({ line }) => line)).toEqual([
      "early_warning",
      "prevention",
      "prevented_by_nothing",
    ]);
    expect(linesDrawn(prevents)[1]).toEqual({
      chips: [],
      label: LENS_WORDS.prevention,
      line: "prevention",
      words: ["Expire notices every hour"],
    });
  });
});

// A lens's tag and the chips of its lines name a lens by its kind's words, which can run to 129
// characters: each is drawn whole and is not held to one line, so it wraps inside its column
// instead of widening the page. The tag keeps part T's one line for every other kind
// (`whitespace-nowrap`), so a lens's tag draws its kind's words in a span of their own that wraps
// anywhere, its shape in another at the top, beside their first line (`NodeRow.tsx`'s `KindTag`).
test("a lens's tag and its chips draw a kind of 129 characters whole, and wrap", () => {
  const kind =
    "pre-mortem: imagine the work failed a year from now, then name each warning the team missed and say why nobody acted on it at all";
  expect(kind).toHaveLength(129);
  const drawn = node({
    id: "lens/2222",
    lens: facts({
      kind,
      triggeredBy: [{ address: TRADE_OFF, id: "lens/1111", words: kindWords(kind) }],
    }),
    reference: PREMORTEM,
    type: "lens",
  });
  const view = renderOnIssue(<LensRow drawn={drawn} />);
  try {
    const tag = view.container.querySelector("[data-node-tag]");
    const words = tag?.querySelector("[data-author-text]");
    const shape = tag?.querySelector("svg")?.parentElement;
    const chip = view.container.querySelector("[data-node-chip] [data-chip-name]");
    expect(tag?.textContent).toBe(kindWords(kind));
    expect(words?.textContent).toBe(kindWords(kind));
    expect(chip?.textContent).toBe(kindWords(kind));
    expect(words?.className, "the tag's words hold one line").toContain("whitespace-normal");
    expect(words?.className, "the tag's words break no word").toContain("[overflow-wrap:anywhere]");
    expect(shape?.className, "the shape is not at the top of the tag").toContain("self-start");
    expect(chip?.className, "the chip holds one line").not.toContain("whitespace-nowrap");
  } finally {
    view.unmount();
  }
});

/** The tone, as `pageKindBullet` and `pageKindTag` name it, that `drawn`'s row draws its bullet
 *  and its tag in. */
function tonesDrawn(drawn: TreeNode) {
  const view = renderOnIssue(<LensRow drawn={drawn} />);
  try {
    const dot = view.container.querySelector("[data-node-bullet] i");
    const tag = view.container.querySelector("[data-node-margin] [data-node-tag]");
    return {
      bullet:
        Object.entries(pageKindBullet).find(([, drawnAs]) =>
          dot?.classList.contains(drawnAs)
        )?.[0] ?? null,
      tag:
        Object.entries(pageKindTag).find(([, drawnAs]) => tag?.classList.contains(drawnAs))?.[0] ??
        null,
    };
  } finally {
    view.unmount();
  }
}

// The spec's Design: a constraint's row draws its bullet and its tag in a statement's colour,
// though it is no block of the spec and so never `isStatement`; a lens, a finding and a risk
// draw theirs in the lenses' own.
test("a constraint's row takes a statement's colour, and a lens's, a finding's and a risk's the lenses'", () => {
  const constraint = node({
    id: "constraint/9a1b",
    lens: facts({ reference: "ISO/IEC 27001 A.8.10" }),
    reference: ISSUE_CONSTRAINT,
    text: "Information deletion",
    type: "constraint",
  });
  expect(tonesDrawn(constraint), "the constraint").toEqual({
    bullet: "statement",
    tag: "statement",
  });
  const finding = node({
    id: "lens/2222#b-f1",
    text: "The store keeps no expiry",
    type: "finding",
  });
  for (const drawn of [premortem(), finding, risk({})]) {
    expect(tonesDrawn(drawn), `the ${drawn.type}`).toEqual({ bullet: "lens", tag: "lens" });
  }
});

// A lens's verdict that links its author's words to a risk the tree read holds,
// `[the failover risk](dispatch://risk/<uuid>)`, draws those words as a link that opens the
// risk's row (`?node=`), as the lens's row draws its words from the tree read
// (`buildIssueTree`).
test("a lens row draws its verdict's author's link to a risk as a link to the risk's row", async () => {
  const lens = "c3000000-0000-4000-8000-000000000003";
  const risk = "d4000000-0000-4000-8000-000000000004";
  const root = buildIssueTree({
    issue: issueDetails(),
    tree: treeAnswer(
      [
        treeNode(KEY, "issue", null, { text: "The issue page draws its lenses" }),
        treeNode(lens, "lens", KEY, {
          attributes: { verdict: `See [the failover risk](dispatch://risk/${risk}) first.` },
        }),
        treeNode(risk, "risk", lens, { attributes: { heading: "Failover takes minutes" } }),
      ],
      [],
      { converted: true, tree: "artifact-tree" }
    ),
  });
  const drawn = root.children.find((row) => row.id === lens);
  if (drawn === undefined) throw new Error("the tree draws no row of the lens");
  const view = renderOnIssue(<LensRow drawn={drawn} />);
  try {
    await waitFor(() => {
      const links = [...view.container.querySelectorAll("a")].filter(
        (anchor) => anchor.textContent === "the failover risk"
      );
      expect(links.map((link) => link.getAttribute("href"))).toEqual([
        `/issues/${KEY}?node=${risk}`,
      ]);
    });
  } finally {
    view.unmount();
  }
});

/** The ids of the risk row the early-warning cases draw: a lens on the issue, a risk under it,
 *  and a constraint of the issue the risk's early warning cites. */
const WARNING_ROW = {
  constraint: "e5000000-0000-4000-8000-000000000005",
  lens: "c3000000-0000-4000-8000-000000000003",
  risk: "d4000000-0000-4000-8000-000000000004",
} as const;

/** Draws a risk row from the tree read (`buildIssueTree`) whose early warning is `earlyWarning`,
 *  and waits until its line draws `expected`: its author's words, and each link in it as its
 *  words and its address. */
async function expectEarlyWarningDrawn(
  earlyWarning: string,
  expected: [string, [string, string][]]
): Promise<void> {
  const { constraint, lens, risk } = WARNING_ROW;
  const root = buildIssueTree({
    issue: issueDetails(),
    tree: treeAnswer(
      [
        treeNode(KEY, "issue", null, { text: "The issue page draws its lenses" }),
        treeNode(lens, "lens", KEY),
        treeNode(risk, "risk", lens, {
          attributes: { early_warning: earlyWarning, heading: "Replies break the HTTP rule" },
        }),
        treeNode(constraint, "constraint", KEY, { attributes: { standard: "RFC 9110" } }),
      ],
      [],
      { converted: true, tree: "artifact-tree" }
    ),
  });
  const drawn = root.children
    .find((row) => row.id === lens)
    ?.children.find((row) => row.id === risk);
  if (drawn === undefined) throw new Error("the tree draws no row of the risk");
  const view = renderOnIssue(<LensRow drawn={drawn} />);
  try {
    await waitFor(() => {
      const line = view.container.querySelector('[data-lens-line="early_warning"]');
      const links = [...(line?.querySelectorAll("a") ?? [])];
      expect([
        line?.querySelector("[data-author-text]")?.textContent ?? "",
        links.map((link): [string, string] => [
          link.textContent ?? "",
          link.getAttribute("href") ?? "",
        ]),
      ]).toEqual(expected);
    });
  } finally {
    view.unmount();
  }
}

// A risk's early warning that cites a constraint the tree read holds by its bare address,
// `dispatch://constraint/<uuid>`, draws the constraint's words in its line as a link that opens
// the constraint's row (`?node=`), as the risk's row draws its lines from the tree read
// (`buildIssueTree`).
test("a risk row draws its early warning's address of a constraint as a link to the constraint's row", async () => {
  const { constraint } = WARNING_ROW;
  await expectEarlyWarningDrawn(`A reply breaks dispatch://constraint/${constraint} first.`, [
    "A reply breaks RFC 9110 first.",
    [["RFC 9110", `/issues/${KEY}?node=${constraint}`]],
  ]);
});

// A risk's early warning whose author's link to a constraint the tree read holds has words that
// carry a mark, `[the **HTTP** rule](dispatch://constraint/<uuid>)`, draws those words as one link
// that opens the constraint's row (`?node=`), not one link for each run of them.
test("a risk row draws its early warning's author's link of marked words as one link", async () => {
  const { constraint } = WARNING_ROW;
  await expectEarlyWarningDrawn(
    `A reply breaks [the **HTTP** rule](dispatch://constraint/${constraint}) first.`,
    [
      "A reply breaks the HTTP rule first.",
      [["the HTTP rule", `/issues/${KEY}?node=${constraint}`]],
    ]
  );
});

// R22 over the one reference framework: a risk's fact line reads a `dispatch://` address in its
// words as every one-line view reads one, through the plain-text projection: by the name the page
// published for its target, never as the address, and with no read of its own for a target the
// page named. A line that names a node the tree read holds keeps that name as the chip that links
// the node's row, and the words on either side of the chip are read alike, the spaces beside it
// kept.
test("a fact line reads a dispatch:// reference the page published by its target's name", async () => {
  const address = "dispatch://CORE-1/spec#b-r1";
  const drawn = risk({
    earlyWarning: `Notices pile up past ${address}`,
    prevention: [
      "Run ",
      { address: ISSUE_CONSTRAINT, id: "constraint/9a1b", words: "Retention under 24 hours" },
      ` before ${address} is read.`,
    ],
  });
  const queryClient = testClient();
  const view = renderOnIssue(
    <ReferenceNamesProvider>
      <Named published={publishedNames(new Map([[address, "Draw the rows"]]))}>
        <LensLines node={drawn} tabStops={false} />
      </Named>
    </ReferenceNamesProvider>,
    queryClient
  );
  try {
    const words = (line: string) =>
      view.container.querySelector(`[data-lens-line="${line}"] [data-author-text]`)?.textContent;
    await waitFor(() =>
      expect([words("early_warning"), words("prevention")]).toEqual([
        "Notices pile up past Draw the rows",
        "Run Retention under 24 hours before Draw the rows is read.",
      ])
    );
    expect(
      view.container
        .querySelector('[data-lens-line="prevention"] [data-node-chip] a')
        ?.getAttribute("href")
    ).toBe("/issues/CORE-1?node=constraint%2F9a1b");
    expect(
      queryClient
        .getQueryCache()
        .getAll()
        .map((query) => query.queryKey)
        .filter((key) => JSON.stringify(key).includes("spec#b-r1")),
      "the line read a target the page named"
    ).toEqual([]);
  } finally {
    view.unmount();
  }
});

// R22 over the one reference framework: a lens line's chip reads a `dispatch://` address in its
// words as every one-line view reads one, through the plain-text projection: by the name the page
// published for its target, never as the address, and with no read of its own for a target the
// page named. The chip still links its own node's row, at the same `?node=`.
test("a lens line's chip reads a dispatch:// reference the page published by its target's name", async () => {
  const address = "dispatch://CORE-1/spec#b-r1";
  const statement = node({
    id: "spec#b-r5",
    lens: facts({
      constrainedBy: [
        { address: ISSUE_CONSTRAINT, id: "constraint/9a1b", words: `Retention as ${address} says` },
      ],
    }),
    reference: "dispatch://CORE-1/spec#b-r5",
    text: "Notices expire",
    type: "requirement",
  });
  const queryClient = testClient();
  const view = renderOnIssue(
    <ReferenceNamesProvider>
      <Named published={publishedNames(new Map([[address, "Draw the rows"]]))}>
        <LensLines node={statement} tabStops={false} />
      </Named>
    </ReferenceNamesProvider>,
    queryClient
  );
  try {
    const link = () =>
      view.container.querySelector('[data-lens-line="constrained_by"] [data-node-chip] a');
    await waitFor(() => expect(link()?.textContent).toBe("Retention as Draw the rows says"));
    expect(link()?.getAttribute("href")).toBe("/issues/CORE-1?node=constraint%2F9a1b");
    expect(
      queryClient
        .getQueryCache()
        .getAll()
        .map((query) => query.queryKey)
        .filter((key) => JSON.stringify(key).includes("spec#b-r1")),
      "the chip read a target the page named"
    ).toEqual([]);
  } finally {
    view.unmount();
  }
});

/** The address the code cases name, and the words they hold it in: once in a code span, and once
 *  outside code. */
const CODE_ADDRESS = "dispatch://CORE-2";
const CODE_WORDS = `Run \`dispatch read ${CODE_ADDRESS}\` before ${CODE_ADDRESS} lands.`;

/** `CODE_WORDS` as a line draws them once the page has named the address: the code as written,
 *  and the address outside it by the issue's key and title. */
const CODE_DRAWN = `Run dispatch read ${CODE_ADDRESS} before CORE-2 Store the notices lands.`;

/** Draws `page` on this issue's route with the page's name for `CODE_ADDRESS` published. */
function renderWithCodeName(page: ReactNode): RenderResult {
  return renderOnIssue(
    <ReferenceNamesProvider>
      <Named published={publishedNames(new Map([[CODE_ADDRESS, "Store the notices"]]))}>
        {page}
      </Named>
    </ReferenceNamesProvider>
  );
}

// A rendered body draws an address in code as its author wrote it (`MarkdownBody.tsx`), so a fact
// line and a chip, drawn from the tree read, read their words from the reading that keeps the
// code: the address in the code span stays as written, and the same address outside code reads
// as the name the page published for its target.
test("a fact line and a chip keep an address in code as written and name it outside code", async () => {
  const { constraint, lens, risk } = WARNING_ROW;
  const root = buildIssueTree({
    issue: issueDetails(),
    tree: treeAnswer(
      [
        treeNode(KEY, "issue", null, { text: "The issue page draws its lenses" }),
        treeNode(lens, "lens", KEY),
        treeNode(risk, "risk", lens, {
          attributes: { early_warning: CODE_WORDS, heading: "Replies break the HTTP rule" },
        }),
        treeNode(constraint, "constraint", KEY, { attributes: { standard: CODE_WORDS } }),
      ],
      [treeTuple("constrained_by", risk, constraint)],
      { converted: true, tree: "artifact-tree" }
    ),
  });
  const drawn = root.children
    .find((row) => row.id === lens)
    ?.children.find((row) => row.id === risk);
  if (drawn === undefined) throw new Error("the tree draws no row of the risk");
  const view = renderWithCodeName(<LensLines node={drawn} tabStops={false} />);
  try {
    const words = (line: string) =>
      view.container.querySelector(`[data-lens-line="${line}"] [data-author-text]`)?.textContent;
    await waitFor(() =>
      expect([words("early_warning"), words("constrained_by")]).toEqual([CODE_DRAWN, CODE_DRAWN])
    );
  } finally {
    view.unmount();
  }
});

// A chain entry, drawn from the tree read, reads the words of the row its lens stands under from
// that row's reading, which keeps the code: the address in the code span stays as written, and
// the same address outside code reads as the name the page published for its target.
test("a chain entry keeps an address in code as written and names it outside code", async () => {
  const user = userEvent.setup();
  const { lens, risk } = WARNING_ROW;
  const first = "f6000000-0000-4000-8000-000000000006";
  const second = "a7000000-0000-4000-8000-000000000007";
  answerLensReads(() =>
    Response.json(
      lensesOf([
        chain([
          lensRead(`dispatch://lens/${first}`, null),
          lensRead(`dispatch://lens/${second}`, null),
        ]),
      ])
    )
  );
  const root = buildIssueTree({
    issue: issueDetails(),
    tree: treeAnswer(
      [
        treeNode(KEY, "issue", null, { text: "The issue page draws its lenses" }),
        treeNode(lens, "lens", KEY),
        treeNode(risk, "risk", lens, { attributes: { heading: CODE_WORDS } }),
        treeNode(first, "lens", risk),
        treeNode(second, "lens", risk),
      ],
      [treeTuple("triggered_by", second, first)],
      { converted: true, tree: "artifact-tree" }
    ),
  });
  const drawn = root.children
    .find((row) => row.id === lens)
    ?.children.find((row) => row.id === risk)
    ?.children.find((row) => row.id === second);
  if (drawn === undefined) throw new Error("the tree draws no row of the second lens");
  const view = renderWithCodeName(<LensRow drawn={drawn} focused />);
  try {
    await user.click(await drawnControl(view));
    await waitFor(() =>
      expect(chainEntries(view).map(({ line }) => line)).toEqual([CODE_DRAWN, CODE_DRAWN])
    );
  } finally {
    view.unmount();
  }
});

/** Draws `row` as its node row and waits until its words paragraph has painted, returning that
 *  paragraph and a cleanup that unmounts the view. */
async function drawnRowParagraph(
  row: TreeNode,
  renderRow: (page: ReactNode) => RenderResult = renderOnIssue
): Promise<{ paragraph: HTMLParagraphElement; unmount: () => void }> {
  const view = renderRow(<LensRow drawn={row} />);
  try {
    const paragraph = await waitFor(() => {
      const found = view.container.querySelector<HTMLParagraphElement>(
        "[data-node-words] [data-author-text] p"
      );
      if (found === null) throw new Error("the row drew no words paragraph");
      return found;
    });
    return { paragraph, unmount: () => view.unmount() };
  } catch (error) {
    view.unmount();
    throw error;
  }
}

// A risk's heading carries a one-line reading that keeps code alone, so `__init__` in backticks
// is drawn in a `code` element rather than parsed again in the page into bold "init".
test("a risk row draws a code span in its heading as code and parses no underscores", async () => {
  const { lens, risk } = WARNING_ROW;
  const root = buildIssueTree({
    issue: issueDetails(),
    tree: treeAnswer(
      [
        treeNode(KEY, "issue", null, { text: "The issue page draws its lenses" }),
        treeNode(lens, "lens", KEY),
        treeNode(risk, "risk", lens, { attributes: { heading: "`__init__` runs twice" } }),
      ],
      [],
      { converted: true, tree: "artifact-tree" }
    ),
  });
  const drawn = root.children
    .find((row) => row.id === lens)
    ?.children.find((row) => row.id === risk);
  if (drawn === undefined) throw new Error("the tree draws no row of the risk");
  const { paragraph, unmount } = await drawnRowParagraph(drawn);
  try {
    expect(paragraph.textContent).toBe("__init__ runs twice");
    expect([...paragraph.querySelectorAll("code")].map((code) => code.textContent)).toEqual([
      "__init__",
    ]);
    expect(paragraph.querySelector("strong, em")).toBeNull();
  } finally {
    unmount();
  }
});

// A lens's verdict carries a one-line reading of its plain words, so escaped emphasis stays as
// asterisks rather than being parsed again in the page into an `em` element.
test("a lens row draws escaped emphasis in its verdict as plain words", async () => {
  const { lens } = WARNING_ROW;
  const root = buildIssueTree({
    issue: issueDetails(),
    tree: treeAnswer(
      [
        treeNode(KEY, "issue", null, { text: "The issue page draws its lenses" }),
        treeNode(lens, "lens", KEY, { attributes: { verdict: "Retry \\*all\\* jobs" } }),
      ],
      [],
      { converted: true, tree: "artifact-tree" }
    ),
  });
  const drawn = root.children.find((row) => row.id === lens);
  if (drawn === undefined) throw new Error("the tree draws no row of the lens");
  const { paragraph, unmount } = await drawnRowParagraph(drawn);
  try {
    expect(paragraph.textContent).toBe("Retry *all* jobs");
    expect(paragraph.querySelector("em")).toBeNull();
  } finally {
    unmount();
  }
});

// A constraint's standard carries a one-line reading that keeps code alone, so `<T>` in backticks
// is drawn as text in a `code` element rather than parsed again in the page into an HTML node.
test("a constraint row draws angle brackets in code as text in code with no HTML node", async () => {
  const { constraint } = WARNING_ROW;
  const root = buildIssueTree({
    issue: issueDetails(),
    tree: treeAnswer(
      [
        treeNode(KEY, "issue", null, { text: "The issue page draws its lenses" }),
        treeNode(constraint, "constraint", KEY, { attributes: { standard: "`<T>` leaks" } }),
      ],
      [],
      { converted: true, tree: "artifact-tree" }
    ),
  });
  const drawn = root.children.find((row) => row.id === constraint);
  if (drawn === undefined) throw new Error("the tree draws no row of the constraint");
  const { paragraph, unmount } = await drawnRowParagraph(drawn);
  try {
    expect(paragraph.textContent).toBe("<T> leaks");
    expect([...paragraph.querySelectorAll("code")].map((code) => code.textContent)).toEqual([
      "<T>",
    ]);
    expect([...paragraph.children].map((child) => child.tagName.toLowerCase())).toEqual(["code"]);
    expect(paragraph.querySelector('[data-type="html"], t')).toBeNull();
  } finally {
    unmount();
  }
});

// A citing verdict keeps a run's code mark alongside a name's link, so another `dispatch://`
// address inside a code span stays as written in a `code` element while the cited risk is named
// and linked to its row.
test("a lens row keeps another dispatch:// address in code as written in a citing verdict", async () => {
  const { lens, risk } = WARNING_ROW;
  const root = buildIssueTree({
    issue: issueDetails(),
    tree: treeAnswer(
      [
        treeNode(KEY, "issue", null, { text: "The issue page draws its lenses" }),
        treeNode(lens, "lens", KEY, {
          attributes: {
            verdict: `Run \`dispatch read ${CODE_ADDRESS}\` before dispatch://risk/${risk}.`,
          },
        }),
        treeNode(risk, "risk", lens, { attributes: { heading: "Failover takes minutes" } }),
      ],
      [],
      { converted: true, tree: "artifact-tree" }
    ),
  });
  const drawn = root.children.find((row) => row.id === lens);
  if (drawn === undefined) throw new Error("the tree draws no row of the lens");
  const { paragraph, unmount } = await drawnRowParagraph(drawn, renderWithCodeName);
  try {
    await waitFor(() => {
      expect(paragraph.textContent).toBe(
        `Run dispatch read ${CODE_ADDRESS} before Failover takes minutes.`
      );
      expect([...paragraph.querySelectorAll("code")].map((code) => code.textContent)).toEqual([
        `dispatch read ${CODE_ADDRESS}`,
      ]);
      expect(
        [...paragraph.querySelectorAll("a")].map((link) => [
          link.textContent,
          link.getAttribute("href"),
        ])
      ).toEqual([["Failover takes minutes", `/issues/${KEY}?node=${risk}`]]);
    });
  } finally {
    unmount();
  }
});
