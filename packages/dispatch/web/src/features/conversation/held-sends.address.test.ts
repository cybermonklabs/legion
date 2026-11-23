import { expect, jest, spyOn, test } from "bun:test";
import type { MutationKey } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";

import { onFakeClock, settle } from "../../__tests__/fake-clock";
import { keptTab } from "../../__tests__/kept-sends-harness";
import { ApiError, api } from "../../api/client";
import { SEND_DEADLINE_MS, SendStoppedError } from "../writes/keyed-write";
import type { HeldSends } from "./held-sends";
import { agentKey, agentSend, heldOf, request, verdict } from "./held-sends-fixture";
import { requestFor, type SentRequest } from "./send-request";

// The held-send store's rows about the address a refused or stopped send's draft lives at: its
// Retry goes there alone, and the reader's own move takes it along (`HeldSends.carry`,
// `HeldSends.carryInto`). The store's other rows are in `held-sends.test.ts`.

/** A comment on a project document, Send's own. */
const documentSend: SentRequest = {
  ...request,
  owner: { artifactId: "artifact-1", kind: "artifact", project: "CORE" },
  sendKey: "send-document",
};

/** A reply to comment `id` on CORE-1, Send's own. */
function replySend(id: string): SentRequest {
  return {
    ...request,
    replyTo: { actor: { id: "bob", kind: "user" }, excerpt: "Earlier", id, parentKind: "comment" },
    sendKey: `send-reply-${id}`,
  };
}

/** Every write a composer's send makes, by the address it reaches: the first is refused with
 *  Dispatch's verdict, and every later one lands. */
function watchAddresses(): { reached: string[]; restore: () => void } {
  const reached: string[] = [];
  const reach = async (where: string, reply: string | null | undefined): Promise<never> => {
    reached.push(reply === null || reply === undefined ? where : `${where}, reply to ${reply}`);
    if (reached.length === 1) throw verdict;
    return {} as never;
  };
  const spies = [
    spyOn(api, "createAgentMessage").mockImplementation((session, input) =>
      reach(`session ${session}`, input.in_reply_to)
    ),
    spyOn(api, "createComment").mockImplementation((issue, input) =>
      reach(`issue ${issue}`, input.reply_to)
    ),
    spyOn(api, "createArtifactComment").mockImplementation((artifact, input) =>
      reach(`document ${artifact}`, input.reply_to)
    ),
  ];
  return {
    reached,
    restore: () => {
      for (const spy of spies) spy.mockRestore();
    },
  };
}

/** A composer's Send or Retry over the refusal it holds under `name`, as `MentionComposer` makes
 *  one (`useComposerSend`'s `send`): the request `requestFor` builds from what the composer shows
 *  now, handed to the store. Says whether the store took it. */
function pressOver(
  store: HeldSends,
  name: MutationKey,
  shown: Omit<SentRequest, "sendKey">
): boolean {
  const held = store.get(name);
  return store.send(
    name,
    requestFor(held?.request, held?.status === "refused" ? held.refusal : undefined, shown)
  );
}

/** `sent` as a composer shows it, without the key its Send minted, with `change` over it. */
function shownAs(
  sent: SentRequest,
  change: Partial<Omit<SentRequest, "sendKey">> = {}
): Omit<SentRequest, "sendKey"> {
  const { sendKey: _key, ...shown } = sent;
  return { ...shown, ...change };
}

/** Refuses `sent` under a composer's name, then presses that composer's Retry twice: once while
 *  its host has moved it to `elsewhere`, another address than the one `sent` was written for,
 *  with nothing carried there (`HeldSends.carry`), and once back on that address, with `draft` in
 *  the field both times. Answers every address a write reached. */
async function retriedFrom(
  sent: SentRequest,
  elsewhere: Partial<Omit<SentRequest, "sendKey">>,
  draft: SentRequest["draft"] = sent.draft
): Promise<{ away: boolean; reached: string[] }> {
  const watched = watchAddresses();
  const { held } = heldOf();
  const name: MutationKey = ["composer", sent.sendKey];
  try {
    held.send(name, sent);
    await waitFor(() => expect(held.get(name)?.status).toBe("refused"));
    const away = pressOver(held, name, shownAs(sent, { ...elsewhere, draft }));
    await settle();
    // Whatever the store made of that press, the composer is back on the send's own address.
    if (held.get(name)?.status === "refused") pressOver(held, name, shownAs(sent, { draft }));
    await waitFor(() => expect(held.get(name)).toBeUndefined());
    return { away, reached: watched.reached };
  } finally {
    watched.restore();
  }
}

