import { afterAll, describe, expect, test } from "bun:test";
import {
  DISPATCH_AS_OF_FILLED_THROUGH_HEADER,
  DISPATCH_AS_OF_NOT_SERVED,
  DISPATCH_AS_OF_SEARCH_NOT_FILLED,
  DISPATCH_AS_OF_TOKEN_NOT_SERVED,
  DISPATCH_INVALID_AS_OF,
} from "@legion/contracts";
import { RESULTS_AT_AS_OF_PROBLEM, resultsAtACutProblem } from "../dispatch-commands/as-of";
import { READ_COMMANDS, READ_FORMS, type Run, refusal, startAsOfStandIn } from "./as-of-stand-in";
import { ISSUE } from "./dispatch-command-surface-responses";

// Dispatch's refusals of a read at a cut (dispatch://INFRA-4620): each read command prints the
// code and says in words what it means for the read, and exits 1. A search the keyword table
// does not reach yet names the time the table reaches, which Dispatch sends in
// `Dispatch-As-Of-Filled-Through`, or says that the table reaches no time yet where Dispatch
// sends no such header. `results --at` with a cut is refused before any call but whoami.

const CUT = "2026-11-10T12:00:00Z";
const TOKEN_CUT = "2026-11-05T08:00:00Z";
/** The time the keyword table is filled through, which only the header names. */
const FILLED_THROUGH = "2026-11-01T00:00:00.000000Z";

const standIn = startAsOfStandIn("as-of-refusals");

afterAll(() => standIn.stop());

function output(run: Run): string {
  return `${run.stdout}${run.stderr}`;
}

/** The refusal, the words it is printed with, and how the run reaches it. */
interface Refused {
  readonly code: string;
  readonly status: number;
  readonly asOf: string;
  readonly whoamiCut?: string;
  /** Words the printed refusal holds beside the code, which Dispatch's own message does not. */
  readonly words: string;
}

const REFUSALS: readonly Refused[] = [
  {
    code: DISPATCH_AS_OF_NOT_SERVED,
    status: 400,
    asOf: CUT,
    words: `gives no answer as of ${CUT}`,
  },
  {
    code: DISPATCH_INVALID_AS_OF,
    status: 400,
    asOf: "yesterday",
    words: "Dispatch cannot read yesterday as a past time",
  },
  {
    // A Dispatch whose read rows refuse every token that carries a cut (INFRA-4619's head).
    code: DISPATCH_AS_OF_TOKEN_NOT_SERVED,
    status: 403,
    asOf: CUT,
    whoamiCut: TOKEN_CUT,
    words: `This token reads Dispatch as of ${TOKEN_CUT}`,
  },
];

describe("a refusal of a read at a cut is printed in words", () => {
  for (const { code, status, asOf, whoamiCut, words } of REFUSALS) {
    for (const argv of READ_COMMANDS) {
      test(`dispatch ${argv.join(" ")} --as-of ${asOf}: ${code}`, async () => {
        const run = await standIn.run([...argv, "--as-of", asOf], {
          ...(whoamiCut === undefined ? {} : { whoamiCut }),
          refuseAtCut: () => refusal(status, code, "as_of: the server refuses this read"),
        });
        expect({
          code: run.code,
          start: output(run).startsWith(`${code}: `),
          words: output(run).includes(words),
          said: output(run).includes("as_of: the server refuses this read"),
        }).toEqual({ code: 1, start: true, words: true, said: true });
      });
    }
  }

  test("a search the keyword table does not reach yet names the time it reaches", async () => {
    const run = await standIn.run([...READ_FORMS.search, "--as-of", CUT], {
      refuseAtCut: () => {
        const refused = refusal(
          409,
          DISPATCH_AS_OF_SEARCH_NOT_FILLED,
          "as_of: the keyword table does not reach this time yet"
        );
        refused.headers.set(DISPATCH_AS_OF_FILLED_THROUGH_HEADER, FILLED_THROUGH);
        return refused;
      },
    });
    expect({
      code: run.code,
      start: output(run).startsWith(`${DISPATCH_AS_OF_SEARCH_NOT_FILLED}: `),
      words: output(run).includes(`cannot search as of ${CUT} yet`),
      filledThrough: output(run).includes(FILLED_THROUGH),
    }).toEqual({ code: 1, start: true, words: true, filledThrough: true });
  });

  test("a search the keyword table does not reach at all says it reaches no time yet", async () => {
    // Dispatch sends no `Dispatch-As-Of-Filled-Through` where the fill has reached no time.
    const run = await standIn.run([...READ_FORMS.search, "--as-of", CUT], {
      refuseAtCut: () =>
        refusal(
          409,
          DISPATCH_AS_OF_SEARCH_NOT_FILLED,
          "as_of: the keyword table has reached no time yet"
        ),
    });
    expect({
      code: run.code,
      start: output(run).startsWith(`${DISPATCH_AS_OF_SEARCH_NOT_FILLED}: `),
      words: output(run).includes(
        `cannot search as of ${CUT} yet: its table of past text reaches no time yet. Search again later.`
      ),
    }).toEqual({ code: 1, start: true, words: true });
  });
});

describe("results --at with a cut", () => {
  test("--at beside --as-of is refused before any call", async () => {
    const run = await standIn.run(["results", "--issue", ISSUE, "--at", "abc1234", "--as-of", CUT]);
    expect({
      code: run.code,
      calls: run.calls.map((call) => call.text),
      says: output(run).includes(RESULTS_AT_AS_OF_PROBLEM),
    }).toEqual({ code: 2, calls: [], says: true });
  });

  // An issue named by an external reference is resolved through Dispatch, so a refusal after the
  // owner's resolution makes that call before it.
  for (const issue of [ISSUE, "owner/repo#12"]) {
    test(`--at under a token that carries a cut is refused after whoami alone: --issue ${issue}`, async () => {
      const run = await standIn.run(["results", "--issue", issue, "--at", "abc1234"], {
        whoamiCut: TOKEN_CUT,
      });
      expect({
        code: run.code,
        calls: run.calls.map((call) => call.text),
        says: output(run).includes(resultsAtACutProblem(TOKEN_CUT)),
      }).toEqual({ code: 2, calls: ["GET /api/v1/whoami"], says: true });
    });
  }
});
