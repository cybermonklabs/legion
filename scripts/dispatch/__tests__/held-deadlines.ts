import { expect, spyOn } from "bun:test";
import {
  bearerCredential,
  type RequestCredential,
} from "../../../packages/envoy-client/src/bearer.ts";
import { DISPATCH_TOOL_DEADLINE_MS } from "../../../packages/envoy-client/src/dispatch-http.ts";

/**
 * Each deadline a script's reads make, and where each went. A broker credential waiting on a
 * pod's missing registration stops at the signal it is handed, so a read must hand it the very
 * deadline that bounds the request: any other signal, one that never ends included, still passes
 * a check that the credential received an AbortSignal. So `AbortSignal.timeout` answers a signal
 * the test holds, the credential records the signal each call hands it, and `fetch` is spied to
 * read each request's signal back: in place of sending it when `answer` is given, and around the
 * real one otherwise.
 */
export interface HeldDeadlines {
  /** The credential to read with: a bearer that records each call's signal. */
  readonly credential: RequestCredential;
  /** Holds that each request handed its credential and its fetch the deadline it made. */
  expectEachRequestBoundByItsDeadline(reads: string): void;
  restore(): void;
}

export function holdDeadlines(answer?: () => Response): HeldDeadlines {
  const made: { readonly ms: number; readonly signal: AbortSignal }[] = [];
  const timeout = spyOn(AbortSignal, "timeout").mockImplementation((ms: number) => {
    const signal = new AbortController().signal;
    made.push({ ms, signal });
    return signal;
  });
  const fetched = spyOn(globalThis, "fetch");
  if (answer !== undefined) {
    fetched.mockImplementation((async (_input: RequestInfo | URL, _init?: RequestInit) =>
      answer()) as typeof fetch);
  }
  const given: (AbortSignal | undefined)[] = [];
  const bearer = bearerCredential("token");
  return {
    credential: {
      ...bearer,
      headers: (method, url, session, signal) => {
        given.push(signal);
        return bearer.headers(method, url, session, signal);
      },
    },
    expectEachRequestBoundByItsDeadline(reads) {
      const sent = fetched.mock.calls.map(([, init]) => init?.signal);
      expect(made.length, `${reads}: no deadline was made`).toBeGreaterThan(0);
      expect(
        made.map(({ ms }) => ms),
        `${reads}: a deadline is not the one DispatchClient puts on its requests`
      ).toEqual(made.map(() => DISPATCH_TOOL_DEADLINE_MS));
      expect(given.length, `${reads}: credential calls, one per deadline made`).toBe(made.length);
      expect(sent.length, `${reads}: requests sent, one per deadline made`).toBe(made.length);
      made.forEach(({ signal }, i) => {
        expect(
          given[i],
          `${reads}: request ${i + 1} handed its credential a signal other than its deadline`
        ).toBe(signal);
        expect(
          sent[i],
          `${reads}: request ${i + 1} was sent bound by a signal other than its deadline`
        ).toBe(signal);
      });
    },
    restore() {
      timeout.mockRestore();
      fetched.mockRestore();
    },
  };
}
