import { afterEach, beforeEach, expect, type Mock, setSystemTime, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { installIntersectionObserver } from "../../__tests__/intersection-observer";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { installResizeObserver } from "../../__tests__/scroll-box";
import { api } from "../../api/client";
import { prependEventToLog } from "../../api/event-feed";
import type {
  Artifact,
  DispatchEvent,
  DisplayTree,
  Handoff,
  IssueAskRow,
  IssueDetails,
  MessageRead,
} from "../../api/types";
import { linkText, pageInkText } from "../../theme/classes";
import { dateKey, dayLabel } from "../activity/activity-model";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { type PublishedReferences, ReferenceNamesProvider } from "../refs/reference-names";
import { buildDispatchReference, buildIssuePath } from "../refs/routes";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { measureNearWords } from "./clamp-fixtures";
import { HistoryHandoff } from "./HistoryHandoff";
import { type AddressedTurn, IssueHistory } from "./IssueHistory";
import { type OutlineWriter, OutlineWriterProvider, useOutlineWriter } from "./outline-writer";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import { Publisher, publishedNames } from "./projection-fixture";
import { failRefreshOverHeld } from "./refresh-over-held-fixture";
import { PAGE_SIZE, SHOW_MORE, showMoreName } from "./show-more-fixture";

const KEY = "CORE-1";

// The history heads each day it reads with a line, and these cases date what it reads some
// seconds, minutes or days before now. Each case runs at one fixed midday, so none reads the
// wall clock: one started just after midnight would put its entries on yesterday.
// `setSystemTime` holds `Date` alone; the timers the cases wait on (React Query's, `waitFor`'s,
// the animation frames) run on the real clock as before.
const NOON = new Date(2026, 10, 19, 12, 0, 0, 0);
beforeEach(() => setSystemTime(NOON));
afterEach(() => setSystemTime());

/** The entries of the history as it draws them, newest first: a line of an event, a run of
 *  uploads or a thread, and never the line that says what is new. */
function drawnEntries(): HTMLElement[] {
  return [
    ...document.querySelectorAll<HTMLElement>("[data-history-day] > ol > [data-history-entry]"),
  ];
}

/** A claim of the issue, which the history draws as one line (`history-model.ts`), made
 *  `20 - seq` seconds ago, so a higher sequence is newer and every one is today's. */
function claim(seq: number): DispatchEvent {
  const at = new Date(Date.now() - (20 - seq) * 1000).toISOString();
  return {
    actor: { id: "planner", kind: "session" },
    created_at: at,
    id: seq,
    issue_key: KEY,
    notify: false,
    payload: {},
    seq,
    type: "issue.claimed",
  } as DispatchEvent;
}

/** The reads the history makes (`IssueHistory.tsx`), stubbed: the log answers `events` and the
 *  issue's messages read `messages`, with no comment and no ask. Returns what restores them. */
function stubHistoryReads(
  events: readonly DispatchEvent[],
  messages: readonly MessageRead[] = []
): () => void {
  const reads = [
    spyOn(api, "getIssueEvents").mockResolvedValue([...events]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([...messages]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  return () => {
    for (const read of reads) read.mockRestore();
  };
}

const ROW_HEIGHT = 40;
const LIST_TOP = 100;

function setScrollY(value: number): void {
  Object.defineProperty(window, "scrollY", { configurable: true, value, writable: true });
}

/** The geometry `ViewportAnchor` reads, faked as `ViewportAnchor.test.tsx` fakes it: every entry
 *  is `ROW_HEIGHT` tall, stacked from `LIST_TOP` in DOM order, and the page scrolls through them
 *  with `window.scrollY`. Anything that is not an entry measures as the list's own box. */
function installRowLayout(): { restore: () => void } {
  const rows = () => [...document.querySelectorAll<HTMLElement>("[data-event-seq]")];
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const all = rows();
    const index = all.indexOf(this as HTMLElement);
    const top = LIST_TOP + (index < 0 ? 0 : index * ROW_HEIGHT) - window.scrollY;
    const height = index < 0 ? all.length * ROW_HEIGHT : ROW_HEIGHT;
    return {
      bottom: top + height,
      height,
      left: 0,
      right: 100,
      toJSON: () => ({}),
      top,
      width: 100,
      x: 0,
      y: top,
    } as DOMRect;
  });
  const point = spyOn(document, "elementFromPoint").mockImplementation(
    (_x, y) => rows()[Math.floor((window.scrollY + y - LIST_TOP) / ROW_HEIGHT)] ?? null
  );
  const originalScrollBy = window.scrollBy;
  window.scrollBy = ((x: number | ScrollToOptions, y?: number) => {
    const top = typeof x === "number" ? y : x.top;
    if (top !== undefined) setScrollY(window.scrollY + top);
  }) as typeof window.scrollBy;
  return {
    restore: () => {
      rect.mockRestore();
      point.mockRestore();
      window.scrollBy = originalScrollBy;
    },
  };
}

// The history reads `["events", <key>]`, the query the live feed prepends every published event
// into at the front of page 0 (`prependEventToLog`), and draws it newest first: each arrival
// pushes a reader who is down in the history one entry further down unless something holds
// their place, as the old Activity tab's `ViewportAnchor` did. Each entry carries
// `data-history-entry`, its own id, the identity the anchor follows.
test("an event arriving while the reader is in the history leaves their entry where it was", async () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const restoreReads = stubHistoryReads(
    Array.from({ length: 12 }, (_, index) => claim(12 - index))
  );
  const layout = installRowLayout();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const issue = { artifacts: [], key: KEY } as unknown as IssueDetails;
  let unmount: (() => void) | undefined;

  try {
    unmount = render(
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <IssueHistory
            activityFollowing={undefined}
            issue={issue}
            item={undefined}
            state={{ [KEY]: { dismissed: [], last_read_seq: 99, pinned: false, seq: 0 } }}
            visitMark={99}
          />
        </QueryClientProvider>
      </MemoryRouter>
    ).unmount;
    await screen.findByRole("list", { name: CONTROL_LABELS.history_list });
    await waitFor(() => expect(document.querySelectorAll("[data-event-seq]")).toHaveLength(12));
    setScrollY(300);
    const reading = document.querySelectorAll<HTMLElement>("[data-event-seq]")[8];
    const seq = reading?.getAttribute("data-event-seq");
    const before = reading?.getBoundingClientRect().top;

    act(() => {
      prependEventToLog(queryClient, claim(13));
    });
    await waitFor(() => expect(document.querySelectorAll("[data-event-seq]")).toHaveLength(13));

    expect(document.querySelector(`[data-event-seq="${seq}"]`)?.getBoundingClientRect().top).toBe(
      before
    );
    // The entry it held onto moved down one place in the list, so the page scrolled by one entry.
    expect(window.scrollY).toBe(300 + ROW_HEIGHT);
  } finally {
    unmount?.();
    layout.restore();
    restoreReads();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

// A reader reading today's entries as midnight passes is still on them: an event that arrives
// after midnight heads a new day at the head of the history, and the entry the reader reads
// stays drawn where it was, as it does for any other arrival. Found by the tester when the day
// before today was drawn folded: the day the reader was in folded to one line under them once the
// first render after midnight read the new date.
test("an event arriving just after midnight leaves the entry the reader reads where it was", async () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  setSystemTime(new Date(2026, 10, 19, 23, 59, 50, 0));
  const restoreReads = stubHistoryReads(
    Array.from({ length: 12 }, (_, index) => claim(12 - index))
  );
  const layout = installRowLayout();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const issue = { artifacts: [], key: KEY } as unknown as IssueDetails;
  let unmount: (() => void) | undefined;

  try {
    unmount = render(
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <IssueHistory
            activityFollowing={undefined}
            issue={issue}
            item={undefined}
            state={{ [KEY]: { dismissed: [], last_read_seq: 99, pinned: false, seq: 0 } }}
            visitMark={99}
          />
        </QueryClientProvider>
      </MemoryRouter>
    ).unmount;
    await screen.findByRole("list", { name: CONTROL_LABELS.history_list });
    await waitFor(() => expect(document.querySelectorAll("[data-event-seq]")).toHaveLength(12));
    setScrollY(300);
    const reading = document.querySelectorAll<HTMLElement>("[data-event-seq]")[8];
    const seq = reading?.getAttribute("data-event-seq");
    const before = reading?.getBoundingClientRect().top;

    // Midnight passes; the first event of the new day arrives.
    setSystemTime(new Date(2026, 10, 20, 0, 0, 30, 0));
    act(() => {
      prependEventToLog(queryClient, claim(13));
    });
    await waitFor(() =>
      expect(
        document.querySelector('[data-event-seq="13"]'),
        "the new day's event is not drawn"
      ).not.toBeNull()
    );

    const still = document.querySelector<HTMLElement>(`[data-event-seq="${seq}"]`);
    expect(
      still,
      "the entry the reader read went from the history as midnight passed"
    ).not.toBeNull();
    expect(still?.getBoundingClientRect().top).toBe(before);
  } finally {
    unmount?.();
    layout.restore();
    restoreReads();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

/** A message the Legion daemon wrote, `ago` seconds ago: a thread of the history whose first
 *  turn's event no page of the log read here holds. */
function agentMessage(id: string, ago: number): MessageRead {
  return {
    message: {
      author: { id: "legion-daemon:CORE", kind: "session" },
      body: `Daemon message ${id}.`,
      broadcast_id: null,
      created_at: new Date(Date.now() - ago * 1000).toISOString(),
      deliveries: [],
      ...messageCorrectionFields(),
      id,
      in_reply_to: null,
      issue_key: KEY,
      reply_block: null,
      target: null,
    },
    replies: [],
  };
}

// A thread whose first turn's event is on a page of the log not read yet carries seq 0, as every
// other such thread does, so two of them share one `data-event-seq`: the anchor follows each
// entry's own id instead, or it holds the first of them while the reader is on the second.
test("a reader on one of two threads whose events are unread keeps their entry when an event arrives", async () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const restoreReads = stubHistoryReads(
    Array.from({ length: 6 }, (_, index) => claim(6 - index)),
    [agentMessage("m-older", 200), agentMessage("m-newer", 100)]
  );
  const layout = installRowLayout();
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const issue = { artifacts: [], key: KEY } as unknown as IssueDetails;
  let unmount: (() => void) | undefined;

  try {
    unmount = render(
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <IssueHistory
            activityFollowing={undefined}
            issue={issue}
            item={undefined}
            state={{ [KEY]: { dismissed: [], last_read_seq: 99, pinned: false, seq: 0 } }}
            visitMark={99}
          />
        </QueryClientProvider>
      </MemoryRouter>
    ).unmount;
    await screen.findByRole("list", { name: CONTROL_LABELS.history_list });
    await waitFor(() => expect(document.querySelectorAll("[data-event-seq]")).toHaveLength(8));
    const entries = [...document.querySelectorAll<HTMLElement>("[data-event-seq]")];
    expect(entries.slice(6).map((entry) => entry.getAttribute("data-event-seq"))).toEqual([
      "0",
      "0",
    ]);
    // The reader is on the last entry, the older of the two threads: 360 px down puts it at the
    // centre of what the list shows of itself, where the anchor probes.
    setScrollY(360);
    const reading = entries[7];
    const before = reading?.getBoundingClientRect().top;

    act(() => {
      prependEventToLog(queryClient, claim(7));
    });
    await waitFor(() => expect(document.querySelectorAll("[data-event-seq]")).toHaveLength(9));

    expect(reading?.getBoundingClientRect().top).toBe(before);
  } finally {
    unmount?.();
    layout.restore();
    restoreReads();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

// A turn's Reply is drawn only under the page's writer, and sets it to reply to that turn; the
// page draws no box at the foot of the outline that sends it yet.
test("a turn's Reply sets the writer to reply to that turn", async () => {
  const restoreReads = stubHistoryReads([], [agentMessage("m-turn", 60)]);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["agents"], []);
  const issue = { artifacts: [], key: KEY } as unknown as IssueDetails;
  let writer: OutlineWriter | undefined;
  function Probe(): ReactNode {
    writer = useOutlineWriter();
    return null;
  }
  const history = (
    <IssueHistory
      activityFollowing={undefined}
      issue={issue}
      item={undefined}
      state={{ [KEY]: { dismissed: [], last_read_seq: 99, pinned: false, seq: 0 } }}
      visitMark={99}
    />
  );
  let unmount: (() => void) | undefined;

  try {
    // With no writer above it, a turn draws no Reply.
    const alone = render(
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>{history}</QueryClientProvider>
      </MemoryRouter>
    );
    try {
      await waitFor(() =>
        expect(document.querySelector('[data-history-item="m-turn"]')).not.toBeNull()
      );
      expect(document.querySelectorAll("[data-history-reply]")).toHaveLength(0);
    } finally {
      alone.unmount();
    }

    unmount = render(
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <OutlineWriterProvider issueKey={KEY}>
            <Probe />
            {history}
          </OutlineWriterProvider>
        </QueryClientProvider>
      </MemoryRouter>
    ).unmount;
    const reply = await screen.findByRole("button", { name: CONTROL_LABELS.reply });
    expect(reply.getAttribute("data-history-reply")).toBe("m-turn");
    expect(writer?.reply, "the writer replied to something before Reply").toBeNull();

    await userEvent.click(reply);

    await waitFor(() =>
      expect(writer?.reply).toMatchObject({ id: "m-turn", parentKind: "message" })
    );
    expect(writer?.editing).toBeUndefined();
  } finally {
    unmount?.();
    restoreReads();
  }
});

// Every day is drawn open: a line that names the day heads its entries, and that line is no
// control, says no count and folds nothing, so a turn two days old is drawn with no press, as
// every entry of today is.
test("every day is drawn open, and its line is no control", async () => {
  const earlier = agentMessage("m-earlier", 2 * 24 * 60 * 60);
  // Today's lines beside it: a document added, then renamed, each a link to the document.
  const plan: Artifact = {
    created_at: new Date().toISOString(),
    created_by: { id: "planner", kind: "session" },
    id: "plan",
    issue_key: KEY,
    kind: "doc",
    name: "plan.md",
    primary: false,
    project: "CORE",
    ref_key: "artifact:plan",
    session_id: null,
    slug: "plan-md",
    versions: [],
  };
  const restoreReads = stubHistoryReads(
    [
      {
        ...claim(3),
        payload: { artifact: plan, previous_name: "draft.md" },
        type: "artifact.renamed",
      } as DispatchEvent,
      {
        ...claim(2),
        payload: { artifact: { ...plan, name: "draft.md" } },
        type: "artifact.created",
      } as DispatchEvent,
      claim(1),
    ],
    [earlier]
  );
  const today = dateKey(new Date().toISOString());
  const earlierDay = dateKey(earlier.message.created_at);
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    await waitFor(() =>
      expect(
        document.querySelector('[data-history-item="m-earlier"]'),
        "the turn of a day before today is not drawn"
      ).not.toBeNull()
    );
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    const days = [...list.querySelectorAll<HTMLElement>("[data-history-day]")];
    expect(days.map((day) => day.getAttribute("data-history-day"))).toEqual([today, earlierDay]);
    expect(days.map((day) => day.firstElementChild?.textContent)).toEqual([
      CONTROL_LABELS.history_today,
      dayLabel(earlierDay, today),
    ]);
    for (const day of days) {
      const line = day.firstElementChild;
      expect(line?.tagName, "a day's line is a control").toBe("P");
      expect(line?.querySelector("button, a"), "a day's line holds a control").toBeNull();
    }
    expect(list.querySelectorAll("[aria-expanded]"), "a day's line folds the day").toHaveLength(0);
    expect(drawnEntries()).toHaveLength(4);
    // A document's lines link to it in the page's ink, as an upload's do, never in the
    // application's link blue.
    for (const name of ["draft.md", "plan.md"]) {
      const link = screen.getByRole("link", { name });
      expect(link.getAttribute("href")).toBe(`/issues/${KEY}/artifacts/plan-md`);
      expect(link, `${name}'s link is not in the page's ink`).toHaveClass(pageInkText);
      for (const token of linkText.split(" ")) {
        expect(link, `${name}'s link is in the application's link blue`).not.toHaveClass(token);
      }
    }
  } finally {
    unmount?.();
    restoreReads();
  }
});

// A block's line names the block by its id, and a UUID is no name a reader reads: the history
// names a question's block by the question's first words, a link to the question's own address,
// which puts the focus on its row in the outline.
test("a repaired block's words are a link to its question's address, and its id is not drawn", async () => {
  const block = "01f349b4-2a84-4ebb-81be-91fa2f4e2300";
  const repaired = {
    ...claim(1),
    payload: { block_id: block, disturbed_by: { id: "alice", kind: "user" }, version: 3 },
    type: "block.repaired",
  } as DispatchEvent;
  const question = {
    block_id: block,
    id: "q1",
    kind: "question",
    question: "Which layout reads best on a phone?",
  };
  // The log holds no refused check, so the history reads no check.
  const reads = [
    spyOn(api, "getIssueEvents").mockResolvedValue([repaired]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([question] as never),
  ];
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    const link = await screen.findByRole("link", { name: question.question });
    expect(link).toHaveAttribute("href", buildIssuePath({ id: "q1", key: KEY, kind: "ask" }));
    expect(
      screen.getByRole("list", { name: CONTROL_LABELS.history_list }),
      "the repaired block's line draws its id"
    ).not.toHaveTextContent(block);
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

/** The following of the address that names the turn a case draws the history at
 *  (`AddressedTurn`). */
const FOLLOWING = "default /issues/CORE-1/messages";

/** The history of `KEY` with nothing above it but a query client that never retries, drawn in
 *  `container` where one is given, at the address of the turn `item` where one is given, and
 *  under the names a page published of its batched read (`published`) where those are given. */
function drawHistory(
  item: string | undefined,
  container?: HTMLElement,
  published?: PublishedReferences
): {
  queryClient: QueryClient;
  unmount: () => void;
} {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["agents"], []);
  const issue = { artifacts: [], key: KEY } as unknown as IssueDetails;
  const history = (
    <IssueHistory
      activityFollowing={undefined}
      issue={issue}
      item={item === undefined ? undefined : { following: FOLLOWING, id: item }}
      state={{ [KEY]: { dismissed: [], last_read_seq: 99999, pinned: false, seq: 0 } }}
      visitMark={99999}
    />
  );
  const { unmount } = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        {published === undefined ? (
          history
        ) : (
          <ReferenceNamesProvider>
            <Publisher published={published} />
            {history}
          </ReferenceNamesProvider>
        )}
      </QueryClientProvider>
    </MemoryRouter>,
    container === undefined ? {} : { container }
  );
  return { queryClient, unmount };
}

/** Refetches the log, whose reads the case has made fail, over the `entries` entries the history
 *  draws, and then takes it back to holding no page, whose first read fails too
 *  (`failRefreshOverHeld`). `message` says what went wrong where the entries did not stay drawn. */
function failLogRefetchThenFirstRead(
  queryClient: QueryClient,
  entries: number,
  message?: string
): Promise<void> {
  const key = ["events", KEY];
  return failRefreshOverHeld({
    client: queryClient,
    held: "the log",
    queryKey: key,
    refresh: () => queryClient.refetchQueries({ queryKey: key }),
    stillDrawn: () =>
      expect(document.querySelectorAll("[data-event-seq]"), message).toHaveLength(entries),
  });
}

/** A claim of the issue at sequence `seq`, made `ago` seconds ago. */
function claimAgo(seq: number, ago: number): DispatchEvent {
  return { ...claim(seq), created_at: new Date(Date.now() - ago * 1000).toISOString(), id: seq };
}

/** A move of a child issue's status at sequence `seq`, made `ago` seconds ago: an event of a row
 *  the outline draws, which the history leaves out. */
function outlineEventAgo(seq: number, ago: number): DispatchEvent {
  return { ...claimAgo(seq, ago), payload: {}, type: "child.status" } as DispatchEvent;
}

/** A full page of the log, 200 events one second apart from sequence `top` down, the newest
 *  made `ago` seconds ago: its newest `entries` are claims, each an entry of the history, and the
 *  rest are events it leaves out (`outlineEventAgo`). Where the history holds no more entries
 *  than it draws at once and an older page is unread, it reads that page before it draws; with
 *  `PAGE_SIZE + 1` entries it draws 20, and Show more, which says "1+", reads the next page
 *  before it draws the rest. */
function pageOf(entries: number, top: number, ago = 1): DispatchEvent[] {
  return Array.from({ length: 200 }, (_, index) =>
    index < entries ? claimAgo(top - index, ago + index) : outlineEventAgo(top - index, ago + index)
  );
}

// The address of a turn of an agents' thread opens Details at the
// history with that turn marked and on screen, whatever page of the log the thread's first turn
// is on, the history drawn down to that thread however far past its first 20 entries it stands.
// Here the log's first page is full, 200 claims of the last few minutes, and the agents'
// thread began half an hour ago, before every event of that page: the history draws a thread
// only once the page that reaches back to it is read (`HistoryInput.loadedFrom`), so the turn
// the address names must still be reached and marked. Found by the tester on a running build of
// b0082656 with a copy of a large real issue, whose log holds 769 events: the address of a
// comment of an agents' thread older than the first page opened Details and marked nothing.
test("the address of an agents' turn older than the log's first page marks that turn", async () => {
  const firstPage = Array.from({ length: 200 }, (_, index) => claimAgo(400 - index, index + 1));
  const olderPage = [claimAgo(10, 3600), claimAgo(9, 3601)];
  const reads = [
    spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) =>
      options?.before === undefined ? [...firstPage] : [...olderPage]
    ),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([agentMessage("m-old", 1800)]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory("m-old").unmount;
    await screen.findByRole("list", { name: CONTROL_LABELS.history_list });
    await waitFor(
      () =>
        expect(
          document.querySelector('[data-history-item="m-old"]')?.getAttribute("aria-current"),
          "the address of a turn older than the log's first page marks nothing"
        ).toBe("true"),
      { timeout: 5000 }
    );
    expect(
      drawnEntries(),
      "the history is not drawn down to the thread of the turn the address names"
    ).toHaveLength(201);
    expect(document.querySelector("[data-history-left]")?.textContent).toBe("2");
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// The history is the issue's log as last read, and a read
// that fails says so with Retry. A read that fails once the history holds the log - a refetch
// after an event, or on a later open of Details past the query's stale time - leaves the entries
// it holds drawn, and says no failure over them. Found by the tester on a running build of
// b0082656: with a large real issue's 79 entries drawn, Details closed and opened again 31 s
// later with the log answering 500 drew "Could not load the activity." and no entry at all.
test("a failed read of the log once the history holds it keeps every entry it holds, and says no failure", async () => {
  const restoreReads = stubHistoryReads([claim(3), claim(2), claim(1)]);
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHistory(undefined);
    unmount = drawn.unmount;
    await waitFor(() => expect(document.querySelectorAll("[data-event-seq]")).toHaveLength(3));

    const failing = spyOn(api, "getIssueEvents").mockRejectedValue(new Error("the log is down"));
    try {
      await act(async () => {
        await drawn.queryClient.refetchQueries({ queryKey: ["events", KEY] });
      });
      expect(failing).toHaveBeenCalled();
      expect(
        document.querySelectorAll("[data-event-seq]"),
        "a failed read of the log erased the history it held"
      ).toHaveLength(3);
      expect(
        drawn.queryClient.getQueryState(["events", KEY])?.status,
        "the refetch did not fail"
      ).toBe("error");
      expect(
        screen.queryByRole("alert"),
        "a failed refetch said a failure over the entries it held"
      ).toBeNull();
      expect(
        screen.queryByRole("button", { name: CONTROL_LABELS.retry }),
        "a failed refetch offered a Retry over the entries it held"
      ).toBeNull();
    } finally {
      failing.mockRestore();
    }
  } finally {
    unmount?.();
    restoreReads();
  }
});

/** An upload of a file that is no document, by `planner`, `ago` seconds ago. */
function upload(seq: number, ago: number, name: string): DispatchEvent {
  const artifact: Artifact = {
    created_at: new Date(Date.now() - ago * 1000).toISOString(),
    created_by: { id: "planner", kind: "session" },
    id: `file-${seq}`,
    issue_key: KEY,
    kind: "file",
    name,
    primary: false,
    project: "CORE",
    ref_key: `artifact:file-${seq}`,
    session_id: null,
    slug: name.replace(".", "-"),
    versions: [],
  };
  return {
    ...claimAgo(seq, ago),
    payload: { artifact },
    type: "artifact.created",
  } as DispatchEvent;
}

// Each event's line has Pin and Unpin, and a reload keeps the pin. One author's uploads in one
// minute are one entry, and that entry is the line of each of its events. Found by the tester on
// a running build of b0082656: every line of a large real issue's history drew Pin but the entry
// of thirteen uploads, which drew none.
test("the entry of one author's uploads has Pin, as every other line of the history does", async () => {
  const restoreReads = stubHistoryReads([
    upload(3, 10, "a.png"),
    upload(2, 20, "b.png"),
    claimAgo(1, 120),
  ]);
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    const uploads = await waitFor(() => {
      const entry = document.querySelector<HTMLElement>('[data-history-uploads="2"]');
      if (entry === null) throw new Error("the two uploads are not one entry");
      return entry;
    });
    // The claim's line draws its Pin: the history's own rule for a line.
    expect(screen.getAllByRole("button", { name: CONTROL_LABELS.pin }).length).toBeGreaterThan(0);
    expect(
      [...uploads.querySelectorAll("button")].filter(
        (button) => button.getAttribute("aria-label") === CONTROL_LABELS.pin
      ),
      "the uploads' entry draws no Pin"
    ).not.toHaveLength(0);
  } finally {
    unmount?.();
    restoreReads();
  }
});

// The model names a run of uploads by its newest upload, so a newer upload by the same author
// within the minute renames the run it joins. Drawn under that name, the run was unmounted and
// mounted anew, and a focus on one of its files' Pins fell to the page; the panel draws the run
// on under the key it first drew it with.
test("a newer upload that joins a run drawn keeps the run's Pin and the focus on it", async () => {
  const restoreReads = stubHistoryReads([
    upload(3, 10, "a.png"),
    upload(2, 20, "b.png"),
    claimAgo(1, 120),
  ]);
  const pinOfOlderFile = () =>
    document.querySelector<HTMLElement>('[data-history-entry="file:2"] button');
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHistory(undefined);
    unmount = drawn.unmount;
    await waitFor(() =>
      expect(document.querySelector('[data-history-uploads="2"]')).not.toBeNull()
    );
    const pin = pinOfOlderFile();
    if (pin === null) throw new Error("the run's older file draws no Pin");
    pin.focus();
    expect(document.activeElement).toBe(pin);

    act(() => {
      prependEventToLog(drawn.queryClient, upload(4, 5, "c.png"));
    });
    await waitFor(() =>
      expect(document.querySelector('[data-history-uploads="3"]')).not.toBeNull()
    );

    expect(pin.isConnected, "the run the newer upload joined was drawn anew").toBe(true);
    expect(pinOfOlderFile()).toBe(pin);
    expect(document.activeElement, "the focus on the run's Pin fell to the page").toBe(pin);
  } finally {
    unmount?.();
    restoreReads();
  }
});

// The history draws its first 20 entries, and under the last one drawn a control that draws the
// next 20 and says how many entries are left. A keyboard press moves the focus to the first
// entry it added, as Show more under every other list of Details does, whether the control
// stays or goes; once every entry is drawn the control goes. The entries are all read on the
// log's first page, so no press reads the log again.
test("the history draws 20 entries at a time, and Show more says how many are left, moves the focus to the first entry it drew, and goes at the end", async () => {
  const restoreReads = stubHistoryReads(
    Array.from({ length: 45 }, (_, index) => claimAgo(45 - index, index + 1))
  );
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    expect(drawnEntries(), "the history drew every entry at once").toHaveLength(PAGE_SIZE);
    expect(more.textContent).toBe(showMoreName(25));
    expect(more.querySelector("[data-history-left]")?.textContent).toBe("25");

    act(() => more.focus());
    await userEvent.keyboard("{Enter}");
    expect(drawnEntries()).toHaveLength(2 * PAGE_SIZE);
    expect(more.querySelector("[data-history-left]")?.textContent).toBe("5");
    expect(
      drawnEntries()[PAGE_SIZE]?.contains(document.activeElement),
      "Show more did not move the focus to the first entry it drew"
    ).toBe(true);

    act(() => more.focus());
    await userEvent.keyboard("{Enter}");
    expect(drawnEntries()).toHaveLength(45);
    expect(more.isConnected, "Show more stayed once every entry was drawn").toBe(false);
    expect(
      drawnEntries()[2 * PAGE_SIZE]?.contains(document.activeElement),
      "Show more went with the last entries and did not move the focus to the first it drew"
    ).toBe(true);
    expect(
      api.getIssueEvents,
      "a press that drew entries already read read the log"
    ).toHaveBeenCalledTimes(1);
  } finally {
    unmount?.();
    restoreReads();
  }
});

// While an older page of the log is unread, the count of entries left among those read says
// "+" after the number, as it cannot say how many the unread pages hold. A press that would leave
// no entry read and not drawn reads the older page first, reading busy meanwhile, and then draws
// the next 20, so the control goes on saying how many are left. A press that draws entries
// already read reads nothing. Found by a row on copies of real issues: a press drew the 5 entries
// left and Show more then said no number.
test("Show more says + while an older page is unread, and a press that would leave 20 or fewer entries read reads that page before it draws the next 20", async () => {
  const firstPage = pageOf(25, 400);
  const olderPage = Array.from({ length: 30 }, (_, index) => claimAgo(100 - index, 300 + index));
  // The older page is held out until the case lets it land.
  const older = Promise.withResolvers<DispatchEvent[]>();
  const getIssueEvents = spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) =>
    options?.before === undefined ? [...firstPage] : older.promise
  );
  const reads = [
    getIssueEvents,
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  const olderReads = () =>
    getIssueEvents.mock.calls.filter(([, options]) => options?.before !== undefined).length;
  const left = () => document.querySelector("[data-history-left]")?.textContent;
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    expect(drawnEntries()).toHaveLength(PAGE_SIZE);
    expect(left(), "the count of entries left says no + with an older page unread").toBe("5+");
    expect(olderReads(), "a history holding more than 20 entries read an older page").toBe(0);

    await userEvent.click(more);
    expect(
      drawnEntries(),
      "the press drew the entries it held before it read the older page"
    ).toHaveLength(PAGE_SIZE);
    await waitFor(() => expect(olderReads()).toBe(1));
    expect(more.getAttribute("aria-busy")).toBe("true");
    expect(more.textContent).toBe(ANSWER_LINES.loading_older);

    await act(async () => older.resolve([...olderPage]));
    await waitFor(() =>
      expect(
        drawnEntries(),
        "the press did not draw the next 20 once the older page landed"
      ).toHaveLength(2 * PAGE_SIZE)
    );
    expect(
      drawnEntries()[25]?.getAttribute("data-event-seq"),
      "the first entry drawn from the page read is not its newest"
    ).toBe("100");
    // The page read was the log's last, so the count says no +.
    expect(left()).toBe("15");
    expect(olderReads()).toBe(1);

    await userEvent.click(more);
    expect(drawnEntries()).toHaveLength(55);
    expect(more.isConnected, "Show more stayed with no entry and no page left").toBe(false);
    expect(olderReads()).toBe(1);
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

/** The history of `KEY` drawn over a log of `pages`, newest first, each read once in turn: the
 *  first with no `before`, each next one with `before` the last sequence of the page above it.
 *  Returns the reads of older pages, what each page's read found drawn as it was sent (its
 *  entries and whether the loading line stood), and what puts everything back. */
function drawOverPages(pages: readonly (readonly DispatchEvent[])[]): {
  atRead: { entries: number; loading: boolean }[];
  olderReads: () => number;
  restore: () => void;
} {
  const atRead: { entries: number; loading: boolean }[] = [];
  const getIssueEvents = spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) => {
    const before = options?.before;
    const at =
      before === undefined ? 0 : pages.findIndex((page) => page.at(-1)?.seq === before) + 1;
    if (before !== undefined) {
      atRead.push({
        entries: drawnEntries().length,
        loading: screen.queryByText(ANSWER_LINES.loading_history) !== null,
      });
    }
    return [...(pages[at] ?? [])];
  });
  const reads = [
    getIssueEvents,
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  const { unmount } = drawHistory(undefined);
  return {
    atRead,
    olderReads: () =>
      getIssueEvents.mock.calls.filter(([, options]) => options?.before !== undefined).length,
    restore: () => {
      unmount();
      for (const read of reads) read.mockRestore();
    },
  };
}

// The log's first page is its newest 200 events, and on a real issue most of them are comments,
// which the history leaves out. Found by a row on copies of real issues: the first page held 0,
// 12 or 17 entries of the history, which drew those and a Show more that said no number, the
// older pages unread. The history reads older pages before it draws, one at a time, until it
// holds more entries than it draws at once or the log has ended, and draws the loading line
// meanwhile: it draws 20 entries and says how many are left.
test("the history reads older pages before it draws until it holds more than 20 entries, and draws the loading line meanwhile", async () => {
  const olderPage = Array.from({ length: 30 }, (_, index) => claimAgo(300 - index, 300 + index));
  const { atRead, olderReads, restore } = drawOverPages([pageOf(7, 600), olderPage]);

  try {
    await waitFor(() =>
      expect(
        drawnEntries(),
        "the history drew the entries of the log's first page alone"
      ).toHaveLength(PAGE_SIZE)
    );
    // The older page was the log's last, so the count says no +.
    expect(screen.getByRole("button", { name: SHOW_MORE }).textContent).toBe(showMoreName(17));
    expect(olderReads(), "the history read the older page more than once").toBe(1);
    expect(
      atRead,
      "the history drew entries, or no loading line, while it read the older page"
    ).toEqual([{ entries: 0, loading: true }]);
  } finally {
    restore();
  }
});

// A history holding exactly 20 entries with an older page unread would draw them all and leave
// none read and not drawn, so Show more would say no number: it reads on until it holds more.
test("a history whose first page holds exactly 20 entries reads the older page before it draws, and Show more says how many are left", async () => {
  const olderPage = Array.from({ length: 30 }, (_, index) => claimAgo(300 - index, 300 + index));
  const { olderReads, restore } = drawOverPages([pageOf(PAGE_SIZE, 600), olderPage]);

  try {
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    expect(
      more.textContent,
      "the history holding exactly 20 entries drew them and said no count"
    ).toBe(showMoreName(30));
    expect(drawnEntries()).toHaveLength(PAGE_SIZE);
    expect(olderReads()).toBe(1);
  } finally {
    restore();
  }
});

// The first entries are drawn from the log's first page as soon as it holds more than 20, before
// the comments, messages and asks the threads are drawn from have answered. A press of Show more
// made then, with 20 or fewer entries left to draw, reads the older page and keeps the entries on
// screen, the control reading busy, rather than taking them away for the loading line the first
// view draws: the reader's focus is on that control and the place under it.
test("a press of Show more made before the threads' reads answer keeps the entries drawn while it reads the older page", async () => {
  const firstPage = pageOf(PAGE_SIZE + 1, 400);
  const olderPage = Array.from({ length: 30 }, (_, index) => claimAgo(100 - index, 300 + index));
  const threads = Promise.withResolvers<never[]>();
  // The older page is held out until the case lets it land.
  const older = Promise.withResolvers<DispatchEvent[]>();
  const getIssueEvents = spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) =>
    options?.before === undefined ? [...firstPage] : older.promise
  );
  const reads = [
    getIssueEvents,
    spyOn(api, "listComments").mockImplementation(() => threads.promise),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    expect(drawnEntries()).toHaveLength(PAGE_SIZE);
    more.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() =>
      expect(getIssueEvents.mock.calls.some(([, options]) => options?.before !== undefined)).toBe(
        true
      )
    );
    expect(
      drawnEntries(),
      "the press took the entries away for the loading line while it read the older page"
    ).toHaveLength(PAGE_SIZE);
    expect(screen.queryByText(ANSWER_LINES.loading_history)).toBeNull();
    await act(async () => {
      threads.resolve([]);
      older.resolve([...olderPage]);
    });
    await waitFor(() =>
      expect(drawnEntries(), "the press drew no more entries once the page landed").toHaveLength(
        2 * PAGE_SIZE
      )
    );
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// Where the whole log holds fewer than 20 entries, the history reads every page, one at a time,
// and draws each entry with no control under them.
test("a history whose log ends with fewer than 20 entries reads each older page and draws every entry with no Show more", async () => {
  const lastPage = [claimAgo(10, 3000), claimAgo(9, 3001), claimAgo(8, 3002)];
  const { atRead, olderReads, restore } = drawOverPages([
    pageOf(5, 900),
    pageOf(4, 700, 300),
    lastPage,
  ]);

  try {
    await waitFor(() =>
      expect(drawnEntries(), "the history did not draw every entry of the log").toHaveLength(12)
    );
    expect(olderReads(), "the history did not read each older page once").toBe(2);
    expect(atRead).toEqual([
      { entries: 0, loading: true },
      { entries: 0, loading: true },
    ]);
    expect(
      screen.queryByRole("button", { name: SHOW_MORE }),
      "Show more is offered with the whole log drawn"
    ).toBeNull();
    expect(screen.queryByText(ANSWER_LINES.loading_history)).toBeNull();
  } finally {
    restore();
  }
});

// Details is reached by Tab, so a control of the history that goes away under the focus leaves
// the focus in the history rather than on the page, as Details' Unpin does (IssueMargin.test.tsx):
// Show more, gone once the log has ended, hands it to the first entry it drew, where Show more
// stood, here once the press has read the older page; the Retry of a failed read, gone once the
// read lands, to the history's heading. Neither move scrolls Details: the reader keeps their
// place. A refetch that fails over the pages held draws no line, so the Retry is a read's that
// failed holding no page.
test("a control of the history that goes away under the focus leaves the focus in the history", async () => {
  const firstPage = pageOf(PAGE_SIZE + 1, 400);
  const olderPage = [claimAgo(10, 300), claimAgo(9, 301)];
  let down = false;
  const reads = [
    spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) => {
      if (down) throw new Error("the log is down");
      return options?.before === undefined ? [...firstPage] : [...olderPage];
    }),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHistory(undefined);
    unmount = drawn.unmount;
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    const before = new Set(list.querySelectorAll("a[href], button"));
    const focus = spyOn(HTMLElement.prototype, "focus");
    more.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(more.isConnected).toBe(false));
    const active = document.activeElement;
    expect(
      active !== null && list.contains(active) && !before.has(active),
      "Show more went away with the log's end and the focus did not go to the first entry the press drew"
    ).toBe(true);
    expect(
      drawnEntries()[PAGE_SIZE]?.contains(active),
      "the focus is not on the first entry the press drew"
    ).toBe(true);
    expect(focus.mock.calls.at(-1), "handing the focus on scrolled Details").toEqual([
      { preventScroll: true },
    ]);
    focus.mockRestore();

    down = true;
    const entries = document.querySelectorAll("[data-event-seq]").length;
    await failLogRefetchThenFirstRead(drawn.queryClient, entries);
    (await screen.findByRole("button", { name: CONTROL_LABELS.retry })).focus();
    down = false;
    await userEvent.keyboard("{Enter}");
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: CONTROL_LABELS.retry })).toBeNull()
    );
    expect(
      document.activeElement,
      "the Retry of the failed read went away once it landed and the focus fell to the page"
    ).toBe(screen.getByRole("heading", { name: CONTROL_LABELS.history }));
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// The reader keeps their place in the history. A failed next page
// is said where Show more stood, and its Retry reads that page: it does Show more's work in
// Show more's place, at the foot of the list, so once the page lands the focus goes where Show
// more hands it - to the first entry it drew - and never to the history's heading at the
// top, from which the reader's next Tab scrolls Details back to the head of the list. Found by
// the tester on a running build of 3bd86200 with a copy of a large real issue: with 81 entries
// drawn, the next page answered 500 once, and its Retry pressed by keyboard put the focus on
// the heading "History" once the page landed.
test("the Retry of a failed next page hands the focus to the first entry it draws, as Show more does", async () => {
  const firstPage = pageOf(PAGE_SIZE + 1, 400);
  const olderPage = [claimAgo(10, 300), claimAgo(9, 301)];
  let down = true;
  const reads = [
    spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) => {
      if (options?.before === undefined) return [...firstPage];
      if (down) throw new Error("the next page is down");
      return [...olderPage];
    }),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    more.focus();
    await userEvent.keyboard("{Enter}");
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    const before = new Set(list.querySelectorAll("a[href], button"));
    retry.focus();
    down = false;
    await userEvent.keyboard("{Enter}");
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: CONTROL_LABELS.retry })).toBeNull()
    );
    await waitFor(() => expect(more.isConnected).toBe(false));
    const active = document.activeElement;
    expect(
      active !== null && list.contains(active) && !before.has(active),
      "the Retry of the next page went away once it landed and the focus did not go to the first entry the press drew"
    ).toBe(true);
    expect(
      drawnEntries()[PAGE_SIZE]?.contains(active),
      "the focus is not on the first entry the press drew"
    ).toBe(true);
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// A read of the next page that fails once the history holds the log keeps its line and its
// Retry drawn where Show more stood while that Retry reads again, and so does a first read of
// the log that failed, under the heading; a refetch that fails over the pages held draws no line.
// Neither Retry is disabled while it reads: Chromium would drop its focus to the page
// (`chromium-focus-fixup.ts`). A keyboard reader stays on the Retry while it reads, and when its
// read fails again.
test("a history Retry pressed by keyboard keeps the focus while it reads and when its read fails again", async () => {
  const firstPage = pageOf(PAGE_SIZE + 1, 400);
  let readLog = async (): Promise<DispatchEvent[]> => [...firstPage];
  let readOlder = async (): Promise<DispatchEvent[]> => {
    throw new Error("the next page is down");
  };
  const reads = [
    spyOn(api, "getIssueEvents").mockImplementation((_key, options) =>
      options?.before === undefined ? readLog() : readOlder()
    ),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  let unmount: (() => void) | undefined;
  let restoreFocus: (() => void) | undefined;

  try {
    const drawn = drawHistory(undefined);
    unmount = drawn.unmount;
    const fetchStatus = () => drawn.queryClient.getQueryState(["events", KEY])?.fetchStatus;
    (await screen.findByRole("button", { name: SHOW_MORE })).focus();
    await userEvent.keyboard("{Enter}");
    const older = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    restoreFocus = emulateChromiumFocusFixup();
    // The read each Retry sends is held out until the case fails it.
    const olderAgain = Promise.withResolvers<DispatchEvent[]>();
    readOlder = () => olderAgain.promise;
    older.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(fetchStatus()).toBe("fetching"));
    expect(
      document.activeElement,
      "the next page's Retry left the focus on the page while it read"
    ).toBe(older);
    olderAgain.reject(new Error("the next page is down again"));
    await waitFor(() => expect(fetchStatus()).toBe("idle"));
    expect(screen.getByRole("button", { name: CONTROL_LABELS.retry })).toBe(older);
    expect(
      document.activeElement,
      "the next page's Retry whose read failed again did not keep the focus"
    ).toBe(older);

    readLog = async () => {
      throw new Error("the log is down");
    };
    const entries = document.querySelectorAll("[data-event-seq]").length;
    await failLogRefetchThenFirstRead(drawn.queryClient, entries);
    const log = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    const logAgain = Promise.withResolvers<DispatchEvent[]>();
    readLog = () => logAgain.promise;
    log.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(fetchStatus()).toBe("fetching"));
    expect(document.activeElement, "the log's Retry left the focus on the page while it read").toBe(
      log
    );
    logAgain.reject(new Error("the log is down again"));
    await waitFor(() => expect(fetchStatus()).toBe("idle"));
    expect(screen.getByRole("button", { name: CONTROL_LABELS.retry })).toBe(log);
    expect(
      document.activeElement,
      "the log's Retry whose read failed again did not keep the focus"
    ).toBe(log);
  } finally {
    restoreFocus?.();
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// The log's Retry showed nothing while the read it sent was out, and a read that failed again
// was said on the same node in the same words, so a screen reader said nothing of it and the
// reader could not tell the press did anything. Retry reads busy while its read is out, enabled
// and holding the focus, and each failure is drawn afresh while Retry stays the same button. A
// refetch that fails over the pages held draws no line, so the Retry is a read's that failed
// holding no page.
test("the log's Retry reads busy while its read is out, and a read that fails again is said again", async () => {
  let readLog = async (): Promise<DispatchEvent[]> => [claim(3), claim(2), claim(1)];
  const reads = [
    spyOn(api, "getIssueEvents").mockImplementation(() => readLog()),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  let unmount: (() => void) | undefined;
  let restoreFocus: (() => void) | undefined;

  try {
    const drawn = drawHistory(undefined);
    unmount = drawn.unmount;
    await waitFor(() => expect(document.querySelectorAll("[data-event-seq]")).toHaveLength(3));
    readLog = async () => {
      throw new Error("the log is down");
    };
    await failLogRefetchThenFirstRead(
      drawn.queryClient,
      3,
      "a failed read of the log erased the history it held"
    );
    const failed = await screen.findByRole("alert");
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
    restoreFocus = emulateChromiumFocusFixup();
    // The read the Retry sends is held out until the case fails it.
    const again = Promise.withResolvers<DispatchEvent[]>();
    readLog = () => again.promise;

    act(() => retry.focus());
    await userEvent.keyboard("{Enter}");
    await waitFor(() =>
      expect(retry, "Retry does not read busy while the read it sent again is out").toHaveAttribute(
        "aria-busy",
        "true"
      )
    );
    expect(retry, "Retry turned disabled while the read it sent again is out").toBeEnabled();
    expect(document.activeElement, "Retry lost the focus once it read busy").toBe(retry);

    await act(async () => again.reject(new Error("the log is down")));
    await waitFor(() =>
      expect(retry, "Retry still reads busy after the read failed again").not.toHaveAttribute(
        "aria-busy"
      )
    );
    const failedAgain = screen.getByRole("alert");
    expect(failedAgain).toHaveTextContent(ANSWER_LINES.history_unreadable);
    expect(
      failedAgain,
      "the second failure was not drawn afresh, so a screen reader would not say it"
    ).not.toBe(failed);
    expect(
      screen.getByRole("button", { name: CONTROL_LABELS.retry }),
      "the line came back with another Retry"
    ).toBe(retry);
    expect(document.activeElement, "Retry lost the focus when the read failed again").toBe(retry);
  } finally {
    restoreFocus?.();
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// A first read of the log that failed holds no page, and a fetch of such a read clears its
// failure as it starts, so the press of its Retry took the line, and the Retry with it, away
// while the read it sent was out, and the history said it was loading in its place: the focus of
// a reader who pressed Retry by keyboard went with the Retry. The line stays, its Retry busy and
// holding the focus, through a read that fails again, and once a read answers the focus goes to
// the history's heading.
test("a first read of the log that failed keeps its line and its Retry, with the focus, while Retry reads again", async () => {
  let readLog = async (): Promise<DispatchEvent[]> => {
    throw new Error("the log is down");
  };
  const reads = [
    spyOn(api, "getIssueEvents").mockImplementation(() => readLog()),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  let unmount: (() => void) | undefined;
  let restoreFocus: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    const failed = await screen.findByRole("alert");
    expect(failed).toHaveTextContent(ANSWER_LINES.history_unreadable);
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
    restoreFocus = emulateChromiumFocusFixup();
    // The read the Retry sends is held out until the case fails it.
    const again = Promise.withResolvers<DispatchEvent[]>();
    readLog = () => again.promise;

    act(() => retry.focus());
    await userEvent.keyboard("{Enter}");
    await waitFor(() => {
      expect(
        retry,
        "the line and its Retry went while the read Retry sent again is out"
      ).toBeInTheDocument();
      expect(retry, "Retry does not read busy while the read it sent again is out").toHaveAttribute(
        "aria-busy",
        "true"
      );
    });
    expect(retry, "Retry turned disabled while the read it sent again is out").toBeEnabled();
    expect(document.activeElement, "Retry lost the focus while the read it sent again is out").toBe(
      retry
    );
    expect(
      screen.queryByText(ANSWER_LINES.loading_history),
      "the history says it is loading beside the failure its Retry reads again"
    ).toBeNull();

    await act(async () => again.reject(new Error("the log is down")));
    await waitFor(() =>
      expect(retry, "Retry still reads busy after the read failed again").not.toHaveAttribute(
        "aria-busy"
      )
    );
    const failedAgain = screen.getByRole("alert");
    expect(failedAgain).toHaveTextContent(ANSWER_LINES.history_unreadable);
    expect(
      failedAgain,
      "the second failure was not drawn afresh, so a screen reader would not say it"
    ).not.toBe(failed);
    expect(
      screen.getByRole("button", { name: CONTROL_LABELS.retry }),
      "the line came back with another Retry"
    ).toBe(retry);
    expect(document.activeElement, "Retry lost the focus when the read failed again").toBe(retry);

    readLog = async () => [claim(3), claim(2), claim(1)];
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(document.querySelectorAll("[data-event-seq]")).toHaveLength(3));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(
      document.activeElement,
      "the log answered, and the focus fell to the page rather than to the history's heading"
    ).toBe(screen.getByRole("heading", { name: CONTROL_LABELS.history }));
  } finally {
    restoreFocus?.();
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// Show more is not disabled while its page is read either: Chromium would drop its focus to
// the page (`chromium-focus-fixup.ts`). A keyboard reader stays on it while the page is out, and
// once the page lands with older pages still left, Show more draws the next 20 entries, the one
// left of the first page and the page's newest, and hands the focus on to the first of them, as
// it does when the log's end takes it away; it says how many entries read are left, with "+" as
// the log holds pages older than it.
test("Show more pressed by keyboard keeps the focus while its page is read, then hands it to the first entry it draws", async () => {
  const firstPage = pageOf(PAGE_SIZE + 1, 600);
  const secondPage = Array.from({ length: 200 }, (_, index) => claimAgo(400 - index, index + 201));
  // The next page is held out until the case lets it land.
  const older = Promise.withResolvers<DispatchEvent[]>();
  const reads = [
    spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) =>
      options?.before === undefined ? [...firstPage] : older.promise
    ),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  let unmount: (() => void) | undefined;
  let restoreFocus: (() => void) | undefined;

  try {
    const drawn = drawHistory(undefined);
    unmount = drawn.unmount;
    const fetchStatus = () => drawn.queryClient.getQueryState(["events", KEY])?.fetchStatus;
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    const before = new Set(list.querySelectorAll("a[href], button"));
    restoreFocus = emulateChromiumFocusFixup();
    more.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(fetchStatus()).toBe("fetching"));
    expect(
      document.activeElement,
      "Show more left the focus on the page while its page was read"
    ).toBe(more);
    expect(more.getAttribute("aria-busy")).toBe("true");

    older.resolve([...secondPage]);
    await waitFor(() => expect(fetchStatus()).toBe("idle"));
    await waitFor(() =>
      expect(more.querySelector("[data-history-left]")?.textContent).toBe("181+")
    );
    expect(drawnEntries()).toHaveLength(2 * PAGE_SIZE);
    expect(
      screen.getByRole("button", { name: SHOW_MORE }),
      "the log has older pages after the second, so Show more stays"
    ).toBe(more);
    const active = document.activeElement;
    expect(
      active !== null && list.contains(active) && !before.has(active),
      "Show more kept the focus once its page landed, rather than handing it to a control the press drew"
    ).toBe(true);
  } finally {
    restoreFocus?.();
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// A second press of a history Retry or of Show more made before React draws the read the first
// one sent finds the render's `isFetching` still false, so it read again: a read of the next page
// of a log that holds pages ends the request that is out and sends another. A press while the
// read is out joins it, and keeps the hold the first press took, so the focus goes where one
// press hands it. A refetch that fails over the pages held draws no line, so the log's Retry is a
// read's that failed holding no page.
test("two presses of the log's Retry before a render send one read, and the focus still goes to the heading", async () => {
  let readLog = async (): Promise<DispatchEvent[]> => [claim(3), claim(2), claim(1)];
  const getIssueEvents = spyOn(api, "getIssueEvents").mockImplementation(() => readLog());
  const reads = [
    getIssueEvents,
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHistory(undefined);
    unmount = drawn.unmount;
    await waitFor(() => expect(document.querySelectorAll("[data-event-seq]")).toHaveLength(3));
    readLog = async () => {
      throw new Error("the log is down");
    };
    await failLogRefetchThenFirstRead(drawn.queryClient, 3);
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    // Every read sent from here is held out until the case lets it land.
    const again = Promise.withResolvers<DispatchEvent[]>();
    readLog = () => again.promise;
    const sent = getIssueEvents.mock.calls.length;
    act(() => retry.focus());

    // Both presses in one task inside one `act`, so React draws nothing between them.
    act(() => {
      retry.click();
      retry.click();
    });
    expect(
      getIssueEvents,
      "the second press ended the read the first one sent and sent another"
    ).toHaveBeenCalledTimes(sent + 1);

    await act(async () => again.resolve([claim(4), claim(3), claim(2), claim(1)]));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: CONTROL_LABELS.retry })).toBeNull()
    );
    expect(getIssueEvents).toHaveBeenCalledTimes(sent + 1);
    expect(
      document.activeElement,
      "the Retry went once its read landed, and the focus did not go to the history's heading"
    ).toBe(screen.getByRole("heading", { name: CONTROL_LABELS.history }));
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

test("two presses of Show more before a render read its page once, and the focus still goes to the first entry it drew", async () => {
  const firstPage = pageOf(PAGE_SIZE + 1, 600);
  const secondPage = Array.from({ length: 200 }, (_, index) => claimAgo(400 - index, index + 201));
  // The next page is held out until the case lets it land.
  const older = Promise.withResolvers<DispatchEvent[]>();
  const getIssueEvents = spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) =>
    options?.before === undefined ? [...firstPage] : older.promise
  );
  const reads = [
    getIssueEvents,
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  const olderReads = () =>
    getIssueEvents.mock.calls.filter(([, options]) => options?.before !== undefined).length;
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHistory(undefined);
    unmount = drawn.unmount;
    const fetchStatus = () => drawn.queryClient.getQueryState(["events", KEY])?.fetchStatus;
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    const before = new Set(list.querySelectorAll("a[href], button"));
    act(() => more.focus());

    // Both presses in one task inside one `act`, so React draws nothing between them.
    act(() => {
      more.click();
      more.click();
    });
    expect(
      olderReads(),
      "the second press ended the read of the page the first one sent and sent another"
    ).toBe(1);

    await act(async () => older.resolve([...secondPage]));
    await waitFor(() => expect(fetchStatus()).toBe("idle"));
    await waitFor(() =>
      expect(more.querySelector("[data-history-left]")?.textContent).toBe("181+")
    );
    expect(olderReads()).toBe(1);
    const active = document.activeElement;
    expect(
      active !== null && list.contains(active) && !before.has(active),
      "Show more pressed twice kept the focus once its page landed, rather than handing it to a control the press drew"
    ).toBe(true);
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

test("two presses of the next page's Retry before a render read that page once, and the focus still goes to the first entry it drew", async () => {
  const firstPage = pageOf(PAGE_SIZE + 1, 400);
  const olderPage = [claimAgo(10, 300), claimAgo(9, 301)];
  let readOlder = async (): Promise<DispatchEvent[]> => {
    throw new Error("the next page is down");
  };
  const getIssueEvents = spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) =>
    options?.before === undefined ? [...firstPage] : readOlder()
  );
  const reads = [
    getIssueEvents,
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  const olderReads = () =>
    getIssueEvents.mock.calls.filter(([, options]) => options?.before !== undefined).length;
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    act(() => more.focus());
    await userEvent.keyboard("{Enter}");
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    const before = new Set(list.querySelectorAll("a[href], button"));
    // Every read of the next page sent from here is held out until the case lets it land.
    const again = Promise.withResolvers<DispatchEvent[]>();
    readOlder = () => again.promise;
    const sent = olderReads();
    act(() => retry.focus());

    // Both presses in one task inside one `act`, so React draws nothing between them.
    act(() => {
      retry.click();
      retry.click();
    });
    expect(
      olderReads(),
      "the second press ended the read of the page the first one sent and sent another"
    ).toBe(sent + 1);

    await act(async () => again.resolve([...olderPage]));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: CONTROL_LABELS.retry })).toBeNull()
    );
    await waitFor(() => expect(more.isConnected).toBe(false));
    expect(olderReads()).toBe(sent + 1);
    const active = document.activeElement;
    expect(
      active !== null && list.contains(active) && !before.has(active),
      "the next page's Retry pressed twice went once its page landed, and the focus did not go to the first entry the press drew"
    ).toBe(true);
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// A live event refetches the log, often on a busy issue. A press of Show more made while that
// refetch was out joined it, since `cancelRefetch: false` joins whatever read of the log is out,
// and read no page. Only the reader's own read of the next page takes a press made meanwhile, so
// a press while a refetch is out reads the next page.
test("Show more pressed while a refetch of the log is out reads the next page", async () => {
  const firstPage = pageOf(PAGE_SIZE + 1, 600);
  const secondPage = Array.from({ length: 200 }, (_, index) => claimAgo(400 - index, index + 201));
  let readFirst = async (): Promise<DispatchEvent[]> => [...firstPage];
  const getIssueEvents = spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) =>
    options?.before === undefined ? readFirst() : [...secondPage]
  );
  const reads = [
    getIssueEvents,
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  const olderReads = () =>
    getIssueEvents.mock.calls.filter(([, options]) => options?.before !== undefined).length;
  // The refetch is held out until the case ends.
  const refetched = Promise.withResolvers<DispatchEvent[]>();
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHistory(undefined);
    unmount = drawn.unmount;
    const fetchStatus = () => drawn.queryClient.getQueryState(["events", KEY])?.fetchStatus;
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    readFirst = () => refetched.promise;
    act(() => {
      void drawn.queryClient.invalidateQueries({ queryKey: ["events", KEY] });
    });
    await waitFor(() => expect(fetchStatus()).toBe("fetching"));

    await userEvent.click(more);
    expect(olderReads(), "Show more pressed while a refetch of the log was out read no page").toBe(
      1
    );
    await waitFor(() =>
      expect(
        document.querySelector('[data-event-seq="400"]'),
        "the page Show more read did not land"
      ).not.toBeNull()
    );
  } finally {
    refetched.resolve([...firstPage]);
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// The last page holds only events the history leaves out, so it adds no entry and no control,
// and the log's end takes the control that read it away: the focus goes to the last control
// drawn before the press, which stood just above that control, rather than to the history's
// heading above the whole list, off screen for a reader at its foot. A press of Show more draws
// an entry, as it reads older pages until it can; the control that reads a page and can draw
// none is the Retry of a failed next page where every entry read is drawn, which stands in Show
// more's place then and hands the focus on as Show more does.
test("the next page's Retry whose last page adds no entry hands the focus to the control above where it stood", async () => {
  const firstPage = pageOf(PAGE_SIZE, 400);
  const lastPage = [outlineEventAgo(10, 300), outlineEventAgo(9, 301)];
  let down = true;
  const reads = [
    spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) => {
      if (options?.before === undefined) return [...firstPage];
      if (down) throw new Error("the next page is down");
      return [...lastPage];
    }),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  let unmount: (() => void) | undefined;
  let restoreFocus: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    // The history read the older page before it drew, and that read failed: it draws the 20
    // entries it holds, with the Retry in Show more's place.
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    expect(drawnEntries()).toHaveLength(PAGE_SIZE);
    expect(
      screen.queryByRole("button", { name: SHOW_MORE }),
      "Show more stands beside the next page's Retry with every entry read drawn, and says no number"
    ).toBeNull();
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    const before = [...list.querySelectorAll("a[href], button")];
    const above = before.at(-1) ?? null;
    expect(
      drawnEntries().at(-1)?.contains(above),
      "the last entry's Pin is not the last control above the Retry"
    ).toBe(true);
    restoreFocus = emulateChromiumFocusFixup();
    down = false;
    retry.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(retry.isConnected).toBe(false));
    expect(drawnEntries(), "the last page added an entry, so the row proves nothing").toHaveLength(
      PAGE_SIZE
    );
    expect(
      list.querySelectorAll("a[href], button"),
      "the last page added a control, so the row proves nothing"
    ).toHaveLength(before.length);
    expect(screen.queryByRole("button", { name: SHOW_MORE })).toBeNull();

    const active = document.activeElement;
    expect(
      active,
      `the log's end took the Retry away and the focus left the foot of the list (focus: ${active?.tagName} ${active?.textContent})`
    ).toBe(above);
  } finally {
    restoreFocus?.();
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

/** How tall the box the history scrolls in is: Details' view. */
const VIEW_HEIGHT = 600;

/** Lays out the history drawn in `box`, Details' own box, for the geometry `ViewportAnchor` and
 *  the landing read: `box` is Details' view, `VIEW_HEIGHT` tall at the top of the window, and
 *  every entry stacks from `LIST_TOP` in DOM order, `heightOf` tall, moved up by the box's
 *  scroll. Where the case gives `turnHeight`, a thread's turns stack inside its entry, each
 *  `turnHeight` tall, and anything inside a turn measures as the turn. Anything else inside an
 *  entry, a control among them, measures as the entry, and anything else as the whole list. The
 *  point the anchor probes at hits the entry drawn there. Returns what restores the measures. */
function installDetailsLayout(
  box: HTMLElement,
  heightOf: (row: Element) => number,
  turnHeight?: (turn: Element) => number
): () => void {
  const rows = () => [...box.querySelectorAll<HTMLElement>("[data-event-seq]")];
  const rectOf = (top: number, height: number) =>
    ({
      bottom: top + height,
      height,
      left: 0,
      right: 100,
      toJSON: () => ({}),
      top,
      width: 100,
      x: 0,
      y: top,
    }) as DOMRect;
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this === box) return rectOf(0, VIEW_HEIGHT);
    const row = this.closest("[data-event-seq]");
    const turn = this.closest("[data-history-item]");
    const start = LIST_TOP - box.scrollTop;
    let top = start;
    for (const one of rows()) {
      if (one === row) {
        if (turn === null || turnHeight === undefined) return rectOf(top, heightOf(one));
        for (const other of one.querySelectorAll("[data-history-item]")) {
          if (other === turn) return rectOf(top, turnHeight(other));
          top += turnHeight(other);
        }
      }
      top += heightOf(one);
    }
    return rectOf(start, top - start);
  });
  const point = spyOn(document, "elementFromPoint").mockImplementation((_x, y) => {
    let top = LIST_TOP - box.scrollTop;
    for (const one of rows()) {
      if (y >= top && y < top + heightOf(one)) return one;
      top += heightOf(one);
    }
    return null;
  });
  return () => {
    rect.mockRestore();
    point.mockRestore();
  };
}

// Show more hands the focus on with no scroll, and `ViewportAnchor` took the reader's place
// only from what the reader did - the pointer over the rows, or a scroll they made - so it held
// no entry then: tall entries just above Show more that change height once the page has landed,
// by commits of their own - a handoff whose fields are read, words whose pictures load - took the
// control the focus was handed to up with them out of Details' view. The anchor records the
// reader's place at each focus that moves into it, so it holds the entry the hand-on put the
// focus in and keeps it in place as the entries above it change height.
test("the entry Show more hands the focus to keeps its place in Details' view as the entries above it change height", async () => {
  const firstPage = pageOf(PAGE_SIZE + 1, 400);
  const olderPage = [claimAgo(10, 300), claimAgo(9, 301)];
  const reads = [
    spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) =>
      options?.before === undefined ? [...firstPage] : [...olderPage]
    ),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  // Details' own box, which the history scrolls in, at the top of the window.
  const box = document.body.appendChild(document.createElement("div"));
  box.style.overflowY = "auto";
  // The two entries drawn last, just above Show more, are a thousand pixels tall, until they
  // change height.
  const heights = new Map([
    ["event:382", 1000],
    ["event:381", 1000],
  ]);
  const rows = () => [...box.querySelectorAll<HTMLElement>("[data-event-seq]")];
  const heightOf = (row: Element) =>
    heights.get(row.getAttribute("data-history-entry") ?? "") ?? ROW_HEIGHT;
  const restoreLayout = installDetailsLayout(box, heightOf);
  // The anchor hears a change of height outside its commits from a resize observer of its root,
  // which happy-dom never calls back: the case calls it back once the entries change height.
  const sizes = installResizeObserver();
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined, box).unmount;
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    await waitFor(() => expect(rows()).toHaveLength(PAGE_SIZE));
    // Details scrolled down to Show more: the first entry the press draws, the first page's
    // last, lands in the middle of the view once the older page is read.
    box.scrollTop = LIST_TOP + (PAGE_SIZE - 2) * ROW_HEIGHT + 2 * 1000 - VIEW_HEIGHT / 2;
    more.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(more.isConnected).toBe(false));
    const entry = document.activeElement?.closest<HTMLElement>("[data-history-entry]");
    if (entry === null || entry === undefined) {
      throw new Error("Show more handed the focus to no entry of the history");
    }
    expect(
      entry.getAttribute("data-history-entry"),
      "Show more handed the focus to an entry other than the first the press drew"
    ).toBe("event:380");
    const handed = entry.getBoundingClientRect().top;
    expect(handed, "the entry Show more handed the focus to is not in Details' view").toBe(
      VIEW_HEIGHT / 2
    );

    // Each tall entry changes height by a commit of its own, which the anchor sees only as its
    // root resized.
    heights.set("event:382", 120);
    heights.set("event:381", 120);
    sizes.resize();

    expect(
      entry.getBoundingClientRect().top,
      "the entries above the focus changed height and moved the entry Show more handed it to out of Details' view"
    ).toBe(handed);
  } finally {
    unmount?.();
    sizes.restore();
    restoreLayout();
    box.remove();
    for (const read of reads) read.mockRestore();
  }
});

