import { afterEach, expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { procRootOf } from "./proc-root";

// procs_naming (lib/process-tree.sh) reads the environment of every entry of a proc root and names
// each process that carries the run's mark; the run's sweep (stop_marked_processes) ends what it
// names. Each entry's read is judged on its own: an entry whose process ended after the walk listed
// it, and one of another user's, are none of the answer, and any other failure to read an entry, or
// a reader that ended other than with an answer, fails the walk with the reader's own words. Each
// row points proc_root at a proc root of its own processes (lib/proc-root.ts).
const processTree = join(import.meta.dir, "process-tree.sh");
const dir = temporaryDirectory("procs-naming-test");
const skip = skipWithout("bash", "mkfifo", "sleep", "xargs", "grep");
// A hang guard only, in place of Bun's 5 s: each row's verdict is its own assertions, and no row
// waits on a clock. A row takes about 50 ms on an idle devbox.
const bound = 60_000;

// Every process a row starts, killed when the row ends.
const started: { kill(signal?: number | NodeJS.Signals): void }[] = [];
afterEach(() => {
  for (const child of started.splice(0)) child.kill("SIGKILL");
});
// marked starts a sleep that carries the mark of the run under WORK, as a process of the run does.
function marked(work: string): number {
  const child = Bun.spawn(["sleep", "60"], {
    env: { PATH: process.env.PATH ?? "", LEGION_E2E_RUN: work },
    stdout: "ignore",
    stderr: "ignore",
  });
  started.push(child);
  return child.pid;
}
// startOf is PID's start time, stat field 22, read past the last `) ` of the whole file.
function startOf(pid: number): string {
  return readFileSync(`/proc/${pid}/stat`, "utf8")
    .replace(/^[\s\S]*\) /, "")
    .split(" ")[19];
}

// harness runs BODY in a bash with lib/process-tree.sh sourced for the run directory WORK, marked
// as a run marks itself (LEGION_E2E_RUN), under the drivers' `set -Eeuo pipefail` and an ERR trap
// that prints, with proc_root at ROOT and PATH, when given, first.
function harness(work: string, root: string, body: string, path = "") {
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
trap 'echo "ERR trap: $BASH_COMMAND" >&2' ERR
. ${JSON.stringify(processTree)}
work=$WORK
export LEGION_E2E_RUN=$work
${body}`,
    ],
    {
      env: {
        PATH: `${path}${path ? ":" : ""}${process.env.PATH ?? ""}`,
        WORK: work,
        proc_root: root,
      },
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  return { code: ran.exitCode, out: ran.stdout.toString(), err: ran.stderr.toString() };
}
const quoted = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

test.skipIf(skip)(
  "a marked process that ends after the walk listed it and before its read is skipped, and the marked process still running is named",
  () => {
    const work = join(dir, "ended");
    mkdirSync(work);
    const live = marked(work);
    const root = procRootOf(dir, [live]);
    // Entry 0, listed first, is a FIFO: the walk's reader opens it once the walk has listed every
    // entry, and that open is the holder's event. The holder, a coproc, started the marked victim;
    // on that event it kills the victim and reaps it, so its /proc entry is gone, and only then
    // closes the FIFO, so the reader goes on to the victim's entry after it ended. The holder's
    // output is read through a copy of its pipe, which stays open once the coproc has ended. The
    // holder says it ended the victim before it closes the FIFO, so once the walk is over its line
    // is there; where it is not, the walk never read entry 0, and the row opens the FIFO itself
    // (read and write, which never waits) to let the holder go, and says so, rather than hang.
    const r = harness(
      work,
      root,
      `mkdir "$proc_root/0" && mkfifo "$proc_root/0/environ"
coproc HOLD {
  sleep 60 </dev/null >/dev/null 2>&1 &
  victim=$!
  echo "$victim"
  exec 3>"$proc_root/0/environ"
  kill -KILL "$victim"
  wait "$victim" 2>/dev/null || :
  echo "ended $victim"
  exec 3>&-
}
exec {held}<&"\${HOLD[0]}"
read -r victim <&"$held"
ln -s "/proc/$victim" "$proc_root/$victim"
st=0; procs_naming "$work" LEGION_E2E_RUN >"$work/named" 2>"$work/walk.err" || st=$?
if read -t 0 -u "$held"; then
  read -r ended <&"$held"
else
  ended="the walk never read entry 0"
  exec 4<>"$proc_root/0/environ"
  read -r _ <&"$held"
fi
echo "walk $st"
if [ "$ended" = "ended $victim" ]; then echo "the victim ended between the listing and the read"; else echo "holder: $ended"; fi
[ -e "/proc/$victim" ] || echo "the victim's entry is gone"
cat "$work/named" "$work/walk.err"`
    );
    expect(r.err).toBe("");
    expect(r.out).toBe(
      `walk 0\nthe victim ended between the listing and the read\nthe victim's entry is gone\n${live} ${startOf(live)}\n`
    );
  },
  bound
);

