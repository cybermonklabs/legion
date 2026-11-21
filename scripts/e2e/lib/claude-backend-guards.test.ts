import { beforeAll, describe, expect, test } from "bun:test";
import { copyFileSync, mkdirSync, writeFileSync } from "node:fs";
import { isAbsolute, join } from "node:path";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { ownGroup } from "./own-group";
import { scriptFunctions } from "./script-functions";

// The Claude Code backend proof's process guards (claude-backend-tmux.sh), taken from the script by
// name and run under its own `set -Eeuo pipefail`, with lib/rig.sh (and so lib/process-tree.sh)
// sourced first, as the script sources it, against live processes this suite starts: bridged,
// whether a claude's parent is the run's `legion claude-bridge -- <claude>`; claude_pid, a pane's
// Claude Code process; and endpoint_mismatch, an agent's environment against the rig.
const fn = scriptFunctions(join(import.meta.dir, "..", "claude-backend-tmux.sh"));
const rig = join(import.meta.dir, "rig.sh");
const root = join(import.meta.dir, "..", "..", "..");
const guards = [
  "read_environ",
  "parent_pid",
  "agent_descendant",
  "claude_pid",
  "claude_ready",
  "bridged",
  "endpoint_mismatch",
]
  .map(fn)
  .join("\n");

const dir = temporaryDirectory("claude-backend-guards-test");
// The stand-in processes are copies of two real programs, and sh runs one wrapper, so a host
// without any of the three has nothing to start and every row here is skipped, as on a host without
// perl, which starts every harness in a group of its own (ownGroup, lib/own-group.ts).
// The gate is given every name: it walks each one and names the missing ones in its warning.
// `found` is the run's own walk, whose real binaries the stand-ins below are copies of.
const found = new Map(["bash", "sleep", "sh"].map((name) => [name, hostTool(name)] as const));
const skip = skipWithout("bash", "sleep", "sh", "perl");
// Only the unreadable-environment case needs an interpreter.
const withoutPython = skipWithout("python3");
function tool(name: string): string {
  const path = found.get(name) ?? null;
  if (path === null) throw new Error(`${name} is not on PATH`);
  return path;
}
// The run's legion is a copy of bash, so a process can carry it as its executable (/proc/<pid>/exe)
// with `claude-bridge` as its first argument: bash runs the script of that name from its working
// directory, which starts its second argument as its child and waits on it, as the bridge waits on
// Claude Code, under the name CLAUDE_ARGV0 gives it when that is set (`exec -a`, as a wrapper or
// another argv[0] gives the real Claude Code), or after holding its own fork a second when
// CLAUDE_FORK_HOLD is set, as the real bridge's child is the bridge until it execs. `pane-shim` runs
// the rest of its arguments, after its `--`, as its child, as a pane's `legion worker-shim` runs the
// bridge.
const work = join(dir, "work");
// The run's claude is a copy of sleep, under a directory whose name holds a space. Beside it: a
// chain of two wrapper scripts, as a devbox's dotfiles shim is one, the first run by bash and the
// second by sh, each running two seconds as only itself before it execs the next, the last a copy
// of sleep elsewhere; a Claude Code that is a copy of bash, which runs the cwd's script `600` (the
// bridge hands it that argument) and so runs an Oh My Pi tool below itself; and that Oh My Pi, a
// copy of sleep.
const claudeDir = join(dir, "bin with space");
const claude = join(claudeDir, "claude");
const wrapper = join(claudeDir, "claude-wrapper");
const wrapper2 = join(dir, "real", "claude-wrapper-2");
const wrapped = join(dir, "real", "claude");
const claudeWithTool = join(claudeDir, "claude-with-tool");
const ompTool = join(dir, "real", "omp");
const cwd = join(dir, "cwd");
beforeAll(() => {
  if (skip) return;
  mkdirSync(work);
  copyFileSync(tool("bash"), join(work, "legion"));
  mkdirSync(claudeDir);
  copyFileSync(tool("sleep"), claude);
  mkdirSync(join(dir, "real"));
  copyFileSync(tool("sleep"), wrapped);
  copyFileSync(tool("sleep"), ompTool);
  copyFileSync(tool("bash"), claudeWithTool);
  writeFileSync(wrapper, `#!${tool("bash")}\nsleep 2\nexec ${JSON.stringify(wrapper2)} "$@"\n`, {
    mode: 0o755,
  });
  writeFileSync(wrapper2, `#!${tool("sh")}\nsleep 2\nexec ${JSON.stringify(wrapped)} "$@"\n`, {
    mode: 0o755,
  });
  mkdirSync(cwd);
  writeFileSync(join(cwd, "worker-shim"), '"$2" 600\n:\n');
  writeFileSync(
    join(cwd, "claude-bridge"),
    'if [ -n "$CLAUDE_FORK_HOLD" ]; then (sleep 1; exec "$2" 600)\n' +
      'elif [ -n "$CLAUDE_ARGV0" ]; then (exec -a "$CLAUDE_ARGV0" "$2" 600)\n' +
      'else "$2" 600; fi\n:\n'
  );
  writeFileSync(join(cwd, "pane-shim"), 'shift\n"$@"\n:\n');
  writeFileSync(join(cwd, "600"), `${JSON.stringify(ompTool)} 600\n:\n`);
});

