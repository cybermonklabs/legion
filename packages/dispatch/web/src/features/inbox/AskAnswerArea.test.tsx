import { afterEach, beforeAll, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: Ctrl/Cmd+Enter pairs and presses on controls a refusal already holds
import { fireEvent, type RenderResult, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { graphNode } from "../../__tests__/graph-fixture";
import { architectureTree } from "../../__tests__/search-fixture";
import { api } from "../../api/client";
import type { Agent, Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { AskCard } from "./AskCard";
import { AskReplyComposer } from "./AskThread";
import { EMPTY_DRAFT_REFUSAL } from "./draft-store";
import { ANSWER_MENTION_REFUSAL, ANSWER_REFUSAL } from "./useAskAnswerForm";

/** INFRA-341 (R2, R3, R4): the ask's shared answer area stores a picked session as a
 * reference in the comment body and refuses an answer that would not tell that session. */

const planner: Agent = {
  capabilities: ["steer"],
  dir: "/workspaces/planner",
  host: "host-a",
  last_activity: null,
  last_seen: 1_700_000_000_000,
  machine_id: "host-a",
  open_asks: 0,
  roles: [],
  session_id: "A",
  state: "live",
  title: "Planner",
};

const PLANNER_MENTION = "[@Planner](dispatch://agent/A)";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

let getArchitecture: Mock<typeof api.getArchitecture>;
let whoAmI: Mock<typeof api.whoAmI>;
let getReferences: Mock<typeof api.getReferences>;
let listAgents: Mock<typeof api.listAgents>;
let getIssueCollaborators: Mock<typeof api.getIssueCollaborators>;
let getIssueSubscribers: Mock<typeof api.getIssueSubscribers>;

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
  listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
  getArchitecture = spyOn(api, "getArchitecture").mockResolvedValue(architectureTree());
  // The shared picker reads the issue's people, stopped roles and architecture components.
  getIssueCollaborators = spyOn(api, "getIssueCollaborators").mockResolvedValue({
    agents: [],
    issue_key: "CORE-1",
    legion_registry: "ok",
    people: [],
  });
  getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
});

afterEach(() => {
  getArchitecture.mockRestore();
  whoAmI.mockRestore();
  getReferences.mockRestore();
  listAgents.mockRestore();
  getIssueCollaborators.mockRestore();
  getIssueSubscribers.mockRestore();
});

function ask(overrides: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: null,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    block_id: null,
    author: { id: "session-1", kind: "session" },
    created_at: "2026-10-08T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Which release path?",
    state: "open",
    urgency: "med",
    waiting_on: "human",
    ...overrides,
  };
}

function thread(input: Ask, replies: Comment[] = []): AskRead {
  return {
    answers: input.answer === null ? [] : [input.answer],
    ask: input,
    claimant: null,
    edits: [],
    followers: [],
    replies,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
  };
}

function comment(overrides: Partial<Comment> = {}): Comment {
  return {
    anchor: null,
    ask_id: "ask-1",
    author: { id: "alice", kind: "user" },
    body: "A reply",
    created_at: "2026-10-08T00:01:00Z",
    edited_at: null,
    id: "comment-1",
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: "agent",
    ...commentDeliveryFields(),
    ...overrides,
  };
}

function renderWithClient(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>{node}</QueryClientProvider>
    </MemoryRouter>
  );
}

/** Opens the shared picker at the end of the draft and picks Planner's session. */
async function pickPlanner(view: RenderResult, field: HTMLTextAreaElement, prefix: string) {
  const user = userEvent.setup();
  await user.click(field);
  await user.clear(field);
  await user.paste(`${prefix}@`);
  await user.click(await view.findByRole("option", { name: /^Planner\b/ }));
  await waitFor(() => expect(field.value).toBe(`${prefix}${PLANNER_MENTION}`));
}

/** The line on screen that a refused button is described by. */
function shownReason(button: HTMLElement): string | null {
  const id = (button.getAttribute("aria-describedby") ?? "").split(" ")[0];
  return id === undefined || id === "" ? null : (document.getElementById(id)?.textContent ?? null);
}

