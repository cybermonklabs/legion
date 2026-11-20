import { expect, type Mock, setSystemTime, spyOn, test } from "bun:test";
import { MAX_ISSUE_ADVANCES, MAX_ISSUE_BLOCKERS } from "@legion/contracts";
import { focusManager, QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import {
  act,
  // biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for an Escape that ends an IME composition, which user-event has no form for
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { graphNode } from "../../__tests__/graph-fixture";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import { eventRefreshTargets } from "../../api/event-refresh";
import { primarySpec } from "../../api/issue-cache";
import { issueQuery, userStateQuery } from "../../api/queries";
import { refreshQueries } from "../../api/query-refresh";
import type {
  ArchitectureSource,
  CommentWithTrees,
  DispatchEvent,
  DisplayTree,
  GraphReferences,
  Issue,
  IssueDetails,
  Priorities,
  ReferenceTargets,
  RepoProject,
  UserState,
} from "../../api/types";
import { linkText, pageFailedText, pageHeaderControl, pageInkText } from "../../theme/classes";
import { assigneeControlName } from "../issue/AssigneeControl";
import { componentsControlName } from "../issue/IssueComponentsLine";
import { labelsControlName } from "../issue/IssueLabels";
import { conversationItemId, pinnedItemMarker, stateForIssue } from "../issue/pins";
import { repoControlName, repoTextControlName } from "../issue/RepoControl";
import { type DocumentBridge, MarginProvider, useMargin } from "../margin/margin-context";
import { issue as marginIssue, specArtifact } from "../margin/margin-fixture";
import { addPriorityControlName } from "../priorities/IssuePriorityLine";
import { buildDispatchReference } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { appKeymap, useKeymapScope } from "../shell/keymap";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { blockedByControlName } from "./BlockedByControl";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { DetailsFocusedNode, DetailsHistory, DetailsPeople } from "./details-more";
import { DETAILS_ROW_ATTRIBUTE, DETAILS_ROW_REQUEST_EVENT, type DetailsRow } from "./details-rows";
import type { IssueField } from "./IssueFieldForms";
import { IssueMarginPanel, PARENT_ERROR_ID } from "./IssueMargin";
import { narrowIssue, patchAnswer } from "./issue-fixtures";
import {
  DETAILS_ATTRIBUTE,
  FilledMarginSlot,
  type MarginDrawer,
  MarginSlotProvider,
  useMarginSlotFill,
} from "./margin-slot";
import { NoTooltips } from "./NoTooltips";
import { MARGIN_SECTION_NAMES, PROPERTY_LABELS } from "./page-structure";
import {
  ANSWER_LINES,
  BLOCKED_BY_REFUSALS,
  CONTROL_LABELS,
  issueStatusWord,
  NODE_TYPE_NAMES,
} from "./page-words";
import { architecture, offPageColours } from "./shared-controls-fixture";
import { DetailsSlot } from "./slots/details";
import { firstUsableIn, usableControl } from "./usable-control";
import { blockedByRefusal } from "./useIssueBlockedBy";
import { type IssueHeaderWrites, useIssueHeaderWrites } from "./useIssueHeaderWrites";
import { appClient, focusPastStaleness } from "./window-focus-fixture";

const issue: IssueDetails = { ...marginIssue, last_seq: 250, status: "todo" };

/** The fixture's issue, closed. */
const closedIssue: IssueDetails = { ...issue, closed_at: "2026-09-10T00:00:00Z", status: "done" };

/** An open Details drawer, whose `setOpen` and `setPage` do nothing. */
const OPEN_DRAWER: MarginDrawer = {
  open: true,
  setOpen: () => undefined,
  setPage: () => undefined,
};

/** The page's words the labels control, the components picker and the Repository row's text
 *  control are named from inside `NoTooltips` (`labelsControlName`, `componentsControlName`,
 *  `repoTextControlName`). */
const CONTROL_NAME_WORDS = { ANSWER_LINES, CONTROL_LABELS };

/** Details as the details slot draws it (`slots/details.tsx`), alone and inside `NoTooltips`:
 *  the panel over its own set of the issue's writes, on the cached issue, so a write's landing
 *  is what the panel draws next. `beside` is drawn above it, over writes of its own. `drawer` is
 *  the Details drawer it is drawn in; with none, Details makes none of the reads it makes only
 *  while open. `redraw` draws the same Details in the drawer it is handed, as a drawer that
 *  opens or closes does. `queryClient` is the client every read goes through: by default one
 *  whose reads are never stale, so none is read again by its age or a window focus. */
function renderDetails(
  beside?: (issue: IssueDetails) => ReactNode,
  drawer?: MarginDrawer,
  queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  })
) {
  const tree = (inDrawer: MarginDrawer | undefined) => (
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarginSlotProvider drawer={inDrawer}>
            <NoTooltips>
              <CachedIssue>
                {(details, state) => (
                  <>
                    {beside?.(details)}
                    <Details issue={details} state={state} />
                  </>
                )}
              </CachedIssue>
            </NoTooltips>
          </MarginSlotProvider>
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(drawer));
  return {
    queryClient,
    redraw: (inDrawer: MarginDrawer) => view.rerender(tree(inDrawer)),
    unmount: view.unmount,
  };
}

function CachedIssue({
  children,
}: {
  children: (issue: IssueDetails, state: UserState | undefined) => ReactNode;
}): ReactNode {
  const details = useQuery(issueQuery("CORE-1")).data;
  const state = useQuery(userStateQuery()).data;
  return details === undefined ? null : children(details, state);
}

function Details({ issue, state }: { issue: IssueDetails; state: UserState | undefined }) {
  const writes = useIssueHeaderWrites(issue, stateForIssue(state, issue.key));
  const [editingField, setEditingField] = useState<IssueField | null>(null);
  return (
    <IssueMarginPanel
      documentArtifact={primarySpec(issue)}
      editingField={editingField}
      isClosed={issue.closed_at !== null}
      issue={issue}
      onEditingField={setEditingField}
      state={state}
      writes={writes}
    />
  );
}

/** The reads Details makes of a fixture's issue, each answering the API's empty shape, and the
 *  issue's PATCH answered as the server does, with the narrow `Issue`. A read that reached the
 *  network would fail the file (`__tests__/network-guard.ts`). */
function stubIssueApi() {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({
    "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 },
  });
  const reads = [
    spyOn(api, "whoAmI").mockResolvedValue({ login: "alice" } as Awaited<
      ReturnType<typeof api.whoAmI>
    >),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue([]),
    // The repository's control reads the project's repository mappings at once for an issue
    // that resolves to none, as this one does; a project that maps none suggests no repository.
    spyOn(api, "listRepoProjects").mockResolvedValue([]),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] }),
  ];
  const pinReads = {
    asks: spyOn(api, "listIssueAsks").mockResolvedValue([]),
    comments: spyOn(api, "listComments").mockResolvedValue([]),
    messages: spyOn(api, "listIssueMessages").mockResolvedValue([]),
  };
  const patchIssue = answerIssuePatches().mockImplementation(async (_key, input) =>
    patchAnswer(issue, input)
  );

  return {
    getIssue,
    getMyState,
    patchIssue,
    pinReads,
    restore: () => {
      getIssue.mockRestore();
      getMyState.mockRestore();
      for (const spy of reads) spy.mockRestore();
      for (const spy of Object.values(pinReads)) spy.mockRestore();
      patchIssue.mockRestore();
    },
  };
}

/** Draws `details` as the issue Dispatch holds now: the cached issue Details draws, and the
 *  answer to the issue's next read. */
function drawIssue(
  getIssue: Mock<typeof api.getIssue>,
  queryClient: QueryClient,
  details: IssueDetails
): void {
  getIssue.mockResolvedValue(details);
  act(() => {
    queryClient.setQueryData(issueQuery("CORE-1").queryKey, details);
  });
}

/** One row of Details, by the name in its label column (`IssueMargin.tsx`'s `Row`). */
function detailsRow(name: string): HTMLElement {
  const row = screen
    .getByText(name, { exact: true, selector: "[data-page-chrome]" })
    .closest(".grid");
  if (!(row instanceof HTMLElement)) throw new Error(`Details draws no ${name} row`);
  return row;
}

/** The row of Details a control elsewhere on the page opens it at (`details-rows.ts`). */
function rowAt(row: DetailsRow): HTMLElement {
  const found = document.querySelector(`[${DETAILS_ROW_ATTRIBUTE}="${row}"]`);
  if (!(found instanceof HTMLElement)) throw new Error(`Details draws no ${row} row`);
  return found;
}

/** How many times `getReferences` read what waits on `reference`, the Blocks row's read: the
 *  references of kind `blocked_by` that point at it. */
function blocksReads(getReferences: Mock<typeof api.getReferences>, reference: string): number {
  return getReferences.mock.calls.filter(
    ([to, options]) => to === reference && options?.kind === "blocked_by"
  ).length;
}

/** Every Escape that reaches the window from now on, by whether a control marked it taken: the
 *  page's keys pass over a taken press (`shell/keymap.ts`), and the phone's Details sheet closes
 *  on an Escape no control took (`shell/useDialog.ts`). */
function watchEscapes(): { readonly stop: () => void; readonly taken: boolean[] } {
  const taken: boolean[] = [];
  const record = (event: KeyboardEvent) => {
    if (event.key === "Escape") taken.push(event.defaultPrevented);
  };
  window.addEventListener("keydown", record);
  return { stop: () => window.removeEventListener("keydown", record), taken };
}

test("the blocked-by editor stays open on a refusal and says what Dispatch refused", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(
      new ApiError(400, { code: "BLOCKED_BY_INPUT", error: "blocked_by issue CORE-9 not found" })
    )
  );
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    const keys = (await screen.findByLabelText("Issues CORE-1 waits on")) as HTMLInputElement;
    await user.click(keys);
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith(
        "CORE-1",
        { blocked_by: ["CORE-9"] },
        keyedAttempt
      )
    );
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("There is no issue CORE-9.");
    expect(alert).not.toHaveTextContent("blocked_by");
    // The key is the reader's own text inside the page's sentence (c-plain-words).
    expect(within(alert).getByText("CORE-9")).toHaveAttribute("data-author-text");
    expect(
      (screen.getByLabelText("Issues CORE-1 waits on") as HTMLInputElement).value,
      "the refused list is no longer in the box for the reader to mend"
    ).toBe("CORE-9");
    expect(screen.queryByText("Could not update this issue.")).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

test("a blocked-by that is set can be changed and cleared from the panel", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-2"] });
  const { unmount } = renderDetails();

  try {
    expect(await screen.findByRole("link", { name: "CORE-2" })).toBeInTheDocument();
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 1) })
    );
    const keys = (await screen.findByLabelText("Issues CORE-1 waits on")) as HTMLInputElement;
    expect(keys.value).toBe("CORE-2");

    await user.clear(keys);
    await user.paste("core-3");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith(
        "CORE-1",
        { blocked_by: ["CORE-3"] },
        keyedAttempt
      )
    );
    // The editor closes on a landing, and the written list is what the panel draws.
    await waitFor(() =>
      expect(screen.queryByLabelText("Issues CORE-1 waits on")).not.toBeInTheDocument()
    );
    expect(await screen.findByRole("link", { name: "CORE-3" })).toBeInTheDocument();

    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 1) })
    );
    await user.clear(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { blocked_by: [] }, keyedAttempt)
    );
  } finally {
    unmount();
    restore();
  }
});

// The refusal is drawn in the editor and nowhere else, so a reader who closed the editor while
// the save was out was shown no refusal at all. Cancel and Escape wait for the answer, and
// Cancel says so: it is drawn and announced as held for as long as the save is out, rather than
// as a control whose press does nothing. An Escape held so is still the editor's, in the box or
// on Cancel, and goes no further.
test("the blocked-by editor refuses Cancel and Escape while the save is out", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const saving = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const escapes = watchEscapes();
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    const keys = (await screen.findByLabelText("Issues CORE-1 waits on")) as HTMLInputElement;
    await user.click(keys);
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));

    const cancel = screen.getByRole("button", { name: CONTROL_LABELS.cancel });
    expect(
      cancel,
      "Cancel is drawn as a control while the save it would leave is out"
    ).toHaveAttribute("aria-disabled", "true");
    await user.click(cancel);
    expect(
      screen.getByLabelText("Issues CORE-1 waits on"),
      "Cancel closed the editor over a save whose refusal it would have drawn"
    ).toBeInTheDocument();
    await user.click(keys);
    await user.keyboard("{Escape}");
    expect(
      screen.getByLabelText("Issues CORE-1 waits on"),
      "Escape closed the editor over a save whose refusal it would have drawn"
    ).toBeInTheDocument();
    act(() => cancel.focus());
    await user.keyboard("{Escape}");
    expect(
      screen.getByLabelText("Issues CORE-1 waits on"),
      "Escape on Cancel closed the editor over a save whose refusal it would have drawn"
    ).toBeInTheDocument();
    expect(
      escapes.taken,
      "an Escape the editor held while its save is out went on to the page's keys"
    ).toEqual([true, true]);

    // The answer closes it, which is the one thing that may.
    saving.resolve(narrowIssue({ ...issue, blocked_by: ["CORE-9"] }));
    await waitFor(() =>
      expect(screen.queryByLabelText("Issues CORE-1 waits on")).not.toBeInTheDocument()
    );
  } finally {
    escapes.stop();
    unmount();
    restore();
  }
});

// A refusal belongs to the attempt that earned it. The editor held the last one until the next
// save started, so opening it again drew a refusal of a list the reader had already dropped.
test("the blocked-by editor opened again draws no refusal from the attempt before", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(
      new ApiError(400, { code: "BLOCKED_BY_INPUT", error: "blocked_by issue CORE-9 not found" })
    )
  );
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    await user.click(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    expect(await screen.findByRole("alert")).toHaveTextContent("There is no issue CORE-9.");
    // The answer gives Cancel back: the editor is the reader's to leave again. A refused Cancel
    // is `aria-disabled` rather than `disabled`, so both are read.
    expect(screen.getByRole("button", { name: CONTROL_LABELS.cancel })).toBeEnabled();
    expect(screen.getByRole("button", { name: CONTROL_LABELS.cancel })).not.toHaveAttribute(
      "aria-disabled",
      "true"
    );

    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.cancel }));
    await waitFor(() =>
      expect(screen.queryByLabelText("Issues CORE-1 waits on")).not.toBeInTheDocument()
    );
    await user.click(screen.getByRole("button", { name: blockedByControlName("CORE-1", 0) }));
    expect(await screen.findByLabelText("Issues CORE-1 waits on")).toBeInTheDocument();
    expect(
      screen.queryByRole("alert"),
      "the editor draws the refusal of a save this reader never made"
    ).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// A closed section hides its rows, and the blocked-by editor's state is the panel's, which stays
// drawn for the life of the page: a reader who closes Development while the save is out finds
// the editor, the list they typed and the refusal that landed meanwhile when they open it again.
test("closing Development keeps the blocked-by editor, its typed list and a refusal that lands after the close", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const saving = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const { queryClient, unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    await user.click(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.paste("CORE-2 CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith(
        "CORE-1",
        { blocked_by: ["CORE-2", "CORE-9"] },
        keyedAttempt
      )
    );

    const development = screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development });
    await user.click(development);
    expect(
      screen.queryByRole("textbox", { name: "Issues CORE-1 waits on" }),
      "a closed Development still shows the blocked-by editor"
    ).toBeNull();
    saving.reject(
      new ApiError(400, { code: "BLOCKED_BY_INPUT", error: "blocked_by issue CORE-9 not found" })
    );
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    await user.click(development);
    expect(
      screen.queryAllByLabelText("Issues CORE-1 waits on"),
      "closing Development threw away the open blocked-by editor"
    ).toHaveLength(1);
    expect(
      screen.getByLabelText<HTMLInputElement>("Issues CORE-1 waits on").value,
      "closing Development threw away the list typed into the editor"
    ).toBe("CORE-2 CORE-9");
    expect(
      screen.queryByRole("alert")?.textContent,
      "the refusal that landed while Development was closed is drawn nowhere"
    ).toBe("There is no issue CORE-9.");
  } finally {
    unmount();
    restore();
    window.localStorage.removeItem(userPreferenceStorageKey("alice", "issue.margin-sections"));
  }
});

// A closed section unmounted its rows, so what a row of a shared control holds of its own was
// thrown away as the reader closed Development: here the repository's box, with a name it refused
// as the reader left it for the section's heading, which the row alone says. The section hides
// its rows instead: the reader sees none of them while it is closed, and finds the box as they
// left it, the refused name still in it, when it opens again.
test("closing Development hides its rows and keeps the repository box's refused draft for its next open", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();
  const box = () => screen.queryByRole("combobox", { name: repoControlName("CORE-1") });

  try {
    await user.click(
      await screen.findByRole("button", {
        name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
      })
    );
    const typed = await screen.findByRole("combobox", { name: repoControlName("CORE-1") });
    await user.click(typed);
    await user.paste("acme");

    const development = screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development });
    await user.click(development);
    expect(development).toHaveAttribute("aria-expanded", "false");
    expect(box(), "a closed Development still shows the repository's box").toBeNull();
    expect(
      screen.queryByRole("button", { name: blockedByControlName("CORE-1", 0) }),
      "a closed Development still shows the Blocked by row's control"
    ).toBeNull();

    await user.click(development);
    expect(
      box(),
      "closing Development threw away the repository's box and its draft"
    ).not.toBeNull();
    expect(box(), "closing Development threw away the name typed into the box").toHaveValue("acme");
    expect(
      box(),
      "closing Development threw away the box's refusal of the name typed into it"
    ).toHaveAttribute("aria-invalid", "true");
  } finally {
    unmount();
    restore();
    window.localStorage.removeItem(userPreferenceStorageKey("alice", "issue.margin-sections"));
  }
});

