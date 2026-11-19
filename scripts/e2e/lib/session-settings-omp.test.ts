import { expect, test } from "bun:test";
import { runMark } from "./session-settings-omp";

// runMark (lib/session-settings-omp.ts) is the run's mark the driver hands every Oh My Pi it
// starts. A driver started without it must stop at once: an Oh My Pi given an empty mark carries
// none, and dispatch-session-settings.sh's stop_omp, which signals a pid only while it carries the
// run's mark, would leave it running after the run.
test("runMark hands on the run's mark, and throws when the driver has none or an empty one", () => {
  expect(runMark({ LEGION_E2E_RUN: "/tmp/run" })).toBe("/tmp/run");
  for (const env of [{}, { LEGION_E2E_RUN: "" }]) {
    expect(() => runMark(env)).toThrow(/LEGION_E2E_RUN is not set/);
  }
});