// Show more pressed with the focus on it: the focus stays on Show more, outside the entries,
// while the page is read, so no act of the reader's names a place in the history. Here the commit
// the page lands in also changes the height of the tall entry just above the list's last entry,
// which takes every entry under it, the page's new ones with them, up by the change. The anchor
// held the entry nearest the middle of Details' view, the tall entry itself, whose top the change
// leaves where it was: the page's entries went up out of the view, and the hand-on gave the focus
// to one above it (seen at -639 px in the browser, where a long turn was cut). The history keeps
// the place of the entry next to Show more from the press until the page's read has settled.
test("Show more keeps the place of the entry above it from the press, as its page lands with a change of height of the entry above that entry", async () => {
  const firstPage = pageOf(PAGE_SIZE + 1, 400);
  const olderPage = [claimAgo(10, 300), claimAgo(9, 301)];
  const reads = [
    spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) =>
      options?.before === undefined ? [...firstPage] : [...olderPage]
    ),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  // Details' own box, which the history scrolls in, at the top of the window.
  const box = document.body.appendChild(document.createElement("div"));
  box.style.overflowY = "auto";
  const rows = () => [...box.querySelectorAll<HTMLElement>("[data-event-seq]")];
  // The entry above the last one drawn is a thousand pixels tall, which the commit that draws the
  // older page's first entry changes to 120 px.
  const heightOf = (row: Element) => {
    if (row.getAttribute("data-history-entry") !== "event:382") return ROW_HEIGHT;
    return box.querySelector('[data-history-entry="event:10"]') === null ? 1000 : 120;
  };
  const restoreLayout = installDetailsLayout(box, heightOf);
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined, box).unmount;
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    await waitFor(() => expect(rows()).toHaveLength(PAGE_SIZE));
    const last = box.querySelector<HTMLElement>('[data-history-entry="event:381"]');
    if (last === null) throw new Error("the last entry drawn is not drawn");
    // Details scrolled down to Show more: the last entry drawn stands 400 px down the view,
    // under the tall entry, which holds the middle of the view.
    box.scrollTop = LIST_TOP + (PAGE_SIZE - 2) * ROW_HEIGHT + 1000 - 400;
    const stood = last.getBoundingClientRect().top;
    expect(stood).toBe(400);
    more.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(more.isConnected).toBe(false));

    expect(
      last.getBoundingClientRect().top,
      "the page landed with a change of height of the entry above the entry next to Show more, and that entry moved in Details' view"
    ).toBe(stood);
    const focused = document.activeElement;
    const entry = focused?.closest<HTMLElement>("[data-history-entry]");
    if (focused === null || entry === null || entry === undefined) {
      throw new Error("Show more handed the focus to no entry of the history");
    }
    expect(
      entry.getAttribute("data-history-entry"),
      "Show more handed the focus to an entry other than the first the press drew"
    ).toBe("event:380");
    const at = focused.getBoundingClientRect();
    expect(
      at.top >= 0 && at.bottom <= VIEW_HEIGHT,
      `Show more handed the focus to a control out of Details' view (${at.top} px, the view 0 to ${VIEW_HEIGHT})`
    ).toBe(true);
    expect(
      at.top >= last.getBoundingClientRect().bottom,
      "Show more handed the focus to a control above the entry next to it"
    ).toBe(true);
  } finally {
    unmount?.();
    restoreLayout();
    box.remove();
    for (const read of reads) read.mockRestore();
  }
});

