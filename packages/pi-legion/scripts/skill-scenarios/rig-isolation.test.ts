import { beforeAll, expect, setDefaultTimeout, test } from "bun:test";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { ownGroup } from "../../../../scripts/e2e/lib/own-group";

// Two runs of one batch, started at the same time as the batch's xargs starts them:
// `rig.sh run askgate on <n> <index> <start>`. The rig.sh under test is this one, copied into a
// checkout that holds its e2e helpers and stand-ins for what a run reaches:
//   - docker: the batch's Postgres container;
//   - packages/dispatch/e2e/run-server.sh: a server that answers every request with the database
//     its DATABASE_URL names;
//   - seed.ts: records the Dispatch that each seed reached, and writes the fixture;
//   - tmux: runs the pane's command at once;
//   - mise: answers `mise where <pin>` with a pin whose bin/omp is the agent, which records the
//     Dispatch variables of its environment, and runs `mise x <pin> -- <command>` as the command
//     (a run names its pin's omp by path).
// Each run must get a database of its own, a server of its own and a DISPATCH_URL of its own, and
// must stop its server when it ends.
setDefaultTimeout(120_000);

// Every row here runs the rig through ownGroup (scripts/e2e/lib/own-group.ts), a perl that puts it
// in a process group of its own (`runRig` below says why it must). Read once, so one warning names
// perl for the whole file.
const withoutPerl = skipWithout("perl");

const root = path.resolve(import.meta.dir, "../../../..");
const scratch = temporaryDirectory("skill-scenarios-isolation", { real: true });

/**
 * Every write this file's body asks for, as a function the `beforeAll` at the end calls. The
 * module body makes nothing: `scratch` is reserved at the call and made by the hook that
 * reservation registered, and a `bun test -t <pattern>` that selects no row of this file runs no
 * hook of it, where a module body that built the checkout and ran the two rigs would leave all of
 * it behind.
 */
const stagings: (() => void)[] = [];

/** Writes `text` to `file`, making its directory, and makes it executable when `mode` says so,
 *  when the staging runs. */
function put(file: string, text: string, mode = 0o644) {
  stagings.push(() => {
    mkdirSync(path.dirname(file), { recursive: true });
    writeFileSync(file, text);
    chmodSync(file, mode);
  });
}

// The checkout: this rig.sh and the e2e helpers it sources, with the two Dispatch stand-ins.
const co = path.join(scratch, "checkout");
const rig = path.join(co, "packages/pi-legion/scripts/skill-scenarios/rig.sh");
const stub = path.join(scratch, "stub");
stagings.push(() => {
  mkdirSync(path.dirname(rig), { recursive: true });
  copyFileSync(path.join(import.meta.dir, "rig.sh"), rig);
  for (const lib of [
    "rig.sh",
    "process-tree.sh",
    "loopback-dsn.sh",
    "loopback-dsn.py",
    "port-file.sh",
    "held-port.py",
  ]) {
    mkdirSync(path.join(co, "scripts/e2e/lib"), { recursive: true });
    copyFileSync(path.join(root, "scripts/e2e/lib", lib), path.join(co, "scripts/e2e/lib", lib));
  }
  mkdirSync(stub);
});
put(
  path.join(co, "packages/dispatch/e2e/run-server.sh"),
  ["#!/usr/bin/env bash", 'exec bun "$STUB_DIR/server.ts"', ""].join("\n"),
  0o755
);
// No DISPATCH_E2E_PORT: the server takes a port of the kernel's choosing and reports it as the Go
// server does (`msg="dispatch: listening" addr=127.0.0.1:<port>`), which start_dispatch reads from
// the run's own log.
put(
  path.join(stub, "server.ts"),
  [
    'import { appendFileSync } from "node:fs";',
    'import path from "node:path";',
    'const database = new URL(process.env.DATABASE_URL ?? "").pathname.slice(1);',
    "const server = Bun.serve({",
    '  hostname: "127.0.0.1",',
    "  port: 0,",
    "  fetch: () => Response.json({ database }),",
    "});",
    "appendFileSync(",
    '  path.join(process.env.STUB_DIR ?? "", "servers.log"),',
    // `\${…}` keeps each placeholder literal here: the stub writes them into server.ts, where
    // Bun expands them when it runs.
    `  \`\${process.pid} \${server.port} \${process.env.DATABASE_URL}\\n\``,
    ");",
    `console.log(\`msg="dispatch: listening" addr=127.0.0.1:\${server.port}\`);`,
    "",
  ].join("\n")
);
put(
  path.join(co, "packages/pi-legion/scripts/skill-scenarios/seed.ts"),
  [
    'import { appendFileSync, writeFileSync } from "node:fs";',
    'import path from "node:path";',
    "const [command, file, runDir] = Bun.argv.slice(2);",
    "const port = Number(process.env.DISPATCH_E2E_PORT);",
    'const answer = await fetch("http://127.0.0.1:" + port + "/api/v1/whoami");',
    "const { database } = await answer.json();",
    "appendFileSync(",
    '  path.join(process.env.STUB_DIR ?? "", "seeds.log"),',
    '  JSON.stringify({ command, port, database }) + "\\n"',
    ");",
    'if (command === "askgate")',
    "  writeFileSync(",
    '    file ?? "",',
    '    JSON.stringify({ issue: "LWEVAL-2", comment: "c1", comment_ref: "dispatch://LWEVAL-2/comment/c1" })',
    "  );",
    'if (command === "capture-askgate")',
    '  for (const name of ["asks.json", "artifacts.json"]) writeFileSync(path.join(runDir ?? "", name), "[]\\n");',
    "",
  ].join("\n")
);

