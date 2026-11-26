/**
 * What `Inbox.test.tsx` and `Inbox.reading-view.test.tsx` both render and read the Inbox page
 * with: the page under a history the test chooses, the rows it lists, the address it writes, and
 * a window width the test can move while it is mounted.
 */
import { spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen } from "@testing-library/react";
import { type ComponentProps, type ReactNode, useState } from "react";
import { MemoryRouter, useNavigate } from "react-router-dom";

import type { Commits } from "../../__tests__/commits";
import { api } from "../../api/client";
import type { InboxRow, Issue, IssueDetails, MyRepliesResponse, MyReplyRow } from "../../api/types";
import { usePageAddress } from "../../lib/page-address";
import { DocumentRuntime, type DocumentRuntimeValue } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { KeymapProvider } from "../shell/KeymapProvider";
import { DIALOG_SCOPE } from "../shell/keymap";
import { READING_PANE_QUERY } from "../shell/useDialog";
import { Inbox } from "./Inbox";
import { issueAsk } from "./inbox-fixture";
import { type InboxRoute, parseInboxSearch } from "./inbox-route";

export function inboxIssue(
  id: string,
  key: string,
  question: string,
  overrides: Partial<InboxRow> = {}
): InboxRow {
  return issueAsk({
    id,
    issue: { assignee: "alice", key, title: `${key} title` },
    issue_key: key,
    question,
    ...overrides,
  });
}

/** The narrow `Issue` a PATCH answers with, for an inbox row's issue. */
export function narrowIssueOf(row: InboxRow, assignee: string | null): Issue {
  return {
    advances: [],
    advances_revision: 0,
    assignee,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    closed_at: null,
    created_at: row.created_at,
    created_by: { id: "alice", kind: "user" },
    external_links: [],
    key: row.issue_key ?? "",
    labels: [],
    last_seq: 2,
    number: 3,
    parent: null,
    blocked_by: [],
    plan_artifact_id: "artifact-plan",
    decisions_artifact_id: "",
    primary_artifact_id: "artifact-3",
    priority: null,
    project: "CORE",
    rank: "U",
    route: null,
    repo: null,
    resolved_repo: null,
    status: "todo",
    title: row.issue?.title ?? "",
    updated_at: row.created_at,
    verification_artifact_id: "artifact-verification",
  };
}

/** The address the Inbox is on, and a way back through the history it has written. */
export function RouteProbe() {
  const address = usePageAddress();
  const navigate = useNavigate();
  return (
    <>
      <output data-testid="location">{`${address.pathname}${address.search}`}</output>
      <button onClick={() => navigate(-1)} type="button">
        History back
      </button>
    </>
  );
}

/** One entry of the history `renderInbox` starts from: a path, or a location with its state. */
type InitialEntry = NonNullable<ComponentProps<typeof MemoryRouter>["initialEntries"]>[number];

/** The list of cards as the Peek drawer mounts it (`InboxDrawer`): a filter the host holds
 *  itself, starting from the address the test opens on and never written back to it, every
 *  narrowing reported to that host, and the keys on the dialog's scope. */
function DrawerHostedInbox(): ReactNode {
  const { search } = usePageAddress();
  const [filter, setFilter] = useState<InboxRoute>(() => parseInboxSearch(search));
  return (
    <Inbox filter={filter} keymapScope={DIALOG_SCOPE} onFilterChange={setFilter} pane={false} />
  );
}

/** `route` is the whole history when it is a list, the last entry being where the reader is:
 *  whether a filter change replaces its entry or pushes one is only visible from a history that
 *  has something underneath it. An entry may carry the state its history entry holds, as one the
 *  reader reloads onto does. `runtime` is for the tests whose reading column opens a
 *  document: that column mounts `ProofDocument`, which wants the document runtime and the
 *  margin the app shell gives it, and every other test's column shows a card and needs
 *  neither. `commits` records each commit of the Inbox; it has no one value a host passes, so
 *  each commit's `host` is null. `pane: false` is the list of cards, mounted as the Peek drawer
 *  mounts it (`DrawerHostedInbox`): the one screen that draws a question's card in its row, where
 *  it is answered, snoozed and assigned. The page reads one row at a time at every width, and the
 *  rows that drive a card in its row drive that list. */
