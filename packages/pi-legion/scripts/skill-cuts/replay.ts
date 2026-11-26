/**
 * The replay of one arm of a case: its captured request body sent to the gateway, and the reply
 * that is the judged step. The body is of either API the gateway serves:
 * a Messages body, or an OpenAI Responses body (gateway.ts's `isResponsesBody`); every round sends
 * the body in its own API, on the case's own model.
 *
 * A reply whose every tool call reads a skill file is not the step: the agent is still loading
 * skills. A read of a skill file is a `read` of a file of the arm's skill (`skill://<skill>` or
 * `skill://<skill>/<path>`, as cases.ts's `skillReadFile` reads one, or a whole read by path of a
 * file of the arm's copy of the skill, as `pathReadFile` reads one: the cut's own copy the case
 * names, or, for a case that names none, the directory its `skill://` reads show the skill
 * installed in, arms.ts's `Arm.pathCopy`), or of a file of another
 * skill (arms.ts's `otherSkillRead`: a `skill://` read of any other skill, or a whole read by path
 * of a skill the profile lists); a `skill://` read, and a read by path of the arm's copy, with
 * one line range (arms.ts's `readRange`: `:N-M`, `:N..M`, `:N+K`, `:N`, `:N-`, or a list that
 * merges into one range, the selector split off as the pin splits it, so `x.md:notes` names the
 * file `x.md:notes`) is one too, answered with the lines of its range in Oh My Pi's range form (a
 * line before, three after; arms.ts's `skillFileRange`, which ends a range with an end that leaves
 * lines after it with `[<n> more lines in file. Use :<next> to continue]`, and `pathRangeAnswer`,
 * which ends one with `[Showing lines <a>-<b> of <n>. Use :<b+1> to continue]` where lines are
 * left out), in every run, session and written cases alike. Each read of the arm's skill is
 * answered the way the arm's files
 * would answer it (arms.ts's `skillFile`: the file under a `[Skill file: <path>]` header whose
 * path is the skill's installed one, the same in every arm, or Oh My Pi's not-found result for a
 * file the arm lacks, or, in `none`, which installs no copy of the skill, Oh My Pi's unknown-skill
 * result; `pathFile` for a read by path: Oh My Pi's result for that path, its hashline line
 * naming the file under the directory the case's reads of the copy show, or its not-found result
 * for the path as read). In `none`, a `skill://` read of the case's skill is answered from the
 * copy of another source the profile lists under the skill's name instead, where it lists one
 * (arms.ts's `caseNameCopy`), as a read of another skill is. In the arm of ours, whose skill is
 * ours under its own name, a read of the case's skill is answered so from such a copy too, where
 * the arm lists one once their copies are gone, and else with Oh My Pi's unknown-skill text
 * (arms.ts's `unknownSkill`), since that arm does not install theirs; a read by path of the
 * arm's copy is answered from our file at that path. Each read of another skill is answered
 * from that skill's installed copy (`ReplayOptions.copies`, context.ts's `installedCopies` of the
 * arm's capture's record), the same text in every arm, and a read of a skill the arm does not
 * install gets Oh My Pi's unknown-skill result. The answers go in the form Oh My Pi sends a tool
 * result in the body's API, and the body goes again with the reply and the answers added; at
 * most `maxSkillRounds` times (MAX_SKILL_ROUNDS). A Messages reply goes back as the assistant's
 * message and its answers as one user message of `tool_result` blocks (F2); a Responses reply's
 * output items go back as they came (its reasoning items' encrypted content included), and after
 * all of them, in the order of the reply's calls, one `function_call_output` item per call
 * holding the answer's text (F8: the Responses API takes no error flag, and Oh My Pi sends none).
 *
 * The first reply that holds no tool call, or any tool call that reads no skill file (a read of
 * another path on disk, a read of a directory of a skill, a read with another selector, a read by
 * path of a file Oh My Pi shows in a form the arms do not rebuild, any other tool), ends the run. The
 * judged step (`Replayed.step`) is each answered round's tool calls, the reads, without their
 * answers, then that reply: the judge sees which files the agent read before it acted, so a case
 * whose line governs a read of a file of the skill ("read references/x.md first") passes on that
 * read. A reply after the last answered round whose every tool call still reads a skill file gets
 * no verdict: the replay rejects with ReadsPastLastRound (READS_PAST_LAST_ROUND), and cli.ts makes
 * the run again as any run with no verdict; judged, the read would fail in every arm. Each
 * answered round's reads are recorded (`Replayed.reads`: the skill, the file, how it was read,
 * and where the answer came from).
 *
 * A written case's run (`ReplayOptions.checkout`, the case's `checkout` copied from its commit;
 * cli.ts gives one to a written case only) also answers a reply whose every tool call is
 * read-only (checkout.ts's `readOnlyCall`): each read of a skill file as above, each other call
 * from the copy of the context session's checkout (checkout.ts's `answerCall`: in the pinned Oh My
 * Pi's form, at the session's paths), and the agent runs on, at most MAX_CHECKOUT_ROUNDS such
 * rounds beside the rounds of skill reads: a whole `read` of a code file the pin summarizes with
 * the pin's summary (arms.ts's `summaryAnswer`); a read with a line selector with the lines it
 * names in the pin's form (arms.ts's `pathRangeAnswer`: one range, a list of ranges, the last N
 * lines, and `:raw` with none, one or several of them); a `grep` whose path keeps a file to line
 * ranges or is a glob, and a `glob` that reaches its limit, as checkout.ts's header says; a `bash`
 * call whose output the pin's minimizer may rewrite, answered by the pinned Oh My Pi's own bash
 * tool run headless in the sandbox, its footer `[raw output: artifact://<N>]` numbered one past
 * the highest artifact the case's history (`CheckoutCopy.history`) and this run's earlier answers
 * name (`RunArtifacts`: one per run, passed to `answerCall`, so the number is the same in every
 * arm and run of a case), and a later `read` of that artifact in the run; and a `read` of one of
 * the pin's internal URLs (`history://`, `local://`, `xd://<device>`, …), answered by the pin's
 * read tool as a fresh session answers it. A reply one of whose read-only calls the copy answers
 * in no form (a selector of a form not rebuilt, a path outside the checkout, a git command where
 * the copy holds no history, a rewritten output or an internal-URL read where the pin's binary is
 * absent or the session's history made the item, a form not rebuilt) gets no verdict, never judged
 * on that call: the replay rejects with UnansweredRead
 * (UNANSWERED_READ), naming the call and why, and cli.ts makes the run again as any run with no
 * verdict. Each such round's answers are recorded (`CheckoutRead`: the tool, its input, `from:
 * "checkout"`), and its calls go into the judged step before the reply that ends the run, as a
 * round of skill reads does; a reply after the last checkout round that is still all read-only
 * calls the copy answers gets no verdict (ReadsPastLastRound). A written case with no checkout
 * (one built before build-cases passed over a context session in no checkout it knows) gives, in
 * place of a copy, why it has none (`Unanswered`): no call of such a reply but its skill reads is
 * answered, so the reply gets no verdict as UnansweredRead, naming its first other call and that
 * why. A session case's run has no checkout, so no such round: its read-only calls end the run as
 * any other call does.
 *
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
 * Each run of a body that names a session id goes with one of its own (gateway.ts's
 * `freshIdentity`: `metadata.user_id`, or a Responses body's `prompt_cache_key`, drawn again in
 * its own form; a body that names none goes with none), the same in every round of the run, and
 * no other byte of the body changes: a captured body holds the id of the one Oh My Pi run that
 * made it, and a gateway that routes or caches by it made it a factor of a whole arm (one body
 * with two fixed ids and with a fresh one per send, interleaved on one deployment: 8, 26 and 18 of
 * 30 runs read the skill's reference). `replayInterleaved` sends the runs of all the arms of a
 * round in one seeded order (`sendOrder`), never one arm's batch after another's.
 *
 * A run of a Messages body also goes with a routing id the caller gives
 * (`ReplayOptions.routingId`, `ArmRuns.routingIds`), sent on every round of the run in
 * gateway.ts's ROUTING_HEADER, by which the gateway pins the call's deployment: with none, every
 * run of one body went to the one deployment its hash picked, and arms of different bodies to
 * different ones. cli.ts gives the run numbered n of every arm of a stage one id; load.ts derives
 * one from the task and the run number. A Responses body goes with none (`Replayed.routingId`
 * null). Each run records the deployment that answered each of its replies
 * (`Replayed.deployments`, gateway.ts's `deploymentOf`).
 */