// A closed section keeps its rows on the page, hidden (`PropertySection`), so a Details opened
// with Development closed read what that section's rows draw, for rows its reader could not see:
// GitHub for each link, which reads it as it is drawn (`GitHubLink`), the titles of the issues
// this one waits on, and what waits on it. Each is read once Development opens.
test("Details open with Development closed reads no GitHub link, title or Blocks, and opening Development reads each", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({
    ...issue,
    blocked_by: ["CORE-3"],
    external_links: [
      { kind: "github_pr", url: "https://github.com/acme/web/pull/7" },
      { url: "https://github.com/acme/web/issues/8" },
    ],
  });
  // A reference with no head, so a pull request's read reads no checks after it.
  const githubRest = spyOn(api, "githubRest").mockImplementation(
    async () =>
      new Response(JSON.stringify({ state: "open", title: "Ship the web app" }), {
        headers: { "Content-Type": "application/json" },
      })
  );
  // The titles of the issues this one waits on, and what waits on it.
  const blocker = buildDispatchReference({ key: "CORE-3", kind: "issue" });
  const titles = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] });
  const titleReads = () => titles.mock.calls.filter(([refs]) => refs.includes(blocker)).length;
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const kept = userPreferenceStorageKey("alice", "issue.margin-sections");
  window.localStorage.setItem(kept, "development");
  const shut: MarginDrawer = { ...OPEN_DRAWER, open: false };
  const { redraw, unmount } = renderDetails(undefined, shut);

  try {
    const development = await screen.findByRole("button", {
      name: MARGIN_SECTION_NAMES.development,
    });
    await waitFor(() => expect(development).toHaveAttribute("aria-expanded", "false"));
    redraw(OPEN_DRAWER);
    // The open drawer's reads start in the commit that opens it: its effects run here.
    await act(async () => {});
    expect(
      githubRest,
      "Details read GitHub for the links of a closed Development"
    ).not.toHaveBeenCalled();
    expect(
      titleReads(),
      "Details read the titles of the issues a closed Development's Blocked by waits on"
    ).toBe(0);
    expect(
      blocksReads(getReferences, reference),
      "Details read what waits on the issue for a closed Development"
    ).toBe(0);

    await user.click(development);
    await waitFor(() =>
      expect(
        githubRest.mock.calls.map(([path]) => path).sort(),
        "opening Development did not read each of its links"
      ).toEqual(["repos/acme/web/issues/8", "repos/acme/web/pulls/7"])
    );
    await waitFor(() => {
      expect(
        titleReads(),
        "opening Development did not read the titles of the issues it waits on"
      ).toBeGreaterThan(0);
      expect(
        blocksReads(getReferences, reference),
        "opening Development did not read what waits on the issue"
      ).toBeGreaterThan(0);
    });
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    githubRest.mockRestore();
    restore();
    window.localStorage.removeItem(kept);
  }
});

// Escape in the blocked-by box is the editor's: it closes it and is taken there, so the phone's
// Details sheet, which closes on an Escape no control took (`useDialog.ts`), stays open.
test("Escape in the blocked-by box closes its editor and goes no further", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const escapes = watchEscapes();
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    const keys = await screen.findByLabelText("Issues CORE-1 waits on");
    await waitFor(() => expect(keys).toHaveFocus());
    await user.keyboard("{Escape}");
    expect(
      screen.queryByLabelText("Issues CORE-1 waits on"),
      "Escape left the blocked-by editor open"
    ).not.toBeInTheDocument();
    expect(
      escapes.taken,
      "Escape in the blocked-by box went on to the page's keys, which close Details on a phone"
    ).toEqual([true]);
  } finally {
    escapes.stop();
    unmount();
    restore();
  }
});

// An Escape that ends an IME composition gives up what the reader was composing, and the
// blocked-by editor closed on it, with the list typed into it. The editor leaves that press alone,
// unmarked, as the page's other editors do, and a plain Escape still closes it.
test("an Escape that ends an IME composition leaves the blocked-by editor open with its list", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const escapes = watchEscapes();
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    const keys = await screen.findByLabelText<HTMLInputElement>("Issues CORE-1 waits on");
    await user.click(keys);
    await user.paste("CORE-2");
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(keys, { isComposing: true, key: "Escape" });
    expect(
      screen.queryByLabelText("Issues CORE-1 waits on"),
      "an Escape that ended an IME composition closed the blocked-by editor"
    ).toBe(keys);
    expect(keys.value, "an Escape that ended an IME composition threw the typed list away").toBe(
      "CORE-2"
    );

    await user.keyboard("{Escape}");
    expect(
      screen.queryByLabelText("Issues CORE-1 waits on"),
      "Escape left the blocked-by editor open"
    ).not.toBeInTheDocument();
    expect(
      escapes.taken,
      "the editor marked the Escape that ended a composition taken, or left a plain one unmarked"
    ).toEqual([false, true]);
  } finally {
    escapes.stop();
    unmount();
    restore();
  }
});

/** Opens an editor of a row by the row's control named `control`, then presses Escape on the
 *  editor's Save (named `save`) and, the editor opened again, on its Cancel. Each press is the
 *  editor's, as Escape in its box is: the editor (its box named `box`) is closed, the row's
 *  control holds the focus again, and the press reached the window, where the page's own Escape
 *  that closes Details and the phone's Details sheet listens (`shell/keymap.ts`, `useDialog.ts`),
 *  only marked taken, which that Escape passes over, or not at all, where the editor stopped it. */
async function expectEscapeLeavesEditor(
  user: UserEvent,
  { box, control, save }: { box: string; control: string; save: string }
): Promise<void> {
  const escapes = watchEscapes();
  try {
    for (const button of [save, CONTROL_LABELS.cancel]) {
      await user.click(await screen.findByRole("button", { name: control }));
      await waitFor(() => expect(screen.getByLabelText(box)).toHaveFocus());
      const pressed = screen.getByRole("button", { name: button });
      act(() => pressed.focus());
      const before = escapes.taken.length;
      await user.keyboard("{Escape}");
      expect(
        screen.queryByLabelText(box),
        `Escape on the editor's ${button} left the editor open`
      ).not.toBeInTheDocument();
      expect(
        document.activeElement,
        `Escape on the editor's ${button} did not give the focus back to the row's control`
      ).toBe(screen.getByRole("button", { name: control }));
      expect(
        escapes.taken.slice(before).every(Boolean),
        `Escape on the editor's ${button} went on to the page's keys, which close Details`
      ).toBe(true);
    }
  } finally {
    escapes.stop();
  }
}

// Escape in an editor's box closed that editor alone, but no control took an Escape on its Save
// or its Cancel, so the page's own Escape closed the whole of Details - the phone's Details sheet
// with it - and the focus fell to the page's body. Escape anywhere in an editor is the editor's.
test("Escape on the parent's Save or Cancel closes its editor alone, with the focus back on Set parent issue", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    await expectEscapeLeavesEditor(user, {
      box: PROPERTY_LABELS.parent,
      control: CONTROL_LABELS.set_parent,
      save: CONTROL_LABELS.save_parent,
    });
  } finally {
    unmount();
    restore();
  }
});

test("Escape on the route's Save or Cancel closes its editor alone, with the focus back on the route's control", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    await expectEscapeLeavesEditor(user, {
      box: PROPERTY_LABELS.route,
      control: "Messages default to no route",
      save: CONTROL_LABELS.save_route,
    });
  } finally {
    unmount();
    restore();
  }
});

test("Escape on the blocked-by Save or Cancel closes its editor alone, with the focus back on its Add", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    await expectEscapeLeavesEditor(user, {
      box: "Issues CORE-1 waits on",
      control: blockedByControlName("CORE-1", 0),
      save: CONTROL_LABELS.save_blocked_by,
    });
  } finally {
    unmount();
    restore();
  }
});

// The box goes with the editor, so leaving it - by Cancel or by a save that lands - gives the
// focus back to the control that opened it, not to the page.
test("leaving the blocked-by editor gives the focus back to its Add or Edit", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.cancel }));
    expect(
      document.activeElement,
      "Cancel in the blocked-by editor left the focus on the page"
    ).toBe(screen.getByRole("button", { name: blockedByControlName("CORE-1", 0) }));

    await user.click(screen.getByRole("button", { name: blockedByControlName("CORE-1", 0) }));
    await user.click(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.paste("CORE-2");
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(
        document.activeElement,
        "a save of the blocked-by list that landed left the focus on the page"
      ).toBe(screen.queryByRole("button", { name: blockedByControlName("CORE-1", 1) }))
    );
  } finally {
    unmount();
    restore();
  }
});

// On a closed issue the Blocked by row draws no control, so leaving its editor once the issue had
// closed under it gave the focus to no control that takes it, and it stayed on the page's body.
// It goes to the first issue the row waits on, and where the row waits on none, to Development's
// heading, the button that opens and closes the section the row is in.
test("on a closed issue, leaving the blocked-by editor hands the focus to the first issue it waits on, else to Development's heading", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    await screen.findByLabelText("Issues CORE-1 waits on");
    drawIssue(getIssue, queryClient, closedIssue);
    await user.keyboard("{Escape}");
    expect(
      document.activeElement,
      "Escape in the blocked-by editor of a closed issue that waits on none did not give the focus to Development's heading"
    ).toBe(screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development }));

    drawIssue(getIssue, queryClient, { ...issue, blocked_by: ["CORE-3"] });
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 1) })
    );
    await screen.findByLabelText("Issues CORE-1 waits on");
    drawIssue(getIssue, queryClient, { ...closedIssue, blocked_by: ["CORE-3"] });
    await user.keyboard("{Escape}");
    expect(
      document.activeElement,
      "Escape in the blocked-by editor of a closed issue did not give the focus to the first issue it waits on"
    ).toBe(within(detailsRow(PROPERTY_LABELS.blocked_by)).getByRole("link", { name: "CORE-3" }));
  } finally {
    unmount();
    restore();
  }
});

// Each refusal of a blocked-by list is a sentence in the page's words, with the key the refusal
// is about inside it as the reader's own text: a key Dispatch knows no issue for, the issue
// itself, and a list longer than an issue waits on.
test("each refusal of a blocked-by list is the page's sentence about the key it names", () => {
  const refused = (code: string, error: string) => new ApiError(400, { code, error });
  expect(
    blockedByRefusal(refused("BLOCKED_BY_INPUT", "blocked_by issue CORE-9 not found"), "CORE-1", [
      "CORE-2",
      "CORE-9",
    ])
  ).toEqual({ key: "CORE-9", words: "not_found" });
  expect(
    blockedByRefusal(refused("BLOCKED_BY_INPUT", "CORE-1 cannot be blocked by itself"), "CORE-1", [
      "CORE-1",
    ])
  ).toEqual({ key: "CORE-1", words: "itself" });
  const many = Array.from({ length: MAX_ISSUE_BLOCKERS + 1 }, (_, index) => `CORE-${index + 2}`);
  expect(
    blockedByRefusal(
      refused("BLOCKED_BY_INPUT", "blocked_by is 1 over the 20-item limit (21/20)"),
      "CORE-1",
      many
    )
  ).toEqual({ words: "too_many" });
  // Any other answer is none of these refusals, and is said as it was before: the server's own
  // words, or the page's line for a save that never reached it.
  expect(
    blockedByRefusal(new ApiError(500, { code: "INTERNAL", error: "boom" }), "CORE-1", ["CORE-2"])
  ).toBeUndefined();
});

// A list that would make the issue wait on itself through the issues it waits on, its parent or
// its children is refused as `DEPENDENCY_CYCLE`, whose message names the field by its wire token
// (`CORE-1 would wait on itself through blocked_by and parent links`). It is said in the page's
// words as the other refusals are, with the issue the server names as the reader's own text; a
// chain too long for the server to follow names none.
test("a blocked-by list that would make a loop is the page's sentence about the issue it names", () => {
  const refused = (error: string) => new ApiError(409, { code: "DEPENDENCY_CYCLE", error });
  expect(
    blockedByRefusal(
      refused("CORE-1 would wait on itself through blocked_by and parent links"),
      "CORE-1",
      ["CORE-7"]
    )
  ).toEqual({ key: "CORE-1", words: "cycle" });
  expect(
    blockedByRefusal(refused("the dependency graph is deeper than 64 links"), "CORE-1", ["CORE-7"])
  ).toEqual({ words: "too_deep" });

  // Each sentence is the page's own: no wire token, and the key in it only where one goes.
  const sentences = Object.values(BLOCKED_BY_REFUSALS).map(
    ({ after, before }) => `${before}KEY${after}`
  );
  expect(sentences).toEqual([
    "KEY would then wait on itself, through the issues it waits on, its parent or its children.",
    "KEY is this issue, and an issue cannot wait on itself.",
    "There is no issue KEY.",
    "These issues wait on one another through more links than Dispatch follows, so it cannot check them for a loop.KEY",
    `An issue can wait on at most ${MAX_ISSUE_BLOCKERS} issues.KEY`,
  ]);
  for (const sentence of sentences) {
    expect(sentence).not.toMatch(/blocked_by|_/u);
  }
});

// Only a list of unbounded length folds after its first ten. An issue waits on at most
// `MAX_ISSUE_BLOCKERS` issues, so the Blocked by row draws every one of them and no Show all.
test("the blockers never fold", async () => {
  const { getIssue, restore } = stubIssueApi();
  const blockers = Array.from({ length: MAX_ISSUE_BLOCKERS }, (_, index) => `CORE-${index + 2}`);
  getIssue.mockResolvedValue({ ...issue, blocked_by: blockers });
  const { unmount } = renderDetails();

  try {
    await screen.findByRole("link", { name: "CORE-2" });
    const row = detailsRow(PROPERTY_LABELS.blocked_by);
    expect(
      within(row)
        .getAllByRole("link")
        .map((link) => link.textContent),
      "the Blocked by row folds the issues this one waits on"
    ).toEqual(blockers);
    expect(
      within(row).queryByRole("button", {
        name: (name) => name.startsWith(CONTROL_LABELS.show_all),
      }),
      "the Blocked by row offers to show a list it already draws whole"
    ).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// What waits on the issue is read from the references that point at it, which another issue's
// write moves: the Blocks row is read again on the event of an issue that starts waiting on this
// one, as the backlink panels are (`api/event-refresh.ts`'s rule for `blocked_by`).
test("Details' Blocks is read again when another issue starts waiting on this one", async () => {
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const node = graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference });
  let edges: GraphReferences["edges"] = [];
  const getReferences = spyOn(api, "getReferences").mockImplementation(async () => ({
    edges,
    node,
  }));
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    await waitFor(() => expect(blocksReads(getReferences, reference)).toBe(1));
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent(CONTROL_LABELS.none)
    );

    // CORE-5 starts waiting on CORE-1, and its event is what the feed hands the refresh, as
    // `__tests__/event-feed.test.ts` drives a blocker change.
    edges = [
      {
        created_at: "2026-10-26T00:00:00Z",
        direction: "in",
        excerpt: { text: "Ship the release" },
        kind: "blocked_by",
        node: graphNode({
          id: "CORE-5",
          issue_key: "CORE-5",
          kind: "issue",
          ref: buildDispatchReference({ key: "CORE-5", kind: "issue" }),
        }),
        source_seq: 4,
      },
    ];
    // The payload as the server writes it - the issue after the write, flat, with what the write
    // moved - in the shape the feed's own test builds it (`event-feed.test.ts`'s `issue`).
    const payload: Record<string, unknown> = {
      ancestors: [],
      assignee: null,
      blocked_by: ["CORE-1"],
      changed: ["blocked_by"],
      closed_at: null,
      key: "CORE-5",
      parent: null,
      project: "CORE",
      references_changed: [{ id: "CORE-1", kind: "issue" }],
      status: "todo",
      title: "Ship the release",
    };
    const waits = {
      actor: { id: "planner", kind: "session" },
      created_at: "2026-10-26T00:00:00Z",
      id: 12,
      issue_key: "CORE-5",
      notify: true,
      payload,
      project: "CORE",
      seq: 4,
      type: "issue.updated",
    } as DispatchEvent;
    refreshQueries(queryClient, eventRefreshTargets(waits, "alice"));

    await waitFor(() =>
      expect(
        blocksReads(getReferences, reference),
        "Blocks was not read again on CORE-5's event"
      ).toBe(2)
    );
    const row = detailsRow(PROPERTY_LABELS.blocks);
    expect(
      await within(row).findByRole("link", { name: "CORE-5" }),
      "Blocks does not draw the issue that started waiting on CORE-1"
    ).toHaveAttribute("href", "/issues/CORE-5");
    expect(row).toHaveTextContent("Ship the release");
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// Blocks says None only for an answer that holds nothing waiting on the issue: while that answer
// is still out the row says nothing, rather than read as an issue nothing waits on.
test("Details' Blocks says nothing while its read is out, and None once it answers with none", async () => {
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const answer = Promise.withResolvers<GraphReferences>();
  const getReferences = spyOn(api, "getReferences").mockImplementation(() => answer.promise);
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    await waitFor(() =>
      expect(getReferences).toHaveBeenCalledWith(reference, { kind: "blocked_by" })
    );
    expect(
      detailsRow(PROPERTY_LABELS.blocks),
      "Blocks reads None while its read is out"
    ).not.toHaveTextContent(CONTROL_LABELS.none);

    answer.resolve({
      edges: [],
      node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
    });
    await waitFor(() =>
      expect(
        detailsRow(PROPERTY_LABELS.blocks),
        "Blocks does not read None for an answer that holds nothing waiting on the issue"
      ).toHaveTextContent(CONTROL_LABELS.none)
    );
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// A Retry that turned disabled while the read it sent again was out lost the focus of a reader
// who pressed it by keyboard, since Chromium moves the focus of a control that turns disabled to
// the page's body (`emulateChromiumFocusFixup` does that here). Retry stays enabled and reads
// busy, and a press while its read is out joins that read and sends nothing (`retryRead`). The
// titles answered once before they failed, because a refetch of a read that holds an answer
// otherwise ends the request that is out and sends another.
test("Retry keeps the focus while the read it sent again is out, and a second press sends none", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const titles = spyOn(api, "getReferenceTargets").mockResolvedValueOnce({ targets: [] });
  const retried = Promise.withResolvers<ReferenceTargets>();
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const stopBlurring = emulateChromiumFocusFixup();
  const retryOfTitles = () =>
    within(detailsRow(PROPERTY_LABELS.blocked_by)).queryByRole("button", {
      name: CONTROL_LABELS.retry,
    });

  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(titles).toHaveBeenCalledTimes(1));
    titles.mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "read failed" }));
    await act(() => queryClient.refetchQueries({ queryKey: ["reference-targets"] }));
    await waitFor(() => expect(retryOfTitles(), "Blocked by offers no Retry").not.toBeNull());
    const retry = retryOfTitles();
    if (retry === null) throw new Error("Blocked by offers no Retry");

    titles.mockImplementation(() => retried.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    expect(titles, "Retry did not read the titles again").toHaveBeenCalledTimes(3);
    expect(document.activeElement, "Retry lost the focus while the read it sent again is out").toBe(
      retry
    );
    await waitFor(() =>
      expect(retry, "Retry does not read busy while the read it sent again is out").toHaveAttribute(
        "aria-busy",
        "true"
      )
    );
    expect(retry, "Retry turned disabled while the read it sent again is out").toBeEnabled();
    expect(document.activeElement, "Retry lost the focus once it read busy").toBe(retry);
    await user.keyboard("{Enter}");
    expect(titles, "a press of Retry while its read is out sent another").toHaveBeenCalledTimes(3);

    await act(async () => retried.resolve({ targets: [] }));
    await waitFor(() =>
      expect(retryOfTitles(), "the titles answered, but Blocked by still offers Retry").toBeNull()
    );
  } finally {
    stopBlurring();
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// A read that failed again was said on the same node in the same words, so a screen reader said
// nothing of it. Each failure is drawn afresh (`errorUpdateCount`), while Retry stays the same
// button, holding the focus of a reader who pressed it by keyboard.
test("a read that fails again in the same words is said again, while Retry keeps the focus", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const titles = spyOn(api, "getReferenceTargets").mockResolvedValueOnce({ targets: [] });
  const retried = Promise.withResolvers<ReferenceTargets>();
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const stopBlurring = emulateChromiumFocusFixup();
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(titles).toHaveBeenCalledTimes(1));
    titles.mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "read failed" }));
    await act(() => queryClient.refetchQueries({ queryKey: ["reference-targets"] }));
    const failed = await blockedBy().findByRole("alert");
    const retry = blockedBy().getByRole("button", { name: CONTROL_LABELS.retry });

    titles.mockImplementationOnce(() => retried.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));
    await act(async () =>
      retried.reject(new ApiError(500, { code: "INTERNAL", error: "read failed" }))
    );
    await waitFor(() =>
      expect(retry, "Retry still reads busy after the read failed again").not.toHaveAttribute(
        "aria-busy"
      )
    );

    const failedAgain = blockedBy().getByRole("alert");
    expect(failedAgain).toHaveTextContent("Could not read the titles of these issues.");
    expect(
      failedAgain,
      "the second failure was not drawn afresh, so a screen reader would not say it"
    ).not.toBe(failed);
    expect(
      blockedBy().getByRole("button", { name: CONTROL_LABELS.retry }),
      "the line came back with another Retry"
    ).toBe(retry);
    expect(document.activeElement, "Retry lost the focus when the read failed again").toBe(retry);
  } finally {
    stopBlurring();
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// A read that failed on its first load holds no answer, and a fetch of such a read clears its
// failure, so the press of its Retry took the line, and the Retry with it, away while the read it
// sent was out: the focus of a reader who pressed Retry by keyboard went to the page's body. The
// line stays, its Retry busy and holding the focus, until the read answers, and the focus then
// goes on in the row rather than to the page: in Blocked by to the row's control, and in Blocks,
// which has no control, to the first issue the answer drew.
test("a read that failed on its first load keeps its Retry, with the focus, while the read is out, and hands the focus on in its row once it answers", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(
    new ApiError(500, { code: "INTERNAL", error: "read failed" })
  );
  const getReferences = spyOn(api, "getReferences").mockRejectedValueOnce(
    new ApiError(500, { code: "INTERNAL", error: "read failed" })
  );
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);
  const stopBlurring = emulateChromiumFocusFixup();
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));
  const blocksRow = () => within(detailsRow(PROPERTY_LABELS.blocks));

  try {
    const titlesRetry = await waitFor(() =>
      blockedBy().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    const titlesHeld = Promise.withResolvers<ReferenceTargets>();
    titles.mockImplementationOnce(() => titlesHeld.promise);
    act(() => titlesRetry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(titles).toHaveBeenCalledTimes(2));
    await waitFor(() => {
      expect(
        titlesRetry,
        "the line and its Retry went while the titles Retry read again are out"
      ).toBeInTheDocument();
      expect(
        titlesRetry,
        "Retry does not read busy while the titles it read again are out"
      ).toHaveAttribute("aria-busy", "true");
    });
    expect(
      document.activeElement,
      "Retry lost the focus while the titles it read again are out"
    ).toBe(titlesRetry);
    await act(async () => titlesHeld.resolve({ targets: [] }));
    await waitFor(() =>
      expect(
        blockedBy().queryByRole("alert"),
        "the titles answered, but Blocked by still says they failed"
      ).toBeNull()
    );
    expect(
      document.activeElement,
      "the titles answered, and the focus fell to the page rather than to the row's control"
    ).toBe(blockedBy().getByRole("button", { name: blockedByControlName("CORE-1", 1) }));

    const blocksRetry = await blocksRow().findByRole("button", { name: CONTROL_LABELS.retry });
    const blocksHeld = Promise.withResolvers<GraphReferences>();
    getReferences.mockImplementationOnce(() => blocksHeld.promise);
    act(() => blocksRetry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(getReferences).toHaveBeenCalledTimes(2));
    await waitFor(() => {
      expect(
        blocksRetry,
        "the line and its Retry went while the read Retry sent again is out"
      ).toBeInTheDocument();
      expect(
        blocksRetry,
        "Retry does not read busy while the read it sent again is out"
      ).toHaveAttribute("aria-busy", "true");
    });
    expect(document.activeElement, "Retry lost the focus while the read it sent again is out").toBe(
      blocksRetry
    );
    await act(async () =>
      blocksHeld.resolve({
        edges: [
          {
            created_at: "2026-10-26T00:00:00Z",
            direction: "in",
            excerpt: { text: "Ship the release" },
            kind: "blocked_by",
            node: graphNode({
              id: "CORE-5",
              issue_key: "CORE-5",
              kind: "issue",
              ref: buildDispatchReference({ key: "CORE-5", kind: "issue" }),
            }),
            source_seq: 4,
          },
        ],
        node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
      })
    );
    const waiting = await blocksRow().findByRole("link", { name: "CORE-5" });
    expect(
      document.activeElement,
      "Blocks answered, and the focus fell to the page rather than to the first issue it drew"
    ).toBe(waiting);
  } finally {
    stopBlurring();
    unmount();
    getReferences.mockRestore();
    titles.mockRestore();
    restore();
  }
});

