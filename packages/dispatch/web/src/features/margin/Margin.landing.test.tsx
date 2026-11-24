import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: the reader's wheel and the margin's own scroll events, which user-event has no form for
import { act, fireEvent, type RenderResult, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useMemo, useState } from "react";
import { createPortal } from "react-dom";
import { MemoryRouter } from "react-router-dom";

import { nextFrame } from "../../__tests__/frames";
import { api } from "../../api/client";
import type { Ask, Comment } from "../../api/types";
import { useDrawerPage } from "../issue-page/drawer-page";
import {
  type MarginDrawer,
  type MarginDrawerPage,
  type MarginFill,
  MarginSlotProvider,
  useFillMarginSlot,
} from "../issue-page/margin-slot";
import { buildIssuePath } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { type KeyBindingDescription, useKeymap } from "../shell/keymap";
import { ShortcutHelp } from "../shell/ShortcutHelp";
import { Margin } from "./Margin";
import { MarginProvider } from "./margin-context";
import {
  anchoredAsk,
  CommentLink,
  comment,
  DESKTOP_WIDTH,
  issue,
  KeptDetailsLinks,
  markedCommentPath,
  PHONE_WIDTH,
  ReportEmptyLayoutButton,
  SameCommentLink,
  SelectedItemLabel,
  stubViewport,
} from "./margin-fixture";
import { RELAYOUT_SETTLES_FRAMES } from "./useCardHold";

const secondComment: Comment = {
  ...comment,
  anchor: comment.anchor === null ? null : { ...comment.anchor, mark_id: "m-2" },
  body: "and this one?",
  id: "comment-2",
};

/** The anchored ask's own thread, which its margin card reads: the ask as the fixture has it,
 *  with nothing answered, edited, replied or followed. */
function answerAnchoredAsk() {
  return spyOn(api, "getAsk").mockResolvedValue({
    answers: [],
    ask: anchoredAsk,
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
  });
}

function stubRects(cardTop: () => number): () => void {
  const bounds = (top: number, height: number) =>
    ({
      bottom: top + height,
      height,
      left: 0,
      right: 384,
      toJSON: () => ({}),
      top,
      width: 384,
      x: 0,
      y: top,
    }) as DOMRect;
  // Stubbed on Element, the prototype that owns the method: patching HTMLElement would leave an
  // own property behind that shadows every other suite's spy on Element.
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ): DOMRect {
    if (this.getAttribute("data-testid") === "margin-sheet") {
      return bounds(0, 600);
    }
    if (this.hasAttribute("data-margin-item")) {
      return bounds(cardTop(), 80);
    }
    return bounds(0, 0);
  });
  return () => rect.mockRestore();
}

/** Stubs `scrollTo` on every element, the call the margin brings a card into view with. On
 *  Element, which owns the method, for the reason `stubRects` gives. */
function stubScrollTo() {
  return spyOn(Element.prototype, "scrollTo").mockImplementation(() => {});
}

function landingClient(comments: Comment[], asks: Ask[] = []): QueryClient {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], asks);
  queryClient.setQueryData(["asks", issue.key], asks);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], comments);
  return queryClient;
}

