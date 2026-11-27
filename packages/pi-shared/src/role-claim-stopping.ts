// The refusal of a role claim once its session's stop has begun, in a module of its own that
// imports nothing of Oh My Pi: claude-envoy throws it too, and a channel server loads no Oh My Pi
// module (`./role-claim-bridge` reads the plugin interface, which imports `@oh-my-pi/pi-utils`).

/**
 * Why a role claim is refused once its session's stop has begun (R24 of dispatch://INFRA-3589):
 * the Envoy entry's claim throws it, for the bridge and for `envoy_role_set`, and so does the
 * bridge's notice subscription that lasts while the session holds a role (`whileHolding`), and
 * claude-envoy's `envoy_role_set`. The two hosts stop differently. The Envoy entry's stop hands
 * back every role the process holds, then deletes the session's record. claude-envoy's stop hands
 * back no role: it deletes the session's record, and a role claim it held stands until the
 * listener's live window passes with no renewal.
 */
export class RoleClaimStoppingError extends Error {
  override readonly name = "RoleClaimStoppingError";

  constructor(role: string) {
    super(`this session is stopping, so it claims no role: role ${role} is not held here`);
  }
}

/**
 * Whether `error` is a claim refused because its session's stop has begun
 * (`RoleClaimStoppingError`). It is read by its name, not by class, because it crosses plugin
 * bundles: the Envoy entry throws it from its own copy of this module, and the Legion entry reads
 * it from another.
 */
export function claimRefusedForStop(error: unknown): boolean {
  return error instanceof Error && error.name === "RoleClaimStoppingError";
}
