/**
 * A stand-in daemon's owed answer and its two wait routes (dispatch://INFRA-2040), shared by the
 * suites that drive a declared wait and the design gate's hold through a host: the phase reminder
 * here (`src/phase-reminder.test.ts`), Oh My Pi's (`packages/pi-legion/extensions/legion.test.ts`
 * and `legion-phase-stall-omp.test.ts`) and Claude Code's
 * (`packages/claude-envoy/tests/legion-phase-hook.test.ts`). Each suite serves these answers
 * through its own fake daemon.
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import type { LegionOwed } from "@legion/contracts/legion-api";
import { z } from "zod";

/** The update round's ask as its card or frame carries its key: the key
 * `packages/contracts/fixtures/daemon-api/update-ask-key.json` holds, behind the listener's
 * role-forward prefix (D9 of dispatch://INFRA-2040's plan). The daemon's own test
 * (`TestUpdateAskKeyIsTheContractFixturesKey` in `packages/daemon/internal/record`) reads that
 * fixture and holds the key the daemon derives to it. The stand-in answers a report with this key
 * `ended: false` and `updateAsk: true`, as the daemon answers the key of its own update ask. */
export const UPDATE_ASK_KEY = `envoy.role.forward.${
  z
    .object({ key: z.string().min(1) })
    .parse(
      JSON.parse(
        readFileSync(
          path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/update-ask-key.json"),
          "utf8"
        )
      )
    ).key
}`;

/** What the stand-in holds, which a case moves: the wait that stands, the closed gate that holds
 * the claim, and how many of the next reports of news fail with a 503. */
export interface WaitStandInState {
  wait: LegionOwed["wait"];
  hold: LegionOwed["hold"];
  failReports: number;
}

export interface WaitStandIn {
  readonly state: WaitStandInState;
  /** The body of every report of news posted, in order. */
  readonly reports: unknown[];
  /** The answer to `POST /legion/v1/claims/owed`: the claim owes the stand-in's work, with the
   * wait while one stands and the hold while one is set. */
  readonly owed: () => Response;
  /** The answer to `POST /legion/v1/claims/wait` or `claims/wait-news` at `route`, `body` being
   * the request's body; undefined for every other route. */
  readonly answer: (route: string, body: unknown) => Response | undefined;
}

/**
 * A stand-in daemon whose claim owes `work`. Each declaration mints a wait (`wait-1`, `wait-2`,
 * …) that replaces the one standing. Each report of news is answered as the daemon answers it
 * (`(*server).waitNews` in `packages/daemon/internal/api/wait.go`): `ended: false` with
 * `updateAsk` for the update ask's key, `ended: true` for the wait that stands, which ends it, and
 * `ended: false` for any other. Each body comes from the real daemon client, which has parsed it
 * with the contract's request schema already, so the stand-in reads it as it comes.
 */
export function waitStandIn(work: string): WaitStandIn {
  const state: WaitStandInState = { wait: undefined, hold: undefined, failReports: 0 };
  const reports: unknown[] = [];
  let minted = 0;
  return {
    state,
    reports,
    owed: () =>
      Response.json({
        owes: true,
        work,
        ...(state.wait === undefined ? {} : { wait: state.wait }),
        ...(state.hold === undefined ? {} : { hold: state.hold }),
      }),
    answer: (route, body) => {
      const request: object = typeof body === "object" && body !== null ? body : {};
      if (route === "/legion/v1/claims/wait") {
        minted += 1;
        state.wait = {
          id: `wait-${minted}`,
          waitingOn: "waitingOn" in request ? String(request.waitingOn) : "",
          wakeAt: "wakeAt" in request ? String(request.wakeAt) : "",
          declaredAt: "2026-11-21T09:00:00.123456Z",
        };
        return Response.json({ wait: state.wait });
      }
      if (route !== "/legion/v1/claims/wait-news") return undefined;
      reports.push(body);
      if (state.failReports > 0) {
        state.failReports -= 1;
        return Response.json({ error: "the daemon is restarting" }, { status: 503 });
      }
      if ("deliveryKey" in request && request.deliveryKey === UPDATE_ASK_KEY) {
        return Response.json({ ended: false, updateAsk: true });
      }
      if (state.wait !== undefined && "waitId" in request && request.waitId === state.wait.id) {
        state.wait = undefined;
        return Response.json({ ended: true });
      }
      return Response.json({ ended: false });
    },
  };
}