/** The history of 45 claims (`claimAgo`), every one on the log's first page, drawn in a box of
 *  Details' own laid out by `heightOf` (`installDetailsLayout`), and scrolled to `scrollTop`:
 *  Show more under its first 20 draws the next 20 with no read. Returns the box, Show more, and
 *  what puts everything back. */
async function drawFortyFiveIn(
  heightOf: (row: Element) => number,
  scrollTop: number
): Promise<{ box: HTMLElement; more: HTMLElement; restore: () => void }> {
  const restoreReads = stubHistoryReads(
    Array.from({ length: 45 }, (_, index) => claimAgo(45 - index, index + 1))
  );
  // Details' own box, which the history scrolls in, at the top of the window.
  const box = document.body.appendChild(document.createElement("div"));
  box.style.overflowY = "auto";
  const restoreLayout = installDetailsLayout(box, heightOf);
  const { unmount } = drawHistory(undefined, box);
  const restore = () => {
    unmount();
    restoreLayout();
    box.remove();
    restoreReads();
  };
  try {
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    expect(drawnEntries()).toHaveLength(PAGE_SIZE);
    box.scrollTop = scrollTop;
    return { box, more, restore };
  } catch (error) {
    restore();
    throw error;
  }
}

/** The entry of `drawFortyFiveIn` drawn last before a press, `TALL_HEIGHT` px tall in the cases
 *  below, so the first entry a press draws stands wholly below Details' view. */
