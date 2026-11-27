import { beforeAll, describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// lib/report-port.sh is the one reader of the port a process that bound port 0 itself logs. These
// rows run it as each of its callers has it: sourced alone, as envoy-session-delivery.sh sources
// it, and through lib/rig.sh.

const root = join(import.meta.dir, "..", "..", "..");
const dir = temporaryDirectory("report-port-test");
const pattern = "127\\.0\\.0\\.1:";
const logs = {
  /** One line naming two ports, as a service that logs its client and monitoring ports at once. */
  twoOnALine: join(dir, "two-on-a-line.log"),
  /** Two lines naming a port each, as a service that restarted into the same log. */
  twoLines: join(dir, "two-lines.log"),
  none: join(dir, "none.log"),
  missing: join(dir, "missing.log"),
};
beforeAll(() => {
  writeFileSync(logs.twoOnALine, "ready on 127.0.0.1:4222, monitoring on 127.0.0.1:8222\n");
  writeFileSync(
    logs.twoLines,
    "booting\nlistening on 127.0.0.1:5000\nlistening on 127.0.0.1:6000\n"
  );
  writeFileSync(logs.none, "booting\nstill booting\n");
});

/** What each caller sources to have the parser; $1 is the checkout. */
const callers = {
  "lib/report-port.sh alone": '. "$1/scripts/e2e/lib/report-port.sh"',
  "lib/rig.sh": `note() { :; }
fail() { echo "$*" >&2; exit 1; }
root=$1 evidence='' work='' timeout_hook=''
. "$root/scripts/e2e/lib/rig.sh"`,
};

/** Runs `call` in a shell that sourced `source`, with the log as $2 and the pattern as $3. */
function run(source: string, call: string, log: string): { code: number; out: string } {
  const result = Bun.spawnSync(
    ["bash", "-c", `set -euo pipefail\n${source}\n${call}`, "report-port", root, log, pattern],
    { stderr: "pipe", stdout: "pipe" }
  );
  return { code: result.exitCode, out: result.stdout.toString() };
}

const printed = 'report_port "$2" "$3"';
// The assignment's own status is the shell's, and VAR is printed whatever it was left holding.
const assigned =
  'port=unset; status=0; reported_port "$2" "$3" port || status=$?; printf \'%s %s\\n\' "$status" "$port"';

for (const [caller, source] of Object.entries(callers)) {
  describe(`the port a log names, through ${caller}`, () => {
    test("is the first of two on one line", () => {
      expect(run(source, printed, logs.twoOnALine)).toEqual({ code: 0, out: "4222\n" });
      expect(run(source, assigned, logs.twoOnALine)).toEqual({ code: 0, out: "0 4222\n" });
    });

    test("is the one on the first line that names one", () => {
      expect(run(source, printed, logs.twoLines)).toEqual({ code: 0, out: "5000\n" });
      expect(run(source, assigned, logs.twoLines)).toEqual({ code: 0, out: "0 5000\n" });
    });

    test("is no answer while no line names one, and the variable keeps what it held", () => {
      expect(run(source, printed, logs.none)).toEqual({ code: 1, out: "" });
      expect(run(source, assigned, logs.none)).toEqual({ code: 0, out: "1 unset\n" });
    });

    test("is no answer while the log is not there", () => {
      expect(run(source, printed, logs.missing)).toEqual({ code: 1, out: "" });
      expect(run(source, assigned, logs.missing)).toEqual({ code: 0, out: "1 unset\n" });
    });
  });
}