function renderCommentLinkLanding(comments: Comment[] = [comment], asks: Ask[] = []): RenderResult {
  return render(
    <MemoryRouter
      initialEntries={[`${buildIssuePath({ key: issue.key, kind: "spec" })}?comment=comment-1`]}
    >
      <QueryClientProvider client={landingClient(comments, asks)}>
        <MarginProvider>
          <SelectedItemLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** Longer than the hold's relayout window, so what follows is judged on its own: whatever the
 *  margin is still rendering lands first, then the window's frames run out. */
async function settled(): Promise<void> {
  await quiet();
  for (let frame = 0; frame <= RELAYOUT_SETTLES_FRAMES; frame += 1) {
    await nextFrame();
  }
}

/** Lets whatever the margin is still doing happen. This one stays on the real clock
 *  (INFRA-1505): the margin corrects a card in an animation frame, and under
 *  `jest.useFakeTimers()` one `tick` runs exactly one frame where 50 ms of real time runs many,
 *  so every "and nothing more moved" below would hold for want of frames rather than because
 *  the margin left the card alone. The fake clock also freezes `performance.now()`, which the
 *  long-task row spins on. */
async function quiet(): Promise<void> {
  const settled = Promise.withResolvers<void>();
  setTimeout(settled.resolve, 50);
  await settled.promise;
}

test("a phone comment deep link highlights its card without expanding it or opening the Thread dialog", async () => {
  // The link's destination on a phone is the Conversation turn. The margin marks the card so the
  // reader finds it when they open the review panel, but a thread on a phone opens only in the
  // Thread dialog: an inline expansion left behind here would strand a Reply composer whose Cancel
  // opens the fullscreen thread (#1239).
  const restoreMatchMedia = stubViewport(PHONE_WIDTH);
  const view = render(
    <MemoryRouter
      initialEntries={[buildIssuePath({ id: comment.id, key: issue.key, kind: "comment" })]}
    >
      <QueryClientProvider client={landingClient([comment])}>
        <MarginProvider>
          <SelectedItemLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe(comment.id)
    );
    expect(screen.queryByRole("dialog", { name: "Thread" })).not.toBeInTheDocument();
    expect(screen.getByTestId("margin-sheet")).toHaveAttribute("data-expanded", "false");
    const card = screen.queryByTestId(`margin-comment-${comment.id}`);
    expect(card?.getAttribute("aria-expanded") ?? "false").toBe("false");
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

/** The new page's fill of the margin's slot, at its plainest: the margin's own content. */
const passThrough: MarginFill = (content) => content;

/** What the new issue page's root does for Details while it is mounted: it sets the drawer's
 *  words and its address rule (`drawer-page.ts`), and, where `fills`, fills the margin's slot so
 *  the margin's cards are drawn in Details. */
function NewPageRoot({ fills }: { fills: boolean }): null {
  useDrawerPage(issue);
  useFillMarginSlot(fills ? passThrough : undefined);
  return null;
}

/** The shell's Details drawer as `app.tsx` holds it, closed on the load unless `open`. */
function Details({
  children,
  fills = false,
  open: openAtLoad = false,
}: {
  children: ReactNode;
  fills?: boolean;
  open?: boolean;
}): ReactNode {
  const [open, setOpen] = useState(openAtLoad);
  const [page, setPage] = useState<MarginDrawerPage | undefined>(undefined);
  const drawer = useMemo<MarginDrawer>(() => ({ open, page, setOpen, setPage }), [open, page]);
  return (
    <MarginSlotProvider drawer={drawer} drawsSlot>
      <NewPageRoot fills={fills} />
      {children}
    </MarginSlotProvider>
  );
}

// R20: on the new issue page a comment's own route, and `?comment=` on an
// issue's address, open the outline on the comment's node, focused. The margin in Details marks
// its copy of the comment and expands nothing at a desktop width either: an expanded thread's
// Reply box takes the focus off that node as it mounts.
test("a desktop link to a comment's node marks its card without expanding its thread", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  // The card below the margin's view, as main's deep-link case puts it, so a hold on it would
  // scroll the margin: only the address's rule keeps it still.
  const restoreRects = stubRects(() => 900);
  const scrollTo = stubScrollTo();
  try {
    for (const path of [
      buildIssuePath({ id: comment.id, key: issue.key, kind: "comment" }),
      `${buildIssuePath({ key: issue.key, kind: "spec" })}?comment=${comment.id}`,
    ]) {
      scrollTo.mockClear();
      const view = render(
        <MemoryRouter initialEntries={[path]}>
          <QueryClientProvider client={landingClient([comment])}>
            <MarginProvider>
              <Details fills open>
                <SelectedItemLabel />
                <Margin />
              </Details>
            </MarginProvider>
          </QueryClientProvider>
        </MemoryRouter>
      );
      try {
        await waitFor(() =>
          expect(screen.getByLabelText("Selected margin item").textContent).toBe(comment.id)
        );
        await settled();
        expect(
          screen.getByTestId(`margin-comment-${comment.id}`).getAttribute("aria-expanded"),
          `${path} expands the comment's thread`
        ).toBe("false");
        expect(screen.queryByRole("textbox", { name: "Reply" })).not.toBeInTheDocument();
        expect(scrollTo, `${path} scrolls the margin to the comment's card`).not.toHaveBeenCalled();
      } finally {
        view.unmount();
      }
    }
  } finally {
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

// R14: Details is closed on every load, but a reader who follows a link to
// a comment beside its document asked to see that comment's card, so the drawer opens for it -
// once, so their Close keeps it closed. A link to a comment's node lands on the outline and
// leaves Details closed.
test("a link to a card beside its document opens Details on it, and a link to a node does not", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const scrollTo = stubScrollTo();
  try {
    for (const [path, opens] of [
      [markedCommentPath(comment.id), true],
      [buildIssuePath({ id: comment.id, key: issue.key, kind: "comment" }), false],
      [`${buildIssuePath({ key: issue.key, kind: "spec" })}?comment=${comment.id}`, false],
    ] as const) {
      const view = render(
        <MemoryRouter initialEntries={[path]}>
          <QueryClientProvider client={landingClient([comment])}>
            <MarginProvider>
              <Details>
                <SelectedItemLabel />
                <Margin />
              </Details>
            </MarginProvider>
          </QueryClientProvider>
        </MemoryRouter>
      );
      try {
        await waitFor(() =>
          expect(screen.getByLabelText("Selected margin item").textContent).toBe(comment.id)
        );
        await settled();
        const details = screen.getByTestId("desktop-margin-shell");
        expect(
          details.hasAttribute("hidden"),
          `${path} ${opens ? "left Details closed" : "opened Details"}`
        ).toBe(!opens);
        if (opens) {
          await user.click(screen.getByRole("button", { name: "Close" }));
          await settled();
          expect(details.hasAttribute("hidden"), "Details opened again after its Close").toBe(true);
        }
      } finally {
        view.unmount();
      }
    }
  } finally {
    restoreMatchMedia();
    scrollTo.mockRestore();
  }
});

// R14: on a phone Details is a sheet over the whole page, so a link followed inside
// it closes the sheet. The page draws its panels into the drawer through a portal, outside the
// drawer's React tree, so the drawer must hear the link's click from the DOM.
test("on a phone, a link followed in a panel portaled into Details closes the sheet", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubViewport(PHONE_WIDTH);
  const tree = (target: Element | null) => (
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={landingClient([])}>
        <MarginProvider>
          <Details open>
            <Margin />
          </Details>
          {target === null
            ? null
            : createPortal(
                <a href="/issues/CORE-2" onClick={(event) => event.preventDefault()}>
                  a panel's link
                </a>,
                target
              )}
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(null));
  try {
    const details = await screen.findByTestId("desktop-margin-shell");
    await waitFor(() => expect(details.hasAttribute("hidden")).toBe(false));
    view.rerender(tree(details));
    await user.click(screen.getByRole("link", { name: "a panel's link" }));
    await waitFor(() =>
      expect(
        details.hasAttribute("hidden"),
        "a link followed in a portaled panel left the phone's Details open"
      ).toBe(true)
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

// A link that carries `KEEPS_DETAILS_ATTRIBUTE`, or stands inside an element that does, such as a
// history turn's time link (`KeptDetailsLinks`), is followed with the phone's sheet left open; a
// plain link beside it still closes the sheet.
test("on a phone, a link that keeps Details open leaves the sheet open, and a plain link still closes it", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubViewport(PHONE_WIDTH);
  const tree = (target: Element | null) => (
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={landingClient([])}>
        <MarginProvider>
          <Details open>
            <Margin />
          </Details>
          {target === null
            ? null
            : createPortal(
                <>
                  <KeptDetailsLinks />
                  <a href="/issues/CORE-2" onClick={(event) => event.preventDefault()}>
                    a panel's link
                  </a>
                </>,
                target
              )}
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(null));
  try {
    const details = await screen.findByTestId("desktop-margin-shell");
    await waitFor(() => expect(details.hasAttribute("hidden")).toBe(false));
    view.rerender(tree(details));
    for (const name of ["a turn's time", "a link inside a turn"]) {
      await user.click(screen.getByRole("link", { name }));
      await settled();
      expect(
        details.hasAttribute("hidden"),
        `"${name}", which keeps Details open, closed the phone's Details`
      ).toBe(false);
    }
    await user.click(screen.getByRole("link", { name: "a panel's link" }));
    await waitFor(() =>
      expect(
        details.hasAttribute("hidden"),
        "a plain link beside the kept ones left the phone's Details open"
      ).toBe(true)
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

/** The list of keys over the page, as `app.tsx` binds its `?`. */
function ListOfKeys(): ReactNode {
  const [snapshot, setSnapshot] = useState<readonly KeyBindingDescription[] | null>(null);
  useKeymap("global", [
    { id: "help", keys: "?", label: "Keyboard shortcuts", run: () => setSnapshot([]) },
  ]);
  return <ShortcutHelp onClose={() => setSnapshot(null)} snapshot={snapshot} />;
}

// R14: Escape closes the innermost layer. With the list of keys open over Details, Escape closes
// the list alone, wherever the focus is: on the list itself in the frame before its filter takes
// it, or on the page after a press on the list's own text.
test("with the list of keys over Details, Escape closes the list alone, the focus outside it", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={landingClient([])}>
        <KeymapProvider>
          <MarginProvider>
            <Details open>
              <Margin />
            </Details>
            <ListOfKeys />
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const details = await screen.findByTestId("desktop-margin-shell");
    const close = await screen.findByTestId("details-close");
    act(() => close.focus());
    const list = () => screen.queryByRole("dialog", { name: "Keyboard shortcuts" });

    // The list holds the focus itself until its filter takes it a frame after it opens; with that
    // frame held, the Escape lands on the list, not on Details' Close.
    const frames = spyOn(window, "requestAnimationFrame").mockImplementation(() => 0);
    try {
      await user.keyboard("?");
      expect(list(), "? drew no list of keys").not.toBeNull();
      expect(document.activeElement, "the list left the focus behind it as it opened").toBe(list());
      await user.keyboard("{Escape}");
    } finally {
      frames.mockRestore();
    }
    expect(list(), "Escape before the list took the focus left the list open").toBeNull();
    expect(details.hasAttribute("hidden"), "that Escape closed Details under the list").toBe(false);

    await user.keyboard("?");
    await user.click(screen.getByRole("heading", { name: "Keyboard shortcuts" }));
    expect(document.activeElement, "a press on the list's text kept the focus").toBe(document.body);
    await user.keyboard("{Escape}");
    expect(list(), "Escape with the focus on the page left the list open").toBeNull();
    expect(details.hasAttribute("hidden"), "that Escape closed Details under the list").toBe(false);
  } finally {
    view.unmount();
    restoreMatchMedia();
  }
});

test("a desktop comment deep link activates Comments and scrolls its card from Pinned", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([comment]);
  const restoreRects = stubRects(() => 900);
  const scrollTo = stubScrollTo();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <CommentLink />
          <SelectedItemLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("margin-comment-comment-1");
    await user.click(screen.getByRole("tab", { name: "Pinned" }));
    expect(screen.getByRole("tab", { name: "Pinned" })).toHaveAttribute("aria-selected", "true");

    await user.click(screen.getByRole("button", { name: "Open comment" }));

    await waitFor(() => {
      expect(screen.getByRole("tab", { name: "Comments" })).toHaveAttribute(
        "aria-selected",
        "true"
      );
      expect(scrollTo).toHaveBeenCalled();
    });
    const card = screen.getByTestId("margin-comment-comment-1");
    await user.click(card);
    expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-1");
  } finally {
    view.unmount();
    restoreMatchMedia();
    getIssue.mockRestore();
    getInbox.mockRestore();
    listIssueAsks.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("a reader who lands on a comment link can still select another card", async () => {
  const user = userEvent.setup();
  // The URL keeps naming its item for the whole visit, so reading it ahead of the reader's own
  // selection - or re-applying it on every render - would pin the linked card as the selected one
  // and leave every later click with nothing to show for it.
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const scrollTo = stubScrollTo();
  const getAsk = answerAnchoredAsk();
  const view = renderCommentLinkLanding([comment, secondComment], [anchoredAsk]);

  try {
    const linked = await screen.findByTestId("margin-comment-comment-1");
    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-1")
    );
    expect(linked).toHaveAttribute("aria-current", "true");

    await user.click(screen.getByTestId("margin-comment-comment-2"));

    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-2")
    );
    const second = screen.getByTestId("margin-comment-comment-2");
    expect(second).toHaveAttribute("aria-current", "true");
    expect(second).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByTestId("margin-comment-comment-1").getAttribute("aria-current")).toBeNull();
  } finally {
    view.unmount();
    restoreMatchMedia();
    scrollTo.mockRestore();
    getAsk.mockRestore();
  }
});

test("a reader who lands on a comment link can select a Needs you ask", async () => {
  const user = userEvent.setup();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const scrollTo = stubScrollTo();
  const getAsk = answerAnchoredAsk();
  const view = renderCommentLinkLanding([comment], [anchoredAsk]);

  try {
    await screen.findByTestId("margin-comment-comment-1");
    await user.click(await screen.findByTestId(`ask-${anchoredAsk.id}`));

    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe(anchoredAsk.id)
    );
    expect(screen.getByTestId("margin-comment-comment-1").getAttribute("aria-current")).toBeNull();
  } finally {
    view.unmount();
    restoreMatchMedia();
    scrollTo.mockRestore();
    getAsk.mockRestore();
  }
});

test("following the same comment link again brings the reader back to its card", async () => {
  const user = userEvent.setup();
  // Once per link is once per navigation: the URL still names comment-1 after the reader has
  // moved to another card, so a link back to it has to be honoured rather than seen as already
  // applied.
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const scrollTo = stubScrollTo();
  const view = render(
    <MemoryRouter
      initialEntries={[`${buildIssuePath({ key: issue.key, kind: "spec" })}?comment=comment-1`]}
    >
      <QueryClientProvider client={landingClient([comment, secondComment])}>
        <MarginProvider>
          <SameCommentLink />
          <SelectedItemLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("margin-comment-comment-1");
    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-1")
    );

    await user.click(screen.getByTestId("margin-comment-comment-2"));
    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-2")
    );

    await user.click(screen.getByRole("button", { name: "Open the same comment" }));

    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-1")
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
    scrollTo.mockRestore();
  }
});

test("following a second comment link selects the item that link names", async () => {
  const user = userEvent.setup();
  // The route selection is applied once per link, not once ever: a reader who follows another
  // link from the same page has to land on its card.
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const scrollTo = stubScrollTo();
  const view = render(
    <MemoryRouter
      initialEntries={[`${buildIssuePath({ key: issue.key, kind: "spec" })}?comment=comment-2`]}
    >
      <QueryClientProvider client={landingClient([comment, secondComment])}>
        <MarginProvider>
          <CommentLink />
          <SelectedItemLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("margin-comment-comment-2");
    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-2")
    );

    await user.click(screen.getByRole("button", { name: "Open comment" }));

    await waitFor(() =>
      expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-1")
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
    scrollTo.mockRestore();
  }
});

test("the margin holds a linked card in view while the margin is still filling in", async () => {
  // The margin arrives in pieces. On the frame it first has the card, the open document has
  // published no mark offsets, so every anchored card is stacked at the top of the margin and
  // the linked one is trivially in view; its real placement lands hundreds of milliseconds
  // later, and the asks that need the reader render above it later still. A margin that scrolls
  // once, or stops the first time the card is in view, leaves it below the fold.
  let cardTop = 0;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = stubScrollTo();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);

  const view = renderCommentLinkLanding();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    // Unplaced at the top of the margin, the card needs no scroll - and is not settled.
    await quiet();
    const beforePlacement = scrollTo.mock.calls.length;
    expect(beforePlacement).toBe(0);

    // The document's marks land and the card takes its real place, far below the scrollport.
    cardTop = 1826;
    act(() => placement.setAttribute("style", "position: absolute; top: 1826px;"));
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(beforePlacement));

    // Inside the scrollport, the margin leaves the card - and the reader's scrolling - alone.
    cardTop = 120;
    act(() => placement.setAttribute("style", "position: absolute; top: 120px;"));
    await quiet();
    const settled = scrollTo.mock.calls.length;
    act(() => placement.setAttribute("style", "position: absolute; top: 140px;"));
    await quiet();
    expect(scrollTo.mock.calls.length).toBe(settled);

    // Cards rendered above push it back out of the scrollport: the margin brings it back.
    cardTop = 900;
    act(() => placement.setAttribute("style", "position: absolute; top: 900px;"));
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(settled));
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("the margin stops correcting a linked card once the reader scrolls the margin", async () => {
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = stubScrollTo();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);

  const view = renderCommentLinkLanding();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));

    // user-event has no form for a wheel: `hover` sends a fixed mouse sequence and no wheel.
    fireEvent.wheel(screen.getByTestId("margin-sheet"));
    await quiet();
    const afterReader = scrollTo.mock.calls.length;

    cardTop = 1200;
    act(() => placement.setAttribute("style", "position: absolute; top: 1200px;"));
    await quiet();
    expect(scrollTo.mock.calls.length).toBe(afterReader);
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

/** The comment link's landing beside a document that registers and reports an empty layout on
 *  the "Report layout" press: by default the spec the margin shows cards for. */
function renderLandingWithReport(artifactId?: string): RenderResult {
  return render(
    <MemoryRouter
      initialEntries={[`${buildIssuePath({ key: issue.key, kind: "spec" })}?comment=comment-1`]}
    >
      <QueryClientProvider client={landingClient([comment])}>
        <MarginProvider>
          <ReportEmptyLayoutButton artifactId={artifactId} />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("a press in the document takes over once the document has reported an empty layout", async () => {
  const user = userEvent.setup();
  // The signal is the document saying it reported, not the size of what it reported: an orphaned
  // comment on a document with no live mark and no typed ask block publishes empty maps, and a
  // gate that read their size would hold that landing against the reader for ever.
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = stubScrollTo();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  // The spec's margin reads the issue's decisions, which this issue has none of.
  const getIssueDecisions = spyOn(api, "getIssueDecisions").mockResolvedValue({
    decisions: [],
    document: null,
    entries: [],
    open_questions: [],
  });
  const view = renderLandingWithReport();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));

    await user.click(screen.getByRole("button", { name: "Report layout" }));
    await quiet();
    act(() => {
      document.body.dispatchEvent(new window.PointerEvent("pointerdown", { bubbles: true }));
    });
    await quiet();
    const afterReader = scrollTo.mock.calls.length;

    cardTop = 1200;
    act(() => placement.setAttribute("style", "position: absolute; top: 1200px;"));
    await quiet();
    expect(scrollTo.mock.calls.length).toBe(afterReader);
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
    getIssueDecisions.mockRestore();
  }
});

test("a press in the document leaves the hold armed while the layout reported is another document's", async () => {
  const user = userEvent.setup();
  // A document the route has left can stay registered - hidden behind the next page's loading
  // view until that page's code arrives - with its layout reported. Its report says nothing
  // about where this margin's cards belong, so a press then is the reader arriving, not leaving.
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = stubScrollTo();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const view = renderLandingWithReport("artifact-handbook");

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));

    await user.click(screen.getByRole("button", { name: "Report layout" }));
    await quiet();
    act(() => {
      document.body.dispatchEvent(new window.PointerEvent("pointerdown", { bubbles: true }));
    });
    await quiet();
    const afterPress = scrollTo.mock.calls.length;

    cardTop = 1200;
    act(() => placement.setAttribute("style", "position: absolute; top: 1200px;"));
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(afterPress));
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("a text update in the margin keeps its anchoring scroll from taking over", async () => {
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = stubScrollTo();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const view = renderCommentLinkLanding();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));
    await settled();

    const text = document.createTreeWalker(card, NodeFilter.SHOW_TEXT).nextNode();
    if (!(text instanceof Text)) {
      throw new Error("Expected the margin card to contain text");
    }
    const sheet = screen.getByTestId("margin-sheet");
    act(() => {
      text.data = `${text.data} after the reference title resolves`;
      sheet.scrollTop = 240;
      // The margin's own scroll event, which user-event has no form for.
      fireEvent.scroll(sheet);
    });
    await quiet();
    const afterReference = scrollTo.mock.calls.length;

    cardTop = 1400;
    act(() => placement.setAttribute("style", "position: absolute; top: 1400px;"));
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(afterReference));
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("a scroll the margin did not perform, with nothing having changed, ends the hold", async () => {
  // The other half of the relayout window: a scroll that no layout change and no correction of
  // ours explains is the reader's, even without a gesture to recognise them by.
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = stubScrollTo();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);

  const view = renderCommentLinkLanding();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));

    // Long enough for the margin's own relayout to have settled, so the scroll below is the
    // reader's and nothing else's.
    await settled();
    const sheet = screen.getByTestId("margin-sheet");
    sheet.scrollTop = 404;
    // The reader's scroll with no gesture behind it, which user-event has no form for.
    fireEvent.scroll(sheet);
    await quiet();
    const afterReader = scrollTo.mock.calls.length;

    cardTop = 1200;
    act(() => placement.setAttribute("style", "position: absolute; top: 1200px;"));
    await quiet();
    expect(scrollTo.mock.calls.length).toBe(afterReader);
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("a relayout that scrolls the margin twice leaves the hold alone", async () => {
  // One relayout moves the scroll more than once: anchoring while the content changes, then
  // clamping as it settles. A rule the first of those consumed read the second as the reader.
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = stubScrollTo();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);

  const view = renderCommentLinkLanding();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));
    await settled();

    const sheet = screen.getByTestId("margin-sheet");
    act(() => placement.setAttribute("style", "position: absolute; top: 950px;"));
    sheet.scrollTop = 120;
    // One relayout's two scrolls, which user-event has no form for.
    fireEvent.scroll(sheet);
    sheet.scrollTop = 240;
    fireEvent.scroll(sheet);
    await quiet();
    const beforeTheNextRelayout = scrollTo.mock.calls.length;

    cardTop = 1400;
    act(() => placement.setAttribute("style", "position: absolute; top: 1400px;"));
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(beforeTheNextRelayout));
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("a stream of scrolls after one relayout does not slide the window along", async () => {
  // The window is re-opened by a layout change, never by a scroll landing inside it: otherwise a
  // reader's gesture-less scrolling - find-in-page, assistive tech - would hold it open forever.
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = stubScrollTo();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);

  const view = renderCommentLinkLanding();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));
    await settled();

    // One relayout, then a stream of scrolls one frame apart, so each lands inside the window the
    // one before it would have extended - a reader dragging a scrollbar the page sees no pointer
    // event for, or paging through find-in-page hits. Twelve frames is four windows' worth.
    const sheet = screen.getByTestId("margin-sheet");
    act(() => placement.setAttribute("style", "position: absolute; top: 950px;"));
    for (const step of [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12]) {
      sheet.scrollTop = 100 * step;
      // A gesture-less scroll of the reader's, which user-event has no form for.
      fireEvent.scroll(sheet);
      await nextFrame();
    }
    const afterReader = scrollTo.mock.calls.length;

    cardTop = 1400;
    act(() => placement.setAttribute("style", "position: absolute; top: 1400px;"));
    await quiet();
    expect(scrollTo.mock.calls.length).toBe(afterReader);
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});