// The stand-ins first on PATH.
const bin = path.join(scratch, "bin");
put(
  path.join(bin, "docker"),
  [
    "#!/usr/bin/env bash",
    'printf \'%s\\n\' "$*" >>"$STUB_DIR/docker.log"',
    'case $1 in port) echo "127.0.0.1:15432" ;; inspect) echo stub-postgres ;; exec) ;; *) exit 1 ;; esac',
    "",
  ].join("\n"),
  0o755
);
// tmux new-session -d -s <session> -x <w> -y <h> <command>: runs <command>, its last argument, at
// once, in the foreground.
put(
  path.join(bin, "tmux"),
  [
    "#!/usr/bin/env bash",
    "shift 2",
    '[ "$1" = new-session ] || exit 1',
    'for command in "$@"; do :; done',
    'bash -c "$command"',
    "",
  ].join("\n"),
  0o755
);
put(
  path.join(bin, "mise"),
  [
    "#!/usr/bin/env bash",
    `if [ "$1" = where ]; then echo ${JSON.stringify(path.join(bin, "pin"))}; exit 0; fi`,
    'while [ $# -gt 0 ] && [ "$1" != -- ]; do shift; done',
    'shift && exec "$@"',
    "",
  ].join("\n"),
  0o755
);
put(
  path.join(bin, "pin", "bin", "omp"),
  [
    "#!/usr/bin/env bash",
    'env | grep "^DISPATCH_" | sort >"$SKILL_SCENARIO_RUN/agent-env.txt"',
    "",
  ].join("\n"),
  0o755
);
// bun: the Bun that runs this file, so the rig's `bun` (load_label's read of the profile settings,
// the server and the seeds) does not depend on how the caller installed one. On a devbox bun is a
// mise shim, which finds its version in mise's global config under the developer's home, not under
// the HOME below: there the shim exits 1 ("No version is set for shim: bun"), and load_label reads
// that as settings without the handler limit. The link is here, not a directory added to PATH: the
// rig was started through python3, a mise shim there too, and mise drops its install directories
// from the PATH of what a shim runs. ownGroup's perl starts it now, and the link stays the one place
// the rig's bun is chosen.
stagings.push(() => symlinkSync(process.execPath, path.join(bin, "bun")));
// A bun that cannot run, as a mise shim with no version set answers: the stand-in the row below
// puts ahead of the link above.
const brokenBun = path.join(scratch, "broken-bun");
put(
  path.join(brokenBun, "bun"),
  ["#!/usr/bin/env bash", 'echo "No version is set for shim: bun" >&2', "exit 1", ""].join("\n"),
  0o755
);

