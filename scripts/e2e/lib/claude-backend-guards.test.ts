import { beforeAll, describe, expect, test } from "bun:test";
import { copyFileSync, mkdirSync, writeFileSync } from "node:fs";
import { isAbsolute, join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { timeLeft } from "@legion/test-fixtures/wait";
import { scriptFunctions } from "./script-functions";

// The Claude Code backend proof's process guards (claude-backend-tmux.sh), taken from the script by
// name and run under its own `set -Eeuo pipefail` against live processes this suite starts:
// bridged, whether a claude's parent is the run's `legion claude-bridge -- <claude>`; claude_pid,
// a pane's Claude Code process; and endpoint_mismatch, an agent's environment against the rig.
const fn = scriptFunctions(join(import.meta.dir, "..", "claude-backend-tmux.sh"));
const guards = [
  "process_args",
  "read_environ",
  "parent_pid",
  "agent_descendant",
  "claude_pid",
  "bridged",
  "endpoint_mismatch",
]
  .map(fn)
  .join("\n");

const dir = temporaryDirectory("claude-backend-guards-test");
// The rows run the guards in the host's bash and wait with its sleep, so a host without them has
// nothing to run them in and every row here is skipped.
// The gate is given both names: it walks each one and names the missing ones in its warning.
const skip = skipWithout("bash", "sleep");
// Only the unreadable-environment case needs an interpreter.
const withoutPython = skipWithout("python3");
// Every process a guard or await_child judges by its executable (/proc/<pid>/exe) or its first
// argument is a copy of the Bun running this file (process.execPath), or that Bun itself ($BUN in
// run), and never a copy of a program found on the host's PATH. A copy of what PATH finds runs as
// itself only where that is a program of its own: a wrapper script runs under its interpreter and
// then execs the real program, so its process is /bin/sh and then /usr/bin/sleep, never the copy;
// and a multi-call binary (busybox, or the uutils coreutils that Ubuntu 25.10 installs as
// /usr/bin/sleep) chooses what to run by the name it is called by, so a copy called claude runs
// nothing and exits.
// The run's legion is a copy of Bun, so a process can carry it as its executable with
// `claude-bridge` as its first argument: Bun runs the script of that name from its working
// directory, which starts its second argument as its child and waits on it, as the bridge waits on
// Claude Code.
const work = join(dir, "work");
// The run's claude is another copy of Bun, under a directory whose name holds a space.
const claudeDir = join(dir, "bin with space");
const claude = join(claudeDir, "claude");
const cwd = join(dir, "cwd");
// The bridge stand-ins' script. `<script> -- CHILD …` starts CHILD, a Bun, with a program that only
// keeps it alive until the run kills it. Bun leaves the `--` after the script's name out of
// process.argv; /proc/<pid>/cmdline, which the guards read, keeps it.
const bridgeScript = `const [first, second] = process.argv.slice(2);
const child = Bun.spawn([first === "--" ? second : first, "-e", "setTimeout(() => {}, 600_000)"], {
  stdio: ["ignore", "ignore", "inherit"],
});
await child.exited;
`;
beforeAll(() => {
  if (skip) return;
  mkdirSync(work);
  copyFileSync(process.execPath, join(work, "legion"));
  mkdirSync(claudeDir);
  copyFileSync(process.execPath, claude);
  mkdirSync(cwd);
  for (const script of ["claude-bridge", "worker-shim"])
    writeFileSync(join(cwd, script), bridgeScript);
});

// run runs BODY after the guards, with the rig's globals set, a scratch directory of its own in
// $scratch, the Bun running this file in $BUN, and three helpers. spawn ARGS… starts ARGS in the
// background, its output away from the test's pipes, and sets pid to its process. await_child EXE
// sets child to pid's first child once that child is running EXE (its /proc/<pid>/exe), not merely
// once it exists: a forked child carries its parent's command line until it execs, and a guard
// asked about it then answers for the parent. before_deadline succeeds while the row's deadline is
// ahead: the run is handed that deadline as LEGION_PROOF_DEADLINE_MS (epoch milliseconds, from
// @legion/test-fixtures/wait), so each wait of BODY asks again until then and no longer, and its
// own failure prints before the row's guard. Every process spawn or await_child names is killed
// when BODY ends.
function run(body: string, vars: Record<string, string> = {}) {
  const prelude = `set -Eeuo pipefail
${guards}
work=$WORK claude_bin=$CLAUDE port_dispatch=1 port_listener=2 port_nats=3 state=/rig
scratch=$(mktemp -d "$SCRATCH/run.XXXXXX")
started=()
trap 'kill -- "\${started[@]}" 2>/dev/null || true' EXIT
spawn() {
  "$@" </dev/null >/dev/null 2>&1 &
  pid=$!
  started+=("$pid")
}
before_deadline() {
  local now=\${EPOCHREALTIME/[.,]/}
  ((10#$now / 1000 < LEGION_PROOF_DEADLINE_MS))
}
await_child() {
  local want seen=
  want=$(realpath -e -- "$1")
  child=
  while :; do
    child=$(cat "/proc/$pid/task/$pid/children" 2>/dev/null) || true
    child=\${child%% *}
    if [ -n "$child" ]; then
      seen=$(readlink "/proc/$child/exe" 2>/dev/null) || seen=
      [ "$seen" != "$want" ] || break
    fi
    before_deadline || break
    sleep 0.05
  done
  [ -z "$child" ] || started+=("$child")
  [ -n "$child" ] && [ "$seen" = "$want" ] ||
    { echo "no child of $pid running $want by the row's deadline (child \${child:-none}, running \${seen:-nothing})" >&2; exit 1; }
}
`;
  const result = Bun.spawnSync(["bash", "-c", `${prelude}\n${body}`], {
    cwd,
    env: {
      PATH: process.env.PATH ?? "",
      WORK: work,
      CLAUDE: claude,
      SCRATCH: dir,
      BUN: process.execPath,
      LEGION_PROOF_DEADLINE_MS: String(Math.floor(Date.now() + timeLeft())),
      ...vars,
    },
  });
  return { code: result.exitCode, out: result.stdout.toString(), err: result.stderr.toString() };
}

describe.skipIf(skip)("bridged", () => {
  test("accepts the run's legion claude-bridge -- <claude> every time, with a pane-sized multi-line argument", () => {
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
  });

  test("refuses a parent that is not the run's legion, whose argument only mentions the bridge", () => {
    const r = run(`
spawn "$BUN" -e 'await Bun.spawn([process.execPath, "-e", "setTimeout(() => {}, 600_000)"]).exited' prompt "your parent is $work/legion claude-bridge -- $claude_bin"
await_child "$BUN"
if bridged "$child"; then echo accepted; else echo refused; fi
`);
    expect(r.err).toBe("");
    expect(r.out.trim()).toBe("refused");
  });

  test("refuses a parent with the bridge's own arguments whose executable is not the run's legion", () => {
    const r = run(`
spawn "$BUN" claude-bridge -- "$claude_bin"
await_child "$claude_bin"
if bridged "$child"; then echo accepted; else echo refused; fi
`);
    expect(r.err).toBe("");
    expect(r.out.trim()).toBe("refused");
  });

  test("refuses the run's legion when claude-bridge is not its subcommand, or the child is not the run's claude", () => {
    const r = run(`
spawn "$work/legion" worker-shim -- "$claude_bin" claude-bridge -- "$claude_bin"
await_child "$claude_bin"
if bridged "$child"; then echo "worker-shim: accepted"; else echo "worker-shim: refused"; fi
spawn "$work/legion" claude-bridge -- "$BUN"
await_child "$BUN"
if bridged "$child"; then echo "other claude: accepted"; else echo "other claude: refused"; fi
`);
    expect(r.err).toBe("");
    expect(r.out).toContain("worker-shim: refused");
    expect(r.out).toContain("other claude: refused");
  });
});

describe.skipIf(skip)("claude_pid", () => {
  test("finds Claude Code under the bridge when its executable's path holds a space", () => {
    const r = run(`
spawn "$work/legion" claude-bridge -- "$claude_bin"
await_child "$claude_bin"
found=$(claude_pid "$pid") || found=none
if [ "$found" = "$child" ]; then echo "found the child"; else echo "found $found, want $child"; fi
`);
    expect(r.err).toBe("");
    expect(r.out.trim()).toBe("found the child");
  });
});

describe.skipIf(skip)("endpoint_mismatch", () => {
  const rig = [
    "DISPATCH_URL=http://127.0.0.1:1",
    "DISPATCH_TOKEN_FILE=/rig/secrets/dispatch-token",
    "ENVOY_URL=http://127.0.0.1:2",
    "ENVOY_NATS_URL=nats://127.0.0.1:3",
  ];
  // agent ENV… starts an agent stand-in, sleep under env -i with exactly ENV, and checks it once its
  // environment is the one it was given, failing the run when it is not by the row's deadline.
  const agent = (env: string[]) => `
spawn env -i ${env.join(" ")} "$(command -v sleep)" 600
ready=
while :; do grep -qz '^ENVOY_NATS_URL=' "/proc/$pid/environ" 2>/dev/null && { ready=1; break; }; before_deadline || break; sleep 0.05; done
[ -n "$ready" ] || { echo "the agent stand-in $pid did not carry its environment by the row's deadline" >&2; exit 1; }
${check}`;
  const check = `status=0
found=$(endpoint_mismatch "$pid") || status=$?
printf 'status %s: %s\\n' "$status" "$found"`;

  test("is clean for an agent with the rig's endpoints and no credential variable", () => {
    expect(run(agent(rig)).out.trim()).toBe("status 1:");
  });

  test("reports a credential variable or a wrong endpoint as a mismatch", () => {
    expect(run(agent([...rig, "CLAUDE_CODE_OAUTH_TOKEN=dummy"])).out.trim()).toBe(
      "status 0: CLAUDE_CODE_OAUTH_TOKEN set, want unset"
    );
    const wrong = rig.map((pair) =>
      pair.startsWith("ENVOY_URL=") ? "ENVOY_URL=http://127.0.0.1:9" : pair
    );
    expect(run(agent(wrong)).out.trim()).toBe(
      "status 0: ENVOY_URL=http://127.0.0.1:9, want http://127.0.0.1:2"
    );
  });

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
while :; do
  [ ! -s "$ready" ] || break
  case $(state "$pid") in Z | X | gone) break ;; esac
  before_deadline || break
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
    }
  );

  test("reports an agent that has exited as not checked", () => {
    const r = run(`true &\npid=$!\nwait "$pid"\n${check}`);
    expect(r.out.trim()).toMatch(
      /^status 2: its environment could not be read \(\/proc\/\d+\/environ\)$/
    );
  });
});