// run runs BODY after lib/rig.sh and the guards, with the rig's globals set, a scratch directory of
// its own in $scratch, and two helpers. spawn ARGS… starts ARGS in the background, its output away
// from the test's pipes, and sets pid to its process. await_child EXE sets child to pid's first child
// once that child is running EXE (its /proc/<pid>/exe), not merely once it exists: a forked child
// carries its parent's command line until it execs, and a guard asked about it then answers for the
// parent. Every process spawn started goes, with everything below it, when BODY ends (stop_tree,
// lib/process-tree.sh, which signals a process only while its parent is the one it was started
// under).
function run(body: string, vars: Record<string, string> = {}) {
  const prelude = `set -Eeuo pipefail
root=${JSON.stringify(root)} evidence=$SCRATCH timeout_hook=
note() { :; }
fail() { echo "fail: $*" >&2; exit 1; }
. ${JSON.stringify(rig)}
${guards}
work=$WORK claude_bin=$CLAUDE claude_exe=$(realpath -e -- "$CLAUDE") port_dispatch=1 port_listener=2 port_nats=3 state=/rig
scratch=$(mktemp -d "$SCRATCH/run.XXXXXX")
started=()
# The shell's own notice of each job the trap stops goes nowhere: it is no output of a row.
trap '{ for p in \${started[@]+"\${started[@]}"}; do stop_tree "$p"; done; } 2>/dev/null' EXIT
spawn() {
  "$@" </dev/null >/dev/null 2>&1 &
  pid=$!
  started+=("$pid")
}
await_child() {
  local want seen= i
  want=$(realpath -e -- "$1")
  child=
  for i in $(seq 1 200); do
    child=$(cat "/proc/$pid/task/$pid/children" 2>/dev/null) || true
    child=\${child%% *}
    if [ -n "$child" ]; then
      seen=$(readlink "/proc/$child/exe" 2>/dev/null) || seen=
      [ "$seen" != "$want" ] || break
    fi
    sleep 0.05
  done
  [ -z "$child" ] || started+=("$child")
  [ -n "$child" ] && [ "$seen" = "$want" ] ||
    { echo "no child of $pid running $want within 10 s (child \${child:-none}, running \${seen:-nothing})" >&2; exit 1; }
}
# await_below EXE sets below to the process anywhere below pid that runs EXE once one does, read
# through every process's children and the kernel's record of what each runs, never a name.
await_below() {
  local want i p queue
  want=$(realpath -e -- "$1")
  below=
  for i in $(seq 1 200); do
    queue=("$pid")
    while [ "\${#queue[@]}" -gt 0 ] && [ -z "$below" ]; do
      for p in $(cat /proc/"\${queue[0]}"/task/*/children 2>/dev/null || :); do
        if [ "$(readlink "/proc/$p/exe" 2>/dev/null || :)" = "$want" ]; then below=$p; else queue+=("$p"); fi
      done
      queue=("\${queue[@]:1}")
    done
    [ -z "$below" ] || return 0
    sleep 0.05
  done
  echo "no process below $pid running $want within 10 s" >&2
  exit 1
}
`;
  // The harness shell, and so every process it spawns, runs in a process group of its own
  // (ownGroup): its EXIT trap stops each tree, and a stopped member of the test runner's own group
  // hangs the runner up where that group is orphaned, as in a Legion pod.
  const result = Bun.spawnSync(ownGroup(["bash", "-c", `${prelude}\n${body}`]), {
    cwd,
    env: { PATH: process.env.PATH ?? "", WORK: work, CLAUDE: claude, SCRATCH: dir, ...vars },
  });
  return { code: result.exitCode, out: result.stdout.toString(), err: result.stderr.toString() };
}

