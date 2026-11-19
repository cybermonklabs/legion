import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { hostTool } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { ownGroup, untilRunning } from "./own-group";

// Every walk of the rig helpers that cannot read the processes it walks must say why and fail, so
// no caller reads a walk that saw nothing as one that found nothing: a proc root that lists no
// process (an empty directory where `proc_root` points), and a /proc that cannot be read at all (an
// empty tmpfs mounted on /proc). And a pid the proof recorded is signalled only while it is still
// the process recorded: stop_pid and signal_group (lib/process-tree.sh) hold it to its start time.
const e2e = join(import.meta.dir, "..");
const root = join(e2e, "..", "..");
const dir = temporaryDirectory("unread-proc-test");

const killed: number[] = [];
afterEach(() => {
  for (const pid of killed.splice(0)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
});

// The probe runs each walk in a shell with the rigs' helpers sourced, the skill-scenario rig's two
// and parked-decision-devbox.sh's stop_agent_daemons taken from their scripts, and prints one line a
// walk: its name, its exit status, and the first line it wrote on stderr. `fail` is a driver's, here
// one that prints and returns, so refuse_leftovers' report reaches the line.
const probe = `set -u
root=${JSON.stringify(root)} timeout_hook=
work=$(mktemp -d "$SCRATCH/work.XXXXXX")
evidence=$work
mkdir -p "$work/state/broker-a" "$work/runs/r1"
printf '{"pid": 12345}\\n' >"$work/state/broker-a/broker.pid"
note() { :; }
fail() { echo "fail: $*" >&2; return 1; }
export LEGION_E2E_RUN=$work
. "$root/scripts/e2e/lib/rig.sh"
. "$root/scripts/e2e/lib/leftovers.sh"
eval "$(sed -n '/^run_env_pids() {/p; /^stop_env_processes() {/,/^}/p' "$root/packages/pi-legion/scripts/skill-scenarios/rig.sh")"
eval "$(sed -n '/^stop_agent_daemons() {/,/^}/p' "$root/scripts/e2e/parked-decision-devbox.sh")"
walk() {
  local name=$1 status=0 said
  shift
  said=$( ("$@") 2>&1 >/dev/null) || status=$?
  printf '%s %s %s\\n' "$name" "$status" "\${said##*$'\\n'}"
}
walk procs_naming procs_naming "$work" LEGION_E2E_RUN XDG_CONFIG_HOME
walk marked_processes marked_processes "$work"
walk stop_marked_processes stop_marked_processes "$work"
walk run_processes run_processes
walk listening_holders listening_holders 5432
walk held_only_by held_only_by 5432 4242
walk run_env_pids run_env_pids "$work/runs"
walk refuse_leftovers refuse_leftovers legion-e2e-unread
walk stop_agent_daemons stop_agent_daemons
`;

type Walk = { name: string; status: number; said: string };
function walks(out: string): Walk[] {
  return out
    .split("\n")
    .filter(Boolean)
    .map((line) => {
      const [name, status, ...said] = line.split(" ");
      return { name: name ?? "", status: Number(status), said: said.join(" ") };
    });
}

test("over a proc root that lists no process, every walk fails with 2 and says it read nothing", () => {
  const scratch = mkdtempSync(join(dir, "empty-root-"));
  const empty = mkdtempSync(join(dir, "proc-"));
  const ran = Bun.spawnSync(ownGroup(["bash", "-c", probe]), {
    env: {
      PATH: process.env.PATH ?? "",
      HOME: process.env.HOME ?? "",
      SCRATCH: scratch,
      proc_root: empty,
    },
  });
  expect(ran.stderr.toString()).toBe("");
  const got = walks(ran.stdout.toString());
  expect(got.map(({ name }) => name)).toEqual([
    "procs_naming",
    "marked_processes",
    "stop_marked_processes",
    "run_processes",
    "listening_holders",
    "held_only_by",
    "run_env_pids",
    "refuse_leftovers",
    "stop_agent_daemons",
  ]);
  // listening_holders and held_only_by read the host's /proc/net/tcp first, which answers here, and
  // fail on the socket they look for, or on the root's walk: either is a failure, not a clean answer.
  for (const { name, status, said } of got) {
    expect({ name, failed: status !== 0, said: said !== "" }).toEqual({
      name,
      failed: true,
      said: true,
    });
  }
  for (const name of ["procs_naming", "marked_processes", "run_processes", "run_env_pids"]) {
    expect(got.find((walk) => walk.name === name)?.said).toMatch(/lists no process's environ/);
  }
  expect(got.find((walk) => walk.name === "refuse_leftovers")?.said).toMatch(/lists no process/);
  expect(got.find((walk) => walk.name === "stop_agent_daemons")?.said).toMatch(
    /could not be read \(run_processes\)/
  );
});

// Root is used only to make the mount namespace and mount an empty tmpfs on its /proc; the probe
// runs as this user (setpriv). Where root cannot be had without a password the row is skipped, by
// name, rather than failed or passed.
const sudo = hostTool("sudo");
const privileged =
  sudo !== null &&
  hostTool("unshare") !== null &&
  hostTool("setpriv") !== null &&
  Bun.spawnSync([sudo, "-n", "true"], { stdout: "ignore", stderr: "ignore" }).exitCode === 0;
if (!privileged) {
  console.warn(
    `Skipped the empty-/proc and pid-namespace rows of ${import.meta.path}: they need sudo -n, unshare and setpriv to make a namespace of their own.`
  );
}
function asUserIn(namespace: string[], setup: string, script: string, env: Record<string, string>) {
  const uid = String(process.getuid?.() ?? 0);
  const gid = String(process.getgid?.() ?? 0);
  return Bun.spawnSync(
    [
      sudo ?? "sudo",
      "-n",
      "unshare",
      ...namespace,
      "bash",
      "-c",
      `set -e\n${setup}\nexec setpriv --reuid="$1" --regid="$2" --init-groups env -i ${Object.entries(
        env
      )
        .map(([key, value]) => `${key}=${JSON.stringify(value)}`)
        .join(" ")} bash -c "$3"`,
      "as-user",
      uid,
      gid,
      script,
    ],
    { stdout: "pipe", stderr: "pipe" }
  );
}

test.skipIf(!privileged)(
  "under an empty /proc every walk and every read of a process's tree fails and says why",
  () => {
    const scratch = mkdtempSync(join(dir, "empty-proc-"));
    const ran = asUserIn(
      ["--mount", "--propagation", "private"],
      "mount -t tmpfs tmpfs /proc",
      `${probe}
walk proc_children proc_children 4242
walk proc_descendants proc_descendants 4242
walk stop_tree stop_tree 4242`,
      { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "", SCRATCH: scratch }
    );
    expect(ran.stderr.toString()).toBe("");
    const got = walks(ran.stdout.toString());
    expect(got).toHaveLength(12);
    for (const { name, status, said } of got) {
      expect({ name, failed: status !== 0, said: said !== "" }).toEqual({
        name,
        failed: true,
        said: true,
      });
    }
    expect(got.find((walk) => walk.name === "listening_holders")?.said).toMatch(
      /\/proc\/net\/tcp cannot be read/
    );
    for (const name of ["proc_children", "proc_descendants", "stop_tree"]) {
      expect(got.find((walk) => walk.name === name)?.said).toMatch(/\/proc has no self/);
    }
  }
);

// A pid recorded with its start (record_start) is signalled only while the start reads the same: a
// recorded start that is not the process's own stands in for a daemon that ended and whose pid
// another process took since. stop_pid and signal_group send that process nothing. The shell sources
// lib/rig.sh as a proof does, so the row reads the stop_pid a proof calls.
const rigPrelude = `root=${JSON.stringify(root)} work=$(mktemp -d) timeout_hook=
evidence=$work
note() { :; }
fail() { echo "fail: $*" >&2; exit 1; }
. "$root/scripts/e2e/lib/rig.sh"`;
test("stop_pid and signal_group leave alone a process whose start is not the one recorded for its pid", async () => {
  const stranger = Bun.spawn(ownGroup(["sleep", "60"]), { stdout: "ignore", stderr: "ignore" });
  killed.push(stranger.pid);
  await untilRunning(stranger.pid, "sleep");
  const start = readFileSync(`/proc/${stranger.pid}/stat`, "utf8")
    .replace(/^[\s\S]*\) /, "")
    .split(" ")[19];
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -u
${rigPrelude}
started_at[${stranger.pid}]=$((${start} + 1))
stop_pid ${stranger.pid}; echo "stop_pid $?"
signal_group ${stranger.pid} KILL; echo "signal_group $?"
read -r s </proc/${stranger.pid}/stat; s=\${s##*) }; echo "state \${s%% *}"`,
    ],
    { env: { PATH: process.env.PATH ?? "", TMPDIR: dir } }
  );
  expect(ran.stderr.toString()).toBe("");
  expect(ran.stdout.toString()).toBe("stop_pid 0\nsignal_group 0\nstate S\n");
  // The control: given its own start, stop_pid ends it.
  const own = Bun.spawnSync(
    ["bash", "-c", `${rigPrelude}\nstarted_at[${stranger.pid}]=${start}\nstop_pid ${stranger.pid}`],
    { env: { PATH: process.env.PATH ?? "", TMPDIR: dir } }
  );
  expect(own.exitCode).toBe(0);
  expect(await stranger.exited).not.toBe(0);
});

