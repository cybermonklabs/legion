import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { jump, realClock } from "../../__tests__/fake-clock";
import { afterFrameAndTasks } from "../../__tests__/frames";
import { pressHeld } from "../../__tests__/held-press";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { seedDeliveryReads } from "../../__tests__/live-socket-harness";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { deliveryTimelineQuery } from "../../api/queries";
import type { Agent, DeliverySessionGroup, DeliveryTimelineWithMeasures } from "../../api/types";
import { emulateChromiumFocusFixup } from "../issue-page/chromium-focus-fixup";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { SEND_DEADLINE_MS, stoppedWriteNote } from "../writes/keyed-write";
import { SessionGroupsEditor } from "./SessionGroupsEditor";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const crew: DeliverySessionGroup = {
  created_at: "2026-10-11T00:00:00Z",
  created_by: { id: "alice@example.com", kind: "user" },
  id: "11111111-1111-1111-1111-111111111111",
  name: "Release crew",
  sessions: ["s2", "s9"],
  updated_at: "2026-10-11T00:00:00Z",
  updated_by: { id: "alice@example.com", kind: "user" },
};

const titles = { s1: "Planner one", s2: "Planner two", s3: "Reviewer" };
const counts = { s1: 3, s2: 2, s3: 1 };

/** A drawn editor: `draw` draws it again over `groups`, and `queryClient` is its client. */
interface DrawnEditor {
  readonly draw: (groups: readonly DeliverySessionGroup[]) => void;
  readonly queryClient: QueryClient;
}

/** The editor over `groups`, as the Delivery page draws it; `draw` draws it again, over the same
 *  client, over the groups a live update brings, as the page does. `inMain` draws it in the
 *  shell's main content, as the app shell draws it. */
function renderEditor(
  groups: readonly DeliverySessionGroup[] = [],
  values: {
    agents?: readonly Agent[];
    titles?: Readonly<Record<string, string>>;
    counted?: boolean;
    counts?: Readonly<Record<string, number>>;
    inMain?: boolean;
  } = {}
): DrawnEditor {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // The agent registry, as the shell's poller last read it: none listed unless a test says so.
  queryClient.setQueryData(["agents"], values.agents ?? []);
  const editor = (shown: readonly DeliverySessionGroup[]) => {
    const drawn = (
      <SessionGroupsEditor
        counted={values.counted}
        counts={values.counts ?? counts}
        groups={shown}
        onClose={() => {}}
        titles={values.titles ?? titles}
      />
    );
    return (
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <KeymapProvider>{values.inMain ? <main>{drawn}</main> : drawn}</KeymapProvider>
        </QueryClientProvider>
      </MemoryRouter>
    );
  };
  const view = render(editor(groups));
  return { draw: (next) => view.rerender(editor(next)), queryClient };
}

/** A Delivery page's timeline read in `queryClient`'s cache, holding `groups` as the live socket
 *  wrote its snapshot, with no observer. */
function seedTimeline(queryClient: QueryClient, groups: readonly DeliverySessionGroup[]) {
  const timeline = deliveryTimelineQuery({ kind: "timeline", window: { preset: 28 }, query: "" });
  queryClient.getQueryCache().build(queryClient, {
    gcTime: Number.POSITIVE_INFINITY,
    meta: timeline.meta,
    queryKey: timeline.queryKey,
  });
  queryClient.setQueryData(timeline.queryKey, {
    session_groups: [...groups],
  } as unknown as DeliveryTimelineWithMeasures);
  return timeline.queryKey;
}

test("it lists the window's sessions busiest first, each with its title, count and group", () => {
  renderEditor([crew]);
  try {
    const rows = within(screen.getByRole("list", { name: "Sessions in this window" })).getAllByRole(
      "listitem"
    );
    expect(rows.map((row) => row.textContent)).toEqual([
      expect.stringContaining("Planner one"),
      expect.stringContaining("Planner two"),
      expect.stringContaining("Reviewer"),
    ]);
    expect(rows[0]?.textContent).toContain("3 PRs");
    expect(rows[1]?.textContent).toContain("Release crew");
  } finally {
    cleanup();
  }
});

// INFRA-3135, round 27: while a new window's read is out the page's answer is the previous
// window's, so the editor it opens is given that window's counts and told they are not counted
// (`counted` false). It lists those sessions still, each with its group, but draws no count beside
// one; given the window's own answer it draws each count again.
test("an editor whose counts are not counted lists each session and its group with no count, and draws the counts once they are", () => {
  renderEditor([crew], { counted: false });
  try {
    const rows = within(screen.getByRole("list", { name: "Sessions in this window" })).getAllByRole(
      "listitem"
    );
    const held = rows.map((row) => row.textContent ?? "");
    expect(held).toEqual([
      expect.stringContaining("Planner one"),
      expect.stringContaining("Planner two"),
      expect.stringContaining("Reviewer"),
    ]);
    expect(held.filter((text) => /\b\d+ PRs?\b/u.test(text))).toEqual([]);
    // Planner two's group, with no separator left where its count would have been.
    expect(held[1]).toContain("Release crew");
    expect(held[1]).not.toContain("·");
    // A session in no group draws nothing beside its name, not an empty box.
    expect(
      rows.flatMap((row) => [...row.children].filter((part) => part.textContent === ""))
    ).toEqual([]);
  } finally {
    cleanup();
  }

  renderEditor([crew], { counted: true });
  try {
    const rows = within(screen.getByRole("list", { name: "Sessions in this window" })).getAllByRole(
      "listitem"
    );
    expect(rows.map((row) => row.textContent)).toEqual([
      expect.stringContaining("3 PRs"),
      expect.stringContaining("2 PRs · Release crew"),
      expect.stringContaining("1 PR"),
    ]);
  } finally {
    cleanup();
  }
});

// Each session is an agent: the checkbox's label keeps its name, and its live link sits beside
// the label, outside it.
test("each session's checkbox is named by its agent, with its live link beside the label", () => {
  renderEditor([]);
  try {
    const checkbox = screen.getByRole("checkbox", { name: "Planner one" });
    const label = checkbox.closest("label");
    // The name shortens to an ellipsis in a narrow editor, so the title is its whole shown name,
    // and the full session id, which tells apart two sessions whose short ids are the same.
    expect(label?.getAttribute("title")).toBe("Planner one (s1)");
    const live = screen.getByRole("link", { name: "Open live conversation with Planner one" });
    expect(live).toHaveAttribute("href", "/agents/s1/live");
    expect(label?.contains(live)).toBe(false);
    expect(live.closest("[data-agent-name]")?.getAttribute("data-agent-name")).toBe("s1");
  } finally {
    cleanup();
  }
});

