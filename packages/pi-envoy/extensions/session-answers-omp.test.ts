import { afterEach, expect, test } from "bun:test";
import { appendFile, link, rm, writeFile } from "node:fs/promises";
import * as path from "node:path";
import type { SessionAnswer } from "@legion/contracts";
import { DISPATCH_FIRST_MARKER } from "@legion/envoy-client/dispatch-first";
import {
  isSessionAnswersText,
  renderSessionAnswers,
  SESSION_ANSWERS_DEADLINE_MS,
  SESSION_ANSWERS_MARKER,
  sessionAnswersFailureLine,
} from "@legion/envoy-client/session-answers";
import {
  type Block,
  type Cleanup,
  deliveryFor,
  messageStream,
  ompRoot,
  type Request,
  resumeId,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { requireOrSkip } from "@legion/test-fixtures/host-tools";

// What people wrote to a session, on every request of the real Oh My Pi's main loop
// (src/session-answers-context.ts), and on none of the other requests its
// `context` handler runs for: a side turn, the handoff prompt and a remote compaction's request.
// Oh My Pi keeps nothing a `context` handler returns, so what reaches the model, where the list
// sits and where the binary puts its cache anchors can only be read off the requests it sends. One
// stand-in is the model gateway, the Envoy listener and Dispatch, whose answers route serves a list
// each case controls. The file also holds one case on Envoy cards, (s), because it needs the same
// harness, its deliveries and its restart: the `delivered:` line src/card-age.ts adds to a card
// keeps its bytes across a resume. LEGION_TEST_OMP names the binary, as in
// dispatch-first-omp.test.ts. A run without one skips, except on GitHub Actions.
const omp = process.env.LEGION_TEST_OMP;
/** The Dispatch token the session is handed. */
const DISPATCH_TOKEN = fixtureSecret("dsp");

/** What the stand-in's answers route does: list `items` (after `delayMs`, at once when unset),
 *  accept and never answer, answer as a Dispatch build older than the route does, or answer
 *  `status` with Dispatch's JSON error body. */
type Route =
  | {
      readonly kind: "list";
      readonly items: readonly SessionAnswer[];
      readonly delayMs?: number;
    }
  | { readonly kind: "hang" }
  | { readonly kind: "missing" }
  | { readonly kind: "error"; readonly status: number };

/** One thing the session is told over RPC, in order, or a change in the answers route before the
 *  next one: a prompt, a manual compaction, a `/btw` side question, a handoff, or an Envoy
 *  delivery (`DELIVERER`) that starts a turn or, quiet, only joins the conversation. A restart
 *  ends omp and starts it again over the same session (`--resume`), as a pod's next life resumes
 *  the session its idle release left. */
type Step =
  | { readonly prompt: string }
  | { readonly compact: true }
  | { readonly btw: string }
  | { readonly handoff: true }
  | { readonly deliver: string; readonly quiet?: true }
  | { readonly route: Route }
  | { readonly restart: true };

/**
 * An extension standing in for an Envoy delivery, which the stand-in's Envoy listener cannot
 * send (pi-envoy reads deliveries off NATS): `/deliver <text>` sends an `envoy-message` as
 * `extensions/envoy.ts` sends a delivery, `deliverAs: "steer"` and `triggerTurn: true`, and
 * `/deliver-quiet <text>` the same with no turn. The host queues a send and `sendMessage`
 * returns nothing to await, so the quiet command polls for the condition itself: it returns once
 * the branch holds one more entry with the text than before it sent, the text as an entry's JSON
 * holds it (a newline escaped), so a text of several lines and one the branch holds already are
 * waited for too.
 */
const DELIVERER = `export default function deliverer(pi) {
  for (const [name, triggerTurn] of [["deliver", true], ["deliver-quiet", false]]) {
    pi.registerCommand(name, {
      description: "Send an envoy-message",
      handler: async (args, context) => {
        const encoded = JSON.stringify(args).slice(1, -1);
        const holding = () =>
          context.sessionManager
            .getBranch()
            .filter((entry) => entry.type === "custom_message" && JSON.stringify(entry).includes(encoded))
            .length;
        const before = holding();
        pi.sendMessage(
          { customType: "envoy-message", content: args, display: true },
          { deliverAs: "steer", triggerTurn }
        );
        while (!triggerTurn && holding() === before) {
          await Bun.sleep(10);
        }
      },
    });
  }
}
`;

/** A request the stand-in served, with when it arrived (`performance.now()`). */
interface Seen extends Request {
  readonly at: number;
}

/** The assignment a case's `task` call hands its subagent, which tells its requests apart. */
const ASSIGNMENT = "SUBAGENT-ASSIGNMENT: report what you were told.";

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** Whether a Messages request is one of a compaction's summary calls, sent under their own system prompt. */
function isSummarization(request: Request): boolean {
  return JSON.stringify(request.body.system ?? null).includes(
    "Summarize user–AI coding-assistant conversations"
  );
}

/** Whether a Messages request is a turn of a conversation, the parent's or a subagent's: one that
 *  offers the agent's tools. The binary's side calls (a compaction summary, a subagent's label, a
 *  skill's routing hint) offer none. */
function isTurn(request: Request): boolean {
  return (
    Array.isArray(request.body.tools) && request.body.tools.length > 0 && !isSummarization(request)
  );
}

/** Whether a turn is the subagent's: it carries the assignment and none of the parent's conversation. */
function isSubagentTurn(request: Request): boolean {
  const messages = JSON.stringify(request.body.messages);
  return messages.includes(ASSIGNMENT) && !messages.includes("First question.");
}

/** Whether a Messages request is a `/btw` side turn: one of its messages is the /btw prompt, which
 *  never enters the conversation. Not only the last, so a list put after it fails the case's
 *  assertions rather than its wait for the side turn. */
function isSideTurn(request: Request): boolean {
  return messagesOf(request).some((message) => textOf(message).includes("<btw>"));
}

/** Whether a Messages request is Anthropic remote compaction's: it carries `compaction`. */
function isRemoteCompaction(request: Request): boolean {
  return "compaction" in request.body;
}

/** Whether a Messages request is the handoff's: one of its messages is the host's handoff prompt,
 *  which never enters the conversation. */
function isHandoff(request: Request): boolean {
  return messagesOf(request).some((message) =>
    textOf(message).includes("Write a handoff document for another instance of yourself.")
  );
}

/** Whether a Messages request is a turn of the main loop: a turn that is none of the above. */
function isMainTurn(request: Request): boolean {
  return (
    isTurn(request) && !isSideTurn(request) && !isRemoteCompaction(request) && !isHandoff(request)
  );
}

function messagesOf(request: Request): unknown[] {
  return Array.isArray(request.body.messages) ? request.body.messages : [];
}

/** The text of a message as the binary sent it: string content, or its text blocks joined. */
function textOf(message: unknown): string {
  if (typeof message !== "object" || message === null || !("content" in message)) return "";
  const { content } = message;
  if (typeof content === "string") return content;
  return (Array.isArray(content) ? content : [])
    .map((block: unknown) =>
      typeof block === "object" &&
      block !== null &&
      "text" in block &&
      typeof block.text === "string"
        ? block.text
        : ""
    )
    .join("");
}

// What the extension appends, as literals of this file rather than imports of its exports: the
// same file runs against an older build of the extension, which does not export them, and must
// fail there on the bytes a request carries rather than on an import.

/** The sentence each list a request appends carries after the list, as its second text block. */
const REPLACES =
  "This list replaces every earlier list of people's answers in this conversation, and every earlier line saying the list could not be read: read only the newest one as current.";

/** The list a request appends when an item of the newest list sent was withdrawn and the read
 *  has no list to give: its first block. */
const NO_ITEM = `${SESSION_ANSWERS_MARKER}\n\nNo item: every item an earlier list showed was withdrawn or is now older than 72 hours.`;

/** How the failure line, which stands in for a list that could not be read, begins. */
const FAILURE_MARKER = "[Dispatch: people's answers from the last 72 hours could not be read (";

/** A list's text as a request carries it: the stand-in model takes text only (`input: [text]`),
 *  and the binary sends such a model a message's text blocks joined with a newline in one text,
 *  so the list arrives as `list`, a newline and the sentence. */
function sentList(list: string | undefined): string {
  if (list === undefined) throw new Error("the list renders nothing");
  return `${list}\n${REPLACES}`;
}

/** The request's one answers message, a list (`sentList`) or the failure line, a user message of
 *  its own with no cache anchor, and its text as the request carries it. */
function listOf(request: Request): string {
  const lists = messagesOf(request).filter((message) => isSessionAnswersText(textOf(message)));
  expect(lists).toHaveLength(1);
  const [list] = lists;
  expect(list).toMatchObject({ role: "user" });
  expect(JSON.stringify(list)).not.toContain("cache_control");
  return textOf(list);
}

/** The request's list, which must be its last message: where a list that shows something new goes. */
function lastListOf(request: Request): string {
  expect(isSessionAnswersText(textOf(messagesOf(request).at(-1)))).toBe(true);
  return listOf(request);
}

/** Every answers message of the request, a list or the failure line, in order: its index among the
 *  request's messages and its text. */
function listsOf(request: Request): { readonly index: number; readonly text: string }[] {
  return messagesOf(request).flatMap((message, index) =>
    isSessionAnswersText(textOf(message)) ? [{ index, text: textOf(message) }] : []
  );
}

/** Whether any message of the request is a list or the failure line. */
function carriesList(request: Request): boolean {
  return messagesOf(request).some((message) => isSessionAnswersText(textOf(message)));
}

/** The request's messages without its answers messages. */
function withoutList(request: Request): unknown[] {
  return messagesOf(request).filter((message) => !isSessionAnswersText(textOf(message)));
}

/** Messages as JSON with their cache anchors set aside: the binary sends an anchored text message
 *  as a block with `cache_control` and an unanchored one as a plain string, which the Messages API
 *  reads as that one text block, so both are written as blocks and the anchors dropped. */
function bare(messages: readonly unknown[]): string {
  return JSON.stringify(
    messages.map((message) =>
      typeof message === "object" && message !== null && "content" in message
        ? {
            ...message,
            content:
              typeof message.content === "string"
                ? [{ type: "text", text: message.content }]
                : message.content,
          }
        : message
    )
  ).replaceAll(/,?"cache_control":\{[^}]*\}/g, "");
}

