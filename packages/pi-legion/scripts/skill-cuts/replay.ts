/**
 * The replay of one arm of a case: its captured request body sent to the gateway, and the reply
 * that is the judged step. The body is of either API the gateway serves:
 * a Messages body, or an OpenAI Responses body (gateway.ts's `isResponsesBody`); every round sends
 * the body in its own API, on the case's own model.
 *
 * A reply whose every tool call is a read of a file of the case's skill (a `read` of
 * `skill://<skill>` or `skill://<skill>/<path>`, as cases.ts's `skillReadFile` reads one) is not
 * the step: the agent is still loading the skill. Each of those reads is answered the way the
 * arm's files would answer it (arms.ts's `skillFile`: the file under a `[Skill file: <path>]`
 * header whose path is the skill's installed one, the same in every arm, or Oh My Pi's not-found
 * result for a file the arm lacks), in the form Oh My Pi sends a tool result in the body's API,
 * and the body goes again with the reply and the answers added; at most `maxSkillRounds` times
 * (MAX_SKILL_ROUNDS). A Messages reply goes back as the assistant's message and its answers as one
 * user message of `tool_result` blocks (F2); a Responses reply's output items go back as they came
 * (its reasoning items' encrypted content included), and after all of them, in the order of the
 * reply's calls, one `function_call_output` item per call holding the answer's text (F8: the
 * Responses API takes no error flag, and Oh My Pi sends none). The first reply that holds no tool
 * call, or any other tool call (a read of another skill, a read of a path on disk, a read of a
 * directory of the skill, any other tool), is the judged step, and so is the reply after the last
 * round, whatever it holds.
 * Every round goes with the same headers (`ReplayOptions.headers`), the ones Oh My Pi sent beside
 * the body (none beside a Responses body).
 *
 * Every run of an arm sends the same body but for its session id, so the gateway's prompt cache
 * can serve all but the first run's prefix. A Messages body is cached only up to its last cache
 * breakpoint, and the bodies Oh My Pi builds hold theirs on the tools and the system prompt only:
 * the pilot's runs read about 20,000 tokens of a 340,000-token body from the cache and paid the
 * rest as input. So each run sends a Messages body with a breakpoint on its last message
 * (`withCacheBreakpoint`). The Responses route caches the prefix a body shares with an earlier
 * one by itself (F9: the second send of a body read all but 3 of its 7,985 input tokens from the
 * cache), so a Responses body goes as it is, with no `cache_control`; its reads are counted as
 * `cacheRead`.
 *
 * Each run goes with a session id of its own (gateway.ts's `freshIdentity`: `metadata.user_id`,
 * or a Responses body's `prompt_cache_key`, drawn again in its own form), the same in every round
 * of the run, and no other byte of the body changes: a captured body holds the id of the one Oh
 * My Pi run that made it, and a gateway that routes or caches by it made it a factor of a whole
 * arm (one body with two fixed ids and with a fresh one per send, interleaved on one deployment:
 * 8, 26 and 18 of 30 runs read the skill's reference). `replayInterleaved` sends the runs of all
 * the arms of a round in one seeded order (`sendOrder`), never one arm's batch after another's.
 */
import { createHash } from "node:crypto";
import { statSync } from "node:fs";
import path from "node:path";
import { type Arm, type ArmName, skillFile } from "./arms";
import { skillReadFile } from "./cases";
import {
  type ContentBlock,
  freshIdentity,
  type Gateway,
  isResponsesBody,
  type MessagesBody,
  type Reply,
  type RequestBody,
  type SendOptions,
  type Spent,
  type ToolCall,
  toolCalls,
} from "./gateway";

/** The most times a replay answers a reply's skill reads and sends again. */
export const MAX_SKILL_ROUNDS = 3;

/** The most cache breakpoints (`cache_control`) one Messages API request may hold. */
export const MAX_BREAKPOINTS = 4;

