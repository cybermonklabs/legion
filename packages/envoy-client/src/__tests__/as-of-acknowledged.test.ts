import { afterAll, describe, expect, test } from "bun:test";
import { DISPATCH_AS_OF_HEADER } from "@legion/contracts";
import { READ_COMMANDS, startAsOfStandIn } from "./as-of-stand-in";

// A Dispatch older than `as_of` ignores the parameter and answers the present, with no
// `Dispatch-As-Of` header (dispatch://INFRA-4620, decision 4). Each of the seven read commands
// refuses such an answer: it exits 1, names the route the answer came from, and prints nothing
// of the answer.

const CUT = "2026-11-10T12:00:00Z";

const standIn = startAsOfStandIn("as-of-acknowledged");

afterAll(() => standIn.stop());

describe("an answer that does not name the cut is refused", () => {
  for (const argv of READ_COMMANDS) {
    test(`dispatch ${argv.join(" ")} --as-of ${CUT}`, async () => {
      const run = await standIn.run([...argv, "--as-of", CUT], { acknowledges: false });
      const output = `${run.stdout}${run.stderr}`;
      // The route the refusal names is one the command asked at the cut: its path and query as
      // sent, `as_of` among them.
      const named = run.calls.filter(
        (call) => call.asOf.length > 0 && output.includes(call.text.slice("GET ".length))
      );
      expect({
        code: run.code,
        named: named.length > 0,
        refused: output.includes(`and its answer has no ${DISPATCH_AS_OF_HEADER} header`),
        lines: output.trimEnd().split("\n").length,
      }).toEqual({ code: 1, named: true, refused: true, lines: 1 });
    });
  }
});
