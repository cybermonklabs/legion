import { afterAll } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { captureEnvironment } from "@legion/test-fixtures/environment";

// The package's preload (both `bun test` commands of the `test` script pass `--preload
// ./test/omp-logs.ts`): Oh My Pi's logger writes and prunes a directory this run owns, never the
// caller's own `~/.omp/logs`.
//
// `@oh-my-pi/pi-utils` (18.8.3, the version `bun.lock` resolves) fixes its log directory as it
// loads: `dirs.ts` builds its `DirResolver` from the environment, and `env.ts` builds it once more
// after reading the caller's `.env` files. The directory is `$XDG_STATE_HOME/omp/logs` when
// `XDG_STATE_HOME/omp` exists and neither a profile (`OMP_PROFILE`, `PI_PROFILE`) nor
// `PI_CODING_AGENT_DIR` is set, and `<home>/.omp/logs` (or a profile's) otherwise. The logger
// prunes that directory on its first record (`pruneStaleProcessLogs`, a synchronous `readdirSync`
// and a `process.kill(pid, 0)` for each file), so a home with tens of thousands of log files held
// the event loop past a row's 20 ms capability budget and the row read four rows ok where it
// reads five (dispatch://INFRA-3314). Setting `HOME` cannot move it: under Bun 1.4.4
// `os.homedir()` ignores a `HOME` set while the process runs.
//
// So this module loads the package before any test file does, with the profile names deleted and
// `XDG_STATE_HOME` naming a directory of its own that holds `omp`, then puts those four names back
// as the caller had them: the resolver keeps the directory it was built with, and every test and
// every process a test starts reads the environment the caller gave the run. It loads the
// package's index, not `dirs.ts` alone, since `env.ts` builds the resolver again from the
// environment when it loads.
const ownedNames = ["OMP_PROFILE", "PI_PROFILE", "PI_CODING_AGENT_DIR", "XDG_STATE_HOME"];
const restore = captureEnvironment(ownedNames);
const state = mkdtempSync(path.join(tmpdir(), "pi-legion-omp-state-"));
// Removed once the run's last file has ended: a preload's `afterAll` runs once, after every file,
// and even when a `-t` filter selects no row (measured on Bun 1.4.4), while an `exit` handler never
// runs under `bun test`. The logger never throws, so a record it writes after that is only lost.
const removeState = () => rmSync(state, { recursive: true, force: true });
afterAll(removeState);
mkdirSync(path.join(state, "omp"));
for (const name of ownedNames) delete process.env[name];
process.env.XDG_STATE_HOME = state;
// A dynamic import: a static one is evaluated before the lines above change the environment.
const { getLogsDir } = await import("@oh-my-pi/pi-utils");
restore();

const logs = getLogsDir();
const owned = path.join(state, "omp", "logs");
if (logs !== owned) {
  removeState();
  throw new Error(
    `Oh My Pi's log directory is ${logs}, not this run's ${owned}: a .env file Oh My Pi reads ` +
      "as it loads (the home's, its config root's, its agent directory's or the working " +
      "directory's) may set PI_CODING_AGENT_DIR"
  );
}
