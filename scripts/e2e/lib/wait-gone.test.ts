import { afterAll, beforeAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptFunctions } from "./script-functions";

// lib/wait-gone.sh: wait_gone, kill_gone and stop_gone, and the stops of the scripts that run
// them, each followed by the step that needs the process gone. Every stand-in is a real process
// of this test (holder.py below), and every check of what it held is a real read: a connect to
// its port, an flock of its lock, `rm -rf` of the directory it writes, the table run_processes
// reads. A stand-in leaves late in one of three ways: it ignores SIGTERM, or it goes on for a
// while after its SIGTERM, or it has threads and memory, so that the kernel releases its port and
// its lock only once the last of its threads has exited, which after a SIGKILL takes a
// measurable time, while /proc already shows its main thread as a zombie. A process that SIGKILL
// has not finished ending, as one in uninterruptible sleep stays, is a stand-in whose SIGKILL the
// harness's own `kill` drops (drop_kill): the wait must run to its deadline and name it. What
// /proc shows of a reused pid, a zombie whose other threads still run and an entry that cannot be
// read is a tree this test writes, the waits pointed at it by wait_gone_proc.
const lib = import.meta.dir;
const e2e = join(lib, "..");
const root = join(e2e, "..", "..");
const dir = temporaryDirectory("wait-gone-test");
const holderPath = join(dir, "holder.py");
const pidsFile = join(dir, "started.pids");
// A case waits out deadlines and stand-ins that leave late, up to about 12 s of its own, and a
// busy machine runs each stand-in slower still.
setDefaultTimeout(60_000);

// The stand-in, written once the suite's directory exists (temporaryDirectory makes it before the
// first test).
const holder = `import ctypes, fcntl, os, signal, socket, sys, threading, time

# holder.py KEY=VALUE...: a stand-in for a service a script stops. It listens on a loopback port
# the kernel picks and writes the number to port=FILE once all of the below is set up.
#   term=default|ignore|slow:SECONDS  what SIGTERM does: end it, nothing, or end it SECONDS later
#   write=DIR   with term=slow, it creates files in DIR until it ends, as a server's drain writes
#   log=FILE    it appends "serving" to FILE, and with term=slow "shutdown-complete" as it ends
#   lock=FILE   it holds an flock on FILE
#   threads=N   it starts N threads that sleep, as every Go binary and Bun has threads
#   mib=M       it touches M MiB, which its exit after SIGKILL takes a measurable time to release
#   leader=exit its main thread exits once it is set up (pthread_exit), and its other threads keep
#               the port and the lock: /proc shows the process's main thread as a zombie
#   cwd=DIR     it works in DIR
args = dict(arg.split("=", 1) for arg in sys.argv[1:])
if "cwd" in args:
    os.chdir(args["cwd"])
term = args.get("term", "default")
if term == "ignore":
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
elif term.startswith("slow:"):
    delay = float(term[5:])

    def on_term(signum, frame):
        end, i = time.monotonic() + delay, 0
        while time.monotonic() < end:
            if "write" in args:
                open(os.path.join(args["write"], "drain.%d" % i), "w").close()
                i += 1
            else:
                time.sleep(0.01)
        if "log" in args:
            with open(args["log"], "a") as log:
                log.write("shutdown-complete\\n")
        os._exit(0)

    signal.signal(signal.SIGTERM, on_term)
listener = socket.socket()
listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
listener.bind(("127.0.0.1", 0))
listener.listen()
if "lock" in args:
    lock = open(args["lock"], "a")
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
for _ in range(int(args.get("threads", "0"))):
    threading.Thread(target=time.sleep, args=(3600,), daemon=True).start()
memory = bytearray(int(args.get("mib", "0")) << 20)
for i in range(0, len(memory), 4096):
    memory[i] = 1
if "log" in args:
    with open(args["log"], "a") as log:
        log.write("serving\\n")
with open(args["port"] + ".new", "w") as port:
    port.write(str(listener.getsockname()[1]))
os.rename(args["port"] + ".new", args["port"])
if args.get("leader") == "exit":
    ctypes.CDLL(None).pthread_exit(None)
time.sleep(3600)
`;
beforeAll(() => writeFileSync(holderPath, holder));

