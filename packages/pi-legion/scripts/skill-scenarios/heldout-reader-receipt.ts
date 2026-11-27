/**
 * Lane D's replay receipts, as heldout-reader.ts reads a run of --records before any call (plan v51
 * T4 lane D step 3 and T6 step 4). A run lane D replayed holds replay.json, the Go driver's
 * teardownEffectsReplayRecord (packages/daemon/internal/daemon/teardown_effects_record_test.go),
 * and, unless its verdict is HARNESS, effects.json, its teardownEffectsRecord, each in the driver's
 * own format: every list an array, and `reason` and its typed cause `fault` on a HARNESS verdict
 * alone. A receipt that is malformed, another run's, or at odds with itself or with the run's
 * outcome.json throws, and the reader refuses its batch before any call.
 *
 * What the reader is given of a run (gradedOf), and what its grade can say (dispositionOf):
 * - `ok`: its outcome and its effects, graded whole.
 * - `DIVERGED`: its record through the call of its first difference, the first request in call
 *   order that the real daemon answered otherwise than the stand-in: its actions through that
 *   call, each answer the receipt names a difference of given as the real daemon gave it, its skill
 *   reads before its first action, whether dispatch-first was injected, and its effects, which the
 *   replay made through that call alone. Nothing later: no later action, no last words, no report.
 *   The item names the seq of its last action (`observedThrough`) and nothing of why it stops. A
 *   line the reader fails there fails the run. (a) The real daemon refused the request (a status
 *   of 400 or more, a 5xx among them): the run fails where the case allows no corrected retry after
 *   that refusal (an allowed refusal of that code whose action's daemon routes name the request's
 *   op), and is otherwise unverified, since the role never saw the refusal; a remake is at most
 *   reported beside it. (b) The real daemon accepted (a status below 400) a request the stand-in
 *   refused: a record clean through that call leaves the run unverified, and one remake with the
 *   added answer named is graded whole. Two acceptances are the same answer
 *   (teardownEffectsSameAnswer), so a receipt that names them as a difference throws.
 * - `HARNESS` of the role's own write (`fault.kind` role-write: a Dispatch write the replay does
 *   not make, by the session whose legion calls the run holds, which outcome.json scores at that
 *   action and time): its whole outcome, as lane R recorded it, and no effects. A line the reader
 *   fails fails the run; otherwise the daemon state is unverified, never a pass.
 * - `HARNESS` of the harness's own records (harness-record): nothing; the run is unverified.
 * No verdict, cause, reason, difference or disposition reaches the reader. Each outcome it is given,
 * a run's whole or observed and a control's, is modelOutcome's projection: no audit record that
 * outcome.ts keeps beside the scored actions (`otherWrites`, `actor`, `change`) reaches it either.
 */
import { LEGION_WORKFLOW_PHASES } from "@legion/contracts/legion-api";
import { z } from "zod";
import type { HeldoutCase, OutcomeRecord } from "./heldout-case";
import { parseInstant } from "./production-dispatch";

const Name = z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._-]*$/, "a name");
const Text = z.string().regex(/\S/, "a text that is not blank");
const Instant = z.string().refine((text) => parseInstant(text) !== undefined, "an RFC 3339 time");
const Phase = z.enum(LEGION_WORKFLOW_PHASES);
const Seq = z.number().int().nonnegative();
const OutcomeSeq = z.number().int().positive();

/** An answer's status and refusal code, null where it names none (teardownEffectsStatus). */
const Answered = z.strictObject({ status: z.number().int(), code: z.string().nullable() });
export type Answered = z.infer<typeof Answered>;

/** One run's effect record, effects.json (teardownEffectsRecord): every answer the real daemon and
 * the client gave in the replay, in its order; the issue's phase before and after; the production
 * check's next look; the handoff records named; and each request the real daemon answered otherwise
 * than the stand-in, in call order: `seq`, its answer's place among `answers`, its `route`, `call`,
 * the original legion call's tool call id, and `outcomeSeq`, the outcome.json action of the
 * stand-in's answer. */
