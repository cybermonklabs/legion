import { existsSync, realpathSync } from "node:fs";
import { basename, delimiter, dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  AGENT_STREAM_WATCH_INTERVAL_MS,
  type AgentStreamCommand,
  type AgentStreamControlMessage,
  agentStreamControlSubject,
  agentStreamFramesSubject,
  DELIVERY_CAPABILITIES,
  type DeliveryCapability,
  LISTENER_CALL_ATTEMPTS,
  LISTENER_CALL_RETRY_DELAY_MS,
  LISTENER_CALL_TIMEOUT_MS,
  legacyAgentSubject,
  type OpenAsk,
  type OpenAsksResponse,
  ROLE_TOPIC_PREFIX,
  slackTopicChannel,
  zodSchemaApi,
} from "@legion/contracts";
import {
  type AskgateOwner,
  closeStore,
  dropAskgateRecord,
  keepStore,
  recordedOwnerMode,
  STORE_BEAT_MS,
  selfOwner,
  writeAskgateRecord,
} from "@legion/envoy-client/askgate-marker";
import {
  type BrokerSessionName,
  forgetBrokerSession,
} from "@legion/envoy-client/broker-credential";
import {
  type ConsumerDelivery,
  type ConsumerFrame,
  type ConsumerGapNotice,
  createConsumerDelivery,
  gapNoticeText,
} from "@legion/envoy-client/consumer-delivery";
import {
  createDeliveryDedupe,
  type DedupeIdentity,
  DedupeIdentitySchema,
  type DeliveryClaim,
  type DispatchDelivery,
  inboundTimestamp,
  postDeliveryReply,
  type RenderInboundResult,
  renderInbound,
  senderLabel,
  wholeCarriedBody,
} from "@legion/envoy-client/delivery";
import {
  closeReplayedRemoval,
  createDeliveryLedger,
  type DeliveryLedger,
  envoyNoticeText,
  failureNoticeFields,
  ledgerIdentity,
  storedText,
} from "@legion/envoy-client/delivery-ledger";
import { commandName } from "@legion/envoy-client/dispatch-command";
import {
  type ActiveDispatchConfig,
  activeDispatchSettings,
  dispatchSettings,
} from "@legion/envoy-client/dispatch-config";
import { defaultExec } from "@legion/envoy-client/dispatch-cwd";
import {
  dispatchFirstSkillFile,
  readDispatchFirstContext,
} from "@legion/envoy-client/dispatch-first";
import { DispatchServiceError, DispatchWriteLandedError } from "@legion/envoy-client/dispatch-http";
import { forgetShownPictures, shownPictures } from "@legion/envoy-client/dispatch-picture-tools";
import { pictureAddresses } from "@legion/envoy-client/dispatch-pictures";
import {
  sessionDirectory as dispatchSessionDirectory,
  sessionStateRoot,
} from "@legion/envoy-client/dispatch-session-directory";
import {
  readResultsSince,
  resultsEnd,
  writeSessionTitle,
} from "@legion/envoy-client/dispatch-session-state";
import { askgateSetting, readEnvoyConfig } from "@legion/envoy-client/envoy-config";
import {
  DEFAULT_TMUX_PLACE_CACHE_MS,
  heartbeatRetryDelayMs,
  MIN_PASS_DEADLINE_MS,
  REGISTRATION_SETTLE_MS,
  SHUTDOWN_SETTLE_MS,
} from "@legion/envoy-client/envoy-settings";
import { LoggableError, messageFor, natsFailureReason } from "@legion/envoy-client/errors";
import type { InboundSubscription } from "@legion/envoy-client/inbound-connection";
import {
  inboundRestoredNoticeText,
  inboundUnavailable,
  inboundUnavailableNoticeText,
} from "@legion/envoy-client/inbound-outage";
import { connectListenerRelay } from "@legion/envoy-client/listener-relay";
import { machineID } from "@legion/envoy-client/machine";
import {
  clientBuild,
  clientConnectionName,
  connectNats,
  NATS_DIAL_PLAN,
  NATS_DIAL_TOKEN_DROPPED,
  type NatsDialHooks,
  type NatsDialPlan,
  natsConnectionOptions,
  natsDialBudget,
} from "@legion/envoy-client/nats-connection";
import { followNatsCredential, keepNatsCredential } from "@legion/envoy-client/nats-credential";
import {
  NoRelaySubjectError,
  RegistrationStoppingError,
  refusedTopicsNoticeText,
  registerWithRelay,
} from "@legion/envoy-client/relay-registration";
import {
  readSessionAnswers,
  sessionAnswersFailureLine,
} from "@legion/envoy-client/session-answers";
import {
  type SessionConfigFile,
  sessionConfigFile,
  sessionDispatchSettings,
  sessionSettings,
} from "@legion/envoy-client/session-settings";
import { postSlackReply } from "@legion/envoy-client/slack-post";
import { refuseUncapturedTopics, replayFollowedSubjects } from "@legion/envoy-client/stream-topics";
import { readTmuxPlace } from "@legion/envoy-client/tmux-place";
import { createTmuxPlaceCache } from "@legion/envoy-client/tmux-place-cache";
import {
  EnvoyToolOperation,
  envoyToolSpecs,
  keyTakenDetails,
  keyTakenText,
  type MessageMetadataArguments,
  parseEnvoyToolArguments,
  publishConfirmationText,
  publishResultDetails,
  sendConfirmationText,
  sessionsOnMachine,
  toMessageMetadata,
} from "@legion/envoy-client/tool-contract";
import {
  createEnvoyClient,
  expandSubscriptionTopics,
  holderStateOf,
  isBusUnanswered,
  mayActOnRoleRecord,
  mergeInterestSources,
  type SubscribeResult,
} from "@legion/envoy-client/transport";
import { type AcceptedUserTurnDelivery, acceptUserTurn } from "@legion/pi-shared/accept-user-turn";
import {
  AgentStreamPublisher,
  type AgentStreamSettingsState,
  type DispatchMessageCardDetails,
  modelReference,
} from "@legion/pi-shared/agent-stream";
import {
  liveContext,
  transcriptPictures,
  withDeliveredPictures,
} from "@legion/pi-shared/delivery-pictures";
import { currentSessionID } from "@legion/pi-shared/dispatch-session-id";
import {
  type AcceptedUserTurn,
  HANDLED_ATTEMPT_ENTRY,
  handledAttempts,
  handledMessages,
  typedInputReply,
} from "@legion/pi-shared/dispatch-user-turn";
import {
  dropInjectedUserTurn,
  endInjectedUserTurns,
  forgetInjectedUserTurns,
  matchInjectedUserTurn,
  messageTag,
  noteInjectedUserTurn,
  noteTypedUserTurn,
} from "@legion/pi-shared/injected-user-turns";
import { LOCAL_ENVOY_NOTICE, publishEnvoyPluginInterface } from "@legion/pi-shared/interface";
import type {
  ContentBlock,
  ContextUsage,
  ImageContent,
  PiApi,
  SessionContext,
  SessionSwitchReason,
  SideTurn,
  ToolResult,
} from "@legion/pi-shared/pi-types";
import {
  type LegionNoticeSubscription,
  type LegionRoleClaim,
  type LegionRoleClaimInstance,
  legionRoleClaimBridge,
} from "@legion/pi-shared/role-claim-bridge";
import { RoleClaimStoppingError } from "@legion/pi-shared/role-claim-stopping";
import { dispatchCommandHead } from "@legion/pi-shared/shell-command";
import {
  type SessionIdentityContext,
  subagentSessionCheck,
} from "@legion/pi-shared/subagent-session";
import { toolFailure, toolSuccess } from "@legion/pi-shared/tool-result";
import * as host from "@oh-my-pi/pi-coding-agent";
import type { UserInputBuiltinCommand } from "@oh-my-pi/pi-coding-agent/extensibility/extensions/send-user-input-handler";
import { logger as hostLogger, postmortem, procmgr, VERSION } from "@oh-my-pi/pi-utils";
import { encode } from "@toon-format/toon";
import { connect, Events, StringCodec } from "nats";
import { name as packageName, version as packageVersion } from "../package.json" with {
  type: "json",
};
import { CARD_DELIVERED_ENTRY, CardAges } from "../src/card-age";
import { CardWake, type EnvoyCardMarker } from "../src/card-wake";
import { withDispatchFirst } from "../src/dispatch-first";
import { sessionDispatchClient } from "../src/dispatch-session";
import { recordEnvoySession, resolveEnvoySession } from "../src/envoy-session";
import { INBOUND_RESTORED_LINES, INBOUND_UNAVAILABLE_LINES } from "../src/inbound-outage-lines";
import { listenerRoleName } from "../src/listener-role-name";
import {
  CONSUMER_STATE_ENTRY,
  consumerSnapshot,
  heldReplayNotices,
  REPLAY_DELIVERED_ENTRY,
  REPLAY_NOTICE_ENTRY,
  REPLAY_STATE_ENTRY,
  rememberedSubjects,
  replaySnapshot,
} from "../src/missed-messages";
import { opensAsk } from "../src/opens-ask";
import {
  createRegistrationState,
  type DialLimit,
  type RegistrationInput,
  type RoleSnapshot,
  type TaskHandle,
} from "../src/registration-state";
import { createRoleTopicFollows, ROLE_TOPICS_VAR, readRoleTopics } from "../src/role-topics";
import { scrubbedHost, scrubbedLogger } from "../src/scrubbed-host";
import { ServerClock } from "../src/server-clock";
import {
  ANSWERS_SENT_ENTRY,
  type CarriedAnswers,
  carriedAnswers,
  withSessionAnswers,
} from "../src/session-answers-context";
import { type SessionInbound, sessionInbound } from "../src/session-inbound";
import { sideTurn } from "../src/side-turn";
import { followBoundMs, startTradeBoundMs } from "../src/start-bound";
import { gatedLogger } from "../src/stop-gates";
import { stepWasRefused, taskWasAbandoned } from "../src/task-queue";
import { transcriptMessages } from "../src/transcript-messages";
import { TypedPictures } from "../src/typed-pictures";
import {
  MOVED_SESSION_REASON,
  runShellCommand,
  shellCommandBusyReport,
  shellCommandFrom,
  shellCommandMessage,
  shellCommandMovedReport,
  shellCommandReport,
  shellCommandShutdownReport,
  shellCommandWithheldReport,
} from "../src/typed-shell";
import { type CardDelivery, UnwrittenFrames } from "../src/unwritten-frames";
import { type AskGateRegistration, registerAskGate } from "./askgate/askgate";
import {
  ADVISOR_GATE_ENTRY_TYPE,
  ADVISOR_GATE_RECORD_ENTRY_TYPE,
  type GateEntry,
  type GateRecordEntry,
} from "./askgate/askgate-core";
import { registerEnvoyMessageRenderer } from "./envoy-message-renderer";
import { registerEnvoyWhoamiCommand } from "./envoy-whoami-command";

// The host's built-in slash commands, each marked when only its terminal runs it, which our fork
// exports beside `sendUserInput` (can1357/oh-my-pi#14323). Loaded once and never awaited, through
// a dynamic import because a static one would fail the whole extension's load on a host without
// the module; such a host lists no built-ins.
let hostBuiltinCommands: (() => readonly UserInputBuiltinCommand[]) | undefined;
void import("@oh-my-pi/pi-coding-agent/extensibility/extensions/send-user-input-handler").then(
  (module) => {
    hostBuiltinCommands = module.listUserInputBuiltinCommands;
  },
  () => undefined
);

/** The host's live `AgentSession`, as far as this extension reads it. */
type LiveAgentSession = NonNullable<host.AgentRef["session"]>;

// The live `AgentSession` under `session`, its live id, found in the host's roster
// (`AgentRegistry`) at every call, since the host keeps one `AgentSession` across `/new`, `/resume`
// and a fork and changes its id. Undefined on a host without the registry export, or listing no
// live session under that id.
function liveAgentSession(session: string): LiveAgentSession | undefined {
  return (
    host.AgentRegistry?.global()
      .list()
      .find((ref) => ref.session?.sessionManager.getSessionId() === session)?.session ?? undefined
  );
}

// The model and thinking level the live session reports, read through the same getters the host's
// `/model` and `/effort` read: `model`, `getAvailableModels()`, `configuredThinkingLevel()` (what
// `/effort` reports, undefined for the model's default) and `getAvailableEffortSelectors()` (what
// `/effort` offers, empty for a model with no adjustable thinking). Unreported with no live session,
// or on a build that lacks any of the four. A model whose provider or id is empty is skipped.
function liveSessionSettings(live: LiveAgentSession | undefined): AgentStreamSettingsState {
  if (
    live === undefined ||
    !("model" in live) ||
    typeof live.configuredThinkingLevel !== "function" ||
    typeof live.getAvailableModels !== "function" ||
    typeof live.getAvailableEffortSelectors !== "function"
  ) {
    return { reported: false };
  }
  const model = modelReference(live.model?.provider, live.model?.id);
  const levels = live.getAvailableEffortSelectors();
  return {
    ...(model !== undefined && { model }),
    models: live.getAvailableModels().flatMap((available) => {
      const listed = modelReference(available.provider, available.id);
      return listed === undefined ? [] : [listed];
    }),
    reported: true,
    thinking:
      levels.length === 0 ? null : { level: live.configuredThinkingLevel() ?? null, levels },
  };
}

const codec = StringCodec();

/**
 * The dial `dialNats` opens this session's NATS connection with: nats.js's own `connect` with the
 * redials every plugin makes (`NATS_DIAL_PLAN`), unless a test put a stand-in in its place, which
 * dials once unless the test gives it a plan of its own. Production never sets one.
 *
 * The stand-in lives on `globalThis` under a process-wide symbol rather than in a binding of this
 * module, because a suite loads this entry many times over — each `import("./envoy.ts?<row>")` is
 * a module of its own, with a closure of its own — and one setter call has to reach every
 * instance. It is the rule the Envoy session record is kept under for the same reason
 * (`src/envoy-session.ts`).
 */
const NATS_CONNECT = Symbol.for("legion.pi-envoy.nats-connect");

interface NatsDialStandIn {
  readonly connect: typeof connect;
  readonly plan: NatsDialPlan;
  /** How the stand-in's dials wait between them, where its test sets that. */
  readonly waits: Pick<NatsDialHooks, "delay" | "random">;
}

interface GlobalNatsConnect {
  [key: symbol]: NatsDialStandIn | undefined;
}

const natsConnectStore = globalThis as unknown as GlobalNatsConnect;

/** A stand-in's plan unless its test gives one: one dial, so a row that holds NATS down fails
 *  each pass's dial at once. */
const DIAL_ONCE: NatsDialPlan = { firstDelayMs: 0, maxDelayMs: 0, budgetMs: 0 };

export function setNatsConnectForTests(
  dial: typeof connect,
  plan: NatsDialPlan = DIAL_ONCE,
  waits: Pick<NatsDialHooks, "delay" | "random"> = {}
): void {
  natsConnectStore[NATS_CONNECT] = { connect: dial, plan, waits };
}

// The slot outlives every instance of this entry in the process, so a file that set a stand-in
// clears it when it ends; otherwise every later file of the same `bun test` process dials that
// file's stand-in, with that file's state. Production callers never call this.
export function resetNatsConnectForTests(): void {
  delete natsConnectStore[NATS_CONNECT];
}

/** The directory of this module: `dist/` in a packed build (`dist/envoy.js`), `extensions/` when
 *  it runs from source. */
const MODULE_DIRECTORY = dirname(fileURLToPath(import.meta.url));

/**
 * What this plugin's NATS connection names as its build after its name (`clientConnectionName`),
 * which legacy-subjects-gate.sh reads from the hub's census of connections: the build-commit stamp
 * the worker image writes beside the packed bundle, else, for a packed build, the release version
 * its manifest carries, else none (a plugin run from its source). A test replaces it.
 */
let buildSource: { readonly stamp: string; readonly packed: boolean } = {
  stamp: join(MODULE_DIRECTORY, "build-commit"),
  packed: basename(MODULE_DIRECTORY) === "dist",
};

export function setBuildSourceForTests(source: {
  readonly stamp: string;
  readonly packed: boolean;
}): void {
  buildSource = source;
}

/** This plugin's build as its NATS connection names it, read at each dial. */
function connectionBuild(): string | undefined {
  return clientBuild(
    buildSource.stamp,
    buildSource.packed ? { name: packageName, version: packageVersion } : undefined
  );
}

/**
 * Whether this process has logged that a session it decides for is not the one a session pod's
 * launch named for its role (`sessionRoleRefusal` in the entry). The line is logged once per
 * process, whichever extension instance of it decides first, and an instance may come from a
 * later load of this entry (each top-level session of an ACP host), so the mark lives on
 * `globalThis`, as the NATS stand-in above does.
 */
const SESSION_ROLE_LOGGED = Symbol.for("legion.pi-envoy.session-role-logged");

interface GlobalSessionRoleLogged {
  [key: symbol]: true | undefined;
}

const sessionRoleLoggedStore = globalThis as unknown as GlobalSessionRoleLogged;

// The mark outlives every instance of this entry in the process, so a suite clears it between
// rows, as it clears the NATS stand-in above. Production callers never call this.
export function resetSessionRoleLoggedForTests(): void {
  delete sessionRoleLoggedStore[SESSION_ROLE_LOGGED];
}

const NATS_RETRY_INTERVAL_MS = 15_000;
/** Why the session's relay subject went when nats.js closed its connection and named no error. */
const RELAY_CONNECTION_CLOSED = "the NATS connection closed";
/** How long a delivery waits for the pictures it embeds before it goes out as text alone. */
const DELIVERY_PICTURES_TIMEOUT_MS = 20_000;
/** How long a delivery waits on the whole body of an item an event carried only in part before
 *  it goes out with that part and the note saying so. */
const WHOLE_BODY_TIMEOUT_MS = 10_000;
/** The most BTW event ids an instance remembers, so a failure notice naming one stays inert. */
const BTW_EVENTS_LIMIT = 1_000;
/** The line a late BTW's card ends with: it runs nothing and asks for nothing. */
const LATE_BTW_NOTE =
  "This BTW was asked while this session was not taking messages. It is shown for your " +
  "information only: it runs no side turn and needs no answer. Dispatch shows whether it was " +
  "answered.";
/** What a person is told when the session their message was addressed to is no longer the one
 *  open here: a `/new`, a `/resume`, or a move the host made with no event for it put the host
 *  on another conversation, and that conversation is neither the one the person wrote to nor
 *  the one that may answer for it. The `!` path says the same of the command it did not run
 *  (`shellCommandMovedReport` in `../src/typed-shell.ts`), from the one sentence both read
 *  here (`MOVED_SESSION_REASON`), so one sentence meets a person wherever a delivery stops
 *  for this. */
const MOVED_SESSION_REFUSAL = `The message was not sent: ${MOVED_SESSION_REASON}`;

/**
 * Aside and Steer go through `pi.sendMessage` on every OMP build; BTW only where the host has a
 * side turn (`sideTurn` in src/side-turn.ts), so a host without one advertises every capability
 * but that one. So every registration of this extension advertises at least `aside` and `steer`,
 * and a plain send to its session never reads `no_receiver`: the listener gives that answer only
 * to a running session that advertises no delivery mode.
 */
const CAPABILITIES_WITHOUT_BTW: readonly DeliveryCapability[] = DELIVERY_CAPABILITIES.filter(
  (capability) => capability !== "btw"
);

/**
 * The identity a delivery is claimed under, each field read on its own by the shared rule for it
 * (`DedupeIdentitySchema`): a field the rule refuses identifies nothing, and a valid sibling still
 * does. `renderInbound` renders a frame whose `dedupe_key` is empty, and that frame's `event_id`
 * must still recognise its second copy.
 */
const ClaimIdentitySchema = DedupeIdentitySchema.extend({
  event_id: DedupeIdentitySchema.shape.event_id.catch(undefined),
  dedupe_key: DedupeIdentitySchema.shape.dedupe_key.catch(undefined),
  source: DedupeIdentitySchema.shape.source.catch(undefined),
  source_event_id: DedupeIdentitySchema.shape.source_event_id.catch(undefined),
});

/**
 * Transcript entry recording the role this session holds. A successful claim writes
 * `{ role, session_id, client_instance_id }`: the role, the session it was claimed for and the
 * process that claimed it, since one transcript can be read by two processes at once (a
 * continuation resuming a live session's id) and only the process that wrote a record acts on
 * it while it holds a role. A `{ role: null }` release record, and any record from a build
 * before the two fields existed, names no process and is read as this process's own, exactly as
 * it was read then.
 */
const ROLE_CLAIM_ENTRY = "envoy-role-claim";

const OPEN_ASKS_TIMEOUT_MS = 3_000;

/**
 * How long a person's direct message waits on Dispatch's acceptance before it arrives as a card
 * instead: the frames on the agent subject are delivered one at a time, so a hung Dispatch must
 * not hold every message behind this one for the client's own minute.
 */
const USER_TURN_ACCEPT_TIMEOUT_MS = 10_000;

/**
 * Transcript entry marking a session Legion drives. The process-wide bridge knows the same
 * thing until the process ends; this is what a resumed session reads.
 */
const LEGION_MANAGED_ENTRY = "legion-managed-session";

/** Custom-message type of the run-end nudge itself; never displayed. */
const ASK_REMINDER_MESSAGE = "dispatch-ask-reminder";
/** Unicode line separators that terminate a displayed ask question. */
const ASK_PROMPT_LINE_SEPARATOR = /\r\n|[\r\n\u0085\u2028\u2029]/;

/**
 * Return the first display line while keeping all other C0/C1 controls visibly inert. The
 * separator search handles CRLF, bare CR/LF, NEL, and Unicode line separators; every other
 * control becomes one space before the bounded prompt line is formed.
 */
function askPromptQuestionHead(question: string): string {
  const firstLineEnd = question.search(ASK_PROMPT_LINE_SEPARATOR);
  const firstLine = firstLineEnd === -1 ? question : question.slice(0, firstLineEnd);
  let normalized: string[] | undefined;
  let segmentStart = 0;
  for (let index = 0; index < firstLine.length; index += 1) {
    const codePoint = firstLine.charCodeAt(index);
    if (codePoint > 0x1f && (codePoint < 0x7f || codePoint > 0x9f)) continue;
    normalized ??= [];
    normalized.push(firstLine.slice(segmentStart, index), " ");
    segmentStart = index + 1;
  }
  return (
    normalized === undefined ? firstLine : `${normalized.join("")}${firstLine.slice(segmentStart)}`
  ).trim();
}
/**
 * What the hidden self-check asks the agent, over a snapshot of its own conversation. It names
 * the session's own open asks, then asks whether the agent is waiting on a human for something
 * those asks do not cover. With none it says the session has none, not that Dispatch has none:
 * another session's open ask can cover the wait, and a line denying that ask exists drew WAITING.
 * One word comes back; only WAITING buys the visible turn.
 * A reply a human already said they will send and that is on its way, such as a go-ahead message
 * the user promised, is an event, so PROCEEDING: WAITING would buy a steer to ask for it.
 * Something a human put off for later ("I'll take a look later") or promised and then did not
 * send is not on its way, so it stays WAITING: an agent waits on a human only through an open
 * ask. WAITING excludes a wait the listed asks cover, and PROCEEDING counts a wait any open ask
 * covers, another session's included.
 */
const ASK_SELF_CHECK_PROMPT = (asks: readonly Pick<OpenAsk, "question">[]): string => {
  const askLimit = 5;
  const questionLimit = 120;
  const promptAsks =
    asks.length === 0
      ? "You have no open asks in Dispatch."
      : [
          "Your open asks in Dispatch:",
          ...asks.slice(0, askLimit).map((ask) => {
            const firstLine = askPromptQuestionHead(ask.question);
            const question =
              firstLine.length <= questionLimit
                ? firstLine
                : `${firstLine.slice(0, questionLimit - 1)}…`;
            return `- ${question}`;
          }),
          ...(asks.length > askLimit ? [`+${asks.length - askLimit} more`] : []),
        ].join("\n");

  return (
    "Your run has just ended. " +
    `${promptAsks}\n\n` +
    "Are you right now waiting on a human for anything none of those asks covers? " +
    "Answer with exactly one word and nothing else: WAITING or PROCEEDING. " +
    "WAITING — you stopped because you need a decision, an approval, or information from a human " +
    "that none of those asks covers and that is not already on its way; something they put off " +
    "for later, or promised and then did not send, is not on its way. " +
    "PROCEEDING — you finished, you will carry on by yourself, or you are waiting only on open " +
    "asks, tools, subagents, or events, including a message, an answer, or an event a human " +
    "already said they will send and that is on its way: a reply on its way is an event, not an " +
    "open question."
  );
};

/**
 * A verdict whose first word is WAITING, after any markdown emphasis or quoting — the reply
 * convention the Legion phase-stall follow-up already uses (`packages/pi-legion/src/phase-stall.ts`).
 * Case-sensitive and first-word-only on purpose: "NOT WAITING", "I am WAITING on Sami" and
 * "Waiting." are all PROCEEDING here, because a false WAITING is the expensive error — the
 * steer it buys asserts the agent said it is waiting, which sends it to page a human with a
 * question nobody had, while a false PROCEEDING is only the silence of the status quo.
 */
const WAITING_VERDICT = /^\W*WAITING\b/;

/** The other choice, named anywhere in the reply: the model echoing the question, not answering it. */
const ECHOED_CHOICE = /\bPROCEEDING\b/;

const isWaitingVerdict = (reply: string): boolean =>
  WAITING_VERDICT.test(reply) && !ECHOED_CHOICE.test(reply);

/**
 * Bound on the self-check, imposed by the extension's own clock rather than the host's: it is a
 * whole-context model call on the session's own model, so it costs what a short turn costs; a
 * minute is long enough for a large context on a slow provider and short enough that a wedged
 * one does not hold the stop window open all night. `ENVOY_SELF_CHECK_TIMEOUT_MS` moves it for a
 * deployment whose provider is slower than that — past the bound the check is simply silent, so
 * one set too low turns the nudge off rather than making it wrong.
 */
const ASK_SELF_CHECK_TIMEOUT_MS = 60_000;

/**
 * Most self-checks one armed period pays for. Work re-arms the check, so an unattended run that
 * keeps working would otherwise buy a hidden model call after every settle; five bounds that.
 */
const ASK_CHECKS_PER_PERIOD = 5;

const UNASKED_WAIT_REMINDER =
  "You just said you are waiting on a human for something no open ask in Dispatch covers. " +
  "Open it now: a decision block in the document it concerns (dispatch doc-edit with an ask block), or " +
  "dispatch ask for a to-do only a human can do, naming exactly what you need and from whom.";

/**
 * One arming period of the run-end nudge. A person's typed input or Dispatch card arms a period.
 * `check_due` is the outstanding check, in the shape of the host's own todo reminder: arming owes
 * one, a completed check spends it, the agent's next real work — a successful tool call that ran
 * anything but `dispatch` commands the ledger records — owes another, and opening the ask itself
 * spends it, since the nudge has nothing left to say about that stop. A turn that only replies and
 * stops does no work, so the nudge's own continuation can never owe one, which is what keeps it
 * from nudging itself forever. `checks` counts the ones spent, capped at `ASK_CHECKS_PER_PERIOD`.
 *
 * `baseline_as_of` is the server clock the period's next Dispatch read asks from, and it moves
 * after every self-check to that snapshot's `as_of`. This keeps the check's view current while the
 * session works between settles.
 *
 * In-memory only, for the life of this process's session. A cold start or a session change begins
 * at period 0, which the stop guard refuses, so nothing nudges before the next person-authored
 * input arms a period — no transcript entry buys anything beyond that.
 */
interface AskAwarenessState {
  readonly session_id: string;
  readonly period: number;
  readonly baseline_as_of: string | null;
  readonly check_due: boolean;
  readonly checks: number;
}

/**
 * A settle the stop-time check is about: its context and the side turn that context offers, how
 * many runs had started by then, and the awareness generation then, so a user turn between the
 * settle and its check is caught.
 */
interface AskSettle {
  readonly context: SessionContext;
  readonly ask: SideTurn;
  readonly seenRun: number;
  readonly generation: number;
}

interface LegionManagedEntry {
  readonly type: "custom";
  readonly customType: typeof LEGION_MANAGED_ENTRY;
  readonly data: { readonly session_id: string };
}

function isLegionManagedEntry(entry: unknown): entry is LegionManagedEntry {
  if (typeof entry !== "object" || entry === null) return false;
  if (!("type" in entry) || entry.type !== "custom") return false;
  if (!("customType" in entry) || entry.customType !== LEGION_MANAGED_ENTRY) return false;
  if (!("data" in entry) || typeof entry.data !== "object" || entry.data === null) return false;
  return "session_id" in entry.data && typeof entry.data.session_id === "string";
}

interface RoleClaimEntry {
  readonly type: "custom";
  readonly customType: typeof ROLE_CLAIM_ENTRY;
  readonly data: {
    readonly role: string | null;
    /** The session the claim was made for; absent on a record a build before it wrote. */
    readonly session_id?: string;
    /** The client instance that wrote the record: one Oh My Pi Envoy extension instance, the
     *  same id its registrations carry. Absent on a record a build before it wrote. */
    readonly client_instance_id?: string;
  };
}

// A claim refused, or handed back, because the session it was decided for was no longer open:
// told apart from a claim that failed, since the session may be open again by the time its caller
// reads (a resume Oh My Pi rolled back during the hand-back).
class SessionLeftError extends Error {}

// A registration failure inside a reconcile pass, told apart from a failed role read: only the
// former warrants a `"reregistered"` regain on the next healthy tick.
class RegistrationError extends Error {}

// A subject this pass could not open (NATS unavailable). The registration needs the relay
// subject, so a pass that opened none fails its registration too and reports that instead
// (`RegistrationError`); this is left for a subject opened beside it, the agent-stream control
// subject, in a pass that did register the session and settle its role, as `decided` says: a
// bridge call goes on with its claim, a heartbeat counts the registration, and the next pass
// opens the subject.
class InboundError extends Error {
  readonly decided: boolean;
  constructor(message: string, decided: boolean, options?: ErrorOptions) {
    super(message, options);
    this.decided = decided;
  }
}

function isRoleClaimEntry(entry: unknown): entry is RoleClaimEntry {
  if (typeof entry !== "object" || entry === null) return false;
  if (!("type" in entry) || entry.type !== "custom") return false;
  if (!("customType" in entry) || entry.customType !== ROLE_CLAIM_ENTRY) return false;
  if (!("data" in entry) || typeof entry.data !== "object" || entry.data === null) return false;
  if (!("role" in entry.data)) return false;
  if (!(entry.data.role === null || typeof entry.data.role === "string")) return false;
  // Both are absent on a record written before they existed, and a record that carries either as
  // something other than a string names neither session nor process: it records nothing this
  // process can read, so it is no claim record at all.
  if ("session_id" in entry.data && typeof entry.data.session_id !== "string") return false;
  if ("client_instance_id" in entry.data && typeof entry.data.client_instance_id !== "string") {
    return false;
  }
  return true;
}

function resolveSkillsDirectory(): string {
  const moduleDirectory = dirname(fileURLToPath(import.meta.url));
  const candidates = [
    // Packed layout: this file is bundled to dist/envoy.js and prepack stages
    // the repo skills/ directory beside it at dist/skills.
    resolve(moduleDirectory, "skills"),
    // Repo layout: this file runs from packages/pi-envoy/extensions/ and the
    // skills live at the repo root.
    resolve(moduleDirectory, "../../../skills"),
  ];
  const found = candidates.find((directory) => existsSync(directory));
  if (!found) {
    throw new Error(`legion skills directory not found; tried: ${candidates.join(", ")}`);
  }
  return found;
}
const SKILLS_DIRECTORY = resolveSkillsDirectory();
// Read once at load, so a plugin packed without the skill fails to load naming the file. A throw
// inside the `context` handler would not do that: Oh My Pi catches it and sends the request anyway.
const DISPATCH_FIRST_CONTEXT = readDispatchFirstContext(dispatchFirstSkillFile(SKILLS_DIRECTORY));

// The `dispatch` command this plugin ships: `bin/` is one directory above this module in both
// layouts (`<pkg>/dist/envoy.js` packed, `<pkg>/extensions/envoy.ts` in the repository). Oh My Pi
// snapshots `process.env` for its shell when it first runs one, after every extension has loaded,
// so `PATH`, `DISPATCH_HOST` and `ENVOY_CONFIG_FILE` below reach every shell command.
// The manifest's `omp.bin` has an Oh My Pi that supports it put `bin/` on `PATH` before this
// module loads, under the plugin's installed path. This module's own location is its realpath, and
// a realpath entry dies with its directory when a symlinked plugins root is re-pointed and the old
// target removed, so the plugin adds `bin/` itself only when no `PATH` entry already names it.
const PLUGIN_BIN = resolve(dirname(fileURLToPath(import.meta.url)), "..", "bin");
const pluginBinOnPath = (process.env.PATH ?? "").split(delimiter).some((entry) => {
  try {
    return entry !== "" && realpathSync(entry) === PLUGIN_BIN;
  } catch {
    return false;
  }
});
if (!pluginBinOnPath) {
  process.env.PATH = `${PLUGIN_BIN}${delimiter}${process.env.PATH ?? ""}`;
}
process.env.DISPATCH_HOST = "omp";
// The session's envoy.json, by Oh My Pi's rule: a named profile's `$PI_CODING_AGENT_DIR/envoy.json`,
// else the file `ENVOY_CONFIG_FILE` names, else the neutral file. The commands the session runs
// are handed the same file, so a `dispatch` command reads what this session reads. A location that
// cannot be worked out (a relative XDG_CONFIG_HOME, ENVOY_CONFIG_FILE or PI_CODING_AGENT_DIR) hands
// nothing on and turns both sections off.
const CONFIG_FILE = sessionConfigFile("omp", process.env);
if (CONFIG_FILE.path !== null) process.env.ENVOY_CONFIG_FILE = CONFIG_FILE.path;
/**
 * The session's Dispatch for one request of this extension's own, which has `ms` to answer: the
 * one builder (`sessionDispatchClient`), resolved again per call, so a Dispatch that moved takes
 * effect without a reload. Undefined when the session has no Dispatch configuration. `session` is
 * the session the request is made as, which is what a broker trade writes under: a caller acting
 * for one session hands that session in, since the live read moves to whichever session the host
 * is on by the time the request goes out.
 */
const timedDispatchClient = async (ms: number, session: () => string = currentSessionID) =>
  (await sessionDispatchClient(CONFIG_FILE, process.cwd(), { deadlineMs: ms }, session))?.client;

/** The host's logger, every message and field it is handed passing the scrub of the secrets this
 *  process holds (`../src/scrubbed-host`). Each extension instance writes through its own tell
 *  gate over it (`logger`, below), and only the stop's own lines are written to it directly
 *  (`releaseRoleClaims`). */
const scrubbedHostLogger = scrubbedLogger(hostLogger);

/** What `envoy_unsubscribe` answers once the session's stop has begun: it drops no topic. */
const STOPPING_UNSUBSCRIBES_NOTHING = "this session is stopping, so it unsubscribes nothing";

/**
 * The warning for a request to take `what` out of the listener's registry that the listener's
 * message bus did not answer (`isBusUnanswered`). The listener writes the record before it sets
 * the filter of the session's consumer, and it gives that answer also when only the filter update
 * met a NATS reconnect, so the record may already be without what the request named. The error
 * logged beside the warning says whether NATS was reconnecting. A registration sets the
 * consumer's filter to the topics it stores in the record (the listener's `registerPass`), so the
 * session's next one brings the filter in line with the record either way.
 */
const busUnansweredRemoval = (what: string): string =>
  `envoy: the listener's message bus did not answer a request to take ${what} out of the registry. ` +
  "The registry may already be without them. " +
  "The session's next registration sets its consumer's filter to the topics the registry holds.";