// A session the registry lists reads by its current title, not the one the delivery read stored;
// a session whose stored title is its own id has none, and reads by its short id.
test("a session reads by its registry title, else its stored title, else its short id", () => {
  renderEditor([], {
    agents: [
      {
        capabilities: [],
        dir: "/home/alice/legion",
        host: "devbox-alice",
        last_activity: null,
        last_seen: 1,
        machine_id: "devbox-alice",
        open_asks: 0,
        roles: [],
        session_id: "s1",
        state: "running",
        title: "Planner renamed",
      },
    ],
    titles: { s1: "Planner one", s2: "Planner two", s3: "s3" },
  });
  try {
    expect(screen.getByRole("checkbox", { name: "Planner renamed" })).toBeDefined();
    expect(screen.getByRole("checkbox", { name: "Planner two" })).toBeDefined();
    expect(screen.getByRole("checkbox", { name: "session:s3…" })).toBeDefined();
  } finally {
    cleanup();
  }
});

test("create: two selected sessions and a name become a group; the live views hear of it from Dispatch, and only the reads that are not live read again", async () => {
  const user = userEvent.setup();
  const create = spyOn(api, "createDeliverySessionGroup").mockResolvedValue(crew);
  const { queryClient } = renderEditor();
  const reads = seedDeliveryReads(queryClient);
  try {
    await user.click(screen.getByRole("checkbox", { name: "Planner one" }));
    await user.click(screen.getByRole("checkbox", { name: "Reviewer" }));
    await user.click(screen.getByLabelText("New group name"));
    await user.paste("Release crew");
    await user.click(screen.getByRole("button", { name: "Create group" }));
    await waitFor(() =>
      expect(create).toHaveBeenCalledWith(
        { name: "Release crew", sessions: ["s1", "s3"] },
        keyedAttempt
      )
    );
    await waitFor(() => expect(queryClient.getQueryState(reads.run)?.isInvalidated).toBe(true));
    expect(queryClient.getQueryState(reads.live)?.isInvalidated).toBe(false);
  } finally {
    cleanup();
    create.mockRestore();
  }
});

// Each write lands and the live update carrying it draws the group anew, as the page draws it,
// before the next press: a group's buttons are held from a write's landing until that update.
test("rename, add, remove and delete each write their one change", async () => {
  const user = userEvent.setup();
  const update = spyOn(api, "updateDeliverySessionGroup").mockResolvedValue(crew);
  const remove = spyOn(api, "deleteDeliverySessionGroup").mockResolvedValue(undefined);
  const { draw, queryClient } = renderEditor([crew]);
  /** Draws `next`, the live update carrying the write just made, once that write has landed, and
   *  answers the group's card as it is drawn now. */
  const drawn = async (next: DeliverySessionGroup) => {
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    draw([next]);
    return screen.getByRole("group", { name: next.name });
  };
  try {
    let group = screen.getByRole("group", { name: "Release crew" });
    const name = within(group).getByLabelText("Name");
    await user.clear(name);
    await user.click(name);
    await user.paste("Ship crew");
    await user.click(within(group).getByRole("button", { name: "Rename" }));
    await waitFor(() =>
      expect(update).toHaveBeenLastCalledWith(crew.id, { name: "Ship crew" }, keyedAttempt)
    );
    const renamed = { ...crew, name: "Ship crew", updated_at: "2026-10-11T00:01:00Z" };
    group = await drawn(renamed);

    await user.click(screen.getByRole("checkbox", { name: "Reviewer" }));
    await user.click(within(group).getByRole("button", { name: "Add selected" }));
    await waitFor(() =>
      expect(update).toHaveBeenLastCalledWith(crew.id, { add: ["s3"] }, keyedAttempt)
    );
    const added = { ...renamed, sessions: ["s2", "s9", "s3"], updated_at: "2026-10-11T00:02:00Z" };
    group = await drawn(added);

    await user.click(within(group).getByRole("button", { name: "Remove Planner two" }));
    await waitFor(() =>
      expect(update).toHaveBeenLastCalledWith(crew.id, { remove: ["s2"] }, keyedAttempt)
    );
    group = await drawn({ ...added, sessions: ["s9", "s3"], updated_at: "2026-10-11T00:03:00Z" });

    await user.click(within(group).getByRole("button", { name: "Delete group" }));
    await waitFor(() => expect(remove).toHaveBeenCalledWith(crew.id, keyedAttempt));
  } finally {
    cleanup();
    update.mockRestore();
    remove.mockRestore();
  }
});

// A change the reader stopped may have been saved: the editor says so, and that each button is
// a press of its own, rather than a failure it cannot know of.
test("a change the reader stopped says it may have been saved, not that it failed", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const remove = spyOn(api, "deleteDeliverySessionGroup").mockRejectedValue(
    new NetworkError("/api/v1/delivery/session-groups", new TypeError("Failed to fetch"))
  );
  const { queryClient } = renderEditor([crew]);
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    await user.click(within(group).getByRole("button", { name: "Delete group" }));
    await stopTheLateWrite(queryClient);
    expect(screen.getByRole("alert").textContent).toBe(
      stoppedWriteNote("the deletion of the group Release crew", "saved", {
        anew: "Making it again sends it as a new write.",
      })
    );
  } finally {
    realClock();
    cleanup();
    queryClient.clear();
    remove.mockRestore();
  }
});

