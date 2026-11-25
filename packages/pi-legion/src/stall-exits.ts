import { closeSync, fsyncSync, mkdirSync, openSync, readFileSync, writeSync } from "node:fs";
import path from "node:path";
import { sanitizeToken } from "@legion/contracts";
import { messageFor } from "@legion/envoy-client/errors";
import type { StallWatchdogExits, StallWatchdogLog } from "@legion/pi-shared/stall-watchdog";

/**
 * The third step of the stall ladder (INFRA-1274, R4) is the only one that costs the process, and
 * this module is what bounds how often a worker may take it: its own count of the stall exits it
 * has made, in a file of `LEGION_STATE_DIR`. `packages/pi-shared/src/stall-watchdog.ts` decides
 * when the step runs and holds the cap (`STALL_EXIT_CAP`); this module reads and writes the
 * count, and nothing else here ends anything.
 *
 * **Why a count at all.** The exit is read by the daemon as a death, and the task is still the
 * claim's pending one, so the daemon relaunches the claim on the same session with that task
 * again. A stall the relaunched worker walks straight back into would end the process again and
 * again, so the worker counts its own exits and stops at `STALL_EXIT_CAP`, which is 2: one below
 * the daemon's `launch_failure_limit`, whose default is 3
 * (the `launch_failure_limit` row of `topLevelKeys` in `packages/daemon/internal/config/keytable.go`) and at which the daemon fails the claim. At
 * the cap the worker hands its owner a session that is stuck, which a person can read, rather
 * than a claim that is failed.
 *
 * **Where the count lives.** `LEGION_STATE_DIR` is the directory the daemon gives this pane
 * (`src/claim-session.ts:43`). Under Kubernetes it is the pod's own, so a pod replacement resets
 * this count while the daemon's death budget is not reset — the two bounds are deliberately not
 * the same bound. Under tmux several panes share one state directory, so the file is named for the
 * claim it counts, by the claim's token: the claim of one agent of an issue, which keeps its token
 * when it moves to another role's step, and two claims of one issue can hold one role (the daemon's
 * `record.WorkerClaim`). One claim's exits therefore never spend another's budget, two claims of
 * one role keep two counts, and a relaunch of the same claim in the same pod reads its
 * own count back, whatever role it was relaunched on.
 *
 * **Written and flushed before the process ends.** Both halves are synchronous and the write ends
 * with `fsync`, because the caller ends the process the moment the write answers: a count still
 * sitting in this process's buffers would be a count the relaunched worker never reads.
 *
 * **A file that cannot be read or written ends nothing.** A missing file is a worker that has
 * ended itself no times, which is every first stall. A file that cannot be read, one holding
 * anything but a count this module wrote, and a session whose claim is not registered yet, so
 * that no file names it, answer `undefined`; a write that fails, or that has no claim to name,
 * answers false. The detector ends no process in either case and tells the owner instead, because
 * a worker that cannot count its own exits would end itself without bound.
 */

/** What one worker's counter file holds. A record rather than a bare number, so a person reading
 * a pod's state directory sees whose count it is and when it was last written. */
export interface StallExitsRecord {
  /** How many times this worker has ended its own process for a stuck tool call. */
  readonly exits: number;
  /** When that count was written, ISO 8601. */
  readonly at: string;
  /** The token of the claim the count belongs to, as the file name spells it. */
  readonly who: string;
}

/** The counter file of the claim `claimToken` counts in, under `stateDir`. */
export function stallExitsFile(stateDir: string, claimToken: string): string {
  return path.join(stateDir, `legion-stall-exits-${sanitizeToken(claimToken)}.json`);
}

export interface StallExitsDeps {
  /** `LEGION_STATE_DIR`, this worker's own state directory (`src/claim-session.ts:43`). */
  readonly stateDir: string;
  /** This session's registered claim, read at each count (the plugin's `paneClaim`): its token
   * names the file. `undefined` while no claim is registered. */
  readonly claim: () => { readonly claimToken: string } | undefined;
  /** The clock the record's moment is taken from. */
  readonly now: () => number;
  readonly log: StallWatchdogLog;
}

/** The detector's `StallWatchdogExits`: this worker's own stall exits, counted in its state
 * directory under its claim's token. */
export function createStallExits(deps: StallExitsDeps): StallWatchdogExits {
  const fileOf = (): { readonly who: string; readonly file: string } | undefined => {
    const who = deps.claim()?.claimToken;
    return who === undefined || who === ""
      ? undefined
      : { who, file: stallExitsFile(deps.stateDir, who) };
  };

  return {
    read: (): number | undefined => {
      const counted = fileOf();
      if (counted === undefined) {
        deps.log.warn("legion: the stall watch has no registered claim to count its exits under");
        return undefined;
      }
      const { file } = counted;
      let text: string;
      try {
        text = readFileSync(file, "utf8");
      } catch (error) {
        // A worker that has never ended itself in this pod has no file, and that is a count of
        // zero, not a file that could not be read.
        if ((error as NodeJS.ErrnoException | undefined)?.code === "ENOENT") return 0;
        deps.log.warn("legion: the stall watch could not read its own exit count", {
          file,
          error: messageFor(error),
        });
        return undefined;
      }
      const exits = exitsOf(text);
      if (exits === undefined) {
        deps.log.warn("legion: the stall watch's exit count is not a count it wrote", { file });
      }
      return exits;
    },

    write: (count: number): boolean => {
      const counted = fileOf();
      if (counted === undefined) {
        deps.log.warn("legion: the stall watch has no registered claim to count its exits under", {
          count,
        });
        return false;
      }
      const { who, file } = counted;
      const record: StallExitsRecord = {
        exits: count,
        at: new Date(deps.now()).toISOString(),
        who,
      };
      try {
        mkdirSync(deps.stateDir, { recursive: true });
        const handle = openSync(file, "w");
        try {
          writeSync(handle, `${JSON.stringify(record)}\n`);
          // Flushed here, because the caller ends this process as soon as the write answers.
          fsyncSync(handle);
        } finally {
          closeSync(handle);
        }
        return true;
      } catch (error) {
        deps.log.warn("legion: the stall watch could not write its own exit count", {
          file,
          count,
          error: messageFor(error),
        });
        return false;
      }
    },
  };
}

/** The count this file holds, or undefined for anything this module did not write: a count the
 * worker cannot trust is one it will not end its process on. */
function exitsOf(text: string): number | undefined {
  let record: unknown;
  try {
    record = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (typeof record !== "object" || record === null) return undefined;
  const { exits } = record as { readonly exits?: unknown };
  return typeof exits === "number" && Number.isInteger(exits) && exits >= 0 ? exits : undefined;
}