// Blocks has no control of its own, so where the read its Retry sent answers with nothing
// waiting on the issue, the focus Retry held goes to the None the row says then.
test("Blocks hands the focus of its Retry to its None when the read answers with none", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences")
    .mockResolvedValue({
      edges: [],
      node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
    })
    .mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "read failed" }));
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blocksRow = () => within(detailsRow(PROPERTY_LABELS.blocks));

  try {
    const retry = await waitFor(() =>
      blocksRow().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    const none = await blocksRow().findByText(CONTROL_LABELS.none);
    expect(
      document.activeElement,
      "Blocks answered with none, and the focus fell to the page rather than to its None"
    ).toBe(none);
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// Blocks said a read that failed again in the same words on the same node, so a screen reader
// said nothing of it. Each failure is drawn afresh (`errorUpdateCount`), here of a read that
// failed on its first load and so holds no answer, while Retry stays the same button, holding
// the focus of a reader who pressed it by keyboard.
test("Blocks says a read that fails again in the same words again, while its Retry keeps the focus", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences")
    .mockResolvedValue({
      edges: [],
      node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
    })
    .mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "read failed" }));
  const retried = Promise.withResolvers<GraphReferences>();
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);
  const stopBlurring = emulateChromiumFocusFixup();
  const blocksRow = () => within(detailsRow(PROPERTY_LABELS.blocks));

  try {
    const retry = await waitFor(() =>
      blocksRow().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    const failed = blocksRow().getByRole("alert");

    getReferences.mockImplementationOnce(() => retried.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));
    await act(async () =>
      retried.reject(new ApiError(500, { code: "INTERNAL", error: "read failed" }))
    );
    await waitFor(() =>
      expect(retry, "Retry still reads busy after the read failed again").not.toHaveAttribute(
        "aria-busy"
      )
    );

    const failedAgain = blocksRow().getByRole("alert");
    expect(failedAgain).toHaveTextContent("Could not read what this issue blocks.");
    expect(
      failedAgain,
      "the second failure was not drawn afresh, so a screen reader would not say it"
    ).not.toBe(failed);
    expect(
      blocksRow().getByRole("button", { name: CONTROL_LABELS.retry }),
      "the line came back with another Retry"
    ).toBe(retry);
    expect(document.activeElement, "Retry lost the focus when the read failed again").toBe(retry);
  } finally {
    stopBlurring();
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// While the Blocked by editor is open the row draws no control of its own, so as the titles the
// Retry sent answer, the focus it held goes to the first issue the row waits on rather than to
// the page.
test("while the Blocked by editor is open, the titles' Retry hands its focus to the first issue the row waits on", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(
    new ApiError(500, { code: "INTERNAL", error: "read failed" })
  );
  const answered = Promise.withResolvers<ReferenceTargets>();
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    const retry = await waitFor(() =>
      blockedBy().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    await user.click(blockedBy().getByRole("button", { name: blockedByControlName("CORE-1", 1) }));
    await blockedBy().findByRole("textbox", { name: "Issues CORE-1 waits on" });

    titles.mockImplementationOnce(() => answered.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(titles).toHaveBeenCalledTimes(2));
    await act(async () => answered.resolve({ targets: [] }));
    await waitFor(() =>
      expect(
        blockedBy().queryByRole("alert"),
        "the titles answered, but Blocked by still says they failed"
      ).toBeNull()
    );
    expect(
      document.activeElement,
      "the titles answered with the editor open, and the focus went elsewhere than the first issue the row waits on"
    ).toBe(blockedBy().getByRole("link", { name: "CORE-3" }));
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// A closed issue's Blocked by draws no control, so as the titles the Retry sent answer, the
// focus it held goes to the first issue the row waits on rather than to the page.
test("on a closed issue, the titles' Retry hands its focus to the first issue the row waits on", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...closedIssue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(
    new ApiError(500, { code: "INTERNAL", error: "read failed" })
  );
  const answered = Promise.withResolvers<ReferenceTargets>();
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    const retry = await waitFor(() =>
      blockedBy().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    expect(
      blockedBy().queryByRole("button", { name: blockedByControlName("CORE-1", 1) }),
      "a closed issue's Blocked by draws its control"
    ).toBeNull();

    titles.mockImplementationOnce(() => answered.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(titles).toHaveBeenCalledTimes(2));
    await act(async () => answered.resolve({ targets: [] }));
    await waitFor(() =>
      expect(
        blockedBy().queryByRole("alert"),
        "the titles answered, but Blocked by still says they failed"
      ).toBeNull()
    );
    expect(
      document.activeElement,
      "the titles answered on a closed issue, and the focus fell to the page rather than to the first issue the row waits on"
    ).toBe(blockedBy().getByRole("link", { name: "CORE-3" }));
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// While the Blocked by editor is open, another session's write can leave the issue waiting on
// nothing: the titles' failure goes with the list, and the row then draws no control and no
// issue, so the focus the line's Retry held goes to the editor's box rather than to the page.
test("while the Blocked by editor is open, the titles' Retry hands its focus to the editor's box once the issue waits on nothing", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const titles = spyOn(api, "getReferenceTargets").mockRejectedValueOnce(
    new ApiError(500, { code: "INTERNAL", error: "read failed" })
  );
  const { queryClient, unmount } = renderDetails(undefined, OPEN_DRAWER);
  const blockedBy = () => within(detailsRow(PROPERTY_LABELS.blocked_by));

  try {
    const retry = await waitFor(() =>
      blockedBy().getByRole("button", { name: CONTROL_LABELS.retry })
    );
    await user.click(blockedBy().getByRole("button", { name: blockedByControlName("CORE-1", 1) }));
    const box = await blockedBy().findByRole("textbox", { name: "Issues CORE-1 waits on" });
    act(() => retry.focus());

    const read = queryClient.getQueryData<IssueDetails>(issueQuery("CORE-1").queryKey);
    if (read === undefined) throw new Error("Details reads no cached issue");
    act(() => queryClient.setQueryData(issueQuery("CORE-1").queryKey, { ...read, blocked_by: [] }));
    await waitFor(() =>
      expect(
        blockedBy().queryByRole("alert"),
        "the issue waits on nothing, but Blocked by still says its titles failed"
      ).toBeNull()
    );
    expect(
      document.activeElement,
      "the issue waits on nothing with the editor open, and the focus fell to the page rather than to the editor's box"
    ).toBe(box);
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
  }
});

// A folded list's Show all goes with its press, so the focus goes on to the first entry it
// opened, the eleventh, rather than falling to the page: in Files and documents, and in Blocks.
test("Show all gives the focus to the first entry it opens", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const notes = Array.from({ length: 12 }, (_, index) => {
    const slug = `notes-${String(index + 1).padStart(2, "0")}`;
    return {
      ...specArtifact,
      id: slug,
      name: `${slug}.md`,
      primary: false,
      ref_key: `CORE-1/${slug}`,
      slug,
    };
  });
  getIssue.mockResolvedValue({ ...issue, artifacts: [specArtifact, ...notes] });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const waiting = Array.from({ length: 12 }, (_, index) => `CORE-${index + 2}`);
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: waiting.map((key) => ({
      created_at: "2026-10-26T00:00:00Z",
      direction: "in",
      excerpt: { text: `Waits for ${key}` },
      kind: "blocked_by",
      node: graphNode({
        id: key,
        issue_key: key,
        kind: "issue",
        ref: buildDispatchReference({ key, kind: "issue" }),
      }),
      source_seq: 4,
    })),
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    // Files and documents: the spec and twelve notes, so the eleventh entry is notes-10.md.
    const showFiles = await screen.findByRole("button", {
      name: `${CONTROL_LABELS.show_all} ${notes.length + 1}`,
    });
    act(() => showFiles.focus());
    await user.keyboard("{Enter}");
    expect(
      document.activeElement,
      "Show all of Files and documents left the focus off the first entry it opened"
    ).toBe(within(screen.getByTestId("issue-files")).getByRole("link", { name: /notes-10\.md/ }));

    const blocks = detailsRow(PROPERTY_LABELS.blocks);
    const showBlocks = await within(blocks).findByRole("button", {
      name: `${CONTROL_LABELS.show_all} ${waiting.length}`,
    });
    act(() => showBlocks.focus());
    await user.keyboard("{Enter}");
    expect(
      document.activeElement,
      "Show all of Blocks left the focus off the first entry it opened"
    ).toBe(within(blocks).getByRole("link", { name: "CORE-12" }));
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// Details draws its own control of Referenced by: the application's `ReferencedByToggle` is in
// its link blue, one sky-blue control among values in the page's ink.
test("Referenced by's control is in the page's ink, opens the list and reads the count", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, referenced_by_count: 3 });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    const toggle = await screen.findByRole("button", {
      name: `${PROPERTY_LABELS.referenced_by} (3)`,
    });
    expect(toggle, "Referenced by's control is not in the page's ink").toHaveClass(pageInkText);
    for (const token of linkText.split(" ")) {
      expect(toggle, "Referenced by's control is in the application's link blue").not.toHaveClass(
        token
      );
    }
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    await user.click(toggle);
    expect(toggle, "Referenced by's control does not say the list is open").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    const list = await screen.findByRole("region", { name: "Referenced by" });
    expect(toggle, "Referenced by's control names no list it opens").toHaveAttribute(
      "aria-controls",
      list.id
    );
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
  }
});

// A closed section keeps its rows on the page, hidden (`PropertySection`), as a closed Details
// keeps the panel (`slots/details.tsx`), so a Referenced by list opened before References or
// Details closed stayed drawn under it, its read live and read again on each event about the
// issue's references, for a list its reader could not see. The list is drawn only while Details
// and References are open, and is drawn again, still open, once the one closed opens.
test("closing References or Details takes an open Referenced by list, and its read, off the page", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, referenced_by_count: 3 });
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  const { queryClient, redraw, unmount } = renderDetails(undefined, OPEN_DRAWER);
  // What observes the list's read (`ReferencedBy`'s `referencesQuery`).
  const listReaders = () =>
    queryClient
      .getQueryCache()
      .find({ exact: true, queryKey: ["references", "to", reference] })
      ?.getObserversCount() ?? 0;

  try {
    await user.click(
      await screen.findByRole("button", { name: `${PROPERTY_LABELS.referenced_by} (3)` })
    );
    const list = await screen.findByRole("region", { name: "Referenced by" });
    expect(listReaders(), "the open Referenced by list does not read what references it").toBe(1);

    const references = screen.getByRole("button", { name: MARGIN_SECTION_NAMES.references });
    await user.click(references);
    expect(references).toHaveAttribute("aria-expanded", "false");
    expect(list, "a closed References still draws the Referenced by list").not.toBeInTheDocument();
    expect(listReaders(), "a closed References still reads what references the issue").toBe(0);

    await user.click(references);
    expect(
      await screen.findByRole("region", { name: "Referenced by" }),
      "References opened again without the Referenced by list the reader left open"
    ).toBeInTheDocument();

    redraw({ ...OPEN_DRAWER, open: false });
    expect(
      screen.queryByRole("region", { name: "Referenced by" }),
      "a closed Details still draws the Referenced by list"
    ).not.toBeInTheDocument();
    expect(listReaders(), "a closed Details still reads what references the issue").toBe(0);

    redraw(OPEN_DRAWER);
    expect(
      await screen.findByRole("region", { name: "Referenced by" }),
      "Details opened again without the Referenced by list the reader left open"
    ).toBeInTheDocument();
    expect(listReaders(), "the list drawn again does not read what references the issue").toBe(1);
  } finally {
    unmount();
    getReferences.mockRestore();
    restore();
    window.localStorage.removeItem(userPreferenceStorageKey("alice", "issue.margin-sections"));
  }
});

// Details' reads of the titles of the issues this one waits on, of what waits on it, of what
// references it and of the project's repository mappings are each read again by the events that
// change them (`api/event-refresh.ts`), so in Details a window focus past the app's 30 s
// staleness reads none of them again. The reader's state keeps the app's defaults, so its second
// read shows the focus was handled.
test("in Details, a window focus past 30 s reads no blocker's title, Blocks, Referenced by or repository mapping again", async () => {
  const user = userEvent.setup();
  const { getIssue, getMyState, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, blocked_by: ["CORE-3"], referenced_by_count: 3 });
  const blocker = buildDispatchReference({ key: "CORE-3", kind: "issue" });
  const titles = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] });
  const titleReads = () => titles.mock.calls.filter(([refs]) => refs.includes(blocker)).length;
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  // The Referenced by list's read, which names no kind of reference (`referencesQuery`).
  const listReads = () =>
    getReferences.mock.calls.filter(([to, options]) => to === reference && options === undefined)
      .length;
  const client = appClient();
  const { unmount } = renderDetails(undefined, OPEN_DRAWER, client);

  try {
    await user.click(
      await screen.findByRole("button", { name: `${PROPERTY_LABELS.referenced_by} (3)` })
    );
    await waitFor(() => {
      expect(titleReads()).toBe(1);
      expect(blocksReads(getReferences, reference)).toBe(1);
      expect(listReads()).toBe(1);
      expect(api.listRepoProjects).toHaveBeenCalledTimes(1);
      expect(client.isFetching()).toBe(0);
    });
    await focusPastStaleness(client);
    await waitFor(() => expect(getMyState).toHaveBeenCalledTimes(2));
    expect(
      titleReads(),
      "a window focus read the titles of the issues this one waits on again"
    ).toBe(1);
    expect(
      blocksReads(getReferences, reference),
      "a window focus read what waits on the issue again"
    ).toBe(1);
    expect(listReads(), "a window focus read what references the issue again").toBe(1);
    expect(
      api.listRepoProjects,
      "a window focus read the project's repository mappings again"
    ).toHaveBeenCalledTimes(1);
  } finally {
    unmount();
    titles.mockRestore();
    getReferences.mockRestore();
    restore();
    setSystemTime();
    focusManager.setFocused(undefined);
  }
});

