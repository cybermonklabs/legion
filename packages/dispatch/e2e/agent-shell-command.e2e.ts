import { expect, test } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import { createAgentMessage, publishAgentStreamFrame, replyToMessageDelivery } from "./api";
import { frontLiveView } from "./live-views";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * A person's `!command` in the live view (INFRA-812, R4; the browser cases of the Verification
 * document's k6, dispatch://INFRA-812/artifact/verification-md).
 *
 * The session runs the line itself and publishes one user message frame carrying the person's
 * line, what the command printed and how it ended (`packages/pi-envoy/src/typed-shell.ts`'s
 * `shellCommandReport`, recorded by `extensions/envoy.ts`): a user message, because the person
 * ran it and the host wrote nothing. The frame carries the Dispatch message id of the message
 * that asked for it, so the view draws the frame over the person's own Dispatch turn rather
 * than beside it, and the `!` line is read once. The plugin also posts that report as the
 * session's reply to the person's message, as it does for every message a session is sent, so
 * the same text reaches the view from both sides.
 *
 * These rows seed all three - Dispatch's stored message, the session's frame and that reply -
 * because what the view does with them together is what the claim is about. Each is spelt here
 * as the wire carries it, never imported from the SPA or the plugin, so this file reads the
 * same against the commit before the change and each row fails there on its own assertion.
 */

/** The session these rows read: a UUID, which Dispatch's agent routes require, and this file's
 *  own, since the relay's replay is process-global. */
const SHELL_ID = "01a0e812-0004-7000-8000-000000000001";

/** The person's message. A command that printed something and then failed, so one frame carries
 *  all three things the check reads: the command, its output and its exit status. Its text is
 *  plain: the transcript renders a turn as Markdown, and a line of shell punctuation would be
 *  read as markup rather than as the person's own words. */
const COMMAND_LINE = "!cat missing.txt";

/** What the command printed, which the shell writes to its error stream and the plugin carries
 *  with the rest. */
const OUTPUT = "cat: missing.txt: No such file or directory";

/** What the plugin publishes once the command has run: the person's line, what it printed as a
 *  fenced block, and the sentence a command that did not exit 0 ends with. It reaches the view
 *  twice - this frame, and the session's reply on Dispatch, which every message to a session
 *  gets (`postTypedInputReply` in `packages/pi-envoy/extensions/envoy.ts`) - and the reader
 *  must meet it once. */
const REPORT = `${COMMAND_LINE}\n\n\`\`\`\n${OUTPUT}\n\`\`\`\n\nThe command ended with exit status 1.`;

/** The person's second message, whose mark is followed by a space. The session runs the trimmed
 *  command, as the CLI does, and opens its report with the line as the person sent it, that
 *  space kept (`shellCommandReport` in `packages/pi-envoy/src/typed-shell.ts`): a report is read
 *  back against the message's own stored body (`isShellCommandReport`), so one opening with the
 *  trimmed `!ls` would leave the view drawing this line twice. */
const SPACED_LINE = "! ls";
const SPACED_OUTPUT = "one.txt";
const SPACED_REPORT = `${SPACED_LINE}\n\n\`\`\`\n${SPACED_OUTPUT}\n\`\`\``;

/** One `!` exchange as the wire carries it: the person's line, and the report the session
 *  answers it with - in its frame, and in its reply on Dispatch. */
interface ShellExchange {
  readonly line: string;
  readonly report: string;
}

const FAILED: ShellExchange = { line: COMMAND_LINE, report: REPORT };
const SPACED: ShellExchange = { line: SPACED_LINE, report: SPACED_REPORT };

function livePath(sessionID: string): string {
  return `/agents/${sessionID}/live`;
}

/** The live session the person talks to. `last_seen` is taken when the row seeds, never at this
 *  module's scope, which a worker evaluates once and every later spec of that worker ages. */
function shellSession(): FakeSession {
  return {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/shell",
    last_seen: Date.now() - 5_000,
    machine_id: "shell-host",
    roles: [],
    session_id: SHELL_ID,
    title: "Shell",
  };
}

/**
 * Seeds the exchanges `!` messages make, as production holds them: each person's Dispatch
 * message, the session's user message frame carrying that message's report and its id, and the
 * session's reply to it, which carries the same report once more. Every frame goes in one
 * replay, which is what the page asks the session for when it opens. Answers the Dispatch
 * message ids in the order the exchanges were given, which are the keys the view must draw each
 * pair under.
 */