// Every stand-in a harness starts is recorded in started.pids; one a harness could not end is
// ended here, by that pid, while its command line still names holder.py, so a pid the kernel gave
// to another process since is never signalled.
afterAll(() => {
  if (!existsSync(pidsFile)) return;
  for (const line of readFileSync(pidsFile, "utf8").split("\n")) {
    const pid = Number(line);
    if (!pid) continue;
    try {
      if (!readFileSync(`/proc/${pid}/cmdline`, "utf8").includes(holderPath)) continue;
      process.kill(pid, "SIGKILL");
    } catch {}
  }
});

// The shell every harness runs above its body. Paths come in through the environment, so no
// harness's own command line names a run's work directory (run_processes reads command lines).
// Its EXIT trap ends what it started; a TERM (bun's, on a timeout) runs that trap too.
const prelude = `set -u
. "$ROOT/scripts/e2e/lib/wait-gone.sh"
started=()
trap 'for p in \${started[@]+"\${started[@]}"}; do builtin kill -KILL "$p" 2>/dev/null; done; wait 2>/dev/null' EXIT
trap 'exit 143' TERM
# holder NAME KEY=VALUE...: holder.py with those settings, in the background; NAME is its pid and
# NAME_port its port once it has written it.
holder() {
  local name=$1 file
  shift
  file=$(mktemp -u "$DIR/port.XXXXXX")
  python3 "$HOLDER" port="$file" "$@" &
  started+=("$!")
  echo "$!" >>"$PIDS"
  printf -v "$name" '%s' "$!"
  for _ in $(seq 1 600); do [ -s "$file" ] && break; sleep 0.05; done
  [ -s "$file" ] || { echo "the stand-in $name wrote no port" >&2; exit 2; }
  printf -v "\${name}_port" '%s' "$(<"$file")"
}
now_ms() { echo $((\${EPOCHREALTIME//[!0-9]/} / 1000)); }
listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
held() { ! flock -n "$1" true; }
# drop_kill PID: a kill that drops every SIGKILL for PID and passes every other call on, so PID
# stays in the process table as a process SIGKILL has not finished ending does.
drop_kill() {
  spare=$1
  kill() {
    case " $* " in *" KILL "* | *" -KILL "* | *" -9 "*) [[ " $* " == *" $spare "* ]] && return 0 ;; esac
    builtin kill "$@"
  }
}
`;

function bash(body: string, env: Record<string, string> = {}, timeout = 60_000) {
  const ran = Bun.spawnSync(["bash", "-c", `${prelude}\n${body}`], {
    env: {
      PATH: process.env.PATH ?? "",
      ROOT: root,
      DIR: dir,
      HOLDER: holderPath,
      PIDS: pidsFile,
      ...env,
    },
    stdout: "pipe",
    stderr: "pipe",
    timeout,
  });
  return { code: ran.exitCode, stdout: ran.stdout.toString(), stderr: ran.stderr.toString() };
}

// The lines KEY=VALUE a harness prints, as a map.
function values(out: string) {
  return Object.fromEntries(
    out
      .split("\n")
      .filter((line) => /^[a-z_]+=/.test(line))
      .map((line) => [line.slice(0, line.indexOf("=")), line.slice(line.indexOf("=") + 1)])
  );
}

let dirs = 0;
function freshDir(name: string) {
  const made = join(dir, `${name}-${++dirs}`);
  mkdirSync(made, { recursive: true });
  return made;
}

