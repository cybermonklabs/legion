import type { LegionOwed } from "@legion/contracts/legion-api";
import { lastCustomEntry } from "./custom-entry";
import { LOCAL_ENVOY_NOTICE } from "./envoy-notice";

/**
 * The phase-stall check (LEGION-208 Stage 4b, task 4b.15; made endless by INFRA-220). A phase
 * worker whose turn ends with work it owes still undone, without the `legion` tool's
 * `handoff_complete` having succeeded, would otherwise sit idle while nothing wakes anyone: in
 * Stage 3 at 1ee62d31 the round-2 implementer wrote its final `legion handoff complete` as text
 * instead of a tool call, so it never ran, and two trees later waited six hours each on a worker
 * that had had its one follow-up. So the worker is reminded at the end of every turn for as long
 * as the daemon answers that its claim owes work (`POST /legion/v1/claims/owed`), except in the
 * one state the daemon itself ends: the wait a `ready_for_update` answer creates
 * (`awaiting-update`), where the daemon is what wakes the worker rather than a reminder.
 * `extensions/legion.ts` feeds this state machine from the session's own hooks and
 * `src/phase-reminder.ts` delivers each reminder: the first of a run from `session_stop`, Oh My
 * Pi's hook for a top-level run about to settle, which the host turns into one more turn in the
 * same session, and every one after it from a timer, so the run settles and the daemon sees the
 * turn end. When `LEGION_STALL_ARCHITECT_AFTER` reminders in a row change nothing, the session
 * tells the architect over Envoy.
 *
 * The machine keeps the reminder's rhythm and nothing of the issue's phase: whether
 * a settle is reminded at all is the daemon's answer alone, asked at that settle. A process that
 * restarts, or a pod that is replaced, therefore changes nothing about whether a role is
 * reminded: it holds no state a restart could reset, and a message from outside the session
 * (news or a daemon prompt) moves the rhythm and nothing else, so the daemon's own update
 * message reaching a role whose phase has finished reminds it of nothing (dispatch://INFRA-2746).
 *
 * The same answer says whether a wait the worker declared with the `legion` tool's
 * `declare_wait` stands, and whether the closed design gate holds a merger
 * (dispatch://INFRA-2040). While either stands, the reminder sends nothing but the one for a tool
 * call written as text (`quietReminderText`). Only the worker's own background work keeps the
 * quiet wait (`waitingMs`).
 */

/** Where the reminder's rhythm stands:
 * - `active`: a turn that settles now earns a reminder, if the daemon answers that the claim
 *   owes work;
 * - `waiting`: the worker's turn ended with background work of its own still running, which buys
 *   the longer quiet wait `LEGION_STALL_WAITING_MS` before the next reminder;
 * - `awaiting-update`: the worker has answered the daemon's update message (`ready_for_update`),
 *   so it is to start nothing until the daemon comes back to it. This wait earns no reminder and
 *   no wait of any length, because the daemon is what wakes the worker: a round, once opened,
 *   ends only by replacing its pod or because that pod is gone, so the process is replaced and the
 *   session comes back on the new pod with the task the round held behind it (`endPodUpdate` in
 *   `packages/daemon/internal/supervise/table.go`). That task arrives as the daemon's own prompt
 *   (`prompt`), which makes the rhythm `active` again. A reminder steered into
 *   this wait would instead start a turn of work the replacement throws away, and before the
 *   round's bound a turn in progress holds that replacement back (INFRA-484).
 * The reminding never stops where a reminder is the worker's only wake: one that changes nothing
 * is followed by another, for as long as the daemon answers that the work is owed. */
export type PhaseStall = "active" | "waiting" | "awaiting-update";

/** The state and how many reminders the worker has done nothing about: `count` climbs by one on
 * every reminder the host actually put in front of the worker and drops to 0 as soon as the worker,
 * or the daemon, does anything at all. */
export interface PhaseStallState {
  readonly state: PhaseStall;
  readonly count: number;
}