/** True for a JSON object, as a body's parts are read. */
export function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** Each object in `value` that holds a cache breakpoint, in the order the request reads them. */
function breakpointsIn(
  value: unknown,
  found: Record<string, unknown>[] = []
): Record<string, unknown>[] {
  if (Array.isArray(value)) {
    for (const item of value) breakpointsIn(item, found);
  } else if (isObject(value)) {
    if (value.cache_control != null) found.push(value);
    for (const field of Object.values(value)) breakpointsIn(field, found);
  }
  return found;
}

/**
 * `body` with a cache breakpoint on the last block of its last message, the end of the prefix
 * that every run of an arm shares, so that the runs after the first read it all from the cache.
 * The block is the last one that can hold a breakpoint: not a thinking block and not an empty
 * text (a last message whose content is a string becomes one text block holding it). The
 * breakpoint keeps its prefix an hour (`{type: "ephemeral", ttl: "1h"}`), or 5 minutes
 * (`{type: "ephemeral"}`) when a breakpoint before it does, since a request may not hold an
 * hour-long one after a shorter one.
 *
 * The body keeps at most MAX_BREAKPOINTS: the earliest breakpoints of its messages are taken out
 * first, and those of its tools and system prompt are kept. A body whose last block holds a
 * breakpoint already, with no more than MAX_BREAKPOINTS in all, a body whose tools and system
 * prompt hold MAX_BREAKPOINTS already, and a body whose last message has no block to hold one
 * come back as they are. Nothing else changes: the body's fields and their order are its own, and
 * `body` itself is never changed.
 */
export function withCacheBreakpoint(body: MessagesBody): MessagesBody {
  const fixed = [...breakpointsIn(body.tools), ...breakpointsIn(body.system)];
  const messages = structuredClone([...body.messages]);
  const last = messages.at(-1);
  if (!isObject(last)) return body;
  if (typeof last.content === "string" && last.content !== "") {
    last.content = [{ type: "text", text: last.content }];
  }
  if (!Array.isArray(last.content)) return body;
  const target = last.content.findLast(
    (block) =>
      isObject(block) &&
      block.type !== "thinking" &&
      block.type !== "redacted_thinking" &&
      !(block.type === "text" && block.text === "")
  );
  if (!isObject(target)) return body;
  const adding = target.cache_control == null;
  const inMessages = breakpointsIn(messages);
  const total = fixed.length + inMessages.length + (adding ? 1 : 0);
  if (total <= MAX_BREAKPOINTS && !adding) return body;
  if (fixed.length + 1 > MAX_BREAKPOINTS) return body;
  const dropped = inMessages
    .filter((holder) => holder !== target)
    .slice(0, Math.max(0, total - MAX_BREAKPOINTS));
  for (const holder of dropped) delete holder.cache_control;
  if (adding) {
    const kept = [...fixed, ...inMessages.filter((holder) => !dropped.includes(holder))];
    const long = kept.every(
      (holder) => isObject(holder.cache_control) && holder.cache_control.ttl === "1h"
    );
    target.cache_control = long ? { type: "ephemeral", ttl: "1h" } : { type: "ephemeral" };
  }
  return { ...body, messages };
}

/** One replay: the judged step, how many rounds of skill reads were answered before it, what each
 *  reply used, in the order sent, and the session id every round of the run went with
 *  (gateway.ts's `freshIdentity`; null for a body that names none). */
export interface Replayed {
  readonly step: Reply;
  readonly rounds: number;
  readonly usage: Spent[];
  readonly userId: string | null;
}

/** How a replay runs: the most rounds of skill reads it answers, and the headers that go with the
 *  body on every round (gateway.ts's `SendOptions`: the `anthropic-*` headers Oh My Pi sent beside
 *  a captured Messages body, whose betas its fields need; none beside a Responses body). */
export interface ReplayOptions extends SendOptions {
  readonly maxSkillRounds?: number;
}

/** A tool call that reads a file of the arm's skill: the call's id and the file, as a path inside
 *  the skill's directory. */
interface SkillRead {
  readonly id: string;
  readonly file: string;
}

/** The skill read `call` is, or undefined when it is any other tool call. A read of a directory
 *  of the skill is none: Oh My Pi answers it with a listing, which the arm does not rebuild. */
