import { beforeAll, describe, expect, spyOn, test } from "bun:test";
import type { IssueCollaboratorAgent } from "@legion/contracts";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";

import { REFUSAL } from "../../../__tests__/draft-matrix-fixture";
import { answerRequests } from "../../../__tests__/network";
import { issueCollaborators, plannerAgent } from "../../../__tests__/search-fixture";
import { api } from "../../../api/client";
import type { Message } from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { ANSWER_LINES, CONTROL_LABELS, THREADS_WORDS } from "../page-words";
import messageCreatedBody from "./fixtures/message-created.json";
import { answerThreads, drawThreads, KEY, stubWindow, threadsAt } from "./threads-view-fixture";

/**
 * New thread (dispatch://INFRA-5704/spec#b-r-new-thread, R10, R11, R15) in each of its states:
 * opened, no agent or no words, sending, refused and sent. The reads of Threads
 * and Dispatch's `201` are the server's answers captured at the stack base (`fixtures/README.md`).
 * No capture holds the agent registry or the issue's collaborators, so the two reads of the `@`
 * picker are answered as every picker's test answers them (`__tests__/search-fixture.ts`): the
 * planner live and holding its role, and the issue's reviewer role, which no live session holds.
 */

/** The role the live planner holds, the planner's session, and the reviewer's role. */
const PLANNER_ROLE = "role:planner";
const PLANNER_SESSION = "session:planner-session";
const REVIEWER_TOKEN = "legion-p-core-1-reviewer";
const STOPPED_ROLE = `role:${REVIEWER_TOKEN}`;

/** The reviewer as the issue's collaborators record it: a Legion session of the issue whose role
 *  no live session holds now. */
const REVIEWER: IssueCollaboratorAgent = {
  kind: "session",
  last_at: null,
  legion: {
    replaced_at: null,
    replaced_by: null,
    role: "reviewer",
    role_token: REVIEWER_TOKEN,
    started_at: "2026-11-26T11:00:00Z",
  },
  reasons: ["claimed"],
  session_id: "reviewer-session",
  title: "Reviewer",
};

/** Dispatch's `201` to New thread's send. */
const CREATED = messageCreatedBody as unknown as Message;

const WORDS = "A new thread to the planner.";

const panel = () => document.querySelector<HTMLElement>("[data-new-thread]") ?? undefined;
const inPanel = () => within(panel() ?? document.body);
const messageField = () =>
  inPanel().getByRole<HTMLTextAreaElement>("textbox", { name: THREADS_WORDS.message_box });
const sendButton = () =>
  within(document.querySelector<HTMLElement>('[data-thread-box="new"]') ?? document.body).getByRole(
    "button"
  );
const option = (target: string) =>
  panel()?.querySelector<HTMLElement>(`[data-thread-party="${target}"]`) ?? undefined;

/** The targets the chooser offers, in its order. */
function partiesOffered(): (string | null)[] {
  return inPanel()
    .queryAllByRole("option")
    .map((one) => one.getAttribute("data-thread-party"));
}

/**
 * Threads at 1440 px, with every read answered: Threads' own with the captured answers, and the
 * picker's two with the planner and the reviewer. `reads` holds the path of each read under `/api/`
 * the page sends once `record()` is called, whichever handler answers it.
 */
function drawWithParties() {
  const window = stubWindow(1440);
  const sent = answerThreads();
  const collaborators = { reads: 0 };
  answerRequests(
    http.get("/api/v1/issues/:key/collaborators", () => {
      collaborators.reads += 1;
      return Response.json(issueCollaborators({ agents: [REVIEWER], issue_key: KEY }));
    })
  );
  const reads: string[] = [];
  let recording = false;
  // Answered last, so it runs first: it notes the read and answers nothing, and the read goes on
  // to the handler that answers it.
  answerRequests(
    http.get("/api/v1/*", ({ request }) => {
      if (recording) reads.push(new URL(request.url).pathname);
    })
  );
  const drawn = drawThreads(threadsAt());
  const agents = spyOn(api, "listAgents").mockResolvedValue([plannerAgent()]);
  return {
    agents,
    collaborators,
    reads,
    sent,
    record(): void {
      recording = true;
    },
    done(): void {
      agents.mockRestore();
      drawn.done();
      window.restore();
    },
  };
}

