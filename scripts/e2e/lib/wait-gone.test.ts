import { describe, expect, test } from "bun:test";
import { mkdirSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { values, waitGoneHarness } from "./wait-gone-harness";

// lib/wait-gone.sh: wait_gone, kill_gone and stop_gone themselves. Every stand-in is a real process
// of this test (holder.py, wait-gone-harness.ts), and every check of what it held is a real read: a
// connect to its port, an flock of its lock. A stand-in leaves late in one of three ways: it ignores
// SIGTERM, or it goes on for a while after its SIGTERM, or it has threads and memory, so that the
// kernel releases its port and its lock only once the last of its threads has exited, which after a
// SIGKILL takes a measurable time, while /proc already shows its main thread as a zombie. A process
// that SIGKILL has not finished ending, as one in uninterruptible sleep stays, is a stand-in whose
// SIGKILL the harness's own `kill` drops (drop_kill): the wait must run to its deadline and name it.
// What /proc shows of a reused pid, a zombie whose other threads still run and an entry that cannot
// be read is a tree this test writes, the waits pointed at it by wait_gone_proc. A pid gone does not
// free what it held when a child it forked holds the same socket and lock: a stand-in that forks one
// (child=) shows that a wait on the pid alone returns with both still held, and that a wait on the
// port and the lock themselves (`port:N`, `lock:FILE`) does not. wait-gone-scripts.test.ts holds the
// scripts' stops.
const { bash, standIn, freshDir } = waitGoneHarness("wait-gone-test");

describe("wait_gone and kill_gone", () => {
  test("a process that leaves 0.3 s after its SIGTERM is waited for, and the wait returns within a read of its exit", () => {
    // The stand-in ends 0.3 s after its SIGTERM, so it is still there at the wait's first read and
    // the wait must read it again: a sleep that took SIGTERM at once was often gone at that first
    // read, and a wait that slept seconds between reads then passed. An observer reads the
    // stand-in's /proc entry in a loop of builtins alone, and writes the moment it reads it gone (no
    // entry, or a zombie): its exit. wait_gone reads its targets every 100 ms, so it returns within
    // one read of that moment; the bound gives the box's scheduling 900 ms more, where a wait that
    // slept seconds between reads would return seconds late.
    const r = bash(`gone=$(mktemp -u "$DIR/gone.XXXXXX") ready=$(mktemp -u "$DIR/ready.XXXXXX")
bash -c 'trap "sleep 0.3; exit 0" TERM; : >"$1"; while :; do sleep 0.05; done' stand-in "$ready" & s=$!; started+=("$s")
until [ -e "$ready" ]; do sleep 0.01; done
( while { read -r st <"/proc/$s/stat"; } 2>/dev/null && [[ \${st##*) } != Z* ]]; do :; done
  echo "$(now_ms)" >"$gone" ) &
echo "sent_ms=$(now_ms)"
kill_gone TERM "$END" "$s"; echo "rc=$?"
echo "returned_ms=$(now_ms)"
wait "$!"
echo "gone_ms=$(<"$gone")"`);
    const v = values(r.stdout);
    expect(v.rc).toBe("0");
    // It outlived its SIGTERM by its drain, so the wait read it at least twice.
    expect(Number(v.gone_ms) - Number(v.sent_ms)).toBeGreaterThanOrEqual(250);
    expect(Number(v.returned_ms) - Number(v.gone_ms)).toBeLessThan(1000);
  });

  test("a process that ignores SIGTERM is still there at the deadline, named, and gone given SIGKILL", () => {
    // The stand-in ignores TERM from its trap on, and keeps that across its exec, so the case waits
    // until it runs `sleep`: a TERM sent before the trap would end it at once on a loaded machine.
    const r = bash(`bash -c 'trap "" TERM; exec sleep 30' & b=$!; started+=("$b")
for _ in $(seq 1 600); do [ "$(cat "/proc/$b/comm" 2>/dev/null)" = sleep ] && break; sleep 0.05; done
deadline=$((EPOCHSECONDS + 2))
kill_gone TERM "$deadline" "$b"; echo "rc=$?"
echo "ended_ms=$(now_ms)"; echo "deadline=$deadline"; echo "pid=$b"
kill_gone KILL "$END" "$b"; echo "killed=$?"`);
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
    expect(ended).toBeLessThan(Number(v.deadline) * 1000 + 4000);
    expect(v.killed).toBe("0");
  });

  test("a zombie whose parent is stopped counts as gone, before the deadline", () => {
    const r = bash(`zpid=$(mktemp -u "$DIR/zpid.XXXXXX")
bash -c 'sleep 30 & echo $! >"$1"; wait' zombie-parent "$zpid" & parent=$!; started+=("$parent")
while [ ! -s "$zpid" ]; do sleep 0.05; done
z=$(<"$zpid")
kill -STOP "$parent"
# Stopped before the kill: a parent the stop has not reached yet would reap the zombie at once.
until [ "$(awk '{print $3}' "/proc/$parent/stat")" = T ]; do sleep 0.01; done
start=$(now_ms)
kill_gone KILL "$END" "$z"; echo "rc=$?"
echo "ms=$(($(now_ms) - start))"
echo "state=$(awk '{print $3}' "/proc/$z/stat" 2>/dev/null)"
kill -CONT "$parent"`);
    const v = values(r.stdout);
    expect(v.rc).toBe("0");
    expect(v.state).toBe("Z");
    // Its parent stays stopped until after: a wait that did not count the zombie gone would have run
    // to END and returned 1.
  });

  // A group's member that leaves late, and one that SIGKILL has not ended: what kill_label and
  // stop_label (envoy-session-delivery.sh), replace-blocks.sh's stop_instance and dev-stack.test.sh's
  // cleanup wait on before their next step meets the group's port, session or directory.
  test("a process group whose member drains for 2 s after SIGTERM is gone, every member, when the wait returns", () => {
    // The member writes READY once its TERM trap is set: a TERM before it would end it at once.
    const r = bash(`ready=$(mktemp -u "$DIR/ready.XXXXXX")
setsid bash -c 'trap "sleep 2; exit 0" TERM; : >"$1"; while :; do sleep 0.1; done' leader "$ready" & g=$!; started+=("$g")
until [ "$(awk '{print $5}' "/proc/$g/stat" 2>/dev/null)" = "$g" ] && [ -e "$ready" ]; do sleep 0.05; done
start=$(now_ms)
kill_gone TERM "$END" "-$g"; echo "rc=$?"
echo "ms=$(($(now_ms) - start))"
kill -0 -- "-$g" 2>/dev/null && echo still=yes`);
    const v = values(r.stdout);
    expect(v.rc).toBe("0");
    expect(v.still).toBeUndefined();
    expect(Number(v.ms)).toBeGreaterThanOrEqual(1500);
  });

  test("a process group whose leader leaves at once on SIGTERM while another member drains for 2 s is gone, every member, when the wait returns", () => {
    // The member that leaves late is not the group's leader, as a service's child that keeps its
    // port outlives the shell that started it: a wait that read the leader alone would return as
    // the leader goes, with the group still holding that member and what it holds.
    const r = bash(`ready=$(mktemp -u "$DIR/member.XXXXXX")
setsid bash -c 'bash -c "trap \\"sleep 2; exit 0\\" TERM; : >\\"\\$1\\"; while :; do sleep 0.1; done" member "$1" & trap "exit 0" TERM; while :; do sleep 0.1; done' leader "$ready" & g=$!; started+=("$g")
until [ "$(awk '{print $5}' "/proc/$g/stat" 2>/dev/null)" = "$g" ] && [ -e "$ready" ]; do sleep 0.05; done
start=$(now_ms)
kill_gone TERM "$END" "-$g"; echo "rc=$?"
echo "ms=$(($(now_ms) - start))"
kill -0 -- "-$g" 2>/dev/null && echo still=yes
builtin kill -KILL -- "-$g" 2>/dev/null`);
    const v = values(r.stdout);
    expect(v.rc).toBe("0");
    expect(v.still).toBeUndefined();
    expect(Number(v.ms)).toBeGreaterThanOrEqual(1500);
  });

  test("a process group with a member SIGKILL has not ended is waited for to the deadline and named", () => {
    const r = bash(`setsid bash -c 'sleep 30 & wait' & g=$!; started+=("$g")
until [ "$(awk '{print $5}' "/proc/$g/stat" 2>/dev/null)" = "$g" ]; do sleep 0.05; done
drop_kill "-$g"
deadline=$((EPOCHSECONDS + 2))
kill_gone KILL "$deadline" "-$g"; echo "rc=$?"
echo "ended_ms=$(now_ms)"; echo "deadline=$deadline"; echo "group=$g"
builtin kill -KILL -- "-$g" 2>/dev/null`);
    const v = values(r.stdout);
    expect(v.rc).toBe("1");
    expect(Number(v.ended_ms)).toBeGreaterThanOrEqual(Number(v.deadline) * 1000);
    expect(Number(v.ended_ms)).toBeLessThan(Number(v.deadline) * 1000 + 4000);
    expect(r.stderr).toMatch(
      new RegExp(
        `^process group ${v.group} still has a process at its deadline, [0-9]+\\.[0-9] s after its SIGKILL$`,
        "m"
      )
    );
  });

  test("a process group whose one member left is a zombie its parent, outside the group, does not reap is gone, as a pid is", () => {
    // kill -0 answers for a zombie member, which holds nothing, so a group is read process by
    // process, each by the rule a pid is (_wait_gone_group_there). The member's parent works outside
    // the group and is stopped, so nothing reaps the member once the SIGTERM ends it: a wait that
    // read kill -0 alone would run to END and return 1.
    const r =
      bash(`leader=$(mktemp -u "$DIR/leader.XXXXXX") member=$(mktemp -u "$DIR/member.XXXXXX")
python3 -c 'import os, signal, sys, time
os.setpgid(0, 0)
signal.signal(signal.SIGTERM, lambda *_: os._exit(0))
open(sys.argv[1] + ".new", "w").write(str(os.getpid())); os.rename(sys.argv[1] + ".new", sys.argv[1])
time.sleep(600)' "$leader" & g=$!; started+=("$g")
until [ -s "$leader" ]; do sleep 0.05; done
python3 -c 'import os, sys, time
pid = os.fork()
if pid == 0:
    os.setpgid(0, int(sys.argv[1]))
    open(sys.argv[2] + ".new", "w").write(str(os.getpid())); os.rename(sys.argv[2] + ".new", sys.argv[2])
    time.sleep(600)
    os._exit(0)
os.waitpid(pid, 0)
time.sleep(600)' "$g" "$member" & parent=$!; started+=("$parent")
until [ -s "$member" ]; do sleep 0.05; done
m=$(<"$member"); started+=("$m")
echo "group_of_member=$(awk '{print $5}' "/proc/$m/stat")"; echo "group=$g"
kill -STOP "$parent"
# Stopped before the kill: a parent the stop has not reached yet would reap the member at once.
until [ "$(awk '{print $3}' "/proc/$parent/stat")" = T ]; do sleep 0.01; done
start=$(now_ms)
kill_gone TERM "$END" "-$g"; echo "rc=$?"
echo "ms=$(($(now_ms) - start))"
echo "state=$(awk '{print $3}' "/proc/$m/stat" 2>/dev/null)"
kill -CONT "$parent"
builtin kill -KILL "$parent" 2>/dev/null`);
    const v = values(r.stdout);
    expect(v.group_of_member).toBe(v.group);
    expect(v.rc).toBe("0");
    expect(v.state).toBe("Z");
    expect(Number(v.ms)).toBeLessThan(5000);
  });

  test("a pid with no process is gone", () => {
    const r = bash(`wait_gone "$END" 999999999; echo "rc=$?"`);
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
    expect(r.stderr).toContain(
      "wait_gone: the deadline '10' is no time in seconds since the epoch"
    );
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
kill_gone KILL "$END" "$h" "port:$h_port" "lock:$LOCK"; echo "killed=$?"
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

  test("given SIGKILL with its port and lock as targets, is waited for until both are free", () => {
    const lock = join(freshDir("threads-kill"), "lock");
    const r = bash(
      `for t in 1 2 3; do
  holder h threads=8 mib=1024 lock="$LOCK.$t"
  kill_gone KILL "$END" "$h" "port:$h_port" "lock:$LOCK.$t"; rc=$?
  listening "$h_port" && echo "trial $t: port held after kill_gone returned $rc"
  held "$LOCK.$t" && echo "trial $t: lock held after kill_gone returned $rc"
done
echo done=yes`,
      { LOCK: lock }
    );
    expect(r.stdout).toBe("done=yes\n");
  });
});

describe("a port or a lock a forked child of the process still holds", () => {
  test("a wait on the pid alone returns while the child still holds the port and the lock", () => {
    const d = freshDir("child-control");
    const r = bash(
      `holder h release="$D/release" lock="$D/lock" pids="$PIDS"
kill_gone KILL "$END" "$h"; echo "rc=$?"
listening "$h_port" && echo port=held
held "$D/lock" && echo lock=held
touch "$D/release"
kill_gone KILL "$END" "port:$h_port" "lock:$D/lock"; echo "freed=$?"`,
      { D: d }
    );
    expect(values(r.stdout)).toMatchObject({ rc: "0", port: "held", lock: "held", freed: "0" });
  });

  test("kill_gone given the port and the lock returns only once the child has let both go", () => {
    const d = freshDir("child");
    const r = bash(
      `for t in 1 2 3; do
  holder h child=1 lock="$D/lock.$t" pids="$PIDS"
  kill_gone KILL "$END" "$h" "port:$h_port" "lock:$D/lock.$t"; rc=$?
  listening "$h_port" && echo "trial $t: port held after kill_gone returned $rc"
  held "$D/lock.$t" && echo "trial $t: lock held after kill_gone returned $rc"
done
echo done=yes`,
      { D: d }
    );
    expect(r.stdout).toBe("done=yes\n");
  });

  test("a port and a lock still held at the deadline are named", () => {
    const d = freshDir("child-named");
    const r = bash(
      `holder h release="$D/release" lock="$D/lock" pids="$PIDS"
kill_gone KILL "$((EPOCHSECONDS + 2))" "$h" "port:$h_port" "lock:$D/lock"; echo "rc=$?"
echo "port=$h_port"
touch "$D/release"`,
      { D: d }
    );
    const v = values(r.stdout);
    expect(v.rc).toBe("1");
    expect(r.stderr).toMatch(
      new RegExp(`^port ${v.port} is still listened on \\(.*\\) at its deadline`, "m")
    );
    expect(r.stderr).toContain(`lock ${d}/lock is still held at its deadline`);
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
  // wait_gone in the background against PROC with DEADLINE (a bash word), CHANGE run 0.5 s in, and
  // what it returned and when. A case that expects the wait to end once CHANGE lands gives it END,
  // so a wait that missed the change runs to END and returns 1; a case that expects it to run to the
  // deadline gives it 3 s, the bound it checks.
  function waitAcross(proc: string, pid: number, change: string, deadline: string) {
    return bash(
      `wait_gone_proc=$PROC
kill() { return 0; }
deadline=${deadline}
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
      `printf '%s\\n' "$(sed 's/ 100 0 0$/ 200 0 0/' "$PROC/4242/stat")" >"$PROC/4242/stat.new" && mv "$PROC/4242/stat.new" "$PROC/4242/stat"`,
      '"$END"'
    );
    expect(r.stdout.trim().split(" ")[0]).toBe("rc=0");
  });

  test("a pid whose start time stays is the same process, still there at the deadline", () => {
    const proc = fakeProc({ 4242: { start: 100 } });
    const r = waitAcross(proc, 4242, ":", '"$((EPOCHSECONDS + 3))"');
    expect(r.stdout.trim().split(" ")[0]).toBe("rc=1");
  });

  test("a zombie whose task directory lists another thread is not gone until that thread is", () => {
    const proc = fakeProc({ 4244: { state: "Z", tasks: [4244, 4245] } });
    const kept = waitAcross(proc, 4244, ":", '"$((EPOCHSECONDS + 3))"');
    expect(kept.stdout.trim().split(" ")[0]).toBe("rc=1");
    const r = waitAcross(proc, 4244, `rmdir "$PROC/4244/task/4245"`, '"$END"');
    expect(r.stdout.trim().split(" ")[0]).toBe("rc=0");
  });

  test("a zombie with no thread but its main one is gone at once", () => {
    const proc = fakeProc({ 4246: { state: "Z" } });
    const r = waitAcross(proc, 4246, ":", '"$END"');
    expect(r.stdout.trim().split(" ")[0]).toBe("rc=0");
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
    expect(r.stderr).toContain(
      `pid 4243: ${proc}/4243/stat could not be read, so it is not known to be gone at its deadline`
    );
    rmSync(join(proc, "4243"), { recursive: true });
  });
});

describe("stop_gone: one deadline for the SIGTERM's wait and the SIGKILL's", () => {
  test("a process that ends on SIGTERM is gone, given no SIGKILL", () => {
    const r = bash(`holder h term=slow:0.5
stop_gone 10 "$END" "$h"; echo "rc=$?"
wait "$h"; echo "status=$?"`);
    expect(values(r.stdout)).toMatchObject({ rc: "0", status: "0" });
  });

  test("a process that ignores SIGTERM gets its SIGKILL at the grace and is gone", () => {
    const r = bash(`holder h term=ignore
start=$(now_ms)
stop_gone 1 "$END" "$h"; echo "rc=$?"
echo "ms=$(($(now_ms) - start))"
wait "$h"; echo "status=$?"`);
    const v = values(r.stdout);
    expect([v.rc, v.status]).toEqual(["0", String(128 + 9)]);
    expect(Number(v.ms)).toBeLessThan(15_000);
  });

  test("the grace is GRACE seconds from the call, whatever fraction of a second the call starts at", () => {
    // The stop starts in the last tenth of a second, where a grace counted from EPOCHSECONDS, a
    // whole second, would end almost at once and SIGKILL a stand-in that needs half of it.
    const r = bash(`holder h term=slow:0.5
while [ "\${EPOCHREALTIME#*.}" -lt 900000 ]; do sleep 0.01; done
stop_gone 1 "$END" "$h"; echo "rc=$?"
wait "$h"; echo "status=$?"`);
    expect(values(r.stdout)).toMatchObject({ rc: "0", status: "0" });
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
    expect(Number(v.ended_ms)).toBeLessThan(Number(v.deadline) * 1000 + 4000);
    expect(r.stderr).toMatch(
      new RegExp(
        `^${standIn(v.pid)} is still in the process table at its deadline, [0-9.]+ s after its SIGKILL$`,
        "m"
      )
    );
  });
});