function skillRead(call: ToolCall, arm: Arm): SkillRead | undefined {
  const { id, name, input } = call;
  if (id === undefined || name !== "read") return undefined;
  if (typeof input !== "object" || input === null || !("path" in input)) return undefined;
  if (typeof input.path !== "string") return undefined;
  const file = skillReadFile(arm.skill, input.path);
  if (file === undefined) return undefined;
  if (arm.skillDir !== undefined) {
    const stats = statSync(path.resolve(arm.skillDir, file), { throwIfNoEntry: false });
    if (stats?.isDirectory()) return undefined;
  }
  return { id, file };
}

/** `body` with `content`, a reply whose every tool call is one of `reads`, and the arm's answer to
 *  each read added, in the form of the body's API (see the file's header). */
function answered(
  body: RequestBody,
  content: readonly ContentBlock[],
  reads: readonly SkillRead[],
  arm: Arm
): RequestBody {
  const answers = reads.map((read) => ({ id: read.id, ...skillFile(arm, read.file) }));
  if (isResponsesBody(body)) {
    const outputs = answers.map(({ id, text }) => ({
      type: "function_call_output",
      call_id: id,
      output: text,
    }));
    return { ...body, input: [...body.input, ...content, ...outputs] };
  }
  const results = answers.map(({ id, text, isError }) => ({
    type: "tool_result",
    tool_use_id: id,
    content: [{ type: "text", text }],
    is_error: isError,
  }));
  return {
    ...body,
    messages: [
      ...body.messages,
      { role: "assistant", content },
      { role: "user", content: results },
    ],
  };
}

/** Sends `sent` for `arm`, the body with its cache breakpoint as `replayOnce` makes it (see the
 *  file's header) and the run's own session id `userId` (`freshIdentity`), which every round of
 *  the run keeps; answers its skill reads, and returns the judged step. `sent` is never changed,
 *  and every round's body shares its parts. `firstSettled` is told once the run's first send has
 *  its reply or its failure. */
async function replaySent(
  sent: RequestBody,
  userId: string | null,
  arm: Arm,
  gateway: Gateway,
  { maxSkillRounds = MAX_SKILL_ROUNDS, headers }: ReplayOptions,
  firstSettled?: () => void
): Promise<Replayed> {
  let body = sent;
  const usage: Spent[] = [];
  for (let rounds = 0; ; rounds += 1) {
    let reply: Reply;
    try {
      reply = await gateway.send(body, { headers });
    } finally {
      if (rounds === 0) firstSettled?.();
    }
    usage.push(reply.spent);
    const calls = toolCalls(reply.message.content);
    const reads = calls.map((call) => skillRead(call, arm));
    if (
      rounds >= maxSkillRounds ||
      calls.length === 0 ||
      !reads.every((read): read is SkillRead => read !== undefined)
    ) {
      return { step: reply, rounds, usage, userId };
    }
    body = answered(body, reply.message.content, reads, arm);
  }
}

/** Sends `body` for `arm` with a session id of its own, answers its skill reads, and returns the
 *  judged step. Rejects when a send does (the gateway's `BudgetSpent` among them). */
export function replayOnce(
  body: RequestBody,
  arm: Arm,
  gateway: Gateway,
  options: ReplayOptions = {}
): Promise<Replayed> {
  const own = freshIdentity(isResponsesBody(body) ? body : withCacheBreakpoint(body));
  return replaySent(own.body, own.id, arm, gateway, options);
}

/** One arm's runs in a round: its arm, its captured body, the headers that go with it (its
 *  capture's `anthropic-*` headers, none beside a Responses body), and how many runs to make. */
export interface ArmRuns {
  readonly arm: Arm;
  readonly body: RequestBody;
  readonly headers?: Readonly<Record<string, string>>;
  readonly runs: number;
}

/** One run's place in a round's send order: its arm, and its number among that arm's runs of
 *  the round, from 0. */
export interface Slot {
  readonly arm: ArmName;
  readonly run: number;
}

/** The send order of runs of several kinds (a round's arms, a load test's tasks), `runs` of each
 *  kind `key`: every `{key, run}`, the kinds in the order given and each kind's runs in order,
 *  then shuffled by the seed (a Fisher-Yates shuffle whose numbers are SHA-256 of
 *  `<seed>:send:<counter>`). The same seed and counts give the same order. */