// A Retry belongs to the address its refused send's draft lives at. From a composer its host has
// moved to another one - another row, a Reply on a message - it is refused, and nothing goes; back
// on that address, it goes there.
test("a refused message to a session is retried to that session alone", async () => {
  const { away, reached } = await retriedFrom(agentSend, {
    owner: { issueKey: "CORE-1", kind: "issue" },
  });
  expect(reached).toEqual(["session planner-session", "session planner-session"]);
  expect(away).toBe(false);
});

// The same of a comment on an issue, from a composer its host moved onto the session.
test("a refused comment on an issue is retried to that issue alone", async () => {
  const { away, reached } = await retriedFrom(request, {
    owner: { kind: "session", sessionId: "planner-session" },
  });
  expect(reached).toEqual(["issue CORE-1", "issue CORE-1"]);
  expect(away).toBe(false);
});

// The same of a comment on a document, from a composer drawn for another document.
test("a refused comment on a document is retried to that document alone", async () => {
  const { away, reached } = await retriedFrom(documentSend, {
    owner: { artifactId: "artifact-2", kind: "artifact", project: "CORE" },
  });
  expect(reached).toEqual(["document artifact-1", "document artifact-1"]);
  expect(away).toBe(false);
});

// An edited draft is still the one written for that address: a press from a composer its host
// moved off the reply is refused, and back on the reply it sends the edited words there.
test("an edited reply refused once is retried as a reply to the same comment alone", async () => {
  const edited = { body: "Held, and then some", mentions: [], replacement: "" };
  const { away, reached } = await retriedFrom(replySend("comment-1"), { replyTo: null }, edited);
  expect(reached).toEqual(["issue CORE-1, reply to comment-1", "issue CORE-1, reply to comment-1"]);
  expect(away).toBe(false);
});

// A refusal goes with the reader's own move of its composer (`carry`), from the address its draft
// lives at alone: a move from another address - one its host had moved the composer to - leaves
// it where it is. The press from where the reader took the draft is its Retry, a new write under
// a key of its own, and a press from the address it left - the host moving the composer back
// there - is refused, since the draft lives elsewhere now.
test("a refused reply the reader carries off its comment goes from where they took it", async () => {
  const createComment = spyOn(api, "createComment")
    .mockRejectedValueOnce(verdict)
    .mockResolvedValue({} as never);
  const { held } = heldOf();
  const sent = replySend("comment-1");
  const name: MutationKey = ["composer", sent.sendKey];
  try {
    held.send(name, sent);
    await waitFor(() => expect(held.get(name)?.status).toBe("refused"));
    held.carry(name, replySend("comment-2"), { owner: sent.owner, replyTo: null });
    expect(held.get(name)?.request).toEqual(sent);
    held.carry(name, sent, { owner: sent.owner, replyTo: null });

    expect(pressOver(held, name, shownAs(sent))).toBe(false);
    expect(pressOver(held, name, shownAs(sent, { replyTo: null }))).toBe(true);
    await waitFor(() => expect(held.get(name)).toBeUndefined());
    expect(createComment.mock.calls.map(([issue, input]) => [issue, input])).toEqual([
      ["CORE-1", { body: "Held", reply_to: "comment-1" }],
      ["CORE-1", { body: "Held" }],
    ]);
    const [first, carried] = createComment.mock.calls.map(([, , bounds]) => bounds.key);
    expect(carried).not.toBe(first);
  } finally {
    createComment.mockRestore();
  }
});

/** What a stopped direct message says once the reader has carried its draft elsewhere. */
const STOPPED_AS_DIRECT_MESSAGE =
  "Dispatch has not confirmed this message, sent as a direct message: it may have been delivered there. " +
  "Sending it from here is a new message, which can be delivered a second time if the first one was.";