// The work directory: the label's profile, and the batch's services file and Dispatch token.
const work = path.join(scratch, "work");
/** A label's built profile, with its Oh My Pi settings `settings` (the model route installers'). */
function label(name: string, settings: string) {
  const dir = path.join(work, "profiles", name);
  put(path.join(dir, `home/.omp/profiles/skills-${name}/agent/config.yml`), settings);
  stagings.push(() => {
    for (const [file, text] of [
      ["checkout", co],
      ["pin", "omp@stub"],
      ["profile", `skills-${name}`],
      ["built-from", `checkout ${co}`],
      ["ready", ""],
    ])
      writeFileSync(path.join(dir, file), `${text}\n`);
  });
}
label(
  "on",
  "modelRoles:\n  default: anthropic/m\nextensionHandlers:\n  toolCallTimeoutMs: 120000\n"
);
// A profile built before the installers raised the handler limit.
label("stale", "modelRoles:\n  default: anthropic/m\n");
put(path.join(work, "services/env"), "ENVOY_URL=http://127.0.0.1:1\n");
put(path.join(work, "services/dispatch-token"), "e2e-token\n");
/** The tag rig.sh names the work directory's containers with. */
const tag = `skill-scenarios-${new Bun.CryptoHasher("sha256").update(work).digest("hex").slice(0, 8)}`;

/** The caller's PATH behind the stand-ins, a HOME of the test's own, and the caller's mise data
 * and config directories when the caller names them: jq is a mise shim on a devbox, which finds
 * nothing under a fresh HOME without them. A caller that names neither gets neither, so the
 * developer's own home decides nothing here. */
const miseData =
  process.env.MISE_DATA_DIR ??
  (process.env.XDG_DATA_HOME === undefined
    ? undefined
    : path.join(process.env.XDG_DATA_HOME, "mise"));
const env = {
  ...bunCacheOff,
  PATH: `${bin}:${process.env.PATH ?? ""}`,
  HOME: path.join(scratch, "home"),
  USER: process.env.USER ?? "rig",
  TMPDIR: scratch,
  ...(miseData === undefined ? {} : { MISE_DATA_DIR: miseData }),
  ...(process.env.XDG_CONFIG_HOME === undefined
    ? {}
    : { XDG_CONFIG_HOME: process.env.XDG_CONFIG_HOME }),
  STUB_DIR: stub,
  SKILL_SCENARIOS_WORK: work,
  SKILL_SCENARIOS_ASKGATE_CASE: "facts",
};

/** Runs the rig, in a process group of its own, and returns its exit status and its stderr.
 *
 * The rig stops its own tree before it exits (stop_tree, scripts/e2e/lib/process-tree.sh), and a
 * process group that is orphaned — one no member's parent watches from another group of the same
 * session — gets SIGHUP and SIGCONT from the kernel with a stopped member in it: Linux sends them
 * the moment the group is orphaned, and the gVisor of a Legion pod whenever another member exits.
 * Where a Legion worker runs this suite each shell command is a session leader, so bun's own group
 * is orphaned: a rig sharing it hangs the test runner up, and `bun test` dies of that SIGHUP
 * (status 129) before printing any totals. Bun.spawn leaves its child in bun's own group, so the rig
 * needs to move out of it.
 *
 * A session of its own is the wrong tool: a setsid child leads its session, so the one parent
 * outside its group is in another session, which the orphan test does not count, and the group is
 * orphaned from the start — the rig hangs itself up with the first stop of its own teardown. What
 * it needs is a group of its own inside bun's session, whose parent, bun, is in another group of
 * that session: that parent is the connection outside the group which keeps it from being orphaned.
 *
 * A POSIX shell reaches setpgid only through job control, and a job-control shell writes its job
 * table's notices on its stderr, which is the rig's: on bash 5.2.21, the ubuntu-24.04 runner's of
 * ci.yaml's pi-legion job, `set -m; "$@" & wait "$!"` leaves `[1]+  Done` and the job's command
 * there (bash 5.2.37 prints none, so that wrapper passed on a devbox and failed on the runner). So
 * no shell stands between the test and the rig: ownGroup's perl calls setpgrp and execs the rig's
 * bash, so the pid Bun.spawn returns is the rig's own, with no job table, nothing of its stderr
 * redirected, and the signal dispositions and environment a plain start would give it. A python3 in
 * its place ignored SIGPIPE and SIGXFSZ and coerced LC_CTYPE, and its posix_spawnp handed all three
 * to the rig. The rig's own shell runs with job control off, as it always has. */
