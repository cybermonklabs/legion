import { ROLE_TOPIC_PREFIX } from "@legion/contracts";
import { messageFor } from "@legion/envoy-client/errors";
import {
  EnvoyApiError,
  type EnvoyClient,
  expandSubscriptionTopics,
  type Interest,
  isBusUnanswered,
  newClientInstanceID,
  roleHeldBy,
  type SubscribeInput,
  type SubscribeResult,
} from "@legion/envoy-client/transport";
import type { RoleRegainReason } from "@legion/pi-shared/role-claim-bridge";
import { createHandBackLedger } from "./hand-back-ledger";
import type { SessionInbound } from "./session-inbound";
import { createQueue, type RunningTask, refuseStep, type TaskQueue } from "./task-queue";

/**
 * The registration and role state of the Envoy extension, this extension instance's client
 * instance id, the two serial queues that write the state (`passes` and `roleMoves`), and the
 * handle each queued task acts through. This module builds the two queues from the generic
 * serial queue of `./task-queue`, and gives that queue the handle builder (`createHandle`). The
 * queue holds each task's order and deadline timer, and knows nothing of this state.
 *
 * A queued task never re-checks a fence before a step, and keeps {@link TaskHandle.check} for the
 * two things that are not steps: a catch-all that would otherwise swallow the refusal, and an
 * action outside the handle that a refused task must not take. `run` hands it a
 * {@link TaskHandle}, and every state write, listener request, subscribe and inbound
 * connection it makes after a wait is a method of that handle. The handle refuses the step once
 * the task's deadline has fired, for a task bound to a session (`actFor`) once that session
 * stopped being the open session, and for a role move once the session's stop has begun
 * (`stopping`). The first refusal latches: a session that closes and opens again does not revive
 * a task a step already refused, so a task that caught the refusal cannot go on writing. A
 * refused step writes nothing and ends the task: past the deadline the caller's promise has
 * already rejected with the deadline rejection (`taskWasAbandoned`) and the late settlement is
 * swallowed; after the session moved, or once the stop began, `run` resolves `undefined`, so a
 * pass the host left mid-pass decides nothing and the user sees no new warning.
 *
 * The state is private to this module: callers read it through the accessors below and write it
 * only through a named operation or a queued task's handle, so there is no shared variable a
 * later edit can write unguarded. The holds this process gave up and still owes the listener are
 * a module of their own, {@link createHandBackLedger}, which this one holds and nothing else
 * reaches.
 */

/** Whether a topic served the session's role, and how many roles had ended, at one moment. */
export interface RoleSnapshot {
  readonly roleBound: boolean;
  readonly endsBefore: number;
}

/** What a registration carries, less the client instance id this module mints and sends itself. */
export type RegistrationInput = Omit<SubscribeInput, "clientInstanceID">;

