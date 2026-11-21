// The capture of the approval message's check e4b07a29 (plan task t-12, the script
// scripts/e2e/approval-message.sh):
// real Oh My Pi sessions of a root architect, each with pi-envoy then pi-legion loaded (the sandbox
// launch's order, `agentArgv` in packages/daemon/internal/runtime/sandbox/manifest.go) and the
// recorder beside this file last, which writes every request body Oh My Pi sends
// (`before_provider_request`) to a file. pi-legion places the approval message itself: this run
// never sets LEGION_CAPTURE_PLACEMENT, so the recorder's own stand-in placement stays off.
//
// The daemon is a stand-in (`runLegionPane`, packages/pi-shared/test/omp-harness.ts): its claim
// routes, the Envoy listener, Dispatch's answers route and the approval route. The approval route
// answers `{approval: null}` until a case sends its entry, and the approved version from then on,
// in the daemon's own words: `approvalWords` runs the daemon's prompts.ApprovalMessage
// (packages/daemon/internal/prompts/cmd/approval-message). The entries are the daemon's own too:
// each file of packages/daemon/internal/workflow/testdata/approval-entries, which the daemon's
// approval-path test writes as the daemon renders it. A notice and a role message reach the
// session as an Envoy card on its role lane, built as the listener builds the envelope of the
// daemon's publish; a task reaches it as the RPC prompt the daemon's shim sends, which a session
// in a turn refuses and the shim sends again when the turn ends.
//
// Two model routes run the same cases (`ModelRoute`): scripts/e2e/approval-message.sh runs them on
// the pod's own models.yml and overlay, whose model is a real one, and
// approval-message-omp.test.ts runs them on a stand-in model in CI. Each session is given
// `CONTRACT` as its appended system prompt, which says what tool calls each message asks for, and
// the stand-in answers by the same rules (`standinReply`), so a case makes the same shape of
// requests on either route. A case drives all its steps first and then checks what the recorder
// wrote, so a failed check still leaves every step's requests in the record.
//
//   bun packages/pi-legion/test/approval-message-capture.ts --omp <binary> --nats <url> \
//     --envoy <plugin> --legion <plugin> --models <models.yml> --overlay <overlay.yml> \
//     [--entries every|each-path] [--evidence <dir>] [--negative-control]
//
// prints `== <case>`, what it observed and `ok <case>` or `FAIL <case>: <why>` for each case, and
// ends with `approval message capture: PASS`, or with FAIL naming the first case that failed, or
// the failure that stopped the run before its cases. It first checks the route: the models.yml's
// `anthropic` provider offers claude-sonnet-5 and the overlay's `default` model.
// `--negative-control` runs the same cases, whose placing ones fail by design, and then exits 0
// exactly when the run sent requests and none of them holds an approval message.
import { createHash, randomUUID } from "node:crypto";
import {
  closeSync,
  existsSync,
  openSync,
  readdirSync,
  readFileSync,
  readSync,
  statSync,
} from "node:fs";
import { copyFile, mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { ROLE_TOPIC_PREFIX } from "@legion/contracts";
import {
  type Block,
  type Cleanup,
  call,
  isSelfCheck,
  isSummarization,
  isTerminalEnd,
  messageStream,
  type Request,
  type Rpc,
  runLegionPane,
  sendRelayFrame,
} from "@legion/pi-shared/test/omp-harness";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { connect, headers as natsHeaders, StringCodec } from "nats";

const RECORDER = path.join(import.meta.dir, "approval-context-recorder.ts");
const ENTRIES = path.resolve(
  import.meta.dir,
  "../../daemon/internal/workflow/testdata/approval-entries"
);
const DAEMON_MODULE = path.resolve(import.meta.dir, "../../daemon");

/** The name every pane's fixture strings carry (`LegionPaneOptions.name`). */
const PANE = "approval";

/** How long one step of a pane may take before the case fails, naming it. A real model at its
 *  highest thinking level takes some tens of seconds over a run of three requests. */
const STEP_MS = 300_000;

/** How long a pane must print no RPC frame before a step that waits for nothing more is done. */
const QUIET_MS = 4_000;

/**
 * The appended system prompt of every session, and the rules `standinReply` answers by. It names
 * the tool calls each kind of message asks for, so a real model makes the requests a case reads,
 * and acts on no task or card.
 */
const CONTRACT = [
  "This session is a test of the model requests that a Legion architect's plugins build. Do no real work.",
  "Follow these rules in every turn, whatever a task, a notice, a card or any other message asks.",
  "1. A user message that says `Run the bash command` and a command in backticks names a command to run. Run each command named that way, in the order named, with one call of the bash tool each.",
  "2. A user message that says `Use the task tool once to hand one subagent this assignment:` asks for one call of the task tool with the assignment in backticks after it, and for nothing else.",
  "3. A user message that says `Run no command` asks for no tool call.",
  "4. Any other message, such as a task, a notice or an Envoy card, asks for one call of the bash tool with the command `echo checked`.",
  "5. When no call is left to make, reply with one short sentence. Call no other tool, read and write no file, and ask no question.",
].join("\n");

/** The prompts the cases send, in the words `CONTRACT` reads. */
const WARM_UP = "Run no command: reply with the word ready.";
const FOLLOW_UP = "Run the bash command `echo again`.";
const IN_TURN =
  "Run the bash command `sleep 4; echo first`, then run the bash command `echo second`.";
const DELEGATE =
  "Use the task tool once to hand one subagent this assignment: `Reply with the word done.`";

/** How the host's handoff prompt begins (`handoff-document.md` at the pinned tag). */
export const HANDOFF_PROMPT = "Write a handoff document for another instance of yourself.";

/** Oh My Pi's date reminder on the wire (`prompts/system/date-cwd-reminder.md` at the pinned
 *  tag), as pi-legion's `DATE_REMINDER` reads it. */
const DATE_REMINDER =
  /^<system-reminder>\nToday: \d{4}-\d{2}-\d{2}; current working directory: '.*'\. Do not repeat this information in your reply\.\n<\/system-reminder>$/;

/** How pi-envoy's people's-answers message begins (`session-answers-context.ts`). */
const ANSWERS_MESSAGE = "[Dispatch: what people wrote to this session";

/** What pi-legion logs once for each session and model that takes no system message inside the
 *  conversation (`tellUserRole`, src/approval-message.ts). */
const USER_ROLE_LINE = "which takes no system message inside the conversation";

// The trees the entries name, as the daemon's approval-path test names them
// (packages/daemon/internal/workflow/approval_route_paths_test.go): OWN_ROOT is a root with its own
// spec, CARRIED_ROOT a root that carries a copy of OTHER_ROOT's gate.
const OWN_ROOT = "ACME-2694";
const OWN_DOCUMENT = "aec8fb48-fd46-458a-99a3-a1b199a46b38";
const OTHER_ROOT = "ACME-900";
const OTHER_DOCUMENT = "5b0f3c2e-7d14-4a9b-8e61-2c9a7f4d1b03";
const CARRIED_ROOT = "ACME-950";

/** One approval, in the fields of the daemon's `prompts.Approval`. */
export interface Approval {
  readonly tree: string;
  readonly spec: string;
  readonly document: string;
  readonly version: number;
  readonly carried?: boolean;
}

/** What the approval route answers for one approval (`api.ClaimApproval`). */
export interface RouteAnswer {
  readonly document: string;
  readonly version: number;
  readonly message: string;
}

function own(version: number): Approval {
  return { tree: OWN_ROOT, spec: OWN_ROOT, document: OWN_DOCUMENT, version };
}

function carried(version: number): Approval {
  return {
    tree: CARRIED_ROOT,
    spec: OTHER_ROOT,
    document: OTHER_DOCUMENT,
    version,
    carried: true,
  };
}

function approvalKey(approval: Approval): string {
  return `${approval.tree} ${approval.spec} ${approval.version} ${approval.carried === true}`;
}

/** One file of testdata/approval-entries: the path of the spec's Problem it is an entry of, and
 *  the approval the route answers once it is sent, which the daemon's test reads at the commit of
 *  that path's transaction (`wantOwn`, `wantCarried`). */
export interface Entry {
  readonly name: string;
  readonly path: number;
  readonly approval: Approval;
}

const ENTRY_FILES: readonly Entry[] = [
  { name: "notice-design-approved-from-artifact-approved", path: 1, approval: own(3) },
  { name: "notice-design-approved-from-a-registration", path: 1, approval: own(3) },
  { name: "notice-design-approved-after-an-exact-undo", path: 1, approval: own(46) },
  { name: "notice-design-approved-from-the-gate-seed", path: 1, approval: own(3) },
  { name: "notice-design-approved-from-gate-above-dispatch", path: 1, approval: own(4) },
  { name: "notice-carried-gate-opened", path: 2, approval: carried(12) },
  {
    name: "task-carried-gate-opened-kept-for-a-parked-carried-root",
    path: 2,
    approval: carried(12),
  },
  { name: "role-message-gate-opened", path: 3, approval: own(3) },
  { name: "task-resume-answer-awaited-gate", path: 4, approval: own(3) },
  { name: "task-resume-tell-in-place", path: 4, approval: own(2) },
  { name: "task-resume-answer-open-gates", path: 4, approval: own(3) },
  { name: "task-resume-boot-catch-up", path: 4, approval: own(3) },
  { name: "task-start-of-a-carried-root", path: 5, approval: carried(12) },
  { name: "task-start-of-a-root-whose-gate-is-open", path: 5, approval: own(3) },
  { name: "task-start-of-the-readmitted-root", path: 5, approval: own(2) },
  { name: "notice-catch-up-at-a-launch", path: 6, approval: own(3) },
  { name: "notice-catch-up-of-a-carried-root", path: 6, approval: carried(12) },
  { name: "task-design-approved-kept-for-a-parked-root", path: 6, approval: own(2) },
];

function entryFile(name: string): Entry {
  const found = ENTRY_FILES.find((candidate) => candidate.name === name);
  if (found === undefined) throw new Error(`no entry file ${name}`);
  return found;
}

/** The route's answer for each approval the cases use, by `approvalKey`. */
export type Words = ReadonlyMap<string, RouteAnswer>;

/**
 * The route's answer for every approval the cases use, its message the daemon's own: one `go run`
 * of packages/daemon/internal/prompts/cmd/approval-message, which fills prompts.ApprovalMessage.
 * The document is named as the route names it (`api.approvalAnswer`): the spec's
 * `dispatch://<key>/spec`, since every approval here names its spec's key.
 */
export async function approvalWords(): Promise<Words> {
  const approvals = [
    ...new Map(
      [...ENTRY_FILES.map((file) => file.approval), own(3), own(4)].map((one) => [
        approvalKey(one),
        one,
      ])
    ).values(),
  ];
  const child = Bun.spawn(["go", "run", "./internal/prompts/cmd/approval-message"], {
    cwd: DAEMON_MODULE,
    stdin: new TextEncoder().encode(JSON.stringify(approvals)),
    stdout: "pipe",
    stderr: "pipe",
  });
  const [printed, problems, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (code !== 0) {
    throw new Error(
      `go run ./internal/prompts/cmd/approval-message exited ${code}: ${problems.trim()}`
    );
  }
  const messages: unknown = JSON.parse(printed);
  if (
    !Array.isArray(messages) ||
    messages.length !== approvals.length ||
    !messages.every((message) => typeof message === "string" && message !== "")
  ) {
    throw new Error(`approval-message printed ${printed.trim()}, not one message per approval`);
  }
  return new Map(
    approvals.map((approval, index) => [
      approvalKey(approval),
      {
        document: `dispatch://${approval.spec}/spec`,
        version: approval.version,
        message: String(messages[index]),
      },
    ])
  );
}

/** The model a pane runs. */
type ModelName = "opus" | "sonnet";

/** Where a pane's model requests go. */
export type ModelRoute =
  | { readonly kind: "standin" }
  | {
      readonly kind: "pod";
      /** The pod's own models.yml's `anthropic` provider, parsed once per run (`podGateway`). */
      readonly gateway: Gateway;
      /** The pod's operator overlay, which the pane reads as the pod's sessions read it
       *  (`PI_CONFIG_FILES`). */
      readonly overlay: string;
    };

/** The stand-in route's profile: provider `anthropic` with the two models and no compat line, so
 *  each takes its compat from Oh My Pi's catalogue, as the pod's models do. */
const STANDIN_PROVIDERS = [
  { provider: "anthropic", models: ["claude-opus-5-5", "claude-sonnet-5"] },
] as const;

/** The pod route's `anthropic` provider, read from its models.yml in this process alone and never
 *  printed: the gateway's base URL, the key where the file holds a literal one, and the ids of the
 *  models it offers. */
interface Gateway {
  readonly provider: Readonly<Record<string, unknown>>;
  readonly base: string;
  readonly key: string | undefined;
  readonly ids: readonly string[];
}

/** The key the run's models.yml names where the pod's names a literal one: the recording proxy
 *  puts the real key in its place as it forwards each request. Random, as a real key is. */
const PROXY_KEY = fixtureSecret("proxy");

function podGateway(models: string): Gateway {
  const parsed = Bun.YAML.parse(readFileSync(models, "utf8")) as {
    readonly providers?: Readonly<Record<string, unknown>>;
  } | null;
  const provider = parsed?.providers?.anthropic;
  if (typeof provider !== "object" || provider === null) {
    fail(`${models} names no anthropic provider`);
  }
  const { baseUrl, apiKey, models: offered } = provider as Record<string, unknown>;
  if (typeof baseUrl !== "string") fail(`${models} names no baseUrl for the anthropic provider`);
  return {
    provider: provider as Record<string, unknown>,
    base: baseUrl.replace(/\/+$/, ""),
    key: typeof apiKey === "string" && !apiKey.startsWith("!") ? apiKey : undefined,
    ids: (Array.isArray(offered) ? offered : []).flatMap((model: unknown) =>
      typeof model === "object" && model !== null && "id" in model && typeof model.id === "string"
        ? [model.id]
        : []
    ),
  };
}

/**
 * The model the pod's overlay puts on `default`, which every case but the second pass's runs
 * (`--model @default`, as the sandbox launch runs it), checked before any case runs: the run's
 * models.yml holds the pod's `anthropic` provider alone, so that provider must offer it, and
 * claude-sonnet-5, which the second pass names. The cases on the default read each placement as
 * `system`, so that model must take a system message inside the conversation.
 */
function overlayDefault(overlay: string, gateway: Gateway, models: string): string {
  if (!gateway.ids.includes("claude-sonnet-5")) {
    fail(`${models}'s anthropic provider offers no model claude-sonnet-5`);
  }
  const parsed = Bun.YAML.parse(readFileSync(overlay, "utf8")) as {
    readonly modelRoles?: Readonly<Record<string, unknown>>;
  } | null;
  const selector = parsed?.modelRoles?.default;
  if (typeof selector !== "string") fail(`${overlay} puts no model on modelRoles.default`);
  // `anthropic/<id>`, the provider's name and a thinking level after a colon each optional.
  const id = selector.replace(/^anthropic\//, "").replace(/:[^/:]*$/, "");
  if (!gateway.ids.includes(id)) {
    fail(
      `${overlay} puts ${selector} on modelRoles.default, which ${models}'s anthropic provider does not offer`
    );
  }
  return selector;
}

/** The run's models.yml, as JSON, which is YAML: the pod's `anthropic` provider alone, its base
 *  URL the stand-in's recording proxy at `base`, and its key, where the pod's file holds a literal
 *  one, `PROXY_KEY`. */
function routeThroughProxy(gateway: Gateway, base: string): string {
  const key = gateway.key === undefined ? {} : { apiKey: PROXY_KEY };
  return JSON.stringify({
    providers: { anthropic: { ...gateway.provider, baseUrl: `${base}/anthropic`, ...key } },
  });
}

/**
 * The recording proxy's forward of one request the stand-in took (and recorded) to the pod's
 * gateway: the headers Oh My Pi sent, `PROXY_KEY` replaced by the real key, and the gateway's
 * answer streamed back as it comes. A failure names neither the gateway nor the key.
 */
async function forward(
  gateway: Gateway,
  url: URL,
  body: Record<string, unknown>,
  sent: Headers
): Promise<Response> {
  const headers = new Headers();
  for (const [name, value] of sent) {
    if (["host", "content-length", "connection", "accept-encoding"].includes(name)) continue;
    headers.set(name, gateway.key === undefined ? value : value.replaceAll(PROXY_KEY, gateway.key));
  }
  const post = Object.keys(body).length > 0;
  try {
    const answer = await fetch(
      `${gateway.base}${url.pathname.slice("/anthropic".length)}${url.search}`,
      { method: post ? "POST" : "GET", headers, ...(post ? { body: JSON.stringify(body) } : {}) }
    );
    const kept = new Headers(answer.headers);
    kept.delete("content-encoding");
    kept.delete("content-length");
    return new Response(answer.body, { status: answer.status, headers: kept });
  } catch {
    return Response.json(
      { type: "error", error: { type: "api_error", message: "the proxy reached no gateway" } },
      { status: 502 }
    );
  }
}

/** What every case runs against. */
export interface CaptureSetup {
  /** The Oh My Pi binary `.omp-pin` names. */
  readonly binary: string;
  /** The NATS server each pane's Envoy plugin connects to. */
  readonly nats: string;
  readonly route: ModelRoute;
  /** The two plugins' entries: a source entry, or a packed plugin's directory. */
  readonly plugins: { readonly envoy: string; readonly legion: string };
  /** Every pane loads pi-legion first, then pi-envoy; the reverse-order case then loads the
   *  sandbox launch's order. */
  readonly reverse: boolean;
  readonly words: Words;
  readonly cleanup: Cleanup;
  /** One line of what a case observed. */
  readonly note: (line: string) => void;
  /** Where each pane's record is copied once the pane ends, named for its case. */
  readonly evidence?: string;
  /** Every request every pane of the run sent, filled as each pane ends: what the negative
   *  control reads. */
  readonly sent: Sent[];
  /** The model every pane of the case runs. */
  readonly model: ModelName;
  /** The case's panes, as each opens: `runCase` reads their logs. */
  readonly panes: Pane[];
}

/** A wire message as the recorder wrote it. */
interface WireMessage {
  readonly role?: unknown;
  readonly content?: unknown;
}

/** What a Messages request is, as its body shows: the conversation's own (`main`), one of a
 *  compaction's summary requests (`summary`, `isSummarization`), the host's handoff prompt
 *  (`handoff`), a side turn (`side`, `isSelfCheck`), or one of the host's other side calls, which
 *  offer no tool (`tool-less`). */
export type RequestKind = "main" | "summary" | "handoff" | "side" | "tool-less";

/** What `request` is (`RequestKind`): how a stand-in model answers it, and how a case reads it. */
export function requestKind(request: Request): RequestKind {
  if (isSummarization(request)) return "summary";
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  if (textOf(messages.at(-1)).includes(HANDOFF_PROMPT)) return "handoff";
  if (isSelfCheck(request)) return "side";
  const { tools } = request.body;
  return Array.isArray(tools) && tools.length > 0 ? "main" : "tool-less";
}

/** One request body Oh My Pi sent, as the recorder wrote it. */
export interface Sent {
  /** Its order among the pane's requests, from 0. */
  readonly index: number;
  readonly pane: string;
  /** What the request is (`requestKind`), or a task subagent's (`sub`). */
  readonly kind: RequestKind | "sub";
  readonly messages: readonly WireMessage[];
  readonly perCall: readonly boolean[];
}

/** A wire message's text: its string content, or its text blocks joined. */
function textOf(message: unknown): string {
  if (typeof message !== "object" || message === null || !("content" in message)) return "";
  const { content } = message;
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((block: unknown) =>
      typeof block === "object" &&
      block !== null &&
      "type" in block &&
      block.type === "text" &&
      "text" in block &&
      typeof block.text === "string"
        ? block.text
        : ""
    )
    .join("\n");
}

/** A wire message's role and content, its `cache_control` marks set aside and a string content
 *  read as the one text block Oh My Pi's cache marking turns it into. */
function normalized(message: unknown): string {
  if (typeof message !== "object" || message === null) return JSON.stringify(message ?? null);
  const role = "role" in message ? message.role : undefined;
  const content = "content" in message ? message.content : undefined;
  const blocks =
    typeof content === "string"
      ? [{ type: "text", text: content }]
      : (Array.isArray(content) ? content : []).map((block: unknown) => {
          if (typeof block !== "object" || block === null) return block;
          const { cache_control: _mark, ...rest } = block as Record<string, unknown>;
          return rest;
        });
  return JSON.stringify({ role, content: blocks });
}

function describe(message: unknown): string {
  if (typeof message !== "object" || message === null) return "nothing";
  const text = textOf(message).replaceAll("\n", " ");
  const role = "role" in message ? String(message.role) : "no role";
  return `${role} ${JSON.stringify(text.length > 80 ? `${text.slice(0, 80)}…` : text)}`;
}

/**
 * The requests the recorder writes to `file`, read as it grows. Each call reads only what was
 * written since the last and parses only its complete lines, so a line the recorder is still
 * appending waits for the next call, and no line that is no `before_provider_request` event is
 * parsed. Answers every request read so far, in order.
 */
function recordReader(file: string, pane: string): () => readonly Sent[] {
  const sent: Sent[] = [];
  let offset = 0;
  return () => {
    const size = statSync(file, { throwIfNoEntry: false })?.size ?? 0;
    if (size <= offset) return sent;
    const chunk = Buffer.alloc(size - offset);
    const fd = openSync(file, "r");
    let read: number;
    try {
      read = readSync(fd, chunk, 0, chunk.length, offset);
    } finally {
      closeSync(fd);
    }
    const complete = chunk.subarray(0, read).lastIndexOf(0x0a) + 1;
    if (complete === 0) return sent;
    offset += complete;
    for (const line of chunk.subarray(0, complete).toString("utf8").split("\n")) {
      if (!line.includes('"event":"before_provider_request"')) continue;
      const event = JSON.parse(line) as Record<string, unknown>;
      if (event.event !== "before_provider_request") continue;
      const payload = (event.payload ?? {}) as Record<string, unknown>;
      const agent = event.agent as { readonly kind?: unknown } | undefined;
      sent.push({
        index: sent.length,
        pane,
        kind:
          agent?.kind === "sub"
            ? "sub"
            : requestKind({ path: "/anthropic/v1/messages", body: payload }),
        messages: (Array.isArray(payload.messages) ? payload.messages : []) as WireMessage[],
        perCall: (Array.isArray(event.perCall) ? event.perCall : []).map((flag) => flag === true),
      });
    }
    return sent;
  };
}

function fail(why: string): never {
  throw new Error(why);
}

/** The indexes of `messages` that are an approval message the run's route answers. */
function approvalsIn(messages: readonly unknown[], words: Words): number[] {
  const texts = new Set([...words.values()].map((answer) => answer.message));
  return messages.flatMap((message, at) => (texts.has(textOf(message)) ? [at] : []));
}

function mains(requests: readonly Sent[]): Sent[] {
  return requests.filter((request) => request.kind === "main");
}

/** The role a placement has: `system` where the model takes a system message inside the
 *  conversation (claude-opus-5-5), a user message where it takes none (claude-sonnet-5). */
type Role = "system" | "user";

/** The role a placement has on the case's model. */
function roleOf(setup: CaptureSetup): Role {
  return setup.model === "sonnet" ? "user" : "system";
}

/** A placement, once the request after it showed its answer. */
interface Placement {
  readonly words: string;
  /** The copy as the first request after the entry held it, normalized. */
  readonly copy: string;
  /** The assistant message that answered that request, normalized. */
  readonly answer: string;
  /** Where the copy sits in a request that holds its answer. */
  readonly place: number;
}

/** Every request of `requests` holds `count` approval messages. */
function holdsCount(requests: readonly Sent[], count: number, words: Words, label: string): void {
  for (const request of requests) {
    const held = approvalsIn(request.messages, words).length;
    if (held !== count) {
      fail(`${label}: request #${request.index} holds ${held} approval messages, want ${count}`);
    }
  }
}

/** `request` holds `placement`'s copy with the same bytes and role, right before the assistant
 *  message that answered it (and, with `atPlace`, where it sat before). */
function kept(request: Sent, placement: Placement, label: string, atPlace = false): void {
  const at = request.messages.findIndex((message) => textOf(message) === placement.words);
  if (at < 0) {
    fail(`${label}: request #${request.index} holds no copy of the approval message it placed`);
  }
  if (normalized(request.messages[at]) !== placement.copy) {
    fail(
      `${label}: request #${request.index} holds the approval message as ${describe(request.messages[at])}, not with the bytes and role it was placed with`
    );
  }
  if (normalized(request.messages[at + 1]) !== placement.answer) {
    fail(
      `${label}: request #${request.index} holds the approval message before ${describe(request.messages[at + 1])}, not right before the assistant message that answered it`
    );
  }
  if (atPlace && at !== placement.place) {
    fail(
      `${label}: request #${request.index} holds the approval message at ${at}, not at ${placement.place} where it sat`
    );
  }
}

/**
 * A new placement in `after`, the requests from an entry on: the first request of the
 * conversation holds `words` as its last entry with `role`, and the copies of `before` at their
 * places; every later request of the conversation holds it with the same bytes and role right
 * before the assistant message that answered that first request, and `before` too. Answers the
 * placement.
 */
function placedLast(
  after: readonly Sent[],
  words: string,
  role: Role,
  all: Words,
  label: string,
  before: readonly Placement[] = []
): Placement {
  const conversation = mains(after);
  const first =
    conversation[0] ?? fail(`${label}: no request of the conversation came after the entry`);
  const last = first.messages.at(-1);
  if (textOf(last) !== words) {
    fail(
      `${label}: the first request after the entry (#${first.index}) ends with ${describe(last)}, not the approval message`
    );
  }
  if (last?.role !== role) {
    fail(
      `${label}: the first request after the entry (#${first.index}) holds the approval message last as ${String(last?.role)}, want ${role}`
    );
  }
  holdsCount(conversation, before.length + 1, all, label);
  for (const earlier of before) kept(first, earlier, label);
  const next =
    conversation[1] ??
    fail(`${label}: no later request of the conversation came to show the message's place`);
  // The answer is the first assistant message after those the first request held.
  const ordinal = first.messages.filter((message) => message.role === "assistant").length;
  let answerAt = -1;
  for (let at = 0, seen = 0; at < next.messages.length; at += 1) {
    if (next.messages[at]?.role !== "assistant") continue;
    if (seen === ordinal) {
      answerAt = at;
      break;
    }
    seen += 1;
  }
  if (answerAt < 1) fail(`${label}: request #${next.index} holds no answer to #${first.index}`);
  const placement: Placement = {
    words,
    copy: normalized(last),
    answer: normalized(next.messages[answerAt]),
    place: answerAt - 1,
  };
  for (const later of conversation.slice(1)) {
    kept(later, placement, label);
    for (const earlier of before) kept(later, earlier, label);
  }
  return placement;
}

/** What a pane's stand-ins answer, which a case changes as it goes: the approval route, and
 *  Dispatch's people's answers. */
interface PaneState {
  route: RouteAnswer | null;
  answers: Record<string, unknown>[];
}

/** What a case does with one life of its pane: the steps of `PaneLife`, each prompt and card
 *  answering the mark before it, the number of requests the pane had sent then. */
interface Life extends Omit<PaneLife, "prompt" | "card"> {
  readonly state: PaneState;
  /** Every request the pane sent so far, in every life. */
  readonly sent: () => readonly Sent[];
  /** `PaneLife.prompt`. Answers the mark before it. */
  readonly prompt: (message: string) => Promise<number>;
  /** `PaneLife.card`. Answers the mark before it. */
  readonly card: (message: string, payload?: string, wait?: boolean) => Promise<number>;
}

interface PaneSpec {
  /** The case's name, which the pane's record is kept under. */
  readonly name: string;
  readonly tree: string;
  /** Lines of the profile's config.yml. */
  readonly config?: readonly string[];
  /** No thinking: the model refuses to summarize a conversation that holds its own thinking
   *  (`Refusal (reasoning_extraction)`, measured on the pod's route), so a compaction case's
   *  session runs `--thinking=off`. */
  readonly thinkingOff?: boolean;
  /** The other plugin order than the run's. */
  readonly reverse?: boolean;
  /** The route's answer from the launch on. */
  readonly route?: RouteAnswer | null;
}

interface Pane {
  readonly sent: () => readonly Sent[];
  /** Every model request the stand-in served: the stand-in model's, or, on the pod's route, those
   *  it recorded and forwarded to the gateway. Each holds its body alone, never a header. */
  readonly modelRequests: () => readonly Request[];
  /** The lines of every Oh My Pi log under the pane's home. */
  readonly logs: () => string[];
  readonly sessions: string;
  readonly relaunch: (life: (life: Life) => Promise<void>) => Promise<void>;
}

/** The listener's summary of a message: its first non-empty line, cut to 160 runes
 *  (`contracts.OneLineSummary`, packages/envoy/internal/contracts/normalize.go). */
function oneLineSummary(message: string): string {
  const line = message.split("\n").find((candidate) => candidate.trim() !== "") ?? "";
  const runes = [...line];
  return runes.length <= 160 ? line : `${runes.slice(0, 159).join("")}…`;
}

/** One life of a pane, as a case drives it over that life's RPC. */
export interface PaneLife {
  /** Sends one RPC command and waits for nothing. */
  readonly send: (command: Record<string, unknown>) => void;
  /** Resolves once `holds` is true, checked at each frame and every quarter second. */
  readonly until: (holds: () => boolean, label: string) => Promise<void>;
  /** Sends one RPC command and resolves with its `response` frame. */
  readonly command: (
    type: string,
    fields?: Record<string, unknown>
  ) => Promise<Record<string, unknown>>;
  /** Resolves once an RPC frame that comes after this call satisfies `holds`. */
  readonly frame: (
    holds: (frame: Record<string, unknown>) => boolean,
    label: string
  ) => Promise<void>;
  /** Sends one RPC prompt and resolves once its run has ended. */
  readonly prompt: (message: string) => Promise<void>;
  /** Relays one Envoy card on the pane's role lane once the session has registered, built as the
   *  listener builds the envelope of the daemon's publish of `message` and `payload`. With
   *  `wait`, resolves once the run it starts has ended; without, once the session took the
   *  frame. */
  readonly card: (message: string, payload?: string, wait?: boolean) => Promise<void>;
  /** Resolves once Oh My Pi has printed no RPC frame for `ms`. */
  readonly quiet: (ms: number) => Promise<void>;
}

/** What drives one pane across its lives: `onFrame` goes to `runLegionPane`, and each life's RPC
 *  to `life`. */
export interface PaneDriver {
  /** Keeps each RPC frame omp prints, counts the runs that ended, and looks at each wait again. */
  readonly onFrame: (frame: object) => void;
  /** The steps a case takes in one life, over a NATS connection of their own that the cleanup
   *  closes. */
  readonly life: (rpc: Rpc) => Promise<PaneLife>;
}

/** The driver of one pane, which this file's cases and the F1 capture
 *  (approval-context-capture.omp.test.ts) both run their panes with. */
export function paneDriver(options: {
  /** The NATS server each card is relayed through. */
  readonly nats: string;
  readonly cleanup: Cleanup;
  /** The pane's role lane, the topic of every card. */
  readonly lane: string;
  /** The session's id once it registered with the listener, empty before. */
  readonly session: () => string;
  /** How long one step may take before it fails, naming the step and the last frames: a bound
   *  only on a host that never does what is waited for, since one that does settles the wait at
   *  the frame, or the record, that says so. */
  readonly stepMs: number;
  /** Told each RPC command sent and each card relayed, as it goes. */
  readonly onStep?: (step: "rpc" | "card", fields: Record<string, unknown>) => void;
}): PaneDriver {
  const frames: Record<string, unknown>[] = [];
  const checks = new Set<() => void>();
  let terminalEnds = 0;
  let lastFrameAt = Date.now();
  return {
    onFrame: (frame) => {
      const record = frame as Record<string, unknown>;
      frames.push(record);
      lastFrameAt = Date.now();
      if (isTerminalEnd(record)) terminalEnds += 1;
      for (const check of [...checks]) check();
    },
    life: async (rpc) => {
      const nc = await connect({ servers: options.nats });
      options.cleanup.push(() => nc.close());
      const codec = StringCodec();
      let seq = 0;
      const until = (holds: () => boolean, label: string): Promise<void> => {
        if (holds()) return Promise.resolve();
        const { promise, resolve, reject } = Promise.withResolvers<void>();
        const check = (): void => {
          if (holds()) resolve();
        };
        // The record and the clock change with no frame, so the wait looks again on a ticker too.
        const ticker = setInterval(check, 250);
        const timer = setTimeout(() => {
          const seen = frames
            .slice(-30)
            .map(
              (frame) => `${String(frame.type)}${frame.command ? `:${String(frame.command)}` : ""}`
            );
          reject(
            new Error(
              `${label} did not happen within ${options.stepMs} ms; the last frames: ${seen}`
            )
          );
        }, options.stepMs);
        checks.add(check);
        // Whatever settles the wait, omp's exit among them, ends its ticker and its bound.
        return Promise.race([promise, rpc.closed]).finally(() => {
          checks.delete(check);
          clearInterval(ticker);
          clearTimeout(timer);
        });
      };
      const send = (command: Record<string, unknown>): void => {
        options.onStep?.("rpc", { command });
        rpc.send(command);
      };
      return {
        send,
        until,
        command: async (type, fields = {}) => {
          const from = frames.length;
          send({ type, ...fields });
          const answered = (): Record<string, unknown> | undefined =>
            frames.slice(from).find((frame) => frame.type === "response" && frame.command === type);
          await until(() => answered() !== undefined, `the response to ${type}`);
          return answered() ?? {};
        },
        frame: (holds, label) => {
          const from = frames.length;
          return until(() => frames.slice(from).some(holds), label);
        },
        prompt: async (message) => {
          const ended = terminalEnds;
          send({ type: "prompt", message });
          await until(
            () => terminalEnds > ended,
            `the run of the prompt ${JSON.stringify(message)}`
          );
        },
        card: async (message, payload, wait = true) => {
          await until(() => options.session() !== "", "the session's registration");
          const sessionID = options.session();
          const ended = terminalEnds;
          const key = `card-${randomUUID()}`;
          seq += 1;
          const summary = oneLineSummary(message);
          // The listener's MessageEnvelope of the daemon's publish: the payload as the publish
          // gave it, or else the whole message where the summary is not all of it.
          const body = payload ?? (summary === message ? undefined : message);
          options.onStep?.("card", { summary, topic: options.lane });
          await sendRelayFrame({
            connection: nc,
            newHeaders: natsHeaders,
            sessionID,
            subject: `notifications.agent.${sessionID}`,
            seq,
            payload: codec.encode(
              JSON.stringify({
                event_id: `evt-${key}`,
                source: "agent",
                source_event_id: `source-${key}`,
                topic: options.lane,
                dedupe_key: `envoy.role.forward.publish.${createHash("sha256").update(key).digest("hex").slice(0, 32)}`,
                issued_at: Date.now(),
                payload_summary: summary,
                ...(body === undefined ? {} : { payload: body }),
                trace_id: `trace-${key}`,
              })
            ),
          });
          if (wait) await until(() => terminalEnds > ended, `the run of the card ${summary}`);
        },
        quiet: (ms) => until(() => Date.now() - lastFrameAt >= ms, `${ms} ms with no frame`),
      };
    },
  };
}

function blocksOf(message: WireMessage | undefined): unknown[] {
  return Array.isArray(message?.content) ? message.content : [];
}

function toolCalls(message: WireMessage | undefined): number {
  return blocksOf(message).filter(
    (block) =>
      typeof block === "object" && block !== null && "type" in block && block.type === "tool_use"
  ).length;
}

/** The stand-in model's answer to one request, by `CONTRACT`'s rules. */
function standinReply(request: Request): readonly Block[] {
  const messages = (
    Array.isArray(request.body.messages) ? request.body.messages : []
  ) as WireMessage[];
  const done: readonly Block[] = [{ type: "text", text: "Done." }];
  switch (requestKind(request)) {
    case "summary":
      return [{ type: "text", text: "SUMMARY-OF-THE-EARLIER-TURNS" }];
    case "handoff":
      return [{ type: "text", text: "HANDOFF-DOCUMENT: the checks ran; nothing is left to do." }];
    case "side":
      return [{ type: "text", text: "Nothing is left to do." }];
    case "tool-less":
      return [{ type: "text", text: "Label." }];
    case "main":
      break;
  }
  // The run so far: every message after the last assistant message that called no tool.
  let start = messages.length;
  while (start > 0) {
    const message = messages[start - 1];
    if (message?.role === "assistant" && toolCalls(message) === 0) break;
    start -= 1;
  }
  const run = messages.slice(start);
  const said = run
    .filter((message) => message.role !== "assistant")
    .map(textOf)
    .join("\n");
  const calls = run.reduce((count, message) => count + toolCalls(message), 0);
  const delegate = /Use the task tool once to hand one subagent this assignment: `([^`]+)`/.exec(
    said
  );
  if (delegate?.[1] !== undefined) {
    if (calls > 0) return done;
    return [
      {
        type: "tool_use",
        name: "task",
        input: {
          i: "Delegating the check",
          context: "A one-step check.",
          tasks: [{ name: "Checker", task: delegate[1], solutionSpace: "Reply in one line." }],
        },
      },
    ];
  }
  if (said.includes("Run no command")) return done;
  const named = [...said.matchAll(/Run the bash command `([^`]+)`/g)].map((match) => match[1]);
  if (named.length === 0) return calls > 0 ? done : [call("bash", { command: "echo checked" })];
  const next = named[calls];
  return next === undefined ? done : [call("bash", { command: next })];
}

/** A person's answer to the session's ask, as Dispatch's answers route lists one. */
export function personAnswer(tree: string): Record<string, unknown> {
  return {
    kind: "answer",
    relation: "answered_your_ask",
    id: "ask_1",
    at: new Date(Date.now() - 60_000).toISOString(),
    author: { kind: "user", id: "person@example.com" },
    text: "Go the second way, and keep the retry at 3.",
    selected: ["The second way"],
    ask: { id: "ask_1", question: "Which way should the fix go?" },
    owner: { issue: { key: tree, title: "The work" } },
    ref: `dispatch://${tree}/ask/ask_1`,
    href: `/issues/${tree}#ask-ask_1`,
  };
}

/**
 * Dispatch's routes a pane reads, as a stand-in answers them: the session's people's answers,
 * `answers`, and the open-asks read, unavailable, which arms no run-end self-check, so every side
 * turn is a case's own. Undefined for any other route.
 */
export function dispatchStandin(
  url: URL,
  answers: readonly Record<string, unknown>[]
): Response | undefined {
  if (url.pathname === "/api/v1/asks/open") return new Response("unavailable", { status: 503 });
  const listed = /^\/api\/v1\/agents\/([^/]+)\/answers$/.exec(url.pathname);
  if (listed === null) return undefined;
  return Response.json({
    session_id: decodeURIComponent(listed[1] ?? ""),
    since: new Date(Date.now() - 72 * 3_600_000).toISOString(),
    total: answers.length,
    items: answers,
  });
}

/**
 * Runs one root architect's pane: its first life `first`, then each `relaunch` over the same
 * session (`--resume`). The pane's record is read as it grows, at every `sent()`; when the pane
 * ends it is copied into the evidence directory and its requests are added to `setup.sent`.
 */
async function openPane(
  setup: CaptureSetup,
  spec: PaneSpec,
  first: (life: Life) => Promise<void>
): Promise<Pane> {
  const scratch = await mkdtemp(path.join(os.tmpdir(), "approval-message-"));
  setup.cleanup.push(() => rm(scratch, { recursive: true, force: true }));
  const recordFile = path.join(scratch, "record.jsonl");
  const contractFile = path.join(scratch, "contract.md");
  await writeFile(contractFile, `${CONTRACT}\n`);
  const sent = recordReader(recordFile, spec.name);
  setup.cleanup.push(async () => {
    setup.sent.push(...sent());
    if (setup.evidence !== undefined && existsSync(recordFile)) {
      await mkdir(setup.evidence, { recursive: true });
      const named = `${setup.model === "sonnet" ? "sonnet-" : ""}${spec.name}`;
      await copyFile(recordFile, path.join(setup.evidence, `${named}.jsonl`));
    }
  });
  const state: PaneState = { route: spec.route ?? null, answers: [] };
  const { route } = setup;
  const lane = `${ROLE_TOPIC_PREFIX}legion-${PANE}-${spec.tree.toLowerCase()}-architect`;
  const legionFirst = setup.reverse !== (spec.reverse === true);

  let root = "";
  let sessionID = "";
  const modelRequests: Request[] = [];
  const gateway = route.kind === "pod" ? route.gateway : undefined;
  const driver = paneDriver({
    nats: setup.nats,
    cleanup: setup.cleanup,
    lane,
    session: () => sessionID,
    stepMs: STEP_MS,
  });

  const lifeOf = async (rpc: Rpc): Promise<Life> => {
    const life = await driver.life(rpc);
    return {
      ...life,
      state,
      sent,
      prompt: async (message) => {
        const mark = sent().length;
        await life.prompt(message);
        return mark;
      },
      card: async (message, payload, wait) => {
        await life.until(() => sessionID !== "", "the session's registration");
        const mark = sent().length;
        await life.card(message, payload, wait);
        return mark;
      },
    };
  };

  const pane = await runLegionPane(
    setup.binary,
    [],
    {
      name: PANE,
      role: "architect",
      tree: spec.tree,
      issue: spec.tree,
      nats: setup.nats,
      prompt: [],
      dispatch: true,
      plugins: legionFirst
        ? [setup.plugins.legion, setup.plugins.envoy]
        : [setup.plugins.envoy, setup.plugins.legion],
      extensions: [RECORDER],
      args: [
        "--model",
        // The pod's route runs the model its overlay puts on `default`, as the sandbox launch does.
        setup.model === "sonnet"
          ? "anthropic/claude-sonnet-5"
          : route.kind === "pod"
            ? "@default"
            : "anthropic/claude-opus-5-5",
        ...(spec.thinkingOff === true ? ["--thinking=off"] : []),
        "--append-system-prompt",
        contractFile,
      ],
      env: {
        LEGION_CAPTURE_RECORD: recordFile,
        ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
        ...(route.kind === "pod" ? { PI_CONFIG_FILES: route.overlay } : {}),
      },
      ...(gateway === undefined
        ? { providers: STANDIN_PROVIDERS, config: spec.config ?? [] }
        : {
            profile: async (agent: string, base: string) => {
              await writeFile(path.join(agent, "models.yml"), routeThroughProxy(gateway, base));
              await writeFile(
                path.join(agent, "config.yml"),
                [...(spec.config ?? []), ""].join("\n")
              );
            },
          }),
      prepare: async ({ workspace }) => {
        root = path.dirname(workspace);
      },
      onFrame: driver.onFrame,
      answer: (url, body, headers) => {
        if (url.pathname === "/anthropic/v1/messages") {
          modelRequests.push({ path: url.pathname, body });
        }
        if (url.pathname === "/v1/interests/subscribe" && typeof body.session_id === "string") {
          sessionID = body.session_id;
        }
        if (url.pathname === "/legion/v1/claims/approval") {
          return Response.json({ approval: state.route });
        }
        if (url.pathname === "/legion/v1/claims/activity") {
          return new Response(null, { status: 204 });
        }
        // A clean turn's outcome, which refuses nothing and fails no claim.
        if (url.pathname === "/legion/v1/claims/turn-outcome") {
          return Response.json({ refusedRun: 0, failed: false });
        }
        const dispatch = dispatchStandin(url, state.answers);
        if (dispatch !== undefined) return dispatch;
        if (gateway !== undefined && url.pathname.startsWith("/anthropic/")) {
          return forward(gateway, url, body, headers);
        }
        if (url.pathname !== "/anthropic/v1/messages") return undefined;
        const reply = standinReply({ path: url.pathname, body });
        return new Response(messageStream(reply, `msg_${modelRequests.length}`), {
          headers: { "content-type": "text/event-stream" },
        });
      },
      whileUp: async (rpc) => {
        await first(await lifeOf(rpc));
      },
    },
    setup.cleanup
  );

  const opened: Pane = {
    sent,
    modelRequests: () => modelRequests,
    logs: () => {
      const logs = path.join(root, "home", ".omp", "logs");
      if (!existsSync(logs)) return [];
      return readdirSync(logs, { recursive: true })
        .map((file) => path.join(logs, String(file)))
        .filter((file) => statSync(file).isFile())
        .flatMap((file) => readFileSync(file, "utf8").split("\n"));
    },
    sessions: pane.sessions,
    relaunch: (life) =>
      pane.resume([], [], async (rpc) => {
        await life(await lifeOf(rpc));
      }),
  };
  setup.panes.push(opened);
  return opened;
}

/** The route's answer for `approval`. */
function answerFor(setup: CaptureSetup, approval: Approval): RouteAnswer {
  return (
    setup.words.get(approvalKey(approval)) ??
    fail(`the daemon's words hold no message for ${approvalKey(approval)}`)
  );
}

/**
 * Sends `file`'s entry as the daemon sends it, and has the route answer `answer` from the moment
 * the entry reaches the session: a notice and a role message as a card on the role lane, a task
 * as the shim's prompt. With `inTurn` the session is in a turn, so a card is an aside, and a task
 * is refused until that turn ends, when the shim sends it again. Answers the mark at the entry.
 */
async function sendEntry(
  life: Life,
  file: Entry,
  answer: RouteAnswer,
  inTurn: boolean
): Promise<number> {
  const text = readFileSync(path.join(ENTRIES, `${file.name}.txt`), "utf8");
  if (file.name.startsWith("task-")) {
    if (inTurn) {
      const ended = life.frame(isTerminalEnd, "the end of the turn that refused the task");
      const refused = await life.command("prompt", { message: text });
      if (refused.success !== false || !String(refused.error).includes("already processing")) {
        fail(`the session in a turn answered the task ${JSON.stringify(refused)}, not busy`);
      }
      await ended;
    }
    life.state.route = answer;
    return life.prompt(text);
  }
  life.state.route = answer;
  if (file.name.startsWith("notice-")) {
    // The card's summary line, then the payload the notice publishes.
    const cut = text.indexOf("\n");
    return life.card(text.slice(0, cut), text.slice(cut + 1), !inTurn);
  }
  // A role message: the daemon publishes its body as both the message and the payload
  // (outbox.roleMessage), the payload encoded as JSON (notify.HTTPPublisher).
  return life.card(text, JSON.stringify(text), !inTurn);
}

/** Makes a later request of the conversation when the run after `mark` made only one. */
async function laterRequest(life: Life, mark: number): Promise<void> {
  if (mains(life.sent().slice(mark)).length < 2) await life.prompt(FOLLOW_UP);
}

/** One line of what a placement's requests showed. */
function noteOf(after: readonly Sent[], placement: Placement, role: Role): string {
  const conversation = mains(after);
  return `request #${conversation[0]?.index} holds the approval message last as ${role}; ${conversation.length - 1} later requests hold it at ${placement.place}, right before its answer`;
}

/** One case of the check. */
export interface CaptureCase {
  readonly name: string;
  readonly run: (setup: CaptureSetup) => Promise<void>;
  /** No approval stands in the case, so no process of it logs the line `runCase` reads. */
  readonly placesNone?: boolean;
}

/** `file`'s entry to a session that is idle after a turn of its own, or that is in a turn after a
 *  tool result. */
function entryCase(file: Entry, inTurn: boolean): CaptureCase {
  return {
    name: `${file.name}, ${inTurn ? "in a turn" : "idle"}`,
    run: async (setup) => {
      const answer = answerFor(setup, file.approval);
      let mark = 0;
      const pane = await openPane(
        setup,
        { name: `${file.name}-${inTurn ? "turn" : "idle"}`, tree: file.approval.tree },
        async (life) => {
          if (!inTurn) {
            await life.prompt(WARM_UP);
            mark = await sendEntry(life, file, answer, false);
          } else {
            const started = life.frame(
              (frame) => frame.type === "tool_execution_start",
              "the slow tool call's start"
            );
            const ended = life.frame(isTerminalEnd, "the end of the turn");
            life.send({ type: "prompt", message: IN_TURN });
            await started;
            mark = await sendEntry(life, file, answer, true);
            await ended;
          }
          await laterRequest(life, mark);
        }
      );
      const requests = pane.sent();
      holdsCount(requests.slice(0, mark), 0, setup.words, "before the entry");
      const placement = placedLast(
        requests.slice(mark),
        answer.message,
        roleOf(setup),
        setup.words,
        "after the entry"
      );
      setup.note(
        `${mark} requests before the entry hold none; ${noteOf(requests.slice(mark), placement, roleOf(setup))}`
      );
    },
  };
}

/** The idle cases of one approval card, each with something of its own in the pane. */
function cardCase(
  name: string,
  extra: {
    readonly reverse?: boolean;
    readonly answers?: boolean;
    /** Two cards in the same window: the role message that the gate opened right after the
     *  notice, both sent before the first request after them. */
    readonly twoCards?: boolean;
  }
): CaptureCase {
  const notice = entryFile("notice-design-approved-from-artifact-approved");
  return {
    name,
    run: async (setup) => {
      const answer = answerFor(setup, notice.approval);
      let mark = 0;
      const pane = await openPane(
        setup,
        { name: name.replaceAll(" ", "-"), tree: OWN_ROOT, reverse: extra.reverse },
        async (life) => {
          await life.prompt(WARM_UP);
          if (extra.answers === true) life.state.answers = [personAnswer(OWN_ROOT)];
          if (extra.twoCards === true) {
            // The notice, then the role message at once, which the session takes before the
            // first request after them is built or as an aside of that run.
            life.state.route = answer;
            const text = readFileSync(path.join(ENTRIES, `${notice.name}.txt`), "utf8");
            const cut = text.indexOf("\n");
            mark = await life.card(text.slice(0, cut), text.slice(cut + 1), false);
            await sendEntry(life, entryFile("role-message-gate-opened"), answer, false);
            await life.quiet(QUIET_MS);
          } else {
            mark = await sendEntry(life, notice, answer, false);
          }
          await laterRequest(life, mark);
        }
      );
      const requests = pane.sent();
      holdsCount(requests.slice(0, mark), 0, setup.words, "before the entry");
      const placement = placedLast(
        requests.slice(mark),
        answer.message,
        roleOf(setup),
        setup.words,
        "after the entry"
      );
      const first = mains(requests.slice(mark))[0];
      if (extra.answers === true) {
        const answers =
          first?.messages.findIndex((message) => textOf(message).startsWith(ANSWERS_MESSAGE)) ?? -1;
        if (answers < 0) {
          fail(`request #${first?.index} holds no people's-answers message beside the placement`);
        }
        setup.note(`request #${first?.index} holds the people's-answers message at ${answers}`);
      }
      setup.note(noteOf(requests.slice(mark), placement, roleOf(setup)));
    },
  };
}

/**
 * A compaction after the placement, by the route's own method order: one that summarizes its
 * answer, and one that keeps it. The first keeps the newest turn alone (`keepRecentTokens: 1`),
 * and a prompt after the entry's run makes that turn another one. The second keeps the recent
 * 8000 tokens, which hold the entry's run, and has a long tool output before it to summarize.
 */
function compactionCase(summarizes: boolean): CaptureCase {
  const notice = entryFile("notice-design-approved-from-artifact-approved");
  return {
    name: `a compaction that ${summarizes ? "summarizes" : "keeps"} the message's answer`,
    run: async (setup) => {
      const answer = answerFor(setup, notice.approval);
      let mark = 0;
      let compacted = 0;
      const pane = await openPane(
        setup,
        {
          name: `compaction-${summarizes ? "summarizes" : "keeps"}`,
          tree: OWN_ROOT,
          config: ["compaction:", `  keepRecentTokens: ${summarizes ? 1 : 8000}`],
          thinkingOff: true,
        },
        async (life) => {
          await life.prompt(summarizes ? WARM_UP : "Run the bash command `seq 1 30000`.");
          mark = await sendEntry(life, notice, answer, false);
          await laterRequest(life, mark);
          if (summarizes) await life.prompt(FOLLOW_UP);
          const response = await life.command("compact");
          if (response.success !== true) fail(`the compaction failed: ${JSON.stringify(response)}`);
          compacted = await life.prompt(FOLLOW_UP);
        }
      );
      const requests = pane.sent();
      const placement = placedLast(
        requests.slice(mark, compacted),
        answer.message,
        roleOf(setup),
        setup.words,
        "before the compaction"
      );
      if (summarizes) {
        const again = placedLast(
          requests.slice(compacted),
          answer.message,
          roleOf(setup),
          setup.words,
          "after the compaction"
        );
        setup.note(
          `after the compaction, ${noteOf(requests.slice(compacted), again, roleOf(setup))}`
        );
      } else {
        const after = mains(requests.slice(compacted));
        if (after.length === 0) fail("no request of the conversation came after the compaction");
        holdsCount(after, 1, setup.words, "after the compaction");
        for (const request of after) kept(request, placement, "after the compaction");
        setup.note(
          `after the compaction, ${after.length} requests hold the copy right before its answer`
        );
      }
      // The compaction's own requests, which reach no `before_provider_request` handler, so the
      // recorder never sees them: the stand-in serves each, as the model or as the recording proxy
      // to the gateway. A remote compaction's request, the one with a `compaction` member, carries
      // the conversation, so it holds the copy at its place and no new placement; a summary request
      // carries the session's messages as text, where no approval message ever is. (Every
      // request to claude-opus-5-5 carries `context_management`, so that member marks none.)
      const own = pane
        .modelRequests()
        .filter((request) => isSummarization(request) || "compaction" in request.body);
      // A compaction whose method makes no model request (snapcompact, on a model that reads
      // images) has no own request to read; the summarizing case's always has one.
      if (own.length === 0 && summarizes) fail("the compaction made no model request of its own");
      let remote = 0;
      own.forEach((request, index) => {
        const conversation: Sent = {
          index,
          pane: "the compaction",
          kind: "main",
          messages: (Array.isArray(request.body.messages)
            ? request.body.messages
            : []) as WireMessage[],
          perCall: [],
        };
        if (isSummarization(request)) {
          holdsCount([conversation], 0, setup.words, "the compaction's summary request");
          return;
        }
        remote += 1;
        holdsCount([conversation], 1, setup.words, "the compaction's own request");
        kept(conversation, placement, "the compaction's own request");
      });
      // A route whose compaction makes no remote request (its method order compacts with a
      // summary, `soft`) exercises no remote compaction's own request, and the run says so.
      setup.note(
        remote > 0
          ? `the compaction's own requests: ${remote} remote, each holding the copy at its place, and ${own.length - remote} summary requests, holding no approval message`
          : `the compaction made no remote request, so a remote compaction's own request was not exercised on this route; its ${own.length} summary requests hold no approval message`
      );
    },
  };
}

/** A request of a side turn, of the handoff prompt or of a task subagent, after the placement. */
function sideCase(kind: "side" | "handoff" | "sub"): CaptureCase {
  const notice = entryFile("notice-design-approved-from-artifact-approved");
  const names = { side: "a side turn", handoff: "the handoff prompt", sub: "a task subagent" };
  return {
    name: names[kind],
    run: async (setup) => {
      const answer = answerFor(setup, notice.approval);
      let mark = 0;
      let step = 0;
      const pane = await openPane(
        setup,
        {
          name: `side-${kind}`,
          tree: OWN_ROOT,
          config: kind === "handoff" ? ["compaction:", "  keepRecentTokens: 1"] : [],
        },
        async (life) => {
          await life.prompt(WARM_UP);
          mark = await sendEntry(life, notice, answer, false);
          await laterRequest(life, mark);
          step = life.sent().length;
          if (kind === "side") {
            const recorded = life.frame(
              (frame) => frame.type === "btw_record",
              "the side question's record"
            );
            const response = await life.command("btw", { question: "What is left to do?" });
            if (response.success !== true)
              fail(`the side question failed: ${JSON.stringify(response)}`);
            await recorded;
            await life.until(
              () =>
                life
                  .sent()
                  .slice(step)
                  .some((request) => request.kind === "side"),
              "the side turn's request"
            );
          } else if (kind === "handoff") {
            await life.prompt(FOLLOW_UP);
            await life.command("handoff");
          } else {
            await life.prompt(DELEGATE);
            // The subagent runs in the background; its result starts a run of the parent's own.
            await life.until(() => {
              const sent = life.sent();
              const lastSub = sent.findLastIndex((request) => request.kind === "sub");
              return lastSub >= 0 && mains(sent.slice(lastSub)).length > 0;
            }, "the subagent's requests and the parent's request after them");
          }
        }
      );
      const requests = pane.sent();
      const placement = placedLast(
        requests.slice(mark).filter((request) => request.kind === "main"),
        answer.message,
        roleOf(setup),
        setup.words,
        "after the entry"
      );
      const own = requests.slice(step).filter((request) => request.kind === kind);
      if (own.length === 0) fail(`no request of ${names[kind]} was sent`);
      if (kind === "sub") {
        holdsCount(own, 0, setup.words, "the subagent");
        setup.note(`${own.length} requests of the subagent hold no approval message`);
        return;
      }
      holdsCount(own, 1, setup.words, names[kind]);
      for (const request of own) kept(request, placement, names[kind]);
      setup.note(
        `${own.length} requests of ${names[kind]} make no new placement and hold the copy right before its answer`
      );
    },
  };
}

/** A fresh session while the approval stands: the route answers it from the launch, and the
 *  start task is the session's first turn. */
const freshSession: CaptureCase = {
  name: "a fresh session",
  run: async (setup) => {
    const start = entryFile("task-start-of-a-root-whose-gate-is-open");
    const answer = answerFor(setup, start.approval);
    const pane = await openPane(
      setup,
      { name: "fresh-session", tree: OWN_ROOT, route: answer },
      async (life) => {
        await sendEntry(life, start, answer, false);
        await laterRequest(life, 0);
      }
    );
    const placement = placedLast(
      pane.sent(),
      answer.message,
      roleOf(setup),
      setup.words,
      "the fresh session"
    );
    setup.note(noteOf(pane.sent(), placement, roleOf(setup)));
  },
};

/**
 * A relaunch into the same session (`--resume`), also after a changes request and after a later
 * approval: the copy stays at its place with the same bytes; the changes request makes no new
 * placement; the later approval's message is placed last, and the earlier copy stays.
 */
const relaunches: CaptureCase = {
  name: "relaunches, a changes request and a later approval",
  run: async (setup) => {
    const approved = entryFile("notice-design-approved-from-artifact-approved");
    const later = entryFile("notice-design-approved-from-gate-above-dispatch");
    const v3 = answerFor(setup, approved.approval);
    const v4 = answerFor(setup, later.approval);
    const marks = {
      approved: 0,
      relaunch: 0,
      changes: 0,
      relaunchAfterChanges: 0,
      later: 0,
      relaunchAfterLater: 0,
    };
    const pane = await openPane(setup, { name: "relaunches", tree: OWN_ROOT }, async (life) => {
      await life.prompt(WARM_UP);
      marks.approved = await sendEntry(life, approved, v3, false);
      await laterRequest(life, marks.approved);
    });
    await pane.relaunch(async (life) => {
      marks.relaunch = await life.prompt(FOLLOW_UP);
      // A person's changes request ends the approval: the route answers none, and the daemon's
      // notice of it reaches the session.
      life.state.route = null;
      marks.changes = await life.card(
        `design-changes-requested on ${OWN_ROOT}`,
        JSON.stringify({
          kind: "design-changes-requested",
          version: 3,
          actor: "user:person@example.com",
          ref: `dispatch://${OWN_ROOT}/spec`,
        })
      );
      await laterRequest(life, marks.changes);
    });
    await pane.relaunch(async (life) => {
      marks.relaunchAfterChanges = await life.prompt(FOLLOW_UP);
      marks.later = await sendEntry(life, later, v4, false);
      await laterRequest(life, marks.later);
    });
    await pane.relaunch(async (life) => {
      marks.relaunchAfterLater = await life.prompt(FOLLOW_UP);
    });
    const requests = pane.sent();
    const segment = (from: number, to?: number): Sent[] => mains(requests.slice(from, to));
    const first = placedLast(
      requests.slice(marks.approved, marks.relaunch),
      v3.message,
      roleOf(setup),
      setup.words,
      "the approval"
    );
    for (const [label, from, to] of [
      ["after the relaunch", marks.relaunch, marks.changes],
      ["after the changes request", marks.changes, marks.relaunchAfterChanges],
      ["after the relaunch after the changes request", marks.relaunchAfterChanges, marks.later],
    ] as const) {
      const after = segment(from, to);
      if (after.length === 0) fail(`${label}: no request of the conversation came`);
      holdsCount(after, 1, setup.words, label);
      for (const request of after) kept(request, first, label, true);
      setup.note(`${label}, ${after.length} requests hold the one copy at ${first.place}`);
    }
    const second = placedLast(
      requests.slice(marks.later, marks.relaunchAfterLater),
      v4.message,
      roleOf(setup),
      setup.words,
      "the later approval",
      [first]
    );
    setup.note(`the later approval: ${noteOf(requests.slice(marks.later), second, roleOf(setup))}`);
    const last = segment(marks.relaunchAfterLater);
    if (last.length === 0) fail("after the last relaunch: no request of the conversation came");
    holdsCount(last, 2, setup.words, "after the relaunch after the later approval");
    for (const request of last) {
      kept(request, first, "after the relaunch after the later approval", true);
      kept(request, second, "after the relaunch after the later approval", true);
    }
    setup.note(`after the last relaunch, ${last.length} requests hold both copies at their places`);
  },
};

/** The calendar day before `day` (YYYY-MM-DD). */
function dayBefore(day: string): string {
  const date = new Date(`${day}T12:00:00Z`);
  date.setUTCDate(date.getUTCDate() - 1);
  return date.toISOString().slice(0, 10);
}

/**
 * Rewrites each journaled date-cwd-reminder entry of the transcripts under `sessions` to name the
 * day before the one it names, as a session resumed the next day holds it, and tells `rewritten`
 * each date it moved. A transcript that holds no such entry is left as it is. Answers how many
 * entries it rewrote.
 */
export async function rewriteReminderDates(
  sessions: string,
  rewritten?: (from: string, to: string) => void
): Promise<number> {
  let count = 0;
  for (const file of (await readdir(sessions, { recursive: true }))
    .filter((name) => name.endsWith(".jsonl"))
    .map((name) => path.join(sessions, name))) {
    let changed = 0;
    const lines = (await readFile(file, "utf8")).split("\n").map((line) => {
      if (line === "") return line;
      const entry = JSON.parse(line) as Record<string, unknown>;
      const data = entry.data as Record<string, unknown> | undefined;
      if (entry.customType !== "date-cwd-reminder" || typeof data?.reminder !== "string") {
        return line;
      }
      const day = /Today: (\d{4}-\d{2}-\d{2});/.exec(data.reminder)?.[1];
      if (day === undefined) return line;
      const before = dayBefore(day);
      changed += 1;
      rewritten?.(day, before);
      return JSON.stringify({
        ...entry,
        data: { ...data, reminder: data.reminder.replace(day, before) },
      });
    });
    if (changed > 0) await writeFile(file, lines.join("\n"));
    count += changed;
  }
  return count;
}

/**
 * The date-reminder case: a card to a session resumed with a journaled date-cwd-reminder entry
 * that names another date, with a new people's-answers message in the same request.
 */
const dateReminder: CaptureCase = {
  name: "the date reminder",
  run: async (setup) => {
    const notice = entryFile("notice-design-approved-from-artifact-approved");
    const answer = answerFor(setup, notice.approval);
    let mark = 0;
    const pane = await openPane(setup, { name: "date-reminder", tree: OWN_ROOT }, async (life) => {
      await life.prompt(WARM_UP);
    });
    if ((await rewriteReminderDates(pane.sessions)) === 0) {
      fail("the transcript holds no date-cwd-reminder entry to rewrite");
    }
    await pane.relaunch(async (life) => {
      life.state.answers = [personAnswer(OWN_ROOT)];
      mark = await sendEntry(life, notice, answer, false);
      await laterRequest(life, mark);
    });
    const requests = pane.sent();
    placedLast(requests.slice(mark), answer.message, roleOf(setup), setup.words, "after the card");
    const [first, next] = mains(requests.slice(mark));
    if (first === undefined || next === undefined) fail("the card made fewer than two requests");
    const reminder = first.messages.findIndex((message) => DATE_REMINDER.test(textOf(message)));
    if (reminder < 0) fail(`request #${first.index} holds no date reminder`);
    if (first.messages[reminder]?.role !== "user") {
      fail(
        `request #${first.index} holds the date reminder as ${describe(first.messages[reminder])}`
      );
    }
    const anchor = first.perCall.lastIndexOf(false);
    if (reminder !== anchor + 1) {
      fail(
        `request #${first.index} holds the date reminder at ${reminder}, not right after ${anchor}, the last message that is not per-call`
      );
    }
    if (!first.messages.some((message) => textOf(message).startsWith(ANSWERS_MESSAGE))) {
      fail(`request #${first.index} holds no people's-answers message`);
    }
    for (let at = 0; at < first.messages.length; at += 1) {
      if (normalized(first.messages[at]) !== normalized(next.messages[at])) {
        fail(
          `request #${next.index} differs from request #${first.index} at ${at}: ${describe(next.messages[at])} where it had ${describe(first.messages[at])}`
        );
      }
    }
    if (next.messages[first.messages.length]?.role !== "assistant") {
      fail(`request #${next.index} holds no answer right after request #${first.index}'s messages`);
    }
    setup.note(
      `request #${first.index}: the reminder a user message at ${reminder}, right after the last message that is not per-call; the people's-answers message; the approval message last as ${roleOf(setup)}; its ${first.messages.length} messages equal request #${next.index}'s before the answer`
    );
  },
};

/** The negative control's second half: R1's words sent as a card that wakes an idle session,
 *  with no approval standing, go as a user message. */
const wordsAsCard: CaptureCase = {
  name: "R1's words sent as a card that wakes an idle session",
  placesNone: true,
  run: async (setup) => {
    const words = answerFor(setup, own(3)).message;
    let mark = 0;
    const pane = await openPane(setup, { name: "words-as-card", tree: OWN_ROOT }, async (life) => {
      await life.prompt(WARM_UP);
      mark = await life.card(words);
    });
    const requests = pane.sent();
    holdsCount(requests, 0, setup.words, "the card");
    const first = mains(requests.slice(mark))[0] ?? fail("no request came after the card");
    // The words' own sentence, which the card carries as the listener's payload.
    const sentence = "Your top responsibility is now to deliver it and no other";
    const card = first.messages.find((message) => textOf(message).includes(sentence));
    if (card === undefined) fail(`request #${first.index} holds no message with R1's words`);
    if (card.role !== "user") fail(`request #${first.index} sends R1's words as ${describe(card)}`);
    setup.note(`request #${first.index} sends the card with R1's words as a user message`);
  },
};

/** Every case of the check, in the order the run takes them. `entries` picks the entry files the
 *  entry cases send: every file, or the first of each of the six paths. */
export function captureCases(entries: "every" | "each-path"): CaptureCase[] {
  // A run of each path sends the first file of each path.
  const files = ENTRY_FILES.filter(
    (file, at) =>
      entries === "every" || ENTRY_FILES.findIndex((first) => first.path === file.path) === at
  );
  return [
    ...files.flatMap((file) => [entryCase(file, false), entryCase(file, true)]),
    cardCase("two cards in the same window", { twoCards: true }),
    cardCase("a people's-answers message in the same request", { answers: true }),
    cardCase("the two plugins in the reverse order", { reverse: true, answers: true }),
    compactionCase(true),
    compactionCase(false),
    freshSession,
    relaunches,
    dateReminder,
    sideCase("sub"),
    sideCase("side"),
    sideCase("handoff"),
    wordsAsCard,
  ];
}

/** Runs `one` on `setup`; on claude-sonnet-5, a case that places the message then reads
 *  pi-legion's log line: each process of the case that carried the message logs one line, which
 *  names the model. */
export async function runCase(one: CaptureCase, setup: CaptureSetup): Promise<void> {
  await one.run(setup);
  if (setup.model !== "sonnet" || one.placesNone === true) return;
  const lines = setup.panes
    .flatMap((pane) => pane.logs())
    .filter((line) => line.includes("anthropic/claude-sonnet-5") && line.includes(USER_ROLE_LINE));
  const processes = new Map<unknown, number>();
  for (const line of lines) {
    const { pid } = JSON.parse(line) as { readonly pid?: unknown };
    processes.set(pid, (processes.get(pid) ?? 0) + 1);
  }
  if (processes.size === 0) fail("pi-legion logged no line naming claude-sonnet-5");
  for (const [pid, count] of processes) {
    if (count !== 1) fail(`process ${String(pid)} logged ${count} lines naming claude-sonnet-5`);
  }
  setup.note(
    `pi-legion logged one line naming the model in each of ${processes.size} processes: ${lines[0]?.slice(0, 160)}`
  );
}

/** The value of `--name` in `args`, or undefined. */
function flag(args: readonly string[], name: string): string | undefined {
  const at = args.indexOf(`--${name}`);
  return at < 0 ? undefined : args[at + 1];
}

/**
 * The script's run: the route's check, every case on the pod's own model route, then every case
 * again on claude-sonnet-5. Answers the exit status: 0 when every case passed, or under
 * `--negative-control` when the run sent requests and none of them holds an approval message,
 * and 1 otherwise.
 */
async function main(args: readonly string[]): Promise<number> {
  const required = (name: string): string =>
    flag(args, name) ?? fail(`--${name} is required (see the head of this file)`);
  const entries = flag(args, "entries") ?? "each-path";
  if (entries !== "every" && entries !== "each-path") {
    fail(`--entries ${entries}: want every or each-path`);
  }
  const negative = args.includes("--negative-control");
  const models = required("models");
  const overlay = required("overlay");
  const gateway = podGateway(models);
  const model = overlayDefault(overlay, gateway, models);
  console.log(
    `   the route: ${models}'s anthropic provider; the cases run the overlay's default ${model}, then claude-sonnet-5`
  );
  const sent: Sent[] = [];
  const base = {
    binary: required("omp"),
    nats: required("nats"),
    route: { kind: "pod", gateway, overlay },
    plugins: { envoy: required("envoy"), legion: required("legion") },
    reverse: false,
    words: await approvalWords(),
    note: (line: string) => console.log(`   ${line}`),
    evidence: flag(args, "evidence"),
    sent,
  } as const;
  const failed: string[] = [];
  const started = Date.now();
  for (const model of ["opus", "sonnet"] as const) {
    for (const one of captureCases(entries)) {
      const name = `${model === "sonnet" ? "claude-sonnet-5: " : ""}${one.name}`;
      console.log(`== ${name}`);
      const began = Date.now();
      const cleanup: Cleanup = [];
      try {
        await runCase(one, { ...base, cleanup, model, panes: [] });
        console.log(`ok ${name} (${Math.round((Date.now() - began) / 1000)}s)`);
      } catch (error) {
        failed.push(`${name}: ${error instanceof Error ? error.message : String(error)}`);
        console.log(`FAIL ${failed.at(-1)}`);
      } finally {
        for (const step of cleanup.splice(0).reverse()) {
          await step().catch((error: unknown) => console.log(`   cleanup: ${String(error)}`));
        }
      }
    }
  }
  console.log(
    `${failed.length} of the cases failed; the run took ${Math.round((Date.now() - started) / 1000)}s and its sessions sent ${sent.length} requests`
  );
  if (negative) {
    // The negative control's verdict is its own check alone: its placing cases fail by design, at
    // their first placement.
    console.log("== negative control: no request holds the approval message");
    const holding = sent.filter((request) => approvalsIn(request.messages, base.words).length > 0);
    const why =
      sent.length === 0
        ? "the run's sessions sent no request"
        : holding.length > 0
          ? `${holding.length} of ${sent.length} requests hold an approval message, the first #${holding[0]?.index} of ${holding[0]?.pane}`
          : undefined;
    if (why !== undefined) {
      console.log(`FAIL negative control: ${why}`);
      console.log(`approval message capture: negative control FAIL: ${why}`);
      return 1;
    }
    console.log(`   none of the run's ${sent.length} requests holds an approval message`);
    console.log("ok negative control");
    console.log("approval message capture: negative control PASS");
    return 0;
  }
  if (failed.length === 0) {
    console.log("approval message capture: PASS");
    return 0;
  }
  console.log(`approval message capture: FAIL: the first case that failed is ${failed[0]}`);
  return 1;
}

if (import.meta.main) {
  // A failure before the cases, a route that offers no model the cases run among them, still ends
  // the run with its verdict line.
  const status = await main(process.argv.slice(2)).catch((error: unknown) => {
    console.log(
      `approval message capture: FAIL: ${error instanceof Error ? error.message : String(error)}`
    );
    return 1;
  });
  process.exit(status);
}
