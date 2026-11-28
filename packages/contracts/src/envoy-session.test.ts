import { expect, test } from "bun:test";
import {
  LISTENER_CALL_ATTEMPTS,
  LISTENER_CALL_BUDGET_MS,
  LISTENER_CALL_RETRY_DELAY_MS,
  LISTENER_CALL_TIMEOUT_MS,
  SESSION_HEARTBEAT_MS,
  SESSION_LIVE_WINDOW_MS,
  THIRD_REFRESH_DONE_MS,
} from "./envoy-session";

// The session timings are one arrangement: a session's third refresh, three heartbeats and one
// whole listener call after its last registration, completes before the live window closes. The
// sums are the module's own, from the numbers it declares, so a number changed alone fails here
// rather than leaving a sum in a comment behind.
test("a session's third refresh completes inside its live window", () => {
  expect(LISTENER_CALL_BUDGET_MS).toBe(
    LISTENER_CALL_ATTEMPTS * LISTENER_CALL_TIMEOUT_MS +
      (LISTENER_CALL_ATTEMPTS - 1) * LISTENER_CALL_RETRY_DELAY_MS
  );
  expect(THIRD_REFRESH_DONE_MS).toBe(3 * SESSION_HEARTBEAT_MS + LISTENER_CALL_BUDGET_MS);
  expect(THIRD_REFRESH_DONE_MS).toBeLessThan(SESSION_LIVE_WINDOW_MS);
});