export const EffectRecord = z.strictObject({
  id: z.uuid(),
  answers: z.array(
    z.strictObject({
      seq: Seq,
      at: Instant,
      by: z.enum(["daemon", "client"]),
      op: Name,
      status: z.number().int(),
      code: z.string().nullable(),
      body: z.json(),
    })
  ),
  phaseBefore: Phase,
  phaseAfter: Phase,
  productionCheck: z
    .strictObject({
      wakeAt: Instant.nullable(),
      checkWakeRow: z.strictObject({ dueAt: Instant }).nullable(),
    })
    .nullable(),
  handoffRecords: z.array(
    z.strictObject({ phase: Name, version: z.number().int().positive(), id: Text })
  ),
  diverged: z.array(
    z.strictObject({
      seq: Seq,
      route: Text,
      call: Text,
      outcomeSeq: OutcomeSeq,
      standin: Answered,
      daemon: Answered,
    })
  ),
});
export type EffectRecord = z.infer<typeof EffectRecord>;
/** A request the real daemon answered otherwise than the stand-in. */
export type Difference = EffectRecord["diverged"][number];

/** A recorded change the replay applied before a call, or that followed the last one
 * (teardownEffectsChange): `outcomeSeq` is absent for a seeded record or a move of the workspace. */
const Change = z.strictObject({
  kind: z.enum(["push", "github", "dispatch", "workspace"]),
  source: Text,
  what: Text,
  at: Instant,
  outcomeSeq: OutcomeSeq.optional(),
});

/** One answer to a call's request (teardownEffectsAnswerRef): the stand-in's, with the outcome.json
 * action that records it where one does, or the real daemon's, with its place among effects.json's
 * answers. */
const AnswerRef = z.strictObject({
  outcomeSeq: OutcomeSeq.optional(),
  seq: Seq.optional(),
  route: Text,
  status: z.number().int(),
  code: z.string().nullable(),
});
type AnswerRef = z.infer<typeof AnswerRef>;

const ClientAnswer = z.strictObject({ isError: z.boolean(), text: z.string() });

/** One legion call the replay made again (teardownEffectsReplayCall). */
const ReplayCall = z.strictObject({
  call: Text,
  op: z.string(),
  at: Instant,
  before: z.array(Change),
  state: z.strictObject({
    phase: Phase,
    tree: z.string(),
    treeRuns: z.boolean(),
    parent: z.string(),
    standing: z.string(),
    branch: z.string(),
    pulls: z.array(
      z.strictObject({ number: z.number().int(), state: z.string(), head: z.string() })
    ),
  }),
  standin: z.array(AnswerRef),
  daemon: z.array(AnswerRef),
  client: z.strictObject({ run: ClientAnswer, replay: ClientAnswer }),
  phaseAfter: Phase,
});

/** The typed cause of a HARNESS verdict (teardownEffectsFault). */
const Fault = z.discriminatedUnion("kind", [
  z.strictObject({
    kind: z.literal("role-write"),
    source: Text,
    event: Text,
    at: Instant,
    outcomeSeq: OutcomeSeq,
    author: z.strictObject({ kind: z.literal("session"), id: Text }),
    session: Text,
    call: Text,
  }),
  z.strictObject({ kind: z.literal("harness-record") }),
]);

/** replay.json (teardownEffectsReplayRecord). `end` is each change the replay made after its last
 * call: a control's trace makes every action it holds, and a run's replay makes none, so a receipt
 * the driver wrote before it held the list holds none. */
export const ReplayRecord = z.strictObject({
  id: z.uuid(),
  verdict: z.enum(["ok", "DIVERGED", "HARNESS"]),
  reason: Text.optional(),
  fault: Fault.optional(),
  seed: z.array(Change),
  calls: z.array(ReplayCall),
  end: z.array(Change).optional(),
  after: z.array(Change),
  notReplayed: z.array(Text),
});
export type ReplayRecord = z.infer<typeof ReplayRecord>;

