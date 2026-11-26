import { afterEach, beforeAll, describe, expect, type Mock, spyOn, test } from "bun:test";
import type { MessageThreadPage } from "@legion/contracts";
import { QueryClient } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { answerRequests } from "../../__tests__/network";
import { api } from "../../api/client";
import type { MessageRead } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { buildIssuePath, parseIssuePath } from "../refs/routes";
import { KEY_REPORTS } from "./page-structure";
import { PRESET_NAMES, THREADS_WORDS } from "./page-words";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import { AppProviders, stubReads } from "./slots/stage-fixtures";
import ask404Body from "./threads/fixtures/ask-404.json";
import comment404Body from "./threads/fixtures/comment-404.json";
import messagesBareBody from "./threads/fixtures/messages-bare.json";
import messagesNoneBody from "./threads/fixtures/messages-none.json";
import { answerThreads, ISSUE, KEY, stubWindow } from "./threads/threads-view-fixture";

/**
 * Where `/issues/<KEY>/messages/<id>` opens on the new page (`useAddressFallback`,
 * dispatch://INFRA-5704/spec#b-r-addresses, R12, decision 501 (7)), over the server's answers
 * captured at the stack base (`threads/fixtures/README.md`): a message of a thread between agents
 * or of an agent's note opens in Threads with its record open and the message focused, and a
 * message of a thread a person wrote in, or of one the Legion daemon began, lands in the outline
 * as on `main`. The page's stage is drawn as the page's root draws it, with the outline's reads
 * the issue's conversation and Threads' reads Threads' own.
 */

/** The issue's conversation as the outline reads it: every root with its replies. */
const CONVERSATION = messagesBareBody as unknown as MessageRead[];

function threadOf(test: (thread: MessageRead) => boolean, what: string): MessageRead {
  const thread = CONVERSATION.find(test);
  if (thread === undefined) throw new Error(`The captured conversation holds no ${what}.`);
  return thread;
}

/** The thread an agent began to another agent's session, which only agents wrote in. */
const BETWEEN_AGENTS = threadOf(
  ({ message }) => message.author.kind === "session" && message.target !== null,
  "thread between agents"
);
/** The agent's note: a root with no target that no person answered. */
const NOTE = threadOf(
  ({ message, replies }) =>
    message.author.kind === "session" &&
    message.target === null &&
    message.author.id !== `legion-daemon:${ISSUE.project}` &&
    (replies ?? []).length === 0,
  "agent's note"
);
/** The thread the Legion daemon began, less the reader's reply in it, so only agents wrote in
 *  it, as the outline leaves out. */
const DAEMON_ALONE = ((daemon: MessageRead): MessageRead => ({
  ...daemon,
  replies: (daemon.replies ?? []).filter((reply) => reply.author.kind !== "user"),
}))(
  threadOf(
    ({ message }) => message.author.id === `legion-daemon:${ISSUE.project}`,
    "thread the Legion daemon began"
  )
);
/** A thread the reader began, which an agent answered. */
const PERSON = threadOf(
  ({ message, replies }) => message.author.kind === "user" && (replies ?? []).length > 0,
  "thread a person began"
);

/** Where the router stands, as a case reads it. */
interface Held {
  pathname?: string;
}

/** The stage a case drew: where its router stands, and how the case takes it off the page. */
interface DrawnMessage {
  readonly held: Held;
  readonly done: () => void;
}

/** The page's stage at the address the router holds, under the page's own providers, as the
 *  page's root draws it (`IssueOutlinePage.tsx`). */
function Page({ held }: { readonly held: Held }): ReactNode {
  const { pathname, search } = useLocation();
  held.pathname = pathname;
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <ProvidersSlot issue={ISSUE} route={route}>
      <StageSlot issue={ISSUE} route={route} />
    </ProvidersSlot>
  );
}

/**
 * The stage at `/messages/<id>` at 1440 px, the outline read from `conversation`, every read of
 * Threads answered, and R12's lookups answered: the message route with the thread between agents,
 * the other two with their `404`.
 */
