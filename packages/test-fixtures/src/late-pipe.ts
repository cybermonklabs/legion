import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

/** What a command wrote through a late pipe, and how it ended. */
export interface LatePipeRun {
  /** Every byte the reader got, from the start of the command to the end of the pipe. */
  readonly stdout: Uint8Array;
  /** The command's exit status; null when it was killed, at `timeoutMs` or by `killWhenFullMs`. */
  readonly exitCode: number | null;
  /** The pipe's size as the kernel set it, which a reader of the result can check. */
  readonly pipeBytes: number;
  /** Whether `killWhenFullMs` killed the command mid-write, its pipe full. */
  readonly killedWhenFull: boolean;
  /** When the reader began to read, in ms after it started the command, both read on the
   *  reader's one monotonic clock: no earlier than `lateMs` unless `killWhenFullMs` replaced it. */
  readonly firstReadMs: number;
}

export interface LatePipeOptions {
  /** The pipe's size: one page by default, the least Linux gives a pipe. */
  readonly pipeBytes?: number;
  /** How long after the command starts its reader reads anything: 2,000 ms by default. */
  readonly lateMs?: number;
  /**
   * Kill the command mid-write, as Codex kills a hook at its timeout and never reads what the hook
   * held: once the pipe holds `pipeBytes` (the command is waiting on its reader), the reader waits
   * this long, reading nothing, then kills the command with its process group (SIGKILL) and only
   * then reads what the pipe held. In place of `lateMs`. A command that ends before its pipe is
   * full is read as it ends, and `killedWhenFull` says which it was.
   */
  readonly killWhenFullMs?: number;
  readonly cwd?: string;
  readonly env?: Record<string, string | undefined>;
  /** The command's whole stdin; it gets none (/dev/null) without this. */
  readonly stdin?: string;
  /** How long the command may run, the reader's delay included: 30,000 ms by default. */
  readonly timeoutMs?: number;
}

/**
 * The reader a loaded machine gives a program: python3 makes an OS pipe of `pipeBytes`
 * (`F_SETPIPE_SZ`, before the command starts), starts `argv` with its stdout on that pipe, reads
 * nothing for `lateMs`, then reads to the end of the pipe. A program that writes more than the
 * pipe holds and then ends without waiting for the rest to drain (`process.exit` on Bun or node)
 * reaches this reader cut, with its exit status unchanged; one that waits reaches it whole.
 *
 * `Bun.spawn`'s `stdout: "pipe"` cannot show this: Bun hands its child a socket whose send buffer
 * holds 212,992 bytes, not a pipe, so a smaller output never waits there. So the pipe is made by
 * python3, which every host that runs these tests has (`skipWithout("python3")` where one may not).
 */
export async function readThroughLatePipe(
  argv: readonly string[],
  options: LatePipeOptions = {}
): Promise<LatePipeRun> {
  const directory = mkdtempSync(join(tmpdir(), "late-pipe-"));
  const out = join(directory, "stdout");
  try {
    const config = {
      argv,
      cwd: options.cwd ?? process.cwd(),
      env: Object.fromEntries(
        Object.entries(options.env ?? process.env).filter(
          (entry): entry is [string, string] => entry[1] !== undefined
        )
      ),
      late: (options.lateMs ?? 2_000) / 1_000,
      killWhenFull: options.killWhenFullMs === undefined ? null : options.killWhenFullMs / 1_000,
      out,
      pipe: options.pipeBytes ?? 4_096,
      stdin: options.stdin ?? null,
      timeout: (options.timeoutMs ?? 30_000) / 1_000,
    };
    const reader = Bun.spawn(["python3", "-c", READER, JSON.stringify(config)], {
      stdout: "pipe",
      stderr: "pipe",
    });
    const [report, errors, code] = await Promise.all([
      new Response(reader.stdout).text(),
      new Response(reader.stderr).text(),
      reader.exited,
    ]);
    if (code !== 0) throw new Error(`the late pipe's reader failed (exit ${code}): ${errors}`);
    const { exitCode, pipeBytes, killedWhenFull, firstReadMs } = JSON.parse(report) as {
      readonly exitCode: number | null;
      readonly pipeBytes: number;
      readonly killedWhenFull: boolean;
      readonly firstReadMs: number;
    };
    return {
      exitCode,
      pipeBytes,
      killedWhenFull,
      firstReadMs,
      stdout: new Uint8Array(readFileSync(out)),
    };
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
}

/** The reader: one JSON argument, the bytes it read to `out`, and its report on stdout. A command
 *  still running at `timeout` is killed with its process group, and reported with no exit code;
 *  with `killWhenFull`, so is one whose pipe has been full that long (FIONREAD: what it holds). */
const READER = `
import array, fcntl, json, os, signal, subprocess, sys, termios, threading, time
c = json.loads(sys.argv[1])
r, w = os.pipe()
fcntl.fcntl(w, 1031, c["pipe"])
size = fcntl.fcntl(w, 1032)
p = subprocess.Popen(c["argv"], cwd=c["cwd"], env=c["env"], stdout=w, start_new_session=True,
                     stdin=subprocess.DEVNULL if c["stdin"] is None else subprocess.PIPE)
os.close(w)
started = time.monotonic()
killed = []
def stop():
    killed.append(True)
    try:
        os.killpg(p.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
timer = threading.Timer(c["timeout"], stop)
timer.daemon = True
timer.start()
if c["stdin"] is not None:
    try:
        p.stdin.write(c["stdin"].encode())
        p.stdin.close()
    except BrokenPipeError:
        pass
full = []
if c["killWhenFull"] is None:
    time.sleep(c["late"])
else:
    held = array.array("i", [0])
    while p.poll() is None and not killed:
        fcntl.ioctl(r, termios.FIONREAD, held)
        if held[0] >= size:
            time.sleep(c["killWhenFull"])
            if p.poll() is None:
                full.append(True)
                stop()
            break
        time.sleep(0.01)
first_read = (time.monotonic() - started) * 1000
chunks = []
while True:
    chunk = os.read(r, 65536)
    if not chunk:
        break
    chunks.append(chunk)
os.close(r)
code = p.wait()
timer.cancel()
with open(c["out"], "wb") as f:
    f.write(b"".join(chunks))
print(json.dumps({"exitCode": None if killed else code, "pipeBytes": size, "killedWhenFull": bool(full),
                  "firstReadMs": first_read}))
`;