function parsed<T>(schema: z.ZodType<T>, text: string): T {
  const result = schema.safeParse(JSON.parse(text));
  if (!result.success) throw new Error(z.prettifyError(result.error).replaceAll("\n", " "));
  return result.data;
}

/** A replay.json; throws naming each part that is not the Go driver's format. */
export function parseReplay(text: string): ReplayRecord {
  return parsed(ReplayRecord, text);
}

/** An effects.json; throws naming each part that is not the Go driver's format. */
export function parseEffects(text: string): EffectRecord {
  return parsed(EffectRecord, text);
}

/** One scored action of an outcome record. */
type Action = OutcomeRecord["actions"][number];
/** An action's audit fields, which outcome.json keeps and no reader is given: a Dispatch write's
 * writer (`actor`) and the change gh-standin kept of a GitHub call (`change`). */
type Audit = "actor" | "change";
/** Each kind of `T` without its audit fields. */
type Scored<T> = T extends unknown ? Omit<T, Audit> : never;
/** A scored action as the reader is given it: every field but its audit fields (modelOutcome). */
export type ItemAction = Scored<Action>;
/** An outcome as the reader is given it (modelOutcome): no id, no other actors' writes, no action's
 * audit fields, and no last words where its record stops short. */
export type ItemOutcome = Omit<OutcomeRecord, "id" | "lastText" | "otherWrites" | "actions"> & {
  readonly actions: readonly ItemAction[];
  readonly lastText?: string;
};
/** Effects as the reader is given them: no id, and no differences where the record stops short. */
export type ItemEffects = Omit<EffectRecord, "id" | "diverged"> & {
  readonly diverged?: EffectRecord["diverged"];
};

/** A DIVERGED run's record through the call of its first difference. */
export interface Observed {
  /** The seq of the last action it holds: the last of that call's requests outcome.json scores. */
  readonly through: number;
  readonly outcome: Omit<OutcomeRecord, "id" | "lastText" | "agentReport">;
  readonly effects: Omit<EffectRecord, "id" | "diverged">;
}

/** What lane D's receipt says of a run, as the reader classified it before any call. */
export type Replay =
  | { readonly verdict: "ok"; readonly effects: EffectRecord }
  | {
      readonly verdict: "DIVERGED";
      readonly first: Difference;
      /** (a) the real daemon refused the request; (b) it accepted one the stand-in refused. */
      readonly branch: "refused" | "accepted";
      /** Whether the case allows a corrected retry after the real refusal; null for (b). */
      readonly retry: "allowed" | "not-allowed" | null;
      readonly observed: Observed;
    }
  | {
      readonly verdict: "HARNESS";
      readonly cause: "role-write";
      readonly reason: string;
      readonly write: {
        readonly event: string;
        readonly outcomeSeq: number;
        readonly call: string;
      };
    }
  | { readonly verdict: "HARNESS"; readonly cause: "harness-record"; readonly reason: string };

/** A daemon action of outcome.json. */
type DaemonAction = Extract<OutcomeRecord["actions"][number], { kind: "daemon" }>;

/** The part of a case a receipt is read against: its actions, and the refusals after which a
 * corrected retry may pass. */
export type CaseRules = Pick<HeldoutCase, "actions" | "allowedRefusals">;

/** The Legion reads of the daemon (teardownEffectsReadOp), which a replay need not pair. */
const DAEMON_READS: readonly string[] = ["read_record", "read_state"];

/** An answer's status and code as a sentence names them. */
function statusText(answer: Answered): string {
  return answer.code === null ? String(answer.status) : `${answer.status} ${answer.code}`;
}

/** The code outcome.json records for an answer: its own, or HTTP_<status> for a refusal that
 * names none (outcome.ts daemonAnswer). */
function outcomeCode(answer: Answered): string | null {
  return answer.code ?? (answer.status >= 400 ? `HTTP_${answer.status}` : null);
}