describe("wait_gone and kill_gone", () => {
  test("a sleep given SIGTERM is gone at once", () => {
    const r = bash(`sleep 30 & started+=("$!")
kill_gone TERM "$((EPOCHSECONDS + 5))" "$!"; echo "rc=$?"`);
    expect(values(r.stdout).rc).toBe("0");
  });

  test("a process that ignores SIGTERM is still there at the deadline, named, and gone given SIGKILL", () => {
    const r = bash(`bash -c 'trap "" TERM; exec sleep 30' & b=$!; started+=("$b")
sleep 0.3
deadline=$((EPOCHSECONDS + 2))
kill_gone TERM "$deadline" "$b"; echo "rc=$?"
echo "ended_ms=$(now_ms)"; echo "deadline=$deadline"; echo "pid=$b"
kill_gone KILL "$((EPOCHSECONDS + 5))" "$b"; echo "killed=$?"`);
    const v = values(r.stdout);
    expect(v.rc).toBe("1");
    expect(r.stderr).toMatch(
      new RegExp(
        `^pid ${v.pid} \\(sleep 30\\) is still in the process table at its deadline, [0-9]+\\.[0-9] s after its SIGTERM$`,
        "m"
      )
    );
    // It never gives up before the deadline, and it ends at it, not a bound later.
    const ended = Number(v.ended_ms);
    expect(ended).toBeGreaterThanOrEqual(Number(v.deadline) * 1000);
    expect(ended).toBeLessThan(Number(v.deadline) * 1000 + 1500);
    expect(v.killed).toBe("0");
  });

  test("a zombie whose parent is stopped counts as gone, before the deadline", () => {
    const r = bash(`zpid=$(mktemp -u "$DIR/zpid.XXXXXX")
bash -c 'sleep 30 & echo $! >"$1"; wait' zombie-parent "$zpid" & parent=$!; started+=("$parent")
while [ ! -s "$zpid" ]; do sleep 0.05; done
z=$(<"$zpid")
kill -STOP "$parent"
start=$(now_ms)
kill_gone KILL "$((EPOCHSECONDS + 5))" "$z"; echo "rc=$?"
echo "ms=$(($(now_ms) - start))"
echo "state=$(awk '{print $3}' "/proc/$z/stat" 2>/dev/null)"
kill -CONT "$parent"`);
    const v = values(r.stdout);
    expect(v.rc).toBe("0");
    expect(v.state).toBe("Z");
    expect(Number(v.ms)).toBeLessThan(2000);
  });

  test("a whole process group given SIGKILL is gone", () => {
    const r = bash(`setsid bash -c 'sleep 30 & sleep 30 & wait' & g=$!; started+=("$g")
sleep 0.3
kill_gone KILL "$((EPOCHSECONDS + 5))" "-$g"; echo "rc=$?"`);
    expect(values(r.stdout).rc).toBe("0");
  });

  test("a pid with no process is gone", () => {
    const r = bash(`wait_gone "$((EPOCHSECONDS + 1))" 999999999; echo "rc=$?"`);
    expect(values(r.stdout).rc).toBe("0");
  });

  test("a deadline that is no time since the epoch is refused, naming it", () => {
    const r = bash(`sleep 30 & started+=("$!")
wait_gone 10 "$!"; echo "wait=$?"
kill_gone TERM 10 "$!"; echo "kill=$?"
stop_gone 1 10 "$!"; echo "stop=$?"
kill -0 "$!" && echo alive=yes`);
    const v = values(r.stdout);
    expect([v.wait, v.kill, v.stop, v.alive]).toEqual(["2", "2", "2", "yes"]);
    expect(r.stderr).toContain("wait_gone: the deadline '10' is no time in seconds since the epoch");
  });
});

describe("a process with threads", () => {
  test("is not gone while its main thread is a zombie and another thread holds its port and lock", () => {
    const lock = join(freshDir("threads"), "lock");
    const r = bash(
      `holder h threads=1 leader=exit lock="$LOCK"
for _ in $(seq 1 100); do [ "$(awk '{print $3}' "/proc/$h/stat")" = Z ] && break; sleep 0.05; done
echo "state=$(awk '{print $3}' "/proc/$h/stat")"
wait_gone "$((EPOCHSECONDS + 1))" "$h"; echo "rc=$?"
echo "left=\${wait_gone_left[*]}"
echo "pid=$h"
listening "$h_port" && echo listening=yes
held "$LOCK" && echo held=yes
kill_gone KILL "$((EPOCHSECONDS + 10))" "$h"; echo "killed=$?"
listening "$h_port" || echo listening_after=no
held "$LOCK" || echo held_after=no`,
      { LOCK: lock }
    );
    const v = values(r.stdout);
    expect(v.state).toBe("Z");
    expect([v.rc, v.left]).toEqual(["1", v.pid]);
    expect([v.listening, v.held]).toEqual(["yes", "yes"]);
    expect([v.killed, v.listening_after, v.held_after]).toEqual(["0", "no", "no"]);
  });

  test("is gone after its SIGKILL only once its last thread has released the port and the lock", () => {
    const lock = join(freshDir("threads-kill"), "lock");
    const r = bash(
      `for t in 1 2 3; do
  holder h threads=8 mib=1024 lock="$LOCK.$t"
  kill_gone KILL "$((EPOCHSECONDS + 10))" "$h"; rc=$?
  listening "$h_port" && echo "trial $t: port held after kill_gone returned $rc"
  held "$LOCK.$t" && echo "trial $t: lock held after kill_gone returned $rc"
done
echo done=yes`,
      { LOCK: lock }
    );
    expect(r.stdout).toBe("done=yes\n");
  });
});

