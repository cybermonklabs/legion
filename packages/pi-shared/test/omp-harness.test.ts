import { expect, test } from "bun:test";
import { isSelfCheck, newestMessage, ownMessages } from "./omp-harness";

// The host's date/cwd reminder as the pinned Oh My Pi sent it in the stall watch's live proof, with
// the pane's local date moved on between two requests (the dump of that run, 2026-11-22).
const REMINDER =
  "<system-reminder>\nToday: 2026-11-23; current working directory: '/scratch/workspace'. Do not repeat this information in your reply.\n</system-reminder>";

const RESTART =
  "Your turn was ended because its `eval` call (toolu_msg_2_0) had been running 13 seconds with no result.";

/** A Messages request whose messages are `messages`. */
function body(...messages: unknown[]): Record<string, unknown> {
  return { messages };
}

test("the reminder sent as a message of its own after the newest message is not the newest message", () => {
  // The restart turn's request in that run: the agent's own message, then the reminder alone.
  const request = body(
    { role: "user", content: [{ type: "tool_result", tool_use_id: "toolu_1", content: "ok" }] },
    { role: "user", content: [{ type: "text", text: RESTART }] },
    {
      role: "user",
      content: [{ type: "text", text: REMINDER, cache_control: { type: "ephemeral" } }],
    }
  );
  expect(newestMessage(request)).toContain("Your turn was ended because");
  expect(ownMessages(request)).toHaveLength(2);
});

test("the reminder ahead of a user turn is taken off, in each form the host sends it", () => {
  // Joined into one text, as the host sends a text-only model a message's blocks.
  expect(
    ownMessages(
      body({ role: "user", content: [{ type: "text", text: `${REMINDER}\nSecond question.` }] })
    )
  ).toEqual([{ role: "user", content: [{ type: "text", text: "Second question." }] }]);
  // A string content, prefixed with a blank line.
  expect(ownMessages(body({ role: "user", content: `${REMINDER}\n\nSecond question.` }))).toEqual([
    { role: "user", content: "Second question." },
  ]);
  // A text block of its own ahead of the turn's blocks.
  expect(
    ownMessages(
      body({
        role: "user",
        content: [
          { type: "text", text: REMINDER },
          { type: "text", text: "Second question." },
        ],
      })
    )
  ).toEqual([{ role: "user", content: [{ type: "text", text: "Second question." }] }]);
});

test("a message that only quotes a reminder, or carries none, is left as it is", () => {
  const quoted = { role: "user", content: [{ type: "text", text: `Look: ${REMINDER}` }] };
  const plain = { role: "assistant", content: [{ type: "text", text: "Done." }] };
  expect(ownMessages(body(quoted, plain))).toEqual([quoted, plain]);
  expect(newestMessage(body())).toBe("null");
});

test("a side question is a side question when the host sends its reminder after it", () => {
  const question = {
    role: "user",
    content: [{ type: "text", text: "<btw>\nWhat is up?\n</btw>" }],
  };
  const reminder = { role: "user", content: [{ type: "text", text: REMINDER }] };
  const turn = { role: "user", content: [{ type: "text", text: "Go on." }] };
  const path = "/anthropic/v1/messages";
  expect(isSelfCheck({ path, body: body(question, reminder) })).toBe(true);
  // A turn after a side question's exchange is no side question.
  expect(isSelfCheck({ path, body: body(question, turn, reminder) })).toBe(false);
});