const slow = 60_000;

describe.skipIf(skip)("bridged", () => {
  test(
    "accepts the run's legion claude-bridge -- <claude> every time, with a pane-sized multi-line argument",
    () => {
      const r = run(`
big=$(for i in $(seq 1 3600); do printf 'line %05d of a system prompt\\n' "$i"; done)
spawn "$work/legion" claude-bridge -- "$claude_bin" "$big"
await_child "$claude_bin"
printf 'argv bytes %s\\n' "$(wc -c <"/proc/$pid/cmdline")"
ok=0
for i in $(seq 1 20); do if bridged "$child"; then ok=$((ok + 1)); fi; done
printf 'accepted %s of 20\\n' "$ok"
`);
      expect(r.err).toBe("");
      expect(r.out).toMatch(/^argv bytes 1\d{5}$/m);
      expect(r.out).toContain("accepted 20 of 20");
    },
    slow
  );

  test(
    "refuses a parent that is not the run's legion, whose argument only mentions the bridge",
    () => {
      const r = run(`
spawn bash -c 'sleep 600; :' prompt "your parent is $work/legion claude-bridge -- $claude_bin"
await_child "$(command -v sleep)"
if bridged "$child"; then echo accepted; else echo refused; fi
`);
      expect(r.err).toBe("");
      expect(r.out.trim()).toBe("refused");
    },
    slow
  );

  test(
    "refuses the run's legion when claude-bridge is not its subcommand, or the child is not the run's claude",
    () => {
      const r = run(`
spawn "$work/legion" worker-shim -- "$claude_bin" claude-bridge -- "$claude_bin"
await_child "$claude_bin"
if bridged "$child"; then echo "worker-shim: accepted"; else echo "worker-shim: refused"; fi
spawn "$work/legion" claude-bridge -- "$(command -v sleep)"
await_child "$(command -v sleep)"
if bridged "$child"; then echo "other claude: accepted"; else echo "other claude: refused"; fi
`);
      expect(r.err).toBe("");
      expect(r.out).toContain("worker-shim: refused");
      expect(r.out).toContain("other claude: refused");
    },
    slow
  );
});