/**
 * Checks that each of `requests` begins with every message of the one before it, the same content
 * once the cache anchors are set aside (`bare`): the prefix the next request reads from the prompt
 * cache. When a pair differs, the first message that differs is printed from both before the check
 * fails, so a failure names the message that changed.
 */
function expectPrefixes(requests: readonly Request[]): void {
  for (const [index, next] of requests.entries()) {
    const previous = requests[index - 1];
    if (previous === undefined) continue;
    const before = messagesOf(previous);
    const head = messagesOf(next).slice(0, before.length);
    if (bare(head) !== bare(before)) {
      const at = before.findIndex((message, place) => bare([message]) !== bare([head[place]]));
      console.log(
        `request ${index} does not begin with request ${index - 1}: message ${at} of ` +
          `${before.length} was ${JSON.stringify(before[at])} and is ` +
          `${JSON.stringify(head[at] ?? null)}`
      );
    }
    expect(bare(head)).toBe(bare(before));
  }
}

/** A cached list's read time and the reason its header gives, from the header's second line. */
function cachedOf(text: string): { readonly readAt: string; readonly reason: string } {
  const match = /^As read at (\S+); this read failed: (.+)\.$/m.exec(text);
  if (match?.[1] === undefined || match[2] === undefined) {
    throw new Error(`not a cached list:\n${text}`);
  }
  return { readAt: match[1], reason: match[2] };
}

const MINUTE = 60_000;

/** A person's answer to the session's ask, written `minutesAgo` minutes ago. */
function answer(id: string, text: string, minutesAgo: number): SessionAnswer {
  return {
    kind: "answer",
    relation: "answered_your_ask",
    id,
    at: new Date(Date.now() - minutesAgo * MINUTE).toISOString(),
    author: { kind: "user", id: "alice" },
    text,
    selected: ["The second way"],
    ask: { id, question: "Which way should the fix go?" },
    owner: { issue: { key: "LEGION-1", title: "The work" } },
    ref: `dispatch://LEGION-1/ask/${id}`,
    href: `/issues/LEGION-1#ask-${id}`,
  };
}

/** The answers request whose read a Messages request waited on: the last one before it. */
function answersBefore(seen: readonly Seen[], request: Seen): Seen {
  const before = seen.filter((other) => other.path.endsWith("/answers") && other.at <= request.at);
  const last = before.at(-1);
  if (last === undefined) throw new Error("no answers request came before the Messages request");
  return last;
}

/**
 * Runs an ordinary session (the Envoy extension from this checkout, Dispatch configured, no Legion
 * environment) through `steps` on the real Oh My Pi. Each turn is answered by `reply`, or `Answer
 * N.`; each compaction summary call with a fixed summary; every other side call with a label.
 * `reply` may change the answers route as the stand-in answers a turn. `methodOrder` is the
 * compaction's (`[soft]` by default) and `model` lines go on the stand-in model's entry in
 * models.yml. With `twoCopies`, omp loads the extension twice: from this checkout and through a
 * hard link to it beside it, which both omp and Bun take for another file (omp loads a symlink and
 * its target once). With `warm`, the profile's model catalog is built before the session starts.
 * Once the steps have settled, `until` is awaited before omp's stdin closes. Returns every request
 * the stand-in served, in order of arrival, the main loop's turns among them, and when each step
 * was sent (`performance.now()`, route changes and restarts left out).
 */
