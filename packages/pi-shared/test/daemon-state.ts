/**
 * What a stub daemon answers, shared by the suites that stub the daemon's routes.
 *
 * The daemon's state as a stub daemon answers `GET /legion/v1/state` with (`stateAtPhase`) is
 * shared by the two suites that stub those routes for the `legion` tool's phase ops: pi-shared's
 * executor (`src/legion-phase-ops.test.ts`) and claude-envoy's host
 * (`packages/claude-envoy/tests/legion-tool.test.ts`). It is the golden `state.json` the daemon's
 * own test writes (`packages/contracts/fixtures/daemon-api`, `packages/daemon/internal/api`), with
 * one issue moved.
 *
 * A route that never answers (`answersNever`) is shared by the two suites that drive a phase
 * worker's turn end against a stub daemon: pi-legion's Oh My Pi pane
 * (`packages/pi-legion/extensions/legion.test.ts`) and claude-envoy's phase hook
 * (`packages/claude-envoy/tests/legion-phase-hook.test.ts`).
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";

/** The one member of the state these suites read, checked without stripping the rest: a stub
 * daemon serves the golden as it was written, so every other member rides through. */
const StateIssues = z.looseObject({ issues: z.record(z.string(), z.looseObject({})) });

/** `state.json` with LEGION-208 at `phase` and `record`'s fields over its own. */
export function stateAtPhase(
  phase: string,
  record: Record<string, unknown> = {}
): Record<string, unknown> {
  const state = StateIssues.parse(
    JSON.parse(
      readFileSync(
        path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/state.json"),
        "utf8"
      )
    )
  );
  return {
    ...state,
    issues: { ...state.issues, "LEGION-208": { ...state.issues["LEGION-208"], phase, ...record } },
  };
}

/** A route of the stub daemon that takes each request and never answers it. The request stays
 * open until its caller aborts it, and then fails as `fetch` does. `signals` holds the signal each
 * request carried, so a case reads whether its caller aborted it. */
export function answersNever(): {
  readonly route: (init: RequestInit | undefined) => Promise<Response>;
  readonly signals: (AbortSignal | undefined)[];
} {
  const signals: (AbortSignal | undefined)[] = [];
  const route = (init: RequestInit | undefined): Promise<Response> => {
    const signal = init?.signal ?? undefined;
    signals.push(signal);
    const open = Promise.withResolvers<Response>();
    signal?.addEventListener("abort", () => open.reject(signal.reason), { once: true });
    return open.promise;
  };
  return { route, signals };
}