// A held button is held by `aria-disabled`, never `disabled`, so it keeps the focus.
test("group actions are held while a save is pending", async () => {
  const user = userEvent.setup();
  let settle: (value: DeliverySessionGroup) => void = () => {};
  const pending = new Promise<DeliverySessionGroup>((resolve) => {
    settle = resolve;
  });
  const update = spyOn(api, "updateDeliverySessionGroup").mockImplementation(() => pending);
  renderEditor([crew]);
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    await user.click(screen.getByRole("checkbox", { name: "Reviewer" }));
    const name = within(group).getByLabelText("Name");
    await user.clear(name);
    await user.click(name);
    await user.paste("Ship crew");
    await user.click(within(group).getByRole("button", { name: "Rename" }));
    await waitFor(() =>
      expect(update).toHaveBeenCalledWith(crew.id, { name: "Ship crew" }, keyedAttempt)
    );

    for (const name of ["Rename", "Add selected", "Remove Planner two", "Delete group"]) {
      const button = within(group).getByRole("button", { name }) as HTMLButtonElement;
      expect(`${name}: ${button.getAttribute("aria-disabled")} ${button.disabled}`).toBe(
        `${name}: true false`
      );
    }
    settle(crew);
  } finally {
    cleanup();
    update.mockRestore();
  }
});

/** `group`'s Remove of Planner two and its Delete group, which a write of the group holds. */
function groupButtons(group: HTMLElement): HTMLButtonElement[] {
  return ["Remove Planner two", "Delete group"].map(
    (name) => within(group).getByRole("button", { name }) as HTMLButtonElement
  );
}

/** `element` as a string a failed assertion prints whole: the body, else its label, else its
 *  text. */
function named(element: Element | null): string {
  if (element === null || element === document.body) return "the body";
  return element.getAttribute("aria-label") ?? element.textContent?.trim() ?? element.tagName;
}

// A write of a group that lands keeps its buttons held until the live update carrying it is
// drawn: the card is keyed by the group's id, so that update fills the same card in place, and
// the group's new `updated_at` ends the hold. Filled, the card's buttons are free again. On the
// held clock, so a second after the landing - when the editor has long heard that the write is no
// longer out - is a moment the row picks.
test("a group's buttons stay held after its write lands, until the live update carrying it is drawn", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const removed = { ...crew, sessions: ["s9"], updated_at: "2026-10-11T00:01:00Z" };
  // Held, and given inside `act` after the press, as docs/testing.md ("The act rule of a Dispatch
  // unit test") says.
  const answer = Promise.withResolvers<DeliverySessionGroup>();
  const update = spyOn(api, "updateDeliverySessionGroup").mockReturnValue(answer.promise);
  const { draw, queryClient } = renderEditor([crew]);
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    await user.click(within(group).getByRole("button", { name: "Remove Planner two" }));
    expect(update).toHaveBeenCalledWith(crew.id, { remove: ["s2"] }, keyedAttempt);
    await act(async () => answer.resolve(removed));
    await jump(1_000);
    expect(queryClient.isMutating()).toBe(0);
    for (const button of groupButtons(group))
      expect(button).toHaveAttribute("aria-disabled", "true");

    draw([removed]);
    expect(screen.getByRole("group", { name: "Release crew" })).toBe(group);
    expect(within(group).queryByRole("button", { name: "Remove Planner two" })).toBeNull();
    expect(within(group).getByRole("button", { name: "Delete group" })).not.toHaveAttribute(
      "aria-disabled"
    );
  } finally {
    realClock();
    cleanup();
    update.mockRestore();
  }
});

// The live update that would carry the write never comes - the stream is down, say - so the
// hold ends at the app's write deadline, and the group's buttons are free again.
test("a group's buttons held for a live update that never comes are free again at the write deadline", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  // Held, and given inside `act` after the press, as docs/testing.md ("The act rule of a Dispatch
  // unit test") says.
  const answer = Promise.withResolvers<DeliverySessionGroup>();
  const update = spyOn(api, "updateDeliverySessionGroup").mockReturnValue(answer.promise);
  const { queryClient } = renderEditor([crew]);
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    await user.click(within(group).getByRole("button", { name: "Remove Planner two" }));
    expect(update).toHaveBeenCalledWith(crew.id, { remove: ["s2"] }, keyedAttempt);
    await act(async () => answer.resolve(crew));
    await jump(0);
    expect(queryClient.isMutating()).toBe(0);
    // Held from the landing, a second short of the deadline still, and free again past it.
    await jump(SEND_DEADLINE_MS - 1_000);
    for (const button of groupButtons(group))
      expect(button).toHaveAttribute("aria-disabled", "true");
    await jump(1_000);
    for (const button of groupButtons(group)) expect(button).not.toHaveAttribute("aria-disabled");
  } finally {
    realClock();
    cleanup();
    update.mockRestore();
  }
});

// The hold that the write deadline ends says why on the group's card, as every hold through a
// read does at its bound: the live update that would carry the write has not come, in the words
// the card says for the groups that update would bring. The line goes once that update is drawn.
test("a group's write whose live update never comes says why on its card at the write deadline, until the update is drawn", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  // Held, and given inside `act` after the press, as docs/testing.md ("The act rule of a Dispatch
  // unit test") says.
  const answer = Promise.withResolvers<DeliverySessionGroup>();
  const update = spyOn(api, "updateDeliverySessionGroup").mockReturnValue(answer.promise);
  const { draw } = renderEditor([crew]);
  const why = "Couldn't load the session groups. Dispatch did not answer within 30 seconds.";
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    await user.click(within(group).getByRole("button", { name: "Remove Planner two" }));
    await act(async () => answer.resolve(crew));
    await jump(SEND_DEADLINE_MS - 1_000);
    expect(within(group).queryByRole("alert")).toBeNull();

    await jump(1_000);
    for (const button of groupButtons(group)) expect(button).not.toHaveAttribute("aria-disabled");
    expect(
      within(group).queryByRole("alert")?.textContent,
      "the card let its buttons go at the deadline and said nothing of the update it waited for"
    ).toBe(why);

    draw([{ ...crew, sessions: ["s9"], updated_at: "2026-10-11T00:01:00Z" }]);
    expect(within(group).queryByRole("alert")).toBeNull();
  } finally {
    realClock();
    cleanup();
    update.mockRestore();
  }
});

/** Lets the write that `pressed` made land, with Chromium's way with a button that turns disabled
 *  (`emulateChromiumFocusFixup`): `pressed` is held by `aria-disabled`, which no engine takes the
 *  focus off, and keeps the focus. Then draws `next`, the live update carrying the write, and
 *  waits a frame and two tasks. */
