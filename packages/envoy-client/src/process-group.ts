// The process group a command runs in, and how it ends: an envoy.json `!` command (`fileValue`,
// ./envoy-config), a person's `!command` answered through Dispatch (pi-envoy's typed-shell),
// AskGate's dry run (pi-envoy's askgate-core) and jj's attribution read (pi-shared's
// jj-attribution). Each starts its command detached, in a group of its own, so only a kill of the
// group reaches every process the command started. Both host plugins depend on this package, so
// the one copy lives here.
import type { Spawn, Subprocess } from "bun";

/** Kills every process of the group `pid` leads, if any is left. */
export function killGroup(pid: number): void {
  try {
    process.kill(-pid, "SIGKILL");
  } catch {
    // The group is already gone.
  }
}

/**
 * The shell script `groupWatchedBy` runs. It moves its stdin, the pipe this process holds, to fd 3,
 * starts the watch in the background (it reads fd 3 until the pipe's last writer is gone, then
 * kills the group whose leader is the command's own pid, `-$$`, itself included), and replaces
 * itself with the caller's argv (`"$0" "$@"`), which reads /dev/null and holds no end of the pipe.
 * A caller that did not spawn it detached leaves the command in the caller's own group, which
 * `-$$` does not name: the watch then kills nothing rather than the caller. The watch writes
 * nowhere, so it holds none of the command's output open.
 */
const WATCH =
  'exec 3<&0; (while read -r line <&3; do :; done; kill -s KILL -- -$$) </dev/null >/dev/null 2>&1 & exec "$0" "$@" </dev/null 3<&-';

/**
 * The argv that runs `argv` in a process group that ends with this process, however this process
 * ends: its own exit, a signal it handles by ending through `process.reallyExit` (as Oh My Pi's
 * SIGINT, SIGTERM, SIGHUP and fatal-error handlers do, which skips the `exit` event), or a SIGKILL
 * nothing can handle. The process need not run anything as it goes: the kernel closes the pipe it
 * held, and the watch in the group sees that.
 *
 * `spawnWatched` is how a caller starts one; this is its argv, and what the watch's own test
 * starts. `argv` replaces the watching shell under the same pid, so `exited`, the exit status and
 * `killGroup` are the command's own; `argv` reads /dev/null. A first word the shell cannot run
 * exits 127 rather than failing the spawn. Linux and macOS: `/bin/sh` and its `read` and `kill`
 * builtins.
 */
export function groupWatchedBy(argv: readonly string[]): string[] {
  return ["/bin/sh", "-c", WATCH, ...argv];
}

/** `Bun.spawn`'s options for a watched command: all but the two `spawnWatched` sets itself. */
export type WatchedSpawnOptions<Out extends Spawn.Readable, Err extends Spawn.Readable> = Omit<
  Spawn.SpawnOptions<"pipe", Out, Err>,
  "stdin" | "detached"
>;

/** A command `spawnWatched` started. */
export interface WatchedCommand<Out extends Spawn.Readable, Err extends Spawn.Readable> {
  /** The command's own process: its pid leads the group, and `exited`, the exit status and a
   *  `killGroup(child.pid)` are the command's. Its stdin is the watch's pipe, not the command's. */
  readonly child: Subprocess<"pipe", Out, Err>;
  /** Kills every process of the group still running, then closes the watch's pipe. A caller runs
   *  it on every path once it is done with the command; each call kills, and the pipe closes once. */
  readonly endGroup: () => void;
}

/**
 * Starts `argv` detached, in a process group of its own that ends with this process however this
 * process ends (`groupWatchedBy`), its stdin the pipe the group's watch reads. The spawn is
 * synchronous, as `Bun.spawn` is, and throws what it throws.
 */
export function spawnWatched<const Out extends Spawn.Readable, const Err extends Spawn.Readable>(
  argv: readonly string[],
  options: WatchedSpawnOptions<Out, Err>
): WatchedCommand<Out, Err> {
  const child = Bun.spawn<"pipe", Out, Err>(groupWatchedBy(argv), {
    ...options,
    stdin: "pipe",
    detached: true,
  });
  let closed = false;
  return {
    child,
    endGroup: () => {
      killGroup(child.pid);
      if (closed) return;
      closed = true;
      void child.stdin.end();
    },
  };
}