export default async function envoyExtension(hostApi: PiApi): Promise<void> {
  // Every notice, tool answer and tool error this extension gives the host passes the scrub of
  // the secrets this process holds, the backstop under the rules that name no peer's text.
  const pi = scrubbedHost(hostApi);
  // Whether this session's stop has begun (`shutDownSession`), the one stop reading every gate
  // and every control-flow read below takes.
  let shuttingDown = false;
  // The stop gates (R24 of dispatch://INFRA-3589, `../src/stop-gates`). Once the stop has begun,
  // the tell gate drops every pane notice (`notify`), held notice (`sendNotice`), notice to the
  // model and log line (`logger`) of this extension's own; the write gate refuses every ledger
  // entry and role record; and the client sends no call of the session's registration or role
  // state (`createEnvoyClient`'s `stopping`). Each reads `shuttingDown`. The stop's own lines go to
  // `scrubbedHostLogger`, which no gate holds. A relay frame already past `deliverRelayed`'s read
  // is a delivered message, not a notice: its card, its records and its answer pass no gate.
  const logger = gatedLogger(() => shuttingDown, scrubbedHostLogger);
  // A pane notice of the extension's own, through the tell gate: every `ui.notify` here goes
  // through this.
  const notify = (
    context: Pick<SessionContext, "ui"> | undefined,
    message: string,
    level: "info" | "warning"
  ): void => {
    if (!shuttingDown) context?.ui.notify(message, level);
  };
  logger.debug("extension instance loaded", { extension: import.meta.url });
  // The interface the Legion plugin reads (`@legion/pi-shared/interface`). `undefined` when an
  // Envoy entry at another interface version published first: that entry's object stays the one
  // Legion finds, and this instance pushes no claim instance onto it (first wins).
  const publishedInterface = publishEnvoyPluginInterface(import.meta.url);
  // Dispatch and Envoy as this session starts with them, each resolved on its own from the
  // session's envoy.json (`CONFIG_FILE`) and the variables over it. The dispatch-first context is
  // injected only where Dispatch is on at load. Dispatch's URL and token are re-read on every use
  // (`dispatchConfigFor`), so a Dispatch that moved - a new dispatch.serverUrl in envoy.json -
  // takes effect without /reload-plugins, and a file that has since broken fails with its own
  // error instead of quietly using the stale endpoint.
  const atStart = await sessionSettings(CONFIG_FILE, process.env, {
    cwd: process.cwd(),
    session: currentSessionID,
  });
  const { envoy } = atStart;
  // Dispatch for a caller acting as the session `session` names. The file and the variables are
  // read at this call, whoever the caller acts as, so only the session identity is pinned by the
  // name handed in: that name says which session a broker trade writes under, and the listener
  // answers 403 to a session presenting another's token. Every reply to a person's message
  // resolves through here for the session that reply names (`postDispatchReply`), so the actor a
  // reply names and the configuration it goes out under are one session and never two.
  const dispatchConfigFor = async (session: BrokerSessionName): Promise<ActiveDispatchConfig> => {
    const resolution = (
      await sessionDispatchSettings(CONFIG_FILE, process.env, {
        cwd: process.cwd(),
        session,
      })
    ).dispatch;
    const config = activeDispatchSettings(resolution);
    if (config === null) {
      throw new Error(
        `Dispatch is no longer configured: ${resolution.enabled ? "" : resolution.notice}`
      );
    }
    return config;
  };
  // Inbound messages arrive over NATS, or through the listener's relay socket where the settings
  // say `ENVOY_INBOUND=listener`: an Envoy with neither (no NATS server, a seed that was refused, or
  // a relay socket its settings cannot open) takes none, and its tools, which call the listener,
  // still work. The start notice says so (`sessionSettings`), with why a section is off, once, and
  // `envoy_subscribe` and the Legion interface's `claim` and `subscribe` refuse naming why
  // (`requireInbound`).
  const inbound = envoy.enabled && !("off" in envoy.inbound);
  const startNotice = atStart.notice ?? "";
  // AskGate (extensions/askgate/) reviews each gated Dispatch write before it posts, in a session
  // that has Dispatch: its own envoy.json and the variables over it name one. A session or pod
  // with no Dispatch has no write to review, so the gate binds nothing there and says nothing. A
  // checkout's file is left out of that test: it can turn off the Dispatch of its own checkout,
  // but not the gate, which a later shell command can still pass with DISPATCH_URL. The session's
  // own envoy.json is the gate's only switch. Its one `tool_call` handler registers with this
  // factory's own handlers (after this entry's handler, below) and waits for the Oh My Pi modules
  // the gate loads. A switch that cannot be read, and a gate that did not load, are said at every
  // session start; a gate that did not load refuses each write it would review.
  const askgateNotices: string[] = [];
  let askgateLoad: Promise<void> = Promise.resolve();
  // The gate's registration, while it is bound: the load, which the start notice reads, and what
  // publishes the Dispatch record of a write that reached the CLI outside the form the gate reads
  // (`recordUnmarked`). The records' shutdown is the recorder's own, registered where the gate
  // binds (`askgate.ts`), so this entry holds no cut of its own.
  let askgateRecorder: AskGateRegistration | undefined;
  // The session's envoy.json as the gate's own reads go through it, set only where the switch is
  // on: the file object the session resolved, not a path rebuilt from it.
  let askgateFile: Extract<SessionConfigFile, { error: null }> | null = null;
  const configFile = CONFIG_FILE.error === null ? CONFIG_FILE : null;
  const ownDispatch = async (file: string): Promise<boolean> => {
    const own = await readEnvoyConfig(file, process.env, { cwd: process.cwd() });
    return (
      await dispatchSettings(process.env, own.kind === "read" ? { ...own, checkout: null } : own)
    ).enabled;
  };
  if (configFile !== null && (atStart.dispatch.enabled || (await ownDispatch(configFile.path)))) {
    const setting = await askgateSetting(configFile.path);
    if (setting.error !== null) askgateNotices.push(setting.error);
    if (setting.enabled) askgateFile = configFile;
  }
  // The record that makes the `dispatch` CLI require AskGate's marker for a gated write
  // (`@legion/envoy-client/askgate-marker`): written, while the gate is bound, for each session id
  // this top-level instance binds (`bindDispatchSession`; a subagent's instance binds none), and
  // kept for every one of them until shutdown, since a shell or kernel started before a switch
  // still acts as the session it started in. A gate that did not load refuses each literal call at
  // the hook (`loadFailureGate`), so those never reach the CLI; it mints no marker and records no
  // mode, so every gated write that does reach the CLI — one outside the literal form, and a
  // subagent's literal one — is refused where the session chose block (`OMP_ASKGATE=block`, which
  // `bindDispatchSession` writes beside the record), and at the warn default posts with a warning
  // and an `unmarked` entry.
  const askgateOwner: AskgateOwner | undefined =
    askgateFile === null ? undefined : selfOwner(sessionStateRoot(process.env));
  const askgateRecords = new Set<string>();
  // While this instance keeps a record, it touches its store every STORE_BEAT_MS (`keepStore`). A
  // process of another pid namespace that shares the state directory (the host, or another agent
  // box, while this process runs in one) cannot look this process up by its pid, so it counts the
  // record live, and its prune keeps the store, only while the store keeps changing. A beat also
  // makes the store again when another process removed it.
  let askgateBeat: Timer | undefined;
  // The listener client exists only while Envoy is on. The listener calls and the NATS connection
  // reach it through `requireEnvoy`, which refuses, saying why Envoy is off, so this session
  // reaches no listener and no NATS server its configuration does not name. Tools, `/whoami`,
  // inbound messages and the heartbeat exist only with Envoy on, so only the Legion interface's
  // `claim` and `subscribe` can meet that refusal. Every reply's `Date` tells how far this
  // machine's clock runs from the listener's, which each card's age states (`../src/server-clock`).
  const serverClock = new ServerClock(() => Date.now());
  const active = envoy.enabled
    ? {
        settings: envoy,
        client: createEnvoyClient({
          baseUrl: envoy.url,
          fetch: serverClock.observing(fetch),
          credential: envoy.credential,
          // The stop gate of the session's listener calls: once the stop has begun, no call of
          // its registration or role state is sent, its own hand-back and DELETE apart.
          stopping: () => shuttingDown,
        }),
      }
    : undefined;
  const requireEnvoy = (): NonNullable<typeof active> => {
    if (active === undefined) {
      throw new Error(
        `Envoy is off for this session: ${envoy.enabled ? "" : (envoy.error ?? envoy.notice)}`
      );
    }
    return active;
  };
  // The deadline each queued task gets, read afresh at every task start. Tolerant of Envoy being
  // off, unlike `requireEnvoy`, which throws: the queues exist whatever the session resolved, and
  // a queue cannot answer a throw with a deadline.
  const passDeadlineMs = (): number => active?.settings.passDeadlineMs ?? MIN_PASS_DEADLINE_MS;
  const delivered = createDeliveryDedupe();
  let connection: SessionInbound | undefined;
  // Stops what keeps `connection` for its life (`SessionInbound.keep`: a NATS connection's broker
  // token keeper): one per connection, replaced with the connection and stopped at shutdown.
  let stopInboundKeeper = (): void => {};
  let sessionDirectory = "";
  let sessionID = "";
  // The key this instance last published its session under, so a switch that changes the id or
  // the transcript path leaves no entry behind for a subagent to resolve (`src/envoy-session.ts`).
  let recordedSessionKey: string | undefined;
  let heartbeatRegistered = false;
  // The session the host last announced it was leaving (`session_before_switch`,
  // `session_before_branch`) and why: read until the next pass sees where the host went, to learn
  // whether the session it went to continues the one it left, and by the frame drop meanwhile. A
  // session event ends it wherever it lands (a `/reload` lands on the session it announced
  // leaving), and so does a run that started on that session under it and ended unaborted there:
  // a switch that goes ahead aborts a run still going, so that switch was cancelled. (A run that
  // starts and ends while another extension's announcement handler still runs reads the same, and
  // no host event tells the two apart.)
  let announced:
    | { readonly from: string; readonly reason: SessionSwitchReason | undefined }
    | undefined;
  // The announcement the current run started under, on the session it announced leaving.
  let runUnderAnnouncement: typeof announced;
  // The session each host session continues, for a session a fork or a branch opened from another:
  // its records are a copy of that session's, so that session's role and listener rows are its
  // own when its own records name none. A session opened any other way (`/new`, a resume, or a
  // move the host made with no word) continues nothing.
  const continues = new Map<string, string>();
  // `id`, then the sessions it continues (a fork's or a branch's parent, and so on).
  const continuedBy = (id: string): readonly string[] => {
    const lineage = [id];
    for (let at = continues.get(id); at !== undefined && !lineage.includes(at); ) {
      lineage.push(at);
      at = continues.get(at);
    }
    return lineage;
  };
  // The sessions the agent was told it now lives under (`session id changed`), each once.
  const notifiedSessions = new Set<string>();
  let activeSessionContext: SessionContext | undefined;
  const inbox: {
    event_id: string;
    at: string;
    from: string;
    summary: string;
  }[] = [];

  let askAwareness: AskAwarenessState = {
    session_id: "",
    period: 0,
    baseline_as_of: null,
    check_due: false,
    checks: 0,
  };
  // Bumped by everything that invalidates a stop-time check already in flight: a new user turn,
  // and a session change. The period alone cannot carry that — a rebound session's restored
  // period may equal the one the pending check read.
  let awarenessGeneration = 0;
  // The person input whose open-asks read is still out, when it is the newest awareness
  // generation. A stop that lands in that read, on that session, belongs to a run the person's
  // input supersedes, so it owes no check: it would otherwise take the bumped generation with the
  // old period's `check_due` and pass every staleness test. A read a newer input or a session
  // change superseded holds nothing back, since the generation has moved past it.
  let armingPersonInput: { readonly session: string; readonly generation: number } | undefined;
  // One stop-time check at a time. `agent_end` handlers are not awaited by the host, so a second
  // stop can arrive while the first one's Dispatch round trip or self-check is open — it clears
  // every guard below, because `check_due` is not spent until both answers are back. The
  // re-checks after each await then make it silent, but only after it has spent a round trip and
  // a model call on an answer that can change nothing. This makes "one check per stop window"
  // the structure rather than an ordering a re-check happens to win: held from before the query
  // until after `check_due` is spent.
  let askCheckInFlight = false;
  // A settle that arrived while a check was open, and passed every guard but that one: its run
  // started after the check's settle, so the open check will not steer, and it runs again for
  // this settle once it is done. Dropped, the newer settle's check would be lost whenever that
  // run finished inside the check's window — the usual case for an agent that acts on a reply
  // and stops within seconds. It carries the run count read at its own settle: the loop drops
  // it when a run has started since, so the re-check pays no Dispatch read for a settle the
  // session has moved past, and `checkAtSettle` judges staleness against that settle rather
  // than trusting its caller to have done so.
  let settledDuringCheck: AskSettle | undefined;
  const takeSettledDuringCheck = (): AskSettle | undefined => {
    const settle = settledDuringCheck;
    settledDuringCheck = undefined;
    return settle;
  };
  // The self-check in flight, so whatever invalidates it can stop it. Dropping the verdict is
  // not enough: the call is a whole-context request on the session's own model, and one the
  // user superseded by typing competes with the turn they are waiting on for the same provider.
  let askCheckAbort: AbortController | undefined;
  const abortSelfCheck = (reason: string): void => {
    askCheckAbort?.abort(new Error(reason));
    askCheckAbort = undefined;
  };
  // Every run the session has started, which is what a stop-time check compares with the value
  // it read at its settle. A run that starts while the check is open — an Envoy delivery woke
  // the session, or another extension continued it — means the verdict no longer describes
  // where the session is, and any work that run does owes the period a check of its own.
  let runSeq = 0;
  // Whether this session's transcript records Legion driving it. Not matched against the id the
  // guard runs under: `/fork` and `/handoff` mint a new id and carry the transcript, and the
  // session stays Legion-driven across one — the role-claim reader beside it is id-agnostic for
  // the same reason. `/new` and `/resume` install a transcript that is their own, so an empty or
  // replaced branch still reads as not-managed.
  let legionManagedTranscript = false;
  /** The bridge set is the process-local record; the transcript is what a fresh process reads. */
  const legionManaged = (id: string): boolean =>
    legionManagedTranscript || legionRoleClaimBridge().managedSessions.has(id);
  // The attempts of a person's direct message this session delivered, as a card or as a turn
  // (`HANDLED_ATTEMPT_ENTRY`): read from every entry of the session file, so a resume, a switch to
  // another branch of the tree, or both keep a delivered attempt from being accepted again.
  let handledDispatchAttempts = new Set<string>();
  // The session ids this instance noted a person's turn under (`noteTypedUserTurn`,
  // `noteInjectedUserTurn`). The record is process-wide, and an ACP host serves several top-level
  // sessions in one process, each with its own instance: leaving an id forgets only the turns
  // this instance sent in there, never another instance's.
  const notedSessionIDs = new Set<string>();

  const availabilityWarningSessionIDs = new Set<string>();

  const warnAskAvailability = (context: SessionContext, error: unknown): void => {
    const sessionID = context.sessionManager.getSessionId();
    if (availabilityWarningSessionIDs.has(sessionID)) return;
    availabilityWarningSessionIDs.add(sessionID);
    notify(
      context,
      `envoy: Dispatch open-ask check unavailable (${messageFor(error)}); the stop-time ask reminder is off until it recovers`,
      "warning"
    );
  };

  const overriddenTimeout = Number(process.env.ENVOY_SELF_CHECK_TIMEOUT_MS);
  const selfCheckTimeoutMs =
    Number.isFinite(overriddenTimeout) && overriddenTimeout > 0
      ? overriddenTimeout
      : ASK_SELF_CHECK_TIMEOUT_MS;
  const selfCheckWarningSessionIDs = new Set<string>();

  /** A self-check that failed is silent by design; only the log says it happened. */
  const logSelfCheckFailure = (failedSessionID: string, error: unknown): void => {
    if (selfCheckWarningSessionIDs.has(failedSessionID)) return;
    selfCheckWarningSessionIDs.add(failedSessionID);
    logger.warn("envoy: run-end waiting self-check failed; no reminder was sent", {
      sessionID: failedSessionID,
      error: messageFor(error),
    });
  };

  const queryOpenAsks = async (
    requestedSessionID: string,
    since?: string
  ): Promise<OpenAsksResponse | null> => {
    const dispatch = await timedDispatchClient(OPEN_ASKS_TIMEOUT_MS);
    if (dispatch === undefined) return null;
    const snapshot = await dispatch.openAsks(requestedSessionID, since);
    availabilityWarningSessionIDs.delete(requestedSessionID);
    return snapshot;
  };

  // The transcript's record that Legion drives this session, written through the write gate:
  // a stopping session writes none (R24 of dispatch://INFRA-3589).
  const markLegionManagedSession = (targetSessionID: string): void => {
    legionRoleClaimBridge().managedSessions.add(targetSessionID);
    if (legionManagedTranscript || shuttingDown) return;
    legionManagedTranscript = true;
    pi.appendEntry(LEGION_MANAGED_ENTRY, { session_id: targetSessionID });
  };

  // Adds `addresses` to `shown`, a session's set of pictures it was shown (`shownPictures`),
  // which later reads and deliveries name instead of sending again: the pictures a delivery
  // showed once the host took it, and those the live context of the transcript the session moved
  // onto, or compacted, already shows.
  const markPicturesShown = (shown: Set<string>, addresses: readonly string[]): void => {
    for (const address of addresses) shown.add(address);
  };

  const restoreLocalSessionState = (context: SessionContext): void => {
    const previousSessionID = sessionID;
    sessionDirectory = context.cwd;
    sessionID = context.sessionManager.getSessionId();
    // Only a top-level instance reaches here — a subagent's session_start and switch events
    // return early — so this publishes the session this process registered, whatever moved the
    // id: a start, a switch, or the heartbeat's drift heal. The subagent instances in this
    // process resolve it from their own transcript path as their reply address.
    recordedSessionKey = recordEnvoySession({
      previousKey: recordedSessionKey,
      sessionFile: context.sessionManager.getSessionFile(),
      sessionID,
    });
    activeSessionContext = context;
    const branch = context.sessionManager.getBranch?.() ?? [];
    // Only a genuine session change clears the armed period — a `/fork`, `/handoff`, resume or
    // switch, each of which mints a different id and leaves the period describing a
    // conversation this session is no longer in. Re-establishing the *same* session is not one:
    // the heartbeat's drift heal runs this for every fresh TUI once the host mints its id, and
    // the `session_start` NATS retry runs it every 15 s for the length of an Envoy outage.
    // Clearing on those disabled the nudge for exactly the sessions it was opened up for.
    if (askAwareness.session_id !== sessionID) {
      abortSelfCheck(`session changed from ${askAwareness.session_id || "none"} to ${sessionID}`);
      askAwareness = {
        session_id: sessionID,
        period: 0,
        baseline_as_of: null,
        check_due: false,
        checks: 0,
      };
      awarenessGeneration++;
    }
    legionManagedTranscript = branch.some(isLegionManagedEntry);
    const entries = context.sessionManager.getEntries();
    handledDispatchAttempts = handledAttempts(entries);
    // The turns of the conversation the session's terminal shows, for a viewer who opens the
    // session: a process that takes a session up records none of them itself
    // (`restoreAgentStream`). That is its live context: Oh My Pi draws a resumed session from the
    // latest compaction's cut on (`display.collapseCompacted`, on by default), and no body before
    // the cut is read (`liveContext`).
    const live = liveContext(branch);
    restoreAgentStream(sessionID, live, entries);
    // The delivery ledger, from every entry of the session file as handled attempts are: what
    // this session was shown, on any branch, with the cards the host wrote, and the topics it
    // follows. The notices it records held and not handed, which a
    // restart or a session change cut short, are held again for the first settled heartbeat.
    ledger?.restore(replaySnapshot(entries));
    unwritten.requeueNotices(heldReplayNotices(entries));
    // The pictures the live context of the transcript the session is now on already shows (a
    // fork's, a handoff's, a resume's, a restart's) are in the history every request sends, so
    // they count as shown; one a compaction or a `/clear` cut off is no longer sent, so it does
    // not, and no entry before that cut is read (`liveContext`). The id the session left is no
    // longer served: what it was shown is forgotten, and so are the turns this instance sent into
    // it (every match asks under the live id; another instance in the process may still serve the
    // id) and its typed turns already handed their pictures (`TypedPictures.dropOtherSessions`
    // says which survive).
    markPicturesShown(shownPictures(sessionID), transcriptPictures(live));
    if (previousSessionID !== sessionID) {
      forgetShownPictures(previousSessionID);
      if (notedSessionIDs.delete(previousSessionID)) forgetInjectedUserTurns(previousSessionID);
      typedPictures.dropOtherSessions(sessionID);
      // What the session left was handed and has not written is no longer waited on.
      unwritten.releaseOtherSessions(sessionID);
      // A `/new`, a fork or a branch mints a new id, and nothing heartbeats the one it left from
      // here on. Left in the registry that id reads as live for the live window and as
      // unresponsive for the gone horizon after it, so peers go on sending to a session that no
      // longer exists (dispatch://INFRA-392). The DELETE goes out behind that id's own
      // registration (`deregisterSession`) and off this chain: no session event waits on it.
      if (previousSessionID !== "") {
        void deregisterSession(previousSessionID).catch((error: unknown) => {
          logger.warn("envoy: the session this process left could not be deregistered", {
            sessionID: previousSessionID,
            detail: messageFor(error),
          });
        });
      }
    }
  };

  // Where this instance has read the `dispatch` command's ledger to (`results.jsonl` in the
  // session's state directory, a line per call, none for `dispatch proxy` or `dispatch login`), so
  // the run-end check sees each call once.
  let dispatchLedger: { readonly sessionID: string; offset: number } | undefined;

  // Names the top-level session in the environment every later shell command takes, and starts
  // its ledger at the end, so a resumed or switched session never reads an earlier run's calls.
  // It also names the CLI state root (`DISPATCH_STATE_DIR`, unless it is already set), so a
  // command that changes HOME or XDG_STATE_HOME still finds the session's ledger and AskGate
  // record. Oh My Pi refreshes the shell's environment after `session_start`, `session_switch`
  // and `session_branch`; any other caller refreshes it itself. A caller after `session_start`
  // passes `ui`, so a record it cannot write is a notice at once: the start notices are shown once.
  // A session with no id yet names none, as "" rather than a delete: a delete reaches
  // `process.env` alone, and the C environment the process started with keeps a value it
  // inherited (an omp started from another session's shell), which a child Oh My Pi starts in a
  // PTY begins from with the variables built from `Bun.env` laid over it. "" lies over that value,
  // and every reader takes "" as unset (the `dispatch` CLI, `legion git`, this file, AskGate).
  const bindDispatchSession = (id: string, ui?: SessionContext["ui"]): void => {
    process.env.DISPATCH_STATE_DIR ||= sessionStateRoot(process.env);
    if (id === "") {
      process.env.DISPATCH_SESSION_ID = "";
      dispatchLedger = undefined;
      return;
    }
    process.env.DISPATCH_SESSION_ID = id;
    if (askgateOwner !== undefined) {
      const dir = dispatchSessionDirectory(process.env, id);
      try {
        // The session's own check mode at the bind: block when it chose block through OMP_ASKGATE,
        // which survives a gate that does not load, or when this process already recorded block for
        // this session id. The gate reads an arm per call and records it in the owner file, so a
        // rebind (a start, a switch, a branch, a host move) keeps a recorded block rather than turn
        // it back to warn before the gate's next call; the gate's call still sets warn when the arm
        // is off. Kept per session in its owner file, so a switch never changes another session's.
        // The write makes this process's store first, so the record never names a missing one.
        const recorded = recordedOwnerMode(dir, askgateOwner);
        writeAskgateRecord(
          dir,
          askgateOwner,
          process.env.OMP_ASKGATE?.trim() === "block" || recorded === "block" ? "block" : "warn"
        );
        askgateRecords.add(dir);
        if (askgateBeat === undefined) {
          const { store } = askgateOwner;
          askgateBeat = setInterval(() => {
            try {
              keepStore(store, Date.now());
            } catch (error) {
              logger.warn(
                `AskGate could not touch its marker store ${store}: ${messageFor(error)}`
              );
            }
          }, STORE_BEAT_MS);
          askgateBeat.unref();
        }
      } catch (error) {
        const notice = `AskGate could not write its record for session ${id} (${messageFor(error)}), so the dispatch CLI does not ask for AskGate's marker there: a Dispatch write AskGate cannot read posts unreviewed.`;
        logger.warn(notice);
        if (ui === undefined) askgateNotices.push(notice);
        else notify({ ui }, `envoy: ${notice}`, "warning");
      }
    }
    dispatchLedger = {
      sessionID: id,
      offset: resultsEnd(dispatchSessionDirectory(process.env, id)),
    };
  };

  // The ledger lines appended since the last read, under the id the `dispatch` command writes
  // under, which is the environment's and not a fresh `getSessionId()`. A line that is not JSON is
  // logged with its file and line number and skipped, so it never stalls the run-end check.
  const readNewDispatchResults = () => {
    const id = process.env.DISPATCH_SESSION_ID;
    if (id === undefined || id === "") return [];
    const dir = dispatchSessionDirectory(process.env, id);
    if (dispatchLedger?.sessionID !== id) {
      dispatchLedger = { sessionID: id, offset: resultsEnd(dir) };
      return [];
    }
    const read = readResultsSince(dir, dispatchLedger.offset, (problem) =>
      logger.warn(`envoy: ${problem}`)
    );
    dispatchLedger.offset = read.offset;
    return read.entries;
  };

  pi.on("resources_discover", async () => ({ skillPaths: [SKILLS_DIRECTORY] }));
  registerEnvoyMessageRenderer(pi);

  // Refuses, saying why inbound messages are off (`inbound.off`: no NATS server, the seed's
  // refusal, or a relay socket the settings cannot open). A subscription and a Legion claim both
  // ask it first: nats.js dials 127.0.0.1:4222 when handed no server, which would attach this
  // session to whatever NATS runs there, and a role's every task and notice reaches its holder
  // only over its inbound connection.
  const requireInbound = () => {
    const { inbound } = requireEnvoy().settings;
    if ("off" in inbound) {
      throw new LoggableError(`inbound messages are off for this session: ${inbound.off}`);
    }
    return inbound;
  };

  // Opens this session's inbound connection and stores nothing: `ensureConnection` keeps the one
  // this session uses, and a queued task dials through its handle, which closes a connection the
  // task was refused while the dial was in flight rather than leave it dangling. Which of the two
  // it is is decided here, once, from the settings, and travels with the connection
  // (`SessionInbound`): its name, whether it carries a live view, how it follows the session the
  // host moves to, and what keeps it.
  //
  // Under `ENVOY_INBOUND=listener` it is the listener's relay socket for the session this process
  // runs now, presenting that session's broker token (`connectListenerRelay`), answered once a
  // socket is open within the same bound a NATS start's trade waits, and within what a queued
  // task (`limit`) has left before it must confirm a subscription and register; the socket opens
  // again on its own after a close, and a token the broker refuses is this session's inbound
  // outage, told once (`inboundOutage`).
  //
  // A NATS dial that fails is dialed again (`connectNats`): the hub closes a dial whose CONNECT the
  // host's busy start delayed past its 2 s authentication timeout, and answers the next one. A
  // queued task's dial (`limit`) redials only while the task can still confirm a subscription and
  // register after it (`PASS_AFTER_DIAL_MS`), and stops once the task stops acting.
  const dialInbound = async (limit?: DialLimit): Promise<SessionInbound> => {
    const inbound = requireInbound();
    if ("relaySetting" in inbound) {
      const boundMs = startTradeBoundMs(passDeadlineMs());
      const leftMs =
        limit === undefined ? boundMs : limit.deadlineAt - performance.now() - PASS_AFTER_DIAL_MS;
      const socket = await connectListenerRelay(
        {
          url: requireEnvoy().settings.url,
          session: () => sessionID,
          credential: inbound.relayCredential,
          outage: inboundOutage,
          log: (line) => logger.warn(`envoy: ${line}`),
          // A socket that closes once the stop has begun is not opened again: the stop drains
          // the connection only after its hand-back.
          stopping: () => shuttingDown,
        },
        AbortSignal.timeout(Math.max(0, Math.min(boundMs, leftMs)))
      );
      return sessionInbound(socket, {
        name: "relay socket",
        liveView: false,
        follow: (followMs) => socket.follow(followMs),
        keep: () => () => undefined,
      });
    }
    const { natsUrls, natsCredential } = inbound;
    // A broker token is signed into the CONNECT itself, so a live one is taken before the dial.
    // The start has nothing to do until it holds one, so it waits for ready within its bound
    // (`TRADE_DEADLINE_MS`, or the pass deadline less the 10 s the rest of the pass needs where
    // that is shorter, or half a pass shorter than 20 s, `startTradeBoundMs`): a first trade's
    // prompt 503 is traded again inside it rather than costing the session its registration until
    // the next heartbeat, and a broker that never grants fails the start naming the trade's cause.
    const broker = natsCredential.kind === "broker" ? natsCredential.broker : undefined;
    if (broker !== undefined) {
      await broker.ready({
        waitForReady: true,
        boundMs: startTradeBoundMs(passDeadlineMs()),
      });
    }
    const standIn = natsConnectStore[NATS_CONNECT];
    const plan = standIn?.plan ?? NATS_DIAL_PLAN;
    // What the task has left once the token is held, which the wait for it spent from.
    const left = limit === undefined ? Infinity : limit.deadlineAt - performance.now();
    // nats.js re-subscribes existing subscriptions on its own once reconnected.
    const nats = await connectNats(
      standIn?.connect ?? connect,
      natsConnectionOptions({
        servers: natsUrls,
        name: clientConnectionName(`omp-${sessionID || "unknown"}`, connectionBuild()),
        credential: natsCredential,
      }),
      { ...plan, budgetMs: natsDialBudget(left, PASS_AFTER_DIAL_MS, plan) },
      {
        ...standIn?.waits,
        firstBoundMs: left,
        stopped: () => shuttingDown || limit?.stopped() === true,
        // A broker token the hub refuses twice is dropped and another trade started at once, once
        // for the session until a dial is admitted, as the keeper does once the connection is open.
        credential: natsCredential,
        // Said at the drop, so a drop at the last dial the pass can afford is logged too.
        onTokenDropped: (_error, attempt) =>
          logger.warn(`envoy: NATS dial ${attempt}: ${NATS_DIAL_TOKEN_DROPPED}`),
        // Said before the wait: a pass that stops, or runs out of time, during it dials no more,
        // so the line says the next dial waits on that.
        onRetry: (error, delayMs, attempt) =>
          logger.warn(
            `envoy: NATS dial ${attempt} failed (${natsFailureReason(error)}); dialing again in ${delayMs} ms, unless the pass stops or runs out of time first`
          ),
      }
    );
    return sessionInbound(nats, {
      name: "NATS connection",
      liveView: true,
      // The auth service grants a session's token that session's subjects alone, so a NATS
      // connection presenting a broker token is admitted again with the token of the session the
      // host switched to (`followNatsCredential`).
      follow: async (followMs) => {
        if (broker !== undefined) await followNatsCredential(nats, broker, followMs);
      },
      // Told as every other inbound outage is, in the log and as a notice: a token that cannot be
      // renewed, or that the server refuses, is a session that stops receiving. The keeper says
      // each cause once and again every 30 minutes while it lasts, so the notice does too.
      keep: () =>
        broker === undefined
          ? () => undefined
          : keepNatsCredential(nats, broker, (text) => {
              logger.warn(`envoy: ${text}`);
              notify(activeSessionContext, `envoy: ${text}`, "warning");
            }),
    });
  };

  // The session's one inbound connection, dialled if it has none. A queued task reaches it through
  // its handle's `connect`, which hands in the task's own fenced dial and checks the task again
  // once this whole call has answered: the wait below, after a dial another caller's connection
  // beat, is inside the step rather than after it.
  const ensureConnection = async (
    dial: () => Promise<SessionInbound> = dialInbound
  ): Promise<SessionInbound> => {
    if (connection?.isClosed() === false) return connection;
    const dialled = await dial();
    // Another pass can establish the connection, and the session's shutdown drop it, while this
    // dial is in flight: cleanup drains `connection` alone, and `maxReconnectAttempts: -1` keeps
    // anything else alive for the process's life. The loser of either race is this dial, closed
    // here rather than left dangling.
    if (shuttingDown || connection?.isClosed() === false) {
      await dialled.close().catch(() => {});
      if (!shuttingDown && connection?.isClosed() === false) return connection;
      throw new LoggableError(
        `the session shut down while its ${dialled.name} was being opened; the connection it opened was closed`
      );
    }
    connection = dialled;
    void watchReconnects(dialled);
    stopInboundKeeper();
    stopInboundKeeper = dialled.keep();
    return connection;
  };

  // nats.js resubscribes every subscription once it reconnects, and the relay socket takes every
  // one again on each socket it opens, but nothing published while it was away is kept for it:
  // each reconnect begins a new epoch and registers it at once, so the listener restarts the
  // session's relay at its ack floor and redelivers what the session never acknowledged
  // (`renewRelayEpoch`). The reader ends with the connection, and stops acting once another
  // connection replaced it.
  const watchReconnects = async (watched: SessionInbound): Promise<void> => {
    try {
      for await (const status of watched.status()) {
        if (connection !== watched) return;
        if (status.type !== Events.Reconnect) continue;
        // The connection answers again, and nats.js has sent every live subscription on it afresh,
        // so the next open pays its own bound rather than going on unconfirmed.
        unanswered.delete(watched);
        renewRelayEpoch();
      }
    } catch (error) {
      logger.warn(`envoy: the ${watched.name}'s status reader failed`, {
        error: natsFailureReason(error),
      });
    }
  };

  // The sessions a delivery is under way for, each with how many: a delivery's Dispatch calls -
  // the read of its body whole, the accept, the picture reads, the reply - resolve its session's
  // broker client and present that client's token, and the work between them (a person's
  // `!command`, a side turn, the host's run of a typed input) can outlast a move away from the
  // session. A pass that forgot the client meanwhile (`leftSessions`) would have the next call
  // trade again under a session this process has left. So the session a delivery is addressed to
  // is held from the delivery's start (`deliverRelayed`) until the last work it handed off ends
  // (`offWorker`, the shell and typed-input runners), and the pass leaves a held session in
  // `leftSessions` for a later pass, which forgets it once nothing holds it.
  const heldSessions = new Map<string, number>();
  const underWay = async <T>(session: string, call: () => Promise<T>): Promise<T> => {
    heldSessions.set(session, (heldSessions.get(session) ?? 0) + 1);
    try {
      return await call();
    } finally {
      const remaining = (heldSessions.get(session) ?? 1) - 1;
      if (remaining > 0) heldSessions.set(session, remaining);
      else heldSessions.delete(session);
    }
  };

  // Posts one reply to a person's message, as the session `replySessionID` names and under the
  // Dispatch resolved here for that same session: the two halves of a reply - the actor it
  // names and the configuration it goes out under - are read from the one argument, so they
  // cannot name two different sessions.
  //
  // A reply names the session its message reached - the one the frame was addressed to, or,
  // for a frame that addressed none, the session that took it - whether or not the host still
  // has that session open: a configuration resolved for the session open when the reply goes
  // out would sign one person's answer with the conversation the host moved to. A session this
  // host has left is logged here, so every reply to one says so - twice for a reply whose
  // first text Dispatch refuses (`postTypedInputReply`) - since the broker may refuse a token
  // for a session this machine no longer runs and the refusal would otherwise read as a plain
  // reply failure.
  //
  // A reply Dispatch made and gave no copy of its answer for (`DispatchWriteLandedError`: `409
  // KEY_COMMITTED`, or a replay of an answer it kept none of) is posted: the attempt is answered
  // with it, so it resolves as any posted reply does, and its line says it landed. Every caller
  // then goes on as after a post - a BTW's side turn is taken, a person's turn is settled - and
  // none answers the same attempt again with an error, or says the delivery failed.
  const postDispatchReply = async (
    delivery: DispatchDelivery,
    result: { readonly body?: string; readonly error?: string },
    replySessionID: string
  ): Promise<void> => {
    if (!sessionOpen(replySessionID)) {
      logger.warn("envoy: a reply is signed as the session its message reached, which is closed", {
        addressed: replySessionID,
        open: sessionID,
      });
    }
    try {
      await postDeliveryReply(
        await dispatchConfigFor(replySessionID),
        replySessionID,
        delivery,
        result
      );
    } catch (error) {
      if (!(error instanceof DispatchWriteLandedError)) throw error;
      logger.warn("envoy: a reply landed on Dispatch, which gave no copy of its answer", {
        resource: delivery.resource,
        id: delivery.id,
        attempt: delivery.attempt,
        error: error.message,
      });
    }
  };

  // A typed input's pictures, until the host's message carrying its Dispatch id starts
  // (`src/typed-pictures.ts`).
  const typedPictures = new TypedPictures();
  pi.on("input", async (event, context) => {
    // An input that finds the host idle is a prompt sent from idle: any run, one the host dropped
    // with no `agent_end` included, is over (`../src/card-wake`).
    cardWake.noteIfIdle(context);
    if (event.source !== "extension") return undefined;
    const images = typedPictures.handOver(event.text);
    return images === undefined ? undefined : { images: [...(event.images ?? []), ...images] };
  });

  // How a card that starts a turn goes, and the wake for one the host folds into the context with
  // no turn (`../src/card-wake`). A card the host's dispose flushes at shutdown wakes nothing. The
  // start-turn mark the Legion entry keeps, as published at this build's interface version, holds
  // the startup phase while it names the session open here (`sessionOpen`: this instance's own and
  // the host's live one), never a `task` subagent's.
  const cardWake = new CardWake({
    now: () => Date.now(),
    sendMessage: (message, options) => pi.sendMessage(message, options),
    startupTurn: () => publishedInterface?.startupTurn,
    isOwnSession: (session) => sessionOpen(session),
  });

  // When each card reached each session this process serves, which every model request shows
  // beside the card's send time (`../src/card-age`): a card's delivery in a session is the one the
  // session records for it, else what that session's own `message_end`, ledger record or entry
  // position gives, so a session the host moves to reads when it was shown the card itself. The
  // listener's clock, from its replies, says how much of the gap this machine's clock made.
  const cardAges = new CardAges({
    now: () => Date.now(),
    clockOffset: () => serverClock.offset(),
    // The delivery a session first gives a card goes into its file, so every later request, and
    // the first of a process that resumes the session, shows the card with the same bytes.
    // `CardAges` calls this synchronously while the request or the card's `message_end` is
    // handled, so the session it names is the one the host is on and no guard is needed, unlike
    // the answers record, which is written after an await. A write that throws is logged; the
    // request still shows the card's time, and a process that resumes the session takes it from
    // the card's ledger record or its place in the session.
    record: (session, entry) => {
      try {
        pi.appendEntry(CARD_DELIVERED_ENTRY, entry);
      } catch (error) {
        logger.warn(
          `envoy: the time card ${entry.event_id} reached session ${session} could not be recorded in the session file`,
          { error: messageFor(error) }
        );
      }
    },
  });

  // A card's message: its `details` name the frame (`ledgerIdentity`), which the host keeps
  // on the message it writes, beside the wake's card marker, and the Dispatch message the card
  // delivers to the session, if it delivers one: the live view draws that message from Dispatch's
  // store, so the agent stream leaves its card out (`DispatchMessageCardDetails`). An `inert` card
  // is a steer that starts no turn on an idle host; a turn streaming when it is handed takes it and
  // runs another model turn for it, so `UnwrittenFrames` hands an inert card only to a settled host
  // (`hostSettled`). It carries `details` of its own all the same, the hand-over state `inert`, so
  // a reading of the stored transcripts says which state held every waiting card; it carries no
  // `envoyCard`, since the wake tracks no inert card and an id there would name nothing.
  // Every other card goes as the wake says (`CardWake.deliveryFor`): an aside with `triggerTurn`,
  // never a steer, so no card waits in the steering queue a person's typed message uses; the wake
  // records a startup card, which goes as a follow-up, before the host has it.
  const handCard = (
    content: string | readonly ContentBlock[],
    deliverAs: CardDelivery,
    identity: DedupeIdentity | undefined,
    dispatchMessageId: string | undefined
  ): void => {
    const known = {
      ...(identity === undefined ? {} : ledgerIdentity(identity)),
      ...(dispatchMessageId === undefined
        ? {}
        : ({ dispatchMessageId } satisfies DispatchMessageCardDetails)),
    };
    if (deliverAs === "inert") {
      pi.sendMessage(
        {
          customType: "envoy-message",
          content,
          display: true,
          details: { ...known, handOver: "inert" } satisfies Pick<EnvoyCardMarker, "handOver">,
        },
        { deliverAs: "steer", triggerTurn: false }
      );
      return;
    }
    const { details, ...options } = cardWake.deliveryFor(
      deliverAs === "aside",
      activeSessionContext
    );
    pi.sendMessage(
      { customType: "envoy-message", content, display: true, details: { ...known, ...details } },
      options
    );
  };

  // The host is idle, holds no message, is not shutting down, and is on the session this instance
  // serves: only then is an inert card handed, or a withdrawn card handed back (`UnwrittenFrames`).
  const hostSettled = (): boolean => {
    const hostContext = activeSessionContext;
    return (
      hostContext?.isIdle?.() === true &&
      hostContext.hasPendingMessages?.() === false &&
      !shuttingDown &&
      hostSessionID() === sessionID
    );
  };

  const unwritten = new UnwrittenFrames({
    sessionID: () => sessionID,
    // Whether the host is on `card`, the conversation a card handed now reaches: a card for any
    // other session is handed to nobody. The host's live id alone, not `sessionOpen`'s two
    // halves: between the host's move and the switch event that tells this instance of it, a
    // frame addressed to the session the host moved to - a fork carries its parent's subject on
    // - is still that session's to be shown.
    hostIsOn: (card) => hostSessionID() === card,
    settled: hostSettled,
    recordDelivered: async (card, identity) => {
      await ledger?.recordDeliveredFor(card, identity);
    },
    handCard,
    // A card the host would be handed in the conversation it moved to: nothing was handed, and
    // nothing of the frame counts as shown. Only a Dispatch delivery is named for a session the
    // host can have left (`cardSession`), and that frame is sent to that session again: the
    // claim goes back with nothing recorded, so the relay answers `nak` and the listener hands
    // the frame to whoever takes the session next (`deliverRelayed`).
    cardNotShown: (card, identity) => {
      logger.warn("envoy: a card was not shown here; the session it names is closed", {
        addressed: card,
        eventID: identity?.event_id,
        open: sessionID,
      });
    },
    handNotice: (content) => {
      pi.sendMessage(
        { customType: "envoy-message", content, display: true, details: LOCAL_ENVOY_NOTICE },
        { deliverAs: "steer", triggerTurn: false }
      );
    },
    matchTurn: (message) => matchInjectedUserTurn(sessionID, message),
    turnWithdrawn: (turnSessionID, messageID) => {
      dropInjectedUserTurn(turnSessionID, messageID);
      typedPictures.withdraw(messageID);
    },
    handBackFailed: (identity, error) => {
      logger.warn("envoy: a card waiting for a settled host could not be handed", {
        eventID: identity.event_id,
        error: messageFor(error),
      });
    },
    handNoticeFailed: (error) => {
      logger.warn("envoy: a replay notice waiting for a settled host could not be handed", {
        error: messageFor(error),
      });
    },
    // A held notice goes to the transcript of the session it was raised for, which the host is on
    // while this instance serves it.
    recordNotice: (id, content) => {
      if (hostSessionID() !== sessionID) throw new Error("the host is on another session");
      pi.appendEntry(REPLAY_NOTICE_ENTRY, { id, content });
    },
    noticeHanded: (id) => {
      pi.appendEntry(REPLAY_NOTICE_ENTRY, { id, handed: true });
    },
    recordNoticeFailed: (error) => {
      logger.warn("envoy: a held replay notice could not be recorded in the session file", {
        error: messageFor(error),
      });
    },
  });
  // A held notice of the extension's own (`UnwrittenFrames.sendNotice`), through the tell gate:
  // every replay, outage and refusal notice goes through this.
  const sendNotice = (content: string): void => {
    if (!shuttingDown) unwritten.sendNotice(content);
  };

  // Shows `session` a card the delivery ledger asked for (a failure notice naming a frame the
  // session was never shown) under a claim of that frame, so a live copy arriving meanwhile is
  // a repeat: `queued` while it waits to be written (`UnwrittenFrames.sendCard`), `shown` when
  // it does not wait, and `failed` when that session is closed, which is the word for a card
  // nobody was shown: nothing was handed and nothing is recorded, and the ledger reports it as
  // a notice that could not be shown (`failureNoticeFailed`). A frame the live path is
  // delivering now is that path's to show and record, and one whose card waits already is that
  // card's. A frame the dedupe let go of that the open session's ledger does not hold (one
  // shown before the host moved to this session, say) is handed with no claim, and waits on
  // its card's write all the same: nothing records it until the host writes it.
  const showReplayCard = (
    session: string,
    content: string,
    envelope: unknown,
    deliverAs: CardDelivery = "card"
  ): "shown" | "queued" | "failed" => {
    const parsed = ClaimIdentitySchema.safeParse(envelope);
    const identity = parsed.success ? parsed.data : undefined;
    const claim = delivered.claim(identity);
    if (
      claim === undefined &&
      identity !== undefined &&
      (delivered.pending(identity) || unwritten.waitsOnCard(identity))
    ) {
      return "queued";
    }
    const outcome = unwritten.sendCard(session, content, deliverAs, identity, claim);
    return outcome === "closed" ? "failed" : outcome;
  };

  // The event ids of BTW frames this process met, live or replayed, so a failure notice naming one
  // is inert too: a BTW never starts a turn. Bounded: the oldest is forgotten first.
  const btwEvents = new Set<string>();
  const noteBtw = (eventID: string | undefined): void => {
    if (eventID === undefined) return;
    btwEvents.delete(eventID);
    btwEvents.add(eventID);
    if (btwEvents.size > BTW_EVENTS_LIMIT) {
      const oldest = btwEvents.values().next();
      if (oldest.done !== true) btwEvents.delete(oldest.value);
    }
  };

  // How long a person's `!command` may run in this session before it is stopped, with what it
  // printed kept. Read inline, as this file's other knobs are.
  const SHELL_COMMAND_LIMIT_MS = Number(process.env.ENVOY_SHELL_COMMAND_LIMIT_MS ?? "") || 120_000;

  // The `!command` running now for each session, and the kill of its process group. The key is
  // the session the command was addressed to, which is the session it runs for and answers as:
  // one command runs for each session at a time, so a second `!` sent to the SAME session is
  // answered naming that session's running command rather than queued behind it, while a `!`
  // sent to another session runs on its own - the host moves between conversations (`/new`,
  // `/resume`) while a command runs, and the conversation it moved to is running nothing. A
  // single slot for the whole instance answered a `!` to the session the host moved to with
  // the command of the session it left, which was neither that session's command nor a command
  // that session was running. The kill is what the shutdown stops each of them with, since a
  // command's group is its own and would otherwise outlive the session (`stop`, set as the
  // command starts).
  const runningShellCommands = new Map<string, { readonly command: string; stop: () => void }>();

  // Posts the session's answer to a person's typed line, and falls back on Dispatch's own words
  // when Dispatch refuses it. Nothing cuts the reply here, so a reply Dispatch refuses - one
  // holding a block longer than it takes, or output it cannot store - would otherwise leave the
  // person who sent the line with no answer at all; Dispatch's words say why, and they are short
  // enough to post in its place. Both the typed path and the `!command` path post through here,
  // so the fallback exists once. Both texts name the one session, so the fallback is signed as
  // the reply it replaces was.
  const postTypedInputReply = async (
    delivery: DispatchDelivery,
    replySessionID: string,
    body: string
  ): Promise<void> => {
    try {
      await postDispatchReply(delivery, { body }, replySessionID);
    } catch (error) {
      if (!(error instanceof DispatchServiceError) || !error.fromDispatch) throw error;
      await postDispatchReply(
        delivery,
        { body: `Your command ran, and Dispatch refused its output: ${error.message}` },
        replySessionID
      );
    }
  };

  // Runs a person's `!command` for them (`../src/typed-shell`). The host is handed nothing: the
  // session runs the rest of the line under the host's own shell configuration, in its own
  // working directory (what still differs from the CLI's own bang command is listed in
  // `../src/typed-shell`'s header), and what it printed goes three ways - the live view's
  // conversation, as the person's own user message, since they ran it and the host wrote
  // nothing; their message on Dispatch, as its reply, which every
  // such message gets; and the agent's transcript, which it reads at its next turn with no turn
  // started for it. The command runs on this detached promise and never inside the queued delivery
  // task, which takes a pass deadline a two-minute command would blow, holding every other Dispatch
  // message for the session behind it.
  const runShellCommandTurn = (
    delivery: DispatchDelivery,
    replySessionID: string,
    turn: AcceptedUserTurn,
    command: string,
    taken: () => Promise<void>
  ): void => {
    // The host is handed nothing, so this message's pictures are released, as every other
    // non-submitted typed input releases them.
    typedPictures.withdraw(turn.messageId);
    // The session the message was accepted for, which every answer to it names: the stream
    // frame, the agent's copy and the reply on Dispatch. Never `sessionID`, which is read here
    // after the delivery has awaited the Dispatch accept and the picture load: a `/new` or a
    // `/resume` whose `session_switch` lands inside that wait has moved it to another
    // conversation, and this command was asked of neither that conversation nor its directory.
    const session = replySessionID;
    // The command this session is running now, if it is running one. Read under the addressed
    // session and no other: a command another session runs is not this session's, and says
    // nothing about whether this session can run a `!`.
    const running = runningShellCommands.get(session);
    // Read before anything is claimed, so a refusal leaves no claim behind: a session the host
    // has left runs nothing, and the directory below is that other conversation's by then.
    const open = sessionOpen(session);
    // The directory this instance recorded beside the session id it is on
    // (`restoreLocalSessionState` writes the two together), so wherever `open` holds - this
    // instance and the host alike still name the addressed session - it is that session's own
    // directory. A `session_tree` move keeps the session id and so skips
    // `restoreLocalSessionState`: a navigation that moved the directory leaves this one behind,
    // and this plugin cannot reach a session's own context by id to do better.
    const cwd = sessionDirectory || process.cwd();
    // Every text this message is answered with opens with the person's line as they sent it and
    // as Dispatch stored it, never the trimmed command: that is what the live view reads a
    // report back against (`isShellCommandReport` in `@legion/contracts`), and an opening it
    // cannot match leaves `! ls` drawn twice, the person's line and then the report.
    const line = turn.body;
    // Why this session runs no command, read once here: the claim below is taken only when there
    // is no answer, and `ran` answers the person with the one read here rather than reading the
    // three conditions again.
    const refusalFor = (): string | undefined => {
      // A session shutting down starts no command: what it started would outlive the session it
      // was asked of, and nothing here would be left to answer for it.
      if (shuttingDown) return shellCommandShutdownReport(line);
      // The session this message was addressed to is not the one open here any more, by this
      // instance's own id or by the host's live one. The command would run in the conversation
      // the host moved to, in its directory, so nothing is run and the person is told that.
      if (!open) return shellCommandMovedReport(line);
      // This session is running a command already, so this one is refused naming that command
      // rather than queued behind it.
      if (running !== undefined) return shellCommandBusyReport(line, running.command);
      return undefined;
    };
    const refusal = refusalFor();
    // Claimed before the command starts rather than from inside it, so a second `!` handed to
    // this session in the same tick is answered naming this one, and claimed only for a command
    // that will run, so a refusal leaves none behind. The claim is this session's alone: a
    // command another session is running leaves this one free to run its own.
    const claim = { command, stop: () => {} };
    if (refusal === undefined) runningShellCommands.set(session, claim);
    const ran = async (): Promise<string> => {
      if (refusal !== undefined) return refusal;
      // No await stands between the `open` read above, the directory read for it, the refusal
      // read from them and the spawn this call makes: `ran` is called in the same run that read
      // them and reaches here in one synchronous run, and `runShellCommand` spawns before its
      // own first await (`src/typed-shell.ts`). That is what makes the guard and the
      // directory describe the same moment as the spawn. One await inserted anywhere on that
      // path lets a `/new` or a `/resume` land inside it, and the whole command then runs for
      // the conversation the host moved to, in its directory.
      try {
        const result = await runShellCommand(command, {
          cwd,
          limitMs: SHELL_COMMAND_LIMIT_MS,
          started: (stop) => {
            claim.stop = stop;
          },
        });
        // Whether the agent of the session the command was asked of is handed a copy: that
        // session counts as open only while this instance and the host alike still name it. A
        // host that moved on is handed nothing, and the report the person and the live view
        // read says so rather than leaving them to assume the agent read the output.
        const handed = sessionOpen(session);
        const printed = shellCommandReport(line, result);
        const report = handed ? printed : shellCommandWithheldReport(printed);
        // The frame carries the Dispatch message id, so the live view re-keys it over the
        // person's Dispatch turn instead of drawing the line twice.
        if (session !== "") {
          agentStreamOf(session).record(
            agentStreamFramesSubject(session),
            {
              content: [{ text: report, type: "text" }],
              role: "user",
              tag: turn.messageId,
              timestamp: Date.now(),
            },
            false,
            turn.messageId
          );
        }
        // The agent's copy names the person's message it answers: a host busy with a turn writes
        // it at its next prompt and fires `message_end` for it then, and the live view already
        // holds the report above, keyed on that message, so the stream leaves the copy out
        // (`DispatchMessageCardDetails`).
        if (handed) {
          pi.sendMessage(
            {
              content: shellCommandMessage(report),
              customType: "envoy-message",
              details: {
                ...LOCAL_ENVOY_NOTICE,
                ...({ dispatchMessageId: turn.messageId } satisfies DispatchMessageCardDetails),
              },
              display: true,
            },
            { deliverAs: "nextTurn", triggerTurn: false }
          );
        }
        return report;
      } finally {
        if (runningShellCommands.get(session) === claim) runningShellCommands.delete(session);
      }
    };
    const answer = async (): Promise<void> => {
      try {
        // Posted for the session the message was addressed to, under that session's own
        // Dispatch (`postDispatchReply` resolves it): a configuration resolved for the session
        // open when the command ends would have a `/new` or a `/resume` in this window write
        // the reply to the person's message under the conversation the host moved to.
        await postTypedInputReply(delivery, session, await ran());
      } catch (error: unknown) {
        logger.warn("envoy: a person's shell command from Dispatch was not answered", {
          attempt: delivery.attempt,
          error: messageFor(error),
          messageID: delivery.id,
        });
      } finally {
        // The host writes nothing for this line, so no turn is awaited: the ledger is asked to
        // record the message here and its claim settles. The record is asked for whether or not
        // the command ran, the frame was published or the reply reached Dispatch, since a
        // redelivery would run the command again. It reaches the transcript only while the
        // addressed session is still the one this process writes to; where it is not, the claim
        // settles all the same and the delivery the transcript lost is logged (`taken`).
        await taken();
      }
    };
    // Held for the addressed session until the reply is posted (`underWay`): the command can
    // outlast a move away from it.
    void underWay(session, answer).catch((error: unknown) => {
      logger.warn("envoy: a person's shell command from Dispatch was not recorded as taken", {
        attempt: delivery.attempt,
        error: messageFor(error),
        messageID: delivery.id,
      });
    });
  };

  // Runs a person's accepted Send or Aside as if typed at the terminal (`pi.sendUserInput`): a
  // command, a skill, a template, or a prompt. Never awaited here: on an idle session the host
  // answers only once the run it starts ends, and the pump that called `deliver` must keep taking
  // frames meanwhile, the person's next Send included. What a command printed, or why it did not
  // run, is the session's reply on Dispatch, which the conversation shows. The turn is noted under
  // its tag before the send, and dropped once the host answers that the input submitted no message
  // (a command, a terminal-only built-in, a refusal, a failure): no message will carry its tag, so
  // the frame is done with then, and otherwise once the host writes the message that carries it.
  //
  // A body whose first character is `!` never reaches here: the caller reads it before this and
  // only there, so a line typed at the session's own terminal keeps the host's shell mode (the
  // plugin's one `input` handler passes on every line whose source is not `extension`). The
  // session runs that line itself (`runShellCommandTurn`), and the host is handed nothing. A BTW
  // never reaches this path, and a broadcast is no user turn at all (`isUserTurnCandidate`), so
  // neither runs a command.
  const runTypedInput = (
    sendUserInput: NonNullable<PiApi["sendUserInput"]>,
    delivery: DispatchDelivery,
    // The session the reply below names and is signed as, however long the host takes to
    // answer the input: `postDispatchReply` reads both halves of the reply from it.
    replySessionID: string,
    turn: AcceptedUserTurn,
    content: string | readonly ContentBlock[],
    pictureAddresses: readonly string[]
  ): void => {
    const images =
      typeof content === "string"
        ? []
        : content.filter((block): block is ImageContent => block.type === "image");
    const noted = sessionID;
    if (images.length > 0) {
      typedPictures.admit(turn.messageId, {
        addresses: pictureAddresses,
        body: turn.body,
        images,
        sessionID: noted,
      });
    }
    noteTypedUserTurn(noted, turn.messageId);
    notedSessionIDs.add(noted);
    // Held for the addressed session until the reply is posted (`underWay`): the host's run of
    // the input can outlast a move away from it.
    void underWay(replySessionID, () =>
      Promise.resolve()
        .then(() =>
          sendUserInput(turn.body, {
            ...(turn.mode === "aside" && { deliverAs: "aside" as const }),
            tag: turn.messageId,
            // The person's own turn, from outside the session: like a line typed at the terminal,
            // it ends a cyber-policy hold the session is under (`PiApi.sendUserInput`'s `origin`).
            origin: "outside",
          })
        )
        .then(
          (result) => {
            const submitted = result.handled === "prompt" || result.handled === "skill";
            if (submitted) {
              // Every input handler has run: pictures never handed over never will be.
              typedPictures.inputPassed(turn.messageId);
              // In the host's queue from now on, unless its message was written already.
              unwritten.turnSubmitted(turn.messageId);
            } else {
              dropInjectedUserTurn(noted, turn.messageId);
              typedPictures.withdraw(turn.messageId);
              void unwritten.turnHandled(turn.messageId);
            }
            return typedInputReply(turn.body, result);
          },
          (error: unknown) => {
            dropInjectedUserTurn(noted, turn.messageId);
            typedPictures.withdraw(turn.messageId);
            void unwritten.turnHandled(turn.messageId);
            return typedInputReply(turn.body, { error: messageFor(error) });
          }
        )
        .then(async (reply) => {
          // A `/model` or `/effort` has run by now: the viewer's header shows its result before
          // the reply that names it reaches the page.
          refreshAgentStreamSettings(hostSessionID());
          if (reply === undefined) return;
          await postTypedInputReply(delivery, replySessionID, reply);
        })
        .catch((error: unknown) => {
          logger.warn("envoy: the reply to a person's typed input did not reach Dispatch", {
            attempt: delivery.attempt,
            error: messageFor(error),
            messageID: delivery.id,
          });
        })
    );
  };

  // A Dispatch actor is a person only when it has no `act`: Dispatch marks a credential that
  // acted for its person with the agent or personal token in that field. An agent-attributed
  // event must not buy a new nudge period.
  const isHumanDispatchCard = (rendered: RenderInboundResult): boolean => {
    const actor = rendered.dispatchActor;
    return actor?.kind === "user" && actor.act === undefined;
  };

  // The user turn a person's direct Send or Aside becomes: only one whose acceptance by this
  // session Dispatch records, as the body and mode Dispatch answers. The step itself is shared
  // with the Claude Code plugin (`acceptUserTurn`, `@legion/pi-shared/accept-user-turn`, which
  // `packages/claude-envoy/src/user-turn-router.ts` calls too), so a fix to the accept rule
  // lands in both hosts; the order of its steps, and why the attempt is recorded as handled
  // before Dispatch is asked - a 404 included - is stated there. Undefined for every frame that
  // is not a turn; the caller then delivers today's card, which is the delivery, and posts
  // nothing.
  //
  // What this host puts into it: the attempts it holds in memory and writes to its transcript
  // (`HANDLED_ATTEMPT_ENTRY`, read back on every start, so a restart or a switch to another
  // branch of the tree keeps a delivered attempt from being accepted again), its Dispatch
  // bounded by `USER_TURN_ACCEPT_TIMEOUT_MS`, and these log lines. The entry goes to the
  // transcript the host has open, the only one this host can write. Every frame is addressed to
  // the session whose consumer relayed it, and that is that session's transcript:
  // `acceptUserTurn` calls `handle` before its first await, and `deliver` calls this in the same
  // run as its `addressedSessionOpen()` check. An entry that lands in another conversation all
  // the same, where that session's next start will not read it, is logged. A forged
  // frame can name an attempt before Dispatch's frame does and so spend it; what that costs is
  // the record's limit (`packages/pi-envoy/README.md`, "A person's message from Dispatch").
  const acceptedUserTurn = (
    rendered: RenderInboundResult,
    acceptedSessionID: string
  ): Promise<AcceptedUserTurnDelivery | undefined> =>
    acceptUserTurn({
      actorId: acceptedSessionID,
      // Asked as the session the accept names as its actor, never as the session open here: the
      // two halves of one accept must not name two sessions.
      dispatch: () => timedDispatchClient(USER_TURN_ACCEPT_TIMEOUT_MS, () => acceptedSessionID),
      handle: (key, delivery) => {
        handledDispatchAttempts.add(key);
        pi.appendEntry(HANDLED_ATTEMPT_ENTRY, {
          attempt: delivery.attempt,
          message_id: delivery.id,
        });
        const open = hostSessionID();
        if (open !== acceptedSessionID) {
          logger.warn("envoy: a handled attempt was written to another session's transcript", {
            addressed: acceptedSessionID,
            attempt: delivery.attempt,
            messageID: delivery.id,
            open,
          });
        }
      },
      handled: (key) => handledDispatchAttempts.has(key),
      rendered,
      warn: (refusal) => {
        const fields = { attempt: refusal.delivery.attempt, messageID: refusal.delivery.id };
        if (refusal.reason === "handled") {
          logger.warn(
            "envoy: a frame names a Dispatch attempt this session already delivered; it arrives as a card",
            fields
          );
          return;
        }
        if (refusal.reason === "no-body") {
          logger.warn(
            "envoy: Dispatch accepted a direct message without the stored body and mode to inject; it arrives as a card",
            fields
          );
          return;
        }
        if (refusal.reason === "landed") {
          logger.warn(
            "envoy: Dispatch recorded this session's accept of a direct message and gave no copy of its answer, so no turn follows; it arrives as a card",
            { ...fields, error: refusal.error.message }
          );
          return;
        }
        logger.warn(
          "envoy: Dispatch did not accept a direct message as this session's turn; it arrives as a card",
          { ...fields, error: messageFor(refusal.error) }
        );
      },
    });

  // `session` is the session the frame is addressed to, which every answer to it names and is
  // signed as: the relay reads it off its owner, before anything here awaits, since every frame
  // reaches the session through its own consumer's relay. `subject` is the subject the frame
  // was published on, and `relayed` the relay frame it came as. A relay frame is
  // answered `done` once it is written, or skipped,
  // suppressed, a repeat, or rejected (after the rejection reply is posted), and `nak` once the
  // host dropped it unwritten. A BTW's side turn and every Dispatch reply run off the caller
  // (`offWorker`), so the frames after one never wait on a model call or a reply.
  const deliver = async (
    session: string,
    subject: string,
    raw: string,
    relayed: ConsumerFrame
  ): Promise<void> => {
    // Whether the session this frame was addressed to is still the one open here, read by this
    // instance's own id and by the host's live one. Every branch that spends the person's
    // attempt or asks the model for them reads this before it acts, since each await before it
    // - the read of the body whole, the accept, the picture load - is a window a `/new` or a
    // `/resume` lands in, and the work would then be another conversation's. A delivery's card
    // needs no such read: `cardSession` below hands `UnwrittenFrames.sendCard` the session the
    // delivery names, and that hands the card to nobody once the host has left that session.
    const addressedSessionOpen = (): boolean => sessionOpen(session);
    // Every reply to this frame names `session` and is signed as it, and `postDispatchReply`
    // reads both halves from that one argument.
    let rendered = renderInbound(raw, session, subject);
    // The session a card of this frame is for, read as that card goes out. A Dispatch delivery
    // is the one frame held to the session it names: the person wrote to that session, and a
    // card handed to nobody leaves its claim released, so the relay naks the frame and the
    // listener sends it again to that session (`deliverRelayed`). Every other frame - a peer's
    // card, a Dispatch notification, a role forward - goes to the conversation the host has
    // open then, the one that took it: nobody addressed a session to hold such a frame to, and
    // the conversation the host wrote it into is the one whose transcript now records it
    // (`writtenCardIdentity`, in `../src/missed-messages`).
    const cardSession = (): string => (rendered.delivery !== undefined ? session : hostSessionID());
    // A human unsubscribed one of our topics: drop it locally too, the same way
    // envoy_unsubscribe does. The event also reaches the issue's own topic (every subscriber,
    // not just the removed session), so this only fires for a removal naming us, and only when
    // the stream stored it after this session started following the topic it names
    // (`closeReplayedRemoval`): a removal the stream kept while the session was away must not
    // close a topic followed again since.
    await closeRemovedSubscriptions(raw, relayed.storedAtMs);
    const dedupeIdentity = ClaimIdentitySchema.safeParse(rendered.envelope);
    const identity = dedupeIdentity.success ? dedupeIdentity.data : undefined;
    const held = rendered.skip ? undefined : delivered.claim(identity);
    if (held === undefined) {
      // Skipped for this session, or a copy of a frame this process shows or has shown: nothing
      relayed.done();
      return;
    }
    // A relay frame's claim answers the listener as the claim ends: `done` once it settles (the
    // host wrote the frame, or handled it with nothing to write, the ledger recording it first),
    // `nak` once it is released with nothing recorded (the host dropped the frame unwritten: a
    // session change, an interrupt that withdrew a person's turn), so the listener sends it again.
    // A release while this call still runs answers only once the call returns: a throw is the
    // relay's own to count (its failure notice comes from `createConsumerDelivery`).
    let handing = true;
    let releasedWhileHanding = false;
    const claim: DeliveryClaim = {
      settle: () => {
        held.settle();
        relayed.done();
        relay?.delivery.persist();
      },
      release: () => {
        held.release();
        if (handing) releasedWhileHanding = true;
        else relayed.nak();
      },
    };
    // A repeat of a frame this session was shown before a restart, which the in-memory claim
    // cannot know of: the delivery ledger, rebuilt from the transcript, holds it.
    if (identity !== undefined && ledger?.delivered(identity) === true) {
      claim.settle();
      return;
    }
    // A payload the card could not lay out is shown as the text its sender sent: said once, for
    // the frame this session is shown, and never for a repeat the checks above answered.
    if (rendered.payloadShownAsText !== undefined) {
      logger.warn(
        "envoy: a message's payload is shown as its text; the card could not lay it out",
        {
          event_id: rendered.envelope?.event_id ?? "unknown",
          error: rendered.payloadShownAsText,
        }
      );
    }
    // A body is bounded block by block, so one event can carry its first part alone, and an
    // event the bus refused carries none of it (`cutItem`). The agent is prompted with the
    // whole body the comment or message route answers, read as the session the frame is
    // addressed to and through that session's own Dispatch; with no Dispatch configured, or a
    // read that fails, it keeps what the event carried and the note that says so. This runs
    // after the receipt and after every drop above, so no acknowledgement waits on it and no
    // frame nobody sees costs a read. A switch landing in that read must not have this session
    // read one person's message as another conversation, with that conversation's credential.
    if (rendered.cutItem !== undefined) {
      const dispatch = await timedDispatchClient(WHOLE_BODY_TIMEOUT_MS, () => session);
      const body =
        dispatch === undefined
          ? undefined
          : await wholeCarriedBody(dispatch, rendered.cutItem, session);
      if (body !== undefined) rendered = renderInbound(raw, session, subject, body);
    }
    const envelope = rendered.envelope;
    // The Dispatch message a card of this frame delivers to the session, which its `details` name
    // (`handCard`): a targeted message's delivery, which Dispatch stores for the session. A
    // targeted comment is no message the session's message list holds.
    const deliveredMessage =
      rendered.delivery?.resource === "message" ? rendered.delivery.id : undefined;
    if (envelope !== undefined) noteInbox(envelope);
    // The host wrote the frame, or handled it without writing anything: the ledger records it,
    // so no redelivery shows it again, here or after a restart, and the claim is settled, so
    // nothing waits on it. The record reaches the transcript of the session
    // it was delivered in even while that session shuts down (`recordDeliveredFor`): a BTW
    // answered as the session stops must not come back from a redelivery after its resume.
    //
    // That write lands only while the addressed session is the one this process writes its
    // transcript to (`writeSessionID`); `recordDeliveredFor` makes that test itself, and answers
    // false when the write it queued did not reach the host - the host moved while the write
    // waited behind the writes before it, or the write failed - which would settle the claim
    // with nothing written: the session's transcript would lose the delivery, and its resume
    // would show the person's line again as a card. The write cannot be made from the
    // conversation the host moved to, so the loss is logged instead of settled in silence.
    const recordTaken = async (frame: DedupeIdentity): Promise<void> => {
      // With no ledger at all, no session's record loses anything.
      if (ledger === undefined || (await ledger.recordDeliveredFor(session, frame))) return;
      logger.warn("envoy: a delivery was not recorded in the session it was addressed to", {
        addressed: session,
        eventID: frame.event_id,
        open: sessionID,
      });
    };
    const taken = async (): Promise<void> => {
      if (identity !== undefined) await recordTaken(identity);
      claim.settle();
    };
    // A delivery whose session is not the one open here any more: the accept below would spend
    // the person's attempt, and whatever Dispatch then answered would be run and shown in the
    // conversation the host moved to. So nothing is accepted and nothing is posted - Dispatch
    // never recorded this attempt as taken - and the claim goes back: the relay answers `nak`,
    // so the listener sends the frame again to the session it names and to no other, never
    // answered by another session. The line below is what says here that a person's message
    // went back unanswered.
    // A card of such a frame goes back the same way, inside `UnwrittenFrames.sendCard`, which
    // hands none to a session that is not open and leaves the pictures it carries unshown.
    const goBack = (): void => {
      logger.warn("envoy: a delivery was not taken here; the session it names is closed", {
        addressed: session,
        eventID: envelope?.event_id,
        open: sessionID,
      });
      claim.release();
    };
    // An error reply means the agent was not handed the frame: Dispatch records the attempt
    // failed, and a same-mode re-send of it must reach this session again, so the claim is
    // released before the reply goes out. A BTW side turn adds nothing to the transcript, so
    // running it again for that re-send repeats no work the session kept. A relay frame is done
    // once the reply is posted: the re-send is a new frame.
    const refuse = async (delivery: DispatchDelivery, error: string): Promise<void> => {
      held.release();
      await postDispatchReply(delivery, { error }, session);
      relayed.done();
    };
    // Runs a side turn or a Dispatch reply off the caller (the relay's queue), so the next frame
    // is handed without waiting on it, held for the addressed session until it ends
    // (`underWay`). One that throws is handled as a delivery that throws: its claim is released
    // and the relay frame is done (a BTW is in the ledger already, so its redelivery would be
    // dropped). Its warning, as each warning of this delivery's own, goes through the tell gate
    // (`logger`), so a reply that fails once the stop has begun writes no line.
    const offWorker = (work: () => Promise<void>): void => {
      relayed.release();
      void underWay(session, work).catch((error: unknown) => {
        held.release();
        logger.warn(`envoy: failed to deliver envelope ${envelope?.event_id ?? "unknown"}`, {
          error: messageFor(error),
        });
        relayed.done();
      });
    };
    try {
      if (rendered.rejectedDelivery !== undefined) {
        logger.warn(
          `envoy: rejecting malformed Dispatch targeted delivery ${rendered.rejectedDelivery.id}`
        );
        const rejected = rendered.rejectedDelivery;
        offWorker(() => refuse(rejected, "Invalid Dispatch targeted delivery frame"));
      } else if (rendered.malformedDelivery === true) {
        logger.warn("envoy: dropping malformed Dispatch targeted delivery without a reply address");
        claim.settle();
      } else if (rendered.delivery?.mode === "btw" && relayed.late === true) {
        // A BTW that arrives late (delivered before, or stored before this connection's epoch
        // began) never starts a side turn: it is one inert card, as a replayed BTW is, written to
        // the transcript and done then. Its answer, if any, is on Dispatch.
        noteBtw(identity?.event_id);
        unwritten.sendCard(
          cardSession(),
          `${storedText(rendered.content, relayed.storedAt)}\n${LATE_BTW_NOTE}`,
          "inert",
          identity,
          claim,
          deliveredMessage
        );
      } else if (rendered.delivery?.mode === "btw") {
        // A BTW is answered on Dispatch or refused there, never shown as a card: a redelivery
        // must never come back as a card that starts a turn. The ledger
        // records it by its event id alone before the side turn and before any reply is posted,
        // so it is never left unrecorded whatever happens after (a reply that fails, a
        // redelivery while the POST is in flight, the process ending mid-turn or mid-POST), and
        // the record reaches the transcript of the session it was delivered in even while that
        // session shuts down (`recordDeliveredFor`), so a resume of it passes the copy over too.
        // A same-mode Retry carries a new event id under the same dedupe key, and still reaches
        // the session once a refusal released the claim. The side turn and the reply run off the
        // caller: the next frame does not wait for the model.
        noteBtw(identity?.event_id);
        if (identity?.event_id !== undefined) {
          await recordTaken({ event_id: identity.event_id });
        }
        const delivery = rendered.delivery;
        const answer = sideTurn(activeSessionContext);
        if (!addressedSessionOpen()) {
          // The session this BTW was asked of is not the one open here any more: the side turn
          // reads the context the host has now, so the conversation it moved to would answer a
          // question nobody asked it, under the name of the session the person wrote to.
          // Nothing is asked: the person is answered with the refusal, the claim is released, and
          // the BTW is asked again only if they send it again.
          offWorker(() => refuse(delivery, MOVED_SESSION_REFUSAL));
        } else if (shuttingDown) {
          // A session that is shutting down starts no model call, but a frame can still drain in.
          offWorker(() => refuse(delivery, "This OMP session is shutting down"));
        } else if (answer === undefined) {
          offWorker(() => refuse(delivery, "This OMP host does not support BTW delivery"));
        } else {
          offWorker(async () => {
            try {
              const reply = await answer({ prompt: delivery.body });
              await postDispatchReply(delivery, { body: reply.replyText }, session);
              await taken();
            } catch (error) {
              await refuse(delivery, messageFor(error));
            }
          });
        }
      } else if (rendered.delivery !== undefined && !addressedSessionOpen()) {
        goBack();
      } else {
        const accepted = await acceptedUserTurn(rendered, session);
        // The pictures the delivered text embeds reach the model beside it, read with the
        // addressed session's own Dispatch bearer, each picture once in this session. They count
        // as shown only once the host took the send: one that throws releases the claim, and the
        // delivery Dispatch retries must carry them again. `session` names the accept, the reads
        // and the replies. The set below is read live, after the accept, so a `/new` or a
        // `/resume` inside the accept has this text built against the set of the conversation
        // the host moved to: a picture that conversation was shown is left out of it, and one it
        // was not is carried again. Nothing is marked from it - each branch below marks the set
        // it reads for itself, once the host was handed the card or the turn: a card for a
        // session that is not the open one is handed to nobody (`UnwrittenFrames.sendCard`),
        // and a turn is sent only past its own check. Both compare session ids, so a host that
        // left this session and came back to it inside the accept reads as one that never left.
        const shown = shownPictures(sessionID);
        if (accepted === undefined) {
          const card = await withDeliveredPictures(
            rendered.content,
            rendered.pictures ?? [],
            () => timedDispatchClient(DELIVERY_PICTURES_TIMEOUT_MS, () => session),
            true,
            shown
          );
          // Only an event attributed directly to a person can make this card a fresh awareness
          // period. Dispatch marks an agent acting for a person in `actor.act`, so it shares the
          // bounded agent-card path instead. This runs after every picture is ready but before
          // the card's agent-initiated run can begin, for the session the card goes to: a frame
          // that is no delivery follows the conversation the host has open (`cardSession`), so a
          // `/new` or a `/resume` during the arming read arms the session it moved to as well.
          if (isHumanDispatchCard(rendered)) {
            let destination = cardSession();
            await armAskAwarenessForPersonInput(activeSessionContext, destination);
            while (cardSession() !== destination) {
              destination = cardSession();
              await armAskAwarenessForPersonInput(activeSessionContext, destination);
            }
          }
          // Settled once the host writes the card, or at once when nothing waits on that. How
          // the card goes, and why, is the wake's (`handCard`, `../src/card-wake`). The accept,
          // the picture load, or both, may have run across a `/new` or a `/resume`: a delivery's
          // card is then for a session open nowhere here, so it is handed to nobody (`closed`)
          // and its pictures stay unshown, which is what the copy the relay sends that session
          // again carries. A frame that is no delivery has its card in the conversation the
          // host moved to, the one that took it (`cardSession`).
          const outcome = unwritten.sendCard(
            cardSession(),
            card.content,
            rendered.delivery?.mode === "aside" ? "aside" : "card",
            identity,
            claim,
            deliveredMessage
          );
          if (outcome !== "closed") markPicturesShown(shownPictures(sessionID), card.shown);
        } else {
          const { turn } = accepted;
          // Sent exactly as Enter, or an aside, at the terminal sends it, the person's pictures
          // beside their text.
          const delivery = await withDeliveredPictures(
            turn.body,
            pictureAddresses(turn.body),
            () => timedDispatchClient(DELIVERY_PICTURES_TIMEOUT_MS, () => session),
            false,
            shown
          );
          // The person's answer, and the end of their attempt with it. The accept spent the
          // attempt, so each answer below settles rather than releases - Dispatch holds the
          // message delivered and refuses a re-send of it - and the reply is what tells the
          // person what became of their line.
          const answerAndSettle = (body: string): void => {
            offWorker(async () => {
              await postDispatchReply(accepted.delivery, { body }, session);
              await taken();
            });
          };
          const sendUserInput = pi.sendUserInput;
          // Why this host sends nothing, on a host that cannot run typed input: a command it
          // was handed would reach the model as words, so the person is answered that nothing
          // was sent. Plain text there has no refusal and goes to the model as before.
          const refusal =
            sendUserInput === undefined
              ? typedInputReply(turn.body, { hostVersion: VERSION })
              : undefined;
          // A `!command` is the session's own to run: nothing is written for it and the host
          // never sees it, which is why it is read here rather than inside the send, where the
          // wait for the host's own message of this turn is armed. A host without typed input
          // runs none, and its lines reach the model as they always did.
          const command = sendUserInput === undefined ? undefined : shellCommandFrom(turn.body);
          if (refusal !== undefined) {
            answerAndSettle(refusal);
          } else if (command !== undefined) {
            runShellCommandTurn(accepted.delivery, session, turn, command, taken);
          } else if (!addressedSessionOpen()) {
            // The session this message was accepted for stopped being the one open here while
            // the accept ran or the pictures loaded. The host is handed nothing: the line would
            // run as the conversation it moved to, and that conversation's answer would be
            // posted under the name of the session the person wrote to. The reply is what tells
            // the person their message is still theirs to send again.
            answerAndSettle(MOVED_SESSION_REFUSAL);
          } else if (sendUserInput === undefined) {
            // Noted right before the send, after the pictures load: a run that ends while they
            // load clears every text note (endInjectedUserTurns), and this turn must still be found.
            const injectionSessionID = sessionID;
            noteInjectedUserTurn(injectionSessionID, turn.body, turn.messageId);
            notedSessionIDs.add(injectionSessionID);
            pi.sendUserMessage(delivery.content, {
              ...(turn.mode === "aside" && { deliverAs: "aside" as const }),
              // The person's own turn, from outside the session, as `runTypedInput` sends it.
              origin: "outside",
            });
            markPicturesShown(shownPictures(injectionSessionID), delivery.shown);
            // Done once the host writes the user message its text is found as, or at the run's
            // end, which stops that search (`endInjectedUserTurns`).
            unwritten.awaitTurn(turn.messageId, {
              sessionID: injectionSessionID,
              identity,
              claim,
              via: "text",
            });
          } else {
            unwritten.awaitTurn(turn.messageId, { sessionID, identity, claim, via: "typed" });
            runTypedInput(
              sendUserInput,
              accepted.delivery,
              session,
              turn,
              delivery.content,
              delivery.shown
            );
          }
        }
      }
    } catch (error) {
      handing = false;
      held.release();
      logger.warn(`envoy: failed to deliver envelope ${envelope?.event_id ?? "unknown"}`, {
        error: messageFor(error),
      });
      // The failure is the relay's to count: the frame comes again, or, from its third delivery
      // on, the relay shows its failure notice (`createConsumerDelivery`).
      throw error;
    }
    handing = false;
    if (releasedWhileHanding) relayed.nak();
  };

  const RESUBSCRIBE_DELAY_MS = Number(process.env.ENVOY_RESUBSCRIBE_DELAY_MS ?? "") || 5_000;

  // The topics this session follows, as the concrete subjects a registration carries
  // (`expandSubscriptionTopics`). The listener relays every one of them from the session's own
  // durable consumer, so the plugin holds no NATS subscription on any of them: after a
  // registration its connection holds exactly its relay subject and its agent-stream control
  // subject.
  const followed = new Set<string>();

  // Follows `topic`; answers each subject it newly followed, so a caller registers only a change
  // and a role's claim knows which of its subjects the session did not follow already
  // (`createRoleTopicFollows`).
  const followNew = (topic: string): string[] => {
    const added: string[] = [];
    for (const subject of expandSubscriptionTopics([topic])) {
      if (followed.has(subject)) continue;
      followed.add(subject);
      added.push(subject);
    }
    return added;
  };
  const follow = (topic: string): boolean => followNew(topic).length > 0;

  // Whether taking `subjects` out of the registry takes a subject the session still follows with
  // them: the client's unsubscribe expands a closed `X.>` to `X` too (`expandSubscriptionTopics` in
  // `transport.ts`), and `X` can be the session's own, which then needs registering again.
  const takesFollowedBase = (subjects: readonly string[]): boolean =>
    expandSubscriptionTopics(subjects).some((subject) => followed.has(subject));

  // Stops following `topic`; false when the session did not follow it, so nothing closed.
  const unfollow = (topic: string): boolean => {
    let dropped = false;
    for (const subject of expandSubscriptionTopics([topic])) {
      if (followed.delete(subject)) dropped = true;
    }
    return dropped;
  };

  // How long one open waits for the server to confirm it (the bound claude-envoy's handoff flush
  // uses for the same round trip), and the whole wait a pass pays: the first open that is not
  // answered within it marks the connection unanswered, and every later open on that connection
  // goes on without waiting (`unanswered`).
  const SUBSCRIBE_FLUSH_TIMEOUT_MS =
    Number(process.env.ENVOY_SUBSCRIBE_FLUSH_TIMEOUT_MS ?? "") || 10_000;

  // The time a pass may still need once its NATS dial answers: the first subscription's
  // confirmation, then one listener call for the registration with its retry. A dial's redials end
  // this long before the pass's deadline (`dialNats`), so a connection that answers late still
  // leaves the pass time to register. 10 + 2 x 5 + 0.25 = 20.25 s at the defaults.
  const PASS_AFTER_DIAL_MS =
    SUBSCRIBE_FLUSH_TIMEOUT_MS +
    LISTENER_CALL_ATTEMPTS * LISTENER_CALL_TIMEOUT_MS +
    (LISTENER_CALL_ATTEMPTS - 1) * LISTENER_CALL_RETRY_DELAY_MS;

  // The connections that have not answered a round trip since they last did. A background
  // confirmation asks each of them for that answer and clears the mark however it ends - on the
  // answer, on a connection closed for good, on a shutdown, or at its cap; a reconnect
  // clears it too (`watchReconnects`), since a reconnected connection answers again and nats.js
  // has sent every live subscription on it afresh. While a mark stands, an open on that connection
  // pays no bound at all: the reconcile pass opens this session's relay subject and its
  // agent-stream control subject one after another before it registers, so a bound paid per open
  // would hold the session's registration for ten seconds times the subjects it opened.
  const unanswered = new Set<SessionInbound>();

  // `promise`, or a rejection naming `what`, once `ms` pass. Its text is this plugin's own, so a
  // NATS failure notice prints it whole (`LoggableError`).
  const within = async <T>(promise: Promise<T>, ms: number, what: string): Promise<T> => {
    const expired = Promise.withResolvers<never>();
    const timer = setTimeout(
      () => expired.reject(new LoggableError(`${what} timed out after ${ms} ms`)),
      ms
    );
    try {
      return await Promise.race([promise, expired.promise]);
    } finally {
      clearTimeout(timer);
    }
  };

  // How long the background confirmation goes on asking a connection that answers nowhere; it
  // waits RESUBSCRIBE_DELAY_MS after each unanswered ask, and each ask waits up to
  // SUBSCRIBE_FLUSH_TIMEOUT_MS for its answer, so with the defaults one ask comes up to 15 s after
  // the one before. Reaching the cap ends the loop, which clears the connection's mark on its way
  // out as every other exit does, so the next open pays a bound of its own rather than going on
  // unconfirmed for the life of a connection that never answers.
  const BACKGROUND_CONFIRM_CAP_MS = 10 * 60_000;

  // The round trip that makes a subject this session opened live rather than merely written:
  // `connection.subscribe` only writes SUB into the connection's out buffer, and core NATS drops
  // a publish no subscription matches yet, so without it the registration can name this session's
  // relay subject — which the listener relays the session's consumer to at once — while the
  // server still has nothing to route to. The window is the whole gap between the write and the
  // next flush, and it is the one `notifications.legion.<project>.controller` lost a triage
  // notice in. claude-envoy's channel flushes before every registration for the same reason.
  //
  // It is bounded, and the bound is paid once per connection, not once per open. A connection that
  // is reconnecting rejects every pending round trip (`resetOutbound`,
  // `nats/lib/nats-base-client/protocol.js`) and answers none until it is back, and with
  // `maxReconnectAttempts: -1` it is never closed, so a confirmation that waited such a rejection
  // out would wait for the whole outage — and the reconcile pass's own contract (above its
  // `openRelay` call) is that only opening the relay needs NATS, while the registration and the
  // role go over HTTP and an outage stops neither. So one bound is awaited:
  // answered, the caller registers a subject the server has, which is the ordering this file
  // exists for; unanswered, the subjects stay open — nats.js sends every live subscription again
  // on each connect (`sendSubscriptions`, before `flushPending`) — the caller goes on exactly as
  // it did before this file took a round trip at all, and the confirmation runs on in the
  // background so the server's answer is still asked for, with nothing waiting on it. The
  // connection is marked unanswered meanwhile (`unanswered`), so a pass that opens both its
  // subjects waits once rather than once per subject. A notice published during the outage is
  // lost, as it is without this file's round trip at all.
  //
  // It throws only on a connection closed for good, or a session shutting down: either way this
  // connection carries nothing more, and the subjects it opened are the caller's to close.
  const confirmSubscribed = async (connection: SessionInbound, what: string): Promise<void> => {
    // This connection has not answered its last round trip, and the background confirmation is
    // already asking: waiting again would pay the bound once per open, where a pass pays it once.
    if (unanswered.has(connection)) return;
    try {
      await within(connection.flush(), SUBSCRIBE_FLUSH_TIMEOUT_MS, `confirming ${what}`);
    } catch (error) {
      if (shuttingDown || connection.isClosed()) throw error;
      // Marked before the loop starts, which is the order its own clear assumes: the loop clears
      // the mark however it ends, and this line is reached only with the connection open and the
      // session running, so its first sleep always comes before that clear.
      unanswered.add(connection);
      void confirmInBackground(connection, what);
    }
  };

  // Asks for the server's answer again, on a connection that has not given one, until it answers,
  // the connection is closed for good, the session shuts down, or BACKGROUND_CONFIRM_CAP_MS pass.
  // Nothing waits on it; what it is for is the entry in `unanswered` it clears, which is what lets
  // the open that failed, and every open after it, go on without paying the bound again. Once it
  // clears the entry the next open pays one bound of its own, which is how a connection that
  // started answering again is noticed.
  const confirmInBackground = async (connection: SessionInbound, what: string): Promise<void> => {
    const until = Date.now() + BACKGROUND_CONFIRM_CAP_MS;
    try {
      while (!shuttingDown && !connection.isClosed() && Date.now() < until) {
        await new Promise((resolve) => setTimeout(resolve, RESUBSCRIBE_DELAY_MS));
        if (shuttingDown || connection.isClosed()) return;
        try {
          await within(connection.flush(), SUBSCRIBE_FLUSH_TIMEOUT_MS, `confirming ${what}`);
          return;
        } catch {
          // Still no answer: ask again, until this connection is gone or the cap is reached.
        }
      }
    } finally {
      unanswered.delete(connection);
    }
  };

  const noteInbox = (envelope: NonNullable<RenderInboundResult["envelope"]>): void => {
    inbox.unshift({
      event_id: envelope.event_id ?? "unknown",
      at: inboundTimestamp(envelope.issued_at),
      from: senderLabel(envelope),
      summary: envelope.payload_summary ?? "unknown",
    });
    if (inbox.length > 50) inbox.pop();
  };

  // What the ledger persists, so a restart follows it again (`replayFollowedSubjects`): the
  // session's own agent subject and every followed topic the session wants for a reason of its
  // own (`registrationState.isRoleBound`). A subject only the held role wants is the role claim's
  // to open again, which a restored process without the role never does.
  const followedSubjects = (): string[] => {
    if (sessionID === "") return [];
    return replayFollowedSubjects(
      legacyAgentSubject(sessionID),
      followed,
      registrationState.isRoleBound
    );
  };

  // Acts on a `subscription.removed` naming this session, which the relay delivers like any
  // other frame: the topics it names are dropped locally, the same way envoy_unsubscribe does,
  // and taken out of the listener's registry, but only those this session followed before the
  // stream stored the frame (`closeReplayedRemoval`), so a removal the stream kept while the
  // session was away never closes a topic followed again since. Registered again without them,
  // the session's consumer stops relaying them at once.
  const closeRemovedSubscriptions = async (raw: string, storedAtMs: number): Promise<void> => {
    const closed = await closeReplayedRemoval(
      { data: raw, storedAtMs },
      {
        sessionID,
        relay: relay?.sessionID === sessionID ? relay.delivery : undefined,
        close: unfollow,
        unregister: async (topics) => {
          await active?.client
            .unsubscribe({ sessionID, topics })
            .catch((error: unknown) =>
              logger.warn(
                isBusUnanswered(error)
                  ? busUnansweredRemoval("the topics of a subscription removal")
                  : "envoy: a subscription removal was not taken out of the registry",
                { topics, error: messageFor(error) }
              )
            );
        },
        followedChanged: ledgerFollowedChanged,
      }
    );
    if (closed.length === 0) return;
    void Promise.resolve()
      .then(() => registerSession())
      .catch(() => undefined);
  };

  // The session this instance and the host are both on: an entry appended now lands in its
  // transcript.
  const transcriptSessionID = (): string | undefined => {
    const host = hostSessionID();
    return host !== "" && host === sessionID ? host : undefined;
  };

  // Tells the session a gap notice: a card held until the host is settled
  // (`UnwrittenFrames.sendNotice`), which the transcript records held, so a restart holds it
  // again.
  const tellGapNotice = (notice: ConsumerGapNotice): void => {
    logger.warn(`envoy: ${notice.message}`);
    sendNotice(gapNoticeText(notice));
  };

  // What this session was already shown, and the notice naming a frame it could not be shown
  // (`@legion/envoy-client/delivery-ledger`). Its sessions are this instance's: work started for
  // one session stops once the host, or this instance, is on another.
  const ledger: DeliveryLedger | undefined =
    active === undefined || !inbound
      ? undefined
      : createDeliveryLedger({
          host: {
            sessionID: () => (shuttingDown ? undefined : transcriptSessionID()),
            // A session shutting down starts no new work, but the host still writes its
            // transcript, so a refused BTW's record still reaches it (`recordDeliveredFor`).
            writeSessionID: transcriptSessionID,
            followedSubjects,
            // The notice goes to the session the ledger addresses it to, which is the session
            // the frame it names was relayed to; `showReplayCard` hands it to that session and
            // to no other (`UnwrittenFrames.sendCard`). One the open session is not handed is
            // thrown back at the ledger, which reports it as a notice that could not be shown
            // (`failureNoticeFailed`), as it does for a notice the host refused.
            showFailureNotice: async (addressed, notice) => {
              const outcome = showReplayCard(
                addressed,
                envoyNoticeText(failureNoticeFields(notice)),
                notice.envelope,
                btwEvents.has(notice.eventId) ? "inert" : "card"
              );
              if (outcome === "failed") throw new Error("the session it names is closed");
              return outcome;
            },
            failureNoticeFailed: (notice, error) => {
              logger.warn("envoy: a message this session was not shown could not be reported", {
                eventID: notice.eventId,
                error: messageFor(error),
              });
              notify(
                activeSessionContext,
                `envoy: message ${notice.eventId} from ${notice.sender} was not shown, nor was the notice naming it (${messageFor(error)})`,
                "warning"
              );
            },
            log: (message, error) =>
              logger.warn(
                `envoy: ${message}`,
                error === undefined ? undefined : { error: messageFor(error) }
              ),
            write: (change) => {
              for (const entry of change.delivered) pi.appendEntry(REPLAY_DELIVERED_ENTRY, entry);
              if (change.state !== undefined) pi.appendEntry(REPLAY_STATE_ENTRY, change.state);
            },
          },
        });

  // The ledger's followed topics are written once a pass has opened the session's inbound: before
  // that, a topic restored from the transcript is not followed again yet, and the write would
  // drop it. A stopping session writes none (the write gate).
  const ledgerOpen = (): boolean =>
    !shuttingDown && sessionID !== "" && registrationState.inboundOpenSession() === sessionID;
  const ledgerFollowedChanged = async (): Promise<void> => {
    if (ledger !== undefined && ledgerOpen()) await ledger.followedChanged();
  };

  // Consumer delivery (`@legion/envoy-client/consumer-delivery`). While a pass holds the
  // session's inbound open, it holds the session's relay subject open, subscribed before any
  // registration, and every registration asks the listener for consumer delivery. The listener
  // relays the session's durable consumer on that subject — its agent subject and every followed
  // topic — each frame handed to `deliver` in stream order and answered `done` once written; a
  // registration it can give no consumer is refused, and the session has no inbound at all until
  // a later heartbeat's registration is answered.
  interface Relay {
    readonly sessionID: string;
    readonly delivery: ConsumerDelivery;
    subscription: InboundSubscription | undefined;
    /** Why the last open of the relay subject failed, or why its subscription ended, in words a
     *  notice may hold (`natsFailureReason`): a registration sent while the subject is missing
     *  names it, whoever sends it. */
    missing: string | undefined;
  }
  let relay: Relay | undefined;
  /** Why the last pass could not open its session's inbound, for a registration sent outside a
   *  pass while that session has no relay yet (the start's dial failed before one existed). */
  let passFailure: { readonly sessionID: string; readonly text: string } | undefined;

  // A relay frame for a session the host is no longer on (a switch, before the next pass closes
  // its relay), or one that drains in while the session shuts down, is one the session cannot
  // take: `nak`, so the listener sends it again to whoever takes the session next.
  const deliverRelayed = async (delivery: ConsumerDelivery, frame: ConsumerFrame) => {
    const owner = relay;
    if (shuttingDown || owner?.delivery !== delivery || !sessionOpen(owner.sessionID)) {
      frame.nak();
      return;
    }
    // The relay's own session, proven open on the line above: the frame is this session's and
    // every answer to it names that session, whatever the host does while it is delivered. The
    // session is held from here, in the same run as that check, until the delivery and the work
    // it hands off end (`underWay`).
    await underWay(owner.sessionID, () =>
      deliver(owner.sessionID, frame.subject, frame.data, frame)
    );
  };
  // The relay of session `id`, its snapshot restored from `entries` (before any `setFollowed`),
  // and every later change of it appended to the transcript of the session it is for
  // (`CONSUMER_STATE_ENTRY`): after a frame is written, after the followed topics change, and
  // after a registration answer moves the stream. The follows map goes with an entry only when it
  // changed, so a written frame costs one resume point beside the frame's own entries; what the
  // relay restored is stored already and is not appended again.
  const createRelay = (id: string, entries: readonly unknown[], ledger: DeliveryLedger): Relay => {
    let persistedFollows = "";
    const delivery: ConsumerDelivery = createConsumerDelivery({
      sessionID: id,
      ledger,
      host: {
        deliver: (frame) => deliverRelayed(delivery, frame),
        gap: tellGapNotice,
        log: (message, error) =>
          logger.warn(
            `envoy: ${message}`,
            error === undefined ? undefined : { error: messageFor(error) }
          ),
      },
      publish: (reply, body) => {
        if (connection === undefined) throw new Error("no NATS connection");
        connection.publish(reply, codec.encode(body));
      },
      persist: (snapshot) => {
        // Another session's transcript is not this relay's to write to: the change is appended
        // once the host records in this session again.
        if (relay?.delivery !== delivery || transcriptSessionID() !== id) return false;
        const follows = JSON.stringify(snapshot.follows);
        pi.appendEntry(CONSUMER_STATE_ENTRY, {
          session_id: id,
          ...(snapshot.resume === undefined ? {} : { resume: snapshot.resume }),
          ...(follows === persistedFollows ? {} : { follows: snapshot.follows }),
        });
        persistedFollows = follows;
        return true;
      },
    });
    const restored = consumerSnapshot(entries, id);
    if (restored !== undefined) delivery.restore(restored);
    persistedFollows = JSON.stringify(delivery.snapshot().follows);
    return { sessionID: id, delivery, subscription: undefined, missing: undefined };
  };

  // Subscribes the relay subject, unless it is open: a subscription on a connection the listener
  // has not been told of begins a new epoch, which the next registration carries. A queued task
  // hands in its own fenced `connect`, so a task the host outran opens no relay subject: the
  // acquisition refuses and this call throws out of the task.
  const subscribeRelay = async (
    owner: Relay,
    connect: () => Promise<SessionInbound> = ensureConnection
  ): Promise<void> => {
    if (owner.subscription !== undefined) return;
    let connected: SessionInbound;
    try {
      connected = await connect();
    } catch (error) {
      // A step the task refused (its pass was abandoned, or its session moved) is no reason the
      // subject is missing: the reason recorded before it stands.
      if (relay === owner && !stepWasRefused(error) && !taskWasAbandoned(error)) {
        owner.missing = natsFailureReason(error);
      }
      throw error;
    }
    if (relay !== owner || owner.subscription !== undefined) return;
    const subscription = connected.subscribe(owner.delivery.subject);
    owner.subscription = subscription;
    owner.missing = undefined;
    owner.delivery.beginEpoch();
    void pumpRelay(owner, subscription, connected);
    // The relay's own registration names this session's delivery subject the moment this returns,
    // and the listener publishes to it at once, so a SUB still in the out buffer costs the first
    // frames. A round trip this connection can still answer later does not fail the open, so only
    // a connection closed for good gets past the confirmation, and then this subject goes with it:
    // `retryRelay` bails while `owner.subscription` is set, and `pumpRelay` starts no retry of its
    // own for a subscription whose owner no longer names it, so the caller - the reconcile pass,
    // or `retryRelay` behind a later pump end - opens the relay again only because this throws
    // with the field cleared.
    try {
      await confirmSubscribed(connected, `the relay subscription for ${owner.sessionID}`);
    } catch (error) {
      if (owner.subscription === subscription) {
        owner.subscription = undefined;
        owner.missing = natsFailureReason(error);
      }
      subscription.unsubscribe();
      throw error;
    }
  };

  // Opens session `id`'s relay subject, closing another session's relay first.
  const openRelay = async (
    context: SessionContext,
    id: string,
    connect: () => Promise<SessionInbound>
  ): Promise<void> => {
    if (ledger === undefined) return;
    if (relay?.sessionID !== id) {
      closeRelay();
      relay = createRelay(id, context.sessionManager.getEntries(), ledger);
    }
    await subscribeRelay(relay, connect);
  };

  // The session left (or shut down): its unsettled frames are answered `nak` and its relay
  // subject is closed. Nothing is relayed until the next registration is answered.
  const closeRelay = (): void => {
    const owner = relay;
    if (owner === undefined) return;
    relay = undefined;
    owner.delivery.close();
    const subscription = owner.subscription;
    owner.subscription = undefined;
    subscription?.unsubscribe();
  };

  // Hands each relay message to the session's consumer delivery, which answers `received` at once
  // and queues the frame in stream order. A subscription that ends under the session (its
  // connection closed) is opened again after the resubscribe delay, on a new epoch, and
  // registered, so the listener restarts the relay at its ack floor.
  const pumpRelay = async (
    owner: Relay,
    subscription: InboundSubscription,
    connected: SessionInbound
  ): Promise<void> => {
    let failure: unknown;
    try {
      for await (const message of subscription) {
        owner.delivery.receive(message);
      }
    } catch (error) {
      // Iterator failure falls through to the resubscribe path below.
      failure = error;
    }
    if (owner.subscription !== subscription) return;
    owner.subscription = undefined;
    if (failure !== undefined) owner.missing = natsFailureReason(failure);
    else if (connected.isClosed()) {
      // nats.js ends each subscription of a connection it closes with no error of its own, so the
      // close is why the subject went: in the plugin's own words at once, then in the close's own
      // error where it had one.
      owner.missing = RELAY_CONNECTION_CLOSED;
      void connected.closed().then((closedWith) => {
        if (!(closedWith instanceof Error) || relay !== owner) return;
        if (owner.subscription === undefined && owner.missing === RELAY_CONNECTION_CLOSED) {
          owner.missing = natsFailureReason(closedWith);
        }
      });
    }
    if (!shuttingDown && relay === owner) retryRelay(owner, RESUBSCRIBE_DELAY_MS);
  };

  const retryRelay = (owner: Relay, delayMs: number): void => {
    setTimeout(() => {
      if (shuttingDown || relay !== owner || owner.subscription !== undefined) return;
      subscribeRelay(owner).then(
        () =>
          void Promise.resolve()
            .then(() => registerSession())
            .catch(() => undefined),
        () => retryRelay(owner, NATS_RETRY_INTERVAL_MS)
      );
    }, delayMs);
  };

  // nats.js reconnected: the relay subscription is the same object, but the listener must restart
  // the relay at its ack floor, so a new epoch is registered at once.
  const renewRelayEpoch = (): void => {
    if (shuttingDown || relay?.subscription === undefined || relay.sessionID !== sessionID) return;
    relay.delivery.beginEpoch();
    void Promise.resolve()
      .then(() => registerSession())
      .catch(() => undefined);
  };

  // What a registration's answer says of consumer delivery, for the session it registered and
  // the relay it asked for. An answer for a session this instance has left, or for a relay since
  // closed, changes nothing.
  const takeDeliveryAnswer = (
    registeredSession: string,
    asked: Relay,
    result: SubscribeResult
  ): void => {
    if (sessionID !== registeredSession || relay !== asked) return;
    asked.delivery.answered(result.delivery);
  };

  // The live agent conversation stream (LEGION-232). This session's own turns, tool calls and
  // streamed output become frames on a core-NATS subject the notification stream does not
  // capture, so nothing is retained by the bus; the Dispatch relay forwards them to a human
  // watching the session and writes none of it down. The publisher stays silent unless a viewer
  // is attached, so a session nobody has open puts no conversation on the wire at all.
  //
  // Each session has a ring of its own, so a replay answers its session's turns alone, however
  // the host's moves and this process's passes interleave: a turn is kept in the ring of the
  // session the host records it under (`recordAgentStreamMessage`), and a control subject replays
  // and arms its own session's ring alone. A ring whose viewer still watches is kept after the
  // host moves on, so what that session still records (a person's shell command it ran) reaches
  // that viewer on its own subject; every other ring of a session this process no longer serves
  // is dropped (`pruneAgentStreams`) when a ring is made and when the control subject moves. So at
  // most the served session's, the host's live one and the rings still watched are held, and a
  // watch lapses within AGENT_STREAM_WATCH_TTL_MS of its viewer's last ping.
  const agentStreams = new Map<string, AgentStreamPublisher>();
  /** The sessions whose ring was filled from their transcript (`restoreAgentStream`). A ring
   *  alone does not say so: the host can record a turn under a session before a pass has taken
   *  that session up, and that ring holds the one turn and none before it. */
  const restoredAgentStreams = new Set<string>();
  /** Forgets the ring of every session but the `kept` ones, and those a viewer still watches. */
  const pruneAgentStreams = (...kept: string[]): void => {
    for (const [session, ring] of agentStreams) {
      if (kept.includes(session) || ring.watched) continue;
      agentStreams.delete(session);
      // The ring it was filled into is gone, so the session is filled again when it is taken up.
      restoredAgentStreams.delete(session);
    }
  };
  const agentStreamOf = (session: string): AgentStreamPublisher => {
    let ring = agentStreams.get(session);
    if (ring === undefined) {
      pruneAgentStreams(agentStreamControlSession);
      ring = new AgentStreamPublisher({
        now: () => Date.now(),
        publish: (subject, payload) => connection?.publish(subject, codec.encode(payload)),
      });
      agentStreams.set(session, ring);
    }
    return ring;
  };
  // A session this process takes up with turns already in its transcript (its start on a session
  // it resumes: a pod's restart, `omp --continue`, `--resume`; a `/resume`; a fork) gets a ring
  // that holds none of them, so a viewer who opened it was replayed none of what its terminal
  // shows (dispatch://INFRA-5990). The ring a session is first given here is filled from its live
  // context (`liveContext`) instead, as the process that ran those turns recorded them. A person's
  // Send the session took as its typed turn carries its Dispatch message as it did then: the
  // turn's tag, where the transcript records that the session handled that message
  // (`handledMessages`), which is the turn `matchInjectedUserTurn` finds live. A session whose
  // ring is held already keeps it, and records its turns as they end.
  const restoreAgentStream = (
    session: string,
    live: readonly unknown[],
    entries: readonly unknown[]
  ): void => {
    if (session === "" || restoredAgentStreams.has(session)) return;
    restoredAgentStreams.add(session);
    const handled = handledMessages(entries);
    agentStreamOf(session).restore(transcriptMessages(live), (message) => {
      const tag = messageTag(message);
      return tag !== undefined && handled.has(tag) ? tag : undefined;
    });
  };
  // The control subscription this session currently holds, and the id it was opened for.
  let agentStreamControl: InboundSubscription | undefined;
  let agentStreamControlSession = "";
  let agentStreamCommandsRefreshedOnWatchAt = -Infinity;
  // When this session last put its context usage on the wire, which bounds how often it sends
  // one, and the reading a turn's last message is owed, waiting for that interval to be up
  // (`refreshAgentStreamContext`).
  let agentStreamContextRefreshedAt = -Infinity;
  let agentStreamContextTrailing: Timer | undefined;
  let agentStreamContextHeld:
    | { readonly session: string; readonly usage: ContextUsage | undefined }
    | undefined;
  let agentStreamViewerAttached = false;

  // The slash commands a viewer can send this session, in its terminal picker's order
  // (`#rebuildSlashCommandAutocomplete` in the host's `modes/interactive-mode.ts`): the host's
  // built-ins, each marked when only the terminal runs it; its extension, custom and skill commands
  // (`pi.getCommands`); then the file commands and prompt templates the live session expands in
  // `prompt()`, read from the host's own `AgentSession` (`slashCommands`, `promptTemplates`); a
  // host that lists no live session under this id (`liveAgentSession`) lists none of the two.
  // The host runs a built-in by its name or any alias before anything else, so those names
  // are reserved first; a name repeated among the rest runs its first source, and the publisher
  // keeps the first. Only a host that can run typed input from an extension offers any; one that
  // cannot sends no list, and its viewer offers no completion.
  const refreshAgentStreamCommands = (session: string): void => {
    if (typeof pi.sendUserInput !== "function") return;
    const builtins = hostBuiltinCommands?.() ?? [];
    const reserved = new Set(builtins.flatMap((builtin) => [builtin.name, ...builtin.aliases]));
    const live = liveAgentSession(session);
    const commands: AgentStreamCommand[] = builtins.map((builtin) => ({
      description: builtin.description,
      name: builtin.name,
      source: "builtin",
      ...(builtin.terminalOnly && { terminalOnly: true as const }),
    }));
    for (const command of [
      ...(pi.getCommands?.() ?? []),
      ...[...(live?.slashCommands ?? []), ...(live?.promptTemplates ?? [])].map((file) => ({
        description: file.description,
        name: file.name,
        source: "prompt" as const,
      })),
    ]) {
      if (reserved.has(command.name)) continue;
      const description =
        typeof command.description === "string" ? { description: command.description } : {};
      commands.push({
        name: command.name,
        source: command.source,
        ...description,
      });
    }
    agentStreamOf(session).setCommands(agentStreamFramesSubject(session), commands);
  };

  // The model and thinking level a viewer's header shows (`liveSessionSettings`). The host fires no
  // event when either is set (`/model`, `/effort`, its terminal's selector), so they are re-read on
  // every control message, on each turn's start, on a retry's fallback to another model, and after
  // a person's typed input runs. The reads are in-memory getters and the publisher sends only a
  // change, so nothing throttles them. A getter that throws must not suppress the replay answer, a
  // turn or a typed input's reply, so it is logged and the last frame stands.
  const refreshAgentStreamSettings = (session: string): void => {
    if (session === "") return;
    try {
      agentStreamOf(session).setSettings(
        agentStreamFramesSubject(session),
        liveSessionSettings(liveAgentSession(session))
      );
    } catch (error) {
      logger.warn("envoy: could not refresh agent stream settings", {
        error: messageFor(error),
        sessionID: session,
      });
    }
  };

  // How often one session may put its context usage on the wire. The figures move with every
  // message of a turn, and a viewer draws a ring from them, so a second or two of lag costs
  // nothing while a frame per message of a storm would be noise.
  const CONTEXT_FRAME_INTERVAL_MS =
    Number(process.env.ENVOY_CONTEXT_FRAME_INTERVAL_MS ?? "") || 2_000;
  // The context window's filling, as a viewer's ring shows it (INFRA-812). The host fires no event
  // for it either, so it is re-read after each message of a turn, at each turn's start and after a
  // compaction, and put on the wire at most once per interval. The read is an in-memory getter
  // over the session's own breakdown; a host too old to have it (`getContextUsage`, read behind
  // the same guard the host's own callers use) publishes nothing. A getter that throws must not
  // fail the handler it runs under, so it is logged and the last frame stands.
  //
  // The interval holds the frame back, never the read: a reading a turn's last message is owed -
  // a short reply, a quick tool round - is taken here and published when the interval is up, so
  // the ring follows the turn rather than holding the figure before its last message for as long
  // as the session then stays idle. The read cannot wait for the timer, because the host answers
  // for the turn that is running: measured on the pinned binary, a session whose turn read 7,661
  // tokens answers 5 two seconds later, with the turn over, and a ring drawn from that would show
  // a full session as empty.
  const publishAgentStreamContext = (session: string, usage: ContextUsage | undefined): void => {
    agentStreamContextRefreshedAt = Date.now();
    try {
      agentStreamOf(session).setContext(agentStreamFramesSubject(session), usage);
    } catch (error) {
      logger.warn("envoy: could not publish the agent stream's context frame", {
        error: messageFor(error),
        sessionID: session,
      });
    }
  };
  const refreshAgentStreamContext = (session: string): void => {
    if (session === "") return;
    const context = activeSessionContext;
    if (context === undefined || typeof context.getContextUsage !== "function") return;
    let usage: ContextUsage | undefined;
    try {
      usage = context.getContextUsage();
    } catch (error) {
      logger.warn("envoy: could not refresh agent stream context", {
        error: messageFor(error),
        sessionID: session,
      });
      return;
    }
    const waited = Date.now() - agentStreamContextRefreshedAt;
    if (waited >= CONTEXT_FRAME_INTERVAL_MS) {
      // A trailing frame still owed is due by now and holds the older reading: it is dropped
      // before this one goes out, or its callback would publish after this frame and leave a
      // session that then stays idle showing the earlier figure.
      clearTimeout(agentStreamContextTrailing);
      agentStreamContextTrailing = undefined;
      agentStreamContextHeld = undefined;
      publishAgentStreamContext(session, usage);
      return;
    }
    // Held rather than dropped, and the newest reading is the one that goes out.
    agentStreamContextHeld = { session, usage };
    if (agentStreamContextTrailing !== undefined) return;
    agentStreamContextTrailing = setTimeout(() => {
      agentStreamContextTrailing = undefined;
      const held = agentStreamContextHeld;
      agentStreamContextHeld = undefined;
      if (held !== undefined) publishAgentStreamContext(held.session, held.usage);
    }, CONTEXT_FRAME_INTERVAL_MS - waited);
    // A frame nobody is waiting on must not hold this process open at its end.
    agentStreamContextTrailing.unref?.();
  };

  // Drops the held reading and the frame it was owed: the session it was read from is no longer
  // the one this publisher serves, so its figures are no longer the ones a viewer would draw.
  const forgetAgentStreamContext = (): void => {
    clearTimeout(agentStreamContextTrailing);
    agentStreamContextTrailing = undefined;
    agentStreamContextHeld = undefined;
    agentStreamContextRefreshedAt = -Infinity;
  };

  const pumpAgentStreamControl = async (
    subscription: InboundSubscription,
    session: string
  ): Promise<void> => {
    try {
      for await (const message of subscription) {
        // Everything a control message can do runs inside this guard. Any client on the bus can
        // publish here, and an unhandled throw ends the `for await` — nats.js unsubscribes, and
        // the session goes deaf to watch and replay until the resubscribe delay below. A `null`
        // payload parses cleanly and then throws on `.type`; a reply over the server's
        // max_payload throws out of the publish.
        try {
          const parsed: unknown = JSON.parse(codec.decode(message.data));
          if (typeof parsed !== "object" || parsed === null) continue;
          const request = parsed as AgentStreamControlMessage;
          if (request.type !== "replay" && request.type !== "watch") continue;
          // A message already queued when the control subject moved belongs to a session whose
          // ring was dropped: answered, it would make that ring again and arm it.
          if (agentStreamControlSession !== session) continue;
          refreshAgentStreamSettings(session);
          const attached = agentStreamViewerAttached;
          if (
            request.type === "replay" ||
            !attached ||
            Date.now() - agentStreamCommandsRefreshedOnWatchAt >= AGENT_STREAM_WATCH_INTERVAL_MS
          ) {
            // A list is optional. A host getter or a third-party command with an invalid description
            // must not suppress the replay answer or leave this viewer unarmed.
            try {
              refreshAgentStreamCommands(session);
            } catch (error) {
              logger.warn("envoy: could not refresh agent stream commands", {
                error: messageFor(error),
                sessionID: session,
              });
            }
            if (request.type === "watch" && attached) {
              agentStreamCommandsRefreshedOnWatchAt = Date.now();
            }
          }
          // The answer is taken before this message arms anything, so a bare replay request
          // from a client that never attached a viewer gets an empty history, not the ring. It is
          // this session's own ring, which holds this session's turns alone.
          const ring = agentStreamOf(session);
          const reply = message.reply;
          const answer =
            request.type === "replay" && reply !== undefined
              ? JSON.stringify(ring.replay(session))
              : undefined;
          // A viewer arming a session nobody was watching is sent the settings and commands as
          // they stand: what was read before it arrived was kept unpublished, and a page held open
          // while this session was relaunched gets no replay from this process (`publishState`).
          const arming = !ring.watched;
          ring.noteViewer();
          agentStreamViewerAttached = true;
          if (arming) ring.publishState(agentStreamFramesSubject(session));
          if (answer === undefined || reply === undefined) continue;
          connection?.publish(reply, codec.encode(answer));
        } catch (error) {
          logger.warn("envoy: agent stream control message failed", {
            error: messageFor(error),
            sessionID: session,
          });
        }
      }
    } catch {
      // A dead iterator is a dropped connection, handled by the resubscribe below.
    }
    if (agentStreamControl === subscription) agentStreamControl = undefined;
    if (shuttingDown || agentStreamControlSession !== session) return;
    setTimeout(() => {
      if (shuttingDown || agentStreamControlSession !== session) return;
      void watchAgentStream(session).catch(() => undefined);
    }, RESUBSCRIBE_DELAY_MS);
  };

  const watchAgentStream = async (
    session: string,
    connect: () => Promise<SessionInbound> = ensureConnection
  ): Promise<void> => {
    // The listener's relay socket carries no agent stream: it refuses the control subject
    // (`subject_not_granted`), and the frames would reach no viewer. A session whose inbound is
    // the socket opens no live view, which its start notice says (`sessionSettings`). The pass opens
    // the relay subject first, so the connection is held here.
    if (connection?.liveView === false) return;
    if (agentStreamControlSession === session && agentStreamControl !== undefined) return;
    agentStreamControl?.unsubscribe();
    agentStreamControl = undefined;
    // A different session under the same extension instance is a different conversation: its
    // ring is its own, and of the sessions this process served before only a ring a viewer still
    // watches is kept, so a viewer opening the new id is served the new conversation alone.
    if (agentStreamControlSession !== session) {
      pruneAgentStreams(session);
      agentStreamViewerAttached = false;
      agentStreamCommandsRefreshedOnWatchAt = -Infinity;
      forgetAgentStreamContext();
    }
    agentStreamControlSession = session;
    // A queued task connects through its handle: an acquisition its task outran refuses there,
    // and the task ends, so a newer pass's control subscription is never overwritten behind its
    // back.
    const connected = await connect();
    // Another open of this subject - the pass's, or the pump's own resubscribe - may have landed
    // while this one dialed, or the host may have moved to another session: either way this
    // open takes nothing, so one subscription answers each viewer.
    if (agentStreamControlSession !== session || agentStreamControl !== undefined) return;
    const subscription = connected.subscribe(agentStreamControlSubject(session));
    agentStreamControl = subscription;
    void pumpAgentStreamControl(subscription, session);
    // A viewer's first ping is published as soon as the registration names this session, and one
    // sent before the server took this SUB would be dropped, leaving the viewer to retry. A round
    // trip this connection can still answer later does not fail the open, so only a connection
    // closed for good gets past the confirmation, and then this subject goes with it: this
    // function returns at once while `agentStreamControl` names the session, so clearing the field
    // is what lets the next pass - or `pumpAgentStreamControl`'s own end path, one resubscribe
    // delay later - open it again.
    try {
      await confirmSubscribed(connected, `the agent stream control subscription for ${session}`);
    } catch (error) {
      if (agentStreamControl === subscription) agentStreamControl = undefined;
      subscription.unsubscribe();
      throw error;
    }
  };

  /** One of this session's messages, on its way to whoever has the session open, tagged with the
   *  Dispatch message a user message delivered when it matches a person's turn this extension sent
   *  in. Only the top-level instance publishes: a `task` subagent's instance never learns a session
   *  id, and its work reaches the viewer as the parent's tool call anyway. `host` is the session
   *  the host records the message under, read from the event's own context: this instance's
   *  `sessionID` trails a move with no event (a rollback) until the next pass, and a message kept
   *  under it would be replayed to a viewer of a session it is not part of. */
  const recordAgentStreamMessage = (message: unknown, streaming: boolean, host: string): void => {
    if (sessionID === "" || host === "") return;
    agentStreamOf(host).record(
      agentStreamFramesSubject(host),
      message,
      streaming,
      matchInjectedUserTurn(host, message)
    );
  };
  /** The session the host is on as an event reports it, else as this instance last saw it. */
  const eventSessionID = (context: SessionContext | undefined): string =>
    context?.sessionManager.getSessionId() ?? hostSessionID();

  pi.on("message_start", async (event, context) => {
    // A tagged message starts in the live session's transcript, so its pictures join that set.
    const shown = typedPictures.started(event.message);
    if (shown !== undefined) markPicturesShown(shownPictures(sessionID), shown);
    recordAgentStreamMessage(event.message, true, eventSessionID(context));
  });
  pi.on("message_update", async (event, context) => {
    recordAgentStreamMessage(event.message, true, eventSessionID(context));
  });

  // The host writes a message to the transcript as it ends it: a card or a person's turn handed to
  // it, idle or taken by a turn after it streamed, is shown from here on (`UnwrittenFrames`). A
  // card's write fixes when it reached this session in the order every request reads it too: the
  // delivery the session records for the card's event, else the time the session's entries give
  // the event (its ledger record, or the newest message time up to its first entry, which is the
  // older card's for a card a resumed process replays), else the time of this write
  // (`../src/card-age`). A card the host appended also arms the wake's check (`../src/card-wake`).
  pi.on("message_end", async (event, context) => {
    const { message } = event;
    if (!shuttingDown) cardWake.messageEnd(message, context);
    const host = eventSessionID(context);
    recordAgentStreamMessage(message, false, host);
    // Every message of a turn moves the context window's filling, and this is where the host has
    // finished one.
    refreshAgentStreamContext(host);
    cardAges.written(
      message,
      context.sessionManager.getSessionId(),
      () => context.sessionManager.getBranch?.() ?? []
    );
    await unwritten.written(message);
  });
  // The host asks whether any extension handles `session_before_compact`, which turns its
  // background compaction off, but never these two: handling them changes nothing it does. A
  // compaction is the one moment the figures fall rather than climb, so both refresh the ring.
  // The host fires `session_compact` once the compaction entry is on the branch, and the pictures
  // the compaction cut off are no longer in any request: the session's shown set becomes what its
  // new live context shows (`liveContext`), replaced rather than added to, so a later read or
  // delivery sends such a picture again. It is the set of the session that compacted, the id its
  // own tool calls carry, which a task subagent's instance knows too.
  pi.on("session_compact", async (_event, context) => {
    cardWake.noteIfIdle(context);
    refreshAgentStreamContext(eventSessionID(context));
    const shown = shownPictures(context.sessionManager.getSessionId());
    shown.clear();
    markPicturesShown(
      shown,
      transcriptPictures(liveContext(context.sessionManager.getBranch?.() ?? []))
    );
  });
  pi.on("auto_compaction_end", async (event, context) => {
    cardWake.autoCompactionEnd(event);
    refreshAgentStreamContext(eventSessionID(context));
  });
  pi.on("auto_retry_end", async (event) => cardWake.autoRetryEnd(event));

  // A turn runs on the model and level set when it starts, and a retry's fallback moves the session
  // onto another model: either can follow a change no event announced. A subagent's instance knows
  // no session id and publishes nothing. Each frame goes to the ring of the session the host is on,
  // which this instance's `sessionID` trails after a move with no event.
  pi.on("turn_start", async (_event, context) => {
    const host = eventSessionID(context);
    refreshAgentStreamSettings(host);
    refreshAgentStreamContext(host);
  });
  pi.on("retry_fallback_applied", async (_event, context) => {
    refreshAgentStreamSettings(eventSessionID(context));
  });

  // The host's own session id. Oh My Pi moves its one session manager before it dispatches the
  // session event that moves `sessionID`, so between the two only this names the session open.
  const hostSessionID = (): string =>
    activeSessionContext?.sessionManager.getSessionId() ?? sessionID;
  // Whether `id` is the session open here, for this instance and the host alike: a role move
  // decided for `id` acts only while it is.
  const sessionOpen = (id: string): boolean => sessionID === id && hostSessionID() === id;
  // What a role move that decided nothing tells the plugin or the model: the session it was
  // decided for stopped being the open one. A claim the listener granted all the same is held
  // here under that session, and the next pass carries it or hands it back; one that never
  // landed claimed nothing. Both the plugin bridge's claim and the tool say the same sentence.
  const sessionLeftDuringClaim = (
    from: string,
    role: string,
    claimAdopted: boolean
  ): SessionLeftError =>
    new SessionLeftError(
      claimAdopted
        ? `the session moved from ${from} to ${hostSessionID()} while its claim of role ${role} was in flight; the next pass keeps the hold for the session the host is on, or hands it back`
        : `the session moved from ${from} to ${hostSessionID()} while its claim of role ${role} ran; nothing was claimed`
    );

  // A role claim the model or a plugin asks for once this session's stop has begun is refused
  // (R24 of dispatch://INFRA-3589). The stop hands back every role this process holds and deletes
  // the session's record next, and a move whose claim was in flight when the stop began keeps
  // nothing of it. The plugin bridge's claim, its role-bound notice subscription and the tool say
  // the same sentence (`RoleClaimStoppingError`), which the Legion boot and the Legion controller
  // read as their session's stop.
  const refuseClaimWhileStopping = (role: string): void => {
    if (shuttingDown) throw new RoleClaimStoppingError(role);
  };

  // The start of the plugin bridge's claim and of its role-bound notice subscription once this
  // instance's stop has begun. The instance stays on the bridge after its stop
  // (`shutDownSession`), so this answers for as long as the process lives. A call for the session
  // this instance serves, which the bridge routes to it by that id, is refused naming the stop
  // (`RoleClaimStoppingError`), and no other instance gets it. A call for a session this instance
  // does not serve reached it through the bridge's fallback (`instances.at(-1)`), and this stop is
  // not that session's: it is refused as an instance that serves no such session refuses it, so
  // the Legion entry does not take it for its own session's stop.
  const refuseBridgeWhileStopping = (targetSessionID: string, role: string, call: string): void => {
    if (!shuttingDown) return;
    if (targetSessionID === hostSessionID()) throw new RoleClaimStoppingError(role);
    throw new Error(`Envoy has no active session for Legion ${call}: ${targetSessionID}`);
  };

  // The Envoy role a session pod's launch named (`ENVOY_SESSION_ROLE`) and the one session it named
  // it for (`ENVOY_SESSION_ROLE_SESSION`, the id the pod starts the agent with; the deployment
  // repository's sessions workload sets both once t-e-32 lands). That session claims the role
  // itself, under its own credential, which the listener holds to that very session, at every
  // pass that registers it (`settleRole`). No other session claims it, by any path: not a process
  // the agent starts in the pod, such as an `omp -p` review, which gets the same environment; not
  // an ACP host's other session; not an id a `/new`, a `/fork` or a tree branch mints, though a
  // fork's or a branch's transcript carries the named session's record of the claim. Each pass
  // compares the session open then. Read when the instance is created; nothing takes them out of
  // the environment.
  const sessionRole = listenerRoleName(process.env.ENVOY_SESSION_ROLE ?? "") || undefined;
  const sessionRoleSession = process.env.ENVOY_SESSION_ROLE_SESSION?.trim() || undefined;
  // The topics the deployment names for each role (`ENVOY_ROLE_TOPICS`), which the session holding
  // that role follows for as long as it holds it, however it took it: a launch's role, a Legion
  // claim, `envoy_role_set`. A pod's launch sets the variable, as it sets the role, so the session
  // that takes the role after a relaunch follows them again. Read when the instance is created; a
  // value it refuses is logged here and said at each session start, and no role follows any of it.
  const roleTopics = readRoleTopics(process.env[ROLE_TOPICS_VAR]);
  if (roleTopics.error !== undefined) logger.warn(`envoy: ${roleTopics.error}`);
  const roleTopicFollows = createRoleTopicFollows({ roleTopics, follow: followNew });
  // Why session `id` may not claim `role`, or undefined when it may: the launch's role is the
  // named session's alone, so every other session is refused it, and with no session named every
  // session is. The name is read as the listener reads a claim's (`listenerRoleName`), so a padded
  // name is the same role. The process logs the refusal once (`SESSION_ROLE_LOGGED`), whichever
  // session and path meets it first, and the person is told once for each session refused, on
  // screen, where the role stays and how to take it back.
  const sessionRoleNotified = new Set<string>();
  const sessionRoleRefusal = (id: string, role: string): string | undefined => {
    if (
      sessionRole === undefined ||
      listenerRoleName(role) !== sessionRole ||
      id === sessionRoleSession
    ) {
      return undefined;
    }
    const refusal = `envoy: ENVOY_SESSION_ROLE ${sessionRole} belongs to session ${sessionRoleSession ?? "none"}; this session ${id} claims nothing`;
    if (sessionRoleLoggedStore[SESSION_ROLE_LOGGED] === undefined) {
      sessionRoleLoggedStore[SESSION_ROLE_LOGGED] = true;
      logger.warn(refusal);
    }
    if (!sessionRoleNotified.has(id)) {
      sessionRoleNotified.add(id);
      notify(
        activeSessionContext,
        sessionRoleSession === undefined
          ? `envoy: role ${sessionRole} is the pod's, but the pod names no session for it, so no session claims it; messages to the role are not delivered here`
          : `envoy: role ${sessionRole} stays with the pod's session ${sessionRoleSession}; messages to the role are not delivered here. /resume ${sessionRoleSession} takes it back`,
        "warning"
      );
    }
    return refusal;
  };
  // A claim the model or a plugin asks for refuses the launch's role to a session the launch did
  // not name, before it calls the listener.
  const requireSessionRoleFor = (id: string, role: string): void => {
    const refusal = sessionRoleRefusal(id, role);
    if (refusal !== undefined) throw new Error(refusal);
  };

  // The registration and role state the two serial queues write, the queues themselves and this
  // extension instance's client instance id, all private to `../src/registration-state`. That
  // module builds the queues from the generic serial queue of `../src/task-queue`, which holds
  // each task's deadline timer. A queued task writes none of it by hand: `run` hands it a handle
  // whose every step — a state write, a listener request, a NATS connection, a subscribe —
  // refuses once that task was abandoned at its deadline or the session it acts for stopped being
  // the open one, and a role move's once this session's stop has begun (`shuttingDown`). The
  // first refusal latches. Nothing here re-checks a fence before a step.
  // `handle.check()` is left at the two places that are not steps: where a catch-all would
  // otherwise swallow the refusal, and before an action outside the handle (a replay trigger, a
  // throw this pass reports) that a refused task must not take.
  const registrationState = createRegistrationState({
    client: () => requireEnvoy().client,
    sessionOpen,
    stopping: () => shuttingDown,
    deadlineMs: passDeadlineMs,
    notify: (message, level) => notify(activeSessionContext, message, level),
    log: logger,
    appendRoleClaimEntry: ({ role, sessionID: claimedFor, clientInstanceID }) => {
      // The transcript is the one thing `omp --resume` guarantees, so it is the durable record
      // of the claim: the listener reaps a dead session's interest row 15 minutes after the
      // row's last write and releases the role claim separately, one session TTL after
      // heartbeats stop (a role claim is not an interest: see `ReapRoleClaims` in
      // packages/envoy/internal/store/kv.go), and this process's memory dies with it. A session
      // that holds a role heartbeats from here on, whichever claim took it. The record names the
      // session the claim was made for and this process, so a second process reading the same
      // transcript — a continuation that resumed a live session's id — tells its own records
      // from this one's. The launch's role is claimed again at every pass, so a claim of it that
      // the newest record on the branch already names — that role, for that session, by this
      // process — writes nothing. A stopping session writes no record at all (the write gate).
      if (shuttingDown) return;
      const newest =
        role === sessionRole
          ? transcriptRoleRecord(activeSessionContext?.sessionManager.getBranch?.() ?? [])
          : undefined;
      const recorded =
        newest?.role === role &&
        newest.session_id === claimedFor &&
        newest.client_instance_id === clientInstanceID;
      if (!recorded) {
        pi.appendEntry(ROLE_CLAIM_ENTRY, {
          role,
          session_id: claimedFor,
          client_instance_id: clientInstanceID,
        });
      }
      if (activeSessionContext !== undefined) ensureHeartbeat(activeSessionContext);
    },
    dial: dialInbound,
    connect: ensureConnection,
    subscribeTopic: async (topic) => follow(topic),
    closeSubjects: (held, subjects) => {
      // Each closed subject exactly: `unfollow` would expand a closed `X.>` back to `X` as well,
      // and `X` can be the session's own subject, which the role's end leaves followed.
      for (const subject of subjects) followed.delete(subject);
      const id = held === "" ? sessionID : held;
      if (subjects.length === 0 || id === "") return;
      // A base the session still follows leaves the row with a closed `X.>` (`takesFollowedBase`),
      // so the session registers again, which puts back every subject it follows, as
      // `envoy_unsubscribe` does. Each step warns in its own words.
      const keptBase = takesFollowedBase(subjects) && id === sessionID;
      void active?.client.unsubscribe({ sessionID: id, topics: subjects }).then(
        () =>
          keptBase
            ? registerSession().then(
                () => undefined,
                (error: unknown) =>
                  logger.warn(
                    "envoy: a followed subject was not registered again; the next heartbeat does it",
                    { sessionID: id, error: messageFor(error) }
                  )
              )
            : undefined,
        (error: unknown) =>
          logger.warn(
            isBusUnanswered(error)
              ? busUnansweredRemoval("a role's notice subjects or topics")
              : "envoy: a role's notice subjects or topics were not taken out of the registry",
            { topics: subjects, sessionID: id, error: messageFor(error) }
          )
      );
    },
    roleTopics: roleTopicFollows,
  });

  // Follows `topic` unless it served a role that has ended since `snapshot` was taken: a
  // role-bound subject is not followed again, and one whose role ends meanwhile is dropped
  // before any registration can carry it. Answers whether the topic is still wanted.
  const followUnlessRoleEnds = (topic: string, snapshot: RoleSnapshot): boolean => {
    const roleEnded = () =>
      snapshot.roleBound && registrationState.roleEndCount() !== snapshot.endsBefore;
    if (roleEnded()) return false;
    follow(topic);
    if (!roleEnded()) return true;
    unfollow(topic);
    return false;
  };

  /**
   * The registrations on the wire, by the session each registers. A `DELETE /v1/sessions/<id>`
   * that overtakes one is undone when it lands: the listener writes the record again, and a
   * session this process no longer heartbeats reads as live for the whole live window and as
   * unresponsive for the gone horizon after it, so peers wait an hour on a pane that has gone
   * (dispatch://INFRA-392). Every deregistration waits its session's registration out first.
   */
  const registrationsInFlight = new Map<string, Promise<void>>();

  /** Records `registering` as the registration of `id` on the wire, for a deregistration of that
   *  session to wait out, and answers it unchanged. Every registration this process makes passes
   *  through here: a pass's, through its handle, and the one outside any queue. */
  const trackRegistration = <T>(id: string, registering: Promise<T>): Promise<T> => {
    if (id === "") return registering;
    // Tracked as a settled-either-way promise, so a deregistration waits for the call to leave
    // the wire without taking on its rejection.
    const settled = registering.then(
      () => undefined,
      () => undefined
    );
    registrationsInFlight.set(id, settled);
    void settled.then(() => {
      if (registrationsInFlight.get(id) === settled) registrationsInFlight.delete(id);
    });
    return registering;
  };

  /** The whole of what a stop (`stopSession`) gives its role release, its deregistration and its
   *  drain. Oh My Pi abandons a shutdown handler after 2 s (`SESSION_SHUTDOWN_HANDLER_TIMEOUT_MS`),
   *  and the second this leaves is for the synchronous work ahead of the race and for the host. A
   *  SIGTERM gets no more: in a Legion pane the shim kills Oh My Pi 5 s after its SIGTERM
   *  (`DefaultGrace`, `packages/daemon/internal/shim`), and the agent shares that grace. The
   *  shared `SHUTDOWN_SETTLE_MS` is what the stop spends waiting on a registration still on the
   *  wire: `REGISTRATION_SETTLE_MS` is five times this whole deadline, so a registration slower
   *  than a second would mean no DELETE was sent at all and the record outlived this process by
   *  the gone horizon, where a DELETE that races a slow registration only may lose. The release
   *  of the session's role claims goes before both (`SHUTDOWN_RELEASE_MS`), so the two waits
   *  leave at least 500 ms here (1.5 s of the host's 2 s) for the DELETE to reach the wire. */
  const SHUTDOWN_DEADLINE_MS = 1_000;

  /** How long the DELETE of a stop waits for the release of the session's role claims, which goes
   *  first: a quarter of a second, as the wait on a registration still on the wire. A listener
   *  that answers at all answers a release in tens of milliseconds. A release that has not
   *  answered by then is no longer waited for: the DELETE goes, and nothing sends the release
   *  again. That release can still land while the process lives; when it does not, the claim
   *  stands until its last renewal is older than the listener's live window. The DELETE goes all
   *  the same, because it is what lets a message to the stopped session wake it. */
  const SHUTDOWN_RELEASE_MS = 250;

  /** Deregisters a session this process has stopped heartbeating — its own at shutdown, the one
   *  it left at a `/new`, a fork or a branch — once that session's registration has settled or
   *  `settleMs` have passed. The DELETE carries this client instance's id
   *  (`registrationState.deregister`), so it removes only a record this instance's registration
   *  wrote. Where the caller must be done by `endsAt` (a shutdown), the DELETE gets what is left
   *  until then: its credential waits on that less the request's reserve, so a broker credential
   *  still waiting on a trade leaves the DELETE time to go without it. */
  const deregisterSession = async (
    id: string,
    settleMs: number = REGISTRATION_SETTLE_MS,
    endsAt?: number
  ): Promise<void> => {
    if (id === "" || active === undefined) return;
    const registering = registrationsInFlight.get(id);
    if (registering !== undefined) {
      const waited = Promise.withResolvers<void>();
      const timer = setTimeout(waited.resolve, settleMs);
      try {
        await Promise.race([registering, waited.promise]);
      } finally {
        clearTimeout(timer);
      }
    }
    await registrationState.deregister(id, endsAt === undefined ? undefined : endsAt - Date.now());
  };

  // Said once per outage: the listener refused this session's registration or may not have
  // recorded it, or the session holds no relay subject. The session still runs, and the heartbeat
  // registers again every tick until one is accepted. The outage's kind (`InboundOutageKind`,
  // which says what each kind means for what reaches the session) picks the log line and the pane
  // note (`INBOUND_UNAVAILABLE_LINES`), and `inboundUnavailableNoticeText` words the notice for
  // every host. A first relay open still being tried at the start, or a run of bus-outage 503s,
  // is told only once `INBOUND_FIRST_OPEN_BOUND_MS` has passed. The log does not wait for the
  // start race: the first registration that found no relay subject is logged at once, at warn
  // (`held`). That line says only what the registration found, and why, and makes no claim about
  // what the session receives. The notice, its card, its pane note and the warning that the
  // session receives nothing wait for the bound.
  // An accepted registration after a told outage tells the session once, by the same path, worded
  // for the kind of the last outage it was told (`INBOUND_RESTORED_LINES`). A session whose stop
  // has begun is told neither (`stopping`): the stop closed the relay on purpose, and the record
  // is deleted next. This report is the one choke point of those notices, and each of its tells
  // goes through the tell gate as well.
  const inboundOutage = inboundUnavailable(
    (outage) => {
      const lines = INBOUND_UNAVAILABLE_LINES[outage.kind];
      logger.warn(lines.log(outage.why));
      notify(activeSessionContext, lines.pane(outage.why), "warning");
      sendNotice(inboundUnavailableNoticeText(outage));
    },
    {
      restored: ({ kind }) => {
        const lines = INBOUND_RESTORED_LINES[kind];
        logger.info(lines.log);
        notify(activeSessionContext, lines.pane, "info");
        sendNotice(inboundRestoredNoticeText(kind));
      },
      held: (relayMissing) => {
        logger.warn(
          `envoy: a registration found that this session holds no relay subject${relayMissing === undefined ? "" : `, because its relay subject could not be opened: ${relayMissing}`}`
        );
      },
      stopping: () => shuttingDown,
    }
  );

  // Where this session's pane sits in tmux, so Dispatch can draw the machine's layout from what
  // the sessions themselves report. The reading runs between registrations, never inside one: a
  // registration takes what the cache holds in the turn it is built (`sendRegistration`), so a
  // tmux server driving a wall of panes delays no registration and leaves none in flight behind
  // a slow call. The cache reads once a `tmuxPlaceCacheMs` — on a timer of its own between the
  // heartbeats, which are three lifetimes apart — rather than once a registration, so a shared
  // server is asked once a lifetime by each of its sessions and the place a registration carries
  // is a lifetime old rather than a heartbeat old. A session shutting down stops that timer and
  // stops the reading already away from writing anything (`createTmuxPlaceCache`).
  const tmuxPlaces = createTmuxPlaceCache({
    read: () => readTmuxPlace(process.env, defaultExec),
    lifetimeMs: () => active?.settings.tmuxPlaceCacheMs ?? DEFAULT_TMUX_PLACE_CACHE_MS,
    onFailure: (error) =>
      logger.debug("envoy: the tmux place reading failed", { error: messageFor(error) }),
  });

  // One registration, whichever path sends it: `registrationState.refreshRegistration` outside a
  // queue, a queued task's own fenced `handle.register` inside one. The sequence every host
  // shares is `registerWithRelay`: the topics the listener will store for this session, the
  // relay's delivery request, the outage told once, and the answer's consumer taken
  // (`takeDeliveryAnswer`).
  //
  // The listener delivers nothing to a self-subscribed session (its own consumer's relay does),
  // so the registered topics are what that consumer is filtered on, which `registrationTopics`
  // decides for every host: every subject the session follows, a role-bound one included, since
  // a notice lane reaches this session from its own consumer and from nowhere else (INFRA-771),
  // but one the listener would not store for it. They are read afresh at every registration.
  // `relayMissing` is why the pass could not open the relay, which the refusal of a registration
  // with none, and so the outage notice, names. A registration sent outside a pass while the
  // session's relay subject is missing names why it went (`Relay.missing`) instead. `dropped`, where
  // given, takes every topic an edge listener refused in this registration, for a caller that names
  // them by what its own registration was answered rather than by what the session follows after.
  //
  // A stopping session registers nothing (R24 of dispatch://INFRA-3589). Its stop closes the
  // relay on purpose and deletes the record next, so a registration that a queued pass or a timer
  // makes once the stop began has nothing to ask for. It is refused here, before
  // `registerWithRelay`, with the error that function throws for it (`RegistrationStoppingError`):
  // a pass reads it as a pass that decided nothing (`reconcileNow`), and every other caller that
  // reports it reports the truth. A registration already on the wire when the stop begins is not
  // sent again (`stopping`): a project refusal answered after that drops no topic, and the
  // session is not told of it. The pass that sent it ends once it settles, accepted or not, with
  // no role move (`reconcileNow`).
  const sendRegistration = (
    send: (input: RegistrationInput) => Promise<SubscribeResult>,
    relayMissing?: string,
    dropped?: string[]
  ): Promise<SubscribeResult> => {
    if (shuttingDown) return Promise.reject(new RegistrationStoppingError());
    // Taken in the turn this call starts, with no await ahead of it, so the whole body is built
    // and posted without a tmux call in between: the session the deregistration at shutdown
    // waits out is the one that reaches the wire, and no registration can resume behind the
    // DELETE (dispatch://INFRA-392). The cache answers the last reading, none while it has had
    // none, and starts the reading the registration after this one carries.
    const place = tmuxPlaces.place();
    const registeringFor = sessionID;
    const asking =
      relay?.sessionID === registeringFor && relay.subscription !== undefined ? relay : undefined;
    const role = registrationState.heldRole();
    return registerWithRelay({
      relay: asking?.delivery,
      relayMissing:
        relayMissing ??
        (asking !== undefined
          ? undefined
          : relay?.sessionID === registeringFor && relay.missing !== undefined
            ? relay.missing
            : passFailure?.sessionID === registeringFor
              ? passFailure.text
              : undefined),
      ownAgentSubject: legacyAgentSubject(registeringFor),
      followed: () => followed,
      roles: role === undefined ? [] : [role],
      outage: inboundOutage,
      send: (topics, delivery) =>
        send({
          sessionID: registeringFor,
          directory: sessionDirectory,
          topics,
          port: 0,
          // Read at every registration: the heartbeat re-registers, which picks up
          // titles assigned after session_start and later renames.
          title: activeSessionContext?.sessionManager.getSessionName?.() ?? "",
          capabilities:
            sideTurn(activeSessionContext) === undefined
              ? CAPABILITIES_WITHOUT_BTW
              : DELIVERY_CAPABILITIES,
          driving: false,
          selfSubscribed: true,
          plugin: { name: packageName, version: packageVersion },
          delivery,
          ...(place === undefined ? {} : { place }),
        }),
      // A registration the listener refused, for any of the reasons `ErrorWireSchema.reason`
      // lists, leaves the session with no consumer at all: the session is told once, at once, and
      // the heartbeat asks again. A registration the bus-outage 503 answered (`isBusUnanswered`)
      // is not recorded for certain, but its consumer may still deliver. That failure is held
      // for the bound first (`INBOUND_FIRST_OPEN_BOUND_MS`): the session is told once only when
      // such a failure comes at or after the bound's end, and an accepted registration inside
      // the bound ends the hold with nothing said.
      answered: (result) => {
        if (asking !== undefined) takeDeliveryAnswer(registeringFor, asking, result);
      },
      // An edge listener refused these topics for the projects of the session's token: they stop
      // being followed, here and in the ledger, so no later registration carries them and a
      // restore does not follow them again, and the session is told which, once, in the places
      // it is told an outage, whichever path registered. Registrations are not serialized, so one
      // that carried a topic another already dropped tells nothing of it again. A session the
      // host has since left keeps its own.
      drop: (topics, reason) => {
        dropped?.push(...topics);
        if (sessionID !== registeringFor) return;
        const stillFollowed = topics.filter((topic) => followed.delete(topic));
        // A role's topic the edge refused leaves the role's and is not followed again for it
        // until the role ends, so `envoy_role_set` no longer names it and no heartbeat sends it.
        roleTopicFollows.refuse(topics);
        if (stillFollowed.length === 0) return;
        ledgerFollowedChanged().catch((error: unknown) => {
          logger.warn(
            "envoy: the topics the listener refused stay in the ledger until it is written next",
            {
              error: messageFor(error),
            }
          );
        });
        notify(
          activeSessionContext,
          `envoy: the listener refused ${stillFollowed.join(", ")} for this session's projects (${reason}); the session no longer follows them`,
          "warning"
        );
        sendNotice(refusedTopicsNoticeText(stillFollowed, reason));
      },
      log: (line) => logger.warn(`envoy: ${line}`),
      stopping: () => shuttingDown,
    });
  };

  // Registers the session outside any queue, carrying this instance's client instance id: a
  // registration moves the session's `last_seen` at the listener even when its answer is lost, so
  // the module first takes back every hold this process abandoned under that session, and times
  // the answer on the one clock the heartbeat's stall test reads. It is recorded as on the wire
  // for its session, so a deregistration of that session waits it out.
  // `dropped` as `sendRegistration`'s.
  const registerSession = (dropped?: string[]) =>
    trackRegistration(
      sessionID,
      sendRegistration(registrationState.refreshRegistration, undefined, dropped)
    );

  // The session title the `dispatch` command reads from the session's `title` file. A shell
  // command takes the environment Oh My Pi captured at the last session event, and a session is
  // named after it starts (the host's automatic title, a Legion pane's), so the name goes
  // through a file, rewritten whenever it changed: at each registration heartbeat while Envoy is
  // on, and at each tool call of the main agent, whatever the tool. A title the host changes
  // reaches the file at the next of those two, and the file is stale until then. The memo is
  // what keeps an unchanged session from writing the file again at every one of them.
  let writtenTitle: { readonly sessionID: string; readonly title: string } | undefined;
  /** Writes the host's live session name to the title file of the session the environment names.
   *  That id is the `dispatch` command's own (`DISPATCH_SESSION_ID`), not the registration's
   *  `sessionID`, which can differ from it, so both writers share one memo and one file: the one
   *  that command reads. Nothing is written while that name is unset, and nothing is written for
   *  an empty title, which would take the session's name away rather than leave it standing
   *  (`readSessionTitle` answers `undefined` for a file holding one). It throws what
   *  `sessionDirectory` throws for a session id that is not one plain path segment, and what the
   *  write itself throws; the writers `dispatchTitleWriter` builds swallow that for both of its
   *  call sites, since a cosmetic title must fail neither a heartbeat nor a tool call. */
  const writeDispatchTitle = (context: SessionContext): void => {
    const id = process.env.DISPATCH_SESSION_ID;
    if (id === undefined || id === "") return;
    const title = context.sessionManager.getSessionName?.() ?? "";
    if (title === "") return;
    if (writtenTitle?.sessionID === id && writtenTitle.title === title) return;
    writeSessionTitle(dispatchSessionDirectory(process.env, id), title);
    writtenTitle = { sessionID: id, title };
  };

  /** Builds one call site's title writer: it writes the file and swallows a failed write, saying
   *  so once. The flag is the writer's own closure variable, as this module's other
   *  once-per-episode flags are (`heartbeatOutageNotified`, `stallNotified`), so its lifetime is
   *  the writer's: each site builds a writer of its own — the heartbeat's, which lives as long as
   *  that heartbeat, and the main agent's tool call's, which lives as long as the session — and a
   *  site cannot be handed another site's flag by mistake. So a session whose writes fail at both
   *  sites still says one line per site, and says it once, since a write that fails fails the same
   *  way at every tick and every call. */
  const dispatchTitleWriter = (): ((context: SessionContext) => void) => {
    // Never cleared: a write that cannot reach the session's state directory fails the same way
    // at every tick and every call, and one line says it.
    let notified = false;
    return (context: SessionContext): void => {
      try {
        writeDispatchTitle(context);
      } catch (error) {
        if (notified) return;
        notified = true;
        logger.warn(
          `envoy: could not write the session title for the dispatch command (${messageFor(error)}); the dispatch command's writes carry no session title`
        );
      }
    };
  };

  const ensureHeartbeat = (context: SessionContext): void => {
    if (heartbeatRegistered) return;
    // Never let a heartbeat tick reject unhandled: OMP treats unhandled
    // rejections as fatal (postmortem exitAfterFatal), so a registry blip
    // would kill a live session. Warn once per outage; registration
    // self-heals on the next successful tick.
    let heartbeatOutageNotified = false;
    // Said once per stall episode, by whichever symptom shows first: a tick that had to refresh
    // the registration itself because a pass was still running, or a pass abandoned at its
    // deadline. One episode, one notification and one log line; cleared when a pass next settles.
    let stallNotified = false;
    // Set only by a failed registration, never by a failed role check afterwards: the listener
    // answers "no holder" for a session whose registry entry lapsed, not for a role read that
    // failed, and only the former warrants a `"reregistered"` regain on the next healthy tick.
    // Cleared only once a whole pass — registration and role — has decided, so a recovery tick
    // whose role read errors, or whose pass the host overtook, still owes the regain.
    let registrationFailed = false;
    let healing = false;
    // Whether the one extra call a failed tick buys is already owed. Armed by a failed interval
    // tick, spent by the call it schedules, and cleared by any pass that settles.
    let retryArmed = false;
    // This heartbeat's own title writer, which says a failed write once (`dispatchTitleWriter`):
    // a write that cannot reach the session's state directory fails the same way at every tick.
    const writeHeartbeatTitle = dispatchTitleWriter();
    // The pane hears about a stall as it hears about a registry outage, and for the same reason:
    // the session is at risk of being reaped by the listener while the pane runs on. A session
    // whose stop has begun hears of none (R24 of dispatch://INFRA-3589, the tell gate): a pass
    // abandoned at its deadline after that point leaves nothing to recover, since the DELETE
    // follows it.
    const noteStall = (detail: string): void => {
      if (stallNotified) return;
      stallNotified = true;
      logger.warn("envoy: reconcile pass stalled; registration refreshed on its own", {
        detail,
        sessionID: context.sessionManager.getSessionId(),
      });
      notify(
        context,
        `envoy: a reconcile pass has stalled (${detail}); the session registration is being refreshed on its own, and recovers once the pass settles`,
        "warning"
      );
    };
    const tick = (fromRetry: boolean): void => {
      // A shutting-down session is about to be deregistered: a tick past that point would put a
      // registration back on the wire behind the DELETE and leave the record standing.
      if (shuttingDown) return;
      // Sessions can be created lazily after session_start (a fresh TUI has no session yet); until
      // the host mints an id there is nothing to register.
      if (context.sessionManager.getSessionId() === "") return;
      // The session's title file, so a session that runs without a tool call of its own — a pane
      // waiting on a message, a session renamed after it started — still has the name the
      // `dispatch` command reads. Written here, synchronously, rather than in the registration
      // body: that body runs inside a promise this tick does not wait for, and a throw there
      // would fail the registration. A failure never fails a heartbeat, as `stampOnce` in
      // packages/claude-envoy/src/envoy-channel-server.ts holds for its own stamp; it is said
      // once, since a title write that fails fails the same way at every tick.
      writeHeartbeatTitle(context);
      // A held inert card or notice, or a card the host withdrew unwritten, is handed first,
      // while the host is settled (`hostSettled`).
      unwritten.handBackWithdrawn();
      if (healing) {
        // A pass that has not settled holds the queue, and with it the registration refresh that
        // is its last step. The age is measured from the last registration accepted for the
        // session running now: one accepted for an earlier session, which is what a `/new` during
        // a wedged pass leaves behind, counts as none at all, or the new session would sit out of
        // the listener's sessions for a whole interval while its pane runs. A session with no
        // accepted registration of its own counts as older than any age. A pass still running a
        // whole refresh interval later has already spent 90 s of the record's 300 s lifetime, so
        // the refresh goes out on its own, outside the queues, exactly as `envoy_subscribe` posts
        // it. A failure is ignored: the next tick tries again.
        const refreshMs = requireEnvoy().settings.heartbeatMs;
        if (registrationState.registrationFresherThan(sessionID, refreshMs)) return;
        noteStall(
          "a pass has not settled a whole refresh interval after the last accepted registration"
        );
        void registerSession().catch(() => undefined);
        return;
      }
      healing = true;
      const afterOutage = registrationFailed;
      const settled = (decided: boolean) => {
        if (decided) registrationFailed = false;
        heartbeatOutageNotified = false;
        stallNotified = false;
        // Back to the plain interval, and a pending extra call is dropped with it: the
        // registration it would refresh is fresh now, and a listener that is still slow is owed
        // nothing.
        retryArmed = false;
      };
      void reconcile(context, { heartbeat: true, afterOutage })
        .then(settled, (error: unknown) => {
          // NATS is down but the registration and the role went through: the session's start or
          // its session event warned of the outage, and every tick tries the agent subject again.
          if (error instanceof InboundError) return settled(error.decided);
          // The pass, or the role move inside it, was abandoned at its deadline. That is an
          // outage like a failed registration: the listener reaps a role claim whose holder left
          // the session registry after one record lifetime, so the next healthy tick re-asserts
          // the role and fires the regain hook (`"reregistered"`), which makes a Legion worker
          // known to its daemon again. One notification per episode, so it stops here rather
          // than becoming a second `registry heartbeat failed` line.
          if (taskWasAbandoned(error)) {
            registrationFailed = true;
            noteStall(messageFor(error));
            return;
          }
          if (error instanceof RegistrationError) registrationFailed = true;
          throw error instanceof RegistrationError ? error.cause : error;
        })
        .catch((error) => {
          // A tick that fails once the stop has begun is no outage: no heartbeat follows it, and
          // the DELETE does, so the line would promise a retry that never comes. Nothing is
          // armed, and the tell gate drops what would be said (R24 of dispatch://INFRA-3589).
          if (shuttingDown) return;
          // The extra call this failure buys, armed only from an interval tick: a retry that
          // fails is followed by the next tick, not by a second retry, so a listener that keeps
          // refusing sees one extra call per interval and no more.
          const retryIn = fromRetry ? undefined : armRetry();
          if (heartbeatOutageNotified) return;
          heartbeatOutageNotified = true;
          const retrying =
            retryIn === undefined
              ? "retrying at the next heartbeat"
              : `retrying once in ${Math.round(retryIn / 1000)} s, then every heartbeat`;
          notify(
            context,
            `envoy: registry heartbeat failed (${messageFor(error)}); ${retrying}`,
            "warning"
          );
        })
        .finally(() => {
          healing = false;
        });
    };
    /** Schedules the extra call a failed heartbeat buys and answers how far out it is, or
     *  nothing when one is already owed. The delay is the shared draw
     *  (`heartbeatRetryDelayMs`), the one claude-envoy's channel server retries on too, so the
     *  panes a slow listener failed together come back spread rather than in one burst. The
     *  timer is the host's, so it is cleared with the session. */
    const armRetry = (): number | undefined => {
      if (retryArmed) return undefined;
      retryArmed = true;
      const delayMs = heartbeatRetryDelayMs();
      context.setTimeout(() => {
        // A pass settled while the delay ran: the registration is fresh and the call is dropped.
        if (!retryArmed) return;
        retryArmed = false;
        tick(true);
      }, delayMs);
      return delayMs;
    };
    context.setInterval(() => tick(false), requireEnvoy().settings.heartbeatMs);
    heartbeatRegistered = true;
  };

  const recoverRegisteredInterests = async (handle: TaskHandle): Promise<void> => {
    // Registered interests are what the session's own consumer is filtered on, so a resumed
    // process must rebuild them or stay deaf to
    // topics it registered before it died (e.g. dispatch thread replies).
    // Quiet on failure like rebind: a brand-new session has no registry
    // entry, and a listener outage must not fail session start.
    // The role lanes (`notifications.role.<role>`) are not followed here: role deliveries arrive
    // on the agent subject, and a pass claims a role back once registered (`settleRole`).
    // In the daemon-launched controller (the bridge's `slackRoleOwned`), every registered Slack
    // topic is unsubscribed from the listener and never restored: a restored topic would be
    // followed for as long as the session runs, outliving the controller role, and the controller
    // hears its listed channels through the topics its role owns (pi-legion's
    // controller-session.ts).
    // The topics the newest ledger state names come back too: the listener reaps a dead session's
    // interests after its grace, and the transcript still says what the session followed. A topic
    // it unfollowed is absent from that state.
    //
    // `fate` decides each registered or remembered topic once, the first that fits:
    //  - A lane: the session's agent subject or a role lane, which this function never follows.
    //  - The role's: a topic the deployment names for a role (`ENVOY_ROLE_TOPICS`) that the
    //    ledger does not remember as the session's own. A process that ended never closed it
    //    (`session_shutdown` deregisters, and a kill runs nothing), so the row still holds it.
    //    It is not followed as the session's own; the role this session wants opens it below.
    //  - Retired: a Slack topic in the daemon-launched controller.
    //  - Followed again, so the registration that ends this pass carries it and the session's
    //    consumer relays what the stream kept for it.
    const registry = await handle.readInterest(sessionID);
    const slackRetired = publishedInterface?.roleClaim.slackRoleOwned === true;
    const registered = registry?.topics ?? [];
    const remembered = rememberedSubjects(activeSessionContext?.sessionManager.getEntries() ?? []);
    const own = new Set(remembered);
    const agentSubject = legacyAgentSubject(sessionID);
    const fate = (topic: string): "lane" | "role" | "retired" | "follow" =>
      topic === agentSubject || topic.startsWith(ROLE_TOPIC_PREFIX)
        ? "lane"
        : roleTopics.names(topic) && !own.has(topic)
          ? "role"
          : slackRetired && slackTopicChannel(topic) !== undefined
            ? "retired"
            : "follow";
    const fates = new Map(
      [...new Set([...registered, ...remembered])].map((topic) => [topic, fate(topic)] as const)
    );
    const retired = registered.filter((topic) => fates.get(topic) === "retired");
    // A step of this pass, so a pass the host left behind unregisters nothing: the listener keeps
    // the retired topics until the pass the move queued reads the registry itself. The failure is
    // this branch's to name: the step warns under the sentences it is handed, one for a bus that
    // did not answer and one for any other failure.
    if (retired.length > 0)
      await handle.unregisterTopics(sessionID, retired, {
        failed: "envoy: could not unregister a resumed controller's retired Slack topics",
        busUnanswered: busUnansweredRemoval("a resumed controller's retired Slack topics"),
      });
    // A topic followed as the session's own is its own even where a role's claims are already in
    // place (a host move with no hold, or a pass whose move decided nothing): `follow` adds
    // nothing for a subject a role already follows, so it is claimed as the session's here, or
    // the role's end would drop it and the ledger would not record it.
    for (const [topic, decided] of fates) {
      if (decided !== "follow") continue;
      await handle.subscribe(topic);
      for (const subject of expandSubscriptionTopics([topic])) roleTopicFollows.claimOwn(subject);
    }
    // The topics of the role this session wants are opened before the pass's first
    // registration, after the session's own: a follow stamp a registration does not carry is
    // dropped, and the transcript restored each topic's stamp from before the process ended, so
    // a topic opened only at the claim would answer done every post the stream kept for it while
    // the session was down. A topic the session remembers as its own stays its own too.
    //
    // The role is the one the pass will claim, read without another listener read: the
    // transcript's or the launch's (`desiredRole`), else the role lane this session's own row
    // holds, which the pass's read of that row would find. It opens only where the listener names
    // this session or nobody as the role's holder: where another session holds it, the stream's
    // backlog on its topics is that session's to read, and this session would be shown it before
    // its claim was refused. A holder read that fails opens them, so an alert is never lost to a
    // listener read. The pass drops them once a refusal names another holder
    // (`settleRoleTopics`), and keeps them through one naming this session's own id.
    const lane = registered.find((topic) => topic.startsWith(ROLE_TOPIC_PREFIX));
    const laneRole = lane?.slice(ROLE_TOPIC_PREFIX.length);
    const wanted =
      (await desiredRole(
        handle,
        sessionID,
        activeSessionContext?.sessionManager.getBranch?.() ?? [],
        false
      )) ?? (laneRole !== registrationState.refusalFor(sessionID) ? laneRole : undefined);
    if (wanted !== undefined) {
      const holder = await handle.readRoleHolder(wanted);
      handle.check();
      if (holder === undefined || holder === sessionID) {
        const dropped = roleTopicFollows.open(wanted);
        if (dropped.length > 0) await dropRoleSubjects(handle, sessionID, dropped);
      }
    }
    roleRowCheck = sessionID;
  };
  // The session whose row may still hold role topics a process that ended never closed, which
  // `settleRoleTopics` reads again after each pass until one read succeeds.
  let roleRowCheck = "";

  // Stops following each of `subjects` exactly and takes them out of `host`'s row, as a step of
  // the pass; answers whether that took a subject the session still follows with them
  // (`takesFollowedBase`), which the caller registers again.
  const dropRoleSubjects = async (
    handle: TaskHandle,
    host: string,
    subjects: readonly string[]
  ): Promise<boolean> => {
    for (const subject of subjects) followed.delete(subject);
    if (subjects.length === 0) return false;
    await handle.unregisterTopics(host, subjects, {
      failed: "envoy: could not unregister the role topics of a role this session does not hold",
      busUnanswered: busUnansweredRemoval("the role topics of a role this session does not hold"),
    });
    return takesFollowedBase(subjects);
  };

  // After a pass settles the role. The topics a resumed session opened for its wanted role are
  // closed once the listener refused that role naming another holder
  // (`closeRefusedRoleTopics`), and stay open through a refusal naming this session's own id, a
  // claim that threw, or a move that decided nothing. Once after a resume, the session's row
  // loses every role topic nothing follows, so its consumer stops relaying them and no later
  // resume reads them as the session's own. One registration then carries whatever changed.
  const settleRoleTopics = async (handle: TaskHandle, host: string): Promise<void> => {
    registrationState.closeRefusedRoleTopics(host);
    let baseLeft = false;
    if (roleRowCheck === host) {
      const row = await handle.readInterest(host);
      handle.check();
      if (row !== undefined) {
        roleRowCheck = "";
        const leftOver = row.topics.filter(
          (topic) => roleTopics.names(topic) && !followed.has(topic)
        );
        baseLeft = await dropRoleSubjects(handle, host, leftOver);
      }
    }
    if (baseLeft || roleTopicFollows.owed()) {
      await registerAgain(handle, host, "the subjects the session follows");
    }
  };

  // The newest role claim entry on `branch`, whatever process wrote it: the last one wins, since
  // a claim followed by a release is no claim.
  const transcriptRoleRecord = (branch: readonly unknown[]): RoleClaimEntry["data"] | undefined =>
    branch.findLast(isRoleClaimEntry)?.data;

  // What that record asks of this process: the role it claims, `null` for a release, and
  // `undefined` where this process acts on no record at all — which `mayActOnRoleRecord`, the
  // one rule both clients read, decides from the writer the record names. Two processes can
  // read one transcript, a continuation resuming a live session's id, which is how a copy's
  // release ended the live process's role for hours (dispatch://INFRA-783): a release another
  // instance wrote ends nothing here while this process holds a role, and a claim it wrote is
  // pressed only when there is no role to lose, where the listener decides the claim on the two
  // processes rather than this side guessing.
  const recordedRole = (branch: readonly unknown[]): string | null | undefined => {
    const record = transcriptRoleRecord(branch);
    if (record === undefined) return undefined;
    return mayActOnRoleRecord(
      record.client_instance_id,
      registrationState.clientInstanceID,
      registrationState.heldRole() !== undefined
    )
      ? record.role
      : undefined;
  };

  // The role the host session `id` should hold:
  //  - The role its own branch records, where that record is this process's to act on
  //    (`recordedRole`): the transcript entry written at claim time is the durable record (it
  //    survives `omp --resume` after the listener has reaped a dead session's rows), and a
  //    recorded release is final. A fork's branch is a copy of its parent's, so it records its
  //    parent's role; a `/new` records none; a resume or branch records its own.
  //  - Otherwise, when `id` is the session the launch named for it, the role a session pod's
  //    launch named (`sessionRole`), which the pass claims outright (`settleRole`), so the
  //    listener never refuses it. A move the model makes with `envoy_role_set` is a record, which
  //    no pass undoes.
  //  - Otherwise the role this process holds, when its hold is under `id` or a session `id`
  //    continues (a tree navigation to before the claim, or a fork from before it).
  //  - Otherwise, once when a pass establishes a resumed or switched session (`readRows`), the
  //    listener's rows for `id` (a same-id resume inside the reap window) and for the sessions it
  //    continues.
  // None of them while the listener has refused that role to `id`, and never the launch's role
  // for a session the launch did not name (`sessionRoleRefusal`): a fork's branch records it, and
  // a fork is another session. The pass claims every role but the launch's softly (`settleRole`):
  // a session's records may be stale evidence (the parent of a fork whose role moved to the
  // child, a second process on the same transcript), and it must not take a role another live
  // session holds. The listener decides that atomically: a soft claim naming a predecessor takes
  // the role from that predecessor only when the predecessor has no live registry entry, when its
  // live entry carries this client instance's own instance id, or when either side carries no
  // instance id. A predecessor still live under another client instance keeps the role, and the
  // claim is refused naming it.
  const desiredRole = async (
    handle: TaskHandle,
    id: string,
    branch: readonly unknown[],
    readRows: boolean
  ): Promise<string | undefined> => {
    const want = (role: string) =>
      registrationState.refusalFor(id) === role || sessionRoleRefusal(id, role) !== undefined
        ? undefined
        : role;
    const recorded = recordedRole(branch);
    if (recorded === null) return undefined;
    if (recorded !== undefined) return want(recorded);
    if (sessionRole !== undefined && sessionRoleRefusal(id, sessionRole) === undefined) {
      return sessionRole;
    }
    const held = registrationState.heldRole();
    const holdID = registrationState.holdSessionID();
    const lineage = continuedBy(id);
    if (held !== undefined && holdID !== undefined && lineage.includes(holdID)) return want(held);
    if (!readRows) return undefined;
    for (const source of lineage) {
      const registry = await handle.readInterest(source);
      const topic = registry?.topics.find((candidate) => candidate.startsWith(ROLE_TOPIC_PREFIX));
      if (topic !== undefined) return want(topic.slice(ROLE_TOPIC_PREFIX.length));
    }
    return undefined;
  };

  // Says, once per session, that the agent now lives under a new id when a fork or a branch
  // carried its conversation over from another session: any identity already baked into that
  // carried context — envoy_whoami output, ids the agent named in sent messages — silently rots,
  // and peers who saved the old id can no longer reach this session. Told without starting a
  // turn, by the pass that converges to the session, while the host is on it, naming the session
  // it continues. A session a `/new` or resume opened, or one the host came back to, has a
  // transcript that matches its id, so it is told nothing.
  const tellIdChanged = (current: string): void => {
    const previous = continues.get(current);
    if (previous === undefined || previous === current || notifiedSessions.has(current)) return;
    notifiedSessions.add(current);
    const content = encode({
      envoy: {
        notice: "session id changed",
        previous_session_id: previous,
        session_id: current,
        detail:
          "This conversation now lives under a new session id. Any identity you shared earlier (envoy_whoami output, session ids named in messages) is stale, and peers using the old id cannot reach you. Re-run envoy_whoami before identifying yourself and re-announce the new id to peers you already introduced yourself to.",
      },
    });
    // A notice to the model, through the tell gate.
    if (shuttingDown) return;
    pi.sendMessage(
      {
        customType: "envoy-message",
        content,
        display: true,
        details: LOCAL_ENVOY_NOTICE,
      },
      { deliverAs: "steer", triggerTurn: false }
    );
  };

  // Learns where the host went. An announcement made before the move (or, without one, the
  // switch's own event) names the reason: a session a fork or a branch opened continues the one
  // it left; one a `/new` or resume opened continues nothing. A move with no word to an id never
  // seen here (a fresh TUI's first id after another, a session file the host moved off) carries
  // the session on, as the heartbeat's drift heal always has; one back to a session seen before
  // (a rollback) teaches nothing, since that session's lineage is how it was first opened. A pass
  // that finds the host still on the session it announced leaving keeps the announcement for the
  // move; a session event there ends it (a `/reload`, which lands back on its own session).
  const noteMove = (host: string, reason?: { readonly kind: SessionSwitchReason | undefined }) => {
    if (host === "") return;
    if (announced !== undefined && host === announced.from) {
      if (reason !== undefined) announced = undefined;
      return;
    }
    const from = announced?.from ?? sessionID;
    const told = announced !== undefined || reason !== undefined;
    const kind = announced !== undefined ? announced.reason : reason?.kind;
    announced = undefined;
    if (from !== "" && from !== host) {
      if (told) {
        if (carriesSession(kind)) continues.set(host, from);
        else continues.delete(host);
      } else if (!knownSessions.has(host)) {
        continues.set(host, from);
      }
      knownSessions.add(from);
      leftSessions.add(from);
    }
    knownSessions.add(host);
  };

  // Every session id this process has been on: a move with no word back to one is a rollback.
  const knownSessions = new Set<string>();

  // The sessions this process has left and still holds a broker client for: a key pair and the
  // token last traded for it (`CLIENTS`, @legion/envoy-client/broker-credential). A session goes
  // in when the host leaves it (`noteMove`, and the pass that closes its agent subject) and out
  // once a pass has run its role move (`settleRole`), whether or not that move decided anything,
  // and no delivery holds it (`underWay`) — so a process that moves between sessions all day holds
  // one client rather than one per session it ever bound. A pass fenced before that point, or a
  // session a delivery still holds, leaves its entry here for the next pass; a call that starts
  // for a session already forgotten trades again under that session's name.
  const leftSessions = new Set<string>();

  interface PassCause {
    // A heartbeat tick, which also checks the listener still resolves a held role.
    readonly heartbeat?: boolean;
    readonly afterOutage?: boolean;
  }

  // Converges this process to the host's session as it is now: that session's agent subject open
  // and registered and Dispatch bound to it, every other agent subject this process opened
  // closed, and the role that session's records name held for it (`settleRole`). Every session
  // event, every heartbeat tick and every bridge call runs one, one at a time; a pass that finds
  // the host moved after any await stops, and the pass the move queues converges. Answers whether
  // it decided the role: a pass that decided nothing, because it was abandoned at its deadline or
  // the host left it mid-pass, answers false.
  const reconcile = (context: SessionContext, cause: PassCause = {}): Promise<boolean> =>
    registrationState.passes
      .run((handle) => reconcileNow(context, cause, handle))
      .then((decided) => decided ?? false);

  const reconcileNow = async (
    context: SessionContext,
    cause: PassCause,
    handle: TaskHandle
  ): Promise<boolean> => {
    // A pass that starts once the stop has begun does nothing (R24 of dispatch://INFRA-3589): its
    // first steps would deregister a session the host left, tell the agent of a new id, dial the
    // inbound and read the registry, and the stop deletes the record next. A pass the stop
    // overtakes ends at the read after its registration, below.
    if (shuttingDown) return false;
    const host = context.sessionManager.getSessionId();
    // Every write, listener call, dial and subscribe below is a step of this pass's handle, which
    // refuses it once the pass was abandoned at its deadline or the host moved out from under it:
    // a pass that no longer decides records nothing of what it opened, and the pass the move
    // queues converges from there.
    handle.actFor(host);
    noteMove(host);
    if (sessionID !== host) restoreLocalSessionState(context);
    if (registrationState.establishedFor() !== host) {
      // A move with no session event refreshed no shell environment: name the live id, and
      // refresh the snapshot the shell takes, as the host does after a session event.
      if ((process.env.DISPATCH_SESSION_ID ?? "") !== host) {
        bindDispatchSession(host, context.ui);
        procmgr.refreshShellConfigCache();
      }
      tellIdChanged(host);
      // A session the host reaches with no event is one no plugin heard, the session last heard
      // included once the host has been elsewhere (a rollback to it).
      if (host !== lastHeard) lastHeard = "";
    }
    for (const other of handle.forgetOtherAgentSessions(host)) {
      unfollow(legacyAgentSubject(other));
      leftSessions.add(other);
    }
    // Another session's relay: its unsettled frames go back to the listener (`nak`).
    if (relay !== undefined && relay.sessionID !== host) closeRelay();
    // Another session's agent-stream control subject, closed here: a viewer of that session is
    // answered nothing more, which a failed move to this one never reaches `watchAgentStream` to
    // do. Its ring is kept only while that viewer still watches; this session's ring holds what
    // the host recorded under it.
    if (agentStreamControlSession !== "" && agentStreamControlSession !== host) {
      agentStreamControl?.unsubscribe();
      agentStreamControl = undefined;
      pruneAgentStreams(host);
      agentStreamControlSession = "";
      agentStreamViewerAttached = false;
      agentStreamCommandsRefreshedOnWatchAt = -Infinity;
      forgetAgentStreamContext();
    }
    if (host === "") {
      handle.markEstablished(host);
      ensureHeartbeat(context);
      return true;
    }
    // The relay subject and the agent-stream control subject are what need NATS, and the
    // registration needs the relay: a session is delivered to through its own consumer alone, so
    // a registration asks for one and the listener refuses one it cannot give (INFRA-771). A
    // NATS outage therefore stops the registration, and with it the role claim that registers
    // first (a Legion claim through the bridge included); the pass reports it, and the next pass
    // opens the relay and registers again. A steady pass touches no NATS.
    let inboundFailure: unknown;
    // A session whose inbound this pass opens, and one a subject of whose inbound went after it
    // opened - its connection closed for good, or its subscription ended - while its own
    // resubscribe has not opened it again: the relay's waits NATS_RETRY_INTERVAL_MS after a dial
    // that failed, and the agent stream's gives up. Both take the one path below, so every
    // heartbeat restores what is missing, and a refused registration below names why it could not.
    const opening = inbound && registrationState.inboundOpenSession() !== host;
    if (
      opening ||
      (inbound &&
        ((relay?.sessionID === host && relay.subscription === undefined) ||
          (agentStreamControlSession === host && agentStreamControl === undefined)))
    ) {
      try {
        if (opening) {
          handle.markAgentSessionOpen(host);
          const connected = await handle.connect();
          // The connection was admitted as one session, and the host switched to another: it
          // follows the session the host runs now (`SessionInbound.follow`), bounded by the pass,
          // as the start's trade is (`followBoundMs`): a follow that outlasted a short pass
          // deadline would run on beside the pass after it.
          await connected.follow(followBoundMs(passDeadlineMs()));
          await handle.subscribe(legacyAgentSubject(host));
        }
        // The relay subject opens before any registration asks for consumer delivery, so the
        // listener's first relayed frame has a subscriber. A viewer's replay request and its watch
        // pings reach the session on the agent-stream control subject; opening it costs one core
        // subscription and publishes nothing, so a session nobody opens stays silent. Each opens
        // only what is not open already.
        await openRelay(context, host, handle.connect);
        await watchAgentStream(host, handle.connect);
        if (opening) {
          if ((context.sessionManager.getBranch?.() ?? []).length > 0) {
            await recoverRegisteredInterests(handle);
          }
          handle.markInboundOpen(host);
        }
      } catch (error) {
        inboundFailure = error;
      }
    }
    // A step this pass was refused ends it here: what it was refused is no NATS outage to
    // report, and the pass the move queued opens the subject.
    handle.check();
    // nats.js's own error can carry what the servers were set to (a resolver's error names the
    // host it looked up, a socket's the port, a password's digits included), so a notice carries
    // its code, the server's own text, or this plugin's own message (`natsFailureReason`).
    const failureText =
      inboundFailure === undefined ? undefined : natsFailureReason(inboundFailure);
    passFailure = failureText === undefined ? undefined : { sessionID: host, text: failureText };
    // Every session subscribes to its own agent subject, so every session is registered;
    // envoy_send treats an unregistered id as dead. The listener rejects a claim from a session it
    // does not currently know, so this comes before the role. A registration refused for want of
    // the relay names why the relay did not open, in the refusal and in the outage notice: it is
    // the cause a person can act on (a hub with no auth service under NATS_AUTH=broker says so and
    // what to unset), which the refusal alone hides.
    const registration = await trackRegistration(
      host,
      sendRegistration(handle.register, failureText)
    ).then(
      () => undefined,
      (error: unknown) => ({ error })
    );
    // The catch-all above turned whatever the registration threw into `registration.error`, a
    // refused step included: this says so rather than report a registration failure.
    handle.check();
    // Once the stop has begun, the pass ends here, whether the listener accepted the
    // registration or not (R24 of dispatch://INFRA-3589). The stop hands the role back and
    // deletes the record next, and nothing asks again. So the pass makes no role move, fires no
    // regain hook and says nothing, as a pass whose session moved does. A registration the stop
    // refused (`sendRegistration`) ends the pass here too.
    if (shuttingDown) return false;
    // Established once the registration landed, which is what a bridge call asks before it runs
    // a pass of its own (`establishForBridge`): a pass this call queues reads the host as it
    // stands when its turn comes, so one queued for an established session would learn a session
    // the host has moved to meanwhile as a continuation of the one it was made for (`noteMove`),
    // and the role of that session would be claimed for it.
    if (registration === undefined) handle.markEstablished(host);
    // Armed whether or not this registration was accepted: a registration the listener refuses,
    // for any of the reasons `ErrorWireSchema.reason` lists, leaves the session with no consumer
    // and so no inbound at all, and the heartbeat is the path that asks again, which is what the
    // session was told (`inboundOutage`).
    ensureHeartbeat(context);

    // The nested move has its own queue and its own deadline: it is fenced by `roleMoves`, not
    // by this pass, and one that decided nothing leaves the role to the next pass. A stop that
    // begins while the move waits its turn or runs refuses its next step: the move claims
    // nothing, fires no regain hook, says nothing and decides nothing.
    const decided =
      (await registrationState.roleMoves.run((move) =>
        settleRole(context, host, cause, registration === undefined, move)
      )) ?? false;
    handle.check();
    // A move that decided nothing leaves the role, and so its topics, to the next pass.
    if (decided) await settleRoleTopics(handle, host);
    // The role move above was the last call for every session this process has left, so each
    // one's key pair and token go now (`leftSessions`), whether or not the move decided, but for
    // a session a delivery still holds (`underWay`), which stays for the next pass. The session
    // running now is never one of them: a rollback to a session this pass left behind keeps
    // trading under it.
    leftSessions.delete(host);
    for (const other of leftSessions) {
      if (heldSessions.has(other)) continue;
      forgetBrokerSession(other);
      leftSessions.delete(other);
    }
    if (registration !== undefined) {
      // A registration the listener refused while the open failed too names that failure here;
      // the client's own refusal of a session with no relay already names it.
      const refused = messageFor(registration.error);
      throw new RegistrationError(
        failureText === undefined || registration.error instanceof NoRelaySubjectError
          ? refused
          : `${refused}, because its relay subject could not be opened: ${failureText}`,
        { cause: registration.error }
      );
    }
    if (failureText !== undefined) {
      // No cause: nats.js's own error still carries what the text leaves out.
      throw new InboundError(failureText, decided);
    }
    return decided;
  };

  // The session the latest session event moved the host to: a pass that decides for a session no
  // event announced (a move with no event, a rollback) is the only one a plugin did not hear, so
  // only its role ends carry a warning.
  let lastHeard = "";

  // The predecessor a soft claim named when the claim threw, for the host session and role it
  // claimed: a `503` the listener answers when the claim stood but its handover record did not
  // store, or a failure that left the claim's outcome unknown. The next claim of that role for
  // that session names it again. Once the claim stands, the listener names this session the role's
  // holder, so a predecessor read off that holder would name none, and the listener would never
  // write the handover record that hands this session the predecessor's unread messages.
  let unsettledPredecessor:
    | { readonly host: string; readonly role: string; readonly previous: string }
    | undefined;
  // A hold that a role move landed followed topics the deployment names for its role
  // (`ENVOY_ROLE_TOPICS`) that no registration has carried yet: register now, inside the move,
  // so the session's consumer is filtered on them from the claim on rather than from the next
  // heartbeat, which can be minutes away. The pass that claimed registered before its claim,
  // since the listener takes no claim from a session it does not know. A registration that fails
  // here leaves them followed, and every later registration carries them.
  const registerRoleTopics = async (handle: TaskHandle, host: string): Promise<void> => {
    if (roleTopicFollows.owed()) await registerAgain(handle, host, "the held role's topics");
  };
  // Registers `host` again as a step of the pass. A registration that fails is warned, naming
  // `what` it was meant to carry, and left to the next heartbeat, which registers every subject
  // the session follows.
  const registerAgain = async (handle: TaskHandle, host: string, what: string): Promise<void> => {
    try {
      await trackRegistration(host, sendRegistration(handle.register));
    } catch (error) {
      handle.check();
      logger.warn(`envoy: ${what} were not registered; the next heartbeat does it`, {
        role: registrationState.heldRole(),
        sessionID: host,
        error: messageFor(error),
      });
    }
  };

  // The pass's role move: converge the role this process holds to the one the host session
  // should hold, for that session. It runs in its turn of the role moves, so a move the model or
  // a plugin made meanwhile is in the transcript by then, and it reads nothing until that turn. A
  // pass whose registration failed hands back what the session does not want and claims nothing,
  // since the listener takes no claim from a session it does not know.
  const settleRole = async (
    context: SessionContext,
    host: string,
    cause: PassCause,
    registered: boolean,
    handle: TaskHandle
  ): Promise<boolean> => {
    // Every write and listener call below is a step of this move's handle, which refuses it once
    // the move was abandoned at its deadline or the host moved: the move then decides nothing,
    // and the move the next pass queues converges from the state this one left. A step is never
    // preceded by a check here, because it checks itself.
    handle.actFor(host);
    const branch = context.sessionManager.getBranch?.() ?? [];
    // The rows are read until a pass has decided for this session, except for a brand-new first
    // session, which has none.
    const settled = registrationState.roleSettledSession();
    const readRows = settled !== host && (branch.length > 0 || settled !== "");
    // A refusal this session carries stands until a session event lands on it. The one refusal
    // that is never recorded is a claim the listener refused because this very session id holds
    // the role under another process, which it grants as soon as that process stops renewing
    // it: nothing latches it, so the pass below claims again.
    const desired = await desiredRole(handle, host, branch, readRows);
    const unheard = lastHeard !== host;
    const held = registrationState.heldRole();
    if (held !== undefined && held !== desired) {
      // A role end after a move no session event announced is one no plugin heard: the hand-back
      // says so in the pane, and who claims it again.
      await handle.handBackHeldRole(unheard);
    }
    if ((await handle.settleEndedHolds(desired)) && unheard && desired !== undefined) {
      handle.noteNoticesClosed(desired);
    }
    if (!registered) return false;
    if (desired === undefined) {
      handle.markRoleSettled(host);
      return true;
    }
    // The launch's role is claimed at every pass that registered the session the launch named,
    // below, rather than read as held between two registrations: the launch named this session
    // its holder.
    const launched = desired === sessionRole && host === sessionRoleSession;
    if (
      !launched &&
      !registrationState.holdIsUnconfirmed() &&
      registrationState.heldRole() === desired &&
      registrationState.holdSessionID() === host
    ) {
      handle.markRoleSettled(host);
      if (cause.heartbeat !== true) return true;
      // A heartbeat pass makes sure the listener still resolves this session as the role's live
      // holder, and fires Legion's regain hook for a role it had to take back.
      const checked = await handle.reassertRole(desired, cause.afterOutage === true);
      const { regain } = checked;
      // A stop that began since the re-assertion's last step fires no regain hook, and the move
      // decides nothing.
      if (shuttingDown) return false;
      const regained = regain === undefined ? undefined : legionRoleClaimBridge().regained;
      if (regain !== undefined && regained !== undefined) {
        // Detached from the heartbeat chain: the hook is a daemon round-trip this side cannot
        // bound (`claims/ready`, retried on an outage or a transport failure), and the chain's
        // `healing` latch must release as soon as registration and the claim are settled, or the
        // next tick could never register. `Promise.resolve().then` also catches a synchronous
        // throw.
        void Promise.resolve()
          .then(() => regained(desired, regain))
          .catch((error: unknown) => {
            logger.warn("envoy: role regain hook failed", {
              role: desired,
              reason: regain,
              error: messageFor(error),
            });
          });
      }
      return checked.decided;
    }
    // A role a session holds by its records, with inbound messages off, is never claimed: every
    // task and notice reaches its holder over NATS.
    if (!inbound) {
      handle.markRoleSettled(host);
      return true;
    }
    // The launch's role is claimed outright, last-claim-wins, as the pod's own claimer did: a
    // holder that is another session is stale (an evicted pod's session the registry still lists
    // as live for minutes) or wrong, and a soft claim would leave the role with it. Every other
    // claim is soft and may supersede this process's own hold of the role, wherever the listener
    // names it, or else the predecessor a claim of this role for this session named when it threw
    // (`unsettledPredecessor`), or else whichever session the host session continues the listener
    // names (a fork's hold from this process or an earlier one).
    const holdID = registrationState.holdSessionID();
    let supersedes =
      registrationState.heldRole() === desired && holdID !== undefined && holdID !== ""
        ? holdID
        : undefined;
    if (
      !launched &&
      supersedes === undefined &&
      unsettledPredecessor?.host === host &&
      unsettledPredecessor.role === desired
    ) {
      supersedes = unsettledPredecessor.previous;
    }
    const lineage = continuedBy(host);
    if (!launched && supersedes === undefined && lineage.length > 1) {
      const holder = await handle.readRoleHolder(desired);
      if (holder !== undefined && holder !== host && lineage.includes(holder)) supersedes = holder;
    }
    try {
      const claimed = await handle.claimRole(
        desired,
        launched ? {} : { soft: true, previousSessionID: supersedes }
      );
      if (unsettledPredecessor?.host === host && unsettledPredecessor.role === desired) {
        unsettledPredecessor = undefined;
      }
      if (claimed.claimed && unheard) handle.noteNoticesClosed(desired);
    } catch (error) {
      if (!launched && supersedes !== undefined) {
        unsettledPredecessor = { host, role: desired, previous: supersedes };
      }
      // This catch would otherwise swallow a step the handle refused: the check says so instead.
      // A move that was refused decided nothing, and the move or pass that replaced it does.
      // That is not a failure to report.
      handle.check();
      logger.warn("envoy: role reclaim failed", {
        role: desired,
        sessionID: host,
        error: messageFor(error),
      });
    }
    await registerRoleTopics(handle, host);
    handle.markRoleSettled(host);
    return true;
  };

  // A bridge call for a session no pass has established yet runs one first. NATS being down does
  // not stop the call: a claim goes over HTTP, as the pass's registration did, and a subscription
  // fails on its own connect.
  const establishForBridge = (context: SessionContext): Promise<void> =>
    reconcile(context).then(
      () => undefined,
      (error: unknown) => {
        if (!(error instanceof InboundError)) throw error;
      }
    );

  // The bridge's state is read through `legionRoleClaimBridge()` at each use; only the instance
  // list is this instance's own entry on the object it published into. `replaces`, the session
  // the Legion registration says this one replaces, goes out as the claim's `previousSessionID`,
  // which the listener reads as the session whose unread direct and role messages it hands to
  // this one. The claim stays a hard one either way: a Legion claim is last-claim-wins.
  const claim: LegionRoleClaim = async (targetSessionID, role, callerContext, replaces) => {
    // A stopping session claims no role, and is not marked as Legion's either.
    refuseBridgeWhileStopping(targetSessionID, role, "role claim");
    const context = callerContext ?? activeSessionContext;
    if (context === undefined || context.sessionManager.getSessionId() !== targetSessionID) {
      throw new Error(`Envoy has no active session for Legion role claim: ${targetSessionID}`);
    }
    // Legion drives this session from here on, and the run-end ask nudge stays out of it.
    // Recorded before the claim runs: a claim that fails midway leaves the session just as
    // Legion-driven as one that succeeds.
    markLegionManagedSession(targetSessionID);
    // The role's every task and notice reaches its holder over NATS: with inbound messages off,
    // a held role would take them all and deliver none, so the claim refuses before any listener
    // call registers the session or sets the role.
    requireInbound();
    requireSessionRoleFor(targetSessionID, role);
    if (registrationState.establishedFor() !== targetSessionID) {
      await establishForBridge(context);
    }
    // The listener rejects a claim from a session it does not currently know
    // (its registration may have expired), so register before claiming. A stop that began before
    // it is refused here in the claim's own words; one that begins while it is on the wire
    // refuses the move's first step, and the claim is refused below in the same words.
    refuseClaimWhileStopping(role);
    await registerSession();
    let claimAdopted = false;
    const claimed = await registrationState.roleMoves.run(
      async (handle) => {
        handle.actFor(targetSessionID);
        await handle.claimRole(role, replaces ? { previousSessionID: replaces } : undefined);
        await registerRoleTopics(handle, targetSessionID);
        return true;
      },
      (refusal) => {
        claimAdopted = refusal.claimAdopted;
      }
    );
    // The move decided nothing, because the session's stop began, or because the session it was
    // decided for stopped being the open one: the plugin that asked hears that, as it hears a
    // claim that failed, and hears whether the listener granted the claim all the same.
    if (claimed === undefined) {
      refuseClaimWhileStopping(role);
      throw sessionLeftDuringClaim(targetSessionID, role, claimAdopted);
    }
  };

  const subscribeNotice: LegionNoticeSubscription = async (
    targetSessionID,
    topic,
    callerContext,
    whileHolding
  ) => {
    // A subscription that lasts while this session holds a role is part of that role's claim:
    // once this session's stop has begun it is refused in the claim's words
    // (`RoleClaimStoppingError`), at its start as the claim is, and after each wait and when a
    // step fails, so the Legion controller reads it as its session's stop. The stop hands the
    // role back next.
    if (whileHolding !== undefined) {
      refuseBridgeWhileStopping(targetSessionID, whileHolding, "notice subscription");
    }
    const refuseWhileStopping = (): void => {
      if (whileHolding !== undefined) refuseClaimWhileStopping(whileHolding);
    };
    const context = callerContext ?? activeSessionContext;
    if (context === undefined || context.sessionManager.getSessionId() !== targetSessionID) {
      throw new Error(
        `Envoy has no active session for Legion notice subscription: ${targetSessionID}`
      );
    }
    // A notice lane reaches the session only over NATS, so a subscription with inbound off
    // would be one nothing is ever delivered on: it refuses before any listener call, as the
    // role claim does.
    requireInbound();
    if (registrationState.establishedFor() !== targetSessionID) {
      try {
        await establishForBridge(context);
      } catch (error) {
        refuseWhileStopping();
        throw error;
      }
    }
    let marked: readonly string[] = [];
    if (whileHolding !== undefined) {
      // A role move still in flight could end the role just after the subscription opens, so the
      // role is read once every move started before this call has landed.
      await registrationState.roleMoves.settled();
      refuseWhileStopping();
      if (!registrationState.holdsRoleTopic(ROLE_TOPIC_PREFIX + whileHolding)) {
        throw new Error(
          `Envoy session ${targetSessionID} does not hold role ${whileHolding} for a notice subscription`
        );
      }
      // Marked before the subscription opens, so the registration it leads to already carries
      // them as the role's, which the role's end closes and unregisters together, and a move the
      // model starts from here on sees them.
      marked = registrationState.markRoleBoundSubjects(
        expandSubscriptionTopics([topic]),
        whileHolding
      );
    }
    // Taken once the subjects are marked: nothing awaits between the two.
    const snapshot = registrationState.roleSnapshot(topic);
    try {
      if (!followUnlessRoleEnds(topic, snapshot)) return;
      // A subscription that lasts beyond any role is the session's own, even on a subject the
      // deployment's role topics already follow: claimed here, the role's end leaves it.
      if (whileHolding === undefined) {
        for (const subject of expandSubscriptionTopics([topic])) roleTopicFollows.claimOwn(subject);
      }
      // The registration is what tells the listener this session follows the topic, so a
      // subscription is open only once it lands.
      await registerSession();
    } catch (error) {
      // Nothing opened, so nothing may keep the session's role in place on its behalf. A role that
      // ended meanwhile already dropped these marks, and a newer subscription may hold them now.
      if (registrationState.roleEndCount() === snapshot.endsBefore) {
        unfollow(topic);
        registrationState.unmarkRoleBoundSubjects(marked);
      }
      refuseWhileStopping();
      throw error;
    }
  };

  const claimInstance: LegionRoleClaimInstance = {
    claim,
    subscribe: subscribeNotice,
    // The host's id, already moved by the time any session event is dispatched; the module
    // `sessionID` follows only once this instance's own rebind has run.
    sessionID: hostSessionID,
  };
  if (publishedInterface !== undefined) publishedInterface.roleClaim.instances.push(claimInstance);

  // A `task` subagent loads its own instance of this module in the parent's process and fires
  // its own session_start. It shares the parent's Envoy identity: registering it would list an
  // untitled session per subagent, heartbeated for as long as the parent process lives. The
  // host's own roster decides (any storage, any transcript or none), and the transcript layout
  // only where the roster gives no opinion.
  const isSubagent = subagentSessionCheck();

  /**
   * The Envoy address a reply to this instance reaches, and the source session its sends carry.
   *
   * An instance that took a session of its own answers with it — `liveSessionID` first, which
   * the slash command passes because a session created lazily after `session_start` has moved
   * past the module copy. A `task` subagent took none: it registers nothing, so its own host
   * session id is an address no message can be delivered to, and the reachable session is the
   * top-level one that spawned it, resolved from its transcript path
   * (`resolveEnvoySession`). That resolves to `""` when this process cannot say which
   * top-level session that is, and an empty address is what the caller reports and sends —
   * naming an unrelated live session would send peers to a session that never spawned it. A hook
   * that already asked the subagent check passes its answer as `subagent`, so it asks once.
   */
  const replyAddress = async (
    context: SessionIdentityContext,
    liveSessionID = "",
    subagent?: boolean
  ): Promise<string> => {
    if (sessionID !== "") return liveSessionID || sessionID;
    if (!(subagent ?? (await isSubagent(context)))) return liveSessionID;
    return resolveEnvoySession(context.sessionManager.getSessionFile());
  };

  pi.on("session_start", async (_event, context) => {
    if (await isSubagent(context)) return;
    // The host's exit stops this session as its `session_shutdown` does (`stopSession`): in RPC
    // mode a SIGTERM emits no `session_shutdown`. Once per instance, and never for a subagent's,
    // which registers nothing. `exitOnly` keeps a cleanup pass the process lives on from it.
    cancelStopAtExit ??= postmortem.register("pi-envoy-session-stop", () => stopSession(context), {
      exitOnly: true,
    });
    bindDispatchSession(context.sessionManager.getSessionId());
    restoreLocalSessionState(context);
    lastHeard = context.sessionManager.getSessionId();
    if (startNotice !== "") notify(context, `envoy: ${startNotice}`, "warning");
    if (roleTopics.error !== undefined) notify(context, `envoy: ${roleTopics.error}`, "warning");
    await askgateLoad;
    for (const notice of askgateNotices) notify(context, `envoy: ${notice}`, "warning");
    if (!inbound) return;
    let established = false;
    const establish = async (): Promise<void> => {
      await reconcile(context);
      established = true;
    };
    try {
      await establish();
    } catch (error) {
      // A pass that fails once the stop has begun is no outage: the stop deletes the record next,
      // so the pane is told nothing and no retry is armed (R24 of dispatch://INFRA-3589).
      if (shuttingDown) return;
      // Whatever the pass failed at: the NATS connection it dials, the relay subject it opens
      // for the session's consumer, or the registration the listener refused or may not have
      // recorded. The session runs either way. Until a pass gets through, a message reaches it
      // only from a consumer a registration already made whose relay subject is still open: the
      // consumer of an accepted registration, or the one the listener made before it answered a
      // bus-outage 503 (`isBusUnanswered`).
      notify(
        context,
        `envoy: this session is not registered (${messageFor(error)}); retrying in the background`,
        "warning"
      );
      let attempting = false;
      context.setInterval(() => {
        // A tick once the stop has begun starts no pass: the stop deletes the record next.
        if (established || attempting || shuttingDown) return;
        attempting = true;
        void establish()
          // A pass that ends once the stop began says nothing (the tell gate): it registered
          // nothing, or the stop deletes the record it wrote.
          .then(() => notify(context, "envoy: this session is registered", "warning"))
          // Quiet on purpose: the initial warning disclosed the outage, and a
          // fresh warning every retry tick would spam the session.
          .catch(() => undefined)
          .finally(() => {
            attempting = false;
          });
      }, NATS_RETRY_INTERVAL_MS);
    }
  });

  // Whether a switch carries the session on (a fork, a branch) rather than replace it (`/new`, a
  // resume): what the session it opens continues, and so what the frame drop delivers.
  const carriesSession = (reason: SessionSwitchReason | undefined): boolean =>
    reason !== "new" && reason !== "resume";

  // A session event: learn what the session the host is on continues, take its id at once (its
  // tools and messages name it from here on), end at once a role its records rule out, then
  // converge in a pass, which may claim again a role the listener refused that session before.
  // `subagent` is whether the context is a subagent's, when the caller already asked
  // (`rebindDispatch`): the check then runs once per event.
  const rebind = async (
    reason: SessionSwitchReason | undefined,
    context: SessionContext,
    subagent?: boolean
  ): Promise<void> => {
    // The cards a check was armed for belong to the session the host left.
    cardWake.cancel();
    // A session event once the stop has begun moves nothing (R24 of dispatch://INFRA-3589): it
    // would deregister the session the host left, end a role and run a pass, and the stop hands
    // the roles back and deletes the record next. The stop is read before the subagent check too,
    // which can publish the transcript and log a failed publish outside the tell gate, and again
    // after its wait.
    if (shuttingDown || (subagent ?? (await isSubagent(context))) || shuttingDown) return;
    const host = context.sessionManager.getSessionId();
    noteMove(host, { kind: reason });
    lastHeard = host;
    registrationState.clearRefusal(host);
    if (sessionID !== host) restoreLocalSessionState(context);
    // Ends at once a role the host session's own records rule out, so the new session never
    // takes the old role's notices while an earlier pass finishes; the pass hands the hold back.
    // Only a record this process acts on ends anything (`recordedRole`): a release another
    // process wrote on this transcript ends no role this one holds. The launch's role ends here
    // too, as a recorded release would end it, when the launch did not name this session: a fork
    // or a branch carries the named session's record of it, and would otherwise follow its notice
    // subjects until its first pass, which waits on its registration.
    const held = registrationState.heldRole();
    const launchRoleRefused = held !== undefined && sessionRoleRefusal(host, held) !== undefined;
    registrationState.endUnwantedRole(
      host,
      launchRoleRefused ? null : recordedRole(context.sessionManager.getBranch?.() ?? []),
      continuedBy(host)
    );
    // With inbound messages off the session opens no agent subject; a heartbeat's pass, when a
    // claimed role runs one, registers it and hands back what the event ended.
    if (!inbound) return;
    try {
      await reconcile(context);
    } catch (error) {
      // A switch during a network outage must degrade, not fail the handler; the heartbeat's next
      // pass or the NATS client's own reconnect converges.
      notify(
        context,
        `envoy: rebind failed (${messageFor(error)}); will recover on reconnect`,
        "warning"
      );
    }
  };

  // Only a switch reports why the session changed; a branch or a tree
  // navigation carries no reason at all. A switch and a branch change the id the `dispatch`
  // command names; Oh My Pi refreshes the shell's environment after those two, and never after
  // a tree navigation, which keeps the session's id.
  const rebindDispatch = async (
    reason: SessionSwitchReason | undefined,
    context: SessionContext
  ): Promise<void> => {
    // The stop is read before the subagent check, as `rebind` reads it, and again after its wait.
    // The answer goes on to `rebind`, which asks nothing again.
    const subagent = shuttingDown ? undefined : await isSubagent(context);
    if (subagent === false && !shuttingDown) {
      bindDispatchSession(context.sessionManager.getSessionId(), context.ui);
    }
    await rebind(reason, context, subagent);
  };
  pi.on("session_switch", (event, context) => {
    cardWake.noteIfIdle(context);
    return rebindDispatch(event.reason, context);
  });
  pi.on("session_branch", (_event, context) => rebindDispatch(undefined, context));
  pi.on("session_tree", (_event, context) => rebind(undefined, context));
  // Oh My Pi emits these before it moves its session manager: the session the host leaves and
  // why, which the next pass reads once the host has moved (`noteMove`). A subagent's instance and
  // a session the host has yet to give an id have nothing to leave. A switch another extension
  // cancels changes nothing: the host's id never moves, so the announcement teaches nothing.
  const announce = (reason: SessionSwitchReason | undefined, context: SessionContext): void => {
    cardWake.cancel();
    const from = context.sessionManager.getSessionId();
    // An earlier announcement whose move came with no event and no pass since is learned from now,
    // before this one replaces it.
    noteMove(from);
    if (from === "" || sessionID === "") return;
    announced = { from, reason };
  };
  pi.on("session_before_switch", async (event, context) => announce(event.reason, context));
  pi.on("session_before_branch", async (_event, context) => announce(undefined, context));

  // Gives back at the listener each role claim this process holds, before the stop deletes the
  // session's record, so the session's next process claims the role at once: the listener
  // grants a claim under this session id to another process only once this one's claim is no
  // longer renewed inside the live window. The release names this process
  // (`handBackAllHolds`), so it frees no claim another process made. It gets its share of the
  // stop (`SHUTDOWN_RELEASE_MS`); one that has not answered by then is left, and the stop goes
  // on to the DELETE. The claim then waits out the live window. Its lines are the stop's own, so
  // they go to the host's logger with no tell gate (`scrubbedHostLogger`), as the hand-back's do.
  const releaseRoleClaims = async (): Promise<void> => {
    if (active === undefined) return;
    try {
      await within(
        registrationState.handBackAllHolds(scrubbedHostLogger),
        SHUTDOWN_RELEASE_MS,
        "the release of this session's role claims"
      );
    } catch (error) {
      scrubbedHostLogger.warn(
        "envoy: the listener did not answer the release of the role claims in time",
        { sessionID, error: messageFor(error) }
      );
    }
  };

  // The stop of this instance's session, run once (`stopSession`) whichever end of the host
  // reaches it first. One end is the host's `session_shutdown`. The other is the host's own exit
  // callbacks (`postmortem` of `@oh-my-pi/pi-utils`, the host's module), which Oh My Pi runs and
  // waits for on a SIGTERM, a SIGINT, a SIGHUP, a quit and a fatal error before the process
  // exits: `session_start` adds this stop to them (`cancelStopAtExit`). In RPC mode, which a
  // Legion pane runs, the exit disposes no session, so a SIGTERM emits no `session_shutdown`,
  // and this callback is the stop. The interactive mode disposes the session from an exit
  // callback of its own, so there both ends come together, and the second waits for the stop
  // the first began. A `/new`, a fork and a branch are no stop: they move this instance to
  // another session and reach neither end, so this stop and its release do not run for them.
  // The session the host left is deregistered on its own (`deregisterSession`, from
  // `restoreLocalSessionState`). A `task` subagent's instance runs this stop at its own
  // `session_shutdown`, but it holds no role and registered nothing, so it sends the listener
  // nothing: its hand-back finds no hold (`handBackAllHolds`), and its `sessionID` is empty, so
  // `deregisterSession` returns at once.
  let stopping: Promise<void> | undefined;
  let cancelStopAtExit: (() => void) | undefined;
  const stopSession = (context: SessionContext): Promise<void> => {
    stopping ??= shutDownSession(context);
    return stopping;
  };

  const shutDownSession = async (context: SessionContext): Promise<void> => {
    cancelStopAtExit?.();
    cancelStopAtExit = undefined;
    shuttingDown = true;
    // A reading still away answers nobody from here on: the DELETE below is what the session
    // leaves behind, and a place written after it would be a registration the heartbeat has
    // stopped refreshing (dispatch://INFRA-392).
    tmuxPlaces.shutDown();
    cardWake.cancel();
    // A person's `!command` runs in a process group of its own, which outlives this process
    // unless something kills it: the session that was asked for it is going, so it goes too,
    // with what it printed never answered. Every one of them: this process serves one session
    // at a time, and a command of the session the host left runs on until it is killed here. A
    // `!` that arrives from here on is refused saying so (`runShellCommandTurn`).
    for (const running of runningShellCommands.values()) running.stop();
    // The instance stays on the role-claim bridge, stopped, after this stop too: a claim or a
    // role-bound notice subscription the bridge routes to this session from here on is refused
    // naming the stop (`refuseBridgeWhileStopping`). The bridge never answers that it has no
    // instance for this session, and never hands its claim to another instance. The instance moves
    // to the front of the bridge's list, so the bridge's fallback for a session no instance serves
    // (`instances.at(-1)`) reaches a live instance where one is bound.
    if (publishedInterface !== undefined) {
      const { instances } = publishedInterface.roleClaim;
      const bound = instances.indexOf(claimInstance);
      if (bound !== -1) instances.unshift(...instances.splice(bound, 1));
    }
    // This session is about to deregister with the listener, so it stops being an address a
    // reply can reach: a subagent still running in this process must report none rather than
    // name it, and a dead entry must not be what makes some other session ambiguous.
    recordedSessionKey = recordEnvoySession({
      previousKey: recordedSessionKey,
      sessionFile: undefined,
      sessionID: "",
    });
    // The relay's unsettled frames go back to the listener before the connection drains.
    closeRelay();
    // The process ends, and with it every record it keeps live; removing them, and its store, is
    // tidiness (a dead process's record is inert). Only the instance that wrote records has any.
    if (askgateOwner !== undefined && askgateRecords.size > 0) {
      for (const dir of askgateRecords) {
        try {
          dropAskgateRecord(dir, askgateOwner);
        } catch {
          // A record left behind names a process that is gone.
        }
      }
      askgateRecords.clear();
      clearInterval(askgateBeat);
      askgateBeat = undefined;
      closeStore(askgateOwner.store);
    }
    // The token keeper stops before the drain: a renewal it ran on a draining connection would be
    // refused and warned about as a failure, at a stop nobody asked to be warned of.
    stopInboundKeeper();
    const deadline = Promise.withResolvers<void>();
    const endsAt = Date.now() + SHUTDOWN_DEADLINE_MS;
    const timer = setTimeout(deadline.resolve, SHUTDOWN_DEADLINE_MS);
    try {
      // The role claims go back first (`releaseRoleClaims`), and the record only after them: the
      // DELETE goes once the release answered or its share of the deadline ran out. The
      // heartbeat has stopped and every registration is refused (`shuttingDown`,
      // `sendRegistration`), so no new registration starts; one already on the wire is waited
      // out before the DELETE, or it would land behind it and leave the session registered with
      // nobody heartbeating it. That wait takes `SHUTDOWN_SETTLE_MS` here rather
      // than the whole of `REGISTRATION_SETTLE_MS`, which is longer than the deadline
      // below: the DELETE has to be on the wire before the host abandons this stop, since a
      // record left behind outlives the process by the gone horizon. The DELETE gets what is left
      // of the deadline once the release is done (`endsAt`), so a broker credential still waiting
      // on a trade leaves it time to go without one. The drain starts beside the deregistration:
      // it needs no listener call, and all of them share that deadline, which is what the host's
      // own limit on a shutdown handler leaves them.
      const open = connection;
      const stopped = releaseRoleClaims().then(() =>
        Promise.allSettled([
          deregisterSession(sessionID, SHUTDOWN_SETTLE_MS, endsAt).catch(() => undefined),
          open?.drain().catch(() => undefined),
        ])
      );
      await Promise.race([stopped.then(() => undefined), deadline.promise]);
    } finally {
      clearTimeout(timer);
      connection = undefined;
      activeSessionContext = undefined;
      followed.clear();
      unanswered.clear();
      agentStreamControl = undefined;
      agentStreamControlSession = "";
      // This process serves the session no longer, so what it was shown goes with it: the id this
      // instance registered, and the host's live one, which a task subagent's tool calls carried
      // (its instance never ran restoreLocalSessionState, so its `sessionID` stayed empty).
      const liveSessionID = context.sessionManager.getSessionId();
      forgetAgentStreamContext();
      agentStreams.clear();
      restoredAgentStreams.clear();
      forgetShownPictures(sessionID);
      if (liveSessionID !== sessionID) forgetShownPictures(liveSessionID);
      // An instance that served no session (a `task` subagent's, whose `sessionID` stayed empty)
      // leaves the role-claim bridge now: the bridge routes nothing to it by id, and kept there
      // it would hold this instance's state for the life of the process. An instance that served
      // a session stays on it, stopped (above).
      if (publishedInterface !== undefined && hostSessionID() === "") {
        const bound = publishedInterface.roleClaim.instances.indexOf(claimInstance);
        if (bound !== -1) publishedInterface.roleClaim.instances.splice(bound, 1);
      }
    }
  };

  pi.on("session_shutdown", async (_event, context) => stopSession(context));

  // An Envoy that is off registers no tool and no `/whoami`: there is no listener to reach. Every
  // tool validates its own arguments inside execute (one refusal naming every problem); the host's
  // schema check is bypassed so it cannot pre-empt that with its own wording.
  if (envoy.enabled) {
    for (const spec of envoyToolSpecs) {
      pi.registerTool({
        name: spec.name,
        label: spec.name,
        description: spec.description,
        parameters: schemaFor(pi, spec.operation),
        lenientArgValidation: true,
        execute: async (_id, parameters, _signal, _onUpdate, context) =>
          execute(spec.operation, parameters, context),
      });
    }
    registerEnvoyWhoamiCommand(pi, replyAddress);
  }

  // What people wrote to the session in the last 72 hours, as the list of
  // `@legion/envoy-client/session-answers` renders it: the session `replyAddress` names, which is
  // the top-level session's own and for a `task` subagent its parent's, the session it writes to
  // Dispatch as (`DISPATCH_SESSION_ID`). Dispatch is resolved with `commands: "kept"`, so no `!`
  // token command runs on the request path, and the read waits at most
  // `SESSION_ANSWERS_DEADLINE_MS` before the last list read stands in. A subagent this process
  // cannot place carries no list rather than another session's. A Dispatch with no answers route
  // carries nothing to the model; the read's `notice`, which it gives once per session and
  // Dispatch URL, goes to the log instead.
  const sessionAnswersText = async (
    context: SessionContext
  ): Promise<CarriedAnswers | undefined> => {
    const session = await replyAddress(context, context.sessionManager.getSessionId());
    if (session === "") return undefined;
    try {
      const read = await readSessionAnswers({
        session,
        dispatch: (
          await sessionDispatchSettings(CONFIG_FILE, process.env, {
            cwd: process.cwd(),
            commands: "kept",
            session: currentSessionID,
          })
        ).dispatch,
        env: process.env,
      });
      if (read.notice !== undefined) logger.warn(`envoy: ${read.notice}`);
      return carriedAnswers(read);
    } catch (error) {
      // Oh My Pi drops all a `context` handler returns when it throws, the dispatch-first skill
      // with it, so a read that throws instead of answering with its failure line still costs
      // only the list: it gives the failure line, with the error as its reason, which
      // `withSessionAnswers` appends after the last message unless the newest answers message
      // sent is a failure line already. The log says why through its failure episode
      // (`readFailed`), once per episode as for every failed read, so nothing is logged here.
      const reason = messageFor(error);
      return { text: sessionAnswersFailureLine(reason), reason };
    }
  };

  if (atStart.dispatch.enabled || inbound) {
    // Every card in a request says when this session got it, beside when it was sent
    // (`../src/card-age`), so a reader whose inbox ran behind does not take a late card's stamp
    // for the present time. Every session that reaches Dispatch, Legion panes and `task` subagents
    // included, also carries the dispatch-first skill on every request at the head, where its
    // bytes never change, and on every request of its main loop what people wrote to it: every
    // answers message an earlier request sent, which the session records and each request
    // rebuilds at its old place with its old bytes, and a new one after the last message when the
    // read shows a change (`withSessionAnswers`). The host runs this handler on its side turns,
    // its handoff prompt and a remote compaction's conversions too; `withSessionAnswers` leaves
    // the answers off those and records nothing for them. Oh My Pi hands this handler no abort
    // signal: it races the handler against the run's own and drops what an aborted handler
    // returns, and the handler goes on to its read, which it shares and which goes on to fill the
    // cache, and to its decision and its record, which come after those of every earlier request
    // of the session, so the request after it rebuilds what it recorded (`withSessionAnswers`).
    pi.on("context", async (event, context) => {
      // The host's own live id, as `message_end` reads it: this one process serves one session at
      // a time and moves between them, and a session that followed the same topic reads the same
      // event when its own host writes it, not when the session before it did (`../src/card-age`).
      // It is also the session the request is built from, as it began: the host can move to
      // another session while the read waits, and an answers message this request sent, like the
      // branch it reads again after the read, belongs to this one alone.
      const id = context.sessionManager.getSessionId();
      const aged = cardAges.annotate(
        event.messages,
        () => context.sessionManager.getBranch?.() ?? [],
        id
      );
      if (!atStart.dispatch.enabled) return aged === undefined ? undefined : { messages: aged };
      const withSkill = withDispatchFirst(aged ?? event.messages, DISPATCH_FIRST_CONTEXT);
      const messages = await withSessionAnswers(withSkill ?? aged ?? event.messages, {
        // The session's branch, read before the read and again after it: undefined once the host
        // is on another session than `id`, whose branch is not this request's, and the request
        // then decides from the branch as it began, and `record` below writes nothing.
        branch: () =>
          context.sessionManager.getSessionId() === id
            ? (context.sessionManager.getBranch?.() ?? [])
            : undefined,
        session: context.sessionManager,
        id,
        // Read off the live `AgentSession` (18.12.2), whose getters the declared host surface
        // leaves out: `get isCompacting() { return this.#C.isCompacting; }` and
        // `get compactionSpeculation() { return this.#C.speculationState; }`, the compaction
        // controller's `this.#i !== undefined || this.#u !== undefined || this.#l !== undefined`
        // (an automatic or manual compaction, or a handoff) and `e.armed ? "armed" : "running"`
        // for a speculative one. A session the registry does not list reads as not compacting. It
        // is read after the read too, so it asks about `id`, the session the request was built
        // from, and not the one the host may have moved to while the read waited.
        compacting: () => {
          const live = liveAgentSession(id);
          return (
            live !== undefined &&
            (("isCompacting" in live && live.isCompacting === true) ||
              ("compactionSpeculation" in live && live.compactionSpeculation === "running"))
          );
        },
        read: () => sessionAnswersText(context),
        // The answers message this request appends goes into the session file as it is sent, so
        // every later request, and the first of a process that resumes the session, rebuilds it
        // where it went. The write is guarded as `recordNotice` is: when the host moved to another
        // session while the read waited, the entry would land in that session's file, so nothing
        // is written and the log says so; the request still carries the message.
        record: (entry) => {
          if (context.sessionManager.getSessionId() !== id) {
            logger.warn(
              "envoy: the message of people's answers this request sent was not recorded: the host moved to another session while the request waited on the read"
            );
            return;
          }
          try {
            pi.appendEntry(ANSWERS_SENT_ENTRY, entry);
          } catch (error) {
            logger.warn(
              "envoy: the message of people's answers this request sent could not be recorded in the session file",
              { error: messageFor(error) }
            );
          }
        },
        // The first failed read of a failure episode, which runs until a read Dispatch answers,
        // whether the request shows the model a message for it (the failure line, or the cached
        // list when it shows something new, after the last message) or keeps the answers messages
        // sent, which do not name this read's failure. Logged once per episode, so a lasting
        // failure logs one line. The reason is this copy's read's own, as its failure line would
        // carry it, or the sentence an older copy's read line names, as that copy's renderer wrote
        // it, ending with a full stop, `?` or `!`.
        readFailed: (reason, shown) =>
          logger.warn(
            shown
              ? `envoy: people's answers could not be read (${reason}); the request tells the model after its last message`
              : `envoy: people's answers could not be read (${reason}); the request keeps the answers messages sent, which do not name this read's failure`
          ),
      });
      const result = messages ?? withSkill ?? aged;
      return result === undefined ? undefined : { messages: result };
    });
  }

  // A typed user turn begins one nudge period. The shared arming path also handles a
  // person-authored Dispatch card before its agent-initiated run starts; cards do not fire
  // `before_agent_start` on the host.
  pi.on("before_agent_start", async (event, context) => {
    if (event.prompt.trim() === "") return undefined;
    await armAskAwarenessForPersonInput(context);
    return undefined;
  });

  // Every run the host starts, typed or not: the user's prompt, an Envoy delivery waking the
  // session, another extension's continuation. `agent_end` compares it with what it read at the
  // settle, so a verdict that arrives after the session has moved on steers nothing. A run that
  // starts on the session a switch announced leaving is marked, and its end tells whether that
  // switch went ahead (`runUnderAnnouncement`).
  pi.on("agent_start", async (_event, context) => {
    runSeq += 1;
    cardWake.agentStart();
    runUnderAnnouncement =
      announced?.from === context.sessionManager.getSessionId() ? announced : undefined;
  });

  // Arms one nudge period. Its callers have already established that the input was authored by a
  // person: a nonempty typed prompt, or a Dispatch card with a `user` actor and no `act`. A
  // `triggerTurn` continuation of an agent-attributed custom message — which is what the nudge
  // below is — reaches neither caller, so the nudge cannot re-arm itself and one stop can produce
  // at most one of them.
  function armAskAwareness(id: string, asOf: string): void {
    askAwareness = {
      session_id: id,
      period: (askAwareness.session_id === id ? askAwareness.period : 0) + 1,
      baseline_as_of: asOf,
      check_due: true,
      checks: 0,
    };
  }

  /**
   * Gives a human-authored input the same safe arming transition, regardless of its host path.
   * `expectedSession` is the session a card is for: a card whose session the host is not on now
   * arms nothing, since the host hands that card to nobody. The period is armed only while the
   * generation and the host's session are those this call started under, so a session change or a
   * newer person input during the open-asks read leaves the state that change or input set.
   */
  async function armAskAwarenessForPersonInput(
    context: SessionContext | undefined,
    expectedSession?: string
  ): Promise<void> {
    const id = context?.sessionManager.getSessionId() ?? "";
    if (
      context === undefined ||
      id === "" ||
      (expectedSession !== undefined && id !== expectedSession) ||
      !context.hasUI ||
      sideTurn(context) === undefined ||
      legionManaged(id)
    ) {
      return;
    }
    // Memoized per instance once settled: one answer for the life of the session.
    if (await isSubagent(context)) return;
    // A person is about to start or has just caused a run that supersedes any previous stop.
    // This happens before the arming query, so a slow or failed query cannot leave a stale
    // self-check free to steer into the person's new work.
    const generation = ++awarenessGeneration;
    abortSelfCheck("a new person turn superseded the self-check");
    armingPersonInput = { session: id, generation };
    try {
      const open = await queryOpenAsks(id);
      if (
        open !== null &&
        generation === awarenessGeneration &&
        context.sessionManager.getSessionId() === id
      ) {
        armAskAwareness(id, open.as_of);
      }
    } catch (error) {
      warnAskAvailability(context, error);
    } finally {
      if (armingPersonInput?.generation === generation) armingPersonInput = undefined;
    }
  }

  // The run-end nudge. `agent_end` is the bare stop signal — no message, no shape to read — so
  // Dispatch supplies the open asks the self-check names. The hidden self-check decides whether
  // the agent is waiting for something outside that list; only its WAITING verdict buys the
  // steer. The check itself is owed and spent like the host's own todo reminder: the arming turn
  // owes one, the check spends it whatever the verdict, opening the ask spends it too, and the
  // agent's next real work owes another (`tool_result` below), so a run that keeps working keeps
  // being checked without a new user turn. A settled turn that only replies does no work, so the
  // nudge's own continuation never owes a check and cannot nudge itself;
  // `ASK_CHECKS_PER_PERIOD` bounds the rest.
  //
  // A run with no UI (`omp -p`, and any other headless launch) never gets it: the host disposes
  // the session at the end of that one run, and its output is already printed, so the steered
  // continuation either races dispose and is recorded aborted with no provider reply, or wins
  // and bills a whole turn whose text nobody reads. Both were measured on OMP 18.2.9. An RPC
  // host — every Legion pane — carries a UI context and is nudged as a terminal is. So does an
  // ACP host, but a client that defers agent-initiated turns gets the steer queued as hidden
  // next-turn context instead of a turn of its own, consumed when the user next prompts.
  pi.on("agent_end", async (event, context) => {
    cardWake.agentEnd(context);
    const id = context.sessionManager.getSessionId();
    const lastReply = event.messages?.findLast((message) => message.role === "assistant");
    // An interrupted run may have taken back the cards it held (`UnwrittenFrames`): those handed
    // before its end are looked at by the next heartbeat.
    if (lastReply?.stopReason === "aborted") unwritten.runAborted();
    // A person's direct message sent through `pi.sendUserMessage` and not yet seen as a user
    // message by the end of the run is not looked for again; one sent as typed input is, by its tag,
    // since the host can run it as a turn of its own after this run
    // (`@legion/pi-shared/injected-user-turns`). The record is kept under this instance's own
    // `sessionID`, where `deliver` notes a turn and the stream recorder matches it, and not under
    // the host's live id, which a fresh terminal's differs from until the heartbeat heals it.
    endInjectedUserTurns(sessionID);
    // A `pi.sendUserMessage` turn no longer looked for is done with now, found or not.
    await unwritten.textTurnsEnded();
    // A run that started under a switch's announcement and ended with a reply it was not aborted
    // in, the host still on the session it announced leaving: that switch was cancelled. A fork's
    // run ends after the host moved, which keeps the announcement, and with it the parent's frames.
    if (runUnderAnnouncement !== undefined && event.willContinue !== true) {
      if (
        announced === runUnderAnnouncement &&
        announced.from === id &&
        lastReply !== undefined &&
        lastReply.stopReason !== "aborted"
      ) {
        announced = undefined;
      }
      runUnderAnnouncement = undefined;
    }
    // Only a run that settled normally is nudged: steering an interrupt (`aborted`), a provider
    // failure (`error`), a truncation, or a run with no reply of its own answers the user's cancel,
    // or a failure, with a turn nobody asked for.
    const ask = sideTurn(context);
    if (
      event.willContinue === true ||
      lastReply?.stopReason !== "stop" ||
      !context.hasUI ||
      id === "" ||
      ask === undefined ||
      !checkOwed(id)
    ) {
      return;
    }
    const settle: AskSettle = { context, ask, seenRun: runSeq, generation: awarenessGeneration };
    if (askCheckInFlight) {
      settledDuringCheck = settle;
      return;
    }
    askCheckInFlight = true;
    // The check runs after this handler returns, on the host's managed timer: a side turn
    // started while an event handler is still running inherits that handler's abort signal
    // (Oh My Pi 18.3), which the host fires at its 30 s handler budget, well inside the check's
    // own `selfCheckTimeoutMs`. The host does not await `agent_end`, so nothing waits on it.
    context.setTimeout(() => checkFromSettle(id, settle), 0);
  });

  // Every stop-time check owed from `first` on, with `askCheckInFlight` held throughout. Each
  // pass after the first needs another real settle during the last, and every check that
  // reaches the model counts against the period's cap, so this ends.
  async function checkFromSettle(id: string, first: AskSettle): Promise<void> {
    try {
      let settle: AskSettle | undefined = first;
      while (settle !== undefined) {
        await checkAtSettle(id, settle);
        settle = takeSettledDuringCheck();
      }
    } finally {
      askCheckInFlight = false;
      settledDuringCheck = undefined;
    }
  }

  // Whether this session's current period still owes a stop-time check.
  function checkOwed(id: string): boolean {
    return !(
      shuttingDown ||
      (armingPersonInput !== undefined &&
        armingPersonInput.session === id &&
        armingPersonInput.generation === awarenessGeneration) ||
      legionManaged(id) ||
      // The host's live id, never the module's `sessionID`: a fresh TUI mints its id after
      // `session_start`, so the two disagree until the registration heartbeat heals the drift
      // (up to `ENVOY_HEARTBEAT_MS`, 90 s by default) and a brand-new terminal went unchecked
      // for that whole window. The period already carries the id that armed it, and a session
      // change resets the period to 0 and bumps the generation, so this is the discriminator —
      // `before_agent_start` and `tool_result` read the live id for the same reason. A `task`
      // subagent arms no period at all (it returns there), so it never reaches the check either.
      askAwareness.session_id !== id ||
      askAwareness.period === 0 ||
      !askAwareness.check_due ||
      askAwareness.checks >= ASK_CHECKS_PER_PERIOD ||
      askAwareness.baseline_as_of === null
    );
  }

  // One stop-time check for `settle`, run with `askCheckInFlight` held. Staleness is judged
  // against the run count and generation read at that settle, never at the start of this call.
  async function checkAtSettle(id: string, settle: AskSettle): Promise<void> {
    const { context, ask, seenRun, generation } = settle;
    const period = askAwareness.period;
    const baseline = askAwareness.baseline_as_of;
    // Both awaits below are windows in which a new user turn can arm another period, a
    // session change can move the session, or the agent's own next run can open the ask this
    // check is about — which clears the check it owed. The re-checks read one list, so they
    // cannot drift.
    const stale = (): boolean =>
      shuttingDown ||
      generation !== awarenessGeneration ||
      // Only the host's live id: see the guard above on the module's `sessionID`.
      context.sessionManager.getSessionId() !== id ||
      legionManaged(id) ||
      askAwareness.session_id !== id ||
      askAwareness.period !== period ||
      !askAwareness.check_due;
    // A settle is checked only while it is still current. The timer runs the first one a
    // macrotask after its stop, and one recorded while the last check was open waits for that
    // check: a run, a user turn, or a session change can have overtaken either meanwhile.
    // `baseline` is restated so the compiler sees it here.
    if (baseline === null || !checkOwed(id) || stale() || runSeq !== seenRun) return;
    let open: OpenAsksResponse | null;
    try {
      open = await queryOpenAsks(id, baseline);
    } catch (error) {
      if (generation === awarenessGeneration) warnAskAvailability(context, error);
      return;
    }
    if (open === null || stale()) return;
    // A run started while Dispatch answered: the conversation a self-check would read is past
    // this settle. Nothing is spent, so the newer run's own settle is where the check happens.
    if (runSeq !== seenRun) return;
    // Every normal settle takes the one path: Dispatch supplies the current open asks for the
    // prompt, not a reason to suppress it. `opened_since` remains part of the snapshot's
    // contract, but does not alter the self-check.
    // The self-check: one hidden, tool-free model call over a snapshot of this conversation
    // (`sideTurn`, the channel a targeted Dispatch BTW already uses), which adds nothing
    // to the transcript and which the user never sees. Only a WAITING verdict — the agent
    // saying it is waiting on a human for something its open asks do not cover — buys the visible
    // turn. PROCEEDING, any other answer, a timeout, and a failure are all silent, so an ordinary
    // settle costs one hidden call and shows nothing.
    //
    // The bound is the check's own clock, not the host's: the signal is still passed, so a
    // host that honours it stops paying for an answer nobody will read, but the await always
    // settles at `selfCheckTimeoutMs` whatever the host does. A host that ignored the signal
    // would otherwise hold `askCheckInFlight` — and with it every later check — for as long as
    // its call hung. A late answer is dropped; its rejection is already handled, so it can
    // never surface as an unhandled one.
    const abort = new AbortController();
    askCheckAbort = abort;
    const answered = ask({
      prompt: ASK_SELF_CHECK_PROMPT(open.asks ?? []),
      signal: abort.signal,
    }).then(
      (reply): string | undefined => reply.replyText,
      (error: unknown): string | undefined => {
        // An abort this extension made — a superseded check, or its own timeout, which logs
        // itself — is not a failure. Logging it would spend the once-per-session warning on
        // an ordinary event, such as the user typing, and silence the real failure after it.
        if (!abort.signal.aborted) logSelfCheckFailure(id, error);
        return undefined;
      }
    );
    const expiry = Promise.withResolvers<undefined>();
    const expire = setTimeout(() => {
      const timedOut = new Error(`self-check timed out after ${selfCheckTimeoutMs} ms`);
      abort.abort(timedOut);
      logSelfCheckFailure(id, timedOut);
      expiry.resolve(undefined);
    }, selfCheckTimeoutMs);
    let verdict: string | undefined;
    try {
      verdict = await Promise.race([answered, expiry.promise]);
    } finally {
      clearTimeout(expire);
      if (askCheckAbort === abort) askCheckAbort = undefined;
    }
    if (stale()) {
      // Whatever invalidated this check aborted it already if it could reach it; a host that
      // is still working on an answer nobody will read stops paying here.
      abort.abort(new Error("the self-check was superseded before its verdict arrived"));
      return;
    }
    // The check ran: it counts against the cap and moves the window on, whatever came back,
    // and it spends the period's outstanding check unless the session started another run
    // meanwhile — that run's own settle is where the check now belongs, and any work it did
    // owes one anyway.
    const superseded = runSeq !== seenRun;
    askAwareness = {
      ...askAwareness,
      check_due: superseded,
      checks: askAwareness.checks + 1,
      baseline_as_of: open.as_of,
    };
    // A verdict about a run the session has moved past is not steered into the one it is in:
    // "you just said you are waiting on a human" would describe a reply it has left behind.
    if (superseded) return;
    if (verdict === undefined || !isWaitingVerdict(verdict.trim())) return;
    // `deliverAs: "nextTurn"` with `triggerTurn` is the host's documented form for a message
    // sent during prompt teardown; on the pin this steer produced exactly one continuation in
    // every interactive and RPC run, so it stays the channel `deliver` already uses.
    pi.sendMessage(
      { customType: ASK_REMINDER_MESSAGE, content: UNASKED_WAIT_REMINDER, display: false },
      { deliverAs: "steer", triggerTurn: true }
    );
  }

  // The title file again (`writeDispatchTitle`, above the heartbeat): the main agent's tool call
  // is the other moment it is written, whatever the tool, so a session whose Dispatch writes
  // come from any path at all carries its name, and a session with Dispatch on and Envoy off,
  // which has no heartbeat, has this as its only writer. A subagent writes nothing:
  // `DISPATCH_SESSION_ID` belongs to the process, so a subagent would put its own name in its
  // parent's file. The write is wrapped as the heartbeat's is: it runs before every tool call of
  // the main agent now, and a title the plugin cannot write — a session id that is no path
  // segment, a state directory it cannot reach — must fail none of them. Said once, since it
  // fails the same way at every call; this writer is built once per session, beside the handler
  // that is registered once per session, so its flag is that session's.
  const writeToolCallTitle = dispatchTitleWriter();
  pi.on("tool_call", async (_event, context) => {
    if (context.agent?.kind !== "main") return undefined;
    writeToolCallTitle(context);
    return undefined;
  });

  // AskGate's `tool_call` handler, registered now, in the factory, so it is there for the first
  // call; it waits for the modules the gate loads (`registerAskGate`). Only a session with
  // Dispatch and the switch on has one (`askgateFile`, above).
  if (askgateFile !== null) {
    const gateFile = askgateFile;
    // What the gate logs at bind is also a notice at session start: the one it logs there says
    // the review deadline is under 30 s, which at the host's default handler limit is every
    // profile that sets nothing, and a review that times out lets the write post. A warning a
    // gated call raises later goes to the plain log alone (`runtimeLogger`): it is about that
    // session's calls, and a notice here would show again at every later session start.
    const gateLogger = {
      warn: (message: string) => {
        logger.warn(message);
        askgateNotices.push(message);
      },
    };
    const registration = registerAskGate(pi, {
      configFile: gateFile,
      logger: gateLogger,
      runtimeLogger: logger,
      session: currentSessionID,
    });
    askgateRecorder = registration;
    askgateLoad = registration.loaded.catch((error: unknown) => {
      const notice = `AskGate did not load, so it refuses each Dispatch write it would review: ${messageFor(error)}. To turn AskGate off for this session, set askgate.enabled to false in ${gateFile.path}.`;
      logger.warn(notice);
      askgateNotices.push(notice);
    });
  }

  pi.on("tool_result", async (event, context) => {
    // Read, and the offset advanced, on every result, failed or not and armed period or not, so a
    // later result never counts the calls this one ran.
    const entries = readNewDispatchResults();
    // A gated write the CLI found no AskGate marker for (refused in block mode, posted with a
    // warning in warn mode) becomes an `advisor-gate` entry, so AskGate's report counts it. Only
    // the top-level instance records it: it reads each line of the session's ledger once, the
    // lines of a subagent's calls included.
    if (
      askgateOwner !== undefined &&
      entries.some((entry) => entry.askgate !== undefined) &&
      !(await isSubagent(context))
    ) {
      for (const [line, { tool, askgate }] of entries.entries()) {
        if (askgate === undefined) continue;
        const entry: GateEntry = {
          advisor: "AskGate",
          tool,
          path: `dispatch ${commandName(tool)}`,
          toolCallId: event.toolCallId,
          decision: askgate.mode === "block" ? "revise" : "allow",
          outcome: "unmarked",
          verdictMode: askgate.mode,
          reason: askgate.why,
          latencyMs: 0,
          revisesForKey: 0,
          argsDigest: "",
        };
        // The entry above reaches only this machine's disk. A write that would have opened an
        // ask for a person also goes to Dispatch, where a session with no access to this machine
        // counts the asks that went out, or were refused, unreviewed: the record carries the
        // tool, the mode and how many asks the write carried, and no ask text. The entry is
        // appended first and the record started behind it, awaited by nothing of this call: a
        // session's records are written one at a time and each read and write has the client's
        // own deadline, so awaiting one here held this tool result for as long as every record
        // queued before it took, past the host's handler limit (#2161, reviewer round 17). A
        // record that fails or that the shutdown cuts rides on an `advisor-gate-record` follow-up
        // of its own, as a gated call's does (`askgate-core.ts`), appended while the session is
        // still live; the recorder's cut at shutdown is what bounds it.
        const asksCarried = askgate.asksCarried ?? 0;
        // One tool call can log several gated writes, so the record's id names the ledger line's
        // own place in it: what the reader de-duplicates a replayed write by, and what ties the
        // follow-up of a record that failed to this entry alone — the gated calls of that same
        // tool call carry its id too.
        const recordId = `${event.toolCallId}#${line}`;
        const recorder = asksCarried > 0 ? askgateRecorder : undefined;
        pi.appendEntry(ADVISOR_GATE_ENTRY_TYPE, {
          ...entry,
          ...(recorder === undefined ? {} : { recordId }),
        } satisfies GateEntry);
        if (recorder === undefined) continue;
        void (async () => {
          let recordError: string | undefined;
          try {
            recordError = await recorder.recordUnmarked({
              entry: { ...entry, asksCarried },
              sessionId: context.sessionManager.getSessionId(),
              directory: context.cwd,
              id: recordId,
            });
          } catch (error) {
            recordError = messageFor(error);
          }
          if (recordError === undefined) return;
          logger.warn(
            `envoy: AskGate did not record the unmarked ${entry.path} on Dispatch: ${recordError}`
          );
          try {
            pi.appendEntry(ADVISOR_GATE_RECORD_ENTRY_TYPE, {
              toolCallId: entry.toolCallId,
              tool: entry.tool,
              recordId,
              recordError,
            } satisfies GateRecordEntry);
          } catch {
            // The session is sealed or its file is gone: there is nothing left to tell, and the
            // tool result this record belongs to was answered long before it ran.
          }
        })();
      }
    }
    if (event.isError) return;
    // The host's live id, never the module's `sessionID`, for the reason the stop guard reads
    // it: in a fresh TUI's drift window the two disagree, and the period is armed against the
    // live one, so a comparison against the module copy matched nothing for up to a heartbeat —
    // the window this nudge was opened up for.
    if (
      askAwareness.session_id !== context.sessionManager.getSessionId() ||
      askAwareness.period === 0
    ) {
      return;
    }
    const openedAsk = entries.some(
      (entry) =>
        entry.details !== undefined && opensAsk({ toolName: entry.tool, details: entry.details })
    );
    // A call that owes no check: a bash command that is one `dispatch` command and wrote to the
    // ledger, or any eval cell during which the ledger grew. The two differ on purpose: a bash
    // command's text is scanned, while an eval cell runs `dispatch` only through a shell its code
    // starts, which no scan reads, so for it the ledger growing is the only signal.
    const bashRanDispatchAlone =
      event.toolName === "bash" && dispatchCommandHead(event.input.command) !== undefined;
    const owesNoCheck = entries.length > 0 && (bashRanDispatchAlone || event.toolName === "eval");
    if (openedAsk) {
      // The agent asked the humans itself, so this stop has nothing left for the nudge to
      // say: it spends the check the period owed rather than ending the period, and aborts a
      // check in flight before its verdict can be used. Later real work can re-arm a fresh
      // check, whose prompt names the ask.
      askAwareness = { ...askAwareness, check_due: false };
      abortSelfCheck("the agent opened the ask itself");
    } else if (!owesNoCheck) {
      // Real work: it owes the period another check, the way finishing a step re-arms the
      // host's todo reminder. A `dispatch` command the ledger records is the agent talking to the
      // humans this nudge is about, not work, so it owes nothing — and a turn that only replies
      // calls no tool at all, which is what stops the nudge's own continuation from re-arming
      // itself.
      askAwareness = { ...askAwareness, check_due: true };
    }
  });

  async function execute(
    operation: EnvoyToolOperation,
    rawParameters: Record<string, unknown>,
    context: SessionContext
  ): Promise<ToolResult> {
    try {
      // The host hands raw arguments through (lenientArgValidation); this is the one
      // refusal, naming every problem, before anything reaches the listener.
      const parameters: Record<string, unknown> = parseEnvoyToolArguments(operation, rawParameters);
      const { client } = requireEnvoy();
      switch (operation) {
        case EnvoyToolOperation.subscribe: {
          // A followed topic reaches the session only through its own consumer's relay, which
          // needs NATS: with inbound off the session would follow a topic it can never receive
          // on, so the tool refuses here, saying why.
          requireInbound();
          const requested = topicsFor(parameters);
          // A topic no stream subject captures is followed but never registered, since the
          // listener stores none: it drops such a topic from the row and names it in a warning
          // (`registrationTopics`). A caller that names one here asked for this topic by name,
          // so it is told by name, in the listener's own words, rather than left following a
          // subject nothing is ever delivered on. A private stream subject is left out of the
          // registration without a word, as it always was (`refuseUncapturedTopics`).
          refuseUncapturedTopics(sessionID, legacyAgentSubject(sessionID), requested);
          const added: string[] = [];
          const already: string[] = [];
          // A subject only the held role wanted is the session's own too from here on, so the
          // role's end leaves it followed and the ledger records it.
          let claimed = false;
          for (const topic of requested) {
            (follow(topic) ? added : already).push(topic);
            for (const subject of expandSubscriptionTopics([topic])) {
              if (roleTopicFollows.claimOwn(subject)) claimed = true;
            }
          }
          let warnings: readonly string[] | undefined;
          let registrationError: string | undefined;
          // The topics this call's own registration was refused, whatever another registration
          // or an unsubscribe did to what the session follows meanwhile.
          const dropped: string[] = [];
          if (added.length > 0) {
            try {
              warnings = (await registerSession(dropped)).warnings;
            } catch (error) {
              registrationError = messageFor(error);
            }
          }
          // A new follow is stamped now, so the session's consumer relays what is published from
          // here on and nothing the stream stored before it; a role's subject claimed as the
          // session's own is now recorded as its own.
          if (added.length > 0 || claimed) await ledgerFollowedChanged();
          // An edge listener refuses a topic of no project of the session's token, and the
          // registration drops it from what the session follows (`registerWithRelay`): the caller
          // asked for it by name, so it is told by name, not reported subscribed.
          const refused = added.filter((topic) =>
            expandSubscriptionTopics([topic]).some((subject) => dropped.includes(subject))
          );
          const kept = added.filter((topic) => !refused.includes(topic));
          const lines = [`Subscribed: ${kept.join(", ") || "(none new)"}`];
          if (refused.length > 0) {
            lines.push(
              `Refused by the listener for this session's projects: ${refused.join(", ")}`
            );
          }
          if (warnings !== undefined && warnings.length > 0) {
            lines.push(`Warnings: ${warnings.join("; ")}`);
          }
          return toolSuccess(lines.join("\n"), {
            added: kept,
            already,
            ...(refused.length === 0 ? {} : { refused }),
            ...(warnings === undefined || warnings.length === 0 ? {} : { warnings }),
            ...(registrationError === undefined ? {} : { registrationError }),
          });
        }
        case EnvoyToolOperation.unsubscribe: {
          // A stopping session drops no topic (R24 of dispatch://INFRA-3589): the stop deletes
          // the session's record next, so the tool is refused before it changes anything, as the
          // client would refuse the removal and the ledger would take no write.
          if (shuttingDown) throw new Error(STOPPING_UNSUBSCRIBES_NOTHING);
          // The session's own inbox is not a subscription the tool manages.
          // A role topic is followed through the independently-held role claim: drop the local
          // follow without unregistering that interest.
          const inbox = legacyAgentSubject(sessionID);
          // Concrete subjects, a named `X.>` already expanded to `X.>` and `X`.
          const candidates = topicsFor(parameters, [...followed]).filter(
            (subject) => subject !== inbox
          );
          // A subject the role this session holds wants lasts as long as the role does: a notice
          // subject a plugin marked for it, or one of the topics the deployment names for it
          // (`ENVOY_ROLE_TOPICS`), which the session may have followed itself too. The tool keeps
          // it and says so rather than leave the session deaf to it; for a role topic it drops only
          // the session's own claim (`releaseOwn`), so the role's end closes it. Each candidate is
          // dropped on its own: `unfollow` would expand a named `X.>` back to `X` as well, and `X`
          // can be the role's own subject, kept here.
          const notices = new Set(registrationState.pluginRoleSubjects());
          const keeps = (subject: string): boolean =>
            notices.has(subject) || roleTopicFollows.has(subject);
          const kept = candidates.filter(keeps);
          // A role topic the session also wanted itself is now only the role's, so the ledger
          // stops recording it as the session's own.
          const released = kept.filter((subject) => roleTopicFollows.releaseOwn(subject));
          const removed = candidates
            .filter((subject) => !keeps(subject))
            .filter((subject) => followed.delete(subject));
          // Persisted at once, so the topic stays unfollowed across a restart.
          if (removed.length > 0 || released.length > 0) await ledgerFollowedChanged();
          const registryRemoved = removed.filter((topic) => !topic.startsWith(ROLE_TOPIC_PREFIX));
          const registrationError =
            registryRemoved.length === 0
              ? undefined
              : await client
                  .unsubscribe({ sessionID, topics: registryRemoved })
                  .then(() => registerSession())
                  .then(() => undefined, messageFor);
          const unsubscribed = `Unsubscribed: ${removed.join(", ") || "(none)"}`;
          // The role these last for: the one held, or, before the claim lands, the one a resumed
          // session opened its topics for and is claiming.
          const held = registrationState.heldRole();
          const lastsFor =
            held !== undefined ? `holds role ${held}` : `claims role ${roleTopicFollows.role()}`;
          return toolSuccess(
            kept.length === 0
              ? unsubscribed
              : `${unsubscribed}\nKept (they last while this session ${lastsFor}): ${kept.join(", ")}`,
            {
              removed,
              ...(kept.length === 0 ? {} : { kept }),
              ...(registrationError === undefined ? {} : { registrationError }),
            }
          );
        }
        case EnvoyToolOperation.listInterests: {
          const registry = await client.getInterest(sessionID);
          const interests = mergeInterestSources(registry.topics, [...followed]);
          return toolSuccess(
            JSON.stringify({ ...registry, topics: [...interests.keys()] }, null, 2),
            {
              interests: [...interests].map(([topic, source]) => ({ topic, source })),
            }
          );
        }
        case EnvoyToolOperation.inbox:
          return toolSuccess(JSON.stringify(inbox, null, 2), { count: inbox.length });
        case EnvoyToolOperation.send: {
          const targetSessionID = stringFor(parameters, "session_id");
          const idempotencyKey = parameters.idempotency_key;
          const result = await client.send({
            sourceSessionID: await replyAddress(context),
            targetSessionID,
            message: stringFor(parameters, "message"),
            ...(typeof idempotencyKey === "string" ? { idempotencyKey } : {}),
            ...toMessageMetadata(parameters as MessageMetadataArguments),
          });
          if (result.keyTaken !== undefined) {
            return toolSuccess(keyTakenText(result, "envoy_send"), keyTakenDetails(result));
          }
          return toolSuccess(sendConfirmationText(result), {
            event_id: result.envelope.event_id,
            recipient: result.recipient,
            recipient_state: result.recipientState,
            confirmed: result.confirmed,
            state: result.state,
            ...(result.lastSeen === undefined ? {} : { last_seen: result.lastSeen }),
          });
        }
        case EnvoyToolOperation.publish: {
          const topic = stringFor(parameters, "topic");
          const inSubagent = await isSubagent(context);
          const source = await replyAddress(context, "", inSubagent);
          const result = await client.publish({
            sourceSessionID: source,
            topic,
            message: stringFor(parameters, "message"),
            ...toMessageMetadata(parameters as MessageMetadataArguments),
          });
          if (result.keyTaken !== undefined) {
            return toolSuccess(keyTakenText(result, "envoy_publish"), keyTakenDetails(result));
          }
          // The skip is the listener's own delivery: the role lane skips the holder it resolved
          // when that holder is the message's source, and the fanout skips every interest whose
          // session is that source. The session still gets its own publish from its own
          // consumer's relay, so its own publish, and a subagent's, does reach it. Only the
          // role lane, which its holder does not follow, actually goes undelivered: a
          // subagent's source is its parent, so a publish to a role that parent holds is
          // accepted, answers with that holder, and reaches nobody. Only the role case is
          // visible here: a plain topic's answer counts its subscribers and names none, since
          // the stream stores one copy each session's own consumer delivers. The listener's drop
          // of a role message whose holder is its source is the general rule
          // (skills/envoy/references/deliveries.md). A direct envoy_send is unaffected: the
          // agent-subject lane has no such skip.
          // A kept answer reached no session at all, so the echo rule cannot apply to it: the
          // holder it names is the session the resolution read, which never received the message.
          const undelivered =
            result.kept !== true && source !== "" && result.holder === source && inSubagent;
          // The listener sends a message for a kept copy no session took only to the publish's
          // source session, and a subagent whose parent cannot be resolved publishes from none.
          const published = publishConfirmationText(result, { senderNamed: source !== "" });
          return toolSuccess(
            undelivered
              ? `${published}\nNot delivered: that holder is the session this subagent sends as, and the listener drops a message whose source session is its recipient, so the agent that spawned you did not receive it. Reach it with a write to its agent:// address instead.`
              : published,
            {
              event_id: result.envelope.event_id,
              topic,
              ...publishResultDetails(result),
              ...(undelivered ? { undelivered_echo: true } : {}),
            }
          );
        }
        case EnvoyToolOperation.setRole: {
          const role = stringFor(parameters, "role");
          // A stopping session claims no role: refused here at once, and again when the move's
          // turn comes, before anything is said, for a call queued before the stop began.
          refuseClaimWhileStopping(role);
          // Read and checked inside the move, so a move landing first is the role this one leaves.
          // The session the move is decided for, read when its turn comes and kept for the
          // refusal below, which the session the host is on by then would misname.
          let target = sessionID;
          let claimAdopted = false;
          const moved = await registrationState.roleMoves.run(
            async (handle) => {
              refuseClaimWhileStopping(role);
              target = sessionID;
              handle.actFor(target);
              requireSessionRoleFor(target, role);
              const held = registrationState.heldRole();
              const moving = held !== undefined && held !== role;
              // Moving off a role closes every subscription another plugin opened `whileHolding`
              // it, so the session would stop taking that role's notices without being told. The
              // model's tool never does that; the plugin that owns the role moves it by its claim.
              const bound = registrationState.pluginRoleSubjects();
              if (moving && bound.length > 0) {
                throw new Error(
                  `This session holds role ${held}, and ${bound.join(", ")} lasts only while it does; envoy_role_set does not move it to ${role}`
                );
              }
              const claimed = await handle.claimRole(role);
              await registerRoleTopics(handle, target);
              return {
                released: moving ? held : undefined,
                warnings: claimed.claimed ? claimed.warnings : [],
                following: roleTopicFollows.subjects(),
              };
            },
            (refusal) => {
              claimAdopted = refusal.claimAdopted;
            }
          );
          // The move decided nothing, because the session's stop began, or because the session
          // it was decided for stopped being the open one: the tool says so rather than report a
          // role it never took, and says which of the two the listener left behind.
          if (moved === undefined) {
            refuseClaimWhileStopping(role);
            throw sessionLeftDuringClaim(target, role, claimAdopted);
          }
          const { released, warnings, following } = moved;
          // The listener's warnings about the claim, each logged where the claim landed
          // (`claimRole`), go to the agent too: a handover of the replaced session it did not make
          // is something the agent that claimed must know. So does what this session now follows
          // of the topics the deployment names for the role, for as long as it holds it, so the
          // model knows why their messages arrive.
          const held =
            released === undefined
              ? `Now holding role: ${role}`
              : `Now holding role: ${role} (released role ${released})`;
          return toolSuccess(
            [
              held,
              ...(following.length === 0
                ? []
                : [
                    `Following while it holds the role (${ROLE_TOPICS_VAR}): ${following.join(", ")}`,
                  ]),
              ...warnings.map((warning) => `Listener warning: ${warning}`),
            ].join("\n"),
            {
              role,
              ...(released === undefined ? {} : { released }),
              ...(warnings.length === 0 ? {} : { warnings }),
              ...(following.length === 0 ? {} : { following }),
            }
          );
        }
        case EnvoyToolOperation.getRole: {
          const role = await client.getRole(stringFor(parameters, "role"));
          // A listener that names only one of the two states still shows the model one answer:
          // what state the holder is in, and when it was last seen.
          const held = { ...role, holder_state: holderStateOf(role) };
          return toolSuccess(JSON.stringify(held, null, 2), held);
        }
        case EnvoyToolOperation.whoami: {
          const inSubagent = await isSubagent(context);
          const address = await replyAddress(context, "", inSubagent);
          // A `task` subagent registers no Envoy session of its own, so the address a reply
          // reaches is the session that spawned it. Its own host id is reported beside that,
          // never as the reply address: nothing is listening on it.
          const subagent = inSubagent
            ? {
                session_id: context.sessionManager.getSessionId(),
                note:
                  address === ""
                    ? "This session is a task subagent and registers no Envoy session of its own, and this process records no top-level session its transcript traces back to, so it has no reply address at all: say who you are in the message body, and reach the agent that spawned you with a write to its agent:// address."
                    : "This session is a task subagent and registers no Envoy session of its own; a reply to session_id reaches the agent that spawned it, which relays it with a write to your agent:// address. Your own envoy_publish never reaches that agent — use a write to its agent:// address for that hop.",
              }
            : undefined;
          return toolSuccess(
            JSON.stringify(
              {
                session_id: address,
                machine_id: machineID(),
                // A subagent's instance stored no directory either (`restoreLocalSessionState`
                // never ran for it), so the host's live one answers for it.
                dir: sessionDirectory || context.cwd,
                ...(subagent === undefined ? {} : { subagent }),
              },
              null,
              2
            ),
            {
              sessionID: address,
              topics: [...followed],
              ...(subagent === undefined ? {} : { subagent }),
            }
          );
        }
        case EnvoyToolOperation.listSessions: {
          const machine = optionalStringFor(parameters, "machine");
          const directory = optionalStringFor(parameters, "dir");
          const title = optionalStringFor(parameters, "title");
          // The stalled sessions are asked for by name: a peer that missed its heartbeats is
          // still running, and a list that leaves it out is read as the peer having died.
          const sessions = await client.listSessions({
            directory,
            title,
            include: "unresponsive",
          });
          const result = sessionsOnMachine(sessions, machine);
          return toolSuccess(JSON.stringify(result, null, 2), {
            count: result.length,
            unresponsive: result.filter((session) => session.state === "unresponsive").length,
            machine,
            dir: directory,
            title,
          });
        }
        case EnvoyToolOperation.slackPost: {
          const posted = await postSlackReply({
            channel: stringFor(parameters, "channel"),
            threadTs: stringFor(parameters, "thread_ts"),
            text: stringFor(parameters, "text"),
          });
          return toolSuccess(
            `posted ${posted.ts} in ${posted.channel}, thread ${posted.threadTs}`,
            { channel: posted.channel, thread_ts: posted.threadTs, ts: posted.ts }
          );
        }
      }
    } catch (error) {
      return toolFailure(error);
    }
  }
}

function schemaFor(pi: PiApi, operation: EnvoyToolOperation): unknown {
  const spec = envoyToolSpecs.find((candidate) => candidate.operation === operation);
  if (spec === undefined) throw new Error(`missing Envoy tool specification for ${operation}`);
  return pi.zod.object(spec.arguments(zodSchemaApi(pi.zod)));
}

function stringFor(parameters: Record<string, unknown>, key: string): string {
  const value = parameters[key];
  if (typeof value !== "string") throw new TypeError(`${key} must be a string`);
  return value;
}

function optionalStringFor(parameters: Record<string, unknown>, key: string): string | undefined {
  const value = parameters[key];
  if (value === undefined) return undefined;
  if (typeof value !== "string") throw new TypeError(`${key} must be a string`);
  return value;
}

function isStringArray(value: unknown): value is readonly string[] {
  return Array.isArray(value) && value.every((entry: unknown) => typeof entry === "string");
}

function topicsFor(
  parameters: Record<string, unknown>,
  fallback: readonly string[] = []
): readonly string[] {
  const value = parameters.topics;
  if (value === undefined) return fallback;
  if (!isStringArray(value)) throw new TypeError("topics must be strings");
  return expandSubscriptionTopics(value);
}
