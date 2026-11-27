import { beforeAll, describe, expect, spyOn, test } from "bun:test";
import type { MessageThreadPage } from "@legion/contracts";
import type { QueryClient } from "@tanstack/react-query";
import { act, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import { newQueryClient, signedInAsReader } from "../../../__tests__/draft-matrix-fixture";
import { plannerAgent } from "../../../__tests__/search-fixture";
import { api } from "../../../api/client";
import type { Message } from "../../../api/types";
import { agentsQuery } from "../../refs/agent-registry";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { CONTROL_LABELS } from "../page-words";
import messageCreatedBody from "./fixtures/message-created.json";
import messagesInBody from "./fixtures/messages-in.json";
import { answerThreads, drawThreads, KEY, stubWindow, threadsAt } from "./threads-view-fixture";

/**
 * The message boxes of an opened record (dispatch://INFRA-5704/spec#b-r-new-thread, R10, R15):
 * the box at the foot and the box under an entry say before the send that a send to a live party
 * that advertises no delivery mode records a failed attempt, as New thread's box says it
 * (`NewThread.test.tsx`), and the send goes as it would without the warning. The record is the
 * reader's message to the planner's role over the server's answers captured at the stack base
 * (`fixtures/README.md`). No capture holds the agent registry, so each case puts the registry's
 * answer in the cache before the record opens, as a page that read it earlier holds it, with the
 * planner as every picker's test draws it (`__tests__/search-fixture.ts`).
 */

const MESSAGES = (messagesInBody as unknown as MessageThreadPage).threads ?? [];

/** The root of the message thread of All's first page whose words begin with `words`. */
function messageRoot(words: string) {
  const found = MESSAGES.find((row) => row.message.body.startsWith(words));
  if (found === undefined) throw new Error(`No message page of the fixtures holds "${words}".`);
  return found.message;
}

/** The reader's message to the planner's role (`role:planner`), with the planner's replies. */
const ROOT = messageRoot("Can the list show");

/** Dispatch's `201` to a box's send. */
const CREATED = messageCreatedBody as unknown as Message;

const WORDS = "Under the root.";

/** What a box says of a party that takes no message, in the words New thread's case reads. */
const FAILED_ATTEMPT = /records a failed attempt/i;

const box = (which: string) =>
  document.querySelector<HTMLElement>(`[data-thread-box="${which}"]`) ?? undefined;

/** Waits for the record's box at the foot, then opens the box under its root. */
async function openBoxes(user: UserEvent): Promise<void> {
  await waitFor(() => expect(box("foot")).toBeDefined());
  await user.click(
    within(
      document.querySelector<HTMLElement>(`[data-thread-entry="${ROOT.id}"]`) ?? document.body
    ).getByRole("button", { name: CONTROL_LABELS.reply })
  );
  await waitFor(() => expect(box(ROOT.id)).toBeDefined());
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

describe("the message boxes of an opened record (R10, R15)", () => {
  test("a live party that advertises no delivery mode: each message box of an opened record says that a send to it records a failed attempt, and the send goes as before", async () => {
    const window = stubWindow(1440);
    answerThreads();
    const user = userEvent.setup();
    const answered = Promise.withResolvers<Message>();
    const create = spyOn(api, "createMessage").mockReturnValue(answered.promise as never);
    // The planner's session is live, listed and holds the thread's role, but its capabilities
    // hold no delivery mode (`deliveryModesOf`), as a session whose plugin cannot reach its
    // conversation advertises.
    const client = signedInAsReader(newQueryClient());
    client.setQueryData(agentsQuery().queryKey, [plannerAgent({ capabilities: [] })]);
    const drawn = drawThreads(threadsAt(ROOT.id), { client });
    try {
      await openBoxes(user);
      for (const [where, which] of [
        ["at the foot", "foot"],
        ["under the root", ROOT.id],
      ] as const) {
        expect(
          box(which)?.textContent ?? "",
          `the box ${where} says nothing of a party that takes no message`
        ).toMatch(FAILED_ATTEMPT);
      }
      await user.type(
        within(box(ROOT.id) as HTMLElement).getByRole("textbox", { name: CONTROL_LABELS.reply }),
        WORDS
      );
      await user.click(
        within(box(ROOT.id) as HTMLElement).getByRole("button", { name: CONTROL_LABELS.send })
      );
      expect(create.mock.calls.map((call) => call.slice(0, 2)) as unknown[]).toEqual([
        [
          KEY,
          {
            body: WORDS,
            delivery: ROOT.deliveries?.[0]?.delivery,
            in_reply_to: ROOT.id,
            target: ROOT.target,
          },
        ],
      ]);
      expect(api.listAgents, "opening the record read the registry").not.toHaveBeenCalled();
      await act(async () => answered.resolve(CREATED));
      await waitFor(() => expect(box(ROOT.id), "the box stays once sent").toBeUndefined());
    } finally {
      answered.resolve(CREATED);
      create.mockRestore();
      drawn.done();
      window.restore();
    }
  });

  /** What the cache holds of the registry as the record opens, in each case where it says
   *  nothing of a live party that takes no message. */
  const SILENT: readonly (readonly [string, (client: QueryClient) => Promise<void> | void])[] = [
    [
      "the session that holds the thread's role advertises Send",
      (client) => {
        client.setQueryData(agentsQuery().queryKey, [plannerAgent()]);
      },
    ],
    [
      "no listed session holds the thread's role, beside a listed session that takes no message",
      (client) => {
        client.setQueryData(agentsQuery().queryKey, [
          plannerAgent({ capabilities: [], roles: [] }),
        ]);
      },
    ],
    ["the registry was never read", () => undefined],
    [
      "the registry's last read failed, and the cache keeps the rows of the read before, whose holder of the role takes no message",
      async (client) => {
        client.setQueryData(agentsQuery().queryKey, [plannerAgent({ capabilities: [] })]);
        const failing = spyOn(api, "listAgents").mockRejectedValue(
          new Error("The listener did not answer.")
        );
        try {
          await client.prefetchQuery({ ...agentsQuery(), staleTime: 0 });
        } finally {
          failing.mockRestore();
        }
      },
    ],
  ];
  for (const [registry, seed] of SILENT) {
    test(`no message box of an opened record warns where ${registry}`, async () => {
      const window = stubWindow(1440);
      answerThreads();
      const user = userEvent.setup();
      const client = signedInAsReader(newQueryClient());
      await seed(client);
      const drawn = drawThreads(threadsAt(ROOT.id), { client });
      try {
        await openBoxes(user);
        expect(
          document.querySelector("[data-thread-record]")?.textContent ?? "",
          "a box warned of a party that nothing shows takes no message"
        ).not.toMatch(FAILED_ATTEMPT);
        expect(api.listAgents, "opening the record read the registry").not.toHaveBeenCalled();
      } finally {
        drawn.done();
        window.restore();
      }
    });
  }
});
