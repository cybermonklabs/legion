/**
 * What the unit tests of Threads' records and addresses share (`ThreadRecord.test.tsx`,
 * `draft-matrix.threads.test.tsx`, `ThreadRow.test.tsx`, `NewThread.test.tsx`,
 * `ThreadsView.test.tsx`, `ThreadsView.addresses.test.tsx`, `ThreadsView.live.test.tsx`,
 * `../address-target.test.tsx`): the issue the captured fixtures were read from
 * (`fixtures/README.md`), the server's answers to every read Threads sends, Threads drawn at an
 * address under what the app and the page provide, with the router's
 * address and its Back, and a window whose width a case sets.
 */
import { spyOn } from "bun:test";
import type { QueryClient } from "@tanstack/react-query";
import { act, render } from "@testing-library/react";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, type NavigateFunction, useLocation, useNavigate } from "react-router-dom";

import { newQueryClient, signedInAsReader } from "../../../__tests__/draft-matrix-fixture";
import { answerRequests } from "../../../__tests__/network";
import { issueCollaborators } from "../../../__tests__/search-fixture";
import { api } from "../../../api/client";
import type { PublishedReferences } from "../../refs/reference-names";
import { parseIssuePath } from "../../refs/routes";
import { useKeymapScope } from "../../shell/keymap";
import { issueDetails } from "../decisions-filter/decisions-fixtures";
import { NoTooltips } from "../NoTooltips";
import { Publisher } from "../projection-fixture";
import { AppProviders } from "../slots/stage-fixtures";
import asksThreadBody from "./fixtures/asks-thread.json";
import commentsInBody from "./fixtures/comments-in.json";
import commentsInBeforeBody from "./fixtures/comments-in-before.json";
import eventsBody from "./fixtures/events.json";
import meStateBody from "./fixtures/me-state.json";
import messagesInBody from "./fixtures/messages-in.json";
import messagesNoneAuthorBody from "./fixtures/messages-none-author.json";
import repliesLimitBody from "./fixtures/replies-limit-200.json";
import repliesUnreadBody from "./fixtures/replies-unread.json";
import { ThreadsView } from "./ThreadsView";

/** The seeded issue the fixtures were read from, and its project. */
export const KEY = "PDFD720CB-1";
export const ISSUE = { ...issueDetails(), key: KEY, project: "PDFD720CB" };

/** The address of Threads, and of the record `id`. */
export function threadsAt(id?: string): string {
  return id === undefined ? `/issues/${KEY}/threads` : `/issues/${KEY}/threads/${id}`;
}

/** What a case answers a route of All with in place of its captured first page. */
export type PageAnswers = Partial<Record<"asks" | "comments" | "messages", () => unknown>>;

/** How many reads of each route the page sent: All's three, Agents', and the reader's unread
 *  threads with no limit (Waiting on me's) and with `limit=200` (the read mark's). */
export interface SentReads {
  agents: number;
  asks: number;
  comments: number;
  messages: number;
  unread: number;
  limited: number;
}

/** Answers every read Threads sends with the server's captured answers, a route of All its own
 *  way where `pages` names one, and counts them. */
export function answerThreads(pages: PageAnswers = {}): SentReads {
  const sent: SentReads = { agents: 0, asks: 0, comments: 0, limited: 0, messages: 0, unread: 0 };
  answerRequests(
    http.get("/api/v1/issues/:key/comments", ({ request }) => {
      sent.comments += 1;
      if (new URL(request.url).searchParams.has("before")) {
        return Response.json(commentsInBeforeBody);
      }
      return Response.json(pages.comments?.() ?? commentsInBody);
    }),
    http.get("/api/v1/issues/:key/messages", ({ request }) => {
      if (new URL(request.url).searchParams.get("person") === "none") {
        sent.agents += 1;
        return Response.json(messagesNoneAuthorBody);
      }
      sent.messages += 1;
      return Response.json(pages.messages?.() ?? messagesInBody);
    }),
    http.get("/api/v1/issues/:key/asks", () => {
      sent.asks += 1;
      return Response.json(pages.asks?.() ?? asksThreadBody);
    }),
    http.get("/api/v1/issues/:key/events", () => Response.json(eventsBody)),
    http.get("/api/v1/me/replies", ({ request }) => {
      if (new URL(request.url).searchParams.has("limit")) {
        sent.limited += 1;
        return Response.json(repliesLimitBody);
      }
      sent.unread += 1;
      return Response.json(repliesUnreadBody);
    }),
    http.get("/api/v1/me/state", () => Response.json(meStateBody)),
    // An edit reads its comment's blocks as it opens. A server with no blocks route refuses the
    // read, and the edit then carries no block ids (`MentionComposer`).
    http.get("/api/v1/comments/:id/blocks", () => new Response(null, { status: 404 })),
    // New thread's chooser reads the issue's collaborators as it opens, as the `@` picker does.
    // No capture holds that read, so it answers as every picker's test does: no agent on the
    // issue (`search-fixture.ts`). A case about the chooser's parties answers it its own way.
    http.get("/api/v1/issues/:key/collaborators", () =>
      Response.json(issueCollaborators({ issue_key: KEY }))
    )
  );
  return sent;
}

