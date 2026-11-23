import { afterEach, expect, test } from "bun:test";
import { writeFile } from "node:fs/promises";
import * as path from "node:path";
import {
  type Cleanup,
  messageStream,
  ompRoot,
  type Request,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";

// pi-envoy sends a person's accepted Send or Aside with `origin: "outside"` (`InputOrigin`), which
// a host that tells input sources apart reads to end its cyber-policy hold. A host built before the
// option must still run the turn, ignoring the field. This runs the real Oh My Pi of the
// repository's .omp-pin, which has no such option, through both calls pi-envoy makes. The stand-in
// extension below makes them exactly as `extensions/envoy.ts` does for a person's turn.
// LEGION_TEST_OMP names the binary, as in dispatch-first-omp.test.ts. A run without one skips,
// except on GitHub Actions.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";

/** The Dispatch message id the typed input is tagged with, as pi-envoy tags a person's turn. */
const TAG = "origin-omp-message";

/**
 * `/send-outside <text>` sends the text with `pi.sendUserMessage`, and `/input-outside <text>` with
 * `pi.sendUserInput`, each with `origin: "outside"`. The typed input is not awaited, as pi-envoy
 * never awaits it: on an idle session the host answers only once the run it started ends.
 */
const SENDER = `export default function sender(pi) {
  pi.registerCommand("send-outside", {
    description: "Send a user message from outside the session",
    handler: async (args) => {
      pi.sendUserMessage(args, { origin: "outside" });
    },
  });
  pi.registerCommand("input-outside", {
    description: "Run typed input from outside the session",
    handler: async (args) => {
      if (typeof pi.sendUserInput !== "function") throw new Error("this host has no sendUserInput");
      void pi.sendUserInput(args, { tag: "${TAG}", origin: "outside" });
    },
  });
}
`;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** The last line of a Messages request's last user message: the host puts its own reminder lines
 *  before the first turn's text. */
function lastUserLine(request: Request): string {
  const messages: unknown[] = Array.isArray(request.body.messages) ? request.body.messages : [];
  const last = messages
    .filter(
      (message): message is { role: string; content: unknown } =>
        typeof message === "object" && message !== null && "role" in message
    )
    .filter((message) => message.role === "user")
    .at(-1);
  const text =
    typeof last?.content === "string"
      ? last.content
      : Array.isArray(last?.content)
        ? last.content
            .map((block: unknown) =>
              typeof block === "object" && block !== null && "text" in block
                ? String(block.text)
                : ""
            )
            .join("\n")
        : "";
  return text.split("\n").at(-1) ?? "";
}

test.skipIf(omp === undefined && !onActions)(
  "a host without the origin option runs a person's turn sent with origin outside",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP names no Oh My Pi binary");
    const { home, workspace, sessions, root } = await ompRoot(
      omp,
      "user-turn-origin-omp-",
      cleanup
    );

    let answered = 0;
    const { requests, base } = serveStandin(cleanup, (url) => {
      if (url.pathname === "/anthropic/v1/messages") {
        answered += 1;
        return new Response(
          messageStream([{ type: "text", text: `Answer ${answered}.` }], `msg_${answered}`),
          { headers: { "content-type": "text/event-stream" } }
        );
      }
      return Response.json({ data: [] });
    });
    await writeStandinProfile(home, base);
    const sender = path.join(root, "origin-sender.ts");
    await writeFile(sender, SENDER);

    // A command's turn has settled at its terminal agent_end.
    let settled = Promise.withResolvers<void>();
    const rpc = spawnRpc(
      omp,
      {
        extensions: [sender],
        home,
        workspace,
        sessions,
        env: {},
        onFrame: (frame) => {
          if (
            "type" in frame &&
            frame.type === "agent_end" &&
            !("isTerminal" in frame && frame.isTerminal === false)
          ) {
            settled.resolve();
          }
        },
      },
      cleanup
    );
    for (const command of [
      "/send-outside A person's Send.",
      "/input-outside A person's typed Send.",
    ]) {
      settled = Promise.withResolvers<void>();
      rpc.send({ type: "prompt", message: command });
      await Promise.race([settled.promise, rpc.closed]);
    }
    await rpc.end();

    const turns = requests.filter((request) => request.path === "/anthropic/v1/messages");
    expect(turns.map(lastUserLine)).toEqual(["A person's Send.", "A person's typed Send."]);
  },
  120_000
);