async function landAndDraw(
  pressed: HTMLButtonElement,
  next: readonly DeliverySessionGroup[],
  editor: DrawnEditor
): Promise<void> {
  await waitFor(() => expect(editor.queryClient.isMutating()).toBe(0));
  await afterFrameAndTasks();
  expect(pressed).toHaveAttribute("aria-disabled", "true");
  expect(pressed.disabled).toBe(false);
  expect(named(document.activeElement)).toBe(named(pressed));
  editor.draw(next);
  await afterFrameAndTasks();
}

// A card's button is held from its press until the live update carrying its write is drawn, by
// `aria-disabled`, so Chromium does not take the focus off it as it would off a disabled button.
// The card is keyed by the group's id, so that update fills it in place: Rename stays the same
// element, still held now that the name is the stored one, and keeps the focus through the write
// and after it - never the page's body, nor the Name box of a card drawn anew, where the reader's
// next key would be typed.
test("Rename pressed keeps the focus through its write, and once the live update draws the renamed group", async () => {
  const user = userEvent.setup();
  const renamed = { ...crew, name: "Ship crew", updated_at: "2026-10-11T00:01:00Z" };
  // Held, and given inside `act` after the press, as docs/testing.md ("The act rule of a Dispatch
  // unit test") says.
  const answer = Promise.withResolvers<DeliverySessionGroup>();
  const update = spyOn(api, "updateDeliverySessionGroup").mockReturnValue(answer.promise);
  const restoreFocus = emulateChromiumFocusFixup();
  const editor = renderEditor([crew], { inMain: true });
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    const name = within(group).getByLabelText("Name") as HTMLInputElement;
    await user.clear(name);
    await user.click(name);
    // Typed with spaces around it; the server stores the name trimmed.
    await user.paste(" Ship crew ");
    const rename = within(group).getByRole("button", { name: "Rename" }) as HTMLButtonElement;
    await user.click(rename);
    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    // The write is out: Rename keeps the focus, held.
    await afterFrameAndTasks();
    expect(named(document.activeElement)).toBe("Rename");
    expect(rename).toHaveAttribute("aria-disabled", "true");
    await act(async () => answer.resolve(renamed));
    await landAndDraw(rename, [renamed], editor);

    expect(named(document.activeElement)).toBe("Rename");
    expect(rename).toHaveAttribute("aria-disabled", "true");
    expect(screen.getByRole("group", { name: "Ship crew" })).toBe(group);
    expect(name.value).toBe("Ship crew");
  } finally {
    restoreFocus();
    cleanup();
    update.mockRestore();
  }
});

// The same for Add selected: the editor lets go of the selection as the write lands, so the
// button is still held by `aria-disabled` once the update is drawn, with no session left to add,
// and keeps the focus it has held since the press.
test("Add selected keeps the focus once the live update draws the group with the sessions added", async () => {
  const user = userEvent.setup();
  const added = { ...crew, sessions: ["s2", "s9", "s3"], updated_at: "2026-10-11T00:01:00Z" };
  const update = spyOn(api, "updateDeliverySessionGroup").mockResolvedValue(added);
  const restoreFocus = emulateChromiumFocusFixup();
  const editor = renderEditor([crew], { inMain: true });
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    await user.click(screen.getByRole("checkbox", { name: "Reviewer" }));
    const add = within(group).getByRole("button", { name: "Add selected" }) as HTMLButtonElement;
    await user.click(add);
    await landAndDraw(add, [added], editor);

    expect(named(document.activeElement)).toBe("Add selected");
    expect(add).toHaveAttribute("aria-disabled", "true");
    expect(screen.getByRole("group", { name: "Release crew" })).toBe(group);
    expect(within(group).getByRole("link", { name: "Reviewer" })).toBeDefined();
  } finally {
    restoreFocus();
    cleanup();
    update.mockRestore();
  }
});

// A member's Remove goes with that member by design. It keeps the focus while its write is out
// and until the live update carrying it is drawn. The card stays through that update, keyed by
// the group's id, never drawn anew.
test("a member's Remove keeps the focus, held, until the live update that takes it away with its member is drawn, and the card stays", async () => {
  const user = userEvent.setup();
  const removed = { ...crew, sessions: ["s9"], updated_at: "2026-10-11T00:01:00Z" };
  const update = spyOn(api, "updateDeliverySessionGroup").mockResolvedValue(removed);
  const restoreFocus = emulateChromiumFocusFixup();
  const editor = renderEditor([crew], { inMain: true });
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    const remove = within(group).getByRole("button", {
      name: "Remove Planner two",
    }) as HTMLButtonElement;
    await user.click(remove);
    await landAndDraw(remove, [removed], editor);

    expect(screen.getByRole("group", { name: "Release crew" })).toBe(group);
  } finally {
    restoreFocus();
    cleanup();
    update.mockRestore();
  }
});

// Delete group keeps the focus while its write is out and until the live update carrying it is
// drawn. Of the only group, that update takes the Groups section away with its card. Create
// group stays held then, with nothing to create, but in the Tab order (`aria-disabled`).
test("Delete group of the only group keeps the focus, held, until its live update takes the card away, and Create group stays held", async () => {
  const user = userEvent.setup();
  const remove = spyOn(api, "deleteDeliverySessionGroup").mockResolvedValue(undefined);
  const restoreFocus = emulateChromiumFocusFixup();
  const editor = renderEditor([crew], { inMain: true });
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    const deleting = within(group).getByRole("button", {
      name: "Delete group",
    }) as HTMLButtonElement;
    await user.click(deleting);
    await landAndDraw(deleting, [], editor);

    const createGroup = screen.getByRole("button", { name: "Create group" });
    expect(createGroup.getAttribute("aria-disabled")).toBe("true");
  } finally {
    restoreFocus();
    cleanup();
    remove.mockRestore();
  }
});