export type PhaseStallInput =
  /** News reached the session from outside it: an Envoy delivery, a message to the role, a
   * person's direct message. It says nothing of what the claim owes, which is the daemon's answer
   * at the next settle; it is something happening, so the count starts again. It is also the one
   * input that ends a wait the worker declared: the reminder reports it to the daemon
   * (`PhaseReminder.news` in `src/phase-reminder.ts`). */
  | { readonly kind: "news" }
  /** A prompt of the daemon's reached the session: a task, the resume task, the update task, the
   * wait's own task. It starts the count again as news does, and ends no wait the worker
   * declared. */
  | { readonly kind: "prompt" }
  /** The `legion` tool's `ready_for_update` returned without throwing: the worker has pushed
   * what it wants kept and answered the daemon's update message on its pod (INFRA-484). */
  | { readonly kind: "update-answered" }
  /** The settling assistant message ran at least one real tool call (`assistantRanTool`). */
  | { readonly kind: "tool-call" }
  | {
      readonly kind: "settle";
      readonly lastAssistantText: string;
      /**
       * The session ends this turn with background work of its own still running, which will wake
       * it when it finishes. Only a host whose turn can end that way sets it: Oh My
       * Pi holds `session_stop` back until its own async jobs are idle, so its settle never has
       * any, while Claude Code's `Stop` hook fires with the running tasks in its payload
       * (`background_tasks`, measured on 2.1.310).
       */
      readonly backgroundWork?: boolean | undefined;
    }
  /** The turn ended because the provider refused it, not because the worker had finished saying
   * something: `message` is the provider's own words for the refusal (Oh My Pi's `errorMessage`
   * beside an `error` stop, Claude Code's `StopFailure` error text). The worker never read the
   * last reminder, so this earns no reminder of its own: the runner waits out its delay and
   * reminds once (`src/phase-reminder.ts`). */
  | { readonly kind: "refused"; readonly message: string }
  /** The host has just delivered a reminder to the worker. */
  | { readonly kind: "reminded" };

/** How one turn ended, which is the whole of what the host classifies for `PhaseReminder.settle`:
 * the worker's own settling words, or the provider's refusal of the turn. */
export type TurnEnd = Extract<PhaseStallInput, { kind: "settle" | "refused" }>;

export interface PhaseStallStep {
  readonly state: PhaseStallState;
  /** The settle has earned a reminder now, which goes out if the daemon answers that the claim
   * owes work (`src/phase-reminder.ts`). */
  readonly remind?: true;
}

/** The custom transcript entry (`pi.appendEntry`) holding the state after each change, so a worker
 * the daemon relaunches with `--resume` restores it from its branch. */
export const PHASE_STALL_ENTRY = "legion-phase-stall";

/** The opening of a tool call written as text in the Anthropic invoke markup, with or without a
 * namespace prefix: the shape the Stage 3 model emitted in place of a `tool_use` block. */
const TOOL_CALL_SHAPED = /<(?:[\w-]+:)?(?:function_calls\b|invoke\s+name\s*=|parameter\s+name\s*=)/;

/** What the reminder tells a worker that waits on something outside its own work: declare the
 * wait with the `legion` tool's `declare_wait`, and what ends it. A message to the worker ends the
 * wait and wakes the worker. The update round's message ends no wait (D9 of dispatch://INFRA-2040's
 * plan), as `declare_wait`'s own sentence says (`PHASE_OP_SENTENCES` in `./legion-phase-ops`). A
 * message whose wake is lost waits until the wait's time. */
const DECLARE_WAIT =
  "If you are waiting on something outside your own work (CI, a review, a person), call the " +
  "`legion` tool with op `declare_wait`, `waiting_on` naming what you wait on and `wake_at` the " +
  "RFC 3339 time to wake you, then end your turn. A message to you ends the wait and wakes you. " +
  "The update round's message does not end the wait. A message whose wake is lost waits until " +
  "the wait's time.";

