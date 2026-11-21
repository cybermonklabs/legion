import { afterEach, test } from "bun:test";
import { existsSync } from "node:fs";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { ROLE_TOPIC_PREFIX } from "@legion/contracts";
import {
  type Block,
  type Cleanup,
  call,
  isTerminalEnd,
  messageStream,
  type Request,
  type Rpc,
  runLegionPane,
} from "@legion/pi-shared/test/omp-harness";
import {
  dispatchStandin,
  type PaneLife,
  paneDriver,
  personAnswer,
  requestKind,
  rewriteReminderDates,
} from "./approval-message-capture";

// Fixture F1 of the approval message's plan: what the pinned Oh My Pi hands a `context` handler,
// and the wire body it sends, for each kind of model request a root architect's pane makes, as
// pi-legion's approval-message tests read them. Each case runs one root architect's pane
// (`runLegionPane`) on a profile of provider `anthropic` and model `claude-opus-5-5` whose base
// URL is the stand-in, with pi-envoy, then pi-legion, then the recorder beside this file loaded:
// the sandbox launch's order, the recorder last. It writes one JSON file under
// fixtures/approval-context/ for its shape: the recorder's `context`, `before_provider_request`,
// `message_end` and `session_compact` events, the model requests the stand-in served with their
// headers, and the case's own steps and the RPC frames that settle them, in the order they came.
// Nothing here asserts.
//
// The file runs only under LEGION_CAPTURE_FIXTURES=1 (`CAPTURE_COMMAND`), so `bun test` and CI
// skip it; a capture then needs LEGION_TEST_OMP, the binary of the repository's .omp-pin, and
// LEGION_TEST_NATS_URL. The remote compaction case answers the compaction request with the
// gateway's own response (remote-compaction-response.json, fixture F2) once that file exists. F2
// is captured once, by hand, from the request this case records; its `captured.command` says how.
const capture = process.env.LEGION_CAPTURE_FIXTURES === "1";

/** What each fixture says it was captured with. */
const CAPTURE_COMMAND =
  "cd packages/pi-legion && LEGION_TEST_OMP=<the omp .omp-pin names> " +
  "LEGION_TEST_NATS_URL=<nats url> LEGION_TEST_NATS_MONITOR_URL=<nats monitor url> " +
  "LEGION_CAPTURE_FIXTURES=1 bun test test/approval-context-capture.omp.test.ts";

const FIXTURES = path.join(import.meta.dir, "fixtures", "approval-context");
const RECORDER = path.join(import.meta.dir, "approval-context-recorder.ts");
/** F2: the gateway's answer to the remote compaction request this file records. */
const REMOTE_RESPONSE = path.join(FIXTURES, "remote-compaction-response.json");
/** The checkout the extensions load from. */
const CHECKOUT = path.resolve(import.meta.dir, "../../..");

/** The pane: a root architect, whose claim token and role lane these name. */
const NAME = "capture";
const ISSUE = "ACME-1";
const ROLE_LANE = `${ROLE_TOPIC_PREFIX}legion-${NAME}-${ISSUE.toLowerCase()}-architect`;

/** The approval message the recorder places in the date-reminder case: the daemon's own words
 *  (packages/daemon/internal/prompts/approval.md, the form for the root's own spec). */
const APPROVAL_TEXT =
  `A person has approved version 24 of the spec of ${ISSUE} (dispatch://${ISSUE}/spec@v24). ` +
  "Your top responsibility is now to deliver it and no other, at the highest standard. " +
  "The team will bring reasons to change it. Be prepared to refuse them, and to help find a " +
  "way to keep the goal and the spec whole. A change to what the spec delivers goes to the " +
  "person as one decision before any work on it, and a requirement the work missed goes back " +
  "to the work.";

/** What a placeholder in a fixture stands for: each local path and address, written over, and
 *  the two references `deduplicated` writes. */
const PLACEHOLDERS: Readonly<Record<string, string>> = {
  "<root>": "the case's scratch root: the profile's home, the workspace and the transcripts",
  "<scratch>": "the capture's own scratch directory, which holds the recorder's file",
  "<standin>": "the stand-in's base URL, http://127.0.0.1:<port>",
  "<standin-host>": "the stand-in's host and port",
  "<checkout>": "the repository checkout the extensions load from",
  "<user-home>": "the home of the user who ran the capture",
  "<tmp>": "the temporary directory",
  "<shared:NAME>":
    "the value `shared.NAME` holds, written once: a request's `system` or `tools`, or a string of 2000 characters or more that comes more than once",
  "<timeline[N].payload>":
    "a wire body the stand-in served that equals the `payload` timeline item N (counted from 0) recorded",
};

