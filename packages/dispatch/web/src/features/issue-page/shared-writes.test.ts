import { expect, jest, spyOn, test } from "bun:test";
import {
  hashKey,
  QueryClient,
  QueryClientProvider,
  type QueryKey,
  useQuery,
} from "@tanstack/react-query";
import {
  act,
  type RenderResult,
  render,
  renderHook,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement, type ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { realClock, settle } from "../../__tests__/fake-clock";
import { renderWithQueries } from "../../__tests__/query-provider";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { ApiError, api, NetworkError } from "../../api/client";
import { landIssueWrite } from "../../api/issue-cache";
import { userStateQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type {
  ArchitectureSource,
  ArtifactApproval,
  Issue,
  IssueComponents,
  IssueDetails,
  UserIssueState,
  UserState,
} from "../../api/types";
import { withoutEllipsis } from "../../components/tooltips";
import {
  pageStatusDoneText,
  pageStatusIdleText,
  pageStatusProgressText,
} from "../../theme/classes";
import { ApprovalSteps } from "../issue/ApprovalSteps";
import { AssigneeControl, assigneeControlName } from "../issue/AssigneeControl";
import type { StepEntry } from "../issue/gated-steps";
import { componentsControlName, IssueComponentsLine } from "../issue/IssueComponentsLine";
import { IssueLabels, labelsControlName } from "../issue/IssueLabels";
import { stateForIssue } from "../issue/pins";
import { RepoControl, repoControlName } from "../issue/RepoControl";
import { issue as marginIssue } from "../margin/margin-fixture";
import { IssuePriorityLine } from "../priorities/IssuePriorityLine";
import { issueStatuses } from "../project/board-model";
import { stoppedWriteNote } from "../writes/keyed-write";
import { isLive, writesOf } from "../writes/write-store";
import { narrowIssue } from "./issue-fixtures";
import { statusGlyph, statusTone } from "./issue-status";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import { StatusGlyph } from "./StatusGlyph";
import {
  architecture,
  doc,
  draw,
  SPEC_ID,
  undraw,
  VERIFICATION_ID,
} from "./shared-controls-fixture";
import { issueUpdateLabel, statusPickOnly, useIssueHeaderWrites } from "./useIssueHeaderWrites";

/** What the new issue page's header and Details share. */

/** The issue both callers write: the margin fixture's issue, at a later event and admitted. */
const issue: IssueDetails = { ...marginIssue, last_seq: 250, status: "todo" };

const state: UserIssueState = { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 };

/** The header and the Details panel of one issue page: two callers of the hook on one client. */
function useBothCallers() {
  const first = useIssueHeaderWrites(issue, state);
  const second = useIssueHeaderWrites(issue, state);
  return { first, second };
}

/** Both callers on a client of their own, signed in as alice, with every PATCH of the issue
 *  answered here: a title write is held open until the test answers `titleSave`, and a status
 *  write is answered at once with the status it set. `submitTitle` has the first caller save a
 *  new title and waits for its PATCH to go. */
function renderBothCallers() {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client }, children);
  const titleSave = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(async (_key, input) =>
    typeof input.title === "string"
      ? titleSave.promise
      : narrowIssue({ ...issue, status: input.status ?? issue.status })
  );
  const statusWrites = () =>
    patchIssue.mock.calls.filter(([, input]) => input.status === "in_progress").length;
  const hook = renderHook(useBothCallers, { wrapper });
  const submitTitle = async () => {
    act(() => {
      hook.result.current.first.drafts.writeTitle("New title");
      hook.result.current.first.drafts.requestTitleSubmit();
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
  };
  return { client, hook, patchIssue, statusWrites, submitTitle, titleSave };
}

/** Waits until no write of `client`'s store may still reach Dispatch: every send started so far,
 *  a second send of a write among them, has been answered. */
async function writesSettled(client: QueryClient): Promise<void> {
  await waitFor(() => expect(writesOf(client).rows().filter(isLive)).toEqual([]));
}

// The source page held a status picked behind another write in one component's state, so Details
// went on showing the old status while the header showed the new one. The pick is a write of its
// own from the moment it is made, so every caller reads it as pending; it waits in the issue's
// line behind the title write, and goes once, after it.
test("a status picked behind a title write is the status both callers read, and it is sent once", async () => {
  const { client, hook, patchIssue, statusWrites, submitTitle, titleSave } = renderBothCallers();

  try {
    await submitTitle();

    act(() => hook.result.current.first.pickStatus("in_progress"));
    const { first, second } = hook.result.current;
    expect(first.pendingStatus).toBe("in_progress");
    expect(second.pendingStatus).toBe("in_progress");
    expect(first.statusSaving).toBe(true);
    expect(second.statusSaving).toBe(true);
    // Every promise the pick chained has run, and it still waits in the line: no status PATCH
    // has gone while the title write is out.
    await act(settle);
    expect(statusWrites()).toBe(0);
    expect(hook.result.current.second.pendingStatus).toBe("in_progress");

    await act(async () => {
      titleSave.resolve(narrowIssue({ ...issue, title: "New title" }));
    });
    await waitFor(() => expect(statusWrites()).toBe(1));
    // A second send would follow the first: wait until no write is left to go.
    await writesSettled(client);
    expect(statusWrites()).toBe(1);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});

// A title Dispatch refuses (too long) stays refused, with its reason, after a status picked while
// it was out has gone: the pick is not `updateIssue`'s write, so it does not replace the refused
// one `titleRefusal` reads, and its landing drains no draft, so the refused title is not sent
// again. The refusal no longer holds the line, so the pick goes then.
test("a status picked behind a refused title write leaves the refusal shown and goes once the line is free", async () => {
  const { client, hook, patchIssue, statusWrites, submitTitle, titleSave } = renderBothCallers();

  try {
    await submitTitle();
    act(() => hook.result.current.first.pickStatus("in_progress"));
    await act(settle);
    expect(statusWrites()).toBe(0);

    await act(async () => {
      titleSave.reject(new ApiError(422, { code: "CAP_EXCEEDED", error: "Title too long" }));
    });
    await waitFor(() => expect(statusWrites()).toBe(1));
    await waitFor(() => expect(hook.result.current.first.statusSaving).toBe(false));
    await writesSettled(client);
    expect(hook.result.current.first.titleRefusal).toBe("Title too long");
    expect(patchIssue.mock.calls.filter(([, input]) => input.title !== undefined)).toHaveLength(1);
    expect(statusWrites()).toBe(1);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});

// The reader may leave the page while the title write is still out. The pick is one write of the
// page's store, which lives on the query client and not on the page, so with no caller drawn it
// still waits its turn, and goes once the title write is answered.
test("a status pick is kept in the write store when the page that made it goes", async () => {
  const { client, hook, patchIssue, statusWrites, submitTitle, titleSave } = renderBothCallers();

  try {
    await submitTitle();
    act(() => hook.result.current.first.pickStatus("in_progress"));
    await act(settle);
    expect(
      writesOf(client)
        .rows()
        .map((write) => write.status)
    ).toEqual(["sending", "queued"]);
    hook.unmount();

    await act(async () => {
      titleSave.resolve(narrowIssue({ ...issue, title: "New title" }));
    });
    await waitFor(() => expect(statusWrites()).toBe(1));
    await writesSettled(client);
    expect(statusWrites()).toBe(1);
    expect(writesOf(client).rows()).toEqual([]);
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});

// A parent save the reader stopped may have been saved: the hook's parent line says so, and that
// saving it again is a write of its own, rather than that the save failed.
test("a parent save the reader stopped says it may have been saved, not that it failed", async () => {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client }, children);
  const patchIssue = answerIssuePatches().mockRejectedValue(
    new NetworkError(`/api/v1/issues/${issue.key}`, new TypeError("Failed to fetch"))
  );
  jest.useFakeTimers();
  const hook = renderHook(() => useIssueHeaderWrites(issue, state), { wrapper });
  try {
    act(() => {
      hook.result.current.drafts.writeParent("CORE-9");
      hook.result.current.drafts.requestParentSubmit();
    });
    await stopTheLateWrite(client);
    expect(hook.result.current.parentSaveFailed).toBe(true);
    expect(hook.result.current.parentError).toBe(
      stoppedWriteNote("the new parent", "saved", {
        anew: "Saving it again sends it as a new write.",
      })
    );
  } finally {
    realClock();
    hook.unmount();
    patchIssue.mockRestore();
  }
});

// The shell's indicator names a header write by the page's own name for each field it carries,
// never the schema's, and a status by the board's word for it.
test("issueUpdateLabel names each field by the page's word", () => {
  expect(issueUpdateLabel("CORE-1", { parent: "CORE-9" })).toBe("Parent of CORE-1 saved");
  expect(issueUpdateLabel("CORE-1", { status: "in_progress" })).toBe("CORE-1 set to In progress");
  expect(issueUpdateLabel("CORE-1", { status: "todo" })).toBe("CORE-1 set to Todo");
  expect(issueUpdateLabel("CORE-1", { parent: "CORE-9", title: "New title" })).toBe(
    "Parent and Title of CORE-1 saved"
  );
  expect(issueUpdateLabel("CORE-1", {})).toBe("CORE-1 saved");
});

// The prototype's `.st-*` puts every status in one of three groups, each with its glyph and its
// colour: done, the work that moves, and the rest, a status the board does not know among them.
test("statusGlyph and statusTone give the prototype's three groups", () => {
  expect([statusGlyph("done"), statusTone("done")]).toEqual(["✓", pageStatusDoneText]);
  const moving = ["in_progress", "testing", "needs_review"];
  for (const status of moving) {
    expect([status, statusGlyph(status), statusTone(status)]).toEqual([
      status,
      "◐",
      pageStatusProgressText,
    ]);
  }
  const others = [
    ...issueStatuses.filter((status) => status !== "done" && !moving.includes(status)),
    "blocked",
  ];
  expect(others).toEqual(expect.arrayContaining(["triage", "icebox", "backlog", "todo", "retro"]));
  for (const status of others) {
    expect([status, statusGlyph(status), statusTone(status)]).toEqual([
      status,
      "○",
      pageStatusIdleText,
    ]);
  }
});

// The page's typeface lacks ◐, which as text would be drawn in a face of the reader's own machine,
// at that face's width; a shape is the same whatever the reader's fonts, so each group's glyph is
// an inline svg with no text in it.
test("StatusGlyph draws each status's shape as an inline svg with no text", () => {
  const groups: [status: string, shapes: (string | null)[][]][] = [
    ["done", [["path", "none", "currentColor"]]],
    [
      "in_progress",
      [
        ["circle", "none", "currentColor"],
        ["path", "currentColor", null],
      ],
    ],
    ["todo", [["circle", "none", "currentColor"]]],
  ];
  for (const [status, shapes] of groups) {
    const view = render(createElement(StatusGlyph, { glyph: statusGlyph(status) }));
    try {
      const svg = view.container.firstElementChild;
      expect({
        elements: Array.from(view.container.children, (element) => element.tagName),
        glyph: svg?.getAttribute("data-status-glyph"),
        hidden: svg?.getAttribute("aria-hidden"),
        shapes: Array.from(svg?.children ?? [], (shape) => [
          shape.tagName,
          shape.getAttribute("fill"),
          shape.getAttribute("stroke"),
        ]),
        sized: svg?.classList.contains("size-[1em]"),
        status,
        text: svg?.textContent,
        textElements: svg?.querySelectorAll("text").length,
      }).toEqual({
        elements: ["svg"],
        glyph: statusGlyph(status),
        hidden: "true",
        shapes,
        sized: true,
        status,
        text: "",
        textElements: 0,
      });
    } finally {
      view.unmount();
    }
  }
});

/** The project's one architecture source, so the components line draws its picker
 *  (`IssueComponentsLine.test.tsx`'s `source`, counting one component). */
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

// The new issue page's add menu reaches four controls of today's pages by name, so each name is
// the one the control has there - `main`'s for three, `CONTROL_LABELS.edit_priorities` for the
// priorities' picker - and each control answers to it.
test("the four control names are those of today's pages", async () => {
  expect(assigneeControlName("CORE-1")).toBe("Assignee of CORE-1");
  expect(repoControlName("CORE-1")).toBe("Repository of CORE-1");
  expect(componentsControlName("CORE-1", null)).toBe("Edit components of CORE-1");

  const sources = spyOn(api, "listProjectArchitectureSources").mockResolvedValue([
    architectureSource,
  ]);
  const view = renderWithQueries(
    createElement(
      MemoryRouter,
      null,
      createElement(AssigneeControl, { assignee: null, issueKey: "CORE-1" }),
      createElement(RepoControl, {
        issueKey: "CORE-1",
        project: "CORE",
        repo: "acme/api",
        resolvedRepo: "acme/api",
      }),
      createElement(IssueComponentsLine, {
        issue: {
          components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
          key: "CORE-1",
          project: "CORE",
          status: "todo",
          title: "Ship",
        },
      }),
      createElement(IssuePriorityLine, {
        disabled: false,
        issue: { advances: [], advances_revision: 1, key: "CORE-1" },
      })
    )
  );
  try {
    const scope = within(view.container);
    expect(scope.getByRole("combobox", { name: "Assignee of CORE-1" })).toBeInTheDocument();
    expect(scope.getByRole("combobox", { name: "Repository of CORE-1" })).toBeInTheDocument();
    expect(
      await scope.findByRole("button", { name: "Edit components of CORE-1" })
    ).toBeInTheDocument();
    expect(scope.getByRole("button", { name: CONTROL_LABELS.edit_priorities })).toBeInTheDocument();
  } finally {
    view.unmount();
    sources.mockRestore();
  }
});

// The new issue page's add menu, its `l` key and its rows find the labels control by the name it
// carries inside `NoTooltips`, which `labelsControlName` builds: the words it draws, then the
// joining word of `page-words.ts` and the issue's key, while it has no label; `main`'s name once
// it has one; and while it saves, the words it draws then, their ellipsis dropped, then the
// joining word of `page-words.ts` and the key. Given `null`, outside `NoTooltips`, it is `main`'s
// name in every state. Each expected name is built from the words of `page-words.ts`.
test("labelsControlName names the labels control as it is drawn", () => {
  const words = { ANSWER_LINES, CONTROL_LABELS };
  const addName = `${CONTROL_LABELS.add_label} ${CONTROL_LABELS.control_name_to} CORE-1`;
  const saving = withoutEllipsis(ANSWER_LINES.saving_labels);
  const savingName = `${saving} ${CONTROL_LABELS.control_name_of} CORE-1`;
  expect(labelsControlName("CORE-1", null)).toBe("Edit labels");
  expect(labelsControlName(undefined, null)).toBe("Edit labels");
  expect(labelsControlName("CORE-1", { labels: [], saving: false, words })).toBe(addName);
  expect(labelsControlName(undefined, { labels: [], saving: false, words })).toBe(
    CONTROL_LABELS.add_label
  );
  expect(labelsControlName("CORE-1", { labels: ["api"], saving: false, words })).toBe(
    "Edit labels"
  );
  expect(labelsControlName("CORE-1", { labels: [], saving: true, words })).toBe(savingName);
  expect(labelsControlName("CORE-1", { labels: ["api"], saving: true, words })).toBe(savingName);
  expect(labelsControlName(undefined, { labels: ["api"], saving: true, words })).toBe(saving);
  // `saving` alone says whether a save is in flight: with it false the name is never the saving
  // one, with a label or none and with a key or none. Of the four states the loop reads, one, a
  // label with no key, is asserted nowhere above.
  for (const labels of [[], ["api"]]) {
    for (const issueKey of ["CORE-1", undefined]) {
      const name = labelsControlName(issueKey, { labels, saving: false, words });
      expect({ issueKey, labels, saving: [saving, savingName].includes(name) }).toEqual({
        issueKey,
        labels,
        saving: false,
      });
    }
  }
  // The saving words are the ones passed, and three dots end them as an ellipsis does.
  expect(
    labelsControlName("CORE-1", {
      labels: [],
      saving: true,
      words: {
        ANSWER_LINES: { ...ANSWER_LINES, saving_labels: "Writing labels..." },
        CONTROL_LABELS,
      },
    })
  ).toBe(`Writing labels ${CONTROL_LABELS.control_name_of} CORE-1`);

  const view = draw(
    createElement(IssueLabels, {
      disabled: false,
      issueKey: "CORE-1",
      labels: [],
      onSave: async () => undefined,
      project: "CORE",
      saving: false,
    }),
    "inside"
  );
  try {
    const control = within(view.container).getByRole("button", { name: addName });
    expect(control.textContent).toBe(CONTROL_LABELS.add_label);
  } finally {
    undraw(view);
  }
});

// The new issue page's add menu and its rows find the components picker by the name its trigger
// carries inside `NoTooltips`, which `componentsControlName` builds: the words the trigger draws,
// then the joining word of `page-words.ts` and the issue's key, and while it saves the page's
// `saving_components` that `ComponentPicker` draws in their place, its ellipsis dropped. On the
// issue's own set the trigger draws a bare "+", a symbol no voice user says, and the name stays
// `main`'s word-first one. Given `null`, as outside `NoTooltips`, it is `main`'s name, which the
// trigger carries there in every state, beside `main`'s "Saving…" while it saves. Each expected
// name inside is built from the words of `page-words.ts`.
test("componentsControlName names the components picker as it is drawn", async () => {
  const notAttached: IssueComponents = {
    ids: [],
    inherited_from: null,
    mode: "inherit",
    reason: null,
    unknown: [],
  };
  const ownSet: IssueComponents = { ...notAttached, ids: ["dispatch-server"], mode: "explicit" };
  const words = { ANSWER_LINES, CONTROL_LABELS };
  const drawnName = (components: IssueComponents, saving = false) =>
    componentsControlName("CORE-1", { components, saving, words });
  const setName = `${CONTROL_LABELS.components_set} ${CONTROL_LABELS.control_name_of} CORE-1`;
  const savingName = `${withoutEllipsis(ANSWER_LINES.saving_components)} ${CONTROL_LABELS.control_name_of} CORE-1`;
  expect(componentsControlName("CORE-1", null)).toBe("Edit components of CORE-1");
  expect(drawnName(notAttached)).toBe(setName);
  expect(drawnName({ ...notAttached, mode: "none", reason: "A process change" })).toBe(setName);
  expect(drawnName({ ...ownSet, inherited_from: "CORE-9" })).toBe(setName);
  expect(drawnName(ownSet)).toBe("Edit components of CORE-1");
  expect(drawnName(notAttached, true)).toBe(savingName);
  expect(drawnName(ownSet, true)).toBe(savingName);
  // The saving words are the ones passed, and three dots end them as an ellipsis does.
  expect(
    componentsControlName("CORE-1", {
      components: notAttached,
      saving: true,
      words: {
        ANSWER_LINES: { ...ANSWER_LINES, saving_components: "Writing components..." },
        CONTROL_LABELS,
      },
    })
  ).toBe(`Writing components ${CONTROL_LABELS.control_name_of} CORE-1`);

  const sources = spyOn(api, "listProjectArchitectureSources").mockResolvedValue([
    architectureSource,
  ]);
  const tree = spyOn(api, "getArchitecture").mockResolvedValue(architecture);
  // Each save is held until the case answers it.
  const answers: ReturnType<typeof Promise.withResolvers<Issue>>[] = [];
  const patchIssue = answerIssuePatches().mockImplementation(() => {
    const answer = Promise.withResolvers<Issue>();
    answers.push(answer);
    return answer.promise;
  });
  const line = (components: IssueComponents, key: string) =>
    createElement(IssueComponentsLine, {
      issue: { components, key, project: "CORE", status: "todo", title: "Ship" },
    });
  const user = userEvent.setup();
  // Picks a component in the picker `trigger` opens and closes it, so its save goes.
  const save = async (trigger: HTMLElement) => {
    await user.click(trigger);
    const body = within(document.body);
    await user.click(
      await body.findByRole("option", { name: "dispatch-server · Dispatch server" })
    );
    // The picker's own Escape handler sits on its search box (`IssueComponentsLine.test.tsx`).
    body.getByRole("combobox", { name: "Search components" }).focus();
    await user.keyboard("{Escape}");
  };
  const views: RenderResult[] = [];
  try {
    const inside = draw(
      createElement("div", null, line(notAttached, "CORE-1"), line(ownSet, "CORE-2")),
      "inside"
    );
    views.push(inside);
    const scope = within(inside.container);
    const trigger = await scope.findByRole("button", { name: setName });
    expect(trigger.textContent).toBe(CONTROL_LABELS.components_set);
    // Its name begins with the words it draws.
    const drawnWords = withoutEllipsis((trigger.textContent ?? "").trim());
    expect(trigger.getAttribute("aria-label")?.startsWith(drawnWords)).toBe(true);
    expect(scope.getByRole("button", { name: "Edit components of CORE-2" }).textContent).toBe("+");

    // A save in flight: `ComponentPicker` draws the page's `saving_components`, and the name
    // begins with those words, their ellipsis dropped.
    await save(trigger);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    const saving = await scope.findByRole("button", { name: savingName });
    expect(saving.textContent).toBe(ANSWER_LINES.saving_components);
    const savingWords = withoutEllipsis((saving.textContent ?? "").trim());
    expect(saving.getAttribute("aria-label")?.startsWith(savingWords)).toBe(true);
    await act(async () => {
      answers[0]?.resolve({} as Issue);
    });
    await scope.findByRole("button", { name: setName });

    const outside = draw(line(notAttached, "CORE-1"), "outside");
    views.push(outside);
    const mains = await within(outside.container).findByRole("button", {
      name: "Edit components of CORE-1",
    });
    expect(mains.textContent).toBe("Set components");
    await save(mains);
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(mains.textContent).toBe("Saving…"));
    expect(mains).toHaveAttribute("aria-label", "Edit components of CORE-1");
  } finally {
    for (const answer of answers) answer.resolve({} as Issue);
    await act(settle);
    for (const view of views) undraw(view);
    patchIssue.mockRestore();
    tree.mockRestore();
    sources.mockRestore();
  }
});

// The header and Details are two callers of the hook on one page, and each reads the status the
// other is saving, from the first moment of its write to its answer.
test("two callers of useIssueHeaderWrites read the status either one is saving", async () => {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client }, children);
  const statusSave = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(async () => statusSave.promise);
  const hook = renderHook(useBothCallers, { wrapper });
  try {
    expect(hook.result.current.second.pendingStatus).toBeUndefined();
    expect(hook.result.current.second.statusSaving).toBe(false);

    act(() => hook.result.current.first.pickStatus("in_progress"));
    // The first caller's write has gone, and its answer is held.
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(patchIssue.mock.calls[0]?.[1]).toEqual({ status: "in_progress" });
    await act(settle);
    const { first, second } = hook.result.current;
    expect(second.pendingStatus).toBe("in_progress");
    expect(second.statusSaving).toBe(true);
    expect(first.pendingStatus).toBe("in_progress");
    expect(first.statusSaving).toBe(true);

    await act(async () => {
      statusSave.resolve(narrowIssue({ ...issue, status: "in_progress" }));
    });
    await waitFor(() => expect(hook.result.current.second.statusSaving).toBe(false));
    expect(hook.result.current.first.statusSaving).toBe(false);
    expect(hook.result.current.first.pendingStatus).toBeUndefined();
    expect(hook.result.current.second.pendingStatus).toBeUndefined();
  } finally {
    hook.unmount();
    patchIssue.mockRestore();
  }
});

