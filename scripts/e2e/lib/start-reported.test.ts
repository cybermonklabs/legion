import { expect, test } from "bun:test";
import { mkdirSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// start_reported (lib/rig.sh) starts a server that binds a port the kernel chooses and names it in
// its log, and assigns the port it named. Every start of a service appends to the service's one
// log, so a second start under the same name finds the first start's line there as well, and the
// port it assigns must be the one its own start named. The stand-in server writes a number where a
// server writes its port and exits a second later by itself: no program dials the number, and no
// signal has to stop the stand-in. The run waits for the first start to exit before the second, as
// a restart does.
const rig = join(import.meta.dir, "rig.sh");
const root = join(import.meta.dir, "..", "..", "..");
const dir = temporaryDirectory("start-reported-test");

test("start_reported assigns the port its own start named, not an earlier start's in the same log", () => {
  const evidence = join(dir, "evidence");
  mkdirSync(join(evidence, "logs"), { recursive: true });
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
root=${JSON.stringify(root)}
work=${JSON.stringify(dir)}
evidence=${JSON.stringify(evidence)}
timeout_hook=
note() { :; }
fail() { echo "FAIL: $*" >&2; exit 1; }
. ${JSON.stringify(rig)}
for number in 11 22; do
  start_reported stand_in port 'stand-in listener on 127\\.0\\.0\\.1:' -- \\
    sh -c "echo 'stand-in listener on 127.0.0.1:$number'; sleep 1"
  echo "$port"
  wait "$stand_in_pid"
done`,
    ],
    { stdout: "pipe", stderr: "pipe" }
  );
  expect(ran.stderr.toString()).toBe("");
  expect(ran.exitCode).toBe(0);
  expect(ran.stdout.toString()).toBe("11\n22\n");
}, 30_000);