// The menu lists the properties with no value and presses the control that sets each one,
// rather than drawing a second copy of it. It finds that control by the name the control
// exports (`blockedByControlName`), so the two sides cannot spell it apart: while each typed
// its own, a word changed in one made the entry disappear with no error anywhere.
test("the add menu opens the control the panel draws, under the name that control exports", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    expect(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) }),
      "the panel draws no blocked-by control under the name it exports"
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.add }));
    await user.click(await screen.findByRole("menuitem", { name: PROPERTY_LABELS.blocked_by }));
    expect(
      await screen.findByLabelText("Issues CORE-1 waits on"),
      "the menu entry reached no control, so picking it opened nothing"
    ).toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// The add menu is a menu a reader on the keyboard can use: it opens with the focus on its first
// entry, the arrow keys walk it, and Escape closes it with the focus back on Add and is taken
// there, so the phone's Details sheet, which closes on an Escape no control took, stays open.
test("the add menu opened by the keyboard takes the focus, walks by the arrows and gives it back on Escape", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const escapes = watchEscapes();
  const { unmount } = renderDetails();

  try {
    const add = await screen.findByRole("button", { name: CONTROL_LABELS.add });
    add.focus();
    await user.keyboard("{Enter}");
    const entries = await screen.findAllByRole("menuitem");
    expect(entries.length, "the add menu lists fewer than two entries to walk").toBeGreaterThan(1);
    await waitFor(() =>
      expect(document.activeElement, "the add menu opened with the focus left on Add").toBe(
        entries[0]
      )
    );
    await user.keyboard("{ArrowDown}");
    expect(document.activeElement, "ArrowDown did not move to the next entry").toBe(entries[1]);
    await user.keyboard("{ArrowUp}");
    expect(document.activeElement, "ArrowUp did not move back to the first entry").toBe(entries[0]);
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu"), "Escape left the add menu open").not.toBeInTheDocument();
    expect(document.activeElement, "Escape did not give the focus back to Add").toBe(add);
    expect(
      escapes.taken,
      "Escape in the add menu went on to the page's keys, which close Details on a phone"
    ).not.toContain(false);

    // An entry chosen by the keyboard opens its control, as one chosen by the pointer does.
    await user.keyboard("{Enter}");
    await waitFor(() => expect(document.activeElement).toBe(screen.getAllByRole("menuitem")[0]));
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(
        screen.getByLabelText(PROPERTY_LABELS.parent),
        "the menu's Parent opened no parent's editor"
      ).toHaveFocus()
    );
  } finally {
    escapes.stop();
    unmount();
    restore();
  }
});

/** No priority is live: the read the Advances row makes, answered empty. */
const noLivePriorities = {
  empty: "No priority is live now, so the list is empty.",
  priorities: [],
  priority_work_parked: { by_priority: {}, count: 0, issues: [] },
  work_under_no_priority: { by_project: {}, count: 0, issues: [] },
};

/** One priority is live, which the picker of an issue that advances none offers. */
const oneLivePriority: Priorities = {
  empty: "",
  priorities: [
    {
      description: "",
      id: "evals-now",
      made_at: "2026-11-01T00:00:00Z",
      made_by: { id: "alice", kind: "user" },
      owner: "CORE controller",
      rank: "evals-now",
      state: "live",
      title: "Evals collecting data now",
      work: {
        by_status: {},
        claimed: 0,
        closed_last_7_days: 0,
        newest_event_at: null,
        open_asks: 0,
      },
    },
  ],
  priority_work_parked: { by_priority: {}, count: 0, issues: [] },
  work_under_no_priority: { by_project: {}, count: 0, issues: [] },
};

// The add control lists what has no value yet, so where nothing can be added - a closed issue,
// or one with a value for every property the menu lists - it is not drawn at all.
test("the add control is not drawn on a closed issue, nor when every property has a value", async () => {
  const { getIssue, restore } = stubIssueApi();
  // The Advances row reads the priorities, to name the one the issue advances.
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  getIssue.mockResolvedValue(closedIssue);
  let view = renderDetails();

  try {
    await screen.findByText(PROPERTY_LABELS.parent, { selector: "[data-page-chrome]" });
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.add }),
      "the add control is drawn on a closed issue"
    ).not.toBeInTheDocument();
    view.unmount();

    // A value for each of the eight properties the menu lists: the components are the parent's.
    getIssue.mockResolvedValue({
      ...issue,
      advances: [{ id: "PRI-7", inherited: false }],
      assignee: "alice",
      blocked_by: ["CORE-3"],
      components: {
        mode: "inherit",
        ids: ["api"],
        unknown: [],
        reason: null,
        inherited_from: "CORE-2",
      },
      labels: ["bug"],
      parent: "CORE-2",
      repo: "acme/widgets",
      resolved_repo: "acme/widgets",
      route: "role:legion-controller-core",
    });
    view = renderDetails();
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(priorities).toHaveBeenCalled());
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.add }),
      "the add control is drawn on an issue with a value for every property it lists"
    ).not.toBeInTheDocument();
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

// Add is drawn while a property it lists has no value and its control is on the page. An issue
// with a value for every other property loses its last label; while the save is out the labels'
// control is named "Saving labels of CORE-1", and in the render where the save landed Add looked
// for "Add a label to CORE-1", which that render's commit had not drawn yet, so it drew nothing
// and nothing drew it again.
test("removing the last label draws Add, whose Labels the row's control now answers", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  const full: IssueDetails = {
    ...issue,
    advances: [{ id: "PRI-7", inherited: false }],
    assignee: "alice",
    blocked_by: ["CORE-3"],
    components: {
      mode: "inherit",
      ids: ["api"],
      unknown: [],
      reason: null,
      inherited_from: "CORE-2",
    },
    labels: ["bug"],
    parent: "CORE-2",
    repo: "acme/widgets",
    resolved_repo: "acme/widgets",
    route: "role:legion-controller-core",
  };
  getIssue.mockResolvedValue(full);
  patchIssue.mockImplementation(async (_key, update) =>
    narrowIssue({ ...full, labels: [...(update.labels ?? full.labels ?? [])] })
  );
  const view = renderDetails();
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(priorities).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: CONTROL_LABELS.add })).not.toBeInTheDocument();
    await user.click(
      screen.getByRole("button", {
        name: labelsControlName("CORE-1", {
          labels: ["bug"],
          saving: false,
          words: CONTROL_NAME_WORDS,
        }),
      })
    );
    await user.click(await screen.findByRole("option", { name: "bug" }));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(
      await screen.findByRole("button", {
        name: labelsControlName("CORE-1", {
          labels: [],
          saving: false,
          words: CONTROL_NAME_WORDS,
        }),
      }),
      "the label was not removed"
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(
        screen.queryByRole("button", { name: CONTROL_LABELS.add }),
        "Labels has no value and its control is drawn, but Details draws no Add"
      ).toBeInTheDocument()
    );
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.add }));
    expect(
      await screen.findByRole("menuitem", { name: PROPERTY_LABELS.labels }),
      "the add menu lists no Labels for the control the row draws"
    ).toBeInTheDocument();
  } finally {
    view.unmount();
    listIssues.mockRestore();
    priorities.mockRestore();
    restore();
  }
});

// Add is drawn again when a control it reaches comes back in a commit of the component that
// draws it. On an issue with a value for every property but the priorities it advances, Add
// lists Advances alone. Picking it presses "Add a priority to CORE-1", whose form replaces that
// control in the same commit, so Add, finding no control, went; Cancel drew the control again in
// a commit of the priority line alone, which Add never read, so Add stayed gone beside the
// control it reaches.
test("Add is drawn again after the Advances form it opened is cancelled", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  getIssue.mockResolvedValue({
    ...issue,
    advances: [],
    assignee: "alice",
    blocked_by: ["CORE-3"],
    components: {
      mode: "inherit",
      ids: ["api"],
      unknown: [],
      reason: null,
      inherited_from: "CORE-2",
    },
    labels: ["bug"],
    parent: "CORE-2",
    repo: "acme/widgets",
    resolved_repo: "acme/widgets",
    route: "role:legion-controller-core",
  });
  const view = renderDetails();
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    await user.click(await screen.findByRole("menuitem", { name: PROPERTY_LABELS.advances }));
    const cancel = await screen.findByRole("button", { name: CONTROL_LABELS.cancel });
    await user.click(cancel);
    expect(
      await screen.findByRole("button", { name: addPriorityControlName("CORE-1") }),
      "the priority's add control is not back"
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(
        screen.queryByRole("button", { name: CONTROL_LABELS.add }),
        "Advances has no value and its control is drawn, but Details draws no Add"
      ).toBeInTheDocument()
    );
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

/** Add's own control, found whether or not it is drawn on the screen, and never the priority
 *  form's own Add button: only Add's control opens a menu. */
function addMenuControl(): Element | null {
  return document.querySelector(
    `[aria-haspopup="menu"][aria-label="${CSS.escape(CONTROL_LABELS.add)}"]`
  );
}

// A press on an entry of Add opens that property's control. While the priorities' save is out,
// "Add a priority to CORE-1" takes no press, so a press on Add's Advances opened nothing: with
// the save held out, Add listed Advances, and pressing it left the menu closed, the form shut and
// the focus on the body. Add lists an entry only while its control can be pressed, and lists it
// again once a refused save gives that control back.
test("Add lists no entry whose control is disabled, and lists it again once it is enabled", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue({
    ...issue,
    advances: [],
    assignee: "alice",
    blocked_by: ["CORE-3"],
    components: {
      mode: "inherit",
      ids: ["api"],
      unknown: [],
      reason: null,
      inherited_from: "CORE-2",
    },
    labels: ["bug"],
    parent: "CORE-2",
    repo: "acme/widgets",
    resolved_repo: "acme/widgets",
    route: "role:legion-controller-core",
  });
  const save = Promise.withResolvers<never>();
  patchIssue.mockImplementationOnce(() => save.promise);
  const view = renderDetails();
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    await user.click(await screen.findByRole("menuitem", { name: PROPERTY_LABELS.advances }));
    const form = await screen.findByTestId("issue-advances");
    await user.selectOptions(await within(form).findByRole("combobox"), "evals-now");
    await user.click(within(form).getByRole("button", { name: CONTROL_LABELS.add }));
    const control = await screen.findByRole("button", { name: addPriorityControlName("CORE-1") });
    // Refused while its write is out, by the rule Add reads (`usableControl`): `disabled`, or
    // `aria-disabled` where the shared control stays focusable while it is busy.
    await waitFor(() =>
      expect(usableControl(control), "the control still takes a press while its write is out").toBe(
        false
      )
    );
    expect(
      addMenuControl(),
      "Add lists Advances while its control is disabled, so a press on it opens nothing"
    ).toBeNull();
    await act(async () =>
      save.reject(new ApiError(409, { code: "REVISION_CONFLICT", error: "the revision moved" }))
    );
    await waitFor(() =>
      expect(
        usableControl(screen.getByRole("button", { name: addPriorityControlName("CORE-1") })),
        "the control still takes no press"
      ).toBe(true)
    );
    await waitFor(() =>
      expect(
        addMenuControl(),
        "Advances has no value and its control is enabled again, but Details draws no Add"
      ).not.toBeNull()
    );
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

/** The issue of the case above: a value for every property but Advances, so Add's one entry is
 *  Advances, and Add is drawn exactly while "Add a priority to CORE-1" takes a press. */
const advancesOnly: IssueDetails = {
  ...issue,
  advances: [],
  assignee: "alice",
  blocked_by: ["CORE-3"],
  components: {
    mode: "inherit",
    ids: ["api"],
    unknown: [],
    reason: null,
    inherited_from: "CORE-2",
  },
  labels: ["bug"],
  parent: "CORE-2",
  repo: "acme/widgets",
  resolved_repo: "acme/widgets",
  route: "role:legion-controller-core",
};

/** Sets `name` on `element`, and returns what takes it off again. */
function setAttributeUntil(element: Element, name: string, value: string): () => void {
  element.setAttribute(name, value);
  return () => element.removeAttribute(name);
}

/** Each way a control Add reaches stops taking a press (`usableControl`), or stops being found by
 *  the name Add looks for, set on "Add a priority to CORE-1" or on the element around it, with
 *  what undoes it. */
const refusals = [
  ["disabled", (control: HTMLElement) => setAttributeUntil(control, "disabled", "")],
  [
    'aria-disabled="true"',
    (control: HTMLElement) => setAttributeUntil(control, "aria-disabled", "true"),
  ],
  [
    "under an element that is hidden",
    (_control: HTMLElement, around: HTMLElement) => setAttributeUntil(around, "hidden", ""),
  ],
  [
    "under an element that is inert",
    (_control: HTMLElement, around: HTMLElement) => setAttributeUntil(around, "inert", ""),
  ],
  [
    'under an element that is aria-hidden="true"',
    (_control: HTMLElement, around: HTMLElement) =>
      setAttributeUntil(around, "aria-hidden", "true"),
  ],
  [
    "under an element that is not drawn",
    (_control: HTMLElement, around: HTMLElement) => {
      around.style.display = "none";
      return () => around.style.removeProperty("display");
    },
  ],
  [
    "under an element of visibility: hidden",
    (_control: HTMLElement, around: HTMLElement) => {
      around.style.visibility = "hidden";
      return () => around.style.removeProperty("visibility");
    },
  ],
  [
    "under an element whose class the stylesheet draws as nothing",
    (_control: HTMLElement, around: HTMLElement) => {
      const sheet = document.createElement("style");
      sheet.textContent = ".drawn-as-nothing { display: none; }";
      document.head.append(sheet);
      around.classList.add("drawn-as-nothing");
      return () => {
        around.classList.remove("drawn-as-nothing");
        sheet.remove();
      };
    },
  ],
  [
    // Another component's own commit renames its control in place, which draws Add no further
    // time: Add finds the control again only by watching its name.
    "renamed in place",
    (control: HTMLElement) => {
      const name = control.getAttribute("aria-label") ?? "";
      control.setAttribute("aria-label", `${name}, renamed`);
      return () => control.setAttribute("aria-label", name);
    },
  ],
  [
    "removed from the page",
    (control: HTMLElement) => {
      const parent = control.parentNode;
      const next = control.nextSibling;
      control.remove();
      return () => {
        parent?.insertBefore(control, next);
      };
    },
  ],
] as const;

// Add lists an entry only while its control takes a press (`usableControl`), whatever stops it
// taking one - on the control or on an element around it - and lists it again once that is
// undone, in a change no component of Add's draws. Each row is one rule of `usableControl`, or
// the name Add finds the control by, and each is a change Add's watch must see.
test.each(
  refusals
)("Add lists no entry whose control is %s, and lists it again once it is not", async (_name, refuse) => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  getIssue.mockResolvedValue(advancesOnly);
  const view = renderDetails();
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    expect(
      screen.getAllByRole("menuitem").map((entry) => entry.textContent),
      "Add lists other than Advances"
    ).toEqual([PROPERTY_LABELS.advances]);
    await user.keyboard("{Escape}");
    const control = screen.getByRole("button", { name: addPriorityControlName("CORE-1") });
    let undo = () => {};
    await act(async () => {
      undo = refuse(control, screen.getByTestId("issue-advances"));
    });
    await waitFor(() =>
      expect(addMenuControl(), "Add lists Advances while its control takes no press").toBeNull()
    );
    await act(async () => undo());
    await waitFor(() =>
      expect(
        addMenuControl(),
        "Advances' control takes a press again, but Details draws no Add"
      ).not.toBeNull()
    );
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

// Details draws the priority's line in one of two places: beside Add while the issue advances
// none, and in the Advances row once it advances one. The first priority added moves it, and the
// focus its add control held as the add landed went with the place the line left, to the page's
// body. It goes on to the add control of the line in the Advances row, named as the control it
// was on, whose next press adds another.
test("the first priority added hands the focus from the add control beside Add to the Advances row's add control", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue(advancesOnly);
  const view = renderDetails();
  try {
    const control = await screen.findByRole("button", { name: addPriorityControlName("CORE-1") });
    act(() => control.focus());
    await user.keyboard("{Enter}");
    const picker = await screen.findByRole("combobox", {
      name: CONTROL_LABELS.priority_to_advance,
    });
    await within(picker).findByRole("option", { name: /Evals collecting data now$/ });
    await user.selectOptions(picker, "evals-now");
    const form = picker.closest("form");
    if (form === null) throw new Error("the picker is in no form");
    // The issue's read after the write is held, so the write lands, and the line gives the focus
    // back to its add control beside Add, before the line moves.
    const reread = Promise.withResolvers<IssueDetails>();
    getIssue.mockImplementation(() => reread.promise);
    await user.tab();
    expect(document.activeElement, "Tab from the picker did not reach the form's Add").toBe(
      within(form).getByRole("button", { name: CONTROL_LABELS.add })
    );
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(
        document.activeElement,
        "the add landed, and the line gave the focus back to no add control beside Add"
      ).toBe(screen.queryByRole("button", { name: addPriorityControlName("CORE-1") }))
    );
    await act(async () =>
      reread.resolve({ ...advancesOnly, advances: [{ id: "evals-now", inherited: false }] })
    );
    const row = await waitFor(() => detailsRow(PROPERTY_LABELS.advances));
    expect(
      document.activeElement,
      "the line moved into the Advances row, and the focus fell to the page rather than to the row's add control"
    ).toBe(within(row).getByRole("button", { name: addPriorityControlName("CORE-1") }));
    expect(patchIssue, "other than one write went out").toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

// The last priority removed moves the line back beside Add, and the focus its Remove held went
// with the Advances row to the page's body. It goes on to the add control beside Add, the one
// control the line draws there. Remove holds the focus until the line moves: while its write is
// out it is `aria-disabled`, never `disabled`.
test("removing the last priority hands the focus from its Remove to the add control beside Add", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue({
    ...advancesOnly,
    advances: [{ id: "evals-now", inherited: false }],
  });
  const view = renderDetails();
  try {
    const remove = await waitFor(() =>
      within(detailsRow(PROPERTY_LABELS.advances)).getByRole("button", { name: /^Remove / })
    );
    // The issue's read after the write is held, so the removal lands before the line moves.
    const reread = Promise.withResolvers<IssueDetails>();
    getIssue.mockImplementation(() => reread.promise);
    const reads = getIssue.mock.calls.length;
    act(() => remove.focus());
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(
        getIssue.mock.calls.length,
        "the removal landed, and the issue was not read again"
      ).toBeGreaterThan(reads)
    );
    // The shared Remove stays refused, and keeps the focus, until the new read is drawn.
    expect(remove, "Remove took a press again before the new read was drawn").toHaveAttribute(
      "aria-disabled",
      "true"
    );
    expect(document.activeElement, "Remove lost the focus before the line moved").toBe(remove);
    await act(async () => reread.resolve(advancesOnly));
    await waitFor(() =>
      expect(
        screen.queryByText(PROPERTY_LABELS.advances, {
          exact: true,
          selector: "[data-page-chrome]",
        }),
        "the issue advances nothing, and Details still draws the Advances row"
      ).toBeNull()
    );
    expect(
      document.activeElement,
      "the line moved back beside Add, and the focus fell to the page rather than to its add control"
    ).toBe(screen.getByRole("button", { name: addPriorityControlName("CORE-1") }));
    expect(patchIssue, "other than one write went out").toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

// On a closed issue the line's add controls are disabled. Another session's write that takes
// away the last priority takes the Advances row away, and the focus on its priority went to the
// disabled add control beside Add, which takes no focus, so it fell to the page's body. It goes
// on to Development's heading, the button that opens and closes the section those rows are in.
test("on a closed issue, the last priority taken away hands the focus from its link to Development's heading", async () => {
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  const closed: IssueDetails = {
    ...advancesOnly,
    advances: [{ id: "evals-now", inherited: false }],
    closed_at: "2026-09-10T00:00:00Z",
    status: "done",
  };
  getIssue.mockResolvedValue(closed);
  const { queryClient, unmount } = renderDetails();
  try {
    const link = await waitFor(() =>
      within(detailsRow(PROPERTY_LABELS.advances)).getByRole("link")
    );
    act(() => link.focus());
    act(() => queryClient.setQueryData(issueQuery("CORE-1").queryKey, { ...closed, advances: [] }));
    await waitFor(() =>
      expect(
        screen.queryByText(PROPERTY_LABELS.advances, {
          exact: true,
          selector: "[data-page-chrome]",
        }),
        "the issue advances nothing, and Details still draws the Advances row"
      ).toBeNull()
    );
    expect(
      document.activeElement,
      "the Advances row went on a closed issue, and the focus did not go to Development's heading"
    ).toBe(screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development }));
  } finally {
    unmount();
    priorities.mockRestore();
    restore();
  }
});

// An Advances row at the cap draws no add control, so the focus the add control beside Add held
// as the line moved into that row goes to the row's first priority rather than to the page.
test("the line moved into an Advances row at the cap hands the focus from the add control beside Add to the row's first priority", async () => {
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(oneLivePriority);
  getIssue.mockResolvedValue(advancesOnly);
  const { queryClient, unmount } = renderDetails();
  try {
    const control = await screen.findByRole("button", { name: addPriorityControlName("CORE-1") });
    act(() => control.focus());
    const atCap = Array.from({ length: MAX_ISSUE_ADVANCES }, (_, index) => ({
      id: `priority-${index + 1}`,
      inherited: false,
    }));
    act(() =>
      queryClient.setQueryData(issueQuery("CORE-1").queryKey, {
        ...advancesOnly,
        advances: atCap,
      })
    );
    const row = await waitFor(() => detailsRow(PROPERTY_LABELS.advances));
    expect(
      within(row).queryByRole("button", { name: addPriorityControlName("CORE-1") }),
      "the Advances row at the cap draws an add control"
    ).toBeNull();
    expect(
      document.activeElement,
      "the line moved into the Advances row at the cap, and the focus did not go to its first priority"
    ).toBe(within(row).getAllByRole("link")[0]);
  } finally {
    unmount();
    priorities.mockRestore();
    restore();
  }
});

// `checkVisibility()` is missing from browsers Dispatch still runs on (`shell/roving.ts`), and
// there `usableControl` reads the attributes alone: a control that takes a press is still
// listed, and an element that has left the page still takes none.
test("with no checkVisibility, Add still lists a control that takes a press", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  getIssue.mockResolvedValue(advancesOnly);
  const checkVisibility = Object.getOwnPropertyDescriptor(Element.prototype, "checkVisibility");
  // Unmounts what the case drew, once it has drawn it.
  let unmount = () => {};
  try {
    Reflect.deleteProperty(Element.prototype, "checkVisibility");
    unmount = renderDetails().unmount;
    expect(checkVisibility, "the DOM has no checkVisibility of its own to take away").toBeDefined();
    expect(typeof document.body.checkVisibility).toBe("undefined");
    await screen.findByRole("link", { name: "CORE-3" });
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    expect(
      screen.getAllByRole("menuitem").map((entry) => entry.textContent),
      "with no checkVisibility, Add does not list Advances, whose control takes a press"
    ).toEqual([PROPERTY_LABELS.advances]);
    expect(
      usableControl(document.createElement("button")),
      "with no checkVisibility, a control that is not on the page takes a press"
    ).toBe(false);
  } finally {
    unmount();
    if (checkVisibility !== undefined) {
      Object.defineProperty(Element.prototype, "checkVisibility", checkVisibility);
    }
    priorities.mockRestore();
    restore();
  }
});

/** Details as the shell's drawer draws it: the details slot, whose panel the drawer's `aside`
 *  holds under a box of the sheet's own (`MarginSheet.tsx`'s `SheetBody`, which the phone's
 *  sheet hides while a margin thread is open), the box found by the test id `sheet-body`. */
function renderDetailsDrawer(details: IssueDetails) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  return render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarginSlotProvider drawsSlot>
            <DetailsSlot issue={details} route={{ key: "CORE-1", kind: "issue" }} />
            <aside {...{ [DETAILS_ATTRIBUTE]: "" }}>
              <div data-testid="sheet-body">
                <MarginBody />
              </div>
            </aside>
          </MarginSlotProvider>
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// The phone's sheet hides the box that holds the panel while a margin thread is open
// (`MarginSheet.tsx`'s `SheetBody`). That box is between the panel and Details' `aside`, where
// `usableControl` reads every element, so a control of Details takes no press while it is
// hidden: Add lists no entry then, and lists it again once the box is shown. Neither change is a
// commit of Add's or a change inside the panel, so Add sees them only by watching the whole
// `aside`; watching the panel alone, a read made while the box was hidden left Add gone.
test("Add lists no entry while the box between the panel and Details' aside is hidden, and lists it again once it is shown", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  const view = renderDetailsDrawer(advancesOnly);
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    expect(
      screen.getAllByRole("menuitem").map((entry) => entry.textContent),
      "Add lists other than Advances"
    ).toEqual([PROPERTY_LABELS.advances]);
    await user.keyboard("{Escape}");
    const box = screen.getByTestId("sheet-body");
    expect(
      box.contains(addMenuControl()) && box.closest(`aside[${DETAILS_ATTRIBUTE}]`) !== null,
      "Add is not under the box in Details' aside"
    ).toBe(true);
    await act(async () => box.setAttribute("hidden", ""));
    await waitFor(() =>
      expect(
        addMenuControl(),
        "Add lists Advances while the box that holds the panel is hidden"
      ).toBeNull()
    );
    await act(async () => box.removeAttribute("hidden"));
    await waitFor(() =>
      expect(
        addMenuControl(),
        "the box that holds the panel is shown again, but Details draws no Add"
      ).not.toBeNull()
    );
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

// Details closed - its `aside` hidden (`Margin.tsx`), which a browser draws as `display: none`
// and happy-dom draws as nothing at all, so the case sets that style too - is no refusal
// (`usableControl`): Details opens on what Add listed while it was closed, so that is what Add
// lists once Details is open, and no read on opening has to put an entry back.
test("Add lists the same entries while Details is closed as once it is open again", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  const view = renderDetailsDrawer(advancesOnly);
  // Add's entries, read whether or not Details is drawn.
  const listed = () =>
    screen.queryAllByRole("menuitem", { hidden: true }).map((entry) => entry.textContent);
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    expect(listed(), "Add lists other than Advances").toEqual([PROPERTY_LABELS.advances]);
    const details = addMenuControl()?.closest<HTMLElement>(`aside[${DETAILS_ATTRIBUTE}]`);
    if (details == null) throw new Error("Add is not in Details' aside");
    await act(async () => {
      details.setAttribute("hidden", "");
      details.style.display = "none";
    });
    expect(listed(), "Add lists other entries while Details is closed").toEqual([
      PROPERTY_LABELS.advances,
    ]);
    await act(async () => {
      details.removeAttribute("hidden");
      details.style.removeProperty("display");
    });
    expect(listed(), "Add lists other entries once Details is open again").toEqual([
      PROPERTY_LABELS.advances,
    ]);
  } finally {
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

// The assignee's control is the header's, and the header can be drawn after Details. Add then
// finds no header to watch, so it watches the body for one drawn later, and lists Assignee once
// a header that draws the assignee's control is on the page, though that is no commit of Add's.
test("Add lists Assignee once a header drawn after it draws the assignee's control", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  // A value for every property Add lists but the assignee.
  getIssue.mockResolvedValue({
    ...advancesOnly,
    advances: [{ id: "PRI-7", inherited: false }],
    assignee: null,
  });
  const header = document.createElement("div");
  header.setAttribute("data-issue-header", "");
  const assignee = document.createElement("button");
  assignee.type = "button";
  assignee.setAttribute("aria-label", assigneeControlName("CORE-1"));
  header.append(assignee);
  const view = renderDetails();
  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(priorities).toHaveBeenCalled());
    expect(
      addMenuControl(),
      "Add is drawn before any header draws the assignee's control, which its one entry reaches"
    ).toBeNull();
    await act(async () => document.body.append(header));
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    expect(
      screen.getAllByRole("menuitem").map((entry) => entry.textContent),
      "Add lists other than Assignee once the header draws the assignee's control"
    ).toEqual([PROPERTY_LABELS.assignee]);
  } finally {
    header.remove();
    view.unmount();
    priorities.mockRestore();
    restore();
  }
});

// With Details open, the repository's control drew its select only once its own read of the
// project's mappings answered, in a commit of that control alone, and Add, read after its own
// commits only, never saw it, so on an issue with a value for every other property Details drew
// no Add beside the control it would press. The control is drawn at once, whatever that read
// answers: on an issue that resolves to no repository the row reads "None", a button whose press
// draws the box in its place. So nothing here waits on the read, which is held out throughout;
// what the case still holds is that Add is drawn beside that one control, lists Repository, and
// that choosing it reaches the box.
test("Add lists Repository on an empty repository row and presses None, which opens the box", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  // Open, Details reads what waits on the issue.
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  // The issue resolves to no repository, so its control reads the mappings at once; the read
  // never answers here, and the control is drawn without it.
  const mappings = Promise.withResolvers<RepoProject[]>();
  const listRepoProjects = spyOn(api, "listRepoProjects").mockImplementation(
    () => mappings.promise
  );
  getIssue.mockResolvedValue({
    ...issue,
    advances: [{ id: "PRI-7", inherited: false }],
    assignee: "alice",
    blocked_by: ["CORE-3"],
    components: {
      mode: "inherit",
      ids: ["api"],
      unknown: [],
      reason: null,
      inherited_from: "CORE-2",
    },
    labels: ["bug"],
    parent: "CORE-2",
    route: "role:legion-controller-core",
  });
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(priorities).toHaveBeenCalled());
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.blocks)).toHaveTextContent(CONTROL_LABELS.none)
    );
    expect(listRepoProjects).toHaveBeenCalled();
    const none = within(detailsRow(PROPERTY_LABELS.repository)).getByRole("button", {
      name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
    });
    expect(none, "the empty Repository row does not read None").toHaveTextContent(
      CONTROL_LABELS.none
    );
    expect(
      screen.queryByRole("combobox", { name: repoControlName("CORE-1") }),
      "the empty Repository row draws its box before None is pressed"
    ).not.toBeInTheDocument();
    await waitFor(() =>
      expect(
        screen.queryByRole("button", { name: CONTROL_LABELS.add }),
        "Repository has no value and its row draws None, but Details draws no Add"
      ).toBeInTheDocument()
    );
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.add }));
    const entry = await screen.findByRole("menuitem", { name: PROPERTY_LABELS.repository });
    expect(
      entry,
      "the add menu lists no Repository for the None the row draws"
    ).toBeInTheDocument();
    await user.click(entry);
    const box = await screen.findByRole("combobox", { name: repoControlName("CORE-1") });
    await waitFor(() =>
      expect(box, "the menu's Repository put no focus in the repository's box").toHaveFocus()
    );
    expect(none, "the menu's Repository left None drawn beside the box").not.toBeInTheDocument();
  } finally {
    unmount();
    listRepoProjects.mockRestore();
    getReferences.mockRestore();
    priorities.mockRestore();
    restore();
  }
});