/** What must not be left in a fixture once the placeholders are written: a local path, a
 *  private host, a deployment key or a credential header. */
const LEAKS: readonly RegExp[] = [
  /\/home\//,
  /\/legion\/scratch/,
  /\/legion\/workspaces/,
  /cluster\.local/,
  /\bINFRA-\d/,
  /"(?:authorization|x-api-key)"\s*:/i,
];

/** The RPC frames the timeline keeps: what settles a case's steps. */
const SETTLING_FRAMES: Readonly<Record<string, true>> = {
  response: true,
  agent_start: true,
  agent_end: true,
  auto_compaction_start: true,
  auto_compaction_end: true,
  manual_compaction_start: true,
  manual_compaction_end: true,
  auto_retry_start: true,
  auto_retry_end: true,
  btw_record: true,
};

/** Headers whose name says they carry a credential; none is written to a fixture. */
const CREDENTIAL_HEADER = /authorization|api-key|cookie|token|secret|auth/i;

/**
 * A long answer of made-up words, about 20 000 characters, the same for the same `seed`:
 * printable ASCII in random order, dense in tokens for its characters, so that snapcompact's
 * bitmap archive (some 14 000 to 21 000 characters to a frame of a few thousand tokens,
 * @oh-my-pi/snapcompact's Anthropic shape) bills fewer tokens than the turns it replaces. Prose,
 * and words of letters and digits alone, bill about as much as their frames, and snapcompact
 * then refuses to run.
 */
function denseAnswer(seed: number): string {
  let state = seed;
  const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$%&*+-=?@^_~";
  const words: string[] = [];
  for (let length = 0; length < 20_000; ) {
    let word = "";
    state = (Math.imul(state, 1_103_515_245) + 12_345) >>> 0;
    const size = 3 + ((state >>> 16) % 6);
    for (let letter = 0; letter < size; letter += 1) {
      state = (Math.imul(state, 1_103_515_245) + 12_345) >>> 0;
      word += alphabet[(state >>> 16) % alphabet.length];
    }
    words.push(word);
    length += size + 1;
  }
  return words.join(" ");
}

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** The binary and the NATS server; a capture run without either fails, naming it. */
function setup(): { readonly binary: string; readonly nats: string } {
  const binary = process.env.LEGION_TEST_OMP;
  const nats = process.env.LEGION_TEST_NATS_URL;
  if (binary === undefined)
    throw new Error("a capture needs LEGION_TEST_OMP: the omp .omp-pin names");
  if (nats === undefined) throw new Error("a capture needs LEGION_TEST_NATS_URL: a nats-server");
  return { binary, nats };
}

/** One SSE reply of `blocks`, its `message_start` reporting `inputTokens` of input. */
function reply(blocks: readonly Block[], id: string, inputTokens?: number): Response {
  return new Response(messageStream(blocks, id, inputTokens), {
    headers: { "content-type": "text/event-stream" },
  });
}

function text(words: string): Block {
  return { type: "text", text: words };
}

/** F2's `response`: what the gateway answered, as the stand-in answers it again. */
function remoteResponse(fixture: unknown): {
  readonly status: number;
  readonly contentType: string;
  readonly body: string;
} {
  const response =
    typeof fixture === "object" && fixture !== null && "response" in fixture
      ? fixture.response
      : undefined;
  if (
    typeof response !== "object" ||
    response === null ||
    !("status" in response) ||
    typeof response.status !== "number" ||
    !("contentType" in response) ||
    typeof response.contentType !== "string" ||
    !("body" in response) ||
    typeof response.body !== "string"
  ) {
    throw new Error(`${REMOTE_RESPONSE} holds no response {status, contentType, body}`);
  }
  return { status: response.status, contentType: response.contentType, body: response.body };
}

/** What the people's answers route lists; the date-reminder case changes it between lives. */
interface AnswersRoute {
  items: Record<string, unknown>[];
}

/** What a case may do between its pane's two lives. */
interface Between {
  /** The pane's transcript directory. */
  readonly sessions: string;
  readonly answers: AnswersRoute;
  /** Makes the recorder place the stand-in approval message with `words` from now on. */
  readonly place: (words: string) => Promise<void>;
  /** Writes a step into the fixture's timeline. */
  readonly note: (step: string, fields?: Record<string, unknown>) => void;
}

