import { describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";
import { e2e, root, values, waitGoneHarness } from "./wait-gone-harness";

// The stops of the scripts that run lib/wait-gone.sh's waits, each cut out of its script by name and
// followed by the step that needs the process gone, against the stand-ins of wait-gone-harness.ts:
// a port the next start binds, a lock the next run takes, a directory the next run removes, a file
// the cleanup prints and the table the next run reads. wait-gone.test.ts holds the waits themselves.
const { bash, standIn, freshDir } = waitGoneHarness("wait-gone-scripts-test");

// shellRead gives SCRIPT as bash itself reads it: the whole text parsed as the body of one function
// and printed back (`declare -f`), so a command split over continuation lines is one line, each
// command of a `;` list is a line of its own, quotes are bash's own, comments are gone and a function
// is printed `name ()` with its body below. A line search over this text meets each command once,
// however the script lays it out. A text bash cannot read is an error, never an empty reading.
function shellRead(script: string): string {
  const ran = Bun.spawnSync(
    ["bash", "-c", 'eval "__script() {\n$(cat)\n}" && declare -f __script'],
    { stdin: Buffer.from(script), stdout: "pipe", stderr: "pipe" }
  );
  const read = ran.stdout.toString();
  if (ran.exitCode !== 0 || !read.startsWith("__script")) {
    throw new Error(
      `bash could not read the script (exit ${ran.exitCode}): ${ran.stderr.toString()}`
    );
  }
  return read;
}

// The commands of Stage 4b's script, as bash reads it (shellRead), that stop or signal the daemon's
// pid, or clear it, other than through stop_daemon_ports, each as `LINE: COMMAND`, LINE counted in
// bash's reading. Every stop of the daemon goes through that one function, which waits for the
// daemon's ports too: a stop of the pid alone leaves its ports to whatever reads them next, the
// daemon's next start or the next run's ports_free on the fixed ports 13372 and 13373. A read of the
// pid (`kill -0`, ss's `pid=`) stops nothing. A clear of daemon_pid (`daemon_pid=`, `=''`, `=""` or
// `unset daemon_pid`) follows that stop (the command before it), or is the declaration before the
// daemon's first start: any other leaves cleanup's stop of the daemon nothing to stop.
function daemonStopsElsewhere(script: string): string[] {
  const signals =
    /\b(?:stop_pid|stop_gone|kill_gone|killed_gone|kill_held|stop_tree|kill_frozen|freeze_if|signal_recorded|signal_group|kill(?! -0\b))\b[^;&|\n]*\$\{?daemon_pid\b/;
  const clears =
    /(?:^|[;&|]\s*)(?:daemon_pid=(?:''|"")?\s*(?:$|;)|unset\s+(?:-v\s+)?daemon_pid\s*(?:$|;))/;
  const found: string[] = [];
  // The indent of stop_daemon_ports's `name ()` line while bash's reading is inside its body.
  let inside: string | undefined;
  let started = false;
  let previous = "";
  shellRead(script)
    .split("\n")
    .forEach((line, i) => {
      // bash prints a function defined inside another as `function name () ` and closes it `};`.
      const header = /^(\s*)(?:function\s+)?stop_daemon_ports \(\)\s*$/.exec(line);
      if (header) inside = header[1];
      const code = line.trim();
      const signalled = inside === undefined && signals.test(code);
      const cleared =
        started && clears.test(code) && !/\bstop_daemon_ports\b/.test(`${previous}\n${code}`);
      if (signalled || cleared) found.push(`${i + 1}: ${code}`);
      if (/\bdaemon_pid=\$!/.test(code)) started = true;
      if (inside !== undefined && /^\s*\};?\s*$/.test(line) && line.startsWith(`${inside}}`))
        inside = undefined;
      if (code) previous = code;
    });
  return found;
}

// The stops of the scripts, each cut out of its script by name, and the step after it that needs
// the process gone: the five kinds this change waits for (a port, a removed directory, a file the
// process writes, the process table, a lock).
describe("each stop of the scripts, then the step that needs the process gone", () => {
  const stage4b = scriptFunctions(join(e2e, "stage4b-sandbox-tree.sh"));
  const verifiers = scriptFunctions(join(e2e, "verifiers-staging-token.sh"));
  const stage1 = scriptFunctions(join(e2e, "stage1-skeleton.sh"));
  const rig = `root=$ROOT work=$WORK evidence=$WORK timeout_hook=
note() { :; }
fail() { echo "FAIL $*"; exit 1; }
. "$ROOT/scripts/e2e/lib/rig.sh"`;

  test("port: lib/process-tree.sh's stop_pid, given the port as Stage 4b's restarts give it, of a daemon that ignores SIGTERM, then Stage 4b's ports_free", () => {
    const work = freshDir("port");
    const r = bash(
      `${rig}
${stage4b("ports_free")}
stop_pid_grace=1
for t in 1 2 3; do
  holder d term=ignore threads=8 mib=1024
  stop_pid "$d" "$END" "port:$d_port" || echo "trial $t: stop_pid returned 1"
  ports_free "$d_port"
done
echo free=yes`,
      { WORK: work }
    );
    expect(r.stdout).toBe("free=yes\n");
  });

  test("port: lib/process-tree.sh's stop_pid of an empty PID signals nothing and still waits for its TARGETs, to the deadline, naming one still held", () => {
    const r = bash(
      `. "$ROOT/scripts/e2e/lib/process-tree.sh"
holder h term=ignore
start=$(now_ms)
deadline=$((EPOCHSECONDS + 2))
stop_pid "" "$deadline" "port:$h_port"; echo "rc=$?"
echo "ended_ms=$(now_ms)"; echo "deadline=$deadline"; echo "port=$h_port"
listening "$h_port" && echo listening=yes`
    );
    const v = values(r.stdout);
    expect(v.rc).toBe("1");
    expect(Number(v.ended_ms)).toBeGreaterThanOrEqual(Number(v.deadline) * 1000);
    expect(v.listening).toBe("yes");
    // It names the wait as one that sent no signal, and times it from the call, which it waited
    // out to the deadline, not from the deadline itself.
    expect(r.stderr).toMatch(
      new RegExp(
        `^port ${v.port} is still listened on \\(.*\\) at its deadline, [0-9]+\\.[0-9] s after its SIGKILL, sent to no process \\(no target was one\\)$`,
        "m"
      )
    );
    expect(Number(/at its deadline, ([0-9]+\.[0-9]) s after/.exec(r.stderr)?.[1])).toBeGreaterThan(
      0
    );
  });

  test("port: Stage 4b's restart_daemon waits for the ports a child of the daemon still holds before it starts the new one", () => {
    const work = freshDir("restart-child");
    const r = bash(
      `${rig}
${stage4b("ports_free")}
${stage4b("stop_daemon_ports")}
${stage4b("restart_stop")}
${stage4b("restart_daemon")}
write_legion_config() { :; }
start_daemon() { ports_free "$port_daemon" "$port_worker_stream" && echo started=yes; }
stop_pid_grace=1
holder daemon_pid child=1 pids="$PIDS"
port_daemon=$daemon_pid_port port_worker_stream=$daemon_pid_port
restart_daemon`,
      { WORK: work }
    );
    expect(r.stdout).toBe("started=yes\n");
  });

  test("port: Stage 4b's cleanup stops its daemon with stop_daemon_ports, which waits for the ports a child of the daemon still holds, then the next run's ports_free", () => {
    // The next run's prerequisites read the fixed ports 13372 and 13373 (ports_free), so the
    // teardown waits for the ports themselves, not the daemon's pid alone.
    expect(stage4b("cleanup")).toMatch(/^\s*\[ -z "\$daemon_pid" \] \|\| stop_daemon_ports$/m);
    const work = freshDir("cleanup-child");
    const r = bash(
      `${rig}
${stage4b("ports_free")}
${stage4b("stop_daemon_ports")}
stop_pid_grace=1
for t in 1 2 3; do
  holder daemon_pid child=1 pids="$PIDS"
  port_daemon=$daemon_pid_port port_worker_stream=$daemon_pid_port
  stop_daemon_ports || echo "trial $t: stop_daemon_ports returned 1"
  ports_free "$port_daemon" "$port_worker_stream"
done
echo free=yes`,
      { WORK: work }
    );
    expect(r.stdout).toBe("free=yes\n");
  });

  test("port: every stop of Stage 4b's daemon goes through stop_daemon_ports, and no other line signals the daemon's pid or clears it", () => {
    // restart_stop (restart_daemon's, and each check's own restart in the body of the script), the
    // hygiene check's stop and cleanup's: daemonStopsElsewhere reads every command of the script as
    // bash reads it, so a stop written inline at a call site is read as a function's is, and one
    // split over continuation lines as one command.
    const script = readFileSync(join(e2e, "stage4b-sandbox-tree.sh"), "utf8");
    expect(daemonStopsElsewhere(script)).toEqual([]);
    // The search finds what it is there for: an inline restart that stops the pid alone, and a
    // hygiene stop that signals it and then clears it.
    const hygiene =
      /^stop_daemon_ports \|\| fail "the daemon \(pid \$daemon_pid\) did not stop or its ports are still held \(named above\); the teardown would race it"$/m;
    expect(script).toMatch(/^restart_stop$/m);
    expect(script).toMatch(hygiene);
    const planted = script
      .replace(
        /^restart_stop$/m,
        'stop_pid "$daemon_pid" || fail "the daemon did not stop"; daemon_pid='
      )
      .replace(hygiene, 'kill -TERM "$daemon_pid"');
    expect(daemonStopsElsewhere(planted)).toEqual([
      expect.stringMatching(
        /^[0-9]+: stop_pid "\$daemon_pid" \|\| fail "the daemon did not stop";$/
      ),
      expect.stringMatching(/^[0-9]+: daemon_pid=;?$/),
      expect.stringMatching(/^[0-9]+: kill -TERM "\$daemon_pid";$/),
      expect.stringMatching(/^[0-9]+: daemon_pid=;?$/),
    ]);
    // A stop whose pid sits on a continuation line, and each way to clear the pid with no stop
    // before it, a form the script does not use today, are found too.
    const continued = script.replace(
      hygiene,
      'kill_gone TERM "$((EPOCHSECONDS + 10))" \\\n  "$daemon_pid" || true'
    );
    expect(daemonStopsElsewhere(continued)).toEqual(
      expect.arrayContaining([
        expect.stringMatching(
          /^[0-9]+: kill_gone TERM "\$\(\(EPOCHSECONDS \+ 10\)\)" "\$daemon_pid" \|\| true;?$/
        ),
      ])
    );
    for (const clear of ["daemon_pid=''", 'daemon_pid=""', "unset daemon_pid"]) {
      const quiet = script.replace(/^restart_stop$/m, clear);
      expect(daemonStopsElsewhere(quiet)).toEqual(
        expect.arrayContaining([expect.stringMatching(new RegExp(`^[0-9]+: ${clear};?$`))])
      );
    }
  });

  test("process table: lib/process-tree.sh's stop_pid, which Stage 2's cleanup runs, of a daemon that drains for a second on SIGTERM, then the next run's refuse_leftovers reads the table", () => {
    const work = freshDir("table-stage2");
    const r = bash(
      `. "$ROOT/scripts/e2e/lib/process-tree.sh"
. "$ROOT/scripts/e2e/lib/leftovers.sh"
fail() { echo "FAIL $*"; exit 1; }
for t in 1 2 3; do
  holder d term=slow:1 threads=8 mib=256
  stop_pid "$d" "$END" || echo "trial $t: stop_pid returned 1"
  leftover_started "$d" >/dev/null && echo "trial $t: refuse_leftovers would read pid $d as running"
done
echo gone=yes`,
      { WORK: work }
    );
    expect(r.stdout).toBe("gone=yes\n");
  });

  // A box-login run as a stage proof makes one: the run marks itself (LEGION_E2E_RUN), takes the
  // lock, and starts its panes; its teardown is stop_box_login_run, and the next run's first step is
  // take_box_login_lock. The sweep reads a proc root of the run's own pane (own_proc_root), not the
  // host's every process.
  const boxLogin = `. "$ROOT/scripts/e2e/lib/box-login.sh"
box_login_stop_grace=1
# run_as WORK: this shell is the box-login run under WORK, its lock at WORK/lock.
run_as() {
  mkdir -p "$1"
  export LEGION_E2E_RUN=$1
  box_login_lock_file=$1/lock
}
# next_run_takes_lock LOCKFILE: a fresh shell, as the next run is, takes the lock and lets it go.
next_run_takes_lock() {
  box_login_lock_file=$1 bash -c '. "$ROOT/scripts/e2e/lib/box-login.sh"; take_box_login_lock' 2>/dev/null
}`;

  test("lock: a run's stop_box_login_run of a pane whose child holds the inherited lock, then the next run's take_box_login_lock", () => {
    // Every pane the run starts, and every process a pane starts, inherits the locked descriptor
    // here. The run ends after its teardown (the subshell below), and the next run takes the lock.
    const d = freshDir("lock");
    const r = bash(
      `${boxLogin}
for t in 1 2 3; do
  (
    run_as "$D/run.$t"
    take_box_login_lock || exit 1
    holder pane term=ignore threads=8 mib=256 child=1 pids="$PIDS"
    proc_root=$(own_proc_root "$pane")
    stop_box_login_run "$LEGION_E2E_RUN" || echo "trial $t: the stop returned 1"
  )
  next_run_takes_lock "$D/run.$t/lock" || echo "trial $t: refused"
done
echo took=yes`,
      { D: d }
    );
    expect(r.stdout).toBe("took=yes\n");
  });

  test("lock: a second box-login run is refused until the first run's last pane is gone, though the pane holds no copy of the run's descriptor, as a tmux pane holds none", () => {
    // A tmux pane holds no copy of the run's lock descriptor (the tmux server does), so the lock
    // alone cannot tell that the panes are gone: the teardown must give up the lock only once they
    // are. The pane drains for 4 s after its SIGTERM, inside a 5 s grace; a second run tries the lock every 0.1 s from
    // the moment the teardown starts, and an observer writes the moment the pane is gone. Neither
    // holds the run's descriptor either: each is started with it closed in it alone (a
    // {varname}>&- on the call itself would close it in this shell for good). The proc root holds
    // this shell too, so the sweep's walk still lists a process once the pane is gone.
    const d = freshDir("lock-order");
    const r = bash(
      `${boxLogin}
box_login_stop_grace=5
run_as "$D/run"
take_box_login_lock || exit 1
eval "holder pane term=slow:4 $box_login_lock_fd>&-"
proc_root=$(own_proc_root "$pane" "$$")
( exec {box_login_lock_fd}>&-
  while { read -r st <"/proc/$pane/stat"; } 2>/dev/null && [[ \${st##*) } != Z* ]]; do :; done
  echo "$(now_ms)" >"$D/pane-gone" ) &
observer=$!
( exec {box_login_lock_fd}>&-
  for _ in $(seq 1 300); do
    if next_run_takes_lock "$D/run/lock"; then echo "$(now_ms)" >"$D/took"; exit 0; fi
    sleep 0.1
  done ) &
second=$!
stop_box_login_run "$D/run"; echo "rc=$?"
wait "$observer" "$second"
echo "gone_ms=$(<"$D/pane-gone")"
echo "took_ms=$(cat "$D/took" 2>/dev/null)"`,
      { D: d }
    );
    const v = values(r.stdout);
    expect(v.rc).toBe("0");
    expect(v.took_ms).not.toBe("");
    expect(Number(v.took_ms)).toBeGreaterThanOrEqual(Number(v.gone_ms));
  });

  test("lock: a pane SIGKILL has not ended is waited for to the deadline and named, and the run keeps its lock and its login link: a second run is refused while the pane runs", () => {
    // The pane holds no copy of the run's descriptor, as a tmux pane holds none, and SIGKILL does
    // not end it (drop_kill), so the sweep fails with it still there; a run that gave up its lock
    // then would let a second run start beside the pane. Each trial is one run, a subshell: the
    // second runs its transcript as the stages do (transcript_to after the lock, then the mark),
    // whose tee holds none of the run's descriptors (lib/transcript.sh), so in both the run's own
    // descriptor is what keeps the second run out.
    const d = freshDir("lock-kept");
    const r = bash(
      `${boxLogin}
. "$ROOT/scripts/e2e/lib/transcript.sh"
for t in plain transcript; do
  (
    mkdir -p "$D/$t"
    box_login_lock_file=$D/$t/lock
    take_box_login_lock || exit 1
    [ "$t" = plain ] || transcript_to "$D/$t/transcript.log"
    export LEGION_E2E_RUN=$D/$t
    eval "holder pane term=ignore $box_login_lock_fd>&-"
    echo "$pane" >"$D/$t.pid"
    proc_root=$(own_proc_root "$pane")
    drop_kill "$pane"
    # The profile's link to the operator's login, as lib/install-box-login-route.sh makes it: the
    # pane still there could open the database again through it, so the run keeps it.
    mkdir -p "$D/$t/agent"; : >"$D/login.db"; ln -s "$D/login.db" "$D/$t/agent/agent.db"
    start=$(now_ms)
    stop_box_login_run "$D/$t" "$D/$t/agent" 2>"$D/$t.err"; echo "$?" >"$D/$t.rc"
    echo "$(($(now_ms) - start))" >"$D/$t.ms"
    if next_run_takes_lock "$D/$t/lock"; then echo took; else echo refused; fi >"$D/$t.next"
    if [ -L "$D/$t/agent/agent.db" ]; then echo kept; else echo gone; fi >"$D/$t.link"
    builtin kill -0 "$pane" 2>/dev/null && echo running >"$D/$t.pane"
    builtin kill -KILL "$pane"
  )
  echo "\${t}_rc=$(<"$D/$t.rc")"; echo "\${t}_ms=$(<"$D/$t.ms")"; echo "\${t}_pid=$(<"$D/$t.pid")"
  echo "\${t}_next=$(<"$D/$t.next")"; echo "\${t}_pane=$(cat "$D/$t.pane" 2>/dev/null)"; echo "\${t}_link=$(<"$D/$t.link")"
done`,
      { D: d }
    );
    const v = values(r.stdout);
    for (const t of ["plain", "transcript"]) {
      const said = readFileSync(join(d, `${t}.err`), "utf8");
      expect([t, v[`${t}_rc`], v[`${t}_next`], v[`${t}_pane`], v[`${t}_link`]]).toEqual([
        t,
        "1",
        "refused",
        "running",
        "kept",
      ]);
      expect(Number(v[`${t}_ms`])).toBeGreaterThanOrEqual(10_000);
      expect(said).toMatch(
        new RegExp(`^${standIn(v[`${t}_pid`])} is still in the process table at its deadline`, "m")
      );
      // One line names what the run could not stop, and that it keeps the lock and the link.
      expect(said.split("\n").filter((line) => line.startsWith("stop_box_login_run:"))).toEqual([
        expect.stringMatching(
          new RegExp(
            `^stop_box_login_run: the run keeps its lock ${d}/${t}/lock until this shell exits, and its link ${d}/${t}/agent/agent.db to the operator's login, since it could not stop ${standIn(v[`${t}_pid`])}$`
          )
        ),
      ]);
    }
  });

  test("lock: a process that inherited the run's descriptor and carries no mark of the run, so no sweep ends it, is waited for to the lock's deadline and named, never left holding the lock unsaid", () => {
    // It is started with the run's mark emptied in its environment, as a process the run started
    // before it marked itself is; the sweep reads it and finds no process of the run.
    const d = freshDir("lock-stays");
    const r = bash(
      `${boxLogin}
run_as "$D/run"
take_box_login_lock || exit 1
LEGION_E2E_RUN= holder stray term=ignore
proc_root=$(own_proc_root "$stray")
start=$(now_ms)
stop_box_login_run "$D/run"; echo "rc=$?"
echo "ms=$(($(now_ms) - start))"
next_run_takes_lock "$D/run/lock" && echo next=took || echo next=refused`,
      { D: d }
    );
    const v = values(r.stdout);
    expect([v.rc, v.next]).toEqual(["1", "refused"]);
    // The lock's deadline is 10 s from the close in whole seconds ($((EPOCHSECONDS + 10))).
    expect(Number(v.ms)).toBeGreaterThanOrEqual(9_000);
    expect(r.stderr).toContain(`lock ${d}/run/lock is still held at its deadline`);
    expect(r.stderr).not.toContain("stop_box_login_run: the run keeps its lock");
  });

  test("lock: a run whose transcript runs, as every stage's does, gives up its lock and its login link at its teardown, and the next run takes it while the run's transcript still writes", () => {
    // The stages take the lock, start the transcript (transcript_to, lib/transcript.sh) and then
    // mark the run. The tee carries no mark and outlives every sweep by design, so it must hold no
    // copy of the run's descriptor: one it held would keep the lock until the driver's last line,
    // and the teardown's wait for the lock to be free would fail on every run, at its deadline.
    const d = freshDir("lock-transcript");
    const r = bash(
      `${boxLogin}
. "$ROOT/scripts/e2e/lib/transcript.sh"
(
  mkdir -p "$D/run"
  box_login_lock_file=$D/run/lock
  take_box_login_lock || exit 1
  transcript_to "$D/run/transcript.log"
  export LEGION_E2E_RUN=$D/run
  holder pane term=default
  proc_root=$(own_proc_root "$pane")
  mkdir -p "$D/run/agent"; : >"$D/login.db"; ln -s "$D/login.db" "$D/run/agent/agent.db"
  start=$(now_ms)
  stop_box_login_run "$D/run" "$D/run/agent" 2>"$D/err"; echo "$?" >"$D/rc"
  echo "$(($(now_ms) - start))" >"$D/ms"
  if next_run_takes_lock "$D/run/lock"; then echo took; else echo refused; fi >"$D/next"
  if [ -L "$D/run/agent/agent.db" ]; then echo kept; else echo gone; fi >"$D/link"
  echo "the run's last line"
)
for _ in $(seq 1 600); do grep -q "the run's last line" "$D/run/transcript.log" && break; sleep 0.05; done
echo "rc=$(<"$D/rc")"; echo "ms=$(<"$D/ms")"; echo "next=$(<"$D/next")"; echo "link=$(<"$D/link")"
[ -e "$D/login.db" ] && echo login=kept
grep -q "the run's last line" "$D/run/transcript.log" && echo transcript=kept`,
      { D: d }
    );
    const v = values(r.stdout);
    expect([v.rc, v.next, v.transcript, v.link, v.login]).toEqual([
      "0",
      "took",
      "kept",
      "gone",
      "kept",
    ]);
    expect(Number(v.ms)).toBeLessThan(10_000);
  });

  test("directory: the verifiers proof's cleanup of a Dispatch that drains into $work for 2 s, then the next run's rm -rf", () => {
    const work = join(freshDir("verifiers"), "verifiers-e2e");
    mkdirSync(join(work, "home", "data"), { recursive: true });
    for (let i = 0; i < 100; i++) writeFileSync(join(work, "home", "data", `record.${i}`), "");
    // Once the cleanup returns nothing may write into $work: the directory's entries, counted twice
    // half a second apart, are the same, and the next run's rm -rf removes it. A cleanup that does
    // not wait leaves the stand-in writing a file every 10 ms for 2 s.
    const r = bash(
      `docker() { :; }
pg_name=none nats_name=none listener_pid=
${verifiers("cleanup")}
holder dispatch_pid term=slow:2 write="$WORK/home/data"
cleanup
entries() { local e=("$WORK/home/data"/*); echo "\${#e[@]}"; }
echo "before=$(entries)"
sleep 0.5
echo "after=$(entries)"
rm -rf "$WORK" && echo removed=yes`,
      { WORK: work }
    );
    const v = values(r.stdout);
    expect(Number(v.before)).toBeGreaterThanOrEqual(100);
    expect(v.after).toBe(v.before);
    expect(v.removed).toBe("yes");
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

  test("process table: the temporal evaluation's stop_dev_server of a server SIGKILL has not ended returns at its deadline, naming it, and never waits on it unbounded", () => {
    // run.sh reaps the dev server, its shell's child, only once stop_gone has seen it gone: a bare
    // `wait` on one still there would block past every bound, and the experiments after it with
    // it. The stop runs in a subshell that the case waits on for 60 s, so a stop that blocks is
    // read as hung rather than holding the case to its own timeout.
    const temporal = scriptFunctions(
      join(root, "docs", "research", "temporal-evaluation-2026-11", "run.sh")
    );
    const d = freshDir("temporal");
    const r = bash(
      `${temporal("stop_dev_server")}
(
  holder server_pid term=ignore
  echo "$server_pid" >"$D/pid"
  drop_kill "$server_pid"
  start=$(now_ms)
  stop_dev_server; echo "$?" >"$D/rc"
  echo "$(($(now_ms) - start))" >"$D/ms"
) 2>"$D/err" & runner=$!
wait_gone "$((EPOCHSECONDS + 60))" "$runner" || echo hung=yes
builtin kill -KILL "$runner" 2>/dev/null
builtin kill -KILL "$(<"$D/pid")" 2>/dev/null
echo "rc=$(cat "$D/rc" 2>/dev/null)"; echo "ms=$(cat "$D/ms" 2>/dev/null)"; echo "pid=$(<"$D/pid")"`,
      { D: d }
    );
    const v = values(r.stdout);
    expect([v.hung, v.rc]).toEqual([undefined, "1"]);
    // stop_gone's deadline is 20 s from the call, in whole seconds.
    expect(Number(v.ms)).toBeGreaterThanOrEqual(19_000);
    expect(readFileSync(join(d, "err"), "utf8")).toMatch(
      new RegExp(`^${standIn(v.pid)} is still in the process table at its deadline`, "m")
    );
  });

  test("process table: the temporal evaluation's stop_sessions ends a recorded session and waits, and a stat that never answers ends its walk at the stop's bound, named", () => {
    // stop_sessions reads every process's stat for the sessions run_experiment recorded
    // (read_until, lib/wait-gone.sh), held to the stop's one bound. A /proc entry that never
    // answers its reader, a FIFO no writer opens beside the session's own entry here, ends the walk
    // at that bound with nothing signalled, and the stop says so; over the host's /proc the same
    // stop ends the session and waits for it.
    const temporal = scriptFunctions(
      join(root, "docs", "research", "temporal-evaluation-2026-11", "run.sh")
    );
    const d = freshDir("sessions");
    const r = bash(
      `${temporal("stop_sessions")}
setsid sleep 300 </dev/null >/dev/null 2>&1 & leader=$!; started+=("$leader")
until [ "$(awk '{print $6}' "/proc/$leader/stat" 2>/dev/null)" = "$leader" ]; do sleep 0.05; done
start=''; process_start "$leader" start
sessions=("$leader:$start")
stuck=$(own_proc_root "$leader"); mkdir "$stuck/999999999"; mkfifo "$stuck/999999999/stat"
began=$(now_ms)
wait_gone_proc=$stuck stop_sessions "$((EPOCHSECONDS + 3))" 2>"$D/err"; echo "stuck_rc=$?"
echo "stuck_ms=$(($(now_ms) - began))"
kill -0 "$leader" 2>/dev/null && echo stuck_left=yes
stop_sessions "$((EPOCHSECONDS + 20))"; echo "rc=$?"
echo "state=$(awk '{print $3}' "/proc/$leader/stat" 2>/dev/null)"`,
      { D: d }
    );
    const v = values(r.stdout);
    expect([v.stuck_rc, v.stuck_left, v.rc]).toEqual(["1", "yes", "0"]);
    expect(Number(v.stuck_ms)).toBeLessThan(10_000);
    expect(["", "Z"]).toContain(v.state ?? "");
    expect(readFileSync(join(d, "err"), "utf8")).toMatch(
      /^stop_sessions: the walk of every \S+\/<pid>\/stat had not ended at the stop's bound, [23] s after the stop began, so no session was stopped$/m
    );
  });

  test("process table: Stage 3's sweep of a run's process with threads and memory, then its read of the table and the next run's", () => {
    const work = freshDir("table");
    // The checkpoint's own lines, from its sweep to its read of the table. The run's processes carry
    // its mark, and the sweep reads a proc root of the stand-in alone; the next run's refuse_leftovers
    // reads the pid's own /proc entry (leftover_started), which still lists a process SIGKILL has
    // not finished ending.
    const script = readFileSync(join(e2e, "stage3-devbox-workflow.sh"), "utf8");
    const block =
      /^stop_marked_processes "\$work" \|\|[^\n]*\n[\s\S]*?a proof process remains[^\n]*\n/m.exec(
        script
      );
    expect(block).not.toBeNull();
    const r = bash(
      `${rig}
. "$ROOT/scripts/e2e/lib/leftovers.sh"
docker() { :; }
tmux() { return 1; }
pg_container=none nats_container=none ptoken=none
export LEGION_E2E_RUN=$WORK
for t in 1 2 3; do
  holder p term=ignore threads=8 mib=1024
  # This shell too, so the walk lists a process (an unmarked one) once the stand-in is gone.
  proc_root=$(own_proc_root "$p" "$$")
  ${block?.[0]}
  leftover_started "$p" >/dev/null && echo "trial $t: the next run's refuse_leftovers would read pid $p as running"
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
${stage4b("stop_daemon_ports")}
${stage4b("restart_stop")}
${stage4b("restart_daemon")}
stop_pid_grace=0
holder daemon_pid term=${stays ? "ignore" : "default"}
${stays ? 'drop_kill "$daemon_pid"' : ""}
port_daemon=$daemon_pid_port port_worker_stream=$daemon_pid_port
echo "pid=$daemon_pid"
restart_daemon`,
        { WORK: work }
      );
    const stays = run(true);
    const pid = values(stays.stdout).pid;
    expect(values(stays.stdout).started).toBeUndefined();
    expect(stays.stdout).toContain(
      `FAIL the daemon (pid ${pid}) did not stop or its ports are still held (named above); its restart would meet its ports`
    );
    expect(stays.stderr).toMatch(
      new RegExp(`^${standIn(pid)} is still in the process table at its deadline`, "m")
    );
    // The control: a daemon that leaves in time lets the restart run.
    expect(values(run(false).stdout).started).toBe("yes");
  });
});