describe("what wait_gone reads of /proc", () => {
  // A tree of the test's own, so the cases do not depend on a reused pid or a slow exit: each
  // entry is a stat line whose fields 3 (state) and 22 (start time) are what the case sets, and a
  // task directory. The harness's kill drops every signal, so no pid of this tree reaches a real
  // process.
  const stat = (pid: number, state: string, start: number) =>
    `${pid} (stand in) ${state} 1 ${pid} ${pid} 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 ${start} 0 0\n`;
  function fakeProc(entries: Record<number, { state?: string; start?: number; tasks?: number[] }>) {
    const proc = freshDir("proc");
    mkdirSync(join(proc, "self"));
    writeFileSync(join(proc, "self", "stat"), stat(1, "S", 1));
    for (const [pid, e] of Object.entries(entries)) {
      mkdirSync(join(proc, pid, "task"), { recursive: true });
      writeFileSync(join(proc, pid, "stat"), stat(Number(pid), e.state ?? "S", e.start ?? 100));
      for (const task of e.tasks ?? [Number(pid)]) mkdirSync(join(proc, pid, "task", String(task)));
    }
    return proc;
  }
  // wait_gone in the background against PROC, CHANGE run 0.5 s in, and what it returned and when.
  function waitAcross(proc: string, pid: number, change: string) {
    return bash(
      `wait_gone_proc=$PROC
kill() { return 0; }
deadline=$((EPOCHSECONDS + 4))
start=$(now_ms)
( wait_gone "$deadline" ${pid}; echo "rc=$? ms=$(($(now_ms) - start))" ) &
sleep 0.5
${change}
wait $!`,
      { PROC: proc }
    );
  }

  test("a pid whose start time changes is a new process, and the one waited on is gone", () => {
    const proc = fakeProc({ 4242: { start: 100 } });
    const r = waitAcross(
      proc,
      4242,
      `printf '%s\\n' "$(sed 's/ 100 0 0$/ 200 0 0/' "$PROC/4242/stat")" >"$PROC/4242/stat.new" && mv "$PROC/4242/stat.new" "$PROC/4242/stat"`
    );
    const [rc, ms] = r.stdout.trim().split(" ");
    expect(rc).toBe("rc=0");
    expect(Number(ms.slice(3))).toBeLessThan(2500);
  });

  test("a pid whose start time stays is the same process, still there at the deadline", () => {
    const proc = fakeProc({ 4242: { start: 100 } });
    const r = waitAcross(proc, 4242, ":");
    expect(r.stdout.trim().split(" ")[0]).toBe("rc=1");
  });

  test("a zombie whose task directory lists another thread is not gone until that thread is", () => {
    const proc = fakeProc({ 4244: { state: "Z", tasks: [4244, 4245] } });
    const kept = waitAcross(proc, 4244, ":");
    expect(kept.stdout.trim().split(" ")[0]).toBe("rc=1");
    const r = waitAcross(proc, 4244, `rmdir "$PROC/4244/task/4245"`);
    const [rc, ms] = r.stdout.trim().split(" ");
    expect(rc).toBe("rc=0");
    expect(Number(ms.slice(3))).toBeLessThan(2500);
  });

  test("a zombie with no thread but its main one is gone at once", () => {
    const proc = fakeProc({ 4246: { state: "Z" } });
    const r = waitAcross(proc, 4246, ":");
    const [rc, ms] = r.stdout.trim().split(" ");
    expect(rc).toBe("rc=0");
    expect(Number(ms.slice(3))).toBeLessThan(1000);
  });

  test("an entry whose stat cannot be read is not gone, and kill_gone says why it names it", () => {
    const proc = fakeProc({});
    mkdirSync(join(proc, "4243", "stat"), { recursive: true });
    const r = bash(
      `wait_gone_proc=$PROC
kill() { return 0; }
kill_gone TERM "$((EPOCHSECONDS + 1))" 4243; echo "rc=$?"`,
      { PROC: proc }
    );
    expect(values(r.stdout).rc).toBe("1");
    expect(r.stderr).toContain(`pid 4243: ${proc}/4243/stat could not be read, so it is not known to be gone at its deadline`);
    rmSync(join(proc, "4243"), { recursive: true });
  });
});

