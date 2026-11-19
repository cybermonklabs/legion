import { afterEach, expect, onTestFinished, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync } from "node:fs";
import { release } from "node:os";
import { join } from "node:path";
import { hostTool } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { ownGroup, untilRunning } from "./own-group";
import { scriptFunctions } from "./script-functions";

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
// one that prints and returns, so refuse_leftovers' report reaches the line. The two port walks ask
// for PORT, a port this test listens on (heldPort), so the socket is found in /proc/net/tcp and
// each walk goes on to the processes it must read; a port nothing listens on ends them earlier, with
// 1, at "0 sockets listen".
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
walk listening_holders listening_holders "$PORT"
walk held_only_by held_only_by "$PORT" 4242
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

// heldPort listens on a loopback port the kernel chooses, until the row ends, and answers it: the
// port the probe's two port walks ask for.
function heldPort(): number {
  const listener = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
  onTestFinished(() => listener.stop(true));
  return listener.port;
}

test("over a proc root that lists no process, every walk fails with 2 and says it read nothing", () => {
  const scratch = mkdtempSync(join(dir, "empty-root-"));
  const empty = mkdtempSync(join(dir, "proc-"));
  const ran = Bun.spawnSync(ownGroup(["bash", "-c", probe]), {
    env: {
      PATH: process.env.PATH ?? "",
      HOME: process.env.HOME ?? "",
      SCRATCH: scratch,
      PORT: String(heldPort()),
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
  for (const { name, status, said } of got) {
    expect({ name, status, said: said !== "" }).toEqual({ name, status: 2, said: true });
  }
  for (const name of ["procs_naming", "marked_processes", "run_processes", "run_env_pids"]) {
    expect(got.find((walk) => walk.name === name)?.said).toMatch(/lists no process's environ/);
  }
  expect(got.find((walk) => walk.name === "refuse_leftovers")?.said).toMatch(/lists no process/);
  for (const name of ["listening_holders", "held_only_by"]) {
    expect(got.find((walk) => walk.name === name)?.said).toMatch(/lists no process's fd directory/);
  }
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
      {
        PATH: process.env.PATH ?? "",
        HOME: process.env.HOME ?? "",
        SCRATCH: scratch,
        PORT: String(heldPort()),
      }
    );
    expect(ran.stderr.toString()).toBe("");
    const got = walks(ran.stdout.toString());
    expect(got).toHaveLength(12);
    for (const { name, status, said } of got) {
      expect({ name, status, said: said !== "" }).toEqual({ name, status: 2, said: true });
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

// pidMaxPerNamespace says whether a kernel of release KERNEL (`uname -r`) keeps pid_max per pid
// namespace, as Linux does from 6.14: there /proc/sys/kernel/pid_max inside a pid namespace of its
// own is that namespace's value. Before 6.14 it is one value for the whole host, and a write from
// a namespace, as real root, sets the host's. A release it cannot read is taken for an older one.
function pidMaxPerNamespace(kernel: string): boolean {
  const version = /^(\d+)\.(\d+)/.exec(kernel);
  if (version === null) return false;
  const [major, minor] = [Number(version[1]), Number(version[2])];
  return major > 6 || (major === 6 && minor >= 14);
}
const namespacePidMax = pidMaxPerNamespace(release());
if (privileged && !namespacePidMax) {
  console.warn(
    `Skipped the pid-namespace row of ${import.meta.path}: kernel ${release()} is older than 6.14, where pid_max is one value for the whole host, so the row's write to its namespace's pid_max would set the host's.`
  );
}

test("pidMaxPerNamespace takes a kernel from 6.14 on for one with a per-namespace pid_max, and any older or unreadable release for one without", () => {
  const verdicts = Object.fromEntries(
    [
      "7.0.0-1017-gcp",
      "7.0.0-1017-azure",
      "6.14.0",
      "6.14.11-1-aws",
      "6.13.12",
      "6.12.0-1027-azure",
      "6.8.0-87-generic",
      "5.15.0",
      "",
      "linux",
    ].map((kernel) => [kernel, pidMaxPerNamespace(kernel)])
  );
  expect(verdicts).toEqual({
    "7.0.0-1017-gcp": true,
    "7.0.0-1017-azure": true,
    "6.14.0": true,
    "6.14.11-1-aws": true,
    "6.13.12": false,
    "6.12.0-1027-azure": false,
    "6.8.0-87-generic": false,
    "5.15.0": false,
    "": false,
    linux: false,
  });
});

// The same with a real reuse: in a pid namespace whose pid_max is 2400, a daemon the rig started
// ends of itself, bash reaps it, and a stranger, no child of the rig's shell, is given its pid; the
// rig's cleanup then stops the daemon's recorded pid. The stranger must be left running. The write
// of 2400 is the namespace's own only from Linux 6.14 (pidMaxPerNamespace), so the row runs only
// there; and the host's pid_max is read before and after it, put back by root if it ever moved, and
// the row then fails, naming it.
const hostPidMax = () => readFileSync("/proc/sys/kernel/pid_max", "utf8").trim();
test.skipIf(!privileged || !namespacePidMax)(
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
    const before = hostPidMax();
    let stdout = "";
    let stderr = "";
    try {
      const ran = asUserIn(
        ["--pid", "--fork", "--mount-proc"],
        "echo 2400 >/proc/sys/kernel/pid_max",
        script,
        { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "" }
      );
      stdout = ran.stdout.toString();
      stderr = ran.stderr.toString();
    } finally {
      const after = hostPidMax();
      if (after !== before) {
        Bun.spawnSync([
          sudo ?? "sudo",
          "-n",
          "sh",
          "-c",
          `echo ${before} >/proc/sys/kernel/pid_max`,
        ]);
      }
      expect(
        { hostPidMax: after },
        `the namespace's pid_max write set the host's to ${after}; it is put back to ${before}`
      ).toEqual({ hostPidMax: before });
    }
    expect(stderr).toBe("");
    expect(stdout).toBe("stranger alive\n");
  },
  120_000
);

// run runs BODY in a bash with lib/rig.sh sourced as a proof sources it (rigPrelude), its $work
// holding the logs directory start_process writes to, and answers its status and both streams.
function run(body: string) {
  const ran = Bun.spawnSync(["bash", "-c", `${rigPrelude}\nmkdir -p "$work/logs"\n${body}`], {
    env: { PATH: process.env.PATH ?? "", TMPDIR: dir },
  });
  return { code: ran.exitCode, out: ran.stdout.toString(), err: ran.stderr.toString() };
}
const pidsIn = (out: string) => (out.match(/\b\d{2,}\b/g) ?? []).map(Number);
const goneOrRunning = `gone() { [ -z "$(proc_start "$1")" ]; }
state() { if gone "$1"; then echo gone; else echo running; fi; }`;

// start_process starts each service under job control, so the job leads a process group of its
// own and its pid is that group's leader: signal_group then stop_pid, the broker walkthrough rig's
// stop, end it with what it started. A setsid in front, as the rig had, forks, since the job already
// leads its group: the recorded pid is a setsid that exits at once, and the stop reaches neither the
// service nor its child.
test("start_process starts a service that leads its own group, and signal_group then stop_pid end it with its child; with setsid in front they reach neither", async () => {
  const r = run(`${goneOrRunning}
start_process svc bash -c 'sleep 300 & echo $! >"$0"; wait' "$work/child"
echo "start $?"
until_true 10 "the service's child" test -s "$work/child"
child=$(<"$work/child")
[ "$(proc_group "$svc_pid")" = "$svc_pid" ] && echo "leads its group" || echo "not its group's leader"
signal_group "$svc_pid" TERM
stop_pid "$svc_pid"
until_true 10 "the service's child to end" gone "$child"
echo "service $(state "$svc_pid"), child $(state "$child")"
start_process svc2 setsid bash -c 'echo $$ >"$0.leader"; sleep 300 & echo $! >"$0"; wait' "$work/child2"
until_true 10 "the setsid service's child" test -s "$work/child2"
leader2=$(<"$work/child2.leader") child2=$(<"$work/child2")
echo "setsid's pids $leader2 $child2"
[ "$svc2_pid" != "$leader2" ] && echo "with setsid the recorded pid is not the service's"
signal_group "$svc2_pid" TERM
stop_pid "$svc2_pid"
echo "with setsid: service $(state "$leader2"), child $(state "$child2")"`);
  killed.push(...pidsIn(/^setsid's pids (.*)$/m.exec(r.out)?.[1] ?? ""));
  expect(r.err).toBe("");
  expect(r.out.replace(/^setsid's pids .*\n/m, "")).toBe(
    "start 0\nleads its group\nservice gone, child gone\nwith setsid the recorded pid is not the service's\nwith setsid: service running, child running\n"
  );
});

// A service that has exited before start_process reads its start records none. start_process
// still succeeds under a driver's `set -e`, so await_start, not the shell's exit, names its log.
// proc_start answers nothing in the row, as it does for a service already gone or a zombie, since
// how fast a service exits is the machine's.
test("start_process of a service that is gone when its start is read succeeds, and await_start names the service's log", () => {
  const r = run(`set -euo pipefail
proc_start() { :; }
start_process quick true
echo "start_process returned"
await_start quick "$quick_pid" 5 "the quick service" false`);
  expect(r.code).toBe(1);
  expect(r.out).toBe("start_process returned\n");
  expect(r.err).toMatch(
    /^fail: the quick service: the process exited; see \S+\/logs\/quick\.log\n$/
  );
});

test("the broker walkthrough rig starts no service through setsid", () => {
  const rig = readFileSync(join(root, "docs/site/media/broker/rig.sh"), "utf8");
  expect(rig.split("\n").filter((line) => /\bstart_process \S+ setsid\b/.test(line))).toEqual([]);
});

// The broker walkthrough rig's helper writes its own pid and start before it runs (record_pid_sh),
// and the rig reads both (record_pid_file), at its start and in a cleanup that runs before that
// read. The row runs the rig's own cleanup: a pid the kernel has given to another process since is
// held to the helper's start, so the stranger is left running; the helper's own start lets the
// cleanup end it.
const broker = scriptFunctions(join(root, "docs/site/media/broker/rig.sh"));
test("the broker rig holds a helper pid it reads late to the start the helper wrote, so a stranger given that pid is left running", async () => {
  const stranger = Bun.spawn(ownGroup(["sleep", "60"]), { stdout: "ignore", stderr: "ignore" });
  killed.push(stranger.pid);
  await untilRunning(stranger.pid, "sleep");
  const start = readFileSync(`/proc/${stranger.pid}/stat`, "utf8")
    .replace(/^[\s\S]*\) /, "")
    .split(" ")[19];
  const r = run(`${goneOrRunning}
${broker("cleanup")}
mkdir -p "$work/agent"
helper_pid='' dispatch_pid='' fake_github_pid='' fake_envoy_pid='' broker_pid='' issuer_pid=''
dispatch_keeper_pid='' broker_database=''
printf '%s %s\\n' ${stranger.pid} ${Number(start) + 1} >"$work/agent/helper.pid"
(cleanup)
echo "cleanup, a stranger given the helper's pid: $(state ${stranger.pid})"
printf '%s %s\\n' ${stranger.pid} ${start} >"$work/agent/helper.pid"
(cleanup)
until_true 10 "the helper to end" gone ${stranger.pid}
echo "cleanup, the helper itself: $(state ${stranger.pid})"`);
  expect(r.out).toBe(
    "cleanup, a stranger given the helper's pid: running\ncleanup, the helper itself: gone\n"
  );
  expect(r.err).toBe("");
  const rig = readFileSync(join(root, "docs/site/media/broker/rig.sh"), "utf8");
  expect(rig).toMatch(/"\$record_pid_sh" "\$work\/agent\/helper\.pid"/);
  expect(rig).toMatch(
    /^until_true 30 "the helper's pid" record_pid_file "\$work\/agent\/helper\.pid" helper_pid$/m
  );
});

// stop_agent (envoy-session-states.sh and clean-session-stop-tmux.sh) ends an agent by its group's
// TERM and its own stop_pid; a member of the group that outlives the TERM is the cleanup sweep's,
// since a group is signalled only while its leader is the process recorded. Both rigs' cleanups run
// the sweep, and the sweep ends that member: it carries the run's mark, as every agent does. The
// sweep reads a proc root of the row's two processes (proc-root.ts's, made in the shell), so its
// time is the row's and not the host's process count.
const states = scriptFunctions(join(e2e, "envoy-session-states.sh"));
const cleanStop = scriptFunctions(join(e2e, "clean-session-stop-tmux.sh"));
test("an agent's member that outlives its TERM is left by stop_agent and ended by the cleanup sweep, which both agents' rigs run", () => {
  const r = run(`${goneOrRunning}
export LEGION_E2E_RUN=$work
${states("own_session")}
${states("agent_pid")}
${states("stop_agent")}
printf '%s\\n' "trap '' TERM" 'echo $$ >"$1"' 'exec sleep 300' >"$work/member.sh"
own_session "$work/agent.pid" bash -c 'bash "$1" "$2" & wait' agent "$work/member.sh" "$work/agent.member"
agent_pid "$work/agent.pid" agent
until_true 10 "the agent's member" test -s "$work/agent.member"
member=$(<"$work/agent.member")
echo "pids $agent $member"
mkdir "$work/proc"
ln -s "/proc/$agent" "$work/proc/$agent"
ln -s "/proc/$member" "$work/proc/$member"
export proc_root=$work/proc
stop_agent "$agent"
until_true 10 "the agent to end" gone "$agent"
echo "after stop_agent: member $(state "$member")"
stop_marked_processes "$work"
echo "sweep $?"
until_true 10 "the member to end" gone "$member"
echo "after the sweep: member $(state "$member")"`);
  killed.push(...pidsIn(/^pids (.*)$/m.exec(r.out)?.[1] ?? ""));
  expect(r.err).toBe("");
  expect(r.out.replace(/^pids .*\n/m, "")).toBe(
    "after stop_agent: member running\nsweep 0\nafter the sweep: member gone\n"
  );
  for (const cleanup of [states("cleanup"), cleanStop("cleanup")]) {
    expect(cleanup).toMatch(/^\s+stop_marked_processes "\$work"/m);
  }
  for (const stopAgent of [states("stop_agent"), cleanStop("stop_agent")]) {
    expect(stopAgent).not.toMatch(/stop_pid "\$1"\n\s+signal_group/);
  }
});
