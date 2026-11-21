import { afterEach, expect, test } from "bun:test";
import { readdir, readFile } from "node:fs/promises";
import path from "node:path";
import {
  type Cleanup,
  messageStream,
  ompRoot,
  type Request,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";

// A turn killed in a provider retry's backoff, on the real Oh My Pi (R2). At the pin (.omp-pin,
// 18.18.6-sami.20261120-192530) the host writes the failed attempt into the transcript, emits
// `auto_retry_start` and sleeps (`#applyRetryPolicy`,
// packages/coding-agent/src/session/turn-recovery.ts:3396-3434); it notifies extensions of the
// run's end (`agent_end`) only once the run settles, with `willContinue` when a continuation
// follows (session/agent-session.ts:5495-5859, 6401-6408). The failed attempt alone reads as a turn
// that ended, so the plugin's marks are what tell the resumed session: one at each run's
// `agent_start`, which makes the transcript one the plugin marks, and one at a settle with no
// continuation to come. A host that renames or drops `agent_start` fails the backoff case here, and
// one that renames or drops `agent_end` fails the cancel case, instead of leaving the note's
// reading wrong in silence. LEGION_TEST_OMP names the binary, as in dispatch-first-omp.test.ts. A
// run without one skips, except on GitHub Actions.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** The plugin's turn mark (`TURN_MARK_ENTRY` in `../src/cut-turn`), written out rather than
 *  imported: it is what transcripts on disk hold, so a rename must fail this file. */
const TURN_MARK_ENTRY = "envoy-turn-mark";

/** An overloaded provider as the Messages API reports one mid-stream: a 200 whose one event is an
 *  error, which the host retries. */
const OVERLOADED = `event: error\ndata: ${JSON.stringify({
  type: "error",
  error: { type: "overloaded_error", message: "Overloaded" },
})}\n\n`;

/** The first sentence of the note (`cutTurnNoticeText`). */
const NOTE = "Your previous turn was interrupted.";

/** One transcript entry as Oh My Pi writes it. */
type Entry = Record<string, unknown>;

/** The session's newest transcript under `sessions`, and its entries: the one transcript before a
 *  resume, and the resumed process's after it. A process killed with SIGKILL can leave its last
 *  line cut short, so a last line that does not parse is left out. */
async function readTranscript(
  sessions: string
): Promise<{ readonly id: string; readonly entries: readonly Entry[] }> {
  // `<timestamp>_<sessionId>.jsonl`, which sorts by when each was made, and the name
  // `runLegionPane`'s resume reads the id from.
  const file = (await readdir(sessions, { recursive: true }))
    .filter((name) => name.endsWith(".jsonl"))
    .sort()
    .at(-1);
  if (file === undefined) throw new Error(`expected a transcript under ${sessions}, found none`);
  const id = /^[^_]+_(.+)\.jsonl$/.exec(path.basename(file))?.[1];
  if (id === undefined) throw new Error(`cannot read a session id from ${file}`);
  const lines = (await readFile(path.join(sessions, file), "utf8")).split("\n").filter(Boolean);
  const entries: Entry[] = [];
  for (const [index, line] of lines.entries()) {
    try {
      entries.push(JSON.parse(line) as Entry);
    } catch (error) {
      if (index !== lines.length - 1) throw error;
    }
  }
  return { id, entries };
}

/** The assistant messages of `entries`, oldest first. */
function assistantReplies(entries: readonly Entry[]): readonly Entry[] {
  const replies: Entry[] = [];
  for (const entry of entries) {
    const message = entry.message;
    if (entry.type !== "message" || typeof message !== "object" || message === null) continue;
    if ("role" in message && message.role === "assistant") replies.push(message as Entry);
  }
  return replies;
}

/** What one run killed in a retry left: the first process's transcript, the resumed process's, and
 *  the Messages request of the resumed session's turn. */
interface RetryRun {
  readonly left: readonly Entry[];
  readonly resumed: readonly Entry[];
  readonly resumedTurn: Request | undefined;
}

/**
 * Runs one Legion-driven session (`../test/legion-managed-session.ts` stands in for the claim) on
 * the real Oh My Pi: its first prompt meets an overloaded provider, and once the host has
 * announced the retry the process is killed with SIGKILL inside that retry's backoff, or, with
 * `cancel`, after the daemon's RPC `abort` cancelled the retry and the run settled. A second
 * process resumes the session and is given one prompt at once, as the daemon gives a relaunched
 * worker its task, which the provider now answers. Returns the transcript the first process left,
 * the one the resumed process left, and the Messages request of the resumed session's turn.
 */
async function killInRetry(binary: string, cancel: boolean): Promise<RetryRun> {
  const { home, workspace, sessions } = await ompRoot(binary, "cut-turn-omp-", cleanup);
  let overloaded = true;
  const { requests, base } = serveStandin(cleanup, (url) => {
    if (url.pathname === "/anthropic/v1/messages") {
      const body = overloaded
        ? OVERLOADED
        : messageStream([{ type: "text", text: "Carrying on." }], `msg_${requests.length}`);
      return new Response(body, { headers: { "content-type": "text/event-stream" } });
    }
    return Response.json({ data: [] });
  });
  // Each backoff is the 8 s cap less up to a quarter of jitter (`calculateRetryBackoffDelayMs`,
  // packages/coding-agent/src/session/retry-fallback-chains.ts:91-101 at the pin): at least 6 s
  // to see the failed attempt reach the file and kill the process inside the backoff.
  await writeStandinProfile(home, base, ["retry:", "  baseDelayMs: 8000"]);
  const envoy = path.join(import.meta.dir, "envoy.ts");

  const announced = Promise.withResolvers<void>();
  const ended = Promise.withResolvers<void>();
  const first = spawnRpc(
    binary,
    {
      extensions: [path.join(import.meta.dir, "../test/legion-managed-session.ts"), envoy],
      home,
      workspace,
      sessions,
      env: {},
      onFrame: (frame) => {
        if (!("type" in frame)) return;
        if (frame.type === "auto_retry_start") announced.resolve();
        if (frame.type === "auto_retry_end") ended.resolve();
      },
    },
    cleanup
  );
  first.send({ type: "prompt", message: "Take the issue through its phase." });
  await Promise.race([announced.promise, first.closed]);
  if (cancel) {
    first.send({ type: "abort" });
    await Promise.race([ended.promise, first.closed]);
  }
  // Wait for what the kill comes after to reach the file: in the backoff the failed attempt, which
  // the host saved before it announced the retry, and after a cancel the end mark the run's settle
  // leaves. A plugin build that writes no end mark waits the whole bound. A real clock,
  // deliberately: the host announces no write of its file, so only reading the file shows it, and
  // the 4 s bound keeps the kill inside the shortest backoff.
  const settledBy = Date.now() + 4_000;
  while (Date.now() < settledBy) {
    const written = (await readdir(sessions, { recursive: true })).some((file) =>
      file.endsWith(".jsonl")
    );
    const entries = written ? (await readTranscript(sessions)).entries : [];
    const mark = entries.filter((entry) => entry.customType === TURN_MARK_ENTRY).at(-1)?.data;
    const ended = typeof mark === "object" && mark !== null && "ended" in mark && mark.ended;
    if (cancel ? ended === true : assistantReplies(entries).at(-1)?.stopReason === "error") break;
    await Bun.sleep(100);
  }
  first.kill("SIGKILL");
  await first.exited;
  const left = (await readTranscript(sessions)).entries;

  overloaded = false;
  const settled = Promise.withResolvers<void>();
  const second = spawnRpc(
    binary,
    {
      extensions: [envoy],
      home,
      workspace,
      sessions,
      env: {},
      args: ["--resume", (await readTranscript(sessions)).id],
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
  const before = requests.length;
  second.send({ type: "prompt", message: "Carry on." });
  await Promise.race([settled.promise, second.closed]);
  await second.end();
  const resumedTurn = requests
    .slice(before)
    .find((request) => request.path === "/anthropic/v1/messages");
  return { left, resumed: (await readTranscript(sessions)).entries, resumedTurn };
}

/** The one run killed in the backoff that the first two cases read, made once: at the pin each run
 *  waits out the provider's own ten retries of the overloaded stream (about 50 s) before the host
 *  announces the retry the kill comes in, and both cases read only what the run returned. */
let killedInBackoff: Promise<RetryRun> | undefined;

test.skipIf(omp === undefined && !onActions)(
  "a turn killed in a provider retry's backoff gets the note before the resumed session's first turn",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    killedInBackoff ??= killInRetry(omp, false);
    const { left, resumedTurn } = await killedInBackoff;

    // The process ended in the retry: the newest reply the host wrote is the failed attempt, which
    // alone reads as a turn that ended.
    expect(assistantReplies(left).at(-1)).toMatchObject({
      stopReason: "error",
      errorMessage: expect.stringContaining("Overloaded"),
    });
    expect(resumedTurn).toBeDefined();
    expect(JSON.stringify(resumedTurn?.body)).toContain(NOTE);
  },
  180_000
);

// R2 places the note before the first turn the resumed session runs, whatever starts it: in the
// transcript and in what the model reads, ahead of that turn's opening prompt. Here the daemon's
// task starts it, sent as an RPC `prompt` the moment the process starts, as a relaunched worker
// is given its task.
test.skipIf(omp === undefined && !onActions)(
  "the note is written ahead of the task that starts the resumed session's first turn",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    killedInBackoff ??= killInRetry(omp, false);
    const { resumed, resumedTurn } = await killedInBackoff;

    const lines = resumed.map((entry) => JSON.stringify(entry));
    const note = lines.findIndex((line) => line.includes(NOTE) && line.includes("custom_message"));
    const task = lines.findIndex(
      (line) => line.includes("Carry on.") && line.includes('"role":"user"')
    );
    expect(note).toBeGreaterThanOrEqual(0);
    expect(task).toBeGreaterThanOrEqual(0);
    expect(note).toBeLessThan(task);
    // What the model read: the note, then the task.
    const request = JSON.stringify(resumedTurn?.body);
    expect(request.indexOf(NOTE)).toBeGreaterThanOrEqual(0);
    expect(request.indexOf(NOTE)).toBeLessThan(request.indexOf("Carry on."));
  },
  180_000
);

test.skipIf(omp === undefined && !onActions)(
  "a provider retry the daemon's abort cancelled leaves an ended turn, and no note",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const { left, resumedTurn } = await killInRetry(omp, true);

    // The cancel ended the retry: at the pin the host writes the cancel itself as the run's last
    // reply, an aborted one, right after the failed attempt it cancelled the retry of.
    const replies = assistantReplies(left);
    expect(replies.at(-1)).toMatchObject({
      stopReason: "aborted",
      errorMessage: "Retry cancelled",
    });
    expect(replies.at(-2)).toMatchObject({
      stopReason: "error",
      errorMessage: expect.stringContaining("Overloaded"),
    });
    // The cancel settled the run with no continuation to come, and its end mark says so.
    expect(left.filter((entry) => entry.customType === TURN_MARK_ENTRY).at(-1)).toMatchObject({
      data: { ended: true },
    });
    expect(resumedTurn).toBeDefined();
    expect(JSON.stringify(resumedTurn?.body)).not.toContain(NOTE);
  },
  180_000
);