const TALL_ENTRY = "event:26";

/** How tall `TALL_ENTRY` is drawn: it fills Details' view to its foot. */
const TALL_HEIGHT = 560;

// The last press of Show more on a real issue drew its entries under a new day's line, and the
// first of them stood below Details' view. The press moved the focus to that entry's control with
// no scroll, so the focus stood where a keyboard reader could not see it. The press brings the
// control it focuses into Details' view by the least scroll, by its foot from below, writing
// Details' box alone: the page under it does not scroll, and no element is scrolled into view.
// Here the 20th entry is 560 px tall and fills Details' view to its foot, so the first entry the
// press draws stands wholly below the view.
test("a keyboard press of Show more whose first entry stands below Details' view moves the focus to that entry's control, wholly in Details' view, and scrolls no page", async () => {
  const { box, more, restore } = await drawFortyFiveIn(
    (row) => (row.getAttribute("data-history-entry") === TALL_ENTRY ? TALL_HEIGHT : ROW_HEIGHT),
    LIST_TOP + (PAGE_SIZE - 1) * ROW_HEIGHT - 40
  );
  const intoView = spyOn(Element.prototype, "scrollIntoView");
  const focus = spyOn(HTMLElement.prototype, "focus");

  try {
    const first = () => drawnEntries()[PAGE_SIZE];
    act(() => more.focus());
    await userEvent.keyboard("{Enter}");
    expect(drawnEntries()).toHaveLength(2 * PAGE_SIZE);
    const focused = document.activeElement;
    expect(
      focused !== null && first()?.contains(focused) === true,
      "Show more did not move the focus to the first entry it drew"
    ).toBe(true);
    expect(focused, "the focus is not on the first entry's control").toBe(
      first()?.querySelector("a[href], button:not(:disabled)") ?? null
    );
    const at = focused?.getBoundingClientRect();
    expect(
      at !== undefined && at.top >= 0 && at.bottom <= VIEW_HEIGHT,
      `Show more moved the focus to a control out of Details' view (${at?.top} to ${at?.bottom} px, the view 0 to ${VIEW_HEIGHT})`
    ).toBe(true);
    expect(at?.bottom, "Details scrolled more than the least that shows the control").toBe(
      VIEW_HEIGHT
    );
    expect(window.scrollY, "the page under Details scrolled").toBe(0);
    expect(
      intoView,
      "an element was scrolled into view, which scrolls the page too"
    ).not.toHaveBeenCalled();
    expect(focus.mock.calls.at(-1), "the focus moved with a scroll of its own").toEqual([
      { preventScroll: true },
    ]);
    expect(box.scrollTop).toBe(LIST_TOP + (PAGE_SIZE - 1) * ROW_HEIGHT);
  } finally {
    intoView.mockRestore();
    focus.mockRestore();
    restore();
  }
});