/** What the router holds as a case reads it: its own navigation, which a case moves the address
 *  with as the app does, and the address it holds now. */
interface Held {
  navigate?: NavigateFunction;
  pathname?: string;
}

/** Threads at the address the router holds, as the stage draws it, with the router's navigation
 *  and address handed out in `held`. */
function AtAddress({ held }: { held: Held }): ReactNode {
  // The page's keys stand in its scope, which the page pushes (`useIssueKeymap`).
  useKeymapScope("issue");
  const { pathname, search } = useLocation();
  held.navigate = useNavigate();
  held.pathname = pathname;
  const route = parseIssuePath(pathname, search);
  return route === undefined ? null : <ThreadsView issue={ISSUE} route={route} />;
}

/** Threads drawn at `path`, signed in as the reader the fixtures were read for, with `published`
 *  the names the page published of what the texts cite. */
export function drawThreads(
  path: string,
  {
    client = signedInAsReader(newQueryClient()),
    published,
  }: { readonly client?: QueryClient; readonly published?: PublishedReferences } = {}
) {
  const spies = [
    spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" } as never),
    spyOn(api, "listAgents").mockResolvedValue([]),
  ];
  const held: Held = {};
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <AppProviders client={client}>
        {published === undefined ? null : <Publisher published={published} />}
        <NoTooltips>
          <AtAddress held={held} />
        </NoTooltips>
      </AppProviders>
    </MemoryRouter>
  );
  return {
    /** The address the router holds now. */
    address(): string | undefined {
      return held.pathname;
    },
    /** Returns to the entry before, as the browser's Back does. */
    back(): void {
      act(() => {
        void held.navigate?.(-1);
      });
    },
    client,
    /** Moves the address to `to`, as a link the reader follows does. */
    go(to: string): void {
      act(() => {
        void held.navigate?.(to);
      });
    },
    view,
    done(): void {
      view.unmount();
      for (const spy of spies) spy.mockRestore();
    },
  };
}

/** Whether a window `width` px wide matches `query`, one `(min-width: …)` or `(max-width: …)`
 *  clause in `px` or `rem`, or `not all and` one, as the page's stage asks; any other query
 *  throws, naming it. */
function widthMatches(width: number, query: string): boolean {
  const negated = /^not all and (.+)$/.exec(query.trim());
  if (negated !== null) return !widthMatches(width, negated[1] ?? "");
  const clause = /^\((min|max)-width:\s*([\d.]+)(px|rem)\)$/.exec(query.trim());
  if (clause === null) throw new Error(`The window's stub answers no media query "${query}".`);
  const px = Number(clause[2]) * (clause[3] === "rem" ? 16 : 1);
  return clause[1] === "min" ? width >= px : width <= px;
}

/** A window `width` px wide that a case can narrow and widen, as a resize does. */
export function stubWindow(width: number) {
  const original = window.matchMedia;
  let current = width;
  const heard = new Set<() => void>();
  window.matchMedia = ((query: string) => ({
    addEventListener: (_type: string, listener: () => void) => heard.add(listener),
    addListener: () => {},
    dispatchEvent: () => true,
    get matches() {
      return widthMatches(current, query);
    },
    media: query,
    onchange: null,
    removeEventListener: (_type: string, listener: () => void) => heard.delete(listener),
    removeListener: () => {},
  })) as unknown as typeof window.matchMedia;
  return {
    resize(next: number): void {
      current = next;
      act(() => {
        for (const listener of heard) listener();
      });
    },
    restore(): void {
      window.matchMedia = original;
    },
  };
}
