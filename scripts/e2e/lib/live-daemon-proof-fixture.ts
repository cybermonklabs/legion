import { chmodSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

/**
 * Where a stand-in sends its signal: `build` is the driver's build, inside the run's `driver`
 * check, before the proof; `proof` is the driver itself, as it starts, inside the `proof` check.
 */
export type KillPoint = "build" | "proof";

/** The signals that end a proof a person or a supervisor stops. */
export type StopSignal = "TERM" | "INT" | "HUP";

/** What one stand-in run asks for: the driver's output, and where to stop the run, if anywhere. */
export type StandInRun = {
  /** The lines the stand-in driver prints, as a real driver prints them. */
  driverOutput: string[];
  /** Where a stand-in signals the run's whole process group; unset, the run goes to its end. */
  kill?: { at: KillPoint; signal: StopSignal };
};

/**
 * The line lib/run-verdict.sh prints above the FAIL line of a run that ended with status 0 before
 * its end, as a signal or an exit of its own ends it.
 */
export const stopped =
  "the run stopped before its end, by a signal or an exit of its own: its status 0 is no pass";

// Each stand-in's own check of where to stop the run: it signals its whole process group, as a
// closed pane (HUP), a Ctrl-C (INT) or timeout's TERM does, and then waits to be ended with it.
// It waits half a second first, so the shell has started every command of the pipeline the
// stand-in runs in: a child bash has forked and not yet run its command still holds the shell's
// traps, and the signal would run the EXIT trap there as well, a window a real driver, which runs
// for minutes, never stops in.
const stopHere = `stop_here() {
  [ "\${LIVE_DAEMON_KILL_AT:-}" = "$1" ] || return 0
  sleep 0.5
  kill -s "$LIVE_DAEMON_KILL_SIGNAL" 0
  sleep 5
}`;

/**
 * Lays out in DIR the commands lib/live-daemon-proof.sh's run reaches, and returns the environment
 * a fixture proof runs the real lib in, so a test drives the real run, its end mark and its last
 * line on a host with no nats-server, no libpq and no time for a Go build:
 *
 *   - `go` answers `go version`, and for `go … test -c … -o FILE` writes the stand-in driver to
 *     FILE;
 *   - `python3` answers lib/loopback-dsn.py with 127.0.0.1, the host a loopback DSN names;
 *   - `jj` answers `jj --version` with the oldest release the daemon's boot starts with, which
 *     approved-version-news.sh's check_jj reads, on a host that has no jj;
 *   - the nats-server LEGION_E2E_NATS_BIN names logs the two lines the `broker` check reads, then
 *     waits to be stopped;
 *   - the driver prints `run.driverOutput`.
 *
 * With `run.kill`, the stand-in at that point signals its whole process group before it does
 * anything else, so the fixture proof must run in a process group of its own: `inOwnGroup` below
 * runs it that way.
 */
export function liveDaemonStandIns(dir: string, run: StandInRun): Record<string, string> {
  const bin = join(dir, "bin");
  mkdirSync(bin, { recursive: true });
  mkdirSync(join(dir, "tmp"), { recursive: true });
  const script = (path: string, body: string) => {
    writeFileSync(path, `#!/usr/bin/env bash\n${stopHere}\n${body}\n`);
    chmodSync(path, 0o755);
  };
  script(
    join(bin, "go"),
    `if [ "$1" = version ]; then echo "go version go1.25.0 linux/amd64"; exit 0; fi
out=''
while [ $# -gt 0 ]; do
  [ "$1" != -o ] || out=$2
  shift
done
stop_here build
cp "$LIVE_DAEMON_STAND_IN_DIR/driver" "$out"`
  );
  script(join(bin, "python3"), "echo 127.0.0.1");
  script(
    join(bin, "jj"),
    `[ "$1" = --version ] || { echo "the fixture's jj answers --version alone" >&2; exit 1; }
echo "jj 0.38.0"`
  );
  script(
    join(dir, "nats-server"),
    `if [ "\${1:-}" = --version ]; then echo "nats-server: v2.15.1"; exit 0; fi
echo "[1] Listening for client connections on 127.0.0.1:4222"
echo "[1] Server is ready"
exec sleep 60`
  );
  script(join(dir, "driver"), `stop_here proof\ncat "$LIVE_DAEMON_STAND_IN_DIR/driver-output"`);
  writeFileSync(join(dir, "driver-output"), run.driverOutput.map((line) => `${line}\n`).join(""));
  return {
    ...(process.env as Record<string, string>),
    PATH: `${bin}:${process.env.PATH ?? ""}`,
    TMPDIR: join(dir, "tmp"),
    LEGION_TEST_PG_DSN: "postgres://fixture@127.0.0.1:1/fixture?sslmode=disable",
    LEGION_E2E_NATS_BIN: join(dir, "nats-server"),
    LIVE_DAEMON_STAND_IN_DIR: dir,
    LIVE_DAEMON_KILL_AT: run.kill?.at ?? "",
    LIVE_DAEMON_KILL_SIGNAL: run.kill?.signal ?? "",
  };
}

/** How a fixture proof ended: its exit status or the signal that ended it, and what it printed. */
export type ProofRun = { code: number | null; signal: string | null; out: string; err: string };

/**
 * Runs COMMAND (argv) in a session and process group of its own (`setsid -w`), so a stand-in's
 * signal to its group reaches the whole run and nothing of the test's. A run whose output goes
 * through lib/transcript.sh prints its stderr on stdout too.
 */
export function inOwnGroup(command: string[], env: Record<string, string>): ProofRun {
  const result = Bun.spawnSync(["setsid", "-w", ...command], { env });
  return {
    code: result.exitCode,
    signal: result.signalCode ?? null,
    out: result.stdout.toString(),
    err: result.stderr.toString(),
  };
}