// Rename pressed by Enter is held from its press until the live update carrying its write is
// drawn (`useHoldUntilDrawn`), by `aria-disabled`, which Chromium does not take the focus off. It
// keeps the focus through the write and once that update is drawn, held then because the name is
// now the stored one.
test("a Rename by Enter keeps the focus, held, through its write and once its live update is drawn", async () => {
  const user = userEvent.setup();
  const renamed = { ...crew, name: "Ship crew", updated_at: "2026-10-11T00:01:00Z" };
  const update = spyOn(api, "updateDeliverySessionGroup").mockResolvedValue(renamed);
  const restoreFocus = emulateChromiumFocusFixup();
  const editor = renderEditor([crew], { inMain: true });
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    const name = within(group).getByLabelText("Name") as HTMLInputElement;
    await user.clear(name);
    await user.click(name);
    await user.paste("Ship crew");
    await user.tab();
    const rename = within(group).getByRole("button", { name: "Rename" }) as HTMLButtonElement;
    expect(rename).toHaveFocus();
    await user.keyboard("{Enter}");
    await landAndDraw(rename, [renamed], editor);
    expect(named(document.activeElement)).toBe("Rename");
    expect(rename).toHaveAttribute("aria-disabled", "true");
  } finally {
    restoreFocus();
    cleanup();
    update.mockRestore();
  }
});

// The live update carrying a write can be drawn before the write's answer comes back. The card's
// hold waits for that update, which is drawn already, and Rename, held because the name is now the
// stored one, keeps the focus once the answer lands.
test("a Rename whose live update is drawn before its answer keeps the focus once the answer lands", async () => {
  const user = userEvent.setup();
  const renamed = { ...crew, name: "Ship crew", updated_at: "2026-10-11T00:01:00Z" };
  // Held, and given inside `act` after the press, as docs/testing.md ("The act rule of a Dispatch
  // unit test") says.
  const answer = Promise.withResolvers<DeliverySessionGroup>();
  const update = spyOn(api, "updateDeliverySessionGroup").mockReturnValue(answer.promise);
  const editor = renderEditor([crew], { inMain: true });
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    const name = within(group).getByLabelText("Name") as HTMLInputElement;
    await user.clear(name);
    await user.click(name);
    await user.paste("Ship crew");
    const rename = within(group).getByRole("button", { name: "Rename" }) as HTMLButtonElement;
    await user.click(rename);
    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    // The live update carrying the rename, drawn while its write is still out.
    editor.draw([renamed]);
    await act(async () => answer.resolve(renamed));
    await waitFor(() => expect(editor.queryClient.isMutating()).toBe(0));
    // A frame and two tasks after the answer, Rename still holds the focus.
    await afterFrameAndTasks();
    expect(named(document.activeElement)).toBe("Rename");
  } finally {
    cleanup();
    update.mockRestore();
  }
});

// The live update carrying a write can come in the same batch as the write's answer. The landing
// puts the card's hold on, and the render that would draw that hold draws the update's version
// already, so it lets the hold go before any commit has drawn it. Rename, held because the name
// is now the stored one, keeps the focus once both are drawn.
test("a Rename whose live update comes in the same batch as its answer keeps the focus once both are drawn", async () => {
  const user = userEvent.setup();
  const renamed = { ...crew, name: "Ship crew", updated_at: "2026-10-11T00:01:00Z" };
  // Held, and given inside `act` after the press, as docs/testing.md ("The act rule of a Dispatch
  // unit test") says.
  const answer = Promise.withResolvers<DeliverySessionGroup>();
  const update = spyOn(api, "updateDeliverySessionGroup").mockReturnValue(answer.promise);
  const editor = renderEditor([crew], { inMain: true });
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    const name = within(group).getByLabelText("Name") as HTMLInputElement;
    await user.clear(name);
    await user.click(name);
    await user.paste("Ship crew");
    await user.click(within(group).getByRole("button", { name: "Rename" }));
    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    // The answer lands in a chain of microtasks, and inside `act` React commits nothing until the
    // task they run in ends. So the card's hold is put on while the card still draws the group it
    // was pressed on, and the live update drawn in that task is drawn in the same render as that
    // hold.
    await act(async () => {
      answer.resolve(renamed);
      for (let turn = 0; turn < 100 && editor.queryClient.isMutating() > 0; turn += 1) {
        await Promise.resolve();
      }
      expect(editor.queryClient.isMutating()).toBe(0);
      editor.draw([renamed]);
    });
    // A frame and two tasks after both are drawn, Rename still holds the focus.
    await afterFrameAndTasks();
    expect(named(document.activeElement)).toBe("Rename");
  } finally {
    cleanup();
    update.mockRestore();
  }
});

// Create group, its group made and drawn, is held after it with its name box emptied: it has
// nothing to create.
test("Create group stays held once its group is made and drawn", async () => {
  const user = userEvent.setup();
  const made: DeliverySessionGroup = {
    ...crew,
    id: "22222222-2222-2222-2222-222222222222",
    name: "Night crew",
    sessions: ["s1"],
  };
  const create = spyOn(api, "createDeliverySessionGroup").mockResolvedValue(made);
  const editor = renderEditor([crew], { inMain: true });
  try {
    await user.click(screen.getByRole("checkbox", { name: "Planner one" }));
    await user.click(screen.getByLabelText("New group name"));
    await user.paste("Night crew");
    await user.click(screen.getByRole("button", { name: "Create group" }));
    await waitFor(() => expect(editor.queryClient.isMutating()).toBe(0));
    editor.draw([crew, made]);
    await afterFrameAndTasks();
    expect(screen.getByRole("button", { name: "Create group" }).getAttribute("aria-disabled")).toBe(
      "true"
    );
  } finally {
    cleanup();
    create.mockRestore();
  }
});

// Create group is held while its write is out, and while it has nothing to create, by
// `aria-disabled`, never `disabled`. A disabled button drops the focus to the page's body in a
// browser, and does here under `emulateChromiumFocusFixup`. The create lands and empties the name,
// so the pressed button keeps the focus through the write and after it, and a second press, by key
// or by pointer, sends nothing.
test("Create group keeps the focus while its write is out and once it has emptied the name, and a second press sends nothing", async () => {
  const user = userEvent.setup();
  // The create's answer, held until the case gives it inside `act`.
  const answer = Promise.withResolvers<DeliverySessionGroup>();
  const create = spyOn(api, "createDeliverySessionGroup").mockReturnValue(answer.promise);
  const stopFocusFixup = emulateChromiumFocusFixup();
  const editor = renderEditor();
  try {
    await user.click(screen.getByRole("checkbox", { name: "Planner one" }));
    await user.click(screen.getByLabelText("New group name"));
    await user.paste("Night crew");
    const button = screen.getByRole("button", { name: "Create group" });
    await pressHeld(user, button, {
      name: "Create group",
      sent: () => create.mock.calls.length,
    });

    await act(async () => answer.resolve({ ...crew, name: "Night crew", sessions: ["s1"] }));
    await waitFor(() => expect(editor.queryClient.isMutating()).toBe(0));
    expect((screen.getByLabelText("New group name") as HTMLInputElement).value).toBe("");
    expect(named(document.activeElement)).toBe("Create group");
    expect(button.getAttribute("aria-disabled")).toBe("true");
    await user.keyboard("{Enter}");
    await user.click(button);
    expect(create).toHaveBeenCalledTimes(1);
  } finally {
    stopFocusFixup();
    cleanup();
    create.mockRestore();
  }
});