// A send the reader stopped may have been delivered where it went, so its key is used only there.
// Carried by the reader's own move to another address, it goes on under a key nothing has gone
// out under, its refusal says the first may have been delivered, and a reload keeps all of it;
// carried back, it is the stopped send again, under its own key and in its own words.
test("a stopped send carried away goes on under a new key, and carried back is its own again", async () => {
  const tab = keptTab();
  await onFakeClock(async () => {
    const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(
      new ApiError(503, { code: "UNAVAILABLE", error: "down" })
    );
    const { held } = heldOf(tab);
    try {
      held.send(agentKey, agentSend);
      for (let left = SEND_DEADLINE_MS + 1_000; left > 0; left -= 500) {
        jest.advanceTimersByTime(500);
        await settle();
      }
      expect(held.stop(agentKey)).toBe(true);
      await settle();
      jest.advanceTimersByTime(60_000);
      await settle();

      held.carry(agentKey, agentSend, {
        owner: { issueKey: "CORE-1", kind: "issue" },
        replyTo: null,
      });
      const away = held.get(agentKey);
      expect(away?.request.owner).toEqual({ issueKey: "CORE-1", kind: "issue" });
      expect(away?.request.sendKey).not.toBe(agentSend.sendKey);
      expect(away?.status === "refused" && away.refusal.message).toBe(STOPPED_AS_DIRECT_MESSAGE);

      const { held: reloaded, writes } = heldOf(tab);
      writes.resume("alice");
      await settle();
      const kept = reloaded.get(agentKey);
      expect(kept?.request).toEqual(away?.request);
      expect(kept?.status === "refused" && kept.refusal.message).toBe(STOPPED_AS_DIRECT_MESSAGE);

      // Back from CORE-1's own address, where `request` goes, to the session.
      reloaded.carry(agentKey, request, { owner: agentSend.owner, replyTo: null });
      const back = reloaded.get(agentKey);
      expect(back?.request).toEqual(agentSend);
      expect(back?.status === "refused" && back.refusal.message).toBe(
        new SendStoppedError().message
      );
    } finally {
      createAgentMessage.mockRestore();
      tab.close();
    }
  });
});

/** The phone thread composer's send name, and the docked composer's, which the thread composer's
 *  Cancel reply carries its draft into (`HeldSends.carryInto`). */
const threadKey: MutationKey = ["conversation-composer", "CORE-1", "phone-thread"];
const dockedKey: MutationKey = ["conversation-composer", "CORE-1"];

// The reader's own move can end the composer a refusal was made in - the phone thread composer's
// Cancel reply - while another composer shows the address they took the draft to. The refusal
// goes into that one (`carryInto`): a press from there is its Retry, a new write under a key of
// its own, and nothing is left under the name it left. Whether that composer takes the draft is
// its own to say, before anything is carried: one holding a send of its own takes none
// (`DraftIntake`).
test("a refused reply carried into another composer goes from there", async () => {
  const createComment = spyOn(api, "createComment")
    .mockRejectedValueOnce(verdict)
    .mockResolvedValue({} as never);
  const { held } = heldOf();
  const sent = replySend("comment-1");
  try {
    held.send(threadKey, sent);
    await waitFor(() => expect(held.get(threadKey)?.status).toBe("refused"));
    held.carryInto(threadKey, dockedKey, { owner: sent.owner, replyTo: null });
    expect(held.get(threadKey)).toBeUndefined();
    expect(pressOver(held, dockedKey, shownAs(sent))).toBe(false);
    expect(pressOver(held, dockedKey, shownAs(sent, { replyTo: null }))).toBe(true);
    await waitFor(() => expect(held.get(dockedKey)).toBeUndefined());
    expect(createComment.mock.calls.map(([issue, input]) => [issue, input])).toEqual([
      ["CORE-1", { body: "Held", reply_to: "comment-1" }],
      ["CORE-1", { body: "Held" }],
    ]);
    const [first, carried] = createComment.mock.calls.map(([, , bounds]) => bounds.key);
    expect(carried).not.toBe(first);
  } finally {
    createComment.mockRestore();
  }
});

/** What a stopped reply says once the reader has carried its draft into another composer. */
const STOPPED_AS_REPLY =
  "Dispatch has not confirmed this message, sent as a reply: it may have been delivered there. " +
  "Sending it from here is a new message, which can be delivered a second time if the first one was.";

// A stopped reply may have been delivered where it went, whichever composer shows its draft next:
// carried into another composer, it goes on there under a key nothing has gone out under, and its
// refusal says the first may have been delivered.
test("a stopped reply carried into another composer goes on there under a new key", async () => {
  await onFakeClock(async () => {
    const createComment = spyOn(api, "createComment").mockRejectedValue(
      new ApiError(503, { code: "UNAVAILABLE", error: "down" })
    );
    const { held } = heldOf();
    const sent = replySend("comment-1");
    try {
      held.send(threadKey, sent);
      for (let left = SEND_DEADLINE_MS + 1_000; left > 0; left -= 500) {
        jest.advanceTimersByTime(500);
        await settle();
      }
      expect(held.stop(threadKey)).toBe(true);
      await settle();
      jest.advanceTimersByTime(60_000);
      await settle();

      held.carryInto(threadKey, dockedKey, { owner: sent.owner, replyTo: null });
      expect(held.get(threadKey)).toBeUndefined();
      const carried = held.get(dockedKey);
      expect(carried?.request.replyTo).toBeNull();
      expect(carried?.request.sendKey).not.toBe(sent.sendKey);
      expect(carried?.status === "refused" && carried.refusal.message).toBe(STOPPED_AS_REPLY);
    } finally {
      createComment.mockRestore();
    }
  });
});