/** The warnings this module writes; `logger` from the host satisfies it. */
export interface RegistrationStateLog {
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

/** What a queued task's NATS dial may spend: the task's deadline, as a `performance.now()`
 *  instant, and whether the task stopped acting (abandoned, refused, or its session moved), after
 *  which the dial starts no further attempt. */
export interface DialLimit {
  readonly deadlineAt: number;
  readonly stopped: () => boolean;
}

/**
 * Everything the module needs from its host, injected so the package's own unit tests exercise it
 * with plain fakes: no Oh My Pi host type is imported here. The connection is the session's inbound
 * connection, NATS or the listener's relay socket (`SessionInbound`): the module closes one a
 * refused task opened, and hands the host's own to a task.
 */
export interface RegistrationStateDeps {
  /** The listener client, read at each call: it throws while Envoy is off for this session, as
   *  `requireEnvoy()` does, and the queues exist whatever the session resolved. */
  readonly client: () => EnvoyClient;
  /** Whether `id` is the session open here, for this instance and the host alike. */
  readonly sessionOpen: (id: string) => boolean;
  /**
   * Whether this session's stop has begun, read at each step of a role move. The stop hands back
   * every role this process holds and deletes the session's record next, so from then on a role
   * move claims nothing, transfers nothing and tells nothing (R24 of dispatch://INFRA-3589).
   */
  readonly stopping: () => boolean;
  /** The deadline each queued task gets, read afresh when that task starts. */
  readonly deadlineMs: () => number;
  /** Tells the pane what a state write decided. */
  readonly notify: (message: string, level: "warning") => void;
  readonly log: RegistrationStateLog;
  /**
   * Writes the durable transcript record of a claim this process landed: the role, the session
   * it was claimed for, and this process's own client instance, so a later read of that record
   * tells a claim this process made from one another process wrote on the same transcript.
   */
  readonly appendRoleClaimEntry: (claim: {
    readonly role: string;
    readonly sessionID: string;
    readonly clientInstanceID: string;
  }) => void;
  /** Opens the session's inbound connection, storing nothing: this module closes one a refused
   *  task opened. A queued task's dial is given the task's deadline and whether the task was
   *  abandoned, so its redials end in time for the rest of the task; a dial outside a task gets
   *  no limit. */
  readonly dial: (limit?: DialLimit) => Promise<SessionInbound>;
  /**
   * The session's one inbound connection, made with `dial` when it has none and kept by the host.
   * The whole acquisition is the fenced step, not the dial alone: the host's own waits after the
   * dial (closing a connection another caller won the race with, and taking that caller's
   * connection instead) happen inside this call, and the handle checks the task again after it.
   */
  readonly connect: (dial: () => Promise<SessionInbound>) => Promise<SessionInbound>;
  /**
   * Opens `topic`'s subjects over `connect`'s connection; answers whether any subjects were new.
   * A queued task is handed its own fenced `connect`, so a subscribe whose connection outran the
   * task opens nothing: a dial it made is closed and the task ends before any subject opens.
   */
  readonly subscribeTopic: (
    topic: string,
    connect: () => Promise<SessionInbound>
  ) => Promise<boolean>;
  /**
   * Stops following the subjects this module no longer keeps open, the live subscriptions
   * staying with the extension, and takes them out of the listener's registry under
   * `sessionID` — the session the hold was under, empty when the module holds none, which the
   * host reads as its own open session. A role's end closes every notice subject a plugin
   * opened while it was held, and those were registered while the role stood: a notice lane
   * reaches a session from its own consumer alone, so they leave the session's consumer filter
   * only by leaving the registry. Best-effort, and never awaited here.
   */
  readonly closeSubjects: (sessionID: string, subjects: readonly string[]) => void;
}

/** The role this process gave up locally while the listener may still name it. */
interface RoleEnded {
  readonly role: string;
  /** Whether a plugin held notice subjects for it, which the role's end closed. */
  readonly pluginHeld: boolean;
}

/** What a heartbeat's re-assertion of a held role decided. */
interface RoleReassertion {
  /** False when the hold moved off this role while the listener was read or claimed. */
  readonly decided: boolean;
  /** Set when the Legion regain hook owes a run for this role. */
  readonly regain?: RoleRegainReason;
}

/**
 * The steps a queued task may take. Each refuses before it acts, so no call site re-checks a
 * fence before a step. Refusal throws, which ends the task, and latches: once a step refused, so
 * does every step after it, whatever the session does next. A role move's steps refuse once the
 * session's stop has begun as well (`RegistrationStateDeps.stopping`).
 */
export interface TaskHandle {
  /**
   * Binds this task to the session it acts for, called once as soon as the task learns it. From
   * then on every step refuses after that session stopped being the open session. Binding twice
   * throws: a task acts for one session.
   */
  readonly actFor: (id: string) => void;
  /**
   * Refuses here, and otherwise does nothing. Two things need it, because neither is a step:
   * code that catches whatever a step threw and would otherwise swallow the refusal, and an
   * action outside the handle (a replay trigger, a throw the caller reports) that a refused task
   * must not take. Before a step, it is redundant: the step refuses itself.
   */
  readonly check: () => void;

  // — registration —
  /** Registers the session at the listener, carrying this instance's client instance id. */
  readonly register: (input: RegistrationInput) => Promise<SubscribeResult>;

  // — the session a pass converges to —
  /** Records that this session's agent subject, registration and Dispatch binding are converged. */
  readonly markEstablished: (id: string) => void;
  /** Forgets every agent session but `host`; answers the ones whose subject the caller must close.
   */
  readonly forgetOtherAgentSessions: (host: string) => readonly string[];
  /** Records that this process may have `id`'s agent subject open. */
  readonly markAgentSessionOpen: (id: string) => void;
  /** Records that `id`'s agent subject, stream watch and registered interests are open in full. */
  readonly markInboundOpen: (id: string) => void;
  /** Records that a pass decided the role for `id`. */
  readonly markRoleSettled: (id: string) => void;

  // — the role —
  /**
   * Ends the role this process holds and hands its hold back at the listener. Answers the role it
   * ended, or `undefined` when it held none. With `tellUser`, says so in the pane: a role end
   * after a move no session event announced is one no plugin heard.
   */
  readonly handBackHeldRole: (tellUser: boolean) => Promise<RoleEnded | undefined>;
  /**
   * Settles the holds session events gave up: one of `wanted`, under a session this process still
   * registers, comes back unconfirmed; every other is handed back, and one the listener refuses is
   * abandoned. Answers whether a hold came back.
   */
  readonly settleEndedHolds: (wanted: string | undefined) => Promise<boolean>;
  /**
   * Claims `role` for this task's session. A hard claim is last-claim-wins. A soft claim naming
   * `previousSessionID` takes the role from that predecessor only when the predecessor has no
   * live registry entry, when its live entry carries this client instance's own instance id, or
   * when either side carries no instance id. A predecessor still live under another client
   * instance keeps the role. A soft claim is also granted when the role is unheld, and when its
   * holder is no longer live. Answers whether the claim landed.
   *
   * A claim the listener granted after this task's deadline fired is left as a pending hand-back,
   * and one granted after the session moved is adopted under the id the listener names.
   */
  readonly claimRole: (
    role: string,
    options?: { readonly soft?: boolean; readonly previousSessionID?: string }
  ) => Promise<boolean>;
  /**
   * Makes sure the listener still resolves this session as `role`'s holder: reads first, and a
   * soft claim goes out only when it does not. The lookup confirms the role only when it names
   * this session *and* this process, since a claim standing under this very session id that
   * another process made is not this one's to renew; a holder naming no process is read as this
   * one's. A refusal naming a different session ends the role here and records it. A refusal
   * naming this session's own id is that other process's standing claim, which the listener
   * grants the moment it stops renewing it: the role stays held here, nothing is recorded or
   * said, and the next heartbeat asks again.
   */
  readonly reassertRole: (role: string, afterOutage: boolean) => Promise<RoleReassertion>;
  /** Says, once, that the notices a plugin opened for `role` stay closed, and forgets that it
   *  opened them. Writes nothing while that plugin's subjects are still open. */
  readonly noteNoticesClosed: (role: string) => void;

