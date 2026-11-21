import { beforeAll, describe, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { timeLeft } from "@legion/test-fixtures/wait";

// claude-backend-tmux.sh builds claude-envoy with packages/claude-envoy/scripts/build.ts, which
// refuses any Bun but the one the repository's .bun-version pins. A devbox whose `bun` is another
// release must be refused in prerequisites, naming the pin, before the run builds or starts
// anything, rather than failing later in the rig at the build line. Every tool is a stand-in, so
// the run stops at its first step that needs a real one and touches nothing outside this directory.
const root = join(import.meta.dir, "..", "..", "..");
const script = join(root, "scripts", "e2e", "claude-backend-tmux.sh");
const pinned = readFileSync(join(root, ".bun-version"), "utf8").trim();
const dir = temporaryDirectory("claude-backend-bun-pin-test");

const bin = join(dir, "bin");
const scratch = join(dir, "tmp");
function stub(name: string, body: string) {
  writeFileSync(join(bin, name), `#!/bin/sh\n${body}\n`);
  chmodSync(join(bin, name), 0o755);
}
/** Every stand-in the run's bin holds, each a name and the shell its stub runs. */
const stubs: Array<[string, string]> = [
  ...["go", "docker", "ss", "tmux", "mise", "gh", "jj", "pgrep", "claude", "python3"].map(
    (name) => [name, "exit 0"] as [string, string]
  ),
  // A Bun that is not the pinned release: it answers --version with another one.
  ["bun", `case "$1" in --version) echo ${pinned === "1.0.0" ? "1.0.1" : "1.0.0"};; esac\nexit 0`],
  ["curl", "printf 200"],
  ["agent-secrets", 'name=$1; shift; [ "$1" = -- ] && shift; export "$name={}"; exec "$@"'],
];
// hostTool gives the real NAME binary to put on the run's PATH. Bun.which takes the first match on
// this process's PATH, which on a machine with a version manager is a shim: a wrapper that resolves
// the tool it stands for out of the caller's environment — the manager's configuration under HOME,
// the configuration of the directory the caller is in, and, with no version pinned, whatever comes
// next on PATH. The run below has a HOME and a PATH of its own, where the manager pins nothing and
// nothing follows the shim, so a shim fails in place of the tool (mise answers `jq is not a valid
// shim`, exit 1) and the run stops at a line that has nothing to do with the pin. So each name
// takes the first entry on PATH that answers `--version` the same way under the run's own
// environment as under this process's: a real binary, which no environment changes. A tool that
// has no `--version` refuses it the same way under either, so it is taken too.
function hostTool(name: string, path = process.env.PATH ?? ""): string | null {
  for (const entry of path.split(":")) {
    if (entry === "") continue;
    const candidate = join(entry, name);
    if (!existsSync(candidate)) continue;
    const answer = (env: Record<string, string>) => {
      const ran = versionProbe(candidate, env);
      if (ran === null) return null; // not a file this process can run
      // A probe the deadline ended says nothing of the binary, so the walk does not pass it over
      // as if the host had no such tool, which would make the row skip: it fails here, by name.
      if (ran.exitedDueToTimeout === true) {
        throw new Error(
          `${candidate} --version was still running at the deadline this probe runs under (the ` +
            `bun test --timeout, from the row-deadline preload's last mark); its output: ` +
            `${ran.stdout.toString()}${ran.stderr.toString()}`
        );
      }
      return `${ran.exitCode} ${ran.stdout.toString()}`;
    };
    const here = answer({ HOME: process.env.HOME ?? "", PATH: path });
    if (here !== null && here === answer({ HOME: dir, PATH: bin })) return candidate;
  }
  return null;
}

/** `candidate --version` under `env`, ended at the deadline it runs under; null where `candidate`
 *  is no file this process can run. spawnSync takes whole milliseconds, and reads 0 as no bound. */
function versionProbe(candidate: string, env: Record<string, string>) {
  try {
    return Bun.spawnSync([candidate, "--version"], {
      env,
      stdout: "pipe",
      stderr: "pipe",
      timeout: Math.max(1, Math.floor(timeLeft())),
    });
  } catch {
    return null;
  }
}

// The real programs the run needs on the PATH it is given, each linked into its bin below.
const tools = [
  "bash",
  "sh",
  "jq",
  "awk",
  "sed",
  "tr",
  "cat",
  "mktemp",
  "date",
  "getconf",
  "readlink",
  "realpath",
  "dirname",
  "basename",
  "grep",
  "env",
  "rm",
  "mkdir",
  "chmod",
  "ls",
  "head",
  "tail",
  "cut",
  "sort",
  "uniq",
  "wc",
  "sleep",
  "kill",
  "id",
  "stat",
  "seq",
  "timeout",
  "touch",
  "cp",
  "mv",
  "ln",
  "find",
  "xargs",
  "tee",
  "od",
  "base64",
  "flock",
  "ps",
  "sha256sum",
];
const keyFile = join(dir, "model-key");

// The walk over PATH runs in this describe's body, which Bun runs while it collects the file's
// rows, before any hook, and which the row-deadline preload already gives a deadline: the probes
// are bounded by `timeLeft()`, and one still running at that deadline throws here, which fails
// the file by name ("Unhandled error between tests") instead of making the row skip. It cannot
// run in a beforeAll: Bun 1.4.4 decides a row's skip when the row is registered, from a boolean
// (`skipIf(condition: boolean): Test<T>`, bun-types 1.4.4 test.d.ts:565), a hook runs only after
// every row is registered, and a row is handed no context to skip itself through (`test.skip()`
// inside a row throws "Cannot call test.skip() inside a test").
describe("claude-backend-tmux.sh's Bun pin", () => {
  const found = new Map(tools.map((name) => [name, hostTool(name)] as const));
  const missing = tools.filter((name) => found.get(name) === null);
  // Why the row that runs the rig can say nothing: a tool the walk found no real binary for would
  // be absent from the run's PATH, and the run would stop at the first line that calls it rather
  // than at the pin; and with no HOME of this process the walk has no second environment to tell
  // a real binary from a version manager's shim.
  const reasons: string[] = [];
  if (missing.length > 0) {
    reasons.push(`this host has no real binary on PATH for: ${missing.join(", ")}`);
  }
  if ((process.env.HOME ?? "") === "") {
    reasons.push(
      "this process has no HOME, which the walk over PATH needs to tell a real binary from a " +
        "version manager's shim"
    );
  }
  const skip = reasons.length > 0;
  if (skip) {
    console.warn(
      `Skipped the test in ${import.meta.path} that runs the rig: ${reasons.join("; ")}.`
    );
  }

  // The run's own bin, home and scratch, built before the row runs: `dir` is reserved at module
  // scope and made by the hook that reservation registered, and a run that selects no row of
  // this file runs neither hook and builds nothing.
  beforeAll(() => {
    mkdirSync(bin);
    mkdirSync(scratch);
    for (const [name, body] of stubs) stub(name, body);
    for (const [name, path] of found) {
      if (path !== null) symlinkSync(path, join(bin, name));
    }
    writeFileSync(keyFile, "sk-test\n", { mode: 0o600 });
  });

  test.skipIf(skip)(
    "a bun other than the pinned release is refused in prerequisites, naming the pin",
    () => {
      const run = Bun.spawnSync(["bash", script], {
        env: {
          ...bunCacheOff,
          HOME: dir,
          PATH: bin,
          TMPDIR: scratch,
          LEGION_E2E_MODEL_ROUTE: "key-file",
          LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile,
          // The review App's key command, which prerequisites requires and has no default for
          // (lib/github-apps.sh), and which this run does run, on the driver: write_github_apps
          // --key-dir resolves the key into the run's own file before the pin check
          // (claude-backend-tmux.sh, "After write_github_apps"), so a run that set no command, or
          // one that printed nothing, would be refused there and never reach the pin this row is
          // about. `printf` is the shell's own builtin, so the command answers under the stand-in
          // PATH this run is given, and the first FAIL line below is still the pin's.
          LEGION_E2E_REVIEW_APP_KEY_COMMAND: "printf 'a stand-in review App key'",
        },
        stdout: "pipe",
        stderr: "pipe",
        // The run ends before the row's deadline, so a run that hangs fails here with its output.
        // spawnSync takes whole milliseconds, and reads 0 as no bound at all.
        timeout: Math.max(1, Math.floor(timeLeft())),
      });
      const output = run.stdout.toString() + run.stderr.toString();
      const failLine = output.split("\n").find((line) => line.startsWith("FAIL ")) ?? "";
      expect(failLine).toMatch(/^FAIL prerequisites: /);
      expect(failLine).toContain(pinned);
    }
  );

  // The walk itself, against two probe scripts this row writes: it needs none of the tools above,
  // so a host that has not got them still runs it.
  test("a tool that answers only for this process's own environment is passed over for a real binary", () => {
    const shims = join(dir, "shims");
    const real = join(dir, "real");
    mkdirSync(shims);
    mkdirSync(real);
    // A shim as a version manager writes one: it stands for the tool while it can read the
    // manager's configuration under HOME, or, with nothing pinned there, while another copy of the
    // tool follows it on PATH. Under the run's own HOME and PATH it has neither, and says so.
    writeFileSync(
      join(shims, "probe"),
      `#!/bin/sh
case ":$PATH:" in *:${real}:*) exec ${join(real, "probe")} "$@" ;; esac
[ "$HOME" = ${JSON.stringify(process.env.HOME ?? "")} ] || { echo 'probe is not a valid shim' >&2; exit 1; }
echo 'probe 1.0'
`,
      { mode: 0o755 }
    );
    writeFileSync(join(real, "probe"), "#!/bin/sh\necho 'probe 1.0'\n", { mode: 0o755 });
    expect(hostTool("probe", `${shims}:${real}`)).toBe(join(real, "probe"));
    expect(hostTool("probe", shims)).toBe(null);
    expect(hostTool("probe", real)).toBe(join(real, "probe"));
  });
});
