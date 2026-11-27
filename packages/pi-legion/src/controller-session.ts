import {
  controllerToken,
  legionControllerNoticeSubject,
  legionProjectToken,
  slackMentionTopic,
} from "@legion/contracts";
import { daemonContract } from "@legion/contracts/daemon-contract";
import { scrub } from "@legion/contracts/held-secrets";
import type { LegionGrant } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import {
  type ClaimCaller,
  claimCaller,
  LegionDaemonApiError,
  type LegionDaemonClient,
} from "@legion/pi-shared/daemon-client";
import {
  claimRoleAgainOnBusy,
  claimStandsWithoutHandover,
  listenerAskedToClaimAgain,
} from "@legion/pi-shared/legion-claim-core";
import type { CommandContext, PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { claimEnvoyRole, subscribeLegionNotice } from "@legion/pi-shared/role-claim-bridge";
import {
  clearStartupTurn,
  markStartupTurnPending,
  markStartupTurnSent,
} from "@legion/pi-shared/startup-turn";
import { recordBootstrappedSession } from "@legion/pi-shared/subagent-session";
import {
  type ClaimTaskTake,
  type TaskTake,
  type TaskTakeDeps,
  takeClaimTask,
} from "@legion/pi-shared/task-take";
import { logger } from "@oh-my-pi/pi-utils";
import pkg from "../package.json";
import {
  classifySession,
  requiredControllerCapability,
  requiredEnvironment,
  requiredSecret,
} from "./classify";
import { daemonControllerSlackChannels, daemonLaunchedController } from "./slack-channels";
import type { ReportedClaim } from "./turn-outcome";

/** The daemon API contract this build speaks, which the controller's registration states as
 * `pluginContract` (`daemonContract`, read once as the module loads). */
const PLUGIN_CONTRACT = daemonContract(pkg, import.meta.dir);

type PersistedTranscript = (
  context: CommandContext | SessionContext
) => Promise<{ readonly sessionFile: string; readonly agentId: string }>;

/** The daemon-launched controller's take of its claim's task (`@legion/pi-shared/task-take`): the
 * take, its read of the claim's value from the daemon, and the claim it takes for. */
interface Taking extends ClaimTaskTake {
  readonly caller: ClaimCaller;
}

/** How far one claim got, which its caller's notice reports. */
interface ClaimProgress {
  /** The daemon holds this claim's registration, and the session's grants mint with its secret. */
  registered: boolean;
}

/** Who started a claim: the controller's own claim at `session_start` or after a session change
 * (`daemon`), or a command a person typed (`typed`). It decides how long a `503` is waited out. */
type ClaimStarter = "daemon" | "typed";

export interface ControllerSession {
  readonly claim: (context: CommandContext | SessionContext) => Promise<void>;
  readonly handleSessionStart: (context: SessionContext) => Promise<void>;
  readonly handleAgentStart: (context: SessionContext) => void;
  readonly reclaimAfterSessionChange: (context: SessionContext) => Promise<void>;
  readonly startCommand: (context: CommandContext) => Promise<void>;
  readonly isClaimedSession: (sessionID: string) => boolean;
  readonly mintGrant: (sessionID: string) => Promise<LegionGrant>;
  /** The daemon-launched controller's take of its start task (`@legion/pi-shared/task-take`),
   * whose hooks `legion.ts` calls for a run of `sessionID` (`agent_start` and `agent_end`) and for
   * the daemon's refusal of that run's report (`src/turn-outcome.ts`): the first run after its
   * accepted take's text was handed to the idle host is the start task's turn, and its end the
   * report naming that task's number.
   * Undefined for another session than the one the take was made for, and for an operator's
   * controller, which takes no task. */
  readonly take: (sessionID: string) => TaskTake | undefined;
  /** The claim whose turns `sessionID` reports (`src/turn-outcome.ts`): the daemon-launched
   * controller's, for the session its take was made for, so the end of its start task's turn
   * names that task's number, which is what finishes it, and a provider error on it reaches the
   * daemon's provider-error wait. Undefined for any other session, and for an operator's
   * controller, which takes no task. */
  readonly reportedClaim: (sessionID: string) => ReportedClaim | undefined;
}

/** The host widget a session change's re-claim shows its outcome in (`reclaimAfterSessionChange`). */
const CLAIM_WIDGET = "legion-controller-claim";

/** How many tries a person's controller claim makes while the listener answers it 503: four
 * waits of 1, 2, 4 and 8 seconds, then the command fails with the listener's answer. */
export const CONTROLLER_CLAIM_BUSY_ATTEMPTS = 5;

/**
 * The controller `legion controller start` launched for the operator: the controller marker
 * (`LEGION_CONTROLLER=1`) without the boot token of a controller the daemon launched itself. It
 * claims from `/legion-controller-start` and sends its own start message.
 */
function operatorLaunchedController(env: NodeJS.ProcessEnv): boolean {
  return classifySession(env).kind === "controller" && !daemonLaunchedController(env);
}

/**
 * Owns the controller session's identity, transcript, role claim, and grant path. This is
 * deliberately separate from the claim session (`claim-session.ts`) and the extension's
 * tool-routing policy: a controller holds no claim, and its session can change in place.
 */
export function createControllerSession(deps: {
  readonly daemon: () => LegionDaemonClient;
  readonly persistedTranscript: PersistedTranscript;
  readonly pi: PiApi;
  /** An accepted take's text to the host (`TaskTakeDeps.run`). */
  readonly runTask: TaskTakeDeps["run"];
  readonly reportDrop: (
    sessionID: string,
    ...drop: Parameters<NonNullable<TaskTakeDeps["reportDrop"]>>
  ) => Promise<void>;
  readonly exitProcess: (code: number) => never;
}): ControllerSession {
  const { daemon, persistedTranscript, pi } = deps;
  // The session the daemon last registered as the controller and the grant minter for its
  // registration's secret, set as soon as that registration is in: the `legion` tool answers for
  // that session from then on, the start turn included, whatever the rest of the claim does.
  let controllerSessionID: string | undefined;
  let controllerCapability: string | undefined;
  let mintControllerGrant: (() => Promise<LegionGrant>) | undefined;
  // Whether this process has ever sent LEGION_CONTROLLER_START_MESSAGE.
  let startMessageSent = false;
  // Whether the claim that made the last registration went on to hold the role and its topics: a
  // claim that fails after its registration leaves the session registered, with no role or wakes.
  let claimComplete = false;
  // The controller's own transcript as of the last registration, which a session navigation
  // compares to decide whether to claim again; a hand-started takeover records none.
  let controllerTranscript: string | undefined;
  // Every claim, and every `/legion-controller-start` decision, runs after the one before it:
  // `serialised` chains each onto this, and counts the ones queued or running.
  let claimChain: Promise<void> = Promise.resolve();
  let claimsQueued = 0;
  // Set when a claim stops at its registration because the session changed while it ran (a `/new`
  // during the start command's claim): the re-claim that session change queues says so, first, in
  // its own turn (`reclaimAfterSessionChange`), and clears it. The stopped claim says nothing: its
  // notice would be shown while the `/new` is still under way, and the `/new` wipes the screen.
  let startHandedOver = false;
  // The claim widget a session change's re-claim left up (`reclaimAfterSessionChange`): the session
  // it was shown for, and how to take it down, which that session's next run (`handleAgentStart`)
  // or a later claim that succeeds does.
  let claimWidget: { readonly session: string; readonly clear: () => void } | undefined;
  // The session whose start message (or, for the daemon's controller, start task's text) was
  // sent while it was idle and whose run has not started: its next `agent_start` is that start's
  // own run, which may begin before or after the re-claim that sent it shows its widget, so it
  // takes no widget down.
  let startTurnSession: string | undefined;
  // The daemon-launched controller's take of its claim's task.
  let taking: Taking | undefined;

  /**
   * The take of the daemon-launched controller's task for the claim `caller` names, replacing any
   * earlier one: the daemon writes the value of the controller's start task at each ready, and the
   * session takes it as any claim takes its task (`takeClaimTask`): the values the Envoy entry
   * hands over reach it from here on, each new epoch of the session's relay reads the value from
   * the daemon again, and the task-turn mark it keeps names the caller's session. The replaced
   * take is closed: what it was still sending stops, an answer that still arrives for it changes
   * nothing, and its mark is cleared, so a late answer for a registration this claim replaced can
   * neither run a text nor take down the mark of the take that replaces it.
   */
  const takeFor = (client: LegionDaemonClient, caller: ClaimCaller): Taking => {
    taking?.take.close();
    const entry = {
      ...takeClaimTask({
        daemon: client,
        caller,
        run: deps.runTask,
        reportDrop: (...drop) => deps.reportDrop(caller.sessionId, ...drop),
        log: logger,
      }),
      caller,
    };
    taking = entry;
    return entry;
  };
  // The wait notices of the claim running now (`claimWith`): a session change's re-claim holds its
  // notices until it settles, and drops these once its claim landed, so no wait is shown after it.
  const waitNotices = new Set<string>();
  // The errors a bounded role claim gave up with on the listener's 503 (`claimWith`): the one
  // failure of a typed claim the daemon-launched controller stays up for (`claimOnce`). Any other
  // 503, such as one to the controller-topic subscription after the role landed, is not in it.
  const roleClaimGaveUp = new WeakSet<object>();

  const serialised = (task: () => Promise<void>): Promise<void> => {
    claimsQueued += 1;
    const run = claimChain.then(task).finally(() => {
      claimsQueued -= 1;
    });
    claimChain = run.catch(() => undefined);
    return run;
  };

  // The start message sent to an idle `sessionID`, marked as its startup turn right before.
  const sendStartTurn = (sessionID: string, startMessage: string): void | Promise<void> => {
    markStartupTurnSent(sessionID);
    startTurnSession = sessionID;
    return pi.sendUserMessage(startMessage);
  };

  // Oh My Pi dropped the start message sent to `sessionID` before its turn began
  // (`PiApi.sendUserMessage`): nothing of it ran, so it is still to send, and the next claim
  // (`/legion-claim-controller`, or a session change's re-claim) sends it. The startup mark the send
  // set goes with it, so the Envoy entry holds no card back for a start that is not coming.
  const startMessageDropped = (sessionID: string, error: unknown): void => {
    logger.error(
      `[legion] Oh My Pi dropped the controller's start message before its turn began; run /legion-claim-controller to send it again: ${messageFor(error)}`
    );
    startMessageSent = false;
    clearStartupTurn(sessionID);
    if (startTurnSession === sessionID) startTurnSession = undefined;
  };

  /**
   * Makes the context's session the controller: it registers on the claim route with the
   * capability `legion controller start` fetched in place of a boot token, then claims the role
   * token the registration names (`legion-<project>-controller`), then follows that project's
   * controller topic (`notifications.legion.<project>.controller`, `notify.ControllerTopic` in
   * the daemon, which lists what it carries) for as long as it holds the role: a later `legion
   * controller start` takes the role, and Envoy drops the topic from this session's interest row
   * and its consumer's filter at its next heartbeat. The project is `LEGION_PROJECT`, and it must
   * be the daemon's, or the topic would be one nobody publishes on. Both are settled before
   * registering, since a registration replaces the running controller's session and secret: an unset
   * `LEGION_PROJECT` stops the claim, and so does one whose controller role is not that of the
   * project `GET /healthz` names (`legionProjectToken`, the rule the daemon applies to its
   * own). The registration's claim token is compared once more after it, the daemon's own answer.
   * Once the registration is in, the session is the controller to the `legion` tool: its grants
   * are minted with the registration's secret from then on, before the role claim, so the start
   * turn's `read_state` and `set_status` work while the rest of the claim runs, and after it fails.
   *
   * The operator-launched controller (`operatorLaunchedController`) reads its
   * LEGION_CONTROLLER_START_MESSAGE before any of that — a missing value refuses here, before
   * registering replaces the running controller's session and claims its role, rather than
   * leaving a half-claimed controller behind a refusal found only after. That message runs the
   * skill's start procedure as the session's first turn, with nothing typed, ahead of any Envoy
   * card or Legion notice that would otherwise race it for the session's one first-turn slot
   * (LEGION-392). It is sent once the registration is in, right before the role claim, so before
   * anything can reach the session on the role's lane or the controller topic. Not after the role
   * claim: the listener forwards a card on the role's lane as soon as it records the role, and
   * that card can reach the session before the role claim's own answer does, which on a pi-envoy
   * that steers every card would start the card's turn first. A role claim refused after the send
   * leaves the start message sent, its turn holding the registration, and the claim's refusal is
   * the operator's to see.
   *
   * That order holds only outside Oh My Pi's startup, so this controller claims from
   * `/legion-controller-start` (`startCommand`), never from `session_start`. Oh My Pi holds every
   * send a `session_start` handler makes and drains them all at the end of its startup, while an
   * Envoy card that finds the session idle starts its own turn: a role claim made during startup
   * lets a card on the role's lane, or a notice on the controller topic, start its turn ahead of
   * the held start message, and a turn started there runs inside the startup wait, before the TUI
   * shows it and before any input is read. `legion controller start` puts the command on Oh My Pi's
   * command line, which Oh My Pi runs after its startup and once the TUI listens, so the role's
   * lane and the controller topic open only once the start message is queued. A claim from
   * `/legion-claim-controller`, or a re-claim after a session change, sends a message still
   * unsent the same way. A send to an idle session marks the registered session in the Envoy
   * plugin interface's `startupTurn` right before it; a send queued behind a turn the operator
   * already started marks nothing, since that run, the session's first, has already started. A
   * claim that finds, once registered, that the session changed since it began (a `/new` while it
   * ran) sends nothing and stops there, unfinished, saying nothing: the session change's re-claim
   * registers the new session, sends the start message there, and tells the operator so
   * (`startHandedOver`). `startMessageSent` keeps this process from sending it twice: a retried
   * claim after a failure past the send, and a later `/legion-claim-controller` in the same
   * session, send nothing more. A start message Oh My Pi dropped before its turn began ran nothing,
   * so it is still to send (`startMessageDropped`): the next claim sends it.
   *
   * The controller topic is no live wake: this session receives what its own JetStream consumer
   * delivers and nothing else (INFRA-771), and the registration above puts the topic in that
   * consumer's filter, so a notice published while this session was away is delivered when it
   * registers again. A notice published before any session followed the topic reaches none — a
   * consumer new to it starts at the stream's end — and the controller skill reads the daemon's
   * state (the `legion` tool's `read_state`) and Dispatch's triage listing at boot for what it
   * missed. Its grants are minted with the registration's own secret, which a later
   * `legion controller start` revokes. Nothing re-runs on a role regain: the Envoy heartbeat keeps
   * the role itself, and the listener relays the claim's task value again to the session that
   * holds the role once more. A refusal is logged and
   * propagates without exiting — the operator started this session and reads it; a refused
   * capability was replaced by a later start. The transcript is reported on every claim, a
   * takeover's included, because the route requires one; the daemon records only the session.
   *
   * A controller the daemon launched itself (`controller: daemon`) is the controller's own session
   * (`LEGION_CONTROLLER=1`) carrying its launch's boot token (`LEGION_BOOT_TOKEN_FILE`) in place of
   * a capability: it claims at `session_start` (`handleSessionStart`), registers with that token,
   * as every pane does, reports ready on `claims/ready`, marking `startupTurn` as sent right before
   * that ready, at which the daemon writes the value of its start task, and reads that value
   * (`claims/task`); only then does it claim the role, follow the controller topic and subscribe
   * to each Slack channel its launch names (`daemonControllerSlackChannels`) for as long as it
   * holds the role, so no card on the role's lane, no notice and no Slack mention can take the
   * start task's turn (the plan's decision 15). The daemon takes the ready before the role claim:
   * its ready route asks nothing of the Envoy role (`packages/daemon/internal/api/claims.go`).
   * From its registration until that read has answered, its take holds the task-turn mark
   * whatever value it holds (`TaskTake.awaitRead`), since its agent subject is open from its start
   * and a card sent to the session itself must not take that turn either. The session takes that
   * task as any claim takes its task, the take's text is its first turn, and that turn's end is
   * reported naming the task's number, as any claim's is (`reportedClaim`, `runEnded`). A failed
   * read of the start task's value is logged and the claim goes on, since the daemon's next write
   * of the value reaches the session as well; any other step of its claim that fails exits Oh My
   * Pi, so the daemon relaunches it, as a pane's failed boot
   * does. A root architect's or phase worker's pane carries a boot token too, its own claim's:
   * `/legion-claim-controller` run there is a takeover by hand like any other, which needs the
   * capability and stops before any daemon call without it.
   *
   * One claim runs at a time, in the order they were asked for: each registration replaces the
   * controller secret the daemon holds, so two claims racing could leave this session holding the
   * secret a later registration replaced, and every controller grant refused. Oh My Pi reads the
   * terminal while `/legion-controller-start`'s claim runs, so `/legion-claim-controller` or the
   * command itself can be typed before it ends. A claim that has to wait says so once, since the
   * daemon client sets no request timeout and a hung claim would otherwise hold it silently.
   */
  const claim = (context: CommandContext | SessionContext): Promise<void> =>
    queueClaim(context, { registered: false }, "typed");

  // Says so when the claim or command about to queue has to wait for one already running: the
  // daemon client sets no request timeout, so a hung claim would otherwise hold it silently.
  const noteWait = (context: CommandContext | SessionContext): void => {
    if (claimsQueued > 0) {
      context.ui.notify(
        "legion: waiting for the controller claim already running to finish.",
        "info"
      );
    }
  };

  const queueClaim = (
    context: CommandContext | SessionContext,
    progress: ClaimProgress,
    startedBy: ClaimStarter
  ): Promise<void> => {
    noteWait(context);
    return serialised(() => claimOnce(context, progress, startedBy));
  };

  // A claim of the daemon-launched controller that fails exits Oh My Pi, so the daemon launches it
  // again, except a command a person typed there whose role claim gave up on the listener's 503
  // after its counted tries (`claimWith`, `roleClaimGaveUp`): that command fails where the person
  // reads it, and the process, which still holds what it held, stays. Any other failure of a typed
  // claim, a 503 to its controller-topic subscription after the role landed included, exits as
  // the daemon's own claim does, so a controller never stays up without its topics.
  const claimOnce = async (
    context: CommandContext | SessionContext,
    progress: ClaimProgress,
    startedBy: ClaimStarter
  ): Promise<void> => {
    const supervised = daemonLaunchedController(process.env);
    if (!supervised) return claimWith(context, false, progress, startedBy);
    try {
      await claimWith(context, true, progress, startedBy);
    } catch (error) {
      if (startedBy === "typed" && error instanceof Error && roleClaimGaveUp.has(error))
        throw error;
      logger.error(
        `[legion] the daemon-launched controller's claim failed; exiting so the daemon launches it again: ${messageFor(error)}`
      );
      deps.exitProcess(1);
    }
  };

  const claimWith = async (
    context: CommandContext | SessionContext,
    supervised: boolean,
    progress: ClaimProgress,
    startedBy: ClaimStarter
  ): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    const capability =
      controllerCapability ??
      (supervised
        ? await requiredSecret(process.env, "LEGION_BOOT_TOKEN")
        : await requiredControllerCapability(process.env));
    controllerCapability = capability;
    const launched = classifySession(process.env).kind === "controller";
    // Read and validate before anything below mutates daemon-side state (the registration that
    // replaces the running controller, the role claim): a missing value refuses here, leaving the
    // previous controller, if any, still running and still registered. A controller the daemon
    // launched carries none: it takes its start task from the value the daemon writes at each
    // ready.
    const startMessage = operatorLaunchedController(process.env)
      ? requiredEnvironment(process.env, "LEGION_CONTROLLER_START_MESSAGE")
      : undefined;
    // The controller's own transcript, which persistedTranscript puts on disk, is recorded so the
    // controller's own `task` subagents are recognised even when the transcript is not a file on
    // disk; a hand-started takeover records nothing.
    const { sessionFile, agentId } = await persistedTranscript(context);
    if (launched) recordBootstrappedSession(sessionFile);

    const project = requiredEnvironment(process.env, "LEGION_PROJECT");
    // The daemon names the project's channels to the controller it launched alone, so only it hears
    // them; any other ignores whatever LEGION_SLACK_CHANNELS it inherited.
    const slackChannels = supervised ? daemonControllerSlackChannels(process.env) : [];
    const client = daemon();
    // The health route, which answers the project in one word: the boot check read the whole
    // project body for it before (dispatch://INFRA-2694).
    const served = await client.health();
    const servedRole = controllerToken(legionProjectToken(served.project, "the daemon's project"));
    if (controllerToken(project) !== servedRole) {
      throw new Error(
        `LEGION_PROJECT ${project} names controller role ${controllerToken(project)}, but the daemon at ${requiredEnvironment(process.env, "LEGION_DAEMON_URL")} serves project ${served.project}, whose controller role is ${servedRole}`
      );
    }
    const registration = await client
      .registerController({
        bootToken: capability,
        sessionId: sessionID,
        ompSessionFile: sessionFile,
        agentId,
        pluginContract: PLUGIN_CONTRACT,
      })
      .catch((error: unknown) => {
        if (error instanceof LegionDaemonApiError) {
          // The daemon's detail is its own text, and it can echo the boot token it was sent.
          logger.error(
            `[legion] claims/register for the controller failed (${error.status}): ${scrub(error.detail)}`
          );
        }
        throw error;
      });
    if (controllerToken(project) !== registration.claimToken) {
      throw new Error(
        `LEGION_PROJECT ${project} names controller role ${controllerToken(project)}, but the daemon registered this controller as ${registration.claimToken}`
      );
    }
    // The daemon now holds this registration, and only its secret mints a controller grant: the
    // `legion` tool answers for this session with it from here on, before the start message below
    // starts a turn that calls the tool, and whether or not the role claim after it succeeds.
    claimComplete = false;
    mintControllerGrant = () =>
      client.controllerGrant({ sessionId: sessionID, secret: registration.secret });
    controllerSessionID = sessionID;
    controllerTranscript = launched ? sessionFile : undefined;
    progress.registered = true;
    const envoyContext = "setInterval" in context ? context : undefined;
    // Once per process, ever, and before the role claim and the subscription below, so no card on
    // the role's lane and no wake on the controller topic can race the start turn for the one
    // first-turn slot (LEGION-392): a card the listener forwards as soon as it records the role can
    // arrive before the role claim resolves here. The session's first turn when it is idle, marked
    // as its startup turn. When the operator already started a turn, queued behind it as a
    // follow-up, which starts no turn of its own and runs inside that run, whose `agent_start` has
    // already fired, so nothing is marked. A hand-started takeover (`startMessage` undefined) sends
    // nothing: the operator's own session speaks for itself. Nor does the daemon's own controller,
    // which takes the start task whose value the daemon writes once it reports ready below.
    if (startMessage !== undefined && !startMessageSent) {
      // A `/new`, `/resume`, or `/fork` since this claim read its session leaves the start message
      // nowhere to go: the session current now is not the one registered, and Envoy refuses a role
      // claim for the replaced one. The session change starts a re-claim
      // (`reclaimAfterSessionChange`, which claims again as this claim stops unfinished), which
      // registers the new session and sends the start message there. That re-claim shows the notice
      // that the claim moved on where the session change's reset of the screen leaves it; one shown
      // from here would land while the `/new` is still under way and be wiped with its transcript.
      if (context.sessionManager.getSessionId() !== sessionID) {
        // The replaced session's start will never be sent: its unsent mark goes too.
        clearStartupTurn(sessionID);
        startHandedOver = true;
        return;
      }
      const send =
        context.isIdle?.() === false
          ? pi.sendUserMessage(startMessage, { deliverAs: "followUp" })
          : sendStartTurn(sessionID, startMessage);
      startMessageSent = true;
      void Promise.resolve(send).catch((error: unknown) => startMessageDropped(sessionID, error));
    }
    const caller = claimCaller(registration, sessionID);
    const taken = supervised ? takeFor(client, caller) : undefined;
    if (taken !== undefined) {
      // The daemon's controller reports ready and reads its start task's value before it claims
      // its role and its topics, so no card on the role's lane and no notice or Slack mention can
      // take the turn of the start task the daemon writes at this ready. Its agent subject is open
      // from its start, so until that read has answered its take holds the task-turn mark
      // whatever value it holds, and a card sent to the session itself waits too.
      const readAnswered = taken.take.awaitRead();
      // The daemon writes the start task's value at this ready, so the start is on its way from
      // here on.
      markStartupTurnSent(sessionID);
      startTurnSession = sessionID;
      await client.ready(caller);
      await taken.read();
      readAnswered();
    }
    // A claim the listener answered 503 wrote nothing - every try met a conflict, or the takeover
    // record a claim writes first did not store - or landed and stands, with only the handover of
    // the session it replaces not recorded (`claimStandsWithoutHandover`). Either is made again
    // after a growing wait (claimRoleAgainOnBusy), each wait logged with its attempt and the
    // listener's answer, and the claim widget shows which of the two it waits on - that the
    // controller holds no Envoy role yet, or that it holds the role and claims again for the
    // handover - until the claim lands (taken down below). The bound follows who started the
    // claim. The daemon-launched controller's own
    // claim at `session_start` claims again, as the claim boot does, rather than exiting for a
    // relaunch that would meet the same 503. It reported ready and read its start task's value
    // above, which reports that value seen, and it takes the task only once its role is held, so it
    // is bounded by the daemon's first take bound (`worker_take_timeout_seconds`, five minutes by
    // default): each bound that passes with no take charges it, and the third charge retires and
    // relaunches it, logging the task, the revision it saw and the bound (supervise's
    // takeBoundOver). A session change there never reaches this claim: the daemon refuses a
    // registration from a session other than the one it recorded (SameAgentRefusal, 409), so the
    // controller exits (claimOnce) and is relaunched on its recorded session. Under `controller:
    // daemon` the controller runs as `omp --mode rpc`, where the widget and the notices are frames
    // to the daemon that nothing shows: each wait shows in the Oh My Pi log of the controller's
    // session. A command a person typed (`/legion-claim-controller`, `/legion-controller-start`),
    // wherever it runs, and any claim of a controller the daemon did not launch, also shows each
    // wait as a notice and gives up after CONTROLLER_CLAIM_BUSY_ATTEMPTS tries, so it fails where
    // the person sees it, with the listener's answer (for a claim that stands, that it holds the
    // role and only the handover is missing); in the daemon-launched controller its process
    // stays up, with the role and topics its first claim took, and its waits are in that log.
    const ui: SessionContext["ui"] = context.ui;
    let waitShown = false;
    const showWaiting = (line: string, error: unknown): void => {
      const shown = claimStandsWithoutHandover(error)
        ? `legion: this controller holds its Envoy role; the handover of the session it replaces is not recorded yet, so it claims again: ${line}`
        : `legion: this controller holds no Envoy role yet: ${line}`;
      if (ui.setWidget === undefined) {
        ui.notify(shown, "warning");
        return;
      }
      ui.setWidget(CLAIM_WIDGET, [shown]);
      claimWidget = { session: sessionID, clear: () => ui.setWidget?.(CLAIM_WIDGET, undefined) };
      waitShown = true;
    };
    try {
      // `replaces`, the session the registration says this one replaces (the controller session
      // the daemon forgot), is the claim's `previous_session_id`: the listener hands that
      // session's unread direct and role messages to this one once the claim lands.
      await claimRoleAgainOnBusy(
        () =>
          claimEnvoyRole(sessionID, registration.claimToken, envoyContext, registration.replaces),
        registration.claimToken,
        supervised && startedBy === "daemon"
          ? {
              log: (line, error) => {
                logger.warn(`[legion] ${line}`);
                showWaiting(line, error);
              },
            }
          : {
              log: (line, error) => {
                logger.warn(`[legion] ${line}`);
                const notice = `legion: ${line}`;
                waitNotices.add(notice);
                ui.notify(notice, "info");
                if (ui.setWidget !== undefined) showWaiting(line, error);
              },
              maxAttempts: CONTROLLER_CLAIM_BUSY_ATTEMPTS,
            }
      );
    } catch (error) {
      // A claim that gave up, or met any other refusal, takes its waiting line down: the claim no
      // longer claims again, and its failure is what the person reads now.
      if (waitShown && claimWidget?.session === sessionID) {
        claimWidget.clear();
        claimWidget = undefined;
      }
      // The listener's 503 thrown here is the bounded role claim giving up after its counted tries,
      // the one failure a typed claim of the daemon-launched controller is answered on screen for
      // (`claimOnce`). One to a claim that stands is not a claim that holds nothing: what it says
      // is that the controller holds the role, and only the handover of the session it replaces is
      // missing.
      if (error instanceof Error && listenerAskedToClaimAgain(error)) {
        const gaveUp = claimStandsWithoutHandover(error)
          ? new Error(
              `this controller holds its Envoy role ${registration.claimToken}; only the handover of the session it replaces is missing, still not recorded after ${CONTROLLER_CLAIM_BUSY_ATTEMPTS} claims: ${messageFor(error)}`,
              { cause: error }
            )
          : error;
        roleClaimGaveUp.add(gaveUp);
        throw gaveUp;
      }
      throw error;
    }
    // The role is held: the waiting line comes down now, so a step after it that fails (the
    // controller topic, a Slack channel) leaves no line saying the claim is still being made.
    if (waitShown && claimWidget?.session === sessionID) {
      claimWidget.clear();
      claimWidget = undefined;
    }
    await subscribeLegionNotice(
      sessionID,
      legionControllerNoticeSubject(project),
      envoyContext,
      registration.claimToken
    );
    // The project's Slack channels, as the daemon that launched this controller names them: each
    // channel's app mentions, for as long as this session holds the controller role, never
    // registered with the listener, so a resume hears the list it is launched with and no other.
    for (const channel of slackChannels) {
      await subscribeLegionNotice(
        sessionID,
        slackMentionTopic(channel),
        envoyContext,
        registration.claimToken
      );
    }
    if (taken !== undefined) {
      // From a timer, once `session_start` has returned: a send made from there, or while Oh My
      // Pi's startup drains its sends, would run the start turn inside that drain, where RPC mode
      // reads no command (`src/claim-session.ts`).
      setImmediate(() => taken.take.ready());
    }
    claimComplete = true;
    // A re-claim's refusal or hand-over the widget still shows is out of date once a claim succeeds.
    claimWidget?.clear();
    claimWidget = undefined;
  };

  /**
   * Only the controller the daemon launched claims at `session_start`: it has no start message of
   * its own to queue ahead of its role's lane, and its claim reports ready, at which the daemon
   * writes the value of the start task it then takes, before it claims that lane. The
   * operator-launched controller claims from `/legion-controller-start` (`startCommand`), after Oh
   * My Pi's startup, and until its registration is in, its `legion`
   * tool answers that the session holds no registered claim. With a start message still to send,
   * it marks this session in the Envoy plugin interface's `startupTurn` with the start unsent
   * (`markStartupTurnPending`) and claims nothing: the Envoy entry opens the session's agent
   * subject at its own `session_start`, and a card that arrives from here until the start command
   * sends the start finds the mark, so it takes no turn and its wake waits for the start turn
   * (`packages/pi-envoy/src/card-wake.ts`). The send marks it sent; the session's first
   * `agent_start` (an operator turn started before the command, after which the start message
   * goes as a follow-up) and its shutdown clear it, and so does the start command's claim when it
   * fails before the send or hands over to a new session.
   */
  const handleSessionStart = async (context: SessionContext): Promise<void> => {
    if (!daemonLaunchedController(process.env)) {
      if (
        operatorLaunchedController(process.env) &&
        process.env.LEGION_CONTROLLER_START_MESSAGE &&
        !startMessageSent
      ) {
        markStartupTurnPending(context.sessionManager.getSessionId());
      }
      return;
    }
    const sessionID = context.sessionManager.getSessionId();
    if (controllerSessionID === undefined || controllerSessionID === sessionID) {
      await queueClaim(context, { registered: false }, "daemon");
    }
  };

  /**
   * `/legion-controller-start`, which `legion controller start` puts on Oh My Pi's command line
   * and Oh My Pi runs once its startup is over and the TUI listens: the operator-launched
   * controller's whole claim (`claim`), which sends the start message right before the role
   * claim. A refusal anywhere in it is logged and shown to the operator as one warning, which says
   * what the claim left: refused before the send, nothing was sent, the unsent `startupTurn` mark
   * `session_start` set is cleared, and `/legion-claim-controller` claims again and sends the
   * start message once that claim succeeds; refused after it (the role
   * claim or the subscription), the start turn runs holding the registration, and
   * `/legion-claim-controller` claims the role and its wakes again. The command itself never fails
   * past that notice. Run again once the start message is sent, it claims nothing: there is
   * nothing to do once a claim succeeded, and while the last claim that registered (the one that
   * sent it, or a later one) did not finish, a warning names `/legion-claim-controller`. A `/new`
   * while its claim runs stops that claim at its registration, before the send, clearing the
   * replaced session's unsent mark, and the command shows nothing: the re-claim the session change
   * starts claims the new session, sends the start message there, marking it sent, and shows the
   * notice that it did so (`reclaimAfterSessionChange`); no warning,
   * as nothing failed. A hand-started takeover and the controller the daemon launched (which
   * claimed at `session_start`) have nothing to do either, and the command says so. Oh My Pi reads
   * the terminal while the claim runs, so the operator can run the command again before it ends:
   * that run says it is waiting and decides after the claim in flight.
   */
  const startCommand = (context: CommandContext): Promise<void> => {
    noteWait(context);
    return serialised(async () => {
      if (!operatorLaunchedController(process.env) || claimComplete) {
        context.ui.notify(
          "legion: nothing to do: the controller claim already succeeded in this process, or this session claims no controller from this command (a hand-started takeover, or the controller the daemon launched).",
          "info"
        );
        return;
      }
      if (startMessageSent) {
        context.ui.notify(
          "legion: this process already sent the start message, but its controller claim did not finish; run /legion-claim-controller to claim the controller role and its wakes again.",
          "warning"
        );
        return;
      }
      // The session the claim reads first, which a refusal before the send leaves unmarked.
      const claimed = context.sessionManager.getSessionId();
      try {
        await claimOnce(context, { registered: false }, "typed");
      } catch (error) {
        const message = messageFor(error);
        logger.error(`[legion] the controller claim failed: ${message}`);
        // Refused before the send: the start `session_start` marked due is not coming, so the
        // Envoy entry is not left holding its startup cards for it.
        if (!startMessageSent) clearStartupTurn(claimed);
        context.ui.notify(
          startMessageSent
            ? `legion: the controller claim failed after the start message was sent: ${message}. The start turn has the controller's registration; run /legion-claim-controller to claim the controller role and its wakes again.`
            : `legion: the controller claim failed: ${message}. Run /legion-claim-controller to claim again; it sends the start message once the claim succeeds.`,
          "warning"
        );
      }
    });
  };

  /**
   * `/new`, `/resume`, or `/fork` can replace the controller session and its transcript in place.
   * The caller never passes a `task` subagent's session (legion.ts `afterSessionChange`). A session
   * that is still the one last registered, on the same transcript, claims again only when that
   * claim did not finish. A re-claim queued behind a start claim that stopped for this session
   * change (`startHandedOver`) first says the claim moved to the new session.
   *
   * A re-claim that has to wait for the claim in flight says so at once, on the screen as the
   * session change stands, so a `/new` held behind a hung start claim says what holds it. Its
   * outcome (that hand-over, its refusal) is shown where the session change's reset leaves it. Oh
   * My Pi's `/new` (command-controller.ts `#runNewSessionFlow` at the pinned tag) awaits
   * `newSession`, which emits session_switch last and awaits every handler (agent-session.ts
   * `newSession`), and only then resets the transcript and clears the scrollback: a notice, which
   * goes to the transcript, shown while the handler runs is wiped before the operator can read it.
   * With a subagent view focused, that flow first returns to the main session, and the replay that
   * return runs can wait on file writes and on `setImmediate` before the reset (session-focus-
   * controller.ts `#attach`, ui-helpers.ts `renderSessionContextIncrementally`), so a notice from
   * a timer can still land before the reset. So the outcome goes to the host's extension widget
   * (`ui.setWidget`, key `legion-controller-claim`), which neither that reset, the transient UI it
   * clears, nor the return to the main session takes down; the session's next run
   * (`handleAgentStart`) or a later claim that succeeds does. A host with no widget shows the
   * outcome as notices from its managed timer once the re-claim settles, a macrotask that runs
   * after an unfocused `/new`'s reset, which waits on nothing but promises.
   *
   * The claim runs with a context whose `ui` collects its notices for the same delivery and that
   * delegates everything else to the host's (`Object.create`, as the host scopes a handler's
   * context, so its live accessors stay live): a notice from anywhere in the claim path joins the
   * outcome, and one shown through that context once the outcome is out (the Envoy role claim
   * keeps the context) goes to the screen at once.
   */
  const reclaimAfterSessionChange = async (context: SessionContext): Promise<void> => {
    if (classifySession(process.env).kind !== "controller") return;
    if (
      claimComplete &&
      context.sessionManager.getSessionId() === controllerSessionID &&
      context.sessionManager.getSessionFile() === controllerTranscript
    ) {
      // Back on the session the daemon still holds as the controller: a widget a re-claim showed
      // for another session (one refused after a `/new`) speaks of that session, not this one.
      if (claimWidget !== undefined && claimWidget.session !== controllerSessionID) {
        claimWidget.clear();
        claimWidget = undefined;
      }
      return;
    }
    const notices: { readonly message: string; readonly level: "info" | "warning" }[] = [];
    let shown = false;
    const notify = (message: string, level: "info" | "warning"): void => {
      if (shown) context.ui.notify(message, level);
      else notices.push({ message, level });
    };
    // The host's widget is passed through as it is: it outlasts the session change's reset, so a
    // claim that waits on the listener shows so at once (`claimWith`'s waiting state).
    const { setWidget } = context.ui;
    const deferred: SessionContext = Object.create(context, {
      ui: {
        value: {
          notify,
          setWidget:
            setWidget === undefined
              ? undefined
              : (key: string, lines: string[] | undefined) => context.ui.setWidget?.(key, lines),
        },
      },
    });
    // Whether this claim reached its registration: one refused after it left this session
    // registered, so its grants mint; one refused before it registered nothing for it.
    const progress: ClaimProgress = { registered: false };
    // Whether the re-claim's claim landed: its wait notices are then dropped from those it shows.
    let landed = false;
    try {
      noteWait(context);
      await serialised(async () => {
        waitNotices.clear();
        if (startHandedOver) {
          startHandedOver = false;
          notify(
            "legion: the session changed while the controller claim ran; the claim for the new session follows and sends the start message there.",
            "info"
          );
        }
        await claimOnce(deferred, progress, "daemon");
        landed = true;
      });
    } catch (error) {
      notify(
        progress.registered
          ? `legion: re-claiming the controller for this session failed (${messageFor(error)}). The session keeps its controller registration, so the \`legion\` tool's set_status still works, but until a re-claim succeeds controller wakes and merges will not reach this session.`
          : `legion: re-claiming the controller for this session failed (${messageFor(error)}). Until a re-claim succeeds, controller wakes and merges will not reach this session, and the \`legion\` tool's set_status and its other operations that call the daemon have no controller grant to mint and fail.`,
        "warning"
      );
    } finally {
      const { ui } = context;
      if (landed) {
        const kept = notices.filter(({ message }) => !waitNotices.has(message));
        notices.splice(0, notices.length, ...kept);
      }
      if (ui.setWidget === undefined) {
        context.setTimeout(() => {
          shown = true;
          for (const { message, level } of notices.splice(0)) ui.notify(message, level);
        }, 0);
      } else {
        shown = true;
        if (notices.length > 0) {
          ui.setWidget(
            CLAIM_WIDGET,
            notices.splice(0).map(({ message }) => message)
          );
          claimWidget = {
            session: context.sessionManager.getSessionId(),
            clear: () => ui.setWidget?.(CLAIM_WIDGET, undefined),
          };
        }
      }
    }
  };

  /**
   * The session's `agent_start`: a run of the session a re-claim's widget was shown for takes that
   * widget down, as the operator has moved on, unless it is the run of the start that session's
   * claim sent (`startTurnSession`), which can begin before or after the widget goes up. Another
   * session's run (a `task` subagent's) takes down nothing.
   */
  const handleAgentStart = (context: SessionContext): void => {
    const sessionID = context.sessionManager.getSessionId();
    if (startTurnSession === sessionID) {
      startTurnSession = undefined;
      return;
    }
    if (claimWidget?.session !== sessionID) return;
    claimWidget.clear();
    claimWidget = undefined;
  };

  const isClaimedSession = (sessionID: string): boolean =>
    controllerSessionID === sessionID && mintControllerGrant !== undefined;

  const mintGrant = async (sessionID: string): Promise<LegionGrant> => {
    if (controllerSessionID !== sessionID || mintControllerGrant === undefined) {
      throw new Error("Controller session is not registered; cannot mint its grant");
    }
    return mintControllerGrant();
  };

  return {
    claim,
    handleSessionStart,
    handleAgentStart,
    reclaimAfterSessionChange,
    startCommand,
    isClaimedSession,
    mintGrant,
    take: (sessionID) => (taking?.caller.sessionId === sessionID ? taking.take : undefined),
    reportedClaim: (sessionID) =>
      taking?.caller.sessionId === sessionID
        ? { caller: taking.caller, role: "controller" }
        : undefined,
  };
}
