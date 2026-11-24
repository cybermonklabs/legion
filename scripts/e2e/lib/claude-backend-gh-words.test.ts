import { describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { expectNoMarker, MARKER } from "./marker-runs";
import { scriptFunctions } from "./script-functions";

// The Claude Code backend proofs' two gh reads whose words reach their output
// (claude-backend-tmux.sh and claude-backend-sandbox.sh): require_smoke_repo_read, the devbox's
// ordinary gh reading the smoke repository, and require_commit_outside_legion, which judges the
// paths of the implementer's commit that lib/workflow.sh's smoke_commit_files reads with gh. Each
// is taken from the script by name with the script's own note and fail, beside lib/workflow.sh
// sourced as the script sources it, and run under the script's `set -Eeuo pipefail` with a
// stand-in gh first on PATH. Whatever answers gh in GitHub's place holds gh's token and writes
// gh's stderr and its answer, so the stand-in puts MARKER in one of them, and the row holds all
// the run printed to no run of it. The twin is the file the run names, which holds the marker; the
// control is the stand-in's own log of the calls it answered.
const root = join(import.meta.dir, "..", "..", "..");
const dir = temporaryDirectory("claude-backend-gh-words-test");
let runs = 0;
const commit = "c".repeat(40);

// proof runs CALL after the script's FUNCTION, with the stand-in gh running GH.
function proof(script: string, fn: string, call: string, gh: string) {
  const functions = scriptFunctions(join(import.meta.dir, "..", script));
  const run = join(dir, `run-${++runs}`);
  const bin = join(run, "bin");
  const evidence = join(run, "evidence");
  const calls = join(run, "gh-calls");
  mkdirSync(bin, { recursive: true });
  mkdirSync(evidence);
  writeFileSync(join(bin, "gh"), `#!/bin/sh\nprintf '%s\\n' "$*" >>'${calls}'\n${gh}\n`, {
    mode: 0o755,
  });
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
root=${JSON.stringify(root)} work=${JSON.stringify(run)} evidence=${JSON.stringify(evidence)}
check=prerequisites repo=sjawhar/legion-smoke
${functions("note")}
${functions("fail")}
. "$root/scripts/e2e/lib/workflow.sh"
${functions(fn)}
${call}
echo "went on"`,
    ],
    {
      env: { PATH: `${bin}:${process.env.PATH ?? ""}`, TMPDIR: run },
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  return {
    code: result.exitCode,
    printed: result.stdout.toString() + result.stderr.toString(),
    evidence,
    calls: existsSync(calls) ? readFileSync(calls, "utf8") : "",
  };
}

const scripts = ["claude-backend-tmux.sh", "claude-backend-sandbox.sh"];

for (const script of scripts) {
  describe(`${script}'s read of the smoke repository prints no word of gh's`, () => {
    const read = (gh: string) =>
      proof(script, "require_smoke_repo_read", "require_smoke_repo_read", gh);

    test("gh's own words on a failed read are kept in a file the refusal names", () => {
      const run = read(`echo 'gh: ${MARKER} (HTTP 502)' >&2\nexit 1`);
      const kept = join(run.evidence, "gh", "repo-view.err");
      expect(run.code).toBe(1);
      expectNoMarker(run.printed);
      expect(run.printed).toContain(
        `FAIL prerequisites: the devbox's ordinary gh cannot read sjawhar/legion-smoke; gh's own words are in ${kept}`
      );
      expect(readFileSync(kept, "utf8")).toContain(MARKER);
      expect(run.calls).toBe("repo view sjawhar/legion-smoke --json name\n");
    });

    // gh writes to stderr on a read that succeeds too (the devbox shim names an inherited
    // GH_TOKEN there): the run goes on, saying nothing of it.
    test("a read that succeeds with words on stderr goes on and prints none of them", () => {
      const run = read(`echo '{"name":"legion-smoke"}'\necho 'gh: ${MARKER}' >&2\nexit 0`);
      expect(run.code).toBe(0);
      expectNoMarker(run.printed);
      expect(run.printed).toBe("went on\n");
      expect(readFileSync(join(run.evidence, "gh", "repo-view.err"), "utf8")).toContain(MARKER);
      expect(run.calls).toBe("repo view sjawhar/legion-smoke --json name\n");
    });
  });
}

for (const script of scripts) {
  describe(`${script}'s check of the implementer's commit prints no path gh answered`, () => {
    const files = `api repos/sjawhar/legion-smoke/commits/${commit} --jq .files[].filename`;
    const check = (paths: string) =>
      proof(
        script,
        "require_commit_outside_legion",
        `require_commit_outside_legion ${commit}`,
        `printf '%s\\n' ${paths}`
      );

    test("a path under .legion/ fails the check, naming the file that keeps the paths", () => {
      const run = check(`smoke/one.md '.legion/${MARKER}'`);
      const kept = join(run.evidence, "gh", "implementer-commit-files.answer");
      expect(run.code).toBe(1);
      expectNoMarker(run.printed);
      expect(run.printed).toContain(
        `the implementer's commit ${commit} changes files under .legion/, where no handoff goes now; the paths GitHub answered are in ${kept}`
      );
      expect(readFileSync(kept, "utf8")).toContain(MARKER);
      expect(run.calls).toBe(`${files}\n`);
    });

    // Control: a commit outside .legion/ passes and the run goes on.
    test("a commit outside .legion/ passes", () => {
      const run = check("smoke/one.md");
      expect(run.code).toBe(0);
      expect(run.printed).toBe("went on\n");
      expect(run.calls).toBe(`${files}\n`);
    });
  });
}