export function sendOrder<K extends string>(
  runs: readonly { readonly key: K; readonly runs: number }[],
  seed: string
): { readonly key: K; readonly run: number }[] {
  type Place = { readonly key: K; readonly run: number };
  const order = runs.flatMap(({ key, runs: n }) =>
    Array.from({ length: n }, (_, run): Place => ({ key, run }))
  );
  let counter = 0;
  const random = () =>
    createHash("sha256").update(`${seed}:send:${counter++}`).digest().readUIntBE(0, 6) / 2 ** 48;
  for (let index = order.length - 1; index > 0; index -= 1) {
    const other = Math.floor(random() * (index + 1));
    [order[index], order[other]] = [order[other] as Place, order[index] as Place];
  }
  return order;
}

/** One run of an interleaved round: its slot, its place in the send order (from 0), and its
 *  outcome, a step or a failure. */
export interface InterleavedRun extends Slot {
  readonly place: number;
  /** The session id every request of the run went with, or null for a body that names none. */
  readonly userId: string | null;
  readonly outcome: PromiseSettledResult<Replayed>;
}

/**
 * The runs of every arm of a round, sent in one order, `sendOrder` under `seed`, never one arm's
 * batch after another's: a gateway's state (its deployment, its cache, its load) then falls on
 * every arm alike. Each run is `replayOnce`'s, with a session id of its own. The runs start in
 * that order, all at once but for one wait (the gateway bounds how many are in flight, and gives
 * its places in the order asked): the first run of each arm in the order goes alone among its
 * arm's, the next run in the order starting only once its first send has its reply, so that the
 * prefix its reply wrote to the cache serves its arm's later runs. Those waits change no place of
 * the order. Each arm's body to send is made once for all its runs, each run's a copy of its top
 * level with its own id. Each run's outcome is kept, whether it was a step or a failure, in the
 * order sent.
 */
export async function replayInterleaved(
  arms: readonly ArmRuns[],
  gateway: Gateway,
  seed: string,
  options: Pick<ReplayOptions, "maxSkillRounds"> = {}
): Promise<InterleavedRun[]> {
  const byName = new Map<ArmName, ArmRuns & { readonly sent: RequestBody }>();
  for (const each of arms) {
    if (!Number.isInteger(each.runs) || each.runs < 0) {
      throw new Error(`a replay runs a whole number of times, 0 or more, not ${each.runs}`);
    }
    if (byName.has(each.arm.name)) throw new Error(`the arm ${each.arm.name} is given twice`);
    const sent = isResponsesBody(each.body) ? each.body : withCacheBreakpoint(each.body);
    byName.set(each.arm.name, { ...each, sent });
  }
  const order: Slot[] = sendOrder(
    arms.map((each) => ({ key: each.arm.name, runs: each.runs })),
    seed
  ).map(({ key, run }) => ({ arm: key, run }));
  const started = new Set<ArmName>();
  const runs: {
    slot: Slot;
    place: number;
    userId: string | null;
    replayed: Promise<Replayed>;
  }[] = [];
  for (const [place, slot] of order.entries()) {
    const each = byName.get(slot.arm);
    if (each === undefined) throw new Error(`no arm ${slot.arm}`);
    const first = !started.has(slot.arm);
    started.add(slot.arm);
    const settled = Promise.withResolvers<void>();
    const own = freshIdentity(each.sent);
    const replayed = replaySent(
      own.body,
      own.id,
      each.arm,
      gateway,
      { ...options, ...(each.headers === undefined ? {} : { headers: each.headers }) },
      settled.resolve
    );
    // Its outcome is read once every run has started; until then a failure is held, not thrown.
    replayed.catch(() => undefined);
    runs.push({ slot, place, userId: own.id, replayed });
    if (first) await settled.promise;
  }
  const outcomes = await Promise.allSettled(runs.map((run) => run.replayed));
  return runs.map(({ slot, place, userId }, index) => ({
    ...slot,
    place,
    userId,
    outcome: outcomes[index] as PromiseSettledResult<Replayed>,
  }));
}