// An issue with no repository of its own that inherits one has no value for Repository, so Add
// lists it; its row draws the inherited repository as text, a button whose press draws the box in
// its place. Add has to press that text: it finds neither "None" nor the box, which the row draws
// only once the text is pressed, and with neither it listed no Repository for the issue at all.
test("Add lists Repository on a row that inherits its repository and presses its text, which opens the box", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const priorities = spyOn(api, "listPriorities").mockResolvedValue(noLivePriorities);
  // Open, Details reads what waits on the issue.
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  // The focus in the box reads the mappings; the read never answers here.
  const mappings = Promise.withResolvers<RepoProject[]>();
  const listRepoProjects = spyOn(api, "listRepoProjects").mockImplementation(
    () => mappings.promise
  );
  // A value for every property Add lists but the repository, which the issue inherits: the API
  // answers no repository of its own and the one it resolves to.
  getIssue.mockResolvedValue({
    ...advancesOnly,
    advances: [{ id: "PRI-7", inherited: false }],
    repo: null,
    resolved_repo: "acme/web",
  });
  const { unmount } = renderDetails(undefined, OPEN_DRAWER);

  try {
    await screen.findByRole("link", { name: "CORE-3" });
    await waitFor(() => expect(priorities).toHaveBeenCalled());
    const text = await within(detailsRow(PROPERTY_LABELS.repository)).findByRole("button", {
      name: repoTextControlName("CORE-1", { resolvedRepo: "acme/web", words: CONTROL_NAME_WORDS }),
    });
    expect(text, "the Repository row does not draw the repository it inherits").toHaveTextContent(
      "acme/web"
    );
    expect(
      screen.queryByRole("combobox", { name: repoControlName("CORE-1") }),
      "the Repository row draws its box before its text is pressed"
    ).not.toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.add }));
    expect(
      screen.getAllByRole("menuitem").map((entry) => entry.textContent),
      "Add lists other than Repository for an issue that inherits its repository"
    ).toEqual([PROPERTY_LABELS.repository]);
    await user.click(screen.getByRole("menuitem", { name: PROPERTY_LABELS.repository }));
    const box = await screen.findByRole("combobox", { name: repoControlName("CORE-1") });
    await waitFor(() =>
      expect(box, "the menu's Repository put no focus in the repository's box").toHaveFocus()
    );
    expect(
      text,
      "the menu's Repository left the inherited repository drawn beside the box"
    ).not.toBeInTheDocument();
  } finally {
    unmount();
    listRepoProjects.mockRestore();
    getReferences.mockRestore();
    priorities.mockRestore();
    restore();
  }
});

/** That `alert`, a failure said in `row` of Details, is Details' own line (`details-error.tsx`):
 *  in the page's colour for what failed, with nothing in the row in a colour that is not the
 *  page's (`offPageColours`), as the shared `QueryError`'s danger colour is not, and with the
 *  page's Retry. */
function expectPageFailure(row: HTMLElement, alert: HTMLElement): void {
  expect(alert, "the failure is not in the page's colour for what failed").toHaveClass(
    pageFailedText
  );
  expect(
    offPageColours(PROPERTY_LABELS.repository, [row, ...row.querySelectorAll("*")]).offenders,
    "the row draws the failure in a colour that is not the page's"
  ).toEqual([]);
  expect(
    within(row).getByRole("button", { name: CONTROL_LABELS.retry }),
    "Retry is not drawn as the page's control"
  ).toHaveClass(pageHeaderControl);
}

// The repository's control reads the project's mappings at once on an issue that resolves to no
// repository, as this one does. A read that fails is said in the Repository row as every other
// row of Details says a failed read, and its Retry reads the mappings again.
test("a failed read of the repository mappings is said in the page's colours, with Retry", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const listRepoProjects = spyOn(api, "listRepoProjects")
    .mockRejectedValueOnce(new Error("the network dropped the read"))
    .mockResolvedValue([]);
  const { unmount } = renderDetails();

  try {
    const row = await waitFor(() => detailsRow(PROPERTY_LABELS.repository));
    const alert = await within(row).findByRole("alert");
    expectPageFailure(row, alert);
    expect(alert.textContent).toBe("Could not load the repositories mapped to CORE.");
    const reads = listRepoProjects.mock.calls.length;
    await user.click(within(row).getByRole("button", { name: CONTROL_LABELS.retry }));
    await waitFor(() => expect(listRepoProjects.mock.calls.length).toBeGreaterThan(reads));
    await waitFor(() =>
      expect(
        within(row).queryByRole("alert"),
        "the failure is still said after the mappings were read"
      ).not.toBeInTheDocument()
    );
  } finally {
    unmount();
    listRepoProjects.mockRestore();
    restore();
  }
});

// A repository Dispatch refuses is said in the Repository row the same way, in Dispatch's words,
// and its Retry makes the refused pick again.
test("a repository Dispatch refuses is said in the page's colours, with Retry", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const reason = 'repo must be "owner/name"';
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "INVALID_ISSUE", error: reason }))
  );
  const { unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", {
        name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
      })
    );
    await user.click(await screen.findByRole("combobox", { name: repoControlName("CORE-1") }));
    await user.paste("acme/web");
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/web" }, keyedAttempt)
    );
    const row = detailsRow(PROPERTY_LABELS.repository);
    const alert = await within(row).findByRole("alert");
    expectPageFailure(row, alert);
    expect(alert.textContent).toBe(reason);
    await user.click(within(row).getByRole("button", { name: CONTROL_LABELS.retry }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(
      patchIssue.mock.calls.map(([, input]) => input),
      "Retry did not send the refused repository again"
    ).toEqual([{ repo: "acme/web" }, { repo: "acme/web" }]);
    await waitFor(() =>
      expect(
        within(row).queryByRole("alert"),
        "the refusal is still said after the repository landed"
      ).not.toBeInTheDocument()
    );
  } finally {
    unmount();
    restore();
  }
});

// A fetch of mappings that hold no answer clears their error, so a line drawn from the error
// alone went at the press of its Retry, and the focus of a reader who pressed Retry by keyboard
// went with it to the page's body. The line stays while the read is out, its Retry busy and
// holding the focus, a second press joins the read that is out, and once the mappings answer and
// the line goes, the focus goes to the row's control rather than to the page.
test("the mappings' Retry keeps the focus while the read is out, and hands it to the row's control once it answers", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const retried = Promise.withResolvers<RepoProject[]>();
  const listRepoProjects = spyOn(api, "listRepoProjects")
    .mockRejectedValueOnce(new Error("the network dropped the read"))
    .mockImplementation(() => retried.promise);
  const { unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();
  const retryOfMappings = () =>
    within(detailsRow(PROPERTY_LABELS.repository)).queryByRole("button", {
      name: CONTROL_LABELS.retry,
    });

  try {
    const row = await waitFor(() => detailsRow(PROPERTY_LABELS.repository));
    await within(row).findByRole("alert");
    const retry = retryOfMappings();
    if (retry === null) throw new Error("the failed read of the mappings offers no Retry");

    act(() => retry.focus());
    await user.keyboard("{Enter}");
    expect(listRepoProjects, "Retry did not read the mappings again").toHaveBeenCalledTimes(2);
    await waitFor(() =>
      expect(retry, "Retry does not read busy while the read it sent again is out").toHaveAttribute(
        "aria-busy",
        "true"
      )
    );
    expect(retryOfMappings(), "the line and its Retry went while the read is out").toBe(retry);
    expect(
      within(row).getByRole("alert").textContent,
      "the line no longer says the read failed while it is read again"
    ).toBe("Could not load the repositories mapped to CORE.");
    expect(document.activeElement, "Retry lost the focus while the read it sent again is out").toBe(
      retry
    );
    await user.keyboard("{Enter}");
    expect(
      listRepoProjects,
      "a press of Retry while its read is out sent another"
    ).toHaveBeenCalledTimes(2);

    await act(async () => retried.resolve([]));
    await waitFor(() =>
      expect(retryOfMappings(), "the mappings answered, but the row still offers Retry").toBeNull()
    );
    expect(
      document.activeElement,
      "the mappings answered, and the focus fell to the page rather than to the row's control"
    ).toBe(
      within(row).getByRole("button", {
        name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
      })
    );
  } finally {
    stopBlurring();
    unmount();
    listRepoProjects.mockRestore();
    restore();
  }
});