/** Whether a client answer's body is the tool's answer `said`, as teardownEffectsRun.call writes
 * it: its text and whether it is an error, and nothing else. */
function saidBy(body: unknown, said: { readonly isError: boolean; readonly text: string }) {
  if (typeof body !== "object" || body === null || Array.isArray(body)) return false;
  const { isError, text, ...rest } = body as Record<string, unknown>;
  return isError === said.isError && text === said.text && Object.keys(rest).length === 0;
}

/**
 * What lane D's receipt says of `outcome`'s run: replay.json (`receipt`), and effects.json
 * (`effects`, null where the run holds none), read against the run's outcome and its case's
 * `rules`. Throws where either is another run's, where the two disagree with each other or with
 * the outcome, and where a DIVERGED run's first difference is two acceptances, which lane D never
 * records as a difference.
 */
export function replayOf(
  outcome: OutcomeRecord,
  receipt: ReplayRecord,
  effects: EffectRecord | null,
  rules: CaseRules
): Replay {
  if (receipt.id !== outcome.id)
    throw new Error(`replay.json is the receipt of ${receipt.id}, not of ${outcome.id}`);
  if (effects !== null && effects.id !== outcome.id)
    throw new Error(`effects.json is the record of ${effects.id}, not of ${outcome.id}`);
  if (receipt.verdict === "HARNESS") return harnessOf(outcome, receipt, effects);
  if (receipt.reason !== undefined || receipt.fault !== undefined)
    throw new Error(
      `replay.json is ${receipt.verdict} and names a reason or a fault, which only a HARNESS receipt names`
    );
  if (effects === null)
    throw new Error(
      `replay.json is ${receipt.verdict}, and the run holds no effects.json, which lane D writes beside every replay that is not HARNESS`
    );
  agree(outcome, receipt, effects);
  if (receipt.verdict === "ok") {
    if (effects.diverged.length > 0 || receipt.notReplayed.length > 0)
      throw new Error(
        "replay.json is ok, and effects.json names a difference or replay.json a call it did not replay"
      );
    covered(outcome, receipt, Number.POSITIVE_INFINITY);
    return { verdict: "ok", effects };
  }
  return divergedOf(outcome, receipt, effects, rules);
}

/** A HARNESS receipt's cause. The role's write is taken only where the receipt types it so and
 * outcome.json holds that write: its author is the role's session, the replay stopped before any
 * record, and the outcome scores a Dispatch write at that action and time. */
function harnessOf(
  outcome: OutcomeRecord,
  receipt: ReplayRecord,
  effects: EffectRecord | null
): Replay {
  const { reason, fault } = receipt;
  if (effects !== null)
    throw new Error(
      "replay.json is HARNESS, and the run holds an effects.json, which lane D writes for no HARNESS replay"
    );
  if (reason === undefined || fault === undefined)
    throw new Error("replay.json is HARNESS and names no reason or no typed fault");
  if (fault.kind === "harness-record") return { verdict: "HARNESS", cause: fault.kind, reason };
  const scored = outcome.actions.find((action) => action.seq === fault.outcomeSeq);
  const replayed = [
    receipt.seed,
    receipt.calls,
    receipt.end ?? [],
    receipt.after,
    receipt.notReplayed,
  ];
  let problem: string | undefined;
  if (fault.author.id !== fault.session)
    problem = `its author ${fault.author.id} is not the role's session ${fault.session}`;
  else if (replayed.some((list) => list.length > 0))
    problem = "it lists replayed records, and a role's write stops the replay before any";
  else if (scored?.kind !== "dispatch" || parseInstant(scored.at) !== parseInstant(fault.at))
    problem = `outcome.json holds no Dispatch write as action ${fault.outcomeSeq} at ${fault.at}`;
  if (problem !== undefined)
    throw new Error(
      `replay.json types its HARNESS verdict as the role's ${fault.event} write (${fault.source}), and ${problem}`
    );
  const write = { event: fault.event, outcomeSeq: fault.outcomeSeq, call: fault.call };
  return { verdict: "HARNESS", cause: fault.kind, reason, write };
}

