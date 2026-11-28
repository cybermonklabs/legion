import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdirSync } from "node:fs";
import { join } from "node:path";
import { SPEC_CHECK_OBSERVED_CUT_NOTE } from "@legion/contracts";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { commitSpec, DISPATCH_CLI, outcomes, runStep } from "../../test/ci-report-step";
import {
  type CIResultsStack,
  startCIResultsStack,
  useCIResultsStackDirectory,
} from "../../test/ci-results-stack";
import { dispatchStackEnabled } from "../../test/dispatch-stack";
import { buildGoBinaries } from "../../test/envoy-stack";

// The stored text of a test whose output is longer than 64 KiB starts at the output's own start,
// or says before the test's body that its start was cut. The report is a real `go test -json`
// run of a module example.com/acc, whole, its package's start and end events included, without
// which the reader gives no test a pass or a fail: TestSeventyK prints "START-MARKER of
// TestSeventyK", 70 lines of 1023 "s", "END-MARKER of TestSeventyK" and fails; TestThreeK prints
// its start marker, 3000 "k" and its end marker and fails, and its stored text is the control,
// which starts at its start whatever the reader keeps of a long output.
// test/fixtures/ci-report-long-output/make-report.sh makes it again from the package of
// test/fixtures/ci-report-long-output/cases. ci-report runs as the action runs it (C3), under the
// real `agent-secrets result-grant`, so the CLI writes through the broker's airlock proxy with the
// run's placeholder alone; the checks are those of
// test/fixtures/ci-report-long-output/spec/output.yaml.

const FIXTURES = join(import.meta.dir, "..", "..", "test", "fixtures", "ci-report-long-output");
const stackDirectory = useCIResultsStackDirectory();
const work = temporaryDirectory("ci-report-long-output");

/** Whether the stored text says where it starts: it starts with the test's own first line, or the
 *  text before the first character of the test's body holds the note that says the text was cut
 *  (SPEC_CHECK_OBSERVED_CUT_NOTE). */
function saysWhereItStarts(stored: string, firstLine: string, bodyChar: string): boolean {
  if (stored.startsWith(firstLine)) return true;
  const body = stored.indexOf(bodyChar.repeat(20));
  return body > 0 && stored.slice(0, body).includes(SPEC_CHECK_OBSERVED_CUT_NOTE);
}

describe.skipIf(!dispatchStackEnabled)("a long output of a test, as Dispatch keeps it", () => {
  let stack: CIResultsStack | undefined;
  const cliState = join(work, "cli-state");

  beforeAll(async () => {
    mkdirSync(cliState, { recursive: true });
    await buildGoBinaries({
      bin: join(work, "bin"),
      packages: ["./cmd/ci-report"],
      stack: "long output of a test",
      binaries: "./cmd/ci-report",
    });
    stack = await startCIResultsStack({ work: stackDirectory });
  }, 600_000);
  afterAll(async () => {
    await stack?.stop();
  }, 60_000);

  test("the stored text of an output over 64 KiB starts at the output's start, or says that its start was cut", async () => {
    if (stack === undefined) throw new Error("the CI results stack did not start");
    const dir = join(work, "checkout");
    const sha = commitSpec(work, dir, join(FIXTURES, "spec"), "long output");
    stack.setFiles(sha, join(dir, ".dispatch", "spec"));
    const reported = await runStep(stack, {
      ciReport: join(work, "bin", "ci-report"),
      dir,
      run: {
        sha,
        ref: "refs/heads/topic/long-output",
        job: "go-checks",
        runAttempt: 1,
        checkRunId: 4201,
      },
      report: join(FIXTURES, "go-test-long-output.jsonl"),
      job: "go-checks",
      dispatchCli: DISPATCH_CLI,
      cliState,
    });
    // Both tests failed, and each check's line and stored result say so: a report with no end of
    // its package would give each could_not_run.
    expect({ code: reported.code, lines: outcomes(reported) }).toEqual({
      code: 0,
      lines: ["seventy-k-output fail exit 0", "three-k-output fail exit 0"],
    });

    const rows = await stack.results(sha);
    const kept = Object.fromEntries(
      rows.map(({ attestation: { predicate } }) => [predicate.check, predicate])
    );
    expect({
      verdicts: rows.map(({ attestation: { predicate } }) => predicate.verdict),
      "three-k-output": saysWhereItStarts(
        kept["three-k-output"]?.observed ?? "",
        "=== RUN   TestThreeK\nSTART-MARKER",
        "k"
      ),
      "seventy-k-output": saysWhereItStarts(
        kept["seventy-k-output"]?.observed ?? "",
        "=== RUN   TestSeventyK\nSTART-MARKER",
        "s"
      ),
    }).toEqual({ verdicts: ["fail", "fail"], "three-k-output": true, "seventy-k-output": true });
  }, 180_000);
});
