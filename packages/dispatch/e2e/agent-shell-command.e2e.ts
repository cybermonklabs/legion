import { expect, test } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import { createAgentMessage, publishAgentStreamFrame } from "./api";
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
 * than beside it, and the `!` line is read once.
 *
 * These rows seed both halves - Dispatch's stored message and the session's frame - because
 * that pair is what the claim is about. Both are spelt here as the wire carries them, never
 * imported from the SPA or the plugin, so this file reads the same against the commit before
 * the change and each row fails there on its own assertion.
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

/** What the plugin publishes once the command has run: the person's line, what it printed, and
 *  the sentence a command that did not exit 0 ends with. */
const REPORT = `${COMMAND_LINE}\n\n${OUTPUT}\n\nThe command ended with exit status 1.`;

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
 * Seeds the exchange one `!` message makes: the person's Dispatch message, and the session's
 * user message frame carrying the report and that message's id. Answers the Dispatch message's
 * id, which is the key the view must draw the pair under.
 */
async function seedShellCommand(): Promise<string> {
  await setLiveSessions([shellSession()]);
  const asked = await createAgentMessage(SHELL_ID, { body: COMMAND_LINE, delivery: "steer" });
  await publishAgentStreamFrame(
    SHELL_ID,
    {
      frames: [
        {
          kind: "message",
          message: {
            at: 1_000,
            dispatchMessageId: asked.id,
            id: "u1000",
            parts: [{ text: REPORT, type: "text" }],
            role: "user",
            streaming: false,
          },
          seq: 1,
          v: 1,
        },
      ],
      session_id: SHELL_ID,
      v: 1,
    },
    "replay"
  );
  return asked.id;
}

test.beforeEach(async () => {
  await resetDatabase();
});

// R4. What a person reads after sending `!cat missing.txt`: their own line,
// what the command printed, and how it ended - all three as their own message, on their own
// side of the conversation, never as something the agent said.
// SLOW(>=2s): a live view opened on the session's replay, over a Dispatch message the row
// stored first.
test("the live view shows a person's shell command, its output and its exit status as their own message", async ({
  browser,
}) => {
  await seedShellCommand();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(livePath(SHELL_ID));
    const own = frontLiveView(page).getByTestId("agent-message-user");
    await expect(own).toContainText(COMMAND_LINE);
    await expect(own).toContainText(OUTPUT);
    await expect(own).toContainText("The command ended with exit status 1.");

    // Theirs, not the session's: nothing here is drawn as a turn the agent took, and nothing
    // names another author.
    await expect(frontLiveView(page).getByTestId("agent-message-assistant")).toHaveCount(0);
    await expect(frontLiveView(page).getByTestId("agent-message-other")).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// R4, D12. The gap row of the plan: the `!` line shows once, not twice. The frame and the
// Dispatch message it names are one message in the conversation, and the key that message is
// drawn under is Dispatch's own - which is what re-keying the frame over the person's turn
// means, and what keeps the two from being drawn one under the other.
// SLOW(>=2s): a live view opened on the session's replay, over a Dispatch message the row
// stored first.
test("a shell command's frame and the Dispatch message it answers are one message, not two", async ({
  browser,
}) => {
  const messageID = await seedShellCommand();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(livePath(SHELL_ID));
    const own = frontLiveView(page).getByTestId("agent-message-user");
    await expect(own).toHaveCount(1);
    // The one turn is the frame - it carries what the command printed, which Dispatch's stored
    // copy of the person's line does not - under the key the stored message would have had.
    await expect(own).toContainText(OUTPUT);
    await expect(own).toHaveAttribute("data-message-id", `dispatch:${messageID}`);

    // Counted across the whole transcript rather than one test id: the line appears in one turn.
    const lines = await frontLiveView(page)
      .getByTestId("agent-thread")
      .evaluate(
        (thread, line) =>
          [...thread.querySelectorAll<HTMLElement>("[data-message-id]")].filter((turn) =>
            (turn.textContent ?? "").includes(line)
          ).length,
        COMMAND_LINE
      );
    expect(lines).toBe(1);
  } finally {
    await context.close();
  }
});