// The first entry a press of Show more draws stands in Details' view: the press moves the focus
// to its control and moves Details by nothing.
test("a keyboard press of Show more whose first entry stands in Details' view moves Details by nothing", async () => {
  const scrollTop = LIST_TOP + (PAGE_SIZE - 1) * ROW_HEIGHT - 300;
  const { box, more, restore } = await drawFortyFiveIn(() => ROW_HEIGHT, scrollTop);

  try {
    act(() => more.focus());
    await userEvent.keyboard("{Enter}");
    expect(drawnEntries()).toHaveLength(2 * PAGE_SIZE);
    const focused = document.activeElement;
    expect(
      focused !== null && drawnEntries()[PAGE_SIZE]?.contains(focused) === true,
      "Show more did not move the focus to the first entry it drew"
    ).toBe(true);
    expect(focused?.getBoundingClientRect().top).toBe(340);
    expect(box.scrollTop, "a focus already in Details' view moved Details").toBe(scrollTop);
  } finally {
    restore();
  }
});

/** A keyboard press of Show more over the 45 claims of `drawFortyFiveIn`, made as the case
 *  above whose first entry stands below Details' view makes it: the press hands the focus to that
 *  entry's control and brings it into Details' view by its foot. The resize observers are
 *  stubbed, as happy-dom never calls them back, and `resize` sets how tall an entry is, a control
 *  in it measuring as the entry, and calls them back as the frame after a change does. Returns
 *  the box, the focused control and its entry's id, `resize`, and what puts everything back. */