function drawMessage(
  id: string,
  conversation: readonly MessageRead[] = CONVERSATION
): DrawnMessage {
  const window = stubWindow(1440);
  const stubs = [...stubReads(), spyOn(api, "listAgents").mockResolvedValue([])];
  (api.listIssueMessages as Mock<typeof api.listIssueMessages>).mockResolvedValue([
    ...conversation,
  ]);
  answerThreads();
  const agents = messagesNoneBody as unknown as MessageThreadPage;
  answerRequests(
    http.get("/api/v1/asks/:id", () => Response.json(ask404Body, { status: 404 })),
    http.get("/api/v1/comments/:id", () => Response.json(comment404Body, { status: 404 })),
    http.get("/api/v1/messages/:id", () =>
      Response.json({ message: BETWEEN_AGENTS.message, replies: BETWEEN_AGENTS.replies })
    ),
    http.get("/api/v1/issues/:key/messages", ({ request }) =>
      new URL(request.url).searchParams.get("person") === "none" ? Response.json(agents) : undefined
    )
  );
  const held: Held = {};
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ id, key: KEY, kind: "message" })]}>
      <AppProviders client={client}>
        <Page held={held} />
      </AppProviders>
    </MemoryRouter>
  );
  return {
    held,
    done(): void {
      view.unmount();
      for (const stub of stubs) stub.mockRestore();
      window.restore();
    },
  };
}

const threadsView = () =>
  document.querySelector(`section[aria-label="${PRESET_NAMES.threads}"]`) ?? undefined;
const pressed = (name: string) =>
  [...document.querySelectorAll("[data-thread-choices] button")].find(
    (button) => button.textContent === name
  );

let drawn: DrawnMessage | undefined;

afterEach(() => {
  drawn?.done();
  drawn = undefined;
});

beforeAll(async () => {
  await warmMarkdownRenderer();
});

describe("a message no view draws opens in Threads, its record open and the message focused", () => {
  test("a reply of a thread between agents opens its thread's record under Agents", async () => {
    const reply = BETWEEN_AGENTS.replies?.[0]?.id ?? "";
    drawn = drawMessage(reply);
    const at = drawn.held;
    await waitFor(() =>
      expect(at.pathname).toBe(
        buildIssuePath({ key: KEY, kind: "threads", record: BETWEEN_AGENTS.message.id })
      )
    );
    await waitFor(() =>
      expect(document.querySelector("[data-thread-record]")).toHaveAttribute(
        "data-thread-record",
        BETWEEN_AGENTS.message.id
      )
    );
    expect(threadsView(), "the record opened outside Threads").toBeDefined();
    expect(pressed(THREADS_WORDS.choices.agents)).toHaveAttribute("aria-pressed", "true");
    await waitFor(() =>
      expect(document.activeElement, "the message is not focused").toBe(
        document.querySelector(`[data-thread-entry="${reply}"]`)
      )
    );
  });

  test("an agent's note opens its own record under All", async () => {
    drawn = drawMessage(NOTE.message.id);
    const at = drawn.held;
    await waitFor(() =>
      expect(at.pathname).toBe(
        buildIssuePath({ key: KEY, kind: "threads", record: NOTE.message.id })
      )
    );
    await waitFor(() =>
      expect(document.querySelector("[data-thread-record]")).toHaveAttribute(
        "data-thread-record",
        NOTE.message.id
      )
    );
    expect(pressed(THREADS_WORDS.choices.all)).toHaveAttribute("aria-pressed", "true");
    await waitFor(() =>
      expect(document.activeElement, "the note is not focused").toBe(
        document.querySelector(`[data-thread-entry="${NOTE.message.id}"]`)
      )
    );
  });
});

describe("every other message opens as on main", () => {
  test("a message of a thread the reader wrote in lands on its row in the outline", async () => {
    const reply = PERSON.replies?.[0]?.id ?? "";
    drawn = drawMessage(reply);
    await waitFor(() =>
      expect(
        document.querySelector(`[data-node-id="${reply}"]`),
        "the outline drew no row of it"
      ).not.toBeNull()
    );
    expect(drawn.held.pathname).toBe(buildIssuePath({ id: reply, key: KEY, kind: "message" }));
    expect(threadsView(), "the message opened in Threads").toBeUndefined();
  });

  test("a message of a thread the Legion daemon began that only agents wrote in lands as main lands it", async () => {
    const id = DAEMON_ALONE.message.id;
    drawn = drawMessage(id, [
      ...CONVERSATION.filter(({ message }) => message.id !== id),
      DAEMON_ALONE,
    ]);
    await waitFor(() =>
      expect(
        document.querySelector("[data-row-report]"),
        "the outline says nowhere that the thread is the agents' alone"
      ).toHaveTextContent(KEY_REPORTS.agents_thread)
    );
    expect(drawn.held.pathname).toBe(buildIssuePath({ id, key: KEY, kind: "message" }));
    expect(threadsView(), "the message opened in Threads").toBeUndefined();
  });
});