describe("stop_gone: one deadline for the SIGTERM's wait and the SIGKILL's", () => {
  test("a process that ends on SIGTERM is gone, given no SIGKILL", () => {
    const r = bash(`holder h term=slow:0.5
stop_gone 10 "$((EPOCHSECONDS + 20))" "$h"; echo "rc=$?"
wait "$h"; echo "status=$?"`);
    expect(values(r.stdout)).toMatchObject({ rc: "0", status: "0" });
  });

  test("a process that ignores SIGTERM gets its SIGKILL at the grace and is gone", () => {
    const r = bash(`holder h term=ignore
start=$(now_ms)
stop_gone 1 "$((EPOCHSECONDS + 20))" "$h"; echo "rc=$?"
echo "ms=$(($(now_ms) - start))"
wait "$h"; echo "status=$?"`);
    const v = values(r.stdout);
    expect([v.rc, v.status]).toEqual(["0", String(128 + 9)]);
    expect(Number(v.ms)).toBeLessThan(5000);
  });

  test("a SIGTERM wait that takes the caller's time leaves the SIGKILL's wait none past the deadline", () => {
    const r = bash(`holder h term=ignore
drop_kill "$h"
deadline=$((EPOCHSECONDS + 2))
stop_gone 10 "$deadline" "$h"; echo "rc=$?"
echo "ended_ms=$(now_ms)"; echo "deadline=$deadline"; echo "pid=$h"`);
    const v = values(r.stdout);
    expect(v.rc).toBe("1");
    expect(Number(v.ended_ms)).toBeGreaterThanOrEqual(Number(v.deadline) * 1000);
    expect(Number(v.ended_ms)).toBeLessThan(Number(v.deadline) * 1000 + 1500);
    expect(r.stderr).toMatch(
      new RegExp(`^pid ${v.pid} \\(python3 .*holder\\.py.*\\) is still in the process table at its deadline, [0-9.]+ s after its SIGKILL$`, "m")
    );
  });
});