/** Throws where effects.json's answers are not the ones replay.json's calls name, in their order
 * (teardownEffectsRun.call: each answer of the real daemon, then the tool's), or where a stand-in
 * answer replay.json joins to an outcome.json action is not that daemon action's answer. */
function agree(outcome: OutcomeRecord, receipt: ReplayRecord, effects: EffectRecord): void {
  const disagree = (why: string) => new Error(`effects.json and replay.json disagree: ${why}`);
  let at = 0;
  for (const call of receipt.calls) {
    const when = parseInstant(call.at);
    for (const ref of call.daemon) {
      const answer = effects.answers[at];
      if (
        ref.seq !== at ||
        ref.outcomeSeq !== undefined ||
        answer === undefined ||
        answer.seq !== at ||
        answer.by !== "daemon" ||
        answer.op !== call.op ||
        parseInstant(answer.at) !== when ||
        answer.status !== ref.status ||
        answer.code !== ref.code
      )
        throw disagree(
          `call ${call.call}'s answer ${ref.route} ${statusText(ref)} is not answer ${at}`
        );
      at += 1;
    }
    const client = effects.answers[at];
    if (
      client === undefined ||
      client.seq !== at ||
      client.by !== "client" ||
      client.op !== call.op ||
      parseInstant(client.at) !== when ||
      client.status !== 0 ||
      client.code !== null ||
      !saidBy(client.body, call.client.replay)
    )
      throw disagree(`call ${call.call}'s tool answer is not answer ${at}`);
    at += 1;
    for (const ref of call.standin) {
      if (ref.seq !== undefined)
        throw disagree(`call ${call.call}'s stand-in answer ${ref.route} names a real answer`);
      if (ref.outcomeSeq === undefined) continue;
      const action = outcome.actions.find((each) => each.seq === ref.outcomeSeq);
      if (action?.kind !== "daemon" || action.answer.code !== outcomeCode(ref))
        throw new Error(
          `replay.json joins call ${call.call}'s stand-in answer ${ref.route} ${statusText(ref)} to outcome.json action ${ref.outcomeSeq}, which records no such daemon answer`
        );
    }
  }
  if (at !== effects.answers.length)
    throw disagree(`effects.json holds ${effects.answers.length} answers, and the calls ${at}`);
}

/** Throws where a daemon write of outcome.json at or before `through` is the stand-in's answer to
 * no call replay.json replayed: the replay missed a request the run made. */
function covered(outcome: OutcomeRecord, receipt: ReplayRecord, through: number): void {
  const joined = new Set(
    receipt.calls.flatMap((call) => call.standin.flatMap((ref) => ref.outcomeSeq ?? []))
  );
  const missed = outcome.actions
    .filter((action) => action.kind === "daemon" && !DAEMON_READS.includes(action.op))
    .filter((action) => action.seq <= through && !joined.has(action.seq))
    .map((action) => action.seq);
  if (missed.length > 0)
    throw new Error(
      `replay.json replayed no call that sent outcome.json's daemon write ${missed.join(", ")}`
    );
}