// Each member is an agent linked to its live conversation; one no pull request of the window
// names has no stored title, so it reads by its short id.
test("group members link to their live conversations, an untitled one by its short id", () => {
  renderEditor([crew]);
  try {
    const group = screen.getByRole("group", { name: "Release crew" });
    expect(within(group).getByRole("link", { name: "Planner two" })).toHaveAttribute(
      "href",
      "/agents/s2/live"
    );
    expect(within(group).getByRole("link", { name: "session:s9…" })).toHaveAttribute(
      "href",
      "/agents/s9/live"
    );
    expect(within(group).getByRole("button", { name: "Remove session:s9…" })).toBeDefined();
  } finally {
    cleanup();
  }
});

// The Layout rule (`web/src/AGENTS.md`), point 3: a group's sessions are a list as long as its
// data, so the group shows ten chips, each with its ✕, and one "+N" that shows the rest in place.
test("a group shows ten of its sessions and a +N that shows the rest", async () => {
  const user = userEvent.setup();
  const sessions = Array.from({ length: 12 }, (_, index) => `member-${index + 1}`);
  renderEditor([{ ...crew, sessions }]);
  try {
    const members = within(screen.getByRole("group", { name: "Release crew" })).getByRole("list", {
      name: "Sessions in Release crew",
    });
    const removable = () => within(members).getAllByRole("button", { name: /^Remove / });
    expect(removable()).toHaveLength(10);
    await user.click(within(members).getByRole("button", { name: "+2, show all 12 sessions" }));
    expect(removable()).toHaveLength(12);
  } finally {
    cleanup();
  }
});

// The Layout rule (`web/src/AGENTS.md`), point 6: a group the reader opened past its "+N" stays
// open when the group changes: a write moves its `updated_at`, and the card, kept by its group's
// id, keeps its `CappedGroup` and the reader's choice in it.
test("a group's opened sessions stay open after a member past the tenth is removed", async () => {
  const user = userEvent.setup();
  const sessions = Array.from({ length: 12 }, (_, index) => `member-${index + 1}`);
  const editor = renderEditor([{ ...crew, sessions }]);
  const members = () =>
    within(screen.getByRole("group", { name: "Release crew" })).getByRole("list", {
      name: "Sessions in Release crew",
    });
  try {
    await user.click(within(members()).getByRole("button", { name: "+2, show all 12 sessions" }));
    editor.draw([{ ...crew, sessions: sessions.slice(0, 11), updated_at: "2026-10-12T00:00:00Z" }]);
    expect(within(members()).getAllByRole("button", { name: /^Remove / })).toHaveLength(11);
    expect(within(members()).getByRole("button", { name: "Show fewer sessions" })).toBeDefined();
  } finally {
    cleanup();
  }
});

// The Layout rule (`web/src/AGENTS.md`), point 6: a change to the group from elsewhere - a
// member removed, another tab's rename - redraws its card in place and keeps the reader's rename
// draft. With no draft, the field shows the group's name as it now stands.
test("a group's rename draft survives a change to the group, and with no draft the field follows a rename", async () => {
  const user = userEvent.setup();
  const other: DeliverySessionGroup = { ...crew, id: "2", name: "Night crew", sessions: ["s3"] };
  const editor = renderEditor([crew, other]);
  const nameOf = (group: string) =>
    within(screen.getByRole("group", { name: group })).getByRole<HTMLInputElement>("textbox", {
      name: "Name",
    });
  try {
    await user.clear(nameOf("Release crew"));
    await user.type(nameOf("Release crew"), "Ship crew");
    editor.draw([
      { ...crew, sessions: ["s1"], updated_at: "2026-10-12T00:00:00Z" },
      { ...other, name: "Day crew", updated_at: "2026-10-12T00:00:00Z" },
    ]);
    expect(nameOf("Release crew").value).toBe("Ship crew");
    expect(nameOf("Day crew").value).toBe("Day crew");
  } finally {
    cleanup();
  }
});

// A space typed after a group's name, to start its next word, stays as the reader typed it, and
// the caret after it, so the next word is a word of its own rather than run into the name.
test("a space typed after a group's name stays, so the next word is a word of its own", async () => {
  const user = userEvent.setup();
  renderEditor([{ ...crew, name: "Ship" }]);
  try {
    const name = within(screen.getByRole("group", { name: "Ship" })).getByRole<HTMLInputElement>(
      "textbox",
      { name: "Name" }
    );
    await user.type(name, " ");
    await user.keyboard("crew");
    expect(name.value).toBe("Ship crew");
  } finally {
    cleanup();
  }
});

// A rename sent with a space after its last word is saved without it. Once the group's name is
// the one this card's Rename sent, the field follows the group's name again, and so shows a later
// rename from another tab, rather than keep the reader's old text with Rename live over it.
test("a rename sent with a trailing space follows the group's name once it lands, and then another tab's rename", async () => {
  const user = userEvent.setup();
  const update = spyOn(api, "updateDeliverySessionGroup").mockResolvedValue({
    ...crew,
    name: "Ship crew",
  });
  const editor = renderEditor([crew]);
  const nameOf = (group: string) =>
    within(screen.getByRole("group", { name: group })).getByRole<HTMLInputElement>("textbox", {
      name: "Name",
    });
  try {
    await user.clear(nameOf("Release crew"));
    await user.type(nameOf("Release crew"), "Ship crew ");
    await user.click(
      within(screen.getByRole("group", { name: "Release crew" })).getByRole("button", {
        name: "Rename",
      })
    );
    await waitFor(() =>
      expect(update).toHaveBeenCalledWith(crew.id, { name: "Ship crew" }, keyedAttempt)
    );
    await waitFor(() => expect(editor.queryClient.isMutating()).toBe(0));
    editor.draw([{ ...crew, name: "Ship crew", updated_at: "2026-10-12T00:00:00Z" }]);
    expect(nameOf("Ship crew").value).toBe("Ship crew");
    editor.draw([{ ...crew, name: "Day crew", updated_at: "2026-10-13T00:00:00Z" }]);
    expect(nameOf("Day crew").value).toBe("Day crew");
  } finally {
    cleanup();
    update.mockRestore();
  }
});

