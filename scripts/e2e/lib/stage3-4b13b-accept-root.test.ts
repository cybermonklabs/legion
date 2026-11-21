import { expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptFunctions } from "./script-functions";

// stage3-4b13b-acceptance.sh sources ACCEPT_ROOT's scripts/e2e/lib/dispatch-identity.sh, which can be
// another, older checkout's. Its require_dispatch_identity fails before the run starts anything,
// naming ACCEPT_ROOT, where that lib predates Dispatch's own listener: an older lib's
// dispatch_identity_start took no listener and set the listener's verifier for a listener holding the
// shared token, and it had no dispatch_identity_stop, so the run would start Dispatch with no
// listener of its own and die on "command not found" in its teardown.
const script = join(import.meta.dir, "..", "stage3-4b13b-acceptance.sh");
const requireDispatchIdentity = scriptFunctions(script)("require_dispatch_identity");
const dir = temporaryDirectory("stage3-4b13b-accept-root-test");

/** Runs the guard after LIB is sourced, with root naming the checkout the lib came from. */
function guard(lib: string, root: string) {
  const run = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail\nroot=${JSON.stringify(root)}\nfail() { echo "FAIL: $*"; exit 1; }\n. ${JSON.stringify(lib)}\n${requireDispatchIdentity}\nrequire_dispatch_identity\necho "went on"`,
    ],
    { env: { PATH: process.env.PATH ?? "" } }
  );
  return { code: run.exitCode, out: run.stdout.toString(), err: run.stderr.toString() };
}

test("a lib from before Dispatch's own listener is refused, naming ACCEPT_ROOT", () => {
  // The older lib's shape: a start that takes no arguments, an issuer pid, and no stop.
  const older = join(dir, "older-dispatch-identity.sh");
  writeFileSync(older, "dispatch_identity_issuer_pid=\ndispatch_identity_start() { :; }\n");
  expect(guard(older, "/older/checkout")).toEqual({
    code: 1,
    out: "FAIL: ACCEPT_ROOT's scripts/e2e/lib/dispatch-identity.sh (/older/checkout) predates Dispatch's own listener (dispatch_identity_stop): point ACCEPT_ROOT at a checkout that has it\n",
    err: "",
  });
});

test("this checkout's lib passes", () => {
  const root = join(import.meta.dir, "..", "..", "..");
  expect(guard(join(import.meta.dir, "dispatch-identity.sh"), root)).toEqual({
    code: 0,
    out: "went on\n",
    err: "",
  });
});