/** A DIVERGED receipt: its first difference, its branch, and the record through that call. */
function divergedOf(
  outcome: OutcomeRecord,
  receipt: ReplayRecord,
  effects: EffectRecord,
  rules: CaseRules
): Replay {
  const [first] = effects.diverged;
  const last = receipt.calls.at(-1);
  if (first === undefined || last === undefined)
    throw new Error(
      "replay.json is DIVERGED, and effects.json names no difference or replay.json no call"
    );
  effects.diverged.forEach((difference, index) => {
    const before = effects.diverged[index - 1];
    const daemon = last.daemon.find((ref) => ref.seq === difference.seq);
    const standin = last.standin.find((ref) => ref.outcomeSeq === difference.outcomeSeq);
    const where = `effects.json's difference at answer ${difference.seq} (${difference.route})`;
    if (before !== undefined && difference.seq <= before.seq)
      throw new Error(`${where} does not follow the one before it in call order`);
    if (difference.call !== last.call)
      throw new Error(
        `${where} names call ${difference.call}, and the last call replay.json replayed is ${last.call}`
      );
    const joins = (ref: AnswerRef | undefined, answer: Answered) =>
      ref !== undefined &&
      ref.route === difference.route &&
      ref.status === answer.status &&
      ref.code === answer.code;
    if (!joins(daemon, difference.daemon) || !joins(standin, difference.standin))
      throw new Error(
        `${where} is not the pair of answers replay.json gives call ${last.call}: the stand-in's ${statusText(difference.standin)} and the real daemon's ${statusText(difference.daemon)}`
      );
  });
  let branch: "refused" | "accepted" | undefined;
  if (first.daemon.status >= 400) branch = "refused";
  else if (first.standin.status >= 400) branch = "accepted";
  if (branch === undefined)
    throw new Error(
      `the first difference, ${first.route} of call ${first.call}, is the stand-in's ${statusText(first.standin)} against the real daemon's ${statusText(first.daemon)}: two acceptances, which lane D records as the same answer and never as a difference`
    );
  const through = Math.max(...last.standin.map((ref) => ref.outcomeSeq ?? 0));
  covered(outcome, receipt, through);
  const real = new Map<number, DaemonAction["answer"]>();
  for (const difference of effects.diverged) {
    const body = effects.answers[difference.seq]?.body ?? null;
    real.set(difference.outcomeSeq, { code: outcomeCode(difference.daemon), body });
  }
  const actions: Observed["outcome"]["actions"] = [];
  for (const action of outcome.actions) {
    if (action.seq > through) break;
    const answer = action.kind === "daemon" ? real.get(action.seq) : undefined;
    if (action.kind === "daemon" && answer !== undefined) actions.push({ ...action, answer });
    else actions.push(action);
  }
  const request = outcome.actions.find((action) => action.seq === first.outcomeSeq);
  const op = request?.kind === "daemon" ? request.op : "";
  const { id: _id, diverged: _diverged, ...observedEffects } = effects;
  return {
    verdict: "DIVERGED",
    first,
    branch,
    retry: branch === "refused" ? retryOf(rules, op, outcomeCode(first.daemon)) : null,
    observed: {
      through,
      outcome: {
        actions,
        skillReads: outcome.skillReads.filter((read) => read.beforeFirstAction),
        dispatchFirstInjected: outcome.dispatchFirstInjected,
      },
      effects: observedEffects,
    },
  };
}

/** Whether the case allows a corrected retry after a real refusal of `code` to a request of `op`:
 * an allowed refusal of that code whose action's daemon routes name that op. Throws where such a
 * refusal's action has a daemon route that names no op, since whether the request is that action
 * cannot then be told. */
function retryOf(rules: CaseRules, op: string, code: string | null): "allowed" | "not-allowed" {
  for (const allowed of rules.allowedRefusals) {
    if (allowed.code !== code) continue;
    const routes = rules.actions.find((action) => action.name === allowed.action)?.routes ?? [];
    const ops = routes.filter((route) => route.kind === "daemon").map((route) => route.match.op);
    if (ops.some((each) => typeof each !== "string"))
      throw new Error(
        `the case allows a corrected retry after ${code} of its action ${allowed.action}, a daemon route of which names no op, so whether the refused ${op} request is that action cannot be told`
      );
    if (ops.includes(op)) return "allowed";
  }
  return "not-allowed";
}

/** What the reader is given of a run, with lane D's `replay` of it (null for a lane R run). */
export interface Graded {
  readonly outcome: ItemOutcome;
  readonly effects: ItemEffects | null;
  /** The seq of the last action of a record that stops short; absent where the record is whole. */
  readonly observedThrough?: number;
}