test("Ask back posts the picked session as a mention, which the answer refuses to carry", async () => {
  const input = ask();
  const posted: CreateCommentInput[] = [];
  const answered: unknown[] = [];
  const view = renderWithClient(
    <AskCard
      answerAsk={async (_id, answerInput) => {
        answered.push(answerInput);
        return input;
      }}
      ask={input}
      createReply={async (_issueKey, commentInput) => {
        posted.push(commentInput);
        return comment({ body: commentInput.body });
      }}
      getAskThread={async () => thread(input)}
    />
  );

  try {
    const field = (await view.findByLabelText("Your answer")) as HTMLTextAreaElement;
    await pickPlanner(view, field, "Ask ");

    // An answer carries no mentions, so Answer refuses this draft - in the tab order, with its
    // reason on screen and as its accessible description.
    const answer = view.getByRole("button", { name: "Answer" });
    expect(answer.getAttribute("aria-disabled")).toBe("true");
    expect(answer.hasAttribute("disabled")).toBe(false);
    expect(answer.getAttribute("title")).toBe(ANSWER_MENTION_REFUSAL);
    expect(shownReason(answer)).toBe(ANSWER_MENTION_REFUSAL);
    // A press on a control the refusal already holds, which user-event will not perform.
    fireEvent.click(answer);
    // Ctrl/Cmd+Enter submits the form without passing through the button, and is refused too.
    fireEvent.keyDown(field, { ctrlKey: true, key: "Enter" });
    fireEvent.keyDown(field, { key: "Enter", metaKey: true });
    expect(answered).toEqual([]);

    await userEvent.setup().click(view.getByRole("button", { name: "Ask back" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({ ask_id: "ask-1", body: `Ask ${PLANNER_MENTION}` });
  } finally {
    view.unmount();
  }
});

// The picker must read again when it opens, even if the last registry poll is still fresh.
test("opening the ask's @ picker reads the registry again, so a session started since the last poll is offered", async () => {
  const builder: Agent = { ...planner, session_id: "B", title: "Builder" };
  listAgents.mockResolvedValue([planner, builder]);
  const input = ask();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  // What the last poll left: the registry as it was before Builder started. It is inside the
  // query's `staleTime`, so nothing but the picker's own read goes looking for a newer one.
  queryClient.setQueryData(["agents"], [planner]);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskCard ask={input} getAskThread={async () => thread(input)} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const field = (await view.findByLabelText("Your answer")) as HTMLTextAreaElement;
    expect(listAgents).not.toHaveBeenCalled();
    const user = userEvent.setup();
    await user.click(field);
    await user.paste("Ask @");
    await view.findByRole("option", { name: /^Planner\b/ });
    await waitFor(() => expect(listAgents).toHaveBeenCalledTimes(1));
    await view.findByRole("option", { name: /^Builder\b/ });
  } finally {
    view.unmount();
  }
});

test("a draft whose box closes and opens again still posts its picked mention", async () => {
  const input = ask();
  const posted: CreateCommentInput[] = [];
  const card = (
    <AskCard
      ask={input}
      createReply={async (_issueKey, commentInput) => {
        posted.push(commentInput);
        return comment({ body: commentInput.body });
      }}
      getAskThread={async () => thread(input)}
    />
  );
  const first = renderWithClient(card);
  const field = (await first.findByLabelText("Your answer")) as HTMLTextAreaElement;
  await pickPlanner(first, field, "Ask ");
  first.unmount();

  const second = renderWithClient(card);
  try {
    const reopened = (await second.findByLabelText("Your answer")) as HTMLTextAreaElement;
    expect(reopened.value).toBe(`Ask ${PLANNER_MENTION}`);
    await userEvent.setup().click(second.getByRole("button", { name: "Ask back" }));
    await waitFor(() =>
      expect(posted).toEqual([{ ask_id: "ask-1", body: `Ask ${PLANNER_MENTION}` }])
    );
  } finally {
    second.unmount();
  }
});

test("the reply box under an answered ask posts the picked session as a mention", async () => {
  const input = ask({
    answer: { at: "2026-10-08T00:02:00Z", selected: [], text: "Ship it.", user: "alice" },
    state: "answered",
  });
  const posted: CreateCommentInput[] = [];
  const view = renderWithClient(
    <AskReplyComposer
      ask={input}
      createReply={async (_issueKey, commentInput) => {
        posted.push(commentInput);
        return comment({ body: commentInput.body });
      }}
    />
  );

  try {
    const field = view.getByLabelText("Reply") as HTMLTextAreaElement;
    const send = view.getByRole("button", { name: "Reply" });
    // An empty box refuses, keeps its place in the tab order, and says why on screen.
    expect(send.getAttribute("aria-disabled")).toBe("true");
    expect(send.hasAttribute("disabled")).toBe(false);
    expect(send.getAttribute("title")).toBe(EMPTY_DRAFT_REFUSAL);
    expect(shownReason(send)).toBe(EMPTY_DRAFT_REFUSAL);
    // A press on a refused Send and the Ctrl+Enter that bypasses the button, both of which
    // user-event will not perform on a control it reads as unpressable.
    fireEvent.click(send);
    fireEvent.keyDown(field, { ctrlKey: true, key: "Enter" });
    expect(posted).toEqual([]);

    await pickPlanner(view, field, "Over to ");
    expect(view.getByRole("list", { name: "Mention targets" }).textContent).toContain("session:A");
    await userEvent.setup().click(send);
    await waitFor(() =>
      expect(posted).toEqual([{ ask_id: "ask-1", body: `Over to ${PLANNER_MENTION}` }])
    );
  } finally {
    view.unmount();
  }
});

test("an empty answer area refuses Answer and Ask back, sends nothing, and keeps the focus", async () => {
  const input = ask();
  const answered: unknown[] = [];
  const posted: unknown[] = [];
  const view = renderWithClient(
    <AskCard
      answerAsk={async (_id, answerInput) => {
        answered.push(answerInput);
        return input;
      }}
      ask={input}
      createReply={async (_issueKey, commentInput) => {
        posted.push(commentInput);
        return comment({ body: commentInput.body });
      }}
      getAskThread={async () => thread(input)}
    />
  );

  try {
    await view.findByLabelText("Your answer");
    const answer = view.getByRole("button", { name: "Answer" });
    const askBack = view.getByRole("button", { name: "Ask back" });
    expect(answer.getAttribute("title")).toBe(ANSWER_REFUSAL);
    expect(shownReason(answer)).toBe(ANSWER_REFUSAL);
    expect(askBack.getAttribute("title")).toBe(EMPTY_DRAFT_REFUSAL);
    expect(shownReason(askBack)).toBe(EMPTY_DRAFT_REFUSAL);
    for (const button of [answer, askBack]) {
      button.focus();
      // A press on a control the refusal already holds, with the focus the loop just set.
      fireEvent.click(button);
      expect(document.activeElement).toBe(button);
    }
    expect(answered).toEqual([]);
    expect(posted).toEqual([]);
  } finally {
    view.unmount();
  }
});

// The draft belongs to the ask, not to the card showing it, so a mention picked in one card
// stands in the other's draft - the one way "Answer with it anyway" is offered over a draft that
// holds a mention, since picking one in the card that offered the choice takes the choice away.
test("Answer with it anyway refuses a draft another card put a mention in", async () => {
  const input = ask();
  const answered: unknown[] = [];
  const view = renderWithClient(
    <>
      <AskCard
        answerAsk={async (_id, answerInput) => {
          answered.push(answerInput);
          return input;
        }}
        ask={input}
        getAskThread={async () => thread(input)}
      />
      <AskCard ask={input} getAskThread={async () => thread(input)} />
    </>
  );

  try {
    const fields = (await view.findAllByLabelText("Your answer")) as HTMLTextAreaElement[];
    const [asking, other] = fields;
    if (asking === undefined || other === undefined) throw new Error("expected two answer boxes");
    // The box's own text and a press on a refused control, in the one task the row measures:
    // user-event has no form for either.
    fireEvent.change(asking, { target: { value: "How does this fit the release plan?" } });
    fireEvent.click(view.getAllByRole("button", { name: "Answer" })[0] as HTMLElement);
    const choice = await view.findByRole("group", { name: "Question-shaped answer" });
    const anyway = within(choice).getByRole("button", { name: "Answer with it anyway" });
    expect(anyway.hasAttribute("aria-disabled")).toBe(false);

    await pickPlanner(view, other, "How does this fit the release plan? ");
    expect(anyway.getAttribute("aria-disabled")).toBe("true");
    expect(anyway.getAttribute("title")).toBe(ANSWER_MENTION_REFUSAL);
    expect(shownReason(anyway)).toBe(ANSWER_MENTION_REFUSAL);
    // A press on a control the refusal already holds: user-event refuses to click what it reads
    // as unpressable, and the press is the whole of what this row proves.
    fireEvent.click(anyway);
    expect(answered).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("an ask reply carries the copy control for its own reference", async () => {
  const input = ask({
    answer: { at: "2026-10-08T00:02:00Z", selected: [], text: "Ship it.", user: "alice" },
    state: "answered",
  });
  const view = renderWithClient(
    <AskCard ask={input} getAskThread={async () => thread(input, [comment()])} />
  );

  try {
    const replies = await view.findByTestId(`thread-${input.id}`);
    const copy = within(replies).getByRole("button", {
      name: "Copy reference dispatch://CORE-1/comment/comment-1",
    });
    expect(copy.closest("[data-copy-ref]")).not.toBeNull();
  } finally {
    view.unmount();
  }
});