/** The viewer's state as Dispatch holds it before the pin: the issue unpinned. */
const unpinned: UserState = { [issue.key]: state };

/** The pin's PUT's answer: Dispatch's state of the issue once it took the pin. */
const pinnedState: UserIssueState = { ...state, pinned: true, seq: 1 };

/** Every read of the viewer's state (`getMyState`), each held until the test answers it. */
function holdStateReads() {
  const reads: ReturnType<typeof Promise.withResolvers<UserState>>[] = [];
  const getMyState = spyOn(api, "getMyState").mockImplementation(() => {
    const read = Promise.withResolvers<UserState>();
    reads.push(read);
    return read.promise;
  });
  return { getMyState, reads };
}

/** Each read of `queryKey` that `client` asks from now on, listed as it calls its query's
 *  function: a read a write holds is listed before it waits, and one that is not held has asked
 *  by then. */
function readsAsked(client: DispatchQueryClient, queryKey: QueryKey) {
  const asked: string[] = [];
  const stop = client.onAttempt((queryHash) => {
    if (queryHash === hashKey(queryKey)) asked.push(queryHash);
  });
  return { asked, stop };
}

/** `client`, signed in as alice, around what a hook renders. */
function signedInWrapper(client: QueryClient) {
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  return ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client }, children);
}