/** What a worker whose turn ended with `work` owed is told, `work` being the daemon's own name for
 * it (`LegionOwed.work`: "the testing phase of LEGION-208", "the production check of
 * LEGION-208"). Both are finished by the `legion` tool's `handoff_complete`. This is the one copy,
 * read by both hosts: the Claude Code bridge kept its own (`packages/daemon/internal/claudebridge/
 * stall.go`, deleted with INFRA-220) until the bridge became pure transport. */
export function owedReminder(work: string): string {
  return (
    `Your turn ended with ${work} still owed: the Legion daemon's record holds it open for your ` +
    "role. If that work is done, call the `legion` tool now with op `handoff_complete` and the " +
    `fields your role's instructions give. ${DECLARE_WAIT} No reminder comes while the wait ` +
    "stands. This reminder repeats at the end of every turn for as long as the daemon's record " +
    "holds that work open and no wait stands."
  );
}

/** What a worker is told when the daemon's answer did not arrive: an error, a timeout, a refusal,
 * or an answer the contract does not describe. It still nudges, since an unreachable daemon is
 * exactly when a stalled worker must still be nudged, and it says that it could not read the
 * record. It names no phase and no operation that ends one, because it read neither: the one
 * operation it names is `declare_wait`, which a worker that waits needs whatever the record says. */
export const UNREAD_REMINDER =
  "Your turn ended, and this reminder could not read the Legion daemon's record of what your " +
  "role owes, so it cannot say whether any work is still yours. If your role's instructions " +
  `leave work of yours unfinished, finish it now with a real tool call. ${DECLARE_WAIT} This ` +
  "reminder repeats at the end of every turn for as long as the record cannot be read.";

const TEXT_TOOL_CALL =
  "Your last message holds a tool call written as text, so it did not run: make it a real tool call. ";

/** The reminder a settling message earns, given what the daemon answered: `owedReminder` for the
 * `work` it named, or `UNREAD_REMINDER` when `work` is undefined because no answer arrived. Either
 * is led by `TEXT_TOOL_CALL` when that message holds a tool call written as text, which is the
 * step the worker failed to take. A reminder no settling message decided (the resume timer's)
 * passes `""` for the text. */
export function reminderText(lastAssistantText: string, work: string | undefined): string {
  const reminder = work === undefined ? UNREAD_REMINDER : owedReminder(work);
  return TOOL_CALL_SHAPED.test(lastAssistantText) ? TEXT_TOOL_CALL + reminder : reminder;
}

/** The closed design gate that holds a merger, as the owed answer gives it (`LegionOwed.hold`):
 * the gated step whose row holds it, the document registered for that step, and the version of it
 * that is with its reviewer. */
export type GateHold = NonNullable<LegionOwed["hold"]>;

/** The one reminder a settling message earns while a wait the worker declared stands, or the
 * closed design gate holds the claim (the owed `answer`'s `hold`): the reminder for a tool call
 * written as text, which says that the call did not run and asks for a real one, and asks for no
 * `handoff_complete`. Undefined when the message holds no tool call written as text: then nothing
 * is due while the wait or the hold stands. It takes the hold off the answer, so the read-set
 * census (`daemon-client.test.ts`) sees each field of the hold it reads.
 *
 * During a hold it also carries the closed note's sentence (dispatch://INFRA-2040, D15), word for
 * word the closed form of the daemon's `workflow.MergerGateNote` (`mergerClosedNote` in
 * `packages/daemon/internal/workflow/gate.go`), which every task of a held merger opens with, so a
 * merger told to repeat its call is told in the same words that the gate holds its merge. The
 * fixture `packages/contracts/fixtures/daemon-api/merger-closed-note.json` holds the two to each
 * other: the daemon's test and this package's test both read it. `tree` is the key of the
 * claim's tree root (`ClaimCapability.tree`), the tree the daemon names: the owed answer names
 * none, and the gate is the tree's. */