async function pressedToFoot(): Promise<{
  box: HTMLElement;
  entry: string;
  focused: HTMLElement;
  resize: (entry: string, height: number) => void;
  restore: () => void;
}> {
  const sizes = installResizeObserver();
  const heights = new Map([[TALL_ENTRY, TALL_HEIGHT]]);
  let undraw: (() => void) | undefined;
  const restore = () => {
    undraw?.();
    sizes.restore();
  };
  try {
    const drawn = await drawFortyFiveIn(
      (row) => heights.get(row.getAttribute("data-history-entry") ?? "") ?? ROW_HEIGHT,
      LIST_TOP + (PAGE_SIZE - 1) * ROW_HEIGHT - 40
    );
    undraw = drawn.restore;
    const { box, more } = drawn;
    // The box holds far more than its view, so no scroll leaves it at its end, which happy-dom,
    // laying nothing out, would read as the box pulled back to an end it shrank to.
    Object.defineProperties(box, {
      clientHeight: { configurable: true, value: VIEW_HEIGHT },
      scrollHeight: { configurable: true, value: 100_000 },
    });
    act(() => more.focus());
    await userEvent.keyboard("{Enter}");
    const focused = document.activeElement;
    const entry = drawnEntries()[PAGE_SIZE];
    if (!(focused instanceof HTMLElement) || entry?.contains(focused) !== true) {
      throw new Error("Show more did not move the focus to the first entry it drew");
    }
    expect(
      focused.getBoundingClientRect().bottom,
      "the press did not bring the focused control into Details' view by its foot, so the case proves nothing"
    ).toBe(VIEW_HEIGHT);
    return {
      box,
      entry: entry.getAttribute("data-history-entry") ?? "",
      focused,
      resize: (id, height) => {
        heights.set(id, height);
        sizes.resize();
      },
      restore,
    };
  } catch (error) {
    restore();
    throw error;
  }
}

// Show more hands the focus to the first entry it drew, brought into Details' view by the least
// scroll, and that entry's control can grow after the hand-on: a reference in the entry's line is
// drawn by its address until the names of the references are read, then by its target's name,
// which can wrap over several lines. Found by the tester on a running build with a copy of a
// large real issue at 1440 px: about 120 ms after a press, the focused link grew from 15 px to
// 51 px, and its foot stood 35 px below Details' view. The hand-on keeps the control wholly in
// that view through its own growth, by the least scroll, which the history's anchor makes
// (`keepFocusedInView`), and the anchor then keeps the control where that scroll left it.
test("a keyboard press of Show more keeps the control it focused wholly in Details' view as the control grows below the view's foot and holds it there as an entry above it shrinks", async () => {
  const { box, entry, focused, resize, restore } = await pressedToFoot();

  try {
    const before = box.scrollTop;
    resize(entry, ROW_HEIGHT + 36);
    expect(
      box.scrollTop - before,
      "the focused control grew 36 px below Details' view, and Details did not follow it"
    ).toBe(36);
    const at = focused.getBoundingClientRect();
    expect(
      at.top >= 0 && at.bottom <= VIEW_HEIGHT,
      `the focused control stands at ${at.top} to ${at.bottom} px, out of Details' view (0 to ${VIEW_HEIGHT})`
    ).toBe(true);

    // The next frame raises that scroll's own event, which the anchor takes for its own; then the
    // tall entry above the control's shrinks, and the anchor keeps the control where it stood.
    act(() => {
      box.dispatchEvent(new Event("scroll"));
    });
    resize(TALL_ENTRY, 500);
    expect(
      focused.getBoundingClientRect().top,
      "the entry above the focused control shrank once Details followed the control, and the control moved"
    ).toBe(at.top);
  } finally {
    restore();
  }
});

// As the tester measured it: the link's first line moved up into the line above it as its
// target's name landed, so the link grew 18 px above its top and 18 px below its foot, and its
// entry 18 px. The anchor puts the link's top back where it stood, then the hand-on brings the
// link's foot into the view: the anchor's resize observer, made as the list mounts, is called
// back before the hand-on's, made at the press, as a browser calls observers in the order they
// were made.
test("a keyboard press of Show more keeps the link it focused wholly in Details' view as the link grows above its top and below its foot", async () => {
  const { entry, focused, resize, restore } = await pressedToFoot();

  try {
    const row = focused.closest<HTMLElement>("[data-history-entry]");
    if (row === null) throw new Error("the focused control stands in no entry");
    // The link reaches 18 px above its entry's top, and down to its entry's foot.
    focused.getBoundingClientRect = () => {
      const at = row.getBoundingClientRect();
      return new DOMRect(at.left, at.top - 18, at.width, at.height + 18);
    };
    resize(entry, ROW_HEIGHT + 18);
    const at = focused.getBoundingClientRect();
    expect(
      at.top >= 0 && at.bottom <= VIEW_HEIGHT,
      `the focused link stands at ${at.top} to ${at.bottom} px, out of Details' view (0 to ${VIEW_HEIGHT})`
    ).toBe(true);
    expect(at.bottom, "Details scrolled more than the least that shows the whole link").toBe(
      VIEW_HEIGHT
    );
  } finally {
    restore();
  }
});

// The keeping of the focused control ends at the reader's first act that scrolls Details: from
// then on the reader's own place is what Details keeps.
test("after a wheel of the reader's over Details, the control a keyboard press of Show more focused grows below the view's foot and Details does not follow it", async () => {
  const { box, entry, resize, restore } = await pressedToFoot();

  try {
    const before = box.scrollTop;
    // The act itself ends the keeping, whatever it scrolls; happy-dom scrolls nothing for it.
    act(() => {
      box.dispatchEvent(new WheelEvent("wheel", { bubbles: true, deltaY: 40 }));
    });
    resize(entry, ROW_HEIGHT + 36);
    expect(
      box.scrollTop,
      "the focused control grew after the reader's wheel, and Details followed it"
    ).toBe(before);
  } finally {
    restore();
  }
});

// A focused control wholly outside Details' view is no place the reader sees: its growth there
// brings it back into the view by no scroll.
test("the control a keyboard press of Show more focused grows wholly below Details' view and Details does not follow it", async () => {
  const { box, entry, focused, resize, restore } = await pressedToFoot();

  try {
    // A scroll no act of the reader's made, as another script's, takes the control wholly below
    // the view, and the frame raises its event.
    box.scrollTop -= VIEW_HEIGHT;
    act(() => {
      box.dispatchEvent(new Event("scroll"));
    });
    expect(
      focused.getBoundingClientRect().top >= VIEW_HEIGHT,
      "the focused control is not wholly below Details' view, so the case proves nothing"
    ).toBe(true);
    const before = box.scrollTop;
    resize(entry, ROW_HEIGHT + 36);
    expect(
      box.scrollTop,
      "the focused control grew wholly below Details' view, and Details followed it"
    ).toBe(before);
  } finally {
    restore();
  }
});

// The control a press of Show more focuses can stop being a control before the reader acts: a
// reference link in the entry's line whose target answers that it is missing loses its `href` to
// its renderer (`RefLink`), its words left as plain text, and Chromium then moves the focus off it
// to the page's body, with a `focusout` that names no next element, a moment after the change. The
// entry and its other controls stand. The focus goes on to the entry's first control left, and
// that control is kept in Details' view as the focused one was. Here the first control of the
// entry the press drew, its author's link, stands for that reference. happy-dom leaves the focus
// on an element that loses its `href`, so the case's `blur()` stands for Chromium's move: the
// same `focusout`, after the change. A removed link is handed on the same way: Chromium sends it
// a `focusout` as it goes, and happy-dom, as a browser may, sends none and puts the focus on the
// body, so that case proves the path of a removal heard only by the mutation observer. The browser
// row of `e2e/issue-page-m.e2e.ts` proves Chromium's own.
for (const [change, take] of [
  [
    "turns into plain words",
    (link: HTMLElement) => {
      link.removeAttribute("href");
      link.blur();
    },
  ],
  ["leaves the entry", (link: HTMLElement) => link.remove()],
] as const) {
  test(`a keyboard press of Show more whose focused link then ${change} moves the focus to the entry's next control and keeps it in Details' view`, async () => {
    const { box, entry, focused, resize, restore } = await pressedToFoot();

    try {
      const row = focused.closest<HTMLElement>("[data-history-entry]");
      const next = row?.querySelectorAll<HTMLElement>("a[href], button:not(:disabled)")[1];
      if (!(focused instanceof HTMLAnchorElement) || next === undefined) {
        throw new Error("the press did not focus a link of an entry with a control after it");
      }
      await act(async () => {
        take(focused);
      });
      expect(
        document.activeElement,
        "the focus fell to the page's body when the focused link left the entry"
      ).toBe(next);
      const before = box.scrollTop;
      resize(entry, ROW_HEIGHT + 36);
      expect(
        box.scrollTop - before,
        "the control the focus went on to grew 36 px below Details' view, and Details did not follow it"
      ).toBe(36);
    } finally {
      restore();
    }
  });
}

// The press reads the log's next page, which adds ten files to the run of uploads drawn last, in
// view above Show more, and draws the next entries under that run. The run grows to 440 px, so
// the files it gained stand in Details' view and the first entry drawn under them does not, by
// 20 px. The focus went to a file the page added, which was in view, and not to the first entry
// the press drew. It goes to that entry, and Details is brought to it by those 20 px.
test("a press of Show more moves the focus to the first entry it drew though a control the page added to a run above stands in view and that entry does not", async () => {
  const firstPage = [
    ...Array.from({ length: PAGE_SIZE - 1 }, (_, index) => claimAgo(400 - index, index + 1)),
    upload(381, 20, "shot-00.png"),
    claimAgo(380, 20),
    ...Array.from({ length: 179 }, (_, index) => outlineEventAgo(379 - index, 20)),
  ];
  const olderPage = [
    ...Array.from({ length: 10 }, (_, index) =>
      upload(200 - index, 21 + index, `shot-${index}.png`)
    ),
    ...Array.from({ length: 25 }, (_, index) => claimAgo(190 - index, 100 + index)),
  ];
  const reads = [
    spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) =>
      options?.before === undefined ? [...firstPage] : [...olderPage]
    ),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  // Details' own box, which the history scrolls in, at the top of the window.
  const box = document.body.appendChild(document.createElement("div"));
  box.style.overflowY = "auto";
  // A run of uploads is a line for each of its files; every other entry is one line.
  const restoreLayout = installDetailsLayout(
    box,
    (row) => ROW_HEIGHT * Number(row.getAttribute("data-history-uploads") ?? 1)
  );
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHistory(undefined, box);
    unmount = drawn.unmount;
    const more = await screen.findByRole("button", { name: SHOW_MORE });
    await waitFor(() => expect(drawnEntries()).toHaveLength(PAGE_SIZE));
    // Details scrolled down to the run of one upload, 140 px down its view, Show more under it.
    box.scrollTop = LIST_TOP + (PAGE_SIZE - 1) * ROW_HEIGHT - 140;
    act(() => more.focus());
    await userEvent.keyboard("{Enter}");
    await waitFor(() =>
      expect(drawn.queryClient.getQueryState(["events", KEY])?.fetchStatus).toBe("idle")
    );
    await waitFor(() => expect(drawnEntries()).toHaveLength(2 * PAGE_SIZE));
    const added = box.querySelector<HTMLElement>('[data-history-entry="file:200"] a[href]');
    const addedAt = added?.getBoundingClientRect();
    expect(
      addedAt !== undefined && addedAt.top >= 0 && addedAt.bottom <= VIEW_HEIGHT,
      "no control the page added to the run stands in Details' view, so the row proves nothing"
    ).toBe(true);

    const focused = document.activeElement;
    expect(
      focused?.closest("[data-history-entry]")?.getAttribute("data-history-entry"),
      "Show more moved the focus to a control other than the first entry it drew"
    ).toBe("event:380");
    const at = focused?.getBoundingClientRect();
    expect(
      at !== undefined && at.top >= 0 && at.bottom <= VIEW_HEIGHT,
      `Show more moved the focus to a control out of Details' view (${at?.top} to ${at?.bottom} px, the view 0 to ${VIEW_HEIGHT})`
    ).toBe(true);
  } finally {
    unmount?.();
    restoreLayout();
    box.remove();
    for (const read of reads) read.mockRestore();
  }
});