// The pin's answer went into the cache under a read of the viewer's state asked while the PUT was
// out, and that read's answer, of the state from before Dispatch took the pin, landed over it: the
// issue read as unpinned again wherever the state was next drawn. Nothing draws the state here,
// as when the reader has left the page, so the pin's settling does not read it again over that.
test("a read of the state asked while the pin is out does not put back the unpinned state its answer replaced", async () => {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const wrapper = signedInWrapper(client);
  const { getMyState, reads } = holdStateReads();
  const pinSave = Promise.withResolvers<UserIssueState>();
  const putIssueState = spyOn(api, "putIssueState").mockReturnValue(pinSave.promise);
  const hook = renderHook(() => useIssueHeaderWrites(issue, state), { wrapper });
  try {
    const first = client.fetchQuery(userStateQuery());
    await act(async () => {
      reads[0]?.resolve(unpinned);
      await first;
    });
    act(() => {
      const { pin, pinGuard } = hook.result.current;
      pinGuard.guard(() => pin.mutate({ issueKey: issue.key, pinned: true }));
    });
    await waitFor(() => expect(putIssueState).toHaveBeenCalledTimes(1));
    // While the PUT is out the state is read again; Dispatch has not taken the pin yet.
    act(() => {
      void client.refetchQueries({ queryKey: userStateQuery().queryKey });
    });
    await waitFor(() => expect(getMyState).toHaveBeenCalledTimes(2));

    await act(async () => {
      pinSave.resolve(pinnedState);
    });
    await waitFor(() => expect(hook.result.current.pin.isSuccess).toBe(true));
    await act(async () => {
      reads[1]?.resolve(unpinned);
      await settle();
    });
    expect(client.getQueryData<UserState>(userStateQuery().queryKey)?.[issue.key]).toEqual(
      pinnedState
    );
  } finally {
    hook.unmount();
    getMyState.mockRestore();
    putIssueState.mockRestore();
  }
});

