import { beforeAll, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { hostGate } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { unrealReader } from "./proc-root";

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
/** The second row's bin: the same tools and stand-ins, but a bun that cannot run at all, as a mise
 *  shim with no version set answers every call, `--version` among them. */
const brokenBin = join(dir, "broken-bin");
const scratch = join(dir, "tmp");
function stub(name: string, body: string, into = bin) {
  writeFileSync(join(into, name), `#!/bin/sh\n${body}\n`);
  chmodSync(join(into, name), 0o755);
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
      try {
        const ran = Bun.spawnSync([candidate, "--version"], {
          env,
          stdout: "pipe",
          stderr: "pipe",
          timeout: 10_000,
        });
        return `${ran.exitCode} ${ran.stdout.toString()}`;
      } catch {
        return null; // not a file this process can run
      }
    };
    const here = answer({ HOME: process.env.HOME ?? "", PATH: path });
    if (here !== null && here === answer({ HOME: dir, PATH: bin })) return candidate;
  }
  return null;
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
const found = new Map(tools.map((name) => [name, hostTool(name)] as const));
const missing = tools.filter((name) => found.get(name) === null);
// Why the rows that run the rig can say nothing: a tool the walk found no real binary for would be
// absent from the run's PATH, and the run would stop at the first line that calls it rather than at
// the pin; and with no HOME of this process the walk has no second environment to tell a real
// binary from a version manager's shim.
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
// Outside CI the rows skip, naming why; under GITHUB_ACTIONS they fail so, each by its own name
// (hostGate).
const host = hostGate({
  reason: reasons.length > 0 ? `The rows that run the rig cannot run: ${reasons.join("; ")}.` : "",
});
const keyFile = join(dir, "model-key");
// The proc root refuse_leftovers reads in prerequisites (`${proc_root:-/proc}`). Over the host's
// /proc refuse_leftovers runs three commands for each process the host has, so the row's time, held
// to the run's 60 s, was the host's process
// count, and a process an earlier run of the proof left anywhere on the host made the first FAIL
// line its own. The run starts no process that walk is to find before it stops, and no earlier
// run's process is the row's, so its root holds one entry alone: an unreal pid whose command line
// only refuse_leftovers reads (unrealReader), so a refuse_leftovers that walked the host's /proc
// again fails the row rather than only slowing it.
const proc = join(dir, "proc");
/** The second row's proc root: unrealReader makes its entry once in a root. */
const brokenProc = join(dir, "broken-proc");

// The run's own bins, home and scratch, built before the rows run: `dir` is reserved at module
// scope and made by the hook that reservation registered, and a run that selects no row of this
// file runs neither hook and builds nothing.
beforeAll(() => {
  mkdirSync(scratch);
  for (const [into, root] of [
    [bin, proc],
    [brokenBin, brokenProc],
  ] as const) {
    mkdirSync(into);
    mkdirSync(root);
    for (const [name, body] of stubs) stub(name, body, into);
    for (const [name, path] of found) {
      if (path !== null) symlinkSync(path, join(into, name));
    }
  }
  stub("bun", 'echo "No version is set for shim: bun" >&2\nexit 1', brokenBin);
  writeFileSync(keyFile, "sk-test\n", { mode: 0o600 });
});

/** Runs claude-backend-tmux.sh with PATH `path` and proc root `root`, and returns its first FAIL
 *  line, after requiring that its refuse_leftovers read the entry `root` holds. */
function firstFailLine(path: string, root: string): string {
  const walked = unrealReader(root);
  const run = Bun.spawnSync(["bash", script], {
    env: {
      HOME: dir,
      PATH: path,
      TMPDIR: scratch,
      proc_root: root,
      LEGION_E2E_MODEL_ROUTE: "key-file",
      LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile,
      // The review App's key command, which prerequisites requires and has no default for
      // (lib/github-apps.sh), and which this run does run, on the driver: write_github_apps
      // --key-dir resolves the key into the run's own file before the pin check
      // (claude-backend-tmux.sh, "After write_github_apps"), so a run that set no command, or
      // one that printed nothing, would be refused there and never reach the pin these rows are
      // about. `printf` is the shell's own builtin, so the command answers under the stand-in
      // PATH this run is given, and the first FAIL line below is still the pin's.
      LEGION_E2E_REVIEW_APP_KEY_COMMAND: "printf 'a stand-in review App key'",
    },
    stdout: "pipe",
    stderr: "pipe",
    timeout: 60_000,
  });
  const output = run.stdout.toString() + run.stderr.toString();
  expect(walked(), `refuse_leftovers read no entry of the proc root it was given:\n${output}`).toBe(
    true
  );
  return output.split("\n").find((line) => line.startsWith("FAIL ")) ?? "";
}

host.test(
  "a bun other than the pinned release is refused in prerequisites, naming the pin",
  () => {
    const failLine = firstFailLine(bin, proc);
    expect(failLine).toMatch(/^FAIL prerequisites: /);
    expect(failLine).toContain(pinned);
  },
  90_000
);

// A bun that cannot run, as mise's shim answers under a HOME whose config names no version, is
// refused in prerequisites with its own words, not read as a Bun of no version.
host.test(
  "a bun that cannot run is refused in prerequisites with its own error",
  () => {
    const failLine = firstFailLine(brokenBin, brokenProc);
    expect(failLine).toMatch(/^FAIL prerequisites: /);
    expect(failLine).toContain(
      `the bun first on PATH (${brokenBin}/bun) could not run \`bun --version\`: No version is set for shim: bun`
    );
    expect(failLine).not.toContain("<no version>");
  },
  90_000
);

// The walk itself, against two probe scripts this row writes: it needs none of the tools above, so
// a host that has not got them still runs it.
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
