import { afterEach, beforeEach, expect, setSystemTime, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { api } from "../../api/client";
import { prependEventToLog } from "../../api/event-feed";
import type { Artifact, DispatchEvent, Handoff, IssueDetails, MessageRead } from "../../api/types";
import { linkText, pageInkText } from "../../theme/classes";
import { dateKey, dayLabel } from "../activity/activity-model";
import { buildIssuePath } from "../refs/routes";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { HistoryHandoff } from "./HistoryHandoff";
import { IssueHistory } from "./IssueHistory";
import { type OutlineWriter, OutlineWriterProvider, useOutlineWriter } from "./outline-writer";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";

const KEY = "CORE-1";

// The history draws every day before the reader's today folded, and these cases date what it
// reads some seconds, minutes or days before now. Each case runs at one fixed midday, so none
// reads the wall clock: one started just after midnight would put its entries on yesterday.
// `setSystemTime` holds `Date` alone; the timers the cases wait on (React Query's, `waitFor`'s,
// the animation frames) run on the real clock as before.
const NOON = new Date(2026, 10, 19, 12, 0, 0, 0);
beforeEach(() => setSystemTime(NOON));
afterEach(() => setSystemTime());

/** A claim of the issue, which the history draws as one line (`history-model.ts`), made
 *  `20 - seq` seconds ago, so a higher sequence is newer and every one is today's, its day open. */
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
 *  issue's messages read `messages`, with no comment and no ask; and the asks with their threads
 *  the page's writer reads (`outline-writer.tsx`), none. Returns what restores them. */
function stubHistoryReads(
  events: readonly DispatchEvent[],
  messages: readonly MessageRead[] = []
): () => void {
  const reads = [
    spyOn(api, "getIssueEvents").mockResolvedValue([...events]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([...messages]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listIssueAskThreads").mockResolvedValue([]),
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
            activityAddress={undefined}
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

// The history draws every day before the reader's today folded. A reader reading today's entries
// as midnight passes is still on that day: an event that arrives after midnight opens the new
// day at the head of the history, and the entry the reader reads stays drawn where it was, as it
// does for any other arrival. Found by the tester: the day the reader was in folded to one line
// under them once the first render after midnight read the new date.
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
            activityAddress={undefined}
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

// Midnight opens the day that was today as if the reader had opened it, and nothing else: a day
// before it the reader opened stays open, one they opened and folded again stays folded, the new
// day at the head is drawn open as today's, and the day that was today folds at a press of its
// line, as any day the reader opened does.
test("midnight opens the day that was today and leaves every other day as the reader left it", async () => {
  setSystemTime(new Date(2026, 10, 19, 23, 59, 50, 0));
  // A claim at sequence `seq` at noon of November's day `date`, in the reader's own time.
  const onDay = (seq: number, date: number): DispatchEvent => ({
    ...claim(seq),
    created_at: new Date(2026, 10, date, 12, 0, 0, 0).toISOString(),
  });
  const restoreReads = stubHistoryReads([
    claim(6),
    claim(5),
    onDay(4, 18),
    onDay(3, 18),
    onDay(2, 17),
    onDay(1, 17),
  ]);
  const lineOf = (date: string) =>
    document.querySelector<HTMLElement>(`[data-history-day="${date}"] > button`);
  const press = async (date: string) => {
    const line = lineOf(date);
    if (line === null) throw new Error(`${date} draws no line`);
    await userEvent.click(line);
  };
  const drawn = (seq: number) => document.querySelector(`[data-event-seq="${seq}"]`) !== null;
  let unmount: (() => void) | undefined;

  try {
    const history = drawHistory(undefined);
    unmount = history.unmount;
    await waitFor(() => expect(lineOf("2026-11-17")).not.toBeNull());
    // The reader opens the 17th, and opens the 18th and folds it again.
    await press("2026-11-17");
    await press("2026-11-18");
    await press("2026-11-18");
    expect(drawn(2)).toBe(true);
    expect(drawn(4)).toBe(false);

    // Midnight passes; the first event of the new day arrives.
    setSystemTime(new Date(2026, 10, 20, 0, 0, 30, 0));
    act(() => {
      prependEventToLog(history.queryClient, claim(7));
    });
    await waitFor(() => expect(drawn(7), "the new day's event is not drawn").toBe(true));

    expect(lineOf("2026-11-20"), "the new day is not drawn as today's").toBeNull();
    expect(
      lineOf("2026-11-19")?.getAttribute("aria-expanded"),
      "the day that was today folded as midnight passed"
    ).toBe("true");
    expect(drawn(6) && drawn(5)).toBe(true);
    expect(
      lineOf("2026-11-18")?.getAttribute("aria-expanded"),
      "midnight opened a day the reader had folded"
    ).toBe("false");
    expect(drawn(4) || drawn(3)).toBe(false);
    expect(
      lineOf("2026-11-17")?.getAttribute("aria-expanded"),
      "midnight folded a day the reader had opened"
    ).toBe("true");
    expect(drawn(2) && drawn(1)).toBe(true);

    // The day that was today is the reader's like any day they opened: a press of its line folds it.
    await press("2026-11-19");
    expect(lineOf("2026-11-19")?.getAttribute("aria-expanded")).toBe("false");
    expect(drawn(6) || drawn(5)).toBe(false);
  } finally {
    unmount?.();
    restoreReads();
  }
});

/** A message an agent wrote outside every thread a person wrote in, `ago` seconds ago: a thread
 *  of the history whose first turn's event no page of the log read here holds. */
function agentMessage(id: string, ago: number): MessageRead {
  return {
    message: {
      author: { id: "planner", kind: "session" },
      body: `Agent note ${id}.`,
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
            activityAddress={undefined}
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
      activityAddress={undefined}
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

// The address of a turn in a day before today opens that day with
// the turn marked, and the day's line is then the reader's like any other: a press on it closes
// the day the address opened.
test("a day the address opened closes when the reader presses its line", async () => {
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
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["agents"], []);
  const issue = { artifacts: [], key: KEY } as unknown as IssueDetails;
  const dayLine = () =>
    document.querySelector<HTMLElement>(
      `[data-history-day="${dateKey(earlier.message.created_at)}"] > button`
    );
  let unmount: (() => void) | undefined;

  try {
    unmount = render(
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <IssueHistory
            activityAddress={undefined}
            issue={issue}
            item="m-earlier"
            state={{ [KEY]: { dismissed: [], last_read_seq: 99, pinned: false, seq: 0 } }}
            visitMark={99}
          />
        </QueryClientProvider>
      </MemoryRouter>
    ).unmount;
    await waitFor(() => expect(dayLine()?.getAttribute("aria-expanded")).toBe("true"));
    expect(
      document.querySelector('[data-history-item="m-earlier"]')?.getAttribute("aria-current")
    ).toBe("true");

    const line = dayLine();
    if (line === null) throw new Error("the earlier day draws no line");
    await userEvent.click(line);

    expect(dayLine()?.getAttribute("aria-expanded")).toBe("false");
    expect(document.querySelector('[data-history-item="m-earlier"]')).toBeNull();

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

/** The history of `KEY` with nothing above it but a query client that never retries, drawn in
 *  `container` where one is given. */
function drawHistory(
  item: string | undefined,
  container?: HTMLElement
): {
  queryClient: QueryClient;
  unmount: () => void;
} {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["agents"], []);
  const issue = { artifacts: [], key: KEY } as unknown as IssueDetails;
  const { unmount } = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <IssueHistory
          activityAddress={undefined}
          issue={issue}
          item={item}
          state={{ [KEY]: { dismissed: [], last_read_seq: 99999, pinned: false, seq: 0 } }}
          visitMark={99999}
        />
      </QueryClientProvider>
    </MemoryRouter>,
    container === undefined ? {} : { container }
  );
  return { queryClient, unmount };
}

/** A claim of the issue at sequence `seq`, made `ago` seconds ago. */
function claimAgo(seq: number, ago: number): DispatchEvent {
  return { ...claim(seq), created_at: new Date(Date.now() - ago * 1000).toISOString(), id: seq };
}

// The address of a turn of an agents' thread opens Details at the
// history with that turn marked and on screen, whatever page of the log the thread's first turn
// is on. Here the log's first page is full, 200 claims of the last few minutes, and the agents'
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
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// The history is the issue's log as last read, and a read
// that fails says so with Retry. A read that fails once the history holds the log - a refetch
// after an event, or on a later open of Details past the query's stale time - leaves the entries
// it holds drawn. Found by the tester on a running build of b0082656: with a large real issue's
// 79 entries drawn, Details closed and opened again 31 s later with the log answering 500 drew
// "Could not load the activity." and no entry at all.
test("a failed read of the log once the history holds it keeps every entry it holds", async () => {
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

// Only a thread waits for the page that reaches back to it, so the day the pages read so far end
// in counted the entries of those pages alone: five claims of one day, two of them on the first
// page, drew "<day> · 2" until Load older drew "<day> · 5". While older pages remain, that day's
// line is its name alone, which still opens it; a day after it keeps its count.
test("the day the pages of the log read so far end in draws no count until the log is in", async () => {
  // `seconds` before noon of the day `back` days before today, in the reader's own time.
  const beforeNoon = (back: number, seconds: number) => {
    const noon = new Date();
    noon.setHours(12, 0, 0, 0);
    noon.setDate(noon.getDate() - back);
    return new Date(noon.getTime() - seconds * 1000).toISOString();
  };
  const claimOn = (seq: number, back: number, seconds: number) => ({
    ...claim(seq),
    created_at: beforeNoon(back, seconds),
  });
  const firstPage = [
    ...Array.from({ length: 198 }, (_, index) => claimOn(400 - index, 2, index)),
    claimOn(202, 3, 0),
    claimOn(201, 3, 1),
  ];
  const olderPage = [claimOn(10, 3, 2), claimOn(9, 3, 3), claimOn(8, 3, 4)];
  const reads = [
    spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) =>
      options?.before === undefined ? [...firstPage] : [...olderPage]
    ),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  const today = dateKey(new Date().toISOString());
  const lineOf = (back: number) => {
    const date = dateKey(beforeNoon(back, 0));
    return {
      element: () => document.querySelector<HTMLElement>(`[data-history-day="${date}"] > button`),
      label: dayLabel(date, today),
    };
  };
  const cut = lineOf(3);
  const after = lineOf(2);
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    const older = await screen.findByRole("button", { name: CONTROL_LABELS.load_older });
    const line = await waitFor(() => {
      const element = cut.element();
      if (element === null) throw new Error("the day the first page ends in draws no line");
      return element;
    });
    expect(line.textContent, "the day the first page ends in counts that page's entries").toBe(
      cut.label
    );
    expect(after.element()?.textContent).toBe(`${after.label} · 198`);
    await userEvent.click(line);
    expect(line.getAttribute("aria-expanded")).toBe("true");
    expect(document.querySelectorAll("[data-history-day] > ol > li")).toHaveLength(2);

    await userEvent.click(older);
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: CONTROL_LABELS.load_older })).toBeNull()
    );

    expect(cut.element()?.textContent).toBe(`${cut.label} · 5`);
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// Details is reached by Tab, so a control of the history that goes away under the focus leaves
// the focus in the history rather than on the page, as Details' Unpin does (IssueMargin.test.tsx):
// Load older, gone once the log has ended, hands it to a control the page it read added, where
// Load older stood; the Retry of a failed read, gone once the read lands, to the history's
// heading. Neither move scrolls Details: the reader keeps their place.
test("a control of the history that goes away under the focus leaves the focus in the history", async () => {
  const firstPage = Array.from({ length: 200 }, (_, index) => claimAgo(400 - index, index + 1));
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
    const older = await screen.findByRole("button", { name: CONTROL_LABELS.load_older });
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    const before = new Set(list.querySelectorAll("a[href], button"));
    const focus = spyOn(HTMLElement.prototype, "focus");
    older.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: CONTROL_LABELS.load_older })).toBeNull()
    );
    const active = document.activeElement;
    expect(
      active !== null && list.contains(active) && !before.has(active),
      "Load older went away with the log's end and the focus did not go to the page it read"
    ).toBe(true);
    expect(focus.mock.calls.at(-1), "handing the focus on scrolled Details").toEqual([
      { preventScroll: true },
    ]);
    focus.mockRestore();

    down = true;
    await act(async () => {
      await drawn.queryClient.refetchQueries({ queryKey: ["events", KEY] });
    });
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
// is said where Load older stood, and its Retry reads that page: it does Load older's work in
// Load older's place, at the foot of the list, so once the page lands the focus goes where Load
// older hands it - to a control the page added - and never to the history's heading at the top,
// from which the reader's next Tab scrolls Details back to the head of the list. Found by the
// tester on a running build of 3bd86200 with a copy of a large real issue: with 81 entries
// drawn, the next page answered 500 once, and its Retry pressed by keyboard put the focus on
// the heading "History" once the page landed.
test("the Retry of a failed next page hands the focus to the page it read, as Load older does", async () => {
  const firstPage = Array.from({ length: 200 }, (_, index) => claimAgo(400 - index, index + 1));
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
    const older = await screen.findByRole("button", { name: CONTROL_LABELS.load_older });
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    older.focus();
    await userEvent.keyboard("{Enter}");
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    const before = new Set(list.querySelectorAll("a[href], button"));
    retry.focus();
    down = false;
    await userEvent.keyboard("{Enter}");
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: CONTROL_LABELS.retry })).toBeNull()
    );
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: CONTROL_LABELS.load_older })).toBeNull()
    );
    const active = document.activeElement;
    expect(
      active !== null && list.contains(active) && !before.has(active),
      "the Retry of the next page went away once it landed and the focus did not go to the page it read"
    ).toBe(true);
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// A read of the log that fails once the history holds it keeps its line and its Retry drawn
// while that Retry reads again - the next page's where Load older stood, the log's under the
// heading - so neither is disabled then: Chromium would drop its focus to the page
// (`chromium-focus-fixup.ts`). A keyboard reader stays on the Retry while it reads, and when its
// read fails again.
test("a history Retry pressed by keyboard keeps the focus while it reads and when its read fails again", async () => {
  const firstPage = Array.from({ length: 200 }, (_, index) => claimAgo(400 - index, index + 1));
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
    (await screen.findByRole("button", { name: CONTROL_LABELS.load_older })).focus();
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
    await act(async () => {
      await drawn.queryClient.refetchQueries({ queryKey: ["events", KEY] });
    });
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
// and holding the focus, and each failure is drawn afresh while Retry stays the same button.
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
    await act(async () => {
      await drawn.queryClient.refetchQueries({ queryKey: ["events", KEY] });
    });
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
    expect(
      document.querySelectorAll("[data-event-seq]"),
      "a failed read of the log erased the history it held"
    ).toHaveLength(3);
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

// Load older is not disabled while its page is read either: Chromium would drop its focus to
// the page (`chromium-focus-fixup.ts`). A keyboard reader stays on it while the page is out, and
// once the page lands with older pages still left, Load older hands the focus on to a control
// that page added, as it does when the log's end takes it away.
test("Load older pressed by keyboard keeps the focus while its page is read, then hands it to the page it read", async () => {
  const firstPage = Array.from({ length: 200 }, (_, index) => claimAgo(600 - index, index + 1));
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
    const load = await screen.findByRole("button", { name: CONTROL_LABELS.load_older });
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    const before = new Set(list.querySelectorAll("a[href], button"));
    restoreFocus = emulateChromiumFocusFixup();
    load.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(fetchStatus()).toBe("fetching"));
    expect(
      document.activeElement,
      "Load older left the focus on the page while its page was read"
    ).toBe(load);
    expect(load.getAttribute("aria-busy")).toBe("true");

    older.resolve([...secondPage]);
    await waitFor(() => expect(fetchStatus()).toBe("idle"));
    await waitFor(() => expect(load.textContent).toBe(CONTROL_LABELS.load_older));
    expect(
      screen.getByRole("button", { name: CONTROL_LABELS.load_older }),
      "the log has older pages after the second, so Load older stays"
    ).toBe(load);
    const active = document.activeElement;
    expect(
      active !== null && list.contains(active) && !before.has(active),
      "Load older kept the focus once its page landed, rather than handing it to a control that page added"
    ).toBe(true);
  } finally {
    restoreFocus?.();
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// A second press of a history Retry or of Load older made before React draws the read the first
// one sent found the render's `isFetching` still false, so it read again: a refetch, or a read of
// the next page, of a log that holds pages ends the request that is out and sends another. A
// press while the read is out joins it, and keeps the hold the first press took, so the focus
// goes where one press hands it.
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
    await act(async () => {
      await drawn.queryClient.refetchQueries({ queryKey: ["events", KEY] });
    });
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

test("two presses of Load older before a render read its page once, and the focus still goes to the page it read", async () => {
  const firstPage = Array.from({ length: 200 }, (_, index) => claimAgo(600 - index, index + 1));
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
    const load = await screen.findByRole("button", { name: CONTROL_LABELS.load_older });
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    const before = new Set(list.querySelectorAll("a[href], button"));
    act(() => load.focus());

    // Both presses in one task inside one `act`, so React draws nothing between them.
    act(() => {
      load.click();
      load.click();
    });
    expect(
      olderReads(),
      "the second press ended the read of the page the first one sent and sent another"
    ).toBe(1);

    await act(async () => older.resolve([...secondPage]));
    await waitFor(() => expect(fetchStatus()).toBe("idle"));
    await waitFor(() => expect(load.textContent).toBe(CONTROL_LABELS.load_older));
    expect(olderReads()).toBe(1);
    const active = document.activeElement;
    expect(
      active !== null && list.contains(active) && !before.has(active),
      "Load older pressed twice kept the focus once its page landed, rather than handing it to a control that page added"
    ).toBe(true);
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

test("two presses of the next page's Retry before a render read that page once, and the focus still goes to the page it read", async () => {
  const firstPage = Array.from({ length: 200 }, (_, index) => claimAgo(400 - index, index + 1));
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
    const load = await screen.findByRole("button", { name: CONTROL_LABELS.load_older });
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    act(() => load.focus());
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
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: CONTROL_LABELS.load_older })).toBeNull()
    );
    expect(olderReads()).toBe(sent + 1);
    const active = document.activeElement;
    expect(
      active !== null && list.contains(active) && !before.has(active),
      "the next page's Retry pressed twice went once its page landed, and the focus did not go to the page it read"
    ).toBe(true);
  } finally {
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// A live event refetches the log, often on a busy issue. A press of Load older made while that
// refetch was out joined it, since `cancelRefetch: false` joins whatever read of the log is out,
// and read no page. Only the reader's own read of the next page takes a press made meanwhile, so
// a press while a refetch is out reads the next page.
test("Load older pressed while a refetch of the log is out reads the next page", async () => {
  const firstPage = Array.from({ length: 200 }, (_, index) => claimAgo(600 - index, index + 1));
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
    const load = await screen.findByRole("button", { name: CONTROL_LABELS.load_older });
    readFirst = () => refetched.promise;
    act(() => {
      void drawn.queryClient.invalidateQueries({ queryKey: ["events", KEY] });
    });
    await waitFor(() => expect(fetchStatus()).toBe("fetching"));

    await userEvent.click(load);
    expect(olderReads(), "Load older pressed while a refetch of the log was out read no page").toBe(
      1
    );
    await waitFor(() =>
      expect(
        document.querySelector('[data-event-seq="201"]'),
        "the page Load older read did not land"
      ).not.toBeNull()
    );
  } finally {
    refetched.resolve([...firstPage]);
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

// The first page holds 199 claims of today and 1 of yesterday, and the last page 2 more claims
// of yesterday. They go into yesterday's day, drawn folded, so that page adds no control, and the
// log's end takes Load older away: the focus goes to the last control drawn before the press,
// which stood just above Load older, rather than to the history's heading above the whole list,
// off screen for a reader at its foot.
test("Load older whose last page adds no control hands the focus to the control above where it stood", async () => {
  // A claim at sequence `seq`, `seconds` before noon of yesterday in the reader's own time.
  const yesterday = (seq: number, seconds: number): DispatchEvent => {
    const noon = new Date();
    noon.setHours(12, 0, 0, 0);
    noon.setDate(noon.getDate() - 1);
    return { ...claim(seq), created_at: new Date(noon.getTime() - seconds * 1000).toISOString() };
  };
  const firstPage = [
    ...Array.from({ length: 199 }, (_, index) => claimAgo(400 - index, index + 1)),
    yesterday(201, 0),
  ];
  const lastPage = [yesterday(10, 1), yesterday(9, 2)];
  const reads = [
    spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) =>
      options?.before === undefined ? [...firstPage] : [...lastPage]
    ),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
  ];
  const yesterdayDate = dateKey(yesterday(0, 0).created_at);
  let unmount: (() => void) | undefined;
  let restoreFocus: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined).unmount;
    const load = await screen.findByRole("button", { name: CONTROL_LABELS.load_older });
    const list = screen.getByRole("list", { name: CONTROL_LABELS.history_list });
    const line = await waitFor(() => {
      const element = list.querySelector<HTMLElement>(
        `[data-history-day="${yesterdayDate}"] > button`
      );
      if (element === null) throw new Error("yesterday's day draws no line");
      return element;
    });
    const before = [...list.querySelectorAll("a[href], button")];
    expect(before.at(-1), "yesterday's line is not the last control above Load older").toBe(line);
    restoreFocus = emulateChromiumFocusFixup();
    load.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(load.isConnected).toBe(false));
    expect(line.textContent, "the last page's claims are not in yesterday's day").toBe(
      `${dayLabel(yesterdayDate, dateKey(new Date().toISOString()))} · 3`
    );
    expect(
      list.querySelectorAll("a[href], button"),
      "the last page added a control, so the row proves nothing"
    ).toHaveLength(before.length);

    const active = document.activeElement;
    expect(
      active,
      `the log's end took Load older away and the focus left the foot of the list (focus: ${active?.tagName} ${active?.textContent})`
    ).toBe(line);
  } finally {
    restoreFocus?.();
    unmount?.();
    for (const read of reads) read.mockRestore();
  }
});