/** The pin as the issue page drives it: the viewer's state read as the page reads it, and the
 *  hook given the issue's row of it, which is what the page shows. */
function usePagePin() {
  const userState = useQuery(userStateQuery());
  const shown = stateForIssue(userState.data, issue.key);
  return { shown, writes: useIssueHeaderWrites(issue, shown) };
}

// A refresh of the viewer's state asked while the pin was out could reach Dispatch before the pin
// did, and its answer put the issue back unpinned under the reader's press. The read waits for the
// pin's answer and is asked once it is in, so the page shows what Dispatch holds after the pin.
test("a read of the state asked while the pin is out waits for its answer, and the page shows what it reads then", async () => {
  const client = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const wrapper = signedInWrapper(client);
  const { getMyState, reads } = holdStateReads();
  const pinSave = Promise.withResolvers<UserIssueState>();
  const putIssueState = spyOn(api, "putIssueState").mockReturnValue(pinSave.promise);
  const { asked, stop } = readsAsked(client, userStateQuery().queryKey);
  const hook = renderHook(usePagePin, { wrapper });
  // Dispatch's state once it took the pin, with a read the reader made elsewhere since.
  const after: UserIssueState = { ...pinnedState, last_read_seq: 7 };
  try {
    await waitFor(() => expect(getMyState).toHaveBeenCalledTimes(1));
    await act(async () => {
      reads[0]?.resolve(unpinned);
    });
    await waitFor(() =>
      expect(client.getQueryData<UserState>(userStateQuery().queryKey)).toEqual(unpinned)
    );

    act(() => {
      const { pin, pinGuard } = hook.result.current.writes;
      pinGuard.guard(() => pin.mutate({ issueKey: issue.key, pinned: true }));
    });
    await waitFor(() => expect(putIssueState).toHaveBeenCalledTimes(1));
    expect(hook.result.current.shown.pinned).toBe(true);

    await act(async () => {
      void client.invalidateQueries({ queryKey: userStateQuery().queryKey });
    });
    // The refresh is asked, and waits: the state is read once, before the pin.
    await waitFor(() => expect(asked).toHaveLength(2));
    expect(getMyState).toHaveBeenCalledTimes(1);
    expect(hook.result.current.shown.pinned).toBe(true);

    await act(async () => {
      pinSave.resolve(pinnedState);
    });
    await waitFor(() => expect(getMyState).toHaveBeenCalledTimes(2));
    getMyState.mockResolvedValue({ [issue.key]: after });
    await act(async () => {
      reads[1]?.resolve({ [issue.key]: after });
    });
    await waitFor(() => expect(hook.result.current.shown).toEqual(after));
    await waitFor(() => expect(hook.result.current.writes.pin.isSuccess).toBe(true));
  } finally {
    hook.unmount();
    stop();
    getMyState.mockRestore();
    putIssueState.mockRestore();
  }
});