  // — listener reads and writes —
  /** The session the listener names as `role`'s holder; `undefined` when it names none or the
   *  read failed. */
  readonly readRoleHolder: (role: string) => Promise<string | undefined>;
  /** The listener's registered interests for a session; `undefined` when the read failed. */
  readonly readInterest: (sessionID: string) => Promise<Interest | undefined>;
  /** Unregisters `topics` as `sessionID`'s interests at the listener. A failure is warned in the
   *  caller's own words and swallowed: the next pass reads
   *  the registry afresh. The warning is `busUnanswered` when the listener's message bus did not
   *  answer (`isBusUnanswered`), since the listener can have written the record before that, and
   *  `failed` for any other failure. */
  readonly unregisterTopics: (
    sessionID: string,
    topics: readonly string[],
    warnings: { readonly failed: string; readonly busUnanswered: string }
  ) => Promise<void>;

  // — the inbound connection —
  /**
   * The session's one inbound connection, dialled if it has none. A connection this task dialled
   * and was refused while the dial was in flight is closed here and never answered, and so is a
   * step the acquisition itself outran: the task ends rather than acting on the connection.
   */
  readonly connect: () => Promise<SessionInbound>;
  /** Opens `topic`'s subjects; answers whether any were new. */
  readonly subscribe: (topic: string) => Promise<boolean>;
}

/** What a task that decided nothing left behind, for the caller that has to say so. */
interface TaskRefusal {
  /**
   * Whether the listener granted this task's claim after its session had moved, so the role is
   * held here under the session the task acted for. The caller tells the model which of the two
   * happened: a claim that never landed, or one the next pass carries or hands back.
   */
  readonly claimAdopted: boolean;
}

/** The module's surface: the two queues, the named operations, and the readers of its state. */
export interface RegistrationState {
  /**
   * This extension instance's client instance id, minted here so an ACP host running several
   * top-level sessions in one process gives each its own. Sent on every registration and on the
   * shutdown deregistration, where the listener compares it.
   */
  readonly clientInstanceID: string;
  /** Reconcile passes: one at a time, each converging this process to the host's session. */
  readonly passes: TaskQueue<TaskHandle, TaskRefusal>;
  /**
   * Role moves: the model's `envoy_role_set`, a plugin's claim, and each pass's own settle. Each
   * step of one refuses once the session's stop has begun (`RegistrationStateDeps.stopping`); a
   * pass reads the stop itself.
   */
  readonly roleMoves: TaskQueue<TaskHandle, TaskRefusal>;

  // — named operations, made on purpose without a live task —
  /**
   * Registers the session at the listener, outside any queue. Every hold this process abandoned
   * under that session becomes a pending hand-back under the same key first, because the
   * registration moves the session's `last_seen` at the listener even when its answer is lost. On
   * the answer, that session is the one registered and the registration clock is now.
   */
  readonly refreshRegistration: (input: RegistrationInput) => Promise<SubscribeResult>;
  /** Whether a registration this process made for `sessionID` was accepted less than `ms` ago. A
   *  registration accepted for another session, or none at all, reads as older than any age. */
  readonly registrationFresherThan: (sessionID: string, ms: number) => boolean;
  /** Deregisters the session at the listener, carrying this instance's client instance id so a
   *  successor instance's registration under the same id is left alone; within `timeoutMs`,
   *  where the caller has only that long (a shutdown's budget). */
  readonly deregister: (sessionID: string, timeoutMs?: number) => Promise<void>;
  /**
   * Hands back at the listener every role hold this process still has there, for a session that
   * is stopping: the role it holds, under the session id the listener names for it, and each hold
   * it gave up and has not handed back yet (`HandBackLedger.handBackAll`). Each release carries
   * this instance's client instance id, so the listener frees only the claims this process made.
   * A release that fails is warned about on `log`, the stop's own writer, and not on the module's
   * `RegistrationStateDeps.log`, which the host's tell gate holds once the stop has begun. It
   * runs outside the queues and changes no state: nothing runs after a stop. A role move still
   * running refuses its next step (`RegistrationStateDeps.stopping`), so this does not wait for
   * one.
   */
  readonly handBackAllHolds: (log: RegistrationStateLog) => Promise<void>;
  /** Forgets the role the listener refused to this session, so a pass may claim it again. */
  readonly clearRefusal: (sessionID: string) => void;
  /**
   * Ends, as a session event lands, the role this process holds when the host session's own
   * records rule it out: a recorded release, another role, or no record while the hold is not
   * under `host` or a session `host` continues (`lineage`). The hold becomes a pending hand-back
   * under the id it was held as.
   */
  readonly endUnwantedRole: (
    host: string,
    recordedRole: string | null | undefined,
    lineage: readonly string[]
  ) => void;
  /** Marks subjects as taken only while this session holds `role`; answers the ones newly marked.
   */
  readonly markRoleBoundSubjects: (subjects: readonly string[], role: string) => readonly string[];
  /** Unmarks subjects whose subscription never opened. */
  readonly unmarkRoleBoundSubjects: (subjects: readonly string[]) => void;
  /** Records a claim the listener granted after its task was abandoned: the hold is real at the
   *  listener, so the next pass hands it back. */
  readonly recordClaimAfterDeadline: (topic: string, sessionID: string) => void;
  /**
   * Records a claim the listener granted after its task's session moved: the new hold is this
   * process's own under the id the listener names, and an earlier role ends and is handed back —
   * a hand-back the listener refused is abandoned, so the pass behind this process's next
   * registration of that hold's session sends it again.
   */
  readonly adoptClaimAfterSessionMoved: (
    topic: string,
    sessionID: string,
    previousTopic: string | undefined,
    previousHoldID: string | undefined
  ) => Promise<void>;

