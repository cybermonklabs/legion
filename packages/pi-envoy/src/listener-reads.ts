import { messageFor } from "@legion/envoy-client/errors";
import {
  EnvoyApiError,
  type EnvoyClient,
  type Interest,
  isBusUnanswered,
} from "@legion/envoy-client/transport";
import type { RegistrationStateDeps } from "./registration-state";

/**
 * What a role lookup answered. A failed read is `{ known: false }` and names no holder, so a
 * caller tells "nobody holds it" from "nobody could be asked", and the type refuses the third
 * state: a holder nobody could ask for.
 */
export type RoleHolderRead =
  | { readonly known: false }
  | { readonly known: true; readonly holder: string | undefined };

/** The sentences a failed unregistration is warned under: one for a bus that did not answer, and
 *  one for any other failure. */
export interface UnregisterWarnings {
  readonly failed: string;
  readonly busUnanswered: string;
}

export interface ListenerReadsDeps {
  /** The task's own gate: it throws where the task no longer decides, before any call goes out. */
  readonly guard: () => void;
  readonly client: () => EnvoyClient;
  readonly log: RegistrationStateDeps["log"];
}

/**
 * The listener reads a running task makes, and the one write that is warned rather than thrown.
 * Each is a step of its task: `guard` runs first, so a task that no longer decides sends nothing,
 * and a failure answers nothing rather than failing the task, since the next pass reads afresh.
 */
export function createListenerReads(deps: ListenerReadsDeps): {
  readonly readRoleHolder: (role: string) => Promise<RoleHolderRead>;
  readonly readInterest: (sessionID: string) => Promise<Interest | undefined>;
  readonly unregisterTopics: (
    sessionID: string,
    topics: readonly string[],
    warnings: UnregisterWarnings
  ) => Promise<void>;
} {
  return {
    readRoleHolder: async (role) => {
      deps.guard();
      return deps
        .client()
        .getRole(role)
        .then(
          (info): RoleHolderRead => ({ known: true, holder: info.holder }),
          (error: unknown): RoleHolderRead => {
            // The listener's 404 for a role no live session holds: it names a lapsed holder where
            // it knows one, and nobody otherwise. Any other failure is no answer at all.
            if (error instanceof EnvoyApiError && error.details.status === 404) {
              return { known: true, holder: error.holder };
            }
            return { known: false };
          }
        );
    },
    readInterest: async (sessionID) => {
      deps.guard();
      return deps
        .client()
        .getInterest(sessionID)
        .catch(() => undefined);
    },
    unregisterTopics: async (sessionID, topics, warnings) => {
      deps.guard();
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
  };
}
