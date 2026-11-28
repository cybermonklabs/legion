import { expect, test } from "bun:test";
import { SESSION_LIVE_WINDOW_MS, THIRD_REFRESH_DONE_MS } from "./envoy-session";

// The session timings are one arrangement: a session's third refresh, three heartbeats and one
// whole listener call after its last registration, completes before the live window closes. The
// module computes that sum from the numbers it declares, so a number changed alone that breaks the
// arrangement fails here rather than leaving a sum in a comment behind.
test("a session's third refresh completes inside its live window", () => {
  expect(THIRD_REFRESH_DONE_MS).toBeLessThan(SESSION_LIVE_WINDOW_MS);
});