// The address of a short reply under a long root turn lands the reply in the middle of Details'
// view, its root turn drawn whole above it. The root turn then changes height in the frame after
// the landing's scroll - its words finish drawing, a picture in them loads - which moves the reply
// by as much and leaves the top of the thread's entry where it was. The history's anchor kept
// that entry's top, so the reply went out of Details' view. Found by the tester on a running build
// at 1440 px, where the root turn was cut from 1,980 px to 190 px after the landing and took the
// reply 1,790 px up. The anchor keeps the landed turn's own place inside its entry (`keepRow`'s
// `element`).
test("the address of a short reply under a long root turn keeps the reply where it landed as the root turn changes height", async () => {
  // Ten claims newer than the thread and ten older, so Details scrolls on both sides of it.
  const log = [
    ...Array.from({ length: 10 }, (_, index) => claimAgo(20 - index, index + 1)),
    ...Array.from({ length: 10 }, (_, index) => claimAgo(10 - index, 300 + index)),
  ];
  const root = agentMessage("m-root", 120);
  const reply = { ...agentMessage("m-reply", 60).message, in_reply_to: "m-root" };
  const restoreReads = stubHistoryReads(log, [{ ...root, replies: [reply] }]);
  // Details' own box, which the history scrolls in, at the top of the window.
  const box = document.body.appendChild(document.createElement("div"));
  box.style.overflowY = "auto";
  Object.defineProperty(box, "clientHeight", { configurable: true, value: VIEW_HEIGHT });
  // The root turn is 2,000 px tall until the case changes its height; the reply is 100 px.
  const turnHeights = new Map([
    ["m-root", 2000],
    ["m-reply", 100],
  ]);
  const turnHeight = (turn: Element) =>
    turnHeights.get(turn.getAttribute("data-history-item") ?? "") ?? ROW_HEIGHT;
  const heightOf = (row: Element) => {
    const turns = [...row.querySelectorAll("[data-history-item]")];
    return turns.length === 0 ? ROW_HEIGHT : turns.reduce((sum, turn) => sum + turnHeight(turn), 0);
  };
  // A thread's turns stack inside its entry.
  const restoreLayout = installDetailsLayout(box, heightOf, turnHeight);
  // The landing scrolls Details as the browser does.
  const intoView = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element,
    how?: boolean | ScrollIntoViewOptions
  ) {
    const at = this.getBoundingClientRect();
    const block = typeof how === "object" ? how.block : "start";
    box.scrollTop += block === "center" ? at.top + at.height / 2 - VIEW_HEIGHT / 2 : at.top;
  });
  // The anchor hears a change of height outside its commits from resize observers, which
  // happy-dom never calls back: the case calls them back as the frame after the change does.
  const sizes = installResizeObserver();
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory("m-reply", box).unmount;
    const marked = await waitFor(() => {
      const turn = box.querySelector<HTMLElement>('[data-history-item="m-reply"]');
      expect(turn?.getAttribute("aria-current"), "the reply's address marked no turn").toBe("true");
      return turn as HTMLElement;
    });
    // The reply is no taller than Details' view, so it lands in the middle of it.
    const landed = (VIEW_HEIGHT - 100) / 2;
    await waitFor(() =>
      expect(marked.getBoundingClientRect().top, "the address did not land the reply").toBe(landed)
    );

    // The root turn changes to 200 px after the landing's scroll, which the anchor sees only as
    // content resized. The next frame raises the landing scroll's own event, then observes the
    // change.
    turnHeights.set("m-root", 200);
    act(() => {
      box.dispatchEvent(new Event("scroll"));
      sizes.resize();
    });

    const at = marked.getBoundingClientRect();
    expect(at.top, "the root turn above the landed reply changed height and moved the reply").toBe(
      landed
    );
    expect(
      at.top >= 0 && at.bottom <= VIEW_HEIGHT,
      `the reply stood at ${at.top} px, out of Details' view (0 to ${VIEW_HEIGHT})`
    ).toBe(true);
  } finally {
    unmount?.();
    sizes.restore();
    restoreLayout();
    intoView.mockRestore();
    box.remove();
    restoreReads();
  }
});

// The address of a turn draws the history down to that turn's thread, however far past the
// first 20 entries it stands, marks the turn and brings it into view; and each following of the
// address does so again, the same address followed a second time among them, however far the
// reader has read since. The landing once happened once for each turn the list drew.
test("a turn past the first 20 entries is drawn by its address, also when the address is followed again", async () => {
  // Twenty-five claims newer than the daemon's thread of a minute ago, whose entry is the 26th.
  const restoreReads = stubHistoryReads(
    Array.from({ length: 25 }, (_, index) => claimAgo(25 - index, index + 1)),
    [agentMessage("m-past", 60)]
  );
  // Each element the history brings into view.
  const landed: Element[] = [];
  const intoView = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element
  ) {
    landed.push(this);
  });
  const landings = () =>
    landed.filter((element) => element.getAttribute("data-history-item") === "m-past").length;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["agents"], []);
  const issue = { artifacts: [], key: KEY } as unknown as IssueDetails;
  const at = (item: AddressedTurn) => (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <IssueHistory
          activityFollowing={undefined}
          issue={issue}
          item={item}
          state={{ [KEY]: { dismissed: [], last_read_seq: 99999, pinned: false, seq: 0 } }}
          visitMark={99999}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  let view: RenderResult | undefined;

  try {
    view = render(at({ following: FOLLOWING, id: "m-past" }));
    await waitFor(() =>
      expect(
        document.querySelector('[data-history-item="m-past"]')?.getAttribute("aria-current"),
        "the address of a turn past the first 20 entries marks no turn"
      ).toBe("true")
    );
    expect(drawnEntries()).toHaveLength(26);
    await waitFor(() => expect(landings(), "the address did not bring its turn into view").toBe(1));

    view.rerender(at({ following: `${FOLLOWING} again`, id: "m-past" }));
    await waitFor(() =>
      expect(
        landings(),
        "the same address followed again did not bring its turn into view again"
      ).toBe(2)
    );
    expect(drawnEntries()).toHaveLength(26);
  } finally {
    view?.unmount();
    intoView.mockRestore();
    restoreReads();
  }
});

// A turn carries the marks `main` draws on a post its author corrected: one its author edited
// reads " · edited" after its author, and one its author deleted is drawn as its deleted line,
// who deleted it and when, in place of its words, with no Reply, as Dispatch takes no reply to a
// deleted post.
test("a turn its author edited reads edited after its author, and a turn its author deleted is drawn as its deleted line", async () => {
  const root = agentMessage("m-edited", 120).message;
  const person = { id: "alice", kind: "user" } as const;
  const restoreReads = stubHistoryReads(
    [],
    [
      {
        message: { ...root, edited_at: new Date(Date.now() - 60_000).toISOString() },
        replies: [
          {
            ...agentMessage("m-deleted", 90).message,
            author: person,
            body: "",
            deleted_at: new Date(Date.now() - 30_000).toISOString(),
            deleted_by: person,
            in_reply_to: "m-edited",
          },
        ],
      },
    ]
  );
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["agents"], []);
  const issue = { artifacts: [], key: KEY } as unknown as IssueDetails;
  let unmount: (() => void) | undefined;

  try {
    unmount = render(
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <OutlineWriterProvider issueKey={KEY}>
            <IssueHistory
              activityFollowing={undefined}
              issue={issue}
              item={undefined}
              state={{ [KEY]: { dismissed: [], last_read_seq: 99, pinned: false, seq: 0 } }}
              visitMark={99}
            />
          </OutlineWriterProvider>
        </QueryClientProvider>
      </MemoryRouter>
    ).unmount;
    const edited = await waitFor(() => {
      const turn = document.querySelector<HTMLElement>('[data-history-item="m-edited"]');
      if (turn === null) throw new Error("the edited turn is not drawn");
      return turn;
    });
    expect(
      edited.querySelector("p")?.textContent?.endsWith(` · ${CONTROL_LABELS.post_edited}`),
      "the turn its author edited does not read edited after its author"
    ).toBe(true);
    expect(edited.querySelector("[data-history-reply]")).not.toBeNull();

    const deleted = document.querySelector<HTMLElement>('[data-history-item="m-deleted"]');
    const line = deleted?.querySelector("[data-post-deleted]");
    expect(line?.textContent, "the turn its author deleted draws no deleted line").toStartWith(
      `${CONTROL_LABELS.post_deleted_by} `
    );
    expect(deleted?.querySelector("[data-history-reply]"), "a deleted turn draws Reply").toBeNull();
    expect(deleted?.textContent, "a deleted turn reads edited").not.toContain(
      CONTROL_LABELS.post_edited
    );
  } finally {
    unmount?.();
    restoreReads();
  }
});

// A turn's words are drawn whole, at any length, with no cut and no "more" control, whether or
// not an address names the turn: here 3,000 words over 400 lines, laid out as the browser would
// lay them out, which the outline cuts to six lines with "more · N words" under them.
test("a long turn is drawn whole, with no more control", async () => {
  const words = Array.from({ length: 3000 }, (_unused, at) => `word${at + 1}`).join(" ");
  const long = agentMessage("m-long", 60);
  const restoreReads = stubHistoryReads(
    [],
    [{ ...long, message: { ...long.message, body: words } }]
  );
  await warmMarkdownRenderer();
  // Every element lays out 9,600 px tall: no DOM of a unit test lays anything out, and a cut
  // reads the words' own boxes (`clamp-measurer.ts`).
  const scrollHeight = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", {
    configurable: true,
    get: () => 9600,
  });
  const boxes = spyOn(Element.prototype, "getBoundingClientRect").mockReturnValue(
    new DOMRect(0, 0, 600, 9600)
  );
  const near = installIntersectionObserver();
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    const turn = await waitFor(() => {
      const drawn = document.querySelector<HTMLElement>('[data-history-item="m-long"]');
      expect(drawn?.textContent, "the long turn's last word is not drawn").toContain("word3000");
      return drawn as HTMLElement;
    });
    // The words are brought near the viewport, where a cut would measure them.
    await measureNearWords(near);

    expect(turn.querySelector("[data-node-more]"), "the long turn draws a more control").toBeNull();
    expect(
      [...turn.querySelectorAll<HTMLElement>("*")].filter(
        (element) => element.style.maxHeight !== ""
      ),
      "the long turn's words are cut"
    ).toEqual([]);
    expect(turn.textContent).toContain("word3000");
  } finally {
    unmount?.();
    near.restore();
    boxes.mockRestore();
    if (scrollHeight === undefined) Reflect.deleteProperty(HTMLElement.prototype, "scrollHeight");
    else Object.defineProperty(HTMLElement.prototype, "scrollHeight", scrollHeight);
    restoreReads();
  }
});

/** Version 1 of a handoff of `KEY`, whose body's summary is `summary`. */
function handoffRecord(summary: string): Handoff {
  return {
    author: { id: "fedcba9876543210", kind: "session", origin: { session_title: "Implementer" } },
    body: JSON.stringify({ schemaVersion: 1, summary }),
    created_at: new Date().toISOString(),
    id: "handoff-1",
    issue_key: KEY,
    phase: "implement",
    version: 1,
  };
}

/** A handoff's fields in the history, drawn open, with nothing above them but a router and
 *  `queryClient`, a query client that never retries where the case gives none. */
function drawHandoff(
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
): { queryClient: QueryClient; unmount: () => void } {
  const { unmount } = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <HistoryHandoff handoffId="handoff-1" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, unmount };
}

// Each handoff of the history is drawn open as the history draws its line, its fields and each
// proof entry read with no press. Only the reader folds it, by its line's Hide fields, and Show
// fields opens it again from the body read once.
test("a handoff is drawn open with its fields and each proof entry, and a press on its line folds it", async () => {
  const restoreReads = stubHistoryReads([
    {
      ...claim(1),
      payload: { handoff_id: "handoff-1", phase: "implement", version: 1 },
      type: "handoff.recorded",
    } as DispatchEvent,
  ]);
  const getHandoff = spyOn(api, "getHandoff").mockResolvedValue({
    ...handoffRecord("Second pass"),
    body: JSON.stringify({
      proof: ["The unit cases pass.", "The type check passes."],
      schemaVersion: 1,
      summary: "Second pass",
    }),
  });
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    await screen.findByText("Second pass");
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    expect(
      list.querySelectorAll('[data-handoff-field="proof"]'),
      "the handoff's proof entries are not drawn open"
    ).toHaveLength(2);
    const toggle = within(list).getByRole("button", { name: CONTROL_LABELS.hide_fields });
    expect(toggle).toHaveAttribute("aria-expanded", "true");

    await userEvent.click(toggle);
    expect(screen.queryByText("Second pass"), "the press did not fold the handoff").toBeNull();
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveTextContent(CONTROL_LABELS.show_fields);

    await userEvent.click(toggle);
    await screen.findByText("Second pass");
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(getHandoff, "opening the handoff again read its body again").toHaveBeenCalledTimes(1);
  } finally {
    unmount?.();
    getHandoff.mockRestore();
    restoreReads();
  }
});

/** Fails a refresh of the handoff over the body its opened fields draw (`Second pass`), and then
 *  its first read (`failRefreshOverHeld`). Where `refreshed` is given, it asserts as well that the
 *  handoff has been read that many times once the refresh has failed. */
function failHandoffRefreshThenFirstRead(
  queryClient: QueryClient,
  getHandoff: Mock<typeof api.getHandoff>,
  refreshed?: number
): Promise<void> {
  return failRefreshOverHeld({
    client: queryClient,
    failNextFetch: () => getHandoff.mockRejectedValueOnce(new Error("the handoff is down")),
    held: "the fields",
    queryKey: ["handoff", "handoff-1"],
    refresh: () => queryClient.invalidateQueries(),
    refreshFailed:
      refreshed === undefined
        ? undefined
        : () => expect(getHandoff).toHaveBeenCalledTimes(refreshed),
    stillDrawn: () => expect(screen.queryByText("Second pass")).not.toBeNull(),
  });
}

// A handoff's opened fields hold its body whatever a later read's outcome, as the history holds
// its log: an invalidation of every read after a write reads the body again, and a read that
// fails then says no failure over the fields it held.
test("a handoff's opened fields stay drawn when a read of them again fails", async () => {
  const getHandoff = spyOn(api, "getHandoff").mockResolvedValue(handoffRecord("Second pass"));
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHandoff();
    unmount = drawn.unmount;
    await screen.findByText("Second pass");

    getHandoff.mockRejectedValue(new Error("the handoff is down"));
    await act(async () => {
      await drawn.queryClient.invalidateQueries();
    });
    expect(getHandoff).toHaveBeenCalledTimes(2);
    expect(
      screen.queryByRole("alert"),
      "a failed read again of the handoff said a failure over the fields it held"
    ).toBeNull();
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.retry }),
      "a failed read again of the handoff offered a Retry over the fields it held"
    ).toBeNull();
    expect(
      screen.queryByText("Second pass"),
      "a failed read again of the handoff erased the fields it held"
    ).not.toBeNull();
  } finally {
    unmount?.();
    getHandoff.mockRestore();
  }
});

// With no body held, the line that said the read failed stays, Retry and all, while the read its
// Retry sends is out. The focus of a keyboard reader on that Retry goes with its line, once the
// read lands, to the fields it read, rather than falling to the page with the Retry.
test("a handoff's Retry pressed by keyboard hands the focus on as its line goes, to the fields it read", async () => {
  const getHandoff = spyOn(api, "getHandoff")
    .mockRejectedValueOnce(new Error("the handoff is down"))
    .mockResolvedValue(handoffRecord("Second pass"));
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHandoff().unmount;
    const toggle = screen.getByRole("button", { name: CONTROL_LABELS.hide_fields });
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    retry.focus();
    await userEvent.keyboard("{Enter}");
    await screen.findByText("Second pass");

    expect(getHandoff).toHaveBeenCalledTimes(2);
    const active = document.activeElement;
    const fields = document.getElementById(toggle.getAttribute("aria-controls") ?? "");
    expect(
      active,
      `the handoff's Retry went with its line and the focus did not go to the fields it read (retry connected: ${retry.isConnected} focus: ${active?.tagName})`
    ).toBe(fields);
  } finally {
    unmount?.();
    getHandoff.mockRestore();
  }
});