// A press of a refused repository's Retry clears the refusal as it sends the pick again, so the
// line and its Retry go at the press, and the focus of a reader who pressed Retry by keyboard
// went with them to the page's body. It goes to the row's box instead, which the row draws while
// the write is out, and from there to the row's text once the repository lands.
test("a refused repository's Retry hands the focus to the row's box as its line goes", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "INVALID_ISSUE", error: "repository refused" }))
  );
  const { unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    await user.click(
      await screen.findByRole("button", {
        name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
      })
    );
    await user.click(await screen.findByRole("combobox", { name: repoControlName("CORE-1") }));
    await user.paste("acme/web");
    await user.keyboard("{Enter}");
    const row = detailsRow(PROPERTY_LABELS.repository);
    await within(row).findByRole("alert");
    const retry = within(row).getByRole("button", { name: CONTROL_LABELS.retry });

    const retried = Promise.withResolvers<Issue>();
    patchIssue.mockImplementationOnce(() => retried.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(
        within(row).queryByRole("alert"),
        "the refusal is still said while the pick is sent again"
      ).not.toBeInTheDocument()
    );
    expect(
      document.activeElement,
      "the refusal's line went, and the focus fell to the page rather than to the row's box"
    ).toBe(within(row).getByRole("combobox", { name: repoControlName("CORE-1") }));

    // Dispatch answers the write, and every read of the issue after it, with the issue's own
    // repository as the one it resolves to.
    const landed = { ...issue, repo: "acme/web", resolved_repo: "acme/web" };
    getIssue.mockResolvedValue(landed);
    await act(async () => retried.resolve(narrowIssue(landed)));
    await waitFor(() =>
      expect(
        within(row).queryByRole("combobox", { name: repoControlName("CORE-1") }),
        "the row still draws its box after the repository landed"
      ).not.toBeInTheDocument()
    );
    expect(
      document.activeElement,
      "the repository landed, and the focus fell to the page rather than to the row's control"
    ).toBe(
      within(row).getByRole("button", {
        name: repoTextControlName("CORE-1", {
          resolvedRepo: "acme/web",
          words: CONTROL_NAME_WORDS,
        }),
      })
    );
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// A repository still out as the issue closes is refused (`ISSUE_CLOSED`): Dispatch writes no
// repository of a closed issue, so the refusal is said with no Retry, which Dispatch would refuse
// again.
test("a repository refused because the issue closed is said without Retry", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const reason = "CORE-1 is closed; reopen it first";
  const saving = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const { queryClient, unmount } = renderDetails();

  try {
    await user.click(
      await screen.findByRole("button", {
        name: repoTextControlName("CORE-1", { resolvedRepo: null, words: CONTROL_NAME_WORDS }),
      })
    );
    await user.click(await screen.findByRole("combobox", { name: repoControlName("CORE-1") }));
    await user.paste("acme/web");
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { repo: "acme/web" }, keyedAttempt)
    );

    // The issue closes while the write is out, and Dispatch refuses the write for it.
    drawIssue(getIssue, queryClient, closedIssue);
    saving.reject(new ApiError(409, { code: "ISSUE_CLOSED", error: reason }));

    const row = detailsRow(PROPERTY_LABELS.repository);
    await waitFor(() =>
      expect(
        within(row).queryByRole("alert")?.textContent,
        "the Repository row does not say why Dispatch refused the repository"
      ).toBe(reason)
    );
    expect(
      within(row).queryByRole("button", { name: CONTROL_LABELS.retry }),
      "the refusal of a repository on a closed issue offers a Retry Dispatch would refuse again"
    ).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

/** A second caller of the issue's writes on the page, as the header slot is: slots share no
 *  state, so each makes its own set (`slots/details.tsx`). */
function OtherWrites({ issue: details }: { issue: IssueDetails }): ReactNode {
  const writes: IssueHeaderWrites = useIssueHeaderWrites(
    details,
    stateForIssue(undefined, details.key)
  );
  return (
    <button onClick={() => writes.updateIssue.mutate({ status: "in_progress" })} type="button">
      Start it from the header
    </button>
  );
}

// The page never says two statuses. The header and Details each call the writes, so Details
// reads every pending write of the issue's fields by their one mutation key, whichever caller
// made it (`useIssueHeaderWrites.ts`'s `pendingStatus`).
test("Details' Status reads the status another caller of the writes is saving", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const saving = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const { unmount } = renderDetails((details) => <OtherWrites issue={details} />);

  try {
    await screen.findByText(PROPERTY_LABELS.status, { selector: "[data-page-chrome]" });
    expect(detailsRow(PROPERTY_LABELS.status)).toHaveTextContent(issueStatusWord("todo"));
    await user.click(screen.getByRole("button", { name: "Start it from the header" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        detailsRow(PROPERTY_LABELS.status),
        "Details' Status kept the saved status while the header's write of another was out"
      ).toHaveTextContent(issueStatusWord("in_progress"))
    );

    saving.resolve(narrowIssue({ ...issue, status: "in_progress" }));
    await waitFor(() =>
      expect(detailsRow(PROPERTY_LABELS.status)).toHaveTextContent(issueStatusWord("in_progress"))
    );
  } finally {
    unmount();
    restore();
  }
});

/** A second caller that saves a title and picks a status while that title is out, as the header
 *  does when the reader picks a status before the title they saved has landed. */
function RetitleThenStart({ issue: details }: { issue: IssueDetails }): ReactNode {
  const writes = useIssueHeaderWrites(details, stateForIssue(undefined, details.key));
  return (
    <>
      <button
        onClick={() => {
          writes.drafts.writeTitle("Review the spec again");
          writes.drafts.requestTitleSubmit();
        }}
        type="button"
      >
        Retitle it from the header
      </button>
      <button onClick={() => writes.pickStatus("in_progress")} type="button">
        Start it from the header
      </button>
    </>
  );
}

// A status picked while another write of the issue is out is held for every caller by
// `pickStatus` (`useIssueHeaderWrites.ts`), so Details draws it at once, and it is sent once,
// after that write.
test("Details' Status reads a status picked behind another write", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const retitling = Promise.withResolvers<Issue>();
  patchIssue
    .mockImplementationOnce(() => retitling.promise)
    .mockImplementationOnce(async () =>
      narrowIssue({ ...issue, status: "in_progress", title: "Review the spec again" })
    );
  const { queryClient, unmount } = renderDetails((details) => <RetitleThenStart issue={details} />);

  try {
    await screen.findByText(PROPERTY_LABELS.status, { selector: "[data-page-chrome]" });
    expect(detailsRow(PROPERTY_LABELS.status)).toHaveTextContent(issueStatusWord("todo"));
    await user.click(screen.getByRole("button", { name: "Retitle it from the header" }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith(
        "CORE-1",
        { title: "Review the spec again" },
        keyedAttempt
      )
    );

    await user.click(screen.getByRole("button", { name: "Start it from the header" }));
    await waitFor(() =>
      expect(
        detailsRow(PROPERTY_LABELS.status),
        "Details' Status kept the saved status while a pick held behind the header's title stood"
      ).toHaveTextContent(issueStatusWord("in_progress"))
    );
    expect(
      patchIssue,
      "the pick went out while the title write before it was out"
    ).toHaveBeenCalledTimes(1);

    retitling.resolve(narrowIssue({ ...issue, title: "Review the spec again" }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(
      patchIssue.mock.calls.map(([, input]) => input),
      "the held status did not go out after the title write it was held behind"
    ).toEqual([{ title: "Review the spec again" }, { status: "in_progress" }]);

    // Once no write is out, the row reads the status the server answered with.
    await waitFor(() => {
      expect(queryClient.isMutating()).toBe(0);
      expect(queryClient.getQueryData(issueQuery("CORE-1").queryKey)?.status).toBe("in_progress");
    });
    expect(detailsRow(PROPERTY_LABELS.status)).toHaveTextContent(issueStatusWord("in_progress"));
    expect(patchIssue, "the held status was sent more than once").toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    restore();
  }
});

/** The margin's own content, drawn through the fill the details slot put in its slot, as the
 *  shell's Details drawer draws it (`MarginSheet.tsx`'s `SheetBody`). */
function MarginBody(): ReactNode {
  return (
    <FilledMarginSlot fill={useMarginSlotFill()}>
      <p>A thread anchored in the document</p>
    </FilledMarginSlot>
  );
}

/** What a document's editor does to the margin while it is mounted (`ProofDocument.tsx`). */
function DocumentEditor(): ReactNode {
  const { registerDocument } = useMargin();
  return (
    <>
      <button
        onClick={() => registerDocument({ artifactId: "artifact-1" } as unknown as DocumentBridge)}
        type="button"
      >
        Open the document
      </button>
      <button onClick={() => registerDocument(undefined)} type="button">
        Close the document
      </button>
    </>
  );
}

// Details holds no card of the margin while no document's editor is on the page, and while one
// is, the margin's own cards stand above the sections, for a comment on a selection.
test("the margin's own cards are drawn only while a document's editor is on the page", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarginSlotProvider drawsSlot>
            <DetailsSlot issue={issue} route={{ key: "CORE-1", kind: "issue" }} />
            <MarginBody />
            <DocumentEditor />
          </MarginSlotProvider>
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const development = await screen.findByRole("button", {
      name: MARGIN_SECTION_NAMES.development,
    });
    expect(
      screen.queryByText("A thread anchored in the document"),
      "Details draws a card of the margin with no document's editor on the page"
    ).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Open the document" }));
    const card = await screen.findByText("A thread anchored in the document");
    expect(
      card.compareDocumentPosition(development) & Node.DOCUMENT_POSITION_FOLLOWING,
      "the margin's own cards are not above Details' sections"
    ).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "Close the document" }));
    await waitFor(() =>
      expect(screen.queryByText("A thread anchored in the document")).not.toBeInTheDocument()
    );
    expect(
      screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development }),
      "the panel was drawn again when the margin's cards went"
    ).toBe(development);
  } finally {
    view.unmount();
    restore();
  }
});

// A control elsewhere on the page that opens Details at one of its rows asks the drawer for that
// row first: the section that holds the row opens for it as its heading opens it, so the
// reader's kept choice follows, and a section already open stays open.
test("a request for the Labels row opens a closed Development in the drawer that holds the panel", async () => {
  const { restore } = stubIssueApi();
  const kept = userPreferenceStorageKey("alice", "issue.margin-sections");
  window.localStorage.setItem(kept, "development");
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarginSlotProvider drawsSlot>
            <DetailsSlot issue={issue} route={{ key: "CORE-1", kind: "issue" }} />
            <aside {...{ [DETAILS_ATTRIBUTE]: "" }}>
              <MarginBody />
            </aside>
          </MarginSlotProvider>
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const development = await screen.findByRole("button", {
      name: MARGIN_SECTION_NAMES.development,
    });
    await waitFor(() => expect(development).toHaveAttribute("aria-expanded", "false"));
    const drawer = development.closest(`aside[${DETAILS_ATTRIBUTE}]`);
    expect(drawer, "the panel is not in the drawer's aside").not.toBeNull();
    const request = (row: DetailsRow) =>
      act(() => {
        drawer?.dispatchEvent(new CustomEvent(DETAILS_ROW_REQUEST_EVENT, { detail: row }));
      });

    request("labels");
    expect(development, "a request for the Labels row left Development closed").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    expect(
      document.querySelector(`[${DETAILS_ROW_ATTRIBUTE}="labels"]`),
      "Development opened without its Labels row"
    ).not.toBeNull();
    expect(window.localStorage.getItem(kept), "the reader's kept choice did not follow").toBe("");

    request("labels");
    expect(development, "a request for a row of an open Development closed it").toHaveAttribute(
      "aria-expanded",
      "true"
    );
  } finally {
    view.unmount();
    restore();
    window.localStorage.removeItem(kept);
  }
});

// Each row draws its name one time. The Labels row of an issue with no label reads its name,
// None, and a control that adds the first label; that control's visible word is not the row's
// name again, which would read "Labels None Labels".
test("the Labels row of an issue with no label draws its name one time", async () => {
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    const row = await waitFor(() => rowAt("labels"));
    expect(row).toHaveTextContent(CONTROL_LABELS.none);
    const control = within(row).getByRole("button", {
      name: labelsControlName("CORE-1", { labels: [], saving: false, words: CONTROL_NAME_WORDS }),
    });
    expect(control, "the Labels row of an issue with no label draws no Edit labels").toBeEnabled();
    expect(control, "the Labels row's control is busy, and takes no press").not.toHaveAttribute(
      "aria-disabled",
      "true"
    );
    expect(
      (row.textContent ?? "").split(PROPERTY_LABELS.labels).length - 1,
      "the Labels row draws its name two times on an issue with no label"
    ).toBe(1);
  } finally {
    unmount();
    restore();
  }
});

/** The issue page's scope of keys, pushed as the page pushes it (`useIssueKeymap.ts`), so a key
 *  the details slot binds in that scope is read. */
function IssueKeys(): null {
  useKeymapScope("issue");
  return null;
}

/** The details slot and the margin's own content in a column, under the issue page's keys
 *  (`IssueKeys`), so the key `l` reads the labels' control the slot draws for `details`. */
function renderWithKeys(details: IssueDetails) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  return render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <IssueKeys />
          <MarginProvider>
            <MarginSlotProvider drawsSlot>
              <DetailsSlot issue={details} route={{ key: "CORE-1", kind: "issue" }} />
              <MarginBody />
            </MarginSlotProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** Whether the key `l` is on, as the list of keys says it. */
function labelsKey(): boolean | undefined {
  return appKeymap.describe().find((entry) => entry.id === "labels")?.enabled;
}

// On an issue with no label the labels' control is named for adding one (`labelsControlName`),
// and the add menu's Labels and the key `l` find it by that name. While both looked for "Edit
// labels", the menu listed no Labels on such an issue and `l` did nothing.
test("on an issue with no label, the Add menu's Labels and the key l reach the labels control", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  // The labels' popover reads the project's issues for the labels it offers.
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const view = renderWithKeys(issue);

  try {
    const control = within(await waitFor(() => rowAt("labels"))).getByRole("button", {
      name: labelsControlName("CORE-1", { labels: [], saving: false, words: CONTROL_NAME_WORDS }),
    });
    expect(control).toHaveAttribute("aria-expanded", "false");

    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.add }));
    await user.click(await screen.findByRole("menuitem", { name: PROPERTY_LABELS.labels }));
    await waitFor(() =>
      expect(control, "the add menu's Labels did not open the labels control").toHaveAttribute(
        "aria-expanded",
        "true"
      )
    );
    await user.keyboard("{Escape}");
    await waitFor(() => expect(control).toHaveAttribute("aria-expanded", "false"));

    await user.keyboard("l");
    await waitFor(() =>
      expect(control, "l did not open the labels control").toHaveAttribute("aria-expanded", "true")
    );
  } finally {
    view.unmount();
    listIssues.mockRestore();
    restore();
  }
});

// While a save of the labels is out, the labels' control is named for it ("Saving labels of
// CORE-1", `labelsControlName` given the page's `ANSWER_LINES`), and the key `l` finds it by that
// name; the control is then busy, `aria-disabled` and refusing to open, so `l` is off until the
// save lands. While `l` read only `disabled`, the list of keys named it enabled for the length of
// the save and the key pressed a control that opened nothing.
test("while the labels are saving, the key l is off, and on again once the save lands", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  // The labels' popover reads the project's issues for the labels it offers.
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const labelled: IssueDetails = { ...issue, labels: ["bug"] };
  const saving = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const view = renderWithKeys(labelled);
  const savingName = labelsControlName("CORE-1", {
    labels: [],
    saving: true,
    words: CONTROL_NAME_WORDS,
  });

  try {
    const row = await waitFor(() => rowAt("labels"));
    await user.click(
      within(row).getByRole("button", {
        name: labelsControlName("CORE-1", {
          labels: ["bug"],
          saving: false,
          words: CONTROL_NAME_WORDS,
        }),
      })
    );
    await user.click(await screen.findByRole("option", { name: "bug" }));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(
      await within(row).findByRole("button", { name: savingName }),
      "the labels control is not named for its save while the save is out"
    ).toBeInTheDocument();
    expect(
      labelsKey(),
      "l is enabled while the labels control it presses is busy with the save"
    ).toBe(false);

    saving.resolve(narrowIssue({ ...labelled, labels: [] }));
    await waitFor(() =>
      expect(within(row).queryByRole("button", { name: savingName })).not.toBeInTheDocument()
    );
    expect(labelsKey(), "l is not on again once the labels' save has landed").toBe(true);
  } finally {
    view.unmount();
    listIssues.mockRestore();
    restore();
  }
});

// `l` opens Details at the Labels row before it presses the labels' control
// (`useOpenDetailsAt`), so Details being closed - its drawer's `aside` hidden, and the control
// under it not drawn - leaves `l` on (`usableControl`, which reads a closed Details as no
// refusal), while an element inside the drawer that hides the row or takes it out of reach
// still turns it off.
test("with Details closed, l is on for the labels control, and off while its row inside Details is hidden or inert", async () => {
  const { restore } = stubIssueApi();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false, staleTime: Infinity } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <IssueKeys />
          <MarginProvider>
            <MarginSlotProvider drawsSlot>
              <DetailsSlot issue={issue} route={{ key: "CORE-1", kind: "issue" }} />
              {/* Closed, as the drawer draws it (`Margin.tsx`): `hidden`, which a browser draws
                  as `display: none` and happy-dom draws as nothing at all. */}
              <aside hidden style={{ display: "none" }} {...{ [DETAILS_ATTRIBUTE]: "" }}>
                <MarginBody />
              </aside>
            </MarginSlotProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const row = await waitFor(() => rowAt("labels"));
    expect(
      row.closest(`aside[${DETAILS_ATTRIBUTE}]`),
      "the Labels row is not in the closed drawer"
    ).toHaveAttribute("hidden");
    const control = row.querySelector(
      `button[aria-label="${labelsControlName("CORE-1", { labels: [], saving: false, words: CONTROL_NAME_WORDS })}"]`
    );
    expect(control, "the Labels row draws no enabled labels control").toBeEnabled();
    expect(control, "the Labels row's control is busy, and takes no press").not.toHaveAttribute(
      "aria-disabled",
      "true"
    );
    expect(labelsKey(), "l is off while Details is closed, though it opens Details first").toBe(
      true
    );
    for (const attribute of ["hidden", "inert"]) {
      row.setAttribute(attribute, "");
      expect(labelsKey(), `l is on while the Labels row inside Details is ${attribute}`).toBe(
        false
      );
      row.removeAttribute(attribute);
      expect(labelsKey(), `l is not on again once the Labels row is no longer ${attribute}`).toBe(
        true
      );
    }
  } finally {
    view.unmount();
    restore();
  }
});

