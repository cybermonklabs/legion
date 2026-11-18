import { describe, expect, test } from "bun:test";
import { dispatchLine, inDirectory } from "@legion/envoy-client/dispatch-command";
import { scopedCommand } from "./askgate-core";

// When a write may have landed, the `dispatch` command prints the command that sends it again
// (`Run: cd <dir> && dispatch … --idempotency-key <key>`), built by `inDirectory` and
// `dispatchLine`. In a session with AskGate on, a write posts only when the gate reads its bash
// call as one literal call, so that command must be in the gate's literal grammar whatever its
// words hold: an apostrophe, a `!`, a `=word`, a directory with a space or an apostrophe.

const KEY = "0b9c1e7a-55f4-4d2e-9a6b-3c2f1d0e8a71";

describe("the command that sends a write again", () => {
  test("AskGate reads it as one literal call", () => {
    for (const [dir, words] of [
      [
        "/tmp/work",
        ["message", "--issue", "DSP-1", "--body", "it's done, isn't it!", "--idempotency-key", KEY],
      ],
      [
        "/tmp/it's a dir",
        [
          "comment",
          "--issue",
          "DSP-1",
          "--body",
          "=ls and 'quoted'",
          "--image",
          "pic.png",
          "--idempotency-key",
          KEY,
        ],
      ],
    ] as const) {
      const line = inDirectory(dir, dispatchLine(words));
      expect(scopedCommand(line)?.tool, line).toBe(`dispatch_${words[0]}`);
    }
  });

  // The control: the `'\''` form the CLI wrote before is outside the grammar, so a test that reads
  // the printed command through the gate can fail.
  test("the backslash form is not in the gate's grammar", () => {
    const line = `cd /tmp/work && dispatch message --issue DSP-1 --body 'it'\\''s done' --idempotency-key ${KEY}`;
    expect(scopedCommand(line)).toBeUndefined();
  });
});
