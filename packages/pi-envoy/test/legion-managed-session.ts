import type { PiApi } from "@legion/pi-shared/pi-types";

/**
 * A stand-in for what `@sjawhar/pi-legion`'s claim leaves in a session it drives: the transcript
 * entry this plugin reads a Legion-driven session by (`LEGION_MANAGED_ENTRY` in
 * `../extensions/envoy.ts`, which a claim writes through `markLegionManagedSession`).
 * `../extensions/cut-turn-omp.test.ts` loads it ahead of the Envoy entry, so the session it starts
 * on the real Oh My Pi is one Legion drives without the Legion entry, a daemon or a NATS server.
 */
export default function legionManagedSession(pi: PiApi): void {
  pi.on("session_start", async (_event, context) => {
    pi.appendEntry("legion-managed-session", {
      session_id: context.sessionManager.getSessionId(),
    });
  });
}