// A closed Development keeps the Labels row on the page, hidden, and `l` reads a control under a
// hidden element as one that takes no press (`usableControl`): the key stays off while the reader
// keeps the section closed, as it was while a closed section drew no rows, and is on again once
// the section opens.
test("with Development closed, l is off, and on again once Development opens", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const view = renderWithKeys(issue);

  try {
    await waitFor(() => rowAt("labels"));
    expect(labelsKey(), "l is off while Development is open").toBe(true);
    const development = screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development });
    await user.click(development);
    expect(development).toHaveAttribute("aria-expanded", "false");
    expect(labelsKey(), "l is on while Development is closed").toBe(false);
    await user.click(development);
    expect(labelsKey(), "l is not on again once Development opens").toBe(true);
  } finally {
    view.unmount();
    restore();
    window.localStorage.removeItem(userPreferenceStorageKey("alice", "issue.margin-sections"));
  }
});

/** The project's one architecture source, so the components line draws its picker
 *  (`shared-writes.test.ts`'s `architectureSource`). */
const architectureSource: ArchitectureSource = {
  branch: "main",
  components: 1,
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  enabled: true,
  last_commit: "3be61df5be90",
  last_error: null,
  last_sync_at: "2026-09-17T00:00:00Z",
  project: "CORE",
  repo: "legion/legion",
};

// On an issue no component is attached to, the components picker's trigger draws "Set
// components" and is named for it inside `NoTooltips` (`componentsControlName` given the
// attachment the line draws), and the add menu's Components finds it by that name. While the
// menu looked for `main`'s "Edit components of CORE-1", it listed no Components on such an
// issue.
test("on an issue with no component, the Add menu's Components reaches the picker", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  // The fixture's issue is not attached. A source draws the picker, and the open picker reads
  // the project's architecture for the components it offers.
  const sources = spyOn(api, "listProjectArchitectureSources").mockResolvedValue([
    architectureSource,
  ]);
  const tree = spyOn(api, "getArchitecture").mockResolvedValue(architecture);
  const { unmount } = renderDetails();

  try {
    const picker = await screen.findByRole("button", {
      name: componentsControlName("CORE-1", {
        components: issue.components,
        saving: false,
        words: CONTROL_NAME_WORDS,
      }),
    });
    expect(picker).toHaveAttribute("aria-expanded", "false");

    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.add }));
    await user.click(await screen.findByRole("menuitem", { name: PROPERTY_LABELS.components }));
    await waitFor(() =>
      expect(picker, "the add menu's Components did not open the picker").toHaveAttribute(
        "aria-expanded",
        "true"
      )
    );
  } finally {
    unmount();
    tree.mockRestore();
    sources.mockRestore();
    restore();
  }
});

// The header's Set parent issue presses the Parent row's first control (`useOpenDetailsAt`,
// which presses a row's first control), so that control is the one that opens the parent's
// editor, and it stands before the parent's link.
test("the Parent row's first control opens the parent's editor, before the parent's link", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();

  try {
    const empty = renderDetails();
    await screen.findByText(PROPERTY_LABELS.parent, { selector: "[data-page-chrome]" });
    const none = document.querySelector(`[${DETAILS_ROW_ATTRIBUTE}="parent"]`);
    expect(firstUsableIn(none)).toHaveTextContent(CONTROL_LABELS.set_parent);
    expect(none).toHaveTextContent(CONTROL_LABELS.none);
    empty.unmount();

    getIssue.mockResolvedValue({ ...issue, parent: "CORE-2" });
    const { unmount } = renderDetails();
    const link = await screen.findByRole("link", { name: "CORE-2" });
    const row = link.closest(`[${DETAILS_ROW_ATTRIBUTE}="parent"]`);
    expect(row, "the parent's link is outside the Parent row").not.toBeNull();
    const control = firstUsableIn(row);
    expect(control).toHaveTextContent(CONTROL_LABELS.edit_parent);
    expect(
      (control?.compareDocumentPosition(link) ?? 0) & Node.DOCUMENT_POSITION_FOLLOWING,
      "the parent's link stands before the control that edits it"
    ).toBeTruthy();

    await user.click(control as HTMLElement);
    const box = await screen.findByLabelText(PROPERTY_LABELS.parent);
    await waitFor(() => expect(box, "the parent's editor took no focus").toHaveFocus());
    unmount();
  } finally {
    restore();
  }
});

// The parent is set, changed and cleared in its own row, from the control that row draws
// first, where the old page opened its editor from the overflow menu.
test("Details saves a parent from the Parent row's editor", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    await screen.findByText(PROPERTY_LABELS.parent, { selector: "[data-page-chrome]" });
    await user.click(
      within(detailsRow(PROPERTY_LABELS.parent)).getByRole("button", {
        name: CONTROL_LABELS.set_parent,
      })
    );
    await waitFor(() =>
      expect(
        screen.queryAllByLabelText(PROPERTY_LABELS.parent),
        "the Parent row's control opened no editor"
      ).toHaveLength(1)
    );
    await user.click(screen.getByLabelText(PROPERTY_LABELS.parent));
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_parent }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { parent: "CORE-9" }, keyedAttempt)
    );
    // The row draws the parent it saved as the way to it.
    const link = await within(detailsRow(PROPERTY_LABELS.parent)).findByRole("link", {
      name: "CORE-9",
    });
    expect(link).toHaveAttribute("href", "/issues/CORE-9");
  } finally {
    unmount();
    restore();
  }
});

test("Details clears the parent with an empty editor save", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, parent: "CORE-9" });
  const { unmount } = renderDetails();

  try {
    await screen.findByRole("link", { name: "CORE-9" });
    await user.click(
      within(detailsRow(PROPERTY_LABELS.parent)).getByRole("button", {
        name: CONTROL_LABELS.edit_parent,
      })
    );
    const parent = (await screen.findByLabelText(PROPERTY_LABELS.parent)) as HTMLInputElement;
    expect(parent.value).toBe("CORE-9");
    await user.clear(parent);
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_parent }));

    await waitFor(() =>
      expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { parent: null }, keyedAttempt)
    );
    // The row's first control is the one the header's Set parent issue presses.
    await waitFor(() =>
      expect(
        firstUsableIn(detailsRow(PROPERTY_LABELS.parent)),
        "the Parent row still offers to edit a parent the issue no longer has"
      ).toHaveTextContent(CONTROL_LABELS.set_parent)
    );
    expect(screen.queryByRole("link", { name: "CORE-9" })).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// The editor closes as the save goes out, so a parent Dispatch refuses is said in the row the
// reader saved it from, in Dispatch's own words, and not as the line for a write that failed. The
// focus is back on the row's control by then, so the line is an alert: it is announced as it
// arrives.
test("a parent Dispatch refuses is said in the Parent row with Dispatch's reason", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const reason = "would create a cycle: CORE-1 → CORE-9 → CORE-1";
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(409, { code: "PARENT_INPUT", error: reason }))
  );
  const { unmount } = renderDetails();

  try {
    await screen.findByText(PROPERTY_LABELS.parent, { selector: "[data-page-chrome]" });
    await user.click(
      within(detailsRow(PROPERTY_LABELS.parent)).getByRole("button", {
        name: CONTROL_LABELS.set_parent,
      })
    );
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.parent));
    await user.paste("CORE-9");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_parent }));

    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        detailsRow(PROPERTY_LABELS.parent).querySelector(`#${PARENT_ERROR_ID}`)?.textContent,
        "the Parent row does not say why Dispatch refused the parent"
      ).toBe(reason)
    );
    expect(
      within(detailsRow(PROPERTY_LABELS.parent)).queryByRole("alert")?.textContent,
      "the Parent row does not announce why Dispatch refused the parent"
    ).toBe(reason);
    expect(screen.queryByText("Could not update this issue.")).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// The route's save is Details' own write, and inline, so the write store leaves its refusal to
// the row: a route Dispatch refuses is said where it was saved, in Dispatch's words and with
// Retry, as a refused parent is in the Parent row.
test("a route Dispatch refuses is said in the Messages go to row with Dispatch's reason and Retry", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const route = "role:legion-controller-core";
  const reason = "invalid route";
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "ROUTE_INVALID", error: reason }))
  );
  const { unmount } = renderDetails();

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.paste(route);
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledWith("CORE-1", { route }, keyedAttempt));

    await waitFor(() =>
      expect(
        within(rowAt("route")).queryByRole("alert")?.textContent,
        "the Messages go to row does not say why Dispatch refused the route"
      ).toBe(reason)
    );
    await user.click(within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.retry }));
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(
      patchIssue.mock.calls.map(([, input]) => input),
      "Retry did not send the refused route again"
    ).toEqual([{ route }, { route }]);
    await waitFor(() =>
      expect(
        within(rowAt("route")).queryByRole("alert"),
        "the refusal is still said after the route landed"
      ).not.toBeInTheDocument()
    );
  } finally {
    unmount();
    restore();
  }
});

// The refusal's line was drawn only while the route's save stood refused, so a press of its Retry
// took the line, and the Retry with it, away while the save it sent again was out: the focus of a
// reader who pressed Retry by keyboard went to the page's body, and stayed there when Dispatch
// refused the route again and the line came back. The line stays, with its Retry holding the
// focus, until the save is answered; a second refusal is drawn afresh with Dispatch's new reason;
// and once the route lands and the line goes, the focus goes to the row's own control rather than
// to the page.
test("the refused route's Retry keeps the focus while the save is out and refused again, and hands it to the row's control once the route lands", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const route = "role:legion-controller-core";
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "ROUTE_INVALID", error: "invalid route" }))
  );
  const { unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();
  const retryOfRoute = () =>
    within(rowAt("route")).queryByRole("button", { name: CONTROL_LABELS.retry });

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.paste(route);
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await waitFor(() =>
      expect(within(rowAt("route")).queryByRole("alert")?.textContent).toBe("invalid route")
    );
    const refused = within(rowAt("route")).getByRole("alert");
    const retry = retryOfRoute();
    if (retry === null) throw new Error("the refused route offers no Retry");

    const retried = Promise.withResolvers<Issue>();
    patchIssue.mockImplementationOnce(() => retried.promise);
    act(() => retry.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    expect(
      retryOfRoute(),
      "the line and its Retry went while the route they sent again is out"
    ).toBe(retry);
    expect(
      document.activeElement,
      "Retry lost the focus while the route it sent again is out"
    ).toBe(retry);
    await waitFor(() =>
      expect(
        retry,
        "Retry does not refuse a press while the route it sent again is out"
      ).toHaveAttribute("aria-disabled", "true")
    );
    expect(document.activeElement, "Retry lost the focus once it refused a press").toBe(retry);
    await user.keyboard("{Enter}");
    expect(patchIssue, "a press of Retry while its save is out sent another").toHaveBeenCalledTimes(
      2
    );

    const reason = "no session holds role:legion-controller-core";
    await act(async () =>
      retried.reject(new ApiError(400, { code: "ROUTE_INVALID", error: reason }))
    );
    await waitFor(() =>
      expect(
        within(rowAt("route")).queryByRole("alert")?.textContent,
        "the row does not say why Dispatch refused the route again"
      ).toBe(reason)
    );
    expect(
      within(rowAt("route")).getByRole("alert"),
      "the second refusal was not drawn afresh, so a screen reader would not say it"
    ).not.toBe(refused);
    expect(retryOfRoute(), "the line came back with another Retry").toBe(retry);
    expect(
      document.activeElement,
      "Retry lost the focus when Dispatch refused the route again"
    ).toBe(retry);
    expect(
      retry,
      "Retry still refuses a press after Dispatch refused the route again"
    ).not.toHaveAttribute("aria-disabled", "true");

    await user.keyboard("{Enter}");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(3));
    await waitFor(() =>
      expect(
        within(rowAt("route")).queryByRole("alert"),
        "the refusal is still said after the route landed"
      ).not.toBeInTheDocument()
    );
    expect(
      document.activeElement,
      "the route landed, and the focus fell to the page rather than to the row's control"
    ).toBe(within(rowAt("route")).getByRole("button", { name: `Messages default to ${route}` }));
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// A refused route stays the draft until the reader leaves the editor, which puts the saved route
// back: the refusal is said while the draft still holds the route Dispatch refused, and not once
// it no longer does, when its Retry would send nothing.
test("a refused route is said only while the draft still holds it", async () => {
  const user = userEvent.setup();
  const { patchIssue, restore } = stubIssueApi();
  const route = "role:legion-controller-core";
  patchIssue.mockImplementationOnce(() =>
    Promise.reject(new ApiError(400, { code: "ROUTE_INVALID", error: "invalid route" }))
  );
  const { unmount } = renderDetails();

  try {
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.paste(route);
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await waitFor(() =>
      expect(within(rowAt("route")).queryByRole("alert")?.textContent).toBe("invalid route")
    );

    await user.click(
      within(rowAt("route")).getByRole("button", { name: `Messages default to ${route}` })
    );
    await user.click(within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.cancel }));
    expect(
      within(rowAt("route")).queryByRole("button", { name: "Messages default to no route" }),
      "Cancel did not put the saved route back"
    ).toBeInTheDocument();
    expect(
      within(rowAt("route")).queryByRole("alert"),
      "the row still says the refusal of a route the reader left, with a Retry that sends nothing"
    ).not.toBeInTheDocument();
  } finally {
    unmount();
    restore();
  }
});

// A route still out as the issue closes is refused (`ISSUE_CLOSED`): Dispatch saves no route of
// a closed issue, so the refusal is said with no Retry, which would send nothing, and the row's
// control, disabled now, names the route the issue holds rather than the one refused.
test("a route refused because the issue closed is said without Retry, and the row names the saved route", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const saved = "role:legion-controller-core";
  const refused = "role:legion-reviewer";
  const reason = "CORE-1 is closed; reopen it first";
  getIssue.mockResolvedValue({ ...issue, route: saved });
  const saving = Promise.withResolvers<Issue>();
  patchIssue.mockImplementationOnce(() => saving.promise);
  const { queryClient, unmount } = renderDetails();

  try {
    await user.click(await screen.findByRole("button", { name: `Messages default to ${saved}` }));
    await user.clear(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.paste(refused);
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await waitFor(() =>
      expect(patchIssue).toHaveBeenCalledWith("CORE-1", { route: refused }, keyedAttempt)
    );

    // The issue closes while the save is out, and Dispatch refuses the save for it.
    drawIssue(getIssue, queryClient, { ...closedIssue, route: saved });
    saving.reject(new ApiError(409, { code: "ISSUE_CLOSED", error: reason }));

    await waitFor(() =>
      expect(
        within(rowAt("route")).queryByRole("alert")?.textContent,
        "the Messages go to row does not say why Dispatch refused the route"
      ).toBe(reason)
    );
    expect(
      within(rowAt("route")).queryByRole("button", { name: CONTROL_LABELS.retry }),
      "the refusal of a route on a closed issue offers a Retry that sends nothing"
    ).not.toBeInTheDocument();
    expect(
      within(rowAt("route"))
        .getByRole("button", { name: /^Messages default to / })
        .getAttribute("aria-label"),
      "the route's control does not name the route the closed issue holds"
    ).toBe(`Messages default to ${saved}`);
  } finally {
    unmount();
    restore();
  }
});

// The box an editor of a row holds the focus in goes with the editor, so leaving it - by a save,
// by Cancel or by Escape - gives the focus back to the row's own control, not to the page.
test("leaving the parent's or the route's editor gives the focus back to its row's control", async () => {
  const user = userEvent.setup();
  const { restore } = stubIssueApi();
  const { unmount } = renderDetails();
  const control = (row: DetailsRow) => firstUsableIn(rowAt(row)) ?? null;

  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.set_parent }));
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.parent));
    await user.paste("CORE-9");
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(document.activeElement, "Save parent by Enter left the focus on the page").toBe(
        control("parent")
      )
    );

    await within(rowAt("parent")).findByRole("link", { name: "CORE-9" });
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.edit_parent }));
    await user.click(within(rowAt("parent")).getByRole("button", { name: CONTROL_LABELS.cancel }));
    expect(document.activeElement, "Cancel of the parent's editor left the focus on the page").toBe(
      control("parent")
    );
    expect(control("parent")).toHaveTextContent(CONTROL_LABELS.edit_parent);

    await user.click(screen.getByRole("button", { name: "Messages default to no route" }));
    await user.click(within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.cancel }));
    expect(document.activeElement, "Cancel of the route's editor left the focus on the page").toBe(
      control("route")
    );
  } finally {
    unmount();
    restore();
  }
});

// On a closed issue the Parent row draws no control and the route's control is disabled, so
// leaving either editor once the issue had closed under it gave the focus to no control that
// takes it, and it stayed on the page's body. It goes to Development's heading, the button that
// opens and closes the section these rows are in, where the row holds nothing else to take it.
test("on a closed issue, leaving the parent's or the route's editor hands the focus to Development's heading", async () => {
  const user = userEvent.setup();
  const { getIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderDetails();
  const heading = () => screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development });

  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.set_parent }));
    await screen.findByLabelText(PROPERTY_LABELS.parent);
    drawIssue(getIssue, queryClient, closedIssue);
    await user.click(within(rowAt("parent")).getByRole("button", { name: CONTROL_LABELS.cancel }));
    expect(
      document.activeElement,
      "Cancel of the parent's editor on a closed issue did not give the focus to Development's heading"
    ).toBe(heading());

    drawIssue(getIssue, queryClient, issue);
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    await screen.findByLabelText(PROPERTY_LABELS.route);
    drawIssue(getIssue, queryClient, closedIssue);
    await user.click(within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.cancel }));
    expect(
      document.activeElement,
      "Cancel of the route's editor on a closed issue did not give the focus to Development's heading"
    ).toBe(heading());
  } finally {
    unmount();
    restore();
  }
});