/** How tall the box the history scrolls in is: Details' view. */
const VIEW_HEIGHT = 600;

// Load older hands the focus on with no scroll, and `ViewportAnchor` took the reader's place
// only from what the reader did - the pointer over the rows, or a scroll they made - so it held
// no entry then: long agents' turns just above Load older, drawn whole until they near the
// screen, are cut once the page has landed, by commits of their own, and the control the focus
// was handed to went up with them out of Details' view. The anchor now records the reader's
// place at each focus that moves into it, so it holds the entry the hand-on put the focus in and
// keeps it in place as the entries above it change height. The browser row "Load older pressed
// as the focus reaches it keeps the focus in view while the turns above it are cut"
// (`e2e/issue-page-m.e2e.ts`) draws the real turns.
test("the entry Load older hands the focus to keeps its place in Details' view as the entries above it are cut", async () => {
  const firstPage = Array.from({ length: 200 }, (_, index) => claimAgo(400 - index, index + 1));
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
  // The first page's two oldest entries, just above Load older, are a thousand pixels tall, as
  // long turns are drawn whole, until they are cut.
  const heights = new Map([
    ["event:202", 1000],
    ["event:201", 1000],
  ]);
  const rows = () => [...box.querySelectorAll<HTMLElement>("[data-event-seq]")];
  const heightOf = (row: Element) =>
    heights.get(row.getAttribute("data-history-entry") ?? "") ?? ROW_HEIGHT;
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
  // Every entry stacks from `LIST_TOP` in DOM order, moved up by the box's scroll; a control
  // measures as the entry it is in, and anything else as the whole list.
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this === box) return rectOf(0, VIEW_HEIGHT);
    const row = this.closest("[data-event-seq]");
    const start = LIST_TOP - box.scrollTop;
    let top = start;
    for (const one of rows()) {
      if (one === row) return rectOf(top, heightOf(one));
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
  // The anchor hears a change of height outside its commits from a resize observer of its root,
  // which happy-dom never calls back: the case calls it back once the entries are cut.
  const observing = new Set<{ readonly callback: ResizeObserverCallback; observer: unknown }>();
  const RealResizeObserver = globalThis.ResizeObserver;
  globalThis.ResizeObserver = class {
    readonly #held: { readonly callback: ResizeObserverCallback; observer: unknown };
    constructor(callback: ResizeObserverCallback) {
      this.#held = { callback, observer: this };
    }
    observe() {
      observing.add(this.#held);
    }
    unobserve() {}
    disconnect() {
      observing.delete(this.#held);
    }
  } as unknown as typeof ResizeObserver;
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined, box).unmount;
    const older = await screen.findByRole("button", { name: CONTROL_LABELS.load_older });
    await waitFor(() => expect(rows()).toHaveLength(200));
    // Details scrolled down to Load older: the first entry the older page adds lands in the
    // middle of the view.
    box.scrollTop = LIST_TOP + 198 * ROW_HEIGHT + 2 * 1000 - VIEW_HEIGHT / 2;
    older.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: CONTROL_LABELS.load_older })).toBeNull()
    );
    const entry = document.activeElement?.closest<HTMLElement>("[data-history-entry]");
    if (entry === null || entry === undefined) {
      throw new Error("Load older handed the focus to no entry of the history");
    }
    expect(
      entry.getAttribute("data-history-entry"),
      "Load older handed the focus to an entry other than the first the page it read added"
    ).toBe("event:10");
    const handed = entry.getBoundingClientRect().top;
    expect(handed, "the entry Load older handed the focus to is not in Details' view").toBe(
      VIEW_HEIGHT / 2
    );

    // Each long entry is cut to six lines by a commit of its own, which the anchor sees only as
    // its root resized.
    heights.set("event:202", 120);
    heights.set("event:201", 120);
    act(() => {
      for (const { callback, observer } of observing) callback([], observer as ResizeObserver);
    });

    expect(
      entry.getBoundingClientRect().top,
      "the entries above the focus were cut and moved the entry Load older handed it to out of Details' view"
    ).toBe(handed);
  } finally {
    unmount?.();
    globalThis.ResizeObserver = RealResizeObserver;
    rect.mockRestore();
    point.mockRestore();
    box.remove();
    for (const read of reads) read.mockRestore();
  }
});