// A rename Dispatch refused is no rename of this card's: the space the reader typed after its last
// word stays as typed when another tab then renames the group to the name it trims to.
test("a refused rename keeps its trailing space when another tab renames the group to the name it trims to", async () => {
  const user = userEvent.setup();
  const update = spyOn(api, "updateDeliverySessionGroup").mockRejectedValue(
    new ApiError(409, {
      code: "SESSION_GROUP_NAME_TAKEN",
      error: 'the group "Ship crew" already has that name',
    })
  );
  const editor = renderEditor([crew]);
  const nameOf = (group: string) =>
    within(screen.getByRole("group", { name: group })).getByRole<HTMLInputElement>("textbox", {
      name: "Name",
    });
  try {
    await user.clear(nameOf("Release crew"));
    await user.type(nameOf("Release crew"), "Ship crew ");
    await user.click(
      within(screen.getByRole("group", { name: "Release crew" })).getByRole("button", {
        name: "Rename",
      })
    );
    expect(
      await screen.findByText(
        `Couldn't save the rename of the group Release crew — the group "Ship crew" already has that name`
      )
    ).toBeDefined();
    editor.draw([{ ...crew, name: "Ship crew", updated_at: "2026-10-12T00:00:00Z" }]);
    expect(nameOf("Ship crew").value).toBe("Ship crew ");
  } finally {
    cleanup();
    update.mockRestore();
  }
});

// Another tab's rename that lands while this card's Rename is still out is no answer to it: the
// space the reader typed after its last word stays as typed, and the field follows the group's
// name once this card's Rename lands.
test("a rename still out keeps its trailing space under another tab's rename to that name, until it lands", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<DeliverySessionGroup>();
  const update = spyOn(api, "updateDeliverySessionGroup").mockReturnValue(answer.promise);
  const editor = renderEditor([crew]);
  const nameOf = (group: string) =>
    within(screen.getByRole("group", { name: group })).getByRole<HTMLInputElement>("textbox", {
      name: "Name",
    });
  try {
    await user.clear(nameOf("Release crew"));
    await user.type(nameOf("Release crew"), "Ship crew ");
    await user.click(
      within(screen.getByRole("group", { name: "Release crew" })).getByRole("button", {
        name: "Rename",
      })
    );
    await waitFor(() =>
      expect(update).toHaveBeenCalledWith(crew.id, { name: "Ship crew" }, keyedAttempt)
    );
    editor.draw([{ ...crew, name: "Ship crew", updated_at: "2026-10-12T00:00:00Z" }]);
    expect(nameOf("Ship crew").value).toBe("Ship crew ");
    answer.resolve({ ...crew, name: "Ship crew", updated_at: "2026-10-12T00:00:01Z" });
    await waitFor(() => expect(nameOf("Ship crew").value).toBe("Ship crew"));
  } finally {
    cleanup();
    update.mockRestore();
  }
});

// This card's Rename lands, and Dispatch's delta for it has not reached the editor; another tab
// then renames the same group. The Rename's answer is the group from its landing on, written into
// the page's groups read as well, so the field follows the newer name, rather than keep this
// card's old draft over it with Rename live.
test("a landed rename whose delta never arrives follows another tab's later rename of the group", async () => {
  const user = userEvent.setup();
  const renamed = { ...crew, name: "Ship crew", updated_at: "2026-10-12T00:00:00Z" };
  const update = spyOn(api, "updateDeliverySessionGroup").mockResolvedValue(renamed);
  const editor = renderEditor([crew]);
  const timeline = seedTimeline(editor.queryClient, [crew]);
  const nameOf = (group: string) =>
    within(screen.getByRole("group", { name: group })).getByRole<HTMLInputElement>("textbox", {
      name: "Name",
    });
  try {
    await user.clear(nameOf("Release crew"));
    await user.type(nameOf("Release crew"), "Ship crew");
    await user.click(
      within(screen.getByRole("group", { name: "Release crew" })).getByRole("button", {
        name: "Rename",
      })
    );
    await waitFor(() =>
      expect(update).toHaveBeenCalledWith(crew.id, { name: "Ship crew" }, keyedAttempt)
    );
    await waitFor(() => expect(editor.queryClient.isMutating()).toBe(0));
    expect(nameOf("Release crew").value).toBe("Ship crew");
    editor.draw([{ ...crew, name: "Day crew", updated_at: "2026-10-12T00:00:05Z" }]);
    expect(nameOf("Day crew").value).toBe("Day crew");
    expect(editor.queryClient.getQueryData(timeline)?.session_groups).toEqual([renamed]);
  } finally {
    cleanup();
    update.mockRestore();
  }
});

// Dispatch took this card's Rename and had no answer to give for it (`409 KEY_COMMITTED`): the
// group stands as the reads that landing starts answer it, so the draft stays as typed until a
// version of the group other than the one in effect at that landing arrives - the group as it
// was, redrawn, is no answer - and then follows the group's name.
test("a rename landed with no answer keeps its draft until a version of the group other than the one at its landing arrives", async () => {
  const user = userEvent.setup();
  const update = spyOn(api, "updateDeliverySessionGroup").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "made; the answer was lost" })
  );
  const editor = renderEditor([crew]);
  const nameOf = (group: string) =>
    within(screen.getByRole("group", { name: group })).getByRole<HTMLInputElement>("textbox", {
      name: "Name",
    });
  try {
    await user.clear(nameOf("Release crew"));
    await user.type(nameOf("Release crew"), "Ship crew ");
    await user.click(
      within(screen.getByRole("group", { name: "Release crew" })).getByRole("button", {
        name: "Rename",
      })
    );
    await waitFor(() =>
      expect(update).toHaveBeenCalledWith(crew.id, { name: "Ship crew" }, keyedAttempt)
    );
    await waitFor(() => expect(editor.queryClient.isMutating()).toBe(0));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(nameOf("Release crew").value).toBe("Ship crew ");
    editor.draw([crew]);
    expect(nameOf("Release crew").value).toBe("Ship crew ");
    editor.draw([{ ...crew, name: "Day crew", updated_at: "2026-10-12T00:00:05Z" }]);
    expect(nameOf("Day crew").value).toBe("Day crew");
  } finally {
    cleanup();
    update.mockRestore();
  }
});