// The same with a real reuse: in a pid namespace whose pid_max is 2400, a daemon the rig started
// ends of itself, bash reaps it, and a stranger, no child of the rig's shell, is given its pid; the
// rig's cleanup then stops the daemon's recorded pid. The stranger must be left running.
test.skipIf(!privileged)(
  "in a pid namespace whose pids wrap, stop_pid of a daemon that ended leaves the stranger that took its pid running",
  () => {
    const scratch = mkdtempSync(join(dir, "reuse-"));
    mkdirSync(join(scratch, "logs"));
    const script = `set -u
root=${JSON.stringify(root)} work=${JSON.stringify(scratch)} evidence=${JSON.stringify(scratch)} timeout_hook=
note() { :; }
fail() { echo "fail: $*" >&2; exit 1; }
. "$root/scripts/e2e/lib/rig.sh"
coproc SPAWNER { while read -r cmd arg; do
  case $cmd in
    spawn) sleep 600 & echo "$!" ;;
    kill) kill -KILL "$arg" 2>/dev/null; wait "$arg" 2>/dev/null; echo done ;;
  esac
done; }
spawn() { echo spawn >&"\${SPAWNER[1]}"; read -r Q <&"\${SPAWNER[0]}"; }
unspawn() { echo "kill $1" >&"\${SPAWNER[1]}"; read -r _ <&"\${SPAWNER[0]}"; }
while spawn; [ "$Q" -lt 400 ]; do unspawn "$Q"; done
unspawn "$Q"
start_process daemon sleep 0.2
sleep 1
n=0
while spawn; [ "$Q" != "$daemon_pid" ]; do
  unspawn "$Q"
  n=$((n + 1))
  [ "$n" -lt 20000 ] || { echo "no stranger took pid $daemon_pid" >&2; exit 1; }
done
stranger_start=$(proc_start "$daemon_pid")
stop_pid "$daemon_pid"
[ "$(proc_start "$daemon_pid")" = "$stranger_start" ] && echo "stranger alive" || echo "stranger signalled"
unspawn "$daemon_pid"`;
    const ran = asUserIn(
      ["--pid", "--fork", "--mount-proc"],
      "echo 2400 >/proc/sys/kernel/pid_max",
      script,
      { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "" }
    );
    expect(ran.stderr.toString()).toBe("");
    expect(ran.stdout.toString()).toBe("stranger alive\n");
  },
  120_000
);