export function quietReminderText(
  lastAssistantText: string,
  tree: string,
  answer: Pick<LegionOwed, "hold">
): string | undefined {
  if (!TOOL_CALL_SHAPED.test(lastAssistantText)) return undefined;
  const hold = answer.hold;
  if (hold === undefined) return TEXT_TOOL_CALL.trimEnd();
  return (
    TEXT_TOOL_CALL +
    `The design gate of ${tree} closed: version ${hold.version} of its ${hold.step} document ` +
    `(${hold.artifactId}) is with its reviewer. Merge nothing, enqueue nothing, and take a pull ` +
    "request you queued back out of the merge queue."
  );
}

/**
 * The next state, and whether the settle has earned a reminder. The count moves only on
 * `reminded`, which the host applies once it has actually put the reminder in front of the worker,
 * so a reminder dropped because a task arrived first counts for nothing; `settle` never changes it.
 * `reminded` from either wait goes back to `active`, because the wait is over, and a settle in
 * either earns no reminder: the reminding is what ends one of them and the daemon the other.
 */
export function stepPhaseStall(state: PhaseStallState, input: PhaseStallInput): PhaseStallStep {
  switch (input.kind) {
    case "news":
    case "prompt":
      // News may be what the worker waited on, and the daemon's task is what ends the update
      // wait: either way something happened, and the count starts again. Whether news ends a wait
      // the worker declared is the daemon's to answer, so the reminder reports it there
      // (`src/phase-reminder.ts`); the rhythm is the same for both.
      return { state: { state: "active", count: 0 } };
    case "update-answered":
      // The worker has answered the update message and is to start nothing until the daemon comes
      // back to it, so a turn ending now has earned no reminder: a reminder here would continue
      // that turn in the same run, with work a replacement would throw away and no new turn for
      // the daemon to see. The wait this begins is the daemon's to end, never a timer's.
      return { state: { state: "awaiting-update", count: 0 } };
    case "tool-call":
      // The worker is working, whatever it is working on: nothing is unanswered any more.
      return { state: { state: state.state, count: 0 } };
    case "refused":
      // The provider, not the worker, ended this turn: nothing was read and nothing was said, so
      // the count stands where the last real turn left it and the settling itself earns no
      // reminder. Reminding such a session at once only buys another refusal, which is what left
      // the implementer of dispatch://INFRA-264 reminded for ever; the runner arms a delayed one
      // instead, and a claim the daemon has failed by then is reminded of nothing. The daemon
      // fails one at once for a refusal whose status cannot clear, or that Oh My Pi classes as
      // OpenAI's cyber-policy refusal (`cyber_policy`, whatever the status), of a prompt's turn or
      // of a reminder's own, which it judges from the turn-outcome report since no prompt ran it,
      // and never for any other refusal, however long the run: at six in a row the daemon sends
      // the `provider-errors` notice once (dispatch://INFRA-4508; `providerErrorRunReached` in the
      // daemon says to whom).
      return { state };
    case "reminded":
      return { state: { state: "active", count: state.count + 1 } };
    case "settle": {
      // Either wait stands as it is until whatever ends that wait ends it.
      if (state.state !== "active") return { state };
      // A worker waiting on its own background job is not stalled: the job's own result starts
      // the next turn. That is the one thing that buys the quiet wait, which is also what bounds
      // it — a job that never finishes is a wait the host's waiting delay ends, and the reminder
      // comes again then rather than never.
      if (input.backgroundWork === true) {
        return { state: { state: "waiting", count: state.count } };
      }
      return { state, remind: true };
    }
  }
}

/** What an arriving message from outside the session is (`inboundOf`): a daemon `prompt`, or
 * `news`, with the delivery's own key where the message carries one. */
export type Inbound =
  | { readonly kind: "prompt" }
  | { readonly kind: "news"; readonly deliveryKey?: string };