// Another tab renames the group while this card's Rename is out, and this Rename then lands with
// no answer to give (`409 KEY_COMMITTED`). The other tab's rename is the version in effect at the
// landing, no answer to this Rename: the draft stays as typed until the read the landing starts
// answers a version after it, and then follows the group's name.
test("a rename landed with no answer waits for the read its landing starts, past another tab's rename that arrived while it was out", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<DeliverySessionGroup>();
  const update = spyOn(api, "updateDeliverySessionGroup").mockReturnValue(answer.promise);
  const editor = renderEditor([crew]);
  const nameOf = (group: string) =>
    within(screen.getByRole("group", { name: group })).getByRole<HTMLInputElement>("textbox", {
      name: "Name",
    });
  try {
    await user.clear(nameOf("Release crew"));
    await user.type(nameOf("Release crew"), "Ship crew ");
    await user.click(
      within(screen.getByRole("group", { name: "Release crew" })).getByRole("button", {
        name: "Rename",
      })
    );
    await waitFor(() =>
      expect(update).toHaveBeenCalledWith(crew.id, { name: "Ship crew" }, keyedAttempt)
    );
    editor.draw([{ ...crew, name: "Day crew", updated_at: "2026-10-12T00:00:05Z" }]);
    expect(nameOf("Day crew").value).toBe("Ship crew ");
    answer.reject(new ApiError(409, { code: "KEY_COMMITTED", error: "made; the answer was lost" }));
    await waitFor(() => expect(editor.queryClient.isMutating()).toBe(0));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(nameOf("Day crew").value).toBe("Ship crew ");
    editor.draw([{ ...crew, name: "Ship crew", updated_at: "2026-10-12T00:00:06Z" }]);
    expect(nameOf("Ship crew").value).toBe("Ship crew");
  } finally {
    cleanup();
    update.mockRestore();
  }
});

test("a refusal shows as the server wrote it", async () => {
  const user = userEvent.setup();
  const create = spyOn(api, "createDeliverySessionGroup").mockRejectedValue(
    new ApiError(409, {
      code: "SESSION_IN_GROUP",
      error: 'session s2 is already in the group "Release crew"; remove it there first',
    })
  );
  renderEditor();
  try {
    await user.click(screen.getByRole("checkbox", { name: "Planner two" }));
    await user.click(screen.getByLabelText("New group name"));
    await user.paste("Other");
    await user.click(screen.getByRole("button", { name: "Create group" }));
    expect(
      await screen.findByText(
        `Couldn't save the new group Other — session s2 is already in the group "Release crew"; remove it there first`
      )
    ).toBeDefined();
  } finally {
    cleanup();
    create.mockRestore();
  }
});

// One refusal speaks for every group, so it names the group it was about, and goes once that
// group is renamed or removed - from another tab, or by another person - rather than stand beside
// groups it was never about, naming one that is not there.
test("a refusal goes once the group it was about is renamed or removed", async () => {
  const user = userEvent.setup();
  const remove = spyOn(api, "deleteDeliverySessionGroup").mockRejectedValue(
    new ApiError(403, { code: "FORBIDDEN", error: "only its maker may delete a group" })
  );
  const other: DeliverySessionGroup = { ...crew, id: "2", name: "Night crew", sessions: ["s3"] };
  const editor = renderEditor([crew, other]);
  try {
    const refusal = (group: string) =>
      `Couldn't save the deletion of the group ${group} — only its maker may delete a group`;
    const deleteOf = async (group: string) =>
      user.click(
        within(screen.getByRole("group", { name: group })).getByRole("button", {
          name: "Delete group",
        })
      );

    await deleteOf("Release crew");
    expect(await screen.findByText(refusal("Release crew"))).toBeDefined();
    editor.draw([{ ...crew, name: "Ship crew", updated_at: "2026-10-12T00:00:00Z" }, other]);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    await deleteOf("Night crew");
    expect(await screen.findByText(refusal("Night crew"))).toBeDefined();
    editor.draw([{ ...crew, name: "Ship crew", updated_at: "2026-10-12T00:00:00Z" }]);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    cleanup();
    remove.mockRestore();
  }
});

// Dispatch made the group and lost its answer (`409 KEY_COMMITTED`): a landing, not a refusal. The
// name and the selection go, so a second press makes nothing, and the editor says what was made
// instead of showing a refusal.
test("a group made with its answer lost clears the form and says so, and a second press makes nothing", async () => {
  const user = userEvent.setup();
  const create = spyOn(api, "createDeliverySessionGroup").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "made; the answer was lost" })
  );
  const { queryClient } = renderEditor();
  const reads = seedDeliveryReads(queryClient);
  try {
    await user.click(screen.getByRole("checkbox", { name: "Planner one" }));
    await user.click(screen.getByLabelText("New group name"));
    await user.paste("Crew");
    await user.click(screen.getByRole("button", { name: "Create group" }));
    const made = await screen.findByTestId("landed-without-answer");
    expect(made.textContent).toBe(
      "The group “Crew” was made, but Dispatch lost its answer, so it may take a moment to appear."
    );
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect((screen.getByLabelText("New group name") as HTMLInputElement).value).toBe("");
    expect(
      (screen.getByRole("checkbox", { name: "Planner one" }) as HTMLInputElement).checked
    ).toBe(false);
    expect(screen.getByRole("button", { name: "Create group" }).getAttribute("aria-disabled")).toBe(
      "true"
    );
    await waitFor(() => expect(queryClient.getQueryState(reads.run)?.isInvalidated).toBe(true));
    expect(create).toHaveBeenCalledTimes(1);
  } finally {
    cleanup();
    create.mockRestore();
  }
});