describe.skipIf(skip)("claude_pid", () => {
  // A pane stand-in: bash for the pane's own `sh -c`, the run's legion running pane-shim for its
  // `legion worker-shim`, and the run's legion running claude-bridge, whose one child is the run's
  // claude.
  const pane = `spawn bash -c '"$@"; :' pane "$work/legion" pane-shim -- "$work/legion" claude-bridge -- "$claude_bin"
await_below "$claude_bin"
found=$(claude_pid "$pid") || found=none
if [ "$found" = "$below" ]; then echo "found the bridge's child"; else echo "found $found, want $below"; fi
printf 'argv[0] %s\\n' "$(tr '\\0' '\\n' <"/proc/$below/cmdline" | sed -n 1p)"`;

  test(
    "finds Claude Code under the pane's bridge when its executable's path holds a space",
    () => {
      const r = run(pane);
      expect(r.err).toBe("");
      expect(r.out).toBe(`found the bridge's child\nargv[0] ${claude}\n`);
    },
    slow
  );

  test(
    "finds Claude Code under the pane's bridge when it runs under another name, as a wrapper or exec -a gives it",
    () => {
      const r = run(`export CLAUDE_ARGV0=not-claude\n${pane}`);
      expect(r.err).toBe("");
      expect(r.out).toBe("found the bridge's child\nargv[0] not-claude\n");
    },
    slow
  );
  test(
    "answers no process until the bridge's child runs Claude Code itself, past the bridge's fork and a chain of two wrapper scripts",
    () => {
      // Each poll before the last exec must answer nothing: the child is first the bridge's own fork,
      // then the first wrapper's bash, then the second wrapper's sh, and none of them is Claude Code.
      const r = run(`export CLAUDE_FORK_HOLD=1
claude_bin=${JSON.stringify(wrapper)} claude_exe=$(realpath -e -- ${JSON.stringify(wrapped)})
spawn bash -c '"$@"; :' pane "$work/legion" pane-shim -- "$work/legion" claude-bridge -- "$claude_bin"
: >"$scratch/stages"
found=
SECONDS=0
while [ "$SECONDS" -lt 60 ]; do
  if found=$(claude_pid "$pid"); then break; fi
  case $found in *"does not run Claude Code yet: it runs "*) printf '%s\\n' "\${found##*it runs }" >>"$scratch/stages" ;; esac
  found=
  sleep 0.02
done
[ -n "$found" ] || { echo "no Claude Code within 60 s" >&2; exit 1; }
[ "$(readlink "/proc/$found/exe")" = "$claude_exe" ] && echo "first answer: the program the chain ends in" || echo "first answer: $found, running $(readlink "/proc/$found/exe")"
for stage in "the bridge's fork:$(realpath -e -- "$work/legion")" "the first wrapper:$(realpath -e -- "$(command -v bash)")" "the second wrapper:$(realpath -e -- "$(command -v sh)")"; do
  if grep -qxF -- "\${stage#*:}" "$scratch/stages"; then echo "refused while it was \${stage%%:*}"; else echo "never saw \${stage%%:*}"; fi
done`);
      expect(r.err).toBe("");
      expect(r.out).toBe(
        "first answer: the program the chain ends in\n" +
          "refused while it was the bridge's fork\n" +
          "refused while it was the first wrapper\n" +
          "refused while it was the second wrapper\n"
      );
    },
    slow
  );

  test(
    "takes the pane's Claude Code for its agent, not an Oh My Pi Claude Code runs below itself",
    () => {
      const r =
        run(`claude_bin=${JSON.stringify(claudeWithTool)} claude_exe=$(realpath -e -- ${JSON.stringify(claudeWithTool)}) omp_exe=${JSON.stringify(ompTool)}
spawn bash -c '"$@"; :' pane "$work/legion" pane-shim -- "$work/legion" claude-bridge -- "$claude_bin"
await_below "$omp_exe"
found=$(agent_descendant "$pid") || found=none
case $(readlink "/proc/$found/exe" 2>/dev/null) in
  "$(realpath "$claude_bin")") echo "the agent: Claude Code" ;;
  "$(realpath "$omp_exe")") echo "the agent: the Oh My Pi below Claude Code" ;;
  *) echo "the agent: $found" ;;
esac`);
      expect(r.err).toBe("");
      expect(r.out).toBe("the agent: Claude Code\n");
    },
    slow
  );
});

