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
 * The first reply that holds no tool call, or a tool call that reads no skill file (a read of
 * another path on disk, a read of a directory of a skill, a read with another selector, a read by
 * path of a file Oh My Pi shows in a form the arms do not rebuild, any other tool) and no read
 * that the replay answers, ends the run. The judged step (`Replayed.step`) is each answered
 * round's answered calls, without their answers, then that reply: the judge sees which files the
 * agent read before it acted, so a case whose line governs a read of a file of the skill ("read
 * references/x.md first") passes on that read. A reply after the last answered round whose every
 * tool call still reads a skill file gets no verdict: the replay rejects with ReadsPastLastRound
 * (READS_PAST_LAST_ROUND), and cli.ts makes the run again as any run with no verdict; judged, the
 * read would fail in every arm. Each answered round's reads are recorded (`Replayed.reads`: the
 * skill, the file, how it was read, and where the answer came from).
 *
 * A mixed reply holds a read the replay answers and a call it does not (the runner's
 * architect, 2026-11-26: the agent batches a read of a reference the text sends it to with the
 * calls that prepare its act, and that batch is not its attempt at the act). Without a rule
 * (`ReplayOptions.mixed`) it ends the run as any reply with another call does. With one, its
 * other calls (and, for a case whose act is text, its text) are sorted with no model call
 * (`replyBoundary`, `callPart`): a behaviour case's rule (`caseAct`) sorts each by the tokens of
 * its arguments against the case's two steps, and a load test's (LOAD_RULE) by what the call can
 * do. A reply with an act (an attempt at the case's act; in a load test, an action that starts
 * the task) is the judged step as it came (`Replayed.mixedAct`). A reply with no act and a call
 * neither rule can sort gets no verdict: the replay rejects with UndecidedReply
 * (MIXED_UNDECIDED), naming those calls, and is not continued. A reply whose other calls only
 * prepare is a round of its own while rounds are left: its reads are answered, and it goes back
 * holding only those read calls (a Messages reply less its unanswered `tool_use` blocks, its
 * thinking and text kept; a Responses reply's output items less its unanswered `function_call`
 * items, its reasoning and message items kept in their order), with one answer per read. No other
 * call is run and no result is made up for one: the calls left out are recorded
 * (`Replayed.leftOut`: the round, the tool and its first argument) and are in no body and in no
 * judged step. After the last round such a reply gets no verdict, as ReadsPastLastRound. In a
 * written case's run, a reply whose every call is read-only (checkout.ts's `readOnlyCall`) is a
 * round of the checkout, below, and never a mixed reply: the rule sorts only a reply that also
 * makes a call that is not read-only, and the reads of such a round it continues take knives'
 * blocks where a round of skill reads would (`knivesRead`).
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
 * arm and run of a case), and a later `read` of that artifact in the run; a `bash` call that
 * names a path under the directory the case's reads show its skill in (`arm.pathCopy`), answered
 * with the arm's own copy of the skill there (checkout.ts's `CheckoutCopy.installed`, from
 * `arm.skillDir`; nothing there in the arm with no skill, so the shell gives its error for a path
 * it lacks); a `read` of a file of the session's dotfiles checkout outside that directory,
 * answered from the commit of the dotfiles the case's profile names, the same in every arm
 * (checkout.ts's `CheckoutCopy.dotfiles`, from cli.ts's `caseDotfiles`, its root found in
 * `arm.pathCopy`); and a `read` of one of
 * the pin's internal URLs (`history://`, `local://`, `xd://<device>`, …), answered by the pin's
 * read tool as a fresh session answers it. A reply one of whose read-only calls the copy answers
 * in no form (a selector of a form not rebuilt, a path outside the checkout and outside that
 * directory, but for a file that dotfiles commit tracks, a git command where
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
 *
 * A saved run is resumed from its saved reply (`resumeReplay`; cli.ts's `behaviour
 * continue-mixed`). The same replay loop runs it from the body `replayOnce` sends first, with
 * the saved session id and routing id. Where earlier rounds are available whole, that loop
 * rebuilds their answers without sending requests, including checkout artifacts, before it
 * answers the saved reply. Its first send is the resulting body with those rounds and their
 * answers. Later replies follow the same rules and round limits as a live run. A resume given
 * the most requests it sends
 * (`furtherRounds`) stops a run whose reply after them would be sent on again with no verdict
 * (FurtherRoundLimit, score.ts's FURTHER_ROUND_LIMIT), so the next round can be priced before it
 * runs. A saved reply no replay sends back is refused before any send (NotResumable).
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
import { type Case, pathReadFile, skillReadFile } from "./cases";
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
  type ToolCall,
  toolCalls,
} from "./gateway";
import { FURTHER_ROUND_LIMIT } from "./score";

/** The most rounds of skill reads a replay answers before a reply that holds another call or none.
 *  Three: of 3,680 posted runs of the behaviour test, 137 had one round answered and none had two;
 *  a read of another skill (dispatch-first, a skill that sends the agent to others), then a read of
 *  the case's skill, then the act, takes two. */
export const MAX_SKILL_ROUNDS = 3;
/** The most rounds of read-only calls a written case's replay answers from its checkout copy,
 *  beside its rounds of skill reads: a look at a few files before the act the line governs. */
export const MAX_CHECKOUT_ROUNDS = 5;

/** How the reason of a run begins whose reply after the last answered round still only reads
 *  skill files, or still reads them beside calls that only prepare the act (ReadsPastLastRound):
 *  such a run gets no verdict. */
export const READS_PAST_LAST_ROUND = "reads-past-last-round";
/** How the reason of a run begins whose reply, in a written case's run, makes a read-only call
 *  its checkout copy answers in no form (UnansweredRead): such a run gets no verdict. */
export const UNANSWERED_READ = "unanswered-read";

/** How the reason of a run begins whose mixed reply has no act and a call the rule sorts neither
 *  as the act nor as preparation (UndecidedReply): such a run gets no verdict, and is not
 *  continued. */
export const MIXED_UNDECIDED = "mixed-undecided";

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

/** One call a continued round left out (see the file's header): the answered round it was in
 *  (from 1, the round whose reads `Replayed.reads` holds at that place), its tool, and its first
 *  argument but the intent label `i`, as text (a string as it is, else its JSON), or null when it
 *  has none. A run record keeps each as it is (cli.ts's runs, load.ts's runs). */
export const LeftOutCall = z.strictObject({
  round: z.int().positive(),
  tool: z.string(),
  argument: z.string().nullable(),
});
export type LeftOutCall = Readonly<z.infer<typeof LeftOutCall>>;

/** One replay: the judged step (each answered round's answered calls, then the reply that ended
 *  the run; its other fields are that reply's), how many rounds of skill reads and of checkout
 *  reads were answered before it and each round's answers in order, the calls the continued
 *  rounds left out (LeftOutCall), whether the reply that ended it is a mixed reply with an act
 *  (`replyBoundary`'s `acts`), what each reply used, in the order sent, the session id every
 *  round of the run went with (gateway.ts's `freshIdentity`; null for a body that names none),
 *  the routing id every round went with (null when none was given, and for a Responses body), and
 *  the deployment that answered each reply, in the order sent (gateway.ts's `deploymentOf`). */
export interface Replayed {
  readonly step: Reply;
  readonly rounds: number;
  readonly checkoutRounds: number;
  readonly reads: readonly (readonly RoundRead[])[];
  /** Whole replies of the answered rounds in order, recorded for written cases. */
  readonly answeredReplies: readonly (readonly ContentBlock[])[];
  readonly leftOut: readonly LeftOutCall[];
  readonly mixedAct: boolean;
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

/** A call of a mixed reply in words: its tool, then its first argument cut to 80 characters. */
function callWords(call: SortedCall): string {
  const { argument } = call;
  if (argument === null) return call.tool;
  return `${call.tool} ${argument.length > 80 ? `${argument.slice(0, 79)}…` : argument}`;
}

/** What a replay rejects with when the reply after its last answered round still only reads skill
 *  files, or reads them beside calls that only prepare the act (`beside`, the calls of a mixed
 *  reply `replyBoundary` would continue), or, in a written case's run, still only makes read-only
 *  calls its checkout copy answers: the run gets no verdict (see the file's header). `replayed` is
 *  the run as it stood, its `step` that reply after the answered rounds' calls. Its message begins
 *  READS_PAST_LAST_ROUND, and names the reply's reads and the calls beside them. */
export class ReadsPastLastRound extends Error {
  constructor(
    readonly replayed: Replayed,
    last: readonly RoundRead[],
    beside: readonly SortedCall[] = []
  ) {
    const reads = last.map(readWords).join(", ");
    const others = beside.map(callWords).join(", ");
    super(
      beside.length > 0
        ? `${READS_PAST_LAST_ROUND}: after ${replayed.rounds} answered rounds of skill reads, the reply still reads skill files (${reads}) beside other calls, none of them an act (${others})`
        : last.every((read) => read.from !== "checkout")
          ? `${READS_PAST_LAST_ROUND}: after ${replayed.rounds} answered rounds of skill reads, the reply still only reads skill files (${reads})`
          : `${READS_PAST_LAST_ROUND}: after ${replayed.checkoutRounds} answered rounds of read-only calls answered from the checkout and ${replayed.rounds} of skill reads, the reply still only makes read-only calls the checkout answers (${reads})`
    );
  }
}

/** What a replay rejects with when a mixed reply has no act and a call the rule sorts neither as
 *  the act nor as preparation (`replyBoundary`'s `undecided`): the run gets no verdict and is not
 *  continued (see the file's header). `replayed` is the run as it stood, its `step` the answered
 *  rounds' calls and that reply as it came. Its message begins MIXED_UNDECIDED, and names the
 *  reply's reads and each undecided call, or its text. */
export class UndecidedReply extends Error {
  constructor(
    readonly replayed: Replayed,
    last: readonly AnsweredRead[],
    undecided: readonly SortedCall[],
    text: boolean
  ) {
    const files = last.map((read) => `${read.skill} ${read.file}`).join(", ");
    const what = [...undecided.map(callWords), ...(text ? ["its text"] : [])].join(", ");
    super(
      `${MIXED_UNDECIDED}: after ${replayed.rounds} answered rounds of skill reads, the reply reads skill files (${files}) beside what the rule sorts neither as an act nor as preparation (${what}), so it is neither judged nor continued`
    );
  }
}

/** What a written case's replay rejects with when its checkout cannot answer a read-only call.
 *  The run gets no verdict; `replayed` keeps the reply and the preceding answered rounds. */
export class UnansweredRead extends Error {
  constructor(
    readonly replayed: Replayed,
    why: string
  ) {
    super(`${UNANSWERED_READ}: ${why}`);
  }
}

/** A continued run waits when it reaches its further-request limit and the next reply would
 *  otherwise be sent back. Its answered rounds, final reply and spend are kept. */
export class FurtherRoundLimit extends Error {
  constructor(
    readonly replayed: Replayed,
    furtherRounds: number,
    last: readonly RoundRead[],
    beside: readonly SortedCall[]
  ) {
    const checkout = last.some((read) => read.from === "checkout");
    const files = last.map(readWords).join(", ");
    const others =
      beside.length === 0
        ? ""
        : ` beside calls that only prepare (${beside.map(callWords).join(", ")})`;
    super(
      `${FURTHER_ROUND_LIMIT}: after ${furtherRounds} further rounds of the continued run (${replayed.rounds + replayed.checkoutRounds} answered rounds in all), its reply still ${checkout ? "makes read-only calls" : "reads skill files"} (${files})${others}, so it would be continued again: it stops with no verdict until the next round is decided`
    );
  }
}

/** How a replay runs: the most rounds of skill reads it answers; the installed copies of the
 *  arm's other skills, which a read of one is answered from (arms.ts's `InstalledCopy`,
 *  context.ts's `installedCopies` of the arm's capture's record; none by default, and a read of a
 *  skill the profile lists with no copy here fails the run); the headers that go with the body on
 *  every round (gateway.ts's `SendOptions`: the `anthropic-*` headers Oh My Pi sent beside a
 *  captured Messages body, whose betas its fields need; none beside a Responses body); the
 *  routing id that goes with a Messages body on every round, in gateway.ts's ROUTING_HEADER (a
 *  UUID; none by default, and none beside a Responses body, whatever is given); the rule that
 *  sorts a mixed reply's other calls (`mixed`, MixedRule; without one a mixed reply ends the
 *  run, as any reply with another call); and `endsAt`, which is told each reply's content and the
 *  answers the replay would make to its reads (`round`; undefined unless `replyBoundary` makes it
 *  a skill round or continues it), before the reply after the last answered round is refused:
 *  each reply a send got, never the saved reply a resumed run starts from (`resumeReplay`).
 *  When it gives true, the run ends at that reply: the reply is the judged step, its answers are
 *  not sent, and they are in no round of `reads`. Without it, a run ends only as the file's
 *  header says. The load test and the continue-mixed probe use it. A written case's run gives
 *  `checkout`, the copy of its checkout its read-only calls are answered from, at most
 *  MAX_CHECKOUT_ROUNDS rounds, or why none is answered. A run without it does not answer a
 *  checkout round. */
export interface ReplayOptions extends SendOptions {
  readonly maxSkillRounds?: number;
  readonly copies?: readonly InstalledCopy[];
  readonly routingId?: string;
  readonly mixed?: MixedRule;
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

/** What splits a call's argument or a step's text into the act rule's tokens (`tokens`):
 *  whitespace and the characters " ' ` ; | & ( ) , = < >. */
const TOKEN_BREAK = /[\s"'`;|&(),=<>]+/;

/** The tokens of `text` as the act rule reads them (the runner's architect,
 *  2026-11-26T06:16:45Z): split at TOKEN_BREAK, lowercased, each of 2 characters or more. */
export function tokens(text: string): string[] {
  return text
    .toLowerCase()
    .split(TOKEN_BREAK)
    .filter((token) => token.length >= 2);
}

/** The tokens of a tool call's arguments `input`: of every string value in it (a number and a
 *  boolean as text), at any depth, but the value of its intent label `i`, which says what the
 *  call is for, not what it does. */
export function argumentTokens(input: unknown): string[] {
  const found: string[] = [];
  const walk = (value: unknown): void => {
    if (typeof value === "string") found.push(...tokens(value));
    else if (typeof value === "number" || typeof value === "boolean") {
      found.push(...tokens(String(value)));
    } else if (Array.isArray(value)) {
      for (const item of value) walk(item);
    } else if (isObject(value)) {
      for (const field of Object.values(value)) walk(field);
    }
  };
  if (isObject(input)) {
    for (const [key, value] of Object.entries(input)) if (key !== "i") walk(value);
  } else walk(input);
  return found;
}

/** A tool call's arguments `input` without its intent label `i`, which says what the call is
 *  for, not what it does: two calls whose arguments differ only in it make the same call. */
function withoutIntent(input: unknown): unknown {
  if (!isObject(input)) return input;
  const { i: _intent, ...rest } = input;
  return rest;
}

/** One act call of a case (`caseAct`): its tool, its arguments without the intent label
 *  (`withoutIntent`), its distinguishing tokens, and whether it only reads or looks
 *  (`onlyLooks`). */
export interface ActCall {
  readonly tool: string;
  readonly input: unknown;
  readonly tokens: ReadonlySet<string>;
  readonly looks: boolean;
}

/** A case's act (`caseAct`): its act calls, and, where its two steps make the same calls, its
 *  distinguishing text tokens (`text`; undefined where the act is calls). */
export interface Act {
  readonly calls: readonly ActCall[];
  readonly text: ReadonlySet<string> | undefined;
}

/**
 * The act of case `c`, from its `realStep` and `violatingStep` (the runner's architect,
 * 2026-11-26T06:16:45Z), with no model: each step's `toolCall` blocks, each its tool and its
 * argument tokens (`argumentTokens`). The shared tokens are those the calls of both steps hold. A
 * call that the other step makes as it is (its tool and its arguments the same, the intent label
 * left out) is the steps' shared context, not the act, each matched once; each call of either
 * step left unmatched is an act call, and its distinguishing tokens are its tokens that are not
 * shared. Where no call is left unmatched, the two steps make the same calls and the act is the
 * text: its distinguishing tokens are the tokens of each step's text that the other step's text
 * lacks.
 */
export function caseAct(c: Pick<Case, "realStep" | "violatingStep">): Act {
  type StepCall = { readonly tool: string; readonly input: unknown };
  const callsOf = (step: Case["realStep"]): StepCall[] =>
    (step.content ?? []).flatMap((block) =>
      block.type === "toolCall" ? [{ tool: block.name ?? "", input: block.arguments }] : []
    );
  const real = callsOf(c.realStep);
  const violating = callsOf(c.violatingStep);
  const tokensOf = (calls: readonly StepCall[]) =>
    new Set(calls.flatMap((call) => argumentTokens(call.input)));
  const realTokens = tokensOf(real);
  const shared = new Set([...tokensOf(violating)].filter((token) => realTokens.has(token)));
  const unmatched = (own: readonly StepCall[], other: readonly StepCall[]): StepCall[] => {
    const left = [...other];
    return own.filter((call) => {
      const at = left.findIndex(
        (each) =>
          each.tool === call.tool &&
          Bun.deepEquals(withoutIntent(each.input), withoutIntent(call.input), true)
      );
      if (at < 0) return true;
      left.splice(at, 1);
      return false;
    });
  };
  const acts = [...unmatched(real, violating), ...unmatched(violating, real)];
  if (acts.length > 0) {
    return {
      calls: acts.map((call) => ({
        tool: call.tool,
        input: withoutIntent(call.input),
        tokens: new Set(argumentTokens(call.input).filter((token) => !shared.has(token))),
        looks: onlyLooks(call.tool, call.input),
      })),
      text: undefined,
    };
  }
  const textOf = (step: Case["realStep"]) =>
    new Set(
      tokens(
        (step.content ?? [])
          .flatMap((block) =>
            block.type === "text" && block.text !== undefined ? [block.text] : []
          )
          .join("\n")
      )
    );
  const realText = textOf(c.realStep);
  const violatingText = textOf(c.violatingStep);
  return {
    calls: [],
    text: new Set([
      ...[...realText].filter((token) => !violatingText.has(token)),
      ...[...violatingText].filter((token) => !realText.has(token)),
    ]),
  };
}

/** The programs whose words a `bash` command's simple command may start with and still only read
 *  or look (LOAD_RULE), each with the test its arguments must pass for that: any other program,
 *  and one whose arguments fail its test, leaves the call undecided. `gh api` as a GET, `gh pr
 *  view`, `pr list`, `run view`, `stack view`, `git worktree list`, `dispatch read`, `doc-read`,
 *  `env` with no argument, `ps`, and `jj log`, `diff`, `status` with no `--tool` are on it by the
 *  runner's architect's ruling of 2026-11-26T10:10:22Z: jj's snapshot of the working copy into
 *  its operation log is bookkeeping, not the task's work. */
const LOOKING_PROGRAMS: Readonly<Record<string, (args: readonly string[]) => boolean>> = (() => {
  const any = () => true;
  /** Where in `args` its subcommand is: the first argument that is no flag nor the value of one
   *  of `valued`'s flags; -1 when none is. */
  const subcommandAt = (args: readonly string[], valued: readonly string[]): number => {
    for (let at = 0; at < args.length; at += 1) {
      const arg = args[at] ?? "";
      if (!arg.startsWith("-")) return at;
      if (valued.includes(arg)) at += 1;
    }
    return -1;
  };
  /** Whether the subcommand of `args` is one of `looks`. */
  const looksBy =
    (looks: readonly string[], valued: readonly string[]) => (args: readonly string[]) =>
      looks.includes(args[subcommandAt(args, valued)] ?? "");
  const listing = ["-a", "-r", "-v", "-vv", "-l", "--all", "--remotes", "--list", "--show-current"];
  /** Whether the arguments `args` of `gh api` make it a GET: no `-X`/`--method` but GET (in any
   *  letter case), none of the flags that send fields or a body and so make it a POST (`-f`, `-F`,
   *  `--field`, `--raw-field`, `--input`, joined forms too, even beside an explicit GET), and an
   *  endpoint (the first argument that is no flag nor a flag's value) other than `graphql`, which
   *  gh always sends as a POST. A cluster of short flags is read as gh reads it: a flag that takes
   *  a value takes the rest of the cluster, or else the next argument. */
  const getOnly = (args: readonly string[]): boolean => {
    const sends = ["--field", "--raw-field", "--input"];
    const valued = [
      "--method",
      "--header",
      "--jq",
      "--template",
      "--hostname",
      "--cache",
      "--preview",
    ];
    let endpoint: string | undefined;
    for (let at = 0; at < args.length; at += 1) {
      const arg = args[at] ?? "";
      if (arg === "--") {
        endpoint ??= args[at + 1];
        break;
      }
      if (!arg.startsWith("-")) {
        endpoint ??= arg;
      } else if (arg.startsWith("--")) {
        const equals = arg.indexOf("=");
        const name = equals < 0 ? arg : arg.slice(0, equals);
        const value = equals < 0 ? undefined : arg.slice(equals + 1);
        if (sends.includes(name)) return false;
        if (value === undefined && valued.includes(name)) at += 1;
        if (name === "--method" && (value ?? args[at] ?? "").toUpperCase() !== "GET") return false;
      } else {
        for (let char = 1; char < arg.length; char += 1) {
          const flag = arg[char] ?? "";
          if (flag === "f" || flag === "F") return false;
          if (!"XHqtp".includes(flag)) continue;
          const joined = arg.slice(char + 1);
          if (joined === "") at += 1;
          const value = joined === "" ? (args[at] ?? "") : joined.replace(/^=/, "");
          if (flag === "X" && value.toUpperCase() !== "GET") return false;
          break;
        }
      }
    }
    return endpoint?.replace(/^\/+/, "") !== "graphql";
  };
  const shown = [
    ["pr", "view"],
    ["pr", "list"],
    ["run", "view"],
    ["stack", "view"],
  ];
  const runs = [
    "-delete",
    "-exec",
    "-execdir",
    "-ok",
    "-okdir",
    "-fprint",
    "-fprint0",
    "-fprintf",
    "-fls",
  ];
  const plain = [
    ...["ls", "cat", "head", "tail", "wc", "pwd", "cd", "grep", "egrep", "fgrep", "rg", "fd"],
    ...["jq", "tree", "stat", "file", "du", "df", "which", "type", "realpath", "readlink"],
    ...["basename", "dirname", "date", "whoami", "id", "hostname", "uname", "echo", "printf"],
    ...["true", "test", "[", "nl", "cut", "tr", "diff", "cmp", "comm", "printenv", "ps"],
  ];
  return {
    ...Object.fromEntries(plain.map((program) => [program, any])),
    find: (args) => !args.some((arg) => runs.includes(arg)),
    sed: (args) =>
      args[0] === "-n" && !args.some((arg) => arg.startsWith("-i") || arg.startsWith("--in-place")),
    sort: (args) => !args.some((arg) => arg.startsWith("-o") || arg.startsWith("--output")),
    command: (args) => args[0] === "-v" || args[0] === "-V",
    git: (args) => {
      const at = subcommandAt(args, ["-C", "-c"]);
      const rest = args.slice(at + 1);
      if (at < 0 || rest.some((arg) => arg.startsWith("--output"))) return false;
      if (args[at] === "branch") return rest.every((arg) => listing.includes(arg));
      if (args[at] === "remote") return rest.every((arg) => arg === "-v");
      if (args[at] === "worktree") return rest[0] === "list";
      const looks = ["status", "log", "diff", "show", "rev-parse", "ls-files", "blame", "grep"];
      return looks.includes(args[at] ?? "");
    },
    docker: looksBy(
      ["ps", "images", "inspect", "logs"],
      ["-H", "--host", "--context", "--config", "-l", "--log-level"]
    ),
    kubectl: looksBy(
      ["get", "describe", "logs"],
      ["-n", "--namespace", "--context", "--kubeconfig", "--cluster", "--user"]
    ),
    gh: (args) =>
      args[0] === "api"
        ? getOnly(args.slice(1))
        : shown.some(([command, sub]) => args[0] === command && args[1] === sub),
    dispatch: looksBy(["read", "doc-read"], []),
    env: (args) => args.length === 0,
    jj: (args) =>
      looksBy(
        ["log", "diff", "status"],
        ["-R", "--repository", "--at-operation", "--at-op", "--color", "--config", "--config-file"]
      )(args) && !args.some((arg) => arg === "--tool" || arg.startsWith("--tool=")),
  };
})();

/** The simple commands of the shell command line `command`, each its words with their quotes
 *  and escapes taken off, split at `;`, `&&`, `||`, `|`, `&` and new lines outside quotes; or
 *  undefined when it runs a command inside another (`$(…)`, backticks), writes a file by a
 *  redirection (`>`, `>>` or `&>` to anything but /dev/null or another stream), or leaves a
 *  quote open. */
export function simpleCommands(command: string): string[][] | undefined {
  const commands: string[][] = [];
  let words: string[] = [];
  let word = "";
  let quoted = false;
  let quote: string | undefined;
  const endWord = () => {
    if (word !== "" || quoted) words.push(word);
    word = "";
    quoted = false;
  };
  const endCommand = () => {
    endWord();
    if (words.length > 0) commands.push(words);
    words = [];
  };
  for (let at = 0; at < command.length; at += 1) {
    const char = command[at] ?? "";
    const next = command[at + 1];
    if (char === "\\" && quote !== "'") {
      word += next ?? "";
      at += 1;
    } else if (quote !== undefined) {
      if (char === quote) quote = undefined;
      else if (quote === '"' && (char === "`" || (char === "$" && next === "("))) return undefined;
      else word += char;
    } else if (char === "'" || char === '"') {
      quote = char;
      quoted = true;
    } else if (char === "`" || (char === "$" && next === "(")) {
      return undefined;
    } else if (char === ">" || (char === "&" && next === ">")) {
      const target = /^&?>>?\s*(\/dev\/null(?![^\s;&|])|&\d+)/.exec(command.slice(at));
      if (target === null) return undefined;
      // A stream number before the redirection is no word of the command.
      if (/^\d*$/.test(word) && !quoted) word = "";
      at += target[0].length - 1;
    } else if (char === ";" || char === "\n" || char === "|" || char === "&") {
      endCommand();
      if ((char === "|" || char === "&") && next === char) at += 1;
    } else if (/\s/.test(char) || char === "<") {
      endWord();
    } else {
      word += char;
    }
  }
  if (quote !== undefined) return undefined;
  endCommand();
  return commands;
}

/** Whether the shell command line `command` only reads or looks (LOAD_RULE): each of its simple
 *  commands (`simpleCommands`), past its variable assignments, starts with a program of
 *  LOOKING_PROGRAMS whose arguments pass that program's test, or asks any program for its help:
 *  its last word is `--help` and each word between the program and it is a plain subcommand word
 *  (no flag, no path, no text with spaces), so `bash -c 'rm -rf x' --help`, which runs the `-c`
 *  text, is not (the runner's architect, 2026-11-26T10:10:22Z). */
export function readOnlyCommand(command: string): boolean {
  const commands = simpleCommands(command);
  if (commands === undefined || commands.length === 0) return false;
  return commands.every((words) => {
    let at = 0;
    while (/^[A-Za-z_][A-Za-z0-9_]*=/.test(words[at] ?? "")) at += 1;
    const program = words[at];
    if (program === undefined) return true;
    const args = words.slice(at + 1);
    const help =
      args[args.length - 1] === "--help" &&
      args.slice(0, -1).every((word) => /^[a-z][a-z0-9-]*$/.test(word));
    return help || LOOKING_PROGRAMS[path.posix.basename(program)]?.(args) === true;
  });
}

/** The tools an agent only reads or looks with (`onlyLooks`). */
const LOOKING_TOOLS = new Set(["read", "grep", "glob", "find", "lsp", "ast_grep"]);

/** Whether a call of tool `tool` with the arguments `input` only reads or looks: a call of
 *  LOOKING_TOOLS, a `write` to the `xd://lsp` device (a query), or a `bash` call whose command
 *  only reads or looks (`readOnlyCommand`). */
function onlyLooks(tool: string, input: unknown): boolean {
  if (LOOKING_TOOLS.has(tool)) return true;
  const field = (key: string): string | undefined => {
    const value = isObject(input) ? input[key] : undefined;
    return typeof value === "string" ? value : undefined;
  };
  const command = tool === "bash" ? field("command") : undefined;
  if (command !== undefined) return readOnlyCommand(command);
  const target = tool === "write" ? field("path") : undefined;
  return target !== undefined && /^xd:\/\/lsp(\/|$)/.test(target);
}

/** What a call of a mixed reply is, beside its answerable skill reads: an act (an attempt at the
 *  case's act; in a load test, an action that starts the task), only preparation for it, or
 *  neither for certain (`callPart`). */
export type CallPart = "act" | "preparation" | "undecided";

/** The rule that sorts a mixed reply's other calls (`callPart`, `replyBoundary`): a behaviour
 *  case's act (`caseAct`), or a load test's (LOAD_RULE), whose question is whether the agent
 *  loaded the tested skill before it started the task. */
export type MixedRule = { readonly kind: "case"; readonly act: Act } | { readonly kind: "load" };

/** The load test's rule (the runner's architect, 2026-11-26T06:20:08Z): a call that only
 *  reads or looks (`onlyLooks`) is preparation; any other `bash` call, and a `write` call whose
 *  path is not known, is undecided; any other call (a `write` or `edit` of a file, a patch, a
 *  `task` dispatch, any tool whose effect is not only a read) is an action. */
export const LOAD_RULE: MixedRule = { kind: "load" };

/** What `rule` makes of a call of tool `tool` with the arguments `input` in a mixed reply (see
 *  the file's header). A case's rule: preparation when the tool is no act call's; an act when the
 *  call is an act call of its tool (its arguments the same, the intent label left out), or its
 *  tokens (`argumentTokens`) hold a distinguishing token of one; preparation when it only reads
 *  or looks (`onlyLooks`) and no act call of its tool does; else undecided. A call whose tokens
 *  the steps share is no less an attempt at the act for that (a violating step that leaves out
 *  the real one's check has no token of its own), so only what the call does makes it
 *  preparation. A load test's rule: LOAD_RULE. */
export function callPart(rule: MixedRule, tool: string, input: unknown): CallPart {
  if (rule.kind === "load") {
    if (onlyLooks(tool, input)) return "preparation";
    if (tool === "bash") return "undecided";
    if (tool === "write" && !(isObject(input) && typeof input.path === "string")) {
      return "undecided";
    }
    return "act";
  }
  const own = rule.act.calls.filter((call) => call.tool === tool);
  if (own.length === 0) return "preparation";
  const bare = withoutIntent(input);
  if (own.some((call) => Bun.deepEquals(call.input, bare, true))) return "act";
  const found = argumentTokens(input);
  if (found.some((token) => own.some((call) => call.tokens.has(token)))) return "act";
  return onlyLooks(tool, input) && !own.some((call) => call.looks) ? "preparation" : "undecided";
}

/** The text of a reply's `content`: its `text` blocks, or the text and refusal parts of its
 *  Responses `message` items, one per line. */
function replyText(content: readonly ContentBlock[]): string {
  return content
    .flatMap((block): string[] => {
      if (block.type === "text") return typeof block.text === "string" ? [block.text] : [];
      if (block.type !== "message" || !Array.isArray(block.content)) return [];
      return block.content.flatMap((part): string[] => {
        const text = isObject(part) ? (part.text ?? part.refusal) : undefined;
        return typeof text === "string" ? [text] : [];
      });
    })
    .join("\n");
}

/** What `rule` makes of a mixed reply's text: for a case whose act is the text, an act when it
 *  holds a distinguishing text token, preparation when it is blank, else undecided (a plan to
 *  answer is not the answer); undefined for any other rule, where the text decides nothing. */
function textPart(rule: MixedRule, content: readonly ContentBlock[]): CallPart | undefined {
  const own = rule.kind === "case" ? rule.act.text : undefined;
  if (own === undefined) return undefined;
  const text = replyText(content);
  if (text.trim() === "") return "preparation";
  return tokens(text).some((token) => own.has(token)) ? "act" : "undecided";
}

/** The first argument of a call's `input` but its intent label `i`, as text: a string as it is,
 *  anything else as its JSON; null when it has none. */
function firstArgument(input: unknown): string | null {
  if (!isObject(input)) return null;
  const entry = Object.entries(input).find(([key]) => key !== "i");
  if (entry === undefined) return null;
  return typeof entry[1] === "string" ? entry[1] : (JSON.stringify(entry[1]) ?? null);
}

/** A call of a mixed reply other than its answerable reads, as `replyBoundary` sorts it: its id,
 *  its tool, its first argument (`firstArgument`), and its part (`callPart`). */
export interface SortedCall {
  readonly id: string | undefined;
  readonly tool: string;
  readonly argument: string | null;
  readonly part: CallPart;
}

/** What a replay does with a reply (`replyBoundary`): `ends`, the run ends at it (no tool call,
 *  no read the replay answers, or without a rule a call that is not one); `skill round`, every
 *  call is a read it answers; or, for a mixed reply, its reads and its other calls sorted, with
 *  what its text is where the rule reads it: `acts`, one is an act, and the run ends at it;
 *  `undecided`, none is, and one is undecided; `continue`, each only prepares. */
export type Boundary =
  | { readonly kind: "ends" }
  | { readonly kind: "skill round"; readonly reads: readonly SkillRead[] }
  | {
      readonly kind: "acts" | "undecided" | "continue";
      readonly reads: readonly SkillRead[];
      readonly others: readonly SortedCall[];
      readonly text: CallPart | undefined;
    };

/**
 * What the replay of `arm` with the installed copies `copies` does with a reply whose content is
 * `content`, under `rule` (see the file's header): the one rule the replay and cli.ts's and
 * load.ts's audits of saved runs apply, with no model. Without a rule a reply with a call that is
 * not a `read` the replay answers ends the run, and no read of it is answered, so it never fails
 * on one the replay cannot answer (`otherSkillRead`'s throw). With one, each `read` of a path or
 * URL is answered where it can be, and a reply with an answered read and another call is sorted
 * by `callPart`, and for a case whose act is the text by its text. Throws as `otherSkillRead`
 * does for a read of a skill the profile lists that `copies` holds no copy of.
 */
export function replyBoundary(
  content: readonly ContentBlock[],
  arm: Arm,
  copies: readonly InstalledCopy[],
  rule?: MixedRule
): Boundary {
  const calls = toolCalls(content);
  if (calls.length === 0) return { kind: "ends" };
  const readOf = ({ id, name, input }: ToolCall) => {
    const target = isObject(input) ? input.path : undefined;
    return id !== undefined && name === "read" && typeof target === "string"
      ? { id, target }
      : undefined;
  };
  if (rule === undefined) {
    const targets = calls.map(readOf);
    if (targets.some((target) => target === undefined)) return { kind: "ends" };
    const reads: SkillRead[] = [];
    for (const target of targets) {
      const read = target && skillRead(target.id, target.target, arm, copies);
      if (read === undefined) return { kind: "ends" };
      reads.push(read);
    }
    return { kind: "skill round", reads };
  }
  const reads: SkillRead[] = [];
  const others: SortedCall[] = [];
  for (const call of calls) {
    const target = readOf(call);
    const read =
      target === undefined ? undefined : skillRead(target.id, target.target, arm, copies);
    if (read !== undefined) {
      reads.push(read);
      continue;
    }
    const tool = call.name ?? "";
    others.push({
      id: call.id,
      tool,
      argument: firstArgument(call.input),
      part: callPart(rule, tool, call.input),
    });
  }
  if (reads.length === 0) return { kind: "ends" };
  if (others.length === 0) return { kind: "skill round", reads };
  const text = textPart(rule, content);
  const parts = [...others.map((call) => call.part), ...(text === undefined ? [] : [text])];
  const kind = parts.includes("act")
    ? "acts"
    : parts.includes("undecided")
      ? "undecided"
      : "continue";
  return { kind, reads, others, text };
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
 *  (`answerCall`, the pinned Oh My Pi's artifacts numbered as the run's `artifacts` hold them, a
 *  `bash` call's sandbox showing the arm's copy of the skill where the case's reads show it,
 *  `arm.pathCopy`: checkout.ts's `CheckoutCopy.installed`, from `arm.skillDir`, none in the arm
 *  with no skill), or, where `checkout` is why the case has none, with no answer; undefined when it
 *  holds no tool call or one that is not read-only; `why` a call of it is answered in no form, when
 *  one is (see the file's header). */
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
  const copy: CheckoutCopy | Unanswered =
    "why" in checkout || arm.pathCopy === undefined
      ? checkout
      : {
          ...checkout,
          installed: {
            dir: arm.pathCopy.dir,
            ...(arm.skillDir === undefined ? {} : { source: arm.skillDir }),
          },
        };
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
    const answer = "why" in copy ? copy : await answerCall(copy, name, input, rows, artifacts);
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

/** `reads`, the skill reads of a reply's `content` a round answers, each with knives' blocks after
 *  it where they go there (`knivesRead`), from the checkout copy `checkout` and the run's
 *  `artifacts`; or why one of them is answered in no form. */
function knivesReads(
  reads: readonly SkillRead[],
  content: readonly ContentBlock[],
  checkout: CheckoutCopy | Unanswered | undefined,
  artifacts: RunArtifacts | undefined,
  responses: boolean
): SkillRead[] | Unanswered {
  const inputs = new Map(toolCalls(content).map(({ id, input }) => [id, input]));
  const given: SkillRead[] = [];
  for (const read of reads) {
    const each = knivesRead(read, inputs.get(read.id), checkout, artifacts, responses);
    if ("why" in each) return each;
    given.push(each);
  }
  return given;
}

/** `body` with `content`, a reply whose every tool call is one of `reads` (a continued mixed
 *  reply already less its other calls), and the answer to each read added, in the form of the
 *  body's API (see the file's header). */
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

/** The body every run of `body` sends first, before its session id: a Messages body with its
 *  cache breakpoint (`withCacheBreakpoint`), a Responses body as it is (see the file's header).
 *  `replayInterleaved` makes it once for all the runs of an arm. */
function sendable(body: RequestBody): RequestBody {
  return isResponsesBody(body) ? body : withCacheBreakpoint(body);
}

/** `sent` (a body as `sendable` makes it) with a run's session id, and that id: a fresh one
 *  (`freshIdentity`) when `userId` is undefined; else `userId` itself, a saved run's own, in the
 *  field `freshIdentity` draws one in (a Messages body's `metadata.user_id`, a Responses body's
 *  `prompt_cache_key`), its other fields and their order kept; and for null the body as it is,
 *  with none. */
function withIdentity(
  sent: RequestBody,
  userId?: string | null
): { readonly body: RequestBody; readonly id: string | null } {
  if (userId === undefined) return freshIdentity(sent);
  if (userId === null) return { body: sent, id: null };
  if (isResponsesBody(sent)) return { body: { ...sent, prompt_cache_key: userId }, id: userId };
  const kept = isObject(sent.metadata) ? sent.metadata : {};
  return { body: { ...sent, metadata: { ...kept, user_id: userId } }, id: userId };
}

/** The first body with the run's ids. A resume also has the saved replies to answer locally
 *  before the first send, and may limit how many further requests it sends. */
interface RunStart {
  readonly sent: RequestBody;
  readonly userId: string | null;
  readonly routingId: string | null;
  readonly saved?: SavedReply;
  readonly furtherRounds?: number;
}

/** Before any send, the saved replies cannot be rebuilt as answered rounds of this replay. */
export class NotResumable extends Error {}

/** Runs live and saved replies through the same loop. The saved replies build up the request
 *  without a send. Only replies to actual sends reach `endsAt`, and `firstSettled` is told
 *  when the first send settles. Checkout answers and knives use one artifact state per run. */
async function replaySent(
  start: RunStart,
  arm: Arm,
  gateway: Gateway,
  {
    maxSkillRounds = MAX_SKILL_ROUNDS,
    copies = [],
    headers,
    mixed,
    endsAt,
    checkout,
  }: ReplayOptions,
  firstSettled?: () => void
): Promise<Replayed> {
  const { userId, routingId } = start;
  const sendHeaders = routingId === null ? headers : { ...headers, [ROUTING_HEADER]: routingId };
  let body = start.sent;
  const usage: Spent[] = [];
  const deployments: string[] = [];
  const reads: RoundRead[][] = [];
  const leftOut: LeftOutCall[] = [];
  // The answered rounds' answered calls (their reads, and the read-only calls the checkout copy
  // answered), which the judged step holds before the reply that ends it.
  const calls: ContentBlock[] = [];
  const answeredReplies: (readonly ContentBlock[])[] = [];
  let rounds = 0;
  let checkoutRounds = 0;
  let sends = 0;
  const artifacts =
    checkout === undefined || "why" in checkout ? undefined : runArtifacts(checkout);
  const prior = start.saved?.answeredReplies ?? [];
  for (let index = 0; ; index += 1) {
    const saved =
      index < prior.length
        ? prior[index]
        : index === prior.length
          ? start.saved?.content
          : undefined;
    let reply: Reply | undefined;
    if (saved === undefined) {
      try {
        reply = await gateway.send(body, { headers: sendHeaders });
      } finally {
        if (sends === 0) firstSettled?.();
      }
      sends += 1;
      usage.push(reply.spent);
      deployments.push(deploymentOf(reply));
    }
    const content = reply?.message.content ?? saved ?? [];
    const tools = checkout === undefined ? [] : toolCalls(content);
    const readOnly =
      tools.length > 0 &&
      tools.every(({ id, name, input }) => id !== undefined && readOnlyCall(name, input));
    // A skill round comes first. Otherwise the checkout gets an all-read-only reply and the
    // mixed rule never sorts its calls.
    const boundary = replyBoundary(content, arm, copies, readOnly ? undefined : mixed);
    const skill = boundary.kind === "skill round";
    const checked = !skill && readOnly && checkout !== undefined;
    const others = boundary.kind === "continue" ? boundary.others : [];
    const replayed = (): Replayed => {
      if (reply === undefined) throw new NotResumable("a saved round could not be rebuilt");
      const step = { ...reply, message: { ...reply.message, content: [...calls, ...content] } };
      return {
        step,
        rounds,
        checkoutRounds,
        reads,
        answeredReplies,
        leftOut,
        mixedAct: boundary.kind === "acts",
        usage,
        userId,
        routingId,
        deployments,
      };
    };
    const round = skill || boundary.kind === "continue" ? boundary.reads : undefined;
    if (reply !== undefined && endsAt?.(content, round) === true) return replayed();
    if (!checked && (boundary.kind === "ends" || boundary.kind === "acts")) {
      if (reply === undefined) {
        throw new NotResumable(
          `the saved reply is not one a replay sends back: the rule makes it ${boundary.kind === "ends" ? "the end of the run" : "a mixed reply that acts"}`
        );
      }
      return replayed();
    }
    if (boundary.kind === "undecided") {
      if (reply === undefined) throw new NotResumable("the saved reply is undecided");
      throw new UndecidedReply(
        replayed(),
        boundary.reads.map(({ read }) => read),
        boundary.others.filter((call) => call.part === "undecided"),
        boundary.text === "undecided"
      );
    }

    if (!checked && rounds >= maxSkillRounds) {
      if (reply === undefined)
        throw new NotResumable("the saved reply is past the last skill round");
      throw new ReadsPastLastRound(
        replayed(),
        (round ?? []).map(({ read }) => read),
        others
      );
    }
    const responses = isResponsesBody(body);
    const answers = checked
      ? await checkoutRound(content, arm, copies, checkout, artifacts, responses)
      : knivesReads(round ?? [], content, checkout, artifacts, responses);
    if (answers === undefined || "why" in answers) {
      const why = answers?.why ?? "no checkout answers for the reply";
      if (reply === undefined) throw new NotResumable(why);
      throw new UnansweredRead(replayed(), why);
    }
    const last = answers.map(({ read }) => read);
    if (checked && checkoutRounds >= MAX_CHECKOUT_ROUNDS) {
      if (reply === undefined) throw new NotResumable("the saved reply is past the last round");
      throw new ReadsPastLastRound(replayed(), last, others);
    }
    if (reply !== undefined && start.furtherRounds !== undefined && sends >= start.furtherRounds) {
      throw new FurtherRoundLimit(replayed(), start.furtherRounds, last, others);
    }
    if (checked) checkoutRounds += 1;
    else rounds += 1;
    leftOut.push(
      ...others.map(({ tool, argument }) => ({ round: reads.length + 1, tool, argument }))
    );
    reads.push(last);
    if (checkout !== undefined) answeredReplies.push(content);
    const answeredIds = new Set(answers.map(({ id }) => id));
    const kept = content.filter(
      (block) => !isToolCallBlock(block) || answeredIds.has(toolCalls([block])[0]?.id ?? "")
    );
    calls.push(...kept.filter(isToolCallBlock));
    body = answered(body, kept, answers);
  }
}

/** Sends `body` for `arm` with a session id of its own when it names one (`freshIdentity`) and
 *  with `options.routingId` (`routingFor`), answers its skill reads, and returns the judged step.
 *  Rejects when a send does (the gateway's `BudgetSpent` among them), with ReadsPastLastRound
 *  when the reply after the last answered round still only reads (or reads skill files beside
 *  calls that only prepare), with UndecidedReply for a mixed reply `options.mixed` leaves
 *  undecided, and with UnansweredRead when a written case's reply makes a read-only call its
 *  checkout copy answers in no form, or that a case with no checkout answers none of.
 *  `firstSettled` is told once the run's first send has its reply or its failure. */
export function replayOnce(
  body: RequestBody,
  arm: Arm,
  gateway: Gateway,
  options: ReplayOptions = {},
  firstSettled?: () => void
): Promise<Replayed> {
  const own = withIdentity(sendable(body));
  const routingId = routingFor(body, options.routingId);
  return replaySent(
    { sent: own.body, userId: own.id, routingId },
    arm,
    gateway,
    options,
    firstSettled
  );
}

/** A saved reply whole, with the run's ids and any earlier answered replies saved whole too.
 *  Earlier tool calls alone cannot reconstruct the original requests. */
export interface SavedReply {
  readonly userId: string | null;
  readonly routingId: string | null;
  readonly content: readonly ContentBlock[];
  readonly answeredReplies?: readonly (readonly ContentBlock[])[];
}

/**
 * Goes on with the saved run `saved` of `body` for `arm` from its saved reply, as the live replay
 * of the run would have (`replaySent`): the body before that reply is the one `replayOnce` sends
 * first (`sendable`), with the run's own session id in place of a fresh one (`withIdentity`), and
 * every request goes with the run's own routing id (`routingFor`) and `options.headers`.
 * Any earlier saved replies and the saved reply are answered in order through that same loop:
 * skill rounds, all-read-only checkout rounds for written cases, then the mixed rule. They
 * count toward the live limits, and are included in the judged step and the recorded reads.
 * `options.furtherRounds` limits new requests only; a reply that would be sent after that many
 * stops with FurtherRoundLimit. `usage` and `deployments` are the new sends' alone.
 * Rejects as `replayOnce` does, and with NotResumable before any send for a saved reply the
 * replay cannot rebuild. `options.endsAt` is told each reply a send got: true ends the run there.
 */
export function resumeReplay(
  body: RequestBody,
  saved: SavedReply,
  arm: Arm,
  gateway: Gateway,
  {
    furtherRounds,
    ...options
  }: Omit<ReplayOptions, "routingId"> & {
    readonly furtherRounds?: number;
  } = {}
): Promise<Replayed> {
  const own = withIdentity(sendable(body), saved.userId);
  const routingId = routingFor(body, saved.routingId ?? undefined);
  return replaySent(
    {
      sent: own.body,
      userId: own.id,
      routingId,
      saved,
      ...(furtherRounds === undefined ? {} : { furtherRounds }),
    },
    arm,
    gateway,
    options
  );
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
 * kept, whether it was a step or a failure, in the order sent. `options.mixed` sorts each run's
 * mixed replies (a behaviour case's act, `caseAct`). A written case's round gives `checkout`, the
 * copy every arm's runs answer read-only calls from, or, where the case has none, why none is
 * answered.
 */
export async function replayInterleaved(
  arms: readonly ArmRuns[],
  gateway: Gateway,
  seed: string,
  options: Pick<ReplayOptions, "maxSkillRounds" | "mixed" | "checkout"> = {}
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
    byName.set(each.arm.name, { ...each, sent: sendable(each.body) });
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
    const own = withIdentity(each.sent);
    const routingId = routingFor(each.sent, each.routingIds?.[nth]);
    const replayed = replaySent(
      { sent: own.body, userId: own.id, routingId },
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
