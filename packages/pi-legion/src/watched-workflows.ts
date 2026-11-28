import { githubDefaultBranchWorkflowSubject } from "@legion/contracts";
import type { LegionControllerRegistration } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import { claimRoleAgainOnBusy } from "@legion/pi-shared/legion-claim-core";
import type { PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { refusedForRoleNotHeld, subscribeLegionNotice } from "@legion/pi-shared/role-claim-bridge";
import { claimRefusedForStop } from "@legion/pi-shared/role-claim-stopping";
import { logger } from "@oh-my-pi/pi-utils";

/** The `customType` of what this process tells its session of its watched workflows: shown in
 * the transcript, and no turn of its own, as Envoy's own notices are (`tellWatched`). */
const WATCHED_WORKFLOWS_NOTICE = "legion-watched-workflows";

/** One topic of the project's watched workflows: the entry, as `legion.yaml` names it
 * (`<owner>/<repo>/<file>`), one of its events, and the default-branch topic of that workflow's
 * completed runs of that event. */
export interface WatchedTopic {
  readonly entry: string;
  readonly event: string;
  readonly topic: string;
}

/** The claim a watched subscription is made for (`subscribeWatchedTopics`). */
interface WatchedClaim {
  /** The plugin API whose session is told of its watched topics (`tellWatched`). */
  readonly pi: PiApi;
  /** The session the controller's registration names. */
  readonly sessionID: string;
  /** The session the claim's context is on now: its session manager's id, read live. The host
   * moves it at a `/new` or `/resume`, before the session change's re-claim runs; once it is not
   * `sessionID`, the task asks and tells nothing more (`subscribeWatchedTopic`). */
  readonly liveSession: () => string;
  /** The role the claim took, which each watched subscription lasts as long as. */
  readonly claimToken: string;
  /** The context the claim hands Envoy's bridge with each of its own subscriptions. */
  readonly envoyContext: SessionContext | undefined;
  /** Hears that Envoy refused a watched subscription because the session's stop has begun
   * (`claimRefusedForStop`): that stop is the claim's too. */
  readonly stopHeard: () => void;
  /** The watched topics whose landed notice the process owes its session: each one a session was
   * told waits after a 503 and not yet told landed, with the session told that it waits, or
   * undefined once a failure other than a 503 followed that wait. The process keeps one map for
   * every claim, so the claim that sees such a topic land tells it landed, whichever claim told
   * it waits, and a claim tells its own session of a wait the map does not name that session for
   * (`subscribeWatchedTopic`). */
  readonly toldWaiting: Map<string, string | undefined>;
}

/** What a watched task's attempt throws, before it reaches Envoy, once the claim's context is on
 * another session than the one the task subscribes for (`subscribeWatchedTopic`). */
class SessionMovedError extends Error {
  override readonly name = "SessionMovedError";

  constructor(sessionID: string) {
    super(`the pane moved off session ${sessionID}`);
  }
}

/** The watched workflows `registration` names (`watchedWorkflows`), one topic per event of each
 * entry, in the registration's order: none from a daemon whose answer does not carry it, which is
 * one before its contract entry. */
function watchedWorkflowsOf(registration: LegionControllerRegistration): WatchedTopic[] {
  const topics: WatchedTopic[] = [];
  for (const watchedWorkflow of registration.watchedWorkflows ?? []) {
    const entry = `${watchedWorkflow.owner}/${watchedWorkflow.repo}/${watchedWorkflow.workflow}`;
    for (const event of watchedWorkflow.events) {
      const topic = githubDefaultBranchWorkflowSubject(
        watchedWorkflow.owner,
        watchedWorkflow.repo,
        watchedWorkflow.workflow,
        "completed",
        event
      );
      topics.push({ entry, event, topic });
    }
  }
  return topics;
}

/** `topics` as a log line names them: each entry with its event, or "none". */
function listed(topics: readonly WatchedTopic[]): string {
  if (topics.length === 0) return "none";
  return topics.map(({ entry, event }) => `${entry} (${event})`).join(", ");
}

/**
 * The watched workflows a claim subscribes. `kept` is the list this process's first registration
 * named, and undefined until that registration. A process keeps one list for its whole life, as
 * it keeps its Slack channels. So the first registration's own list is the answer once, and `kept`
 * is the answer for every later registration. A later registration that names another list changes
 * nothing in this process. It writes one warning line, which says that the changed list takes
 * effect at the controller's next launch. The session is told nothing of it. A list is the topics
 * it names: the same topics in another order are the same list.
 */
export function watchedWorkflowsKept(
  kept: readonly WatchedTopic[] | undefined,
  registration: LegionControllerRegistration
): readonly WatchedTopic[] {
  const named = watchedWorkflowsOf(registration);
  if (kept === undefined) return named;
  const keptTopics = new Set(kept.map(({ topic }) => topic));
  const namedTopics = new Set(named.map(({ topic }) => topic));
  if (
    namedTopics.size !== keptTopics.size ||
    [...namedTopics].some((topic) => !keptTopics.has(topic))
  ) {
    logger.warn(
      `[legion] the daemon's registration names another watched-workflows list; the changed list takes effect at the controller's next launch, and until then this process follows the list its first registration named. Followed: ${listed(kept)}. Named now: ${listed(named)}.`
    );
  }
  return kept;
}

/** Tells `pi`'s session `content` as Envoy tells it its own notices (`handNotice` in
 * pi-envoy's envoy.ts): shown in the transcript, and no turn starts for it. A session that
 * refuses it (`pi.sendMessage` throws) is not told: the refusal is logged with the notice, so no
 * notice of a watched topic fails the claim. */
function tellWatched(pi: PiApi, content: string): void {
  try {
    pi.sendMessage(
      { customType: WATCHED_WORKFLOWS_NOTICE, content, display: true },
      { deliverAs: "steer", triggerTurn: false }
    );
  } catch (error) {
    logger.warn(
      `[legion] the session refused this notice of its watched workflows, so it is not told: ${messageFor(error)}. The notice: ${content}`
    );
  }
}

/**
 * Subscribes `topic` for as long as the claim's session holds the claim's role, and answers
 * whether the next watched topic is to be subscribed. Envoy's 503 (an edge listener that cannot
 * read the project's repositories answers `repositories_unknown`) is waited out as a role claim's
 * is (`claimRoleAgainOnBusy`: 1 s, doubling to 30 s, a log line per wait). The claim's session is
 * told once that the topic waits, naming Envoy's answer, and the session is told once that it
 * landed. What was told is the process's, not the claim's (`toldWaiting`): a later claim says
 * nothing of a wait its own session was told of already, a claim of another session (after a
 * `/new` or `/resume`) tells that session the topic waits, and the subscription that lands tells
 * the landed notice an earlier claim's wait promised. A refusal because the session's stop has
 * begun (`claimRefusedForStop`) is that stop (`stopHeard`), and says nothing; one because the
 * session holds the role no more (`refusedForRoleNotHeld`) ends the watched subscriptions with
 * one log line. Any other failure is logged and told, and gives this topic up alone: a landed
 * notice it owed stays owed, and the next claim's 503 tells the wait again. Once `signal` aborts -
 * the stop, a session change or a later registration - nothing more is asked or said: an attempt
 * in flight at the abort says nothing whatever it is answered, a 503 included, since
 * `claimRoleAgainOnBusy` calls no `log` once the signal has aborted.
 *
 * At a `/new` or `/resume` the host moves the claim's context to another session first, and the
 * session change's re-claim aborts `signal` only later, if it gets that far. So the task reads the
 * context's session (`liveSession`) before each attempt, at each 503 and at each answer, at that
 * moment: once it is not `sessionID`, the task speaks for a session the pane has left. It makes no
 * attempt (the attempt throws `SessionMovedError` before it reaches Envoy), tells nothing, leaves
 * `toldWaiting` as it was, so a landed notice it owed stays owed to the claim of the session the
 * pane is on now, and ends, with one debug line where an attempt failed.
 */
async function subscribeWatchedTopic(
  { pi, sessionID, liveSession, claimToken, envoyContext, stopHeard, toldWaiting }: WatchedClaim,
  { entry, event, topic }: WatchedTopic,
  signal: AbortSignal
): Promise<boolean> {
  const watched = `watched workflow ${entry}'s ${event} runs (topic ${topic})`;
  const sessionMoved = (): boolean => liveSession() !== sessionID;
  try {
    await claimRoleAgainOnBusy(
      async () => {
        if (sessionMoved()) throw new SessionMovedError(sessionID);
        await subscribeLegionNotice(sessionID, topic, envoyContext, claimToken);
      },
      { what: `subscription of ${watched}`, again: "subscribing it again" },
      {
        log: (line, error) => {
          logger.warn(`[legion] ${line}`);
          if (sessionMoved()) return;
          if (toldWaiting.get(topic) === sessionID) return;
          toldWaiting.set(topic, sessionID);
          tellWatched(
            pi,
            `legion: Envoy answered the subscription of ${watched} 503, so this session does not hear those runs yet; the controller subscribes it again after a growing wait, and says so once Envoy takes it. Envoy's answer: ${messageFor(error)}`
          );
        },
        signal,
      }
    );
  } catch (error) {
    if (signal.aborted) return false;
    if (error instanceof SessionMovedError || sessionMoved()) {
      logger.debug(
        `[legion] the pane moved off session ${sessionID}, so that session's watched subscriptions end at the one of ${watched}, telling the session the pane is on now nothing; a claim of that session subscribes its own.`
      );
      return false;
    }
    if (claimRefusedForStop(error)) {
      stopHeard();
      return false;
    }
    if (refusedForRoleNotHeld(error)) {
      logger.warn(
        `[legion] this session holds Envoy role ${claimToken} no more, so it subscribes no more of its watched workflows' topics: ${messageFor(error)}`
      );
      return false;
    }
    logger.error(
      `[legion] the subscription of ${watched} failed, and it is not made again: ${messageFor(error)}`
    );
    // The landed notice stays owed, and the wait is told again at the next 503.
    if (toldWaiting.has(topic)) toldWaiting.set(topic, undefined);
    tellWatched(
      pi,
      `legion: the subscription of ${watched} failed, so this session may not hear those runs until the controller claims its role again: ${messageFor(error)}`
    );
    return true;
  }
  if (signal.aborted || sessionMoved()) return false;
  if (toldWaiting.delete(topic)) {
    tellWatched(
      pi,
      `legion: Envoy answered the subscription of ${watched} without a 503 at last; this session hears those runs now, unless Envoy's own "Envoy topics refused" notice names that topic. A run that ended while the topic waited is not delivered, so read ${entry}'s newest completed run of the ${event} event on its repository's default branch now, and take it as a delivered run.`
    );
  }
  return true;
}

/**
 * Subscribes the watched workflows of a claim that has just finished, in a task of its own that
 * the claim does not wait for. Nothing Envoy answers about a watched topic reaches the claim. So
 * no answer exits the process, costs the role or another topic, or holds back the daemon's ready.
 * The daemon's registration deadline would retire a controller that never reported ready while
 * an edge listener withheld its GitHub topics. The task subscribes `topics` one after the other
 * (`subscribeWatchedTopic`). A failure the task did not expect ends it with one error line, and
 * reaches the claim no more than the notices do (`tellWatched`). Once `signal` aborts (the stop, a
 * session change or a later registration), or the claim's context is on another session
 * (`subscribeWatchedTopic`), no further topic is asked for.
 */
export function subscribeWatchedTopics(
  claim: WatchedClaim,
  topics: readonly WatchedTopic[],
  signal: AbortSignal
): void {
  void (async () => {
    for (const watchedTopic of topics) {
      if (signal.aborted) return;
      if (!(await subscribeWatchedTopic(claim, watchedTopic, signal))) return;
    }
  })().catch((error: unknown) => {
    logger.error(
      `[legion] the subscriptions of this session's watched workflows stopped on an error, and no more of them are made until the controller claims its role again: ${messageFor(error)}`
    );
  });
}