// Load older pressed with the focus on it: the focus stays on Load older, outside the entries,
// while the page is read, so no act of the reader's names a place in the history. Here the commit
// the page lands in also cuts the long turn just above the list's last entry, as a turn drawn
// whole is cut to six lines once it shows, which takes every entry under that turn, the page's
// new ones with them, up by the cut. The anchor held the entry nearest the middle of Details'
// view, the long turn itself, whose top the cut leaves where it was: the page's entries went up
// out of the view, and the hand-on gave the focus to one above it (seen at -639 px in the
// browser). The history now keeps the place of the entry next to Load older from the press
// until the page's read has settled.
test("Load older keeps the place of the entry above it from the press, as its page lands with a cut of the turn above that entry", async () => {
  const firstPage = Array.from({ length: 200 }, (_, index) => claimAgo(400 - index, index + 1));
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
  // The first page's entry above its last is a long turn, a thousand pixels drawn whole, which
  // the commit that draws the older page's first entry cuts to 120 px.
  const heightOf = (row: Element) => {
    if (row.getAttribute("data-history-entry") !== "event:202") return ROW_HEIGHT;
    return box.querySelector('[data-history-entry="event:10"]') === null ? 1000 : 120;
  };
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
  // Every entry stacks from `LIST_TOP` in DOM order, moved up by the box's scroll; a control
  // measures as the entry it is in, and anything else as the whole list.
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this === box) return rectOf(0, VIEW_HEIGHT);
    const row = this.closest("[data-event-seq]");
    const start = LIST_TOP - box.scrollTop;
    let top = start;
    for (const one of rows()) {
      if (one === row) return rectOf(top, heightOf(one));
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
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHistory(undefined, box).unmount;
    const older = await screen.findByRole("button", { name: CONTROL_LABELS.load_older });
    await waitFor(() => expect(rows()).toHaveLength(200));
    const last = box.querySelector<HTMLElement>('[data-history-entry="event:201"]');
    if (last === null) throw new Error("the first page's last entry is not drawn");
    // Details scrolled down to Load older: the first page's last entry stands 400 px down the
    // view, under the long turn, which holds the middle of the view.
    box.scrollTop = LIST_TOP + 198 * ROW_HEIGHT + 1000 - 400;
    const stood = last.getBoundingClientRect().top;
    expect(stood).toBe(400);
    older.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: CONTROL_LABELS.load_older })).toBeNull()
    );

    expect(
      last.getBoundingClientRect().top,
      "the page landed with a cut of the turn above the entry next to Load older, and that entry moved in Details' view"
    ).toBe(stood);
    const focused = document.activeElement;
    const entry = focused?.closest<HTMLElement>("[data-history-entry]");
    if (focused === null || entry === null || entry === undefined) {
      throw new Error("Load older handed the focus to no entry of the history");
    }
    expect(
      entry.getAttribute("data-history-entry"),
      "Load older handed the focus to an entry other than the first the page it read added"
    ).toBe("event:10");
    const at = focused.getBoundingClientRect();
    expect(
      at.top >= 0 && at.bottom <= VIEW_HEIGHT,
      `Load older handed the focus to a control out of Details' view (${at.top} px, the view 0 to ${VIEW_HEIGHT})`
    ).toBe(true);
    expect(
      at.top >= last.getBoundingClientRect().bottom,
      "Load older handed the focus to a control above the entry next to it"
    ).toBe(true);
  } finally {
    unmount?.();
    rect.mockRestore();
    point.mockRestore();
    box.remove();
    for (const read of reads) read.mockRestore();
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

/** A handoff's line's disclosure in the history, with nothing above it but a query client that
 *  never retries. */
function drawHandoff(): { queryClient: QueryClient; unmount: () => void } {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const { unmount } = render(
    <QueryClientProvider client={queryClient}>
      <HistoryHandoff handoffId="handoff-1" />
    </QueryClientProvider>
  );
  return { queryClient, unmount };
}

// A handoff's opened fields hold its body whatever the last read's outcome, as the history holds
// its log: an invalidation of every read after a write reads the body again, and a read that
// fails then says so with its Retry above the fields, never in their place.
test("a handoff's opened fields stay drawn when a read of them again fails", async () => {
  const getHandoff = spyOn(api, "getHandoff").mockResolvedValue(handoffRecord("Second pass"));
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHandoff();
    unmount = drawn.unmount;
    await userEvent.click(screen.getByRole("button", { name: CONTROL_LABELS.show_fields }));
    await screen.findByText("Second pass");

    getHandoff.mockRejectedValue(new Error("the handoff is down"));
    await act(async () => {
      await drawn.queryClient.invalidateQueries();
    });
    expect(getHandoff).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("alert")).toHaveTextContent(ANSWER_LINES.handoff_unreadable);
    expect(screen.getByRole("button", { name: CONTROL_LABELS.retry })).toBeInTheDocument();
    expect(
      screen.queryByText("Second pass"),
      "a failed read again of the handoff erased the fields it held"
    ).not.toBeNull();
  } finally {
    unmount?.();
    getHandoff.mockRestore();
  }
});

