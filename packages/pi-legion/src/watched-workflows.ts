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
export const WATCHED_WORKFLOWS_NOTICE = "legion-watched-workflows";

/** One topic of the project's watched workflows: the entry, as `legion.yaml` names it
 * (`<owner>/<repo>/<file>`), one of its events, and the default-branch topic of that workflow's
 * completed runs of that event. */
export interface WatchedTopic {
  readonly entry: string;
  readonly event: string;
  readonly topic: string;
}

/** The watched workflows of one registration (`watchedWorkflowsOf`). */
export interface WatchedWorkflows {
  /** One topic per event of each entry, in the registration's order. */
  readonly topics: readonly WatchedTopic[];
  /** The list as the session reads it: `<owner>/<repo>/<file>@<event>,…` per entry, the
   * entries and each entry's events sorted, so one list in another order reads the same;
   * `none` for no entry. */
  readonly listed: string;
}

/** The claim a watched subscription is made for (`subscribeWatchedTopics`). */
export interface WatchedClaim {
  /** The plugin API whose session is told of its watched topics (`tellWatched`). */
  readonly pi: PiApi;
  /** The session the controller's registration names. */
  readonly sessionID: string;
  /** The role the claim took, which each watched subscription lasts as long as. */
  readonly claimToken: string;
  /** The context the claim hands Envoy's bridge with each of its own subscriptions. */
  readonly envoyContext: SessionContext | undefined;
  /** Hears that Envoy refused a watched subscription because the session's stop has begun
   * (`claimRefusedForStop`): that stop is the claim's too. */
  readonly stopHeard: () => void;
}

/** The watched workflows `registration` names (`watchedWorkflows`): none from a daemon whose
 * answer does not carry it, which is one before its contract entry. */
export function watchedWorkflowsOf(registration: LegionControllerRegistration): WatchedWorkflows {
  const topics: WatchedTopic[] = [];
  const entries: string[] = [];
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
    entries.push(`${entry}@${[...watchedWorkflow.events].sort().join(",")}`);
  }
  return { topics, listed: entries.length === 0 ? "none" : entries.sort().join(", ") };
}

/** Tells `pi`'s session `content` as Envoy tells it its own notices (`handNotice` in
 * pi-envoy's envoy.ts): shown in the transcript, and no turn starts for it. Answers whether the
 * session took it. A session that refuses it (`pi.sendMessage` throws) is not told: the refusal
 * is logged with the notice, and the caller hears only `false`, so no notice of a watched topic
 * fails the claim. */
export function tellWatched(pi: PiApi, content: string): boolean {
  try {
    pi.sendMessage(
      { customType: WATCHED_WORKFLOWS_NOTICE, content, display: true },
      { deliverAs: "steer", triggerTurn: false }
    );
    return true;
  } catch (error) {
    logger.warn(
      `[legion] the session refused this notice of its watched workflows, so it is not told: ${messageFor(error)}. The notice: ${content}`
    );
    return false;
  }
}

/**
 * Subscribes `topic` for as long as the claim's session holds the claim's role, and answers
 * whether the next watched topic is to be subscribed. Envoy's 503 (an edge listener that cannot
 * read the project's repositories answers `repositories_unknown`) is waited out as a role claim's
 * is (`claimRoleAgainOnBusy`: 1 s, doubling to 30 s, a log line per wait), and the session is told
 * once that the topic waits, naming Envoy's answer, and once that it landed. A refusal because
 * the session's stop has begun (`claimRefusedForStop`) is that stop (`stopHeard`), and says
 * nothing; one because the session holds the role no more (`refusedForRoleNotHeld`) ends the
 * watched subscriptions with one log line. Any other failure is logged and told, and gives this
 * topic up alone. Once `signal` aborts - the stop, or a later registration - nothing more is
 * asked or said.
 */
export async function subscribeWatchedTopic(
  { pi, sessionID, claimToken, envoyContext, stopHeard }: WatchedClaim,
  { entry, event, topic }: WatchedTopic,
  signal: AbortSignal
): Promise<boolean> {
  const watched = `watched workflow ${entry}'s ${event} runs (topic ${topic})`;
  let waited = false;
  try {
    await claimRoleAgainOnBusy(
      () => subscribeLegionNotice(sessionID, topic, envoyContext, claimToken),
      { what: `subscription of ${watched}`, again: "subscribing it again" },
      {
        log: (line, error) => {
          logger.warn(`[legion] ${line}`);
          if (waited) return;
          waited = true;
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
    tellWatched(
      pi,
      `legion: the subscription of ${watched} failed, so this session does not hear those runs until the controller claims its role again: ${messageFor(error)}`
    );
    return true;
  }
  if (signal.aborted) return false;
  if (waited) {
    tellWatched(
      pi,
      `legion: Envoy answered the subscription of ${watched} without a 503 at last; this session hears those runs now, unless Envoy's own "Envoy topics refused" notice names that topic.`
    );
  }
  return true;
}

/**
 * Subscribes `topics`, the watched workflows of a claim that has just finished, each after the
 * one before it (`subscribeWatchedTopic`), in a task of its own that the claim does not wait for:
 * nothing Envoy answers about a watched topic reaches the claim, so none exits the process, costs
 * the role or another topic, or holds back the daemon's ready, whose registration deadline would
 * retire a controller that never reported ready while an edge listener withheld its GitHub
 * topics. A failure the task did not expect ends it with one error line, and reaches the claim no
 * more than the notices do (`tellWatched`). Once `signal` aborts, no further topic is asked for.
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
