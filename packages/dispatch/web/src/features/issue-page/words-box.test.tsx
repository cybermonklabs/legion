import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { type RenderResult, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";
import { type ReactNode, useState } from "react";
import { Link, MemoryRouter, useLocation, useNavigate, useNavigationType } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { graphNode } from "../../__tests__/graph-fixture";
import { answerRequests } from "../../__tests__/network";
import {
  artifactRecordHandler,
  decisionsAndReadMarkHandlers,
} from "../../__tests__/proof-document";
import { api } from "../../api/client";
import type { GraphReferences, IssueDetails, IssueReferences } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { DESKTOP_WIDTH, PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { parseIssuePath } from "../refs/routes";
import { artifact, issueDetails } from "./decisions-filter/decisions-fixtures";
import { useFollowing } from "./slots/following";
import { ProvidersSlot } from "./slots/providers";
import { SLOT_FILLED, StageSlot, type ViewSlotProps, type ViewSlots } from "./slots/stage";
import { AppProviders, stubReads } from "./slots/stage-fixtures";
import { ViewRowSlot } from "./slots/view-row";

/**
 * The filter by words (part N's R4, decision 375), one case per row of the plan's state table
 * ST-1 and one for the location's state it writes. Each case draws the page's own view row over
 * the page's own stage, whose views are stand-ins that say which view they are and the words
 * they draw with, at the address the router holds. ST-1's two Escape rows are part N's Escape
 * order's (`escape-order.test.tsx`).
 */

/** The issue's spec and a document of no role, which the node menu opens. */
const NOTES = artifact("artifact-notes", "notes-md");
const ISSUE: IssueDetails = {
  ...issueDetails(),
  artifacts: [artifact("artifact-spec", "spec", true), NOTES],
};

/** How many views the stage has drawn anew, so a view drawn again reads as a new one. */
let viewsDrawn = 0;

/** A view as the stage draws it in place of the outline: which view it is, the words it draws
 *  with, and which drawing of a view it is. */
function StandInView({ route }: ViewSlotProps): ReactNode {
  const { search } = useLocation();
  const [drawing] = useState(() => {
    viewsDrawn += 1;
    return viewsDrawn;
  });
  return (
    <div
      data-drawing={drawing}
      data-stand-in-view={route.kind}
      data-words={new URLSearchParams(search).get("q") ?? ""}
    />
  );
}

/** Every view is a stand-in; Conversation is the outline itself. */
const STAND_INS: ViewSlots = {
  conversation: undefined,
  decisions: StandInView,
  plan: StandInView,
  spec: StandInView,
  verification: StandInView,
};

/** Where the router is: the address with its words, how it got there, the location's state, and
 *  the following the stage and the views read; and Back. */
function Where(): ReactNode {
  const { pathname, search, state } = useLocation();
  const navigate = useNavigate();
  return (
    <>
      <output
        data-following={useFollowing()}
        data-move={useNavigationType()}
        data-state={JSON.stringify(state)}
        data-testid="where"
      >
        {`${pathname}${search}`}
      </output>
      <button onClick={() => navigate(-1)} type="button">
        Back
      </button>
      <Link to="/issues/CORE-1/spec?q=other">Other words</Link>
    </>
  );
}

/** The view row over the stage at the address the router holds, under the page's own
 *  providers, as the page's root draws them. */
function Page(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <ProvidersSlot issue={ISSUE} route={route}>
      <ViewRowSlot issue={ISSUE} route={route} />
      <StageSlot filled={SLOT_FILLED} issue={ISSUE} route={route} views={STAND_INS} />
      <Where />
    </ProvidersSlot>
  );
}

let restoreViewport: () => void = () => {};
let restoreReads: { mockRestore(): void }[] = [];

beforeEach(() => {
  restoreReads = [
    ...stubReads(),
    spyOn(api, "getArtifactText").mockResolvedValue({
      markdown: "The notes",
      token: "sha256:notes",
      version: null,
    }),
  ];
  // What an opened document reads beside its text: its record, its blocks, the issue's
  // decisions and the reader's read mark, and what references it.
  answerRequests(
    artifactRecordHandler((id) => (ISSUE.artifacts ?? []).find((each) => each.id === id)),
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
  restoreViewport();
  restoreViewport = () => {};
  for (const read of restoreReads) read.mockRestore();
});

/** The page at `address`, with `state` in its location where one is given, at `width` px: 1440,
 *  where the row is the row of five with its small controls at its end, unless given. */
function drawPage(
  address: string,
  { state, width = DESKTOP_WIDTH }: { state?: unknown; width?: number } = {}
): RenderResult {
  // A case that draws the page twice stubs the width once at a time.
  restoreViewport();
  restoreViewport = stubViewport(width);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const [pathname, search = ""] = address.split("?");
  return render(
    <MemoryRouter initialEntries={[{ pathname, search: search === "" ? "" : `?${search}`, state }]}>
      <AppProviders client={client}>
        <DocumentRuntime.Provider value={fakeDocumentRuntime({ text: "The notes" }).runtime}>
          <Page />
        </DocumentRuntime.Provider>
      </AppProviders>
    </MemoryRouter>
  );
}

/** Where the router is, as `Where` draws it. */
function where(): HTMLElement {
  return screen.getByTestId("where");
}

/** The following the page is at. */
function following(): string {
  return where().dataset.following ?? "";
}

/** The words box, open. */
function box(): HTMLElement {
  return screen.getByRole("searchbox", { name: "Filter" });
}

/** Opens the box from its control, as a press does, and types `words` into it. */
async function typeWords(user: UserEvent, words: string): Promise<void> {
  await user.click(screen.getByRole("button", { name: "Filter" }));
  await user.type(box(), words);
  await waitFor(() => expect(where()).toHaveTextContent(`?q=${words}`));
}

test("shut, a press on Filter opens the box with the caret in it and writes no address", async () => {
  const user = userEvent.setup();
  const view = drawPage("/issues/CORE-1/spec");
  try {
    expect(screen.queryByRole("searchbox", { name: "Filter" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Filter" }));
    await waitFor(() => expect(box()).toHaveFocus());
    expect(screen.queryByRole("button", { name: "Filter" })).toBeNull();
    expect(where()).toHaveTextContent("/issues/CORE-1/spec");
    expect(where()).toHaveAttribute("data-move", "POP");
  } finally {
    view.unmount();
  }
});

test("shut, t f opens the box with the caret in it, and at 760 px and below the Outline sheet opens with the box in it", async () => {
  const user = userEvent.setup();
  const view = drawPage("/issues/CORE-1/spec", { width: PHONE_WIDTH });
  try {
    // The row draws the one control and Outline, and no words control of its own.
    await screen.findByRole("button", { name: "Outline" });
    expect(screen.queryByRole("button", { name: "Filter" })).toBeNull();
    await user.keyboard("tf");
    const sheet = await screen.findByRole("dialog");
    await waitFor(() =>
      expect(within(sheet).getByRole("searchbox", { name: "Filter" })).toHaveFocus()
    );
    expect(where()).toHaveTextContent("/issues/CORE-1/spec");
    expect(where()).toHaveAttribute("data-move", "POP");
  } finally {
    view.unmount();
  }
});

test("open, empty and focused, typing a writes ?q=a by a replace marked as the box's own, in one following", async () => {
  const user = userEvent.setup();
  const view = drawPage("/issues/CORE-1/spec");
  try {
    const before = following();
    await user.click(screen.getByRole("button", { name: "Filter" }));
    await user.type(box(), "a");
    await waitFor(() => expect(where()).toHaveTextContent("/issues/CORE-1/spec?q=a"));
    expect(where()).toHaveAttribute("data-move", "REPLACE");
    expect(JSON.parse(where().dataset.state ?? "null")).toMatchObject({ words: before });
    expect(following()).toBe(before);
  } finally {
    view.unmount();
  }
});

test("open with words and focused, typing more replaces each time in the following the box began in, and the view stays drawn", async () => {
  const user = userEvent.setup();
  const view = drawPage("/issues/CORE-1/spec");
  try {
    const before = following();
    const specView = () => view.container.querySelector<HTMLElement>('[data-stand-in-view="spec"]');
    await waitFor(() => expect(specView()).not.toBeNull());
    const drawing = specView()?.dataset.drawing;
    await typeWords(user, "a");
    for (const words of ["ab", "abc", "abcd"]) {
      await user.type(box(), words.slice(-1));
      await waitFor(() => expect(where()).toHaveTextContent(`/issues/CORE-1/spec?q=${words}`));
      expect(where()).toHaveAttribute("data-move", "REPLACE");
      expect(following(), `typing ${words} began a new following`).toBe(before);
      expect(specView()?.dataset.drawing, `typing ${words} drew the view anew`).toBe(drawing);
      expect(specView()).toHaveAttribute("data-words", words);
    }
  } finally {
    view.unmount();
  }
});

test("open with words and focused, deleting every word removes ?q= by a replace and the box stays open while it holds the caret", async () => {
  const user = userEvent.setup();
  const view = drawPage("/issues/CORE-1/spec");
  try {
    const before = following();
    await typeWords(user, "ab");
    await user.clear(box());
    await waitFor(() => expect(where().textContent).toBe("/issues/CORE-1/spec"));
    expect(where()).toHaveAttribute("data-move", "REPLACE");
    expect(following()).toBe(before);
    expect(box()).toHaveFocus();
    expect(box()).toHaveValue("");
  } finally {
    view.unmount();
  }
  // A box the address opened with its words, which the reader then puts the caret in, as well.
  const linked = drawPage("/issues/CORE-1/spec?q=ab");
  try {
    const before = following();
    await user.click(box());
    await user.clear(box());
    await waitFor(() => expect(where().textContent).toBe("/issues/CORE-1/spec"));
    expect(following()).toBe(before);
    expect(box()).toHaveFocus();
  } finally {
    linked.unmount();
  }
});

test("open, empty and focused, a blur shuts the box into its control", async () => {
  const user = userEvent.setup();
  const view = drawPage("/issues/CORE-1/spec");
  try {
    await user.click(screen.getByRole("button", { name: "Filter" }));
    await waitFor(() => expect(box()).toHaveFocus());
    await user.tab();
    await waitFor(() => expect(screen.queryByRole("searchbox", { name: "Filter" })).toBeNull());
    expect(screen.getByRole("button", { name: "Filter" })).toBeVisible();
    // Emptied with the caret in it, and then left, it shuts too.
    await typeWords(user, "ab");
    await user.clear(box());
    await user.click(where());
    await waitFor(() => expect(screen.queryByRole("searchbox", { name: "Filter" })).toBeNull());
    expect(screen.getByRole("button", { name: "Filter" })).toBeVisible();
  } finally {
    view.unmount();
  }
});

test("open with words, a blur leaves the box open with its words", async () => {
  const user = userEvent.setup();
  const view = drawPage("/issues/CORE-1/spec");
  try {
    await typeWords(user, "ab");
    await user.tab();
    expect(box()).not.toHaveFocus();
    expect(box()).toHaveValue("ab");
    expect(screen.queryByRole("button", { name: "Filter" })).toBeNull();
    expect(where()).toHaveTextContent("/issues/CORE-1/spec?q=ab");
  } finally {
    view.unmount();
  }
});

test("any state, a link or Back to an address whose ?q= differs starts a new following and the box shows that address's words", async () => {
  const user = userEvent.setup();
  const view = drawPage("/issues/CORE-1/spec");
  try {
    await typeWords(user, "ab");
    const typed = following();
    await user.click(screen.getByRole("link", { name: "Other words" }));
    await waitFor(() => expect(where()).toHaveTextContent("/issues/CORE-1/spec?q=other"));
    const linked = following();
    expect(linked).not.toBe(typed);
    expect(box()).toHaveValue("other");
    expect(view.container.querySelector('[data-stand-in-view="spec"]')).toHaveAttribute(
      "data-words",
      "other"
    );

    await user.click(screen.getByRole("button", { name: "Back" }));
    await waitFor(() => expect(where()).toHaveTextContent("/issues/CORE-1/spec?q=ab"));
    expect(following()).not.toBe(linked);
    expect(box()).toHaveValue("ab");
  } finally {
    view.unmount();
  }
});

test("open with words, a press on a view draws the view with the words and the box keeps them", async () => {
  const user = userEvent.setup();
  const view = drawPage("/issues/CORE-1/spec");
  try {
    await typeWords(user, "ab");
    await user.click(screen.getByRole("button", { name: "Plan" }));
    await waitFor(() => expect(where()).toHaveTextContent("/issues/CORE-1/plan?q=ab"));
    await waitFor(() =>
      expect(view.container.querySelector('[data-stand-in-view="plan"]')).toHaveAttribute(
        "data-words",
        "ab"
      )
    );
    expect(box()).toHaveValue("ab");
    // And on to another view, the words still with the reader.
    await user.click(screen.getByRole("button", { name: "Verification" }));
    await waitFor(() => expect(where()).toHaveTextContent("/issues/CORE-1/verification?q=ab"));
    expect(view.container.querySelector('[data-stand-in-view="verification"]')).toHaveAttribute(
      "data-words",
      "ab"
    );
    expect(box()).toHaveValue("ab");
  } finally {
    view.unmount();
  }
});

test("open with words, opening a document draws the document and keeps the box and ?q= for the return", async () => {
  const user = userEvent.setup();
  const view = drawPage("/issues/CORE-1/spec");
  try {
    await typeWords(user, "ab");
    await user.click(screen.getByRole("button", { name: "Files and documents" }));
    await user.click(await screen.findByRole("button", { name: "notes-md" }));
    await waitFor(() => expect(where()).toHaveTextContent("/issues/CORE-1/artifacts/notes-md"));
    expect(await screen.findByRole("heading", { name: "notes-md" })).toBeVisible();

    await user.click(screen.getByRole("button", { name: "Back to the conversation" }));
    await waitFor(() => expect(where()).toHaveTextContent("/issues/CORE-1/spec?q=ab"));
    expect(box()).toHaveValue("ab");
    expect(view.container.querySelector('[data-stand-in-view="spec"]')).toHaveAttribute(
      "data-words",
      "ab"
    );
  } finally {
    view.unmount();
  }
});

// The location's state table: a press of the count of what waits on the reader acts once, so
// the box's write leaves it out, and keeps the outline an opened document goes back to.
test("a words write drops a waiting press and keeps the outline address", async () => {
  const user = userEvent.setup();
  const view = drawPage("/issues/CORE-1/decisions", {
    state: { outline: "/issues/CORE-1/plan", waitingPress: 1 },
  });
  try {
    const before = following();
    await typeWords(user, "ab");
    expect(JSON.parse(where().dataset.state ?? "null")).toEqual({
      outline: "/issues/CORE-1/plan",
      words: before,
    });
  } finally {
    view.unmount();
  }
});