// The stops of the scripts, each cut out of its script by name, and the step after it that needs
// the process gone: the five kinds this change waits for (a port, a removed directory, a file the
// process writes, the process table, a lock).
describe("each stop of the scripts, then the step that needs the process gone", () => {
  const stage4b = scriptFunctions(join(e2e, "stage4b-sandbox-tree.sh"));
  const stage2 = scriptFunctions(join(e2e, "stage2-tmux-supervision.sh"));
  const verifiers = scriptFunctions(join(e2e, "verifiers-staging-token.sh"));
  const stage1 = scriptFunctions(join(e2e, "stage1-skeleton.sh"));
  const rig = `root=$ROOT work=$WORK evidence=$WORK timeout_hook=
note() { :; }
fail() { echo "FAIL $*"; exit 1; }
. "$ROOT/scripts/e2e/lib/rig.sh"`;

  test("port: lib/rig.sh's stop_pid of a daemon that ignores SIGTERM, then Stage 4b's ports_free", () => {
    const work = freshDir("port");
    const r = bash(
      `${rig}
${stage4b("ports_free")}
stop_pid_grace=1
for t in 1 2 3; do
  holder d term=ignore threads=8 mib=1024
  stop_pid "$d" || echo "trial $t: stop_pid returned 1"
  ports_free "$d_port"
done
echo free=yes`,
      { WORK: work }
    );
    expect(r.stdout).toBe("free=yes\n");
  });

  test("port: Stage 2's stop_pid of a daemon that drains for a second on SIGTERM, then the port's next bind", () => {
    const work = freshDir("port-stage2");
    const r = bash(
      `${stage4b("ports_free")}
${stage2("stop_pid")}
fail() { echo "FAIL $*"; exit 1; }
for t in 1 2 3; do
  holder d term=slow:1 threads=8 mib=256
  stop_pid "$d" || echo "trial $t: stop_pid returned 1"
  ports_free "$d_port"
done
echo free=yes`,
      { WORK: work }
    );
    expect(r.stdout).toBe("free=yes\n");
  });

  test("lock: lib/box-login.sh's stop_box_login_processes of a pane that ignores SIGTERM, then the next run's take_box_login_lock", () => {
    const lock = join(freshDir("lock"), "box-login.lock");
    const r = bash(
      `. "$ROOT/scripts/e2e/lib/box-login.sh"
box_login_stop_grace=1
for t in 1 2 3; do
  holder pane term=ignore threads=8 mib=1024 lock="$LOCK.$t"
  stop_box_login_processes "$pane" || echo "trial $t: the stop returned 1"
  box_login_lock_file=$LOCK.$t bash -c '. "$ROOT/scripts/e2e/lib/box-login.sh"; take_box_login_lock' || echo "trial $t: refused"
done
echo took=yes`,
      { LOCK: lock }
    );
    expect(r.stdout).toBe("took=yes\n");
  });

  test("lock: a pane SIGKILL has not ended is waited for to the deadline and named, never left holding the lock unsaid", () => {
    const lock = join(freshDir("lock-stays"), "box-login.lock");
    const r = bash(
      `. "$ROOT/scripts/e2e/lib/box-login.sh"
box_login_stop_grace=1
holder pane term=ignore lock="$LOCK"
drop_kill "$pane"
start=$(now_ms)
stop_box_login_processes "$pane"; echo "rc=$?"
echo "ms=$(($(now_ms) - start))"
echo "pid=$pane"`,
      { LOCK: lock }
    );
    const v = values(r.stdout);
    expect(v.rc).toBe("1");
    expect(Number(v.ms)).toBeGreaterThanOrEqual(10_000);
    const pid = v.pid;
    expect(r.stderr).toMatch(new RegExp(`^pid ${pid} \\(python3 .*holder\\.py.*\\) is still in the process table at its deadline`, "m"));
  });

  test("directory: the verifiers proof's cleanup of a Dispatch that drains into $work for 2 s, then the next run's rm -rf", () => {
    const work = join(freshDir("verifiers"), "verifiers-e2e");
    mkdirSync(join(work, "home", "data"), { recursive: true });
    for (let i = 0; i < 3000; i++) writeFileSync(join(work, "home", "data", `record.${i}`), "");
    const r = bash(
      `docker() { :; }
pg_name=none nats_name=none listener_pid=
${verifiers("cleanup")}
holder dispatch_pid term=slow:2 write="$WORK/home/data"
cleanup
rm -rf "$WORK" && echo removed=yes`,
      { WORK: work }
    );
    expect(values(r.stdout).removed).toBe("yes");
    expect(existsSync(work)).toBe(false);
  });

  test("file: Stage 1's cleanup of a failed run prints the daemon's log with the line it writes 1 s after its SIGTERM", () => {
    const work = freshDir("stage1");
    mkdirSync(join(work, "logs"));
    const r = bash(
      `${rig}
docker() { :; }
container=none ok= daemon_keeper_pid=
${stage1("cleanup")}
holder pid term=slow:1 log="$WORK/logs/daemon.log"
cleanup`,
      { WORK: work }
    );
    expect(r.stdout).toContain("serving\nshutdown-complete\n");
  });

  test("process table: Stage 3's sweep of a run's process with threads and memory, then its read of the table", () => {
    const work = freshDir("table");
    // The checkpoint's own lines, from its sweep to its read of the table.
    const script = readFileSync(join(e2e, "stage3-devbox-workflow.sh"), "utf8");
    const block = /^kill_gone KILL .*\$\(run_processes\)[^\n]*\n[\s\S]*?a proof process remains"\n/m.exec(script);
    expect(block).not.toBeNull();
    const r = bash(
      `${rig}
docker() { :; }
tmux() { return 1; }
pg_container=none nats_container=none ptoken=none
for t in 1 2 3; do
  holder p cwd="$WORK" term=ignore threads=8 mib=1024
  ${block?.[0]}
done
echo swept=yes`,
      { WORK: work }
    );
    expect(r.stdout).toBe("swept=yes\n");
  });

  test("Stage 4b's restart_daemon does not start a second daemon beside one still there at its stop's deadline", () => {
    const work = freshDir("restart");
    const run = (stays: boolean) =>
      bash(
        `${rig}
write_legion_config() { :; }
start_daemon() { echo started=yes; }
${stage4b("restart_daemon")}
stop_pid_grace=0
holder daemon_pid term=${stays ? "ignore" : "default"}
${stays ? 'drop_kill "$daemon_pid"' : ""}
echo "pid=$daemon_pid"
restart_daemon`,
        { WORK: work }
      );
    const stays = run(true);
    const pid = values(stays.stdout).pid;
    expect(values(stays.stdout).started).toBeUndefined();
    expect(stays.stdout).toContain(`FAIL the daemon (pid ${pid}) did not stop (named above); its replacement would meet its ports`);
    expect(stays.stderr).toMatch(new RegExp(`^pid ${pid} \\(python3 .*holder\\.py.*\\) is still in the process table at its deadline`, "m"));
    // The control: a daemon that leaves in time lets the restart run.
    expect(values(run(false).stdout).started).toBe("yes");
  });
});