interface CaseOptions {
  /** Lines appended to config.yml. */
  readonly config?: readonly string[];
  /** The model's input kinds in models.yml; text alone by default. */
  readonly input?: readonly ("text" | "image")[];
  /** The model's compat flags in models.yml; none by default. */
  readonly compat?: Readonly<Record<string, boolean>>;
  /** Answers one turn of the conversation, the parent's or a subagent's (no side call), or
   *  undefined for `Answer <turn>.`; `turn` counts them from 1, this one included. */
  readonly turn?: (turn: number, request: Request) => Response | undefined;
  /** The pane's first life. */
  readonly first: (life: PaneLife) => Promise<void>;
  /** A second life over the same session (`--resume`), with no prompt, after `between`. */
  readonly second?: {
    readonly between: (pane: Between) => Promise<void>;
    readonly life: (life: PaneLife) => Promise<void>;
  };
}

/**
 * Runs one capture: a root architect's pane through `options`' lives, then writes
 * fixtures/approval-context/<shape>.json, every local path and address written over
 * (`PLACEHOLDERS`).
 */
async function capturePane(shape: string, how: string, options: CaseOptions): Promise<void> {
  const { binary, nats } = setup();
  const version = await (async () => {
    const child = Bun.spawn([binary, "--version"], { stdout: "pipe", stderr: "pipe" });
    const printed = (await new Response(child.stdout).text()).trim();
    if ((await child.exited) !== 0) throw new Error(`${binary} --version failed`);
    return printed;
  })();
  const scratch = await mkdtemp(path.join(os.tmpdir(), "approval-capture-"));
  cleanup.push(() => rm(scratch, { recursive: true, force: true }));
  const recordFile = path.join(scratch, "record.jsonl");
  const placementFile = path.join(scratch, "placement.txt");
  const answers: AnswersRoute = { items: [] };
  const remote = existsSync(REMOTE_RESPONSE)
    ? remoteResponse(JSON.parse(await readFile(REMOTE_RESPONSE, "utf8")))
    : undefined;

  const timeline: Record<string, unknown>[] = [];
  const note = (step: string, fields: Record<string, unknown> = {}): void => {
    timeline.push({ at: Date.now(), source: "case", step, ...fields });
  };
  let lastServedAt = Date.now();
  let served = 0;
  let turns = 0;
  let sessionID = "";
  let root = "";
  let base = "";
  // Every step a case takes goes into the timeline: each RPC command it sends, and each card.
  const driver = paneDriver({
    nats,
    cleanup,
    lane: ROLE_LANE,
    session: () => sessionID,
    stepMs: 120_000,
    onStep: (step, fields) => note(step, fields),
  });

  const answerMessages = (request: Request): Response => {
    served += 1;
    const id = `msg_${served}`;
    const kind = requestKind(request);
    if (kind === "summary") return reply([text("SUMMARY-OF-THE-EARLIER-TURNS")], id);
    if ("compaction" in request.body) {
      if (remote !== undefined) {
        return new Response(remote.body, {
          status: remote.status,
          headers: { "content-type": remote.contentType },
        });
      }
      // Before F2 is captured: a reply with no compaction block, which fails the remote method.
      return reply([text("No compaction block: F2 is not captured yet.")], id);
    }
    if (kind === "handoff") {
      return reply([text("HANDOFF-DOCUMENT: the plan is drafted; next, review it.")], id);
    }
    if (kind === "side") return reply([text("PROCEEDING")], id);
    if (kind === "tool-less") return reply([text("Label.")], id);
    turns += 1;
    return options.turn?.(turns, request) ?? reply([text(`Answer ${turns}.`)], id);
  };

  /** The case's view of one life of the pane: the shared driver's, whose `quiet` here waits for
   *  the stand-in to serve no model request for `ms`. */
  const lifeOf = async (rpc: Rpc): Promise<PaneLife> => ({
    ...(await driver.life(rpc)),
    quiet: async (ms) => {
      // A real clock, deliberately: what is waited for is that the real host sends nothing
      // more, and a host that sends nothing emits no signal to await.
      while (Date.now() - lastServedAt < ms) await Bun.sleep(100);
    },
  });

  const pane = await runLegionPane(
    binary,
    [],
    {
      name: NAME,
      role: "architect",
      tree: ISSUE,
      issue: ISSUE,
      nats,
      prompt: [],
      dispatch: true,
      providers: [
        {
          provider: "anthropic",
          models: ["claude-opus-5-5"],
          ...(options.input === undefined ? {} : { input: options.input }),
          ...(options.compat === undefined ? {} : { compat: options.compat }),
        },
      ],
      config: options.config ?? [],
      extensions: [RECORDER],
      env: { LEGION_CAPTURE_RECORD: recordFile, LEGION_CAPTURE_PLACEMENT: placementFile },
      prepare: async ({ workspace }) => {
        root = path.dirname(workspace);
      },
      onFrame: (frame) => {
        driver.onFrame(frame);
        const record = frame as Record<string, unknown>;
        if (SETTLING_FRAMES[String(record.type)] === true) {
          const { messages: _messages, ...kept } = record;
          timeline.push({ at: Date.now(), source: "rpc", frame: kept });
        }
      },
      answer: (url, body, headers) => {
        if (base === "") base = url.origin;
        if (url.pathname === "/anthropic/v1/messages") {
          lastServedAt = Date.now();
          const kept: Record<string, string> = {};
          const withheld: string[] = [];
          for (const [header, value] of headers) {
            if (CREDENTIAL_HEADER.test(header)) withheld.push(header);
            else kept[header] = value;
          }
          timeline.push({
            at: lastServedAt,
            source: "standin",
            path: url.pathname,
            query: url.search,
            headers: kept,
            // A credential header is named, never written: F2 sends the gateway's own in its
            // place.
            withheldHeaders: withheld,
            body,
          });
        }
        if (url.pathname === "/v1/interests/subscribe" && typeof body.session_id === "string") {
          sessionID = body.session_id;
        }
        const dispatch = dispatchStandin(url, answers.items);
        if (dispatch !== undefined) return dispatch;
        if (url.pathname !== "/anthropic/v1/messages") return undefined;
        return answerMessages({ path: url.pathname, body });
      },
      whileUp: async (rpc) => {
        await options.first(await lifeOf(rpc));
      },
    },
    cleanup
  );

  const { second } = options;
  if (second !== undefined) {
    await second.between({
      sessions: pane.sessions,
      answers,
      note,
      place: (words) => writeFile(placementFile, words),
    });
    await pane.resume([], [], async (rpc) => {
      await second.life(await lifeOf(rpc));
    });
  }

  const recorded: Record<string, unknown>[] = existsSync(recordFile)
    ? (await readFile(recordFile, "utf8"))
        .split("\n")
        .filter(Boolean)
        .map((line) => ({ source: "recorder", ...(JSON.parse(line) as Record<string, unknown>) }))
    : [];
  const { timeline: ordered, shared } = deduplicated(
    [...timeline, ...recorded]
      .sort((left, right) => Number(left.at) - Number(right.at))
      .map(({ at: _at, ...item }) => item)
  );
  const fixture = {
    captured: {
      command: CAPTURE_COMMAND,
      omp: version,
      date: new Date().toISOString().slice(0, 10),
    },
    shape,
    how,
    placeholders: PLACEHOLDERS,
    timeline: ordered,
    shared,
  };
  // Longest first, so a path under another is written over before the one it is under.
  const written = [
    [root, "<root>"],
    [scratch, "<scratch>"],
    [base, "<standin>"],
    [base.replace(/^https?:\/\//, ""), "<standin-host>"],
    [CHECKOUT, "<checkout>"],
    [os.homedir(), "<user-home>"],
    [os.tmpdir(), "<tmp>"],
  ]
    .filter(([local]) => local !== undefined && local !== "")
    .sort(([left], [right]) => (right?.length ?? 0) - (left?.length ?? 0))
    .reduce(
      (json, [local, placeholder]) => json.replaceAll(local ?? "", placeholder ?? ""),
      JSON.stringify(fixture, null, 2)
    );
  for (const leak of LEAKS) {
    const found = leak.exec(written);
    if (found !== null) {
      throw new Error(
        `${shape}: the fixture still holds ${JSON.stringify(found[0])} at ${found.index}: ` +
          JSON.stringify(written.slice(Math.max(0, found.index - 80), found.index + 80))
      );
    }
  }
  await mkdir(FIXTURES, { recursive: true });
  await writeFile(path.join(FIXTURES, `${shape}.json`), `${written}\n`);
}

/** How long a string must be before `deduplicated` writes a repeated one once. */
const LONG_STRING = 2_000;

/**
 * The timeline written once per value that would otherwise repeat, which is most of a fixture's
 * size: each request's `system` and `tools` go into `shared` and each body that holds them names
 * them (`<shared:system-1>`); each string of `LONG_STRING` characters or more that comes more
 * than once (a long answer, a tool's output, the dispatch-first skill) goes into `shared` the
 * same way (`<shared:text-1>`); and each wire body the stand-in served that equals a
 * `before_provider_request` payload before it names that item (`<timeline[N].payload>`).
 */
function deduplicated(items: readonly Record<string, unknown>[]): {
  readonly timeline: unknown[];
  readonly shared: Record<string, unknown>;
} {
  const shared: Record<string, unknown> = {};
  const names = new Map<string, string>();
  const nameOf = (kind: string, value: unknown): string => {
    const json = JSON.stringify(value);
    let name = names.get(json);
    if (name === undefined) {
      const count = [...names.values()].filter((known) => known.startsWith(`${kind}-`)).length;
      name = `${kind}-${count + 1}`;
      names.set(json, name);
      shared[name] = value;
    }
    return `<shared:${name}>`;
  };
  const sharedIn = (body: unknown): unknown => {
    if (typeof body !== "object" || body === null) return body;
    const out: Record<string, unknown> = { ...body };
    for (const kind of ["system", "tools"]) {
      if (out[kind] !== undefined) out[kind] = nameOf(kind, out[kind]);
    }
    return out;
  };
  const bodies = items.map((item, index) => {
    if (item.event === "before_provider_request") {
      return { ...item, payload: sharedIn(item.payload) };
    }
    if (item.source !== "standin") return item;
    for (let earlier = index - 1; earlier >= 0; earlier -= 1) {
      const candidate = items[earlier];
      if (
        candidate?.event === "before_provider_request" &&
        Bun.deepEquals(candidate.payload, item.body)
      ) {
        return { ...item, body: `<timeline[${earlier}].payload>` };
      }
    }
    return { ...item, body: sharedIn(item.body) };
  });
  // Long strings: counted over the whole timeline first, then each repeated one named.
  const seen = new Map<string, number>();
  const count = (value: unknown): void => {
    if (typeof value === "string") {
      if (value.length >= LONG_STRING) seen.set(value, (seen.get(value) ?? 0) + 1);
    } else if (Array.isArray(value)) {
      for (const element of value) count(element);
    } else if (typeof value === "object" && value !== null) {
      for (const element of Object.values(value)) count(element);
    }
  };
  const named = (value: unknown): unknown => {
    if (typeof value === "string") {
      return (seen.get(value) ?? 0) > 1 ? nameOf("text", value) : value;
    }
    if (Array.isArray(value)) return value.map(named);
    if (typeof value === "object" && value !== null) {
      return Object.fromEntries(Object.entries(value).map(([key, field]) => [key, named(field)]));
    }
    return value;
  };
  count(bodies);
  return { timeline: bodies.map(named), shared };
}

const CASE_MS = 300_000;

test.skipIf(!capture)(
  "idle-card",
  async () => {
    await capturePane(
      "idle-card",
      "A root architect's pane, idle with no prompt, is given one card on its role lane. The " +
        "card's run is a tool call (the stand-in answers bash `echo checked`), then text.",
      {
        turn: (turn) =>
          turn === 1 ? reply([call("bash", { command: "echo checked" })], "msg_tool") : undefined,
        first: async (life) => {
          await life.card("ACME-1: a person commented on the spec.");
          await life.quiet(1_500);
        },
      }
    );
  },
  CASE_MS
);

test.skipIf(!capture)(
  "task-prompt",
  async () => {
    await capturePane(
      "task-prompt",
      "Two task prompts, as the daemon sends a task over RPC (`prompt`), each answered with text: " +
        "the second request carries the first prompt's answer.",
      {
        first: async (life) => {
          await life.prompt("ACME-1: write the plan.");
          await life.prompt("ACME-1: now list the risks.");
        },
      }
    );
  },
  CASE_MS
);

test.skipIf(!capture)(
  "tool-result-then-aside",
  async () => {
    await capturePane(
      "tool-result-then-aside",
      "A task prompt whose answer is a slow tool call (bash `sleep 3; echo slept`). A card is " +
        "relayed once the tool starts, so it reaches the next request as an aside after the tool " +
        "result. The stand-in answers that request with text.",
      {
        turn: (turn) =>
          turn === 1
            ? reply([call("bash", { command: "sleep 3; echo slept" })], "msg_tool")
            : undefined,
        first: async (life) => {
          const started = life.frame(
            (frame) => frame.type === "tool_execution_start",
            "the tool call's start"
          );
          const ended = life.frame(isTerminalEnd, "the run's end");
          life.send({ type: "prompt", message: "ACME-1: check the build." });
          await started;
          await life.card("ACME-1: CI finished on the pull request.", undefined, false);
          await ended;
          await life.quiet(1_500);
        },
      }
    );
  },
  CASE_MS
);

test.skipIf(!capture)(
  "compaction-remote",
  async () => {
    await capturePane(
      "compaction-remote",
      "Three task prompts, then the RPC `compact` under `compaction.methodOrder: [remote]` and " +
        "`keepRecentTokens: 1`, then one more prompt. The stand-in is no first-party endpoint, " +
        "so the model sets `compat: {supportsServerCompaction: true, bedrockMessagesApi: true}`, " +
        "which is what makes the pinned build take it for one that compacts " +
        "(ai/src/providers/anthropic-compaction.ts:53-58). The compaction request is answered " +
        "with F2 (remote-compaction-response.json), the gateway's own response to the request " +
        "an earlier capture recorded. The compaction request reaches `context` twice and " +
        "`before_provider_request` never: the native request passes no payload hook " +
        "(agent/src/compaction/anthropic.ts:262-280), so the stand-in's record is its wire body.",
      {
        config: ["compaction:", "  keepRecentTokens: 1", "  methodOrder: [remote]"],
        compat: { supportsServerCompaction: true, bedrockMessagesApi: true },
        first: async (life) => {
          await life.prompt("ACME-1: write the plan.");
          await life.prompt("ACME-1: now list the risks.");
          await life.prompt("ACME-1: now name the first step.");
          await life.command("compact");
          await life.prompt("ACME-1: continue with the first step.");
        },
      }
    );
  },
  CASE_MS
);

test.skipIf(!capture)(
  "compaction-snapcompact",
  async () => {
    await capturePane(
      "compaction-snapcompact",
      "Four task prompts with long answers of made-up words (20 000 characters each, dense in " +
        "tokens), then the RPC `compact` under `compaction.methodOrder: [snapcompact]` and " +
        "`keepRecentTokens: 1` on a model that takes images (`input: [text, image]`), then one " +
        "more prompt. Snapcompact keeps one frame's worth of text at each end of what it " +
        "archives and images only the middle (@oh-my-pi/snapcompact, `planArchive`), so the " +
        "archived turns must hold more than two frames of text before it reduces anything. It " +
        "makes no model request of its own.",
      {
        input: ["text", "image"],
        config: ["compaction:", "  keepRecentTokens: 1", "  methodOrder: [snapcompact]"],
        turn: (turn) =>
          turn <= 4 ? reply([text(denseAnswer(turn))], `msg_long_${turn}`) : undefined,
        first: async (life) => {
          await life.prompt("ACME-1: write the plan.");
          await life.prompt("ACME-1: now list the risks.");
          await life.prompt("ACME-1: now name the first step.");
          await life.prompt("ACME-1: now name the second step.");
          await life.command("compact");
          await life.prompt("ACME-1: continue with the first step.");
        },
      }
    );
  },
  CASE_MS
);

test.skipIf(!capture)(
  "compaction-handoff",
  async () => {
    await capturePane(
      "compaction-handoff",
      "Two task prompts under `compaction.methodOrder: [handoff]` and `thresholdTokens: " +
        "100000`; the second answer reports 150000 input tokens, so the host compacts " +
        "automatically once that run ends (the handoff method runs only automatically, " +
        "coding-agent/src/session/session-maintenance.ts:1364-1365). Its request is the handoff " +
        "prompt, answered with a handoff document. Then one more prompt. A threshold whose " +
        "recovery band the system prompt and tools alone fill compacts nothing " +
        "(`#thresholdFixedContextDeadEnd`), hence 100000.",
      {
        config: [
          "compaction:",
          "  keepRecentTokens: 1",
          "  thresholdTokens: 100000",
          "  methodOrder: [handoff]",
        ],
        turn: (turn) => (turn === 2 ? reply([text("Answer 2.")], "msg_big", 150_000) : undefined),
        first: async (life) => {
          await life.prompt("ACME-1: write the plan.");
          const compacted = life.frame(
            (frame) => frame.type === "auto_compaction_end",
            "the automatic compaction's end"
          );
          await life.prompt("ACME-1: now list the risks.");
          await compacted;
          await life.quiet(1_500);
          await life.prompt("ACME-1: continue with the first step.");
        },
      }
    );
  },
  CASE_MS
);

test.skipIf(!capture)(
  "compaction-shake",
  async () => {
    await capturePane(
      "compaction-shake",
      "Under `compaction.methodOrder: [shake]` and `thresholdTokens: 100000`: a task prompt " +
        "whose run makes two tool calls with long outputs (bash `seq 1 6000`, then " +
        "`seq 100001 106000`), the last answer reporting 150000 input tokens, so the host " +
        "shakes automatically once that run ends (shake runs only automatically, " +
        "coding-agent/src/session/session-maintenance.ts:1364-1365, and spares the newest " +
        "16000 tokens, agent/src/compaction/shake.ts:48-53). Shake makes no model request. " +
        "Then one more prompt.",
      {
        config: ["compaction:", "  thresholdTokens: 100000", "  methodOrder: [shake]"],
        turn: (turn) => {
          if (turn === 1) return reply([call("bash", { command: "seq 1 6000" })], "msg_seq_1");
          if (turn === 2) {
            return reply([call("bash", { command: "seq 100001 106000" })], "msg_seq_2");
          }
          if (turn === 3) return reply([text("Answer 3.")], "msg_big", 150_000);
          return undefined;
        },
        first: async (life) => {
          const compacted = life.frame(
            (frame) => frame.type === "auto_compaction_end",
            "the automatic compaction's end"
          );
          await life.prompt("ACME-1: print the numbers.");
          await compacted;
          await life.quiet(1_500);
          await life.prompt("ACME-1: continue with the first step.");
        },
      }
    );
  },
  CASE_MS
);

test.skipIf(!capture)(
  "compaction-soft",
  async () => {
    await capturePane(
      "compaction-soft",
      "Three task prompts, then the RPC `compact` under `compaction.methodOrder: [soft]` and " +
        "`keepRecentTokens: 1`, then one more prompt. The summary request reaches " +
        "`before_provider_request` and no `context` handler.",
      {
        config: ["compaction:", "  keepRecentTokens: 1", "  methodOrder: [soft]"],
        first: async (life) => {
          await life.prompt("ACME-1: write the plan.");
          await life.prompt("ACME-1: now list the risks.");
          await life.prompt("ACME-1: now name the first step.");
          await life.command("compact");
          await life.prompt("ACME-1: continue with the first step.");
        },
      }
    );
  },
  CASE_MS
);

test.skipIf(!capture)(
  "aborted-turn",
  async () => {
    const held: ReadableStreamDefaultController<Uint8Array>[] = [];
    cleanup.push(async () => {
      for (const controller of held) {
        try {
          controller.close();
        } catch {
          // Already closed by the host's abort.
        }
      }
    });
    await capturePane(
      "aborted-turn",
      "A task prompt whose answer streams `message_start` and some text, then holds; the RPC " +
        "`abort` ends the run. Then one more prompt, whose request shows what is left of the " +
        "aborted turn.",
      {
        turn: (turn) => {
          if (turn !== 1) return undefined;
          const started = messageStream([text("Partial answer, cut off")], "msg_aborted")
            .split("\n\n")
            .filter((event) => !event.includes("content_block_stop"))
            .filter((event) => !event.includes("message_delta"))
            .filter((event) => !event.includes("message_stop"))
            .join("\n\n");
          return new Response(
            new ReadableStream<Uint8Array>({
              start(controller) {
                controller.enqueue(new TextEncoder().encode(`${started}\n\n`));
                held.push(controller);
              },
            }),
            { headers: { "content-type": "text/event-stream" } }
          );
        },
        first: async (life) => {
          const streaming = life.frame(
            (frame) => frame.type === "message_update",
            "the partial answer's update"
          );
          const ended = life.frame((frame) => frame.type === "agent_end", "the aborted run's end");
          life.send({ type: "prompt", message: "ACME-1: write the plan." });
          await streaming;
          await life.command("abort");
          await ended;
          await life.prompt("ACME-1: write the plan again.");
        },
      }
    );
  },
  CASE_MS
);

test.skipIf(!capture)(
  "retried-request",
  async () => {
    await capturePane(
      "retried-request",
      "A task prompt answered four times. Attempt 1: HTTP 529 with Anthropic's " +
        "`overloaded_error` body. Attempt 2: `message_start`, then an SSE `error` event of the " +
        "same type. Both fail before any content block, and Oh My Pi's provider retries them " +
        "itself, sending the same body again with no new `context` or `before_provider_request` " +
        "event (ai/src/providers/anthropic.ts:3721-3734). Attempt 3: `message_start`, one " +
        "thinking block, then the same SSE `error`: the thinking block marks the stream unsafe " +
        "to replay for the provider (anthropic.ts:2959-2960), so it gives up, and the agent " +
        "retries the turn, thinking alone being safe to replay for it " +
        "(coding-agent/src/session/turn-recovery.ts:1782-1815, :1960-1968): a new `context` " +
        "event without the failed turn, between `auto_retry_start` and `auto_retry_end`. " +
        "Attempt 4 is answered with text.",
      {
        turn: (turn) => {
          const overloaded = {
            type: "error",
            error: { type: "overloaded_error", message: "Overloaded" },
          };
          if (turn === 1) return Response.json(overloaded, { status: 529 });
          if (turn !== 2 && turn !== 3) return undefined;
          const [started] = messageStream([], `msg_overloaded_${turn}`).split("\n\n");
          const thinking = [
            `event: content_block_start\ndata: ${JSON.stringify({ type: "content_block_start", index: 0, content_block: { type: "thinking", thinking: "" } })}`,
            `event: content_block_delta\ndata: ${JSON.stringify({ type: "content_block_delta", index: 0, delta: { type: "thinking_delta", thinking: "Weighing the plan." } })}`,
          ];
          return new Response(
            [
              started,
              ...(turn === 3 ? thinking : []),
              `event: error\ndata: ${JSON.stringify(overloaded)}`,
              "",
            ].join("\n\n"),
            { headers: { "content-type": "text/event-stream" } }
          );
        },
        first: async (life) => {
          await life.prompt("ACME-1: write the plan.");
          await life.quiet(3_000);
        },
      }
    );
  },
  CASE_MS
);

test.skipIf(!capture)(
  "side-turn",
  async () => {
    await capturePane(
      "side-turn",
      "A task prompt answered with text, then the RPC `btw` (a side question), which the host " +
        "sends as a side turn over the conversation.",
      {
        first: async (life) => {
          await life.prompt("ACME-1: write the plan.");
          const recorded = life.frame(
            (frame) => frame.type === "btw_record",
            "the side question's record"
          );
          await life.command("btw", { question: "What is left to do?" });
          await recorded;
          await life.quiet(1_500);
        },
      }
    );
  },
  CASE_MS
);

test.skipIf(!capture)(
  "handoff-prompt",
  async () => {
    await capturePane(
      "handoff-prompt",
      "Two task prompts under `compaction.keepRecentTokens: 1`, then the RPC `handoff`, whose " +
        "request is the host's handoff prompt, answered with a handoff document; then one more " +
        "prompt. Under the default 20000 the conversation is too short to cut, and the host " +
        "refuses the handoff (`Nothing to hand off (already compacted)`, " +
        "coding-agent/src/session/session-maintenance.ts:2249-2256).",
      {
        config: ["compaction:", "  keepRecentTokens: 1"],
        first: async (life) => {
          await life.prompt("ACME-1: write the plan.");
          await life.prompt("ACME-1: now list the risks.");
          await life.command("handoff");
          await life.quiet(1_500);
          await life.prompt("ACME-1: continue with the first step.");
        },
      }
    );
  },
  CASE_MS
);

test.skipIf(!capture)(
  "task-subagent",
  async () => {
    await capturePane(
      "task-subagent",
      "A task prompt whose answer is a `task` call handing one subagent an assignment. The " +
        "subagent's requests come from its own session (`agent.kind` is `sub`); then the " +
        "parent's next request carries the task's result.",
      {
        turn: (turn) =>
          turn === 1
            ? reply(
                [
                  {
                    type: "tool_use",
                    name: "task",
                    input: {
                      i: "Delegating the report",
                      context: "A one-step check.",
                      tasks: [
                        {
                          name: "Reporter",
                          task: "SUBAGENT-ASSIGNMENT: report what you were told.",
                          solutionSpace: "Reply in one line.",
                        },
                      ],
                    },
                  },
                ],
                "msg_task"
              )
            : undefined,
        first: async (life) => {
          await life.prompt("ACME-1: delegate the report.");
          await life.quiet(1_500);
        },
      }
    );
  },
  CASE_MS
);

test.skipIf(!capture)(
  "date-reminder-resume",
  async () => {
    await capturePane(
      "date-reminder-resume",
      "Life 1: one task prompt, answered with text. Between the lives, the transcript's " +
        "`date-cwd-reminder` entries are rewritten to the day before, Dispatch's answers route " +
        "starts to list a person's answer, and the recorder starts to place a stand-in approval " +
        "message (a custom message of `legion-approval`, text only, attribution `agent`): last on " +
        "the first request, then right before its answer. Life 2 (`--resume`, no prompt): one " +
        "card, whose run is a tool call (bash `echo checked`), then text. The card's request " +
        "carries a new people's-answers message and the placement, then Oh My Pi's reminder of " +
        "today's date; the next request is the tool round.",
      {
        turn: (turn) =>
          turn === 2 ? reply([call("bash", { command: "echo checked" })], "msg_tool") : undefined,
        first: async (life) => {
          await life.prompt("ACME-1: write the plan.");
        },
        second: {
          between: async ({ sessions, answers, place, note }) => {
            await rewriteReminderDates(sessions, (from, to) => {
              note("journal entry rewritten", { from, to });
            });
            answers.items = [personAnswer(ISSUE)];
            note("answers route lists one answer", { items: answers.items });
            await place(APPROVAL_TEXT);
            note("the recorder places the approval message", { text: APPROVAL_TEXT });
          },
          life: async (life) => {
            await life.card("ACME-1: a person answered your ask.");
            await life.quiet(1_500);
          },
        },
      }
    );
  },
  CASE_MS
);
