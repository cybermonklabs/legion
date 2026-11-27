import { expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { basename, join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { holdsMarker, marker } from "./gh-words";
import { expectNoMissingHelpers, nameMissingHelpers, scriptFunctions } from "./script-functions";

// The harness of the Stage 3 proofs' gh sites (stage3-4b13b-gh-words.test.ts,
// stage3-devbox-gh-words.test.ts): a proof prints no word a gh call answered or said. Whatever
// answers gh's call in GitHub's place holds gh's token and writes gh's stderr (the answer's
// message, or the client's error) and its answer, so either can carry the token. A row runs a site
// of a script as the script writes it, read out of the script by the lines that bound it
// (scriptSpan) or called as the function it is, under the script's own `set` line and ERR trap,
// with the libraries it sources (lib/rig.sh and lib/workflow.sh) and every function it defines,
// and a stand-in gh first on PATH whose stderr or answer for one call holds the marker
// not-a-secret-token. No run of eight of its characters may be in what the run printed, stdout and
// stderr together. The stand-in records each call (the control that it is the gh the site runs)
// and all it wrote (the twin: the words the site was handed do hold the marker), and a row that
// keeps the words in a file holds that file to the marker too, so the operator still finds them. A
// site's controls run it with no marker anywhere and hold it to how it ended before: the fix
// changes only what is printed.

export { marker };

const e2e = join(import.meta.dir, "..");
const root = join(e2e, "..", "..");

/**
 * Reads the script `script` under scripts/e2e once and returns `span`, which gives the lines from
 * the one line that matches `from` up to, not including, the first line after it that matches
 * `before`: a site of the script's top level, or of a long function's body, as the script writes
 * it, wherever it moves. A row's anchors match the script before its fix and after it, so one test
 * file runs at both. `span` throws, naming the script, when `from` matches no line or more than
 * one, or no line after it matches `before`.
 */
export function scriptSpan(script: string): (from: RegExp, before: RegExp) => string {
  const lines = readFileSync(join(e2e, script), "utf8").split("\n");
  return (from, before) => {
    const starts = lines.flatMap((line, at) => (from.test(line) ? [at] : []));
    if (starts.length !== 1) {
      throw new Error(`${script} holds ${starts.length} lines matching ${from}, want one`);
    }
    const start = starts[0];
    const end = lines.findIndex((line, at) => at > start && before.test(line));
    if (end < 0) throw new Error(`${script} holds no line matching ${before} after ${from}`);
    return lines.slice(start, end).join("\n");
  };
}

/**
 * The shell a site of the script at `path` runs in: the script's `set` line, the libraries it
 * sources that hold the helpers its gh sites call, every function the script defines, as it writes
 * each (after the libraries, as the script defines them), and its top-level ERR trap, which a
 * failed command of the site fires as the script's own does.
 */
function scriptShell(path: string): { set: string; body: string; trap: string } {
  const text = readFileSync(path, "utf8");
  const set = /^set -.*$/m.exec(text);
  const trap = /^trap '.*' ERR$/m.exec(text);
  if (set === null || trap === null) {
    throw new Error(`${basename(path)} has no top-level set line or ERR trap`);
  }
  const fn = scriptFunctions(path);
  const names = new Set([...text.matchAll(/^([A-Za-z_][A-Za-z0-9_]*)\(\) \{/gm)].map((m) => m[1]));
  const body = `. "$root/scripts/e2e/lib/rig.sh"
. "$root/scripts/e2e/lib/workflow.sh"
${[...names].map((name) => fn(name)).join("\n")}`;
  return { set: set[0], body, trap: trap[0] };
}

/** What the stand-in gh does on one call: writes `out` and `err`, then exits `code`. */
export interface Answer {
  out?: string;
  err?: string;
  code?: number;
}
/** The stand-in's answers to each call whose words hold `match`, the first rule that matches
 *  deciding: the nth such call gets the nth answer, and every call after the last the last. */
export interface Rule {
  match: string;
  answers: Answer[];
}

/** A run of a site: its stand-in's answers, and what the site must do with them. */
export interface Row {
  name: string;
  gh: Rule[];
  /** Shell after the site's own setup, for this row alone. */
  setup?: string;
  /** A call the stand-in must have answered: the control that it is the gh the site runs. */
  called: string;
  /** The status the run ends with, as the site ended before it printed no word of gh's. */
  code: number;
  /** Words the run must print: the site's own words for what it found. */
  says?: string[];
  /** A file under the evidence directory that keeps the words the site no longer prints, which
   *  the run names and which holds the marker. */
  kept?: string;
  /** A file under the evidence directory the run names, whose words are a helper's of
   *  lib/workflow.sh: what it holds is that helper's to decide. */
  names?: string;
}

interface Run {
  code: number | null;
  output: string;
  calls: string;
  said: string;
  evidence: string;
}

export const sha = (digit: string) => digit.repeat(40);
/** gh's stderr for a refused call, the marker in the answer's message. */
export const said = (): Answer => ({ err: `gh: Bad Gateway: ${marker} (HTTP 502)\n`, code: 1 });
export const answered = (out: string): Answer => ({ out: `${out}\n` });

/**
 * Reserves the calling test file's scratch directory and returns its `siteRows`. A test file calls
 * it once, at its own module scope: `temporaryDirectory` registers the hooks that make and remove
 * the directory where it is called.
 */
export function siteHarness(prefix: string) {
  const dir = temporaryDirectory(prefix);
  let runs = 0;

  /** Runs CODE of SCRIPT, after SETUP, in a run directory of its own with the stand-in gh first
   *  on PATH answering by GH. */
  function runSite(script: string, setup: string, code: string, gh: Rule[]): Run {
    const run = join(dir, `run-${++runs}`);
    const bin = join(run, "bin");
    const evidence = join(run, "evidence");
    mkdirSync(bin, { recursive: true });
    mkdirSync(join(run, "work"));
    mkdirSync(evidence);
    writeFileSync(join(run, "calls"), "");
    writeFileSync(join(run, "said"), "");
    const cases = gh.map((rule, at) => {
      if (/["$`\\]/.test(rule.match)) {
        throw new Error(`a rule's match holds a shell quote: ${rule.match}`);
      }
      rule.answers.forEach((answer, n) => {
        const file = join(run, `answer-${at}-${n}`);
        writeFileSync(`${file}.out`, answer.out ?? "");
        writeFileSync(`${file}.err`, answer.err ?? "");
        writeFileSync(`${file}.code`, String(answer.code ?? 0));
      });
      return `  *"${rule.match}"*) rule=${at} last=${rule.answers.length - 1} ;;`;
    });
    // The stand-in names no word of a call no rule answers: a word a site passed it is the site's
    // own. A call that hands it its input (`--input -`) has it read whole first, as gh does.
    writeFileSync(
      join(bin, "gh"),
      `#!/usr/bin/env bash
args="$*"
printf '%s\\n' "$args" >>'${run}/calls'
case "$args" in *"--input -"*) cat >/dev/null ;; esac
case "$args" in
${cases.join("\n")}
  *) echo 'stand-in gh: a call no rule answers' >&2; exit 3 ;;
esac
n=$(cat '${run}/count-'"$rule" 2>/dev/null || echo 0)
printf '%s' "$((n + 1))" >'${run}/count-'"$rule"
[ "$n" -le "$last" ] || n=$last
answer='${run}/answer-'"$rule-$n"
cat "$answer.out"
cat "$answer.err" >&2
cat "$answer.out" "$answer.err" >>'${run}/said'
exit "$(cat "$answer.code")"
`,
      { mode: 0o755 }
    );
    const path = join(e2e, script);
    const shell = scriptShell(path);
    // The run is a file, not an argument: the libraries and the script's functions are more text
    // than one argument may carry. Both scripts name the smoke repository before they source
    // lib/workflow.sh, which reads it.
    writeFileSync(
      join(run, "site.sh"),
      `${shell.set}
root=${JSON.stringify(root)} work=${JSON.stringify(join(run, "work"))} evidence=${JSON.stringify(evidence)} bin=${JSON.stringify(bin)}
check=site check_started=2026-01-01T00:00:00Z
repo=sjawhar/legion-smoke
${nameMissingHelpers(path)}
${shell.body}
${setup}
${shell.trap}
site() {
${code}
}
site
`
    );
    const result = Bun.spawnSync(["bash", join(run, "site.sh")], {
      env: { ...process.env, PATH: `${bin}:${process.env.PATH}` },
    });
    const output = `${result.stdout}${result.stderr}`;
    expectNoMissingHelpers(output);
    return {
      code: result.exitCode,
      output,
      calls: readFileSync(join(run, "calls"), "utf8"),
      said: readFileSync(join(run, "said"), "utf8"),
      evidence,
    };
  }

  /** The control every run of a site answers: it ran the stand-in, which got the call the row is
   *  about, and it ended as the site did before it printed no word of gh's, printing its words. */
  function expectSiteRan(run: Run, row: Row): void {
    if (!run.calls.includes(row.called)) {
      throw new Error(
        `the stand-in gh got no call holding "${row.called}"; the run printed:\n${run.output}`
      );
    }
    expect({ code: run.code, output: run.output }).toMatchObject({ code: row.code });
    for (const words of row.says ?? []) expect(run.output).toContain(words);
  }

  /** The rows of a site of SCRIPT, each a run whose stand-in's words for one call hold the
   *  marker, and its controls, each a run with no marker anywhere. SETUP is the site's variables
   *  and the stubs of what it calls beside gh. */
  return function siteRows(
    script: string,
    code: () => string,
    setup: string,
    rows: Row[],
    controls: Row[] = []
  ) {
    test.each(rows)("$name", (row) => {
      const run = runSite(script, `${setup}\n${row.setup ?? ""}`, code(), row.gh);
      // The twin: the words the stand-in handed the site hold the marker.
      expect(holdsMarker(run.said)).toBe(true);
      if (holdsMarker(run.output)) {
        throw new Error(`the marker reached what the site printed:\n${run.output}`);
      }
      expectSiteRan(run, row);
      for (const file of [row.kept, row.names]) {
        if (file !== undefined) expect(run.output).toContain(join(run.evidence, file));
      }
      if (row.kept !== undefined) {
        expect(holdsMarker(readFileSync(join(run.evidence, row.kept), "utf8"))).toBe(true);
      }
    });
    if (controls.length > 0) {
      test.each(controls)("control: $name", (row) => {
        const run = runSite(script, `${setup}\n${row.setup ?? ""}`, code(), row.gh);
        expect(holdsMarker(run.said)).toBe(false);
        expectSiteRan(run, row);
      });
    }
  };
}
