import { beforeAll, expect, test } from "bun:test";
import { copyFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import { hostGate, hostTool } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { ownGroup } from "./own-group";
import { rowWaits } from "./row-waits";
import { scriptFunctions } from "./script-functions";

// The controller start proof's lookup of the controller's Oh My Pi (controller-start-tmux.sh's
// controller_omp STATE_DIR CLI_PID), taken from the script by name and run under its own
// `set -Eeuo pipefail` with lib/rig.sh sourced, against a `legion controller start` stand-in this
// suite starts: the run's legion is a copy of bash that runs `sh -c` of the Oh My Pi as its one
// child, as controller.go's exec.Command("sh", "-c", …) does, and the Oh My Pi is a copy of sleep,
// started through env so that its environment ends with exactly the variables a row gives it.
const controllerOmp = scriptFunctions(join(import.meta.dir, "..", "controller-start-tmux.sh"))(
  "controller_omp"
);
const rig = join(import.meta.dir, "rig.sh");
const root = join(import.meta.dir, "..", "..", "..");
const dir = temporaryDirectory("controller-start-guards-test");
// perl: run starts every harness in a group of its own (ownGroup, lib/own-group.ts).
const host = hostGate({ programs: ["bash", "sleep", "sh", "env", "perl"] });
const work = join(dir, "work");
const bin = join(dir, "bin");
beforeAll(() => {
  if (host.missing) return;
  const bash = hostTool("bash");
  const sleep = hostTool("sleep");
  if (bash === null || sleep === null) throw new Error("bash or sleep is not on PATH");
  mkdirSync(work);
  copyFileSync(bash, join(work, "legion"));
  mkdirSync(bin);
  // The pinned Oh My Pi, as `mise x <pin> -- omp` runs it, and the same program under another
  // name, as an omp_invocation may run it (`omp_invocation: <any program>`).
  copyFileSync(sleep, join(bin, "omp"));
  copyFileSync(sleep, join(bin, "oh-my-pi"));
});

// run runs BODY after controller_omp, with the rig's globals and lib/rig.sh, and one helper:
// start_cli EXE VAR=VALUE… starts the legion stand-in, whose `sh -c` child runs EXE under env with
// VAR=VALUE… appended to its environment, sets cli to the stand-in's pid and omp to EXE's once EXE
// runs, and fails the run when it does not by until_true_deadline, the end of the row's waits
// (lib/row-waits.ts): how soon it runs is a loaded host's. Each stand-in is stopped with everything
// under it when BODY ends, its start failed or not (stop_tree, lib/process-tree.sh).
function run(body: string) {
  const script = `set -Eeuo pipefail
trap 'echo "ERR trap: $BASH_COMMAND" >&2' ERR
root=${JSON.stringify(root)}
work=$WORK evidence=$WORK timeout_hook=
note() { :; }
fail() { echo "fail: $*" >&2; exit 1; }
. ${JSON.stringify(rig)}
until_true_deadline=$((SECONDS + ${waits.secondsLeft()}))
${controllerOmp}
started=()
# The shell's own "Killed" notice of each job the trap stops goes nowhere: it is no output of a row.
trap '{ for p in \${started[@]+"\${started[@]}"}; do stop_tree "$p"; done; } 2>/dev/null' EXIT
start_cli() {
  local exe=$1 kid want
  want=$(realpath -e -- "$1")
  shift
  env -i PATH="$PATH" "$work/legion" -c 'sh -c "env \\"\\$@\\"; :" sh "$@"; :' legion "$@" "$exe" 600 </dev/null >/dev/null 2>&1 &
  cli=$!
  started+=("$cli")
  omp=
  while :; do
    kid=$(cat /proc/"$cli"/task/*/children 2>/dev/null || :)
    kid=\${kid%% *}
    if [ -n "$kid" ]; then
      omp=$(cat /proc/"$kid"/task/*/children 2>/dev/null || :)
      omp=\${omp%% *}
      # Its environment, which every row reads next (controller_omp), only once the exec has laid it
      # out (environ_laid_out, lib/process-tree.sh): the exe names EXE before that.
      [ -n "$omp" ] && [ "$(readlink "/proc/$omp/exe" 2>/dev/null || :)" = "$want" ] && environ_laid_out "$omp" && break
      omp=
    fi
    [ "$SECONDS" -lt "$until_true_deadline" ] || break
    sleep 0.05
  done
  [ -n "$omp" ] || { echo "the stand-in $cli never ran $exe by the end of the row's waits" >&2; exit 1; }
}
# padding prints COUNT variables of a hundred bytes, which sort after LEGION_STATE_DIR as mise x
# leaves Oh My Pi's environment sorted.
padding() {
  local i value
  value=$(printf '%090d' 0)
  for ((i = 0; i < $1; i++)); do printf 'ZZPAD_%05d=%s\\n' "$i" "$value"; done
}
state=$work/controller-state
${body}`;
  // The harness shell, and so every stand-in it starts, runs in a process group of its own
  // (ownGroup): its EXIT trap stops each tree, and a stopped member of the test runner's own group
  // hangs the runner up where that group is orphaned, as in a Legion pod.
  const result = Bun.spawnSync(ownGroup(["bash", "-c", script]), {
    env: { PATH: process.env.PATH ?? "", WORK: work },
  });
  return { code: result.exitCode, out: result.stdout.toString(), err: result.stderr.toString() };
}

// lookups calls controller_omp twenty times and prints how many answered the stand-in's Oh My Pi.
const lookups = `found=0
for i in $(seq 1 20); do
  got=$(controller_omp "$state" "$cli") || got=
  [ "$got" != "$omp" ] || found=$((found + 1))
done
echo "found $found of 20"`;

const slow = 120_000;
const waits = rowWaits(slow);

host.describe("controller_omp", () => {
  test(
    "finds the Oh My Pi the run's legion controller start launched whatever name it runs under",
    () => {
      const r = run(`
omp_exe=${JSON.stringify(join(bin, "oh-my-pi"))}
start_cli "$omp_exe" LEGION_STATE_DIR="$state"
${lookups}`);
      expect(r.err).toBe("");
      expect(r.out).toBe("found 20 of 20\n");
    },
    slow
  );

  test(
    "reads LEGION_STATE_DIR from an environment with a mebibyte after it",
    () => {
      // A `tr '\\0' '\\n' <environ | grep -q` stops reading at its match, tr is killed writing the
      // rest, and pipefail makes the pipeline's status 141: the read answers absent.
      const r = run(`
omp_exe=${JSON.stringify(join(bin, "omp"))}
mapfile -t pad < <(padding 10000)
start_cli "$omp_exe" LEGION_STATE_DIR="$state" "\${pad[@]}"
printf 'environment bytes %s\\n' "$(wc -c <"/proc/$omp/environ")"
${lookups}`);
      expect(r.err).toBe("");
      const bytes = Number(/^environment bytes (\d+)$/m.exec(r.out)?.[1]);
      expect(bytes).toBeGreaterThan(1_000_000);
      expect(r.out).toEndWith("\nfound 20 of 20\n");
    },
    slow
  );

  test(
    "refuses an Oh My Pi of another state directory, and a legion whose line of children has none",
    () => {
      const r = run(`
omp_exe=${JSON.stringify(join(bin, "omp"))}
start_cli "$omp_exe" LEGION_STATE_DIR=/elsewhere
st=0
why=$(controller_omp "$state" "$cli") || st=$?
echo "another state: status $st: $why"
start_cli "$omp_exe" OTHER=1
st=0
why=$(controller_omp "$state" "$cli") || st=$?
echo "no state: status $st: $why"
st=0
why=$(controller_omp "$state" "$omp") || st=$?
echo "the Oh My Pi itself: status $st: $why"
`);
      expect(r.err).toBe("");
      const want = join(work, "controller-state").replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      const lines = r.out.trimEnd().split("\n");
      expect(lines).toHaveLength(3);
      expect(lines[0]).toMatch(
        new RegExp(
          `^another state: status 1: the Oh My Pi \\d+ of \\d+ has LEGION_STATE_DIR=/elsewhere, want ${want}$`
        )
      );
      expect(lines[1]).toMatch(
        new RegExp(
          `^no state: status 1: the Oh My Pi \\d+ of \\d+ has LEGION_STATE_DIR=, want ${want}$`
        )
      );
      expect(lines[2]).toMatch(
        /^the Oh My Pi itself: status 1: no process below \d+ runs \S+\/bin\/omp$/
      );
    },
    slow
  );
});