/** A scored action without its audit fields, every other field in its place. */
function scoredAction(action: Action): ItemAction {
  if (action.kind === "dispatch") {
    const { actor: _actor, ...scored } = action;
    return scored;
  }
  if (action.kind === "github") {
    const { change: _change, ...scored } = action;
    return scored;
  }
  return action;
}

/**
 * The one projection of an outcome that the reader is given, whole or through its first
 * difference's call, of a run and of a control alike: its scored actions and what the run read,
 * without `otherWrites` and without each action's `actor` and `change`. Those audit records stay in
 * outcome.json, and a frozen trace holds none of them, so they cannot tell a run from a control.
 * Every other key keeps its place, so an outcome that holds none of them is given byte for byte as
 * it was before outcome.ts kept them.
 */
export function modelOutcome(
  outcome: Omit<OutcomeRecord, "id" | "lastText"> & { readonly lastText?: string }
): ItemOutcome {
  const { otherWrites: _otherWrites, ...kept } = outcome;
  return { ...kept, actions: outcome.actions.map(scoredAction) };
}

/** What the reader is given of a run, or undefined where it is given nothing of it: its outcome
 * through modelOutcome, whole or observed, and its effects. */
export function gradedOf(outcome: OutcomeRecord, replay: Replay | null): Graded | undefined {
  const { id: _id, ...whole } = outcome;
  if (replay === null) return { outcome: modelOutcome(whole), effects: null };
  switch (replay.verdict) {
    case "ok": {
      const { id: _effectsId, ...effects } = replay.effects;
      return { outcome: modelOutcome(whole), effects };
    }
    case "DIVERGED": {
      const { outcome: observed, effects, through } = replay.observed;
      return { outcome: modelOutcome(observed), effects, observedThrough: through };
    }
    case "HARNESS":
      return replay.cause === "role-write"
        ? { outcome: modelOutcome(whole), effects: null }
        : undefined;
  }
}

/** One run's grade by its own id, before any join to arms. */
export interface Disposition {
  readonly id: string;
  readonly case: string;
  /** `R`: a plain session or a skill-use run, which lane D does not replay; `D`: lane D's run. */
  readonly lane: "R" | "D";
  /** Lane D's verdict; null in lane R. */
  readonly replay: "ok" | "DIVERGED" | "HARNESS" | null;
  /** What the reader graded: the outcome and its effects, the outcome alone (lane R's record),
   * the record through the first difference's call, or nothing. */
  readonly graded: "outcome-and-effects" | "outcome" | "observed" | "none";
  /** `fail` is a behavioral failure the record shows; `unverified` is never a pass or a failure. */
  readonly status: "pass" | "fail" | "unverified";
  readonly why: string;
  /** `none`; `beside`: at most one remake, reported beside the run and never in its place;
   * `whole`: one remake with the added answer named, graded whole, which grades the run. */
  readonly remake: "none" | "beside" | "whole";
  /** A HARNESS run's typed cause and the reason replay.json gives. */
  readonly harness?: { readonly cause: "role-write" | "harness-record"; readonly reason: string };
  /** A DIVERGED run's first difference, its branch, and where its record stops. */
  readonly difference?: {
    readonly call: string;
    readonly outcomeSeq: number;
    readonly seq: number;
    readonly route: string;
    readonly standin: Answered;
    readonly daemon: Answered;
    readonly branch: "refused" | "accepted";
    readonly retry: "allowed" | "not-allowed" | null;
    readonly observedThrough: number;
  };
}

/** A run as dispositionOf reads it. */
interface DisposedRun {
  readonly case: string;
  readonly outcome: OutcomeRecord;
  readonly replay: Replay | null;
}

/** The reader's grade of a run's lines; a line it left null was not shown either way. */
interface LineGrades {
  readonly lines: readonly { readonly name: string; readonly pass: boolean | null }[];
}

