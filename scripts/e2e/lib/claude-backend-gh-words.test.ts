import { describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { expectNoMarker, MARKER } from "./marker-runs";
import { scriptFunctions } from "./script-functions";

// The Claude Code backend proofs' read of the smoke repository through the devbox's ordinary gh
// (require_smoke_repo_read in claude-backend-tmux.sh and claude-backend-sandbox.sh), taken from
// each script by name with the script's own fail and run under its `set -Eeuo pipefail`, a
// stand-in gh first on PATH. Whatever answers gh in GitHub's place holds gh's token and writes
// gh's stderr, so the stand-in says MARKER there, and the row holds all the read printed to no run
// of it. The twin is the file the refusal names, which holds the marker; the control is the
// stand-in's own log of the one call it answered.
const dir = temporaryDirectory("claude-backend-gh-words-test");
let runs = 0;

function readRepo(script: string, gh: string) {
  const fn = scriptFunctions(join(import.meta.dir, "..", script));
  const run = join(dir, `run-${++runs}`);
  const bin = join(run, "bin");
  const evidence = join(run, "evidence");
  const calls = join(run, "gh-calls");
  mkdirSync(bin, { recursive: true });
  writeFileSync(join(bin, "gh"), `#!/bin/sh\nprintf '%s\\n' "$*" >>'${calls}'\n${gh}\n`, {
    mode: 0o755,
  });
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
check=prerequisites evidence=${JSON.stringify(evidence)} repo=sjawhar/legion-smoke
${fn("fail")}
${fn("require_smoke_repo_read")}
require_smoke_repo_read
echo "read $repo"`,
    ],
    { env: { PATH: `${bin}:${process.env.PATH ?? ""}` }, stdout: "pipe", stderr: "pipe" }
  );
  return {
    code: result.exitCode,
    printed: result.stdout.toString() + result.stderr.toString(),
    kept: join(evidence, "gh", "repo-view.err"),
    calls: existsSync(calls) ? readFileSync(calls, "utf8") : "",
  };
}

for (const script of ["claude-backend-tmux.sh"]) {
  describe(`${script}'s read of the smoke repository prints no word of gh's`, () => {
    test("gh's own words on a failed read are kept in a file the refusal names", () => {
      const run = readRepo(script, `echo 'gh: ${MARKER} (HTTP 502)' >&2\nexit 1`);
      expect(run.code).toBe(1);
      expectNoMarker(run.printed);
      expect(run.printed).toContain(
        `FAIL prerequisites: the devbox's ordinary gh cannot read sjawhar/legion-smoke; gh's own words are in ${run.kept}`
      );
      expect(readFileSync(run.kept, "utf8")).toContain(MARKER);
      expect(run.calls).toBe("repo view sjawhar/legion-smoke --json name\n");
    });

    // gh writes to stderr on a read that succeeds too (the devbox shim names an inherited
    // GH_TOKEN there): the run goes on, saying nothing of it.
    test("a read that succeeds with words on stderr goes on and prints none of them", () => {
      const run = readRepo(
        script,
        `echo '{"name":"legion-smoke"}'\necho 'gh: ${MARKER}' >&2\nexit 0`
      );
      expect(run.code).toBe(0);
      expectNoMarker(run.printed);
      expect(run.printed).toBe("read sjawhar/legion-smoke\n");
      expect(readFileSync(run.kept, "utf8")).toContain(MARKER);
      expect(run.calls).toBe("repo view sjawhar/legion-smoke --json name\n");
    });
  });
}