async function runRig(args: string[], vars: Record<string, string> = {}) {
  const child = Bun.spawn(ownGroup(["bash", rig, ...args]), {
    env: { ...env, ...vars },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [code, stderr] = await Promise.all([child.exited, new Response(child.stderr).text()]);
  return { code, stderr };
}

/** What the two runs reported, and each server one of them started: both read by the hook below,
 *  which stages the checkout and then runs the two rigs. */
let runs: { n: number; code: number; stderr: string; dir: string }[] = [];
let servers: { pid: number; port: number; database: string }[] = [];
/** The batch's start second, which every run of this file is given; set with the two runs. */
let start = "";
const lines = (file: string) =>
  existsSync(file)
    ? readFileSync(file, "utf8")
        .split("\n")
        .filter((line) => line !== "")
    : [];

beforeAll(async () => {
  // Nothing is staged and no rig is started where the rows do not run: perl starts every one of
  // them (ownGroup), and it would throw on a host that does not have it.
  if (withoutPerl) return;
  // A staging may queue another, so the queue is drained until it is empty.
  for (let stage = stagings.shift(); stage !== undefined; stage = stagings.shift()) stage();
  mkdirSync(env.HOME);
  start = String(Math.floor(Date.now() / 1000));
  runs = await Promise.all(
    [1, 2].map(async (n) => {
      const { code, stderr } = await runRig(["run", "askgate", "on", String(n), String(n), start]);
      return { n, code, stderr, dir: path.join(work, "runs", `askgate-facts-on-${n}`) };
    })
  );
  servers = lines(path.join(stub, "servers.log")).map((line) => {
    const [pid = "", port = "", url = ""] = line.split(" ");
    return { pid: Number(pid), port: Number(port), database: new URL(url).pathname.slice(1) };
  });
});

test.skipIf(withoutPerl)("both runs finish", () => {
  for (const { code, stderr } of runs) expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
});

test.skipIf(withoutPerl)("each run makes a database of its own in the batch's Postgres", () => {
  const made = lines(path.join(stub, "docker.log")).filter((line) =>
    line.includes("CREATE DATABASE")
  );
  for (const line of made) expect(line).toStartWith(`exec ${tag}-pg psql `);
  expect(made.map((line) => /CREATE DATABASE (\w+)/.exec(line)?.[1]).sort()).toEqual([
    "run_askgate_facts_on_1",
    "run_askgate_facts_on_2",
  ]);
});

test.skipIf(withoutPerl)(
  "each run starts a Dispatch server of its own, on its own database and port",
  () => {
    expect(servers.map((server) => server.database).sort()).toEqual([
      "run_askgate_facts_on_1",
      "run_askgate_facts_on_2",
    ]);
    expect(new Set(servers.map((server) => server.port)).size).toBe(2);
  }
);

test.skipIf(withoutPerl)(
  "each run's agent, seeds and capture reach the run's own Dispatch and no other",
  () => {
    const seeds = lines(path.join(stub, "seeds.log")).map((line) => JSON.parse(line));
    const urls = new Set<string>();
    for (const { n, dir } of runs) {
      const database = `run_askgate_facts_on_${n}`;
      const server = servers.find((s) => s.database === database);
      const url = `http://127.0.0.1:${server?.port}`;
      urls.add(url);
      expect(JSON.parse(readFileSync(path.join(dir, "dispatch.json"), "utf8"))).toEqual({
        postgres: "stub-postgres",
        database,
        port: server?.port,
        url,
      });
      expect(lines(path.join(dir, "pane.env"))).toContain(`DISPATCH_URL=${url}`);
      expect(lines(path.join(dir, "agent-env.txt"))).toContain(`DISPATCH_URL=${url}`);
      // The run's world, its fixture and its capture, each through that server to that database.
      expect(seeds.filter((seed) => seed.database === database)).toEqual(
        ["project", "askgate", "capture-askgate"].map((command) => ({
          command,
          port: server?.port,
          database,
        }))
      );
    }
    expect(urls.size).toBe(2);
  }
);

// The run's exit handler kills its server's whole tree before the run exits, so a server's pid is
// gone or a zombie by the time its run has ended.
test.skipIf(withoutPerl)("each run stops its Dispatch server when it ends", () => {
  expect(servers.length).toBe(2);
  const running = servers.filter(
    ({ pid }) =>
      existsSync(`/proc/${pid}/stat`) &&
      !/^\d+ \(.*\) Z /.test(readFileSync(`/proc/${pid}/stat`, "utf8"))
  );
  expect(running).toEqual([]);
});

// AskGate's review would time out under Oh My Pi's default handler limit, so a run whose profile
// leaves the limit there scores the timeout rather than the rule: the rig refuses it, starting nothing.
test.skipIf(withoutPerl)(
  "a run on a profile whose settings leave the handler limit at its default is refused, naming the key",
  async () => {
    const { code, stderr } = await runRig(["run", "askgate", "stale", "1", "3", start]);
    expect(code).toBe(1);
    expect(stderr).toContain("extensionHandlers.toolCallTimeoutMs to 120000 or more");
    expect(existsSync(path.join(work, "runs", "askgate-facts-stale-1"))).toBe(false);
    expect(lines(path.join(stub, "docker.log")).join("\n")).not.toContain("stale");
  }
);

// The read of a profile's settings is bun's, and a bun that cannot run says nothing of the key: the
// rig names bun's failure and its error, and never the key, for a profile that sets the limit and
// for one that does not. The second is the control of the row above, whose refusal names the key
// only where bun read the settings.
test.skipIf(withoutPerl)(
  "a run whose bun cannot read the profile settings is refused, naming bun's failure and not the key",
  async () => {
    for (const [name, index] of [
      ["on", "4"],
      ["stale", "5"],
    ]) {
      const { code, stderr } = await runRig(["run", "askgate", name, "9", index, start], {
        PATH: `${brokenBun}:${env.PATH}`,
      });
      expect(code).toBe(1);
      expect(stderr).toContain(`label ${name}: bun could not read the profile settings`);
      expect(stderr).toContain("No version is set for shim: bun");
      expect(stderr).not.toContain("extensionHandlers.toolCallTimeoutMs to 120000 or more");
      expect(existsSync(path.join(work, "runs", `askgate-facts-${name}-9`))).toBe(false);
    }
  }
);

// The link case's ask states the reindex time from a file, for AskGate's content 9 (an unsourced
// claim about what we run today): the run's working directory must hold that file, with the line
// the prompt quotes.
test.skipIf(withoutPerl)(
  "a link run's working directory holds the crontab its prompt cites, with the line it quotes",
  async () => {
    const { code, stderr } = await runRig(["run", "askgate", "on", "1", "4", start], {
      SKILL_SCENARIOS_ASKGATE_CASE: "link",
    });
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    const dir = path.join(work, "runs", "askgate-link-on-1");
    const prompt = readFileSync(path.join(dir, "prompt.txt"), "utf8");
    const quoted = /export-host\.crontab runs .*\(its line (\d+): `([^`]+)`\)/.exec(prompt);
    const [, line = "", text = ""] = quoted ?? [];
    expect(quoted).not.toBeNull();
    const crontab = lines(path.join(dir, "cwd", "export-host.crontab"));
    expect(crontab[Number(line) - 1]).toBe(text);
  }
);