// A write of the issue's fields lands one way whichever row made it: its answer merged over the
// cached details, or the issue read again when Dispatch took the write with no answer, and the
// issue lists refreshed either way.
test("landIssueWrite merges an answer and reads the issue again when none came", () => {
  const land = (next: Issue | null) => {
    const client = new QueryClient();
    client.setQueryData(["issue", issue.key], issue);
    client.setQueryData(["issues", "pinned"], []);
    landIssueWrite(client, issue.key, next);
    const landed = {
      details: client.getQueryData<IssueDetails>(["issue", issue.key]),
      issueReadAgain: client.getQueryState(["issue", issue.key])?.isInvalidated,
      listsReadAgain: client.getQueryState(["issues", "pinned"])?.isInvalidated,
    };
    client.clear();
    return landed;
  };

  const answered = land(narrowIssue({ ...issue, title: "New title" }));
  expect(answered.details?.title).toBe("New title");
  // The answer is the narrow `Issue`: what the details alone hold stays as it was.
  expect(answered.details?.artifacts).toEqual(issue.artifacts);
  expect(answered.issueReadAgain).toBe(false);
  expect(answered.listsReadAgain).toBe(true);

  const unanswered = land(null);
  expect(unanswered.details?.title).toBe(issue.title);
  expect(unanswered.issueReadAgain).toBe(true);
  expect(unanswered.listsReadAgain).toBe(true);
});

