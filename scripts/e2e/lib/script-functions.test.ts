/** A library a script sources is found by resolving the path the script writes, against the
 *  checkout for `$root` and against the script's own directory for any other variable. Reading
 *  such a path by its last segments names a different file where the path holds `..`, and a
 *  library read under the wrong name hides what the real one holds.
 */

import { expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { rmSync, writeFileSync } from "node:fs";
import { join, relative } from "node:path";
import { libRowBoundMs, rowWaits } from "./row-waits";
import { sourcedLibraries } from "./script-functions";

rowWaits(libRowBoundMs);

const checkout = join(import.meta.dir, "..", "..", "..");

/** The libraries a script holding `lines` sources, as paths under the checkout. The script is
 *  written in the checkout's own scripts/e2e/lib, so a path written against the script's own
 *  directory lands where it would in the checkout, and under a name of this run alone, so two
 *  runs never read each other's. */
function sourcedBy(...lines: string[]): string[] {
  const script = join(checkout, "scripts", "e2e", "lib", `sourced-path-control-${randomUUID()}.sh`);
  try {
    writeFileSync(script, `#!/usr/bin/env bash\n${lines.join("\n")}\n`);
    return sourcedLibraries(script)
      .filter((path) => path !== script)
      .map((path) => relative(checkout, path));
  } finally {
    rmSync(script, { force: true });
  }
}

// #4296's loopback-dsn.sh writes this line. Read by its last segments it names
// scripts/e2e/lib/pg-url.sh, a file that does not exist; resolved, it names the one it sources.
test("a sourced path that climbs out of its own directory names the file it reaches", () => {
  expect(sourcedBy('. "$psql_env_dir/../../../packages/envoy/scripts/lib/pg-url.sh"')).toEqual([
    "packages/envoy/scripts/lib/pg-url.sh",
  ]);
});

// The shape INFRA-3862's coordinator reports: a script sourcing a library of another package by a
// path holding /lib/<name>.sh. Matched by its tail it reads as a library of scripts/e2e/lib,
// which is where the suite's ENOENT came from.
test("a sourced library of another package is read from that package, not from scripts/e2e/lib", () => {
  expect(sourcedBy('. "$co/packages/envoy/scripts/lib/pg-url.sh"')).toEqual([
    "packages/envoy/scripts/lib/pg-url.sh",
  ]);
});
test("a sourced path against the checkout names the file it reaches", () => {
  expect(sourcedBy('. "$root/packages/envoy/scripts/lib/exec-env.sh"')).toEqual([
    "packages/envoy/scripts/lib/exec-env.sh",
  ]);
});

test("a sourced path against the script's own directory names the file beside it", () => {
  expect(sourcedBy('. "$lib/port-file.sh"')).toEqual(["scripts/e2e/lib/port-file.sh"]);
});