test.skipIf(skip)(
  "an entry whose environment cannot be read for another reason fails the walk naming that entry and its error, and names no process",
  () => {
    const work = join(dir, "unreadable");
    mkdirSync(work);
    const live = marked(work);
    const root = procRootOf(dir, [live]);
    // A directory where entry 7's environment would be: the read fails, and not because the process
    // ended or is another user's. Read as no match, the walk names the run's other processes and
    // says nothing, and a process whose entry could not be read is left running by the sweep.
    const r = harness(
      work,
      root,
      `mkdir -p "$proc_root/7/environ"
st=0; procs_naming "$work" LEGION_E2E_RUN >"$work/named" 2>"$work/walk.err" || st=$?
echo "walk $st, named $(wc -l <"$work/named")"`
    );
    expect(r.err).toBe("");
    expect(r.out).toBe("walk 2, named 0\n");
    expect(readFileSync(join(work, "walk.err"), "utf8")).toMatch(
      new RegExp(
        `^procs_naming: xargs exited 123 reading ${quoted(root)}/<pid>/environ: \\S*grep: ${quoted(root)}/7/environ: Is a directory, so the processes naming ${quoted(work)} were not read\\n$`
      )
    );
  },
  bound
);

test.skipIf(skip)(
  "a reader that ends other than with an answer fails the walk with the reader's own words, or says it printed no complaint",
  () => {
    const work = join(dir, "reader");
    mkdirSync(work);
    const live = marked(work);
    const said = join(dir, "reader-said");
    const silent = join(dir, "reader-silent");
    for (const [bin, script] of [
      [said, "echo 'xargs: cannot fork: Resource temporarily unavailable' >&2\nexit 1\n"],
      [silent, "exit 1\n"],
    ] as const) {
      mkdirSync(bin);
      writeFileSync(join(bin, "xargs"), `#!/bin/sh\n${script}`, { mode: 0o755 });
    }
    const walk = `st=0; procs_naming "$work" LEGION_E2E_RUN >"$work/named" 2>"$work/walk.err" || st=$?
echo "walk $st, named $(wc -l <"$work/named")"
cat "$work/walk.err"`;
    const root = procRootOf(dir, [live]);
    const told = harness(work, root, walk, said);
    expect(told.err).toBe("");
    expect(told.out).toBe(
      `walk 2, named 0\nprocs_naming: xargs exited 1 reading ${root}/<pid>/environ: xargs: cannot fork: Resource temporarily unavailable, so the processes naming ${work} were not read\n`
    );
    const quiet = harness(work, root, walk, silent);
    expect(quiet.err).toBe("");
    expect(quiet.out).toBe(
      `walk 2, named 0\nprocs_naming: xargs exited 1 reading ${root}/<pid>/environ: xargs ended with status 1 and printed no complaint, so the processes naming ${work} were not read\n`
    );
  },
  bound
);
