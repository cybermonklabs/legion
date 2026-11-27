import { expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

// The nightly hosted proof verification-results.sh (.github/e2e-scripts.json) runs this driver,
// and no check of a pull request runs that proof. The driver reads a value of @legion/contracts by
// the file path of the module that exports it, since the root package does not depend on that
// package, so a value moved to another module fails the driver as it loads, before it reads one
// argument: dispatch://INFRA-1109 moved MAX_ISSUE_PAGE_LIMIT out of dispatch-api-limits.ts and
// the driver exited 1 with "Export named 'MAX_ISSUE_PAGE_LIMIT' not found". A driver that loads
// answers a call with no arguments with its usage line and exit 2, and touches nothing.
test("the verification-results driver loads and answers a call with no arguments with its usage", () => {
  const driver = fileURLToPath(new URL("../verification-results.ts", import.meta.url));
  const env: Record<string, string> = {};
  for (const [name, value] of Object.entries(process.env)) {
    if (value !== undefined && !name.startsWith("VERIFICATION_RESULTS_E2E_")) env[name] = value;
  }
  const run = Bun.spawnSync([process.execPath, driver], { env, stdout: "pipe", stderr: "pipe" });
  const stderr = run.stderr.toString();
  expect(stderr, stderr).toStartWith("usage: ");
  expect(run.exitCode, stderr).toBe(2);
});