describe.skipIf(skip)("endpoint_mismatch", () => {
  const rig = [
    "DISPATCH_URL=http://127.0.0.1:1",
    "DISPATCH_TOKEN_FILE=/rig/secrets/dispatch-token",
    "ENVOY_URL=http://127.0.0.1:2",
    "ENVOY_NATS_URL=nats://127.0.0.1:3",
  ];
  // agent ENV… starts an agent stand-in, sleep under env -i with exactly ENV, and checks it once its
  // environment is the one it was given, failing the run when it never is.
  const agent = (env: string[]) => `
spawn env -i ${env.join(" ")} "$(command -v sleep)" 600
ready=
for i in $(seq 1 200); do grep -qz '^ENVOY_NATS_URL=' "/proc/$pid/environ" 2>/dev/null && { ready=1; break; }; sleep 0.05; done
[ -n "$ready" ] || { echo "the agent stand-in $pid never carried its environment" >&2; exit 1; }
${check}`;
  const check = `status=0
found=$(endpoint_mismatch "$pid") || status=$?
printf 'status %s: %s\\n' "$status" "$found"`;

  test(
    "is clean for an agent with the rig's endpoints and no credential variable",
    () => {
      expect(run(agent(rig)).out.trim()).toBe("status 1:");
    },
    slow
  );

  test(
    "reports a credential variable or a wrong endpoint as a mismatch",
    () => {
      expect(run(agent([...rig, "CLAUDE_CODE_OAUTH_TOKEN=dummy"])).out.trim()).toBe(
        "status 0: CLAUDE_CODE_OAUTH_TOKEN set, want unset"
      );
      const wrong = rig.map((pair) =>
        pair.startsWith("ENVOY_URL=") ? "ENVOY_URL=http://127.0.0.1:9" : pair
      );
      expect(run(agent(wrong)).out.trim()).toBe(
        "status 0: ENVOY_URL=http://127.0.0.1:9, want http://127.0.0.1:2"
      );
    },
    slow
  );

  // The run's listener takes no credential on loopback, and the daemon hands a pane no Envoy
  // bearer, so a pane that carries one was given a shared token from somewhere.
  test(
    "reports an Envoy bearer variable as a mismatch",
    () => {
      expect(run(agent([...rig, "ENVOY_TOKEN_FILE=/rig/secrets/envoy-token"])).out.trim()).toBe(
        "status 0: ENVOY_TOKEN_FILE set, want unset"
      );
      expect(run(agent([...rig, "ENVOY_TOKEN=dummy"])).out.trim()).toBe(
        "status 0: ENVOY_TOKEN set, want unset"
      );
    },
    slow
  );

  test.skipIf(withoutPython)(
    "reports an environment it cannot read as not checked, never clean, while the agent still runs with a credential variable in it",
    () => {
      // A process that makes itself non-dumpable (prctl PR_SET_DUMPABLE 0) has its /proc entries
      // owned by root, so its own user can no longer read its environment. The stand-in writes its
      // ready file only once prctl succeeded and PR_GET_DUMPABLE reads 0, and keeps its stderr; the
      // run asserts it is still running, not a zombie, on both sides of the check.
      // It runs the interpreter python3 reports as sys.executable, resolved here with the caller's
      // environment and passed as data, not the first python3 on PATH: under `env -i` a version
      // manager's shim there (mise's, with no python selected) has no version and starts nothing.
      const resolved = Bun.spawnSync(["python3", "-I", "-c", "import sys; print(sys.executable)"]);
      const python = resolved.stdout.toString().trim();
      if (resolved.exitCode !== 0 || !isAbsolute(python)) {
        throw new Error(
          `could not resolve the python3 interpreter (exit ${resolved.exitCode}): ${resolved.stderr.toString() || "no absolute executable path"}`
        );
      }
      const r = run(
        `
ready=$scratch/ready err=$scratch/stderr
env -i ${rig.join(" ")} CLAUDE_CODE_OAUTH_TOKEN=dummy "$PYTHON" -c '
import ctypes, os, sys, time
libc = ctypes.CDLL(None, use_errno=True)
if libc.prctl(4, 0, 0, 0, 0) != 0:
    sys.exit("prctl(PR_SET_DUMPABLE, 0) failed: errno %d" % ctypes.get_errno())
if libc.prctl(3, 0, 0, 0, 0) != 0:
    sys.exit("PR_GET_DUMPABLE is not 0 after PR_SET_DUMPABLE 0")
with open(sys.argv[1] + ".tmp", "w") as f:
    f.write("not dumpable\\n")
os.rename(sys.argv[1] + ".tmp", sys.argv[1])
time.sleep(600)
' "$ready" </dev/null >/dev/null 2>"$err" &
pid=$!
started+=("$pid")
state() { local s; s=$(cat "/proc/$1/stat" 2>/dev/null) || { echo gone; return; }; s=\${s##*) }; echo "\${s%% *}"; }
for i in $(seq 1 200); do
  [ ! -s "$ready" ] || break
  case $(state "$pid") in Z | X | gone) break ;; esac
  sleep 0.05
done
[ -s "$ready" ] || { echo "the stand-in $pid did not start (state $(state "$pid")): $(cat "$err")" >&2; exit 1; }
echo "fixture: $(cat "$ready")"
echo "before: $(state "$pid")"
if { : <"/proc/$pid/environ"; } 2>/dev/null; then echo "environ: readable"; else echo "environ: unreadable"; fi
${check}
echo "after: $(state "$pid")"
printf 'stderr: [%s]\\n' "$(cat "$err")"`,
        { PYTHON: python }
      );
      expect(r.err).toBe("");
      const lines = r.out.trim().split("\n");
      expect(lines[0]).toBe("fixture: not dumpable");
      expect(lines[1]).toMatch(/^before: [RSD]$/);
      expect(lines[2]).toBe("environ: unreadable");
      expect(lines[3]).toMatch(
        /^status 2: its environment could not be read \(\/proc\/\d+\/environ\)$/
      );
      expect(lines[4]).toMatch(/^after: [RSD]$/);
      expect(lines[5]).toBe("stderr: []");
    },
    slow
  );

  test(
    "reports an agent that has exited as not checked",
    () => {
      const r = run(`true &\npid=$!\nwait "$pid"\n${check}`);
      expect(r.out.trim()).toMatch(
        /^status 2: its environment could not be read \(\/proc\/\d+\/environ\)$/
      );
    },
    slow
  );
});