  // — readers —
  /** The role this session holds, if any. */
  readonly heldRole: () => string | undefined;
  /** Whether `topic` is the role topic this session holds. */
  readonly holdsRoleTopic: (topic: string) => boolean;
  /** The session id the listener names for the role this session holds. */
  readonly holdSessionID: () => string | undefined;
  /** Whether `subject` is taken only while this session holds its role. */
  readonly isRoleBound: (subject: string) => boolean;
  /** Every role-bound subject, in the order they were marked. */
  readonly roleBoundSubjects: () => readonly string[];
  /** Whether `topic` serves the role this session holds, and how many roles had ended, at once. */
  readonly roleSnapshot: (topic: string) => RoleSnapshot;
  /** The role the listener refused to this session's automatic claim, if any. */
  readonly refusalFor: (sessionID: string) => string | undefined;
  /** How many roles have ended, which a role-bound subscribe compares against. */
  readonly roleEndCount: () => number;
  /** Whether the hold came back from a hand-back rather than from a claim the listener granted. */
  readonly holdIsUnconfirmed: () => boolean;
  /** The session the last pass converged to in full. */
  readonly establishedFor: () => string;
  /** The session whose agent subject, stream watch and registered interests a pass opened. */
  readonly inboundOpenSession: () => string;
  /** The session whose role the last pass decided. */
  readonly roleSettledSession: () => string;
}

/**
 * The registration state, which `createRegistrationState` owns and hands nothing else: the one
 * role hold, the refusals and notice subjects of the role, and the sessions a pass converged. The
 * named operations and a queued task's handle write it.
 */
interface RegistrationRecord {
  // The one role this process holds at the listener, and the session id the listener names for
  // it, which a pass that keeps the role for another session supersedes by naming it.
  claimedRoleTopic: string | undefined;
  claimedRoleSessionID: string | undefined;
  // Whether that hold came back from a hand-back a session event left rather than from a claim the
  // listener granted: nothing has checked it at the listener since, so a pass claims it softly,
  // naming it, before trusting it.
  holdUnconfirmed: boolean;
  // Per session, a role the listener refused to that session's automatic claim because another
  // live session holds it: that holder is the newer one, so no pass claims the role for that
  // session again until a session event lands on it or the process restarts. A refusal naming
  // this session's own id is never one of them: that is another process's standing claim under
  // this id, which the listener grants the moment it stops renewing it, so neither `claimRole`
  // nor `reassertRole` records it and the next pass asks again.
  readonly refusedRoles: Map<string, string>;
  // The role a plugin last opened role-bound notices for: a pass that claims that role back after
  // a role end closed them says they stay closed until that plugin claims it again.
  roleNoticesFor: string | undefined;
  // Notice subjects this session takes only while it holds `claimedRoleTopic`. They are never
  // registered with the listener, so a resumed process cannot recover them: the role's claim is
  // their only source, and a role end closes them.
  readonly roleNoticeSubjects: Set<string>;
  // The session whose agent subject, registration and Dispatch binding the last pass converged to.
  establishedSession: string;
  // The session whose role the last pass decided.
  roleSettledFor: string;
  // The sessions whose agent subject this process may have open.
  readonly agentSessions: Set<string>;
  // The session whose agent subject, stream watch and registered interests a pass opened in full.
  inboundOpenFor: string;
}

export function createRegistrationState(deps: RegistrationStateDeps): RegistrationState {
  const clientInstanceID = newClientInstanceID();

  // The role hold, the refusals and notice subjects of the role, and the sessions a pass
  // converged, which a queued task's handle writes too (`RegistrationRecord`).
  const state: RegistrationRecord = {
    claimedRoleTopic: undefined,
    claimedRoleSessionID: undefined,
    holdUnconfirmed: false,
    refusedRoles: new Map<string, string>(),
    roleNoticesFor: undefined,
    roleNoticeSubjects: new Set<string>(),
    establishedSession: "",
    roleSettledFor: "",
    agentSessions: new Set<string>(),
    inboundOpenFor: "",
  };
  // The holds this process gave up and still owes the listener: both books, their keys, the
  // release itself and the reading of a refused claim against an abandoned hold.
  const holds = createHandBackLedger({
    client: deps.client,
    clientInstanceID,
    warn: (message, context) => deps.log.warn(message, context),
  });
  // Counts role ends, so a role-bound subscription still opening when its role ended can tell.
  let roleEnds = 0;
  // When the listener last accepted a registration, and the session it was accepted for. A
  // registration accepted for an earlier session says nothing about the one running now, so it
  // reads as no registration at all; a session that never had one reads as older than any age.
  let registeredFor = "";
  let registeredAt = -Infinity;

  const heldRole = (): string | undefined =>
    state.claimedRoleTopic?.slice(ROLE_TOPIC_PREFIX.length);
  const isRoleBound = (subject: string): boolean => state.roleNoticeSubjects.has(subject);

  // The session stops holding its role: close the notice subjects it took for it, so a holder
  // another session replaced stops taking the role's wakes, and take them out of the listener's
  // registry with them.
  const endRole = (): void => {
    const held = state.claimedRoleSessionID ?? "";
    const closed = [...state.roleNoticeSubjects];
    state.claimedRoleTopic = undefined;
    state.claimedRoleSessionID = undefined;
    roleEnds += 1;
    state.roleNoticeSubjects.clear();
    if (closed.length > 0) deps.closeSubjects(held, closed);
  };

  const refreshRegistration = (input: RegistrationInput): Promise<SubscribeResult> => {
    // Before the call, because the registration moves the session's `last_seen` at the listener
    // even when its answer is lost: a hold of that session this process abandoned is its own
    // again, and the pass behind this registration sends the release.
    holds.adoptFor(input.sessionID);
    return deps
      .client()
      .subscribe({ ...input, clientInstanceID })
      .then((result) => {
        registeredFor = input.sessionID;
        registeredAt = Date.now();
        return result;
      });
  };

  /**
   * Takes the hold of `topic` under `sessionID`: the role this session held, when the hold moves
   * to another role, ends first and its own hold is handed back at the listener under the id it
   * was held as (a fork's parent, until a pass carried it). `recordClaim` runs between the two,
   * where the claim's durable transcript record belongs: the hold is this process's by then, and
   * the hand-back that follows can fail or outrun the task. A hand-back the listener refused is
   * abandoned rather than thrown: the claim this move carries landed, and the role it superseded
   * is sent again by the pass behind this process's next registration of that hold's session.
   */
  const moveHold = async (
    topic: string,
    sessionID: string,
    previousTopic: string | undefined,
    previousHoldID: string | undefined,
    recordClaim?: () => void
  ): Promise<void> => {
    const movedOff = previousTopic === topic ? undefined : previousTopic;
    if (movedOff !== undefined) endRole();
    state.claimedRoleTopic = topic;
    state.claimedRoleSessionID = sessionID;
    recordClaim?.();
    if (movedOff !== undefined && previousHoldID) {
      await holds.handBackOrAbandon(movedOff, previousHoldID, {
        topic: movedOff,
        sessionID: previousHoldID,
      });
    }
  };

  /**
   * The handle builder of both queues: one task's handle, and what the task left behind, which the
   * queue hands `onRefused` when a step refused the task. `live` and `deadlineAt` come from the
   * queue. `stopped`: whether the task refuses its steps once the session's stop began.
   */
  const createHandle = (
    live: () => boolean,
    deadlineAt: number,
    stopped: () => boolean
  ): RunningTask<TaskHandle, TaskRefusal> => {
    let bound: string | undefined;
    // Whether this task may still write: no step of it has been refused yet, its deadline has not
    // fired, the session's stop has not begun where the task is a role move (`stopped`) and, once
    // bound, its session is still the open one. The one question every step below asks, asked
    // here alone.
    //
    // The first refusal latches. A session can close and open again — a switch away and back —
    // while a task waits on I/O, and a task that caught its refusal would otherwise pass the next
    // step and go on writing for a pass that replaced it.
    let refused = false;
    const refusal = { claimAdopted: false };
    const acting = (): boolean => {
      if (refused) return false;
      if (!stopped() && live() && (bound === undefined || deps.sessionOpen(bound))) return true;
      refused = true;
      return false;
    };
    const refuse = (message: string): never => {
      refused = true;
      return refuseStep(message);
    };
    const guard = (): void => {
      if (acting()) return;
      refuse(
        stopped()
          ? "the role move refused a step: this session's stop has begun"
          : live()
            ? `the task acting for session ${bound} refused a step: that session is no longer the open one`
            : "the task refused a step: it was abandoned at its deadline"
      );
    };
    // The stop read after a wait whose outcome a role move acts on whatever else became of it: a
    // hand-back that landed, a claim the listener answered. Once the session's stop has begun,
    // the move ends here, and nothing it would do next is done.
    const refuseOnceStopping = (message: string): void => {
      if (stopped()) refuse(message);
    };
    const actingFor = (): string => {
      if (bound === undefined) {
        throw new Error("this queued task has not named the session it acts for (actFor)");
      }
      return bound;
    };

    const handBackHeldRole = async (tellUser: boolean): Promise<RoleEnded | undefined> => {
      guard();
      const role = heldRole();
      const holdID = state.claimedRoleSessionID;
      const topic = state.claimedRoleTopic;
      if (role === undefined || topic === undefined) return undefined;
      const pluginHeld = state.roleNoticeSubjects.size > 0;
      endRole();
      if (holdID !== undefined && holdID !== "") {
        await holds.handBackOrAbandon(topic, holdID, { role, sessionID: holdID });
      }
      // The end has landed, so the pane hears about it whatever became of this task, unless the
      // session's stop began meanwhile: a stopping session is told nothing.
      refuseOnceStopping(`the role move ended role ${role} as the session's stop began`);
      if (tellUser) {
        const host = bound ?? "";
        deps.log.warn("envoy: role ended after a move with no session event", {
          role,
          sessionID: host,
        });
        deps.notify(
          pluginHeld
            ? `envoy: role ${role} ended: the host moved to session ${host} with no session event, and that session's records do not name it; the plugin that opened its notices claims it again on its next session switch or restart`
            : `envoy: role ${role} ended: the host moved to session ${host} with no session event, and that session's records do not name it; if this session should hold it, run envoy_role_set`,
          "warning"
        );
      }
      return { role, pluginHeld };
    };

    const settleEndedHolds = async (wanted: string | undefined): Promise<boolean> => {
      guard();
      const restored = await holds.settleEnded(acting, (hold) => {
        // A hold is adopted only for the session this process still registers: one under a
        // session it has left may be another process's now, and a claim naming it would take
        // that live process's role.
        if (
          wanted === undefined ||
          hold.topic !== ROLE_TOPIC_PREFIX + wanted ||
          !deps.sessionOpen(hold.id)
        ) {
          return false;
        }
        state.claimedRoleTopic = hold.topic;
        state.claimedRoleSessionID = hold.id;
        state.holdUnconfirmed = true;
        return true;
      });
      // Refused during the last hand-back, or at the break inside it: the restore this task made
      // is the replacing one's to decide.
      guard();
      return restored;
    };

    const claimRole = async (
      role: string,
      options: { readonly soft?: boolean; readonly previousSessionID?: string } = {}
    ): Promise<boolean> => {
      const id = actingFor();
      guard();
      const topic = ROLE_TOPIC_PREFIX + role;
      const previousTopic = state.claimedRoleTopic;
      const previousHoldID = state.claimedRoleSessionID;
      const result = await deps.client().setRole({
        sessionID: id,
        role,
        soft: options.soft,
        previousSessionID: options.previousSessionID,
        // The process making the claim: a role already held under this very session id is the
        // listener's to decide on, and it decides on this id — granted where the standing claim
        // names this same process, refused where another process still renews it.
        clientInstanceID,
      });
      // The session's stop began while the claim was in flight. The stop has handed back what
      // this process held and deletes the session's record next, so nothing of the claim is kept:
      // no hold, no transcript record, no word. A claim the listener granted all the same waits
      // out the live window, as one the stop's release did not reach does.
      refuseOnceStopping(
        `the role move that claimed ${role} for session ${id} ended: the session's stop began while the claim was in flight`
      );
      if (!live()) {
        // The listener may have granted the claim while this task was abandoned. Nothing of it goes
        // in the transcript — the task that replaced this one owns the role from here — but the
        // hold is real at the listener, so leave it pending for the next pass to hand back.
        if (result.claimed) holds.recordEndedHold(topic, id);
        refuse(
          `the role move that claimed ${role} for session ${id} was abandoned at its deadline while the claim was in flight`
        );
      }
      if (result.claimed) {
        state.refusedRoles.delete(id);
        state.holdUnconfirmed = false;
        // The listener names this claim's session for the role now, so no hold of it this process
        // gave up is left to hand back or wait on (one under this same id would end the new hold).
        holds.forgetHoldsOf(topic);
      }
      if (!deps.sessionOpen(id)) {
        // The session moved while the claim was in flight. A hold that landed is kept as this
        // process's own, under the id the listener names, and the pass the move queues converges
        // it. Handing it back here would lose a role a fork should carry. The caller is told
        // which of the two happened through the refusal's `claimAdopted`.
        if (result.claimed) {
          await moveHold(topic, id, previousTopic, previousHoldID);
          refusal.claimAdopted = true;
        }
        refuse(`the session moved on while its claim of role ${role} was in flight`);
      }
      if (!result.claimed) {
        endRole();
        // A refusal naming this task's own session id is the standing claim of another process
        // under that id, which the listener grants the moment that process stops renewing it.
        // Nothing says another session took the role, so no refusal is latched and nothing is
        // said — the rule `reassertRole` keeps for the same state. A latch would make
        // `desiredRole` want no role until a session event landed, and the line would be written
        // out again on every heartbeat for as long as that other process lived; the next pass
        // claims again instead.
        if (result.holder === id) return false;
        // Only a soft claim is refused. A holder that is this process's own abandoned hold is
        // waited out, since the listener reaps it; any other is the newer holder.
        const waitingOnReap = await holds.observeAbandonedHold(topic, role, result.holder, live);
        // Read across a wait too: once the session's stop has begun, nothing is recorded or said.
        refuseOnceStopping(
          `the role move that claimed ${role} for session ${id} ended: the session's stop began before the refusal was recorded`
        );
        // The hold was read across a wait: this task may have been refused since, and the claim
        // that replaced it may already have cleared the refusal this one is about to record.
        // Nothing is written then, and the line says so.
        const notRecorded = !live()
          ? "the move was abandoned at its deadline before the refusal was recorded"
          : deps.sessionOpen(id)
            ? undefined
            : "the session moved on before the refusal was recorded";
        if (notRecorded === undefined && !waitingOnReap) state.refusedRoles.set(id, role);
        deps.log.warn(
          notRecorded !== undefined
            ? `envoy: role held by another live session; ${notRecorded}`
            : waitingOnReap
              ? "envoy: role still held by a session this process left; claiming again once the listener reaps it"
              : "envoy: role held by another live session; not reclaimed",
          { role, sessionID: id, holder: result.holder }
        );
        return false;
      }
      // The transcript is the one thing `omp --resume` guarantees, so it is the durable record of
      // the claim: the listener releases a role claim one session TTL after heartbeats stop, and
      // this process's memory dies with it. The record names the session claimed for and this
      // process, so a reader tells it from a record another process wrote on the same transcript.
      await moveHold(topic, id, previousTopic, previousHoldID, () =>
        deps.appendRoleClaimEntry({ role, sessionID: id, clientInstanceID })
      );
      // The stop began while the role this claim moved off was handed back. The stop handed back
      // the new hold with every other, so the move reports no role the session no longer holds.
      refuseOnceStopping(
        `the role move that claimed ${role} for session ${id} ended: the session's stop began as the claim landed`
      );
      return true;
    };

    const reassertRole = async (role: string, afterOutage: boolean): Promise<RoleReassertion> => {
      const id = actingFor();
      guard();
      const topic = ROLE_TOPIC_PREFIX + role;
      const client = deps.client();
      // Whether this process still holds the role the re-assertion was decided for: a session
      // event can end it while the listener is read or claimed, and the task that ends it owns
      // the role from there.
      const stillHolds = (): boolean => state.claimedRoleTopic === topic;
      const holding = await client.getRole(role).then(
        (info) => info,
        (error: unknown) => {
          // 404 is the listener's answer for "no live holder", not a failure.
          if (error instanceof EnvoyApiError && error.details.status === 404) return undefined;
          throw error;
        }
      );
      guard();
      if (!stillHolds()) return { decided: false };
      // This session *and* this process (`roleHeldBy`, the one rule both clients read): a claim
      // standing under this very session id that another process made is not this one's to
      // renew, while a holder naming no process at all is read as this one's.
      if (roleHeldBy(holding, id, clientInstanceID)) {
        return afterOutage ? { decided: true, regain: "reregistered" } : { decided: true };
      }
      const result = await client.setRole({ sessionID: id, role, soft: true, clientInstanceID });
      guard();
      if (!stillHolds()) return { decided: false };
      if (!result.claimed) {
        if (result.holder === id) {
          // The standing claim of another process under this session's id, which the listener
          // grants the moment that process stops renewing it. Nothing says another session took
          // the role, so the role stays held here and the next heartbeat asks again: no
          // `endRole`, and no refusal latched either — a latch would make `desiredRole` want no
          // role on the next pass and hand this one back, undoing what this branch keeps. The
          // pane is told nothing, because nothing changed for it, and the log is left alone too:
          // the state is normal, self-resolving, and would otherwise be written out again on
          // every heartbeat for as long as the other process lives. `claimRole` keeps the same
          // rule for the same state.
          return { decided: true };
        }
        endRole();
        state.refusedRoles.set(id, role);
        deps.log.warn("envoy: role re-assertion refused; held by another live session", {
          role,
          sessionID: id,
          holder: result.holder,
        });
        deps.notify(
          `envoy: role ${role} is now held by session ${result.holder}; this session no longer holds it`,
          "warning"
        );
        return { decided: true };
      }
      deps.log.warn("envoy: role re-asserted after the listener lost the claim", {
        role,
        sessionID: id,
        previousHolder: holding?.holder ?? null,
      });
      return { decided: true, regain: "reclaimed" };
    };

    // The one dial a queued task makes, its own: a connection opened for a task that was refused
    // while the dial was in flight is closed here rather than left dangling, and the task ends.
    // Private: a task asks for the session's connection, not for a dial.
    const dial = async (): Promise<SessionInbound> => {
      guard();
      const dialled = await deps.dial({ deadlineAt, stopped: () => !acting() });
      if (!acting()) {
        await dialled.close().catch(() => {});
        refuse(
          live()
            ? `the task acting for session ${bound} opened its ${dialled.name} for a session that is no longer the open one; the connection it opened was closed`
            : `the task that opened the ${dialled.name} was abandoned at its deadline while it was being opened; the connection it opened was closed`
        );
      }
      return dialled;
    };

    // The whole acquisition of the session's connection is the step, not the dial inside it: the
    // host waits again after a dial another caller's connection beat, closing this one and
    // answering with theirs, and a deadline or a session move during that wait would otherwise go
    // unseen — the task would open subjects and replace the stream control for a session it no
    // longer decides for. A connection this task did not open is left alone: it is the session's.
    const connect = async (): Promise<SessionInbound> => {
      guard();
      const connected = await deps.connect(dial);
      if (!acting()) {
        refuse(
          live()
            ? `the task acting for session ${bound} took its ${connected.name} for a session that is no longer the open one`
            : `the task that took the ${connected.name} was abandoned at its deadline while the connection was made`
        );
      }
      return connected;
    };

    const handle: TaskHandle = {
      actFor: (id) => {
        if (bound !== undefined) {
          throw new Error(
            `this queued task already acts for session ${bound} and cannot act for ${id}`
          );
        }
        bound = id;
      },
      check: guard,
      register: (input) => {
        guard();
        // Timed whatever becomes of this task: the heartbeat's stall test reads one clock, and a
        // registration the listener accepted moved the session's `last_seen` whoever asked for it.
        return refreshRegistration(input);
      },
      markEstablished: (id) => {
        guard();
        state.establishedSession = id;
      },
      forgetOtherAgentSessions: (host) => {
        guard();
        const closed: string[] = [];
        for (const other of state.agentSessions) {
          if (other === host) continue;
          closed.push(other);
          state.agentSessions.delete(other);
          if (state.inboundOpenFor === other) state.inboundOpenFor = "";
        }
        return closed;
      },
      markAgentSessionOpen: (id) => {
        guard();
        state.agentSessions.add(id);
      },
      markInboundOpen: (id) => {
        guard();
        state.inboundOpenFor = id;
      },
      markRoleSettled: (id) => {
        guard();
        state.roleSettledFor = id;
      },
      handBackHeldRole,
      settleEndedHolds,
      claimRole,
      reassertRole,
      noteNoticesClosed: (role) => {
        guard();
        if (state.roleNoticesFor !== role || state.roleNoticeSubjects.size > 0) return;
        state.roleNoticesFor = undefined;
        deps.notify(
          `envoy: role ${role} is held again, but the notices a plugin opened for it stay closed until that plugin claims it again on its next session switch or restart`,
          "warning"
        );
      },
      readRoleHolder: async (role) => {
        guard();
        return deps
          .client()
          .getRole(role)
          .then(
            (info) => info.holder,
            () => undefined
          );
      },
      readInterest: async (sessionID) => {
        guard();
        return deps
          .client()
          .getInterest(sessionID)
          .catch(() => undefined);
      },
      unregisterTopics: async (sessionID, topics, warnings) => {
        guard();
        await deps
          .client()
          .unsubscribe({ sessionID, topics: [...topics] })
          .catch((error: unknown) =>
            deps.log.warn(isBusUnanswered(error) ? warnings.busUnanswered : warnings.failed, {
              sessionID,
              topics,
              error: messageFor(error),
            })
          );
      },
      connect,
      subscribe: async (topic) => {
        guard();
        // `connect` is this task's own step, so a subscribe whose connection outran the task
        // opens no subject: the acquisition refuses and this call throws out of the adapter.
        const opened = await deps.subscribeTopic(topic, connect);
        guard();
        return opened;
      },
    };

    return { handle, refusal };
  };

  return {
    clientInstanceID,
    // A pass reads the stop itself, once its registration settled (the extension's
    // `reconcileNow`); a role move refuses each step once the stop began.
    passes: createQueue(deps.deadlineMs, (live, deadlineAt) =>
      createHandle(live, deadlineAt, () => false)
    ),
    roleMoves: createQueue(deps.deadlineMs, (live, deadlineAt) =>
      createHandle(live, deadlineAt, deps.stopping)
    ),

    refreshRegistration,
    registrationFresherThan: (sessionID, ms) =>
      registeredFor === sessionID && Date.now() - registeredAt < ms,
    deregister: async (sessionID, timeoutMs) => {
      await deps.client().unregisterSession({
        sessionID,
        clientInstanceID,
        ...(timeoutMs === undefined ? {} : { timeoutMs }),
      });
    },
    handBackAllHolds: (log) =>
      holds.handBackAll(
        state.claimedRoleTopic === undefined || !state.claimedRoleSessionID
          ? undefined
          : { topic: state.claimedRoleTopic, id: state.claimedRoleSessionID },
        (message, context) => log.warn(message, context)
      ),
    clearRefusal: (sessionID) => {
      state.refusedRoles.delete(sessionID);
    },
    endUnwantedRole: (host, recordedRole, lineage) => {
      const held = heldRole();
      const topic = state.claimedRoleTopic;
      const holdID = state.claimedRoleSessionID;
      if (held === undefined || topic === undefined || holdID === undefined) return;
      const carried = holdID === host || lineage.includes(holdID);
      const wanted = recordedRole === undefined ? carried : recordedRole === held;
      if (wanted) return;
      endRole();
      if (holdID !== "") holds.recordEndedHold(topic, holdID);
    },
    markRoleBoundSubjects: (subjects, role) => {
      const marked: string[] = [];
      for (const subject of subjects) {
        if (state.roleNoticeSubjects.has(subject)) continue;
        state.roleNoticeSubjects.add(subject);
        marked.push(subject);
      }
      state.roleNoticesFor = role;
      return marked;
    },
    unmarkRoleBoundSubjects: (subjects) => {
      for (const subject of subjects) state.roleNoticeSubjects.delete(subject);
    },
    recordClaimAfterDeadline: holds.recordEndedHold,
    adoptClaimAfterSessionMoved: moveHold,

    heldRole,
    holdsRoleTopic: (topic) => state.claimedRoleTopic === topic,
    holdSessionID: () => state.claimedRoleSessionID,
    isRoleBound,
    roleBoundSubjects: () => [...state.roleNoticeSubjects],
    roleSnapshot: (topic) => ({
      roleBound: expandSubscriptionTopics([topic]).some(isRoleBound),
      endsBefore: roleEnds,
    }),
    refusalFor: (sessionID) => state.refusedRoles.get(sessionID),
    roleEndCount: () => roleEnds,
    holdIsUnconfirmed: () => state.holdUnconfirmed,
    establishedFor: () => state.establishedSession,
    inboundOpenSession: () => state.inboundOpenFor,
    roleSettledSession: () => state.roleSettledFor,
  };
}