// A handoff's Retry hands the focus on only after the reader's press of it. A read that answers on
// its own - a window focus, an invalidation of every read after a write - takes the line away with
// no press of the reader's, so the focus its Retry held stays on the page's body, as on every page
// of Dispatch.
test("a handoff's Retry taken away by a read the reader did not press for leaves the focus on the page's body", async () => {
  const getHandoff = spyOn(api, "getHandoff")
    .mockRejectedValueOnce(new Error("the handoff is down"))
    .mockResolvedValue(handoffRecord("Second pass"));
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHandoff();
    unmount = drawn.unmount;
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    act(() => retry.focus());

    await act(async () => {
      await drawn.queryClient.invalidateQueries();
    });
    await screen.findByText("Second pass");
    expect(getHandoff).toHaveBeenCalledTimes(2);
    expect(retry.isConnected, "the handoff's Retry stayed once the read answered").toBe(false);
    expect(
      document.activeElement,
      "a read the reader did not press for took the handoff's Retry away, and the focus left the page's body"
    ).toBe(document.body);
  } finally {
    unmount?.();
    getHandoff.mockRestore();
  }
});

// With no body held, the line that says a read of it failed stays, its Retry busy, while the read
// that Retry sent is out. A press of the busy Retry joins that read and sends no other. A press
// moves no focus: a reader who presses the busy Retry, or comes back to it and presses it again,
// keeps the focus there. A refresh that fails over a body held draws no line, so the Retry is a
// read's that failed holding no body.
test("a handoff's busy Retry sends no read while the one it sent is out, and keeps the focus", async () => {
  const getHandoff = spyOn(api, "getHandoff").mockResolvedValue(handoffRecord("Second pass"));
  // The read the Retry sends is held out until the case lets it land.
  const again = Promise.withResolvers<Handoff>();
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHandoff();
    unmount = drawn.unmount;
    await screen.findByText("Second pass");
    await failHandoffRefreshThenFirstRead(drawn.queryClient, getHandoff, 2);
    expect(getHandoff).toHaveBeenCalledTimes(3);
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    getHandoff.mockImplementation(() => again.promise);

    for (let press = 0; press < 3; press += 1) await userEvent.click(retry);

    expect(
      getHandoff,
      "each press of the busy Retry sent another read while one was out"
    ).toHaveBeenCalledTimes(4);
    expect(retry, "the Retry went while the read it sent is out").toBeInTheDocument();
    expect(retry, "Retry does not read busy while the read it sent is out").toHaveAttribute(
      "aria-busy",
      "true"
    );
    expect(
      document.activeElement,
      "a press of the busy Retry moved the focus off it to the toggle"
    ).toBe(retry);

    await act(async () => again.resolve(handoffRecord("Third pass")));
    await screen.findByText("Third pass");
    expect(screen.queryByRole("alert")).toBeNull();
  } finally {
    unmount?.();
    getHandoff.mockRestore();
  }
});

// A second press of a handoff's Retry made before React draws the read the first one sent finds
// the render's `isFetching` still false. A press while the read is out joins it, sends no other
// read, and moves no focus. A refresh that fails over a body held draws no line, so the Retry is a
// read's that failed holding no body.
test("two presses of a handoff's Retry before a render send one read", async () => {
  const getHandoff = spyOn(api, "getHandoff").mockResolvedValue(handoffRecord("Second pass"));
  // Every read sent from here is held out until the case lets it land.
  const again = Promise.withResolvers<Handoff>();
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHandoff();
    unmount = drawn.unmount;
    await screen.findByText("Second pass");
    await failHandoffRefreshThenFirstRead(drawn.queryClient, getHandoff, 2);
    expect(getHandoff).toHaveBeenCalledTimes(3);
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    getHandoff.mockImplementation(() => again.promise);
    act(() => retry.focus());

    // Both presses in one task inside one `act`, so React draws nothing between them.
    act(() => {
      retry.click();
      retry.click();
    });
    expect(
      getHandoff,
      "the second press ended the read the first one sent and sent another"
    ).toHaveBeenCalledTimes(4);
    expect(
      document.activeElement,
      "the press that sent the read moved the focus off the Retry, whose line stays while the read is out"
    ).toBe(retry);

    await act(async () => again.resolve(handoffRecord("Third pass")));
    await screen.findByText("Third pass");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(getHandoff).toHaveBeenCalledTimes(4);
  } finally {
    unmount?.();
    getHandoff.mockRestore();
  }
});

// A reader who presses a handoff's Retry twice by keyboard - the first press seemed to do
// nothing, or the key repeated - asked for the fields once. The first press hands the focus to
// the toggle above the line, so the second lands on that toggle and shuts the fields the reader
// pressed Retry to read. Found by the tester on a running build of 7c09739d: on a seeded issue,
// Show fields with the handoff's read failing, then Enter twice on Retry, left the toggle
// reading "Show fields" and the fields shut once the read landed.
test("a handoff's Retry pressed twice by keyboard leaves its fields open once the read lands", async () => {
  const getHandoff = spyOn(api, "getHandoff").mockRejectedValueOnce(
    new Error("the handoff is down")
  );
  // The read the Retry sends is held out until the case lets it land.
  const again = Promise.withResolvers<Handoff>();
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHandoff().unmount;
    const toggle = screen.getByRole("button", { name: CONTROL_LABELS.hide_fields });
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    getHandoff.mockImplementation(() => again.promise);
    retry.focus();
    await userEvent.keyboard("{Enter}{Enter}");
    await act(async () => again.resolve(handoffRecord("Second pass")));

    expect(
      toggle,
      "the second press of Retry landed on the toggle and shut the fields it asked for"
    ).toHaveAttribute("aria-expanded", "true");
    await screen.findByText("Second pass");
    expect(getHandoff, "the two presses read the handoff more than once").toHaveBeenCalledTimes(2);
  } finally {
    unmount?.();
    getHandoff.mockRestore();
  }
});

// With no body held, a fetch of the read clears its failure and puts it back to pending as it
// starts, so a press of Retry drew the loading line in its line's place, and the reader's focus
// went from the Retry to that line. The line that said the read failed stays instead, its Retry
// busy and holding the focus while the read it sent is out, where a second Enter joins that read
// and the fields stay open; it stays, with the same Retry and the focus, where the read fails
// again, and goes once a read lands, handing the focus to the fields it drew.
test("a handoff's Retry with no body held keeps its line and the focus while its read is out, then hands it to what the read drew", async () => {
  const getHandoff = spyOn(api, "getHandoff").mockRejectedValueOnce(
    new Error("the handoff is down")
  );
  // Each read a Retry sends is held out until the case lets it land.
  let again = Promise.withResolvers<Handoff>();
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHandoff().unmount;
    const toggle = screen.getByRole("button", { name: CONTROL_LABELS.hide_fields });
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    getHandoff.mockImplementation(() => again.promise);
    retry.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => {
      expect(
        retry,
        "the line and its Retry went while the read Retry sent again is out"
      ).toBeInTheDocument();
      expect(retry, "Retry does not read busy while the read it sent again is out").toHaveAttribute(
        "aria-busy",
        "true"
      );
    });
    expect(
      screen.queryByText(ANSWER_LINES.loading_handoff),
      "the loading line took the place of the failure its Retry reads again"
    ).toBeNull();
    expect(
      document.activeElement,
      `the press of Retry moved the focus off it while its line stays (focus: ${document.activeElement?.tagName} ${document.activeElement?.textContent})`
    ).toBe(retry);
    await userEvent.keyboard("{Enter}");
    expect(toggle, "the second press of Retry shut the fields it asked for").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    expect(document.activeElement, "the second press of the busy Retry moved the focus").toBe(
      retry
    );
    expect(getHandoff, "the second press sent another read").toHaveBeenCalledTimes(2);

    await act(async () => again.reject(new Error("the handoff is still down")));
    await waitFor(() =>
      expect(retry, "Retry still reads busy after the read failed again").not.toHaveAttribute(
        "aria-busy"
      )
    );
    expect(
      screen.getByRole("button", { name: CONTROL_LABELS.retry }),
      "the line came back with another Retry"
    ).toBe(retry);
    expect(document.activeElement, "Retry lost the focus when the read failed again").toBe(retry);

    again = Promise.withResolvers<Handoff>();
    await userEvent.keyboard("{Enter}");
    await act(async () => again.resolve(handoffRecord("Second pass")));
    await screen.findByText("Second pass");
    const fields = document.getElementById(toggle.getAttribute("aria-controls") ?? "");
    expect(fields?.tagName).toBe("DL");
    expect(
      document.activeElement,
      "the failure line went with the focus on its Retry and did not hand it to the fields the read drew"
    ).toBe(fields);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(getHandoff).toHaveBeenCalledTimes(3);
  } finally {
    unmount?.();
    getHandoff.mockRestore();
  }
});

// A refresh that fails over a body held draws no line, and the fields stay drawn. With no body
// held, the line that says a read of it failed stays while the read its Retry sent is out, so the
// focus of a keyboard reader stays on the busy Retry, where a second Enter joins that read and
// the fields stay open; once the read lands, the line goes and hands the focus to the fields it
// drew.
test("a refresh that fails over a handoff's body held draws no line, and its Retry keeps the focus while its read is out, then hands it to the fields", async () => {
  const getHandoff = spyOn(api, "getHandoff").mockResolvedValue(handoffRecord("Second pass"));
  // The read the Retry sends is held out until the case lets it land.
  const again = Promise.withResolvers<Handoff>();
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHandoff();
    unmount = drawn.unmount;
    const toggle = screen.getByRole("button", { name: CONTROL_LABELS.hide_fields });
    await screen.findByText("Second pass");
    await failHandoffRefreshThenFirstRead(drawn.queryClient, getHandoff);
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    getHandoff.mockImplementation(() => again.promise);
    retry.focus();

    await userEvent.keyboard("{Enter}");
    expect(
      document.activeElement,
      `the press of Retry moved the focus off it while its line stays (focus: ${document.activeElement?.tagName} ${document.activeElement?.textContent})`
    ).toBe(retry);
    await userEvent.keyboard("{Enter}");
    expect(toggle, "the second press of Retry shut the fields it asked for").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    expect(document.activeElement, "the second press of the busy Retry moved the focus").toBe(
      retry
    );
    expect(getHandoff, "the second press sent another read").toHaveBeenCalledTimes(4);

    await act(async () => again.resolve(handoffRecord("Third pass")));
    await screen.findByText("Third pass");
    expect(screen.queryByRole("alert")).toBeNull();
    const fields = document.getElementById(toggle.getAttribute("aria-controls") ?? "");
    expect(fields?.tagName).toBe("DL");
    expect(
      document.activeElement,
      "the failure line went with the focus on its Retry and did not hand it to the fields"
    ).toBe(fields);
  } finally {
    unmount?.();
    getHandoff.mockRestore();
  }
});

// A handoff's prose field draws through the shared renderer from the server's reading
// (`field_trees`), so a `dispatch://` reference in it reads as its target's name and links to
// it, never as the raw address.
test("a handoff's prose field draws a reference as its target's name, not its address", async () => {
  await warmMarkdownRenderer();
  const reference = "dispatch://CORE-2";
  const summary = `Follows ${reference} and **keeps** the tests green.`;
  const getHandoff = spyOn(api, "getHandoff").mockResolvedValue({
    ...handoffRecord(summary),
    body: JSON.stringify({
      files: ["a.ts"],
      proof: [`See ${reference}`],
      schemaVersion: 1,
      summary,
    }),
    field_trees: {
      "/proof/0": displayTree(`See ${reference}`) as DisplayTree,
      "/summary": displayTree(summary) as DisplayTree,
    },
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["issue-summary", "CORE-2"], {
    documents: [],
    key: "CORE-2",
    project: "CORE",
    status: "in_progress",
    title: "Warm the cache",
  });
  const { unmount } = drawHandoff(queryClient);
  const summaryField = () => document.querySelector('[data-handoff-field="summary"]');

  try {
    const link = await waitFor(() => {
      const found = summaryField()?.querySelector<HTMLAnchorElement>(
        `a[data-dispatch-ref="${reference}"]`
      );
      if (found == null || found.textContent !== "CORE-2 Warm the cache") {
        throw new Error(
          `summary did not draw the reference's target name (text: ${summaryField()?.textContent})`
        );
      }
      return found;
    });
    expect(link).toHaveAttribute("href", "/issues/CORE-2");
    expect(summaryField()?.textContent).not.toContain(reference);
    expect(summaryField()?.querySelector("strong")?.textContent).toBe("keeps");
    expect(
      document.querySelector(`[data-handoff-field="proof"] a[data-dispatch-ref="${reference}"]`)
        ?.textContent
    ).toBe("CORE-2 Warm the cache");
    expect(document.querySelector('[data-handoff-field="files"] pre')?.textContent).toBe(
      '[\n  "a.ts"\n]'
    );
  } finally {
    unmount();
    queryClient.clear();
    getHandoff.mockRestore();
  }
});

// A line of the history that names a block by a UUID names it by the one line of the node that
// carries it, as the outline draws a node's line: a `dispatch://` address in the question reads as
// its target's name, from the names the page published, and never as the address its author typed.
test("a block's line in the history reads a reference as its target's name", async () => {
  await warmMarkdownRenderer();
  const block = "01f349b4-2a84-4ebb-81be-91fa2f4e2300";
  const askId = "7a1c3e5f-9b2d-4e6a-8c0f-1d3b5a7c9e20";
  const address = buildDispatchReference({ key: "CORE-2", kind: "issue" });
  const question = `Wait on ${address} first. Then ship.`;
  const restoreReads = stubHistoryReads([
    {
      ...claim(1),
      payload: { block_id: block, disturbed_by: { id: "alice", kind: "user" }, version: 3 },
      type: "block.repaired",
    } as DispatchEvent,
  ]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([
    {
      block_id: block,
      id: askId,
      kind: "question",
      question,
      question_tree: displayTree(question),
    } as unknown as IssueAskRow,
  ]);
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(
      undefined,
      undefined,
      publishedNames(new Map([[address, "Ship the release"]]))
    ).unmount;
    const path = buildIssuePath({ id: askId, key: KEY, kind: "ask" });
    const line = await waitFor(() => {
      const found = document.querySelector<HTMLElement>("[data-event-seq]");
      if (found?.querySelector(`a[href="${path}"]`) == null) {
        throw new Error(`the block's line is not drawn (text: ${found?.textContent})`);
      }
      return found;
    });
    await waitFor(() =>
      expect(
        line.querySelector(`a[href="${path}"]`)?.textContent,
        "the block's line does not read its reference as its target's name"
      ).toBe("Wait on CORE-2 Ship the release first.")
    );
    expect(line.textContent).toContain("repaired server-owned state on block “Wait on CORE-2");
    expect(line.textContent, "the block's line draws the address as typed").not.toContain(
      "dispatch://"
    );
  } finally {
    unmount?.();
    listIssueAsks.mockRestore();
    restoreReads();
  }
});