import { createHash } from "node:crypto";
import { statSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import {
  type Arm,
  type ArmName,
  caseNameCopy,
  type InstalledCopy,
  newReadRows,
  otherSkillRead,
  ownSkillReadFile,
  pathFile,
  readRange,
  skillFile,
  skillFileRange,
  unknownSkill,
} from "./arms";
import { pathReadFile, skillReadFile } from "./cases";
import {
  answerCall,
  type CheckoutCopy,
  knivesAdded,
  type RunArtifacts,
  readOnlyCall,
  runArtifacts,
  type Unanswered,
} from "./checkout";
import {
  type ContentBlock,
  deploymentOf,
  freshIdentity,
  type Gateway,
  isResponsesBody,
  isToolCallBlock,
  type MessagesBody,
  type Reply,
  type RequestBody,
  ROUTING_HEADER,
  type SendOptions,
  type Spent,
  toolCalls,
} from "./gateway";

/** The most rounds of skill reads a replay answers before a reply that holds another call or none.
 *  Three: of 3,680 posted runs of the behaviour test, 137 had one round answered and none had two;
 *  a read of another skill (dispatch-first, a skill that sends the agent to others), then a read of
 *  the case's skill, then the act, takes two. */
export const MAX_SKILL_ROUNDS = 3;
/** The most rounds of read-only calls a written case's replay answers from its checkout copy,
 *  beside its rounds of skill reads: a look at a few files before the act the line governs. */
export const MAX_CHECKOUT_ROUNDS = 5;

/** How the reason of a run begins whose reply after the last answered round still only reads
 *  skill files (ReadsPastLastRound): such a run gets no verdict. */
export const READS_PAST_LAST_ROUND = "reads-past-last-round";
/** How the reason of a run begins whose reply, in a written case's run, makes a read-only call
 *  its checkout copy answers in no form (UnansweredRead): such a run gets no verdict. */
export const UNANSWERED_READ = "unanswered-read";

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

/** One read an answered round answered: the skill (the name the profile lists it under, or the
 *  name a `skill://` URL gives a skill the profile does not list), the file inside its directory,
 *  how it was read (`skill` for a `skill://` URL, `path` for a whole read by path), where the
 *  answer came from (`arm`, the arm's own directory; `copy`, another skill's installed copy, or
 *  in `none` and the arm of ours the copy of another source listed under the case's skill's name;
 *  `absent`, Oh My Pi's unknown-skill or not-found result where the arm installs no copy of the
 *  skill: every other read of the case's skill in `none` and the arm of ours), and whether the
 *  answer is an error. A run record keeps each as it is (cli.ts's runs, load.ts's replies). */
export const AnsweredRead = z.strictObject({
  skill: z.string(),
  file: z.string(),
  by: z.enum(["skill", "path"]),
  from: z.enum(["arm", "copy", "absent"]),
  isError: z.boolean(),
});
export type AnsweredRead = Readonly<z.infer<typeof AnsweredRead>>;

/** One read-only call a checkout round answered from the written case's checkout copy
 *  (checkout.ts's `answerCall`): its tool, its input as the reply gave it (a bash call's command
 *  among it), and whether the answer is an error. A run record keeps each as it is (cli.ts's
 *  runs). A record made before the runner rebuilt the pin's summaries may hold `wholeText`: a
 *  read of a code file of 100 lines or more, answered then with its whole text in place of a
 *  summary the pin may have shown; no run writes it now, and the report still counts those it
 *  finds. */
export const CheckoutRead = z.strictObject({
  from: z.literal("checkout"),
  tool: z.string(),
  input: z.unknown(),
  isError: z.boolean(),
  wholeText: z.literal(true).optional(),
});
export type CheckoutRead = Readonly<z.infer<typeof CheckoutRead>>;

/** One answer an answered round recorded: a read of a skill file, or a call the checkout copy
 *  answered. */
export type RoundRead = AnsweredRead | CheckoutRead;

/** One replay: the judged step (each answered round's tool calls, then the reply that ended the
 *  run; its other fields are that reply's), how many rounds of skill reads and of checkout reads
 *  were answered before it and each round's answers in order, what each reply used, in the order
 *  sent, the session id every round of the run went with (gateway.ts's `freshIdentity`; null for
 *  a body that names none), the routing id every round went with (null when none was given, and
 *  for a Responses body), and the deployment that answered each reply, in the order sent
 *  (gateway.ts's `deploymentOf`). */
export interface Replayed {
  readonly step: Reply;
  readonly rounds: number;
  readonly checkoutRounds: number;
  readonly reads: readonly (readonly RoundRead[])[];
  readonly usage: Spent[];
  readonly userId: string | null;
  readonly routingId: string | null;
  readonly deployments: string[];
}

/** What an answered round's answer names: a skill read's skill and file, a checkout read's tool
 *  and the path, pattern or command it named. */
function readWords(read: RoundRead): string {
  if (read.from !== "checkout") return `${read.skill} ${read.file}`;
  const input = typeof read.input === "object" && read.input !== null ? read.input : {};
  const named = ["command", "path", "pattern"]
    .map((key) => Reflect.get(input, key))
    .find((value) => typeof value === "string");
  return `${read.tool} ${named ?? ""}`.trim();
}

/** What a replay rejects with when the reply after its last answered round still only reads skill
 *  files, or, in a written case's run, still only makes read-only calls its checkout copy answers:
 *  the run gets no verdict (see the file's header). `replayed` is the run as it stood, its `step`
 *  that reply after the answered rounds' calls. Its message begins READS_PAST_LAST_ROUND, and names
 *  the reply's reads. */
export class ReadsPastLastRound extends Error {
  constructor(
    readonly replayed: Replayed,
    last: readonly RoundRead[]
  ) {
    const reads = last.map(readWords).join(", ");
    super(
      last.every((read) => read.from !== "checkout")
        ? `${READS_PAST_LAST_ROUND}: after ${replayed.rounds} answered rounds of skill reads, the reply still only reads skill files (${reads})`
        : `${READS_PAST_LAST_ROUND}: after ${replayed.checkoutRounds} answered rounds of read-only calls answered from the checkout and ${replayed.rounds} of skill reads, the reply still only makes read-only calls the checkout answers (${reads})`
    );
  }
}

/** What a written case's replay rejects with when a reply's every call is read-only and the
 *  checkout copy answers one of them in no form (checkout.ts's `answerCall`), or the case has no
 *  checkout and one of them is no skill read: the run gets no verdict, never judged on a call
 *  every arm would take alike (see the file's header).
 *  `replayed` is the run as it stood, its `step` that reply after the answered rounds' calls. Its
 *  message begins UNANSWERED_READ, and names the call and why. */
export class UnansweredRead extends Error {
  constructor(
    readonly replayed: Replayed,
    why: string
  ) {
    super(`${UNANSWERED_READ}: ${why}`);
  }
}

/** How a replay runs: the most rounds of skill reads it answers; the installed copies of the
 *  arm's other skills, which a read of one is answered from (arms.ts's `InstalledCopy`,
 *  context.ts's `installedCopies` of the arm's capture's record; none by default, and a read of a
 *  skill the profile lists with no copy here fails the run); the headers that go with the body on
 *  every round (gateway.ts's `SendOptions`: the `anthropic-*` headers Oh My Pi sent beside a
 *  captured Messages body, whose betas its fields need; none beside a Responses body); the
 *  routing id that goes with a Messages body on every round, in gateway.ts's ROUTING_HEADER (a
 *  UUID; none by default, and none beside a Responses body, whatever is given); and `endsAt`,
 *  which is told each reply's content and the answers made to its reads (`round`; undefined when
 *  the reply holds no tool call, or one that reads no skill file), before the reply after the
 *  last answered round is refused. When it gives true, the run ends at that reply: the reply is
 *  the judged step, its answers are not sent, and they are in no round of `reads`. Without it, a
 *  run ends only as the file's header says. load.ts's load test uses it. A written case's run
 *  gives `checkout`, the copy of its checkout its read-only calls are answered from, at most
 *  MAX_CHECKOUT_ROUNDS rounds of them, or, for a written case with no checkout, why none is
 *  answered (see the file's header); a run without one answers none, and is judged on them. */
export interface ReplayOptions extends SendOptions {
  readonly maxSkillRounds?: number;
  readonly copies?: readonly InstalledCopy[];
  readonly routingId?: string;
  readonly endsAt?: (
    content: readonly ContentBlock[],
    round: readonly SkillRead[] | undefined
  ) => boolean;
  readonly checkout?: CheckoutCopy | Unanswered;
}

/** The routing id a run of `body` goes with when `id` is given: `id` beside a Messages body, and
 *  none (null) beside a Responses body, whose route takes no header of the caller's. */
function routingFor(body: RequestBody, id: string | undefined): string | null {
  return id === undefined || isResponsesBody(body) ? null : id;
}

/** A tool call an answered round answered: the call's id, the answer's text, and its record,
 *  which says whether the answer is an error. */
interface AnsweredCall {
  readonly id: string;
  readonly text: string;
  readonly read: RoundRead;
  /** A text block a plugin adds after the tool's (knives', checkout.ts's `KnivesRepository`). */
  readonly added?: string;
}

/** A tool call that reads a skill file, answered. */
export interface SkillRead extends AnsweredCall {
  readonly read: AnsweredRead;
}

/** The read `target` the tool call `id` makes, answered, or undefined when it reads no skill file
 *  (see the file's header). A read with one line range (arms.ts's `readRange`: `:N-M`, `:N..M`,
 *  `:N+K`, `:N`, `:N-`, or a list that merges into one) is answered with those lines
 *  (`skillFileRange`, arms.ts's `pathFile` and `otherSkillRead`), one with any other selector not.
 *  In an arm whose profile lists a copy of
 *  another source under the case's skill's name (arms.ts's `caseNameCopy`: `none`, or the arm of
 *  ours once their copies are gone), a `skill://` read of the case's skill is a read of that copy,
 *  which `otherSkillRead` answers (in `none`, `ownSkillReadFile` gives no file). A read of a
 *  directory of the arm's skill is none: Oh My Pi answers it with a listing, which the arm does
 *  not rebuild; nor is a read by path that `pathFile` does not answer, nor one `otherSkillRead`
 *  does not, nor a `skill://` range past the end of its file (a range past the end of a file of
 *  the arm's copy read by path, `arm.pathCopy`, gets the pin's notice, `pathFile`). In the arm of
 *  ours that lists no such copy, a read of the case's skill (`arm.ours.theirs`) gets Oh My Pi's
 *  unknown-skill text (`unknownSkill`): the arm does not install it. */
function skillRead(
  id: string,
  target: string,
  arm: Arm,
  copies: readonly InstalledCopy[]
): SkillRead | undefined {
  const from = arm.skillDir === undefined ? "absent" : "arm";
  const ranged = readRange(target);
  const whole = ranged?.path ?? target;
  const file = ownSkillReadFile(arm, whole);
  if (file !== undefined) {
    if (ranged !== undefined && ranged.range === undefined) return undefined;
    if (arm.skillDir !== undefined) {
      const stats = statSync(path.resolve(arm.skillDir, file), { throwIfNoEntry: false });
      if (stats?.isDirectory()) return undefined;
    }
    const answer =
      ranged?.range === undefined ? skillFile(arm, file) : skillFileRange(arm, file, ranged.range);
    if (answer === undefined) return undefined;
    const read = { skill: arm.skill, file, by: "skill", from, isError: answer.isError } as const;
    return { id, text: answer.text, read };
  }
  const theirs = caseNameCopy(arm) === undefined ? arm.ours?.theirs : undefined;
  const theirFile = theirs === undefined ? undefined : skillReadFile(theirs, whole);
  if (theirs !== undefined && theirFile !== undefined) {
    if (ranged !== undefined && ranged.range === undefined) return undefined;
    const answer = unknownSkill(arm, theirs);
    const read = {
      skill: theirs,
      file: theirFile,
      by: "skill",
      from: "absent",
      isError: answer.isError,
    } as const;
    return { id, text: answer.text, read };
  }
  const byPath = pathFile(arm, target);
  if (byPath !== undefined) {
    const read = {
      skill: arm.skill,
      file: pathReadFile(arm, whole) ?? whole,
      by: "path",
      from,
      isError: byPath.isError,
    } as const;
    return { id, text: byPath.text, read };
  }
  const other = otherSkillRead(arm, copies, target);
  if (other === undefined) return undefined;
  const { text, ...read } = other;
  return { id, text, read };
}

/** The reads a reply's `content` makes, answered, or undefined when it holds no tool call, or a
 *  tool call that reads no skill file. No read is answered unless every call is a `read` of a path
 *  or URL, so a reply that also makes another call never fails on a read the replay cannot
 *  answer (`otherSkillRead`'s throw). */
function roundReads(
  content: readonly ContentBlock[],
  arm: Arm,
  copies: readonly InstalledCopy[]
): SkillRead[] | undefined {
  const calls: { id: string; target: string }[] = [];
  for (const { id, name, input } of toolCalls(content)) {
    const target = isObject(input) ? input.path : undefined;
    if (id === undefined || name !== "read" || typeof target !== "string") return undefined;
    calls.push({ id, target });
  }
  if (calls.length === 0) return undefined;
  const reads: SkillRead[] = [];
  for (const { id, target } of calls) {
    const read = skillRead(id, target, arm, copies);
    if (read === undefined) return undefined;
    reads.push(read);
  }
  return reads;
}

/** Why a Responses body's run is not answered where knives adds its blocks: they are a text block
 *  of their own after the tool's (checkout.ts's `KnivesRepository`), which a Responses body's one
 *  output string has no place for. */
const RESPONSES_KNIVES =
  "knives adds its blocks as a text block of their own after the tool's, which the runner does not rebuild in a Responses body's output";

/** `read`, the answer to the skill read `input` of a written case's run, with knives' blocks after
 *  it where they go there (checkout.ts's `knivesAdded`, which reads its `path` as knives reads a
 *  read's), from the case's checkout copy `checkout` and the run's `artifacts`; as it is without
 *  a copy (a session case, or a written case whose copy is why it has none); or why not. */
function knivesRead(
  read: SkillRead,
  input: unknown,
  checkout: CheckoutCopy | Unanswered | undefined,
  artifacts: RunArtifacts | undefined,
  responses: boolean
): SkillRead | Unanswered {
  if (checkout === undefined || "why" in checkout || artifacts === undefined) return read;
  if (!isObject(input)) return read;
  const added = knivesAdded(checkout, input, artifacts);
  if (added === undefined) return read;
  if (typeof added !== "string") return { why: `read ${JSON.stringify(input)}: ${added.why}` };
  if (responses) return { why: `read ${JSON.stringify(input)}: ${RESPONSES_KNIVES}` };
  return { ...read, added };
}

/** The answers to a reply's `content` from the checkout copy `checkout` when every tool call of it
 *  is read-only (checkout.ts's `readOnlyCall`): each read of a skill file as `skillRead` answers
 *  it (with knives' blocks where they go there, `knivesRead`), each other call from the copy
 *  (`answerCall`, the pinned Oh My Pi's artifacts numbered as the run's `artifacts` hold them),
 *  or, where `checkout` is why the case has none, with no answer; undefined when it holds no tool
 *  call or one that is not read-only; `why` a call of it is answered in no form, when one is (see
 *  the file's header). */
async function checkoutRound(
  content: readonly ContentBlock[],
  arm: Arm,
  copies: readonly InstalledCopy[],
  checkout: CheckoutCopy | Unanswered,
  artifacts: RunArtifacts | undefined,
  responses: boolean
): Promise<AnsweredCall[] | Unanswered | undefined> {
  const calls = toolCalls(content);
  if (calls.length === 0) return undefined;
  for (const { id, name, input } of calls) {
    if (id === undefined || !readOnlyCall(name, input)) return undefined;
  }
  const rows = newReadRows(arm);
  const answers: AnsweredCall[] = [];
  for (const { id = "", name, input } of calls) {
    const target = name === "read" && isObject(input) ? input.path : undefined;
    const skill = typeof target === "string" ? skillRead(id, target, arm, copies) : undefined;
    if (skill !== undefined) {
      const given = knivesRead(skill, input, checkout, artifacts, responses);
      if ("why" in given) return given;
      answers.push(given);
      continue;
    }
    const answer =
      "why" in checkout ? checkout : await answerCall(checkout, name, input, rows, artifacts);
    if ("why" in answer) return { why: `${name} ${JSON.stringify(input)}: ${answer.why}` };
    const { text, isError, added } = answer;
    if (added !== undefined && responses) {
      return { why: `${name} ${JSON.stringify(input)}: ${RESPONSES_KNIVES}` };
    }
    const read = { from: "checkout", tool: name ?? "", input, isError } as const;
    answers.push({ id, text, read, ...(added === undefined ? {} : { added }) });
  }
  return answers;
}

/** `body` with `content`, a reply whose every tool call is one of `reads`, and the answer to each
 *  read added, in the form of the body's API (see the file's header). */
function answered(
  body: RequestBody,
  content: readonly ContentBlock[],
  reads: readonly AnsweredCall[]
): RequestBody {
  if (isResponsesBody(body)) {
    const outputs = reads.map(({ id, text }) => ({
      type: "function_call_output",
      call_id: id,
      output: text,
    }));
    return { ...body, input: [...body.input, ...content, ...outputs] };
  }
  const results = reads.map(({ id, text, read, added }) => ({
    type: "tool_result",
    tool_use_id: id,
    content: [
      { type: "text", text },
      ...(added === undefined ? [] : [{ type: "text", text: added }]),
    ],
    is_error: read.isError,
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
 *  file's header), the run's own session id `userId` (`freshIdentity`) and its routing id
 *  `routingId` (`routingFor`; none when null), which every round of the run keeps; answers its
 *  skill reads, and, with `checkout`, its rounds of read-only calls, and returns the judged step,
 *  or rejects with ReadsPastLastRound or, with `checkout`, UnansweredRead (see the file's
 *  header). `sent` is never changed, and every round's body shares its parts. `firstSettled` is
 *  told once the run's first send has its reply or its failure. */
async function replaySent(
  sent: RequestBody,
  userId: string | null,
  routingId: string | null,
  arm: Arm,
  gateway: Gateway,
  { maxSkillRounds = MAX_SKILL_ROUNDS, copies = [], headers, endsAt, checkout }: ReplayOptions,
  firstSettled?: () => void
): Promise<Replayed> {
  const sendHeaders = routingId === null ? headers : { ...headers, [ROUTING_HEADER]: routingId };
  let body = sent;
  const usage: Spent[] = [];
  const deployments: string[] = [];
  const reads: RoundRead[][] = [];
  // The answered rounds' tool calls, which the judged step holds before the reply that ends it.
  const calls: ContentBlock[] = [];
  let rounds = 0;
  let checkoutRounds = 0;
  // The pinned Oh My Pi's artifacts this run's answers make, numbered past the case's history's.
  const artifacts =
    checkout === undefined || "why" in checkout ? undefined : runArtifacts(checkout);
  for (let sends = 0; ; sends += 1) {
    let reply: Reply;
    try {
      reply = await gateway.send(body, { headers: sendHeaders });
    } finally {
      if (sends === 0) firstSettled?.();
    }
    usage.push(reply.spent);
    deployments.push(deploymentOf(reply));
    const content = reply.message.content;
    const round = roundReads(content, arm, copies);
    const step = { ...reply, message: { ...reply.message, content: [...calls, ...content] } };
    const replayed = {
      step,
      rounds,
      checkoutRounds,
      reads,
      usage,
      userId,
      routingId,
      deployments,
    };
    if (endsAt?.(content, round) === true) return replayed;
    let answers: readonly AnsweredCall[];
    if (round !== undefined) {
      if (rounds >= maxSkillRounds) {
        throw new ReadsPastLastRound(
          replayed,
          round.map(({ read }) => read)
        );
      }
      rounds += 1;
      const inputs = toolCalls(content);
      const given: SkillRead[] = [];
      for (const [index, read] of round.entries()) {
        const each = knivesRead(
          read,
          inputs[index]?.input,
          checkout,
          artifacts,
          isResponsesBody(body)
        );
        if ("why" in each) throw new UnansweredRead(replayed, each.why);
        given.push(each);
      }
      answers = given;
    } else {
      const looked =
        checkout === undefined
          ? undefined
          : await checkoutRound(content, arm, copies, checkout, artifacts, isResponsesBody(body));
      if (looked === undefined) return replayed;
      if ("why" in looked) throw new UnansweredRead(replayed, looked.why);
      if (checkoutRounds >= MAX_CHECKOUT_ROUNDS) {
        throw new ReadsPastLastRound(
          replayed,
          looked.map(({ read }) => read)
        );
      }
      checkoutRounds += 1;
      answers = looked;
    }
    reads.push(answers.map(({ read }) => read));
    calls.push(...content.filter(isToolCallBlock));
    body = answered(body, content, answers);
  }
}

/** Sends `body` for `arm` with a session id of its own when it names one (`freshIdentity`) and
 *  with `options.routingId` (`routingFor`), answers its skill reads, and returns the judged step.
 *  Rejects when a send does (the gateway's `BudgetSpent` among them), with ReadsPastLastRound
 *  when the reply after the last answered round still only reads, and with UnansweredRead when a
 *  written case's reply makes a read-only call its checkout copy answers in no form, or that a
 *  case with no checkout answers none of. `firstSettled` is told once the run's first send has
 *  its reply or its failure. */
export function replayOnce(
  body: RequestBody,
  arm: Arm,
  gateway: Gateway,
  options: ReplayOptions = {},
  firstSettled?: () => void
): Promise<Replayed> {
  const own = freshIdentity(isResponsesBody(body) ? body : withCacheBreakpoint(body));
  const routingId = routingFor(body, options.routingId);
  return replaySent(own.body, own.id, routingId, arm, gateway, options, firstSettled);
}

/** One arm's runs in a round: its arm, its captured body, the headers that go with it (its
 *  capture's `anthropic-*` headers, none beside a Responses body), the installed copies of its
 *  other skills (`ReplayOptions.copies`), how many runs to make, and the routing id of each run
 *  (`ReplayOptions.routingId`): its runs in the order sent take them in turn, so the arm's k-th
 *  run in the send order goes with the k-th (none given, or a Responses body: none). */
export interface ArmRuns {
  readonly arm: Arm;
  readonly body: RequestBody;
  readonly headers?: Readonly<Record<string, string>>;
  readonly copies?: readonly InstalledCopy[];
  readonly runs: number;
  readonly routingIds?: readonly string[];
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
  /** The routing id every request of the run went with, or null for none. */
  readonly routingId: string | null;
  readonly outcome: PromiseSettledResult<Replayed>;
}

/**
 * The runs of every arm of a round, sent in one order, `sendOrder` under `seed`, never one arm's
 * batch after another's: a gateway's state (its deployment, its cache, its load) then falls on
 * every arm alike. Each run is `replayOnce`'s, with a session id of its own when its body names
 * one, and the routing id its arm gives it (`ArmRuns.routingIds`). The runs start in that order,
 * all at once but for one wait (the gateway bounds how many are in flight, and gives its places
 * in the order asked): the first run of each arm in the order goes alone among its arm's, the
 * next run in the order starting only once its first send has its reply or has failed, so that
 * the prefix its reply wrote to the cache serves its arm's later runs. Those waits change no
 * place of the order. Each arm's body to send is made once for all its runs, each run's a copy
 * of its top level with its own id (the body itself, when it names no id). Each run's outcome is
 * kept, whether it was a step or a failure, in the order sent. A written case's round gives
 * `checkout`, the copy every arm's runs answer read-only calls from, or, where the case has none,
 * why none is answered.
 */
export async function replayInterleaved(
  arms: readonly ArmRuns[],
  gateway: Gateway,
  seed: string,
  options: Pick<ReplayOptions, "maxSkillRounds" | "checkout"> = {}
): Promise<InterleavedRun[]> {
  const byName = new Map<ArmName, ArmRuns & { readonly sent: RequestBody }>();
  for (const each of arms) {
    if (!Number.isInteger(each.runs) || each.runs < 0) {
      throw new Error(`a replay runs a whole number of times, 0 or more, not ${each.runs}`);
    }
    if (byName.has(each.arm.name)) throw new Error(`the arm ${each.arm.name} is given twice`);
    if (each.routingIds !== undefined && each.routingIds.length !== each.runs) {
      throw new Error(
        `the arm ${each.arm.name} makes ${each.runs} runs and is given ${each.routingIds.length} routing ids`
      );
    }
    const sent = isResponsesBody(each.body) ? each.body : withCacheBreakpoint(each.body);
    byName.set(each.arm.name, { ...each, sent });
  }
  const order: Slot[] = sendOrder(
    arms.map((each) => ({ key: each.arm.name, runs: each.runs })),
    seed
  ).map(({ key, run }) => ({ arm: key, run }));
  // How many runs of each arm have started, in the send order.
  const started = new Map<ArmName, number>();
  const runs: {
    slot: Slot;
    place: number;
    userId: string | null;
    routingId: string | null;
    replayed: Promise<Replayed>;
  }[] = [];
  for (const [place, slot] of order.entries()) {
    const each = byName.get(slot.arm);
    if (each === undefined) throw new Error(`no arm ${slot.arm}`);
    const nth = started.get(slot.arm) ?? 0;
    started.set(slot.arm, nth + 1);
    const settled = Promise.withResolvers<void>();
    const own = freshIdentity(each.sent);
    const routingId = routingFor(each.sent, each.routingIds?.[nth]);
    const replayed = replaySent(
      own.body,
      own.id,
      routingId,
      each.arm,
      gateway,
      {
        ...options,
        ...(each.headers === undefined ? {} : { headers: each.headers }),
        ...(each.copies === undefined ? {} : { copies: each.copies }),
      },
      settled.resolve
    );
    // Its outcome is read once every run has started; until then a failure is held, not thrown.
    replayed.catch(() => undefined);
    runs.push({ slot, place, userId: own.id, routingId, replayed });
    if (nth === 0) await settled.promise;
  }
  const outcomes = await Promise.allSettled(runs.map((run) => run.replayed));
  return runs.map(({ slot, place, userId, routingId }, index) => ({
    ...slot,
    place,
    userId,
    routingId,
    outcome: outcomes[index] as PromiseSettledResult<Replayed>,
  }));
}