// Only the Status pill's pick alone is a control's write, shown in the shell's indicator and
// rolled back on a refusal; a write that carries any other field is the header's.
test("statusPickOnly is true for a status alone", () => {
  expect(statusPickOnly({ status: "done" })).toBe(true);
  expect(statusPickOnly({ status: "done", title: "New title" })).toBe(false);
  expect(statusPickOnly({ title: "New title" })).toBe(false);
  expect(statusPickOnly({})).toBe(false);
  expect(statusPickOnly(undefined)).toBe(false);
});

/** The list's entry for `step`. */
function entryOf(view: RenderResult, step: string): HTMLElement {
  const item = view.container.querySelector<HTMLElement>(`[data-approval-step="${step}"]`);
  if (item === null) throw new Error(`no entry for ${step}`);
  return item;
}

/** Records the reader's every review as Dispatch answers an approval of `artifactId`. */
function answerReviews(artifactId: string) {
  return spyOn(api, "createArtifactReview").mockResolvedValue({
    actor: { id: "alice", kind: "user" },
    artifact_id: artifactId,
    ask_id: null,
    content_sha256: null,
    content_sha256_source: null,
    created_at: "2026-09-11T00:00:00Z",
    id: "review-1",
    kind: "person",
    reason: null,
    state: "approved",
    version: 1,
  });
}

