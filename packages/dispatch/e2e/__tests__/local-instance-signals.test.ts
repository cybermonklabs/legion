import { afterAll, expect, test } from "bun:test";
import {
  closeSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { ignoresSignal, sleepRuns } from "@legion/test-fixtures/held-process";

import { rowLimit, watch, whether } from "./bounded-subprocess";

// What `bun run dispatch:local` (e2e/local-instance.ts) does with a signal during its first step,
// the run of packages/envoy/scripts/dev-postgres.sh: a Ctrl-C, a SIGTERM, a closing terminal's
// SIGHUP and its quit key's SIGQUIT each stop that step's process, which the command started
// detached, in a session of its own (`Supervisor.start`), so no signal sent to the command
// reaches it. A signal the command does not listen for ends it by its default action and leaves
// that process running.
//
// No database, build or container runs. The command checks only that its tools are on PATH
// before that step, so PATH leads with a directory of stand-ins: one for each tool, which does
// nothing, and a `bash` that becomes the step's process - it records its pid and becomes
// `sleep 276` under it, a command line no other process of this directory's tests has. The command
// runs under a shell that sets the core size to 0 and execs it, so a SIGQUIT that met its default
// action would write no core into the tree.
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const stubs = mkdtempSync(join(tmpdir(), "local-instance-signals-"));
const seconds = "276";
const standIns: number[] = [];
afterAll(() => {
  for (const pid of standIns) {
    if (sleepRuns(pid, seconds)) process.kill(pid, "SIGKILL");
  }
  rmSync(stubs, { force: true, recursive: true });
});

/** The pid the stand-in recorded, or 0 before it has. */
function recorded(pidFile: string): number {
  try {
    return Number(readFileSync(pidFile, "utf8"));
  } catch {
    return 0;
  }
}

/** dispatch:local run as these rows run it, under a shell that ignores `ignored` first where one
 *  is named, with the stand-ins of `dir` first on PATH, its output and the stand-in's in the file
 *  `said`: a pipe would stay open for as long as a stand-in the command left behind runs. */
function startLocal(dir: string, ignored?: "HUP") {
  const bin = join(dir, "bin");
  mkdirSync(bin);
  for (const tool of ["go", "psql", "flock", "openssl", "python3", "docker"]) {
    writeFileSync(join(bin, tool), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
  }
  const pidFile = join(dir, "step.pid");
  writeFileSync(
    join(bin, "bash"),
    `#!/bin/sh\necho $$ >'${pidFile}.new'\nmv '${pidFile}.new' '${pidFile}'\nexec sleep ${seconds}\n`,
    { mode: 0o755 }
  );
  const said = join(dir, "said");
  const output = openSync(said, "w");
  const ignore = ignored === undefined ? "" : `trap '' ${ignored}; `;
  const local = Bun.spawn(
    [
      "/bin/sh",
      "-c",
      `${ignore}ulimit -c 0 && exec "$@"`,
      "sh",
      process.execPath,
      "e2e/local-instance.ts",
    ],
    {
      cwd: dispatchPackage,
      env: { HOME: process.env.HOME, PATH: `${bin}:${process.env.PATH}`, TMPDIR: dir },
      stdio: ["ignore", output, output],
    }
  );
  closeSync(output);
  return { local, pidFile, said: () => readFileSync(said, "utf8") };
}

for (const signal of ["SIGINT", "SIGTERM", "SIGHUP", "SIGQUIT"] as const) {
  test(
    `a ${signal} during dispatch:local's first step stops that step's process, not the command alone`,
    async () => {
      const { local, pidFile, said } = startLocal(mkdtempSync(join(stubs, `${signal}-`)));
      const ended = watch(local.exited);
      try {
        const started = await whether(() => sleepRuns(recorded(pidFile), seconds), ended);
        expect(started, `the step's stand-in never ran; the command said:\n${said()}`).toBe(true);
        const standIn = recorded(pidFile);
        standIns.push(standIn);
        local.kill(signal);
        expect(await whether(ended), `dispatch:local still ran after its ${signal}`).toBe(true);
        expect(
          await whether(() => !sleepRuns(standIn, seconds)),
          `the step's stand-in (pid ${standIn}) still runs after dispatch:local ended on a ${signal} ` +
            `(exit code ${local.exitCode}, signal ${local.signalCode}):\n${said()}`
        ).toBe(true);
      } finally {
        if (!ended()) local.kill("SIGKILL");
      }
    },
    rowLimit("short", "short", "short")
  );
}

// `nohup bun run dispatch:local &` starts the command with SIGHUP ignored, so that it outlives the
// terminal. It keeps that ignore through its first step, when every listener it arms is armed: the
// kernel's record of what it ignores still names SIGHUP there, where a listener for SIGHUP would
// have cleared it and ended the command on the hangup it was started to outlive. So the SIGHUP
// sent then changes nothing, and the SIGTERM sent after it still stops the step.
test(
  "a SIGHUP dispatch:local was started ignoring stays ignored, and a SIGTERM after it still stops the first step",
  async () => {
    const { local, pidFile, said } = startLocal(mkdtempSync(join(stubs, "nohup-")), "HUP");
    const ended = watch(local.exited);
    try {
      const started = await whether(() => sleepRuns(recorded(pidFile), seconds), ended);
      expect(started, `the step's stand-in never ran; the command said:\n${said()}`).toBe(true);
      const standIn = recorded(pidFile);
      standIns.push(standIn);
      expect(
        ignoresSignal(local.pid, "SIGHUP"),
        `dispatch:local, started with SIGHUP ignored, no longer ignores it during its first step:\n${said()}`
      ).toBe(true);
      local.kill("SIGHUP");
      local.kill("SIGTERM");
      expect(await whether(ended), "dispatch:local still ran after its SIGTERM").toBe(true);
      expect(local.signalCode, `dispatch:local ended by its SIGHUP:\n${said()}`).not.toBe("SIGHUP");
      expect(
        await whether(() => !sleepRuns(standIn, seconds)),
        `the step's stand-in (pid ${standIn}) still runs after dispatch:local ended on its SIGTERM:\n${said()}`
      ).toBe(true);
    } finally {
      if (!ended()) local.kill("SIGKILL");
    }
  },
  rowLimit("short", "short", "short")
);