/** What an arriving message is, by its kind and never by its text (D8 of dispatch://INFRA-2040's
 * plan), or undefined when it did not come from outside the session. The daemon delivers its task
 * as the RPC `prompt`, which Oh My Pi records as a user message: a `prompt`, which ends no wait the
 * worker declared. An Envoy delivery arrives as an `envoy-message` custom message: `news`, with
 * the card's `details.dedupe_key` as its key. pi-envoy's `handCard` copies that key from the frame
 * (`ledgerIdentity` in `@legion/envoy-client/delivery-ledger`) when it names the frame's event, the
 * update ask's key among them (D9 of the same plan); a card that carries none is news with no key.
 * A person's direct message from Dispatch that the Envoy extension sent in as the user's own turn
 * is a user message as well, or for a `/skill:` the host's `skill-prompt` custom message:
 * `extensions/legion.ts` recognises it before asking this (`matchInjectedUserTurn`), and takes it
 * as news with no key. Anything else (the host's own continuations, this check's reminder among
 * them, the Envoy extension's own notices and its wake for cards the host folded with no turn,
 * both carrying `LOCAL_ENVOY_NOTICE`, tool results, the model's replies) is neither. A reminder
 * the plugin sends through `pi.sendMessage` carries its own custom type for that reason: a
 * `role: "user"` message or an `envoy-message` would make the reminder reset its own count. */
export function inboundOf(message: unknown): Inbound | undefined {
  if (typeof message !== "object" || message === null || !("role" in message)) return undefined;
  if (message.role === "user") return { kind: "prompt" };
  if (
    message.role !== "custom" ||
    !("customType" in message) ||
    message.customType !== "envoy-message"
  ) {
    return undefined;
  }
  const details =
    "details" in message && typeof message.details === "object" && message.details !== null
      ? message.details
      : undefined;
  if (details === undefined) return { kind: "news" };
  if ("localNotice" in details && details.localNotice === LOCAL_ENVOY_NOTICE.localNotice) {
    return undefined;
  }
  return "dedupe_key" in details && typeof details.dedupe_key === "string"
    ? { kind: "news", deliveryKey: details.dedupe_key }
    : { kind: "news" };
}

/** The text blocks of an assistant message, joined; empty for anything else. A text block is
 * `{ type: "text", text }` on both hosts' shapes: Oh My Pi's own message model, which `message_end`
 * and `session_stop` hand the plugin (`packages/pi-shared/src/agent-stream.ts` reads it the same
 * way), and Claude Code's, whose `Stop` hook hands its host the last message as a string, so that
 * host never comes here. */