/**
 * The grade of `run`, from the reader's `label` of it (undefined for a run it was not given) in a
 * batch whose controls make it `batch`. Every line the reader failed fails the run; a line it left
 * null never passes it. In an INCONCLUSIVE batch the labels grade no run: each is unverified.
 */
export function dispositionOf(
  run: DisposedRun,
  label: LineGrades | undefined,
  batch: "CONCLUSIVE" | "INCONCLUSIVE"
): Disposition {
  const { replay } = run;
  const base: Pick<Disposition, "id" | "case" | "lane" | "replay"> = {
    id: run.outcome.id,
    case: run.case,
    lane: replay === null ? "R" : "D",
    replay: replay?.verdict ?? null,
  };
  if (replay?.verdict === "HARNESS" && replay.cause === "harness-record")
    return {
      ...base,
      graded: "none",
      status: "unverified",
      why: "lane D's own records made the replay HARNESS, so the run has no behavioral grade",
      remake: "beside",
      harness: { cause: replay.cause, reason: replay.reason },
    };
  if (label === undefined) throw new Error(`the labels grade no line of ${run.outcome.id}`);
  const failed = label.lines.filter((line) => line.pass === false).map((line) => line.name);
  const fails = `the reader failed ${failed.join(", ")}`;
  const conclusive = batch === "CONCLUSIVE";
  const inconclusive = "the batch is INCONCLUSIVE, so its labels grade no run";
  if (replay === null || replay.verdict === "ok") {
    const graded = replay === null ? "outcome" : "outcome-and-effects";
    if (!conclusive)
      return { ...base, graded, status: "unverified", why: inconclusive, remake: "none" };
    return label.lines.every((line) => line.pass === true)
      ? { ...base, graded, status: "pass", why: "every line passed", remake: "none" }
      : { ...base, graded, status: "fail", why: fails, remake: "none" };
  }
  if (replay.verdict === "HARNESS") {
    const harness = { cause: replay.cause, reason: replay.reason };
    const daemon = `lane D did not replay the role's ${replay.write.event} write (outcome.json action ${replay.write.outcomeSeq}), so the daemon state is unverified`;
    let status: Disposition["status"] = "unverified";
    let why = `${inconclusive}; ${daemon}`;
    if (conclusive && failed.length > 0) {
      status = "fail";
      why = `${fails} on outcome.json, the run's lane R record`;
    } else if (conclusive) {
      why = `no line failed on outcome.json; ${daemon}`;
    }
    return { ...base, graded: "outcome", status, why, remake: "beside", harness };
  }
  const { first, branch, retry, observed } = replay;
  const difference = {
    call: first.call,
    outcomeSeq: first.outcomeSeq,
    seq: first.seq,
    route: first.route,
    standin: first.standin,
    daemon: first.daemon,
    branch,
    retry,
    observedThrough: observed.through,
  };
  const record = `the record through call ${first.call} (action ${observed.through})`;
  const refused = `the real daemon refused ${first.route} with ${statusText(first.daemon)} (outcome.json action ${first.outcomeSeq})`;
  let status: Disposition["status"] = "unverified";
  let why = inconclusive;
  let remake: Disposition["remake"] = branch === "refused" ? "beside" : "none";
  if (conclusive && failed.length > 0) {
    status = "fail";
    why = `${fails} on ${record}`;
  } else if (conclusive && branch === "refused" && retry === "not-allowed") {
    status = "fail";
    why = `${refused}, and the case allows no corrected retry after it`;
  } else if (conclusive && branch === "refused") {
    why = `no line failed on ${record}; ${refused}, after which the case allows a corrected retry, and the role never saw it`;
  } else if (conclusive) {
    why = `no line failed on ${record}, whose request the real daemon accepted and the stand-in refused (${statusText(first.standin)}): one remake with the added answer named, graded whole, grades the run`;
    remake = "whole";
  }
  return { ...base, graded: "observed", status, why, remake, difference };
}
