import { afterEach, beforeAll, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import type { AskRead, AskWithTrees, IssueReferenceSummary } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { writesOf } from "../writes/write-store";
import { AskCard } from "./AskCard";
import { AskReplyComposer } from "./AskThread";
import type { OwnedAsk } from "./ask-name";

/**
 * The label of every write on an ask - the line the Writes indicator reads - holds the question's
 * plain-text projection (`useQuestionLabel`), never its markdown: the bold goes, and the
 * `dispatch://` address reads as its issue's key alone, with no status and no title. CORE-2's
 * summary is in the cache before the card draws, so the issue is read at once and what each row
 * reads is the label, never whether the reference resolves.
 */
const QUESTION = "**Ship** it? dispatch://CORE-2";
const PROJECTED = "Ship it? CORE-2";

const summary: IssueReferenceSummary = {
  documents: [],
  key: "CORE-2",
  project: "CORE",
  status: "in_progress",
  title: "Cut the release",
};

let whoAmI: Mock<typeof api.whoAmI>;
let getReferences: Mock<typeof api.getReferences>;
let getIssueSubscribers: Mock<typeof api.getIssueSubscribers>;

beforeAll(async () => {
  await warmMarkdownRenderer();
});

beforeEach(() => {
  whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "ask-1", kind: "ask" }),
  });
  getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
});

afterEach(() => {
  whoAmI.mockRestore();
  getReferences.mockRestore();
  getIssueSubscribers.mockRestore();
});

/** An open question on CORE-1 whose words are `QUESTION`, carrying the reading the server sends
 *  beside them, as an Inbox read does. */
function openAsk(
  overrides: Partial<OwnedAsk> = {}
): OwnedAsk & Pick<AskWithTrees, "question_tree"> {
  return {
    anchor: null,
    answer: null,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "session-1", kind: "session" },
    block_id: null,
    created_at: "2026-10-08T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: QUESTION,
    question_tree: displayTree(QUESTION),
    state: "open",
    urgency: "med",
    waiting_on: "human",
    ...overrides,
  };
}

function thread(ask: OwnedAsk): AskRead {
  return {
    answers: [],
    ask,
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
  };
}

/** `node` over a client signed in as alice that already holds CORE-2's summary, under the key
 *  registry an open dialog reaches Escape through. */
function draw(node: ReactNode) {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(["issue-summary", "CORE-2"], summary);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>{node}</KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { client, view };
}

/** The label of the one write out on `client`, once it has gone. */
async function labelOut(client: QueryClient): Promise<string | undefined> {
  await waitFor(() => expect(writesOf(client).rows()).toHaveLength(1));
  return writesOf(client).rows()[0]?.label;
}

/** A write that stays out, so its line is still in the store when the row reads it. */
const neverAnswered = <T,>(): Promise<T> => Promise.withResolvers<T>().promise;

test("an answer's label reads the question's plain-text projection", async () => {
  const user = userEvent.setup();
  const ask = openAsk();
  const { client, view } = draw(
    <AskCard answerAsk={neverAnswered} ask={ask} initialThread={thread(ask)} />
  );
  const card = within(view.container);
  try {
    await user.click(card.getByLabelText("Your answer"));
    await user.paste("Ship it on Friday.");
    await user.click(card.getByRole("button", { name: "Answer" }));
    expect(await labelOut(client)).toBe(`Answer to “${PROJECTED}”`);
  } finally {
    view.unmount();
    writesOf(client).clear();
  }
});

test("a question back's label reads the question's plain-text projection", async () => {
  const user = userEvent.setup();
  const ask = openAsk();
  const { client, view } = draw(
    <AskCard createReply={neverAnswered} ask={ask} initialThread={thread(ask)} />
  );
  const card = within(view.container);
  try {
    await user.click(card.getByLabelText("Your answer"));
    await user.paste("Which build?");
    await user.click(card.getByRole("button", { name: "Ask back" }));
    expect(await labelOut(client)).toBe(`Question back on “${PROJECTED}”`);
  } finally {
    view.unmount();
    writesOf(client).clear();
  }
});

test("a reply's label reads the question's plain-text projection", async () => {
  const user = userEvent.setup();
  const ask = openAsk({
    answer: { at: "2026-10-08T01:00:00Z", selected: null, text: "Ship it.", user: "alice" },
    state: "answered",
  });
  const { client, view } = draw(<AskReplyComposer ask={ask} createReply={neverAnswered} />);
  const form = within(view.container);
  try {
    await user.click(form.getByRole("textbox", { name: "Reply" }));
    await user.paste("Shipped.");
    await user.click(form.getByRole("button", { name: "Reply" }));
    expect(await labelOut(client)).toBe(`Reply on “${PROJECTED}”`);
  } finally {
    view.unmount();
    writesOf(client).clear();
  }
});

test("a decline's label reads the question's plain-text projection", async () => {
  const user = userEvent.setup();
  const ask = openAsk({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const { client, view } = draw(
    <AskCard ask={ask} declineAsk={neverAnswered} initialThread={thread(ask)} />
  );
  const card = within(view.container);
  try {
    await user.click(await card.findByRole("button", { name: "Decline to answer" }));
    const dialog = card.getByRole("dialog", { name: "Decline to answer" });
    await user.click(within(dialog).getByRole("button", { name: "Decline to answer" }));
    expect(await labelOut(client)).toBe(`Decline of “${PROJECTED}”`);
  } finally {
    view.unmount();
    writesOf(client).clear();
  }
});