export function assistantText(message: unknown): string {
  if (typeof message !== "object" || message === null) return "";
  if (!("role" in message) || message.role !== "assistant" || !("content" in message)) return "";
  if (!Array.isArray(message.content)) return "";
  return message.content
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

/** Whether a settled assistant message ran at least one real tool call: its `content` holds a
 * tool-call block. The plugin reads this from the host's `message_end`, which is where a settled
 * message is: the `tool_call` handler it registers for the stall watch (INFRA-1274) records one
 * call's arguments and returns, and says nothing about whether the turn that call belongs to
 * settled. Neither handler blocks or shapes a call the worker makes.
 *
 * Two block names are accepted because the hosts disagree on it: Oh My Pi's `message_end` hands a
 * `toolCall` block (`{ type: "toolCall", id, name, arguments }`, the shape
 * `packages/pi-shared/src/agent-stream.ts` reads), and Claude Code names the same thing `tool_use`
 * (its stream-json, which `packages/daemon/internal/claudebridge/bridge.go` reads). Matching only
 * `tool_use` once left a working Oh My Pi worker reported to its architect as stalled. */
export function assistantRanTool(message: unknown): boolean {
  if (typeof message !== "object" || message === null) return false;
  if (!("role" in message) || message.role !== "assistant" || !("content" in message)) return false;
  if (!Array.isArray(message.content)) return false;
  return message.content.some(
    (block: unknown) =>
      typeof block === "object" &&
      block !== null &&
      "type" in block &&
      (block.type === "toolCall" || block.type === "tool_use")
  );
}

/** The provider's refusal of the turn an assistant message ended, or undefined when that message
 * is not one: Oh My Pi stops such a reply with `error` and words it in `errorMessage`
 * (`AssistantMessage` in `src/omp-host.d.ts`; `packages/pi-envoy/extensions/askgate/askgate.ts`
 * reads the same pair). A refusal the host could not word still has to read as a refusal, so the
 * stop reason alone stands in for the words. Claude Code never comes here: its own `StopFailure`
 * hook is what says a turn ended on an API error, and hands the error text with it. */
export function assistantRefusal(message: unknown): string | undefined {
  if (typeof message !== "object" || message === null) return undefined;
  if (!("stopReason" in message) || message.stopReason !== "error") return undefined;
  const worded =
    "errorMessage" in message && typeof message.errorMessage === "string"
      ? message.errorMessage
      : "";
  return worded === "" ? "error" : worded;
}

/** How the turn one Oh My Pi settling message ended, as the machine takes it: the provider's
 * refusal when it refused the turn, and otherwise what the worker said. One classification, so a
 * refused turn is an input of the machine rather than a check on its answer. */
export function turnEndOf(message: unknown): TurnEnd {
  const refusal = assistantRefusal(message);
  return refusal === undefined
    ? { kind: "settle", lastAssistantText: assistantText(message) }
    : { kind: "refused", message: refusal };
}

/** The states a record holds: the rhythm's, and nothing of the issue's phase, which is the
 * daemon's to answer. The wait a `ready_for_update` answer creates is not one of them: a record is
 * read only by the process a relaunch starts, and that relaunch is the round the wait belonged to
 * ending, so the wait is over before anything restores it. It is recorded as the `waiting` it is a
 * kind of, which a restored session times out as it always did. */
export type RecordedPhaseStall = Exclude<PhaseStall, "awaiting-update">;

/** The state as a record holds it (`RecordedPhaseStall`). */
export function recordedPhaseStall(state: PhaseStall): RecordedPhaseStall {
  return state === "awaiting-update" ? "waiting" : state;
}

/** The state one recorded entry holds, or undefined when it holds none this build understands.
 * `open` and `closed` are the release before INFRA-3136, which kept a model of the phase: both
 * read as the rhythm's `active` with their count, since whether anything is owed is the daemon's
 * answer now, and a `closed` record resumed on is reminded only if the daemon says the claim owes
 * work. `quiet` and `text-call` are the build before that: both mean nothing has been reminded
 * yet, so a session resumed on one is `active` with no reminders counted. */
function phaseStallFrom(
  data: object
): { readonly state: RecordedPhaseStall; readonly count: number } | undefined {
  if (!("state" in data) || typeof data.state !== "string") return undefined;
  const recorded = "count" in data ? data.count : undefined;
  const count =
    typeof recorded === "number" && Number.isInteger(recorded) && recorded >= 0 ? recorded : 0;
  switch (data.state) {
    case "active":
    case "waiting":
      return { state: data.state, count };
    case "open":
    case "closed":
      return { state: "active", count };
    case "quiet":
    case "text-call":
      return { state: "active", count: 0 };
    default:
      return undefined;
  }
}

/** The data of the branch's last entry this build understands, or undefined when it holds none:
 * the one place the restore reads, so the state and the plugin's own report fields come from one
 * entry rather than two scans that could disagree. */
function lastPhaseStallData(branch: readonly unknown[]): object | undefined {
  return lastCustomEntry(branch, PHASE_STALL_ENTRY, (data) =>
    phaseStallFrom(data) === undefined ? undefined : data
  );
}

/** The head of the worker's last message the architect notice quotes, and so all of it the record
 * keeps: a file and a transcript written on every settle hold no more than the notice can use. */
export const LAST_MESSAGE_LIMIT = 500;

/** One report of news the daemon has not answered yet: the wait it names (`waitId`), and the
 * reported delivery's own key, verbatim, absent for a person's message, which has none
 * (`LegionWaitNewsRequest`). */
export interface WaitNewsReport {
  readonly waitId: string;
  readonly deliveryKey?: string;
}

/** What one recorded entry carries. The machine never reads `reported`, `reportKey`,
 * `lastMessage`, `waitId` and `unsentNews`, and each host persists them its own way — the Oh My
 * Pi transcript entry, the Claude Code state file — but they ride the same record, so a relaunch
 * mid-run neither repeats an accepted notice, nor loses the key a refused one is retried under,
 * nor sends one that has forgotten what the worker last said, nor forgets the wait news is
 * reported against or a report of news the daemon never answered. */
export interface PhaseStallEntry {
  readonly state: RecordedPhaseStall;
  readonly count: number;
  readonly reported: boolean;
  readonly reportKey?: string;
  /** The head (`LAST_MESSAGE_LIMIT`) of what the worker said as its last turn settled; absent
   * while it has said nothing, and in a record an earlier build wrote. */
  readonly lastMessage?: string;
  /** The id of the wait the worker declared (`declare_wait`) that the plugin last knew to stand:
   * news after it is reported against it. Absent where none is known. */
  readonly waitId?: string;
  /** The reports of news the daemon has not answered, oldest first: each is sent again at the
   * next turn's end, timer or boot. A wait one of them names has ended in the plugin, whatever
   * the daemon's answer still says. Absent where none is kept. */
  readonly unsentNews?: readonly WaitNewsReport[];
}

/** Whether the architect has already been told about the run of reminders one record belongs to,
 * and the idempotency key that notice was sent under. */
function phaseStallReportFrom(data: object): {
  readonly reported: boolean;
  readonly reportKey: string | undefined;
} {
  const reported = "reported" in data && data.reported === true;
  const key =
    "reportKey" in data && typeof data.reportKey === "string" ? data.reportKey : undefined;
  return { reported, reportKey: key };
}

/** The report one recorded element holds, as a list of none or one, so a malformed element of a
 * record is dropped rather than refusing the record. */
function waitNewsReportFrom(value: unknown): WaitNewsReport[] {
  if (typeof value !== "object" || value === null) return [];
  if (!("waitId" in value) || typeof value.waitId !== "string" || value.waitId === "") return [];
  const key =
    "deliveryKey" in value && typeof value.deliveryKey === "string" && value.deliveryKey !== ""
      ? value.deliveryKey
      : undefined;
  return [{ waitId: value.waitId, ...(key === undefined ? {} : { deliveryKey: key }) }];
}

/** The record one entry's data holds, or undefined when it holds no state this build understands.
 * The one assembly of a restored record: the Oh My Pi transcript entry reaches it through
 * `restorePhaseStallEntry` and the Claude Code state file through `readPhaseStallRecord`
 * (`packages/claude-envoy/src/legion-phase-hook.ts`), so no host assembles its own. */
export function phaseStallEntryFrom(data: object | undefined): PhaseStallEntry | undefined {
  if (data === undefined) return undefined;
  const stall = phaseStallFrom(data);
  if (stall === undefined) return undefined;
  const report = phaseStallReportFrom(data);
  const lastMessage =
    "lastMessage" in data && typeof data.lastMessage === "string"
      ? data.lastMessage.slice(0, LAST_MESSAGE_LIMIT)
      : "";
  const waitId = "waitId" in data && typeof data.waitId === "string" ? data.waitId : "";
  const unsentNews =
    "unsentNews" in data && Array.isArray(data.unsentNews)
      ? data.unsentNews.flatMap(waitNewsReportFrom)
      : [];
  return {
    state: stall.state,
    count: stall.count,
    reported: report.reported,
    ...(report.reportKey === undefined ? {} : { reportKey: report.reportKey }),
    ...(lastMessage === "" ? {} : { lastMessage }),
    ...(waitId === "" ? {} : { waitId }),
    ...(unsentNews.length === 0 ? {} : { unsentNews }),
  };
}

/** The record the branch's last phase-stall entry holds, or undefined when it holds none (a fresh
 * session, which starts `active` with no reminders counted). */
export function restorePhaseStallEntry(branch: readonly unknown[]): PhaseStallEntry | undefined {
  return phaseStallEntryFrom(lastPhaseStallData(branch));
}