async function runSession(
  binary: string,
  steps: readonly Step[],
  options: {
    readonly reply?: (request: Request, setRoute: (route: Route) => void) => readonly Block[];
    readonly config?: readonly string[];
    readonly methodOrder?: string;
    readonly model?: readonly string[];
    readonly twoCopies?: boolean;
    readonly until?: Promise<unknown>;
    readonly warm?: boolean;
  } = {}
): Promise<{
  readonly seen: Seen[];
  readonly turns: Seen[];
  readonly answers: Seen[];
  readonly sentAt: number[];
  readonly summary: string;
}> {
  const { home, workspace, sessions, root } = await ompRoot(
    binary,
    "session-answers-omp-",
    cleanup
  );
  const summary = "SUMMARY-OF-THE-EARLIER-TURNS";
  let route: Route = { kind: "list", items: [] };
  const setRoute = (next: Route) => {
    route = next;
  };
  // A route that never answers holds its request until the case ends, and lets go of it before
  // the stand-in stops (cleanup runs last first), which waits on every open request.
  const release = Promise.withResolvers<void>();

  const seen: Seen[] = [];
  let answered = 0;
  // A /btw step has settled once the stand-in serves its side turn: omp answers the RPC command
  // before it sends the side turn's request.
  let sideTurnServed = Promise.withResolvers<void>();
  const { base } = serveStandin(cleanup, async (url, body) => {
    const request: Seen = { path: url.pathname, body, at: performance.now() };
    seen.push(request);
    if (url.pathname === "/anthropic/v1/messages") {
      answered += 1;
      if (isSideTurn(request)) sideTurnServed.resolve();
      const blocks: readonly Block[] = isSummarization(request)
        ? [{ type: "text", text: summary }]
        : !isTurn(request)
          ? [{ type: "text", text: "Label." }]
          : (options.reply?.(request, setRoute) ?? [{ type: "text", text: `Answer ${answered}.` }]);
      return new Response(messageStream(blocks, `msg_${answered}`), {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    // An unavailable open-asks read arms no run-end self-check, so every Messages request is a
    // turn of the conversation or one of the binary's side calls.
    if (url.pathname === "/api/v1/asks/open") return new Response("unavailable", { status: 503 });
    const answers = /^\/api\/v1\/agents\/([^/]+)\/answers$/.exec(url.pathname);
    if (answers !== null) {
      const current = route;
      if (current.kind === "hang") {
        return release.promise.then(() => new Response("released", { status: 503 }));
      }
      if (current.kind === "missing") {
        // What a Dispatch build without the route answers (routes/router.go's fallback).
        return Response.json(
          {
            code: "NOT_FOUND",
            error: `no route for GET ${url.pathname}`,
            hint: "GET /api/v1 lists every route",
          },
          { status: 404 }
        );
      }
      if (current.kind === "error") {
        // What Dispatch answers a read it cannot serve: its JSON error body, with the status.
        return Response.json(
          { code: "UNAVAILABLE", error: "the database is unavailable" },
          { status: current.status }
        );
      }
      // A slow Dispatch answers after a real delay: the case times the request against the read's
      // own deadline, a platform timer inside the binary's process.
      if (current.delayMs !== undefined) await Bun.sleep(current.delayMs);
      return Response.json({
        session_id: decodeURIComponent(answers[1] ?? ""),
        since: new Date(Date.now() - 72 * 60 * MINUTE).toISOString(),
        total: current.items.length,
        items: current.items,
      });
    }
    // The Envoy listener: registration and any read answer with an interest, and a registration
    // is answered the consumer the listener gave the session (`deliveryFor`), as every stand-in
    // listener of this repository answers one: the client refuses a registration answered no
    // delivery, which would leave this session's Envoy inbound unavailable.
    const id = typeof body.session_id === "string" ? body.session_id : "";
    return Response.json({
      session_id: id,
      machine_id: "session-answers-machine",
      dir: workspace,
      topics: [],
      ...deliveryFor(url.pathname, body, id),
    });
  });
  cleanup.push(async () => release.resolve());
  await writeStandinProfile(home, base, [
    "compaction:",
    // Keep only the newest turn, so a short conversation still has something to summarize.
    "  keepRecentTokens: 1",
    `  methodOrder: ${options.methodOrder ?? "[soft]"}`,
    ...(options.config ?? []),
  ]);
  if (options.model !== undefined) {
    // The stand-in model is the last entry models.yml lists.
    await appendFile(
      path.join(home, ".omp", "agent", "models.yml"),
      options.model.map((line) => `        ${line}\n`).join("")
    );
  }
  if (options.warm === true) {
    // A fresh profile has no model catalog, so the binary builds one beside its first turn, and
    // that work holds its event loop for one to two seconds (measured with --cpu-prof: the model
    // registry's refresh), whatever any extension does: a request waits on it as long with an
    // answers route that answers at once. A pane's profile has the catalog already. Building it
    // first keeps that host cost out of a case that times the answers read.
    const built = Bun.spawn([binary, "models", "refresh"], {
      cwd: workspace,
      env: { HOME: home, PATH: "/usr/local/bin:/usr/bin:/bin" },
      stdout: "ignore",
      stderr: "pipe",
    });
    if ((await built.exited) !== 0) {
      throw new Error(`omp models refresh failed:\n${await new Response(built.stderr).text()}`);
    }
  }
  const extension = path.join(import.meta.dir, "envoy.ts");
  const extensions = [extension];
  if (options.twoCopies === true) {
    // Beside the original, so its relative imports resolve to the same modules a symlink's did.
    const copy = path.join(import.meta.dir, `.envoy-copy-${process.pid}-${Date.now()}.ts`);
    await link(extension, copy);
    cleanup.push(() => rm(copy, { force: true }));
    extensions.push(copy);
  }
  if (steps.some((step) => "deliver" in step)) {
    const deliverer = path.join(root, "deliverer.ts");
    await writeFile(deliverer, DELIVERER);
    extensions.push(deliverer);
  }

  // A prompt and a delivery have settled at their terminal agent_end; a compaction, a handoff and
  // a quiet delivery (an RPC prompt that runs a command) at their RPC response; a /btw once its
  // side turn was served.
  let settle: ((frame: object) => boolean) | undefined;
  let settled = Promise.withResolvers<void>();
  /** omp over the session, with `args` after the harness's flags; its cleanup goes on `cleanup`
   *  after the stand-in's, so it is killed before the stand-in stops. */
  const start = (args: readonly string[]) =>
    spawnRpc(
      binary,
      {
        extensions,
        home,
        workspace,
        sessions,
        env: { ENVOY_URL: base, DISPATCH_URL: base, DISPATCH_TOKEN },
        args,
        onFrame: (frame) => {
          if (settle?.(frame)) {
            settle = undefined;
            settled.resolve();
          }
        },
      },
      cleanup
    );
  let rpc = start([]);
  const sentAt: number[] = [];
  for (const step of steps) {
    if ("route" in step) {
      route = step.route;
      continue;
    }
    if ("restart" in step) {
      // omp exits once its stdin closes, as a pod's process ends at its idle release, and the
      // next process resumes the session from its transcript, as the pod's next life does.
      await rpc.end();
      rpc = start(["--resume", await resumeId(sessions)]);
      continue;
    }
    if ("btw" in step) {
      sideTurnServed = Promise.withResolvers<void>();
      sentAt.push(performance.now());
      rpc.send({ type: "btw", question: step.btw });
      await Promise.race([sideTurnServed.promise, rpc.closed]);
      continue;
    }
    settled = Promise.withResolvers<void>();
    const command =
      "compact" in step
        ? "compact"
        : "handoff" in step
          ? "handoff"
          : "deliver" in step && step.quiet === true
            ? "prompt"
            : undefined;
    let commandError: unknown;
    settle =
      command !== undefined
        ? (frame) => {
            if (!("type" in frame && frame.type === "response")) return false;
            if (!("command" in frame && frame.command === command)) return false;
            if (!("success" in frame && frame.success === true)) {
              commandError = "error" in frame ? frame.error : "no error given";
            }
            return true;
          }
        : (frame) =>
            "type" in frame &&
            frame.type === "agent_end" &&
            !("isTerminal" in frame && frame.isTerminal === false);
    sentAt.push(performance.now());
    rpc.send(
      "prompt" in step
        ? { type: "prompt", message: step.prompt }
        : "deliver" in step
          ? {
              type: "prompt",
              message: `/${step.quiet === true ? "deliver-quiet" : "deliver"} ${step.deliver}`,
            }
          : { type: command }
    );
    await Promise.race([settled.promise, rpc.closed]);
    if (commandError !== undefined) throw new Error(`omp refused the ${command}: ${commandError}`);
  }
  if (options.until !== undefined) await Promise.race([options.until, rpc.closed]);
  await rpc.end();

  return {
    seen,
    turns: seen.filter(
      (request) => request.path === "/anthropic/v1/messages" && isMainTurn(request)
    ),
    answers: seen.filter((request) => request.path.endsWith("/answers")),
    sentAt,
    summary,
  };
}

/** A reply that answers the turn after `prompt` with a `task` call handing `ASSIGNMENT` to one
 *  subagent, calling `onTask` as it does; resolves `subagent` when the subagent's first turn
 *  arrives. Every other turn is answered `Done.`. */
function delegating(prompt: string, onTask: (setRoute: (route: Route) => void) => void = () => {}) {
  const subagent = Promise.withResolvers<void>();
  let delegated = false;
  const reply = (request: Request, setRoute: (route: Route) => void): readonly Block[] => {
    if (isSubagentTurn(request)) {
      subagent.resolve();
      return [{ type: "text", text: "Done." }];
    }
    if (!delegated && JSON.stringify(messagesOf(request)).includes(prompt)) {
      delegated = true;
      onTask(setRoute);
      return [
        {
          type: "tool_use",
          name: "task",
          input: {
            i: "Delegating the report",
            context: "A one-step check.",
            tasks: [{ name: "Reporter", task: ASSIGNMENT, solutionSpace: "Reply in one line." }],
          },
        },
      ];
    }
    return [{ type: "text", text: "Done." }];
  };
  return { reply, subagent: subagent.promise };
}

const skipped = requireOrSkip(
  "LEGION_TEST_OMP, naming the Oh My Pi binary of .omp-pin",
  omp !== undefined,
  "pi-envoy"
);
const binary = (): string => {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset");
  return omp;
};

test.skipIf(skipped)(
  "(a) a person's answer that appears after turn 1 is in turn 2, word for word",
  async () => {
    const item = answer("ask_a", 'Go the *second* way.\nKeep `retry` at 3, and say "done".', 2);
    const session = await runSession(binary(), [
      { prompt: "First question." },
      { route: { kind: "list", items: [item] } },
      { prompt: "Second question." },
    ]);

    const [first, second, ...rest] = session.turns;
    expect(rest).toEqual([]);
    expect(carriesList(first as Seen)).toBe(false);
    // Compared after JSON parsing: the text the model reads, not its escaped bytes.
    const list = lastListOf(second as Seen);
    expect(list).toBe(sentList(renderSessionAnswers([item], { now: Date.now() })));
    expect(list).toContain(item.text);
  },
  180_000
);

test.skipIf(skipped)(
  "(b) an answer that appears after a manual compaction is in the next turn",
  async () => {
    const item = answer("ask_b", "Compaction or not, ship it.", 1);
    const session = await runSession(binary(), [
      { prompt: "First question." },
      { prompt: "Second question." },
      { compact: true },
      { route: { kind: "list", items: [item] } },
      { prompt: "Third question." },
    ]);

    const [first, second, third, ...rest] = session.turns;
    expect(rest).toEqual([]);
    expect(carriesList(first as Seen)).toBe(false);
    expect(carriesList(second as Seen)).toBe(false);
    // The third turn is sent over the compacted history, with the list as its last message.
    const third_ = JSON.stringify(messagesOf(third as Seen));
    expect(third_).not.toContain("First question.");
    expect(third_).toContain(session.summary);
    expect(listOf(third as Seen)).toBe(sentList(renderSessionAnswers([item], { now: Date.now() })));
  },
  180_000
);

test.skipIf(skipped)(
  "(c) a subagent started after the answer carries it in its first request, read under its parent's id",
  async () => {
    const item = answer("ask_c", "The subagent should hear this too.", 1);
    const { reply, subagent } = delegating("Delegate it.");
    const session = await runSession(
      binary(),
      [
        { prompt: "First question." },
        { route: { kind: "list", items: [item] } },
        { prompt: "Delegate it." },
      ],
      { reply, until: subagent }
    );

    const parentID = session.answers[0]?.path.split("/")[4];
    expect(parentID).toBeString();
    const first = session.turns.find(isSubagentTurn);
    if (first === undefined) throw new Error("the subagent sent no Messages request");
    expect(listOf(first)).toBe(sentList(renderSessionAnswers([item], { now: Date.now() })));
    // Every read, the subagent's included, asked for the parent's list.
    expect(new Set(session.answers.map((request) => request.path))).toEqual(
      new Set([`/api/v1/agents/${parentID}/answers`])
    );
    expect(answersBefore(session.seen, first).path).toBe(`/api/v1/agents/${parentID}/answers`);
  },
  180_000
);

test.skipIf(skipped)(
  "(d) an answer, a reply to its comment, a comment on its document and a message to it are each in the list word for word",
  async () => {
    const items: SessionAnswer[] = [
      answer("ask_d", "Answer: use the **new** schema.", 1),
      {
        kind: "comment",
        relation: "replied_in_a_comment_thread",
        id: "comment_reply",
        at: new Date(Date.now() - 2 * MINUTE).toISOString(),
        author: { kind: "user", id: "bob" },
        text: "Reply: that paragraph is wrong;\n- fix the date\n- keep the link",
        owner: { issue: { key: "LEGION-1", title: "The work" } },
        ref: "dispatch://LEGION-1/comment/comment_reply",
        href: "/issues/LEGION-1#comment-comment_reply",
      },
      {
        kind: "comment",
        relation: "commented_on_a_document",
        id: "comment_doc",
        at: new Date(Date.now() - 3 * MINUTE).toISOString(),
        author: { kind: "user", id: "carol" },
        text: "Document comment: the table needs a total row.",
        owner: { document: { project: "LEGION", slug: "runbook", name: "Runbook" } },
        ref: "dispatch://LEGION/artifact/runbook/comment/comment_doc",
        href: "/projects/LEGION/documents/runbook#comment-comment_doc",
      },
      {
        kind: "message",
        relation: "sent_you_a_message",
        id: "message_1",
        at: new Date(Date.now() - 4 * MINUTE).toISOString(),
        author: { kind: "user", id: "dave" },
        text: "Message: pause after the migration — I'll check the numbers.",
        owner: null,
        ref: null,
        href: "/agents/session/live",
      },
    ];
    const session = await runSession(binary(), [
      { route: { kind: "list", items } },
      { prompt: "First question." },
    ]);

    const [first, ...rest] = session.turns;
    expect(rest).toEqual([]);
    const list = listOf(first as Seen);
    expect(list).toBe(sentList(renderSessionAnswers(items, { now: Date.now() })));
    for (const item of items) expect(list).toContain(item.text);
  },
  180_000
);

test.skipIf(skipped)(
  "(e) twelve items past 8,000 characters: the newest prefix and the left-out line with its count",
  async () => {
    const items = Array.from({ length: 12 }, (_, index) =>
      answer(
        `ask_e${index}`,
        `Item ${index}: ${"word ".repeat(180)}end of item ${index}.`,
        index + 1
      )
    );
    expect(items.reduce((sum, item) => sum + item.text.length, 0)).toBeGreaterThan(8_000);
    const session = await runSession(binary(), [
      { route: { kind: "list", items } },
      { prompt: "First question." },
    ]);

    const carried = listOf(session.turns[0] as Seen);
    // The cap is the list's, the first block's: the sentence comes after the list.
    expect(carried.endsWith(`\n${REPLACES}`)).toBe(true);
    const list = carried.slice(0, -`\n${REPLACES}`.length);
    expect(list.length).toBeLessThan(8_000);
    // Shown items are the newest, a strict prefix: the first left out ends the list.
    const shown = items.map((item) => list.includes(item.text));
    const count = shown.indexOf(false);
    expect(count).toBeGreaterThan(0);
    expect(shown.slice(count).every((included) => !included)).toBe(true);
    const leftOut = items.length - count;
    expect(
      list.endsWith(
        `\n\n${leftOut} older items left out to stay under 8,000 characters: run \`dispatch answers\` to read every one in full.`
      )
    ).toBe(true);
    expect(carried).toBe(sentList(renderSessionAnswers(items, { now: Date.now() })));
  },
  180_000
);

// The wait each reading measures is the deadline the extension gives Dispatch
// (`SESSION_ANSWERS_DEADLINE_MS`, 800 ms) as the machine schedules it: on this pod, with no job
// of the tester's running, two runs of the one-reading row read 853, 841, 848 ms and 855, 839,
// 858 ms, about 50 ms over the deadline against a 1,000 ms bound. A reading can also fall under
// the deadline, since the deadline starts before the request reaches the stand-in that measures
// the gap: a re-run read 682 ms.
//
// A median of three readings per label rather than one, because one reading judges the pod's
// scheduling rather than the route. At c772c9881dd2, whose copy of this file is byte for byte the
// one this change starts from, the single reading for `turn 1 with no cache` failed the bound at
// 1012.5 ms in the package suite, and a re-run of this file alone passed. The route hangs the
// same either way: that reading sat 212 ms over the deadline where a reading on this pod sits
// about 50 ms over it, so what crossed is the pod's scheduling, not the route. The median of
// three is the second of three, so no one slow reading can carry it: two of the three have to
// cross before the row fails.
//
// Each reading is a session of its own, ended before the next one starts, so no reading is taken
// beside the stand-in and the binary of an earlier one. All three are printed with the median
// beside them, whether or not the row passes.
//
// Nine sessions run here, three readings for each of the three labels, and the budget below is
// read from what one of them costs: two runs of the row read 209 s and 191 s on this pod, about
// 22 s a session. Nine sessions at 40 s each is 360 s, which is the timeout below - over half
// again the longer of those two runs - so a pod that schedules each session slower fails this
// row on the route it measures rather than on the clock.
test.skipIf(skipped)(
  "(f) an answers route that never answers holds no request for a second, by the median of three readings: turn 1, turn 2 after a good read, and a subagent's first request",
  async () => {
    const deadline = `Dispatch did not answer within ${SESSION_ANSWERS_DEADLINE_MS} ms`;
    /** One reading: the gap between the answers request a Messages request waited on and the
     *  request itself. */
    const waitOf = (session: { readonly seen: Seen[] }, request: Seen): number =>
      request.at - answersBefore(session.seen, request).at;
    /** Three fresh readings of one case and their median against the bound, with every reading
     *  printed and the median beside them. */
    const median = async (label: string, reading: () => Promise<number>): Promise<void> => {
      const readings: number[] = [];
      for (let run = 0; run < 3; run += 1) {
        // This reading's stand-in, binary and scratch root, ended as `afterEach` ends them, so
        // the next reading starts on a machine holding none of them.
        const from = cleanup.length;
        try {
          readings.push(await reading());
        } finally {
          for (const step of cleanup.splice(from).reverse()) await step();
        }
      }
      const middle = [...readings].sort((first, second) => first - second)[1] as number;
      const taken = readings.map((ms) => ms.toFixed(0)).join(", ");
      console.log(
        `(f) ${label}: the Messages request came ${taken} ms after the answers request over ` +
          `three fresh sessions, a median of ${middle.toFixed(0)} ms`
      );
      expect(middle).toBeLessThan(1_000);
    };

    // Turn 1, with no list ever read: the failure line.
    await median("turn 1 with no cache", async () => {
      const cold = await runSession(
        binary(),
        [{ route: { kind: "hang" } }, { prompt: "First question." }],
        { warm: true }
      );
      const [coldFirst] = cold.turns;
      if (coldFirst === undefined) throw new Error("no turn 1");
      expect(listOf(coldFirst)).toBe(sessionAnswersFailureLine(deadline));
      return waitOf(cold, coldFirst);
    });

    // Turn 2 after a good read: the read hangs and answers from the cache, which shows the same
    // item, so the list keeps the place turn 1 gave it and the bytes it was placed with, not the
    // cached read's header (`As read at …; this read failed: …`), whose changed bytes would cost
    // the request behind them a prompt-cache miss.
    const item = answer("ask_f", "Read before Dispatch hung.", 1);
    await median("turn 2 after a good read", async () => {
      const cached = await runSession(
        binary(),
        [
          { route: { kind: "list", items: [item] } },
          { prompt: "First question." },
          { route: { kind: "hang" } },
          { prompt: "Second question." },
        ],
        { warm: true }
      );
      const [cachedFirst, cachedSecond] = cached.turns;
      if (cachedFirst === undefined || cachedSecond === undefined) throw new Error("no turn 2");
      const placed = listOf(cachedFirst);
      expect(placed).toBe(sentList(renderSessionAnswers([item], { now: Date.now() })));
      expect(listOf(cachedSecond)).toBe(placed);
      expect(textOf(messagesOf(cachedSecond).at(-1))).toBe("Second question.");
      return waitOf(cached, cachedSecond);
    });

    // The subagent's first request: Dispatch hangs from the parent's delegating turn on, so the
    // subagent's read is the first to hang. `task` runs in the foreground here, so no read of the
    // parent's is in flight when the subagent's starts.
    await median("the subagent's first request", async () => {
      const { reply, subagent } = delegating("Delegate it.", (setRoute) =>
        setRoute({ kind: "hang" })
      );
      const sub = await runSession(
        binary(),
        [{ route: { kind: "list", items: [item] } }, { prompt: "Delegate it." }],
        { reply, until: subagent, config: ["async:", "  enabled: false"], warm: true }
      );
      const subFirst = sub.turns.find(isSubagentTurn);
      if (subFirst === undefined) throw new Error("the subagent sent no Messages request");
      expect(sub.answers.length).toBeGreaterThanOrEqual(2);
      expect(answersBefore(sub.seen, subFirst)).toBe(sub.answers[1] as Seen);
      const carried = listOf(subFirst);
      const carriedCache = cachedOf(carried);
      expect(carriedCache.reason).toBe(deadline);
      expect(carried).toBe(
        sentList(renderSessionAnswers([item], { now: Date.now(), cached: carriedCache }))
      );
      return waitOf(sub, subFirst);
    });
  },
  360_000
);

test.skipIf(skipped)(
  "(g) a Dispatch with no answers route: no line on the request, and no answers request on the next turn",
  async () => {
    const session = await runSession(binary(), [
      { route: { kind: "missing" } },
      { prompt: "First question." },
      { prompt: "Second question." },
    ]);

    const [first, second, ...rest] = session.turns;
    expect(rest).toEqual([]);
    // `dispatch answers` fails against that Dispatch too, so neither request names it.
    for (const turn of [first as Seen, second as Seen]) {
      expect(carriesList(turn)).toBe(false);
      expect(JSON.stringify(messagesOf(turn))).not.toContain("dispatch answers");
    }
    expect(textOf(messagesOf(first as Seen).at(-1))).toContain("First question.");
    expect(textOf(messagesOf(second as Seen).at(-1))).toContain("Second question.");
    expect(session.answers).toHaveLength(1);
    expect((session.answers[0] as Seen).at).toBeLessThan((first as Seen).at);
  },
  180_000
);

test.skipIf(skipped)(
  "(h) a change in the list leaves the cached prefix byte for byte, and the list carries no cache anchor",
  async () => {
    const older = answer("ask_h1", "The first answer.", 3);
    const newer = answer("ask_h2", "A second answer, after turn 1.", 1);
    const session = await runSession(binary(), [
      { route: { kind: "list", items: [older] } },
      { prompt: "First question." },
      { route: { kind: "list", items: [newer, older] } },
      { prompt: "Second question." },
    ]);

    const [n, next, ...rest] = session.turns;
    if (n === undefined || next === undefined) throw new Error("want two turns");
    expect(rest).toEqual([]);
    // N+1 begins with every message of N, N's list included, so a change appends a list after
    // N+1's last message rather than move or edit the one N sent, which stays right after the
    // message it followed.
    expectPrefixes([n, next]);
    const [placed] = listsOf(n);
    if (placed === undefined) throw new Error("request N carries no list");
    expect(placed.index).toBe(messagesOf(n).length - 1);
    const lists = listsOf(next);
    expect(lists.map(({ index }) => index)).toEqual([placed.index, messagesOf(next).length - 1]);
    expect(textOf(messagesOf(next)[placed.index - 1])).toBe("First question.");
    // Neither list carries a cache anchor.
    for (const { index } of lists) {
      expect(messagesOf(next)[index]).toMatchObject({ role: "user" });
      expect(JSON.stringify(messagesOf(next)[index])).not.toContain("cache_control");
    }
    // Each list shows a new item, so each went after its request's last message, and they differ.
    const olderList = sentList(renderSessionAnswers([older], { now: Date.now() }));
    expect(lastListOf(n)).toBe(olderList);
    expect(lists.map(({ text }) => text)).toEqual([
      olderList,
      sentList(renderSessionAnswers([newer, older], { now: Date.now() })),
    ]);
    // N's messages through its last cache_control-bearing one are N+1's first, byte for byte.
    const messages = messagesOf(n);
    const anchor = messages.findLastIndex((message) =>
      JSON.stringify(message).includes("cache_control")
    );
    expect(anchor).toBeGreaterThanOrEqual(0);
    expect(anchor).toBeLessThan(messages.length - 1);
    expect(JSON.stringify(messagesOf(next).slice(0, anchor + 1))).toBe(
      JSON.stringify(messages.slice(0, anchor + 1))
    );
    console.log(
      `(h) request N+1's last two messages: ${JSON.stringify(messagesOf(next).slice(-2))}`
    );
  },
  180_000
);

// A list that shows nothing new is no new turn: on the turns after the one it first went on, it
// stays right after the message it followed there, each request ends with its own last message,
// and the list's bytes are inside the prefix the next request reads from the cache.
test.skipIf(skipped)(
  "(o) a list with nothing new stays where it first went: later requests end with their own message and cache through it",
  async () => {
    const item = answer("ask_o", "Ship the first way.", 2);
    const session = await runSession(binary(), [
      { route: { kind: "list", items: [item] } },
      { prompt: "First question." },
      { prompt: "Second question." },
      { prompt: "Third question." },
    ]);

    const [first, second, third, ...rest] = session.turns;
    if (first === undefined || second === undefined || third === undefined) {
      throw new Error("want three turns");
    }
    expect(rest).toEqual([]);
    const expected = sentList(renderSessionAnswers([item], { now: Date.now() }));
    expect(lastListOf(first)).toBe(expected);
    const listAt = (request: Request) =>
      messagesOf(request).findIndex((message) => isSessionAnswersText(textOf(message)));
    for (const [request, prompt] of [
      [second, "Second question."],
      [third, "Third question."],
    ] as const) {
      expect(listOf(request)).toBe(expected);
      expect(textOf(messagesOf(request).at(-1))).toBe(prompt);
      expect(textOf(messagesOf(request)[listAt(request) - 1])).toBe("First question.");
    }
    // Turn 2's messages through its last cache anchor, which comes after the list, are turn 3's
    // first, the same content once the anchors are set aside (`bare`).
    const anchor = messagesOf(second).findLastIndex((message) =>
      JSON.stringify(message).includes("cache_control")
    );
    expect(anchor).toBeGreaterThan(listAt(second));
    expect(bare(messagesOf(third).slice(0, anchor + 1))).toBe(
      bare(messagesOf(second).slice(0, anchor + 1))
    );
  },
  180_000
);

// What people wrote reaches a request without changing a byte an earlier request sent: every
// answers message a request carried stays where it went, with its bytes, and a change goes after
// the last message as a list of its own, with the sentence saying the newest list is the current
// one. Each case checks the prefix and the places first, and the texts after, so a build that
// moves or takes out a list sent fails on the bytes at that list's place.
test.skipIf(skipped)(
  "(p) after a compaction, a new answer, an edit and two retractions each go after the last message, and every request begins with the one before it",
  async () => {
    const first = answer("ask_p1", "Take the first way.", 3);
    const second = answer("ask_p2", "Keep the old flag too.", 2);
    // The first answer changed by its author: Dispatch keeps its id and time and stamps the edit.
    const edited: SessionAnswer = {
      ...first,
      text: "Take the first way, behind the flag.",
      edited_at: new Date().toISOString(),
    };
    const session = await runSession(binary(), [
      { route: { kind: "list", items: [first] } },
      { prompt: "First question." },
      { prompt: "Second question." },
      { prompt: "Third question." },
      { compact: true },
      { prompt: "Fourth question." },
      { prompt: "Fifth question." },
      { route: { kind: "list", items: [second, first] } },
      { prompt: "Sixth question." },
      { route: { kind: "list", items: [second, edited] } },
      { prompt: "Seventh question." },
      // Each retraction leaves the read while the answer's time is inside the window.
      { route: { kind: "list", items: [second] } },
      { prompt: "Eighth question." },
      { route: { kind: "list", items: [] } },
      { prompt: "Ninth question." },
      { prompt: "Tenth question." },
    ]);

    expect(session.turns).toHaveLength(10);
    // The turns after the compaction, which summarized the message the first list followed.
    const turns = session.turns.slice(3);
    const messages = turns.map(messagesOf);
    expect(JSON.stringify(messages[0])).not.toContain("First question.");
    expect(JSON.stringify(messages[0])).toContain(session.summary);
    expectPrefixes(turns);
    // The fourth question's request ends with a list and the fifth's with its prompt; each change
    // adds one list after the last message; the turn with no change adds none.
    const lists = turns.map(listsOf);
    expect(lists.map((sent) => sent.length)).toEqual([1, 1, 2, 3, 4, 5, 5]);
    expect(
      turns.map((request, index) => lists[index]?.at(-1)?.index === messagesOf(request).length - 1)
    ).toEqual([true, false, true, true, true, true, false]);
    expect(textOf(messages[1]?.at(-1))).toBe("Fifth question.");
    expect(lists[6]).toEqual(lists[5]);
    expect(textOf(messages[6]?.at(-1))).toBe("Tenth question.");

    for (const sent of lists.slice(2, 6)) {
      expect(sent.at(-1)?.text.endsWith(`\n${REPLACES}`)).toBe(true);
    }
    expect(textOf(messages[5]?.at(-1))).toBe(`${NO_ITEM}\n${REPLACES}`);
    const now = Date.now();
    expect(lists[6]?.map(({ text }) => text)).toEqual([
      sentList(renderSessionAnswers([first], { now })),
      sentList(renderSessionAnswers([second, first], { now })),
      sentList(renderSessionAnswers([second, edited], { now })),
      sentList(renderSessionAnswers([second], { now })),
      `${NO_ITEM}\n${REPLACES}`,
    ]);
  },
  180_000
);

// The model reads that a read failed only when the read has no list to give: the failure line,
// appended once and kept as a list is. A recovery to an empty window has nothing new to say, and a
// failed read that gives the cached list with the same items keeps the list sent.
test.skipIf(skipped)(
  "(q) after a compaction, a 503, a recovery to an empty window and a list: the failure line once, nothing for the recovery, the list after the last message",
  async () => {
    const item = answer("ask_q", "Read it once Dispatch is back.", 1);
    const session = await runSession(binary(), [
      { prompt: "First question." },
      { prompt: "Second question." },
      { compact: true },
      { prompt: "Third question." },
      { prompt: "Fourth question." },
      { route: { kind: "error", status: 503 } },
      { prompt: "Fifth question." },
      { prompt: "Sixth question." },
      { route: { kind: "list", items: [] } },
      { prompt: "Seventh question." },
      { route: { kind: "list", items: [item] } },
      { prompt: "Eighth question." },
    ]);

    expect(session.turns).toHaveLength(8);
    const turns = session.turns.slice(2);
    const messages = turns.map(messagesOf);
    expect(JSON.stringify(messages[0])).not.toContain("First question.");
    expect(JSON.stringify(messages[0])).toContain(session.summary);
    expectPrefixes(turns);
    // The first failed read, with nothing cached that renders, puts the failure line after the
    // last message; the second failure and the recovery to an empty window add nothing and end
    // with their prompts; the list goes after the last message, the failure line kept at its place.
    const lists = turns.map(listsOf);
    expect(lists.map((sent) => sent.length)).toEqual([0, 0, 1, 1, 1, 2]);
    const [failure] = lists[2] ?? [];
    if (failure === undefined) throw new Error("the first failed read's request carries nothing");
    expect(failure.index).toBe((messages[2]?.length ?? 0) - 1);
    expect(lists[3]).toEqual([failure]);
    expect(lists[4]).toEqual([failure]);
    expect(textOf(messages[3]?.at(-1))).toBe("Sixth question.");
    expect(textOf(messages[4]?.at(-1))).toBe("Seventh question.");
    expect(lists[5]?.[0]).toEqual(failure);
    expect(lists[5]?.[1]?.index).toBe((messages[5]?.length ?? 0) - 1);

    console.log(`(q) the failure line: ${failure.text}`);
    expect(failure.text).toStartWith(FAILURE_MARKER);
    // The failure line alone, with no sentence after it.
    expect(failure.text).toEndWith("); run `dispatch answers`.]");
    expect(lists[5]?.[1]?.text).toBe(sentList(renderSessionAnswers([item], { now: Date.now() })));

    // A failed read that gives the cached list, with the same item, keeps the list sent where it
    // went, with its bytes, and adds nothing.
    const cached = await runSession(binary(), [
      { route: { kind: "list", items: [item] } },
      { prompt: "First question." },
      { route: { kind: "hang" } },
      { prompt: "Second question." },
    ]);
    const [read, held, ...rest] = cached.turns;
    if (read === undefined || held === undefined) throw new Error("want two turns");
    expect(rest).toEqual([]);
    expectPrefixes([read, held]);
    expect(listsOf(read)).toHaveLength(1);
    expect(listsOf(held)).toEqual(listsOf(read));
    expect(textOf(messagesOf(held).at(-1))).toBe("Second question.");
    expect(listsOf(read)[0]?.text).toBe(
      sentList(renderSessionAnswers([item], { now: Date.now() }))
    );
  },
  180_000
);

// A pod's process ends at its idle release, and its next life resumes the session from the
// session file, with no memory of where a list went: the session holds it.
test.skipIf(skipped)(
  "(r) a list placed, then a restart: the resumed process's first request carries it at its old place with its bytes",
  async () => {
    const item = answer("ask_r", "Pick it up where you left it.", 1);
    const session = await runSession(binary(), [
      { route: { kind: "list", items: [item] } },
      { prompt: "First question." },
      { prompt: "Second question." },
      { prompt: "Third question." },
      { restart: true },
      { prompt: "Fourth question." },
    ]);

    const [, , before, resumed, ...rest] = session.turns;
    if (before === undefined || resumed === undefined) throw new Error("want four turns");
    expect(rest).toEqual([]);
    expectPrefixes([before, resumed]);
    const [placed] = listsOf(before);
    if (placed === undefined) throw new Error("the request before the restart carries no list");
    expect(listsOf(resumed)).toEqual([placed]);
    expect(textOf(messagesOf(before)[placed.index - 1])).toBe("First question.");
    expect(textOf(messagesOf(resumed)[placed.index - 1])).toBe("First question.");
    expect(textOf(messagesOf(resumed).at(-1))).toBe("Fourth question.");

    expect(placed.text).toBe(sentList(renderSessionAnswers([item], { now: Date.now() })));
  },
  180_000
);

// Every request shows each Envoy card with `delivered:`, the time the session got it
// (src/card-age.ts). The session records that time when it first uses it, so a process that resumes
// the session shows the card with the bytes the process before it sent. A resumed process can also
// write the same card again before its first request, as it replays a card whose ledger record the
// process before it never wrote: that write takes the time the session records, and the card
// already sent keeps its text. The duplicate is written quiet, with no turn, so its `message_end`
// comes before the resumed process's first request; a duplicate that starts a turn is written after
// that request's `context` handler has fixed the card's time. The restart puts the duplicate's write
// more than a second past the first, in another second of the line.
test.skipIf(skipped)(
  "(s) an Envoy card keeps its delivered line after a resume, also when the resumed process writes it again before its first request",
  async () => {
    const card = [
      "envoy:",
      "  id: evt-s-card",
      "  from: rec-peer",
      "  topic: agent.rec",
      `  at: ${new Date(Date.now() - 3 * MINUTE).toISOString()}`,
      "  message: Card delivered before the restart",
    ].join("\n");
    const session = await runSession(binary(), [
      { deliver: card },
      { prompt: "First question." },
      { restart: true },
      // The same card, its event id and text, written again by the resumed process before its
      // first request.
      { deliver: card, quiet: true },
      { prompt: "Second question." },
    ]);

    const [a, b, c, ...rest] = session.turns;
    if (a === undefined || b === undefined || c === undefined) throw new Error("want three turns");
    expect(rest).toEqual([]);
    /** The texts of the request's card messages, in order: the first is the card the first
     *  process wrote. */
    const cardsOf = (request: Request): string[] =>
      messagesOf(request)
        .map(textOf)
        .filter((text) => text.includes("\n  id: evt-s-card\n"));
    const oldCard = (request: Request): string => {
      const [found] = cardsOf(request);
      if (found === undefined) throw new Error("the request carries no card");
      return found;
    };
    // Printed whether or not the case passes, so a failure shows the line in every request.
    const lines = [a, b, c].map(
      (request) => /^ {2}delivered: .*$/m.exec(oldCard(request))?.[0] ?? "(no delivered line)"
    );
    // The resumed process writes the card again only after it was sent there, once it has
    // started, and the first process fixed its time before request A: this gap is a lower bound.
    const again = (session.sentAt[2] as number) - a.at;
    console.log(
      `(s) the old card's delivered line in requests A, B and C: ${JSON.stringify(lines)}; ` +
        `the card went to the resumed process again ${again.toFixed(0)} ms after request A`
    );

    expectPrefixes([b, c]);
    const sent = oldCard(a);
    for (const request of [b, c]) expect(oldCard(request)).toBe(sent);
    expect(sent).toMatch(/^ {2}delivered: /m);
    // The resumed process wrote the card again before C.
    expect(cardsOf(c)).toHaveLength(2);
  },
  180_000
);

/** A request's messages as role and text, cache anchors aside. */
function shapeOf(messages: readonly unknown[]): [unknown, string][] {
  return messages.map((message) => [
    typeof message === "object" && message !== null && "role" in message ? message.role : null,
    textOf(message),
  ]);
}

// Oh My Pi runs a `context` handler on every conversion of messages for a model, not only on the
// main loop's requests. A side turn (`AgentSession.runEphemeralTurn`, which a Dispatch BTW, the
// stop-time self-check and `/btw` all reach) converts
// `[...this.messages, {role: "developer", content: [{type: "text", text: ND}], attribution:
// "agent"}, ...history, {role: "user", content: [{type: "text", text: promptText}], attribution:
// "agent"}]` (`#jr`), and must end with its prompt.
test.skipIf(skipped)(
  "(i) a /btw side turn keeps its prompt last, carries no list and starts no read; the turns around it carry the list",
  async () => {
    const item = answer("ask_i", "Side turns stay as they were.", 1);
    const session = await runSession(
      binary(),
      [
        { route: { kind: "list", items: [item] } },
        { prompt: "First question." },
        // A read for the side turn would hang here, and hold its request for the whole deadline.
        { route: { kind: "hang" } },
        { btw: "What is up?" },
        { route: { kind: "list", items: [item] } },
        { prompt: "Second question." },
      ],
      { warm: true }
    );

    const sides = session.seen.filter(
      (request) => request.path === "/anthropic/v1/messages" && isSideTurn(request)
    );
    expect(sides).toHaveLength(1);
    const side = sides[0] as Seen;
    const messages = messagesOf(side);
    expect(textOf(messages.at(-1))).toContain("Question:\nWhat is up?\n</btw>");
    expect(textOf(messages.at(-2))).toContain("Ephemeral side-channel turn");
    expect(carriesList(side)).toBe(false);
    // The dispatch-first skill is on it as on every request.
    expect(JSON.stringify(messages)).toContain(DISPATCH_FIRST_MARKER);
    // No answers request between the /btw and its side turn: it read nothing and waited on nothing.
    const btwSent = session.sentAt[1] as number;
    expect(session.answers.filter((read) => read.at >= btwSent && read.at <= side.at)).toEqual([]);
    console.log(
      `(i) the side turn's request came ${(side.at - btwSent).toFixed(0)} ms after the /btw command`
    );
    expect(side.at - btwSent).toBeLessThan(SESSION_ANSWERS_DEADLINE_MS);

    // The main loop's turns on both sides carry the list, read fresh: no read was left hanging.
    const [first, second, ...rest] = session.turns;
    expect(rest).toEqual([]);
    const expected = sentList(renderSessionAnswers([item], { now: Date.now() }));
    expect(listOf(first as Seen)).toBe(expected);
    expect(listOf(second as Seen)).toBe(expected);
  },
  180_000
);

// The handoff converts `[...this.#e.agent.state.messages, {role: "user", content: [{type: "text",
// text: b}], attribution: "agent", ...}]`, `b` the host's handoff prompt, and must end with it.
test.skipIf(skipped)(
  "(j) the handoff prompt stays last and carries no list; the turn after the handoff carries it",
  async () => {
    const item = answer("ask_j", "Hand it over as it is.", 1);
    const session = await runSession(binary(), [
      { route: { kind: "list", items: [item] } },
      { prompt: "First question." },
      { handoff: true },
      { prompt: "Second question." },
    ]);

    const handoffs = session.seen.filter(
      (request) => request.path === "/anthropic/v1/messages" && isHandoff(request)
    );
    expect(handoffs).toHaveLength(1);
    const handoff = handoffs[0] as Seen;
    expect(textOf(messagesOf(handoff).at(-1))).toStartWith(
      "<critical>\nWrite a handoff document for another instance of yourself."
    );
    expect(carriesList(handoff)).toBe(false);
    const answersBetween = session.answers.filter(
      (read) => read.at >= (session.sentAt[1] as number) && read.at <= handoff.at
    );
    expect(answersBetween).toEqual([]);

    const [first, second, ...rest] = session.turns;
    expect(rest).toEqual([]);
    const expected = sentList(renderSessionAnswers([item], { now: Date.now() }));
    expect(listOf(first as Seen)).toBe(expected);
    expect(listOf(second as Seen)).toBe(expected);
  },
  180_000
);

// Anthropic remote compaction (`buildLiveProviderContext`) converts the conversation up to its cut,
// counts the result, and keeps that many messages of the whole conversation's conversion:
//   const ve = (await this.convertMessagesToLlm(Se, ce, pe)).length;
//   const Re = await this.agent.buildSideRequestContext(await this.convertMessagesToLlm([...Se, ...le], ce, pe));
//   ... return { ...Re, messages: Re.messages.slice(0, Le) };
// A list on the first conversion would make the request keep the first message past the cut. The
// binary runs it for a model with `compat.supportsServerCompaction` that it takes for a
// first-party endpoint (`i4`): `if (e.compat.bedrockMessagesApi === true && (jre(r) || r ===
// _f(e.baseUrl))) return true;`, so the stand-in model sets both, and its compaction request is
// the Messages request that carries `compaction`. The stand-in answers it with no compaction
// block, so the binary goes on to summarize locally, as on a failed remote compaction.
test.skipIf(skipped)(
  "(k) a remote compaction's request ends at its cut with no list, and the turn after it carries the list",
  async () => {
    const item = answer("ask_k", "Compact it remotely.", 1);
    const session = await runSession(
      binary(),
      [
        { route: { kind: "list", items: [item] } },
        { prompt: "First question." },
        { prompt: "Second question." },
        { prompt: "Third question." },
        { compact: true },
        { prompt: "Fourth question." },
      ],
      {
        methodOrder: "[remote, soft]",
        model: ["compat: {supportsServerCompaction: true, bedrockMessagesApi: true}"],
      }
    );

    const compactions = session.seen.filter(isRemoteCompaction);
    expect(compactions).toHaveLength(1);
    const compaction = compactions[0] as Seen;
    const [, , third, fourth, ...rest] = session.turns;
    if (third === undefined || fourth === undefined) throw new Error("want four turns");
    expect(rest).toEqual([]);
    // The cut falls between the third question and its answer: the compaction request is the third
    // turn's request without its list, message for message, and carries none.
    const compacted = messagesOf(compaction);
    expect(textOf(compacted.at(-1))).toBe("Third question.");
    expect(shapeOf(compacted)).toEqual(shapeOf(withoutList(third)));
    expect(carriesList(compaction)).toBe(false);
    const expected = sentList(renderSessionAnswers([item], { now: Date.now() }));
    expect(listOf(third)).toBe(expected);
    expect(listOf(fourth)).toBe(expected);
  },
  180_000
);

// The same compaction with its cut right after a delivered message, which is what every Envoy
// delivery with `triggerTurn` is: the branch holds it as a `custom_message` entry, and the
// compaction's conversion makes of it `{role: "custom", ..., timestamp: new Date(s).getTime()}`
// (`yu`). The cut search (`qge`) cuts where the role changes between assistant and not, so with
// the newest turn kept the cut comes between the delivery and its reply.
test.skipIf(skipped)(
  "(m) a remote compaction cut right after a delivered message ends at it with no list, and keeps no message past it",
  async () => {
    const item = answer("ask_m", "Compact after the delivery.", 1);
    const session = await runSession(
      binary(),
      [
        { route: { kind: "list", items: [item] } },
        { prompt: "First question." },
        { prompt: "Second question." },
        { deliver: "ENVOY-DELIVERY: CI finished." },
        { compact: true },
        { prompt: "Fourth question." },
      ],
      {
        methodOrder: "[remote, soft]",
        model: ["compat: {supportsServerCompaction: true, bedrockMessagesApi: true}"],
      }
    );

    const compactions = session.seen.filter(isRemoteCompaction);
    expect(compactions).toHaveLength(1);
    const compaction = compactions[0] as Seen;
    const [, , delivered, fourth, ...rest] = session.turns;
    if (delivered === undefined || fourth === undefined) throw new Error("want four turns");
    expect(rest).toEqual([]);
    // The delivery's turn ends at the delivery, then its list; the compaction request is that
    // turn's request without its list, message for message: nothing of the reply after the cut.
    const compacted = messagesOf(compaction);
    expect(textOf(compacted.at(-1))).toContain("ENVOY-DELIVERY: CI finished.");
    expect(shapeOf(compacted)).toEqual(shapeOf(withoutList(delivered)));
    expect(carriesList(compaction)).toBe(false);
    const expected = sentList(renderSessionAnswers([item], { now: Date.now() }));
    expect(listOf(delivered)).toBe(expected);
    expect(listOf(fourth)).toBe(expected);
  },
  180_000
);

// A remote compaction that keeps no tail: two delivered messages with no turn after a turn, the
// newest kept, and no role change from assistant to not past the cut, so `qge` returns the whole
// conversation's length and `buildLiveProviderContext` gets an empty tail. Its two conversions,
// `convert(Se)` and `convert([...Se, ...[]])`, then hold the same messages, both ending at the
// branch's last message, while the session compacts (`isCompacting`, the manual compaction's
// controller set).
test.skipIf(skipped)(
  "(n) a remote compaction that keeps no tail carries no list, and the turn after it carries the list",
  async () => {
    const item = answer("ask_n", "Compact it all.", 1);
    const session = await runSession(
      binary(),
      [
        { route: { kind: "list", items: [item] } },
        { prompt: "First question." },
        { deliver: "QUIET-ONE: a notice.", quiet: true },
        { deliver: "QUIET-TWO: another notice.", quiet: true },
        { compact: true },
        { prompt: "Second question." },
      ],
      {
        methodOrder: "[remote, soft]",
        model: ["compat: {supportsServerCompaction: true, bedrockMessagesApi: true}"],
      }
    );

    const compactions = session.seen.filter(isRemoteCompaction);
    expect(compactions).toHaveLength(1);
    const compaction = compactions[0] as Seen;
    const [first, second, ...rest] = session.turns;
    if (first === undefined || second === undefined) throw new Error("want two turns");
    expect(rest).toEqual([]);
    // The whole conversation, ending at the newest delivered message, with no list.
    const compacted = messagesOf(compaction);
    expect(textOf(compacted.at(-1))).toContain("QUIET-TWO: another notice.");
    expect(JSON.stringify(compacted)).toContain("QUIET-ONE: a notice.");
    expect(JSON.stringify(compacted)).toContain("First question.");
    expect(carriesList(compaction)).toBe(false);
    const expected = sentList(renderSessionAnswers([item], { now: Date.now() }));
    expect(listOf(first)).toBe(expected);
    expect(listOf(second)).toBe(expected);
  },
  180_000
);

// Two copies of this extension (a checkout's and an installed one, say) each run their own
// `context` handler on one request. When the first reads an empty list it appends nothing, so the
// second must still see that the request was decided, or it reads Dispatch again and the request
// waits on both reads.
test.skipIf(skipped)(
  "(l) two copies and an empty list: one answers request, and the request waits under a second",
  async () => {
    const session = await runSession(
      binary(),
      [{ route: { kind: "list", items: [], delayMs: 600 } }, { prompt: "First question." }],
      { twoCopies: true, warm: true }
    );

    // Both copies ran: each asked for the open asks as its turn started.
    expect(session.seen.filter((request) => request.path === "/api/v1/asks/open")).toHaveLength(2);
    const [first, ...rest] = session.turns;
    if (first === undefined) throw new Error("no turn 1");
    expect(rest).toEqual([]);
    expect(session.answers).toHaveLength(1);
    const ms = first.at - (session.answers[0] as Seen).at;
    console.log(`(l) the Messages request came ${ms.toFixed(0)} ms after the answers request`);
    expect(ms).toBeLessThan(1_000);
    expect(carriesList(first)).toBe(false);
  },
  180_000
);