export function renderInbox(
  route: InitialEntry | readonly InitialEntry[] = "/",
  {
    commits,
    pane = true,
    runtime,
  }: { commits?: Commits<null>; pane?: boolean; runtime?: DocumentRuntimeValue } = {}
) {
  const entries = Array.isArray(route) ? [...route] : [route as InitialEntry];
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const list = pane ? <Inbox /> : <DrawerHostedInbox />;
  const inbox = (
    <KeymapProvider>
      <RouteProbe />
      {commits === undefined ? list : <commits.Recorded host={null}>{list}</commits.Recorded>}
    </KeymapProvider>
  );
  const view = render(
    <MemoryRouter initialEntries={entries} initialIndex={entries.length - 1}>
      <QueryClientProvider client={queryClient}>
        {runtime === undefined ? (
          inbox
        ) : (
          <DocumentRuntime.Provider value={runtime}>
            <MarginProvider>{inbox}</MarginProvider>
          </DocumentRuntime.Provider>
        )}
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, unmount: view.unmount };
}

/** The list's band headings, in order. The reading column's own heading - the title of the item
 *  it shows - is none of them. */
export function headings(): string[] {
  return screen
    .getAllByRole("heading", { level: 2 })
    .filter((heading) => heading.closest('[data-testid="inbox-pane"]') === null)
    .map((heading) => heading.textContent ?? "");
}

/** The rows' ask ids in DOM order: the order the reader sees. */
export function rowIds(): string[] {
  return [...document.querySelectorAll("[data-inbox-row]")].map(
    (row) => row.getAttribute("data-inbox-row") ?? ""
  );
}

/** An hour from now, as the server stores it: inside any snooze window the reader can pick. */
export function soon(hours = 3): string {
  return new Date(Date.now() + hours * 60 * 60 * 1000).toISOString();
}

/** A width the test can move while the Inbox is mounted, which `widenToReadingPane` cannot:
 *  happy-dom answers `matchMedia` from its own window, but it delivers no `change` to a list
 *  made before `setViewport`, so a column already standing never hears the viewport narrow.
 *  This is the stub `__tests__/responsive-shell.test.tsx` installs - one object per query,
 *  answering `matches` for the query it was asked - with the one addition a move needs: it keeps
 *  the listeners `useMediaQuery` registers, so `setWidth` delivers the `change` a browser sends.
 */
export function movableReadingPaneWidth(width: number): {
  restore: () => void;
  setWidth: (next: number) => void;
} {
  const originalMatchMedia = window.matchMedia;
  const changeListeners = new Set<(event: Event) => void>();
  let now = width;
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: (type: string, listener: EventListenerOrEventListenerObject | null) => {
        if (type === "change" && typeof listener === "function") changeListeners.add(listener);
      },
      addListener: () => {},
      dispatchEvent: () => true,
      get matches() {
        return query === READING_PANE_QUERY && now >= 1280;
      },
      media: query,
      onchange: null,
      removeEventListener: (type: string, listener: EventListenerOrEventListenerObject | null) => {
        if (type === "change" && typeof listener === "function") changeListeners.delete(listener);
      },
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  return {
    restore: () => {
      window.matchMedia = originalMatchMedia;
    },
    setWidth: (next: number) => {
      now = next;
      for (const listener of changeListeners) listener(new Event("change"));
    },
  };
}

/** The ask ids of the rows marked selected: exactly one, or the test has found a second. */
export function selectedIds(): string[] {
  return [...document.querySelectorAll('[data-inbox-selected="true"]')].map(
    (row) => row.getAttribute("data-inbox-row") ?? ""
  );
}

export function address(): string {
  return screen.getByTestId("location").textContent ?? "";
}

/** The pane reads one field of the question's issue: whether it is closed. */
export function mockIssueReads() {
  return spyOn(api, "getIssue").mockResolvedValue({ closed_at: null } as unknown as IssueDetails);
}

/** A read of the reader's threads as `GET /api/v1/me/replies` answers it. */
function threadsReadOf(rows: readonly MyReplyRow[]): MyRepliesResponse {
  return {
    as_of: "2026-10-12T02:00:00Z",
    limit: 0,
    offset: 0,
    rows: [...rows],
    total: rows.length,
  };
}

/** The Inbox's own thread read (`inboxThreadsQuery`), held until the test lands it: the read the
 *  page makes once the sign-in read has answered, which a reload or a link can see land after
 *  the asks'. Every other replies read - an owner's unread set - answers at once with nothing. */
export function holdThreadsRead(): {
  land: (rows: readonly MyReplyRow[]) => Promise<void>;
  restore: () => void;
} {
  const held = Promise.withResolvers<MyRepliesResponse>();
  const read = spyOn(api, "listMyReplies").mockImplementation((page) =>
    page.state === "inbox" ? held.promise : Promise.resolve(threadsReadOf([]))
  );
  return {
    land: async (rows) => {
      await act(async () => {
        held.resolve(threadsReadOf(rows));
        await held.promise;
      });
    },
    restore: () => read.mockRestore(),
  };
}
