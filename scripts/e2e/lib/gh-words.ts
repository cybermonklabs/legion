import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { MARKER, markerRuns } from "./marker-runs";

// What a harness of lib/workflow.sh's and parked-decision-devbox.sh's gh calls shares: the marker
// a stand-in gh puts in its stderr or its answer, the check that a run printed no run of it, and the
// stand-in itself. Whatever answers gh in GitHub's place holds gh's token and writes both, so a
// script prints neither (lib/workflow.sh, gh_kept).

export const marker = MARKER;

/** Whether TEXT holds a run of eight characters of the marker (redacttest.Piece's run). */
export function holdsMarker(text: string): boolean {
  return markerRuns(text).length > 0;
}

/**
 * The lines of TEXT that hold a run of the marker: a row asserts there are none, so a failure
 * prints the very lines that carried it.
 */
export function markerLines(text: string): string[] {
  return text.split("\n").filter(holdsMarker);
}

/**
 * One rule of the stand-in gh: a bash glob over ` <its arguments> `, its exit status, what it
 * answers on stdout (an array is one answer per matching call, the last repeating) and what it
 * says on stderr.
 */
export type Rule = { match: string; code?: number; out?: string | string[]; err?: string };

/**
 * Writes the stand-in gh into BIN. It logs each call, one line, to $GH_DIR/calls, drains stdin for
 * `--input -`, and answers as the first rule of $GH_DIR/rules that matches; a call no rule matches
 * fails with status 3.
 */
export function writeStandInGh(bin: string): void {
  mkdirSync(bin, { recursive: true });
  writeFileSync(
    join(bin, "gh"),
    `#!/usr/bin/env bash
set -uo pipefail
printf '%s\\n' "\${*//$'\\n'/ }" >>"$GH_DIR/calls"
case " $* " in *" --input - "*) cat >/dev/null ;; esac
while IFS=$'\\x1f' read -r glob code out err; do
  # shellcheck disable=SC2053 # the rule's glob is a pattern
  [[ " $* " == $glob ]] || continue
  if [ -d "$out" ]; then
    n=$(($(cat "$out.count" 2>/dev/null || echo 0) + 1))
    echo "$n" >"$out.count"
    [ -e "$out/$n" ] || n=$(find "$out" -type f | wc -l)
    cat "$out/$n"
  elif [ -n "$out" ]; then
    cat "$out"
  fi
  [ -z "$err" ] || cat "$err" >&2
  exit "$code"
done <"$GH_DIR/rules"
echo "stand-in gh: no rule answers this call" >&2
exit 3
`,
    { mode: 0o755 }
  );
}

/** Writes RULES as the stand-in reads them into GH_DIR, with an empty log of calls. */
export function writeRules(ghDir: string, rules: Rule[]): void {
  mkdirSync(ghDir, { recursive: true });
  const lines = rules.map((rule, i) => {
    let out = "";
    if (Array.isArray(rule.out)) {
      out = join(ghDir, `out-${i}`);
      mkdirSync(out);
      for (const [n, text] of rule.out.entries()) writeFileSync(join(out, String(n + 1)), text);
    } else if (rule.out !== undefined) {
      out = join(ghDir, `out-${i}`);
      writeFileSync(out, rule.out);
    }
    let err = "";
    if (rule.err !== undefined) {
      err = join(ghDir, `err-${i}`);
      writeFileSync(err, rule.err);
    }
    // A unit separator between the fields: a tab is whitespace to read, which drops an empty one.
    return [rule.match, String(rule.code ?? 0), out, err].join("\x1f");
  });
  writeFileSync(join(ghDir, "rules"), `${lines.join("\n")}\n`);
  writeFileSync(join(ghDir, "calls"), "");
}
