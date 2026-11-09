/**
 * The one number the claim boot's deferred role claim runs on
 * (`src/legion-claim-core.ts`, `bootLegionClaim`): how long it waits for its session's first turn
 * before it takes the session's Envoy name itself.
 *
 * The boot claims the name at the session's first turn, because the Envoy listener keeps a
 * message published to a role no session holds and hands the whole kept lane over the moment the
 * role is claimed: claimed before the daemon's task, that lane takes the session's first turn and
 * the task waits for the turn to end; claimed at the first turn, it arrives inside a turn that is
 * already running. A session that is given no task at all starts no turn of its own — a role
 * brought back with none — and the bound is what keeps it from being unaddressable for longer
 * than that: it claims its name at the bound, and the lane kept for it is handed over then.
 */

/** 30 seconds: three times the longest launch measured on this deployment, where the slowest
 * launch read took 10 seconds from the daemon's launch to the task it refused (INFRA-1573's
 * spec, which reads 8 and 10 seconds for the two launches it measures). */
export const DEFAULT_CLAIM_FIRST_TURN_BOUND_MS = 30_000;

/** The log one bad value writes its `warn` to (`logger` from `@oh-my-pi/pi-utils` in production,
 * the pane's own log line in the Claude Code host). */
export interface ClaimFirstTurnBoundLog {
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

/** The one variable that moves the bound, in the `LEGION_*` family a Legion session's
 * environment carries, and one value for every role and both harnesses. A test that would not
 * wait 30 seconds sets it on the session it starts. For a deployment to move it from the
 * daemon's own environment, the daemon must copy the name on, as it copies each
 * `LEGION_STALL_*` (`packages/daemon/internal/daemon/specs.go`, `stallLimitVariables`); that
 * list does not hold this name. */
const BOUND_VARIABLE = "LEGION_CLAIM_FIRST_TURN_BOUND_MS";

/**
 * The bound this session's claim boot runs on, read from the session's own environment. The
 * variable is not required: an environment that names none runs on
 * {@link DEFAULT_CLAIM_FIRST_TURN_BOUND_MS}, and a value that is not an integer of 0 or more
 * leaves one `warn` naming the variable and that same default.
 *
 * 0 is a value the reader honours: the name is taken as soon as `claims/ready` has answered,
 * waiting for no turn at all, which is the order this change replaced.
 */
export function claimFirstTurnBoundMs(
  env: Record<string, string | undefined>,
  log: ClaimFirstTurnBoundLog
): number {
  const raw = env[BOUND_VARIABLE];
  if (raw === undefined) return DEFAULT_CLAIM_FIRST_TURN_BOUND_MS;
  const value = raw.trim() === "" ? Number.NaN : Number(raw);
  if (!Number.isInteger(value) || value < 0) {
    log.warn(
      `legion: ${BOUND_VARIABLE} is not an integer of 0 or more; using ${DEFAULT_CLAIM_FIRST_TURN_BOUND_MS}`,
      { value: raw }
    );
    return DEFAULT_CLAIM_FIRST_TURN_BOUND_MS;
  }
  return value;
}