// Given `actionsOn`, each entry whose document's approval request waits on the reader carries its
// own Approve and Request changes, which review that entry's document, and no other entry carries
// any; not given, the primary document's entry alone carries them, as on `main`.
test("ApprovalSteps draws Approve and Request changes on each entry of actionsOn and on no other", async () => {
  const entries: StepEntry[] = [
    {
      document: doc(SPEC_ID, "spec.md", {
        by: { id: "bob", kind: "user" },
        latest_version: 1,
        reason: "The second requirement names no owner.",
        state: "changes_requested",
        version: 1,
      }),
      label: "Spec",
      step: "spec",
    },
    {
      document: doc(VERIFICATION_ID, "verification.md", {
        latest_version: 1,
        state: "awaiting",
        waiting_on: "human",
      }),
      label: "Verification",
      step: "criteria",
    },
  ];
  const createArtifactReview = answerReviews(VERIFICATION_ID);
  const user = userEvent.setup();
  const drawSteps = (actionsOn: readonly string[] | undefined) =>
    renderWithQueries(
      createElement(
        MemoryRouter,
        null,
        createElement(ApprovalSteps, { actionsOn, entries, primaryArtifactId: SPEC_ID })
      )
    );
  const buttonsOf = (view: RenderResult, step: string) =>
    within(entryOf(view, step))
      .queryAllByRole("button")
      .map((button) => button.textContent);

  try {
    const waitingOnReader = drawSteps([VERIFICATION_ID]);
    try {
      expect(buttonsOf(waitingOnReader, "criteria")).toEqual([
        "Awaiting approval",
        "Approve",
        "Request changes",
      ]);
      expect(buttonsOf(waitingOnReader, "spec")).toEqual(["Changes requested"]);

      await user.click(
        within(entryOf(waitingOnReader, "criteria")).getByRole("button", { name: "Approve" })
      );
      await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(1));
      expect(createArtifactReview.mock.calls[0]?.[0]).toBe(VERIFICATION_ID);
      expect(createArtifactReview.mock.calls[0]?.[1]).toEqual({ state: "approved" });
    } finally {
      waitingOnReader.unmount();
    }

    const primaryAlone = drawSteps(undefined);
    try {
      expect(buttonsOf(primaryAlone, "spec")).toEqual([
        "Changes requested",
        "Approve",
        "Request changes",
      ]);
      expect(buttonsOf(primaryAlone, "criteria")).toEqual(["Awaiting approval"]);
    } finally {
      primaryAlone.unmount();
    }
  } finally {
    createArtifactReview.mockRestore();
  }
});