// With no body held, the read a handoff's Retry sends is pending, and the loading line takes the
// place of the line that said the read failed, Retry and all. The focus of a keyboard reader on
// that Retry goes with its line to the loading line, and on to the fields once the read lands,
// rather than falling to the page with the Retry.
test("a handoff's Retry pressed by keyboard hands the focus on as its line goes, to the fields it read", async () => {
  const getHandoff = spyOn(api, "getHandoff")
    .mockRejectedValueOnce(new Error("the handoff is down"))
    .mockResolvedValue(handoffRecord("Second pass"));
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHandoff().unmount;
    const toggle = screen.getByRole("button", { name: CONTROL_LABELS.show_fields });
    toggle.focus();
    await userEvent.keyboard("{Enter}");
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

// Where a body is held, the line that says a read of it failed stays, its Retry busy, while the
// read that Retry sent is out. A press of the busy Retry joins that read rather than ending it
// and sending another (`cancelRefetch: false`). A press moves no focus: a reader who presses the
// busy Retry, or comes back to it and presses it again, keeps the focus there.
test("a handoff's busy Retry sends no read while the one it sent is out, and keeps the focus", async () => {
  const getHandoff = spyOn(api, "getHandoff").mockResolvedValue(handoffRecord("Second pass"));
  // The read the Retry sends is held out until the case lets it land.
  const again = Promise.withResolvers<Handoff>();
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHandoff();
    unmount = drawn.unmount;
    await userEvent.click(screen.getByRole("button", { name: CONTROL_LABELS.show_fields }));
    await screen.findByText("Second pass");
    getHandoff.mockRejectedValueOnce(new Error("the handoff is down"));
    await act(async () => {
      await drawn.queryClient.invalidateQueries();
    });
    expect(getHandoff).toHaveBeenCalledTimes(2);
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
    getHandoff.mockImplementation(() => again.promise);

    for (let press = 0; press < 3; press += 1) await userEvent.click(retry);

    expect(
      getHandoff,
      "each press of the busy Retry sent another read while one was out"
    ).toHaveBeenCalledTimes(3);
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

// A second press of a handoff's Retry made before React draws the read the first one sent found
// the render's `isFetching` still false, so it read the body again: a refetch of a read that
// holds a body ends the request that is out and sends another. A press while the read is out
// joins it, and moves no focus.
test("two presses of a handoff's Retry before a render send one read", async () => {
  const getHandoff = spyOn(api, "getHandoff").mockResolvedValue(handoffRecord("Second pass"));
  // Every read sent from here is held out until the case lets it land.
  const again = Promise.withResolvers<Handoff>();
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHandoff();
    unmount = drawn.unmount;
    const toggle = screen.getByRole("button", { name: CONTROL_LABELS.show_fields });
    await userEvent.click(toggle);
    await screen.findByText("Second pass");
    getHandoff.mockRejectedValueOnce(new Error("the handoff is down"));
    await act(async () => {
      await drawn.queryClient.invalidateQueries();
    });
    expect(getHandoff).toHaveBeenCalledTimes(2);
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
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
    ).toHaveBeenCalledTimes(3);
    expect(
      document.activeElement,
      "the press that sent the read moved the focus off the Retry, whose line stays while the read is out"
    ).toBe(retry);

    await act(async () => again.resolve(handoffRecord("Third pass")));
    await screen.findByText("Third pass");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(getHandoff).toHaveBeenCalledTimes(3);
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
    const toggle = screen.getByRole("button", { name: CONTROL_LABELS.show_fields });
    toggle.focus();
    await userEvent.keyboard("{Enter}");
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    getHandoff.mockImplementation(() => again.promise);
    retry.focus();
    await userEvent.keyboard("{Enter}{Enter}");
    await act(async () => again.resolve(handoffRecord("Second pass")));

    expect(
      toggle,
      "the second press of Retry landed on the toggle and shut the fields it asked for"
    ).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("Second pass")).toBeInTheDocument();
    expect(getHandoff, "the two presses read the handoff more than once").toHaveBeenCalledTimes(2);
  } finally {
    unmount?.();
    getHandoff.mockRestore();
  }
});

// With no body held, the loading line takes the place of the line that said the read failed, so
// the focus of a keyboard reader on its Retry goes to that line, where an Enter presses nothing
// and the fields stay open; then to the Retry of the line drawn again where the read fails again,
// and to the fields once a read lands.
test("a handoff's Retry with no body held hands the focus to the loading line, then to what the read drew", async () => {
  const getHandoff = spyOn(api, "getHandoff").mockRejectedValueOnce(
    new Error("the handoff is down")
  );
  // Each read a Retry sends is held out until the case lets it land.
  let again = Promise.withResolvers<Handoff>();
  let unmount: (() => void) | undefined;

  try {
    unmount = drawHandoff().unmount;
    const toggle = screen.getByRole("button", { name: CONTROL_LABELS.show_fields });
    toggle.focus();
    await userEvent.keyboard("{Enter}");
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    getHandoff.mockImplementation(() => again.promise);
    retry.focus();
    await userEvent.keyboard("{Enter}");
    const loading = await screen.findByText(ANSWER_LINES.loading_handoff);
    expect(
      document.activeElement,
      `the press of Retry did not hand the focus to the loading line in its line's place (focus: ${document.activeElement?.tagName} ${document.activeElement?.textContent})`
    ).toBe(loading);
    await userEvent.keyboard("{Enter}");
    expect(toggle, "an Enter on the loading line shut the fields").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    expect(document.activeElement, "an Enter on the loading line moved the focus").toBe(loading);

    await act(async () => again.reject(new Error("the handoff is still down")));
    const drawnAgain = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    expect(
      document.activeElement,
      "the loading line went with the focus and did not hand it to the Retry of the read that failed again"
    ).toBe(drawnAgain);

    again = Promise.withResolvers<Handoff>();
    await userEvent.keyboard("{Enter}");
    expect(document.activeElement).toBe(await screen.findByText(ANSWER_LINES.loading_handoff));
    await act(async () => again.resolve(handoffRecord("Second pass")));
    await screen.findByText("Second pass");
    const fields = document.getElementById(toggle.getAttribute("aria-controls") ?? "");
    expect(fields?.tagName).toBe("DL");
    expect(
      document.activeElement,
      "the loading line went with the focus and did not hand it to the fields the read drew"
    ).toBe(fields);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(getHandoff).toHaveBeenCalledTimes(3);
  } finally {
    unmount?.();
    getHandoff.mockRestore();
  }
});

// Where a body is held, the line that says a read of it failed stays while the read its Retry
// sent is out, so the focus of a keyboard reader stays on the busy Retry, where a second Enter
// joins that read and the fields stay open; once the read lands, the line goes and hands the
// focus to the fields it drew.
test("a handoff's Retry with a body held keeps the focus while its read is out, then hands it to the fields", async () => {
  const getHandoff = spyOn(api, "getHandoff").mockResolvedValue(handoffRecord("Second pass"));
  // The read the Retry sends is held out until the case lets it land.
  const again = Promise.withResolvers<Handoff>();
  let unmount: (() => void) | undefined;

  try {
    const drawn = drawHandoff();
    unmount = drawn.unmount;
    const toggle = screen.getByRole("button", { name: CONTROL_LABELS.show_fields });
    await userEvent.click(toggle);
    await screen.findByText("Second pass");
    getHandoff.mockRejectedValueOnce(new Error("the handoff is down"));
    await act(async () => {
      await drawn.queryClient.invalidateQueries();
    });
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
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
    expect(getHandoff, "the second press sent another read").toHaveBeenCalledTimes(3);

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