test("a relayout's own scroll that lands after a long task leaves the hold alone", async () => {
  // The browser delivers a relayout's scroll at a rendering update, and a page busy mounting its
  // editor can go hundreds of milliseconds without one: CI's WebKit let the margin clamp to a
  // shorter content height, then delivered that clamp's scroll after a long task, and a window
  // timed on the wall clock read it as the reader. No frame has run here, so it is the layout's.
  let cardTop = 900;
  const restoreRects = stubRects(() => cardTop);
  const scrollTo = stubScrollTo();
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);

  const view = renderCommentLinkLanding();

  try {
    const card = await screen.findByTestId("margin-comment-comment-1");
    const placement = card.parentElement;
    if (placement === null) {
      throw new Error("Expected the anchored card to be positioned by its placement wrapper");
    }
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(0));
    await settled();

    const sheet = screen.getByTestId("margin-sheet");
    placement.setAttribute("style", "position: absolute; top: 700px;");
    // The observer's callback is a microtask: it has seen the relayout before the long task.
    await Promise.resolve();
    // A real long task, which no fake clock can model: what this row needs is that no rendering
    // update runs between the relayout and the scroll, and a synchronous loop is the only way to
    // hold one off. The span stays at 500 ms and no measurement can shorten it: `useCardHold`
    // counts rendering frames rather than milliseconds, so a loop of any length blocks the same
    // frames; a shorter span would read as sized by evidence while holding off less, and would
    // stop defeating a window a later implementation measured in real time.
    const busyUntil = performance.now() + 500;
    while (performance.now() < busyUntil) {
      // The long task: no rendering update runs until it ends.
    }
    sheet.scrollTop = 300;
    // The clamp's own scroll, delivered after a long task: user-event has no form for it.
    fireEvent.scroll(sheet);
    await quiet();
    const afterClamp = scrollTo.mock.calls.length;

    cardTop = 1400;
    act(() => placement.setAttribute("style", "position: absolute; top: 1400px;"));
    await waitFor(() => expect(scrollTo.mock.calls.length).toBeGreaterThan(afterClamp));
  } finally {
    view.unmount();
    restoreMatchMedia();
    restoreRects();
    scrollTo.mockRestore();
  }
});