async function seedShellCommands(exchanges: readonly ShellExchange[]): Promise<readonly string[]> {
  await setLiveSessions([shellSession()]);
  const seeded: { readonly exchange: ShellExchange; readonly id: string }[] = [];
  for (const exchange of exchanges) {
    const asked = await createAgentMessage(SHELL_ID, { body: exchange.line, delivery: "steer" });
    seeded.push({ exchange, id: asked.id });
  }
  await publishAgentStreamFrame(
    SHELL_ID,
    {
      frames: seeded.map(({ exchange, id }, index) => ({
        kind: "message",
        message: {
          at: 1_000 * (index + 1),
          dispatchMessageId: id,
          id: `u${index + 1}000`,
          parts: [{ text: exchange.report, type: "text" }],
          role: "user",
          streaming: false,
        },
        seq: index + 1,
        v: 1,
      })),
      session_id: SHELL_ID,
      v: 1,
    },
    "replay"
  );
  // The plugin answers each of these messages on Dispatch too, with the report it published: the
  // view has the same text from both sides, and draws it once.
  for (const { exchange, id } of seeded) {
    await replyToMessageDelivery(
      id,
      { attempt: 1, body: exchange.report },
      { id: SHELL_ID, kind: "session" }
    );
  }
  return seeded.map(({ id }) => id);
}

test.beforeEach(async () => {
  await resetDatabase();
});

// R4. What a person reads after sending `!cat missing.txt`: their own line,
// what the command printed, and how it ended - all three as their own message, on their own
// side of the conversation, never as something the agent said - including the session's reply
// on Dispatch, which carries that same report and is drawn nowhere.
// SLOW(>=2s): a live view opened on the session's replay, over a Dispatch message and a reply
// the row stored first.
test("the live view shows a person's shell command, its output and its exit status as their own message", async ({
  browser,
}) => {
  await seedShellCommands([FAILED]);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(livePath(SHELL_ID));
    const own = frontLiveView(page).getByTestId("agent-message-user");
    await expect(own).toContainText(COMMAND_LINE);
    await expect(own).toContainText(OUTPUT);
    await expect(own).toContainText("The command ended with exit status 1.");

    // Theirs, not the session's: nothing here is drawn as a turn the agent took, neither a
    // streamed one nor the session's Dispatch reply saying it all again, and nothing names
    // another author.
    await expect(frontLiveView(page).getByTestId("agent-message-assistant")).toHaveCount(0);
    await expect(frontLiveView(page).getByTestId("agent-dispatch-reply")).toHaveCount(0);
    await expect(frontLiveView(page).getByTestId("agent-message-other")).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// R4, D12. The gap row of the plan: the `!` line shows once, not twice. The frame, the Dispatch
// message it names and the session's reply carrying the same report are one message in the
// conversation, and the key that message is drawn under is Dispatch's own - which is what
// re-keying the frame over the person's turn means, and what keeps them from being drawn one
// under the other. The second message's mark is followed by a space, which the session keeps in
// its report: the line a person sent is the line the view reads that report back against, so
// `! ls` is drawn once too - their line, the report, and no third copy.
// SLOW(>=2s): a live view opened on the session's replay, over two Dispatch messages and the
// replies the row stored first.
test("a shell command's frame and the Dispatch message it answers are one message, not two", async ({
  browser,
}) => {
  const [failedID = "", spacedID = ""] = await seedShellCommands([FAILED, SPACED]);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(livePath(SHELL_ID));
    const own = frontLiveView(page).getByTestId("agent-message-user");
    await expect(own).toHaveCount(2);
    // Each turn is the frame - it carries what the command printed, which Dispatch's stored
    // copy of the person's line does not - under the key the stored message would have had.
    await expect(own.first()).toContainText(OUTPUT);
    await expect(own.first()).toHaveAttribute("data-message-id", `dispatch:${failedID}`);
    await expect(own.last()).toContainText(SPACED_OUTPUT);
    await expect(own.last()).toHaveAttribute("data-message-id", `dispatch:${spacedID}`);
    // Neither report is drawn a third time as the session's own reply.
    await expect(frontLiveView(page).getByTestId("agent-dispatch-reply")).toHaveCount(0);

    // Counted across the whole transcript rather than one test id: each line is in one turn.
    const counts = await frontLiveView(page)
      .getByTestId("agent-thread")
      .evaluate(
        (thread, lines) => {
          const turns = [...thread.querySelectorAll<HTMLElement>("[data-message-id]")];
          return lines.map(
            (line) => turns.filter((turn) => (turn.textContent ?? "").includes(line)).length
          );
        },
        [COMMAND_LINE, SPACED_LINE]
      );
    expect(counts).toEqual([1, 1]);
  } finally {
    await context.close();
  }
});
