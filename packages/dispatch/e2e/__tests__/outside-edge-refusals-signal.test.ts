/** How `e2e/acceptance/outside-edge-refusals.sh` ends when a HUP, INT or TERM comes: at one verdict
 *  line, never at bash's own exit with nothing printed; FAIL where a refusal line was reached
 *  before it; with every job it started killed and the directory its answers went to removed,
 *  even when a second signal comes during that cleanup. The fixtures, and the stand-in each row
 *  starts, are `outside-edge-refusals-fixtures.ts`'s.
 */

import { describe, expect, test } from "bun:test";
import { existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { hostTool } from "@legion/test-fixtures/host-tools";
import { rowLimit } from "./bounded-subprocess";
import {
  canaryBody,
  edgeFixtures,
  type Line,
  linesOf,
  passVerdict,
  planted,
  probeLine,
  production,
  refusals,
  routed,
  routedTable,
  said,
  script,
  scriptName,
  sent,
  standIn,
  withoutTools,
} from "./outside-edge-refusals-fixtures";
import { bash, closedPort, curl, verdict } from "./outside-path-reach-fixtures";

const { check, directoryOf, listFile, planLists } = edgeFixtures(import.meta.url);

describe(`${scriptName}: a signal`, () => {
  // A signal can come while the script reads its lists, before any probe, and a signal sent to the
  // script's own bash alone must end the run at once. Here a bash wrapper sends SIGTERM to that
  // bash alone while the refusal list, a named pipe, is read. In `held open`, the wrapper holds the
  // pipe open with nothing written, and its open returns only once the script has opened the pipe
  // to read, so the read waits. In `never opened`, the routed list is a pipe the wrapper writes the
  // routed table to and closes, and nothing opens the refusal list's pipe to write, so the script's
  // open of it waits. A moment later the wrapper sends the TERM, and waits up to 40 s for the
  // script to end, the list still held; then it lets the read end. The run must end while the list
  // is held, at its verdict line and exit 2, never at bash's own exit 143 with nothing printed: the
  // script reads each list in a job of its own and waits for it in `wait`, which the signal
  // interrupts. A script that read the list in a command substitution ended only once the read
  // did, after the wrapper let it end, and the wrapper then prints `held: no`. In `held open`, the
  // script's reader is certain to have the pipe open when the TERM comes, since the wrapper's own
  // open returned only then; once the run has ended, the wrapper writes one byte to the pipe. That
  // write succeeds only where a process still holds the pipe open to read, the reader the run left
  // behind; with none, it meets no reader, and the subshell that made it dies of SIGPIPE.
  for (const mode of ["held open", "never opened"]) {
    test.skipIf(withoutTools)(
      `reads UNKNOWN at once, never 143 with nothing printed, when a signal to its own process comes while it reads a list ${mode}${mode === "held open" ? ", and leaves no reader on it" : ""}`,
      async () => {
        const fifo = join(directoryOf("fifo"), "refusals");
        const routedPipe = join(directoryOf("fifo"), "routed");
        const sleep = hostTool("sleep") ?? "sleep";
        const wrapper = [
          "shell=$1 script=$2 mode=$3 fifo=$4 routed=$5 table=$6",
          'mkfifo "$fifo" || exit 70',
          '[ "$mode" = "held open" ] || mkfifo "$routed" || exit 70',
          '"$shell" "$script" &',
          "pid=$!",
          'if [ "$mode" = "held open" ]; then exec 3>"$fifo"; else printf "%s\\n" "$table" >"$routed"; fi',
          `${sleep} 0.2`,
          'kill -TERM "$pid"',
          "end=$((SECONDS + 40))",
          `while kill -0 "$pid" 2>/dev/null && [ "$SECONDS" -lt "$end" ]; do ${sleep} 0.05; done`,
          'if kill -0 "$pid" 2>/dev/null; then held=no; else held=yes; fi',
          "left=''",
          'if [ "$mode" = "held open" ] && [ "$held" = yes ]; then',
          "  if (printf x >&3) 2>/dev/null; then left=', a reader left on it: yes'; else left=', a reader left on it: no'; fi",
          "fi",
          'if [ "$mode" = "held open" ]; then exec 3>&-; elif [ "$held" = no ]; then exec 3<>"$fifo"; exec 3>&-; fi',
          'wait "$pid"',
          'printf "exit %s, ended while the list was held: %s%s\\n" "$?" "$held" "$left" >&2',
        ].join("\n");
        const run = await check(
          `signal while reading, ${mode}`,
          {
            OUTSIDE_EDGE_ADDRESS: `http://127.0.0.1:${await closedPort()}`,
            OUTSIDE_EDGE_ROUTED: mode === "held open" ? listFile(routedTable) : routedPipe,
            OUTSIDE_EDGE_REFUSALS: fifo,
          },
          {
            argv: [
              bash,
              "-c",
              wrapper,
              "signal-while-reading",
              bash,
              script,
              mode,
              fifo,
              routedPipe,
              routedTable.join("\n"),
            ],
            bound: "short",
          }
        );
        expect(run.stdout, said(run)).toBe(
          "UNKNOWN: the run was stopped by a signal before its verdict\n"
        );
        expect(run.stderr, said(run)).toBe(
          `exit 2, ended while the list was held: yes${mode === "held open" ? ", a reader left on it: no" : ""}\n`
        );
        expect(run.code, said(run)).toBe(0);
      },
      // The wrapper, a bounded child under the short bound, which runs the script inside it: the
      // script stops before any probe.
      rowLimit("short")
    );
  }

  /** A PATH directory whose `curl` sends SIGTERM to the script's own bash before it probes, on
   *  each call whose arguments, joined by spaces, hold `when`, and then runs the host's curl. The
   *  script's bash is the topmost ancestor whose command line runs the script (the command
   *  substitution that starts curl is a fork of it with the same command line), so a row that
   *  uses it needs /proc. bash runs the trap once that probe returns. */
  function signallingCurl(when: string): string {
    const directory = directoryOf("path");
    writeFileSync(
      join(directory, "curl"),
      [
        `#!${bash}`,
        `case " $* " in *" --version "*) exec ${curl} "$@" ;; esac`,
        "signal() {",
        "  local p=$PPID target='' stat",
        '  while [ "$p" -gt 1 ] && [ -r "/proc/$p/stat" ]; do',
        `    case $(tr '\\0' ' ' <"/proc/$p/cmdline") in *"${script}"*) target=$p ;; esac`,
        '    stat=$(cat "/proc/$p/stat")',
        // biome-ignore lint/suspicious/noTemplateCurlyInString: bash's own expansion, the fields after the command name.
        "    set -- ${stat##*) }",
        "    p=$2",
        "  done",
        '  [ -z "$target" ] || kill -TERM "$target"',
        "}",
        `case " $* " in *'${when}'*) signal ;; esac`,
        `exec ${curl} "$@"`,
        "",
      ].join("\n"),
      { mode: 0o755 }
    );
    return `${directory}:${process.env.PATH ?? ""}`;
  }

  test.skipIf(withoutTools || !existsSync("/proc/self/stat"))(
    "reads UNKNOWN, never PASS, when a signal stops the run before its verdict, and leaves no scratch behind",
    async () => {
      const edge = await standIn(production);
      try {
        const tmp = directoryOf("tmp");
        const run = await check(
          "signal",
          { OUTSIDE_EDGE_ADDRESS: edge.address, ...planLists() },
          { path: signallingCurl(""), tmp }
        );
        expect(verdict(run), said(run)).toBe(
          "UNKNOWN: the run was stopped by a signal before its verdict"
        );
        expect(run.code, said(run)).toBe(2);
        expect(linesOf(run, "refused"), said(run)).toEqual([]);
        // The first probe, the one the signal came during, and no other.
        expect(await edge.seen()).toEqual([sent(routed[0] as Line)]);
        expect(readdirSync(tmp)).toEqual([]);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  // An answer is evidence whatever came after it: a signal that stops the run after a refusal line
  // was reached ends it at FAIL, exit 1, naming that line, never at UNKNOWN. Here a route is
  // planted for the first refusal line, and the run's `curl` sends the signal at the probe of the
  // second line itself, after that line's unrouted control.
  test.skipIf(withoutTools || !existsSync("/proc/self/stat"))(
    "fails naming the line reached, never reads UNKNOWN, when a signal stops the run after a line was reached",
    async () => {
      const [first, second] = refusals as [Line, Line];
      const edge = await standIn([planted(first), ...production]);
      try {
        const tmp = directoryOf("tmp");
        const run = await check(
          "signal after reached",
          { OUTSIDE_EDGE_ADDRESS: edge.address, ...planLists() },
          { path: signallingCurl(` Host: ${second.host} `), tmp }
        );
        expect(linesOf(run, "REACHED"), said(run)).toEqual([
          probeLine("REACHED", first.name, 200, "canary", canaryBody),
        ]);
        expect(verdict(run), said(run)).toBe(
          `FAIL: reached ${first.name}, before a signal stopped the run`
        );
        expect(run.code, said(run)).toBe(1);
        expect((await edge.seen()).at(-1)).toEqual(sent(second));
        expect(readdirSync(tmp)).toEqual([]);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  // The same holds in the moment between a REACHED line being recorded and being printed: bash runs
  // a pending trap between two commands, and no input can time a signal into that gap. So this row
  // runs a copy of the script, under the file's scratch, with `kill -TERM $$` put after the first
  // statement of its REACHED branch, and plants a route for the first refusal line. The script
  // records the line before it prints it, so the copy must end at FAIL naming that line. A script
  // that printed the line first read UNKNOWN here.
  test.skipIf(withoutTools)(
    "fails naming the line reached when a signal comes between recording a REACHED line and printing it",
    async () => {
      const [first] = refusals as [Line];
      const text = readFileSync(script, "utf8");
      const branch = "    REACHED)\n";
      const at = text.indexOf(branch);
      expect(at, "the script's REACHED branch").toBeGreaterThan(-1);
      expect(text.indexOf(branch, at + 1), "a second REACHED branch").toBe(-1);
      // After the branch's first statement: its comment lines, then one line of code.
      let end = at + branch.length;
      while (text.slice(end).trimStart().startsWith("#")) end = text.indexOf("\n", end) + 1;
      end = text.indexOf("\n", end) + 1;
      const copy = join(directoryOf("copy"), scriptName);
      writeFileSync(copy, `${text.slice(0, end)}      kill -TERM $$\n${text.slice(end)}`, {
        mode: 0o755,
      });
      const edge = await standIn([planted(first), ...production]);
      try {
        const run = await check(
          "signal between record and print",
          { OUTSIDE_EDGE_ADDRESS: edge.address, ...planLists() },
          { argv: [bash, copy] }
        );
        expect(verdict(run), said(run)).toBe(
          `FAIL: reached ${first.name}, before a signal stopped the run`
        );
        expect(run.code, said(run)).toBe(1);
      } finally {
        await edge.stop();
      }
    },
    // The copy of the script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );

  // From the first line of a verdict on, through the exit's cleanup, a signal is ignored: a second
  // signal can neither print a second verdict nor cut the cleanup short. Here the run's `rm`, the
  // one command of the EXIT trap's cleanup that is a program of PATH, sends SIGTERM to the script's
  // bash, its parent, and then removes the directory as the host's `rm` does. The run must end at
  // its one PASS line and exit 0, with nothing left under its TMPDIR. A run whose cleanup took the
  // signal printed a second verdict, UNKNOWN, after its PASS, and exited 2.
  test.skipIf(withoutTools || hostTool("rm") === null)(
    "prints one verdict and leaves no directory when a signal comes during the exit's cleanup",
    async () => {
      const edge = await standIn(production);
      try {
        const directory = directoryOf("path");
        writeFileSync(
          join(directory, "rm"),
          [`#!${bash}`, 'kill -TERM "$PPID"', `exec ${hostTool("rm")} "$@"`, ""].join("\n"),
          { mode: 0o755 }
        );
        const tmp = directoryOf("tmp");
        const run = await check(
          "signal during cleanup",
          { OUTSIDE_EDGE_ADDRESS: edge.address, ...planLists() },
          { path: `${directory}:${process.env.PATH ?? ""}`, tmp }
        );
        expect(
          run.stdout.split("\n").filter((line) => /^(PASS|FAIL|UNKNOWN):/.test(line)),
          said(run)
        ).toEqual([passVerdict]);
        expect(run.code, said(run)).toBe(0);
        expect(readdirSync(tmp)).toEqual([]);
      } finally {
        await edge.stop();
      }
    },
    // The script, a bounded child under the bound of a run that probes.
    rowLimit("edgeRun")
  );
});