// Where `actionsOn` names two or more documents the list holds two Approve and two Request
// changes, so inside `NoTooltips` each is named by the words it draws and then the document it
// reviews, in the page's words, and a voice user says what they read (WCAG 2.5.3). A stale
// approval's Approve draws its version, and its name says the version of the document. Naming
// one document, each action is named by its words alone, as on `main`.
test("ApprovalSteps names each document's actions when actionsOn holds two documents", async () => {
  const awaiting: ArtifactApproval = { latest_version: 1, state: "awaiting", waiting_on: "human" };
  const stepsWith = (specApproval: ArtifactApproval): StepEntry[] => [
    { document: doc(SPEC_ID, "spec.md", specApproval), label: "Spec", step: "spec" },
    {
      document: doc(VERIFICATION_ID, "verification.md", awaiting),
      label: "Verification",
      step: "criteria",
    },
  ];
  const drawSteps = (entries: StepEntry[], actionsOn: readonly string[]) =>
    draw(
      createElement(ApprovalSteps, { actionsOn, entries, primaryArtifactId: SPEC_ID }),
      "inside"
    );
  /** Each named action of the list: the entry it is drawn in, its name and the words it draws,
   *  and whether the name begins with those words. */
  const actionsOf = (view: RenderResult, named: readonly (readonly [string, string])[]) =>
    named.map(([step, name]) => {
      const word = within(entryOf(view, step)).getByRole("button", { name }).textContent ?? "";
      return { begins: name === word || name.startsWith(`${word} `), name, step, word };
    });
  const createArtifactReview = answerReviews(VERIFICATION_ID);
  const user = userEvent.setup();

  try {
    const both = drawSteps(stepsWith(awaiting), [SPEC_ID, VERIFICATION_ID]);
    try {
      expect(
        actionsOf(both, [
          ["spec", "Approve the spec"],
          ["spec", "Request changes to the spec"],
          ["criteria", "Approve the verification"],
          ["criteria", "Request changes to the verification"],
        ])
      ).toEqual([
        { begins: true, name: "Approve the spec", step: "spec", word: "Approve" },
        {
          begins: true,
          name: "Request changes to the spec",
          step: "spec",
          word: "Request changes",
        },
        { begins: true, name: "Approve the verification", step: "criteria", word: "Approve" },
        {
          begins: true,
          name: "Request changes to the verification",
          step: "criteria",
          word: "Request changes",
        },
      ]);

      await user.click(
        within(both.container).getByRole("button", { name: "Approve the verification" })
      );
      await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(1));
      expect(createArtifactReview.mock.calls[0]?.[0]).toBe(VERIFICATION_ID);
      expect(createArtifactReview.mock.calls[0]?.[1]).toEqual({ state: "approved" });
    } finally {
      undraw(both);
    }

    const stale = drawSteps(stepsWith({ latest_version: 5, state: "stale", version: 3 }), [
      SPEC_ID,
      VERIFICATION_ID,
    ]);
    try {
      expect(actionsOf(stale, [["spec", "Approve v5 of the spec"]])).toEqual([
        { begins: true, name: "Approve v5 of the spec", step: "spec", word: "Approve v5" },
      ]);
    } finally {
      undraw(stale);
    }

    const one = drawSteps(stepsWith(awaiting), [VERIFICATION_ID]);
    try {
      expect(
        actionsOf(one, [
          ["criteria", "Approve"],
          ["criteria", "Request changes"],
        ])
      ).toEqual([
        { begins: true, name: "Approve", step: "criteria", word: "Approve" },
        { begins: true, name: "Request changes", step: "criteria", word: "Request changes" },
      ]);
      // Named by its words alone, it carries no name of its own, as on `main`.
      expect(one.container.querySelectorAll("button[aria-label]")).toHaveLength(0);
    } finally {
      undraw(one);
    }
  } finally {
    createArtifactReview.mockRestore();
  }
});