/** Waits for All's rows, then opens New thread and waits for its chooser's parties. */
async function openNewThread(user: UserEvent): Promise<void> {
  await waitFor(() => expect(document.querySelector("[data-thread-row]")).toBeTruthy());
  await user.click(screen.getByRole("button", { name: THREADS_WORDS.new_thread }));
  await waitFor(() => expect(partiesOffered().length).toBeGreaterThan(0));
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

describe("New thread (R10, R11, R15)", () => {
  test("New thread opened puts the caret in its box and reads only what the @ picker reads, once it opens", async () => {
    const at = drawWithParties();
    const user = userEvent.setup();
    try {
      await waitFor(() => expect(document.querySelector("[data-thread-row]")).toBeTruthy());
      expect(
        at.agents,
        "the chooser read the registry before New thread opened"
      ).not.toHaveBeenCalled();
      expect(at.collaborators.reads, "the chooser read the collaborators before it opened").toBe(0);
      const before = { ...at.sent };
      at.record();
      await user.click(screen.getByRole("button", { name: THREADS_WORDS.new_thread }));
      await waitFor(() =>
        expect(messageField(), "New thread put no caret in its box").toHaveFocus()
      );
      await waitFor(() =>
        expect(partiesOffered()).toEqual([PLANNER_ROLE, PLANNER_SESSION, STOPPED_ROLE])
      );
      expect(at.agents).toHaveBeenCalledTimes(1);
      expect(at.reads, "New thread read what the @ picker does not").toEqual([
        `/api/v1/issues/${KEY}/collaborators`,
      ]);
      expect(at.sent, "New thread read a page of the list again").toEqual(before);
    } finally {
      at.done();
    }
  });

  test("the chooser names each party by its name and its detail, and its field keeps the parties whose name or detail holds the words", async () => {
    const at = drawWithParties();
    const user = userEvent.setup();
    try {
      await openNewThread(user);
      const stopped = option(STOPPED_ROLE);
      expect(stopped).toHaveAccessibleName(`${REVIEWER_TOKEN} reviewer · not running, not told`);
      const filter = inPanel().getByRole("textbox", { name: THREADS_WORDS.chooser_filter });
      expect(filter).toHaveAttribute("placeholder", THREADS_WORDS.chooser_filter);
      await user.type(filter, "running");
      expect(partiesOffered(), "the filter kept a party whose words do not hold it").toEqual([
        STOPPED_ROLE,
      ]);
      await user.clear(filter);
      await user.type(filter, "PLANNER");
      expect(partiesOffered(), "the filter reads the words in one case only").toEqual([
        PLANNER_ROLE,
        PLANNER_SESSION,
      ]);
    } finally {
      at.done();
    }
  });

  test("no agent chosen or no words: Send is not offered", async () => {
    const at = drawWithParties();
    const user = userEvent.setup();
    try {
      await openNewThread(user);
      expect(sendButton(), "Send is offered with no agent and no words").toBeDisabled();
      await user.type(messageField(), WORDS);
      expect(sendButton(), "Send is offered with no agent chosen").toBeDisabled();
      await user.click(option(PLANNER_ROLE) as HTMLElement);
      expect(option(PLANNER_ROLE)).toHaveAttribute("aria-selected", "true");
      expect(option(STOPPED_ROLE)).toHaveAttribute("aria-selected", "false");
      expect(sendButton(), "Send is not offered once an agent and words are there").toBeEnabled();
      await user.clear(messageField());
      expect(sendButton(), "Send is offered with no words").toBeDisabled();
    } finally {
      at.done();
    }
  });

  test("a live party that advertises no delivery mode: New thread says that a send to it records a failed attempt, as a direct message's box on main says", async () => {
    const at = drawWithParties();
    // The session is live and listed, but its capabilities hold no delivery mode
    // (`deliveryModesOf`), as a session whose plugin cannot reach its conversation advertises.
    // `plainDelivery` then answers steer, which the server records as a failed attempt.
    at.agents.mockResolvedValue([plannerAgent({ capabilities: [] })]);
    const user = userEvent.setup();
    try {
      await openNewThread(user);
      await user.click(option(PLANNER_SESSION) as HTMLElement);
      await user.type(messageField(), WORDS);
      expect(
        (panel()?.textContent ?? "").match(/records a failed attempt/i),
        "New thread says nothing of a chosen agent that takes no message: its send records a failed attempt in silence"
      ).not.toBeNull();
    } finally {
      at.done();
    }
  });

  test("sending: a live party's message goes as its session takes a plain message, and the box holds the words with Send busy", async () => {
    const at = drawWithParties();
    const user = userEvent.setup();
    const answered = Promise.withResolvers<Message>();
    const create = spyOn(api, "createMessage").mockReturnValue(answered.promise as never);
    try {
      await openNewThread(user);
      await user.click(option(PLANNER_ROLE) as HTMLElement);
      await user.type(messageField(), WORDS);
      await user.click(sendButton());
      expect(create.mock.calls.map((call) => call.slice(0, 2)) as unknown[]).toEqual([
        [KEY, { body: WORDS, delivery: "steer", target: PLANNER_ROLE }],
      ]);
      await waitFor(() => expect(messageField()).toBeDisabled());
      expect(sendButton()).toHaveTextContent(ANSWER_LINES.sending);
      expect(sendButton()).toBeDisabled();
      expect(messageField().value, "the send took the reader's words").toBe(WORDS);
    } finally {
      await act(async () => answered.resolve(CREATED));
      create.mockRestore();
      at.done();
    }
  });

  test("refused: the words stay and Retry sends them again, to a role no live session holds as an aside", async () => {
    const at = drawWithParties();
    const user = userEvent.setup();
    const create = spyOn(api, "createMessage").mockRejectedValue(REFUSAL);
    try {
      await openNewThread(user);
      await user.click(option(STOPPED_ROLE) as HTMLElement);
      await user.type(messageField(), WORDS);
      await user.click(sendButton());
      await inPanel().findByText(REFUSAL.message);
      expect(messageField().value, "the refusal took the reader's words").toBe(WORDS);
      expect(create.mock.calls[0]?.[1]).toEqual({
        body: WORDS,
        delivery: "aside",
        target: STOPPED_ROLE,
      });
      await user.click(inPanel().getByRole("button", { name: CONTROL_LABELS.retry }));
      await waitFor(() => expect(create).toHaveBeenCalledTimes(2));
      expect(create.mock.calls[1]?.[1]).toEqual(create.mock.calls[0]?.[1]);
    } finally {
      create.mockRestore();
      at.done();
    }
  });

  test("refused with no words of Dispatch's: the line says the message was not sent, not a reply", async () => {
    const at = drawWithParties();
    const user = userEvent.setup();
    // A failure with no answer of Dispatch's: the write store holds a lost connection's send and
    // sends it again, so the line is for any other failure, such as an answer it cannot read.
    const create = spyOn(api, "createMessage").mockRejectedValue(
      new TypeError("The answer could not be read.")
    );
    try {
      await openNewThread(user);
      await user.click(option(STOPPED_ROLE) as HTMLElement);
      await user.type(messageField(), WORDS);
      await user.click(sendButton());
      expect(await inPanel().findByRole("alert")).toHaveTextContent(
        ANSWER_LINES.threads_message_not_sent
      );
      expect(inPanel().queryByText(ANSWER_LINES.threads_reply_not_sent)).toBeNull();
      expect(messageField().value, "the refusal took the reader's words").toBe(WORDS);
    } finally {
      create.mockRestore();
      at.done();
    }
  });

  test("sent: the new thread opens at its address, drawn from Dispatch's answer with no lookup", async () => {
    const at = drawWithParties();
    const user = userEvent.setup();
    const create = spyOn(api, "createMessage").mockResolvedValue(CREATED as never);
    const lookups = [
      spyOn(api, "getMessageThread"),
      spyOn(api, "getMessage"),
      spyOn(api, "getComment"),
      spyOn(api, "getAsk"),
    ];
    try {
      await openNewThread(user);
      await user.click(option(STOPPED_ROLE) as HTMLElement);
      await user.type(messageField(), WORDS);
      await user.click(sendButton());
      await waitFor(() =>
        expect(document.querySelector("[data-thread-record]")).toHaveAttribute(
          "data-thread-record",
          CREATED.id
        )
      );
      expect(panel(), "New thread stays open beside its sent thread").toBeUndefined();
      const root = document.querySelector<HTMLElement>(`[data-thread-entry="${CREATED.id}"]`);
      expect(root, "the opened thread draws no message").toBeTruthy();
      expect(root?.textContent).toContain(CREATED.body);
      for (const lookup of lookups) {
        expect(lookup, "the sent thread was looked up").not.toHaveBeenCalled();
      }
    } finally {
      for (const lookup of lookups) lookup.mockRestore();
      create.mockRestore();
      at.done();
    }
  });
});