// The refused route's Retry handed its focus to the row's control alone. A closed issue's route
// control is disabled and takes no focus, so a Retry that went as the issue closed left the focus
// on the page's body; and while the route's editor is open the row draws no such control, so a
// Retry pressed there, whose route then landed, did the same. The focus goes on to the editor's
// box while the editor is open, and to Development's heading where the row holds nothing to take
// it.
test("the refused route's Retry hands its focus to the editor's box while it is open, and on a closed issue to Development's heading", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  const route = "role:legion-controller-core";
  const refuse = () =>
    patchIssue.mockImplementationOnce(() =>
      Promise.reject(new ApiError(400, { code: "ROUTE_INVALID", error: "invalid route" }))
    );
  const { queryClient, unmount } = renderDetails();
  const retryOfRoute = () =>
    within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.retry });
  const refused = () =>
    waitFor(() =>
      expect(within(rowAt("route")).queryByRole("alert")?.textContent).toBe("invalid route")
    );

  try {
    refuse();
    await user.click(await screen.findByRole("button", { name: "Messages default to no route" }));
    await user.click(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.paste(route);
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await refused();
    // The editor opened again over the refused route, which the line still says.
    await user.click(
      within(rowAt("route")).getByRole("button", { name: `Messages default to ${route}` })
    );
    const box = await screen.findByLabelText(PROPERTY_LABELS.route);
    act(() => retryOfRoute().focus());
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(
        within(rowAt("route")).queryByRole("alert"),
        "the refusal is still said after the route landed"
      ).not.toBeInTheDocument()
    );
    expect(
      document.activeElement,
      "the route landed with its editor open, and the focus did not go to the editor's box"
    ).toBe(box);

    await user.click(within(rowAt("route")).getByRole("button", { name: CONTROL_LABELS.cancel }));
    refuse();
    await user.click(screen.getByRole("button", { name: `Messages default to ${route}` }));
    await user.clear(await screen.findByLabelText(PROPERTY_LABELS.route));
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.save_route }));
    await refused();
    act(() => retryOfRoute().focus());
    drawIssue(getIssue, queryClient, { ...closedIssue, route });
    expect(
      within(rowAt("route")).queryByRole("button", { name: CONTROL_LABELS.retry }),
      "the refusal of a route on a closed issue offers a Retry that sends nothing"
    ).toBeNull();
    expect(
      document.activeElement,
      "the Retry went as the issue closed, and the focus did not go to Development's heading"
    ).toBe(screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development }));
  } finally {
    unmount();
    restore();
  }
});

// Another session's close disables the route's control, and the focus a reader had on it went to
// the page's body (`emulateChromiumFocusFixup` moves it there, as Chromium does), since no hand-off
// of the panel answers a control that turns disabled. It goes to Development's heading, the button
// that opens and closes the section the row is in.
test("a close made elsewhere hands the focus of the route's control it disables to Development's heading", async () => {
  const { getIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    const control = await screen.findByRole("button", { name: "Messages default to no route" });
    act(() => control.focus());
    drawIssue(getIssue, queryClient, closedIssue);
    await waitFor(() =>
      expect(control, "the close left the route's control enabled").toBeDisabled()
    );
    expect(
      document.activeElement,
      "the close disabled the route's control, and the focus did not go to Development's heading"
    ).toBe(screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development }));
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// A press on Details' plain text moves the focus to the page's body and sends no `focusin`, so
// the panel still kept the route's control the reader had left, and a close made elsewhere then
// moved the focus to Development's heading although the reader was no longer in the section. A
// control the reader leaves while it still takes a press is forgotten, and the close moves
// nothing.
test("a close made elsewhere leaves the focus on the body once the reader left the route's control", async () => {
  const { getIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    const control = await screen.findByRole("button", { name: "Messages default to no route" });
    act(() => control.focus());
    // As a press on plain text does: the focus leaves the control for the body.
    act(() => control.blur());
    expect(document.activeElement, "the blur left the focus on the route's control").toBe(
      document.body
    );
    drawIssue(getIssue, queryClient, closedIssue);
    await waitFor(() =>
      expect(control, "the close left the route's control enabled").toBeDisabled()
    );
    expect(
      document.activeElement,
      "the reader had left the route's control, and the close moved the focus to Development's heading"
    ).toBe(document.body);
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// A switch to another window or tab blurs the route's control but leaves it the document's
// active element, where the focus comes back on return, so the panel still keeps it, and a close
// made elsewhere meanwhile hands its focus to Development's heading as it would with the reader
// still on the page.
test("a window switch keeps the route's control noted, and a close made elsewhere then hands its focus to Development's heading", async () => {
  const { getIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    const control = await screen.findByRole("button", { name: "Messages default to no route" });
    act(() => control.focus());
    // As a switch to another window does: the control hears the focus leave (`focusout` is what
    // React's `onBlur` listens to) and stays the document's active element.
    act(() => {
      control.dispatchEvent(new FocusEvent("focusout", { bubbles: true }));
    });
    expect(
      document.activeElement,
      "the window switch moved the focus off the route's control"
    ).toBe(control);
    drawIssue(getIssue, queryClient, closedIssue);
    await waitFor(() =>
      expect(control, "the close left the route's control enabled").toBeDisabled()
    );
    expect(
      document.activeElement,
      "the reader only switched windows, and the close did not move the focus to Development's heading"
    ).toBe(screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development }));
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// A closed issue's Parent row draws no control, so another session's close takes Set parent issue
// off the page, and the focus a reader had on it went to the page's body. It goes to Development's
// heading, the button that opens and closes the section the row is in.
test("a close made elsewhere hands the focus of Set parent issue it takes away to Development's heading", async () => {
  const { getIssue, restore } = stubIssueApi();
  const { queryClient, unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    const control = await screen.findByRole("button", { name: CONTROL_LABELS.set_parent });
    act(() => control.focus());
    drawIssue(getIssue, queryClient, closedIssue);
    await waitFor(() =>
      expect(control, "the close left Set parent issue on the page").not.toBeInTheDocument()
    );
    expect(
      document.activeElement,
      "the close took Set parent issue away, and the focus did not go to Development's heading"
    ).toBe(screen.getByRole("button", { name: MARGIN_SECTION_NAMES.development }));
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// The parent's link takes a press on a closed issue too, so a close made elsewhere leaves the focus
// a reader has on it where it is.
test("a close made elsewhere leaves the focus on the parent's link, which still takes a press", async () => {
  const { getIssue, restore } = stubIssueApi();
  getIssue.mockResolvedValue({ ...issue, parent: "CORE-2" });
  const { queryClient, unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    const link = await screen.findByRole("link", { name: "CORE-2" });
    act(() => link.focus());
    drawIssue(getIssue, queryClient, { ...closedIssue, parent: "CORE-2" });
    await waitFor(() =>
      expect(
        within(rowAt("parent")).queryByRole("button", { name: CONTROL_LABELS.edit_parent }),
        "the close left Edit parent issue on the page"
      ).toBeNull()
    );
    expect(document.activeElement, "the close moved the focus off the parent's link").toBe(link);
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// A control busy with its own write refuses a press (`aria-disabled`) but keeps the focus, and a
// close made elsewhere takes nothing from it: the blocked-by editor stays open on a closed issue
// while its save is out, and the focus a reader had on its Save stays there. Read as a control
// that takes no press (`usableControl`), the busy Save passed for one the close had disabled,
// and the close moved its focus to Development's heading.
test("a close made elsewhere leaves the focus on the blocked-by editor's Save while its save is out", async () => {
  const user = userEvent.setup();
  const { getIssue, patchIssue, restore } = stubIssueApi();
  // The save is held out for the whole case.
  patchIssue.mockImplementationOnce(() => new Promise(() => undefined));
  const { queryClient, unmount } = renderDetails();
  const stopBlurring = emulateChromiumFocusFixup();

  try {
    await user.click(
      await screen.findByRole("button", { name: blockedByControlName("CORE-1", 0) })
    );
    await user.click(await screen.findByLabelText("Issues CORE-1 waits on"));
    await user.paste("CORE-3");
    const save = screen.getByRole("button", { name: CONTROL_LABELS.save_blocked_by });
    await user.click(save);
    await waitFor(() =>
      expect(save, "Save takes a press while its save is out").toHaveAttribute(
        "aria-disabled",
        "true"
      )
    );
    expect(document.activeElement, "the save moved the focus off Save").toBe(save);
    drawIssue(getIssue, queryClient, closedIssue);
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Messages default to no route" }),
        "the close left the route's control enabled"
      ).toBeDisabled()
    );
    expect(document.activeElement, "the close moved the focus off the busy Save").toBe(save);
  } finally {
    stopBlurring();
    unmount();
    restore();
  }
});

// The pins are read for their words only where there is a pin, so an issue with none costs no
// request of the questions, comments and messages.
test("with no pin, Pinned reads None and sends no request", async () => {
  const { pinReads, restore } = stubIssueApi();
  const { unmount } = renderDetails();

  try {
    const pins = await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    expect(pins).toHaveTextContent(CONTROL_LABELS.none);
    expect(within(pins).queryAllByRole("link")).toHaveLength(0);
    // Every other read of the panel has answered by the time the repository's has.
    await waitFor(() => expect(api.listRepoProjects).toHaveBeenCalled());
    expect(pinReads.asks, "Pinned read the issue's questions with no pin").not.toHaveBeenCalled();
    expect(
      pinReads.comments,
      "Pinned read the issue's comments with no pin"
    ).not.toHaveBeenCalled();
    expect(
      pinReads.messages,
      "Pinned read the issue's messages with no pin"
    ).not.toHaveBeenCalled();
  } finally {
    unmount();
    restore();
  }
});

// Pinned said None before the reader's state had answered, so a reader with pins read for a
// moment that they had none. Until that state answers the section draws its heading alone, and
// once it answers with no pin it says None.
test("Pinned says no None until the reader's state has answered, and None once it holds no pin", async () => {
  const { getMyState, restore } = stubIssueApi();
  const state = Promise.withResolvers<UserState>();
  getMyState.mockImplementation(() => state.promise);
  const { unmount } = renderDetails();

  try {
    const pins = await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    await waitFor(() => expect(getMyState).toHaveBeenCalled());
    expect(pins, "Pinned says None before the reader's state has answered").not.toHaveTextContent(
      CONTROL_LABELS.none
    );
    await act(async () =>
      state.resolve({ "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 } })
    );
    await waitFor(() =>
      expect(
        pins,
        "Pinned does not say None once the reader's state holds no pin"
      ).toHaveTextContent(CONTROL_LABELS.none)
    );
  } finally {
    unmount();
    restore();
  }
});

// The outline reads the questions, the comments and the messages under the keys Pinned reads
// them by, and keeps them fresh by events (`useIssueTree.ts`), so Pinned reads them with the same
// options: a window focus past the app's 30 s staleness reads none of them again. The reader's
// state keeps the app's defaults, so its second read shows the focus was handled.
test("with a pin, a window focus past 30 s reads none of Pinned's questions, comments and messages again", async () => {
  const { getMyState, pinReads, restore } = stubIssueApi();
  getMyState.mockResolvedValue(
    pinnedState(conversationItemId("comment", "4a7c2e19-8b3d-4f60-a1e5-6c9d0b2f3e47"))
  );
  const client = appClient();
  const { unmount } = renderDetails(undefined, undefined, client);

  try {
    await waitFor(() => {
      for (const read of Object.values(pinReads)) expect(read).toHaveBeenCalledTimes(1);
      expect(client.isFetching()).toBe(0);
    });
    await focusPastStaleness(client);
    await waitFor(() => expect(getMyState).toHaveBeenCalledTimes(2));
    expect(pinReads.asks, "a window focus read Pinned's questions again").toHaveBeenCalledTimes(1);
    expect(pinReads.comments, "a window focus read Pinned's comments again").toHaveBeenCalledTimes(
      1
    );
    expect(pinReads.messages, "a window focus read Pinned's messages again").toHaveBeenCalledTimes(
      1
    );
  } finally {
    unmount();
    restore();
    setSystemTime();
    focusManager.setFocused(undefined);
  }
});

/** The reader's state with the conversation `id` (`conversationItemId`'s `<kind>:<id>`) of
 *  CORE-1 pinned, as `pins.ts` writes a pin into `dismissed`. */
function pinnedState(id: string): UserState {
  return {
    "CORE-1": { dismissed: [pinnedItemMarker(id)], last_read_seq: 0, pinned: false, seq: 0 },
  };
}

/** A comment of CORE-1 as the comments read answers it, with the server's reading of its body
 *  where `tree` is given. */
function commentOf(id: string, body: string, tree?: DisplayTree): CommentWithTrees {
  return {
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body,
    ...(tree === undefined ? {} : { body_tree: tree }),
    created_at: "2026-09-09T00:00:00Z",
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

// Until the read that holds a pin's words answers, the pin is drawn by the page's word for its
// kind, never by the id it was pinned under; once it answers, by its first line.
test("a pin whose words are not read yet is drawn by its kind's word, never its id", async () => {
  const id = "5b0f9e64-2c1d-4a7e-8f3b-91d2c6a4e870";
  const { getMyState, pinReads, restore } = stubIssueApi();
  getMyState.mockResolvedValue(pinnedState(conversationItemId("comment", id)));
  const comments = Promise.withResolvers<CommentWithTrees[]>();
  pinReads.comments.mockImplementation(() => comments.promise);
  const { unmount } = renderDetails();

  try {
    const pins = await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    const link = await within(pins).findByRole("link");
    await waitFor(() => expect(pinReads.comments).toHaveBeenCalled());
    expect(
      link.textContent,
      "the pin is not drawn by its kind's word while its words are unread"
    ).toBe(NODE_TYPE_NAMES.comment);
    for (const part of id.split("-")) {
      expect(link.textContent, "the pin is drawn by its comment's id").not.toContain(part);
    }

    comments.resolve([commentOf(id, "Ship the fix tonight. Then watch the board.")]);
    await waitFor(() =>
      expect(
        within(pins).getByRole("link").textContent,
        "the pin is not drawn by its comment's first line once the comments are read"
      ).toBe("Ship the fix tonight.")
    );
  } finally {
    unmount();
    restore();
  }
});

// No read holds a handoff's words, so its pin is drawn by the page's word for it, and it opens
// the history, where the handoff is.
test("a pinned handoff is drawn by its kind's word and opens the history", async () => {
  const { getMyState, restore } = stubIssueApi();
  getMyState.mockResolvedValue(
    pinnedState(conversationItemId("handoff", "8e2d4c1a-7b3f-4e9d-a6c5-0f1b2d3e4a5c"))
  );
  const { unmount } = renderDetails();

  try {
    const pins = await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    const link = await within(pins).findByRole("link");
    expect(link.textContent).toBe(NODE_TYPE_NAMES.handoff);
    expect(link, "the handoff's pin does not open the history").toHaveAttribute(
      "href",
      "/issues/CORE-1/activity"
    );
  } finally {
    unmount();
    restore();
  }
});

// A pin's line is its record's words as its row in the outline reads them, from the server's
// reading of the text, so the symbols that format the body are not drawn as text.
test("a pinned comment written in markdown is drawn by its words, without the marks", async () => {
  const id = "3c9a7e15-6d2b-4f80-b1e4-7a5c9d0e2f36";
  const { getMyState, pinReads, restore } = stubIssueApi();
  getMyState.mockResolvedValue(pinnedState(conversationItemId("comment", id)));
  const reading: DisplayTree = {
    content: [
      {
        content: [
          { marks: [{ type: "strong" }], text: "Bold", type: "text" },
          { text: " words. Then more.", type: "text" },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  pinReads.comments.mockResolvedValue([commentOf(id, "**Bold** words. Then more.", reading)]);
  const { unmount } = renderDetails();

  try {
    const pins = await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    await waitFor(() =>
      expect(
        within(pins).getByRole("link").textContent,
        "the pin is not drawn by its comment's words"
      ).toBe("Bold words.")
    );
    expect(within(pins).getByRole("link").textContent).not.toContain("*");
  } finally {
    unmount();
    restore();
  }
});

// An Unpin takes its pin's row away, so the focus goes on to the next pin's link, else the one
// before it, else the Pinned heading, rather than falling to the page.
test("Unpin gives the focus to the next pin, else the one before, else the Pinned heading", async () => {
  const user = userEvent.setup();
  const { getMyState, pinReads, restore } = stubIssueApi();
  const [first, second, third] = [
    "1d4f0a6e-3b2c-4e8d-9a7f-5c6b1e2d3f40",
    "2e5a1b7f-4c3d-4f9e-8b6a-6d7c2f3e4a51",
    "3f6b2c8a-5d4e-4a0f-9c7b-7e8d3a4f5b62",
  ] as const;
  getMyState.mockResolvedValue({
    "CORE-1": {
      dismissed: [first, second, third].map((id) =>
        pinnedItemMarker(conversationItemId("comment", id))
      ),
      last_read_seq: 0,
      pinned: false,
      seq: 0,
    },
  });
  pinReads.comments.mockResolvedValue([
    commentOf(first, "First pin."),
    commentOf(second, "Second pin."),
    commentOf(third, "Third pin."),
  ]);
  // Each unpin is held out, so the page shows its own state of the pins throughout.
  const putIssueState = spyOn(api, "putIssueState").mockReturnValue(new Promise(() => undefined));
  const { unmount } = renderDetails();
  const unpin = async (id: string) => {
    const row = document.querySelector(`[data-pin="${conversationItemId("comment", id)}"]`);
    if (!(row instanceof HTMLElement)) throw new Error(`Pinned draws no row for ${id}`);
    act(() => within(row).getByRole("button", { name: CONTROL_LABELS.unpin }).focus());
    await user.keyboard("{Enter}");
  };

  try {
    const pins = await screen.findByRole("region", { name: CONTROL_LABELS.pinned });
    await within(pins).findByRole("link", { name: "Third pin." });

    await unpin(second);
    await waitFor(() => expect(within(pins).getAllByRole("link")).toHaveLength(2));
    expect(document.activeElement, "Unpin did not give the focus to the next pin").toBe(
      within(pins).getByRole("link", { name: "Third pin." })
    );

    await unpin(third);
    await waitFor(() => expect(within(pins).getAllByRole("link")).toHaveLength(1));
    expect(
      document.activeElement,
      "Unpin of the last pin did not give the focus to the one before it"
    ).toBe(within(pins).getByRole("link", { name: "First pin." }));

    await unpin(first);
    await waitFor(() => expect(pins).toHaveTextContent(CONTROL_LABELS.none));
    expect(
      document.activeElement,
      "Unpin of the only pin did not give the focus to the Pinned heading"
    ).toBe(within(pins).getByRole("heading", { name: CONTROL_LABELS.pinned }));
  } finally {
    unmount();
    putIssueState.mockRestore();
    restore();
  }
});

// Details keeps three places nothing fills yet (`details-more.tsx`): the people, the focused
// row's facts and the history. Until something fills them, each draws nothing.
test("Details' places for the people, the focused row's facts and the history draw nothing", () => {
  for (const Place of [DetailsPeople, DetailsFocusedNode, DetailsHistory]) {
    const view = render(<Place issue={issue} state={undefined} />);
    expect(view.container.innerHTML, `${Place.name} drew something`).toBe("");
    view.unmount();
  }
});
