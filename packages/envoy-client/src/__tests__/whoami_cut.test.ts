import { afterAll, beforeAll, describe, expect, setSystemTime, test } from "bun:test";
import {
  ASK_OPENED,
  callsAtCutProblems,
  leftOutParts,
  READ_COMMANDS,
  startAsOfStandIn,
} from "./as-of-stand-in";
import { ISSUE } from "./dispatch-command-surface-responses";

// A token that carries a cut reads Dispatch at that cut, whatever `--as-of` the command is given
// (dispatch://INFRA-4620, decision 6). Whoami names the cut, and each of the seven read commands,
// run with no `--as-of` and with a later one, sends the token's cut as `as_of` to each route of
// kind "answers", calls no route that holds no history, names the token's cut in its `Left out
// at` line, and counts from it. The test's clock stands years after both times.

/** The cut the token carries. */
const TOKEN_CUT = "2026-11-05T08:00:00Z";
/** A later `--as-of`, a week after the token's cut, which the token's cut replaces. */
const LATER = "2026-11-12T08:00:00Z";
const CLOCK = "2031-06-01T00:00:00Z";

const standIn = startAsOfStandIn("whoami-cut");

beforeAll(() => {
  setSystemTime(new Date(CLOCK));
});

afterAll(() => {
  setSystemTime();
  standIn.stop();
});

/** What a run under the token's cut printed that only a count from that cut prints: the answers'
 *  window, each open ask's age and the Inbox's left-out Later band. */
function countedFrom(argv: readonly string[], stdout: string): string[] {
  const opened = Date.parse(ASK_OPENED);
  const hours = Math.floor((Date.parse(TOKEN_CUT) - opened) / 3_600_000);
  const expected: Record<string, readonly string[]> = {
    answers: [
      `1 item people wrote to this session in the 72 hours before ${TOKEN_CUT}, newest first, word for word:`,
      "The words of an hour before.",
    ],
    "open-asks": [`- ${Math.floor(hours / 24)}d ${hours % 24}h · `],
    inbox: [`Later left out at ${TOKEN_CUT}`],
  };
  return (expected[argv[0] ?? ""] ?? []).filter((words) => !stdout.includes(words));
}

describe("a token's cut replaces --as-of", () => {
  for (const argv of READ_COMMANDS) {
    for (const asOf of [[], ["--as-of", LATER]]) {
      test(`dispatch ${[...argv, ...asOf].join(" ")} under a token cut at ${TOKEN_CUT}`, async () => {
        const run = await standIn.run([...argv, ...asOf], { whoamiCut: TOKEN_CUT });
        expect({
          code: run.code,
          calls: callsAtCutProblems(run, TOKEN_CUT),
          namesTheCut: leftOutParts(run.stdout, TOKEN_CUT) !== undefined,
          namesTheLaterTime: run.stdout.includes(LATER),
          notCountedFromTheCut: countedFrom(argv, run.stdout),
        }).toEqual({
          code: 0,
          calls: [],
          namesTheCut: true,
          namesTheLaterTime: false,
          notCountedFromTheCut: [],
        });
      });
    }
  }
});

// `dispatch issues` is no read command of the seven, so it reads no whoami and sends no `as_of`,
// but under a token that carries a cut Dispatch answers its listing as of that cut all the same,
// and leaves out where each route reaches. The listing then names the route, and says nothing of
// whether it reaches anyone: no value of the present, and no `undefined`.
describe("dispatch issues under a token's cut", () => {
  test("names a route whose reach the listing left out, and claims no reach for it", async () => {
    const argv = ["issues", "--project", "DSP"];
    const present = await standIn.run(argv);
    const past = await standIn.run(argv, { whoamiCut: TOKEN_CUT });
    const route = (stdout: string) =>
      stdout.split("\n").find((line) => line.startsWith(`${ISSUE} `)) ?? stdout;
    expect({
      present: [present.code, route(present.stdout).split(" · ").at(-1)],
      past: [past.code, route(past.stdout).split(" · ").at(-1)],
      undefinedPrinted: past.stdout.includes("undefined"),
      sentAsOf: past.calls.filter((call) => call.asOf.length > 0).map((call) => call.text),
    }).toEqual({
      present: [0, "route role:implementer (nobody holds it right now)"],
      past: [0, "route role:implementer (the listing does not say whether it reaches anyone)"],
      undefinedPrinted: false,
      sentAsOf: [],
    });
  });

  // `--json` and the call's details carry each row's route members. A listing at the cut left
  // them out, and a `null` there would say the present's "no route", "nobody holds it" or "no
  // reason": a member left out is left out of the row, and a listing of the present keeps null.
  test("--json leaves out of each row a route member the listing left out, and keeps null at the present", async () => {
    const argv = ["issues", "--project", "DSP", "--json"];
    const present = await standIn.run(argv);
    const past = await standIn.run(argv, { whoamiCut: TOKEN_CUT });
    const row = (stdout: string) => JSON.parse(stdout).issues[0];
    const reach = (listed: Record<string, unknown>) =>
      Object.fromEntries(
        ["route", "route_status", "route_holder", "route_status_reason"].map((name) => [
          name,
          name in listed ? listed[name] : "(not in the row)",
        ])
      );
    expect({
      present: [present.code, reach(row(present.stdout))],
      past: [past.code, reach(row(past.stdout))],
    }).toEqual({
      present: [
        0,
        {
          route: "role:implementer",
          route_status: "no_holder",
          route_holder: null,
          route_status_reason: null,
        },
      ],
      past: [
        0,
        {
          route: "role:implementer",
          route_status: "(not in the row)",
          route_holder: "(not in the row)",
          route_status_reason: "(not in the row)",
        },
      ],
    });
  });
});
