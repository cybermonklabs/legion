import { afterAll, beforeAll, beforeEach, setDefaultTimeout } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// The harness the tests of lib/wait-gone.sh share: wait-gone.test.ts, the waits themselves, and
// wait-gone-scripts.test.ts, each stop of the scripts that run them and the step after it that
// needs the process gone. This one file holds the stand-in (holder.py), the shell every harness runs
// above its body and the suite's last clean-up, so both files run the same fixtures.
export const lib = import.meta.dir;
export const e2e = join(lib, "..");
export const root = join(e2e, "..", "..");
// Each case's own timeout, the one fixed time the harness sets, and what every wait a case needs an
// answer from runs to: END (set in bash below) is that timeout counted from the case's start, less
// what the case does after the wait, so that a wait that never ends fails the case with its own
// line before bun stops it. A bound that is itself what a case checks (a 2 s deadline the wait must
// run to, a grace of 1 s) keeps its own value, and a mutant of wait-gone.sh shows the case fails
// when the bound is not kept. It is set before temporaryDirectory registers its hooks: bun takes a
// hook's timeout when the hook is registered, and the directory's removal, a few hundred files
// and the fake /proc trees, took more than bun's 5 s default on a loaded devbox.
const caseTimeoutMs = 300_000;

// The stand-in, holder.py, written into each file's temporary directory before its first test.
const holder = `import ctypes, fcntl, os, random, signal, socket, sys, threading, time

# holder.py KEY=VALUE...: a stand-in for a service a script stops. It listens on a loopback port of
# its own choosing (below) and writes the number to port=FILE once all of the below is set up.
#   term=default|ignore|slow:SECONDS  what SIGTERM does: end it, nothing, or end it SECONDS later
#   write=DIR   with term=slow, it creates a file in DIR every 10 ms until it ends, as a server's
#               drain writes (a few hundred files, never so many that removing them takes minutes)
#   log=FILE    it appends "serving" to FILE, and with term=slow "shutdown-complete" as it ends
#   lock=FILE   it holds an flock on FILE
#   child=SECONDS  it forks a child, once the port and the lock are set up, that holds both (the
#               same socket and the same open lock file) until SECONDS after this process is gone
#   release=FILE   with child=, the child holds on until FILE is there instead
#   pids=FILE   the child appends its pid to FILE, for the suite's last cleanup
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
            time.sleep(0.01)
        if "log" in args:
            with open(args["log"], "a") as log:
                log.write("shutdown-complete\\n")
        os._exit(0)

    signal.signal(signal.SIGTERM, on_term)
# The port: one picked at random below the kernel's ephemeral range and taken by binding it, so a
# port another program holds is never used. Both bounds are read at run time: the range from
# ip_local_port_range, the lowest port this user may bind from ip_unprivileged_port_start. A port
# the kernel picks for a bind of port 0 is one of the range, and once the stand-in frees it the
# kernel hands it to the next program on the machine that binds port 0, so a case that reads the
# port free after the stop (ports_free) can read that program's listener: Stage 4b's cleanup case
# did, once in 20 runs beside the box's load. No bind of port 0 is handed a port below the range;
# only a program that names this very port can take it.
low = int(open("/proc/sys/net/ipv4/ip_local_port_range").read().split()[0])
# Port 0 is no port: a bind of it is the kernel's pick from the range.
first = max(1, int(open("/proc/sys/net/ipv4/ip_unprivileged_port_start").read()))
if low <= first:
    sys.exit("holder.py: the ephemeral range starts at %d, so no port from %d below it is free to pick" % (low, first))
listener = None
for _ in range(1000):
    candidate = socket.socket()
    candidate.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        candidate.bind(("127.0.0.1", random.randrange(first, low)))
    except OSError:
        candidate.close()
        continue
    listener = candidate
    break
if listener is None:
    sys.exit("holder.py: no free port from %d to %d in 1000 tries" % (first, low - 1))
listener.listen()
if "lock" in args:
    lock = open(args["lock"], "a")
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
if "child" in args or "release" in args:
    parent = os.getpid()
    if os.fork() == 0:
        if "pids" in args:
            with open(args["pids"], "a") as pids:
                stat = open("/proc/self/stat").read()
                pids.write("%d %s\\n" % (os.getpid(), stat[stat.rindex(")") + 2:].split()[19]))
        while os.getppid() == parent:
            time.sleep(0.01)
        if "release" in args:
            while not os.path.exists(args["release"]):
                time.sleep(0.01)
        else:
            time.sleep(float(args["child"]))
        os._exit(0)
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

// The shell every harness runs above its body. Paths come in through the environment, so no
// harness's own command line names a run's work directory. Its EXIT trap ends what it started; a
// TERM (bun's, on a timeout) runs that trap too. Each stand-in's start is recorded in started_at,
// as a rig's start_process records each service (lib/process-tree.sh's record_start), so the
// rig's stop_pid, which signals a pid only while it is that process, stops it.
const prelude = `set -u
. "$ROOT/scripts/e2e/lib/wait-gone.sh"
declare -A started_at=()
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
  local start=''
  process_start "$!" start || true
  echo "$! $start" >>"$PIDS"
  [ -z "$start" ] || started_at[$!]=$start
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
# own_proc_root PID...: a proc root of these processes alone, a link to /proc/PID for each
# (lib/proc-root.ts's form), so a sweep's walk (procs_naming, through proc_root) reads them and not
# every process of the host.
own_proc_root() {
  local r p
  r=$(mktemp -d "$DIR/proc.XXXXXX")
  for p in "$@"; do ln -s "/proc/$p" "$r/$p"; done
  printf '%s' "$r"
}
`;

// The lines KEY=VALUE a harness prints, as a map.
export function values(out: string) {
  return Object.fromEntries(
    out
      .split("\n")
      .filter((line) => /^[a-z_]+=/.test(line))
      .map((line) => [line.slice(0, line.indexOf("=")), line.slice(line.indexOf("=") + 1)])
  );
}

/**
 * waitGoneHarness NAME is one test file's harness, called once at the file's top: it sets the file's
 * case timeout, makes its temporary directory NAME and registers the file's hooks (each case's start,
 * the stand-in written once the directory exists, and the last clean-up of every stand-in a harness
 * could not end), and gives the file `bash`, which runs a body under the prelude, and the helpers
 * around it.
 */
export function waitGoneHarness(name: string) {
  setDefaultTimeout(caseTimeoutMs);
  const dir = temporaryDirectory(name);
  const holderPath = join(dir, "holder.py");
  const pidsFile = join(dir, "started.pids");
  let caseStart = Date.now();
  beforeEach(() => {
    caseStart = Date.now();
  });
  // The stand-in, written once the suite's directory exists (temporaryDirectory makes it before the
  // first test).
  beforeAll(() => writeFileSync(holderPath, holder));
  // Every stand-in a harness starts is recorded in started.pids with its start time (stat field 22);
  // one a harness could not end is ended here, by that pid, while the pid still has that start time,
  // so a pid the kernel gave to another process since is never signalled. Only /proc/PID/stat is
  // read, which waits on nothing of the process it describes.
  afterAll(() => {
    if (!existsSync(pidsFile)) return;
    for (const line of readFileSync(pidsFile, "utf8").split("\n")) {
      const [pid, start] = line.split(" ").map(Number);
      if (!pid || !start) continue;
      try {
        const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
        if (Number(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19]) !== start) continue;
        process.kill(pid, "SIGKILL");
      } catch {}
    }
  });

  function bash(body: string, env: Record<string, string> = {}) {
    const ran = Bun.spawnSync(["bash", "-c", `${prelude}\n${body}`], {
      env: {
        PATH: process.env.PATH ?? "",
        ROOT: root,
        DIR: dir,
        HOLDER: holderPath,
        PIDS: pidsFile,
        // END: the epoch second at which the case's own timeout leaves 60 s for what follows the
        // wait (its later reads, the harness's exit, bun's own bookkeeping).
        END: String(Math.floor((caseStart + caseTimeoutMs - 60_000) / 1000)),
        ...env,
      },
      stdout: "pipe",
      stderr: "pipe",
      // Stopped 30 s before the case's own timeout, so the case reports what it read.
      timeout: Math.max(1_000, caseStart + caseTimeoutMs - 30_000 - Date.now()),
    });
    return { code: ran.exitCode, stdout: ran.stdout.toString(), stderr: ran.stderr.toString() };
  }

  // What kill_gone names a stand-in by when it could not wait it out: its pid and holder.py's own
  // path, whatever runs python3. A python3 that is a wrapper (a mise shim) execs /usr/bin/python3 by
  // its full path, so the interpreter's name on the command line is not `python3` there.
  const holderPattern = holderPath.replace(/[.*+?^$|()[\]{}\\]/g, "\\$&");
  function standIn(pid: string) {
    return `pid ${pid} \\([^)]*${holderPattern}[^)]*\\)`;
  }

  let dirs = 0;
  function freshDir(dirName: string) {
    const made = join(dir, `${dirName}-${++dirs}`);
    mkdirSync(made, { recursive: true });
    return made;
  }

  return { dir, bash, standIn, freshDir };
}
