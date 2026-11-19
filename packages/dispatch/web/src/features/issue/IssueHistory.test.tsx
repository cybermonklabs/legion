import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { prependEventToLog } from "../../api/event-feed";
import type { DispatchEvent, IssueDetails, MessageRead } from "../../api/types";
import { IssueHistory } from "./IssueHistory";

const KEY = "CORE-1";

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
// `data-event-seq`, the identity the anchor follows.
test("an event arriving while the reader is in the history leaves their entry where it was", async () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue(
    Array.from({ length: 12 }, (_, index) => claim(12 - index))
  );
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const listIssueMessages = spyOn(api, "listIssueMessages").mockResolvedValue([]);
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
            issue={issue}
            item={undefined}
            state={{ [KEY]: { dismissed: [], last_read_seq: 99, pinned: false, seq: 0 } }}
          />
        </QueryClientProvider>
      </MemoryRouter>
    ).unmount;
    await screen.findByRole("list", { name: "Issue activity" });
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
    getIssueEvents.mockRestore();
    listComments.mockRestore();
    listIssueMessages.mockRestore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
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
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue(
    Array.from({ length: 6 }, (_, index) => claim(6 - index))
  );
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const listIssueMessages = spyOn(api, "listIssueMessages").mockResolvedValue([
    agentMessage("m-older", 200),
    agentMessage("m-newer", 100),
  ]);
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
            issue={issue}
            item={undefined}
            state={{ [KEY]: { dismissed: [], last_read_seq: 99, pinned: false, seq: 0 } }}
          />
        </QueryClientProvider>
      </MemoryRouter>
    ).unmount;
    await screen.findByRole("list", { name: "Issue activity" });
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
    getIssueEvents.mockRestore();
    listComments.mockRestore();
    listIssueMessages.mockRestore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});
