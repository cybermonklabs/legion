import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import {
  appendFileSync,
  chmodSync,
  copyFileSync,
  cpSync,
  existsSync,
  lstatSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  realpathSync,
  renameSync,
  rmSync,
  statSync,
  symlinkSync,
  utimesSync,
  writeFileSync,
} from "node:fs";
import { homedir, tmpdir } from "node:os";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import {
  memoryRoot,
  temporaryDirectories,
  temporaryDirectory,
} from "@legion/test-fixtures/temporary-directories";
import type { SpawnOptions, SyncSubprocess } from "bun";
import { z } from "zod";
import { argMaxForceHolds } from "../../../../scripts/e2e/lib/arg-max-force";
import { ownGroup, untilRunning } from "../../../../scripts/e2e/lib/own-group";
import { procRootOf } from "../../../../scripts/e2e/lib/proc-root";
import { rowWaits } from "../../../../scripts/e2e/lib/row-waits";
import { scriptFunctions } from "../../../../scripts/e2e/lib/script-functions";
import { ANSWERS, BOOTED, labelOf, SKIPS, scenarioOf } from "./completion-answers";
import { routeFor } from "./gh-call";
import { routeOf, WORLD_ROUTES, worldRouteOf } from "./gh-routes";
import { headUnmoved } from "./health";
import { leakCheckEnv } from "./leak-check-env";
import { PERSON_ROUTE_SOURCES } from "./person-routes";
import {
  AGENT_WRITTEN,
  agentsOwn,
  agentWritten,
  firstLaunch,
  hostProvided,
  nameWords,
  runEntries,
  runIdentity,
  SCENARIOS,
  textWords,
  vocabulary,
  WORLD_PROJECT,
  WORLD_REMOTE_VCS,
  worldFiles,
} from "./run-names";
import {
  bundled,
  scriptDefinition,
  sourcesCommit,
  spawnChecked as spawnCheckedAnywhere,
  writeGhSelectors,
} from "./test-programs";
import { WORLD_REPO, WORLD_SPEC } from "./world-issue";

// Every test here runs rig.sh and its stand-ins as real processes. On a loaded machine one test
// takes longer than bun's 5 s default (the eight-turn conversation took 11 s), so each test gets
// one minute, and a wait of a row on its processes (untilRunning) ends 20 s before that.
const waits = rowWaits(60_000);

/** The gh stand-in the rows call and put behind a pane's `gh`, a bundle built from gh-standin.ts
 * for this suite, so no call transpiles it again (test-programs.ts). */
const GH_STANDIN = bundled(path.join(import.meta.dir, "gh-standin.ts"));

// The judge runs in a Docker container; every other row here needs no container. Read once, so
// one warning names docker for the whole file.
const withoutDocker = skipWithout("docker");
// The routes rule reads a label's daemon's routes with daemon-routes.go, which `go run` compiles.
const withoutGo = skipWithout("go");
// A label's real install (composeRuns) runs make_omp_home, which reads the caller's jj
// configuration, and install-plugin-profile.sh, which runs the pinned Oh My Pi through mise.
const withoutInstall = skipWithout("mise", "jj");
/** This checkout, the one the head label is built from. */
const CHECKOUT = path.resolve(import.meta.dir, "../../../..");
/** The checkout a run built here names in its built-from unless its row names another. */
const RUN_CHECKOUT = "/recorded/checkout";
/** A run's services-from as rig.sh's services_from writes it, one `<service> <scope> <from>
 * <digest>` a line, every service of the label's own (scope `label`) from `checkout`: the file
 * health's "service provenance" rule holds to the run's built-from. */
function servicesFrom(checkout: string): string {
  return [
    `envoy-listener label ${checkout} sha256:${"0".repeat(64)}`,
    `dispatch label ${checkout} sha256:${"1".repeat(64)}`,
    `plugins label ${checkout} -`,
    `model-route label ${checkout} -`,
    `daemon-facts label ${checkout} sha256:${"2".repeat(64)}`,
    `worker-pane label ${checkout} sha256:${"2".repeat(64)}`,
    "omp pin omp@stub missing",
    "",
  ].join("\n");
}

/** The line a run's own Dispatch server writes when it starts listening (rig.sh's start_dispatch
 * reads its port from it), before the rig seeds the world: the first line of the run's
 * logs/dispatch.log, and its only line while no request has reached the server. */
const LISTENING =
  'time=2026-11-13T17:40:00.000Z level=INFO msg="dispatch: listening" addr=127.0.0.1:43101';
/** The record a run's own GitHub stand-in keeps (github-standin.ts, which start_dispatch starts
 * before the run's Dispatch), its logs/github.log: its listening line, then what a worker world's
 * seeds make its Dispatch ask GitHub, as measured on 2026-11-16 (github-routes.ts): the
 * repository's installation, the token's mint and the read of a result's commit, each at GitHub's
 * own status. A run with its Dispatch's record has this one too, as every run rig.sh starts has. */
const GITHUB_RECORD = [
  '{"at":"2026-11-13T17:39:59.000Z","listening":"127.0.0.1:43100"}',
  '{"at":"2026-11-13T17:40:01.000Z","method":"GET","path":"/repos/fernbrook/widgets/installation","accept":"application/vnd.github+json","status":200,"route":"GET /repos/{owner}/{repo}/installation"}',
  '{"at":"2026-11-13T17:40:01.000Z","method":"POST","path":"/app/installations/1/access_tokens","accept":"application/vnd.github+json","status":201,"route":"POST /app/installations/{id}/access_tokens"}',
  `{"at":"2026-11-13T17:40:01.000Z","method":"GET","path":"/repos/fernbrook/widgets/commits/${"c".repeat(40)}","accept":"application/vnd.github.sha","status":200,"route":"GET /repos/{owner}/{repo}/commits/{sha}"}`,
]
  .map((line) => `${line}\n`)
  .join("");
/** The first line of a run's head watcher's record (head-watcher.ts, which start_heads starts for
 * every worker world), its logs/heads.log, and its only line while nothing was pushed. */
const HEADS_OPENED = '{"at":"2026-11-13T17:39:59.000Z","opened":"heads.log"}\n';
/** The body GitHub answers a request for an endpoint it does not serve with, 404, as gh prints it
 * on stdout before `gh: Not Found (HTTP 404)` on stderr: indented with `\r\n` line ends, and no
 * line end after it (gh-standin.ts's `notFoundBody`, measured on 2026-11-18). */
const GITHUB_404 =
  '{\r\n  "message": "Not Found",\r\n  "documentation_url": "https://docs.github.com/rest",\r\n  "status": "404"\r\n}';

// rig.sh's install_model_route, run with the checkout's real installers: the route a label's
// profile reaches Anthropic by, picked by LEGION_E2E_MODEL_ROUTE.
const root = path.resolve(import.meta.dir, "../../../..");
const fn = scriptFunctions(path.join(import.meta.dir, "rig.sh"));
const rigScript = readFileSync(path.join(import.meta.dir, "rig.sh"), "utf8");
/** rig.sh's own top-level assignment of `name`, the line as the script writes it, so a harness
 * that builds a world with the script's functions builds it with the script's own values. */
function assignment(name: string): string {
  const found = new RegExp(`^${name}=.*$`, "m").exec(rigScript);
  if (found === null) throw new Error(`rig.sh assigns no ${name} at the start of a line`);
  return found[0];
}
/** rig.sh's definition of `name`, here-documents and all (test-programs.ts's `scriptDefinition`). */
const definition = (name: string) => scriptDefinition(rigScript, name);
/** rig.sh's own top-level single-quoted assignment of `name`, over as many lines as it takes, as
 * the script writes it (private_world). */
function quotedAssignment(name: string): string {
  const found = new RegExp(`^${name}='[^']*'$`, "m").exec(rigScript);
  if (found === null) throw new Error(`rig.sh assigns no quoted ${name} at the start of a line`);
  return found[0];
}
/** What rig.sh's namespace_probe prints and its status, under `env`: the probe every run's start
 * makes (require_namespace), in the same form launch.sh starts the agent in, beside the reader of
 * its caller git's system configuration (system_gitconfig), so a probe that read that file would
 * read it here as rig.sh's does. It runs once as this file loads, before operatorHome is
 * initialized, so it starts through the programs' own spawnChecked: the file's own (below) reads
 * operatorHome, which throws a ReferenceError there. */
function namespaceProbe(env: Record<string, string>) {
  const result = spawnCheckedAnywhere(
    [
      "bash",
      "-c",
      `${assignment("namespace_path")}\n${fn("system_gitconfig")}\n${fn("namespace_probe")}\nnamespace_probe`,
    ],
    { env }
  );
  return { code: result.exitCode, stdout: result.stdout.toString().trim() };
}
/** Why this host refuses the namespace every run's agent starts in, by the probe's own words, or
 * undefined where it makes one: read once, so one warning names the refusal for the whole file,
 * as the Docker rows' does. Every row that starts an agent in it skips on such a host (a GitHub
 * runner answers `unshare: write failed /proc/self/uid_map: Operation not permitted`); every
 * other row runs everywhere. */
const namespaceRefusal = (() => {
  const probe = namespaceProbe({ PATH: process.env.PATH ?? "" });
  if (probe.code === 0) return undefined;
  console.warn(
    `Skipped the namespace rows in ${import.meta.path}. This host refuses the namespace every run's agent starts in (rig.sh's namespace_probe): ${probe.stdout}`
  );
  return probe.stdout;
})();
const withoutNamespace = namespaceRefusal !== undefined;
/** The login every answer of a world names a user by (rig.sh's `world_login`). */
const WORLD_LOGIN = assignment("world_login").replace(/^world_login='(.*)'$/, "$1");
/** rig.sh's values naming the two GitHub Apps a world's roles commit as (role_identity), which
 * every builder that runs a world's commits or a worker's boot sets. */
const WORLD_APPS = [
  "world_implement_app",
  "world_implement_app_id",
  "world_review_app",
  "world_review_app_id",
];
/**
 * The directory this file's work directory goes in: memoryRoot, so /dev/shm where the machine has a
 * tmpfs there, unless its real path holds a character other than letters, digits and `/._-`, or is
 * in the developer's home, and then /tmp. A plain path, because the model route installers and
 * judge-omp write the path of each key command, and of the key file, into a profile as one plain
 * `!command` word, and refuse any other character in it (install-model-gateway.sh and
 * install-key-file-model-route.sh at `--dest`, rig.sh's judge_keys at the key file): under a TMPDIR
 * with a space or a quote in its name, every row that installs a route failed on that refusal. Not
 * in the developer's home, because childCwd is in this directory (memoryRoot gives TMPDIR where
 * /dev/shm does not suit, and a TMPDIR can be in the home). A tmpfs, because the file's afterAll
 * removes the whole directory under the file's 60 s: on the disk TMPDIR names, that removal of
 * about 1,600 entries took 70 to 90 s at a five-minute load of about 2,200, and failed the file
 * with "a beforeEach/afterEach hook timed out" after every row had passed. `need` is twice what
 * this file held there at its largest, at its last row: 147,079 KiB in 39,643 entries (measured
 * 2026-11-25, the whole file on a pod's disk: 366 rows passed, 9 skipped for want of docker, and
 * its removal passed). The earlier 32 MiB, sixteen times the 1.8 MiB the file held then, took a
 * pod's /dev/shm of 64 MiB, which this file filled: from the describe "the issue workspace's
 * remote" on, 90 rows failed on `ENOSPC`. Where no tmpfs has that room, the directory is the
 * disk's.
 */
const memory = memoryRoot({ need: 2 * 147_079 * 1024 });
const memoryPath = realpathSync(memory);
// A HOME that does not exist holds no config, and realpathSync would throw on it.
const developerHome = existsSync(homedir()) ? realpathSync(homedir()) : path.resolve(homedir());
/** Whether DIR, a real path, is the developer's home or a directory in it. Every path is in a
 * home of `/`. */
function inDeveloperHome(dir: string): boolean {
  const relative = path.relative(developerHome, dir);
  return !(relative === ".." || relative.startsWith(`..${path.sep}`) || path.isAbsolute(relative));
}
const workRoot =
  /^[A-Za-z0-9/._-]+$/.test(memoryPath) && !inDeveloperHome(memoryPath) ? memory : "/tmp";
if (workRoot !== memory) {
  console.error(
    `rig.test.ts: ${memory} holds a character a plain \`!command\` word cannot, or is in ${developerHome}, so the work directory is under /tmp`
  );
}
// A name of no measurement: every path a run's agent reads is built on it, and rig.sh's launch
// starts no agent whose world names the rig (run-names.ts's guard).
const work = temporaryDirectory("suite", { root: workRoot });
const operatorHome = path.join(work, "operator-home");
/** The cwd of each child process that spawnChecked starts here. The test runner's own cwd is in
 * the checkout, and every checkout on a devbox is in the developer's home. mise reads a config
 * file in the cwd and in each directory above it. Under a HOME that is not the developer's, it
 * reads the developer's ~/.config/mise/config.toml found there as the config of a project, not as
 * its global config. That file holds [settings], [env] and [tool_alias], and mise loads each of
 * them only from a trusted file. Under that HOME no record marks the file trusted, so every mise
 * shim exits 1 there, python3 included ("Config files in /home/<user>/.config/mise/config.toml
 * are not trusted", in services_up's row). The work directory is not in the developer's home
 * (workRoot), so no directory above this one is. */
const childCwd = path.join(work, "child-cwd");
// The home the probes must not reach, with the one thing they look for in it: a judge that could
// read it would list this directory. It and childCwd are staged in a hook, below the one the
// reservation registered: a run that selects no row of this file makes none of them. Each level
// is made on its own, never with `recursive`, so this hook makes nothing above the reserved root
// and a root the fixture did not make is an error here rather than a directory nothing removes.
beforeAll(() => {
  mkdirSync(operatorHome);
  mkdirSync(path.join(operatorHome, ".ssh"));
  writeFileSync(path.join(operatorHome, ".ssh", "id_ed25519"), "not a key\n", { mode: 0o600 });
  mkdirSync(childCwd);
});
/** The variables every child process of this file gets: the caller's PATH, a HOME of the test's
 * own, and the caller's mise data and config directories and PUBLIC_LEAK_CHECK when the caller
 * names them. A tool on PATH can be a mise shim (jq and jj are, on a devbox). Under a fresh HOME
 * such a shim finds neither its installs nor its global config, so it fails. make_omp_home keeps
 * the operator's two directories for the processes it starts for the same reason, and base_env
 * passes MISE_DATA_DIR to every agent. A caller that names none of them gets none: the
 * developer's own home decides nothing here. */
const miseData =
  process.env.MISE_DATA_DIR ??
  (process.env.XDG_DATA_HOME === undefined
    ? undefined
    : path.join(process.env.XDG_DATA_HOME, "mise"));
const childEnv = {
  PATH: process.env.PATH ?? "",
  HOME: operatorHome,
  ...(miseData === undefined ? {} : { MISE_DATA_DIR: miseData }),
  ...(process.env.XDG_CONFIG_HOME === undefined
    ? {}
    : { XDG_CONFIG_HOME: process.env.XDG_CONFIG_HOME }),
  // On a Legion pod the `git` on PATH is the leak-check wrapper (/opt/legion/bin/git), which
  // refuses every push whose shared check it cannot find: through PUBLIC_LEAK_CHECK, then
  // public-leak-check.sh on PATH, then the pointer file under the configuration home, which the
  // fresh HOME above does not hold. A pod carries the variable, so a childEnv without it had every
  // push a world's build makes to its own remote.git refused on a pod with no pointer file (40
  // rows, tester 2026-11-15). leak-check-env.ts gives it here as it gives it to every child
  // environment the rig builds by hand, and rig.sh's push_guard to an agent's pane by the same
  // rule; the "childEnv" rows below hold it to that.
  ...leakCheckEnv(),
};
/** `spawnChecked` for this file: a child that gets the test's HOME (`operatorHome`, which every
 * `childEnv` carries) starts in childCwd, outside the developer's home, unless its row names a
 * `cwd` of its own. Every synchronous child of this file that gets that HOME starts through it; the
 * one asynchronous child that does (`greeting`) names childCwd itself. A child that gets no
 * environment runs under the caller's own HOME (the key command and the libpq probe, among others),
 * where mise reads the caller's config as its global config; a row that sets another HOME names its
 * own `cwd`. */
function spawnChecked<
  const In extends SpawnOptions.Writable = "ignore",
  const Out extends SpawnOptions.Readable = "pipe",
  const Err extends SpawnOptions.Readable = "pipe",
>(cmds: string[], options?: SpawnOptions.SpawnSyncOptions<In, Out, Err>): SyncSubprocess<Out, Err> {
  return spawnCheckedAnywhere(
    cmds,
    options?.env?.HOME === operatorHome ? { ...options, cwd: options.cwd ?? childCwd } : options
  );
}

let profiles = 0;
/** A label's profile directory with an empty Oh My Pi home, as make_omp_home leaves it. */
function profileDir(): string {
  const dir = path.join(work, `profile-${++profiles}`);
  mkdirSync(path.join(dir, "home", ".omp"), { recursive: true });
  return dir;
}
function keyFile(mode: number): string {
  const file = path.join(work, `key-${profiles}-${mode.toString(8)}`);
  writeFileSync(file, "sk-ant-rig-test-0000000000000000\n", { mode });
  return file;
}
function route(dir: string, env: Record<string, string>, model = "") {
  const result = spawnChecked(
    [
      "bash",
      "-c",
      `route_model_id=claude-opus-4-8\n${fn("install_model_route")}\n${fn("install_provider_route")}\ninstall_model_route "$1" skills-test "$2" "$3"`,
      "-",
      root,
      dir,
      model,
    ],
    { env: { ...childEnv, ...env } }
  );
  return { code: result.exitCode, stderr: result.stderr.toString() };
}

describe("install_model_route", () => {
  test("key-file writes the profile's route, its key command under model-key and its record under key", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "key-file",
      LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
    });
    expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
    const models = readFileSync(
      path.join(dir, "home/.omp/profiles/skills-test/agent/models.yml"),
      "utf8"
    );
    expect(models).toContain(`!exec ${path.join(dir, "model-key", "anthropic-key")}`);
    expect(models).toContain("https://api.anthropic.com");
    const served = spawnChecked([path.join(dir, "model-key", "anthropic-key")]);
    expect(served.stdout.toString().trim()).toBe("sk-ant-rig-test-0000000000000000");
    expect(existsSync(path.join(dir, "key", "anthropic-key.log"))).toBe(true);
  });

  test("key-file without LEGION_E2E_ANTHROPIC_KEY_FILE fails, naming the variable", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, { LEGION_E2E_MODEL_ROUTE: "key-file" });
    expect(code).toBe(1);
    expect(stderr).toContain("LEGION_E2E_ANTHROPIC_KEY_FILE is unset");
    expect(existsSync(path.join(dir, "home/.omp/profiles/skills-test/agent/models.yml"))).toBe(
      false
    );
  });

  test("a key file others can read is refused, and no key command is left behind", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "key-file",
      LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o644),
    });
    expect(code).not.toBe(0);
    expect(stderr).toContain("group or other can read it");
    expect(existsSync(path.join(dir, "model-key"))).toBe(false);
  });

  /** An operator's models.yml holding the gateway block the provider route names, and another. */
  function providerFile(apiKey: string): string {
    const file = path.join(work, `models-${profiles}.yml`);
    writeFileSync(
      file,
      [
        "providers:",
        "  openai:",
        '    apiKey: "!secrets get --value OPENAI_KEY"',
        "  box-gateway:",
        "    baseUrl: http://gateway.internal.example:4000",
        `    apiKey: "${apiKey}"`,
        "    api: anthropic-messages",
        "    authHeader: true",
        "    models:",
        "      - id: claude-opus-5-5",
        "        thinking:",
        "          mode: anthropic-adaptive",
        "          efforts: [low, high]",
        "",
      ].join("\n")
    );
    return file;
  }

  test("provider copies the named block, its !cat key command as written, with the rig's one model", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: providerFile("!cat /run/user/1000/box/gateway-key"),
      LEGION_E2E_MODEL_PROVIDER: "box-gateway",
    });
    expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
    const agent = path.join(dir, "home/.omp/profiles/skills-test/agent");
    expect(JSON.parse(readFileSync(path.join(agent, "models.yml"), "utf8"))).toEqual({
      providers: {
        "box-gateway": {
          baseUrl: "http://gateway.internal.example:4000",
          apiKey: "!cat /run/user/1000/box/gateway-key",
          api: "anthropic-messages",
          authHeader: true,
          models: [{ id: "claude-opus-4-8" }],
        },
      },
    });
    const config = readFileSync(path.join(agent, "config.yml"), "utf8");
    expect(config).toContain("enabledModels:\n  - box-gateway/claude-opus-4-8\n");
    expect(config).toContain("  - anthropic\n");
    expect(config).toContain("  default: box-gateway/claude-opus-4-8\n");
    expect(config).toContain("  task: box-gateway/claude-opus-4-8\n");
    expect(existsSync(path.join(dir, "model-key"))).toBe(false);
  });

  test("provider with a model the block lists takes the block's own entry for it", () => {
    const dir = profileDir();
    const { code, stderr } = route(
      dir,
      {
        LEGION_E2E_MODEL_ROUTE: "provider",
        LEGION_E2E_MODEL_PROVIDER_FILE: providerFile("!cat /run/user/1000/box/gateway-key"),
        LEGION_E2E_MODEL_PROVIDER: "box-gateway",
      },
      "claude-opus-5-5"
    );
    expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
    const agent = path.join(dir, "home/.omp/profiles/skills-test/agent");
    expect(
      JSON.parse(readFileSync(path.join(agent, "models.yml"), "utf8")).providers["box-gateway"]
        .models
    ).toEqual([
      { id: "claude-opus-5-5", thinking: { mode: "anthropic-adaptive", efforts: ["low", "high"] } },
    ]);
    expect(readFileSync(path.join(agent, "config.yml"), "utf8")).toContain(
      "  default: box-gateway/claude-opus-5-5\n"
    );
  });

  // Catches: a profile whose one model is disabled because the operator's block carries a bundled
  // provider's name, which Oh My Pi refuses to start on ("No model available matching
  // enabledModels (anthropic/claude-opus-4-8) with usable credentials").
  test("provider leaves its own name out of the disabled providers", () => {
    const dir = profileDir();
    // The operator's own block, under a bundled provider's name: the deployment's gateway block
    // is often `anthropic`, since that is the API it speaks.
    const file = providerFile("!cat /run/user/1000/box/gateway-key");
    writeFileSync(
      file,
      `${readFileSync(file, "utf8")}  anthropic:
    baseUrl: http://gateway.internal.example:4000
    apiKey: "!cat /run/user/1000/box/gateway-key"
    api: anthropic-messages
`
    );
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: file,
      LEGION_E2E_MODEL_PROVIDER: "anthropic",
    });
    expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
    const agent = path.join(dir, "home/.omp/profiles/skills-test/agent");
    const config = readFileSync(path.join(agent, "config.yml"), "utf8");
    expect(config).toContain("enabledModels:\n  - anthropic/claude-opus-4-8\n");
    expect(config).toContain("disabledProviders:\n  - amazon-bedrock\n");
    expect(config).not.toContain("  - anthropic\n");
  });

  // An Oh My Pi config that disables a provider by name disables a models.yml block of that name
  // too, so a box whose gateway block is named `anthropic` would leave the agents no model.
  test("provider whose block is named after a built-in provider leaves that provider enabled", () => {
    const dir = profileDir();
    const file = path.join(work, `models-${profiles}-anthropic.yml`);
    writeFileSync(
      file,
      [
        "providers:",
        "  anthropic:",
        "    baseUrl: http://gateway.internal.example:4000",
        '    apiKey: "!cat /run/user/1000/box/gateway-key"',
        "    api: anthropic-messages",
        "",
      ].join("\n")
    );
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: file,
      LEGION_E2E_MODEL_PROVIDER: "anthropic",
    });
    expect({ code, stderr: code === 0 ? "" : stderr }).toEqual({ code: 0, stderr: "" });
    const config = readFileSync(
      path.join(dir, "home/.omp/profiles/skills-test/agent/config.yml"),
      "utf8"
    );
    expect(config).toContain("enabledModels:\n  - anthropic/claude-opus-4-8\n");
    expect(config).toContain("disabledProviders:\n  - amazon-bedrock\n");
    expect(config).not.toContain("  - anthropic\n");
  });

  test("provider without LEGION_E2E_MODEL_PROVIDER_FILE fails, naming the variable", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER: "box-gateway",
    });
    expect(code).toBe(1);
    expect(stderr).toContain("LEGION_E2E_MODEL_PROVIDER_FILE is unset");
  });

  test("provider refuses a block absent from the file, and one whose key is not read by !cat", () => {
    const missing = route(profileDir(), {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: providerFile("!cat /run/user/1000/box/gateway-key"),
      LEGION_E2E_MODEL_PROVIDER: "elsewhere",
    });
    expect(missing.code).toBe(1);
    expect(missing.stderr).toContain("has no provider block elsewhere");
    const literal = profileDir();
    const inline = route(literal, {
      LEGION_E2E_MODEL_ROUTE: "provider",
      LEGION_E2E_MODEL_PROVIDER_FILE: providerFile("sk-literal-key"),
      LEGION_E2E_MODEL_PROVIDER: "box-gateway",
    });
    expect(inline.code).toBe(1);
    expect(inline.stderr).toContain('does not read its key as "!cat <absolute file>"');
    expect(existsSync(path.join(literal, "home/.omp/profiles/skills-test/agent/models.yml"))).toBe(
      false
    );
  });

  test("an unknown route fails, naming the three it takes", () => {
    const dir = profileDir();
    const { code, stderr } = route(dir, { LEGION_E2E_MODEL_ROUTE: "bedrock" });
    expect(code).toBe(1);
    expect(stderr).toContain("it is gateway, key-file or provider");
  });
});

// The key-file route's key command reads the operator's key for as long as it exists
// (install-key-file-model-route.sh: the caller removes --key-dir on every exit that keeps no
// profile). A setup that stops after the route is installed leaves no profile to use it.
describe("a setup that fails after install_model_route", () => {
  /** A checkout whose plugin installer and home maker do nothing, with the real key-file route. */
  function checkout(): string {
    const co = freshDir("route-checkout");
    mkdirSync(path.join(co, "scripts/e2e/lib"), { recursive: true });
    mkdirSync(path.join(co, "skills/asd-ste100"), { recursive: true });
    writeFileSync(path.join(co, "skills/asd-ste100/SKILL.md"), "# STE\n");
    writeFileSync(path.join(co, ".omp-pin"), "omp@pin\n");
    // The real key-file route, with the libs it sources: model-endpoint.sh for its base URL's
    // check, and omp-home.sh for the line that shuts the setup wizard, whose make_omp_home is then
    // replaced by one that only makes the home's .omp.
    for (const file of ["install-key-file-model-route.sh", "model-endpoint.sh", "omp-home.sh"]) {
      cpSync(path.join(root, "scripts/e2e/lib", file), path.join(co, "scripts/e2e/lib", file));
    }
    appendFileSync(
      path.join(co, "scripts/e2e/lib/omp-home.sh"),
      'make_omp_home() { mkdir -p "$1/.omp"; }\n'
    );
    writeFileSync(path.join(co, "scripts/e2e/lib/install-plugin-profile.sh"), "exit 0\n");
    return co;
  }
  const routeEnv = () => ({
    LEGION_E2E_MODEL_ROUTE: "key-file",
    LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
  });

  // Catches: a label profile that fails after its route is installed (here the superpowers probe)
  // and keeps a key command that still reads the operator's key.
  test("cmd_profile removes the label's model-key and marks nothing ready", () => {
    const work = freshDir("route-work");
    const script = [
      "set -euo pipefail",
      ...[
        "fail",
        "label_dir",
        "on_exit",
        "drop_unready_keys",
        "install_model_route",
        "cmd_profile",
      ].map(fn),
      "checkout_commit() { printf 'fixture parents=base\\n'; }",
      "rig_mise() { :; }",
      "install_superpowers() { fail 'the superpowers probe failed'; }",
      'work=$1; cmd_profile probe "$2"',
    ].join("\n");
    const failed = bash(script, [work, checkout()], {
      ...routeEnv(),
      SKILL_SCENARIOS_SUPERPOWERS: "set",
    });
    expect(failed.code).toBe(1);
    expect(failed.stderr).toContain("the superpowers probe failed");
    const P = path.join(work, "profiles/probe");
    // The profile is installed under an id of no meaning (P/install), with production's own
    // profile name, so no path a run's agent reads names the label.
    const id = readFileSync(path.join(P, "install"), "utf8").trim();
    expect(id).toMatch(/^[0-9a-f]{12}$/);
    expect(readFileSync(path.join(P, "profile"), "utf8")).toBe("legion\n");
    const I = path.join(work, "installs", id);
    expect(existsSync(path.join(I, "home/.omp/profiles/legion/agent/models.yml"))).toBe(true);
    expect(existsSync(path.join(I, "model-key"))).toBe(false);
    expect(existsSync(path.join(P, "ready"))).toBe(false);

    // The same setup that succeeds keeps its key command: a ready profile uses it.
    const kept = freshDir("route-work");
    const ok = bash(
      script.replace("fail 'the superpowers probe failed'", ":"),
      [kept, checkout()],
      {
        ...routeEnv(),
        SKILL_SCENARIOS_SUPERPOWERS: "set",
      }
    );
    expect({ code: ok.code, stderr: ok.stderr }).toEqual({ code: 0, stderr: "" });
    const installed = readFileSync(path.join(kept, "profiles/probe/install"), "utf8").trim();
    expect(existsSync(path.join(kept, "installs", installed, "model-key/anthropic-key"))).toBe(
      true
    );
    expect(existsSync(path.join(kept, "profiles/probe/ready"))).toBe(true);
  });

  /** judge_profiles and its helpers, called on the checkout $1 and the work directory $2. A row
   * puts an install_model_route definition before it. */
  const judgeProfiles = [
    "set -euo pipefail",
    ...["fail", "on_exit", "drop_unready_keys", "judge_profiles"].map(fn),
    "checkout_commit() { printf 'fixture parents=base\\n'; }",
    "root=$1; work=$2; judge_model=fixture; judge_thinking=high; judge_profiles",
  ].join("\n");

  // Catches: a judge-profile build that fails at its second profile and keeps the first one's key
  // command.
  test("judge_profiles removes both profiles' model-key and marks nothing ready", () => {
    const work = freshDir("judge-route-work");
    // The bare profile's route cannot be installed, after the ste profile's is.
    const script = [
      fn("install_model_route").replace("install_model_route()", "installed_route()"),
      'install_model_route() { [ "$2" != judge-bare ] || { echo "no route for bare" >&2; return 1; }; installed_route "$@"; }',
      judgeProfiles,
    ].join("\n");
    const failed = bash(script, [checkout(), work], routeEnv());
    expect(failed.code).toBe(1);
    expect(failed.stderr).toContain("the judge's model route (key-file) could not be installed");
    const dir = path.join(work, "judge-profiles");
    expect(readFileSync(path.join(dir, "bare/install.log"), "utf8")).toBe("no route for bare\n");
    expect(existsSync(path.join(dir, "ste/home/.omp/profiles/judge-ste/agent/models.yml"))).toBe(
      true
    );
    expect(existsSync(path.join(dir, "ste/model-key"))).toBe(false);
    expect(existsSync(path.join(dir, "bare/model-key"))).toBe(false);
    expect(existsSync(path.join(dir, "ready"))).toBe(false);
  });

  // Catches: a judge-profile build that removes the unready build an earlier call left (its
  // install.log says why that call failed) and builds over it.
  test("judge_profiles refuses an unready build an earlier call left, and keeps it", () => {
    const work = freshDir("judge-route-unready");
    const co = checkout();
    // The bare profile's route cannot be installed, after the ste profile's is, as the row above
    // fails it: main's judge_profiles makes no home with make_omp_home (#5010).
    const script = [
      fn("install_model_route").replace("install_model_route()", "installed_route()"),
      'install_model_route() { [ "$2" != judge-bare ] || { echo "no route for bare" >&2; return 1; }; installed_route "$@"; }',
      judgeProfiles,
    ].join("\n");
    expect(bash(script, [co, work], routeEnv()).code).toBe(1);
    const dir = path.join(work, "judge-profiles");
    writeFileSync(path.join(dir, "ste", "operator-notes"), "read before the rebuild\n");
    const before = treeDigest(dir);
    const again = bash(script, [co, work], routeEnv());
    expect(again.code).toBe(1);
    expect(again.stderr).toContain(
      `judge-omp: ${dir} holds a judge-profile build an earlier call left unready (no ${dir}/ready)`
    );
    expect(treeDigest(dir)).toBe(before);
  });

  // Catches: a judge profile that needs jj. No Oh My Pi and no jj runs under a judge's HOME on this
  // host, yet a home made by make_omp_home read the operator's jj config, so judge-omp failed on a
  // machine with no jj on PATH ("make_omp_home failed for the judge's ste profile"). Here the
  // checkout's omp-home.sh is the real one, and the only jj on PATH records its call and fails.
  test("judge_profiles builds both profiles and runs no jj", () => {
    const work = freshDir("judge-nojj-work");
    const co = checkout();
    cpSync(
      path.join(root, "scripts/e2e/lib/omp-home.sh"),
      path.join(co, "scripts/e2e/lib/omp-home.sh")
    );
    const bin = freshDir("judge-nojj-bin");
    const calls = path.join(bin, "jj-calls");
    writeFileSync(
      path.join(bin, "jj"),
      `#!/bin/sh\necho "$*" >>'${calls}'\necho "jj: not on this machine" >&2\nexit 127\n`,
      { mode: 0o755 }
    );
    const built = bash(`${fn("install_model_route")}\n${judgeProfiles}`, [co, work], {
      ...routeEnv(),
      PATH: `${bin}:${childEnv.PATH}`,
    });
    expect({ code: built.code, stderr: built.stderr }).toEqual({ code: 0, stderr: "" });
    expect(existsSync(calls)).toBe(false);
    const dir = path.join(work, "judge-profiles");
    for (const kind of ["ste", "bare"]) {
      expect(
        existsSync(path.join(dir, `${kind}/home/.omp/profiles/judge-${kind}/agent/models.yml`))
      ).toBe(true);
    }
    expect(
      existsSync(
        path.join(dir, "ste/home/.omp/profiles/judge-ste/agent/skills/asd-ste100/SKILL.md")
      )
    ).toBe(true);
    expect(existsSync(path.join(dir, "ready"))).toBe(true);
  });
});

// rig.sh's rig_mise: the mise every `mise` of the rig runs, the profile installer's and each agent's
// `mise x <pin>` among them. A worker pod's image carries no mise (its `tools` stage alone installs
// one), so the rig fetches its own at that stage's pin, the Dockerfile's ARG MISE_VERSION and ARG
// MISE_SHA256, checks it, installs it under the work directory and puts it first on PATH. GitHub's
// release downloads are a `curl` stand-in first on PATH (curlStandIn), as the judge rows stand a
// `hawk-token` in for the gateway, and a release is an archive laid out as jdx/mise's are
// (miseRelease).
/** The worker image's Dockerfile, below a checkout. */
const DOCKERFILE = "packages/daemon/docker/worker.Dockerfile";
/** This checkout's Dockerfile, the one rig_mise reads under the rig's root. */
const dockerfileText = readFileSync(path.join(root, DOCKERFILE), "utf8");
/** The value of the Dockerfile text's `ARG <name>=<value>` line. */
function dockerfileArg(text: string, name: string): string {
  const found = new RegExp(`^ARG ${name}=(.*)$`, "m").exec(text);
  if (found?.[1] === undefined) throw new Error(`the Dockerfile has no ARG ${name}=`);
  return found[1];
}
/** The linux-x64 archive of mise `version` that the worker image's tools stage fetches. */
const miseUrl = (version: string) =>
  `https://github.com/jdx/mise/releases/download/${version}/mise-${version}-linux-x64.tar.gz`;
const fileSha256 = (file: string) =>
  new Bun.CryptoHasher("sha256").update(readFileSync(file)).digest("hex");
/** A release archive of mise `version`, laid out as jdx/mise's (mise/bin/mise), whose mise stands
 * in for the real one: `--version` prints `<version> linux-x64 (stand-in)`, `where <tool>` prints
 * /pinned/<tool>, and `x <tool> -- <command>...` runs the command with MISE_RAN naming this mise. */
function miseRelease(version: string): string {
  const dir = freshDir("mise-release");
  mkdirSync(path.join(dir, "mise", "bin"), { recursive: true });
  writeFileSync(
    path.join(dir, "mise", "bin", "mise"),
    [
      "#!/bin/sh",
      "case $1 in",
      `--version) echo "${version.replace(/^v/, "")} linux-x64 (stand-in)" ;;`,
      'where) echo "/pinned/$2" ;;',
      'x) shift 3; MISE_RAN=$0; export MISE_RAN; exec "$@" ;;',
      "esac",
      "",
    ].join("\n"),
    { mode: 0o755 }
  );
  const archive = path.join(dir, "release.tar.gz");
  const packed = spawnChecked(["tar", "-czf", archive, "-C", dir, "mise"]);
  if (packed.exitCode !== 0) throw new Error(`tar could not pack ${dir}: ${packed.stderr}`);
  return archive;
}
/** A directory holding a `curl` that stands in for GitHub's release downloads: it appends the URL
 * it is asked for to $CURL_LOG and copies $CURL_SERVES to the file its -o names, or, with
 * CURL_SERVES unset, fails as `curl -f` fails on a 404. */
function curlStandIn(): string {
  const bin = freshDir("mise-curl");
  writeFileSync(
    path.join(bin, "curl"),
    [
      "#!/usr/bin/env bash",
      "out='' url=''",
      "while [ $# -gt 0 ]; do case $1 in -o) out=$2; shift 2 ;; -*) shift ;; *) url=$1; shift ;; esac; done",
      'printf \'%s\\n\' "$url" >>"$CURL_LOG"',
      'if [ -z "$CURL_SERVES" ]; then echo "curl: (22) The requested URL returned error: 404" >&2; exit 22; fi',
      'cp "$CURL_SERVES" "$out"',
      "",
    ].join("\n"),
    { mode: 0o755 }
  );
  return bin;
}
/** A rig root holding this checkout's Dockerfile with its two mise ARGs rewritten to `version`
 * and `sha`: all rig_mise reads of the rig's root. */
function miseRoot(version: string, sha: string): string {
  const top = freshDir("mise-root");
  const text = dockerfileText
    .replace(/^ARG MISE_VERSION=.*$/m, `ARG MISE_VERSION=${version}`)
    .replace(/^ARG MISE_SHA256=.*$/m, `ARG MISE_SHA256=${sha}`);
  expect([dockerfileArg(text, "MISE_VERSION"), dockerfileArg(text, "MISE_SHA256")]).toEqual([
    version,
    sha,
  ]);
  mkdirSync(path.join(top, path.dirname(DOCKERFILE)), { recursive: true });
  writeFileSync(path.join(top, DOCKERFILE), text);
  return top;
}
/** rig.sh's rig_mise with `top` as the rig's root and `work` as its work directory, then the
 * `mise` the PATH it leaves resolves to, as stdout's last line. */
function rigMise(top: string, work: string, env: Record<string, string>) {
  return bash(
    [
      "set -euo pipefail",
      ...["fail", "note", "rig_mise"].map(fn),
      "root=$1 work=$2",
      assignment("mise_dir"),
      "rig_mise",
      "command -v mise",
    ].join("\n"),
    [top, work],
    env
  );
}
/** The lines of FILE, none where it is absent. */
const fileLines = (file: string) =>
  existsSync(file) ? readFileSync(file, "utf8").split("\n").filter(Boolean) : [];

describe("the rig's own mise", () => {
  /** A label's checkout whose plugin installer records the `mise` its PATH resolves to and runs
   * it, where install-plugin-profile.sh runs `mise where`, `mise install` and `mise x`, with the
   * real key-file model route, the libs it sources, and a home maker that makes an empty home. */
  function labelCheckout(): string {
    const co = freshDir("mise-checkout");
    mkdirSync(path.join(co, "scripts/e2e/lib"), { recursive: true });
    mkdirSync(path.join(co, "skills/asd-ste100"), { recursive: true });
    writeFileSync(path.join(co, "skills/asd-ste100/SKILL.md"), "# STE\n");
    writeFileSync(path.join(co, ".omp-pin"), "omp@pin\n");
    for (const file of ["install-key-file-model-route.sh", "model-endpoint.sh", "omp-home.sh"]) {
      cpSync(path.join(root, "scripts/e2e/lib", file), path.join(co, "scripts/e2e/lib", file));
    }
    appendFileSync(
      path.join(co, "scripts/e2e/lib/omp-home.sh"),
      'make_omp_home() { mkdir -p "$1/.omp"; }\n'
    );
    writeFileSync(
      path.join(co, "scripts/e2e/lib/install-plugin-profile.sh"),
      [
        "dest=''",
        "while [ $# -gt 0 ]; do case $1 in --dest) dest=$2; shift 2 ;; *) shift ;; esac; done",
        'mkdir -p "$dest"',
        'command -v mise >"$dest/mise-path"',
        'mise --version >"$dest/mise-version"',
        "",
      ].join("\n")
    );
    return co;
  }
  /** cmd_profile of `label` from checkout `co`, with `top` as the rig's root and `work` as its
   * work directory. */
  const profile = (
    top: string,
    work: string,
    label: string,
    co: string,
    env: Record<string, string>
  ) =>
    bash(
      [
        "set -euo pipefail",
        ...[
          "fail",
          "note",
          "label_dir",
          "on_exit",
          "drop_unready_keys",
          "install_model_route",
          "rig_mise",
          "cmd_profile",
        ].map(fn),
        "checkout_commit() { printf 'fixture parents=base\\n'; }",
        "root=$1 work=$2",
        assignment("mise_dir"),
        'cmd_profile "$3" "$4"',
      ].join("\n"),
      [top, work, label, co],
      {
        LEGION_E2E_MODEL_ROUTE: "key-file",
        LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
        ...env,
      }
    );
  /** The caller's variables for a row whose GitHub is `curl`'s stand-in, serving `serves`. */
  const served = (curl: string, serves?: string): Record<string, string> => ({
    PATH: `${curl}:${childEnv.PATH}`,
    CURL_LOG: path.join(curl, "urls"),
    ...(serves === undefined ? {} : { CURL_SERVES: serves }),
  });

  // Catches: a profile build on a pod with no mise (install-plugin-profile.sh's `mise: command not
  // found`), one that runs a mise of the caller's PATH, and one that fetches a mise other than the
  // one the copied Dockerfile's two ARGs pin.
  test("a profile build fetches mise at its Dockerfile's two ARGs, checks it, and runs the plugin installer on it", () => {
    const version = "v2099.1.2";
    const archive = miseRelease(version);
    const sha = fileSha256(archive);
    const top = miseRoot(version, sha);
    const work = freshDir("mise-work");
    const curl = curlStandIn();
    const built = profile(top, work, "head", labelCheckout(), served(curl, archive));
    expect({ code: built.code, stderr: built.stderr }).toEqual({ code: 0, stderr: "" });
    const mise = path.join(work, "tools", "bin", "mise");
    expect(fileLines(path.join(curl, "urls"))).toEqual([miseUrl(version)]);
    expect(built.stdout.split("\n")).toContain(
      `skill-scenarios: mise: fetched ${miseUrl(version)}, SHA-256 ${sha} as ARG MISE_SHA256 of ${top}/${DOCKERFILE} expects; installed ${mise}, which prints 2099.1.2 linux-x64 (stand-in)`
    );
    // The archive's own mise, alone in the directory every pane's PATH names.
    expect(readdirSync(path.dirname(mise))).toEqual(["mise"]);
    expect(readFileSync(mise)).toEqual(
      readFileSync(path.join(path.dirname(archive), "mise", "bin", "mise"))
    );
    // Both plugin installs ran it, the first mise on their PATH.
    const id = readFileSync(path.join(work, "profiles", "head", "install"), "utf8").trim();
    for (const plugin of ["pi-envoy", "pi-legion"]) {
      const dest = path.join(work, "installs", id, plugin);
      expect(readFileSync(path.join(dest, "mise-path"), "utf8")).toBe(`${mise}\n`);
      expect(readFileSync(path.join(dest, "mise-version"), "utf8")).toBe(
        "2099.1.2 linux-x64 (stand-in)\n"
      );
    }
    expect(existsSync(path.join(work, "profiles", "head", "ready"))).toBe(true);
    // Nothing of the fetch is left in the work directory.
    expect(readdirSync(work).filter((name) => name.startsWith(".mise"))).toEqual([]);
  });

  // Catches: a profile build that installs, or runs, a mise whose bytes are not the ones the
  // worker image pins, and a refusal that leaves a half-built label behind.
  test("a profile build whose fetched mise is not the Dockerfile's SHA-256 is refused, naming the check, and installs nothing", () => {
    const version = dockerfileArg(dockerfileText, "MISE_VERSION");
    const sha = dockerfileArg(dockerfileText, "MISE_SHA256");
    const archive = miseRelease("v0.0.1");
    const work = freshDir("mise-work");
    const curl = curlStandIn();
    const refused = profile(root, work, "head", labelCheckout(), served(curl, archive));
    expect({ code: refused.code, stderr: refused.stderr }).toEqual({
      code: 1,
      stderr: `skill-scenarios: mise check: expected SHA-256 ${sha} (ARG MISE_SHA256 of ${root}/${DOCKERFILE}) of ${miseUrl(version)}, got ${fileSha256(archive)}; nothing was installed\n`,
    });
    expect(fileLines(path.join(curl, "urls"))).toEqual([miseUrl(version)]);
    expect(readdirSync(work)).toEqual([]);
  });

  // Catches: a fetch that fails taken for an archive, and a refusal that leaves the fetch behind.
  test("a mise fetch that fails is refused, naming the fetch and curl's answer, and installs nothing", () => {
    const top = miseRoot("v2099.1.2", "0".repeat(64));
    const work = freshDir("mise-work");
    const curl = curlStandIn();
    const refused = rigMise(top, work, served(curl));
    expect({ code: refused.code, stderr: refused.stderr }).toEqual({
      code: 1,
      stderr: `skill-scenarios: mise fetch: expected the archive of mise v2099.1.2 (ARG MISE_VERSION of ${top}/${DOCKERFILE}) from ${miseUrl("v2099.1.2")}, got curl exit 22: curl: (22) The requested URL returned error: 404; nothing was installed\n`,
    });
    expect(readdirSync(work)).toEqual([]);
  });

  // Catches: a version or SHA-256 typed into rig.sh, which the two drift from as the Dockerfile
  // moves on, and a Dockerfile whose pin cannot be read taken for one.
  test("the mise rig_mise fetches and checks is the one its Dockerfile's two ARGs name", () => {
    const archive = miseRelease("v2099.1.2");
    const sha = fileSha256(archive);
    const curl = curlStandIn();
    const pinned = miseRoot("v2099.1.2", sha);
    const accepted = rigMise(pinned, freshDir("mise-work"), served(curl, archive));
    expect({ code: accepted.code, stderr: accepted.stderr }).toEqual({ code: 0, stderr: "" });
    expect(fileLines(path.join(curl, "urls"))).toEqual([miseUrl("v2099.1.2")]);
    // The same archive, under a Dockerfile that pins another release and another SHA-256.
    const other = miseRoot("v2099.3.4", "e".repeat(64));
    const work = freshDir("mise-work");
    const refused = rigMise(other, work, served(curl, archive));
    expect({ code: refused.code, stderr: refused.stderr }).toEqual({
      code: 1,
      stderr: `skill-scenarios: mise check: expected SHA-256 ${"e".repeat(64)} (ARG MISE_SHA256 of ${other}/${DOCKERFILE}) of ${miseUrl("v2099.3.4")}, got ${sha}; nothing was installed\n`,
    });
    expect(fileLines(path.join(curl, "urls"))).toEqual([
      miseUrl("v2099.1.2"),
      miseUrl("v2099.3.4"),
    ]);
    expect(readdirSync(work)).toEqual([]);
    // A root with no Dockerfile pins nothing, and nothing is fetched.
    const bare = freshDir("mise-root");
    const unpinned = rigMise(bare, freshDir("mise-work"), served(curl, archive));
    expect({ code: unpinned.code, stderr: unpinned.stderr }).toEqual({
      code: 1,
      stderr: `skill-scenarios: mise pin: expected one ARG MISE_VERSION=<release> and one ARG MISE_SHA256=<sha256> in ${bare}/${DOCKERFILE}, got '' and ''; nothing was installed\n`,
    });
    expect(fileLines(path.join(curl, "urls"))).toHaveLength(2);
  });

  // Catches: a rig that fetches mise for every command, and one that keeps a mise whose bytes or
  // pin are no longer the ones its record and its Dockerfile name.
  test("a mise the work directory holds from the Dockerfile's archive is reused, and any other is fetched again", () => {
    const archive = miseRelease("v2099.1.2");
    const top = miseRoot("v2099.1.2", fileSha256(archive));
    const work = freshDir("mise-work");
    const curl = curlStandIn();
    const urls = path.join(curl, "urls");
    const mise = path.join(work, "tools", "bin", "mise");
    const first = rigMise(top, work, served(curl, archive));
    expect(first.code).toBe(0);
    expect(first.stdout.trim().split("\n").at(-1)).toBe(mise);
    const again = rigMise(top, work, served(curl, archive));
    expect({ code: again.code, stdout: again.stdout, stderr: again.stderr }).toEqual({
      code: 0,
      stdout: `${mise}\n`,
      stderr: "",
    });
    expect(fileLines(urls)).toHaveLength(1);
    // A binary whose bytes are not the ones recorded is fetched again and replaced.
    writeFileSync(mise, "#!/bin/sh\necho changed\n", { mode: 0o755 });
    expect(rigMise(top, work, served(curl, archive)).code).toBe(0);
    expect(fileLines(urls)).toHaveLength(2);
    expect(readFileSync(mise)).toEqual(
      readFileSync(path.join(path.dirname(archive), "mise", "bin", "mise"))
    );
    // So is one the Dockerfile no longer pins, once it pins another release.
    const next = miseRelease("v2099.5.6");
    const moved = rigMise(miseRoot("v2099.5.6", fileSha256(next)), work, served(curl, next));
    expect(moved.code).toBe(0);
    expect(fileLines(urls).at(-1)).toBe(miseUrl("v2099.5.6"));
    expect(readFileSync(mise)).toEqual(
      readFileSync(path.join(path.dirname(next), "mise", "bin", "mise"))
    );
  });

  // Catches: an agent's pane that names a mise other than the rig's first, and a world that does
  // not show the rig's mise, so the agent's `mise x` fails in its namespace.
  test("a run's pane names the rig's own mise first after its stand-ins, and its world shows it read-only", () => {
    const archive = miseRelease("v2099.1.2");
    const top = miseRoot("v2099.1.2", fileSha256(archive));
    const work = freshDir("mise-work");
    const R = path.join(work, "runs", "0a1b2c3d4e5f");
    const curl = curlStandIn();
    const result = bash(
      [
        "set -euo pipefail",
        ...[
          "fail",
          "note",
          "rig_mise",
          "push_guard",
          "base_env",
          "pane_var",
          "system_gitconfig",
          "world_paths",
        ].map(fn),
        "root=$1 work=$2 R=$3 I=$4 profile=legion",
        assignment("mise_dir"),
        "rig_mise >/dev/null",
        "base_env",
        'world_paths "$R" "$work" "$I" "$R/pane.env"',
      ].join("\n"),
      [top, work, R, profileDir()],
      { ...served(curl, archive), USER: "rig" }
    );
    expect({ code: result.code, stderr: result.stderr }).toEqual({ code: 0, stderr: "" });
    const tools = path.join(work, "tools", "bin");
    expect(fileLines(path.join(R, "pane.env"))).toContain(
      `PATH=${R}/bin:${tools}:${curl}:${childEnv.PATH}`
    );
    expect(result.stdout.split("\n")).toContain(`ro ${tools}`);
  });

  // Catches: `rig.sh world` reading a run's pin through the caller's mise, which a worker pod has
  // none of, where the run's own agent ran the mise its pane names.
  test("rig.sh world reads a run's pin through the mise its pane's PATH names", () => {
    const archive = miseRelease("v2099.1.2");
    const work = freshDir("mise-world");
    const tools = path.join(work, "tools", "bin");
    mkdirSync(tools, { recursive: true });
    cpSync(path.join(path.dirname(archive), "mise", "bin", "mise"), path.join(tools, "mise"));
    const R = path.join(work, "runs", "0a1b2c3d4e5f");
    mkdirSync(R, { recursive: true });
    writeFileSync(`${R}.json`, JSON.stringify({ run: path.basename(R), ran: R }));
    writeFileSync(
      path.join(R, "pane.env"),
      `HOME=${R}/home\nPATH=${R}/bin:${tools}:/usr/bin:/bin\n`
    );
    mkdirSync(path.join(work, "profiles", "head"), { recursive: true });
    writeFileSync(path.join(work, "profiles", "head", "pin"), "omp@pin\n");
    const shown = spawnChecked(["bash", path.join(import.meta.dir, "rig.sh"), "world", "head", R], {
      env: { ...childEnv, SKILL_SCENARIOS_WORK: work },
    });
    expect({ code: shown.exitCode, stderr: shown.stderr.toString() }).toEqual({
      code: 0,
      stderr: "",
    });
    expect(shown.stdout.toString().split("\n")).toContain("ro /pinned/omp@pin");
  });
});

/** Runs SCRIPT (rig.sh functions and a call of them) under bash with ARGS as $1..., in childEnv
 * with ENV over it: a variable ENV gives as undefined is left out of the child's environment. */
function bash(script: string, args: string[] = [], env: Record<string, string | undefined> = {}) {
  const result = spawnChecked(["bash", "-c", script, "-", ...args], {
    env: { ...childEnv, ...env },
  });
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}
let dirs = 0;
/** A new directory under the test's work directory. */
function freshDir(name: string): string {
  const dir = path.join(work, `${name}-${++dirs}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}
/** A digest of the tree at DIR: each entry's path below DIR, its kind and mode, and a file's bytes
 * or a link's target, walked in byte order, so a tree one entry, byte or mode apart digests apart. */
function treeDigest(dir: string): string {
  const hasher = new Bun.CryptoHasher("sha256");
  const walk = (below: string) => {
    const entry = path.join(dir, below);
    const stat = lstatSync(entry);
    hasher.update(`${below}\0${stat.mode}\0`);
    if (stat.isSymbolicLink()) hasher.update(readlinkSync(entry));
    else if (stat.isFile()) hasher.update(readFileSync(entry));
    else if (stat.isDirectory())
      for (const name of readdirSync(entry).sort()) walk(path.join(below, name));
    hasher.update("\0");
  };
  walk(".");
  return hasher.digest("hex");
}
/** The directory of the run whose record (rig.sh's cmd_run) names it `name` in the runs directory
 * `runs`, or undefined for none: a run's directory is named by an id, and its record beside it
 * says which run it is. */
function namedRun(runs: string, name: string): string | undefined {
  for (const file of existsSync(runs) ? readdirSync(runs) : [])
    if (
      file.endsWith(".json") &&
      JSON.parse(readFileSync(path.join(runs, file), "utf8")).name === name
    )
      return path.join(runs, file.slice(0, -".json".length));
  return undefined;
}

// Catches: a child of this file that starts in the test runner's cwd, or a work directory in the
// developer's home. Either way the developer's home is above the child's cwd, where a mise shim run
// under the test's HOME exits 1 (childCwd). Every child that gets the test's HOME starts in
// childCwd, unless its row names a cwd of its own: through spawnChecked, and the one asynchronous
// child (`greeting`) names it itself. The row checks bash(), which starts through spawnChecked.
// CI's PATH holds no mise shim, so services_up's row passes in CI either way. Without childCwd this
// row fails in CI too, because a runner's checkout is in the runner's home.
test("a child of this file starts in childCwd, outside the developer's home", () => {
  const cwd = realpathSync(childCwd);
  expect(bash("pwd -P")).toEqual({ code: 0, stdout: `${cwd}\n`, stderr: "" });
  expect({ cwd, inDeveloperHome: inDeveloperHome(cwd) }).toEqual({ cwd, inDeveloperHome: false });
});

// cmd_profile records the label's commit in built-from with checkout_commit. A git worktree holds
// no .jj, and jj alone printed an empty commit line for it.
describe("checkout_commit", () => {
  const git = (dir: string, ...args: string[]) => {
    const result = spawnChecked(
      [
        "git",
        "-C",
        dir,
        "-c",
        "user.name=Rig Test",
        "-c",
        "user.email=rig@example.invalid",
        "-c",
        "commit.gpgsign=false",
        ...args,
      ],
      { env: childEnv }
    );
    if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr}`);
    return result.stdout.toString().trim();
  };
  function gitCheckout() {
    const dir = freshDir("git-checkout");
    git(dir, "init", "-q", "-b", "main");
    writeFileSync(path.join(dir, "a.txt"), "a\n");
    git(dir, "add", "-A");
    git(dir, "commit", "-qm", "first");
    const first = git(dir, "rev-parse", "HEAD");
    writeFileSync(path.join(dir, "b.txt"), "b\n");
    git(dir, "add", "-A");
    git(dir, "commit", "-qm", "second");
    return { dir, first, head: git(dir, "rev-parse", "HEAD") };
  }
  const commit = (dir: string) => bash(`${fn("checkout_commit")}\ncheckout_commit "$1"`, [dir]);

  // Catches: a checkout_commit that reads jj alone (an empty line for git), or names no parents.
  test("a clean git checkout prints its HEAD and HEAD's parents", () => {
    const { dir, first, head } = gitCheckout();
    expect(commit(dir)).toEqual({ code: 0, stdout: `${head} parents=${first}\n`, stderr: "" });
  });

  // Catches: a built-from that names a commit the label's files do not match.
  test("a git checkout with a change git status lists is marked dirty", () => {
    const { dir, first, head } = gitCheckout();
    writeFileSync(path.join(dir, "a.txt"), "changed\n");
    expect(commit(dir).stdout).toBe(`${head} parents=${first} dirty\n`);
  });

  // Catches: a checkout_commit that hides a failed read, which cmd_profile would record as an
  // empty commit line.
  test("a directory that is no checkout fails", () => {
    expect(commit(freshDir("no-checkout")).code).not.toBe(0);
  });
});

// cmd_live_read reads every path skill_reads prints. A skill can hold examples/ beside
// references/ (asd-ste100 holds examples/), and a read of SKILL.md and references/ alone left
// examples/ unread.
describe("skill_reads", () => {
  function checkout() {
    const co = freshDir("skills-checkout");
    const skill = path.join(co, "skills", "demo");
    for (const file of [
      "SKILL.md",
      "references/rules.md",
      "examples/before-after.md",
      "examples/data.json",
      "LICENSE",
    ]) {
      mkdirSync(path.dirname(path.join(skill, file)), { recursive: true });
      writeFileSync(path.join(skill, file), `${file}\n`);
    }
    return co;
  }
  const reads = (co: string, skill: string) =>
    bash(`${fn("fail")}\n${fn("skill_reads")}\nskill_reads "$1" "$2"`, [co, skill]);

  // Catches: a list that leaves examples/ out, or reads a file that is not markdown.
  test("SKILL.md first, then every other markdown file in byte order", () => {
    expect(reads(checkout(), "demo")).toEqual({
      code: 0,
      stdout: [
        "skill://demo",
        "skill://demo/examples/before-after.md",
        "skill://demo/references/rules.md",
        "",
      ].join("\n"),
      stderr: "",
    });
  });

  // Catches: a live read of a skill the checkout does not have, which reads nothing and passes.
  test("a skill the checkout lacks fails, naming it", () => {
    const co = checkout();
    const { code, stderr } = reads(co, "absent");
    expect(code).toBe(1);
    expect(stderr).toContain(`${co} has no skill absent`);
  });
});

// Oh My Pi walks a run's cwd's ancestors for a project `.omp`, past the run's own HOME. Under a
// work directory below the operator's HOME, a live read loaded the operator's plugins from
// $HOME/.omp in place of the label's packed ones, and nothing scored that as a failure.
describe("omp_ancestor", () => {
  const ancestor = (dir: string) => bash(`${fn("omp_ancestor")}\nomp_ancestor "$1"`, [dir]);

  // Catches: a check that misses a `.omp` above the work directory, the measured case.
  test("names the nearest ancestor that holds .omp", () => {
    const top = freshDir("omp-above");
    mkdirSync(path.join(top, ".omp"));
    const work = path.join(top, "a", "work");
    expect(ancestor(work)).toEqual({ code: 0, stdout: `${top}/.omp\n`, stderr: "" });
  });

  // Catches: a check that starts at the parent and so misses the work directory's own `.omp`.
  test("the work directory's own .omp counts", () => {
    const work = freshDir("omp-self");
    writeFileSync(path.join(work, ".omp"), "");
    expect(ancestor(work).stdout).toBe(`${work}/.omp\n`);
  });

  // Catches: a check that refuses every work directory.
  test("prints nothing and fails when no ancestor holds .omp", () => {
    expect(ancestor(path.join(freshDir("omp-none"), "work"))).toEqual({
      code: 1,
      stdout: "",
      stderr: "",
    });
  });

  // Catches: a rig.sh that defines the check and never runs it.
  test("rig.sh refuses such a work directory before it makes it, naming the .omp", () => {
    const top = freshDir("omp-rig");
    mkdirSync(path.join(top, ".omp"));
    const work = path.join(top, "work");
    const result = spawnChecked(["bash", path.join(import.meta.dir, "rig.sh"), "score"], {
      env: { ...childEnv, SKILL_SCENARIOS_WORK: work },
    });
    expect(result.exitCode).toBe(1);
    expect(result.stderr.toString()).toContain(`${top}/.omp`);
    expect(existsSync(work)).toBe(false);
  });
});

// `rig.sh score` and `rig.sh health` read each run's daemon contract out of the facts cache
// services_up's label_facts fills under the work directory (daemon-pane.ts's `labelDaemon`).
// Catches: a scorer handed no SKILL_SCENARIOS_WORK where the caller named none and the work
// directory is the default, which kept its own cache under TMPDIR, read nothing label_facts read,
// and ran the reader again. The `bun` on PATH records what it was given.
test("rig.sh score and rig.sh health are given the work directory whose facts cache they read", () => {
  const bin = freshDir("bun-recorder");
  const seen = path.join(bin, "seen");
  writeFileSync(
    path.join(bin, "bun"),
    `#!/bin/sh\nprintf '%s %s\\n' "$SKILL_SCENARIOS_WORK" "$(basename "$1")" >>'${seen}'\n`,
    { mode: 0o755 }
  );
  const tmp = freshDir("default-work-tmp");
  for (const command of ["score", "health"]) {
    const result = spawnChecked(["bash", path.join(import.meta.dir, "rig.sh"), command], {
      env: { ...childEnv, PATH: `${bin}:${childEnv.PATH}`, TMPDIR: tmp },
    });
    expect([command, result.exitCode]).toEqual([command, 0]);
  }
  expect(readFileSync(seen, "utf8")).toBe(`${tmp}/work score.ts\n${tmp}/work health.ts\n`);
});

// A run of `rig.sh class-search` at 7812df81 exited 2 after 549 s with both streams empty, and
// nothing said that bun was what had failed. Catches: a wrapper that lets bun's nonzero exit
// through with no word of its own, or that says so and exits 0 or with a status of its own. The
// `bun` on PATH exits 2 and prints nothing.
test("rig.sh class-search names bun's exit status on stderr when bun exits nonzero, and keeps it", () => {
  const bin = freshDir("bun-silent");
  writeFileSync(path.join(bin, "bun"), "#!/bin/sh\nexit 2\n", { mode: 0o755 });
  const result = spawnChecked(["bash", path.join(import.meta.dir, "rig.sh"), "class-search"], {
    env: { ...childEnv, PATH: `${bin}:${childEnv.PATH}`, TMPDIR: freshDir("bun-silent-tmp") },
  });
  expect([result.exitCode, result.stdout.toString(), result.stderr.toString()]).toEqual([
    2,
    "",
    "class-search: bun exited 2\n",
  ]);
});

// Every batch numbers each label's runs from one above the label's highest run, live or archived,
// so a later batch never takes an earlier run's name (cmd_run refuses one, archive_run keeps one).
describe("next_run", () => {
  const next = (work: string, scenario: string, label: string) =>
    bash(`${fn("next_run")}\nwork=$1\nnext_run "$2" "$3"`, [work, scenario, label]);

  // Catches: numbering that starts over at 1 and so overwrites run 1, or that counts directories
  // instead of reading their numbers.
  test("one above the highest number of the scenario and label", () => {
    const work = freshDir("numbering");
    for (const run of [
      "spec-writing-s1-1",
      "spec-writing-s1-10",
      "spec-writing-s1-x-4",
      "spec-writing-s1-2-7",
      "spec-rewrite-s1-30",
      "spec-writing-main-40",
    ])
      mkdirSync(path.join(work, "runs", run), { recursive: true });
    writeFileSync(path.join(work, "runs", "spec-writing-s1-99"), "");
    expect(next(work, "spec-writing", "s1")).toEqual({ code: 0, stdout: "11\n", stderr: "" });
  });

  // Catches: a first batch that fails because $work/runs does not exist yet.
  test("1 when the label has no run of the scenario", () => {
    expect(next(freshDir("numbering-empty"), "spec-rewrite", "s1").stdout).toBe("1\n");
  });

  // Catches: numbering that reads $work/runs alone. A run that ended is in the archive
  // (archive_run), so a later batch numbered from runs/ alone would take its number, and the
  // archive would replace it with the new run when that one ended.
  test("counts the runs the archive holds", () => {
    const work = freshDir("numbering-archive");
    mkdirSync(path.join(work, "runs", "spec-writing-s1-3"), { recursive: true });
    mkdirSync(path.join(work, "archive", "runs", "spec-writing-s1-12"), { recursive: true });
    expect(next(work, "spec-writing", "s1").stdout).toBe("13\n");
  });
});

// Two batches of one scenario in a row, each `rig.sh batch <scenario> 1 head`, with the batch's
// services stubbed as the services rows below stub them (both URLs given, so no container) and a
// rig.sh of the row's own standing in for `rig.sh run`: it logs the run's name and leaves the
// run's directory in the archive, as a real run that ended does. Before every batch was numbered,
// a batch of any scenario but spec-writing and spec-rewrite started at 1 every time, so the second
// batch's run 1 took the first batch's name, and archive_run replaced the healthy archived run 1.
describe("cmd_batch numbering", () => {
  /** One batch of `scenario`, one run of label head, in the work directory `work`. */
  function batch(work: string, scenario: string, env: Record<string, string> = {}) {
    const here = path.join(work, "rig");
    mkdirSync(here, { recursive: true });
    writeFileSync(
      path.join(here, "rig.sh"),
      [
        "# rig.sh run <scenario> <label> <n> <index> <start>, as cmd_batch's xargs starts it.",
        "set -euo pipefail",
        '[ "$1" = run ]',
        `name=$2\${SKILL_SCENARIOS_ASKGATE_CASE:+-$SKILL_SCENARIOS_ASKGATE_CASE}-$3-$4`,
        'mkdir -p "$WORK/archive/runs"',
        'mkdir "$WORK/archive/runs/$name"',
        'printf \'%s\\n\' "$name" >>"$WORK/runs.log"',
        "",
      ].join("\n")
    );
    return bash(
      [
        "set -euo pipefail",
        ...[
          "fail",
          "note",
          "on_exit",
          "given_services",
          "connections_needed",
          "safe_url",
          "services_files",
          "lock_services",
          "services_up",
          "services_down",
          "build_dir",
          "go_binary",
          "daemon_pane_binary",
          "label_services",
          "label_provenance",
          "next_run",
          "cmd_batch",
        ].map(fn),
        // The real scenario_case, which reads the case through this directory's run-names.ts,
        // while `here` is the batch's stub rig.
        fn("scenario_case").replace(/^scenario_case\(\)/, "real_scenario_case()"),
        'scenario_case() { here=$RIG real_scenario_case "$@"; }',
        "load_label() { co=$root; }",
        "rig_mise() { :; }",
        "push_guard() { :; }",
        "require_namespace() { :; }",
        "tmux_ready() { :; }",
        "stop_batch() { services_down; }",
        "stop_tree() { :; }",
        'until_true() { shift 2; "$@"; }',
        "pg_sql() { printf '%s\\n' 100; }",
        "nats_ready() { :; }",
        'start_listener() { envoy_port=4100; : >"$work/listener_$1.port"; }',
        "services_from() { printf 'envoy-listener label %s -\\n' \"$co\"; }",
        'go() { case "$*" in "run ./internal/dispatch/store/cmd/connections -process") echo 30 ;; *) : >"$3" ;; esac; }',
        "label_facts() { :; }",
        "label_contract() { :; }",
        "work=$1 here=$2 root=$3 tag=rig-test containers=()",
        "services_postgres=$work/services/postgres-url tokens=$work/services/tokens",
        // services_up reads the given Postgres URL with pg-url.sh and loopback-dsn.sh, as rig.sh
        // sources them.
        '. "$root/packages/envoy/scripts/lib/pg-url.sh"',
        '. "$root/scripts/e2e/lib/loopback-dsn.sh"',
        'cmd_batch "$4" 1 head',
      ].join("\n"),
      [work, here, root, scenario],
      {
        WORK: work,
        SKILL_SCENARIOS_PARALLEL: "1",
        SKILL_SCENARIOS_POSTGRES_URL: "postgres://rig@127.0.0.1:5432/postgres",
        SKILL_SCENARIOS_NATS_URL: "nats://127.0.0.1:4222",
        RIG: import.meta.dir,
        ...env,
      }
    );
  }

  // Catches: a batch of a scenario other than spec-writing and spec-rewrite numbered from 1, which
  // gave the second batch's run the first batch's name, and a batch whose teardown leaves a file
  // of its services that refuses the next batch (lock_services).
  test("two batches of a scenario in a row number their runs 1 and 2", () => {
    const work = freshDir("batch-numbering");
    for (const _ of [1, 2])
      expect(batch(work, "ask-on-message")).toMatchObject({ code: 0, stderr: "" });
    expect(readFileSync(path.join(work, "runs.log"), "utf8")).toBe(
      "ask-on-message-head-1\nask-on-message-head-2\n"
    );
    expect(readdirSync(path.join(work, "archive", "runs")).sort()).toEqual([
      "ask-on-message-head-1",
      "ask-on-message-head-2",
    ]);
  });

  // Catches: numbering that reads the scenario's runs without the case every run name of a cased
  // scenario carries, which finds none and starts at 1 again, and numbering that counts another
  // case's runs as this case's, and numbering that reads no run's record, where a run's name is
  // since the names left the world (rig.sh's cmd_run), so a record's number is taken again.
  test("a cased scenario's batches are numbered within their case, from names and records alike", () => {
    const work = freshDir("batch-numbering-case");
    mkdirSync(path.join(work, "archive", "runs", "askgate-facts-head-4"), { recursive: true });
    mkdirSync(path.join(work, "archive", "runs", "0123456789ab"), { recursive: true });
    writeFileSync(
      path.join(work, "archive", "runs", "0123456789ab.json"),
      JSON.stringify({ name: "askgate-link-head-2" })
    );
    for (const _ of [1, 2])
      expect(batch(work, "askgate", { SKILL_SCENARIOS_ASKGATE_CASE: "link" })).toMatchObject({
        code: 0,
        stderr: "",
      });
    expect(readFileSync(path.join(work, "runs.log"), "utf8")).toBe(
      "askgate-link-head-3\naskgate-link-head-4\n"
    );
  });
});

// Each serial run's exit records what appeared at the top of the machine's shared scratch while
// it ran, by the record scratch_before took when it started, for health.ts, and deletes nothing
// there: what appeared may be anything else this user ran meanwhile, and the run's agent writes
// its own /tmp under the run (launch). Each row stands a directory of its own in for /tmp
// (shared_scratch).
describe("scratch_clean", () => {
  function cleaned(parallel: string) {
    const shared = freshDir("shared-scratch");
    const R = freshDir("scratch-run");
    // cmd_run makes the run's logs before scratch_before writes its record there.
    mkdirSync(path.join(R, "logs"));
    writeFileSync(path.join(shared, "pre-batch.json"), "from an earlier batch\n");
    const result = bash(
      [
        fn("note"),
        fn("scratch_before"),
        fn("scratch_clean"),
        "R=$1 shared_scratch=($2)",
        "scratch_before",
        'echo run >"$2/LWEVAL-1-review.json"',
        'mkdir "$2/a-dir" && echo run >"$2/a-dir/inside"',
        "scratch_clean",
      ].join("\n"),
      [R, shared],
      { SKILL_SCENARIOS_PARALLEL: parallel }
    );
    return { shared, R, result };
  }

  // Catches: a record that misses what the run's time added, or that names what was there before
  // it, and a cleanup that deletes what appeared, which may be another process's of this user; and
  // a record at the top of the run's directory, where its agents read it and launch's guard read
  // other suites' /tmp names in it as the run's world (CI at 8de01663).
  test("a serial run records what appeared in the shared scratch, and removes nothing there", () => {
    const { shared, R, result } = cleaned("1");
    expect(result).toEqual({ code: 0, stdout: "", stderr: "" });
    expect(readdirSync(shared).sort()).toEqual(["LWEVAL-1-review.json", "a-dir", "pre-batch.json"]);
    expect(readFileSync(path.join(shared, "a-dir", "inside"), "utf8")).toBe("run\n");
    expect(
      readFileSync(path.join(R, "logs", "scratch-created.txt"), "utf8")
        .split("\n")
        .filter((line) => line !== "")
        .map((line) => line.replace(/^\S+ /, ""))
        .sort()
    ).toEqual([path.join(shared, "LWEVAL-1-review.json"), path.join(shared, "a-dir")]);
    expect(readdirSync(R)).toEqual(["logs"]);
    expect(readdirSync(path.join(R, "logs")).sort()).toEqual([
      "scratch-before.txt",
      "scratch-created.txt",
    ]);
  });

  // Catches: a record at a parallelism above 1, which would name a concurrent run's files as this
  // run's.
  test("runs side by side leave the shared scratch as it is, record nothing, and say so", () => {
    const { shared, R, result } = cleaned("2");
    expect(result.code).toBe(0);
    expect(result.stdout).toContain(
      "only serial runs (SKILL_SCENARIOS_PARALLEL=1) record what a run made there"
    );
    expect(readdirSync(shared).sort()).toEqual(["LWEVAL-1-review.json", "a-dir", "pre-batch.json"]);
    expect(existsSync(path.join(R, "logs", "scratch-created.txt"))).toBe(false);
  });
});

// A run's exit moves its directory out of the runs directory every run of a batch is made in,
// so a later run's agent finds no finished sibling beside its own (the 2026-11-14 agents, every
// push refused, read the pointer files of runs that had ended).
describe("archive_run", () => {
  const archive = (work: string, R: string) =>
    bash(`${fn("fail")}\n${fn("archive_run")}\nwork=$1 R=$2\narchive_run`, [work, R]);

  // Catches: a run left in runs/ when it ends, and a sibling moved with it.
  test("a run that ended leaves runs/ for the archive", () => {
    const work = freshDir("archive");
    const R = path.join(work, "runs", "threads-closed-correct-head-1");
    mkdirSync(path.join(R, "sessions"), { recursive: true });
    writeFileSync(path.join(R, "out.txt"), "exit=0\n");
    mkdirSync(path.join(work, "runs", "threads-closed-correct-head-2"));
    const before = treeDigest(R);
    expect(archive(work, R)).toEqual({ code: 0, stdout: "", stderr: "" });
    expect(readdirSync(path.join(work, "runs"))).toEqual(["threads-closed-correct-head-2"]);
    expect(treeDigest(path.join(work, "archive", "runs", "threads-closed-correct-head-1"))).toBe(
      before
    );
  });

  // Catches: an archive that removes the archived run of the run's name and moves the new run
  // there (a second batch of one scenario deleted the first batch's healthy run 1 so, and put its
  // own failed run 1 in its place, with exit 0), or nests the new run inside it.
  test("an archived run of the run's name is refused, named, and kept byte for byte, and the run stays in runs/", () => {
    const work = freshDir("archive-taken");
    const R = path.join(work, "runs", "ask-on-message-head-1");
    mkdirSync(path.join(R, "sessions"), { recursive: true });
    writeFileSync(path.join(R, "out.txt"), "a failed run\nexit=1\n");
    const archived = path.join(work, "archive", "runs", "ask-on-message-head-1");
    mkdirSync(path.join(archived, "sessions"), { recursive: true });
    writeFileSync(path.join(archived, "out.txt"), "a healthy run\nexit=0\n");
    writeFileSync(path.join(archived, "sessions", "s.jsonl"), '{"type":"session"}\n');
    const [archivedBefore, runBefore] = [treeDigest(archived), treeDigest(R)];
    const result = archive(work, R);
    expect(result.code).toBe(1);
    expect(result.stderr).toBe(
      `skill-scenarios: archive_run: run ask-on-message-head-1 is already archived at ${archived}; neither is removed or replaced, and this run stays at ${R}\n`
    );
    expect(treeDigest(archived)).toBe(archivedBefore);
    expect(treeDigest(R)).toBe(runBefore);
  });

  // Catches: a move onto an archived run that appeared after the check above, which `mv` would
  // nest the run inside (or, for an empty one, replace). The `mv` here makes that run first.
  test("an archived run that appears after the check is neither entered nor replaced, and the run stays", () => {
    const work = freshDir("archive-race");
    const R = path.join(work, "runs", "ask-on-message-head-1");
    mkdirSync(R, { recursive: true });
    writeFileSync(path.join(R, "out.txt"), "exit=1\n");
    const archived = path.join(work, "archive", "runs", "ask-on-message-head-1");
    const result = bash(
      `${fn("fail")}\n${fn("archive_run")}\nmv() { mkdir -p "\${@: -1}" && echo earlier >"\${@: -1}/out.txt" && command mv "$@"; }\nwork=$1 R=$2\narchive_run`,
      [work, R]
    );
    expect(result.code).toBe(1);
    expect(result.stderr).toContain(
      `skill-scenarios: archive_run: run ask-on-message-head-1 could not move to ${archived}; nothing there is removed or replaced, and this run stays at ${R}`
    );
    expect(readdirSync(archived)).toEqual(["out.txt"]);
    expect(readFileSync(path.join(archived, "out.txt"), "utf8")).toBe("earlier\n");
    expect(readFileSync(path.join(R, "out.txt"), "utf8")).toBe("exit=1\n");
  });

  // Catches: a live read's directory, which is no run of runs/, moved with the runs.
  test("a directory outside runs/ stays where it is", () => {
    const work = freshDir("archive-live");
    const R = path.join(work, "live-read", "head");
    mkdirSync(R, { recursive: true });
    expect(archive(work, R).code).toBe(0);
    expect(existsSync(R)).toBe(true);
    expect(existsSync(path.join(work, "archive"))).toBe(false);
  });
});

// A run or a live read never replaces an earlier one of its name: each is refused before its exit
// handler is set (that handler stops the processes of its directory and archives it), naming the
// directory, which stays byte for byte as it was.
describe("a run or live read of a name already taken", () => {
  /** A work directory holding, at `taken` below it, an earlier run with a transcript of its own. */
  function takenWork(taken: string) {
    const work = freshDir("taken");
    const dir = path.join(work, taken);
    mkdirSync(path.join(dir, "sessions"), { recursive: true });
    writeFileSync(path.join(dir, "out.txt"), "an earlier run\nexit=0\n");
    writeFileSync(path.join(dir, "sessions", "s.jsonl"), '{"type":"session"}\n');
    mkdirSync(path.join(work, "services"));
    writeFileSync(path.join(work, "services", "env-head"), "");
    writeFileSync(path.join(work, "services", "services-from-head"), "");
    mkdirSync(path.join(work, "profiles", "head"), { recursive: true });
    return { work, dir, digest: treeDigest(dir) };
  }
  const stubs = [
    "load_label() { P=$work/profiles/$1; I=$P; co=$work/checkout; }",
    "push_guard() { :; }",
    "require_namespace() { :; }",
    "tmux_ready() { :; }",
    "rig_mise() { :; }",
    "scratch_before() { :; }",
    'stop_run() { echo stop_run >>"$work/stopped"; }',
  ];
  const run = (work: string) =>
    bash(
      [
        ...[
          "fail",
          "on_exit",
          "scenario_case",
          "run_named",
          "cmd_run",
          "label_services",
          "label_provenance",
        ].map(fn),
        ...stubs,
        "work=$1 here=$2",
        "cmd_run ask-on-message head 3 1 100",
      ].join("\n"),
      [work, import.meta.dir]
    );
  /** The run directories and records of the work directory's runs/, which a refused run adds
   * none to. */
  const made = (work: string) =>
    existsSync(path.join(work, "runs")) ? readdirSync(path.join(work, "runs")).sort() : [];

  // Catches: a run that removes the live run of its name (cmd_run started from `rm -rf`), or sets
  // the exit handler that would stop and archive that run's directory.
  test("a run whose name a live run holds is refused, and the live run is kept", () => {
    const { work, dir, digest } = takenWork("runs/ask-on-message-head-3");
    const result = run(work);
    expect(result.code).toBe(1);
    expect(result.stderr).toBe(
      `skill-scenarios: run ask-on-message-head-3 is already at ${dir}; a run never replaces another (a batch numbers its runs above each label's highest)\n`
    );
    expect(treeDigest(dir)).toBe(digest);
    expect(made(work)).toEqual(["ask-on-message-head-3"]);
    expect(existsSync(path.join(work, "stopped"))).toBe(false);
  });

  // Catches: a run whose archived name it takes, which runs and then cannot be archived, or
  // replaces the archived run when it ends.
  test("a run whose name an archived run holds is refused before it makes its directory", () => {
    const { work, dir, digest } = takenWork("archive/runs/ask-on-message-head-3");
    const result = run(work);
    expect(result.code).toBe(1);
    expect(result.stderr).toBe(
      `skill-scenarios: run ask-on-message-head-3 is already at ${dir}; a run never replaces another (a batch numbers its runs above each label's highest)\n`
    );
    expect(treeDigest(dir)).toBe(digest);
    expect(made(work)).toEqual([]);
    expect(existsSync(path.join(work, "stopped"))).toBe(false);
  });

  // Catches: a name a run's record holds, live or archived, taken again, since a run's directory
  // is its id and names nothing, so the record is where the name a batch numbers by is held.
  test("a run whose name a recorded run holds is refused, naming that run's directory", () => {
    for (const runs of ["runs", "archive/runs"]) {
      const { work, dir, digest } = takenWork(`${runs}/0123456789ab`);
      writeFileSync(`${dir}.json`, JSON.stringify({ name: "ask-on-message-head-3" }));
      const result = run(work);
      expect(result.code).toBe(1);
      expect(result.stderr).toBe(
        `skill-scenarios: run ask-on-message-head-3 is already at ${dir}; a run never replaces another (a batch numbers its runs above each label's highest)\n`
      );
      expect(treeDigest(dir)).toBe(digest);
      expect(made(work)).toEqual(runs === "runs" ? ["0123456789ab", "0123456789ab.json"] : []);
    }
  });

  // Catches: a live read that removes the earlier live read of its label (it started from
  // `rm -rf`).
  test("a live read of a label whose earlier live read is there is refused, and the earlier one is kept", () => {
    const { work, dir, digest } = takenWork("live-read/head");
    const result = bash(
      [
        ...["fail", "on_exit", "cmd_live_read"].map(fn),
        ...stubs,
        "work=$1",
        "cmd_live_read head dispatch",
      ].join("\n"),
      [work]
    );
    expect(result.code).toBe(1);
    expect(result.stderr).toContain(
      `skill-scenarios: live-read: ${dir} could not be made (mkdir says why above); an earlier live read of label head is never replaced`
    );
    expect(treeDigest(dir)).toBe(digest);
    expect(existsSync(path.join(work, "stopped"))).toBe(false);
  });
});

// A run's services are its label's in its batch: the services file naming that label's listener,
// and the label's provenance (services_up), which the run carries as <run>/services-from beside
// its built-from from before anything of it starts. The scenario's runner stands in for what the
// run starts, and records what the run's directory held when it began.
describe("cmd_run and its label's services", () => {
  /** A run of label base, in a work directory whose batch left `services` (file name → text). */
  function started(services: Record<string, string>) {
    const work = freshDir("run-services");
    mkdirSync(path.join(work, "services"));
    for (const [file, text] of Object.entries(services))
      writeFileSync(path.join(work, "services", file), text);
    mkdirSync(path.join(work, "profiles", "base"), { recursive: true });
    writeFileSync(path.join(work, "profiles", "base", "built-from"), "checkout /co/base\n");
    const result = bash(
      [
        ...[
          "fail",
          "on_exit",
          "scenario_case",
          "run_named",
          "run_project",
          "label_services",
          "label_provenance",
          "cmd_run",
        ].map(fn),
        "load_label() { P=$work/profiles/$1; I=$P; co=/co/$1; }",
        "push_guard() { :; }",
        "require_namespace() { :; }",
        "tmux_ready() { :; }",
        "scratch_before() { :; }",
        "stop_run() { :; }",
        'run_ask_on_message() { cp "$R/services-from" "$R/at-start" && printf \'%s\\n\' "$services_env" >"$R/services-env" && echo exit=0 >"$R/out.txt"; }',
        "work=$1 here=$2 tag=legion-00000000 worker_repo=fernbrook/widgets",
        "cmd_run ask-on-message base 1 1 100",
      ].join("\n"),
      [work, import.meta.dir]
    );
    return { work, run: namedRun(path.join(work, "runs"), "ask-on-message-base-1"), result };
  }

  // Catches: a run with no record of what its services came from, or one recorded only after its
  // agent started, and a run pointed at another label's services file.
  test("a run carries its label's provenance before its world starts, and reads its label's services file", () => {
    const provenance = "envoy-listener label /co/base sha256:00\n";
    const { work, run, result } = started({
      "env-base": "ENVOY_URL=http://127.0.0.1:4102\n",
      "services-from-base": provenance,
      "env-head": "ENVOY_URL=http://127.0.0.1:4101\n",
      "services-from-head": "envoy-listener label /co/head sha256:00\n",
    });
    expect(result).toMatchObject({ code: 0, stderr: "" });
    if (run === undefined) throw new Error("the run left no record of its name");
    expect(readFileSync(path.join(run, "at-start"), "utf8")).toBe(provenance);
    expect(readFileSync(path.join(run, "services-from"), "utf8")).toBe(provenance);
    expect(readFileSync(path.join(run, "built-from"), "utf8")).toBe("checkout /co/base\n");
    expect(readFileSync(path.join(run, "services-env"), "utf8")).toBe(
      `${path.join(work, "services", "env-base")}\n`
    );
    // Catches: a run directory named for the run's scenario, case or label, which every path its
    // agent reads is built on, and a run whose record of what it is is missing, wrong or late.
    const id = path.basename(run);
    expect(id).toMatch(/^[0-9a-f]{12}$/);
    expect(JSON.parse(readFileSync(`${run}.json`, "utf8"))).toEqual({
      run: id,
      scenario: "ask-on-message",
      case: "",
      label: "base",
      n: 1,
      name: "ask-on-message-base-1",
      batch: "legion-00000000-100",
      ran: run,
    });
    expect(readdirSync(path.join(work, "runs")).sort()).toEqual([id, `${id}.json`]);
  });

  // Catches: a run started on a batch that brought up no services for its label, or wrote no
  // provenance for it, which would run on another label's listener or with none recorded.
  test("a run whose batch left no services file or no provenance for its label is refused before it starts", () => {
    const cases: Record<string, string>[] = [
      { "env-head": "", "services-from-head": "" },
      { "env-base": "" },
      { "services-from-base": "" },
    ];
    for (const services of cases) {
      const { work, run, result } = started(services);
      expect(result.code).toBe(1);
      expect(result.stderr).toContain(
        "skill-scenarios: a run needs its batch's services for label base"
      );
      expect(run).toBeUndefined();
      expect(existsSync(path.join(work, "runs"))).toBe(false);
    }
  });
});

// Every run's agent starts in a detached tmux session (rig.sh's launch). A pod on an older image
// carried no tmux, and a tmux binary put there alone could not load libevent_core-2.1.so.7; the
// current worker image carries tmux (3.5a, read 2026-11-25). A batch met either only mid-run, once
// its databases and services were up. So a batch and a run check their tmux before they make
// anything (tmux_ready): `tmux -V`, then one detached session started and killed on a socket of the
// check's own. Each row puts a `tmux` stand-in of its own first on PATH, or gives a PATH with no
// tmux on it at all, and holds a refused command's work directory to its state before the command:
// no database, service or run directory.
describe("tmux_ready, before a batch or a run makes anything", () => {
  /** The end of every refusal of a tmux. */
  const NOTHING = "every run's agent starts in a detached tmux session (launch), so nothing starts";
  /** What a pod's bare tmux binary printed on 2026-11-17 for every call, `-V` included, the loader
   * failing before tmux ran. */
  const UNLOADABLE =
    "error while loading shared libraries: libevent_core-2.1.so.7: cannot open shared object file: No such file or directory";
  /** A directory holding a `tmux` that logs each call's arguments to <dir>/calls and answers as
   * tmux does: `-V` with its version, `new-session -d` and `kill-session` with nothing. The step
   * `failing` names (`-V`, `new-session` or `kill-session`, read past `-S <socket>`) prints its
   * own path and `message` on stderr instead, and exits 127. */
  function tmuxStandIn(failing?: { step: string; message: string }): string {
    const dir = freshDir("tmux-standin");
    writeFileSync(
      path.join(dir, "tmux"),
      [
        "#!/usr/bin/env bash",
        `printf '%s\\n' "$*" >>${JSON.stringify(path.join(dir, "calls"))}`,
        'step=$1; [ "$1" != -S ] || step=$3',
        ...(failing === undefined
          ? []
          : [
              `if [ "$step" = ${failing.step} ]; then echo "$0: ${failing.message}" >&2; exit 127; fi`,
            ]),
        '[ "$step" != -V ] || echo "tmux 3.5a"',
        "",
      ].join("\n"),
      { mode: 0o755 }
    );
    return dir;
  }
  /** A directory holding a link to each program a batch or a run here runs besides tmux, and no
   * tmux: a PATH of it alone is a host with no tmux, whatever this host has. */
  function noTmux(): string {
    const dir = freshDir("no-tmux");
    for (const program of ["bash", "mkdir", "mktemp", "rm", "date", "seq", "xargs", "dirname"]) {
      const found = Bun.which(program, { PATH: childEnv.PATH });
      if (found === null) throw new Error(`no ${program} on this host's PATH`);
      symlinkSync(found, path.join(dir, program));
    }
    return dir;
  }
  /** Each step at which a batch and a run refuse a tmux: `none`, no tmux on PATH; `-V`, a tmux
   * whose `-V` fails; `new-session`, one that answers `-V` and starts no session (the libevent
   * case); `kill-session`, one that starts the session and cannot kill it. */
  const REFUSED = ["none", "-V", "new-session", "kill-session"] as const;
  /** A PATH whose tmux a batch and a run refuse at `step`, and the refusal's stderr. */
  function refusal(step: (typeof REFUSED)[number]): { PATH: string; stderr: string } {
    if (step === "none")
      return { PATH: noTmux(), stderr: `skill-scenarios: tmux -V: no tmux on PATH; ${NOTHING}\n` };
    const message = step === "kill-session" ? "can't find session: ready" : UNLOADABLE;
    const bin = tmuxStandIn({ step, message });
    const tmux = path.join(bin, "tmux");
    return {
      PATH: `${bin}:${childEnv.PATH}`,
      stderr: `skill-scenarios: tmux ${step}: ${tmux} exited 127: ${tmux}: ${message}; ${NOTHING}\n`,
    };
  }

  /** `rig.sh batch ask-on-message 1 head` on PATH `PATH`, in a work directory of its own: its
   * status and stderr, the work directory and its digest before the batch, and each line the
   * batch's `rig.sh run` logged, a stand-in rig that logs its arguments and the tmux its batch
   * handed it. Each step a batch takes after its checks is a stub that makes in the work
   * directory what that step makes there: the rig's mise, the services' lock and a label's
   * services file. */
  function batch(PATH: string) {
    const work = freshDir("tmux-batch");
    mkdirSync(path.join(work, "profiles", "head"), { recursive: true });
    const here = freshDir("tmux-batch-rig");
    writeFileSync(
      path.join(here, "rig.sh"),
      'printf \'%s %s\\n\' "$*" "$RIG_TMUX_CHECKED" >>"$(dirname "$0")/runs.log"\n'
    );
    const before = treeDigest(work);
    const result = bash(
      [
        "set -euo pipefail",
        ...["fail", "note", "on_exit", "tmux_ready", "cmd_batch"].map(fn),
        "scenario_case() { :; }",
        "load_label() { :; }",
        "push_guard() { :; }",
        "require_namespace() { :; }",
        'rig_mise() { mkdir -p "$work/tools/bin"; }',
        'lock_services() { S=$work/services; mkdir -p "$S/logs"; : >"$S/up"; }',
        'services_up() { : >"$S/env-$1"; }',
        "stop_batch() { :; }",
        "next_run() { echo 1; }",
        "work=$1 here=$2",
        "PATH=$3",
        "cmd_batch ask-on-message 1 head",
      ].join("\n"),
      [work, here, PATH],
      { SKILL_SCENARIOS_PARALLEL: "1" }
    );
    return { ...result, work, before, runs: fileLines(path.join(here, "runs.log")) };
  }

  /** `rig.sh run ask-on-message base 1 1 100` on PATH `PATH`, with RIG_TMUX_CHECKED `checked`
   * (unset for undefined), in a work directory whose batch left the label's services, as the
   * "cmd_run and its label's services" rows build one: its status and stderr, the work directory
   * and its digest before the run, and the run's directory, undefined for none. */
  function run(PATH: string, checked?: string) {
    const work = freshDir("tmux-run");
    mkdirSync(path.join(work, "services"));
    writeFileSync(path.join(work, "services", "env-base"), "");
    writeFileSync(path.join(work, "services", "services-from-base"), "");
    mkdirSync(path.join(work, "profiles", "base"), { recursive: true });
    writeFileSync(path.join(work, "profiles", "base", "built-from"), "checkout /co/base\n");
    const before = treeDigest(work);
    const result = bash(
      [
        "set -euo pipefail",
        ...[
          "fail",
          "on_exit",
          "scenario_case",
          "run_named",
          "run_project",
          "label_services",
          "label_provenance",
          "tmux_ready",
          "cmd_run",
        ].map(fn),
        "load_label() { P=$work/profiles/$1; I=$P; co=/co/$1; }",
        "push_guard() { :; }",
        "require_namespace() { :; }",
        "scratch_before() { :; }",
        "stop_run() { :; }",
        'run_ask_on_message() { echo exit=0 >"$R/out.txt"; }',
        "work=$1 here=$2 tag=legion-00000000 worker_repo=fernbrook/widgets",
        "PATH=$3",
        "cmd_run ask-on-message base 1 1 100",
      ].join("\n"),
      [work, import.meta.dir, PATH],
      { RIG_TMUX_CHECKED: checked }
    );
    return {
      ...result,
      work,
      before,
      ran: namedRun(path.join(work, "runs"), "ask-on-message-base-1"),
    };
  }

  /** The steps of each call a stand-in tmux at `bin` logged, its `-S <socket>` read off. */
  const steps = (bin: string) =>
    fileLines(path.join(bin, "calls")).map((call) => call.replace(/^-S \S+ /, ""));
  /** What tmux_ready asks of a tmux, in order. */
  const CHECK = ["-V", "new-session -d -s ready -x 200 -y 50 sleep 60", "kill-session -t =ready"];

  /** Holds a batch whose tmux is refused at `step` to its refusal: status 1, the refusal's
   * stderr, no run started, and its work directory as it was. */
  function refusedBatch(step: (typeof REFUSED)[number]) {
    const { PATH, stderr } = refusal(step);
    const refused = batch(PATH);
    expect({ code: refused.code, stderr: refused.stderr }).toEqual({ code: 1, stderr });
    expect(refused.runs).toEqual([]);
    expect(treeDigest(refused.work)).toBe(refused.before);
  }

  // Catches: a batch that meets the missing tmux only at its first launch, once its services are up.
  test("a batch with no tmux on PATH is refused, naming tmux -V, and makes nothing", () => {
    refusedBatch("none");
  });

  // Catches: a batch that never runs `tmux -V`, which starts its services on a tmux that cannot run.
  test("a batch whose tmux -V fails is refused, naming tmux -V and what tmux printed, and makes nothing", () => {
    refusedBatch("-V");
  });

  // Catches: a check that only asks `tmux -V`, which a tmux that starts no session answers: the
  // libevent case, refused only at the session start.
  test("a batch whose tmux answers -V and starts no session is refused, naming new-session and what tmux printed, and makes nothing", () => {
    refusedBatch("new-session");
  });

  // Catches: a check that starts a session and never kills it, or takes a kill that failed.
  test("a batch whose tmux cannot kill the session it started is refused, naming kill-session, and makes nothing", () => {
    refusedBatch("kill-session");
  });

  // Catches: a check on the work directory's own tmux server (<work>/tmux.sock), which every run
  // reads, a check that leaves its socket's directory behind, and a batch that does not hand its
  // runs the tmux it checked.
  test("a batch whose tmux starts and kills a session on a socket of its own goes on, handing its runs that tmux", () => {
    const bin = tmuxStandIn();
    const started = batch(`${bin}:${childEnv.PATH}`);
    expect({ code: started.code, stderr: started.stderr }).toEqual({ code: 0, stderr: "" });
    expect(started.runs.map((line) => line.split(" "))).toEqual([
      [
        "run",
        "ask-on-message",
        "head",
        "1",
        "1",
        expect.stringMatching(/^\d+$/),
        path.join(bin, "tmux"),
      ],
    ]);
    expect(steps(bin)).toEqual(CHECK);
    const sockets = fileLines(path.join(bin, "calls")).map((call) => call.split(" ")[1]);
    const socket = sockets[1] ?? "";
    expect(sockets.slice(1)).toEqual([socket, socket]);
    expect(path.dirname(path.dirname(socket))).toBe(started.work);
    expect(socket).not.toBe(path.join(started.work, "tmux.sock"));
    expect(existsSync(path.dirname(socket))).toBe(false);
    expect(readdirSync(path.join(started.work, "services")).sort()).toEqual([
      "env-head",
      "logs",
      "up",
    ]);
  });

  // Catches: a run started on its own that meets its tmux only at its launch, once its own
  // Dispatch database and server are made.
  test("a run its batch did not check is refused for each tmux a batch refuses, before it makes its directory", () => {
    for (const step of REFUSED) {
      const { PATH, stderr } = refusal(step);
      const refused = run(PATH);
      expect({ step, code: refused.code, stderr: refused.stderr }).toEqual({
        step,
        code: 1,
        stderr,
      });
      expect(refused.ran).toBeUndefined();
      expect(treeDigest(refused.work)).toBe(refused.before);
    }
  });

  // Catches: a batch's runs each checked again, which a batch of N runs pays N times for the one
  // tmux their batch checked, and a run that takes a check of another tmux than its PATH's.
  test("a run its batch checked the tmux of asks nothing of it, and any other run checks its tmux", () => {
    const bin = tmuxStandIn();
    const PATH = `${bin}:${childEnv.PATH}`;
    const checked = run(PATH, path.join(bin, "tmux"));
    expect({ code: checked.code, stderr: checked.stderr }).toEqual({ code: 0, stderr: "" });
    expect(checked.ran).toBeDefined();
    expect(steps(bin)).toEqual([]);
    for (const other of [undefined, path.join(freshDir("other-tmux"), "tmux")]) {
      rmSync(path.join(bin, "calls"), { force: true });
      const own = run(PATH, other);
      expect({ code: own.code, stderr: own.stderr }).toEqual({ code: 0, stderr: "" });
      expect(own.ran).toBeDefined();
      expect(steps(bin)).toEqual(CHECK);
    }
  });
});

// A Legion pod's `git` is a leak-check wrapper (/opt/legion/bin/git): every push runs the leak
// guard, which finds the shared check through PUBLIC_LEAK_CHECK. A run's HOME and configuration
// home are its own and hold no pointer file, so a pane without the variable had every push of the
// 2026-11-14 batch refused with `LEAK CHECK SETUP ERROR`. Each row builds a run's pane with
// base_env under a `git` of the row's own first on the caller's PATH.
describe("push_guard", () => {
  /** A `git` that, as Legion's wrapper does, runs a push with `-c core.hooksPath=<hooks>`. */
  const WRAPPER =
    '#!/bin/sh\n# a push runs with -c core.hooksPath=/opt/legion/githooks\nexec /usr/bin/git "$@"\n';
  /** A `git` that runs the real one and nothing else. */
  const PLAIN = '#!/bin/sh\nexec /usr/bin/git "$@"\n';
  /** base_env for a run of its own, with `git` first on the caller's PATH and the caller's
   * PUBLIC_LEAK_CHECK `check` (none for undefined): its status, its stderr and its pane's lines.
   * The row's `check` replaces the one childEnv carries from this host, so a host's own
   * PUBLIC_LEAK_CHECK decides nothing here. */
  function pane(git: string, check: string | undefined, callerPath?: string) {
    const dir = freshDir("push-guard");
    const bin = path.join(dir, "caller-bin");
    mkdirSync(bin);
    writeFileSync(path.join(bin, "git"), git, { mode: 0o755 });
    const R = path.join(dir, "run");
    const result = bash(
      [fn("fail"), fn("push_guard"), fn("base_env"), "R=$1 I=$2 profile=legion", "base_env"].join(
        "\n"
      ),
      [R, profileDir()],
      {
        PATH: callerPath ?? `${bin}:${childEnv.PATH}`,
        USER: "rig",
        PUBLIC_LEAK_CHECK: check,
      }
    );
    const file = path.join(R, "pane.env");
    const lines = existsSync(file) ? readFileSync(file, "utf8").split("\n") : [];
    return { ...result, R, lines };
  }
  const check = () => {
    const file = path.join(freshDir("leak-check"), "public-leak-check.sh");
    writeFileSync(file, "#!/bin/sh\nexit 0\n", { mode: 0o755 });
    return file;
  };

  // Catches: a pane that drops the caller's PUBLIC_LEAK_CHECK, as base_env's did.
  test("a wrapper git and a readable check: the pane names the check", () => {
    const named = check();
    const { code, stderr, lines } = pane(WRAPPER, named);
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    expect(lines).toContain(`PUBLIC_LEAK_CHECK=${named}`);
  });

  // Catches: a world whose shared leak check runs its git as the pane's does, so the check's
  // ls-remote of origin's https URL reaches the live github.com and asks the pane's credential
  // (push_guard says what was measured): in a run whose world has a remote of its own
  // (world_remote's <run>/lib/git-core), the pane names the world's run of the check, which runs
  // the caller's check with that exec path and with the arguments the guard gave it; and a world
  // whose namespace then shows the world's run and hides the check it runs (world_paths).
  test("a world with a remote of its own: the pane names its run of the check, and its world shows the check", () => {
    const named = check();
    const R = path.join(freshDir("push-guard-world"), "run");
    mkdirSync(path.join(R, "lib", "git-core"), { recursive: true });
    const result = bash(
      [
        ...["fail", "push_guard", "base_env", "pane_var", "system_gitconfig", "world_paths"].map(
          fn
        ),
        "R=$1 I=$2 profile=legion",
        "base_env",
        'world_paths "$R" "$R/work" "$I" "$R/pane.env"',
      ].join("\n"),
      [R, profileDir()],
      { USER: "rig", PUBLIC_LEAK_CHECK: named }
    );
    expect({ code: result.code, stderr: result.stderr }).toEqual({ code: 0, stderr: "" });
    const run = path.join(R, "lib", "public-leak-check.sh");
    expect(readFileSync(path.join(R, "pane.env"), "utf8").split("\n")).toContain(
      `PUBLIC_LEAK_CHECK=${run}`
    );
    // The hook a pane's git runs refuses a check that is no executable file (leakguard's `found`,
    // packages/daemon/internal/leakguard/check.go: `info.Mode().Perm()&0o111 == 0`, "is no
    // executable file"), and a push whose check is refused fails with LEAK CHECK SETUP ERROR: a
    // reviewer's push was refused so on 2026-11-25, in a pod whose shared check is 0755 and whose
    // wrapper here was 0644.
    expect(lstatSync(run).mode & 0o111).not.toBe(0);
    expect(result.stdout.split("\n")).toContain(`ro ${path.dirname(named)}`);
    writeFileSync(named, '#!/bin/sh\nprintf "%s|%s\\n" "$GIT_EXEC_PATH" "$*"\n');
    const ran = spawnChecked(["bash", run, "origin", "https://github.com/fernbrook/widgets"], {
      env: childEnv,
    });
    expect(ran.stdout.toString()).toBe(
      `${path.join(R, "lib", "git-core")}|origin https://github.com/fernbrook/widgets\n`
    );
  });

  // Catches: a world built whose guard would refuse every push, which measures nothing.
  test("a wrapper git and no readable check: the run is refused, naming the variable", () => {
    for (const named of [undefined, path.join(freshDir("leak-check"), "missing.sh")]) {
      const { code, stderr } = pane(WRAPPER, named);
      expect(code).toBe(1);
      expect(stderr).toContain(
        `is a leak-check wrapper, which refuses every push whose leak check it cannot find, and PUBLIC_LEAK_CHECK names no readable check (${named ?? "unset"})`
      );
    }
  });

  // Catches: a rig that demands a check where no wrapper would run one, or invents a variable.
  test("any other git and no check: nothing is added and nothing is refused", () => {
    const { code, stderr, lines } = pane(PLAIN, undefined);
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    expect(lines.filter((line) => line.startsWith("PUBLIC_LEAK_CHECK="))).toEqual([]);
  });

  // Catches: a run stopped by `set -u` where the caller's shell exports no USER, as an issue
  // pod's did on 2026-11-17 (`rig.sh: line 1005: USER: unbound variable`, before any agent).
  test("a caller with no USER: the pane names the caller's own user, by id", () => {
    const dir = freshDir("push-guard");
    const R = path.join(dir, "run");
    const { code, stderr } = bash(
      [fn("fail"), fn("push_guard"), fn("base_env"), "R=$1 I=$2 profile=legion", "base_env"].join(
        "\n"
      ),
      [R, profileDir()],
      { PATH: `/usr/bin:/bin:${childEnv.PATH}`, USER: undefined, PUBLIC_LEAK_CHECK: undefined }
    );
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    const user = Bun.spawnSync(["id", "-un"]).stdout.toString().trim();
    expect(readFileSync(path.join(R, "pane.env"), "utf8").split("\n")).toContain(`USER=${user}`);
  });

  // Catches: a fix that gets the run's pushes through by getting around the guard. Under
  // Legion's own wrappers and the caller's check, a push from the run's pane passes the guard,
  // and the same push carrying a credential the shared check refuses is refused by it. The key
  // is built from two parts here so this file's own push carries no such line.
  const guard = "/opt/legion/bin/git";
  const shared = process.env.PUBLIC_LEAK_CHECK ?? "";
  test.skipIf(!existsSync(guard) || !existsSync("/opt/legion/bin/legion") || !existsSync(shared))(
    "a push from a run's pane passes the guard, and one carrying a secret is refused by it",
    () => {
      const { code, stderr, R, lines } = pane(PLAIN, shared, `/opt/legion/bin:/usr/bin:/bin`);
      expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
      const vars = Object.fromEntries(
        lines
          .filter((line) => line.includes("="))
          .map((line) => {
            const at = line.indexOf("=");
            return [line.slice(0, at), line.slice(at + 1)];
          })
      );
      const real = (...args: string[]) =>
        spawnChecked([
          "/usr/bin/git",
          "-c",
          "user.name=A",
          "-c",
          "user.email=a@example.invalid",
          ...args,
        ]);
      const remote = path.join(R, "remote.git");
      const src = path.join(R, "src");
      expect(real("init", "-q", "--bare", remote).exitCode).toBe(0);
      expect(real("init", "-q", src).exitCode).toBe(0);
      writeFileSync(path.join(src, "README.md"), "The widgets CLI.\n");
      expect(real("-C", src, "add", "-A").exitCode).toBe(0);
      expect(real("-C", src, "commit", "-qm", "init").exitCode).toBe(0);
      const push = () => {
        const made = spawnChecked(
          ["/bin/sh", "-c", 'git -C "$1" push "$2" HEAD:refs/heads/main', "-", src, remote],
          {
            env: vars,
          }
        );
        return { code: made.exitCode, err: made.stderr.toString() };
      };
      const clean = push();
      expect(clean).toMatchObject({ code: 0 });
      expect(clean.err).not.toContain("LEAK CHECK SETUP ERROR");
      writeFileSync(path.join(src, "keys.txt"), `aws ${"AKIA"}${"QZXWVUTSRQPONMLK"}\n`);
      expect(real("-C", src, "add", "-A").exitCode).toBe(0);
      expect(real("-C", src, "commit", "-qm", "keys").exitCode).toBe(0);
      const secret = push();
      expect(secret.code).not.toBe(0);
      expect(secret.err).toContain("LEAK CHECK: refusing");
    }
  );
});

// childEnv is read once, as this file loads, from the environment its process started in, and
// every script a row runs gets it and nothing more. The rows that failed for its dropping
// PUBLIC_LEAK_CHECK (40 of them, on a pod with no pointer file: tester, 2026-11-15) pass on every
// host whose `git` is no leak-check wrapper, GitHub's runner among them, so none of them can catch
// the variable dropped again there. These two bring a pod's refusal to every host: the second
// starts this file again in a child `bun test` whose environment, set before the child loads the
// file, names a readable PUBLIC_LEAK_CHECK and puts first on PATH a `git` that refuses every push
// unless that variable names a readable file; and it runs there the first row alone, which builds
// a world with rig.sh's own functions and pushes it to the world's remote.git.
describe("childEnv", () => {
  const BUILD = "a world built for real pushes to its own remote under a leak-check git";
  /** Set for the child `bun test` the second row starts, and by nothing else. */
  const CHILD = "SKILL_SCENARIOS_CHILD_ENV_ROW";

  // Registered in that child alone: anywhere else the `git` on PATH and the check are the host's,
  // and the row would read the host rather than childEnv.
  if (process.env[CHILD] !== undefined)
    test(BUILD, () => {
      const dir = freshDir("child-env-world");
      const result = bash(
        [
          ...[
            "fail",
            "fixture_open",
            "fixture_git",
            "fixture_first_commit",
            "fixture_head_commit",
            "fixture_implement",
            "fixture_workspace",
            "workspace_form",
            "world_remote",
            "world_remote_app",
            "role_identity",
            "role_env",
            "fixture_checks",
            "fixture_body",
            "fixture_write",
            "concluded_checks",
            "base_ruleset_routes",
            "tester_proof_world",
          ].map(fn),
          definition("fixture_cli"),
          ...[
            "worker_repo",
            "worker_base",
            "world_author_name",
            "world_author_email",
            "world_login",
            "world_implement_app",
            "world_implement_app_id",
            "world_review_app",
            "world_review_app_id",
            "world_remote_vcs",
          ].map(assignment),
          "R=$1 worker_key=LWEVAL-1",
          "tester_proof_world 1 100",
        ].join("\n"),
        [dir]
      );
      expect(result).toMatchObject({ code: 0, stderr: "" });
      // The pull request's branch on the world's remote is the commit its checkout made.
      const heads = bash(
        'git -C "$1/remote.git" rev-parse legion/LWEVAL-1 && git -C "$1/src" rev-parse HEAD',
        [dir]
      );
      const [pushed = "", made] = heads.stdout.trim().split("\n");
      expect(pushed).toMatch(/^[0-9a-f]{40}$/);
      expect(pushed).toBe(made);
    });

  // Catches: a childEnv that drops the caller's PUBLIC_LEAK_CHECK, on any host. The stand-in git
  // then refuses the world's pushes with `LEAK CHECK SETUP ERROR`, as a pod's wrapper refused the
  // 40 rows', the child's row fails on that stderr, and this row fails quoting the child's report.
  test("it carries the caller's PUBLIC_LEAK_CHECK to every push a world's build makes", () => {
    const dir = freshDir("child-env");
    const bin = path.join(dir, "bin");
    mkdirSync(bin);
    const check = path.join(dir, "public-leak-check.sh");
    writeFileSync(check, "#!/bin/sh\nexit 0\n", { mode: 0o755 });
    const pushes = path.join(dir, "pushes");
    // Legion's leak-check git as far as this row needs one (/opt/legion/bin/git and its pre-push
    // hook): a push runs only where PUBLIC_LEAK_CHECK names a readable file, every other command
    // is the real git's, and each push is recorded by its refspec with the check it ran under.
    writeFileSync(
      path.join(bin, "git"),
      [
        "#!/bin/sh",
        'case " $* " in',
        '*" push "*)',
        "  for last; do :; done",
        '  if [ -f "$PUBLIC_LEAK_CHECK" ] && [ -r "$PUBLIC_LEAK_CHECK" ]; then',
        `    printf '%s %s\\n' "$last" "$PUBLIC_LEAK_CHECK" >>'${pushes}'`,
        "  else",
        `    printf '%s refused\\n' "$last" >>'${pushes}'`,
        `    echo "LEAK CHECK SETUP ERROR: the push of $last: the shared public leak check could not be found: PUBLIC_LEAK_CHECK names \${PUBLIC_LEAK_CHECK:-none}" >&2`,
        "    exit 2",
        "  fi",
        "  ;;",
        "esac",
        'exec /usr/bin/git "$@"',
        "",
      ].join("\n"),
      { mode: 0o755 }
    );
    const child = spawnChecked(
      [process.execPath, "test", import.meta.path, "-t", `^childEnv ${BUILD}$`],
      {
        cwd: path.resolve(import.meta.dir, "../.."),
        env: {
          ...process.env,
          PATH: `${bin}:${process.env.PATH ?? ""}`,
          PUBLIC_LEAK_CHECK: check,
          [CHILD]: "1",
        },
      }
    );
    const report = `${child.stdout.toString()}${child.stderr.toString()}`;
    expect({ code: child.exitCode, report }).toEqual({
      code: 0,
      report: expect.stringMatching(/^ 1 pass$[\s\S]*^ 0 fail$/m),
    });
    // Both pushes the world makes went through the stand-in, each under the check: its base
    // (fixture_first_commit) and the pull request's branch (fixture_head_commit).
    expect(readFileSync(pushes, "utf8")).toBe(`main ${check}\nlegion/LWEVAL-1 ${check}\n`);
  });
});

// Every worker run's Envoy role topics are its Legion project's and its issue's, and worker_boot
// gives all three worker scenarios the same issue key. So the project is the only thing that keeps
// one run's Envoy messages out of another's session, and `skills<label>r<n>` does not: it names no
// scenario. Measured on 2026-11-06, with the batch's NATS given through SKILL_SCENARIOS_NATS_URL
// and so left running between batches, a run of the scenario threads-closed replaced, numbered 2
// of the label head, was delivered the Envoy message the reviewer run
// reviewer-text-findings-text-only-head-2 published 44 minutes earlier, under the project both
// were given, skillsheadr2: it named the bot thread PRRT_kwDOrig0001, which exists only in the
// reviewer scenario's world, and the agent resolved it. A run that reads another run's
// instructions measures nothing about its own rule.
describe("run_project", () => {
  const project = (scenario: string, runCase: string, label: string, n: string) =>
    bash(`${fn("run_project")}\nrun_project "$1" "$2" "$3" "$4"`, [scenario, runCase, label, n]);

  // Catches: a project built from the label and the run number alone, which two worker scenarios
  // of one label and number share, so Envoy delivers each run's messages to both.
  test("two scenarios of one label and run number get projects of their own", () => {
    const reviewer = project("reviewer-text-findings", "text-only", "head", "2");
    const closed = project("threads-closed", "correct", "head", "2");
    const tester = project("tester-proof", "", "head", "2");
    expect(reviewer.code).toBe(0);
    expect(closed.code).toBe(0);
    expect(tester.code).toBe(0);
    expect(new Set([reviewer.stdout, closed.stdout, tester.stdout]).size).toBe(3);
  });

  // Catches: a project that keeps a hyphen or an upper-case letter from the scenario, the case or
  // the label, which isLegionProjectToken refuses, so no role token can be minted for the run.
  test("each project is a Legion project token", () => {
    for (const [scenario, runCase, label] of [
      ["reviewer-text-findings", "text-only", "head"],
      ["threads-closed", "code-defect", "base"],
      ["tester-proof", "", "ablate-R2"],
    ] as const)
      expect(project(scenario, runCase, label, "3").stdout.trim()).toMatch(/^[a-z0-9]+$/);
  });

  // Catches: a project that drops the case, so the two cases of one scenario, which run as two
  // batches against the same NATS, share their role topics.
  test("two cases of one scenario get projects of their own", () => {
    expect(project("threads-closed", "correct", "head", "1").stdout).not.toBe(
      project("threads-closed", "code-defect", "head", "1").stdout
    );
  });
});

// A cased scenario's case reaches four places that must agree: cmd_batch refuses a case before it
// starts a service, cmd_run puts it in the run's directory name, the run writes it to
// <run>/case.txt, and score.ts reads the name and the file and holds them to each other. All four
// read it through scenario_case.
describe("scenario_case", () => {
  // scenario_case reads the rig's one scenario table (run-names.ts's SCENARIOS) through this
  // directory's run-names.ts.
  const cases = (scenario: string, env: Record<string, string> = {}) =>
    bash(
      `${fn("fail")}\n${fn("scenario_case")}\nhere=$2\nscenario_case "$1"`,
      [scenario, import.meta.dir],
      env
    );

  // Catches: a reviewer case the rig takes in one place and refuses in another.
  test("reviewer-text-findings takes text-only, defect and last-push", () => {
    for (const name of ["text-only", "defect", "last-push"])
      expect(cases("reviewer-text-findings", { SKILL_SCENARIOS_REVIEWER_CASE: name })).toEqual({
        code: 0,
        stdout: `${name}\n`,
        stderr: "",
      });
  });

  // Catches: a batch that runs the wrong world because the variable is unset or misspelt, which
  // would score as the reviewer's own verdict.
  test("an unset or unknown reviewer case fails, naming the variable and every case", () => {
    const unset: Record<string, string> = {};
    for (const env of [unset, { SKILL_SCENARIOS_REVIEWER_CASE: "text" }]) {
      const result = cases("reviewer-text-findings", env);
      expect(result.code).toBe(1);
      expect(result.stdout).toBe("");
      expect(result.stderr).toContain("SKILL_SCENARIOS_REVIEWER_CASE");
      expect(result.stderr).toContain("not text-only, defect or last-push");
    }
  });

  // Catches: a generalisation that drops askgate's own cases, or that gives an uncased scenario a
  // case and so a directory name no scorer reads.
  test("askgate keeps its cases, and a scenario that takes none prints nothing", () => {
    expect(cases("askgate", { SKILL_SCENARIOS_ASKGATE_CASE: "nochange-block" }).stdout).toBe(
      "nochange-block\n"
    );
    expect(cases("askgate", { SKILL_SCENARIOS_ASKGATE_CASE: "nope" }).code).toBe(1);
    expect(cases("tester-proof", { SKILL_SCENARIOS_REVIEWER_CASE: "defect" })).toEqual({
      code: 0,
      stdout: "",
      stderr: "",
    });
  });

  // Catches: a threads-closed batch that runs a case its check did not name — an unset variable
  // read as a default, which v47's k2 and k3 never leave unset — or one that takes a misspelt case,
  // or the retired text-fixes, for a measured one.
  test("threads-closed takes its four cases, and fails unset and on any other word", () => {
    for (const name of ["accept", "correct", "crossing", "code-defect"])
      expect(cases("threads-closed", { SKILL_SCENARIOS_THREADS_CASE: name })).toEqual({
        code: 0,
        stdout: `${name}\n`,
        stderr: "",
      });
    const unset: Record<string, string> = {};
    for (const env of [unset, { SKILL_SCENARIOS_THREADS_CASE: "text-fixes" }]) {
      const result = cases("threads-closed", env);
      expect(result.code).toBe(1);
      expect(result.stdout).toBe("");
      expect(result.stderr).toContain("SKILL_SCENARIOS_THREADS_CASE");
      expect(result.stderr).toContain("not accept, correct, crossing or code-defect");
    }
  });

  // Catches: a batch that starts its services and its runs before it reads the case (cmd_batch
  // calls scenario_case before it loads a label or locks the services).
  test("a batch of a cased scenario refuses an unset case before it starts anything", () => {
    const dir = freshDir("reviewer-batch");
    const result = spawnChecked(
      ["bash", path.join(import.meta.dir, "rig.sh"), "batch", "reviewer-text-findings", "1", "x"],
      { env: { ...childEnv, SKILL_SCENARIOS_WORK: dir } }
    );
    expect(result.exitCode).toBe(1);
    expect(result.stderr.toString()).toContain("SKILL_SCENARIOS_REVIEWER_CASE");
    expect(existsSync(path.join(dir, "services"))).toBe(false);
  });
});

// The three records a worker run's score reads what the run did from: the pushes, the gh and bun
// stand-ins' calls, and the daemon stand-in's answers. The fixture leaves the push log empty,
// because the remote's post-receive hook has already written its own two pushes into it and
// those are the world being built, not acts of the run; it creates neither of the other two,
// because each is opened by the stand-in that keeps it, with one line of its own, and a record
// that is not there at all is the score's way of saying nothing wrote it (score.ts's
// `unscored`: a missing record is a rig error naming it, one holding no line of the run's own a
// reading of the run). Catches: a rig that pre-creates a record, which made a
// stand-in that never ran read as a run that called nothing — a code-defect head passed on an
// empty calls.jsonl, and the missing-file rig error fired only for a run that never got a
// world.
describe("fixture_head_commit", () => {
  test("it empties the push log and creates no record of its own", () => {
    const dir = freshDir("fixture-records");
    writeFileSync(path.join(dir, "pushes.log"), "the fixture's own push\n");
    const result = bash(
      `${fn("fixture_head_commit")}\nfixture_git() { :; }\nR=$1 worker_key=LWEVAL-1\nfixture_head_commit`,
      [dir]
    );
    expect(result.code).toBe(0);
    expect(readFileSync(path.join(dir, "pushes.log"), "utf8")).toBe("");
    for (const file of ["calls.jsonl", "daemon.log"])
      expect(existsSync(path.join(dir, file))).toBe(false);
  });
});

// The two text findings a round's world carries are named constants (rig.sh's
// text_finding_doc_line and text_finding_trigger), and every world passes round_world the pair it
// means to have. round_world takes no default, because a default is what made the negative
// control's own text invisible: `${5-…}` kept the push-trigger comment out of the code-defect
// world through an unset fifth argument, and `${5:-…}` would have put it back, adding a second
// text-only finding to the control with no test anywhere to notice.
describe("round_world and the worlds' own text findings", () => {
  /** WORLD built with GUARD, with round_world stubbed: the arguments it was given, and what the
   * stub had to set for the rest of the world to build (round_world's own outputs). */
  function built(
    world: string,
    guard: string,
    worldCase = guard === "" ? "correct" : "code-defect"
  ) {
    const dir = freshDir(`world-text-${world}`);
    const result = bash(
      [
        fn("fail"),
        fn(world),
        fn("resolution_protection_routes"),
        fn("concluded_checks"),
        fn("base_ruleset_routes"),
        'round_world() { printf "%s\\n" "$@" >"$R/round-args"; C1=0123456789abcdef0123;',
        "  worker_pr=1001; worker_pr_url=https://example.invalid/pull/1001; }",
        "fixture_reviewer_commit() { :; }",
        "fixture_body() { :; }",
        "fixture_checks() { :; }",
        "fixture_write() { :; }",
        "R=$1 worker_key=LWEVAL-1 worker_repo=example/widgets worker_base=main",
        "text_finding_doc_line=THE-DOC-LINE text_finding_trigger=THE-TRIGGER",
        `${world} 1 100 "$2" "$3"`,
      ].join("\n"),
      [dir, guard, worldCase]
    );
    expect(result).toMatchObject({ code: 0, stderr: "" });
    const written = readFileSync(path.join(dir, "round-args"), "utf8");
    return written.split("\n").slice(0, -1);
  }

  // Catches: a world that takes a finding from a default rather than naming it, so an edit to the
  // default moves a world that never mentions it.
  test("each world names the text findings it carries", () => {
    expect(built("review_world", "")).toEqual(["1", "100", "", "THE-DOC-LINE", "THE-TRIGGER"]);
    expect(built("review_world", "dropped")).toEqual([
      "1",
      "100",
      "dropped",
      "THE-DOC-LINE",
      "THE-TRIGGER",
    ]);
    // last-push's world is text-only's on another base, with text-only's two findings.
    expect(built("review_world", "", "last-push")).toEqual([
      "1",
      "100",
      "",
      "THE-DOC-LINE",
      "THE-TRIGGER",
    ]);
    expect(built("approved_world", "")).toEqual(["1", "100", "", "THE-DOC-LINE", "THE-TRIGGER"]);
    // accept's workflow comment is honest, so its one text finding is the document's, which the
    // reviewer's own commit closes; crossing's world is accept's but for the check.
    expect(built("approved_world", "", "accept")).toEqual(["1", "100", "", "THE-DOC-LINE", ""]);
    expect(built("approved_world", "", "crossing")).toEqual(["1", "100", "", "THE-DOC-LINE", ""]);
  });

  // Catches: a negative control that quietly gains a text finding. Its one finding is the code's,
  // and the fifth argument it passes is the empty trigger — an honest workflow comment — which is
  // a finding it is the control for not having.
  test("the dropped guard's world documents R2 and carries an honest workflow comment", () => {
    expect(built("approved_world", "dropped")).toEqual([
      "1",
      "100",
      "dropped",
      "A blank name, like no name at all, prints the usage line on stderr and exits 2.",
      "",
    ]);
  });

  // Catches: a round_world that answers a caller naming no text at all, which is the default
  // coming back.
  test("round_world refuses a call that names no doc line and trigger", () => {
    const result = bash(`${fn("fail")}\n${fn("round_world")}\nround_world 1 100 ''`);
    expect(result.code).toBe(1);
    expect(result.stderr).toContain("expected INDEX START GUARD DOC_LINE TRIGGER");
  });
});

// What worker_boot hands the daemon stand-in and the pane. Catches: a project computed a second
// time here
// rather than taken from the run (the recorder would subscribe to one project's topics while the
// agents published on another's, and every send would read as never made); a repository or a
// branch handed to either of them here, which is a second place the world's own names could
// drift from the world (rig.sh's worker world comment: both read $R/world.json, so the boot
// hands each the run directory and no name of its own); and a boot that names no phase, which
// serves the role's own phase to a loop the daemon starts no phase for.
describe("worker_boot", () => {
  /** The `gh` rig.sh's standins leaves in $R/bin: the stand-in behind a wrapper, as the pane's
   * PATH reaches it, naming the run `run` it records under as the bundle's banner does (no
   * variable of the pane names it), with the arguments it was given kept beside the run so a row
   * can see what the boot ran it for. */
  const recordingGh = (dir: string, run = dir) =>
    [
      "#!/bin/sh",
      `printf '%s\\n' "$*" >>"${path.join(dir, "pane-gh-args")}"`,
      `GH_DATA_DIR='${run}' exec "${process.execPath}" "${GH_STANDIN}" gh "$@"`,
      "",
    ].join("\n");

  /** worker_boot for ROLE in PHASE, with every process it starts stubbed, boot_agent's launch of
   * the agent among them (its arguments kept in boot-agent-args), on a run whose world
   * fixture_write wrote, and the boot's own result unjudged. `gh` writes the pane's own gh, and
   * `panePath` the PATH the pane file carries, which is where the boot looks for it: a row breaks
   * one of the two, or writes a gh that names no run, to say what a run whose agents could reach
   * no stand-in must leave behind. `before` is the role of an
   * agent the run booted and ran before this one, whose output is the run's out.txt. Each process
   * started keeps the run's boot record as it stood then (booted-at-<process>), and each `bun`
   * the build of daemon-pane.go it was given (bun-builds) and its work directory (bun-works).
   * `remote` puts an issue workspace's remote helper (world_remote) in the run before the boot. */
  function bootResult(
    role: string,
    phase: string,
    {
      completed = "",
      gh = recordingGh,
      panePath = (dir: string) => `${path.join(dir, "bin")}:${childEnv.PATH}`,
      before = "",
      remote = false,
    } = {}
  ) {
    const dir = freshDir(`worker-boot-${role}`);
    mkdirSync(path.join(dir, "logs"), { recursive: true });
    mkdirSync(path.join(dir, "bin"), { recursive: true });
    writeFileSync(path.join(dir, "logs", "daemon.log"), "listening on http://127.0.0.1:4321\n");
    if (before !== "") {
      writeFileSync(path.join(dir, "out.txt"), `the ${before} is done\nexit=0\n`);
      writeFileSync(path.join(dir, "booted"), `boot ${before}\n`);
      mkdirSync(path.join(dir, "home"));
    }
    writeFileSync(path.join(dir, "bin", "gh"), gh(dir), { mode: 0o755 });
    if (remote)
      writeFileSync(path.join(dir, "bin", `git-remote-${WORLD_REMOTE_VCS}`), "a helper\n", {
        mode: 0o755,
      });
    // The pane the daemon would give, carrying the Envoy address a pane whose environment
    // configures none gets (daemon-pane.ts's DEFAULT_ENVOY_URL), the PATH it hands the agent
    // (daemon-pane.ts's `path`, the run's own bin first), and the batch's services file,
    // carrying the listener it started: the two addresses the run's pane.env is built from.
    writeFileSync(
      path.join(dir, "worker.env"),
      `ENVOY_URL=http://127.0.0.1:9020\nPI_NO_TITLE=1\nPATH=${panePath(dir)}\n`
    );
    writeFileSync(
      path.join(dir, "services.env"),
      "ENVOY_URL=http://127.0.0.1:34567\nENVOY_NATS_URL=nats://127.0.0.1:4222\n"
    );
    const result = bash(
      [
        fn("fail"),
        fn("build_dir"),
        fn("worker_boot"),
        fn("role_identity"),
        fn("role_env"),
        fn("git_pairs"),
        fn("adopt_workspace"),
        fn("world_remote_app"),
        fn("fixture_write"),
        fn("concluded_checks"),
        fn("base_ruleset_routes"),
        'start_process() { printf "%s\\n" "$*" >"$R/started-$1"; cp "$R/booted" "$R/booted-at-$1"; [ "$1" != daemon ] || echo "listening on http://127.0.0.1:4321" >"$R/logs/daemon.log"; }',
        "await_start() { :; }",
        "stop_pid() { :; }",
        "base_env() { :; }",
        "standins() { :; }",
        'boot_agent() { printf "%s\\n" "$*" >"$R/boot-agent-args"; }',
        `bun() { printf "%s\\n" "$*" >>"$R/bun-args"; printf "%s\\n" "\${SKILL_SCENARIOS_DAEMON_PANE:-}" >>"$R/bun-builds"; printf "%s\\n" "\${SKILL_SCENARIOS_WORK:-}" >>"$R/bun-works"; }`,
        `R=$1 here=$2 worker_key=LWEVAL-1 worker_repo=$3 worker_base=main booted='${before}' daemon_pid=''`,
        "C1=0123456789abcdef0123 body=a-body worker_pr=1001",
        "worker_pr_url=https://example.invalid/pull/1001",
        "services_env=$R/services.env work=$R/work tokens=$R/work/services/tokens dispatch_port=1 profile=$R/p co=$R/c",
        assignment("world_author_name"),
        assignment("world_author_email"),
        assignment("world_login"),
        ...WORLD_APPS.map(assignment),
        assignment("world_remote_vcs"),
        "fixture_write '[]' '[]'",
        'worker_boot the-run "$4" "$5" "$6" "$7"',
      ].join("\n"),
      [dir, import.meta.dir, "example/widgets", "skillsproject1", role, phase, completed]
    );
    return { dir, result };
  }

  /** A boot that went through: the run directory, what it handed the daemon stand-in, what it
   * handed the pane (worker-pane.ts, through the stubbed `bun`), and that world's own
   * repository and branch. */
  function boot(role: string, phase: string, completed = "") {
    const { dir, result } = bootResult(role, phase, { completed });
    expect(result).toMatchObject({ code: 0, stderr: "" });
    const bunArgs = readFileSync(path.join(dir, "bun-args"), "utf8").trim().split("\n");
    return {
      dir,
      started: readFileSync(path.join(dir, "started-daemon"), "utf8").trim(),
      bunArgs,
      pane: bunArgs.find((line) => line.includes("worker-pane.ts")) ?? "",
      world: JSON.parse(readFileSync(path.join(dir, "world.json"), "utf8")),
    };
  }

  // Catches: a rig word in the identity a worker agent's own jj commits carry, which its push puts
  // into the history every later role reads (`Rig Worker`, as the pane once named it): it is its
  // role's App, as a pod's worker commits as, in the pane file `launch` hands the agent (jj's
  // and git's variables both) and in the jj configuration it names, and the class of what the
  // rig writes into a repository holds it to no word of the rig's ("what the rig writes into a
  // run's world" below). Two roles of two Apps differ.
  test("the agent's identity is its role's App, in the pane and in its jj configuration", () => {
    const { dir } = boot("implementer", "reviewing");
    const pane = readFileSync(path.join(dir, "pane.env"), "utf8").split("\n");
    const identity = pane.filter((line) =>
      /^(JJ_(USER|EMAIL)|GIT_(AUTHOR|COMMITTER)_(NAME|EMAIL))=/.test(line)
    );
    const implementer = "widgets-implementer[bot]";
    const implementerEmail = `318406215+${implementer}@users.noreply.github.com`;
    expect(identity).toEqual([
      `JJ_USER=${implementer}`,
      `JJ_EMAIL=${implementerEmail}`,
      `GIT_AUTHOR_NAME=${implementer}`,
      `GIT_AUTHOR_EMAIL=${implementerEmail}`,
      `GIT_COMMITTER_NAME=${implementer}`,
      `GIT_COMMITTER_EMAIL=${implementerEmail}`,
    ]);
    const config = readFileSync(path.join(dir, "jj.toml"), "utf8");
    expect(config).toContain(`name = "${implementer}"`);
    expect(config).toContain(`email = "${implementerEmail}"`);
    expect([...identity, config].filter((text) => /fixture|\brig\b/i.test(text))).toEqual([]);
    const reviewer = readFileSync(path.join(boot("reviewer", "reviewing").dir, "pane.env"), "utf8")
      .split("\n")
      .filter((line) => /^JJ_(USER|EMAIL)=/.test(line));
    expect(reviewer).toEqual([
      "JJ_USER=widgets-reviewer[bot]",
      "JJ_EMAIL=318406277+widgets-reviewer[bot]@users.noreply.github.com",
    ]);
  });

  // Catches: a boot that leaves the issue workspace's remote helper presenting another role's
  // App, so the remote takes or refuses a push by the App of the role booted before it, or by
  // the implement App the world was built with (world_remote_app): the reviewer's workflow push
  // would be taken where GitHub refuses the review App's. A run with no helper gets none.
  test("the remote helper presents the booted role's App", () => {
    const helperOf = (dir: string) =>
      readFileSync(path.join(dir, "bin", `git-remote-${WORLD_REMOTE_VCS}`), "utf8");
    const booted = (role: string) => {
      const { dir, result } = bootResult(role, "reviewing", { remote: true });
      expect(result).toMatchObject({ code: 0, stderr: "" });
      return helperOf(dir).split("\n")[2];
    };
    expect(booted("reviewer")).toBe("export GITHUB_APP=widgets-reviewer");
    expect(booted("tester")).toBe("export GITHUB_APP=widgets-reviewer");
    expect(booted("implementer")).toBe("export GITHUB_APP=widgets-implementer");
    const { dir } = boot("reviewer", "reviewing");
    expect(existsSync(path.join(dir, "bin", `git-remote-${WORLD_REMOTE_VCS}`))).toBe(false);
  });

  // Catches: the pane's own ENVOY_URL left in beside the batch's, which is one fact in two
  // lines of one file. `launch` runs the agent under `env -i` with those lines, so the address
  // the session reaches is whichever came last, and a reader of pane.env is told two. The
  // run's Envoy is the listener the batch started, and the daemon's pane default is no address
  // of this rig's.
  test("the pane file names the batch's Envoy listener, once", () => {
    const { dir } = boot("implementer", "accepting");
    const pane = readFileSync(path.join(dir, "pane.env"), "utf8").split("\n");
    expect(pane.filter((line) => line.startsWith("ENVOY_URL="))).toEqual([
      "ENVOY_URL=http://127.0.0.1:34567",
    ]);
    // The rest of the pane the daemon gave is kept whole.
    expect(pane).toContain("PI_NO_TITLE=1");
    expect(pane).toContain("ENVOY_NATS_URL=nats://127.0.0.1:4222");
  });

  // Catches: a pane whose git reads none of the settings production's runtime gives every role
  // process (GIT_CONFIG_COUNT=0, as the pane held), so `git config -l --show-scope` lists none of
  // the `command` lines a pod's lists, and a pane that names them in other words than the
  // runtime's (runtime/sandbox's gitConfigPairs, manifest.go), which an agent's `env` shows.
  test("the pane carries production's five git settings, once and in the runtime's words", () => {
    const { dir } = boot("implementer", "reviewing");
    const pane = readFileSync(path.join(dir, "pane.env"), "utf8").split("\n");
    expect(pane.filter((line) => line.startsWith("GIT_CONFIG"))).toEqual([
      "GIT_CONFIG_COUNT=5",
      "GIT_CONFIG_KEY_0=credential.https://github.com.helper",
      "GIT_CONFIG_VALUE_0=",
      "GIT_CONFIG_KEY_1=credential.https://github.com.helper",
      "GIT_CONFIG_VALUE_1=!legion git-credential",
      "GIT_CONFIG_KEY_2=credential.https://github.com.useHttpPath",
      "GIT_CONFIG_VALUE_2=true",
      "GIT_CONFIG_KEY_3=core.fsmonitor",
      "GIT_CONFIG_VALUE_3=",
      "GIT_CONFIG_KEY_4=core.hooksPath",
      "GIT_CONFIG_VALUE_4=/dev/null",
    ]);
  });

  // Catches: a run whose record of its calls nothing opens, where a run that called no gh and
  // no bun leaves no calls.jsonl at all and scores as the rig error of a record nothing wrote
  // rather than as the run that called nothing (score.ts's `unscored`). The line is the gh
  // stand-in's own, which gh-standin.test.ts holds it to; this is the boot asking for it,
  // before the agent starts — and asking the gh the pane's PATH resolves to, in the pane's own
  // environment, not the stand-in script by its path, so the line says the run's own gh reached
  // the stand-in. The record lands under the run the pane file names and the boot adds nothing
  // to that environment, so a record at all is that environment having been the agent's.
  test("the gh the pane's PATH resolves to opens the record of the run's calls", () => {
    const { dir, result } = bootResult("merger", "merging");
    expect(result).toMatchObject({ code: 0, stderr: "" });
    // The arguments the pane's own gh was run with, which the wrapper kept: the flag alone.
    expect(existsSync(path.join(dir, "pane-gh-args"))).toBe(true);
    expect(readFileSync(path.join(dir, "pane-gh-args"), "utf8")).toBe("--open\n");
    expect(existsSync(path.join(dir, "calls.jsonl"))).toBe(true);
    expect(
      readFileSync(path.join(dir, "calls.jsonl"), "utf8")
        .split("\n")
        .filter((line) => line !== "")
        .map((line) => JSON.parse(line) as unknown)
    ).toMatchObject([{ opened: "calls.jsonl" }]);
    // The stand-in is reached through that wrapper and never run by its own path.
    expect(readFileSync(path.join(dir, "bun-args"), "utf8")).not.toContain("gh-standin.ts");
  });

  // Catches: an opening line the rig writes where no call of the run could have been recorded.
  // The record is opened by running the pane's own gh, so a wrapper that cannot start leaves no
  // record at all, which score.ts's `unscored` reads as the rig error it is (score.test.ts's
  // "reviewer-text-findings: a missing stand-in record is a rig error"), rather than a record
  // holding the opening line alone, which it reads as the run that called nothing
  // ("reviewer-text-findings: an empty stand-in record is a reading of the run, named in its row").
  test("a pane gh that cannot start opens no record, and the boot says so", () => {
    // A wrapper that cannot start: the interpreter it execs is not there, as a $R/bin/gh
    // written against a bun that has gone would be.
    const { dir, result } = bootResult("merger", "merging", {
      gh: () => '#!/bin/sh\nexec /nonexistent/bun /nonexistent/gh-standin.ts gh "$@"\n',
    });
    expect(result.code).not.toBe(0);
    expect(result.stderr).toContain(`skill-scenarios: worker_boot: the pane's gh (${dir}/bin/gh)`);
    expect(existsSync(path.join(dir, "calls.jsonl"))).toBe(false);
  });

  // The other half of that rule: a pane PATH the stand-in is not on. Every call the agent made
  // would reach the real gh or nothing, and none of them would be recorded, so the record stays
  // absent here too.
  test("a pane PATH that reaches no gh opens no record, and the boot says so", () => {
    const { dir, result } = bootResult("merger", "merging", {
      panePath: (run: string) => path.join(run, "empty"),
    });
    expect(result.code).not.toBe(0);
    expect(result.stderr).toContain(
      `skill-scenarios: worker_boot: ${dir}/pane.env names no PATH with a gh on it`
    );
    expect(existsSync(path.join(dir, "calls.jsonl"))).toBe(false);
  });

  // The third way the opening line could say what is not so: a gh that names no run. The
  // stand-in records under the run its bundle's banner names (standins), which no variable of
  // the pane carries, so a bundle that lost it records no call of the agent's either, and leaves
  // no record here.
  test("a gh that names no run opens no record, and the boot says so", () => {
    const { dir, result } = bootResult("merger", "merging", {
      gh: (run: string) => recordingGh(run, ""),
    });
    expect(result.code).not.toBe(0);
    expect(result.stderr).toContain("gh: GH_DATA_DIR is unset");
    expect(result.stderr).toContain(`skill-scenarios: worker_boot: the pane's gh (${dir}/bin/gh)`);
    expect(existsSync(path.join(dir, "calls.jsonl"))).toBe(false);
  });

  test("the stand-in is given the run's project, the run's own directory and the phase", () => {
    const { dir, started, world } = boot("implementer", "accepting");
    expect(started.split(" ").slice(-5)).toEqual([
      "skillsproject1",
      "LWEVAL-1",
      dir,
      "implementer",
      "accepting",
    ]);
    // The repository is no argument of the boot: the stand-in reads it, and the branch beside
    // it, out of the world in that directory.
    expect(started.split(" ")).not.toContain(world.repo);
    expect(world).toMatchObject({ repo: "example/widgets", base: "main" });
  });

  // Catches: a stand-in started without the rig's work directory, which keeps no facts cache
  // there and so runs `go run` for each fact at every boot (daemon-pane.ts's `factsCache`).
  test("the stand-in is given the rig's work directory for its facts cache", () => {
    const { dir, started } = boot("tester", "");
    expect(started.split(" ").slice(0, 3)).toEqual([
      "daemon",
      "env",
      `SKILL_SCENARIOS_WORK=${dir}/work`,
    ]);
  });

  // Catches: a world statement that never reaches the stand-in. threads-closed's implementer
  // completed its round before the run began, which is what makes either label's daemon refuse
  // its completion 409 HANDOFF_NOT_CURRENT_PHASE (daemon-standin.ts's COMPLETED_ROWS); a boot
  // that dropped COMPLETED would have the head stand-in take the implementer as a role that left
  // its phase with nothing on its row.
  test("the stand-in is given the roles the world says completed their phase", () => {
    const { started } = boot("implementer", "reviewing", "implementer");
    expect(started.split(" ").slice(-2)).toEqual(["reviewing", "implementer"]);
  });

  // The pane's addressing is the world's own, as the stand-in's record is, and worker-pane.ts
  // reads both out of the world in the run directory it is given first. A worker follows
  // the addressing its system prompt carries: the pane named skills-rig/project on trunk while
  // every world is example/widgets on main, and recorded runs posted their review to the
  // repository the addressing named, where the scorer's repository rule read it as no review at
  // all (rig.sh's worker world comment holds that story).
  test("the pane is given the run directory and no repository or branch of its own", () => {
    const { dir, pane, world } = boot("reviewer", "accepting");
    const given = pane.split(" ");
    expect(given[1]).toBe(dir);
    expect(given.at(-1)).toBe(`${dir}/c`);
    expect(given).not.toContain(world.repo);
    expect(given).not.toContain(world.base);
  });

  // A boot that names no phase leaves the phase to the stand-in, which serves the role's own.
  test("a boot that names no phase hands the stand-in an empty one", () => {
    const { dir, started } = boot("tester", "");
    expect(started.split(" ").slice(-2)).toEqual([dir, "tester"]);
  });

  // Catches: a worker launched by a bare `launch`, which no claim boot refusal relaunches
  // (boot_agent's rows in "launch" below hold the relaunch itself), or launched as another role,
  // whose record the relaunch would read for the wrong stand-in's lines.
  test("the agent is launched through boot_agent, with the run's name and its role", () => {
    const { dir } = boot("reviewer", "reviewing");
    expect(readFileSync(path.join(dir, "boot-agent-args"), "utf8")).toBe("the-run reviewer\n");
  });

  // The run's boot record and the label's build of daemon-pane.go, which every boot carries.
  describe("boot record and daemon facts", () => {
    /** The label's build of daemon-pane.go under the run's work directory, where rig.sh's
     * daemon_pane_binary keeps it for the label's checkout ($R/c here). */
    const paneBuild = (dir: string) =>
      path.join(
        dir,
        "work",
        "builds",
        new Bun.CryptoHasher("sha256").update(path.join(dir, "c")).digest("hex").slice(0, 16),
        "daemon-pane"
      );

    // Catches: a boot left off the run's record, and one recorded only once its stand-in
    // listened, which leaves a boot whose stand-in timed out (a measured threads-closed head
    // run's reviewer) off the record that health reads it on.
    test("the boot is on the run's boot record before its stand-in starts", () => {
      const { dir } = boot("tester", "");
      expect(readFileSync(path.join(dir, "booted-at-daemon"), "utf8")).toBe("boot tester\n");
      expect(readFileSync(path.join(dir, "booted"), "utf8")).toBe("boot tester\n");
    });

    // Catches: a second agent's boot that replaces the first's record, and the first agent's
    // output left standing as out.txt for a second agent that never ran, which reads as that
    // agent's exit.
    test("a second agent's boot adds its line, and moves the first agent's output aside", () => {
      const { dir, result } = bootResult("reviewer", "reviewing", { before: "implementer" });
      expect(result).toMatchObject({ code: 0, stderr: "" });
      expect(readFileSync(path.join(dir, "booted"), "utf8")).toBe(
        "boot implementer\nboot reviewer\n"
      );
      expect(readFileSync(path.join(dir, "out-implementer.txt"), "utf8")).toBe(
        "the implementer is done\nexit=0\n"
      );
      expect(existsSync(path.join(dir, "out.txt"))).toBe(false);
    });

    // Catches: a stand-in or a pane composed by `go run` of daemon-pane.go, a compile of the
    // daemon module per read (the boot of the 2026-11-15 preflight that timed out), or by a build
    // of any checkout but the label's.
    test("the stand-in and the pane are given the label's build of daemon-pane.go", () => {
      const { dir, started, bunArgs } = boot("reviewer", "reviewing");
      expect(started.split(" ").slice(0, 4)).toEqual([
        "daemon",
        "env",
        `SKILL_SCENARIOS_WORK=${dir}/work`,
        `SKILL_SCENARIOS_DAEMON_PANE=${paneBuild(dir)}`,
      ]);
      const builds = readFileSync(path.join(dir, "bun-builds"), "utf8").split("\n");
      const pane = bunArgs.findIndex((line) => line.includes("worker-pane.ts"));
      expect(pane).toBeGreaterThanOrEqual(0);
      expect(builds[pane]).toBe(paneBuild(dir));
    });

    // Catches: a run whose gh refuses an owner outside its world's project by no table, or by a
    // table read anew at the boot rather than the one services_up read into the work directory's
    // facts cache, or by any checkout's but the label's; the agent's namespace shows neither the
    // checkout nor the cache, so the table is written into the run before the agent starts.
    test("the run's gh is given its label's gh selectors, out of the work directory's facts cache", () => {
      const { dir, bunArgs } = boot("reviewer", "reviewing");
      const works = readFileSync(path.join(dir, "bun-works"), "utf8").split("\n");
      const at = bunArgs.indexOf(
        `${import.meta.dir}/daemon-pane.ts gh-selectors ${dir}/c ${dir}/gh-selectors.json`
      );
      expect(at).toBeGreaterThanOrEqual(0);
      expect(works[at]).toBe(`${dir}/work`);
      expect(at).toBeLessThan(bunArgs.findIndex((line) => line.includes("worker-pane.ts")));
    });
  });
});

// Catches: a recorder that computes a project of its own instead of taking the run's. It would
// subscribe to the topics of a project nothing publishes on, and every message of the run would
// read as never sent — a silent failure of each row, not an error of the rig.
describe("start_recorder", () => {
  test("it subscribes to the topics of the project it was given", () => {
    const dir = freshDir("recorder");
    writeFileSync(path.join(dir, "services.env"), "ENVOY_NATS_URL=nats://127.0.0.1:4222\n");
    const result = bash(
      [
        fn("start_recorder"),
        'start_process() { printf "%s\\n" "$*" >"$R/started"; }',
        "await_start() { :; }",
        "R=$1 here=$2 services_env=$R/services.env",
        'start_recorder "$3"',
      ].join("\n"),
      [dir, import.meta.dir, "skillsproject1"]
    );
    expect(result).toMatchObject({ code: 0, stderr: "" });
    expect(readFileSync(path.join(dir, "started"), "utf8").trim()).toEndWith(
      "legion-skillsproject1-"
    );
  });
});

// The second half of the implementer's last chance. In accept and crossing the implementer's
// answer is the run's one half. In code-defect the implementer tells the architect and the
// reviewer (Verification v47's k2 negative control), so a wake of none is a run that already
// fails that message, and booting a reviewer on it would only score an agent the case's terms no
// longer reach. Catches: a rig that boots a reviewer on an empty prompt anyway, or none on a wake
// it was sent. In correct the implementer must have sent a wake, so the reviewer is booted
// whatever it sent and the empty wake stays the fail on toldReviewer it already was.
describe("run_threads_closed", () => {
  /** The loop run for CASE on a wake of WAKE, with both agents and the services stubbed: each
   * worker_boot it made, in order, the prompt the reviewer half would have been given, the
   * record of that wake the run keeps, and the run's boot record, to which the stubbed
   * worker_boot adds the line the real one writes. */
  function loop(threadsCase: string, wake: string) {
    const dir = freshDir(`threads-loop-${threadsCase}`);
    writeFileSync(path.join(dir, "threads.json"), "[]\n");
    writeFileSync(path.join(dir, "wake.json"), wake);
    const result = bash(
      [
        fn("note"),
        fn("fail"),
        fn("skip_reason"),
        fn("run_threads_closed"),
        fn("steps_seed"),
        fn("label_model"),
        "start_recorder() { :; }",
        "worker_run_world() { pr=1001; head=0123456789ab; }",
        "seed_ts() { :; }",
        "judge_ts() { :; }",
        "await_start() { :; }",
        'reviewer_wake() { cat "$R/wake.json"; }',
        'worker_boot() { printf "%s\\n" "$*" >>"$R/boots"; printf "boot %s\\n" "$3" >>"$R/booted"; }',
        "R=$1 here=$3 worker_key=LWEVAL-1 worker_repo=example/widgets",
        'run_threads_closed the-run skillsproject1 1 100 "$2"',
      ].join("\n"),
      [dir, threadsCase, import.meta.dir]
    );
    expect(result).toMatchObject({ code: 0, stderr: "" });
    return {
      boots: readFileSync(path.join(dir, "boots"), "utf8").trim().split("\n"),
      prompt: readFileSync(path.join(dir, "prompt.txt"), "utf8"),
      wake: JSON.parse(readFileSync(path.join(dir, "reviewer-wake.json"), "utf8")),
      booted: readFileSync(path.join(dir, "booted"), "utf8"),
    };
  }

  test("code-defect boots no reviewer when the implementer sent it nothing", () => {
    expect(loop("code-defect", "[]\n").boots).toEqual([
      "the-run-implementer skillsproject1 implementer accepting implementer",
    ]);
  });

  test("code-defect boots the reviewer when the implementer woke it", () => {
    expect(loop("code-defect", '[{"message":"pushed 0123456789ab"}]\n').boots).toEqual([
      "the-run-implementer skillsproject1 implementer accepting implementer",
      "the-run-reviewer skillsproject1 reviewer accepting implementer",
    ]);
  });

  // Catches, beside the empty wake: a correct loop booted in `accepting`, where no approval stands
  // and main's engine leaves the issue in `reviewing` (rig.sh's approved_world), so the approval of
  // the head the implementer's push made, which ends the round, is a path no run of this rig could
  // take. code-defect stays in `accepting` (the two rows above): its approved head is `CLEAN`, and
  // its control reads the messages to the architect and the reviewer as the one route, which a
  // `REQUEST_CHANGES` in `reviewing` would not leave it.
  test("correct boots both halves in reviewing, and the reviewer on an empty wake, which it fails on", () => {
    expect(loop("correct", "[]\n").boots).toEqual([
      "the-run-implementer skillsproject1 implementer reviewing implementer",
      "the-run-reviewer skillsproject1 reviewer reviewing implementer",
    ]);
  });

  // Catches: an accept or crossing loop booted in `accepting`, where the reviewer has approved
  // nothing and the issue stands in `reviewing` (Verification v47's k2 and k3); and one that boots
  // a reviewer on whatever the implementer sent, which neither case scores.
  test("accept and crossing boot the implementer in reviewing and no reviewer, whatever the implementer sent", () => {
    for (const threadsCase of ["accept", "crossing"])
      for (const wake of ["[]\n", '[{"message":"pushed 0123456789ab"}]\n'])
        expect([threadsCase, loop(threadsCase, wake).boots]).toEqual([
          threadsCase,
          ["the-run-implementer skillsproject1 implementer reviewing implementer"],
        ]);
  });

  // Catches: a world whose seeds record the tester's pass at the pull request's head, the
  // reviewer's commit, where v47's k2 holds it at the head below it and no result at the
  // reviewer's commit: run_threads_closed records it at the implementer's commit worker_run_world
  // leaves in $implemented.
  test("the tester's pass is seeded at the implementer's commit, below the reviewer's", () => {
    const dir = freshDir("threads-seeded-at");
    writeFileSync(path.join(dir, "threads.json"), "[]\n");
    writeFileSync(path.join(dir, "wake.json"), "[]\n");
    const result = bash(
      [
        fn("note"),
        fn("fail"),
        fn("skip_reason"),
        fn("run_threads_closed"),
        "start_recorder() { :; }",
        "worker_run_world() { pr=1001; head=0123456789ab; implemented=fedcba987654; }",
        'seed_ts() { printf "%s\\n" "$*" >>"$R/seeds"; }',
        "steps_seed() { :; }",
        "judge_ts() { :; }",
        "await_start() { :; }",
        'reviewer_wake() { cat "$R/wake.json"; }',
        "worker_boot() { :; }",
        "R=$1 here=$2 worker_key=LWEVAL-1 worker_repo=example/widgets",
        "run_threads_closed the-run skillsproject1 1 100 accept",
      ].join("\n"),
      [dir, import.meta.dir]
    );
    expect(result).toMatchObject({ code: 0, stderr: "" });
    expect(readFileSync(path.join(dir, "seeds"), "utf8").split("\n")[0]).toBe(
      `tester-result ${dir}/fixture.json ${dir}/test.json fedcba987654`
    );
  });

  // The implementer's prompt is the reviewer's message as spec v36's R1 has it send one: its
  // commit by SHA, what that commit changes and each thread it left open, and no approval, which
  // waits for the implementer's answer; in code-defect, where the reviewer pushed nothing, the
  // approval and its thread. accept and crossing share one reviewer commit, so one message.
  // Catches: a world whose implementer is told of an approval no world gives in those cases, and
  // code-defect's message moved off the approval it reports.
  test("each case's implementer is woken by the reviewer's message of its own commit, or of its approval in code-defect", () => {
    const implementerPrompt = (threadsCase: string) => {
      const dir = freshDir(`threads-prompt-${threadsCase}`);
      writeFileSync(
        path.join(dir, "threads.json"),
        threadsCase === "accept" || threadsCase === "crossing"
          ? "[]\n"
          : '[{"id":"PRRT_kwDOfix002","path":".github/workflows/greet.yml","body":"the comment names a push trigger"}]\n'
      );
      const result = bash(
        [
          fn("note"),
          fn("fail"),
          fn("run_threads_closed"),
          "start_recorder() { :; }",
          "worker_run_world() { pr=1001; head=0123456789ab; }",
          "seed_ts() { :; }",
          "steps_seed() { :; }",
          'worker_boot() { cp "$R/prompt.txt" "$R/implementer-prompt.txt"; exit 0; }',
          "R=$1 worker_key=LWEVAL-1 worker_repo=example/widgets",
          'run_threads_closed the-run skillsproject1 1 100 "$2"',
        ].join("\n"),
        [dir, threadsCase]
      );
      expect(result).toMatchObject({ code: 0, stderr: "" });
      return readFileSync(path.join(dir, "implementer-prompt.txt"), "utf8");
    };
    const shared =
      "Reviewer → implementer, LWEVAL-1. On PR #1001 (https://github.com/example/widgets/pull/1001, repository example/widgets) I pushed my text commit 0123456789ab, which rewrites the sentence of docs/cli.md the change made false, and rewords the example line docs/cli.md gives for `bun greet.ts Ada`. Threads I left open:\nnone\nOver to you.\n";
    expect(implementerPrompt("accept")).toBe(shared);
    expect(implementerPrompt("crossing")).toBe(shared);
    expect(implementerPrompt("correct")).toStartWith(
      "Reviewer → implementer, LWEVAL-1. On PR #1001 (https://github.com/example/widgets/pull/1001, repository example/widgets) I pushed my text commit 0123456789ab, which rewrites the sentence of docs/cli.md the change made false. Threads I left open:\n"
    );
    expect(implementerPrompt("correct")).not.toContain("approved");
    expect(implementerPrompt("correct")).toContain(
      "Threads I left open:\n- Thread PRRT_kwDOfix002, .github/workflows/greet.yml: the comment names a push trigger\nOver to you.\n"
    );
    expect(implementerPrompt("code-defect")).toStartWith(
      "Reviewer → implementer, LWEVAL-1. I approved PR #1001 (https://github.com/example/widgets/pull/1001, repository example/widgets) at 0123456789ab."
    );
  });

  // The reviewer's own wake is the implementer's message and nothing else (review-threads.md
  // step 2), and Envoy delivers each message of it, so the prompt is one delivery line per
  // message in the order Envoy accepted them. Catches: a prompt built from the last message
  // alone, which drops a wake the run did send; one that invents a head the implementer never
  // named; and a run that boots its reviewer on a prompt of some other run's making.
  test("the reviewer's prompt is one delivery line per message, in Envoy's own order", () => {
    const { prompt, wake } = loop(
      "correct",
      `${JSON.stringify([
        { message: "The text fixes are pushed: the head to read is 0123456789ab." },
        { message: "Both threads carry my reply." },
      ])}\n`
    );
    expect(prompt).toBe(
      "Implementer → reviewer, LWEVAL-1. The text fixes are pushed: the head to read is 0123456789ab.\n" +
        "Implementer → reviewer, LWEVAL-1. Both threads carry my reply.\n"
    );
    // <run>/reviewer-wake.json keeps what the reviewer was given and nothing else: the prompt is
    // in prompt.txt, which the agent is booted on.
    expect(wake).toEqual({
      wake: [
        { message: "The text fixes are pushed: the head to read is 0123456789ab." },
        { message: "Both threads carry my reply." },
      ],
    });
  });

  // Catches: a rig that writes the implementer's own prompt for the reviewer when the
  // implementer published nothing, which would hand the second half a wake nobody sent.
  test("an implementer that published nothing leaves the reviewer an empty prompt", () => {
    const { prompt, wake } = loop("correct", "[]\n");
    expect(prompt).toBe("");
    expect(wake).toEqual({ wake: [] });
  });

  // The run's boot record names the reviewer either way: booted, or skipped for the one reason
  // completion-answers.ts's SKIPS declares, which health and the score hold the line to.
  describe("boot record", () => {
    const declared = SKIPS["threads-closed"]?.["code-defect"]?.reviewer;
    const accepted = SKIPS["threads-closed"]?.accept?.reviewer;
    const crossed = SKIPS["threads-closed"]?.crossing?.reviewer;

    // Catches: a skip that fires although the implementer woke the reviewer, which would leave
    // a booted reviewer recorded as never booted.
    test("code-defect whose implementer woke the reviewer boots it and records no skip", () => {
      expect(loop("code-defect", '[{"message":"pushed 0123456789ab"}]\n').booted).toBe(
        "boot implementer\nboot reviewer\n"
      );
    });

    // Catches: a reviewer left unbooted with nothing on the record, and a skip whose reason is
    // the rig's own words rather than the one SKIPS declares.
    test("code-defect whose implementer sent the reviewer nothing records the skip, with SKIPS' reason", () => {
      expect(declared).toBeDefined();
      expect(loop("code-defect", "[]\n").booted).toBe(
        `boot implementer\nskip reviewer ${declared}\n`
      );
    });

    test("correct boots the reviewer on an empty wake, and records no skip", () => {
      expect(loop("correct", "[]\n").booted).toBe("boot implementer\nboot reviewer\n");
    });

    // Catches: an accept run whose unbooted reviewer is left off its record, or put there with a
    // reason SKIPS does not declare for the case.
    test("accept records the reviewer's skip with SKIPS' reason, whatever the implementer sent", () => {
      expect(accepted).toBeDefined();
      expect(loop("accept", '[{"message":"pushed 0123456789ab"}]\n').booted).toBe(
        `boot implementer\nskip reviewer ${accepted}\n`
      );
    });

    test("crossing records the reviewer's skip with SKIPS' reason, whatever the implementer sent", () => {
      expect(crossed).toBeDefined();
      expect(loop("crossing", '[{"message":"pushed 0123456789ab"}]\n').booted).toBe(
        `boot implementer\nskip reviewer ${crossed}\n`
      );
    });
  });
});

// services_up reads the daemon facts of every stand-in worker_boots names before any run starts
// (label_facts), so each boot reads its facts out of files. Catches: a scenario runner that boots
// a stand-in the list leaves out, whose boot would read its facts itself, and a list entry no
// runner boots.
describe("worker_boots", () => {
  const listed = /^worker_boots=\((.*)\)$/m.exec(rigScript)?.[1]?.split(" ") ?? [];
  /** Each stand-in `runner` boots with `args`, as `<role>` or `<role>:<phase>`, on a wake of
   * `wake`, with its world, its services and its agents stubbed, and this checkout as its
   * label's, whose daemon ends a reviewer's task (daemon-pane.ts's `reviewRoundSentences`). */
  function boots(runner: string, args: string[], wake = "[]\n"): string[] {
    const dir = freshDir(`worker-boots-${runner}`);
    writeFileSync(path.join(dir, "threads.json"), "[]\n");
    writeFileSync(path.join(dir, "wake.json"), wake);
    writeFileSync(
      path.join(dir, "fixture.json"),
      JSON.stringify({ implement: { version: 1 }, test: { version: 1 } })
    );
    const result = bash(
      [
        fn("note"),
        fn("fail"),
        fn("skip_reason"),
        fn(runner),
        fn("steps_seed"),
        fn("label_model"),
        "start_recorder() { :; }",
        "worker_run_world() { pr=1001; head=0123456789ab; }",
        "seed_ts() { :; }",
        "judge_ts() { :; }",
        "await_start() { :; }",
        'reviewer_wake() { cat "$R/wake.json"; }',
        `worker_boot() { printf "%s%s\\n" "$3" "\${4:+:$4}" >>"$R/boots"; }`,
        "R=$1 here=$2 co=$3 worker_key=LWEVAL-1 worker_repo=example/widgets",
        `${runner} the-run skillsproject1 1 100 ${args.join(" ")}`,
      ].join("\n"),
      [dir, import.meta.dir, root]
    );
    expect(result).toMatchObject({ code: 0, stderr: "" });
    return readFileSync(path.join(dir, "boots"), "utf8").trim().split("\n");
  }

  test("every stand-in a scenario boots is one services_up reads the facts of, and no other", () => {
    const booted = [
      ...boots("run_tester_proof", []),
      ...boots("run_reviewer_text_findings", ["text-only"]),
      ...boots("run_threads_closed", ["correct"]),
      ...boots("run_threads_closed", ["accept"]),
      ...boots("run_threads_closed", ["crossing"]),
      ...boots("run_threads_closed", ["code-defect"], '[{"message":"pushed"}]\n'),
    ];
    expect([...new Set(booted)].sort()).toEqual([...listed].sort());
  });

  // Catches: a worker run whose issue's results nobody captures, so no bar on a recorded result
  // can be scored, or whose agents' texts no judge reads (Verification v47's rule 8); and a capture
  // or a judging made before the run's last agent has ended, which would miss what it did.
  test("each run of reviewer-text-findings and threads-closed ends by capturing its handoffs and results, then judging it", () => {
    const ends = (runner: string, args: string[]) => {
      const dir = freshDir(`worker-ends-${runner}`);
      writeFileSync(path.join(dir, "threads.json"), "[]\n");
      writeFileSync(path.join(dir, "wake.json"), "[]\n");
      writeFileSync(
        path.join(dir, "fixture.json"),
        JSON.stringify({ implement: { version: 1 }, test: { version: 1 } })
      );
      const result = bash(
        [
          fn("note"),
          fn("fail"),
          fn("skip_reason"),
          fn(runner),
          "start_recorder() { :; }",
          "worker_run_world() { pr=1001; head=0123456789ab; }",
          "steps_seed() { :; }",
          "await_start() { :; }",
          'reviewer_wake() { cat "$R/wake.json"; }',
          'seed_ts() { printf "seed_ts %s\\n" "$*" >>"$R/ends"; }',
          'judge_ts() { printf "judge_ts %s\\n" "$*" >>"$R/ends"; }',
          'worker_boot() { printf "worker_boot %s\\n" "$3" >>"$R/ends"; }',
          "R=$1 here=$2 co=$3 worker_key=LWEVAL-1 worker_repo=example/widgets",
          `${runner} the-run skillsproject1 1 100 ${args.join(" ")}`,
        ].join("\n"),
        [dir, import.meta.dir, root]
      );
      expect(result).toMatchObject({ code: 0, stderr: "" });
      return readFileSync(path.join(dir, "ends"), "utf8")
        .trim()
        .split("\n")
        .slice(-4)
        .map((line) => line.replaceAll(dir, "$R"));
    };
    const last = (role: string) => [
      `worker_boot ${role}`,
      "seed_ts capture-handoffs $R/fixture.json $R",
      "seed_ts capture-results $R/fixture.json $R",
      "judge_ts run $R",
    ];
    expect(ends("run_reviewer_text_findings", ["text-only"])).toEqual(last("reviewer"));
    expect(ends("run_threads_closed", ["correct"])).toEqual(last("reviewer"));
    expect(ends("run_threads_closed", ["accept"])).toEqual(last("implementer"));
    expect(ends("run_threads_closed", ["crossing"])).toEqual(last("implementer"));
  });
});

// `rig.sh health <run dir>...`: whether each run's world could take what its agents did, read
// through completion-answers.ts, which the score's refused-completion rule reads too. Each row
// builds run directories as a batch leaves them: named as cmd_run names a run, with a boot record
// that boots every role its scenario boots, each role's own output and transcript, a daemon.log
// that each stand-in of the run opened with its own line before its agent's, the log of the run's
// own Dispatch server, and the built-from and services-from cmd_run copies into it.
describe("health", () => {
  const complete = (status: number, code?: string, role?: string, phase?: string) => ({
    at: "2026-11-13T18:00:00.000Z",
    path: "/legion/v1/handoff/complete",
    status,
    ...(code === undefined ? {} : { code }),
    ...(role === undefined ? {} : { role }),
    ...(phase === undefined ? {} : { phase }),
  });
  const NOT_CURRENT = "HANDOFF_NOT_CURRENT_PHASE";
  /** The line the run's Dispatch server writes for a request it served (its access log,
   * packages/envoy/internal/dispatch/accesslog), as Go's slog text handler writes it: ended at
   * `at`, `elapsed` ms after it began, a read of the shared agent token, which names no session. */
  const served = (
    at: string,
    method: string,
    route: string,
    requested: string,
    status: number,
    elapsed = 4
  ) =>
    `time=${at} level=INFO msg="dispatch: request" method=${method} route=${route} path=${requested} caller_kind=shared-token caller="" address=127.0.0.1 peer=127.0.0.1 user_agent=Bun/1.3.5 status=${status} elapsed_ms=${elapsed} bytes=97`;
  /** Dispatch's read of the newest handoff of `phase` of the run's issue, answered `status`. */
  const handoffRead = (at: string, phase: string, status: number, elapsed?: number) =>
    served(
      at,
      "GET",
      "/api/v1/issues/{key}/handoffs/{phase}/latest",
      `/api/v1/issues/LWEVAL-1/handoffs/${phase}/latest`,
      status,
      elapsed
    );
  /** One transcript of an agent, holding its session's header alone: what each role a run
   * booted leaves in its own session directory before a row writes its turns over it. */
  const TRANSCRIPT = "2026-11-14T00-00-00-000Z_s1.jsonl";
  let repoMade: string | undefined;
  /** The repository every run's built-from commit is in, which each health call names (`--repo`):
   * health rebuilds a run's label's daemon sources from that commit alone (daemon-pane.ts's
   * `recordedCheckout`), never off the checkout its built-from names. */
  const labelRepo = () => {
    repoMade ??= path.join(freshDir("health"), "labels.git");
    return repoMade;
  };
  /** The commit a run's built-from records for `checkout`, in labelRepo: the label sources that
   * checkout holds (`sourcesCommit`), or, for RUN_CHECKOUT, which is no directory here, this
   * checkout's Dispatch routes table and agents' client alone (person-routes.ts's
   * PERSON_ROUTE_SOURCES): a run built here, whose agents' 403s rule 16 reads by them, and whose
   * commit holds no daemon. */
  const builtCommit = (checkout: string) =>
    checkout === RUN_CHECKOUT
      ? sourcesCommit(labelRepo(), CHECKOUT, PERSON_ROUTE_SOURCES)
      : sourcesCommit(labelRepo(), checkout);
  /** A run directory named `name`, under a directory of its own, as a run whose every role
   * finished leaves it: a boot record booting each role its scenario boots (ANSWERS' `boots`),
   * in order, and each role's own output, ending with its `exit=` line, and transcript, the
   * first role's in sessions/ and each later one's in sessions/<role>/ (rig.sh's worker_boot).
   * Its daemon.log holds one opening line per boot, each followed by that boot's lines, boot k
   * (from 0) opened at 17:5k, a minute apart and before any line a row's stand-in answers;
   * `finished` false leaves the last role's out.txt with no `exit=` line, as a run that never
   * ended leaves it; `checkout` is the checkout its built-from names, with the commit of the
   * label sources that checkout holds (`builtCommit`: this checkout's Dispatch sources alone for
   * RUN_CHECKOUT, which is no directory here), or no commit line for `commit` false; `namespace`
   * true gives it the world a run's namespace records (namespace.txt, world_paths' kinds): its
   * credentials covered and its own directory shown, which is all a run that names no path needs,
   * and false none, as a run from before the namespace holds; `provenance` its services-from,
   * every service of its label's own from that checkout by default and none at
   * all for false; `dispatch` is the lines of its Dispatch server's log after the server's
   * listening line, and false for a run with no log. */
  function run(
    name: string,
    boots: object[][],
    {
      finished = true,
      checkout = RUN_CHECKOUT,
      commit = true,
      namespace = true,
      provenance = servicesFrom(checkout),
      dispatch = [],
    }: {
      finished?: boolean;
      checkout?: string;
      commit?: boolean;
      namespace?: boolean;
      provenance?: string | false;
      dispatch?: string[] | false;
    } = {}
  ) {
    const dir = path.join(freshDir("health"), name);
    mkdirSync(dir);
    const roles = ANSWERS[scenarioOf(name) ?? ""]?.boots ?? [];
    writeFileSync(path.join(dir, BOOTED), roles.map((role) => `boot ${role}\n`).join(""));
    for (const [at, role] of roles.entries()) {
      const last = at === roles.length - 1;
      writeFileSync(
        path.join(dir, last ? "out.txt" : `out-${role}.txt`),
        last && !finished ? "the agent's output\n" : "the agent's output\nexit=0\n"
      );
      const sessions = at === 0 ? path.join(dir, "sessions") : path.join(dir, "sessions", role);
      mkdirSync(sessions, { recursive: true });
      writeFileSync(path.join(sessions, TRANSCRIPT), `${JSON.stringify({ type: "session" })}\n`);
    }
    writeFileSync(
      path.join(dir, "built-from"),
      `checkout ${checkout}\n${commit ? `commit ${builtCommit(checkout)} parents=\n` : ""}`
    );
    if (namespace)
      writeFileSync(
        path.join(dir, "namespace.txt"),
        `credentials /var/run/legion\ncredentials /var/run/secrets\nrw ${dir}\n`
      );
    if (provenance !== false) writeFileSync(path.join(dir, "services-from"), provenance);
    writeFileSync(
      path.join(dir, "daemon.log"),
      boots
        .flatMap((lines, index) => [
          { at: `2026-11-13T17:5${index}:00.000Z`, opened: "daemon.log" },
          ...lines,
        ])
        .map((line) => `${JSON.stringify(line)}\n`)
        .join("")
    );
    if (dispatch !== false) {
      mkdirSync(path.join(dir, "logs"));
      writeFileSync(
        path.join(dir, "logs", "dispatch.log"),
        [LISTENING, ...dispatch].map((line) => `${line}\n`).join("")
      );
      writeFileSync(path.join(dir, "logs", "github.log"), GITHUB_RECORD);
      writeFileSync(path.join(dir, "logs", "heads.log"), HEADS_OPENED);
    }
    return dir;
  }
  /** The caller's go settings that decide where a `go run` builds and what it fetches, as `go env`
   * resolves them rather than as the environment names them: GOCACHE and GOMODCACHE default to
   * places under HOME (or XDG_CACHE_HOME), which childEnv replaces with a HOME of its own, so a
   * runner that sets neither variable (setup-go exports no GOCACHE; CI's pi-legion job restores
   * its caches into the defaults, ~/.cache/go-build and ~/go/pkg/mod) handed the routes rule an
   * empty cache under that HOME. On CI run 43612603343 that rule then built daemon-pane.go,
   * the daemon module's whole graph fetched and compiled, and the 404 row timed out at 60 s.
   * Read once, on the first call; empty on a host without go, whose rows that need it skip. */
  let callerGo: Record<string, string> | undefined;
  function goSettings(): Record<string, string> {
    if (callerGo !== undefined) return callerGo;
    const names = ["GOCACHE", "GOMODCACHE", "GOPATH", "GOFLAGS", "GOTOOLCHAIN"];
    const printed = withoutGo
      ? undefined
      : spawnChecked(["go", "env", "-json", ...names], { stdout: "pipe", stderr: "pipe" });
    const resolved: Record<string, string> =
      printed?.exitCode === 0 ? JSON.parse(printed.stdout.toString()) : {};
    callerGo = Object.fromEntries(Object.entries(resolved).filter(([, value]) => value !== ""));
    return callerGo;
  }
  /** `rig.sh health` given exactly `args`, under childEnv with the caller's go caches and `vars`
   * over it (a TMPDIR, say, which decides no verdict), from the working directory `cwd`. */
  function healthArgs(vars: Record<string, string>, args: string[], cwd?: string) {
    // The go caches of the caller's, which the routes rule's `go run` compiles with: a HOME of
    // its own would have go build from an empty cache (goSettings).
    const go = goSettings();
    const result = spawnChecked(["bash", path.join(import.meta.dir, "rig.sh"), "health", ...args], {
      cwd,
      env: { ...childEnv, ...go, SKILL_SCENARIOS_WORK: freshDir("health-work"), ...vars },
    });
    return {
      code: result.exitCode,
      stdout: result.stdout.toString(),
      stderr: result.stderr.toString(),
    };
  }
  /** `rig.sh health` of `dirs`, naming labelRepo (`--repo`), which holds each run's commit. */
  const healthIn = (vars: Record<string, string>, ...dirs: string[]) =>
    healthArgs(vars, ["--repo", labelRepo(), ...dirs]);
  const health = (...dirs: string[]) => healthIn({}, ...dirs);
  /** The verdict line health printed for the run directory `dir`. */
  const verdict = (stdout: string, dir: string) =>
    stdout.split("\n").find((line) => line.startsWith(`${path.basename(dir)}  `));
  /** The line health printed on stderr for a run it refused to judge. */
  const refusal = (stderr: string, dir: string) =>
    stderr
      .split("\n")
      .find((line) => line.startsWith(`health: no verdict on ${path.basename(dir)}: `));
  /** That line for a run from before the namespace (no namespace.txt) that no rule that can judge
   * it fails: rules 10 and 12 read the world it ran in, which it never recorded. */
  const unrecorded = (dir: string) =>
    `health: no verdict on ${path.basename(dir)}: it records no namespace.txt, the world its agents ran in, which rules 10 and 12 read; nothing else it recorded gives that world, and no reading of the pod stands in for it`;
  let olderMade: string | undefined;
  /** A checkout holding an older daemon's engine and completion route alone, as main ff2a10ef
   * writes them (completion-answers.ts's `refused` and `issue`): the daemon health reads a run's
   * completions by where an answer turns on it, out of the commit of it its built-from records
   * (daemon-pane.ts's `labelDaemon`), never out of the label's name. Made once for the rows. */
  function olderDaemon(): string {
    if (olderMade !== undefined) return olderMade;
    const older = path.join(freshDir("health"), "older-daemon");
    const internal = path.join(older, "packages", "daemon", "internal");
    mkdirSync(path.join(internal, "workflow"), { recursive: true });
    mkdirSync(path.join(internal, "api"), { recursive: true });
    writeFileSync(
      path.join(internal, "workflow", "engine.go"),
      'package workflow\n\nfunc (e *Engine) handoff(fact intake.HandoffComplete) (intake.Result, error) {\n\tif RoleFor(issue.Phase) != fact.Role {\n\t\treturn refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf("the %s does not run %s of %s", fact.Role, issue.Phase, issue.Key)), nil\n\t}\n\treturn intake.Result{}, nil\n}\n'
    );
    writeFileSync(
      path.join(internal, "api", "handoff.go"),
      "package api\n\nfunc (s *server) handoffComplete(w http.ResponseWriter, r *http.Request) {\n\tword, writer, handoffPhase := phase.Handoff(issue.Phase)\n\trecordBacked := handoffPhase && writer == grant.Role\n}\n"
    );
    olderMade = older;
    return older;
  }

  // Catches: a refusal the world should not have given, read as a healthy run, and a reason
  // that does not say which refusal it was. The answer is no daemon's of either shape, so the run's
  // own daemon is read for it, this checkout's.
  test.skipIf(withoutGo)(
    "a run with a refused completion is unhealthy, named with its status and code",
    () => {
      const dir = run(
        "reviewer-text-findings-defect-head-1",
        [
          [
            complete(400, "HANDOFF_RECORD_REQUIRED", "reviewer"),
            complete(200, undefined, "reviewer"),
          ],
        ],
        { checkout: CHECKOUT }
      );
      const { code, stdout } = health(dir);
      expect(code).toBe(1);
      expect(verdict(stdout, dir)).toBe(
        "reviewer-text-findings-defect-head-1  unhealthy: refused completion: POST /legion/v1/handoff/complete of the reviewer answered 400 HANDOFF_RECORD_REQUIRED, which the head label's daemon does not answer it with in this world"
      );
      expect(stdout).toEndWith("health: 1 of 1 runs unhealthy\n");
    }
  );

  // Catches: a run's daemon read where no answer turns on it, or an answer that turns on it
  // judged by some daemon when the run's own cannot be read. The commit the run's built-from
  // records holds no daemon (RUN_CHECKOUT's): a reviewer refused 409 HANDOFF_NOT_CURRENT_PHASE
  // with no earlier 200 is an answer one shape gives and the other does not, so whose world the
  // run met is unknown, while the implementer's refusal and the reviewer's 200 are every daemon's
  // answers. The run is a defect run, whose world must take the reviewer's completion (rule 3,
  // read by the run's case), so both rules read that answer. The verdict names the commit, never
  // the directory health rebuilt it in. A source that is not there is refused with no Go run: the
  // `go` on health's PATH here records a call and refuses it, so a health that compiled the reader
  // to say a file is missing, which cost the tester's fixture of 2026-11-14 a minute, fails this
  // row on any host.
  test("a run whose answer turns on a daemon that cannot be read is unhealthy, naming why", () => {
    const bin = freshDir("no-go");
    const ran = path.join(bin, "ran");
    writeFileSync(path.join(bin, "go"), `#!/bin/sh\necho "$*" >>'${ran}'\nexit 1\n`, {
      mode: 0o755,
    });
    const dir = run("reviewer-text-findings-defect-head-2", [
      [complete(409, NOT_CURRENT, "reviewer")],
    ]);
    const { code, stdout } = healthIn({ PATH: `${bin}:${childEnv.PATH}` }, dir);
    expect(code).toBe(1);
    const commit = `<commit ${builtCommit(RUN_CHECKOUT)}>`;
    expect(verdict(stdout, dir)).toBe(
      `reviewer-text-findings-defect-head-2  unhealthy: completion shapes unknown: an answer the stand-in logged turns on how the head label's daemon takes a completion, which cannot be read: ${commit}/packages/daemon/internal/workflow/engine.go is not there, so how the daemon of ${commit} takes a completion is unknown (daemon-completion.go reads it)`
    );
    expect(stdout).toContain("health: completions: 1 of 1 runs\n");
    expect(stdout).toContain("health: taken: 1 of 1 runs\n");
    expect(existsSync(ran)).toBe(false);
  });

  // The architect's ruling of 2026-11-18: a reader judges a run from its archive and the
  // provenance it recorded, rebuilds what else it needs from those by digest, or refuses, and a
  // refusal is no verdict. A run whose answer turns on its label's daemon is judged by the
  // sources of the commit its built-from records, rebuilt out of the repositories the caller
  // names; where that cannot be done health prints no verdict on the run, names it and the input
  // on stderr, counts it in no rule, and exits 2, whatever the other runs read. A run that fails
  // a rule that can judge it is unhealthy all the same. Catches: the daemon read off the checkout
  // the built-from names, as the pod holds it now (each refused run here would read the
  // checkout's sources); a refusal read as a verdict, healthy or unhealthy, which a batch runner
  // would count or replace; and a run with a judged failure refused rather than unhealthy.
  test("a run whose label's daemon cannot be rebuilt from its recorded commit gets no verdict, and health exits 2 naming it", () => {
    const refused = [[complete(409, NOT_CURRENT, "reviewer")]];
    const noCommit = run("reviewer-text-findings-text-only-head-9", refused, {
      checkout: CHECKOUT,
      commit: false,
    });
    const dirty = run("reviewer-text-findings-text-only-head-10", refused, { checkout: CHECKOUT });
    writeFileSync(
      path.join(dirty, "built-from"),
      `checkout ${CHECKOUT}\ncommit ${sourcesCommit(labelRepo(), CHECKOUT)} parents= dirty\n`
    );
    const elsewhere = run("reviewer-text-findings-text-only-head-11", refused, {
      checkout: CHECKOUT,
    });
    const unfinished = run("reviewer-text-findings-text-only-head-12", refused, {
      checkout: CHECKOUT,
      commit: false,
      finished: false,
    });
    const judged = run("reviewer-text-findings-text-only-head-13", [
      [complete(200, undefined, "reviewer")],
    ]);
    const { code, stdout, stderr } = health(noCommit, dirty, unfinished, judged);
    expect(code).toBe(2);
    expect([noCommit, dirty, unfinished, judged].map((dir) => verdict(stdout, dir))).toEqual([
      undefined,
      undefined,
      "reviewer-text-findings-text-only-head-12  unhealthy: unfinished: the reviewer's output, <run>/out.txt, has no exit= line",
      "reviewer-text-findings-text-only-head-13  healthy",
    ]);
    const commit = sourcesCommit(labelRepo(), CHECKOUT);
    expect([refusal(stderr, noCommit), refusal(stderr, dirty)]).toEqual([
      "health: no verdict on reviewer-text-findings-text-only-head-9: its built-from records no commit (`commit <sha>`), so the daemon sources its label ran cannot be rebuilt",
      `health: no verdict on reviewer-text-findings-text-only-head-10: its built-from's commit ${commit} is of a checkout with changes no commit holds (\`dirty\`), so the daemon sources its label ran cannot be rebuilt`,
    ]);
    expect(stderr).toContain("health: 2 of 4 runs refused\n");
    expect(stdout).toContain("health: completions: 0 of 2 runs\n");
    expect(stdout).toEndWith("health: 1 of 2 runs unhealthy\n");
    // No repository named, and one that does not hold the commit.
    const none = healthArgs({}, [elsewhere]);
    expect({ code: none.code, stdout: verdict(none.stdout, elsewhere) }).toEqual({
      code: 2,
      stdout: undefined,
    });
    expect(refusal(none.stderr, elsewhere)).toBe(
      `health: no verdict on reviewer-text-findings-text-only-head-11: its label's daemon sources are rebuilt from commit ${commit}, which its built-from records, out of a repository the caller names (--repo <git dir>), and none was named`
    );
    const other = path.join(freshDir("health"), "other.git");
    expect(spawnChecked(["git", "init", "-q", "--bare", other]).exitCode).toBe(0);
    const lacking = healthArgs({}, ["--repo", other, elsewhere]);
    expect(lacking.code).toBe(2);
    expect(refusal(lacking.stderr, elsewhere)).toBe(
      `health: no verdict on reviewer-text-findings-text-only-head-11: no repository named (${other}) holds commit ${commit}, which its built-from records`
    );
  });

  // The architect's ruling A of 2026-11-18: a run is a directory the rig made, and a symlink to
  // one, read through, is judged by what the run given does not hold, as score.ts's `runsOf`
  // refuses one in a runs directory. The run linked is healthy, read as itself. Catches: a
  // symlinked run judged through its link.
  test("a run given as a symlink gets no verdict, and health exits 2 naming its kind", () => {
    const dir = run("reviewer-text-findings-text-only-head-13", [
      [complete(200, undefined, "reviewer")],
    ]);
    expect(verdict(health(dir).stdout, dir)).toBe(
      "reviewer-text-findings-text-only-head-13  healthy"
    );
    const link = path.join(freshDir("health-link"), path.basename(dir));
    symlinkSync(dir, link);
    const { code, stdout, stderr } = health(link);
    expect([code, stdout, stderr.split("\n")[0]]).toEqual([
      2,
      "",
      `health: ${link} is a symlink, not a run directory`,
    ]);
  }, 60_000);

  // Catches: a daemon of a shape pair ANSWERS gives no answers judged by some other pair's. This
  // checkout's engine beside an older daemon's route is a daemon no checkout the rig has read pairs
  // them in, so what its world answers a late reviewer is unknown, and health says so, naming the
  // pair, rather than reading it as either daemon.
  test.skipIf(withoutGo)(
    "a run whose daemon pairs one shape's engine with the other's route is unhealthy, naming the pair",
    () => {
      const mixed = path.join(freshDir("health"), "mixed-daemon");
      const internal = path.join(mixed, "packages", "daemon", "internal");
      mkdirSync(path.join(internal, "workflow"), { recursive: true });
      mkdirSync(path.join(internal, "api"), { recursive: true });
      writeFileSync(
        path.join(internal, "workflow", "engine.go"),
        "package workflow\n\nfunc (e *Engine) leftItsPhase(row record.PhaseRow) bool {\n\treturn !row.Completed()\n}\n\nfunc (e *Engine) handoff() {\n\te.leftItsPhase(row)\n}\n"
      );
      cpSync(
        path.join(olderDaemon(), "packages", "daemon", "internal", "api", "handoff.go"),
        path.join(internal, "api", "handoff.go")
      );
      const dir = run(
        "reviewer-text-findings-text-only-head-4",
        [[complete(409, NOT_CURRENT, "reviewer")]],
        { checkout: mixed }
      );
      const { code, stdout } = health(dir);
      expect(code).toBe(1);
      expect(verdict(stdout, dir)).toContain(
        "a daemon of the shape `recorded issue` (its engine's late role, then its route's record) has no answers for reviewer-text-findings's completions (completion-answers.ts's ANSWERS)"
      );
    }
  );

  // Catches: a health that calls the daemon's own deliberate refusal unhealthy. threads-closed's
  // implementer, whose round stands, is refused 409 HANDOFF_NOT_CURRENT_PHASE by a daemon of either
  // shape: the head run's and the base run's (the fixture's threads-closed-correct-base-1,
  // which its boot record now reads unfinished, pinned that label's alone). No daemon is read for
  // it: their built-from names a checkout holding none (RUN_CHECKOUT).
  test("a threads-closed run whose only refusal is the implementer's not-current-phase is healthy", () => {
    const named = run("threads-closed-correct-head-1", [
      [complete(409, NOT_CURRENT, "implementer")],
      [complete(200, undefined, "reviewer")],
    ]);
    const base = run("threads-closed-correct-base-1", [
      [complete(409, NOT_CURRENT, "implementer")],
      [],
    ]);
    const { code, stdout } = health(named, base);
    expect(code).toBe(0);
    expect(verdict(stdout, named)).toBe("threads-closed-correct-head-1  healthy");
    expect(verdict(stdout, base)).toBe("threads-closed-correct-base-1  healthy");
    expect(stdout).toEndWith("health: 0 of 2 runs unhealthy\n");
  });

  // Catches: a completion line that names no role given the role of the agent booted in its
  // place, which read this head threads-closed 409 HANDOFF_NOT_CURRENT_PHASE as the implementer's,
  // the one role that answer is allowed to; and a line naming a role the scenario boots none of
  // read as a role's. Whose completion either was is unknown, so each fails closed, whatever
  // its answer.
  test("a completion line naming no role, or a role the scenario never boots, is unhealthy", () => {
    const unnamed = run("threads-closed-correct-head-3", [[complete(409, NOT_CURRENT)]]);
    const taken = run("threads-closed-code-defect-head-3", [[complete(200, undefined)]]);
    const stranger = run("reviewer-text-findings-defect-base-1", [
      [complete(200, undefined, "tester"), complete(200, undefined, "reviewer")],
    ]);
    const { code, stdout } = health(unnamed, taken, stranger);
    expect(code).toBe(1);
    expect(verdict(stdout, unnamed)).toBe(
      "threads-closed-correct-head-3  unhealthy: unkeyable completion: a completion line names no role (POST /legion/v1/handoff/complete answered 409 HANDOFF_NOT_CURRENT_PHASE), so whose completion it was is unknown"
    );
    expect(verdict(stdout, taken)).toContain(
      "unhealthy: unkeyable completion: a completion line names no role (POST /legion/v1/handoff/complete answered 200)"
    );
    expect(verdict(stdout, stranger)).toContain(
      "unhealthy: unkeyable completion: a completion line names the tester, a role reviewer-text-findings boots none of"
    );
    expect(stdout).toEndWith("health: 3 of 3 runs unhealthy\n");
  });

  // A threads-closed reviewer's review round stands on its row from before the run: the world's
  // seeds record its review handoff (rig.sh's run_threads_closed, seed.ts review-handoff), which
  // the stand-in reads onto the row (daemon-standin.ts's `HANDOFFS`). So this checkout's daemon,
  // which the run is built from, refuses that reviewer's late completion 409
  // HANDOFF_NOT_CURRENT_PHASE with no earlier 200 of its own (`leftItsPhase`'s
  // `row.Completed()`), and the run is healthy. Catches: the table of before INFRA-764's ruling
  // on the completions production refuses, which read no seeded row and called this refusal a
  // world the daemon would not have served, where the daemon serves exactly it (this row held
  // the run unhealthy until then).
  test.skipIf(withoutGo)(
    "a head threads-closed reviewer refused not-current-phase is healthy: its seeded round stands",
    () => {
      const named = run(
        "threads-closed-correct-head-2",
        [[complete(409, NOT_CURRENT, "implementer")], [complete(409, NOT_CURRENT, "reviewer")]],
        { checkout: CHECKOUT }
      );
      const { code, stdout } = health(named);
      expect(code).toBe(0);
      expect(verdict(stdout, named)).toBe("threads-closed-correct-head-2  healthy");
    }
  );

  // Catches: one exemption for every label, and an exemption keyed by the label's name. An older
  // engine (main `ff2a10ef`'s) refuses a reviewer whose round has moved the issue 409
  // HANDOFF_NOT_CURRENT_PHASE, which is how that daemon takes the completion; this checkout's
  // records it, so the same answer with no earlier 200 of the reviewer's is a refusal this
  // checkout's daemon never gives a reviewer here. Which of the two a run met is its built-from's
  // checkout's, whatever the label is called: the base label is built from main at the head's
  // newest forward merge, whose daemon is this checkout's, and a head built from an older checkout
  // has that checkout's daemon.
  test.skipIf(withoutGo)(
    "a reviewer-text-findings reviewer refused not-current-phase is healthy on an older daemon and not on this checkout's, whatever the label",
    () => {
      const refusal = [[complete(409, NOT_CURRENT, "reviewer")]];
      const baseOnOlder = run("reviewer-text-findings-text-only-base-1", refusal, {
        checkout: olderDaemon(),
      });
      const base = run("reviewer-text-findings-text-only-base-2", refusal, { checkout: CHECKOUT });
      const head = run("reviewer-text-findings-text-only-head-1", refusal, { checkout: CHECKOUT });
      const headOnOlder = run("reviewer-text-findings-text-only-head-2", refusal, {
        checkout: olderDaemon(),
      });
      const { code, stdout } = health(baseOnOlder, base, head, headOnOlder);
      expect(code).toBe(1);
      expect(verdict(stdout, baseOnOlder)).toBe("reviewer-text-findings-text-only-base-1  healthy");
      expect(verdict(stdout, headOnOlder)).toBe("reviewer-text-findings-text-only-head-2  healthy");
      for (const [dir, label] of [
        [base, "base"],
        [head, "head"],
      ] as const)
        expect(verdict(stdout, dir)).toContain(
          `unhealthy: refused completion: POST /legion/v1/handoff/complete of the reviewer answered 409 HANDOFF_NOT_CURRENT_PHASE, which the ${label} label's daemon`
        );
      expect(stdout).toEndWith("health: 2 of 4 runs unhealthy\n");
    }
  );

  // Catches: a reviewer's late refusal by this checkout's daemon read by its code alone. This
  // checkout's route reads back the review record a late reviewer names, and its engine records
  // that completion 200 unless the reviewer's row already holds one, after which it refuses a
  // late one 409 HANDOFF_NOT_CURRENT_PHASE. In reviewer-text-findings the row holds one only
  // once a completion of the reviewer's own was taken earlier in the run (completion-answers.ts's
  // `ANSWERS`, `AFTER_TAKEN`): not with none before it, and not with the 200 after it. In
  // threads-closed the world's seeds stand for the reviewer's round, so the refusal is the
  // daemon's answer whatever came before it (the three threads-closed runs here were held
  // unhealthy until INFRA-764's ruling on the completions production refuses). This route
  // refuses no reviewer 409 HANDOFF_RECORD_MISMATCH in this rig, `accepting` included, since the
  // phase its completion is of is `reviewing`, which writes a record. A completion naming a
  // record another session wrote is refused 403 HANDOFF_SESSION_MISMATCH, which a role with a
  // seeded record can meet (threads-closed's two) and a reviewer-text-findings reviewer, whose
  // world seeds no review record, cannot; and a role's repeat of its own completion is answered
  // 409 HANDOFF_ALREADY_RECORDED, in any world, once the workflow was handed one of that role's.
  // Each run is built from this checkout, whose daemon health reads for those answers.
  test.skipIf(withoutGo)(
    "a head reviewer refused not-current-phase is healthy where its row holds a completion: the seeds' in threads-closed, its own earlier 200 elsewhere",
    () => {
      const built = { checkout: CHECKOUT };
      const fixes = run(
        "threads-closed-correct-head-4",
        [
          [complete(409, NOT_CURRENT, "implementer", "reviewing")],
          [
            complete(200, undefined, "reviewer", "reviewing"),
            complete(409, NOT_CURRENT, "reviewer", "accepting"),
          ],
        ],
        built
      );
      const review = run(
        "reviewer-text-findings-text-only-head-3",
        [
          [
            complete(200, undefined, "reviewer", "reviewing"),
            complete(409, NOT_CURRENT, "reviewer", "implementing"),
          ],
        ],
        built
      );
      const alone = run(
        "threads-closed-code-defect-head-1",
        [
          [complete(409, NOT_CURRENT, "implementer", "accepting")],
          [complete(409, NOT_CURRENT, "reviewer", "accepting")],
        ],
        built
      );
      const before = run(
        "threads-closed-correct-head-8",
        [
          [complete(409, NOT_CURRENT, "implementer", "reviewing")],
          [
            complete(409, NOT_CURRENT, "reviewer", "accepting"),
            complete(200, undefined, "reviewer", "accepting"),
          ],
        ],
        built
      );
      const another = run(
        "threads-closed-code-defect-head-6",
        [
          [complete(200, undefined, "implementer", "accepting")],
          [complete(409, NOT_CURRENT, "reviewer", "accepting")],
        ],
        built
      );
      const first = run(
        "reviewer-text-findings-text-only-head-5",
        [[complete(409, NOT_CURRENT, "reviewer", "implementing")]],
        built
      );
      const beforeTaken = run(
        "reviewer-text-findings-text-only-head-6",
        [
          [
            complete(409, NOT_CURRENT, "reviewer", "implementing"),
            complete(200, undefined, "reviewer", "implementing"),
          ],
        ],
        built
      );
      const MISMATCH = "HANDOFF_RECORD_MISMATCH";
      const accepting = run(
        "threads-closed-code-defect-head-2",
        [
          [complete(409, NOT_CURRENT, "implementer", "accepting")],
          [complete(409, MISMATCH, "reviewer", "accepting")],
        ],
        built
      );
      const SESSION = "HANDOFF_SESSION_MISMATCH";
      const RECORDED = "HANDOFF_ALREADY_RECORDED";
      const anotherSession = run(
        "threads-closed-correct-head-9",
        [
          [
            complete(403, SESSION, "implementer", "reviewing"),
            complete(409, NOT_CURRENT, "implementer", "reviewing"),
            complete(409, RECORDED, "implementer", "reviewing"),
          ],
          [complete(403, SESSION, "reviewer", "reviewing")],
        ],
        built
      );
      const noSeededReview = run(
        "reviewer-text-findings-text-only-head-7",
        [[complete(403, SESSION, "reviewer", "reviewing")]],
        built
      );
      const neverHanded = run(
        "reviewer-text-findings-text-only-head-8",
        [[complete(409, RECORDED, "reviewer", "reviewing")]],
        built
      );
      const { code, stdout } = health(
        fixes,
        review,
        alone,
        before,
        another,
        first,
        beforeTaken,
        accepting,
        anotherSession,
        noSeededReview,
        neverHanded
      );
      expect(code).toBe(1);
      for (const dir of [fixes, review, alone, before, another, anotherSession])
        expect(verdict(stdout, dir)).toBe(`${path.basename(dir)}  healthy`);
      const refused = (answer: string) =>
        `unhealthy: refused completion: POST /legion/v1/handoff/complete of the reviewer answered ${answer}, which the head label's daemon does not answer it with in this world`;
      for (const dir of [first, beforeTaken])
        expect(verdict(stdout, dir)).toBe(
          `${path.basename(dir)}  ${refused(`409 ${NOT_CURRENT}`)}`
        );
      expect(verdict(stdout, accepting)).toBe(
        `threads-closed-code-defect-head-2  ${refused(`409 ${MISMATCH}`)}`
      );
      expect(verdict(stdout, noSeededReview)).toBe(
        `reviewer-text-findings-text-only-head-7  ${refused(`403 ${SESSION}`)}`
      );
      expect(verdict(stdout, neverHanded)).toBe(
        `reviewer-text-findings-text-only-head-8  ${refused(`409 ${RECORDED}`)}`
      );
      expect(stdout).toEndWith("health: 5 of 11 runs unhealthy\n");
    }
  );

  // Catches: a run whose stand-in answered 404 a route its label's daemon registers read as
  // healthy, which read every head run of the kept batch healthy though each logged the plugin's
  // `claims/turn-outcome` answered 404; and the same 404 read as unhealthy where the label's
  // daemon registers no such route, which is that daemon's own answer. A daemon's routes are read
  // out of the source of the checkout the run's built-from names: this checkout's for the head,
  // and for the other label a checkout holding a daemon's API source alone that registers no
  // `claims/turn-outcome`, as main `ff2a10ef`'s does not. A line naming no method (the kept
  // batch's, written before the stand-in logged one) is held to every route of its path; a 404 of
  // a route no daemon registers is any daemon's own answer; and a run with a 404 whose label's
  // daemon's routes cannot be read (its built-from names a checkout holding no daemon) is
  // unhealthy, since whose answer it was is unknown.
  test.skipIf(withoutGo)(
    "a 404 of a route the run's label's daemon registers is unhealthy, and of one it does not, healthy",
    () => {
      const older = path.join(freshDir("health"), "older-daemon");
      const api = path.join(older, "packages", "daemon", "internal", "api");
      mkdirSync(api, { recursive: true });
      writeFileSync(
        path.join(api, "server.go"),
        'package api\n\nfunc routes() {\n\tmux.HandleFunc("GET /legion/v1/state", s.state)\n\tmux.HandleFunc("POST /legion/v1/claims/ready-for-update", s.readyForUpdate)\n}\n'
      );
      const notFound = (route: string, method?: string) => ({
        at: "2026-11-13T18:00:00.000Z",
        ...(method === undefined ? {} : { method }),
        path: route,
        status: 404,
        role: "reviewer",
      });
      const turnOutcome = "/legion/v1/claims/turn-outcome";
      const head = run("threads-closed-code-defect-head-7", [[notFound(turnOutcome)]], {
        checkout: CHECKOUT,
      });
      const update = run(
        "threads-closed-code-defect-head-8",
        [[notFound("/legion/v1/claims/ready-for-update", "POST")]],
        { checkout: CHECKOUT }
      );
      const base = run("threads-closed-code-defect-base-7", [[notFound(turnOutcome, "POST")]], {
        checkout: older,
      });
      const nowhere = run("threads-closed-code-defect-head-9", [[notFound("/nowhere", "POST")]], {
        checkout: CHECKOUT,
      });
      const unknown = run("threads-closed-code-defect-head-10", [[notFound(turnOutcome, "POST")]]);
      const { code, stdout } = health(head, update, base, nowhere, unknown);
      expect(code).toBe(1);
      expect(verdict(stdout, head)).toBe(
        `threads-closed-code-defect-head-7  unhealthy: unserved route: the stand-in answered a request of ${turnOutcome} 404, a route the head label's daemon registers (${CHECKOUT} at ${sourcesCommit(labelRepo(), CHECKOUT)})`
      );
      expect(verdict(stdout, update)).toBe(
        `threads-closed-code-defect-head-8  unhealthy: unserved route: the stand-in answered POST /legion/v1/claims/ready-for-update 404, a route the head label's daemon registers (${CHECKOUT} at ${sourcesCommit(labelRepo(), CHECKOUT)})`
      );
      expect(verdict(stdout, base)).toBe("threads-closed-code-defect-base-7  healthy");
      expect(verdict(stdout, nowhere)).toBe("threads-closed-code-defect-head-9  healthy");
      expect(verdict(stdout, unknown)).toContain(
        `unhealthy: routes unknown: the stand-in answered ${turnOutcome} 404, and which routes the head label's daemon registers cannot be read: `
      );
      expect(stdout).toContain("health: routes: 3 of 5 runs\n");
      expect(stdout).toEndWith("health: 3 of 5 runs unhealthy\n");
    }
  );

  // Catches: the routes rule's `go run` handed the go cache of health's own HOME rather than the
  // caller's. Where the caller names no GOCACHE (CI's runner, whose restored cache is the default
  // under the runner's HOME), the variables alone carry nothing, and every row whose run has a
  // 404 built from an empty cache: on CI run 43612603343 the row above timed out at 60 s so. Each
  // `go` health starts is a wrapper that records the GOCACHE it was given and runs the real go.
  test.skipIf(withoutGo)(
    "the routes rule's go runs build in the caller's own go cache, not one under health's HOME",
    () => {
      const bin = freshDir("go-wrapper");
      const seen = path.join(bin, "seen");
      writeFileSync(
        path.join(bin, "go"),
        `#!/bin/sh\nprintf '%s\\n' "$GOCACHE" >>'${seen}'\nexec '${Bun.which("go")}' "$@"\n`,
        { mode: 0o755 }
      );
      const checkout = path.join(freshDir("health"), "routes-daemon");
      const api = path.join(checkout, "packages", "daemon", "internal", "api");
      mkdirSync(api, { recursive: true });
      writeFileSync(
        path.join(api, "server.go"),
        'package api\n\nfunc routes() {\n\tmux.HandleFunc("GET /legion/v1/state", s.state)\n}\n'
      );
      const dir = run(
        "threads-closed-code-defect-head-13",
        [
          [
            {
              at: "2026-11-13T18:00:00.000Z",
              method: "POST",
              path: "/nowhere",
              status: 404,
              role: "reviewer",
            },
          ],
        ],
        { checkout }
      );
      const { stdout } = healthIn({ PATH: `${bin}:${childEnv.PATH}` }, dir);
      expect(verdict(stdout, dir)).toBe("threads-closed-code-defect-head-13  healthy");
      const cache = goSettings().GOCACHE ?? "";
      expect(cache).not.toBe("");
      expect(readFileSync(seen, "utf8")).toBe(`${cache}\n`);
    }
  );

  // Catches: a run whose stand-in answered a turn outcome that failed the claim read as healthy.
  // The daemon fails a claim whose provider refused turns up to its launch failure limit, stops
  // its process and takes its secret, so what the run did after is no world the daemon serves;
  // a turn outcome answered without failing the claim is no such run.
  test("a run whose turn outcome failed the claim is unhealthy", () => {
    const outcome = (failed: boolean) => ({
      at: "2026-11-13T18:00:00.000Z",
      method: "POST",
      path: "/legion/v1/claims/turn-outcome",
      status: 200,
      role: "reviewer",
      failed,
    });
    const spent = run("threads-closed-code-defect-head-11", [[outcome(false), outcome(true)]]);
    const kept = run("threads-closed-code-defect-head-12", [[outcome(false), outcome(false)]]);
    const { code, stdout } = health(spent, kept);
    expect(code).toBe(1);
    expect(verdict(stdout, spent)).toBe(
      "threads-closed-code-defect-head-11  unhealthy: failed claim: the stand-in answered the reviewer's turn outcome at 2026-11-13T18:00:00.000Z with failed, so the daemon would have failed that claim"
    );
    expect(verdict(stdout, kept)).toBe("threads-closed-code-defect-head-12  healthy");
    expect(stdout).toContain("health: failed claim: 1 of 2 runs\n");
  });

  // Catches: a run whose claim sent a capability report after its boot's ready read as healthy.
  // The daemon normalises a report's rows and cleans each detail (`capabilities.Normalize`), which
  // the stand-in does not model (the class search's residual of 2026-11-19T05:18Z), so from that
  // call on the world's capability rows are not production's. Each stand-in's part of the log is
  // one claim's: a boot ready refused and sent again is still the boot's, and the next agent's own
  // boot ready, after its stand-in's opening line, is that agent's first.
  test("a run whose claim sent a claims/ready after its boot's is unhealthy", () => {
    const ready = (status: number, role: string, minute: number) => ({
      at: `2026-11-13T18:0${minute}:00.000Z`,
      method: "POST",
      path: "/legion/v1/claims/ready",
      status,
      role,
      sessionId: `ses-${role}`,
    });
    const reported = run("threads-closed-code-defect-head-14", [
      [ready(403, "implementer", 0), ready(204, "implementer", 1)],
      [ready(204, "reviewer", 2), ready(204, "reviewer", 3)],
    ]);
    const booted = run("threads-closed-code-defect-head-15", [
      [ready(403, "implementer", 0), ready(204, "implementer", 1)],
      [ready(204, "reviewer", 2)],
    ]);
    const { code, stdout } = health(reported, booted);
    expect(code).toBe(1);
    expect(verdict(stdout, reported)).toBe(
      "threads-closed-code-defect-head-14  unhealthy: capability report: daemon.log:6 is a claims/ready of the reviewer's claim (session ses-reviewer) at 2026-11-13T18:03:00.000Z, after its boot's, so from that call on the world's capability rows are not production's (the daemon normalises a report's rows, which the stand-in does not)"
    );
    expect(verdict(stdout, booted)).toBe("threads-closed-code-defect-head-15  healthy");
    expect(stdout).toContain("health: capability report: 1 of 2 runs\n");
  });

  // Catches: a code outside the run's own entry read as allowed because some entry allows it. The
  // run is built from an older daemon, whose answers are read for it.
  test.skipIf(withoutGo)("a refusal whose code is outside the run's entry is unhealthy", () => {
    const dir = run(
      "threads-closed-code-defect-base-2",
      [[complete(409, "HANDOFF_RECORD_MISMATCH", "implementer")]],
      { checkout: olderDaemon() }
    );
    const { code, stdout } = health(dir);
    expect(code).toBe(1);
    expect(verdict(stdout, dir)).toContain(
      "unhealthy: refused completion: POST /legion/v1/handoff/complete of the implementer answered 409 HANDOFF_RECORD_MISMATCH"
    );
  });

  // Catches: a run that never finished read as healthy on a log that says nothing went wrong.
  test("a run with no exit= line is unhealthy", () => {
    const dir = run(
      "reviewer-text-findings-text-only-head-2",
      [[complete(200, undefined, "reviewer")]],
      {
        finished: false,
      }
    );
    const { code, stdout } = health(dir);
    expect(code).toBe(1);
    expect(verdict(stdout, dir)).toBe(
      "reviewer-text-findings-text-only-head-2  unhealthy: unfinished: the reviewer's output, <run>/out.txt, has no exit= line"
    );
  });

  // Catches: a reviewer-text-findings world that took no completion read as healthy. Its world
  // must take at least one of the reviewer's; threads-closed's requires none, and a run of it
  // with no completion at all is healthy.
  test("reviewer-text-findings needs a completion taken, and threads-closed needs none", () => {
    const none = run("reviewer-text-findings-defect-head-2", [[]]);
    const taken = run("reviewer-text-findings-defect-head-3", [
      [complete(200, undefined, "reviewer")],
    ]);
    const quiet = run("threads-closed-code-defect-head-1", [[]]);
    const { code, stdout } = health(none, taken, quiet);
    expect(code).toBe(1);
    expect(verdict(stdout, none)).toBe(
      "reviewer-text-findings-defect-head-2  unhealthy: no completion taken: no completion of the reviewer was answered as the head label's daemon answers one it takes"
    );
    expect(verdict(stdout, taken)).toBe("reviewer-text-findings-defect-head-3  healthy");
    expect(verdict(stdout, quiet)).toBe("threads-closed-code-defect-head-1  healthy");
  });

  // Catches: a usage error read as a verdict. No argument, a path that is no directory, a
  // directory that is no run of a scenario and label health reads, and an option naming nothing
  // each exit 2 and print no run.
  test("a usage error exits 2 and prints no verdict", () => {
    const healthy = run("threads-closed-correct-head-4", [[]]);
    for (const args of [
      [],
      [healthy, path.join(freshDir("health"), "missing")],
      [healthy, freshDir("tester-proof-head")],
      [run("threads-closed-correct-ablate-1", [[]])],
      ["--tmp-snapshot", path.join(freshDir("health"), "missing"), healthy],
      ["--repo", path.join(freshDir("health"), "missing"), healthy],
    ]) {
      const { code, stdout, stderr } = health(...args);
      expect([args, code, stdout]).toEqual([args, 2, ""]);
      expect(stderr).toContain(
        "usage: rig.sh health [--tmp-snapshot <file>] [--repo <git dir>]... <run dir>..."
      );
    }
  });

  /** The world's repository and the route that refuses, as rig.sh's fixture_write writes them:
   * GitHub's 403 to a role App's read of the base's protection, and a route that answers. */
  const WORLD = {
    key: "LWEVAL-1",
    repo: "example/widgets",
    pr: 1001,
    branch: "legion/LWEVAL-1",
    base: "main",
    head: "0123456789abcdef0123",
  };
  const ROUTES = [
    {
      match: "^gh api .*/branches/main/protection",
      stderr: "gh: Resource not accessible by integration (HTTP 403)\n",
      exit: 1,
    },
    { match: "^gh pr view", stdout: { number: 1001 } },
  ];
  /** One tool call of an agent, made at `at`, with the text of its result. */
  type Step = { tool: string; args: object; result?: string; at?: string };
  /** A run named `name` as a worker batch leaves it: its pane file recording it under `ran`, the
   * directory its transcripts' paths are written under (a run read from the archive or from a
   * copy is read under another), <work>/runs/<name> for the work directory `work` (/recorded/work
   * unless one is given), with `pane`'s lines after its own; the world launch wrote for the
   * namespace its agents ran in (namespace.txt: world_paths' lines for the run, as `rig.sh world`
   * prints them from its record under a caller whose scratch root, TMPDIR, is /recorded/scratch,
   * then the lines `world` adds), which a run from before the rig made one holds none of
   * (`namespace` false); the transcript `steps` under `sessions/<agent>` (the run's own for ""),
   * and each of `others` under `sessions/<its agent>`; its gh calls, the world's routes and
   * repository, a reviewer completion taken, its built-from and services-from naming `checkout`
   * (RUN_CHECKOUT unless one is given), and, given `before`, the shared scratch's entries when it
   * started (rig.sh's scratch_before) and, given `created`, the entries a serial run's exit found
   * had appeared there, each with its time (rig.sh's scratch_clean), both in the run's logs as
   * rig.sh writes them, or at its top (`scratchAt` "top") as a run recorded before 2026-11-25
   * holds them. */
  function worldRun(
    name: string,
    {
      steps = [],
      agent = "",
      others = {},
      calls = [],
      before,
      created,
      checkout = RUN_CHECKOUT,
      pane = [],
      work = "/recorded/work",
      namespace = true,
      world = [],
      scratchAt = "logs",
    }: {
      steps?: Step[];
      agent?: string;
      others?: Record<string, Step[]>;
      calls?: object[];
      before?: string[];
      created?: [at: string, entry: string][];
      checkout?: string;
      pane?: string[];
      work?: string;
      namespace?: boolean;
      world?: string[];
      scratchAt?: "logs" | "top";
    }
  ) {
    const dir = run(name, [[complete(200, undefined, "reviewer")]], { checkout, namespace });
    const ran = `${work}/runs/${name}`;
    writeFileSync(
      path.join(dir, "pane.env"),
      [
        `HOME=${ran}/home`,
        `TMPDIR=${ran}/tmp`,
        `SKILL_SCENARIO_RUN=${ran}`,
        `PATH=${ran}/bin:/usr/bin`,
        ...pane,
        "",
      ].join("\n")
    );
    if (namespace) {
      // rig.sh makes its work directory before it reads a command, and the recorded scratch root
      // is no path of this host's, so the directory the run is staged in stands for it.
      const declared = spawnChecked(
        ["bash", path.join(import.meta.dir, "rig.sh"), "world", labelOf(dir), dir],
        {
          env: {
            ...childEnv,
            SKILL_SCENARIOS_WORK: path.dirname(dir),
            TMPDIR: "/recorded/scratch",
          },
        }
      );
      if (declared.exitCode !== 0) throw new Error(`rig.sh world: ${declared.stderr.toString()}`);
      writeFileSync(
        path.join(dir, "namespace.txt"),
        `${declared.stdout.toString()}${world.map((line) => `${line}\n`).join("")}`
      );
    }
    const records = scratchAt === "logs" ? path.join(dir, "logs") : dir;
    if (before !== undefined || created !== undefined) mkdirSync(records, { recursive: true });
    if (before !== undefined)
      writeFileSync(
        path.join(records, "scratch-before.txt"),
        before.map((one) => `${one}\n`).join("")
      );
    if (created !== undefined)
      writeFileSync(
        path.join(records, "scratch-created.txt"),
        created.map(([at, entry]) => `${Date.parse(at) / 1000}.0000000000 ${entry}\n`).join("")
      );
    for (const [into, made] of [[agent, steps] as const, ...Object.entries(others)]) {
      const sessions = path.join(dir, "sessions", into);
      mkdirSync(sessions, { recursive: true });
      const entries = [
        { type: "session", id: "s1" },
        ...made.flatMap((step, index) => [
          {
            type: "message",
            timestamp: step.at ?? "2026-11-14T00:00:00.000Z",
            message: {
              role: "assistant",
              content: [
                { type: "toolCall", id: `c${index}`, name: step.tool, arguments: step.args },
              ],
            },
          },
          ...(step.result === undefined
            ? []
            : [
                {
                  type: "message",
                  message: {
                    role: "toolResult",
                    toolCallId: `c${index}`,
                    toolName: step.tool,
                    content: [{ type: "text", text: step.result }],
                  },
                },
              ]),
        ]),
      ];
      writeFileSync(
        path.join(sessions, TRANSCRIPT),
        entries.map((entry) => `${JSON.stringify(entry)}\n`).join("")
      );
    }
    writeFileSync(
      path.join(dir, "calls.jsonl"),
      [{ at: "2026-11-14T00:00:00.000Z", opened: "calls.jsonl" }, ...calls]
        .map((call) => `${JSON.stringify(call)}\n`)
        .join("")
    );
    writeFileSync(path.join(dir, "fixtures.json"), JSON.stringify({ routes: ROUTES }));
    writeFileSync(path.join(dir, "world.json"), JSON.stringify(WORLD));
    return { dir, ran };
  }
  /** A gh call as the gh stand-in records it. */
  const gh = (by: string, argv: string[], exit: number, route: string | null, review?: object) => ({
    at: "2026-11-14T00:00:01.000Z",
    as: "gh",
    by,
    argv,
    route,
    ...(review === undefined ? {} : { review }),
    exit,
  });

  // Catches: a rule that reads more than it means. This run stays in its own directory, HOME and
  // TMPDIR under the path its pane file records (a `>` inside a quoted sed script, a here-document
  // body naming another run, and prose in a file's content are no writes or reaches), was shown
  // the leak check's source and a clean push's `LEAK CHECK:` lines, and met only the gh refusals
  // reviewer-text-findings declares for a reviewer: the base's protection 403, and the 422 of a
  // submit, with a `--jq` failure over an answer the world gave. Every rule passes it.
  test("a run that stayed in its own world and met only declared refusals is healthy", () => {
    const name = "reviewer-text-findings-text-only-head-5";
    const ran = `/recorded/work/runs/${name}`;
    const { dir } = worldRun(name, {
      steps: [
        {
          tool: "bash",
          args: {
            command: `cat ${ran}/ws/README.md > ${ran}/tmp/copy && cp /etc/hostname ${ran}/ws/h 2>/dev/null && env | sed 's/=x/=<set>/'`,
          },
        },
        {
          tool: "bash",
          args: {
            command: `cat > ${ran}/tmp/body.md <<'EOF'\nsee /recorded/work/runs/threads-closed-correct-base-1/out.txt\nEOF`,
          },
        },
        {
          tool: "write",
          args: { path: `${ran}/tmp/LWEVAL-1-review.json`, content: "writes /tmp/x in prose" },
        },
        { tool: "edit", args: { input: `[${ran}/ws/docs/cli.md#5C66]\nPUT 1.=1:\n+text\n` } },
        {
          tool: "read",
          args: { path: "/opt/leak-check/public-leak-check.sh" },
          result: '26:  echo "LEAK CHECK ERROR: $*. The check could not finish (exit 2)." >&2\n',
        },
        {
          tool: "bash",
          args: { command: `jj -R ${ran}/ws git push --bookmark legion/LWEVAL-1` },
          result: "LEAK CHECK: read 1 commit(s) in 0s\nLEAK CHECK: clean: the push\n",
        },
      ],
      calls: [
        gh(
          "reviewer",
          ["api", "repos/example/widgets/branches/main/protection"],
          1,
          ROUTES[0]?.match ?? ""
        ),
        gh(
          "reviewer",
          ["api", "-X", "POST", "repos/example/widgets/pulls/1001/reviews/6001/events"],
          1,
          null
        ),
        gh(
          "reviewer",
          ["pr", "view", "1001", "--json", "comments", "-q", ".comments[]"],
          1,
          "^gh pr view"
        ),
      ],
    });
    const { code, stdout } = health(dir);
    expect(verdict(stdout, dir)).toBe(`${name}  healthy`);
    for (const rule of ["push guard", "gh", "another run", "another run's write"])
      expect(stdout).toContain(`health: ${rule}: 0 of 1 runs\n`);
    // No write of it left its own directory: a `>` inside a quoted sed script redirects nothing.
    expect(stdout).toContain(
      "health: wrote outside its own directory, HOME and TMPDIR (no verdict): 0 of 1 runs. Appeared in the shared scratch while the run ran, by an unknown writer (no verdict): 0 of 1 runs\n"
    );
    expect(code).toBe(0);
  });

  // Catches: a push the guard refused, or a guard that could not start, read as a healthy run.
  // No world of this rig plants a secret, so either is the guard refusing what a production pod
  // lets through; the 2026-11-14 batch met the first in every base run and in five head ones.
  test("a push-guard refusal an agent was shown is unhealthy, naming the transcript and the line", () => {
    const setup = worldRun("threads-closed-correct-head-4", {
      agent: "reviewer",
      steps: [
        {
          tool: "bash",
          args: { command: "jj git push --bookmark legion/LWEVAL-1" },
          result:
            "Changes to push to origin:\ngit: LEAK CHECK SETUP ERROR: the push to origin: the shared public leak check could not be found: PUBLIC_LEAK_CHECK names none\n",
        },
      ],
    }).dir;
    const refused = worldRun("threads-closed-code-defect-base-4", {
      steps: [
        {
          tool: "bash",
          args: { command: "git push origin HEAD" },
          result:
            "LEAK CHECK: refusing the push to origin: the shared public leak check refused it\n  LEAK CHECK: Nothing was pushed.\n",
        },
      ],
    }).dir;
    const { code, stdout } = health(setup, refused);
    expect(code).toBe(1);
    expect(verdict(stdout, setup)).toBe(
      'threads-closed-correct-head-4  unhealthy: push guard: sessions/reviewer/2026-11-14T00-00-00-000Z_s1.jsonl holds the guard\'s refusal "git: LEAK CHECK SETUP ERROR: the push to origin: the shared public leak check could not be found: PUBLIC_LEAK_CHECK names none"'
    );
    expect(verdict(stdout, refused)).toContain(
      "unhealthy: push guard: sessions/2026-11-14T00-00-00-000Z_s1.jsonl holds the guard's refusal \"LEAK CHECK: refusing the push to origin"
    );
    expect(stdout).toContain("health: push guard: 2 of 2 runs\n");
  });

  // Catches: a refusal of the gh stand-in read as healthy because it is no completion: its own
  // fallback 404 for a call no route of the world answers, which is the world missing an answer
  // GitHub gives (the 2026-11-14 batch's `gh api repos/<repo>`), and a production refusal met by
  // a role the scenario does not declare it for (the base's protection 403 is the reviewer's
  // merge-gate read, and no implementer's). Each call is one its agent typed, as rule 7 reads
  // only those (health.ts's `typedBy`).
  test("a gh refusal the scenario does not declare for the role is unhealthy", () => {
    const typed = (argv: string[]): Step => ({
      tool: "bash",
      args: { command: `gh ${argv.join(" ")}` },
    });
    const read = ["api", "repos/example/widgets", "--jq", ".default_branch"];
    const unrouted = worldRun("reviewer-text-findings-defect-base-3", {
      steps: [typed(read)],
      calls: [gh("reviewer", read, 1, null)],
    }).dir;
    const protection = ["api", "repos/example/widgets/branches/main/protection"];
    const role = worldRun("threads-closed-code-defect-base-2", {
      steps: [typed(protection)],
      calls: [gh("implementer", protection, 1, ROUTES[0]?.match ?? "")],
    }).dir;
    const { code, stdout } = health(unrouted, role);
    expect(code).toBe(1);
    expect(verdict(stdout, unrouted)).toBe(
      "reviewer-text-findings-defect-base-3  unhealthy: gh refusal: calls.jsonl:2, `gh api repos/example/widgets --jq .default_branch` of the reviewer, exited 1: gh: Not Found (HTTP 404) (no route of the world answers it), which reviewer-text-findings does not declare for that role"
    );
    expect(verdict(stdout, role)).toContain(
      "of the implementer, exited 1: gh: Resource not accessible by integration (HTTP 403), which threads-closed does not declare for that role"
    );
    expect(stdout).toContain("health: gh: 2 of 2 runs\n");
  });

  /** The routes rig.sh's fixture_write writes for every world, built here by running it. */
  function sharedRoutes(): { match: string; stderr?: string; exit?: number }[] {
    const dir = freshDir("health-shared-routes");
    const result = bash(
      [
        fn("fixture_write"),
        fn("concluded_checks"),
        fn("base_ruleset_routes"),
        fn("role_identity"),
        "R=$1 worker_key=LWEVAL-1 worker_repo=example/widgets worker_base=main",
        assignment("world_login"),
        ...WORLD_APPS.map(assignment),
        "C1=0123456789abcdef0123 body=a-body worker_pr=1001",
        "worker_pr_url=https://example.invalid/pull/1001",
        "fixture_write '[]' '[]'",
      ].join("\n"),
      [dir]
    );
    expect(result).toMatchObject({ code: 0, stderr: "" });
    return (
      JSON.parse(readFileSync(path.join(dir, "fixtures.json"), "utf8")) as {
        routes: { match: string; stderr?: string; exit?: number }[];
      }
    ).routes;
  }

  // The world holds no workflow run, so GitHub answers the jobs of a run 404 and gh refuses
  // `gh run view` and `gh run watch` of one, or naming none where no terminal can pick one: the
  // answers production gives, which any role meets that follows the merge gate's run read or
  // pr-body.md's CI watch. Catches those refusals read as a world answering less than production,
  // which voids a run whose agent met only GitHub; and the declaration read as one of every 404
  // a route gives, which would pass a world refusing a call GitHub answers (the files of the pull
  // request) as though it were GitHub's.
  test("a gh refusal production gives for a workflow run the world has not got is declared for every role", () => {
    const routes = sharedRoutes();
    const matching = (argv: string[]) => routeFor(routes, argv)?.match ?? null;
    const noRun = (by: string) =>
      [
        ["api", "repos/example/widgets/actions/runs/7/jobs", "--jq", ".jobs[]"],
        ["run", "view", "7", "--log-failed"],
        ["run", "view"],
        ["run", "watch"],
        ["run", "watch", "7"],
      ].map((argv) => gh(by, argv, 1, matching(argv)));
    const withRoutes = (dir: string, extra: object[] = []) =>
      writeFileSync(
        path.join(dir, "fixtures.json"),
        JSON.stringify({ routes: [...extra, ...routes] })
      );
    const reviewing = worldRun("reviewer-text-findings-text-only-head-1", {
      calls: noRun("reviewer"),
    }).dir;
    withRoutes(reviewing);
    const looping = worldRun("threads-closed-correct-head-2", {
      calls: [...noRun("implementer"), ...noRun("reviewer")],
    }).dir;
    withRoutes(looping);
    // A world that refuses a call GitHub answers, in GitHub's own words: still the world's.
    const files = {
      match: "^gh api .*pulls/1001/files",
      stderr: "gh: Not Found (HTTP 404)\n",
      exit: 1,
    };
    const refusing = worldRun("threads-closed-correct-head-3", {
      agent: "reviewer",
      steps: [{ tool: "bash", args: { command: "gh api repos/example/widgets/pulls/1001/files" } }],
      calls: [gh("reviewer", ["api", "repos/example/widgets/pulls/1001/files"], 1, files.match)],
    }).dir;
    withRoutes(refusing, [files]);
    const { code, stdout } = health(reviewing, looping, refusing);
    expect(verdict(stdout, reviewing)).toBe("reviewer-text-findings-text-only-head-1  healthy");
    expect(verdict(stdout, looping)).toBe("threads-closed-correct-head-2  healthy");
    expect(verdict(stdout, refusing)).toBe(
      "threads-closed-correct-head-3  unhealthy: gh refusal: calls.jsonl:2, `gh api repos/example/widgets/pulls/1001/files` of the reviewer, exited 1: gh: Not Found (HTTP 404), which threads-closed does not declare for that role"
    );
    expect(stdout).toContain("health: gh: 1 of 3 runs\n");
    expect(code).toBe(1);
  });

  // Catches: an error answer of the run's own Dispatch to an agent read as healthy because it is
  // no completion and no gh call: the read of a handoff the world records before its first agent
  // (a world that lost it), a 5xx, and a declared answer met before any agent was booted (the
  // rig's own seeding, which takes no error) or after a boot past the roles the scenario boots;
  // a declaration read as one of every status of its path, or of every path it starts; and an
  // answer keyed to the wrong agent: the first booted rather than the one whose turn it came in,
  // or the one booted last before the request ended rather than before it began.
  test("a Dispatch error answer the scenario does not declare for the agent's role is unhealthy, naming the call, the role and the answer", () => {
    const lost = run(
      "reviewer-text-findings-defect-head-6",
      [[complete(200, undefined, "reviewer")]],
      {
        dispatch: [
          handoffRead("2026-11-13T17:50:20.000Z", "plan", 200),
          handoffRead("2026-11-13T17:50:30.000Z", "implement", 404),
        ],
      }
    );
    const later = run("threads-closed-correct-head-12", [[], []], {
      dispatch: [
        handoffRead("2026-11-13T17:50:30.000Z", "plan", 200),
        handoffRead("2026-11-13T17:51:30.000Z", "plan", 200),
        handoffRead("2026-11-13T17:51:40.000Z", "review", 404),
      ],
    });
    // Begun two seconds before the reviewer's boot and ended one second after it.
    const overlapping = run("threads-closed-code-defect-head-16", [[], []], {
      dispatch: [handoffRead("2026-11-13T17:51:01.000Z", "review", 404, 3000)],
    });
    const failing = run("threads-closed-code-defect-base-13", [[]], {
      dispatch: [
        served(
          "2026-11-13T17:50:30.000Z",
          "POST",
          "/api/v1/issues/{key}/results",
          "/api/v1/issues/LWEVAL-1/results",
          500
        ),
      ],
    });
    const seeding = run(
      "reviewer-text-findings-text-only-base-6",
      [[complete(200, undefined, "reviewer")]],
      { dispatch: [handoffRead("2026-11-13T17:45:00.000Z", "plan", 404)] }
    );
    const past = run(
      "reviewer-text-findings-defect-base-7",
      [[complete(200, undefined, "reviewer")], []],
      { dispatch: [handoffRead("2026-11-13T17:51:30.000Z", "plan", 404)] }
    );
    const unauthorized = run(
      "reviewer-text-findings-text-only-head-7",
      [[complete(200, undefined, "reviewer")]],
      { dispatch: [handoffRead("2026-11-13T17:50:30.000Z", "plan", 401)] }
    );
    const unrouted = run(
      "reviewer-text-findings-text-only-head-8",
      [[complete(200, undefined, "reviewer")]],
      {
        dispatch: [
          served(
            "2026-11-13T17:50:30.000Z",
            "GET",
            "unmatched",
            "/api/v1/issues/LWEVAL-1/handoffs/plan/latest/body",
            404
          ),
        ],
      }
    );
    // The plan every world records (seed.ts plan) read back as not found: a world that lost it.
    const planless = run(
      "reviewer-text-findings-text-only-head-9",
      [[complete(200, undefined, "reviewer")]],
      { dispatch: [handoffRead("2026-11-13T17:50:30.000Z", "plan", 404)] }
    );
    const runs = [
      lost,
      later,
      overlapping,
      failing,
      seeding,
      past,
      unauthorized,
      unrouted,
      planless,
    ];
    const { code, stdout } = health(...runs);
    const read = (phase: string) => `GET /api/v1/issues/LWEVAL-1/handoffs/${phase}/latest`;
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      `reviewer-text-findings-defect-head-6  unhealthy: dispatch refusal: logs/dispatch.log:3, ${read("implement")} of the reviewer, answered 404, which reviewer-text-findings does not declare for that role`,
      `threads-closed-correct-head-12  unhealthy: dispatch refusal: logs/dispatch.log:4, ${read("review")} of the reviewer, answered 404, which threads-closed does not declare for that role`,
      `threads-closed-code-defect-head-16  unhealthy: dispatch refusal: logs/dispatch.log:2, ${read("review")} of the implementer, answered 404, which threads-closed does not declare for that role`,
      "threads-closed-code-defect-base-13  unhealthy: dispatch refusal: logs/dispatch.log:2, POST /api/v1/issues/LWEVAL-1/results of the implementer, answered 500, which threads-closed does not declare for that role",
      `reviewer-text-findings-text-only-base-6  unhealthy: dispatch refusal: logs/dispatch.log:2, ${read("plan")}, made before the run's first agent was booted, answered 404, which no role's declaration takes`,
      `reviewer-text-findings-defect-base-7  unhealthy: dispatch refusal: logs/dispatch.log:2, ${read("plan")}, made after boot 2 of the run, past the roles its boot record boots, answered 404, which no role's declaration takes`,
      `reviewer-text-findings-text-only-head-7  unhealthy: dispatch refusal: logs/dispatch.log:2, ${read("plan")} of the reviewer, answered 401, which reviewer-text-findings does not declare for that role`,
      `reviewer-text-findings-text-only-head-8  unhealthy: dispatch refusal: logs/dispatch.log:2, ${read("plan")}/body of the reviewer, answered 404, which reviewer-text-findings does not declare for that role`,
      `reviewer-text-findings-text-only-head-9  unhealthy: dispatch refusal: logs/dispatch.log:2, ${read("plan")} of the reviewer, answered 404, which reviewer-text-findings does not declare for that role`,
    ]);
    expect(stdout).toContain("health: dispatch: 9 of 9 runs\n");
    expect(code).toBe(1);
  });

  // Catches: an answer production gives a correct agent read as a broken world: HANDOFF_NOT_FOUND
  // for the reviewer-text-findings reviewer's `review`, which its world holds none of before the
  // reviewer writes it; and a run whose Dispatch refused nothing read as unhealthy, on its answers
  // of 2xx and 101 or on a line of the server's that is no request's though its message starts
  // with the same words. Every world records the `plan` handoff (seed.ts plan), so a correct
  // agent's read of it is answered 200 here.
  test("a Dispatch error answer the scenario declares for the agent's role, and a Dispatch that refused nothing, are healthy", () => {
    const reviewing = run(
      "reviewer-text-findings-text-only-head-6",
      [[complete(200, undefined, "reviewer")]],
      {
        dispatch: [
          served(
            "2026-11-13T17:45:00.000Z",
            "POST",
            "/api/v1/issues/{key}/handoffs/{phase}",
            "/api/v1/issues/LWEVAL-1/handoffs/implement",
            201
          ),
          handoffRead("2026-11-13T17:50:10.000Z", "plan", 200),
          handoffRead("2026-11-13T17:50:11.000Z", "implement", 200),
          handoffRead("2026-11-13T17:50:12.000Z", "test", 200),
          handoffRead("2026-11-13T17:50:13.000Z", "review", 404),
        ],
      }
    );
    const looping = run("threads-closed-correct-head-13", [[], []], {
      dispatch: [
        handoffRead("2026-11-13T17:50:10.000Z", "plan", 200),
        handoffRead("2026-11-13T17:50:11.000Z", "review", 200),
        handoffRead("2026-11-13T17:51:10.000Z", "plan", 200),
      ],
    });
    const quiet = run("threads-closed-code-defect-head-17", [[]], {
      dispatch: [
        served("2026-11-13T17:50:10.000Z", "GET", "/api/v1/whoami", "/api/v1/whoami", 200),
        'time=2026-11-13T17:50:11.000Z level=WARN msg="dispatch: request authenticated with a previous shared agent token" entry=2 address=127.0.0.1 user_agent=Bun/1.3.5 path=/api/v1/whoami',
        served("2026-11-13T17:50:12.000Z", "GET", "/ws/live", "/ws/live", 101),
      ],
    });
    const runs = [reviewing, looping, quiet];
    const { code, stdout } = health(...runs);
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      "reviewer-text-findings-text-only-head-6  healthy",
      "threads-closed-correct-head-13  healthy",
      "threads-closed-code-defect-head-17  healthy",
    ]);
    expect(stdout).toContain("health: dispatch: 0 of 3 runs\n");
    expect(code).toBe(0);
  });

  // Catches: a run whose Dispatch record is missing read as one whose Dispatch refused nothing,
  // where every run health reads starts a Dispatch of its own with that log; and a request line
  // that names no answer read as no refusal.
  test("a run with no Dispatch log, or with a request line that names no answer, is unhealthy", () => {
    const missing = run("threads-closed-code-defect-head-18", [[]], { dispatch: false });
    const unread = run("threads-closed-code-defect-head-19", [[]], {
      dispatch: [
        'time=2026-11-13T17:50:30.000Z level=INFO msg="dispatch: request" method=GET route=/api/v1/whoami path=/api/v1/whoami elapsed_ms=2',
      ],
    });
    const { code, stdout } = health(missing, unread);
    expect(verdict(stdout, missing)).toBe(
      "threads-closed-code-defect-head-18  unhealthy: no Dispatch record: the run has no <run>/logs/dispatch.log, so whether its Dispatch refused a call of its agents is unknown"
    );
    expect(verdict(stdout, unread)).toStartWith(
      "threads-closed-code-defect-head-19  unhealthy: unreadable record: logs/dispatch.log:2 is no request line of Dispatch's: "
    );
    expect(stdout).toContain("health: dispatch: 2 of 2 runs\n");
    expect(code).toBe(1);
  });

  // Catches: an answer production gives every agent of a Legion pod read as a broken world.
  // `dispatch send-to-agent` posts to /api/v1/agents/{session_id}/messages, a route that requires
  // the person scope, and a pod's agents run under the shared token, so Dispatch answers it 403
  // OAUTH_SCOPE_REQUIRED: production gave the tester's own pod exactly that answer
  // (`dispatch send-to-agent`, 2026-11-20T19:53:57Z). INFRA-764's round 27 at efea8554: the base
  // reviewer of threads-closed-correct-base-1 (run 4b566212b91f) messaged the implementer that
  // way, its world's Dispatch answered as production does, and rule 16 failed the run for it, so
  // the world-health check refused the batch.
  test("Dispatch's 403 to an agent's send-to-agent, which production answers every shared-token session, is healthy", () => {
    const messaging = run("threads-closed-correct-base-14", [[], []], {
      dispatch: [
        handoffRead("2026-11-13T17:50:10.000Z", "plan", 200),
        served(
          "2026-11-13T17:51:10.000Z",
          "POST",
          "/api/v1/agents/{session_id}/messages",
          "/api/v1/agents/01a1fa6a-91d9-730e-8cb2-d7c4a0372466/messages",
          403
        ),
      ],
    });
    const { stdout } = health(messaging);
    expect(verdict(stdout, messaging)).toBe("threads-closed-correct-base-14  healthy");
  });

  // Catches: rule 16 declaring one person-scoped route of the class it holds, where an agent's
  // dispatch command reaches four more, and production answers each 403 OAUTH_SCOPE_REQUIRED to a
  // shared-token session, as it answered send-to-agent: `dispatch inbox` reads /api/v1/inbox,
  // /api/v1/me/replies and /api/v1/credential-requests (packages/envoy-client's inboxCommand), and
  // `dispatch review` posts /api/v1/artifacts/{id}/versions/{number}/reviews (reviewCommand), each
  // `person`-scoped in packages/envoy/internal/dispatch/api/routes_table.go's routeScopes; the
  // tester's own pod ran `dispatch inbox` under the shared token and was answered 403
  // OAUTH_SCOPE_REQUIRED (2026-11-21T03:30:08Z). A run whose agent runs either is as healthy as
  // the one above: the world answered as production does.
  test("Dispatch's 403 to an agent's dispatch inbox and dispatch review, which production answers every shared-token session, is healthy", () => {
    const reading = run("threads-closed-correct-base-15", [[], []], {
      dispatch: [
        handoffRead("2026-11-13T17:50:10.000Z", "plan", 200),
        served("2026-11-13T17:51:10.000Z", "GET", "/api/v1/inbox", "/api/v1/inbox", 403),
        served("2026-11-13T17:51:10.100Z", "GET", "/api/v1/me/replies", "/api/v1/me/replies", 403),
        served(
          "2026-11-13T17:51:10.200Z",
          "GET",
          "/api/v1/credential-requests",
          "/api/v1/credential-requests",
          403
        ),
        served(
          "2026-11-13T17:51:20.000Z",
          "POST",
          "/api/v1/artifacts/{id}/versions/{number}/reviews",
          "/api/v1/artifacts/0b2c0f5e-8d2a-4a51-9a0e-1f2b3c4d5e6f/versions/3/reviews",
          403
        ),
      ],
    });
    const { stdout } = health(reading);
    expect(verdict(stdout, reading)).toBe("threads-closed-correct-base-15  healthy");
  });

  // Catches: production's 403 to `dispatch send-to-agent` declared for some scenarios and roles
  // alone, where production gives it every session of the shared agent token, whatever its
  // scenario and role: threads-closed's implementer, of a scenario that declares nothing for any
  // role, and reviewer-text-findings' reviewer, whose role declares another answer.
  test("Dispatch's 403 to send-to-agent is healthy for every role of every scenario", () => {
    const implementing = run("threads-closed-code-defect-head-80", [[]], {
      dispatch: [
        served(
          "2026-11-13T17:50:30.000Z",
          "POST",
          "/api/v1/agents/{session_id}/messages",
          "/api/v1/agents/01a1fa6a-91d9-730e-8cb2-d7c4a0372466/messages",
          403
        ),
      ],
    });
    const reviewing = run(
      "reviewer-text-findings-defect-head-80",
      [[complete(200, undefined, "reviewer")]],
      {
        dispatch: [
          served(
            "2026-11-13T17:50:30.000Z",
            "POST",
            "/api/v1/agents/{session_id}/messages",
            "/api/v1/agents/01a1fa6a-91d9-730e-8cb2-d7c4a0372466/messages",
            403
          ),
        ],
      }
    );
    const runs = [implementing, reviewing];
    const { stdout } = health(...runs);
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      "threads-closed-code-defect-head-80  healthy",
      "reviewer-text-findings-defect-head-80  healthy",
    ]);
  });

  // Catches: rule 16 declaring every 403 of Dispatch's, or every answer of a person-scoped route,
  // where it declares production's 403 to the shared token on the routes the run's label's
  // Dispatch gives the person scope and its agents' client calls alone (person-routes.ts's
  // `labelPersonRoutes`): a 403 to a result the implementer records, a route that needs no
  // person scope; a 403 to GET /api/v1/users, a person-scoped route no command of the agents'
  // client sends; and a 401 to /api/v1/inbox, a route of the class answered otherwise. Each is a
  // world refusing what production lets through, or answering as production does not.
  test("a Dispatch 403 to a route that needs no person scope or that the agents' client never calls, and another answer of a person-scoped route, are unhealthy", () => {
    const recording = run("threads-closed-code-defect-head-81", [[]], {
      dispatch: [
        served(
          "2026-11-13T17:50:30.000Z",
          "POST",
          "/api/v1/issues/{key}/results",
          "/api/v1/issues/LWEVAL-1/results",
          403
        ),
      ],
    });
    const picking = run("threads-closed-correct-head-81", [[], []], {
      dispatch: [served("2026-11-13T17:51:10.000Z", "GET", "/api/v1/users", "/api/v1/users", 403)],
    });
    const unauthorized = run("threads-closed-correct-base-16", [[], []], {
      dispatch: [served("2026-11-13T17:51:10.000Z", "GET", "/api/v1/inbox", "/api/v1/inbox", 401)],
    });
    const runs = [recording, picking, unauthorized];
    const { code, stdout } = health(...runs);
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      "threads-closed-code-defect-head-81  unhealthy: dispatch refusal: logs/dispatch.log:2, POST /api/v1/issues/LWEVAL-1/results of the implementer, answered 403, which threads-closed does not declare for that role",
      "threads-closed-correct-head-81  unhealthy: dispatch refusal: logs/dispatch.log:2, GET /api/v1/users of the reviewer, answered 403, which threads-closed does not declare for that role",
      "threads-closed-correct-base-16  unhealthy: dispatch refusal: logs/dispatch.log:2, GET /api/v1/inbox of the reviewer, answered 401, which threads-closed does not declare for that role",
    ]);
    expect(stdout).toContain("health: dispatch: 3 of 3 runs\n");
    expect(code).toBe(1);
  });

  // Catches: a 403 judged by a Dispatch source of no shape the reading knows, as a label whose
  // Dispatch gives no route the person scope (the 403 undeclared with no word of why) or by this
  // checkout's source in its place (a label read by another's): the run's commit holds a routes
  // table whose `routeScopes` is renamed, and the verdict names the file, the marker and the
  // commit, never the directory health rebuilt the sources in.
  test("a Dispatch 403 of a run whose label's Dispatch sources hold no person-scoped routes this reads is unhealthy, naming the file and the marker", () => {
    const checkout = freshDir("renamed-scopes");
    const opener = "var routeScopes = map[string]routeScopeSet{";
    for (const file of PERSON_ROUTE_SOURCES) {
      const text = readFileSync(path.join(CHECKOUT, file), "utf8");
      mkdirSync(path.dirname(path.join(checkout, file)), { recursive: true });
      writeFileSync(
        path.join(checkout, file),
        file.endsWith("routes_table.go")
          ? text.replace(opener, "var routeScopesByRoute = map[string]routeScopeSet{")
          : text
      );
    }
    const reading = run("threads-closed-correct-base-17", [[], []], {
      checkout,
      dispatch: [served("2026-11-13T17:51:10.000Z", "GET", "/api/v1/inbox", "/api/v1/inbox", 403)],
    });
    const { code, stdout } = health(reading);
    expect(verdict(stdout, reading)).toBe(
      `threads-closed-correct-base-17  unhealthy: dispatch refusal unknown: logs/dispatch.log:2, GET /api/v1/inbox of the reviewer, answered 403, and which routes the base label's Dispatch gives the person scope that its agents' client calls cannot be read: the Dispatch sources of <commit ${builtCommit(checkout)}> are of no shape its person-scoped routes are read by: packages/envoy/internal/dispatch/api/routes_table.go holds no ${JSON.stringify(opener)}, the line that opens the table of the scopes each route needs`
    );
    expect(code).toBe(1);
  });

  // Catches: a run that reached another run's directory measured as its own: the 2026-11-14
  // agents, refused every push, read the pointer file of a sibling in the same runs directory,
  // and of a run of another batch. Any agent's transcript counts, a later half's too.
  test("a tool call naming another run's directory is unhealthy, wherever that run is", () => {
    const sibling = worldRun("threads-closed-correct-base-2", {
      agent: "reviewer",
      steps: [
        {
          tool: "read",
          args: {
            path: "/recorded/work/runs/threads-closed-correct-head-1/state/home/.config/public-leak-check/path",
          },
        },
      ],
    }).dir;
    const batch = worldRun("reviewer-text-findings-defect-base-4", {
      steps: [
        {
          tool: "bash",
          args: { command: "ls /elsewhere/archive/runs/reviewer-text-findings-defect-base-2/tmp" },
        },
      ],
    }).dir;
    const { code, stdout } = health(sibling, batch);
    expect(code).toBe(1);
    expect(verdict(stdout, sibling)).toBe(
      "threads-closed-correct-base-2  unhealthy: another run: a read call in sessions/reviewer/2026-11-14T00-00-00-000Z_s1.jsonl names /recorded/work/runs/threads-closed-correct-head-1/state/home/.config/public-leak-check/path, under /recorded/work/runs/threads-closed-correct-head-1"
    );
    expect(verdict(stdout, batch)).toContain(
      "unhealthy: another run: a bash call in sessions/2026-11-14T00-00-00-000Z_s1.jsonl names /elsewhere/archive/runs/reviewer-text-findings-defect-base-2/tmp, under /elsewhere/archive/runs/reviewer-text-findings-defect-base-2"
    );
    expect(stdout).toContain("health: another run: 2 of 2 runs\n");
  });

  // Four runs from before the rig gave each run a namespace (no namespace.txt, as the 2026-11-14
  // batch left them), whose /tmp was the machine's: rule 9 judges them, and a run no rule that
  // can judge it fails gets no verdict, since rules 10 and 12 read a world it never recorded.
  // Catches: a run's own write outside its directory read as a verdict on it, which voids the run
  // that wrote a file rather than the one a file reached; a later run's read of an earlier run's
  // write let through, here placed only in a subagent's transcript, one level under a later
  // half's; reads judged in the order the runs were listed rather than by their times, which reads
  // the run that read the path before anyone wrote it as contaminated; a read of an entry the
  // shared scratch held when the run started let through; a write that climbs out of the run
  // read as the run's own; and a line of the runs that wrote outside their directory that does
  // not say which run it was, or where it wrote.
  test("a read of another run's write, or of scratch from before the run, is unhealthy; a run's own writes are not", () => {
    const writer = worldRun("threads-closed-correct-head-5", {
      namespace: false,
      before: [],
      steps: [
        {
          tool: "write",
          args: { path: "/tmp/LWEVAL-1-review.json", content: "{}" },
          at: "2026-11-14T01:00:00.000Z",
        },
        {
          tool: "bash",
          args: {
            command: "gh api -X POST repos/x/pulls/1/reviews --input /tmp/LWEVAL-1-review.json",
          },
          at: "2026-11-14T01:01:00.000Z",
        },
        {
          tool: "write",
          args: {
            path: "/recorded/work/runs/threads-closed-correct-head-5/ws/../../../../../tmp/placeholder",
            content: "",
          },
          at: "2026-11-14T01:02:00.000Z",
        },
      ],
    }).dir;
    const reader = worldRun("threads-closed-code-defect-head-5", {
      namespace: false,
      before: ["/tmp/LWEVAL-1-review.json"],
      agent: "reviewer/2026-11-14T02-00-00-000Z_s2",
      steps: [
        {
          tool: "bash",
          args: { command: "cat /tmp/LWEVAL-1-review.json" },
          at: "2026-11-14T02:00:00.000Z",
        },
      ],
    }).dir;
    const earlier = worldRun("threads-closed-code-defect-head-4", {
      namespace: false,
      before: [],
      steps: [
        {
          tool: "bash",
          args: { command: "cat /tmp/LWEVAL-1-review.json 2>/dev/null" },
          at: "2026-11-14T00:30:00.000Z",
        },
      ],
    }).dir;
    const stale = worldRun("threads-closed-code-defect-head-3", {
      namespace: false,
      before: ["/tmp/pre-batch.json"],
      steps: [
        {
          tool: "read",
          args: { path: "/tmp/pre-batch.json" },
          at: "2026-11-14T03:00:00.000Z",
        },
      ],
    }).dir;
    const { code, stdout, stderr } = health(reader, writer, earlier, stale);
    expect(code).toBe(2);
    expect([writer, earlier].map((dir) => [verdict(stdout, dir), refusal(stderr, dir)])).toEqual([
      [undefined, unrecorded(writer)],
      [undefined, unrecorded(earlier)],
    ]);
    expect(verdict(stdout, reader)).toBe(
      "threads-closed-code-defect-head-5  unhealthy: another run's write: a bash call in sessions/reviewer/2026-11-14T02-00-00-000Z_s2/2026-11-14T00-00-00-000Z_s1.jsonl at 2026-11-14T02:00:00.000Z reads /tmp/LWEVAL-1-review.json, which threads-closed-correct-head-5 wrote at 2026-11-14T01:00:00.000Z (write in sessions/2026-11-14T00-00-00-000Z_s1.jsonl)"
    );
    expect(verdict(stdout, stale)).toBe(
      "threads-closed-code-defect-head-3  unhealthy: shared scratch from before the run: a read call in sessions/2026-11-14T00-00-00-000Z_s1.jsonl at 2026-11-14T03:00:00.000Z reads /tmp/pre-batch.json, which no run given wrote before it and /tmp/pre-batch.json was in /tmp when the run started (scratch-before.txt)"
    );
    expect(stdout).toContain("health: another run's write: 2 of 2 runs\n");
    expect(stdout).toContain(
      "health: wrote outside its own directory, HOME and TMPDIR (no verdict): 1 of 4 runs: threads-closed-correct-head-5 (/tmp/LWEVAL-1-review.json, /tmp/placeholder). Appeared in the shared scratch while the run ran, by an unknown writer (no verdict): 0 of 4 runs\n"
    );
  });

  // rig.sh writes a run's records of the shared scratch into its logs (scratch_before,
  // scratch_clean), which its agents' world hides; a run recorded before 2026-11-25 holds them at
  // its top. Catches: such a run read as one with no record; and a run recorded since read from a
  // file of that name at its top, which its agents could write: here one naming the entry the
  // agent reads, which would make that read one of scratch from before the run.
  test("a run's scratch records are read from its logs, or from its top for a run recorded before they moved", () => {
    const read: Step = {
      tool: "read",
      args: { path: "/tmp/pre-batch.json" },
      at: "2026-11-14T03:00:00.000Z",
    };
    const old = worldRun("threads-closed-code-defect-head-31", {
      namespace: false,
      before: ["/tmp/pre-batch.json"],
      scratchAt: "top",
      steps: [read],
    }).dir;
    const planted = worldRun("threads-closed-code-defect-head-32", {
      namespace: false,
      before: [],
      steps: [read],
    }).dir;
    writeFileSync(path.join(planted, "scratch-before.txt"), "/tmp/pre-batch.json\n");
    const { stdout, stderr } = health(old, planted);
    expect(verdict(stdout, old)).toBe(
      "threads-closed-code-defect-head-31  unhealthy: shared scratch from before the run: a read call in sessions/2026-11-14T00-00-00-000Z_s1.jsonl at 2026-11-14T03:00:00.000Z reads /tmp/pre-batch.json, which no run given wrote before it and /tmp/pre-batch.json was in /tmp when the run started (scratch-before.txt)"
    );
    expect([verdict(stdout, planted), refusal(stderr, planted)]).toEqual([
      undefined,
      unrecorded(planted),
    ]);
  });

  // Catches: a write into a run's own /tmp counted as one outside its directory. In its namespace
  // /tmp is <run>/tmp (rig.sh's private_world), and on 2026-11-16 the tester measured this line at
  // 2 of 8 runs and at 4 of 8 where every write behind both counts was such a one (/tmp/body.md,
  // /tmp/reply-body.txt, /tmp/lweval1-approve.json, /tmp/lweval1-body.md). And the other way: a
  // write that did leave the run, to a path its world shows read-write and the runs of its label
  // share or to one outside its world, left out; a line that gives the count alone, so which runs
  // wrote, and where, is read out of a copy of them; and one that names a path once for each
  // write of it, or every path of a run that wrote many.
  test("a write into a run's own /tmp is none outside its directory, and the line names each run that wrote outside with its paths", () => {
    const bash = (command: string): Step => ({ tool: "bash", args: { command } });
    const write = (target: string): Step => ({
      tool: "write",
      args: { path: target, content: "{}" },
    });
    const own = worldRun("threads-closed-correct-head-11", {
      steps: [
        bash("cat > /tmp/body.md <<'EOF'\nFixed in 0123456.\nEOF"),
        write("/tmp/x.json"),
        bash("gh api -X POST repos/example/widgets/pulls/1001/reviews --input /tmp/x.json"),
        write(
          "/recorded/work/runs/threads-closed-correct-head-11/ws/../../../../../tmp/placeholder"
        ),
      ],
    }).dir;
    const shared = worldRun("threads-closed-correct-head-12", {
      steps: [bash("echo call >> /recorded/work/profiles/head/key/calls")],
    }).dir;
    const climbed = worldRun("threads-closed-code-defect-head-11", {
      steps: [
        write("/legion/tmp/LWEVAL-1-review.json"),
        bash(
          "cp /legion/tmp/LWEVAL-1-review.json /legion/tmp/copy.json && touch /legion/tmp/a /legion/tmp/b"
        ),
        write("/legion/tmp/LWEVAL-1-review.json"),
      ],
    }).dir;
    const runs = [own, shared, climbed];
    const { code, stdout } = health(...runs);
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      "threads-closed-correct-head-11  healthy",
      "threads-closed-correct-head-12  healthy",
      "threads-closed-code-defect-head-11  healthy",
    ]);
    expect(stdout).toContain(
      "health: wrote outside its own directory, HOME and TMPDIR (no verdict): 2 of 3 runs: threads-closed-correct-head-12 (/recorded/work/profiles/head/key/calls); threads-closed-code-defect-head-11 (/legion/tmp/LWEVAL-1-review.json, /legion/tmp/copy.json, /legion/tmp/a and 1 more). Appeared in the shared scratch while the run ran, by an unknown writer (no verdict): 0 of 3 runs\n"
    );
    expect(code).toBe(0);
  });

  // The architect's measure of 2026-11-16 at 19e19657: `1 of 32 runs:
  // reviewer-text-findings-text-only-head-5 (/tmp/go-build218583819)`, a path only that run's
  // scratch-created.txt names, its time 04:46:00Z, six seconds after the run ended, in the
  // machine's /tmp, none of which the run's namespace gave its agents.
  // Catches: an entry only the scratch snapshot recorded named as a write of the run's, in the
  // one kind the line had or in a kind of its own; and the other way: an entry a call of the run
  // wrote, itself or a path under it, named as one that appeared by an unknown writer.
  test("the line names what a run's calls wrote apart from what only appeared in the shared scratch while it ran", () => {
    const appeared = worldRun("reviewer-text-findings-text-only-head-5", {
      created: [["2026-11-16T04:46:00.000Z", "/tmp/go-build218583819"]],
      steps: [{ tool: "bash", args: { command: "cat > /tmp/body.md <<'EOF'\nFixed.\nEOF" } }],
    }).dir;
    const wrote = worldRun("threads-closed-correct-head-24", {
      namespace: false,
      before: [],
      created: [
        ["2026-11-14T00:00:01.000Z", "/tmp/LWEVAL-1-review.json"],
        ["2026-11-14T00:00:02.000Z", "/tmp/lweval1"],
        ["2026-11-14T00:00:03.000Z", "/tmp/go-build1"],
      ],
      steps: [
        { tool: "write", args: { path: "/tmp/LWEVAL-1-review.json", content: "{}" } },
        { tool: "bash", args: { command: "mkdir -p /tmp/lweval1/ws" } },
      ],
    }).dir;
    // head-24 predates the namespace, so no rule that can judge it fails it and it gets no
    // verdict; the line names what it wrote all the same, as it names every run given.
    const { code, stdout, stderr } = health(appeared, wrote);
    expect([appeared, wrote].map((dir) => verdict(stdout, dir))).toEqual([
      "reviewer-text-findings-text-only-head-5  healthy",
      undefined,
    ]);
    expect(refusal(stderr, wrote)).toBe(unrecorded(wrote));
    expect(stdout).toContain(
      "health: wrote outside its own directory, HOME and TMPDIR (no verdict): 1 of 2 runs: threads-closed-correct-head-24 (/tmp/LWEVAL-1-review.json, /tmp/lweval1/ws, /tmp/lweval1). Appeared in the shared scratch while the run ran, by an unknown writer (no verdict): 2 of 2 runs: reviewer-text-findings-text-only-head-5 (/tmp/go-build218583819); threads-closed-correct-head-24 (/tmp/go-build1)\n"
    );
    expect(code).toBe(2);
  });

  // A host with no TMPDIR, whose work directory is under /tmp: the namespace binds the run's own
  // directory and every other path its world shows there back over the run's /tmp.
  // Catches: a path the world shows under /tmp read as one of the run's own /tmp, which hides a
  // write to the model route's record, a path the runs of its label share; and the run's own
  // directory, or its own /tmp, read as the machine's /tmp, whose entry of the work directory
  // was there when the run started, which voids every run of such a host.
  test("under a work directory in /tmp, a run's own directory and what its world shows there are those paths, and the rest its own /tmp", () => {
    const bash = (command: string): Step => ({ tool: "bash", args: { command } });
    const work = "/tmp/recorded-work";
    const name = "threads-closed-correct-head-13";
    const ran = `${work}/runs/${name}`;
    const { dir } = worldRun(name, {
      work,
      before: [work, "/tmp/body.md"],
      steps: [
        bash(`cat ${ran}/ws/README.md ${work}/profiles/head/built-from > /tmp/body.md`),
        bash(`cat /tmp/body.md > ${ran}/ws/notes.md`),
        bash(`echo call >> ${work}/profiles/head/key/calls`),
      ],
    });
    const { code, stdout } = health(dir);
    expect(verdict(stdout, dir)).toBe(`${name}  healthy`);
    expect(stdout).toContain(
      `health: wrote outside its own directory, HOME and TMPDIR (no verdict): 1 of 1 runs: ${name} (${work}/profiles/head/key/calls). Appeared in the shared scratch while the run ran, by an unknown writer (no verdict): 0 of 1 runs\n`
    );
    expect(code).toBe(0);
  });

  // Catches: a run in a namespace held to another run's /tmp or to the machine's. Each run's /tmp
  // is its own <run>/tmp, so a file of one name in two runs' /tmp is two files: the later run's
  // read of it read as a read of the earlier run's write; the same read held to an entry of that
  // name a serial run's exit found in the machine's /tmp (scratch-created.txt), or to one the
  // machine's /tmp held when the run started (scratch-before.txt). And the other way: a path both
  // runs do reach, here one outside every run, no longer read as another run's write.
  test("a run in a namespace reads its own /tmp, whatever another run's or the machine's held, and a path two runs reach is still another run's write", () => {
    const at = (time: string, step: Step): Step => ({ ...step, at: `2026-11-14T${time}.000Z` });
    const write = (target: string): Step => ({
      tool: "write",
      args: { path: target, content: "{}" },
    });
    const read = (target: string): Step => ({ tool: "bash", args: { command: `cat ${target}` } });
    const writer = worldRun("threads-closed-correct-head-14", {
      created: [["2026-11-14T01:30:00.000Z", "/tmp/lweval1-body.md"]],
      steps: [
        at("01:00:00", write("/tmp/LWEVAL-1-review.json")),
        at("01:00:00", write("/legion/tmp/LWEVAL-1-review.json")),
      ],
    }).dir;
    const reader = worldRun("threads-closed-code-defect-head-14", {
      before: ["/tmp/LWEVAL-1-review.json", "/tmp/lweval1-body.md"],
      steps: [at("02:00:00", read("/tmp/LWEVAL-1-review.json /tmp/lweval1-body.md"))],
    }).dir;
    const climber = worldRun("threads-closed-code-defect-head-15", {
      steps: [at("02:00:00", read("/legion/tmp/LWEVAL-1-review.json"))],
    }).dir;
    const { code, stdout } = health(reader, climber, writer);
    const session = "sessions/2026-11-14T00-00-00-000Z_s1.jsonl";
    expect([reader, climber, writer].map((dir) => verdict(stdout, dir))).toEqual([
      "threads-closed-code-defect-head-14  healthy",
      `threads-closed-code-defect-head-15  unhealthy: another run's write: a bash call in ${session} at 2026-11-14T02:00:00.000Z reads /legion/tmp/LWEVAL-1-review.json, which threads-closed-correct-head-14 wrote at 2026-11-14T01:00:00.000Z (write in ${session})`,
      "threads-closed-correct-head-14  healthy",
    ]);
    expect(stdout).toContain("health: another run's write: 1 of 3 runs\n");
    expect(stdout).toContain(
      "health: wrote outside its own directory, HOME and TMPDIR (no verdict): 1 of 3 runs: threads-closed-correct-head-14 (/legion/tmp/LWEVAL-1-review.json). Appeared in the shared scratch while the run ran, by an unknown writer (no verdict): 1 of 3 runs: threads-closed-correct-head-14 (/tmp/lweval1-body.md)\n"
    );
    expect(code).toBe(1);
  });

  // Catches: a directory an agent names as a run in its own /tmp read as another run's. In a
  // namespace no run is under /tmp but the run's own, so the name is a path of <run>/tmp; a run
  // from before the rig made a namespace named the machine's /tmp, where another run may be.
  test("a path named as a run under /tmp is the run's own scratch in a namespace, and another run's where the run had none", () => {
    const steps: Step[] = [
      { tool: "bash", args: { command: "mkdir -p /tmp/threads-closed-correct-base-1/ws" } },
    ];
    const scratch = worldRun("threads-closed-correct-head-16", { steps }).dir;
    const machine = worldRun("threads-closed-correct-head-17", {
      namespace: false,
      steps,
    }).dir;
    const { code, stdout } = health(scratch, machine);
    expect(verdict(stdout, scratch)).toBe("threads-closed-correct-head-16  healthy");
    expect(verdict(stdout, machine)).toBe(
      "threads-closed-correct-head-17  unhealthy: another run: a bash call in sessions/2026-11-14T00-00-00-000Z_s1.jsonl names /tmp/threads-closed-correct-base-1/ws, under /tmp/threads-closed-correct-base-1"
    );
    expect(stdout).toContain("health: another run: 1 of 2 runs\n");
    expect(stdout).toContain(
      "health: wrote outside its own directory, HOME and TMPDIR (no verdict): 1 of 2 runs: threads-closed-correct-head-17 (/tmp/threads-closed-correct-base-1/ws). Appeared in the shared scratch while the run ran, by an unknown writer (no verdict): 0 of 2 runs\n"
    );
    expect(code).toBe(1);
  });

  // Rule 10 reads the run's namespace (the architect's ruling of 2026-11-18T08:40Z). In a run's
  // namespace /tmp is its own <run>/tmp, and a run from before the namespace recorded no world, so
  // rule 10 does not judge it (the architect's ruling of 2026-11-18T14:59Z: no world is worked out
  // again from the pod) and a run no other rule fails gets no verdict. Catches: a run in a
  // namespace held to the machine's /tmp; and a run with none held to a world worked out at
  // judging time, which read this one unhealthy under the judge's TMPDIR.
  test("a name under /tmp is a run's own in its namespace, and a run with none is not judged by a world", () => {
    const work = "/tmp/recorded-work";
    const steps: Step[] = [{ tool: "read", args: { path: `${work}/profiles/base/built-from` } }];
    const own = worldRun("threads-closed-correct-head-18", { work, before: [], steps }).dir;
    const machine = worldRun("threads-closed-correct-head-19", {
      work,
      namespace: false,
      before: [],
      steps,
    }).dir;
    const { code, stdout, stderr } = health(own, machine);
    expect([own, machine].map((dir) => verdict(stdout, dir))).toEqual([
      "threads-closed-correct-head-18  healthy",
      undefined,
    ]);
    expect(refusal(stderr, machine)).toBe(unrecorded(machine));
    expect(stdout).toContain("health: outside its world: 0 of 1 runs\n");
    expect(code).toBe(2);
  });

  // A run in a namespace is judged by the world it recorded (namespace.txt), never by one worked
  // out again at judging time: round 25's reviewer-text-findings-defect-head-1 (b37dc3dc7f25),
  // judged from a copy whose work directory was gone, read "outside its world" for a skill of its
  // label's install under the scratch root, which its namespace showed. Catches: the world read
  // through `rig.sh world` for every run, which reads a shown install as covered (head-20) and
  // takes the scratch root from the judge's TMPDIR rather than from the run's (head-21, judged
  // under another); and a run from before the namespace held to the world `rig.sh world` works
  // out of the pod (head-22, which read unhealthy under the judge's TMPDIR until the architect's
  // ruling of 2026-11-18T14:59Z).
  test("a run in a namespace is held to the world it recorded, whatever the judge's TMPDIR, and one with none to no world", () => {
    const read = (target: string): Step => ({ tool: "read", args: { path: target } });
    const session = "sessions/2026-11-14T00-00-00-000Z_s1.jsonl";
    const hidden = "which the run's world hides and does not show again (rig.sh's world_paths)";
    const shown = worldRun("threads-closed-correct-head-20", {
      world: ["ro /recorded/scratch/installs/abc"],
      steps: [read("/recorded/scratch/installs/abc/skills/legion-worker/SKILL.md")],
    }).dir;
    const atRecording = health(shown);
    expect(verdict(atRecording.stdout, shown)).toBe("threads-closed-correct-head-20  healthy");
    const covered = worldRun("threads-closed-correct-head-21", {
      steps: [read("/recorded/scratch/another-role/notes.md")],
    }).dir;
    const old = worldRun("threads-closed-correct-head-22", {
      namespace: false,
      steps: [read("/judging/scratch/another-role/notes.md")],
    }).dir;
    const elsewhere = healthIn({ TMPDIR: "/judging/scratch" }, covered, old);
    expect([covered, old].map((dir) => verdict(elsewhere.stdout, dir))).toEqual([
      `threads-closed-correct-head-21  unhealthy: outside its world: a read call in ${session} names /recorded/scratch/another-role/notes.md, under /recorded/scratch, ${hidden}`,
      undefined,
    ]);
    expect(refusal(elsewhere.stderr, old)).toBe(unrecorded(old));
  });

  // Catches: rule 10 reading a name of an ancestor of the run's own directory as a reach out of
  // its world. The namespace covers the work directory with a tmpfs and mounts the run's own
  // directory under it, so the ancestors it makes for that mount hold the run alone, and a call
  // that names one reads nothing the world hides. INFRA-764's round 32 at b281d36d: in run
  // 189e7ebeb7da (threads-closed, correct, base) a subagent of the reviewer, its cwd
  // <run>/cwd, ran `find /legion/scratch/tmp/b32/w/runs -maxdepth 3 -name '*.ts'` and found only
  // its own run's <run>/src and <run>/ws files, and health read "outside its world ... names
  // /legion/scratch/tmp/b32/w/runs, under /legion/scratch/tmp, which the run's world hides and
  // does not show again". A name beside that ancestor, which the cover hides, stays a reach out.
  test("a name of an ancestor of the run's own directory is in its world, and a name beside it is not", () => {
    const work = "/recorded/scratch/w";
    const ancestor = worldRun("threads-closed-correct-head-97", {
      work,
      steps: [
        { tool: "bash", args: { command: `find ${work}/runs -maxdepth 3 -name '*.ts'` } },
        { tool: "bash", args: { command: `ls -la ${work}` } },
      ],
    }).dir;
    const beside = worldRun("threads-closed-correct-head-98", {
      work,
      steps: [{ tool: "read", args: { path: `${work}/notes.md` } }],
    }).dir;
    const { stdout } = health(ancestor, beside);
    expect([ancestor, beside].map((dir) => verdict(stdout, dir))).toEqual([
      "threads-closed-correct-head-97  healthy",
      `threads-closed-correct-head-98  unhealthy: outside its world: a read call in sessions/2026-11-14T00-00-00-000Z_s1.jsonl names ${work}/notes.md, under /recorded/scratch, which the run's world hides and does not show again (rig.sh's world_paths)`,
    ]);
  });

  // Catches: rule 10 reading the directory health is given as the run's own world. A judge reads a
  // copy or the archive (<judge>/archive/<id>), which no world of the run showed: the run's world
  // is the directory its record names and the paths its namespace showed, so a name above the
  // copy, or under it but not under a path the run showed, is read as any other (the tester's
  // finding of 2026-11-22T04:07:41Z, the class of round 25's "a reader reads the run's archive
  // and its recorded provenance alone"). Here the judge's directory is under what the run's world
  // covers, as a scratch root the run shared with its judge is. It is outside /tmp, under the
  // caller's HOME, wherever the suite's TMPDIR is: in a namespace /tmp is the run's own <run>/tmp,
  // so a name there is in its world, and a judge under a TMPDIR in /tmp (CI's) would test nothing.
  const judges = temporaryDirectories("health-judge", { root: homedir() });
  test("a name above or under the copy health is given is no part of the run's world", () => {
    const judge = judges();
    expect([judge, judge === "/tmp" || judge.startsWith("/tmp/")]).toEqual([judge, false]);
    const copy = (name: string, steps: Step[]) => {
      const at = path.join(judge, "archive", name);
      cpSync(worldRun(name, { steps, world: [`cover ${judge}`] }).dir, at, { recursive: true });
      return at;
    };
    const above = copy("threads-closed-correct-head-95", [
      { tool: "bash", args: { command: `ls -la ${judge}/archive` } },
    ]);
    const under = copy("threads-closed-correct-head-96", [
      {
        tool: "read",
        args: { path: `${judge}/archive/threads-closed-correct-head-96/notes.md` },
      },
    ]);
    const { stdout } = health(above, under);
    const session = "sessions/2026-11-14T00-00-00-000Z_s1.jsonl";
    const hidden = `under ${judge}, which the run's world hides and does not show again (rig.sh's world_paths)`;
    expect([above, under].map((dir) => verdict(stdout, dir))).toEqual([
      `threads-closed-correct-head-95  unhealthy: outside its world: a bash call in ${session} names ${judge}/archive, ${hidden}`,
      `threads-closed-correct-head-96  unhealthy: outside its world: a read call in ${session} names ${under}/notes.md, ${hidden}`,
    ]);
  });

  // A run recorded before 2026-11-19 holds an `empty` line for its system git configuration
  // (world_paths' old kind: an empty file bound over the host's), and a run since holds a `ro`
  // line for the host's own file, which nothing mounts. Catches: health reading either as a path
  // the world hides, so a read of /etc/gitconfig, which every agent's git makes, reads as a reach
  // out of the world; and health refusing a run for a kind its rules do not read.
  test("a read of the system git configuration is in the run's world, whether the run recorded the old `empty` line or a `ro` one", () => {
    const steps: Step[] = [
      { tool: "read", args: { path: "/etc/gitconfig" } },
      { tool: "bash", args: { command: "cat /etc/gitconfig && git config -l --show-scope" } },
    ];
    /** A run whose world holds `line` for /etc/gitconfig and no other line for it. */
    const recorded = (name: string, line: string) => {
      const { dir } = worldRun(name, { steps });
      const file = path.join(dir, "namespace.txt");
      const kept = readFileSync(file, "utf8")
        .split("\n")
        .filter((one) => one !== "" && !one.endsWith(" /etc/gitconfig"));
      writeFileSync(file, [...kept, line, ""].join("\n"));
      return dir;
    };
    const runs = [
      recorded("threads-closed-correct-head-45", "empty /etc/gitconfig"),
      recorded("threads-closed-correct-head-46", "ro /etc/gitconfig"),
    ];
    const { code, stdout } = health(...runs);
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      "threads-closed-correct-head-45  healthy",
      "threads-closed-correct-head-46  healthy",
    ]);
    expect(stdout).toContain("health: outside its world: 0 of 2 runs\n");
    expect(code).toBe(0);
  });

  // Both arms of a batch run in one pod, whose system git configuration every run's world shows
  // as the pod holds it (rig.sh's world_paths) and every run records (services_from's
  // `system-gitconfig`). Catches: a batch whose runs record two read as healthy, though their
  // agents' git did not read one world; and two runs compared that are of two batches, or a run
  // compared that records none (one recorded before 2026-11-19).
  test("runs of one batch that record two system git configurations are unhealthy, and runs of two batches are not compared", () => {
    const sha = (digit: string) => `sha256:${digit.repeat(64)}`;
    /** A run of `batch` with the id `id`, its record beside it, whose provenance records the
     * system git configuration /etc/gitconfig at `digest`, or none where that is undefined. */
    const recorded = (n: number, id: string, batch: string, digest?: string) => {
      const name = `threads-closed-correct-head-${n}`;
      const named = worldRun(name, {}).dir;
      const dir = path.join(path.dirname(named), id);
      renameSync(named, dir);
      writeFileSync(
        `${dir}.json`,
        JSON.stringify({
          run: id,
          scenario: "threads-closed",
          case: "correct",
          label: "head",
          n,
          name,
          batch,
          ran: dir,
        })
      );
      if (digest !== undefined)
        appendFileSync(
          path.join(dir, "services-from"),
          `system-gitconfig shared /etc/gitconfig ${digest}\n`
        );
      return dir;
    };
    const runs = [
      recorded(47, "0123456789a1", "legion-00000000-1", sha("a")),
      recorded(48, "0123456789a2", "legion-00000000-1", sha("b")),
      recorded(49, "0123456789a3", "legion-00000000-2", sha("a")),
      recorded(50, "0123456789a4", "legion-00000000-2"),
    ];
    const { code, stdout } = health(...runs);
    const differ = (other: string, digest: string, theirs: string) =>
      `system git configuration: <run>/services-from records /etc/gitconfig at ${digest}, and ${other} of its batch legion-00000000-1 records /etc/gitconfig at ${theirs}, so the batch's agents' git did not read one pod's system configuration`;
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      `0123456789a1  unhealthy: ${differ("0123456789a2", sha("a"), sha("b"))}  (threads-closed-correct-head-47)`,
      `0123456789a2  unhealthy: ${differ("0123456789a1", sha("b"), sha("a"))}  (threads-closed-correct-head-48)`,
      "0123456789a3  healthy  (threads-closed-correct-head-49)",
      "0123456789a4  healthy  (threads-closed-correct-head-50)",
    ]);
    expect(stdout).toContain("health: system git configuration: 2 of 4 runs\n");
    expect(code).toBe(1);
  });

  // Each launch mounts a tmpfs of its own on /dev/shm (rig.sh's private_world), so two runs in
  // namespaces that write and read one name there each reach their own; two runs from before the
  // namespace shared the machine's. Catches: `mounted` reading /tmp alone, so the later run in a
  // namespace reads as reading the earlier one's write, and the line of writes outside a run
  // lists a write the machine never kept; and a run from before the namespace no longer read on
  // the machine's /dev/shm.
  test("a name under /dev/shm is a tmpfs of the agent's launch alone in a namespace, and the machine's in a run with none", () => {
    const at = (time: string, step: Step): Step => ({ ...step, at: `2026-11-14T${time}.000Z` });
    const shm = "/dev/shm/LWEVAL-1-review.json";
    const write: Step = { tool: "write", args: { path: shm, content: "{}" } };
    const read: Step = { tool: "bash", args: { command: `cat ${shm}` } };
    const writer = worldRun("threads-closed-correct-head-23", {
      steps: [at("01:00:00", write)],
    });
    const reader = worldRun("threads-closed-code-defect-head-23", {
      steps: [at("02:00:00", read)],
    });
    const oldWriter = worldRun("threads-closed-correct-head-24", {
      namespace: false,
      before: [],
      steps: [at("01:30:00", write)],
    });
    const oldReader = worldRun("threads-closed-code-defect-head-24", {
      namespace: false,
      before: [],
      steps: [at("02:30:00", read)],
    });
    const runs = [writer, reader, oldWriter, oldReader].map((one) => one.dir);
    const { code, stdout, stderr } = health(...runs);
    const session = "sessions/2026-11-14T00-00-00-000Z_s1.jsonl";
    // correct-head-24 predates the namespace and no rule that can judge it fails it.
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      "threads-closed-correct-head-23  healthy",
      "threads-closed-code-defect-head-23  healthy",
      undefined,
      `threads-closed-code-defect-head-24  unhealthy: another run's write: a bash call in ${session} at 2026-11-14T02:30:00.000Z reads ${shm}, which threads-closed-correct-head-24 wrote at 2026-11-14T01:30:00.000Z (write in ${session})`,
    ]);
    expect(refusal(stderr, oldWriter.dir)).toBe(unrecorded(oldWriter.dir));
    expect(stdout).toContain("health: another run's write: 1 of 3 runs\n");
    expect(stdout).toContain(
      `health: wrote outside its own directory, HOME and TMPDIR (no verdict): 1 of 4 runs: threads-closed-correct-head-24 (${shm}). Appeared in the shared scratch while the run ran, by an unknown writer (no verdict): 0 of 4 runs\n`
    );
    expect(code).toBe(2);
  });

  // Each agent a run boots starts in a launch of its own (rig.sh's worker_boot), with a /dev/shm
  // of its own, so a script one agent wrote there is gone for the next, while the run's /tmp is
  // the same <run>/tmp for every agent. Catches: rule 13 crediting one agent's script under
  // /dev/shm to a later agent of the run that runs its path (head-25); and the other way, a
  // script the writer ran itself (head-26), one in the run's own /tmp (head-27), or one under
  // /dev/shm of a run from before the namespace, whose agents shared the machine's (head-28),
  // read as reaching nothing.
  test("a script one agent wrote into its launch's /dev/shm is that agent's alone, and one in the run's /tmp every agent's", () => {
    const url = "https://api.github.com/repos/cybermonklabs/legion/pulls/2827";
    const wrote = (target: string): Step => ({
      tool: "write",
      args: { path: target, content: `curl -sS ${url}\n` },
      at: "2026-11-14T01:00:00.000Z",
    });
    const ran = (target: string): Step => ({
      tool: "bash",
      args: { command: `bash ${target}` },
      at: "2026-11-14T02:00:00.000Z",
    });
    const shm = "/dev/shm/fetch.sh";
    const later = worldRun("threads-closed-correct-head-25", {
      steps: [wrote(shm)],
      others: { reviewer: [ran(shm)] },
    }).dir;
    const writer = worldRun("threads-closed-correct-head-26", {
      steps: [wrote(shm), ran(shm)],
    }).dir;
    const scratch = worldRun("threads-closed-correct-head-27", {
      steps: [wrote("/tmp/fetch.sh")],
      others: { reviewer: [ran("/tmp/fetch.sh")] },
    }).dir;
    const old = worldRun("threads-closed-correct-head-28", {
      namespace: false,
      steps: [wrote(shm)],
      others: { reviewer: [ran(shm)] },
    }).dir;
    const runs = [later, writer, scratch, old];
    const { stdout } = health(...runs);
    const implementer = "sessions/2026-11-14T00-00-00-000Z_s1.jsonl";
    const reviewer = "sessions/reviewer/2026-11-14T00-00-00-000Z_s1.jsonl";
    const reach = (by: string, script: string) =>
      `unhealthy: off-world network: a bash call in ${by} reaches api.github.com (${url}) in ${script}, which a write call in ${implementer} wrote and it runs, no host of the run's world (127.0.0.1, localhost, ::1)`;
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      "threads-closed-correct-head-25  healthy",
      `threads-closed-correct-head-26  ${reach(implementer, shm)}`,
      `threads-closed-correct-head-27  ${reach(reviewer, "/tmp/fetch.sh")}`,
      `threads-closed-correct-head-28  ${reach(reviewer, shm)}`,
    ]);
  });

  // Catches, one run each: a run that reads a path of the scratch root outside its world (another
  // role's notes); a base run that reads the head label's checkout, whose checkout only the head
  // run's built-from names; a read of PID 1's environment, where the 2026-11-14 probe found the
  // caller's Dispatch variables; a path under /var/run/legion/private; a curl of GitHub's API; a
  // read that fetches a GitHub page; and a web search that came back with a page of the
  // repository under test, whose query alone is part of the world (the architect's ruling of
  // 2026-11-22T01:44:54Z), each read as healthy. And the run that
  // reaches only the loopback and the model gateway its profile's models.yml names, and reads
  // only its own checkout and its label's profile, read as unhealthy. The scratch root is the one
  // each run's namespace.txt covers, /recorded/scratch, whatever the judge's TMPDIR.
  test("a run that reaches out of its world, by path, process, credential or network, is unhealthy", () => {
    const read = (target: string): Step => ({ tool: "read", args: { path: target } });
    const bash = (command: string): Step => ({ tool: "bash", args: { command } });
    const scratch = worldRun("threads-closed-correct-head-6", {
      steps: [read("/recorded/scratch/another-role/notes.md")],
    }).dir;
    const otherLabel = worldRun("threads-closed-correct-base-6", {
      checkout: "/recorded/base-co",
      steps: [read("/recorded/head-co/skills/legion-worker/references/review-threads.md")],
    }).dir;
    // The 2026-11-14 probe ran before the rig gave each run a namespace of its own, so its PID 1
    // was the pod's.
    const proc = worldRun("threads-closed-code-defect-head-6", {
      namespace: false,
      steps: [bash("tr '\\0' '\\n' </proc/1/environ | grep DISPATCH")],
    }).dir;
    const secret = worldRun("threads-closed-code-defect-head-7", {
      steps: [read("/var/run/legion/private/token")],
    }).dir;
    const curl = worldRun("threads-closed-code-defect-head-8", {
      steps: [bash("curl -sS https://api.github.com/repos/cybermonklabs/legion/pulls/2827")],
    }).dir;
    const fetched = worldRun("threads-closed-code-defect-head-9", {
      steps: [read("https://github.com/cybermonklabs/legion/blob/main/README.md")],
    }).dir;
    const searched = worldRun("threads-closed-code-defect-base-6", {
      steps: [
        {
          tool: "web_search",
          args: { query: "legion review threads" },
          result:
            "[1] legion/skills at main · cybermonklabs/legion\n    https://github.com/cybermonklabs/legion/tree/main/skills\n",
        },
      ],
    }).dir;
    const gateway = "http://model-gateway.example.invalid:4000";
    const own = worldRun("threads-closed-correct-head-7", {
      checkout: "/recorded/head-co",
      before: [],
      // A world's address off the loopback, as a batch given SKILL_SCENARIOS_NATS_URL has one.
      pane: ["OMP_PROFILE=skills-head", "DISPATCH_URL=http://dispatch.example.invalid:4101"],
      steps: [
        bash(
          `curl -sS http://127.0.0.1:4100/v1/sessions && curl -sS http://dispatch.example.invalid:4101/api/v1/whoami && curl -sS ${gateway}/v1/models`
        ),
        read("/recorded/head-co/skills/legion-worker/references/review-threads.md"),
        read("/recorded/work/profiles/head/built-from"),
        read("/tmp/LWEVAL-1-review.json"),
        bash("tr '\\0' '\\n' </proc/self/environ | grep -c SKILL_SCENARIO_RUN"),
      ],
    }).dir;
    const agentDir = path.join(own, "home", ".omp", "profiles", "skills-head", "agent");
    mkdirSync(agentDir, { recursive: true });
    writeFileSync(
      path.join(agentDir, "models.yml"),
      JSON.stringify({ providers: { anthropic: { baseUrl: gateway } } })
    );
    const runs = [scratch, otherLabel, proc, secret, curl, fetched, searched, own];
    const { code, stdout } = health(...runs);
    expect(code).toBe(1);
    const session = "sessions/2026-11-14T00-00-00-000Z_s1.jsonl";
    const hidden = "which the run's world hides and does not show again (rig.sh's world_paths)";
    const loopback = "127.0.0.1, localhost, ::1";
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      `threads-closed-correct-head-6  unhealthy: outside its world: a read call in ${session} names /recorded/scratch/another-role/notes.md, under /recorded/scratch, ${hidden}`,
      `threads-closed-correct-base-6  unhealthy: outside its world: a read call in ${session} names /recorded/head-co/skills/legion-worker/references/review-threads.md, under /recorded/head-co, ${hidden}`,
      `threads-closed-code-defect-head-6  unhealthy: another process: a bash call in ${session} reads /proc/1/environ, a process the run does not own`,
      `threads-closed-code-defect-head-7  unhealthy: credentials: a read call in ${session} names /var/run/legion/private/token, under /var/run/legion`,
      `threads-closed-code-defect-head-8  unhealthy: off-world network: a bash call in ${session} reaches api.github.com (https://api.github.com/repos/cybermonklabs/legion/pulls/2827), no host of the run's world (${loopback})`,
      `threads-closed-code-defect-head-9  unhealthy: off-world network: a read call in ${session} reaches github.com (https://github.com/cybermonklabs/legion/blob/main/README.md), no host of the run's world (${loopback})`,
      `threads-closed-code-defect-base-6  unhealthy: off-world network: a web_search call in ${session} searches the web, which no host of the run's world serves, and it came back with https://github.com/cybermonklabs/legion, a page of the repository under test`,
      "threads-closed-correct-head-7  healthy",
    ]);
    for (const [rule, count] of [
      ["outside its world", 2],
      ["another process", 1],
      ["credentials", 1],
      ["off-world network", 3],
    ] as const)
      expect(stdout).toContain(`health: ${rule}: ${count} of 8 runs\n`);
    // No reason names the gateway: the healthy run's call to it is in its world.
    expect(stdout).not.toContain("model-gateway.example.invalid");
  });

  // The runs here shared the pod's processes (no namespace.txt), as every run did before the rig
  // gave each a PID namespace of its own, where every process /proc shows is the run's own (the
  // row below this one). Rule 11 judges them; one it passes, which no other rule that can judge
  // it fails, gets no verdict, since rules 10 and 12 read a world it never recorded.
  // Catches: a read of the caller's own shell's environment, `/proc/$$/environ`, read as another
  // process's. `$$`, `${$}` and `$BASHPID` name the shell that runs the command, as `/proc/self`
  // names the caller, and the worker prompt both labels load teaches that read
  // (packages/daemon/internal/prompts/go/worker-common.md: `export DISPATCH_URL="$(tr '\0' '\n' <
  // /proc/$$/environ | sed -n 's/^DISPATCH_URL=//p')"`): two runs of the 2026-11-16 batch that
  // looked their session id up that way were called unhealthy and replaced
  // (reviewer-text-findings-text-only-base-3, threads-closed-correct-head-1). And the other
  // way: a variable or a glob that can name another process read as the caller's own.
  test("a read of the caller's own shell's environment is no other process's, and a variable or a glob in its place still is", () => {
    const bash = (command: string): Step => ({ tool: "bash", args: { command } });
    const own = worldRun("threads-closed-correct-head-21", {
      namespace: false,
      steps: [
        bash(
          "export DISPATCH_URL=\"$(tr '\\0' '\\n' < /proc/$$/environ | sed -n 's/^DISPATCH_URL=//p')\""
        ),
        bash("tr '\\0' '\\n' < /proc/$BASHPID/environ | grep -c DISPATCH"),
        bash(["tr '\\0' '\\n' < /proc/$", "{$}/environ | grep -c DISPATCH"].join("")),
        bash(["cat /proc/$", "{BASHPID}/cmdline"].join("")),
      ],
    }).dir;
    const variable = worldRun("threads-closed-correct-head-22", {
      namespace: false,
      steps: [bash("tr '\\0' '\\n' < /proc/$pid/environ | grep DISPATCH")],
    }).dir;
    const glob = worldRun("threads-closed-correct-head-23", {
      namespace: false,
      steps: [bash("cat /proc/*/environ | tr '\\0' '\\n' | grep DISPATCH")],
    }).dir;
    const runs = [own, variable, glob];
    const { stdout, stderr } = health(...runs);
    const session = "sessions/2026-11-14T00-00-00-000Z_s1.jsonl";
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      undefined,
      `threads-closed-correct-head-22  unhealthy: another process: a bash call in ${session} reads /proc/$pid/environ, a process the run does not own`,
      `threads-closed-correct-head-23  unhealthy: another process: a bash call in ${session} reads /proc/*/environ, a process the run does not own`,
    ]);
    expect(refusal(stderr, own)).toBe(unrecorded(own));
  });

  // A run's agents start in a PID namespace of the run's own (rig.sh's launch.sh: `unshare -Urm
  // -p --fork --mount-proc`), so its /proc is that namespace's and every process it shows,
  // PID 1 among them, is the run's own; a run from before the rig made a namespace (no
  // <run>/namespace.txt) shared the pod's processes. Measured in INFRA-764 round 25
  // (not-counted/2e4bff83420a, threads-closed-correct-head-2, which holds namespace.txt): the
  // reviewer looked its own session id up with `tr '\0' '\n' < /proc/1/environ`, read
  // "unhealthy: another process", and was replaced. Catches: a namespaced run's own PID 1, or any
  // other PID its /proc shows, read as another process; and a run with no namespace let through
  // for the same reads.
  test("every process a run's PID namespace shows is the run's own, and in a run with none it is another process's", () => {
    const bash = (command: string): Step => ({ tool: "bash", args: { command } });
    const pidOne =
      "SID=\"$(tr '\\0' '\\n' < /proc/1/environ | sed -n 's/^DISPATCH_SESSION_ID=//p')\"";
    const others =
      "tr '\\0' ' ' < /proc/$PPID/cmdline; tr '\\0' '\\n' < /proc/42/environ | head -1";
    const own = worldRun("threads-closed-code-defect-head-41", { steps: [bash(pidOne)] }).dir;
    const shared = worldRun("threads-closed-code-defect-head-42", {
      namespace: false,
      steps: [bash(pidOne)],
    }).dir;
    const ownOthers = worldRun("threads-closed-code-defect-head-43", { steps: [bash(others)] }).dir;
    const sharedOthers = worldRun("threads-closed-code-defect-head-44", {
      namespace: false,
      steps: [bash(others)],
    }).dir;
    const runs = [own, shared, ownOthers, sharedOthers];
    const { stdout } = health(...runs);
    const session = "sessions/2026-11-14T00-00-00-000Z_s1.jsonl";
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      "threads-closed-code-defect-head-41  healthy",
      `threads-closed-code-defect-head-42  unhealthy: another process: a bash call in ${session} reads /proc/1/environ, a process the run does not own`,
      "threads-closed-code-defect-head-43  healthy",
      `threads-closed-code-defect-head-44  unhealthy: another process: a bash call in ${session} reads /proc/$PPID/cmdline, a process the run does not own`,
    ]);
  });

  // The architect's ruling of 2026-11-18T14:59Z: a run from before the namespace recorded no
  // world, and its world is never worked out again from the pod (`rig.sh world` took the scratch
  // root from the judge's TMPDIR and the label's install from the work directory as it stands).
  // Rules 10 and 12 read that world, so they do not judge such a run; the other rules do, and one
  // that fails it makes it unhealthy, since a run is healthy only when every rule holds. A run they
  // all pass gets no verdict: stderr names it and namespace.txt, and health exits 2 (the
  // architect's condition of 18:56Z: a refusal is not a verdict). Judged under a TMPDIR that names
  // the scratch root its reads are under, so a world worked out at judging time would cover them.
  // Catches: the world worked out of the pod for such a run, which read head-45 unhealthy outside
  // its world here and healthy under another TMPDIR; any world assumed for it (an empty one read
  // it healthy); and a run a world-free rule fails refused rather than unhealthy (head-46).
  test("a run from before the namespace is judged by the rules that need no world, and gets no verdict where they all pass it", () => {
    const read = (target: string): Step => ({ tool: "read", args: { path: target } });
    const reaches = worldRun("threads-closed-code-defect-head-45", {
      namespace: false,
      steps: [read("/recorded/scratch/another-role/notes.md"), read("/var/run/legion/token")],
    }).dir;
    const proc = worldRun("threads-closed-code-defect-head-46", {
      namespace: false,
      steps: [{ tool: "bash", args: { command: "tr '\\0' '\\n' </proc/1/environ" } }],
    }).dir;
    const recorded = worldRun("threads-closed-code-defect-head-47", {}).dir;
    const { code, stdout, stderr } = healthIn(
      { TMPDIR: "/recorded/scratch" },
      reaches,
      proc,
      recorded
    );
    expect(code).toBe(2);
    expect([reaches, proc, recorded].map((dir) => verdict(stdout, dir))).toEqual([
      undefined,
      "threads-closed-code-defect-head-46  unhealthy: another process: a bash call in sessions/2026-11-14T00-00-00-000Z_s1.jsonl reads /proc/1/environ, a process the run does not own",
      "threads-closed-code-defect-head-47  healthy",
    ]);
    expect(stderr.split("\n").filter((line) => line.startsWith("health: "))).toEqual([
      unrecorded(reaches),
      "health: 1 of 3 runs refused",
    ]);
    expect(stdout).toContain("health: outside its world: 0 of 2 runs\n");
    expect(stdout).toContain("health: credentials: 0 of 2 runs\n");
    expect(stdout).toEndWith("health: 1 of 2 runs unhealthy\n");
  });

  // The architect's ruling of 2026-11-18: an environment health reads decides no verdict. The
  // same two archived runs, one whose answer turns on its label's daemon (a reviewer refused
  // not-current-phase with no earlier 200, which this checkout's daemon never answers) and one that
  // reads under the scratch root its namespace.txt covers, are judged under two TMPDIRs, from two
  // working directories, with an empty work directory and one whose profiles and installs name
  // another label's daemon (an older one, which answers that refusal) and another install.
  // Catches: health reading the daemon off the work directory's profile or the checkout as it
  // stands, the world off the judge's TMPDIR, or any path off its working directory.
  test.skipIf(withoutGo)(
    "one archived run reads the same under another TMPDIR, working directory and work directory",
    () => {
      const daemon = run(
        "reviewer-text-findings-text-only-head-14",
        [[complete(409, NOT_CURRENT, "reviewer")]],
        { checkout: CHECKOUT }
      );
      const world = worldRun("threads-closed-correct-head-38", {
        steps: [{ tool: "read", args: { path: "/recorded/scratch/another-role/notes.md" } }],
      }).dir;
      const populated = freshDir("populated-work");
      for (const label of ["head", "base"]) {
        mkdirSync(path.join(populated, "profiles", label), { recursive: true });
        writeFileSync(path.join(populated, "profiles", label, "checkout"), `${olderDaemon()}\n`);
        writeFileSync(path.join(populated, "profiles", label, "install"), "0123456789ab\n");
      }
      mkdirSync(path.join(populated, "installs", "0123456789ab"), { recursive: true });
      const judged = [
        healthArgs(
          { TMPDIR: freshDir("judge-tmp"), SKILL_SCENARIOS_WORK: freshDir("empty-work") },
          ["--repo", labelRepo(), daemon, world],
          freshDir("judge-cwd")
        ),
        healthArgs(
          { TMPDIR: freshDir("judge-tmp"), SKILL_SCENARIOS_WORK: populated },
          ["--repo", labelRepo(), daemon, world],
          populated
        ),
      ];
      expect(judged[1]).toEqual(judged[0] ?? {});
      expect(judged[0]?.code).toBe(1);
      expect([daemon, world].map((dir) => verdict(judged[0]?.stdout ?? "", dir))).toEqual([
        "reviewer-text-findings-text-only-head-14  unhealthy: refused completion: POST /legion/v1/handoff/complete of the reviewer answered 409 HANDOFF_NOT_CURRENT_PHASE, which the head label's daemon does not answer it with in this world",
        "threads-closed-correct-head-38  unhealthy: outside its world: a read call in sessions/2026-11-14T00-00-00-000Z_s1.jsonl names /recorded/scratch/another-role/notes.md, under /recorded/scratch, which the run's world hides and does not show again (rig.sh's world_paths)",
      ]);
    }
  );

  // Rule 21 over whole runs (health.ts's `credentialPrinted`, the architect's ruling of
  // 2026-11-18T11:34Z and 11:38Z): a run whose agent printed its GitHub token is unhealthy as an
  // agent printing its credential, named ahead of the refusal the world answered the call with —
  // rule 7's for both, `gh auth token` being no write as gh's source reads it (gh-writes.json)
  // and `gh auth status -t` a read the world refused — and a plain `gh auth status`, whose token
  // gh masks, is healthy. Catches: the rule left out of health's RULES, or read after rule 7, so
  // the run's line names the world's refusal and not what the agent did.
  test("a run whose agent printed its GitHub token is unhealthy for it, and gh auth status alone is healthy", () => {
    const bash = (command: string): Step => ({ tool: "bash", args: { command } });
    const status = "^gh auth status( |$)";
    const token = worldRun("threads-closed-correct-head-31", {
      steps: [bash("gh auth token")],
      calls: [{ ...gh("reviewer", ["auth", "token"], 1, null), handler: "unrouted" }],
    }).dir;
    const shown = worldRun("threads-closed-correct-head-32", {
      steps: [bash("gh auth status -t")],
      calls: [{ ...gh("reviewer", ["auth", "status", "-t"], 1, status), handler: "unrouted" }],
    }).dir;
    const plain = worldRun("threads-closed-correct-head-33", {
      steps: [bash("gh auth status")],
      calls: [{ ...gh("reviewer", ["auth", "status"], 0, status), handler: "auth-status" }],
    }).dir;
    const runs = [token, shown, plain];
    const { stdout } = health(...runs);
    const session = "sessions/2026-11-14T00-00-00-000Z_s1.jsonl";
    const printed = (call: string) =>
      `unhealthy: credential printed: a bash call in ${session} runs \`${call}\`, which prints the run's GitHub token`;
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      `threads-closed-correct-head-31  ${printed("gh auth token")}`,
      `threads-closed-correct-head-32  ${printed("gh auth status -t")}`,
      "threads-closed-correct-head-33  healthy",
    ]);
    expect(stdout).toContain("health: credential printed: 2 of 3 runs\n");
  });

  // The architect's ruling of 2026-11-18 on `gh send-telemetry`: gh itself starts it after a
  // command, a POST to GitHub's telemetry service the world refuses as a write it does not model,
  // so rules 7 and 20 read only a call whose first word stands after a `gh` an agent ran
  // (health.ts's `typedBy`), and any `gh api` an agent typed covers each of its api calls, whose
  // endpoint a shell can build. The tester's three rows. Catches: a rule that counts every record
  // of calls.jsonl, which read the first run unhealthy for a call no command of its agent made;
  // and one that credits a call by its whole argv, which read the third healthy since its agent's
  // text holds `"$ep"` and not the endpoint.
  test("rules 7 and 20 read only the gh calls an agent ran, a gh api covering its every api call", () => {
    const bash = (command: string): Step => ({ tool: "bash", args: { command } });
    const view = gh("reviewer", ["pr", "view", "1001"], 0, "^gh pr view( |$)");
    const telemetry = { ...gh("reviewer", ["send-telemetry"], 1, null), handler: "unrouted" };
    const put = ["api", "--method", "PUT", "repos/example/widgets/pulls/1001/reviews/5002"];
    const typedOnlyView = worldRun("threads-closed-correct-head-34", {
      steps: [bash("gh pr view 1001")],
      calls: [{ ...view, handler: "pull-request" }, telemetry],
    }).dir;
    const typedTelemetry = worldRun("threads-closed-correct-head-35", {
      steps: [bash("echo '{}' | gh send-telemetry")],
      calls: [telemetry],
    }).dir;
    const builtEndpoint = worldRun("threads-closed-correct-head-36", {
      steps: [bash('ep=repos/example/widgets/pulls/1001/reviews/5002; gh api --method PUT "$ep"')],
      calls: [{ ...gh("reviewer", put, 1, null), handler: "unrouted" }],
    }).dir;
    // A script the agent wrote and ran is its own, as rule 13 reads one.
    const ranScript = worldRun("threads-closed-correct-head-37", {
      steps: [
        { tool: "write", args: { path: "post.sh", content: "#!/bin/sh\ngh send-telemetry\n" } },
        bash("sh post.sh"),
      ],
      calls: [telemetry],
    }).dir;
    const runs = [typedOnlyView, typedTelemetry, builtEndpoint, ranScript];
    const { stdout } = health(...runs);
    const refused = (line: number, call: string) =>
      `unhealthy: unmodelled write: calls.jsonl:${line}, \`gh ${call}\` of the reviewer, exited 1: no route or answer of the world models it, so it was refused 404 where GitHub takes it`;
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      "threads-closed-correct-head-34  healthy",
      `threads-closed-correct-head-35  ${refused(2, "send-telemetry")}`,
      `threads-closed-correct-head-36  ${refused(2, put.join(" "))}`,
      `threads-closed-correct-head-37  ${refused(2, "send-telemetry")}`,
    ]);
  });

  // Catches: an eval cell that reaches a host off the run's world read as no reach at all. The
  // agents of these scenarios run eval cells (round 17's published through one), and a Python
  // `urllib.request.urlopen` or a JavaScript `fetch` of api.github.com reads the public pull
  // request's reviews as surely as a curl does. Measured on 2026-11-15 against
  // reviewer-text-findings-defect-base-1 of round 17 with one call planted: the bash curl read
  // unhealthy, and the same URL from a Python or a JavaScript eval cell read healthy. And an eval
  // cell that reaches only the loopback read as off its world.
  test("an eval cell that reaches a host off its world is off-world, in either language", () => {
    const evalCell = (language: string, code: string): Step => ({
      tool: "eval",
      args: { language, code },
    });
    const url = "https://api.github.com/repos/cybermonklabs/legion/pulls/2827/reviews";
    const python = worldRun("threads-closed-code-defect-head-6", {
      steps: [
        evalCell("py", `import urllib.request\nprint(urllib.request.urlopen("${url}").read())`),
      ],
    }).dir;
    const javascript = worldRun("threads-closed-code-defect-head-7", {
      steps: [evalCell("js", `const r = await fetch("${url}");\nconsole.log(await r.text());`)],
    }).dir;
    const loopback = worldRun("threads-closed-code-defect-head-8", {
      steps: [
        evalCell(
          "js",
          `console.log(await (await fetch("http://127.0.0.1:4100/v1/sessions")).text());`
        ),
      ],
    }).dir;
    const runs = [python, javascript, loopback];
    const { code, stdout } = healthIn({ TMPDIR: "/recorded/scratch" }, ...runs);
    expect(code).toBe(1);
    const [py, js, own] = runs.map((dir) => verdict(stdout, dir));
    expect(py).toMatch(
      /^threads-closed-code-defect-head-6 {2}unhealthy: off-world network: .*api\.github\.com/
    );
    expect(js).toMatch(
      /^threads-closed-code-defect-head-7 {2}unhealthy: off-world network: .*api\.github\.com/
    );
    expect(own).toBe("threads-closed-code-defect-head-8  healthy");
    expect(stdout).toContain("health: off-world network: 2 of 3 runs\n");
  });

  // The words in place of the process's number that the row of the caller's own shell (above the
  // eval cell's) does not hold, each in a run that shared the pod's processes (no namespace.txt),
  // as that row's runs did.
  // Catches: `$PPID` read as the caller's own shell, though it is that shell's parent; a braced
  // variable, a number, or a command substitution in either form (`$(…)`, `` `…` ``)
  // read as the caller's own, or not read as a process's number at all, as a substitution was
  // before 2026-11-16; the shell's own number with a digit after it (`$$0`) read as the shell's
  // own; a read of another process after one of the shell's own in the same call let through; and
  // `/proc/self` no longer read as the caller's own.
  test("a read of the shell's parent, a braced variable, a number or a substitution in place of the process is another process's; /proc/self is the caller's own", () => {
    const bash = (command: string): Step => ({ tool: "bash", args: { command } });
    const braced = ["/proc/$", "{ppid}/environ"].join("");
    const steps: Record<string, string> = {
      "threads-closed-code-defect-head-21":
        "tr '\\0' '\\n' < /proc/$$/environ | grep -c DISPATCH; tr '\\0' '\\n' < /proc/$PPID/environ",
      "threads-closed-code-defect-head-22": `ppid=$(ps -o ppid= $$); cat ${braced}`,
      "threads-closed-code-defect-head-23": "tr '\\0' ' ' < /proc/4242/cmdline",
      "threads-closed-code-defect-head-24": "tr '\\0' '\\n' < /proc/$(pgrep bun)/environ",
      "threads-closed-code-defect-head-25": "cat /proc/`pgrep -n bun`/cmdline",
      "threads-closed-code-defect-head-26": "cat /proc/$$0/environ",
      "threads-closed-code-defect-head-27":
        "tr '\\0' '\\n' < /proc/self/environ | grep -c DISPATCH; cat /proc/thread-self/cmdline",
    };
    const runs = Object.entries(steps).map(
      ([name, command]) => worldRun(name, { namespace: false, steps: [bash(command)] }).dir
    );
    const { stdout, stderr } = health(...runs);
    const read = (name: string, target: string) =>
      `${name}  unhealthy: another process: a bash call in sessions/2026-11-14T00-00-00-000Z_s1.jsonl reads ${target}, a process the run does not own`;
    // head-27, which rule 11 passes, predates the namespace: no rule that can judge it fails it.
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      read("threads-closed-code-defect-head-21", "/proc/$PPID/environ"),
      read("threads-closed-code-defect-head-22", braced),
      read("threads-closed-code-defect-head-23", "/proc/4242/cmdline"),
      read("threads-closed-code-defect-head-24", "/proc/$(pgrep bun)/environ"),
      read("threads-closed-code-defect-head-25", "/proc/`pgrep -n bun`/cmdline"),
      read("threads-closed-code-defect-head-26", "/proc/$$0/environ"),
      undefined,
    ]);
    expect(refusal(stderr, runs[6] ?? "")).toBe(unrecorded(runs[6] ?? ""));
    expect(stdout).toContain("health: another process: 6 of 6 runs\n");
  });

  // Catches: a host a bash command reaches with no URL's scheme read as no host at all: a git
  // remote in scp form, a curl operand that names a host bare, a `gh api --hostname` of a host the
  // run's gh does not serve, and a gh that is not the run's own stand-in. And a run whose curl
  // writes to a file with a dot in its name (`-o report.json`), whose git remote is a local path,
  // whose gh names github.com, which its stand-in plays, and whose bare gh is that stand-in read
  // as reaching any.
  test("a bash call that reaches a host with no scheme is off its world", () => {
    const bash = (command: string): Step => ({ tool: "bash", args: { command } });
    const scp = worldRun("threads-closed-correct-head-6", {
      steps: [bash("git clone git@github.com:cybermonklabs/legion.git src")],
    }).dir;
    const bare = worldRun("threads-closed-correct-head-7", {
      steps: [bash("curl -sS api.github.com/repos/cybermonklabs/legion")],
    }).dir;
    const hostname = worldRun("threads-closed-correct-head-8", {
      steps: [bash("gh api --hostname ghe.example.com repos/cybermonklabs/legion/pulls/2827")],
    }).dir;
    const joined = worldRun("threads-closed-code-defect-head-6", {
      steps: [bash("gh api --hostname=github.com repos/example/widgets/pulls/1001")],
    }).dir;
    const real = worldRun("threads-closed-code-defect-head-7", {
      steps: [bash("/usr/bin/gh pr view 1001")],
    });
    const local = worldRun("threads-closed-correct-head-9", {
      steps: [
        bash("curl -sS -o report.json http://127.0.0.1:4101/api/v1/whoami"),
        bash("git -C ws remote add origin ../remote.git && git -C ws fetch origin"),
        bash("gh pr view 1001"),
      ],
    }).dir;
    const runs = [scp, bare, hostname, joined, real.dir, local];
    const { stdout } = health(...runs);
    const session = "sessions/2026-11-14T00-00-00-000Z_s1.jsonl";
    const off = (named: string, host: string) =>
      `off-world network: a bash call in ${session} reaches ${host} (${named}), no host of the run's world (127.0.0.1, localhost, ::1)`;
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      `threads-closed-correct-head-6  unhealthy: ${off("git@github.com:cybermonklabs/legion.git", "github.com")}`,
      `threads-closed-correct-head-7  unhealthy: ${off("api.github.com/repos/cybermonklabs/legion", "api.github.com")}`,
      `threads-closed-correct-head-8  unhealthy: off-world network: a bash call in ${session} reaches ghe.example.com (ghe.example.com), no host the run's gh serves (github.com, 127.0.0.1, localhost, ::1)`,
      "threads-closed-code-defect-head-6  healthy",
      `threads-closed-code-defect-head-7  unhealthy: off-world network: a bash call in ${session} reaches github.com (/usr/bin/gh), a gh that is not the run's own stand-in (${real.dir}/bin/gh, ${real.ran}/bin/gh), and no world of this rig serves another`,
      "threads-closed-correct-head-9  healthy",
    ]);
  });

  // Catches: a link an agent writes into a comment, a review reply or a commit message read as a
  // host it reached (a reviewer's reply names the pull request it answers), and that exclusion
  // read so wide that a curl in the same command line as such a text goes unread.
  test("a URL in text a command writes is no reach, and a curl beside it still is", () => {
    const bash = (command: string): Step => ({ tool: "bash", args: { command } });
    const link = "https://github.com/cybermonklabs/legion/pull/2827";
    const comment = worldRun("threads-closed-correct-head-6", {
      steps: [
        bash(`gh pr comment 1001 --body 'see ${link}'`),
        bash(`git commit -m "Answer ${link}" && gh pr create --title "${link}" --body="${link}"`),
      ],
    }).dir;
    const field = worldRun("threads-closed-correct-head-7", {
      steps: [
        bash(
          `gh api -f body='${link}' -F title="${link}" repos/example/widgets/pulls/1001/comments`
        ),
      ],
    }).dir;
    const beside = worldRun("threads-closed-correct-head-8", {
      steps: [
        bash(
          `gh pr comment 1001 --body 'see ${link}' && curl -sS https://github.com/cybermonklabs/legion`
        ),
      ],
    }).dir;
    const runs = [comment, field, beside];
    const { stdout } = health(...runs);
    expect(runs.map((dir) => verdict(stdout, dir))).toEqual([
      "threads-closed-correct-head-6  healthy",
      "threads-closed-correct-head-7  healthy",
      "threads-closed-correct-head-8  unhealthy: off-world network: a bash call in sessions/2026-11-14T00-00-00-000Z_s1.jsonl reaches github.com (https://github.com/cybermonklabs/legion), no host of the run's world (127.0.0.1, localhost, ::1)",
    ]);
  });

  // Catches: a credential path named by the name /var/run resolves to (/run on a host whose
  // /var/run is a link to it, as a Legion pod's and a GitHub runner's is) read as no credential.
  test.skipIf(realpathSync("/var/run") === "/var/run")(
    "a credential path named by its resolved name is unhealthy",
    () => {
      const run = realpathSync("/var/run");
      const dir = worldRun("threads-closed-code-defect-head-7", {
        steps: [{ tool: "read", args: { path: `${run}/legion/private/token` } }],
      }).dir;
      const { stdout } = health(dir);
      expect(verdict(stdout, dir)).toBe(
        `threads-closed-code-defect-head-7  unhealthy: credentials: a read call in sessions/2026-11-14T00-00-00-000Z_s1.jsonl names ${run}/legion/private/token, under ${run}/legion`
      );
    }
  );

  // Catches: a claim boot refusal read in one label's wording alone. main ff2a10ef's plugin
  // words it `claim boot failed after claims/register registered <token>; …`, with no step
  // (packages/pi-shared/src/legion-claim-core.ts:97 there), and this checkout's `claim boot failed
  // at <step> after …`: read for this checkout's opening only, a base agent refused at its boot
  // was never relaunched (`health.ts --boot-refusal`) and its run read as one whose claim boot met
  // nothing. Each run is read for the wording of the checkout its built-from names, and a head
  // sentence in a base run is none its plugin prints: that run is read on past the claim boot, to
  // the reviewer's completion it never made, which a defect run's world must take (rule 3, read by
  // the run's case).
  test("a claim boot sentence is read in the wording of the run's own label's plugin", () => {
    const baseCheckout = freshDir("base-claim-checkout");
    const core = path.join(baseCheckout, "packages", "pi-shared", "src", "legion-claim-core.ts");
    mkdirSync(path.dirname(core), { recursive: true });
    writeFileSync(
      core,
      // biome-ignore lint/suspicious/noTemplateCurlyInString: the base module's source, verbatim.
      "    host.log(\n      `claim boot failed after claims/register registered ${claim.claimToken}; ${host.stopping}: ${messageFor(error)}`\n    );\n"
    );
    const baseLine =
      "claim boot failed after claims/register registered legion-skills1-lweval-1-reviewer; exiting so the daemon launches it again: ECONNREFUSED";
    const headLine =
      "claim boot failed at claimRole after claims/register registered legion-skills1-lweval-1-reviewer with the daemon at http://127.0.0.1:4321; exiting so the daemon launches it again: ECONNREFUSED";
    const refused = (name: string, checkout: string, line: string) => {
      const dir = run(name, [[]], { checkout });
      writeFileSync(path.join(dir, "out.txt"), `[legion] ${line}\nexit=1\n`);
      return dir;
    };
    const base = refused("reviewer-text-findings-text-only-base-9", baseCheckout, baseLine);
    const head = refused("reviewer-text-findings-text-only-head-9", CHECKOUT, headLine);
    const crossed = refused("reviewer-text-findings-defect-base-10", baseCheckout, headLine);
    const never =
      "and the agent was not relaunched: the rig relaunches an agent once when its claim boot is refused before its first turn, and never after it";
    const { stdout } = health(base, head, crossed);
    expect(verdict(stdout, base)).toBe(
      `reviewer-text-findings-text-only-base-9  unhealthy: claim boot refused: ${baseLine} (<run>/out.txt), ${never}`
    );
    expect(verdict(stdout, head)).toBe(
      `reviewer-text-findings-text-only-head-9  unhealthy: claim boot refused: ${headLine} (<run>/out.txt), ${never}`
    );
    expect(verdict(stdout, crossed)).toStartWith(
      "reviewer-text-findings-defect-base-10  unhealthy: no completion taken: "
    );
    // The relaunch rig.sh's boot_agent asks for, of the base agent refused before its first turn.
    const asked = spawnChecked(
      [
        "bun",
        path.join(import.meta.dir, "health.ts"),
        "--boot-refusal",
        base,
        "reviewer",
        "2026-11-13T00:00:00.000Z",
        path.join(base, "sessions"),
      ],
      { env: childEnv, stdout: "pipe", stderr: "pipe" }
    );
    expect({ code: asked.exitCode, stdout: asked.stdout.toString() }).toEqual({
      code: 0,
      stdout: `${baseLine}\n`,
    });
  });

  // Catches: a base run whose Envoy listener the head checkout built read as healthy, the
  // pairing that put main ff2a10ef's pi-envoy beside a listener from after #2603 and lost the
  // base runs' claim boot to its `409 not_requested`; and a run whose every service of its
  // label's own came from that label's checkout read as unhealthy, on either label.
  test("a base run whose listener the head checkout built is unhealthy, and a run whose services of its own are all its label's, healthy", () => {
    const base = "/recorded/base-checkout";
    const head = "/recorded/head-checkout";
    const foreign = run("threads-closed-code-defect-base-9", [[]], {
      checkout: base,
      provenance: servicesFrom(base).replace(
        `envoy-listener label ${base}`,
        `envoy-listener label ${head}`
      ),
    });
    const own = run("threads-closed-code-defect-base-10", [[]], { checkout: base });
    const ownHead = run("threads-closed-code-defect-head-14", [[]], { checkout: head });
    const { code, stdout } = health(foreign, own, ownHead);
    expect(code).toBe(1);
    expect(verdict(stdout, foreign)).toBe(
      `threads-closed-code-defect-base-9  unhealthy: foreign service: the run's envoy-listener came from ${head}, not from ${base}, the checkout of the base label its built-from names`
    );
    expect(verdict(stdout, own)).toBe("threads-closed-code-defect-base-10  healthy");
    expect(verdict(stdout, ownHead)).toBe("threads-closed-code-defect-head-14  healthy");
    expect(stdout).toContain("health: service provenance: 1 of 3 runs\n");
  });

  // Catches: a run that carries no record of what its services came from read as healthy, and
  // one whose record names a service of its label's own as no label's (the listener, here, as a
  // shared one) or whose built-from names no checkout to hold the record to.
  test("a run with no service provenance, one naming a service of its label's own as shared, or one with no checkout to hold it to, is unhealthy", () => {
    const missing = run(
      "reviewer-text-findings-defect-head-4",
      [[complete(200, undefined, "reviewer")]],
      { provenance: false }
    );
    const shared = run("threads-closed-correct-head-9", [[]], {
      provenance: servicesFrom(RUN_CHECKOUT).replace(
        `envoy-listener label ${RUN_CHECKOUT}`,
        `envoy-listener shared ${RUN_CHECKOUT}`
      ),
    });
    const unnamed = run("threads-closed-correct-head-10", [[]]);
    writeFileSync(path.join(unnamed, "built-from"), "skills 00\n");
    const { code, stdout } = health(missing, shared, unnamed);
    expect(code).toBe(1);
    expect(verdict(stdout, missing)).toBe(
      "reviewer-text-findings-defect-head-4  unhealthy: no service provenance: the run has no <run>/services-from, so which checkout its Envoy listener and its other services came from is unknown"
    );
    expect(verdict(stdout, shared)).toBe(
      `threads-closed-correct-head-9  unhealthy: service provenance: <run>/services-from names no envoy-listener of the head label's own checkout (${RUN_CHECKOUT})`
    );
    expect(verdict(stdout, unnamed)).toContain(
      "threads-closed-correct-head-10  unhealthy: service provenance unknown: "
    );
    expect(verdict(stdout, unnamed)).toContain("names no checkout");
  });

  // Catches: a batch that cannot tell how often each label's agents were relaunched at their
  // boot, where a label whose world refused its plugins at every boot relaunches every agent and
  // reads healthy on each relaunch, and a count that merges the labels or leaves one out.
  test("health prints how often each label's agents were relaunched at their boot", () => {
    const relaunched = (name: string, roles: string[]) => {
      const dir = run(name, [[]]);
      for (const role of roles)
        mkdirSync(path.join(dir, "boot-1", role, "sessions"), { recursive: true });
      return dir;
    };
    const { stdout } = health(
      relaunched("threads-closed-correct-base-11", ["implementer", "reviewer"]),
      relaunched("threads-closed-code-defect-base-11", ["implementer"]),
      relaunched("threads-closed-code-defect-base-12", []),
      relaunched("threads-closed-code-defect-head-15", [])
    );
    expect(stdout.split("\n").filter((line) => line.startsWith("health: boot relaunches"))).toEqual(
      [
        "health: boot relaunches, base: 3 in 2 of 3 runs",
        "health: boot relaunches, head: 0 in 0 of 1 runs",
      ]
    );
  });

  // Kept from the tester's fixture of 2026-11-14 (health-fixture/, retired by the architect's
  // ruling of 2026-11-18): its completion lines as the stand-in wrote them then, a field order of
  // their own (threads-closed-correct-base-1's implementer refusal, -head-2's reviewer
  // refusal), and a daemon.log line that is no JSON (threads-closed-code-defect-base-1's). Its
  // role-less 409 and its head reviewer's late refusal after its own 200 and before it are the
  // rows "a completion line naming no role…" and "a head reviewer refused not-current-phase…"
  // above. The reviewer's refusal turns on the label's daemon, so its run is built from this
  // checkout, whose daemon refuses it. Catches: a completion line read by the order of its
  // fields, and a daemon.log that does not parse read as a run with no completion.
  test.skipIf(withoutGo)(
    "a completion line is read by its fields in any order, and a daemon.log line that is no JSON is an unreadable record",
    () => {
      const implementer = run("threads-closed-correct-base-1", [
        [
          {
            code: NOT_CURRENT,
            at: "2026-11-14T00:00:00Z",
            path: "/legion/v1/handoff/complete",
            role: "implementer",
            status: 409,
          },
        ],
        [],
      ]);
      const reviewer = run(
        "threads-closed-correct-head-2",
        [
          [],
          [
            {
              phase: "reviewing",
              code: "HANDOFF_RECORD_MISMATCH",
              at: "2026-11-14T00:00:00Z",
              role: "reviewer",
              path: "/legion/v1/handoff/complete",
              status: 409,
            },
          ],
        ],
        { checkout: CHECKOUT }
      );
      const unparsed = run("threads-closed-code-defect-base-1", [[], []]);
      appendFileSync(path.join(unparsed, "daemon.log"), "not json at all\n");
      const { code, stdout } = health(implementer, reviewer, unparsed);
      expect(code).toBe(1);
      expect([implementer, reviewer, unparsed].map((dir) => verdict(stdout, dir))).toEqual([
        "threads-closed-correct-base-1  healthy",
        "threads-closed-correct-head-2  unhealthy: refused completion: POST /legion/v1/handoff/complete of the reviewer answered 409 HANDOFF_RECORD_MISMATCH, which the head label's daemon does not answer it with in this world",
        'threads-closed-code-defect-base-1  unhealthy: unreadable record: JSON Parse error: Unexpected identifier "not"',
      ]);
    }
  );

  // Kept from the tester's fixture of 2026-11-14 (threads-closed-code-defect-head-5): another
  // run's path named only in a subagent's transcript, two levels under the run's sessions, a
  // later half's subagent's. Catches: rule 8 reading the agents' own transcripts alone.
  test("another run's path named only in a subagent's transcript is another run", () => {
    const named = "/recorded/work/runs/threads-closed-code-defect-head-1/state/home/x";
    const { dir } = worldRun("threads-closed-code-defect-head-5", {
      others: { "reviewer/sub": [{ tool: "bash", args: { command: `cat ${named}` } }] },
    });
    const { code, stdout } = health(dir);
    expect(code).toBe(1);
    expect(verdict(stdout, dir)).toBe(
      `threads-closed-code-defect-head-5  unhealthy: another run: a bash call in sessions/reviewer/sub/${TRANSCRIPT} names ${named}, under /recorded/work/runs/threads-closed-code-defect-head-1`
    );
  });
});

// Catches: a world with no `gh pr list` route, so a role with no pull request number reads
// `gh: Not Found (HTTP 404)` where the worker skill has it find the one open pull request of its
// branch (`gh pr list --repo <repo> --head legion/<KEY> --state open --json number`), which reads
// as a branch with no pull request open on it; and a world that answers neither of the two
// base-branch reads the merge gate prescribes, where GitHub answers both and the rule has a
// refused read reported rather than guessed around (91 of them in the batch of 2026-11-10).
describe("fixture_write", () => {
  /** The world fixture_write writes under a directory of its own, open against BASE, and
   * `gh <args…>` answered out of it by the stand-in. Every world of this rig is on one base,
   * which fixture_write writes into each of its answers from `$worker_base`, so a row that
   * builds another base reads whether those answers are the world's own. */
  function world(base = "main") {
    const dir = freshDir("fixture-routes");
    const result = bash(
      [
        fn("fixture_write"),
        fn("concluded_checks"),
        fn("base_ruleset_routes"),
        fn("role_identity"),
        `R=$1 worker_key=LWEVAL-1 worker_repo=example/widgets worker_base=${base}`,
        assignment("world_login"),
        ...WORLD_APPS.map(assignment),
        "C1=0123456789abcdef0123 body=a-body worker_pr=1001",
        "worker_pr_url=https://example.invalid/pull/1001",
        "fixture_write '[]' '[]'",
      ].join("\n"),
      [dir]
    );
    expect(result).toMatchObject({ code: 0, stderr: "" });
    writeGhSelectors(dir);
    return (...args: string[]) =>
      spawnChecked(["bun", GH_STANDIN, "gh", ...args], {
        env: { ...childEnv, GH_DATA_DIR: dir },
      });
  }

  // The two reads of skill://legion-worker/references/merge-gate.md, each with the jq that rule
  // writes: every base requires the checks the head reports (`test`, `lint`), which its one
  // ruleset names, and it has no classic protection, so the branch's protection names none. A 404
  // at either is a refused read, which the rule has the reviewer report to the architect and
  // never guess around. Catches a base whose classic protection names the checks and whose
  // rulesets name none, which gave a reviewer that reads the rulesets alone no required check.
  test("the world answers the base branch's rulesets and its protection, as GitHub does", () => {
    const gh = world();
    const rules = gh(
      "api",
      "--paginate",
      "repos/example/widgets/rules/branches/main",
      "--jq",
      '.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context'
    );
    expect([rules.exitCode, rules.stdout.toString()]).toEqual([0, "test\nlint\n"]);
    const branch = gh(
      "api",
      "repos/example/widgets/branches/main",
      "--jq",
      ".protection.required_status_checks.checks[].context"
    );
    expect([branch.exitCode, branch.stdout.toString()]).toEqual([0, ""]);
    // And the branch read answers the branch itself, as GitHub answers one a ruleset alone
    // protects: protected, with the classic protection off.
    const read = gh("api", "repos/example/widgets/branches/main");
    expect(JSON.parse(read.stdout.toString())).toMatchObject({
      name: "main",
      protected: true,
      protection: {
        enabled: false,
        required_status_checks: { enforcement_level: "off", contexts: [], checks: [] },
      },
    });
  });

  // Catches: the protection endpoint answered from the branch route, whose regex matched that
  // path too, and one answered as a token that may read a repository's administration is.
  // `…/branches/main/protection` is GitHub's own read of the classic protection, which GitHub
  // refuses a role App 403 (production-shapes.json's `branches-main-protection`); the branch
  // object there tells a reviewer this base's protection in a body that answers another call.
  test("the world answers the base branch's protection endpoint with GitHub's 403 to a role App", () => {
    const read = world()("api", "repos/example/widgets/branches/main/protection");
    expect([read.exitCode, read.stderr.toString()]).toEqual([
      1,
      "gh: Resource not accessible by integration (HTTP 403)\n",
    ]);
    expect(JSON.parse(read.stdout.toString())).toEqual({
      message: "Resource not accessible by integration",
      documentation_url:
        "https://docs.github.com/rest/branches/branch-protection#get-branch-protection",
      status: "403",
    });
  });

  test("the world answers a listing of the pull requests open on its branch", () => {
    const listed = world()(
      "pr",
      "list",
      "--repo",
      "example/widgets",
      "--head",
      "legion/LWEVAL-1",
      "--state",
      "open",
      "--json",
      "number",
      "--jq",
      ".[0].number"
    );
    expect(listed.stdout.toString().trim()).toBe("1001");
    expect(listed.exitCode).toBe(0);
  });

  // Catches: the pull request's base written here as the word every world of this rig happens
  // to carry rather than as the world's own `$worker_base`. The base is the other side of the
  // diff the stand-in serves from the run's own remote, the branch the merge gate's two reads
  // are of, and the default branch the pane's addressing names (worker-pane.ts), so a world
  // built on another base and answering `main` tells each of those a branch this pull request
  // is not open against. This world is built on `trunk`, and every answer that names the base
  // names it.
  test("the pull request is open against the world's own base branch, not a word of this rig's", () => {
    const gh = world("trunk");
    const view = gh("pr", "view", "1001", "--json", "baseRefName", "--jq", ".baseRefName");
    expect([view.exitCode, view.stdout.toString().trim()]).toEqual([0, "trunk"]);
    const api = gh("api", "repos/example/widgets/pulls/1001", "--jq", ".base.ref");
    expect([api.exitCode, api.stdout.toString().trim()]).toEqual([0, "trunk"]);
    const branch = gh("api", "repos/example/widgets/branches/trunk", "--jq", ".name");
    expect([branch.exitCode, branch.stdout.toString().trim()]).toEqual([0, "trunk"]);
  });
});

// A world is its routes in order: the gh stand-in answers the first one whose regex matches
// "gh <argv…>" (gh-standin.ts), so a route moved above another changes what every agent of the
// scenario is told with nothing else in the rig changing, and no row of this file read that
// order before. Catches: the `resolveReviewThread` mutation answered by the `reviewThreads`
// query's route, which would tell an implementer its resolution listed the threads; a review
// submission answered by the route that lists the reviews already on the pull request, which
// would hand a reviewer the fixture's own approval as the review it just created; a REST thread
// reply answered by fixture_write's catch-all POST, whose id is no comment's; and a world whose
// `reviewThreads` serves fixture_write's empty list in place of the threads the approval left.
describe("the worlds' routes, in the order the stand-in matches them", () => {
  /** WORLD built for real under a run directory of its own, with two steps stubbed out:
   * `fixture_workspace`, which makes the issue workspace with jj and which no route of any
   * world reads, and `fixture_cli`, whose heredoc holds a line that is `}` alone, which
   * `scriptFunctions` reads as the end of the function. The stub writes the two files that step
   * writes, since the diff of the world's own remote is one of the answers below, and no route
   * reads what is in them. */
  function built(world: string, guard = ""): string {
    const dir = freshDir(`world-routes-${world}`);
    const result = bash(
      [
        fn("fail"),
        ...[
          "fixture_open",
          "fixture_git",
          "role_identity",
          "role_env",
          "fixture_docs_before",
          "fixture_first_commit",
          "fixture_round_files",
          "fixture_head_commit",
          "fixture_implement",
          "fixture_test_handoff",
          "fixture_checks",
          "fixture_body",
          "fixture_write",
          "concluded_checks",
          "base_ruleset_routes",
          "resolution_protection_routes",
          "round_world",
          "fixture_reviewer_commit",
          world,
        ].map(fn),
        'fixture_cli() { printf "export const greet = 1;\\n" >"$src/greet.ts"; : >"$src/greet.test.ts"; }',
        "fixture_workspace() { :; }",
        "R=$1 worker_key=LWEVAL-1 worker_repo=example/widgets worker_base=main",
        assignment("world_author_name"),
        assignment("world_author_email"),
        assignment("world_login"),
        ...WORLD_APPS.map(assignment),
        "text_finding_doc_line=THE-DOC-LINE text_finding_trigger=THE-TRIGGER",
        `${world} 1 100 "$2" "$3"`,
      ].join("\n"),
      [dir, guard, guard === "" ? "correct" : "code-defect"]
    );
    expect(result).toMatchObject({ code: 0, stderr: "" });
    writeGhSelectors(dir);
    return dir;
  }
  /** What the gh stand-in answers for `gh <args…>` in the world under `dir`: its status, its
   * stdout and its stderr, which is where a refusal's own text goes. */
  function call(dir: string, ...args: string[]) {
    const made = spawnChecked(["bun", GH_STANDIN, "gh", ...args], {
      env: { ...childEnv, GH_DATA_DIR: dir },
    });
    return {
      code: made.exitCode,
      out: made.stdout.toString().trim(),
      err: made.stderr.toString(),
    };
  }
  /** The same answer without its stderr, for the rows that read what the world served. */
  function answer(dir: string, ...args: string[]) {
    const { code, out } = call(dir, ...args);
    return { code, out };
  }
  /** A thread resolved as review-threads.md resolves one, reading the mutation's own answer
   * back: the call every world's `resolveReviewThread` route answers. */
  const resolve = (thread: string) => [
    "api",
    "graphql",
    "-f",
    `query=mutation { resolveReviewThread(input: {threadId: "${thread}"}) { thread { isResolved } } }`,
    "--jq",
    ".data.resolveReviewThread.thread.isResolved",
  ];
  /** The resolve of the one thread the approved world's approval left, on the workflow file: the
   * world of correct, the case `built` builds it in (rig.sh's approved_world). */
  const RESOLVE = resolve("PRRT_kwDOfix002");
  /** The pull request's review threads listed as review-threads.md lists them, reading `jq` out
   * of the answer: the call the world's `reviewThreads` route answers. */
  const threadQuery = (jq: string) => [
    "api",
    "graphql",
    "-f",
    "query=query { repository { pullRequest { reviewThreads(first: 50) { nodes { id path } } } } }",
    "--jq",
    jq,
  ];
  const IDS = '[.data.repository.pullRequest.reviewThreads.nodes[].id] | join(",")';

  test("the approved world resolves, lists, replies and creates a review, each by its own route", () => {
    const dir = built("approved_world");
    expect(answer(dir, ...RESOLVE)).toEqual({ code: 0, out: "true" });
    expect(answer(dir, ...threadQuery(IDS))).toEqual({ code: 0, out: "PRRT_kwDOfix002" });
    const reply = [
      "api",
      "--method",
      "POST",
      // The first comment of the reviewer's one thread (rig.sh's fixture_write numbers the
      // comments of the world's own threads from 7001), on the `COMMENT` review 5001.
      "repos/example/widgets/pulls/1001/comments/7001/replies",
      "-f",
      "body=fixed",
    ];
    // The first comment the run makes.
    expect(answer(dir, ...reply, "--jq", ".id")).toEqual({ code: 0, out: "9001" });
    // The submission's route takes the verdict's `--method POST` on either side of the path, and
    // the read of the reviews already there takes neither: one route for the POST, one for the
    // GET, and the POST's first. Each create carries its verdict, so neither is a pending review,
    // which GitHub would refuse a second of while the first stands; and each is a review of its
    // own, the route's id the first's and the next the second's (gh-threads.ts's
    // `createdReviewId`).
    for (const [argv, id] of [
      [["api", "--method", "POST", "repos/example/widgets/pulls/1001/reviews"], "5002"],
      [["api", "repos/example/widgets/pulls/1001/reviews", "--method", "POST"], "5003"],
    ] as const)
      expect(answer(dir, ...argv, "-f", "event=APPROVE", "--jq", ".id")).toEqual({
        code: 0,
        out: id,
      });
    // The reviews list holds the reviewer's `COMMENT` review first: correct's reviewer has
    // approved nothing (rig.sh's approved_world).
    expect(
      answer(dir, "api", "repos/example/widgets/pulls/1001/reviews", "--jq", ".[0].state")
    ).toEqual({ code: 0, out: "COMMENTED" });
    // No world routes `gh pr diff`: it is the diff of the run's own remote at the call, which is
    // the change the implementer's commit carries over the branch the pull request is open
    // against.
    expect(answer(dir, "pr", "diff", "1001", "--name-only")).toEqual({
      code: 0,
      out: [".github/workflows/greet.yml", "docs/cli.md", "greet.test.ts", "greet.ts"].join("\n"),
    });
  });

  test("the review round's world resolves a thread and lists the bot's, each by its own route", () => {
    const dir = built("review_world");
    // A thread the review round's world has not got is GitHub's NOT_FOUND, and its own the bot's.
    expect(answer(dir, ...RESOLVE)).toEqual({ code: 1, out: "null" });
    expect(answer(dir, ...resolve("PRRT_kwDOrig0001"))).toEqual({ code: 0, out: "true" });
    expect(answer(dir, ...threadQuery(IDS))).toEqual({ code: 0, out: "PRRT_kwDOrig0001" });
  });

  // Catches: a world GitHub could not produce, and one that leaves the reviewing path
  // unexercised. Main's engine keeps the record in `reviewing` until GitHub reports the approved
  // head `CLEAN`, `UNSTABLE` or `HAS_HOOKS` (review 5887659447, finding 3); a head `BLOCKED` with
  // every check green and an approval standing is one whose base requires review threads
  // resolved while a thread is open, so a world serving that state must answer that rule where
  // GitHub states it for a base a ruleset alone protects — the rules in force on the branch and
  // the ruleset itself — while the branch's classic protection is off and its endpoint refuses a
  // role App 403. Every base requires the checks the head reports, `test` and `lint`, which the
  // rulesets read prints and the branch's protection does not, and its code-scanning tool's
  // `CodeQL`, which the head reports too. Every head's checks are concluded: `BLOCKED` comes from
  // the threads, or, in the review round's worlds, from the approval its base requires and no
  // review has given yet (INFRA-764's ruling of 2026-11-18), never from CI.
  test("a BLOCKED head's base requires conversation resolution, every check green, and a CLEAN head's base requires only its checks", () => {
    for (const [world, guard, blocked] of [
      ["review_world", "", true],
      ["review_world", "dropped", true],
      ["approved_world", "", true],
      ["approved_world", "dropped", false],
    ] as const) {
      const dir = built(world, guard);
      const head = headOf(dir);
      const states = '[.check_runs[] | .name + "=" + .status] | join(",")';
      const checks = call(dir, "pr", "checks", "1001");
      expect([
        world,
        guard,
        answer(dir, "api", `repos/example/widgets/commits/${head}/check-runs`, "--jq", states),
        checks.code,
        checks.out.split("\n").map((line) => line.split("\t")[1]),
        answer(
          dir,
          "pr",
          "view",
          "1001",
          "--json",
          "mergeStateStatus",
          "--jq",
          ".mergeStateStatus"
        ),
        answer(dir, "api", "repos/example/widgets/pulls/1001", "--jq", ".mergeable_state"),
      ]).toEqual([
        world,
        guard,
        { code: 0, out: "test=completed,lint=completed,CodeQL=completed" },
        0,
        ["pass", "pass", "pass"],
        { code: 0, out: blocked ? "BLOCKED" : "CLEAN" },
        { code: 0, out: blocked ? "blocked" : "clean" },
      ]);
      // The rule, at each read of the base, and the merge gate's own reads of its required
      // checks (skill://legion-worker/references/merge-gate.md), which name the ruleset's.
      const base = "repos/example/widgets/branches/main";
      const rules = "repos/example/widgets/rules/branches/main";
      const resolution =
        '[.[] | select(.type=="pull_request") | .parameters.required_review_thread_resolution] | any';
      expect([
        world,
        guard,
        answer(dir, "api", base, "--jq", "[.protected, .protection.enabled] | @json"),
        answer(dir, "api", `${base}/protection`, "--jq", ".required_status_checks"),
        answer(dir, "api", rules, "--jq", resolution),
        answer(dir, "api", "repos/example/widgets/rulesets/1", "--jq", `.rules | ${resolution}`),
        answer(dir, "api", base, "--jq", ".protection.required_status_checks.checks[].context"),
        answer(
          dir,
          "api",
          rules,
          "--jq",
          '.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context'
        ),
      ]).toEqual([
        world,
        guard,
        { code: 0, out: "[true,false]" },
        {
          code: 1,
          out: JSON.stringify({
            message: "Resource not accessible by integration",
            documentation_url:
              "https://docs.github.com/rest/branches/branch-protection#get-branch-protection",
            status: "403",
          }),
        },
        { code: 0, out: String(blocked) },
        { code: 0, out: String(blocked) },
        { code: 0, out: "" },
        { code: 0, out: "test\nlint" },
      ]);
    }
  });

  // Catches: a world that carries a behavioural defect of its own, which is what the
  // reviewer-text-findings scenario states it does not. Measured on 2026-11-12: with no
  // `oven-sh/setup-bun` step, `bun` is absent on `ubuntu-latest` and the job can never pass, so
  // a run that refused the change for it was right about the world and the arm scored it a
  // failure. The comment above the trigger is the text finding the scenario is built on, so the
  // row holds both: the install is there, and the comment still names a trigger `on:` does not.
  test("the round's workflow installs bun and keeps the comment that names a trigger it has not", () => {
    const dir = built("approved_world");
    const workflow = readFileSync(
      path.join(dir, "src", ".github", "workflows", "greet.yml"),
      "utf8"
    );
    expect(workflow).toContain("uses: oven-sh/setup-bun@");
    expect(workflow).toContain("run: bun test\n");
    expect(workflow).toContain("THE-TRIGGER");
    expect(workflow.indexOf("uses: oven-sh/setup-bun@")).toBeLessThan(
      workflow.indexOf("run: bun test\n")
    );
  });

  /** The route GitHub submits one review of this world's pull request on, as `gh api` takes it:
   * without the leading slash, which `submit` writes, and with it, which a route copied from
   * GitHub's own documentation carries. */
  const submitRoute = (id: number) => `repos/example/widgets/pulls/1001/reviews/${id}/events`;
  /** The create of a review on the world's pull request whose body names no `event`, which is
   * what leaves a pending one (gh-standin.ts's `pendingReview`), at `commit_id`. The id the
   * world answers it with is the id that review is then submitted under. */
  function createPending(dir: string, commit: string) {
    const created = call(
      dir,
      "api",
      "--method",
      "POST",
      "repos/example/widgets/pulls/1001/reviews",
      "-f",
      `commit_id=${commit}`,
      "-f",
      "body=two findings"
    );
    expect([created.code, created.err]).toEqual([0, ""]);
  }
  /** The submit of a review on the world's pull request, with its status, its answer and its
   * stderr: the call a round makes on `…/reviews/<id>/events`. */
  function submit(dir: string, id: number, event: string) {
    return call(dir, "api", "--method", "POST", submitRoute(id), "-f", `event=${event}`);
  }
  /** The world's own head, which is what a create naming no commit is pending at and what each
   * row below creates its review at. */
  function headOf(dir: string): string {
    const read = JSON.parse(readFileSync(path.join(dir, "world.json"), "utf8")) as { head: string };
    return read.head;
  }

  // The submit endpoint is routed by no world and answered by the stand-in for all of them
  // (gh-standin.ts's reviewSubmission), so each world refuses the submit of a review no run of
  // its own left pending, which is every review these worlds carry. Catches: that refusal
  // written into one world, which is how review_world — the world both reviewer-text-findings
  // cases boot — came to answer the call with fixture_write's generic POST route, `{id:6001}`
  // and exit 0: a reviewer that approved wrongly and then sent REQUEST_CHANGES there had that
  // change request read as the verdict standing, and the defect control passed a round that
  // approved. Each world is asked with the id its own create answers (rig.sh), since the
  // refusal is of the review and not of an id this file picked, and with each verdict a round
  // submits, since an approval answered here is the same invention as a change request.
  for (const [world, id] of [
    ["review_world", 6001],
    ["approved_world", 5002],
  ] as const)
    test(`${world} refuses the submit of a review that is not pending`, () => {
      const dir = built(world);
      for (const event of ["APPROVE", "REQUEST_CHANGES"]) {
        const refused = submit(dir, id, event);
        expect(refused.code).toBe(1);
        expect(refused.err).toContain("(HTTP 422)");
        expect(refused.err).toContain("Can not submit a review that is not pending.");
        expect(refused.out).toBe("");
      }
    });

  // The four rows below ask of a world rig.sh builds what gh-standin.test.ts asks of a run
  // directory it writes itself, and neither is a copy of the other: these calls go through the
  // world's own routes, in the order the stand-in matches them, which is what the submit
  // endpoint's refusal survived every suite for — a world routed that call and answered it
  // ahead of the stand-in. The rows there vary the stand-in's own state cheaply, where a world
  // of this rig has one shape.
  // The one review a run left pending is the one this endpoint answers, and GitHub answers
  // every other id 422. Catches: a stand-in that answers whatever id a submit names once the
  // run has a pending review, which would hand a round that submitted the wrong review an
  // approval of the head the right one was created at.
  test("a submit answers the review this run left pending, and refuses every other id", () => {
    const dir = built("review_world");
    const head = headOf(dir);
    createPending(dir, head);
    const other = submit(dir, 5003, "APPROVE");
    expect(other.code).toBe(1);
    expect(other.err).toContain("(HTTP 422)");
    expect(other.out).toBe("");
    const submitted = submit(dir, 6001, "APPROVE");
    expect(submitted.code).toBe(0);
    expect(JSON.parse(submitted.out)).toMatchObject({
      id: 6001,
      commit_id: head,
      state: "APPROVED",
    });
  });

  // The rows below ask each world rig.sh builds, through its own routes in the order the stand-in
  // matches them, what gh-standin.test.ts asks of a run directory it writes itself: a world's
  // route answering ahead of the stand-in is how the submit endpoint came to be answered
  // `{id:6001}` with no head on it, so each answer is asked of both worlds, with the id each
  // world's create answers.
  const WORLD_IDS = [
    ["review_world", 6001],
    ["approved_world", 5002],
  ] as const;
  const REVIEWS = "repos/example/widgets/pulls/1001/reviews";

  // GitHub keeps one pending review per user and pull request and refuses the create of a second
  // while one stands. Catches: a world that takes the second create and makes it the pending
  // review, which answered a round that read the head again and created a second review 200 and
  // then submitted a review GitHub never created, at the head that second create named.
  for (const [world, id] of WORLD_IDS)
    test(`${world} refuses a second pending create while one stands, and submits the first`, () => {
      const dir = built(world);
      const head = headOf(dir);
      createPending(dir, head);
      const again = call(dir, "api", REVIEWS, "-f", `commit_id=${"7".repeat(40)}`, "-f", "body=b");
      expect(again).toEqual({
        code: 1,
        out: "",
        err: "gh: Unprocessable Entity (HTTP 422)\nUser can only have one pending review per pull request\n",
      });
      const submitted = submit(dir, id, "REQUEST_CHANGES");
      expect(submitted.code).toBe(0);
      expect(JSON.parse(submitted.out)).toMatchObject({
        id,
        commit_id: head,
        state: "CHANGES_REQUESTED",
      });
    });

  // GitHub's create takes the events APPROVE, REQUEST_CHANGES and COMMENT, or none, and requires
  // a body for the second and third. Catches: a world whose create route answers an `approve`, a
  // `PENDING` or a bodyless change request 200 with an id, where GitHub answers 422 and creates
  // nothing — so nothing is left pending for the submit after them either.
  for (const [world, id] of WORLD_IDS)
    test(`${world} refuses the creates GitHub refuses`, () => {
      const dir = built(world);
      for (const fields of [
        ["-f", "event=approve", "-f", "body=looks good"],
        ["-f", "event=PENDING"],
        ["-f", "event=REQUEST_CHANGES", "-f", `commit_id=${headOf(dir)}`],
      ]) {
        const refused = call(dir, "api", "--method", "POST", REVIEWS, ...fields);
        expect([fields, refused.code, refused.out, refused.err]).toEqual([
          fields,
          1,
          "",
          expect.stringContaining("gh: Unprocessable Entity (HTTP 422)\n"),
        ]);
      }
      expect(submit(dir, id, "APPROVE").err).toContain(
        "Can not submit a review that is not pending."
      );
    });

  // GitHub answers `GET …/reviews/<id>` with that review as it stands. Catches: the read of the
  // review the run itself created answered by a route — `[]` in review_world, the list of
  // reviews in approved_world — which `text-only-head-3` and `defect-base-3` of review
  // 5887659447 got for `--jq '{id,state,commit_id}'`, and exited 1 on.
  for (const [world, id] of WORLD_IDS)
    test(`${world} answers a GET of the review the run created with that review`, () => {
      const dir = built(world);
      const head = headOf(dir);
      const read = () =>
        answer(
          dir,
          "api",
          `${REVIEWS}/${id}`,
          "--jq",
          "{id,state,commit_id,user:.user.login,body}"
        );
      createPending(dir, head);
      const pending = read();
      expect([pending.code, JSON.parse(pending.out)]).toEqual([
        0,
        { id, state: "PENDING", commit_id: head, user: WORLD_LOGIN, body: "two findings" },
      ]);
      expect(submit(dir, id, "APPROVE").code).toBe(0);
      expect(JSON.parse(read().out)).toMatchObject({ id, state: "APPROVED", commit_id: head });
    });

  // GitHub answers `PATCH …/pulls/<n>` with the pull request as the PATCH left it. Catches: the
  // generic write route's `{id:6001, html_url}` answered to it, which 23 recorded PATCH calls read
  // `.body` back from as null (review 5887659447, finding 6).
  for (const [world] of WORLD_IDS)
    test(`${world} answers a PATCH of the pull request's body with the pull request`, () => {
      const dir = built(world);
      const patched = answer(
        dir,
        "api",
        "-X",
        "PATCH",
        "repos/example/widgets/pulls/1001",
        "-f",
        "body=the fixed body",
        "--jq",
        "{number,body}"
      );
      expect([patched.code, JSON.parse(patched.out)]).toEqual([
        0,
        { number: 1001, body: "the fixed body" },
      ]);
      expect(answer(dir, "pr", "view", "1001", "--json", "body", "--jq", ".body").out).toBe(
        "the fixed body"
      );
    });

  // The verb decides what the call is, in every spelling gh takes it in and by the reader the
  // score reads the same call with (`isPost`): a call naming GET is a read of that path
  // whatever its fields say, which GitHub answers 404, since it has no read there; it submits
  // nothing, and the review it did not submit is still pending for the call that does. That
  // last call is written with the leading slash `gh api` takes either way,
  // which is how the route reads wherever it is copied from GitHub's own documentation.
  // Catches: a GET answered as the submission of the pending review, which gh never sent and
  // GitHub never answers; a GET read as the submit that left no pending review behind it, where
  // the submit that follows would be refused 422; and a submit read as none for its leading
  // slash, which would then be refused as a write no route of the world models.
  test("a GET of the submit route submits nothing, and a submit written with a leading slash submits", () => {
    const dir = built("review_world");
    const head = headOf(dir);
    createPending(dir, head);
    for (const verb of [["-X", "GET"], ["--method=GET"], ["-XGET"]])
      expect(call(dir, "api", ...verb, submitRoute(6001), "-f", "event=APPROVE")).toEqual({
        code: 1,
        out: GITHUB_404,
        err: "gh: Not Found (HTTP 404)\n",
      });
    const submitted = call(
      dir,
      "api",
      "--method",
      "POST",
      `/${submitRoute(6001)}`,
      "-f",
      "event=APPROVE"
    );
    expect(submitted.code).toBe(0);
    expect(JSON.parse(submitted.out)).toMatchObject({
      id: 6001,
      commit_id: head,
      state: "APPROVED",
    });
  });

  // The create is chosen by the call's verb and by no regex of its own, through the world's own
  // routes: gh sends a call carrying a field or an `--input` as a POST, so each spelling below
  // is the create `--method POST` is, it is answered by the create route, and the review it
  // leaves pending is the one the submit that follows is answered with. Catches: a world whose
  // create route asks for `(-X|--method) ?POST` written out, which sent a create written
  // `-f commit_id=<head>`, `--method=POST`, `-XPOST` or in lower case to the GET route under it
  // — `[]` in review_world, the reviews already there in approved_world — so no pending review
  // was recorded and the submit that followed was refused 422 where GitHub answers 200.
  for (const [name, verb] of [
    ["no method at all", []],
    ["--method=POST", ["--method=POST"]],
    ["-XPOST", ["-XPOST"]],
    ["--method post", ["--method", "post"]],
  ] as const)
    test(`a create written with ${name} is the create of a pending review`, () => {
      const dir = built("review_world");
      const head = headOf(dir);
      const created = call(
        dir,
        "api",
        ...verb,
        "repos/example/widgets/pulls/1001/reviews",
        "-f",
        `commit_id=${head}`,
        "-f",
        "body=two findings"
      );
      expect([created.code, created.err]).toEqual([0, ""]);
      expect(JSON.parse(created.out)).toMatchObject({ id: 6001, state: "PENDING" });
      const submitted = submit(dir, 6001, "APPROVE");
      expect(submitted.code).toBe(0);
      expect(JSON.parse(submitted.out)).toMatchObject({
        id: 6001,
        commit_id: head,
        state: "APPROVED",
      });
    });

  // The comparison the rule has a role read (review-threads.md step 2) is the run's own
  // remote's, answered before the routes, and a call that writes to that path is no comparison:
  // the stand-in reads the verb with the score's own reader (`methodOf`), so a write in any
  // spelling gh takes falls through to the routes, where no route models it, and GitHub's 404 is
  // the answer, as GitHub refuses a write to an endpoint it does not serve. Catches: a comparison
  // read off the path alone, which would answer a write with the two refs' diff and tell the
  // caller its call succeeded; a verb read by a regex of the stand-in's own, which read `--method
  // POST` and left `--method=post` and `-XPOST` answered as comparisons; and the write answered
  // `{id:6001}`, which the route every write fell to gave it.
  test("a comparison is the run's own remote's, and a POST to that path is no comparison", () => {
    const dir = built("review_world");
    const range = "repos/example/widgets/compare/main...legion/LWEVAL-1";
    expect(answer(dir, "api", range, "--jq", ".status")).toEqual({ code: 0, out: "ahead" });
    for (const verb of [["--method", "POST"], ["--method=post"], ["-XPOST"]])
      expect(call(dir, "api", ...verb, range, "--jq", ".id")).toEqual({
        code: 1,
        out: GITHUB_404,
        err: "gh: Not Found (HTTP 404)\n",
      });
  });

  // The three answers the world approved_world builds gives on its own reviews path, which a round
  // reads its work back from. Catches: a body edit applied to every object a route answers with
  // that carries a `body`, so after the implementer's own edit the review already on the pull
  // request read back as the pull request's text; a create answered `{id, html_url}` alone,
  // which says nothing about whether the review was submitted and at what head — the answer
  // the threads-closed head arm's round read before it submitted a second time; and a read of
  // one review answered with the whole list, which is no answer to the call that was made.
  test("the approved world answers a body edit, a create and one review each as GitHub does", () => {
    const dir = built("approved_world");
    const head = headOf(dir);
    // The head is the reviewer's own text commit, and the passes stand at the implementer's commit
    // below it (rig.sh's approved_world). Correct's review 5001 is the reviewer's `COMMENT` review,
    // whose inline comment opened its thread, and its body is empty, as GitHub lists one.
    const implemented = bash('git -C "$1/remote.git" rev-parse "$2^"', [dir, head]).stdout.trim();
    const reviewBody = () =>
      answer(dir, "api", "repos/example/widgets/pulls/1001/reviews/5001", "--jq", ".body").out;
    const review = reviewBody();
    expect(review).toBe("");
    // The body's check line says what the check ran and what it read, as a recorded result
    // reads (fixture_checks), so a reviewer reading the body reads a check that names its run.
    expect(answer(dir, "pr", "view", "1001", "--json", "body", "--jq", ".body").out).toContain(
      "pass at " +
        implemented.slice(0, 12) +
        ", recorded by implementer. Ran `bun greet.ts '  Ada  '` and read `Hello, Ada!`, so the name is trimmed"
    );
    expect(answer(dir, "pr", "edit", "1001", "--body", "the fixed body").code).toBe(0);
    expect(answer(dir, "pr", "view", "1001", "--json", "body", "--jq", ".body")).toEqual({
      code: 0,
      out: "the fixed body",
    });
    expect(reviewBody()).toBe(review);
    // One review by its id is that review and not the list of them, which is what the reviews
    // route answers the path above it.
    expect(
      answer(dir, "api", "repos/example/widgets/pulls/1001/reviews/5001", "--jq", ".state")
    ).toEqual({ code: 0, out: "COMMENTED" });
    expect(
      answer(dir, "api", "repos/example/widgets/pulls/1001/reviews", "--jq", "length")
    ).toEqual({ code: 0, out: "1" });
    // And a path under one review — its comments — is no read of that review: it is the list of
    // that review's inline comments, as GitHub answers a list there.
    expect(
      answer(dir, "api", "repos/example/widgets/pulls/1001/reviews/5001/comments", "--jq", "type")
    ).toEqual({ code: 0, out: "array" });
    // The create: the id, the URL and the user are the world's, and the state, the head the
    // review was created at and its body are the call's own (gh-standin.ts), in GitHub's whole
    // review (`pull-request-review`): its node id, pages and links, and no `submitted_at` while it
    // is pending.
    const created = call(
      dir,
      "api",
      "--method",
      "POST",
      "repos/example/widgets/pulls/1001/reviews",
      "-f",
      `commit_id=${head}`
    );
    expect(created.code).toBe(0);
    const page = "https://github.com/example/widgets/pull/1001#pullrequestreview-5002";
    const pull = "https://api.github.com/repos/example/widgets/pulls/1001";
    expect(JSON.parse(created.out)).toEqual({
      id: 5002,
      node_id: "PRR_kwDOAAAAAc4AAE5002",
      html_url: page,
      pull_request_url: pull,
      _links: { html: { href: page }, pull_request: { href: pull } },
      user: expect.objectContaining({ login: WORLD_LOGIN, type: "Bot" }),
      author_association: "NONE",
      is_copilot_code_review: false,
      copilot_code_review_effort: null,
      state: "PENDING",
      commit_id: head,
      body: "",
    });
  });

  // <run>/threads.json is what threadsClosed reads the scenario's threads from, and the route
  // and the pull request's body are what the agents read them from: three copies of one fact.
  // Catches: a world whose record and whose answers name different threads, where an
  // implementer would reply on threads the score does not count and the score would ask for
  // replies on threads no agent can see.
  test("the approved world's threads.json is the threads its route serves and its body names", () => {
    const dir = built("approved_world");
    const threads = JSON.parse(readFileSync(path.join(dir, "threads.json"), "utf8")) as {
      id: string;
      path: string;
    }[];
    // Correct's one thread, on the workflow file the review App may not push: the document's
    // finding is closed by the reviewer's own commit, which left no thread on it.
    expect(threads.map((thread) => [thread.id, thread.path])).toEqual([
      ["PRRT_kwDOfix002", ".github/workflows/greet.yml"],
    ]);
    const served = answer(
      dir,
      ...threadQuery(
        '[.data.repository.pullRequest.reviewThreads.nodes[] | .id + " " + .path] | join(",")'
      )
    );
    expect(served.out).toBe(threads.map((thread) => `${thread.id} ${thread.path}`).join(","));
    const body = answer(dir, "pr", "view", "1001", "--json", "body", "--jq", ".body").out;
    expect(body).toContain(`**Threads:** 0 resolved, ${threads.length} unresolved.`);
    for (const thread of threads) expect(body).toContain(`- Thread ${thread.id}: still open`);
  });

  // The negative control's world: one thread, on the sentence the dropped guard makes false, and
  // no text finding of its own. Catches a control that gains the workflow thread the measured
  // arm carries, which would give it a thread a text fix closes.
  test("the dropped guard's world carries the one thread no text fix closes", () => {
    const dir = built("approved_world", "dropped");
    expect(
      (JSON.parse(readFileSync(path.join(dir, "threads.json"), "utf8")) as { id: string }[]).map(
        (thread) => thread.id
      )
    ).toEqual(["PRRT_kwDOfix101"]);
  });
});

// The routes a world answers are the routes GitHub answers a role, and GitHub answers the same
// routes whichever label asks: every gh read either label's role texts name, and every route a
// recorded run called, each declared with its evidence (gh-routes.ts's WORLD_ROUTES and
// gh-route-evidence.ts's ROUTE_EVIDENCE). These
// rows pin the table against the worlds rig.sh builds: every route of the table answered in
// every world, with the shape GitHub's answer has; and every route a world writes into its
// fixtures.json reached by a route of the table, so a route added to a world and declared nowhere
// fails here. Measured at 3a2ea66a: 14 gh calls of round 17's forty and the world-health check's
// eight runs refused with no refusal production gives (gh-refused-calls.json), which voids each
// run that meets one.
/** `text` as the one JSON value it holds, written compactly, or `text` as it is when it holds no
 * one JSON value: jq indents the values it prints, and a row reads the value. */
function compact(text: string): string {
  try {
    return JSON.stringify(JSON.parse(text));
  } catch {
    return text;
  }
}
describe("the world's gh routes: every read a role text names, answered as GitHub answers it", () => {
  const WORLDS = [
    "tester_proof_world",
    "review_world",
    "review_world dropped",
    "approved_world",
    "approved_world dropped",
  ] as const;
  type WorldName = (typeof WORLDS)[number];
  /** WORLD, with GUARD after a space, built for real under a directory of its own, with the two
   * steps "the worlds' routes" rows stub stubbed the same way. */
  function build(name: WorldName): string {
    const [world = "", guard = ""] = name.split(" ");
    const dir = freshDir(`gh-routes-${world}`);
    const result = bash(
      [
        fn("fail"),
        ...[
          "fixture_open",
          "fixture_git",
          "role_identity",
          "role_env",
          "fixture_docs_before",
          "fixture_first_commit",
          "fixture_round_files",
          "fixture_head_commit",
          "fixture_implement",
          "fixture_test_handoff",
          "fixture_checks",
          "fixture_body",
          "fixture_write",
          "concluded_checks",
          "base_ruleset_routes",
          "resolution_protection_routes",
          "round_world",
          "fixture_reviewer_commit",
          world,
        ].map(fn),
        'fixture_cli() { printf "export const greet = 1;\\n" >"$src/greet.ts"; : >"$src/greet.test.ts"; }',
        "fixture_workspace() { :; }",
        "R=$1 worker_key=LWEVAL-1 worker_repo=example/widgets worker_base=main",
        assignment("world_author_name"),
        assignment("world_author_email"),
        assignment("world_login"),
        ...WORLD_APPS.map(assignment),
        "text_finding_doc_line=THE-DOC-LINE text_finding_trigger=THE-TRIGGER",
        `${world} 1 100 "$2" "$3"`,
      ].join("\n"),
      [dir, guard, guard === "" ? "correct" : "code-defect"]
    );
    expect(result).toMatchObject({ code: 0, stderr: "" });
    writeGhSelectors(dir);
    return dir;
  }
  const worlds = new Map<WorldName, string>();
  /** The pane's own gh configuration (its GH_CONFIG_DIR), the hosts.yml the daemon renders for a
   * role's claim (packages/daemon's `ghconfig.Hosts`), which `gh auth status` names and never
   * reads: its token is one of another length than the rig's, which no answer names, and random,
   * as a real one is. */
  let paneGh = "";
  const PANE_TOKEN = `ghs_${fixtureSecret("pane")}`;
  beforeAll(() => {
    for (const name of WORLDS) worlds.set(name, build(name));
    paneGh = freshDir("pane-gh");
    writeFileSync(
      path.join(paneGh, "hosts.yml"),
      [
        ...["github.com:", `    oauth_token: ${PANE_TOKEN}`, "    user: x-access-token"],
        ...["    git_protocol: https", "    users:", "        x-access-token:"],
        `            oauth_token: ${PANE_TOKEN}`,
        "",
      ].join("\n")
    );
  });
  const dirOf = (name: WorldName) => worlds.get(name) ?? "";
  /** What the gh stand-in answers `gh <args…>` in the world under `dir`, from a pane whose gh
   * configuration is `paneGh`, with an answer that is one JSON value read as that value written
   * compactly, whatever indentation jq gave it. */
  function gh(dir: string, ...args: string[]) {
    const made = spawnChecked(["bun", GH_STANDIN, "gh", ...args], {
      env: { ...childEnv, GH_DATA_DIR: dir, GH_CONFIG_DIR: paneGh },
    });
    return {
      code: made.exitCode,
      out: compact(made.stdout.toString().trim()),
      err: made.stderr.toString(),
    };
  }
  const headOf = (dir: string) =>
    (JSON.parse(readFileSync(path.join(dir, "world.json"), "utf8")) as { head: string }).head;
  const REPO = "repos/example/widgets";
  /** The rules every world's base holds, production's seven in production's order (rig.sh's
   * base_ruleset_routes). */
  const RULE_TYPES = [
    "deletion",
    "non_fast_forward",
    "required_linear_history",
    "pull_request",
    "required_status_checks",
    "merge_queue",
    "code_scanning",
  ];
  /** The one world whose head GitHub reports `CLEAN`: its approval stands, every check its base
   * requires is green and its base requires no thread resolved. Every other world's head is
   * `BLOCKED`: the tester's and the review round's by the approval their base requires and no
   * review has given (INFRA-764's ruling of 2026-11-18), correct's by that same missing approval
   * and its open thread (rig.sh's approved_world, resolution_protection_routes). */
  const CLEAN_WORLD: WorldName = "approved_world dropped";
  /** The review threads each world's graph holds: `approved_world` is correct's, whose reviewer's
   * `COMMENT` review left one thread on the workflow file (`build`). */
  const THREADS: Record<WorldName, string> = {
    tester_proof_world: "",
    review_world: "PRRT_kwDOrig0001",
    "review_world dropped": "PRRT_kwDOrig0001",
    approved_world: "PRRT_kwDOfix002",
    "approved_world dropped": "PRRT_kwDOfix101",
  };
  /** Whether `world` is one approved_world builds, whose reviewer's review 5001 opened its one
   * thread: a `COMMENT` review in correct, the approval in code-defect (the dropped guard). */
  const approved = (world: WorldName) => world.startsWith("approved_world");
  /** The reviewThreads query review-threads.md gives, word for word. */
  const THREAD_QUERY =
    "query($o:String!,$r:String!,$n:Int!,$endCursor:String){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}}";
  /** The two queries merger.md gives, word for word. */
  const MERGER_WATCH =
    "query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){state mergedAt isInMergeQueue mergeStateStatus headRefOid mergeQueueEntry{state position}}}}";
  const MERGER_TIMELINE =
    "query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){timelineItems(itemTypes:[ADDED_TO_MERGE_QUEUE_EVENT,REMOVED_FROM_MERGE_QUEUE_EVENT],last:20){nodes{__typename ... on AddedToMergeQueueEvent{createdAt actor{login}} ... on RemovedFromMergeQueueEvent{createdAt reason actor{login} beforeCommit{oid}}}}}}}";
  type Answer = { code: number; out: string; err: string };
  /** A call of each route of WORLD_ROUTES, as a role writes it in a world whose head is `head`,
   * any other spelling of it a world answers by a route of its own (`also`), and, for a read,
   * what GitHub answers it with in `world`. A write's answer is the rows' of gh-standin.test.ts
   * and "the worlds' routes"; here it is reached and nothing more. */
  const SAMPLES: Record<
    string,
    {
      argv: (head: string) => string[];
      also?: string[][];
      answers?: (got: Answer, world: WorldName, head: string) => unknown;
    }
  > = {
    "GET repos/{owner}/{repo}": {
      argv: () => [
        "api",
        REPO,
        "--jq",
        "[.full_name, .default_branch, .squash_merge_commit_message]",
      ],
      answers: (got) =>
        expect(got).toMatchObject({
          code: 0,
          out: JSON.stringify(["example/widgets", "main", "COMMIT_MESSAGES"]),
        }),
    },
    "GET repos/{owner}/{repo}/pulls/{number}": {
      argv: () => ["api", `${REPO}/pulls/1001`, "--jq", ".head.sha"],
      answers: (got, _, head) => expect(got).toMatchObject({ code: 0, out: head }),
    },
    "GET repos/{owner}/{repo}/pulls/{number}/reviews": {
      argv: () => ["api", "--paginate", `${REPO}/pulls/1001/reviews`, "--jq", "[.[] | .id] | @csv"],
      // The reviewer's review 5001 in a world approved_world builds, and the bot's own COMMENTED
      // review in a review round's world, which its thread came in.
      answers: (got, world) =>
        expect(got).toMatchObject({
          code: 0,
          out: approved(world) ? "5001" : world.startsWith("review_world") ? "4001" : "",
        }),
    },
    "GET repos/{owner}/{repo}/pulls/{number}/reviews/{id}": {
      argv: () => ["api", `${REPO}/pulls/1001/reviews/5001`, "--jq", ".state"],
      answers: (got, world) =>
        expect(got).toMatchObject(
          approved(world)
            ? { code: 0, out: world === CLEAN_WORLD ? "APPROVED" : "COMMENTED" }
            : { code: 1, err: "gh: Not Found (HTTP 404)\n" }
        ),
    },
    "GET repos/{owner}/{repo}/pulls/{number}/reviews/{id}/comments": {
      argv: () => ["api", `${REPO}/pulls/1001/reviews/5001/comments`, "--jq", "[.[].id]"],
      // Each such world's review 5001 carries the one inline comment that opened its one thread:
      // correct's on the workflow file, code-defect's on docs/cli.md.
      answers: (got, world) =>
        expect(got).toMatchObject(
          approved(world)
            ? { code: 0, out: "[7001]" }
            : { code: 1, err: "gh: Not Found (HTTP 404)\n" }
        ),
    },
    "GET repos/{owner}/{repo}/pulls/{number}/comments": {
      argv: () => ["api", `${REPO}/pulls/1001/comments`, "--jq", "[.[].id]"],
      answers: (got, world) =>
        expect(got).toMatchObject({
          code: 0,
          out: world === "tester_proof_world" ? "[]" : "[7001]",
        }),
    },
    // A file's contents at a commit, and at the default branch when the read names no ref (the
    // tester's cross-check found a reviewer reading the workflow so, with `--jq .content`).
    "GET repos/{owner}/{repo}/contents/{path+}": {
      argv: (head) => [
        "api",
        `${REPO}/contents/greet.ts?ref=${head}`,
        "-H",
        "Accept: application/vnd.github.raw",
      ],
      also: [["api", `${REPO}/contents/greet.ts`, "--jq", ".content"]],
      answers: (got) =>
        expect(got).toMatchObject({ code: 0, out: "export const greet = 1;", err: "" }),
    },
    "GET repos/{owner}/{repo}/issues/{number}/comments": {
      argv: () => ["api", `${REPO}/issues/1001/comments`],
      answers: (got) => expect(got).toMatchObject({ code: 0, out: "[]" }),
    },
    "GET repos/{owner}/{repo}/commits/{ref}/check-runs": {
      argv: (head) => [
        "api",
        `${REPO}/commits/${head}/check-runs`,
        "--jq",
        "[.total_count, (.check_runs | length)]",
      ],
      // `test`, `lint` and the code-scanning tool's `CodeQL` (rig.sh's concluded_checks).
      answers: (got) => expect(got).toMatchObject({ code: 0, out: "[3,3]" }),
    },
    "GET repos/{owner}/{repo}/commits/{ref}/status": {
      argv: (head) => [
        "api",
        `${REPO}/commits/${head}/status`,
        "--jq",
        "[.state, .total_count, (.statuses | length), .sha]",
      ],
      answers: (got, _, head) =>
        expect(got).toMatchObject({ code: 0, out: JSON.stringify(["pending", 0, 0, head]) }),
    },
    "GET repos/{owner}/{repo}/actions/runs": {
      argv: (head) => ["api", `${REPO}/actions/runs?head_sha=${head}&per_page=100`],
      answers: (got) =>
        expect(got).toMatchObject({
          code: 0,
          out: JSON.stringify({ total_count: 0, workflow_runs: [] }),
        }),
    },
    "GET repos/{owner}/{repo}/actions/runs/{run_id}/jobs": {
      argv: () => ["api", `${REPO}/actions/runs/1/jobs`],
      answers: (got) =>
        expect(got).toMatchObject({
          code: 1,
          out: JSON.stringify({
            message: "Not Found",
            documentation_url:
              "https://docs.github.com/rest/actions/workflow-jobs#list-jobs-for-a-workflow-run",
            status: "404",
          }),
          err: "gh: Not Found (HTTP 404)\n",
        }),
    },
    "GET repos/{owner}/{repo}/rules/branches/{branch+}": {
      argv: () => ["api", "--paginate", `${REPO}/rules/branches/main`, "--jq", "[.[].type]"],
      answers: (got) =>
        expect(got).toMatchObject({
          code: 0,
          out: JSON.stringify(RULE_TYPES),
        }),
    },
    "GET repos/{owner}/{repo}/rulesets": {
      argv: () => ["api", `${REPO}/rulesets`, "--jq", "[.[] | [.id, .name, .enforcement]]"],
      answers: (got) =>
        expect(got).toMatchObject({ code: 0, out: JSON.stringify([[1, "main", "active"]]) }),
    },
    "GET repos/{owner}/{repo}/rulesets/{id}": {
      argv: () => ["api", `${REPO}/rulesets/1`, "--jq", "[.rules[].type]"],
      answers: (got) =>
        expect(got).toMatchObject({
          code: 0,
          out: JSON.stringify(RULE_TYPES),
        }),
    },
    "GET repos/{owner}/{repo}/branches/{branch}/protection": {
      argv: () => [
        "api",
        `${REPO}/branches/main/protection`,
        "--jq",
        ".required_status_checks.contexts",
      ],
      answers: (got) =>
        expect(got).toMatchObject({
          code: 1,
          err: "gh: Resource not accessible by integration (HTTP 403)\n",
        }),
    },
    "GET repos/{owner}/{repo}/branches/{branch+}": {
      argv: () => ["api", `${REPO}/branches/main`, "--jq", "[.name, .protected]"],
      answers: (got) => expect(got).toMatchObject({ code: 0, out: JSON.stringify(["main", true]) }),
    },
    "GET repos/{owner}/{repo}/compare/{basehead+}": {
      argv: () => ["api", `${REPO}/compare/main...legion/LWEVAL-1`, "--jq", ".status"],
      answers: (got) => expect(got).toMatchObject({ code: 0, out: "ahead" }),
    },
    "GET repos/{owner}/{repo}/git/ref/{ref+}": {
      argv: () => ["api", `${REPO}/git/ref/heads/legion/LWEVAL-1`, "--jq", "[.ref, .object.sha]"],
      answers: (got, _, head) =>
        expect(got).toMatchObject({
          code: 0,
          out: JSON.stringify(["refs/heads/legion/LWEVAL-1", head]),
        }),
    },
    "GET repos/{owner}/{repo}/git/refs": {
      argv: () => [
        "api",
        `${REPO}/git/refs`,
        "--jq",
        '[.[] | select(.ref == "refs/pull/1001/head") | .object.sha]',
      ],
      answers: (got, _, head) =>
        expect(got).toMatchObject({ code: 0, out: JSON.stringify([head]) }),
    },
    "GET repos/{owner}/{repo}/git/refs/{ref+}": {
      argv: () => ["api", `${REPO}/git/refs/heads/legion/LWEVAL-1`, "--jq", "[.ref, .object.sha]"],
      answers: (got, _, head) =>
        expect(got).toMatchObject({
          code: 0,
          out: JSON.stringify(["refs/heads/legion/LWEVAL-1", head]),
        }),
    },
    "GET repos/{owner}/{repo}/git/commits/{sha}": {
      argv: (head) => ["api", `${REPO}/git/commits/${head}`, "--jq", "[.sha, .tree.sha != null]"],
      answers: (got, _, head) =>
        expect(got).toMatchObject({ code: 0, out: JSON.stringify([head, true]) }),
    },
    "GET repos/{owner}/{repo}/git/trees/{sha+}": {
      argv: (head) => [
        "api",
        `${REPO}/git/trees/${head}?recursive=1`,
        "--jq",
        '[.sha, ([.tree[].path] | index("greet.ts") != null)]',
      ],
      answers: (got, _, head) =>
        expect(got).toMatchObject({ code: 0, out: JSON.stringify([head, true]) }),
    },
    "GET repos/{owner}/{repo}/commits/{ref+}": {
      argv: (head) => ["api", `${REPO}/commits/${head}`, "--jq", '[.sha, (.stats | has("total"))]'],
      answers: (got, _, head) =>
        expect(got).toMatchObject({ code: 0, out: JSON.stringify([head, true]) }),
    },
    "GET repos/{owner}/{repo}/commits": {
      argv: (head) => ["api", `${REPO}/commits?sha=${head}&per_page=1`, "--jq", ".[0].sha"],
      answers: (got, _, head) => expect(got).toMatchObject({ code: 0, out: head }),
    },
    "GET repos/{owner}/{repo}/branches": {
      argv: () => ["api", `${REPO}/branches`, "--jq", "[.[] | [.name, .protected]]"],
      answers: (got) =>
        expect(got).toMatchObject({
          code: 0,
          out: JSON.stringify([
            ["legion/LWEVAL-1", false],
            ["main", true],
          ]),
        }),
    },
    "GET repos/{owner}/{repo}/tags": {
      argv: () => ["api", `${REPO}/tags`],
      answers: (got) => expect(got).toMatchObject({ code: 0, out: "[]" }),
    },
    "GET repos/{owner}/{repo}/pulls/{number}/commits": {
      argv: () => ["api", `${REPO}/pulls/1001/commits`, "--jq", ".[-1].sha"],
      answers: (got, _, head) => expect(got).toMatchObject({ code: 0, out: head }),
    },
    // Every world's pull request adds greet.ts: fixture_cli writes it after fixture_first_commit
    // made the base, and fixture_head_commit commits it on the issue branch, in tester_proof_world
    // and in round_world, which review_world and approved_world build on.
    "GET repos/{owner}/{repo}/pulls/{number}/files": {
      argv: () => ["api", `${REPO}/pulls/1001/files`, "--jq", '[.[].filename] | index("greet.ts")'],
      answers: (got) => expect(got).toMatchObject({ code: 0, out: expect.stringMatching(/^\d+$/) }),
    },
    "GET user": {
      argv: () => ["api", "user", "--jq", ".login"],
      answers: (got) =>
        expect(got).toMatchObject({
          code: 1,
          out: JSON.stringify({
            message: "Resource not accessible by integration",
            documentation_url:
              "https://docs.github.com/rest/users/users#get-the-authenticated-user",
            status: "403",
          }),
          err: "gh: Resource not accessible by integration (HTTP 403)\n",
        }),
    },
    "GH auth status": {
      argv: () => ["auth", "status"],
      answers: (got) =>
        expect(got).toEqual({
          code: 0,
          out: [
            "github.com",
            `  ✓ Logged in to github.com account x-access-token (${paneGh}/hosts.yml)`,
            "  - Active account: true",
            "  - Git operations protocol: https",
            // The rig's token as gh masks it (daemon-pane.ts's RIG_GH_TOKEN: `ghs_` and 36).
            `  - Token: ghs_${"*".repeat(36)}`,
          ].join("\n"),
          err: "",
        }),
    },
    "GH pr view": {
      argv: () => [
        "pr",
        "view",
        "1001",
        "--json",
        "headRefOid,isCrossRepository",
        "--jq",
        "[.headRefOid, .isCrossRepository]",
      ],
      answers: (got, _, head) =>
        expect(got).toMatchObject({ code: 0, out: JSON.stringify([head, false]) }),
    },
    "GH pr list": {
      argv: () => [
        "pr",
        "list",
        "--head",
        "legion/LWEVAL-1",
        "--state",
        "open",
        "--json",
        "number",
        "--jq",
        ".[0].number",
      ],
      answers: (got) => expect(got).toMatchObject({ code: 0, out: "1001" }),
    },
    "GH pr diff": {
      argv: () => ["pr", "diff", "1001", "--name-only"],
      answers: (got) =>
        expect(got).toMatchObject({ code: 0, out: expect.stringContaining("greet.ts") }),
    },
    "GH pr checks": {
      argv: () => ["pr", "checks", "1001"],
      answers: (got) =>
        expect(got).toMatchObject({ code: 0, out: expect.stringContaining("test\tpass") }),
    },
    "GH run view": {
      argv: () => ["run", "view"],
      also: [["run", "view", "1", "--log-failed"]],
      answers: (got) =>
        expect(got).toMatchObject({
          code: 1,
          out: "",
          err: expect.stringMatching(
            /^run or job ID required when not running interactively\n\nUsage: {2}gh run view \[<run-id>\] \[flags\]\n/
          ),
        }),
    },
    "GH run watch": {
      argv: () => ["run", "watch"],
      answers: (got) =>
        expect(got).toMatchObject({
          code: 1,
          out: "",
          err: expect.stringMatching(
            /^run ID required when not running interactively\n\nUsage: {2}gh run watch <run-id> \[flags\]\n/
          ),
        }),
    },
    "GRAPHQL query repository.pullRequest.reviewThreads": {
      argv: () => [
        "api",
        "graphql",
        "--paginate",
        "--slurp",
        "-f",
        `query=${THREAD_QUERY}`,
        "-F",
        "o=example",
        "-F",
        "r=widgets",
        "-F",
        "n=1001",
        "--jq",
        '[.[].data.repository.pullRequest.reviewThreads.nodes[].id] | join(",")',
      ],
      answers: (got, world) => expect(got).toMatchObject({ code: 0, out: THREADS[world] }),
    },
    "GRAPHQL query viewer.login": {
      argv: () => ["api", "graphql", "-f", "query={viewer{login}}", "--jq", ".data.viewer.login"],
      answers: (got) => expect(got).toMatchObject({ code: 0, out: WORLD_LOGIN }),
    },
    "GRAPHQL query repository.pullRequest": {
      argv: () => [
        "api",
        "graphql",
        "-F",
        "number=1001",
        "-f",
        "owner=example",
        "-f",
        "name=widgets",
        "-f",
        `query=${MERGER_WATCH}`,
      ],
      answers: (got, world, head) =>
        expect([got.code, JSON.parse(got.out)]).toEqual([
          0,
          {
            data: {
              repository: {
                pullRequest: {
                  state: "OPEN",
                  mergedAt: null,
                  isInMergeQueue: false,
                  mergeStateStatus: world === CLEAN_WORLD ? "CLEAN" : "BLOCKED",
                  headRefOid: head,
                  mergeQueueEntry: null,
                },
              },
            },
          },
        ]),
    },
    "GRAPHQL query repository.pullRequest.timelineItems": {
      argv: () => [
        "api",
        "graphql",
        "-F",
        "number=1001",
        "-f",
        "owner=example",
        "-f",
        "name=widgets",
        "-f",
        `query=${MERGER_TIMELINE}`,
      ],
      answers: (got) =>
        expect(got).toMatchObject({
          code: 0,
          out: JSON.stringify({
            data: { repository: { pullRequest: { timelineItems: { nodes: [] } } } },
          }),
        }),
    },
    "POST repos/{owner}/{repo}/pulls/{number}/reviews": {
      argv: (head) => [
        "api",
        "--method",
        "POST",
        `${REPO}/pulls/1001/reviews`,
        "-f",
        `commit_id=${head}`,
        "-f",
        "event=APPROVE",
      ],
    },
    "POST repos/{owner}/{repo}/pulls/{number}/reviews/{id}/events": {
      argv: () => [
        "api",
        "--method",
        "POST",
        `${REPO}/pulls/1001/reviews/6001/events`,
        "-f",
        "event=APPROVE",
      ],
    },
    "PUT repos/{owner}/{repo}/pulls/{number}/reviews/{id}": {
      argv: () => ["api", "--method", "PUT", `${REPO}/pulls/1001/reviews/5001`, "-f", "body=b"],
    },
    "PATCH repos/{owner}/{repo}/pulls/{number}": {
      argv: () => ["api", "--method", "PATCH", `${REPO}/pulls/1001`, "-f", "body=the body"],
    },
    "POST repos/{owner}/{repo}/pulls/{number}/comments/{id}/replies": {
      argv: () => [
        "api",
        "--method",
        "POST",
        `${REPO}/pulls/1001/comments/7/replies`,
        "-f",
        "body=fixed",
      ],
    },
    "POST repos/{owner}/{repo}/pulls/{number}/comments": {
      argv: (head) => [
        "api",
        "--method",
        "POST",
        `${REPO}/pulls/1001/comments`,
        "-f",
        "body=x",
        "-f",
        `commit_id=${head}`,
      ],
    },
    "POST repos/{owner}/{repo}/issues/{number}/comments": {
      argv: () => ["api", "--method", "POST", `${REPO}/issues/1001/comments`, "-f", "body=a note"],
    },
    "GH pr comment": { argv: () => ["pr", "comment", "1001", "--body", "a note"] },
    "GH pr edit": { argv: () => ["pr", "edit", "1001", "--body", "the body"] },
    "GRAPHQL mutation resolveReviewThread": {
      argv: () => [
        "api",
        "graphql",
        "-f",
        "query=mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}",
        "-F",
        "id=PRRT_kwDOfix002",
      ],
    },
    "GRAPHQL mutation addPullRequestReviewThreadReply": {
      argv: () => [
        "api",
        "graphql",
        "-f",
        "query=mutation($t:ID!,$b:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$t,body:$b}){comment{id}}}",
        "-F",
        "t=PRRT_kwDOfix002",
        "-f",
        "b=fixed",
      ],
    },
  };

  // Catches: a route of the table with no call here, and a call that is no call of its route, so
  // the rows below would answer some other route under its name.
  test("every route of the world's table has a call of its own here", () => {
    const HEAD = "0123456789abcdef0123456789abcdef01234567";
    expect(
      WORLD_ROUTES.map(({ route }) => {
        const sample = SAMPLES[route];
        const calls = sample === undefined ? [] : [sample.argv(HEAD), ...(sample.also ?? [])];
        const rows = calls.map((argv) => {
          const key = routeOf(argv)?.key;
          return key === undefined ? "no call" : worldRouteOf(key)?.route;
        });
        return [route, rows.length === 0 ? ["no call"] : [...new Set(rows)]];
      })
    ).toEqual(WORLD_ROUTES.map(({ route }) => [route, [route]]));
    expect(Object.keys(SAMPLES).sort()).toEqual(WORLD_ROUTES.map(({ route }) => route).sort());
    // The one spelling a world answers apart: `gh run view <id>`, a run the world has not got.
    expect(gh(dirOf("tester_proof_world"), ...(SAMPLES["GH run view"]?.also?.[0] ?? []))).toEqual({
      code: 1,
      out: "",
      err: "failed to get run: HTTP 404: Not Found (https://api.github.com/repos/example/widgets/actions/runs/1?exclude_pull_requests=true)\n",
    });
  });

  // Catches a route a role text names, or a recorded run called, that a world refuses or answers
  // in a shape GitHub never gives: the one regex `check-runs|status` answered the combined status
  // and the workflow runs with the check-runs list, the repository itself had no answer, and the
  // review threads paged nowhere.
  test.each(
    WORLD_ROUTES.filter(({ route }) => SAMPLES[route]?.answers !== undefined).map(
      ({ route }) => route
    )
  )("every world answers %s as GitHub does", (route) => {
    const sample = SAMPLES[route];
    for (const world of WORLDS) {
      const dir = dirOf(world);
      const head = headOf(dir);
      const got = gh(dir, ...(sample?.argv(head) ?? []));
      try {
        sample?.answers?.(got, world, head);
      } catch (error) {
        throw new Error(`${world}: ${error instanceof Error ? error.message : String(error)}`);
      }
    }
  });

  // Catches a route added to a world that the table does not
  // declare: a world answering a call no role text names and no recorded run made, with no
  // evidence for why GitHub serves it. A route another of the same world stands ahead of is dead
  // there, so a route counts as reached where any world reaches it.
  test("every route a world writes is one a route of the table reaches", () => {
    const reached = new Set<string>();
    const written = new Map<string, string>();
    for (const world of WORLDS) {
      const dir = dirOf(world);
      const head = headOf(dir);
      const { routes } = JSON.parse(readFileSync(path.join(dir, "fixtures.json"), "utf8")) as {
        routes: { match: string; methods?: string[] }[];
      };
      const identity = (one: { match: string; methods?: string[] }) =>
        `${one.methods?.join(",") ?? "*"} ${one.match}`;
      for (const one of routes) written.set(identity(one), world);
      for (const sample of Object.values(SAMPLES))
        for (const argv of [sample.argv(head), ...(sample.also ?? [])]) {
          const found = routeFor(routes, argv);
          if (found !== undefined) reached.add(identity(found));
        }
    }
    expect([...written].filter(([identity]) => !reached.has(identity))).toEqual([]);
  });

  // gh pages a GraphQL query that takes `$endCursor` until a page has no next one, asking again
  // with the page's `endCursor` (`--paginate`), and with `--slurp` prints the pages as one list.
  // Catches: a world whose every page is the first, which holds such a loop for ever, and one
  // that pages nothing, which hands a `first:1` reader one thread of two. Correct's world holds
  // one thread of its own, so a comment of the run's opens the second, as GitHub opens one for a
  // review comment that replies to none, and the pages hold the world's thread and then the run's.
  test("the world pages the review threads as GitHub pages them", () => {
    const dir = build("approved_world");
    const opened = gh(
      dir,
      "api",
      "--method",
      "POST",
      `${REPO}/pulls/1001/comments`,
      "-f",
      "body=This sentence says what the CLI does with the spaces.",
      "-f",
      `commit_id=${headOf(dir)}`,
      "-f",
      "path=docs/cli.md",
      "-F",
      "line=1",
      "--jq",
      ".id"
    );
    expect(opened).toEqual({ code: 0, out: "9001", err: "" });
    const query = THREAD_QUERY.replace("first:100", "first:1");
    const paged = gh(
      dir,
      "api",
      "graphql",
      "--paginate",
      "--slurp",
      "-f",
      `query=${query}`,
      "-F",
      "o=example",
      "-F",
      "r=widgets",
      "-F",
      "n=1001"
    );
    expect(paged.code).toBe(0);
    const pages = JSON.parse(paged.out) as {
      data: {
        repository: {
          pullRequest: {
            reviewThreads: {
              pageInfo: { hasNextPage: boolean; endCursor: string };
              nodes: { id: string }[];
            };
          };
        };
      };
    }[];
    expect(
      pages.map(({ data }) => {
        const { pageInfo, nodes } = data.repository.pullRequest.reviewThreads;
        return [nodes.map(({ id }) => id), pageInfo.hasNextPage, typeof pageInfo.endCursor];
      })
    ).toEqual([
      [["PRRT_kwDOfix002"], true, "string"],
      [["PRRT_kwDOAAAAAc4AAD9001"], false, "string"],
    ]);
    // A pull request the world has not got is GitHub's NOT_FOUND, with the data it found and the
    // place in the query that named it (`locations`: measured on 2026-11-17 against production,
    // `…{pullRequest(number:999999){number} name}}` was answered so; THREAD_QUERY's `pullRequest`
    // starts at column 85). gh prints GitHub's body as it came.
    const elsewhere = gh(
      dir,
      "api",
      "graphql",
      "-f",
      `query=${THREAD_QUERY}`,
      "-F",
      "o=example",
      "-F",
      "r=widgets",
      "-F",
      "n=100"
    );
    expect(elsewhere).toEqual({
      code: 1,
      out: JSON.stringify({
        data: { repository: { pullRequest: null } },
        errors: [
          {
            type: "NOT_FOUND",
            path: ["repository", "pullRequest"],
            locations: [{ line: 1, column: 85 }],
            message: "Could not resolve to a PullRequest with the number of 100.",
          },
        ],
      }),
      err: "gh: Could not resolve to a PullRequest with the number of 100.\n",
    });
  });

  // gh pages a REST list by `per_page`, and with `--paginate` asks for every page, running `--jq`
  // over each. Catches a world that hands every page the whole list, which a `--paginate` reader
  // reads twice.
  test("the world pages a REST list by per_page", () => {
    const dir = dirOf("tester_proof_world");
    const head = headOf(dir);
    const paged = gh(
      dir,
      "api",
      "--paginate",
      `${REPO}/commits/${head}/check-runs?per_page=1`,
      "--jq",
      ".check_runs[].name"
    );
    expect(paged).toEqual({ code: 0, out: "test\nlint\nCodeQL", err: "" });
    const second = gh(
      dir,
      "api",
      `${REPO}/commits/${head}/check-runs?per_page=1&page=2`,
      "--jq",
      "[.total_count, [.check_runs[].name]]"
    );
    expect(second).toEqual({ code: 0, out: JSON.stringify([3, ["lint"]]), err: "" });
  });

  // What the run did is the world's state: the comments it posted are the pull request's
  // conversation, and the inline comments its review carried are the pull request's review
  // comments, as GitHub answers both. Catches lists frozen at what the world held before the run.
  test("the conversation and the review comments are the ones the run left", () => {
    const dir = build("review_world");
    const head = headOf(dir);
    expect(gh(dir, "pr", "comment", "1001", "--body", "a note on the change").code).toBe(0);
    expect(
      gh(dir, "api", `${REPO}/issues/1001/comments`, "--jq", "[.[] | {user: .user.login, body}]")
    ).toEqual({
      code: 0,
      out: JSON.stringify([{ user: WORLD_LOGIN, body: "a note on the change" }]),
      err: "",
    });
    expect(
      gh(dir, "pr", "view", "1001", "--json", "comments", "--jq", "[.comments[].body]").out
    ).toBe(JSON.stringify(["a note on the change"]));
    const review = path.join(dir, "review.json");
    writeFileSync(
      review,
      JSON.stringify({
        commit_id: head,
        event: "COMMENT",
        body: "one note",
        comments: [{ path: "greet.ts", line: 1, side: "RIGHT", body: "a line note" }],
      })
    );
    expect(
      gh(dir, "api", "--method", "POST", `${REPO}/pulls/1001/reviews`, "--input", review).code
    ).toBe(0);
    // The pull request's review comments: the bot's, which the world seeded in its own review
    // 4001, then the run's.
    expect(
      gh(
        dir,
        "api",
        `${REPO}/pulls/1001/comments`,
        "--jq",
        "[.[] | [.pull_request_review_id, .path, .line, .body, .commit_id]]"
      ).out
    ).toBe(
      JSON.stringify([
        [
          4001,
          "greet.test.ts",
          4,
          "greet.test.ts:4: `greets by name` names the same thing the next test does; name each test for the case it covers.",
          head,
        ],
        [6001, "greet.ts", 1, "a line note", head],
      ])
    );
    // A review's own comments are listed in GitHub's older shape of a review comment, which names
    // its place in the diff by `position` and carries no `line` (production-shapes.json's
    // `…/reviews/{n}/comments`): the line the review named is the first of greet.ts, which the
    // change adds, so the first line of its diff.
    expect(
      gh(
        dir,
        "api",
        `${REPO}/pulls/1001/reviews/6001/comments`,
        "--jq",
        ".[] | [.pull_request_review_id, .path, .position, .line, .body, .commit_id]"
      ).out
    ).toBe(JSON.stringify([6001, "greet.ts", 1, null, "a line note", head]));
    expect(
      gh(dir, "api", `${REPO}/pulls/1001/reviews`, "--jq", "[.[] | [.id, .state, .body]]").out
    ).toBe(
      JSON.stringify([
        [4001, "COMMENTED", ""],
        [6001, "COMMENTED", "one note"],
      ])
    );
  });

  // A call outside the routes the world answers stays the refusal it has always been, GitHub's
  // 404 with its body, recorded with no route, so `rig.sh health` still reports a world that
  // answers less than production (health.ts's UNROUTED for a read, its unmodelled-write rule for
  // a write). Catches a world that answers everything: the loose `pulls/<n>` regex answered the
  // pull request's files with the pull request, and a GraphQL route answered every mutation with
  // the review threads. The files are a route of the world's own since INFRA-764's reads of
  // 2026-11-21 (gh-routes.ts's WORLD_ROUTES), so the read here is another endpoint under the pull
  // request that the world serves no answer for, its requested reviewers.
  test("a call no route of the world answers is refused, with no route on its record", () => {
    const dir = build("approved_world");
    const refused = [
      ["api", `${REPO}/pulls/1001/requested_reviewers`],
      [
        "api",
        "graphql",
        "-f",
        "query=mutation($pr:ID!,$head:GitObjectID!){enqueuePullRequest(input:{pullRequestId:$pr,expectedHeadOid:$head}){mergeQueueEntry{position}}}",
        "-f",
        "pr=PR_x",
        "-f",
        "head=abc",
      ],
    ];
    for (const argv of refused)
      expect(gh(dir, ...argv)).toEqual({
        code: 1,
        out: compact(GITHUB_404),
        err: "gh: Not Found (HTTP 404)\n",
      });
    const recorded = readFileSync(path.join(dir, "calls.jsonl"), "utf8")
      .split("\n")
      .filter((line) => line.includes('"argv"'))
      .map((line) => (JSON.parse(line) as { route: string | null }).route);
    expect(recorded).toEqual([null, null]);
  });
});

// The 14 gh calls the world-health check found refused at 3a2ea66a (gh-refused-calls.json, the
// tester's file: round 17's forty and the check's eight runs), each made again word for word in
// the world its run booted — the run's pull request number and head read as this world's — and
// answered as GitHub answers it: the repository's default branch, the pull request's
// conversation, the head's combined status and workflow runs, the review threads paged, and the
// review create carrying the body it was sent. Each failed at 3a2ea66a: the first two had no
// route, the next two a route that answered another shape, the GraphQL ones a page gh's
// `--slurp` never prints, and the create an answer with no body.
describe("the world answers every gh call the census found refused", () => {
  const refused = JSON.parse(
    readFileSync(path.join(import.meta.dir, "gh-refused-calls.json"), "utf8")
  ) as {
    batch: string;
    run: string;
    line: number;
    argv: string[];
    input_file?: string;
    input_body?: Record<string, unknown>;
  }[];
  /** The world each scenario case boots (rig.sh's worker_fixture): its function, its guard and its
   * case. The census's threads-closed runs are of the retired text-fixes case, whose approved
   * world with its threads open in `reviewing` is correct's today, so their records are made again
   * there. */
  const WORLD_OF: Record<string, [string, string, string]> = {
    "reviewer-text-findings-text-only": ["review_world", "", ""],
    "reviewer-text-findings-defect": ["review_world", "dropped", ""],
    "threads-closed-text-fixes": ["approved_world", "", "correct"],
    "threads-closed-code-defect": ["approved_world", "dropped", "code-defect"],
  };
  function build(world: string, guard: string, scenarioCase: string): string {
    const dir = freshDir(`gh-refused-${world}`);
    const result = bash(
      [
        fn("fail"),
        ...[
          "fixture_open",
          "fixture_git",
          "role_identity",
          "role_env",
          "fixture_docs_before",
          "fixture_first_commit",
          "fixture_round_files",
          "fixture_head_commit",
          "fixture_implement",
          "fixture_test_handoff",
          "fixture_checks",
          "fixture_body",
          "fixture_write",
          "concluded_checks",
          "base_ruleset_routes",
          "resolution_protection_routes",
          "round_world",
          "fixture_reviewer_commit",
          world,
        ].map(fn),
        'fixture_cli() { printf "export const greet = 1;\\n" >"$src/greet.ts"; : >"$src/greet.test.ts"; }',
        "fixture_workspace() { :; }",
        "R=$1 worker_key=LWEVAL-1 worker_repo=example/widgets worker_base=main",
        assignment("world_author_name"),
        assignment("world_author_email"),
        assignment("world_login"),
        ...WORLD_APPS.map(assignment),
        "text_finding_doc_line=THE-DOC-LINE text_finding_trigger=THE-TRIGGER",
        `${world} 1 100 "$2" "$3"`,
      ].join("\n"),
      [dir, guard, scenarioCase]
    );
    expect(result).toMatchObject({ code: 0, stderr: "" });
    writeGhSelectors(dir);
    return dir;
  }
  function gh(dir: string, args: string[]) {
    const made = spawnChecked(["bun", GH_STANDIN, "gh", ...args], {
      env: { ...childEnv, GH_DATA_DIR: dir },
    });
    return {
      code: made.exitCode,
      out: compact(made.stdout.toString().trim()),
      err: made.stderr.toString(),
    };
  }
  /** `argv` with its `--jq` and the filter after it taken off, which prints the answer itself. */
  const bare = (argv: string[]) =>
    argv.filter((_, index) => argv[index] !== "--jq" && argv[index - 1] !== "--jq");

  test("the file holds the 14 refused calls the check measured", () => {
    expect(refused.map(({ batch, run, line }) => `${batch}/${run}:${line}`)).toHaveLength(14);
  });

  test.each(
    refused.map((call) => [`${call.batch}/${call.run}/calls.jsonl:${call.line}`, call] as const)
  )("%s is answered as GitHub answers it", (_, call) => {
    const [, scenarioCase = ""] = /^(.*)-(?:base|head)-\d+$/.exec(call.run) ?? [];
    const [world, guard, worldCase] = WORLD_OF[scenarioCase] ?? ["", "", ""];
    const dir = build(world, guard, worldCase);
    const head = (
      JSON.parse(readFileSync(path.join(dir, "world.json"), "utf8")) as { head: string }
    ).head;
    const text = call.argv.join(" ");
    const number = /(?:pulls|issues)\/(\d+)|n=(\d+)/.exec(text);
    const recordedPr = number?.[1] ?? number?.[2];
    const recordedHead =
      /[0-9a-f]{40}/.exec(text)?.[0] ?? (call.input_body?.commit_id as string | undefined);
    const here = (value: string) => {
      let out = value;
      if (recordedPr !== undefined)
        out = out.replaceAll(`/${recordedPr}/`, "/1001/").replaceAll(`n=${recordedPr}`, "n=1001");
      if (recordedHead !== undefined) out = out.replaceAll(recordedHead, head);
      return out;
    };
    let argv = call.argv.map(here);
    if (call.input_file !== undefined && call.input_body !== undefined) {
      const input = path.join(dir, "input.json");
      writeFileSync(input, here(JSON.stringify(call.input_body)));
      argv = argv.map((arg) => (arg === call.input_file ? input : arg));
    }
    const got = gh(dir, argv);
    const plain = gh(dir, bare(argv));
    const threads =
      world !== "approved_world"
        ? ["PRRT_kwDOrig0001"]
        : guard === "dropped"
          ? ["PRRT_kwDOfix101"]
          : ["PRRT_kwDOfix002"];
    if (/\/issues\/\d+\/comments/.test(text)) {
      expect(got).toMatchObject({ code: 0, err: "" });
      expect([plain.code, JSON.parse(plain.out)]).toEqual([0, []]);
    } else if (/^repos\/example\/widgets$/.test(call.argv[1] ?? "")) {
      expect(got).toEqual({ code: 0, out: '"main"', err: "" });
      expect(JSON.parse(plain.out)).toMatchObject({
        full_name: "example/widgets",
        default_branch: "main",
      });
    } else if (/\/actions\/runs\?/.test(text)) {
      expect(got).toEqual({ code: 0, out: "", err: "" });
      expect(JSON.parse(plain.out)).toEqual({ total_count: 0, workflow_runs: [] });
    } else if (/\/commits\/[0-9a-f]+\/status/.test(text)) {
      expect(got).toEqual({ code: 0, out: "", err: "" });
      expect(JSON.parse(plain.out)).toMatchObject({
        state: "pending",
        statuses: [],
        total_count: 0,
        sha: head,
      });
    } else if (call.argv[1] === "graphql") {
      expect(got.code).toBe(0);
      expect(got.out.split("\n").map((row) => row.split("\t")[0])).toEqual(threads);
      const pages = JSON.parse(plain.out) as {
        data: { repository: { pullRequest: { reviewThreads: { pageInfo: unknown } } } };
      }[];
      expect(pages.map(({ data }) => data.repository.pullRequest.reviewThreads.pageInfo)).toEqual([
        { hasNextPage: false, endCursor: expect.any(String) },
      ]);
    } else {
      expect(argv.slice(0, 4)).toEqual([
        "api",
        "--method",
        "POST",
        "repos/example/widgets/pulls/1001/reviews",
      ]);
      expect([got.code, JSON.parse(got.out)]).toEqual([
        0,
        { state: "CHANGES_REQUESTED", commit_id: head, body_head: head },
      ]);
    }
  });
});

// Everything the rig writes into a repository an agent of a run reads is the world's, and the
// world says nothing of the rig: the measurement's own words in a world tell the agent what it is
// being measured on. Measured in the batch review 5887659447 audited (finding 2): the heredoc
// writing the world's `.github/workflows/greet.yml` carried the rig's explanation — "The
// scenario's premise is a change carrying text findings … is the text finding the run is there
// to find" — into all 20 reviewer-text-findings transcripts and 15 of the 20 threads-closed
// ones, and agents reasoned from it. The commits carried rig words too (`Fixture`, `rig`, jj's
// `Rig Worker`), and `git log` is something the world writes; so did gh's answers, whose user was
// `rig[bot]`. So the row is over the class and not that one comment: it builds every world rig.sh
// writes a repository for, for real, and reads every file of each repository's tree, its whole
// history (`git log --all -p`, each commit's author, committer and message, and jj's for the
// issue workspace), the hook the rig writes into the bare remote, and every answer the world's
// gh routes serve (`fixtures.json`), with the run's own directory masked, since that path is the
// operator's and no text of the rig's.
describe("what the rig writes into a run's world", () => {
  /** The rig's own vocabulary, which names the measurement and nothing a repository of the
   * world would hold (the architect's ruling of round 15, item b); and the words run-names.ts
   * derives from the rig's scenario table and the label names, with the names the rig gave its
   * world (REMOVED_NAMES), which the ruling of 2026-11-16 took out of everything an agent reads. */
  const RIG_WORDS = /scenario|premise|there to find|fixture|\bthe run\b|\brig\b/i;
  const VOCABULARY = vocabulary(["head", "base"]);
  /** jj's own record of each commit of the issue workspace: author, committer and message. */
  const JJ_COMMITS =
    'author.name() ++ " <" ++ author.email() ++ ">\\n" ++ committer.name() ++ " <" ++ committer.email() ++ ">\\n" ++ description ++ "\\n"';
  /** Every text an agent can read in each of `repositories` under `dir`, by where it is: each
   * file of its tree (`<repository>/<path>`), the whole of its history (`<repository> history`
   * from git, `<repository> jj history` from jj), and, for a bare repository, which has no tree,
   * each hook the rig wrote into it. A command that fails throws, so a repository the build did
   * not make is no repository read as clean. */
  function texts(dir: string, repositories: string[]): Map<string, string> {
    const env = { ...childEnv, GIT_CONFIG_GLOBAL: "/dev/null", JJ_CONFIG: "/dev/null" };
    const read = (cwd: string, ...argv: string[]) => {
      const made = spawnChecked(argv, { cwd, env });
      if (made.exitCode !== 0) throw new Error(`${argv.join(" ")} in ${cwd}: ${made.stderr}`);
      return made.stdout.toString();
    };
    const found = new Map<string, string>();
    const keep = (where: string, text: string) => found.set(where, text.replaceAll(dir, "<run>"));
    for (const repository of repositories) {
      const at = path.join(dir, repository);
      const jj = existsSync(path.join(at, ".jj"));
      const bare = !jj && !existsSync(path.join(at, ".git"));
      if (!jj || existsSync(path.join(at, ".git")))
        keep(
          `${repository} history`,
          read(at, "git", "log", "--all", "-p", "--format=%an <%ae>%n%cn <%ce>%n%B")
        );
      if (jj)
        keep(
          `${repository} jj history`,
          read(
            at,
            "jj",
            "log",
            "--ignore-working-copy",
            "--no-graph",
            "-r",
            "all()",
            "-T",
            JJ_COMMITS
          )
        );
      const files = bare
        ? readdirSync(path.join(at, "hooks"))
            .filter((hook) => !hook.endsWith(".sample"))
            .map((hook) => path.join("hooks", hook))
        : readdirSync(at, { recursive: true, encoding: "utf8" }).filter(
            (name) => ![".git", ".jj"].includes(name.split(path.sep)[0] ?? "")
          );
      for (const name of files)
        if (statSync(path.join(at, name)).isFile())
          keep(`${repository}/${name}`, readFileSync(path.join(at, name), "utf8"));
    }
    return found;
  }
  /** WORLD built for real under a run directory of its own, with rig.sh's own functions and
   * values: no step stubbed, the issue workspace cloned with jj, and fixture_cli read whole
   * (`definition`). `worldCase` is the case the world's function builds (worker_fixture passes
   * it). */
  function builtWorld(
    world: string,
    guard: string,
    worldCase = guard === "" ? "correct" : "code-defect"
  ): string {
    const dir = freshDir(`world-words-${world}`);
    const values = [
      "worker_repo",
      "worker_base",
      "world_author_name",
      "world_author_email",
      "world_login",
      ...WORLD_APPS,
      "world_remote_vcs",
    ];
    const result = bash(
      [
        ...[
          "fail",
          "fixture_open",
          "fixture_git",
          "fixture_docs_before",
          "fixture_first_commit",
          "fixture_round_files",
          "fixture_head_commit",
          "fixture_implement",
          "fixture_test_handoff",
          "fixture_workspace",
          "workspace_form",
          "world_remote",
          "world_remote_app",
          "role_identity",
          "role_env",
          "fixture_checks",
          "fixture_body",
          "fixture_write",
          "concluded_checks",
          "base_ruleset_routes",
          "resolution_protection_routes",
          "round_world",
          "fixture_reviewer_commit",
          world,
        ].map(fn),
        definition("fixture_cli"),
        ...[...values, "text_finding_doc_line", "text_finding_trigger"].map(assignment),
        `R=$1 worker_key=${WORLD_PROJECT}-1`,
        `${world} 1 100 "$2" "$3"`,
      ].join("\n"),
      [dir, guard, worldCase]
    );
    expect(result).toMatchObject({ code: 0, stderr: "" });
    return dir;
  }

  // Catches: a sentence of the rig's in any file a world writes into a repository — the greet.yml
  // comment that told every agent what it was there to find — a rig word in a commit's identity
  // or message, which `git log` and `jj log` show the agent (the worker world's `Fixture`, the
  // brainstorm repository's `rig` and the issue workspace's `Rig Worker`), and one in any answer
  // the world's gh routes serve, which the agent reads as GitHub's: the world's user was
  // `rig[bot]`, in `gh api user` and in each review the run created. last-push's world is read
  // too: its base's rules answer `require_last_push_approval: true`, an identifier that holds the
  // case `last-push` and names no word of the rig's (run-names.ts's `besideWord`, the architect's
  // ruling D6 of INFRA-764's plan v9), as every other world's `false` does.
  test("no file, commit, commit identity or gh answer of a run's world holds a word of the rig's", () => {
    const read = new Map<string, string>();
    for (const [world, guard, worldCase] of [
      ["tester_proof_world", "", undefined],
      ["review_world", "", undefined],
      ["review_world", "", "last-push"],
      ["review_world", "dropped", undefined],
      ["approved_world", "", undefined],
      ["approved_world", "dropped", undefined],
    ] as const) {
      const name = `${world}${guard === "" ? "" : ` ${guard}`}${worldCase === undefined ? "" : ` ${worldCase}`}`;
      const dir = builtWorld(world, guard, worldCase);
      const clone = `repos/github.com/${WORLD_REPO}`;
      for (const [where, text] of texts(dir, ["src", "remote.git", "ws", clone]))
        read.set(`${name}: ${where}`, text);
      // The issue workspace's remote helper and the leak check's (world_remote), which an agent
      // can list and read on its PATH and in the run's directory.
      for (const file of [`bin/git-remote-${WORLD_REMOTE_VCS}`, "lib/git-core/git-remote-https"])
        read.set(
          `${name}: ${file}`,
          readFileSync(path.join(dir, file), "utf8").replaceAll(dir, "<run>")
        );
      const { routes } = JSON.parse(readFileSync(path.join(dir, "fixtures.json"), "utf8")) as {
        routes: { match: string; stdout?: unknown; graph?: unknown; stderr?: string }[];
      };
      // A route's whole answer: what it prints, the GraphQL graph it answers from, and its
      // refusal.
      for (const [index, route] of routes.entries())
        read.set(
          `${name}: route ${index} (${route.match})`,
          `${JSON.stringify(route.stdout ?? "")}\n${JSON.stringify(route.graph ?? "")}\n${route.stderr ?? ""}`.replaceAll(
            dir,
            "<run>"
          )
        );
    }
    const brainstorm = freshDir("world-words-brainstorm");
    mkdirSync(path.join(brainstorm, "cwd"));
    const built = bash(
      [
        definition("run_brainstorm"),
        "own_dispatch() { :; }",
        "dispatch_session() { :; }",
        assignment("world_author_name"),
        assignment("world_author_email"),
        "R=$1",
        "run_brainstorm brainstorm-1",
      ].join("\n"),
      [brainstorm]
    );
    expect(built).toMatchObject({ code: 0, stderr: "" });
    for (const [where, text] of texts(brainstorm, ["cwd"]))
      read.set(`run_brainstorm: ${where}`, text);
    // What was read: every repository of each world, its tree, its history and the remote's hook,
    // and the world's remote helpers, so a build that left one out is no repository read as clean.
    expect([...read.keys()]).toEqual(
      expect.arrayContaining([
        "approved_world: src/.github/workflows/greet.yml",
        "approved_world: src/docs/cli.md",
        "approved_world: src/greet.ts",
        "approved_world: src history",
        "approved_world: remote.git history",
        "approved_world: remote.git/hooks/post-receive",
        "approved_world: ws/.github/workflows/greet.yml",
        "approved_world: ws jj history",
        `approved_world: repos/github.com/${WORLD_REPO} history`,
        `approved_world: repos/github.com/${WORLD_REPO} jj history`,
        `approved_world: bin/git-remote-${WORLD_REMOTE_VCS}`,
        "approved_world: lib/git-core/git-remote-https",
        "review_world dropped: ws/docs/cli.md",
        "tester_proof_world: ws/greet.test.ts",
        "run_brainstorm: cwd/todo.py",
        "run_brainstorm: cwd/README.md",
        "run_brainstorm: cwd history",
      ])
    );
    // And the gh answers: every world's routes, `gh api user` (GitHub's 403 to a role App, its
    // route `^gh api (.* )?/?user…`) among them, so a world whose fixtures.json the walk did not
    // read is no answer read as clean.
    for (const world of [
      "tester_proof_world",
      "review_world",
      "review_world last-push",
      "approved_world dropped",
    ])
      expect([...read.keys()]).toEqual(
        expect.arrayContaining([
          expect.stringMatching(new RegExp(`^${world}: route \\d+ .*api \\(\\.\\* \\)\\?/\\?user`)),
        ])
      );
    // last-push's base rules, which the walk read, answer the identifier that holds its case.
    expect(
      [...read].some(
        ([where, text]) =>
          where.startsWith("review_world last-push: route ") &&
          text.includes('"require_last_push_approval":true')
      )
    ).toBe(true);
    for (const [where, text] of read)
      if (where.endsWith("history"))
        expect([where, text]).toEqual([where, expect.stringMatching(/@example\.invalid>/)]);
    const words = [...read].flatMap(([where, text]) => {
      const word = RIG_WORDS.exec(text);
      const derived = textWords(text, VOCABULARY);
      return [
        ...(word === null
          ? []
          : [
              `${where}: "${word[0]}" in "${text.slice(Math.max(0, word.index - 40), word.index + 40)}"`,
            ]),
        ...(derived === undefined ? [] : [`${where}: "${derived}" (the derived vocabulary)`]),
      ];
    });
    expect(words).toEqual([]);
    // It builds six worlds for real: the file ran five in 37 s alone on a quiet pod, and past the
    // file's 60 s default beside a second suite (INFRA-764, 2026-11-19), so it is bounded at three
    // minutes.
  }, 180_000);
});

// The issue workspace's remote and its configuration, as an agent of the run meets them: in a world
// built for real with rig.sh's own functions (tester_proof_world, with fixture_workspace and
// world_remote unstubbed), its stand-ins (standins), the gh selectors worker_boot writes and the
// push guard's line push_guard prints, read under the pane an agent gets: the run's bin first on
// PATH, the pane's own HOME and configuration homes, production's five git pairs (git_pairs) and
// the role's identity. The architect's ruling of 2026-11-18 (INFRA-764): `git remote -v` shows
// GitHub's URL while every fetch and push reaches the world's own remote, the clone's
// configuration is a production clone's but for remote.origin.vcs, and `.git` is the gitdir file a
// pod's `jj workspace add` writes.
describe("the issue workspace's remote", () => {
  const ORIGIN = `https://github.com/${WORLD_REPO}`;
  /** What the tester read in a Legion issue pod's workspace on 2026-11-17 (`git config -l
   * --show-scope`), its repository written as the world's: the clone's own keys, then the five
   * GIT_CONFIG pairs of the pod's runtime. */
  const PRODUCTION_CONFIG = [
    "local\tcore.repositoryformatversion=0",
    "local\tcore.filemode=true",
    "local\tcore.bare=false",
    "local\tcore.logallrefupdates=true",
    "local\tcore.warnambiguousrefs=false",
    `local\tremote.origin.url=${ORIGIN}`,
    "local\tremote.origin.fetch=+refs/heads/*:refs/remotes/origin/*",
    "local\tbranch.main.remote=origin",
    "local\tbranch.main.merge=refs/heads/main",
    "local\tcredential.helper=",
    "local\tcredential.helper=!legion git-credential",
    "local\tcredential.interactive=false",
    "local\tcredential.usehttppath=true",
    "local\tcredential.https://github.com.helper=",
    "local\tcredential.https://github.com.helper=!legion git-credential",
    "command\tcredential.https://github.com.helper=",
    "command\tcredential.https://github.com.helper=!legion git-credential",
    "command\tcredential.https://github.com.usehttppath=true",
    "command\tcore.fsmonitor=",
    "command\tcore.hookspath=/dev/null",
  ];
  /** The one key production's clone has not got, which the README names: the world's helper. */
  const RESIDUAL = `local\tremote.origin.vcs=${WORLD_REMOTE_VCS}`;

  /** A world under a run directory of its own, as cmd_run builds one before its first agent boots,
   * with `env` over the caller's environment; its directory. `builder` is the world function:
   * tester_proof_world, or review_world, the text-only round whose head carries the workflow
   * .github/workflows/greet.yml. */
  function world(
    name: string,
    env: Record<string, string> = {},
    builder: "tester_proof_world" | "review_world" = "tester_proof_world"
  ): string {
    const R = freshDir(name);
    const result = bash(
      [
        ...[
          "fail",
          "fixture_open",
          "fixture_git",
          "fixture_first_commit",
          "fixture_head_commit",
          "fixture_implement",
          "fixture_workspace",
          "workspace_form",
          "world_remote",
          "world_remote_app",
          "role_identity",
          "role_env",
          "fixture_checks",
          "fixture_body",
          "fixture_write",
          "concluded_checks",
          "base_ruleset_routes",
          "tester_proof_world",
          "fixture_docs_before",
          "fixture_round_files",
          "fixture_test_handoff",
          "resolution_protection_routes",
          "round_world",
          "review_world",
          "standins",
          "push_guard",
        ].map(fn),
        definition("fixture_cli"),
        ...[
          "worker_repo",
          "worker_base",
          "world_author_name",
          "world_author_email",
          "world_login",
          ...WORLD_APPS,
          "world_remote_vcs",
          "text_finding_doc_line",
          "text_finding_trigger",
        ].map(assignment),
        `R=$1 here=$2 worker_key=${WORLD_PROJECT}-1`,
        `${builder} 1 100 ''`,
        "standins",
        'push_guard >"$R/guard.env"',
      ].join("\n"),
      [R, import.meta.dir],
      env
    );
    expect(result).toMatchObject({ code: 0, stderr: "" });
    writeGhSelectors(R);
    return R;
  }
  /** `NAME=value` lines as the variables `env -i` makes of them. */
  const variables = (lines: string) =>
    Object.fromEntries(
      lines
        .split("\n")
        .filter((line) => line.includes("="))
        .map((line) => [line.slice(0, line.indexOf("=")), line.slice(line.indexOf("=") + 1)])
    );
  /** The variables of the pane an implementer's agent gets in R's issue workspace, with `over`
   * over them. */
  function paneOf(R: string, over: Record<string, string> = {}) {
    const role = bash(
      `${fn("role_identity")}\n${fn("role_env")}\n${WORLD_APPS.map(assignment).join("\n")}\nrole_env implementer`
    );
    mkdirSync(path.join(R, "home"), { recursive: true });
    writeFileSync(path.join(R, "jj.toml"), '[ui]\npaginate = "never"\n');
    return {
      ...childEnv,
      HOME: path.join(R, "home"),
      PATH: `${path.join(R, "bin")}:${childEnv.PATH}`,
      XDG_CONFIG_HOME: path.join(R, "state", "home", ".config"),
      XDG_STATE_HOME: path.join(R, "state", "home", ".local", "state"),
      PUBLIC_LEAK_CHECK: undefined,
      ...variables(readFileSync(path.join(R, "guard.env"), "utf8")),
      ...variables(bash(`${fn("git_pairs")}\ngit_pairs`).stdout),
      ...variables(role.stdout),
      LEGION_ROLE: "implementer",
      GH_REPO: WORLD_REPO,
      GIT_TERMINAL_PROMPT: "0",
      JJ_CONFIG: path.join(R, "jj.toml"),
      ...over,
    };
  }
  /** COMMAND run by bash in R's issue workspace under the pane an implementer's agent gets there,
   * with `over` over it: its exit, its stdout and its stderr. */
  function inWorkspace(R: string, command: string, over: Record<string, string> = {}) {
    const ran = spawnChecked(["bash", "-c", command], {
      cwd: path.join(R, "ws"),
      env: paneOf(R, over),
    });
    return { code: ran.exitCode, out: ran.stdout.toString(), err: ran.stderr.toString() };
  }
  /** COMMAND run as inWorkspace runs it, inside the namespace launch.sh starts the agent in. */
  function inNamespace(R: string, command: string) {
    const ran = namespaced(R, paneOf(R), path.join(R, "ws"), ["bash", "-c", command]);
    return { code: ran.code, out: ran.stdout, err: ran.stderr };
  }
  /** ARGV run at CWD under `env -i` with ENV alone, inside the namespace launch.sh starts an agent
   * of the run R in: R's world as world_paths declares it for a pane of ENV's PATH, written to
   * R/namespace.txt as launch writes it, and mounted by private_world. */
  function namespaced(
    R: string,
    env: Record<string, string | undefined>,
    cwd: string,
    argv: string[]
  ) {
    writeFileSync(path.join(R, "pane.env"), `PATH=${env.PATH}\n`);
    mkdirSync(path.join(R, "tmp"), { recursive: true });
    const vars = Object.entries(env).flatMap(([name, value]) =>
      value === undefined ? [] : [`${name}=${value}`]
    );
    return bash(
      [
        fn("pane_var"),
        fn("system_gitconfig"),
        fn("world_paths"),
        assignment("namespace_path"),
        quotedAssignment("private_world"),
        "R=$1 cwd=$2",
        'world_paths "$R" "$3" "$4" "$R/pane.env" >"$R/namespace.txt"',
        "shift 4",
        'exec env -i PATH="$namespace_path" unshare -Urm -p --fork --mount-proc sh -c "$private_world" - "$(id -u)" "$(id -g)" "$R" "$cwd" env -i "$@"',
      ].join("\n"),
      [R, cwd, freshDir("namespace-work"), freshDir("namespace-install"), ...vars, ...argv]
    );
  }
  /** Which system scope the rows read the agent's git by: the namespace's, where the host makes
   * one, which shows the host git's system file as the host holds it, as a pod's world shows the
   * pod's (world_paths); the host's own file where it makes none (a GitHub runner), which the
   * agent's git there would read, since no agent starts without the namespace. */
  const SYSTEM_SCOPE = withoutNamespace
    ? "the host's system file, since this host makes no namespace"
    : "the agent's namespace, which shows the host's system file";
  /** The system scope's lines the agent's git reads under R's pane, in the namespace and outside
   * it alike: the host's own file's, each `system\t<key>=<value>`, and none where the host has
   * none. On a deployment's pod they are production's: INFRA's worker image of 2026-11-19 lists
   * `system\tcore.packedgitwindowsize=32m` and `system\tcore.packedgitlimit=256m`. */
  function systemScope(R: string): string[] {
    const read = spawnChecked(["git", "config", "--show-scope", "--system", "--list"], {
      env: { PATH: paneOf(R).PATH },
    });
    return read.exitCode === 0 ? read.stdout.toString().split("\n").filter(Boolean) : [];
  }
  /** The pushes the world's remote logged (its post-receive's pushes.log), one `<ref> <old> <new>`
   * a push, of the refs `kept` names. */
  const pushed = (R: string, kept: RegExp) =>
    readFileSync(path.join(R, "pushes.log"), "utf8")
      .split("\n")
      .filter((line) => kept.test(line))
      .map((line) => line.split(" ").slice(1).join(" "));
  /** `git <args>` of the world's bare remote, outside any pane: what it holds. */
  const remote = (R: string, ...args: string[]) =>
    spawnChecked(["/usr/bin/git", "-C", path.join(R, "remote.git"), ...args])
      .stdout.toString()
      .trim();
  /** A commit on the remote's default branch made outside the world's panes, as GitHub's merge of
   * another developer's pull request moves it: its id. The remote refuses every push to its base,
   * as production's ruleset does (rig.sh's fixture_first_commit), so the commit is written into
   * the remote's base by the remote itself, as GitHub writes a merge, and no push hook runs. */
  function advanceBase(R: string, line: string): string {
    const src = path.join(R, "src");
    const git = (...args: string[]) =>
      spawnChecked(
        [
          "/usr/bin/git",
          "-c",
          "user.name=A",
          "-c",
          "user.email=a@example.invalid",
          "-C",
          src,
          ...args,
        ],
        { env: { ...childEnv, GIT_CONFIG_GLOBAL: "/dev/null" } }
      );
    expect(git("checkout", "-q", "main").exitCode).toBe(0);
    writeFileSync(path.join(src, "README.md"), `${line}\n`, { flag: "a" });
    expect(git("commit", "-qam", line).exitCode).toBe(0);
    expect(
      spawnChecked(
        ["/usr/bin/git", "-C", path.join(R, "remote.git"), "fetch", "-q", src, "main:main"],
        { env: { ...childEnv, GIT_CONFIG_GLOBAL: "/dev/null" } }
      ).exitCode
    ).toBe(0);
    return git("rev-parse", "HEAD").stdout.toString().trim();
  }

  // Catches: an origin that names the run's own bare remote, as `jj git clone <run>/remote.git`
  // left it, which told an agent that read `git remote -v` its GitHub was a directory of this
  // machine; and an origin routed by url.<local>.insteadOf, which git and jj both print
  // rewritten to the local path (measured on 2026-11-17).
  test("git remote -v and jj git remote list print GitHub's URL", () => {
    const R = world("remote-origin");
    expect(inWorkspace(R, "git remote -v")).toEqual({
      code: 0,
      out: `origin\t${ORIGIN} (fetch)\norigin\t${ORIGIN} (push)\n`,
      err: "",
    });
    expect(inWorkspace(R, "jj git remote list")).toMatchObject({
      code: 0,
      out: `origin ${ORIGIN}\n`,
    });
  });

  // Catches: a clone whose configuration is not a production clone's: the two keys `jj git
  // clone` wrote (core.ignorecase, core.precomposeunicode), the ones it left out
  // (core.warnambiguousrefs, branch.main.*, every credential line), a pane with no command-scope
  // pairs (GIT_CONFIG_COUNT=0, as it was), a key out of production's order or twice, and a key
  // the world's transport needs in a scope the agent reads (protocol.ext.allow,
  // url.*.insteadOf). The tester compares by scope, key and count with one residual; this holds
  // the values and the order too. And a system scope the agent's git reads that is not the
  // host's own: the empty file world_paths' old `empty` kind bound over the host's, which hid the
  // two lines of INFRA's worker image of 2026-11-19 (core.packedgitwindowsize=32m and
  // core.packedgitlimit=256m) where its panes read them; and the two lines git 2.48 and later
  // write for jj's relative worktree, core.repositoryformatversion=1 and
  // extensions.relativeworktrees=true (fixture_workspace's repair). The local and command scopes
  // are held to production's lines everywhere, and the system scope to the host's own file's,
  // in the namespace and on a host that makes none (a GitHub runner, whose file is that runner's
  // and no pod's), which the row's name says.
  test(`git config -l --show-scope is a production clone's, key for key, but remote.origin.vcs, its system scope read in ${SYSTEM_SCOPE}`, () => {
    const R = world("remote-config");
    const fetch = PRODUCTION_CONFIG.indexOf(
      "local\tremote.origin.fetch=+refs/heads/*:refs/remotes/origin/*"
    );
    const expected = [
      ...systemScope(R),
      ...PRODUCTION_CONFIG.slice(0, fetch + 1),
      RESIDUAL,
      ...PRODUCTION_CONFIG.slice(fetch + 1),
    ];
    const read = withoutNamespace ? inWorkspace : inNamespace;
    expect({ system: SYSTEM_SCOPE, ...read(R, "git config -l --show-scope") }).toEqual({
      system: SYSTEM_SCOPE,
      code: 0,
      out: `${expected.join("\n")}\n`,
      err: "",
    });
    expect(inWorkspace(R, "git config --get-regexp remote")).toEqual({
      code: 0,
      out: `remote.origin.url ${ORIGIN}\nremote.origin.fetch +refs/heads/*:refs/remotes/origin/*\nremote.origin.vcs ${WORLD_REMOTE_VCS}\nbranch.main.remote origin\n`,
      err: "",
    });
  });

  // Catches: a workspace whose .git is a repository of its own (a clone, as `jj git clone` made
  // it) where a pod's is the gitdir file `jj workspace add` writes into a shared clone's
  // worktrees, which `cat .git` and `cat .git/config` read otherwise; a worktree left unlocked,
  // which provisioning locks so that a `git worktree prune` that cannot see it skips it
  // (packages/daemon/internal/workspace's lockGitWorktree); and either link relative, as git 2.48
  // and later write them for jj (`gitdir: ../repos/…` at CI's 427b02d4), where a pod's git 2.47.3
  // writes both absolute.
  test(".git is the gitdir file of a locked worktree of the shared clone", () => {
    const R = world("remote-gitdir");
    const clone = path.join(R, "repos", "github.com", WORLD_REPO);
    expect(lstatSync(path.join(R, "ws", ".git")).isFile()).toBe(true);
    expect(inWorkspace(R, "cat .git")).toEqual({
      code: 0,
      out: `gitdir: ${clone}/.git/worktrees/ws\n`,
      err: "",
    });
    expect(readFileSync(path.join(clone, ".git", "worktrees", "ws", "gitdir"), "utf8")).toBe(
      `${path.join(R, "ws")}/.git\n`
    );
    expect(inWorkspace(R, "cat .git/config").code).not.toBe(0);
    expect(inWorkspace(R, "git worktree list --porcelain").out).toContain(
      `worktree ${path.join(R, "ws")}\nHEAD ${remote(R, "rev-parse", `legion/${WORLD_PROJECT}-1`)}\ndetached\nlocked legion workspace: its directory may be invisible to other processes sharing this clone\n`
    );
  });

  // Catches: a build that goes on with a workspace in any form but a pod's, which a git of 2.48
  // or later leaves where its repair is missing (fixture_workspace): both links relative,
  // core.repositoryformatversion=1 and extensions.relativeworktrees=true, as CI's git 2.56.1
  // wrote them at 427b02d4 and git 2.55.0 did here. A host's git 2.47.3 writes none of it, so the
  // row writes that form over a world by hand, each file as git 2.55.0 wrote it.
  test("workspace_form names each way a workspace is not in a pod's form, and nothing for a pod's", () => {
    const R = world("remote-form");
    const clone = path.join(R, "repos", "github.com", WORLD_REPO);
    const entry = path.join(clone, ".git", "worktrees", "ws");
    const form = () =>
      bash(`${fn("workspace_form")}\nworkspace_form "$1" "$2"`, [clone, path.join(R, "ws")]);
    expect(form()).toEqual({ code: 0, stdout: "", stderr: "" });
    writeFileSync(
      path.join(R, "ws", ".git"),
      `gitdir: ../repos/github.com/${WORLD_REPO}/.git/worktrees/ws\n`
    );
    writeFileSync(path.join(entry, "gitdir"), "../../../../../../../ws/.git\n");
    const config = path.join(clone, ".git", "config");
    for (const [key, value] of [
      ["core.repositoryformatversion", "1"],
      ["extensions.relativeWorktrees", "true"],
    ])
      expect(spawnChecked(["/usr/bin/git", "config", "--file", config, key, value]).exitCode).toBe(
        0
      );
    expect(form()).toEqual({
      code: 0,
      stdout: [
        `${R}/ws/.git reads 'gitdir: ../repos/github.com/${WORLD_REPO}/.git/worktrees/ws', not 'gitdir: ${entry}'`,
        `${entry}/gitdir reads '../../../../../../../ws/.git', not '${R}/ws/.git'`,
        `${config} holds core.repositoryformatversion=1, not 0`,
        `${config} holds an extension, extensions.relativeworktrees true`,
        "",
      ].join("\n"),
      stderr: "",
    });
  });

  /** A directory holding a `git` that names FILE as its system configuration, as `git config
   * --system --edit` names /etc/gitconfig to its editor, or names none and fails where FILE is
   * undefined, and runs the real git for every other call. */
  function systemGit(file: string | undefined): string {
    const bin = freshDir("system-git");
    writeFileSync(
      path.join(bin, "git"),
      `#!/bin/sh\n[ "$*" != "config --system --edit" ] || ${file === undefined ? "exit 1" : `exec $GIT_EDITOR '${file}'`}\nexec /usr/bin/git "$@"\n`,
      { mode: 0o755 }
    );
    return bin;
  }
  /** Two lines of a system configuration, those INFRA's worker image carries in /etc/gitconfig
   * (2026-11-19), as that file writes them. */
  const SYSTEM_TEXT = "[core]\n\tpackedGitWindowSize = 32m\n\tpackedGitLimit = 256m\n";

  // Catches: a run's world that declares the pane git's system configuration other than as the
  // host holds it: shown empty (world_paths' old `empty` kind, an empty file bound over it), which
  // hid the two lines INFRA's worker image of 2026-11-19 carries where its panes read them; a
  // file holding nothing left out, which the host's git reads as it is; and a file the host has
  // not got declared.
  test("world_paths shows the pane git's system configuration read-only where the host has the file, and declares none where it has none", () => {
    const R = freshDir("system-gitconfig-world");
    const file = path.join(R, "gitconfig");
    writeFileSync(path.join(R, "pane.env"), `PATH=${systemGit(file)}:${childEnv.PATH}\n`);
    const declared = (text: string | undefined) => {
      if (text === undefined) rmSync(file, { force: true });
      else writeFileSync(file, text);
      const shown = bash(
        [
          fn("pane_var"),
          fn("system_gitconfig"),
          fn("world_paths"),
          'world_paths "$1" "$2" "$3" "$1/pane.env"',
        ].join("\n"),
        [R, freshDir("system-gitconfig-work"), freshDir("system-gitconfig-install")]
      );
      expect({ code: shown.code, stderr: shown.stderr }).toEqual({ code: 0, stderr: "" });
      return shown.stdout
        .split("\n")
        .filter((line) => line.endsWith(` ${file}`) || line.startsWith("empty "));
    };
    expect(declared(SYSTEM_TEXT)).toEqual([`ro ${file}`]);
    expect(declared("")).toEqual([`ro ${file}`]);
    expect(declared(undefined)).toEqual([]);
  });

  // Catches: a namespace that shows the agent's git another system configuration than the
  // host's: its file hidden, shown empty (world_paths' old `empty` kind) or bound over by
  // anything, which hid INFRA's two lines from the agents of a pod whose panes read them; and a
  // namespace that mounts onto the file at all, which a gVisor pod refuses (`mount point is not a
  // directory`), so that pod refused every run.
  test.skipIf(withoutNamespace)(
    "the namespace shows that file as the host holds it, with no mount onto it, and the agent's git reads the host's system scope",
    () => {
      const R = freshDir("system-gitconfig-run");
      const file = path.join(R, "gitconfig");
      writeFileSync(file, SYSTEM_TEXT);
      const ran = namespaced(R, { PATH: `${systemGit(file)}:${childEnv.PATH}` }, R, [
        "sh",
        "-c",
        'cat "$1" && awk -v file="$1" \'$5 == file\' /proc/self/mountinfo',
        "-",
        file,
      ]);
      expect({ code: ran.code, stdout: ran.stdout }).toEqual({ code: 0, stdout: SYSTEM_TEXT });
      expect(readFileSync(file, "utf8")).toBe(SYSTEM_TEXT);
      // The host's own git, whose system file is the host's (/etc/gitconfig on a pod), read
      // outside any namespace and inside one.
      const scope = ["git", "config", "--show-scope", "--system", "--list"];
      const host = spawnChecked(scope, { env: { PATH: childEnv.PATH } });
      const agent = freshDir("system-gitconfig-scope");
      const inside = namespaced(agent, { PATH: childEnv.PATH }, agent, scope);
      expect({ code: inside.code, stdout: inside.stdout }).toEqual({
        code: host.exitCode,
        stdout: host.stdout.toString(),
      });
    }
  );

  // Catches: a probe that refuses a host every run's namespace starts on: one that mounts onto
  // its caller git's system configuration where the host has one, which a gVisor pod refuses
  // (`mount point is not a directory`) and private_world never mounts, so that pod's every row of
  // the namespace skipped and `rig.sh batch` and `run` refused every launch. A host that refuses
  // the namespace itself (a GitHub runner) refuses it whatever the file holds.
  test("namespace_probe answers the same whatever its caller git's system configuration holds", () => {
    const dir = freshDir("system-gitconfig-probe");
    const file = path.join(dir, "gitconfig");
    const none = namespaceProbe({ PATH: `${systemGit(undefined)}:${childEnv.PATH}` });
    for (const text of ["", SYSTEM_TEXT]) {
      writeFileSync(file, text);
      expect(namespaceProbe({ PATH: `${systemGit(file)}:${childEnv.PATH}` })).toEqual(none);
      expect(readFileSync(file, "utf8")).toBe(text);
    }
  });

  // Catches: a run's provenance that does not record the system git configuration its world
  // shows the agents' git, so a batch whose arms read two could not be told from one that read
  // one (health.ts's rule 23); a digest recorded for a file the host has not got; and a file
  // recorded where the git names none.
  test("each run's provenance records its system git configuration's digest, and none where the host has none", () => {
    const dir = freshDir("system-gitconfig-provenance");
    const file = path.join(dir, "gitconfig");
    const recorded = (bin: string) => {
      const result = bash(
        [
          `. ${JSON.stringify(path.join(root, "packages/envoy/scripts/lib/pg-url.sh"))}`,
          ...["fail", "given_services", "safe_url", "file_digest", "system_gitconfig"].map(fn),
          fn("services_from"),
          "pinned_omp() { echo /x/omp; }",
          `here=${import.meta.dir} co=/co pin=omp@pin I=/w/installs/0a1b2c3d4e5f profile=legion`,
          "nats_url=nats://127.0.0.1:4222 pg_url=postgres://127.0.0.1:5432/postgres",
          "services_from /l /d /p",
        ].join("\n"),
        [],
        {
          PATH: `${bin}:${childEnv.PATH}`,
          SKILL_SCENARIOS_POSTGRES_URL: "postgres://127.0.0.1:5432/postgres",
          SKILL_SCENARIOS_NATS_URL: "nats://127.0.0.1:4222",
          PUBLIC_LEAK_CHECK: undefined,
        }
      );
      expect({ code: result.code, stderr: result.stderr }).toEqual({ code: 0, stderr: "" });
      return result.stdout.split("\n").filter((line) => line.startsWith("system-gitconfig "));
    };
    const named = systemGit(file);
    writeFileSync(file, SYSTEM_TEXT);
    const digest = new Bun.CryptoHasher("sha256").update(SYSTEM_TEXT).digest("hex");
    expect(recorded(named)).toEqual([`system-gitconfig shared ${file} sha256:${digest}`]);
    rmSync(file);
    expect(recorded(named)).toEqual([`system-gitconfig shared ${file} none`]);
    expect(recorded(systemGit(undefined))).toEqual(["system-gitconfig shared none none"]);
  });

  // Catches: a workspace missing what a pod's provisioning writes beside the clone and the
  // worktree (packages/daemon/internal/workspace): the record of when it made the workspace
  // (RecordCreated's .jj/legion-created, an RFC 3339 instant in UTC as Go's RFC3339Nano prints
  // it), the `.codegraph/` line of the clone's .git/info/exclude (excludeCodegraphDirectory), and
  // the repository's jj configuration as the agents' jj reads it, `trunk()` and unreachable
  // commits kept (ensureFetchConfiguration), which the build set where no agent's jj reads it.
  test("the workspace carries the creation record, the codegraph exclude and the jj configuration a pod's does", () => {
    const before = Date.now();
    const R = world("remote-provisioned");
    const created = readFileSync(path.join(R, "ws", ".jj", "legion-created"), "utf8");
    expect(created).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d*[1-9])?Z\n$/);
    const at = Date.parse(created.trim().replace(/(\.\d{1,3})\d*Z$/, "$1Z"));
    expect(at >= Math.floor(before / 1000) * 1000 && at <= Date.now()).toBe(true);
    const exclude = readFileSync(
      path.join(R, "repos", "github.com", WORLD_REPO, ".git", "info", "exclude"),
      "utf8"
    );
    expect(exclude.split("\n").filter((line) => line === ".codegraph/")).toEqual([".codegraph/"]);
    expect(exclude.endsWith("\n.codegraph/\n")).toBe(true);
    expect(inWorkspace(R, "jj config list --repo")).toEqual({
      code: 0,
      out: 'revset-aliases."trunk()" = "main@origin"\ngit.abandon-unreachable-commits = false\n',
      err: "",
    });
  });

  // Catches: a world whose jj kept the repository's configuration in the operator's
  // configuration home (jj 0.45's `$XDG_CONFIG_HOME/jj/repos/<config-id>/`), so the agents'
  // first jj command printed `Per-repo config not found. Generating an empty one.`, which a pod's
  // roles never see (all 35 runs recorded on 2026-11-17 showed it), and wrote into the operator's
  // directory at every build and every adoption of the workspace.
  test("the agents' first jj command prints nothing of a missing configuration, and the build writes none of the operator's", () => {
    const operator = freshDir("remote-operator-config");
    const R = world("remote-jj-config", { XDG_CONFIG_HOME: operator });
    writeFileSync(path.join(R, "jj.toml"), '[ui]\npaginate = "never"\n');
    const adopted = bash(
      [fn("role_identity"), fn("role_env"), fn("adopt_workspace"), ...WORLD_APPS.map(assignment)]
        .concat(["R=$1", "adopt_workspace implementer"])
        .join("\n"),
      [R],
      { XDG_CONFIG_HOME: operator }
    );
    expect(adopted).toMatchObject({ code: 0, stderr: "" });
    expect(readdirSync(operator)).toEqual([]);
    expect(inWorkspace(R, "jj log -r @- --no-graph -T commit_id")).toEqual({
      code: 0,
      out: remote(R, "rev-parse", `legion/${WORLD_PROJECT}-1`),
      err: "",
    });
  });

  // Catches: a fetch or a push that reaches anything but the world's remote, or that the remote
  // does not take as a push: the helper that let the pane's GIT_CONFIG pairs reach
  // git-receive-pack, where core.hooksPath=/dev/null kept the remote's post-receive from running
  // and pushes.log stayed empty (measured on 2026-11-18); a push that prints the local path where
  // production's prints `To https://github.com/<owner>/<repo>`; and a new bookmark's jj push or a
  // jj fetch that does not land.
  test("a jj and a git push reach the world's remote and print production's lines, and so do a jj and a git fetch", () => {
    const R = world("remote-push");
    const jjPush = inWorkspace(
      R,
      'printf "a note\\n" >notes.md && jj commit -m "docs: a note" >/dev/null 2>&1 && jj bookmark create notes -r @- >/dev/null 2>&1 && jj git push --bookmark notes'
    );
    const notes = remote(R, "rev-parse", "refs/heads/notes");
    expect(jjPush.code).toBe(0);
    expect(jjPush.err).toContain(
      `Changes to push to origin:\n  bookmark: notes [add to ${notes.slice(0, 12)}]\n`
    );
    expect(pushed(R, / refs\/heads\/notes /)).toEqual([
      `refs/heads/notes ${"0".repeat(40)} ${notes}`,
    ]);
    const gitPush = inWorkspace(R, "git push origin HEAD:refs/heads/notes-git");
    expect(gitPush.code).toBe(0);
    expect(gitPush.err).toContain(`To ${ORIGIN}\n * [new branch]      HEAD -> notes-git\n`);
    expect(pushed(R, / refs\/heads\/notes-git /)).toEqual([
      `refs/heads/notes-git ${"0".repeat(40)} ${notes}`,
    ]);
    const first = advanceBase(R, "A first change of the base.");
    const jjFetch = inWorkspace(R, "jj git fetch");
    expect(jjFetch.code).toBe(0);
    expect(jjFetch.err).toContain("bookmark: main@origin [updated] tracked\n");
    expect(inWorkspace(R, "jj log -r main@origin --no-graph -T commit_id").out).toBe(first);
    const second = advanceBase(R, "A second change of the base.");
    const gitFetch = inWorkspace(R, "git fetch origin");
    expect(gitFetch.code).toBe(0);
    expect(gitFetch.err).toContain(
      `From ${ORIGIN}\n   ${first.slice(0, 7)}..${second.slice(0, 7)}  main       -> origin/main\n`
    );
  });

  /** The pane worker_boot leaves ROLE's agent in R's issue workspace, over the implementer's: the
   * remote helper written for ROLE's App (world_remote_app), ROLE's identity and its LEGION_ROLE. */
  function paneAs(R: string, role: string): Record<string, string> {
    const made = bash(
      [
        fn("role_identity"),
        fn("role_env"),
        fn("world_remote_app"),
        ...WORLD_APPS.map(assignment),
        assignment("world_remote_vcs"),
        'R=$1 && world_remote_app "$2" && role_env "$2"',
      ].join("\n"),
      [R, role]
    );
    expect(made).toMatchObject({ code: 0, stderr: "" });
    return { ...variables(made.stdout), LEGION_ROLE: role };
  }
  /** The pull request's branch of every world these rows build. */
  const BRANCH = `legion/${WORLD_PROJECT}-1`;
  /** The lines of a push's output that give the remote's answer: each `remote:` line, git's ` ! `
   * line of a ref the remote refused, and jj's report of one, without what the pod's git wrapper
   * and the leak check print beside them. */
  const answered = (err: string) =>
    err
      .split("\n")
      .filter((line) => /^(remote:| ! |Warning: The remote rejected| {2}refs\/)/.test(line))
      .map((line) => line.trimEnd());
  /** GitHub's reason for the push of an App without `workflows` that changes greet.yml (rig.sh's
   * fixture_first_commit, docs/solutions/github/…-pr2516.md). */
  const WORKFLOW_REFUSAL =
    "refusing to allow a GitHub App to create or update workflow `.github/workflows/greet.yml` without `workflows` permission";
  /** A commit of the review world's workspace that makes COMMAND's change, made with jj under
   * PANE: its id. */
  function committed(R: string, pane: Record<string, string>, command: string, message: string) {
    const made = inWorkspace(
      R,
      `${command} && jj commit -m '${message}' >/dev/null 2>&1 && jj log -r @- --no-graph -T commit_id`,
      pane
    );
    expect({ code: made.code, err: made.err }).toEqual({ code: 0, err: "" });
    expect(made.out).toMatch(/^[0-9a-f]{40}$/);
    return made.out;
  }
  /** The fix of the review world's workflow comment, the trigger it names that `on:` lacks. */
  const FIX_WORKFLOW = "sed -i '1s/ and on every push to main//' .github/workflows/greet.yml";

  // Catches: a remote that refuses the review App a push touching no workflow, which production
  // takes from an App with `contents: write` (appauth's writePermissions): a refusal that reads
  // every changed path as a workflow's, or every push of the review App as refused.
  test("the remote takes the review App's push of a docs change, through git push and jj git push", () => {
    const R = world("remote-docs-taken", {}, "review_world");
    const reviewer = paneAs(R, "reviewer");
    const head = remote(R, "rev-parse", BRANCH);
    const first = committed(
      R,
      reviewer,
      "printf 'It trims the spaces around the name.\\n' >>docs/cli.md",
      "docs: say that the name is trimmed"
    );
    const gitPush = inWorkspace(R, `git push origin ${first}:refs/heads/${BRANCH}`, reviewer);
    expect({ code: gitPush.code, answered: answered(gitPush.err) }).toEqual({
      code: 0,
      answered: [],
    });
    const second = committed(
      R,
      reviewer,
      "printf 'A blank name exits 2.\\n' >>docs/cli.md",
      "docs: say what a blank name does"
    );
    const jjPush = inWorkspace(
      R,
      `jj git fetch >/dev/null 2>&1 && jj bookmark set ${BRANCH} -r ${second} >/dev/null 2>&1 && jj git push --bookmark ${BRANCH}`,
      reviewer
    );
    expect({ code: jjPush.code, answered: answered(jjPush.err) }).toEqual({
      code: 0,
      answered: [],
    });
    expect(remote(R, "rev-parse", BRANCH)).toBe(second);
    expect(pushed(R, /./)).toEqual([
      `refs/heads/${BRANCH} ${head} ${first}`,
      `refs/heads/${BRANCH} ${first} ${second}`,
    ]);
  });

  // GitHub refuses a push of an App without the `workflows` permission that creates, updates or
  // deletes a workflow file, `refusing to allow a GitHub App to create or update workflow
  // `<path>` without `workflows` permission` (a reviewer's push, docs/solutions/github/…-pr2516.md
  // and …-LEGION-779.md), and production's review App holds none (appauth's writePermissions).
  // Catches: a remote that takes the review App's fix of a workflow comment, which production
  // refuses and which leaves the fix a review thread for the implementer; one that refuses it in
  // other words, or moves the branch, or logs the push; and either client printing another
  // answer.
  test("the remote refuses the review App's push of a workflow change with GitHub's line, through git push and jj git push, and its branch does not move", () => {
    const R = world("remote-workflow-refused", {}, "review_world");
    const reviewer = paneAs(R, "reviewer");
    const head = remote(R, "rev-parse", BRANCH);
    const fix = committed(R, reviewer, FIX_WORKFLOW, "ci: say when the tests run");
    const gitPush = inWorkspace(R, `git push origin ${fix}:refs/heads/${BRANCH}`, reviewer);
    expect({ code: gitPush.code, answered: answered(gitPush.err) }).toEqual({
      code: 1,
      answered: [` ! [remote rejected] ${fix} -> ${BRANCH} (${WORKFLOW_REFUSAL})`],
    });
    expect(gitPush.err).toContain(`To ${ORIGIN}\n`);
    expect(gitPush.err).toContain(`error: failed to push some refs to '${ORIGIN}'\n`);
    const jjPush = inWorkspace(
      R,
      `jj bookmark set ${BRANCH} -r ${fix} >/dev/null 2>&1 && jj git push --bookmark ${BRANCH}`,
      reviewer
    );
    expect({ code: jjPush.code, answered: answered(jjPush.err) }).toEqual({
      code: 1,
      answered: [
        "Warning: The remote rejected the following updates:",
        `  refs/heads/${BRANCH} (reason: ${WORKFLOW_REFUSAL})`,
      ],
    });
    expect(jjPush.err).toContain("Error: Failed to push some bookmarks\n");
    expect(remote(R, "rev-parse", BRANCH)).toBe(head);
    expect(pushed(R, /./)).toEqual([]);
  });

  // Catches: a remote that refuses the implement App's workflow push, which production takes
  // from the App that holds `workflows: write` (appauth's writePermissions), so the fix the
  // reviewer could not push could not land at all; and a remote that refuses a commit by who
  // made it rather than by the App that pushes it.
  test("the remote takes the implement App's push of the workflow change it refused the review App", () => {
    const R = world("remote-workflow-implementer", {}, "review_world");
    const reviewer = paneAs(R, "reviewer");
    const head = remote(R, "rev-parse", BRANCH);
    const fix = committed(R, reviewer, FIX_WORKFLOW, "ci: say when the tests run");
    expect(inWorkspace(R, `git push origin ${fix}:refs/heads/${BRANCH}`, reviewer).code).toBe(1);
    const implementer = paneAs(R, "implementer");
    const jjPush = inWorkspace(
      R,
      `jj bookmark set ${BRANCH} -r ${fix} >/dev/null 2>&1 && jj git push --bookmark ${BRANCH}`,
      implementer
    );
    expect({ code: jjPush.code, answered: answered(jjPush.err) }).toEqual({
      code: 0,
      answered: [],
    });
    expect(remote(R, "rev-parse", BRANCH)).toBe(fix);
    expect(pushed(R, /./)).toEqual([`refs/heads/${BRANCH} ${head} ${fix}`]);
  });

  // A push that breaks both the App's permission and the base's ruleset: GitHub refuses a token
  // what its permission does not grant before it reads a ruleset (unmeasured, rig.sh's
  // fixture_first_commit), so it prints the workflow line alone. Catches: a remote that reads the
  // ruleset first and prints its `remote:` lines and `push declined due to repository rule
  // violations` for the review App's push, or both refusals; and one whose workflow refusal
  // spares the base.
  test("the review App's push of a workflow change to the base reads as the workflow refusal alone", () => {
    const R = world("remote-workflow-base", {}, "review_world");
    const reviewer = paneAs(R, "reviewer");
    const base = remote(R, "rev-parse", "main");
    const fix = committed(R, reviewer, FIX_WORKFLOW, "ci: say when the tests run");
    const gitPush = inWorkspace(R, `git push origin ${fix}:refs/heads/main`, reviewer);
    expect({ code: gitPush.code, answered: answered(gitPush.err) }).toEqual({
      code: 1,
      answered: [` ! [remote rejected] ${fix} -> main (${WORKFLOW_REFUSAL})`],
    });
    const jjPush = inWorkspace(
      R,
      `jj bookmark set main -r ${fix} >/dev/null 2>&1 && jj git push --bookmark main`,
      reviewer
    );
    expect({ code: jjPush.code, answered: answered(jjPush.err) }).toEqual({
      code: 1,
      answered: [
        "Warning: The remote rejected the following updates:",
        `  refs/heads/main (reason: ${WORKFLOW_REFUSAL})`,
      ],
    });
    expect(remote(R, "rev-parse", "main")).toBe(base);
    // The implement App's same push breaks the ruleset alone, and reads as that refusal.
    const ruleset = inWorkspace(
      R,
      `git push origin ${fix}:refs/heads/main`,
      paneAs(R, "implementer")
    );
    expect({ code: ruleset.code, last: answered(ruleset.err).at(-1) }).toEqual({
      code: 1,
      last: ` ! [remote rejected] ${fix} -> main (push declined due to repository rule violations)`,
    });
    expect(pushed(R, /./)).toEqual([]);
  });

  // Catches: a world whose push's leak check reaches the live GitHub. The shared check runs `git
  // ls-remote -- https://github.com/<owner>/<repo>`, a URL no remote's helper takes: it was
  // refused there, the check asked `git credential fill`, the pane's `legion git-credential` ran,
  // and the world's token went to github.com (measured on 2026-11-18: 2 connections, one with
  // `Authorization: Basic`). Its git runs with the world's exec path (push_guard, world_remote),
  // so it reads the world's heads and asks for nothing; and the check still refuses what it must:
  // a planted private host (the guard's own rows' plant, built from two parts here so this file
  // names none) is refused and never reaches the remote.
  const guard = "/opt/legion/bin/git";
  test.skipIf(
    !existsSync(guard) ||
      !existsSync("/opt/legion/bin/legion") ||
      !existsSync(process.env.PUBLIC_LEAK_CHECK ?? "")
  )(
    "a push's leak check reaches nothing but the world, asks no credential, and still refuses",
    () => {
      const R = world("remote-leak-check");
      const bin = freshDir("remote-leak-check-bin");
      const calls = path.join(bin, "legion-calls");
      writeFileSync(
        path.join(bin, "legion"),
        `#!/bin/sh\necho "legion $*" >>'${calls}'\nexec /opt/legion/bin/legion "$@"\n`,
        { mode: 0o755 }
      );
      writeFileSync(calls, "");
      const curl = path.join(R, "curl.log");
      const traced = {
        PATH: `${bin}:${path.join(R, "bin")}:${childEnv.PATH}`,
        GIT_TRACE_CURL: curl,
        GIT_TRACE_CURL_NO_DATA: "1",
      };
      expect(readFileSync(path.join(R, "guard.env"), "utf8")).toBe(
        `PUBLIC_LEAK_CHECK=${path.join(R, "lib", "public-leak-check.sh")}\n`
      );
      const clean = inWorkspace(
        R,
        'printf "a note\\n" >notes.md && jj commit -m "docs: a note" >/dev/null 2>&1 && jj bookmark create notes -r @- >/dev/null 2>&1 && jj git push --bookmark notes',
        traced
      );
      expect(clean.code).toBe(0);
      const notes = remote(R, "rev-parse", "refs/heads/notes");
      expect(pushed(R, / refs\/heads\/notes /)).toEqual([
        `refs/heads/notes ${"0".repeat(40)} ${notes}`,
      ]);
      const host = ["wobble-fern.quiet-harbor.ts", "net"].join(".");
      const planted = inWorkspace(
        R,
        `printf 'the runner answers at ${host}\\n' >>notes.md && jj commit -m "docs: where it runs" >/dev/null 2>&1 && jj bookmark set notes -r @- >/dev/null 2>&1 && jj git push --bookmark notes`,
        traced
      );
      expect(planted.code).not.toBe(0);
      expect(planted.err).toContain("LEAK CHECK: refusing the push to origin");
      expect(planted.err).toContain("names a Tailscale MagicDNS host");
      expect(remote(R, "rev-parse", "refs/heads/notes")).toBe(notes);
      expect({
        connections:
          (existsSync(curl) ? readFileSync(curl, "utf8") : "").match(/Connected to /g) ?? [],
        credential: readFileSync(calls, "utf8"),
      }).toEqual({ connections: [], credential: "" });
    }
  );
});

// A batch given SKILL_SCENARIOS_POSTGRES_URL makes each run's database on that server
// (own_dispatch), and names the server in its notes, its failures and each run's dispatch.json.
describe("safe_url", () => {
  const pgUrl = path.join(root, "packages/envoy/scripts/lib/pg-url.sh");
  // Catches: a note, a failure message or a run's dispatch.json that prints the password of a
  // server the operator gave, in a Postgres URL's user info as libpq reads it (to the first `@`,
  // a `?` in the password included), in its `password=` or `sslpassword=` parameter (its key
  // percent-decoded, as libpq reads `%70assword=`), or in a NATS
  // URL's user info as Go's net/url reads it (to the last `@`); a query left with a dangling `?`
  // or `&`; a safe_url that changes a URL with no password in it; and a URL pg-url.sh refuses (a
  // raw `@` in its hosts) printed as it is, where safe_url prints a placeholder.
  test("safe_url drops the password of a URL and keeps the rest", () => {
    expect(
      bash(`. ${JSON.stringify(pgUrl)}\n${fn("safe_url")}\nfor url; do safe_url "$url"; done`, [
        "postgres://u:p@h:1/d",
        "postgres://u@h:1/d",
        "nats://127.0.0.1:4222",
        "postgres://u:?@127.0.0.1:5433/legion_test",
        "postgres://u@localhost:5432@10.1.2.3/db",
        "postgres://u@127.0.0.1:5433/d?sslmode=disable&password=p&application_name=x",
        "postgresql://u@127.0.0.1:5433/d?sslpassword=p",
        "postgres://u:p@127.0.0.1:5433/d?password=p&sslmode=disable",
        "nats://u:p@ss@127.0.0.1:4222",
        "postgres://u@127.0.0.1:5433/d?%70assword=p&sslmode=disable",
      ])
    ).toEqual({
      code: 0,
      stdout:
        "postgres://u@h:1/d\npostgres://u@h:1/d\nnats://127.0.0.1:4222\n" +
        "postgres://u@127.0.0.1:5433/legion_test\n" +
        "<a Postgres URL pg-url.sh refuses>\n" +
        "postgres://u@127.0.0.1:5433/d?sslmode=disable&application_name=x\n" +
        "postgresql://u@127.0.0.1:5433/d\n" +
        "postgres://u@127.0.0.1:5433/d?sslmode=disable\n" +
        "nats://u@127.0.0.1:4222\n" +
        "postgres://u@127.0.0.1:5433/d?sslmode=disable\n",
      stderr: "-: refused a Postgres URL: a host holds a raw @ (the URL is not printed)\n",
    });
  });
});

// services_up reads a given SKILL_SCENARIOS_POSTGRES_URL before anything connects to that server
// or the batch writes the URL anywhere. python3 with libpq: its guards read the URL with this
// host's libpq (scripts/e2e/lib/loopback-dsn.py), which refuses every URL where it finds none.
const withoutPython = skipWithout("python3");
const withoutLibpq =
  !withoutPython &&
  Bun.spawnSync([
    "python3",
    "-c",
    "import ctypes.util, sys; sys.exit(ctypes.util.find_library('pq') is None)",
  ]).exitCode !== 0;
if (withoutLibpq) {
  console.warn(
    `Skipped the services_up rows in ${import.meta.path}. services_up reads a given Postgres ` +
      `URL with libpq, and ctypes.util.find_library("pq") finds none on this host; install libpq5.`
  );
}
describe.skipIf(withoutPython || withoutLibpq)("services_up", () => {
  const pgUrl = path.join(root, "packages/envoy/scripts/lib/pg-url.sh");
  const loopbackDsn = path.join(root, "scripts/e2e/lib/loopback-dsn.sh");
  /** services_up given URL and a NATS, in a services directory of its own, with a psql first on
   * PATH that appends to psql.log each time it runs. until_true runs its command once, so a URL
   * that passed every check would reach psql at once. Answers services_up's status and standard
   * error, whether psql ran, and what the services directory holds. */
  function up(url: string) {
    const dir = freshDir("services-up");
    const bin = path.join(dir, "bin");
    const log = path.join(dir, "psql.log");
    const services = path.join(dir, "services");
    mkdirSync(bin);
    mkdirSync(services);
    writeFileSync(path.join(bin, "psql"), `#!/bin/sh\necho ran >>${JSON.stringify(log)}\n`, {
      mode: 0o755,
    });
    const result = bash(
      `set -euo pipefail\n. "$1"\n. "$2"\n${fn("fail")}\n${fn("given_services")}\n` +
        `${fn("safe_url")}\n${fn("pg_sql")}\n${fn("services_up")}\n` +
        `until_true() { shift 2; "$@"; }\n` +
        `S=$3\nservices_env=$S/env\nservices_postgres=$S/postgres-url\nservices_up`,
      [pgUrl, loopbackDsn, services],
      {
        PATH: `${bin}:${childEnv.PATH}`,
        SKILL_SCENARIOS_POSTGRES_URL: url,
        SKILL_SCENARIOS_NATS_URL: "nats://127.0.0.1:4222",
      }
    );
    return {
      code: result.code,
      stderr: result.stderr,
      psqlRan: existsSync(log),
      written: readdirSync(services),
    };
  }
  const host = (name: string) =>
    `SKILL_SCENARIOS_POSTGRES_URL must name 127.0.0.1, localhost or ::1: libpq reads the host ${name}, and each run creates a database there`;
  const sslpassword =
    "SKILL_SCENARIOS_POSTGRES_URL holds an sslpassword parameter, a client key's passphrase, whose value this line does not print: pg_sql takes only the password out of the URL it gives psql, so psql's arguments, which every user of this machine can read, would hold the passphrase";

  // Catches: a services_up that connects to, or writes into its services file, a given URL that
  // its checks refuse, because a check is gone or runs after the write or the first psql: a host
  // that is not loopback, the `localhost?x@evil.example` shape whose host libpq reads as
  // evil.example (scripts/e2e/lib/loopback-dsn.sh), a key=value connection string, an
  // `sslpassword` parameter (its key percent-decoded too), a URL pg-url.sh refuses (a raw `@` in
  // its path, which pg-url.sh names on its own line first), and a value run_database_url refuses
  // to write into each run's connection string (a newline, which it refuses in a password the same
  // way). Each refusal is the one its own check prints, and holds no password.
  test("refuses a URL the runs cannot use before psql runs or the URL is written", () => {
    for (const [url, refusal, before = ""] of [
      ["postgres://u:p@db.example:5432/postgres", host("db.example")],
      ["postgres://localhost?x:p@evil.example:5432/postgres", host("evil.example")],
      [
        "host=127.0.0.1 port=5432 user=u password=p dbname=postgres",
        "SKILL_SCENARIOS_POSTGRES_URL is not a postgres:// or postgresql:// URL: the batch names the server in its notes and in each run's dispatch.json, and it can take a password out of a URL alone, never out of a key=value connection string",
      ],
      ["postgres://u:p@127.0.0.1:5432/postgres?sslmode=require&sslpassword=x", sslpassword],
      ["postgresql://u@localhost:5432/postgres?%73slpassword=x&sslmode=require", sslpassword],
      [
        "postgres://u@127.0.0.1:5432/post@gres",
        "SKILL_SCENARIOS_POSTGRES_URL is a URL pg-url.sh refuses, for the reason on the line above: pg_sql and safe_url take its password out with pg-url.sh, so the batch could neither reach that server nor name it",
        "-: refused a Postgres URL: an @ sits between the first / and the ? (the URL is not printed)\n",
      ],
      [
        "postgres://u@localhost:5432/postgres?application_name=x%0A",
        "SKILL_SCENARIOS_POSTGRES_URL gives application_name U+000A, a character that does not print: it reaches the rewritten connection string, where a caller writes that string into a file, and this refusal prints no value that holds one",
      ],
    ]) {
      expect({ url, ...up(url) }).toEqual({
        url,
        code: 1,
        stderr: `${before}skill-scenarios: ${refusal}\n`,
        psqlRan: false,
        written: [],
      });
    }
  });
});

// run_env_pids names each process whose HOME is under a run's directory under DIR, with its start
// time, for the exit handlers' stop_env_processes. It walks every process's environment, so a
// machine whose /proc/<pid>/environ entries together pass ARG_MAX must not hand them to grep as its
// arguments: past about 80,000 processes grep could not start, and the run had nothing to stop.
// perl: each row's processes and harness start through ownGroup (scripts/e2e/lib/own-group.ts).
const withoutPerl = skipWithout("perl");
describe.skipIf(withoutPerl)("run_env_pids and stop_env_processes", () => {
  const force = path.join(root, "scripts/e2e/lib/arg-max-force.sh");
  const processTree = path.join(root, "scripts/e2e/lib/process-tree.sh");
  const start = (pid: number) =>
    readFileSync(`/proc/${pid}/stat`, "utf8")
      .replace(/^.*\) /, "")
      .split(" ")[19];
  // Each row's run process and its harness shell run in process groups of their own (ownGroup,
  // scripts/e2e/lib/own-group.ts): stop_env_processes stops what it ends, and a stopped member of
  // the test runner's own group hangs the runner up where that group is orphaned, as in a Legion pod.
  // A row that sweeps waits until its run process runs sleep (untilRunning), and so leads its own
  // group, before it sweeps. The walk reads `proc_root`, a proc root of the row's run process alone
  // (scripts/e2e/lib/proc-root.ts), so a row's time is its own and not the host's process count.
  const runProcess = (dir: string) =>
    Bun.spawn(ownGroup(["sleep", "60"]), {
      env: { PATH: process.env.PATH ?? "", HOME: path.join(dir, "run-1", "home") },
      stdout: "ignore",
      stderr: "ignore",
    });
  const groupBash = (script: string, args: string[], procRoot: string) => {
    const result = spawnChecked(ownGroup(["bash", "-c", script, "-", ...args]), {
      env: { ...childEnv, proc_root: procRoot },
    });
    return {
      code: result.exitCode,
      stdout: result.stdout.toString(),
      stderr: result.stderr.toString(),
    };
  };

  // Catches: a walk that hands grep every /proc/<pid>/environ at once, which under the force
  // (scripts/e2e/lib/arg-max-force.sh) cannot start and names nothing. The force cannot hold under
  // gVisor (a Legion pod's kernel): there the row is skipped, by that name (argMaxForceHolds).
  test.skipIf(!argMaxForceHolds())(
    "names a process of the run, with its start time, where a program handed every /proc/<pid>/environ would not start",
    () => {
      const dir = freshDir("env-pids");
      const scratch = freshDir("env-pids-scratch");
      const run = runProcess(dir);
      try {
        const old = 'grep -lzsF -e "HOME=$2/" "$proc_root"/[0-9]*/environ';
        const forced = groupBash(
          `set -euo pipefail\n. ${JSON.stringify(force)}\n. ${JSON.stringify(processTree)}\n${fn("run_env_pids")}\narg_max_force environ 6144 "$1" ${JSON.stringify(old)}\nrun_env_pids "$2"`,
          [scratch, dir],
          procRootOf(scratch, [run.pid])
        );
        expect(forced).toEqual({ code: 0, stdout: `${run.pid} ${start(run.pid)}\n`, stderr: "" });
      } finally {
        run.kill("SIGKILL");
      }
    }
  );

  // Catches: a stop_env_processes that does not end what its walk names. The loop that ends each
  // process, held to its start and its mark, is kill_held (scripts/e2e/lib/process-tree.sh), the
  // run's sweep's own; its rows, a pid that passed to another process and a parent that exits after
  // the freeze, are run-processes.test.ts's, and are not written a second time here.
  test("stops each process of the run its walk names, with the run's sweep's own loop", async () => {
    const dir = freshDir("env-stop");
    const run = runProcess(dir);
    await untilRunning(run.pid, "sleep", waits.end());
    try {
      const rig = path.join(root, "scripts/e2e/lib/rig.sh");
      const prelude = `set -euo pipefail\nroot=${JSON.stringify(root)} work=$1 evidence=$1 timeout_hook=\nnote() { :; }\nfail() { exit 1; }\n. ${JSON.stringify(rig)}\n${fn("stop_env_processes")}`;
      const own = groupBash(
        `${prelude}\n${fn("run_env_pids")}\nstop_env_processes "$1"; echo "status $?"`,
        [dir],
        procRootOf(dir, [run.pid])
      );
      expect(own).toEqual({ code: 0, stdout: "status 0\n", stderr: "" });
      expect(await run.exited).not.toBe(0);
      expect(run.signalCode).toBe("SIGKILL");
    } finally {
      run.kill("SIGKILL");
    }
  });

  // Catches: a walk or a stop that misses an agent in the PID namespace of its own every run's agent
  // starts in (rig.sh's launch: `unshare -Urm -p --fork --mount-proc`, the agent's command in a
  // second user namespace, under `env -i` and the pane's variables), or that names it by the pid
  // that namespace gives it, where it is the namespace's first process. The rig reads its own
  // /proc, where the agent has the pid and the start time the rig's namespace gives it, and every
  // pid it names and signals is that one. The agent here is the namespace's first process, its
  // parent the unshare, which carries no HOME; it says `started` on stdout before it execs the
  // agent, so by then the unshare has it for its one child.
  test.skipIf(withoutNamespace)(
    "names and ends an agent in a PID namespace of its own by the pid the rig's own /proc gives it",
    async () => {
      const dir = freshDir("env-namespace");
      const agent = Bun.spawn(
        ownGroup([
          "env",
          "-i",
          `PATH=${process.env.PATH ?? ""}`,
          "unshare",
          "-Urm",
          "-p",
          "--fork",
          "--mount-proc",
          "sh",
          "-c",
          'echo started && exec unshare -U --map-user="$1" --map-group="$2" env -i HOME="$3" sleep 60',
          "-",
          String(process.getuid?.() ?? 0),
          String(process.getgid?.() ?? 0),
          path.join(dir, "run-1", "home"),
        ]),
        { stdout: "pipe", stderr: "ignore" }
      );
      try {
        await agent.stdout.getReader().read();
        const inner = Number(
          readdirSync(`/proc/${agent.pid}/task`)
            .flatMap((task) =>
              readFileSync(`/proc/${agent.pid}/task/${task}/children`, "utf8").trim().split(/\s+/)
            )
            .find((child) => child !== "")
        );
        await untilRunning(inner, "sleep", waits.end());
        const procRoot = procRootOf(dir, [agent.pid, inner]);
        const named = groupBash(
          `set -euo pipefail\n. ${JSON.stringify(processTree)}\n${fn("run_env_pids")}\nrun_env_pids "$1"`,
          [dir],
          procRoot
        );
        expect(named).toEqual({ code: 0, stdout: `${inner} ${start(inner)}\n`, stderr: "" });
        const rig = path.join(root, "scripts/e2e/lib/rig.sh");
        const stopped = groupBash(
          `set -euo pipefail\nroot=${JSON.stringify(root)} work=$1 evidence=$1 timeout_hook=\nnote() { :; }\nfail() { exit 1; }\n. ${JSON.stringify(rig)}\n${fn("stop_env_processes")}\n${fn("run_env_pids")}\nstop_env_processes "$1"; echo "status $?"`,
          [dir],
          procRoot
        );
        expect(stopped).toEqual({ code: 0, stdout: "status 0\n", stderr: "" });
        expect(await agent.exited).not.toBe(0);
        expect(existsSync(`/proc/${inner}`)).toBe(false);
      } finally {
        agent.kill("SIGKILL");
      }
    }
  );
});

// cmd_batch and cmd_run read SKILL_SCENARIOS_SOURCE through source_dir before they start anything.
describe("source_dir", () => {
  function work(files: string[]) {
    const dir = freshDir("sources");
    mkdirSync(path.join(dir, "sources", "acme-1"), { recursive: true });
    for (const file of files) writeFileSync(path.join(dir, "sources", "acme-1", file), "x\n");
    return dir;
  }
  const sourceDir = (dir: string, scenario: string, name: string) =>
    bash(`${fn("fail")}\n${fn("source_dir")}\nwork=$1\nsource_dir "$2" "$3"`, [
      dir,
      scenario,
      name,
    ]);
  const full = ["title.txt", "messages.md", "asks.json"];

  // Catches: a spec-rewrite run seeded from a source with no old spec, which seed.ts would refuse
  // only after the batch started its services.
  test("spec-rewrite refuses a source without old-spec.md, naming it", () => {
    const dir = work(full);
    const { code, stderr } = sourceDir(dir, "spec-rewrite", "acme-1");
    expect(code).toBe(1);
    expect(stderr).toContain(`the source ${dir}/sources/acme-1 has no old-spec.md`);
  });

  // Catches: a spec-writing run refused for want of an old spec it never reads.
  test("spec-writing takes the same source, and prints its directory", () => {
    const dir = work(full);
    expect(sourceDir(dir, "spec-writing", "acme-1")).toEqual({
      code: 0,
      stdout: `${dir}/sources/acme-1\n`,
      stderr: "",
    });
    expect(sourceDir(work([...full, "old-spec.md"]), "spec-rewrite", "acme-1").code).toBe(0);
  });

  // Catches: a run that starts on a source missing a file seed.ts reads, or a name that leaves
  // $work/sources.
  test("refuses an unset name, a path, and a source missing a file", () => {
    const dir = work(["title.txt", "asks.json"]);
    expect(sourceDir(dir, "spec-writing", "").stderr).toContain("SKILL_SCENARIOS_SOURCE is unset");
    expect(sourceDir(dir, "spec-writing", "../sources/acme-1").stderr).toContain(
      "is not a directory name"
    );
    expect(sourceDir(dir, "spec-writing", "acme-1").stderr).toContain("has no messages.md");
  });
});

// The conversation of a spec run: rig.sh's spec_turns with stand-ins for omp (launch), seed.ts
// (seed_ts) and the judge's person stand-in (judge_ts). Each stand-in logs its call, and the
// seed.ts state stand-in hands back the state the test scripted for that turn.
describe("spec_turns", () => {
  interface Turn {
    asks?: number;
    approval?: boolean;
  }
  function conversation(
    turns: Turn[],
    options: { secondTranscript?: boolean; failAt?: number } = {}
  ) {
    const run = freshDir("spec-run");
    const states = freshDir("spec-states");
    mkdirSync(path.join(run, "sessions"));
    turns.forEach((turn, index) => {
      const t = index + 1;
      writeFileSync(
        path.join(states, `state-${t}.json`),
        JSON.stringify({
          turn: t,
          open_asks: Array.from({ length: turn.asks ?? 0 }, (_, k) => ({
            id: `ask-${t}-${k}`,
            question: "Q?",
            options: [],
            multiple: false,
          })),
          approval: turn.approval ? { ask_id: `approval-${t}`, version: t } : null,
        })
      );
    });
    const standIns = `
launch() {
  printf 'launch %s | %s\\n' "$*" "$(<"$R/prompt.txt")" >>"$R/calls"
  [ -n "\${SECOND:-}" ] && [ $# -gt 1 ] && : >"$R/sessions/fresh.jsonl"
  printf x >>"$R/launches"
  status=0
  [ "$(wc -c <"$R/launches")" != "\${FAIL_AT:-0}" ] || status=1
  printf 'output of %s\\nexit=%s\\n' "$*" "$status" >"$R/out.txt"
  printf '{}\\n' >>"$R/sessions/run.jsonl"
}
seed_ts() {
  printf 'seed %s\\n' "$*" >>"$R/calls"
  [ "$1" != state ] || cp "$STATES/state-$3.json" "$R/state-$3.json"
}
judge_ts() {
  printf 'judge %s\\n' "$*" >>"$R/calls"
  jq -c '.open_asks[] | {ask: .id, selected: [], text: "t"}' "$R/state-$3.json" >"$R/answers-$3.jsonl"
}`;
    const result = bash(
      `${fn("note")}\n${fn("fail")}\n${fn("spec_turns")}\n${standIns}\nR=$1\nspec_turns spec-writing-s1-1 LWEVAL-2 "Design it."`,
      [run],
      {
        STATES: states,
        ...(options.secondTranscript ? { SECOND: "1" } : {}),
        ...(options.failAt === undefined ? {} : { FAIL_AT: String(options.failAt) }),
      }
    );
    const read = (name: string) =>
      existsSync(path.join(run, name)) ? readFileSync(path.join(run, name), "utf8") : "";
    const calls = read("calls")
      .trimEnd()
      .split("\n")
      .map((line) => line.replaceAll(run, "<R>"));
    return { ...result, run, read, calls };
  }
  const answered = "The person answered your asks on LWEVAL-2.";
  const approved = "The person approved the spec of LWEVAL-2.";

  // Catches: a loop that answers before it reads the state, continues without --continue (a second
  // session), skips the stand-in, or sends another prompt than the three the rig.sh header gives.
  test("asks, then an approval: answer, continue, approve, continue once more, stop", () => {
    const { code, calls, read } = conversation([{ asks: 2 }, { approval: true }, {}]);
    expect(code).toBe(0);
    expect(calls).toEqual([
      "launch spec-writing-s1-1 | Design it.",
      "seed state <R>/fixture.json 1 <R>",
      "judge answer <R> 1",
      "seed answer <R>/fixture.json <R>/answers-1.jsonl",
      `launch spec-writing-s1-1 --continue | ${answered}`,
      "seed state <R>/fixture.json 2 <R>",
      "seed approve <R>/fixture.json",
      `launch spec-writing-s1-1 --continue | ${approved}`,
      "seed state <R>/fixture.json 3 <R>",
    ]);
    expect(read("turns.txt")).toBe("1 first exit=0\n2 answered exit=0\n3 approved exit=0\n");
    expect(read("answers.jsonl")).toBe(read("answers-1.jsonl"));
    expect(read("out-2.txt")).toBe("output of spec-writing-s1-1 --continue\nexit=0\n");
  });

  // Catches: a loop that answers the open asks when an approval request is open too (the rig.sh
  // header's order puts the approval first).
  test("an open approval request goes before open asks", () => {
    const { calls } = conversation([{ asks: 1, approval: true }, {}]);
    expect(calls.slice(1, 3)).toEqual([
      "seed state <R>/fixture.json 1 <R>",
      "seed approve <R>/fixture.json",
    ]);
    expect(calls.some((call) => call.startsWith("judge"))).toBe(false);
  });

  // Catches: a loop that continues a session with nothing for it to answer.
  test("no open ask and no approval request ends the run after its turn", () => {
    const { code, calls, read } = conversation([{}]);
    expect(code).toBe(0);
    expect(calls).toEqual([
      "launch spec-writing-s1-1 | Design it.",
      "seed state <R>/fixture.json 1 <R>",
    ]);
    expect(read("turns.txt")).toBe("1 first exit=0\n");
  });

  // Catches: a run past 8 turns, or an answer after the eighth turn that no turn reads.
  test("stops after 8 turns, answering nothing after the eighth", () => {
    const { calls, read } = conversation(Array.from({ length: 9 }, () => ({ asks: 1 })));
    expect(calls.filter((call) => call.startsWith("launch"))).toHaveLength(8);
    expect(calls.filter((call) => call.startsWith("judge"))).toHaveLength(7);
    expect(calls.at(-1)).toBe("seed state <R>/fixture.json 8 <R>");
    expect(read("turns.txt").trimEnd().split("\n")).toHaveLength(8);
  });

  // Catches: a --continue that starts a second transcript beside the first, which every reader of
  // the run would then read as one of two.
  test("a later turn that leaves a second transcript fails the run, naming its sessions", () => {
    const { code, stderr, run } = conversation([{ asks: 1 }, {}], { secondTranscript: true });
    expect(code).toBe(1);
    expect(stderr).toContain(`after turn 2, ${run}/sessions holds 2 transcripts`);
  });

  // Catches: a loop that answers and continues a session whose turn failed or timed out.
  test("a turn that does not exit 0 ends the run after its state", () => {
    const { code, calls, read } = conversation([{ asks: 1 }, { asks: 1 }, {}], { failAt: 2 });
    expect(code).toBe(0);
    expect(calls.at(-1)).toBe("seed state <R>/fixture.json 2 <R>");
    expect(calls.filter((call) => call.startsWith("judge"))).toHaveLength(1);
    expect(read("turns.txt")).toBe("1 first exit=0\n2 answered exit=1\n");
  });
});

// cmd_profile's superpowers step, with a stand-in mise: it records the environment omp got and
// writes a transcript whose one tool result is the text in result.txt beside it.
describe("install_superpowers", () => {
  function setup(result: string) {
    const bin = freshDir("superpowers-bin");
    const label = freshDir("superpowers-label");
    const src = freshDir("superpowers-src");
    for (const skill of ["writing-plans", "brainstorming"]) {
      mkdirSync(path.join(src, "skills", skill), { recursive: true });
      writeFileSync(
        path.join(src, "skills", skill, "SKILL.md"),
        `---\nname: ${skill}\n---\n\n# Skill ${skill}\n`
      );
    }
    writeFileSync(path.join(src, "package.json"), '{"version":"9.9.9"}\n');
    writeFileSync(path.join(label, "built-from"), "checkout /x\n");
    // The transcript line the stand-in omp leaves, made here: the stand-in runs under the probe's
    // own `env -i`, where a jq on PATH may be a mise shim that cannot run.
    writeFileSync(
      path.join(bin, "probe.jsonl"),
      `${JSON.stringify({ message: { role: "toolResult", content: [{ type: "text", text: result }] } })}\n`
    );
    // `mise where` names the pin's install, whose bin/omp the probe runs by path; any other call is
    // the probe's `mise x <pin> -- <omp> -p …`.
    mkdirSync(path.join(bin, "pin", "bin"), { recursive: true });
    writeFileSync(path.join(bin, "pin", "bin", "omp"), "#!/bin/sh\n", { mode: 0o755 });
    writeFileSync(
      path.join(bin, "mise"),
      `#!/usr/bin/env bash
if [ "$1" = where ]; then echo "$(dirname "$0")/pin"; exit 0; fi
printf '%s\\n' "$@" >"$(dirname "$0")/argv.txt"
env >"$(dirname "$0")/env.txt"
while [ $# -gt 0 ] && [ "$1" != --session-dir ]; do shift; done
mkdir -p "$2"
cp "$(dirname "$0")/probe.jsonl" "$2/probe.jsonl"
echo done
`,
      { mode: 0o755 }
    );
    // Run from the label's own temporary directory, with the label for both P and I: an install
    // path the call gets wrong lands there, never in the checkout the suite runs from (an earlier
    // argument order wrote packages/pi-legion/skills-s1).
    const run = () =>
      bash(
        `${fn("fail")}\n${fn("pinned_omp")}\n${fn("install_superpowers")}\nsuperpowers_skills=(writing-plans brainstorming)\ncd "$1"\ninstall_superpowers "$1" "$1" legion omp@pin`,
        [label],
        { PATH: `${bin}:${childEnv.PATH}`, SKILL_SCENARIOS_SUPERPOWERS: src, USER: "rig" }
      );
    return { bin, label, run };
  }

  // Catches: a profile step that copies the skills somewhere the pinned Oh My Pi does not read, or
  // gives the probe the operator's Envoy (the label's pi-envoy would register with it).
  test("copies both skills into the profile, probes with no Envoy, and marks the label", () => {
    const { bin, label, run } = setup("# Skill writing-plans\n\nbody");
    expect(run()).toMatchObject({ code: 0, stderr: "" });
    const skills = path.join(label, "home/.omp/profiles/legion/agent/skills");
    expect(readFileSync(path.join(skills, "brainstorming/SKILL.md"), "utf8")).toContain(
      "# Skill brainstorming"
    );
    expect(existsSync(path.join(skills, "writing-plans/SKILL.md"))).toBe(true);
    expect(existsSync(path.join(label, "superpowers"))).toBe(true);
    // Each variable is read whole, by name: a substring would also match a longer name such as
    // X_OMP_PROFILE, and env can print any variable first.
    const env = new Map(
      readFileSync(path.join(bin, "env.txt"), "utf8")
        .split("\n")
        .flatMap((line): [string, string][] => {
          const eq = line.indexOf("=");
          return eq > 0 ? [[line.slice(0, eq), line.slice(eq + 1)]] : [];
        })
    );
    expect(env.get("ENVOY_URL")).toBe("http://127.0.0.1:1");
    expect(env.get("OMP_PROFILE")).toBe("legion");
    expect(env.get("HOME")).toBe(`${label}/home`);
    expect(readFileSync(path.join(label, "built-from"), "utf8")).toContain(
      "superpowers-skill writing-plans "
    );
    // The probe names the pin's own binary, so no omp earlier on PATH stands in for it.
    expect(readFileSync(path.join(bin, "argv.txt"), "utf8")).toStartWith(
      `x\nomp@pin\n--\n${bin}/pin/bin/omp\n-p\n`
    );
  });

  // Catches: a probe that passes when Oh My Pi did not load the skill.
  test("fails when the read answers Unknown skill, and marks nothing", () => {
    const { label, run } = setup("Unknown skill: writing-plans\nAvailable: dispatch");
    const { code, stderr } = run();
    expect(code).toBe(1);
    expect(stderr).toContain("does not load skill://writing-plans (Unknown skill)");
    expect(existsSync(path.join(label, "superpowers"))).toBe(false);
  });

  // Catches: a probe that passes when the agent read nothing at all.
  test("fails when no tool result holds the skill's heading", () => {
    const { label, run } = setup("some other file");
    const { code, stderr } = run();
    expect(code).toBe(1);
    expect(stderr).toContain("read no skill://writing-plans");
    expect(existsSync(path.join(label, "superpowers"))).toBe(false);
  });
});

// rig.sh judge-omp, the whole script, in its Docker container (Docker is on the devbox and on CI's
// ubuntu-24.04 runners), with the real key-file route and a stand-in omp that records its argv and
// environment. SKILL_SCENARIOS_JUDGE_OMP puts the stand-in where the pinned omp's binary is
// mounted, so no test needs a model. The checkout's own skills/asd-ste100 is the ste skill.
describe("judge-omp", () => {
  // Each row starts one or two judge containers, and a container's start on a loaded host has
  // taken longer than the file's 60 s bound, so these rows get a bound of their own.
  const judgeBound = 180_000;
  // Every row that reaches the container needs Docker, so a machine without it (a Legion
  // worker's pod) skips those rows and the rest of the file still runs. GitHub Actions is the
  // exception, as in extensions/controller-start-omp.test.ts: its ubuntu-24.04 runners have
  // Docker, and a runner that lost it must fail these rows rather than quietly skip them.
  // The bad-argument row is not skipped: judge-omp refuses its arguments before it looks for
  // Docker, so it runs anywhere.
  const onActions = process.env.GITHUB_ACTIONS === "true";
  const skip = withoutDocker && !onActions;
  // Staged in a hook: Bun runs this body even where the rows do not run, and a write here would
  // make the suite's work directory and leave it behind.
  let work = "";
  let outside = "";
  let omp = "";
  beforeAll(() => {
    work = freshDir("judge-work");
    outside = path.join(work, "outside.txt");
    writeFileSync(outside, "a file beside the judge's directory\n");
    mkdirSync(path.join(work, "runs"), { recursive: true });
    omp = path.join(freshDir("judge-omp-bin"), "omp");
    // The stand-in writes everything through the one file the judge may write, answer.json: argv
    // (one word a line), then `env` lines, then one line per probe (`<name> allowed|denied`), then
    // one `mode <name> <bits>` line per key path, which only a judge holding the key can read.
    writeFileSync(
      omp,
      `#!/bin/bash
probe() { if "\${@:2}" >/dev/null 2>&1; then echo "probe $1 allowed"; else echo "probe $1 denied"; fi; }
# DROP: write a marker into every directory this judge can write outside its own directory.
# SEEK: report any marker another judge left anywhere this judge can read.
if [ -n "\${DROP:-}" ]; then
  find / \\( -path /proc -o -path /sys -o -path /dev -o -path "$PWD" \\) -prune -o -type d -writable -print 2>/dev/null |
    while IFS= read -r d; do printf 'private input\\n' >"$d/leak-$DROP" 2>/dev/null; done
fi
{
  printf 'argv %s\\n' "$0" "$@"
  printf 'env %s\\n' "PWD=$PWD" "HOME=$HOME" "OMP_PROFILE=$OMP_PROFILE"
  compgen -e | grep -q '^LEGION_E2E' && echo "env LEGION_E2E set"
  probe read-outside cat "$OUTSIDE"
  probe read-home-ssh ls "$OPERATOR_HOME/.ssh"
  probe list-work ls "$WORK/runs"
  probe read-own cat "$PWD/prompt.txt"
  probe write-own-dir touch "$PWD/new.txt"
  probe write-sessions touch "$PWD/sessions/s.jsonl"
  probe read-skill cat "$HOME/.omp/profiles/$OMP_PROFILE/agent/skills/asd-ste100/SKILL.md"
  probe read-models cat "$HOME/.omp/profiles/$OMP_PROFILE/agent/models.yml"
  key=$(sed -n 's/.*!exec \\(.*\\)"$/\\1/p' "$HOME/.omp/profiles/$OMP_PROFILE/agent/models.yml" | head -1)
  probe key-command test -x "$key"
  probe gateway-base-url grep -q 'baseUrl: https://gateway.internal.example/anthropic' "$HOME/.omp/profiles/$OMP_PROFILE/agent/models.yml"
  probe token-is-jwt bash -c '[[ "$("$1")" =~ ^eyJ[A-Za-z0-9_-]*\\.[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+$ ]]' - "$key"
  probe dbus-set test -n "\${DBUS_SESSION_BUS_ADDRESS:-}"
  printf 'mode %s\\n' "key-dir $(stat -c %a /opt/key 2>/dev/null || echo none)" \\
    "token $(stat -c %a /opt/key/token 2>/dev/null || echo none)"
  [ -z "\${SEEK:-}" ] || echo "seek $(find / \\( -path /proc -o -path /sys -o -path /dev \\) -prune -o -name 'leak-*' -print 2>/dev/null | head -1)"
} >"$PWD/answer.json"
echo judged
exit "$(cat "$PWD/exit-status" 2>/dev/null || echo 0)"
`,
      { mode: 0o755 }
    );
  });
  const judge = (
    kind: string,
    dir: string,
    output = "answer.json",
    env: Record<string, string> = {}
  ) =>
    spawnChecked(["bash", path.join(import.meta.dir, "rig.sh"), "judge-omp", kind, dir, output], {
      env: {
        ...childEnv,
        USER: "rig",
        SKILL_SCENARIOS_WORK: work,
        SKILL_SCENARIOS_JUDGE_OMP: omp,
        SKILL_SCENARIOS_JUDGE_ENV: `OUTSIDE=${outside} OPERATOR_HOME=${operatorHome} WORK=${work}`,
        LEGION_E2E_MODEL_ROUTE: "key-file",
        LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
        ...env,
      },
    });
  let jobs = 0;
  function judgeDir(): string {
    const dir = path.join(work, "judging", `job-${++jobs}`);
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, "prompt.txt"), "Write answer.json.\n");
    return dir;
  }
  /** The stand-in's record of one kind (`argv`, `env`, `probe`), from the judge's answer file. */
  const recorded = (dir: string, kind: string) =>
    readFileSync(path.join(dir, "answer.json"), "utf8")
      .split("\n")
      .filter((line) => line.startsWith(`${kind} `))
      .map((line) => line.slice(kind.length + 1));
  /**
   * A judge's exit code, and whether the rig refused: any line of its stderr starting
   * `skill-scenarios: `, the prefix note() and fail() give every word of the rig's own. Equality
   * against the whole stderr string read a byte from docker, getent, mise or flock as a failed
   * row; what a row is about is that the judge ran and the rig refused nothing. A result that is
   * not clean carries the stderr that was read, so a failing row still names what went wrong; a
   * clean result reports no stderr.
   */
  const judged = (result: { exitCode: number; stderr: Buffer }) => {
    const stderr = result.stderr.toString();
    const refused = stderr.split("\n").some((line) => line.startsWith("skill-scenarios: "));
    const clean = !refused && result.exitCode === 0;
    return { code: result.exitCode, refused, stderr: clean ? "" : stderr };
  };
  /** What `judged` reads back from a judge that ran and a rig that refused nothing. */
  const judgedCleanly = { code: 0, refused: false, stderr: "" };

  // Catches: a judge session with the label's plugins, the operator's skills or the default model,
  // run outside its own directory, or with no record of its model.
  test.skipIf(skip)(
    "ste runs omp in the directory on its prompt, no plugins, asd-ste100 alone, the pinned model",
    () => {
      const dir = judgeDir();
      const result = judge("ste", dir);
      expect(judged(result)).toEqual(judgedCleanly);
      expect(recorded(dir, "argv")).toEqual([
        "/opt/omp/omp",
        "-p",
        "Write answer.json.",
        "--no-extensions",
        "--skills",
        "asd-ste100",
        "--model",
        "anthropic/claude-opus-4-8",
        "--thinking",
        "high",
        "--session-dir",
        `${dir}/sessions`,
      ]);
      expect(recorded(dir, "env")).toEqual([
        `PWD=${dir}`,
        "HOME=/home/judge",
        "OMP_PROFILE=judge-ste",
      ]);
      expect(readFileSync(path.join(dir, "out.txt"), "utf8")).toBe("judged\nexit=0\n");
      expect(readFileSync(path.join(dir, "judge-model.txt"), "utf8")).toBe(
        "model anthropic/claude-opus-4-8\nthinking high\n"
      );
      const skill = path.join(work, "judge-profiles/ste/home/.omp/profiles/judge-ste/agent/skills");
      expect(readFileSync(path.join(skill, "asd-ste100/SKILL.md"), "utf8")).toBe(
        readFileSync(path.join(root, "skills/asd-ste100/SKILL.md"), "utf8")
      );
      expect(
        readFileSync(
          path.join(work, "judge-profiles/ste/home/.omp/profiles/judge-ste/agent/models.yml"),
          "utf8"
        )
      ).toContain("https://api.anthropic.com");
    },
    judgeBound
  );

  // Catches: a judge that reads the run's transcript, other runs or the operator's files beside its
  // own directory (as a findings judge did, given no check), or that writes anything but its one
  // answer file and its sessions.
  test.skipIf(skip)(
    "a judge sees only its own directory, read-only, with its answer file and sessions writable",
    () => {
      const dir = judgeDir();
      expect(judge("ste", dir).exitCode).toBe(0);
      expect(Object.fromEntries(recorded(dir, "probe").map((line) => line.split(" ")))).toEqual({
        "read-outside": "denied",
        "read-home-ssh": "denied",
        "list-work": "denied",
        "read-own": "allowed",
        "write-own-dir": "denied",
        "write-sessions": "allowed",
        "read-skill": "allowed",
        "read-models": "allowed",
        "key-command": "allowed",
        "gateway-base-url": "denied",
        "token-is-jwt": "denied",
        "dbus-set": "denied",
      });
      expect(existsSync(path.join(dir, "new.txt"))).toBe(false);
    },
    judgeBound
  );

  // Catches: a bare judge that still loads skills, or a judge-omp that hides omp's failure.
  test.skipIf(skip)(
    "bare loads no skill, and judge-omp exits with omp's status",
    () => {
      const dir = judgeDir();
      writeFileSync(path.join(dir, "exit-status"), "3\n");
      const result = judge("bare", dir);
      expect(result.exitCode).toBe(3);
      expect(recorded(dir, "argv")).toContain("--no-skills");
      expect(recorded(dir, "argv")).not.toContain("--skills");
      expect(recorded(dir, "env")).toContain("OMP_PROFILE=judge-bare");
      expect(recorded(dir, "probe")).toContain("read-skill denied");
      expect(readFileSync(path.join(dir, "out.txt"), "utf8")).toBe("judged\nexit=3\n");
      expect(
        existsSync(
          path.join(work, "judge-profiles/bare/home/.omp/profiles/judge-bare/agent/skills")
        )
      ).toBe(false);
    },
    judgeBound
  );

  // Catches: a mount two judges share that one can write and the next can read, such as the key
  // command's call record, a path one judge could leave its private input in for another to read.
  // The first judge writes a marker into every directory it can write outside its own directory;
  // the second, of the same kind, looks for one anywhere it can read.
  test.skipIf(skip)(
    "one judge cannot leave a file that a second judge can read",
    () => {
      const first = judgeDir();
      const env = (word: string) => ({
        SKILL_SCENARIOS_JUDGE_ENV: `OUTSIDE=${outside} OPERATOR_HOME=${operatorHome} WORK=${work} ${word}`,
      });
      expect(judge("ste", first, "answer.json", env("DROP=one")).exitCode).toBe(0);
      const second = judgeDir();
      expect(judge("ste", second, "answer.json", env("SEEK=1")).exitCode).toBe(0);
      expect(recorded(second, "seek")).toEqual([""]);
    },
    judgeBound
  );

  // Catches: a judge kind the rig has no profile for, a directory with no prompt, and an answer
  // file name that escapes the judge's directory or names another of its files.
  test(
    "refuses another kind, a directory with no prompt.txt and a bad answer name",
    () => {
      expect(judge("plugins", judgeDir()).stderr.toString()).toContain("is ste or bare");
      expect(judge("ste", freshDir("judging-empty")).stderr.toString()).toContain(
        "has no prompt.txt"
      );
      for (const output of ["../answer.json", "sessions", "prompt.txt", "a/b.json", ""]) {
        const result = judge("ste", judgeDir(), output);
        expect(result.exitCode).not.toBe(0);
        expect(result.stderr.toString()).toContain("is not the name of an answer file");
      }
    },
    judgeBound
  );

  // Catches: a judge that truncates an earlier judge's out.txt or judge-model.txt in its
  // directory, or removes the key directory or the world (a judge in the pod's HOME and /tmp)
  // beside it that a judge killed outright left. Each is refused before anything else of the judge
  // runs (so these rows need no Docker), and kept.
  test("refuses a directory an earlier judge left its output, model record, key directory or world in, and keeps each", () => {
    for (const leftover of ["out.txt", "judge-model.txt", ".key", ".world"]) {
      const dir = judgeDir();
      const beside = leftover.startsWith(".") ? `${dir}${leftover}` : undefined;
      const file = beside === undefined ? path.join(dir, leftover) : path.join(beside, "token");
      mkdirSync(path.dirname(file), { recursive: true });
      writeFileSync(file, "an earlier judge's\n");
      const result = judge("ste", dir);
      expect(result.exitCode).toBe(1);
      expect(result.stderr.toString()).toContain(
        `judge-omp: ${beside ?? file} is there from an earlier judge of ${dir}; nothing of it is removed or replaced`
      );
      expect(readFileSync(file, "utf8")).toBe("an earlier judge's\n");
    }
  });

  // The route of a batch whose services are given (Verification v47's rule 1: no docker), which a
  // Legion pod has: the judge runs in the pod, in a namespace of its own (private_world) with a
  // HOME of its own, as a run's agent does. It has a work directory and a key file of its own, in
  // a directory that holds nothing the judge's world hides, since the profile's key command names
  // the key file it was built with. Catches: a judge-omp that needs docker where the services are
  // given; a judge that reads the operator's home, the work directory or a file beside its own
  // directory, or runs under the operator's HOME; a profile or key command it cannot read; and a
  // world or key directory left behind.
  test.skipIf(withoutNamespace)(
    "where the services are given, a judge runs in the pod, in its own namespace and HOME, and sees its directory alone",
    () => {
      const podWork = freshDir("judge-pod-work");
      mkdirSync(path.join(podWork, "runs"));
      const podOutside = path.join(podWork, "outside.txt");
      writeFileSync(podOutside, "a file beside the judge's directory\n");
      const key = path.join(freshDir("judge-pod-key"), "key");
      copyFileSync(keyFile(0o600), key);
      chmodSync(key, 0o600);
      const dir = path.join(podWork, "judging", "job-1");
      mkdirSync(dir, { recursive: true });
      writeFileSync(path.join(dir, "prompt.txt"), "Write answer.json.\n");
      const result = judge("ste", dir, "answer.json", {
        SKILL_SCENARIOS_WORK: podWork,
        SKILL_SCENARIOS_POSTGRES_URL: "postgres://rig@127.0.0.1:1/rig",
        SKILL_SCENARIOS_NATS_URL: "nats://127.0.0.1:1",
        LEGION_E2E_ANTHROPIC_KEY_FILE: key,
        SKILL_SCENARIOS_JUDGE_ENV: `OUTSIDE=${podOutside} OPERATOR_HOME=${operatorHome} WORK=${podWork}`,
      });
      expect(judged(result)).toEqual(judgedCleanly);
      expect(recorded(dir, "argv")).toEqual([
        omp,
        "-p",
        "Write answer.json.",
        "--no-extensions",
        "--skills",
        "asd-ste100",
        "--model",
        "anthropic/claude-opus-4-8",
        "--thinking",
        "high",
        "--session-dir",
        `${dir}/sessions`,
      ]);
      expect(recorded(dir, "env")).toEqual([
        `PWD=${dir}`,
        `HOME=${dir}.world/home`,
        "OMP_PROFILE=judge-ste",
      ]);
      // The namespace mounts no file, so the judge's directory is shown read-write whole.
      expect(Object.fromEntries(recorded(dir, "probe").map((line) => line.split(" ")))).toEqual({
        "read-outside": "denied",
        "read-home-ssh": "denied",
        "list-work": "denied",
        "read-own": "allowed",
        "write-own-dir": "allowed",
        "write-sessions": "allowed",
        "read-skill": "allowed",
        "read-models": "allowed",
        "key-command": "allowed",
        "gateway-base-url": "denied",
        "token-is-jwt": "denied",
        "dbus-set": "denied",
      });
      expect(readFileSync(path.join(dir, "out.txt"), "utf8")).toBe("judged\nexit=0\n");
      expect(existsSync(`${dir}.world`)).toBe(false);
      expect(existsSync(`${dir}.key`)).toBe(false);
    },
    judgeBound
  );

  // The gateway route with the real installer and key command, and a stand-in hawk-token first on
  // PATH that mints a JWT and counts each mint. The key command needs the operator's home and
  // session bus, so it runs on the host; the container gets only the token it printed.
  describe("on the gateway route", () => {
    // Staged in a hook, for the reason the describe above gives.
    let gatewayWork = "";
    let fakeBin = "";
    let mints = "";
    beforeAll(() => {
      gatewayWork = freshDir("judge-gateway-work");
      mkdirSync(path.join(gatewayWork, "runs"), { recursive: true });
      fakeBin = freshDir("judge-gateway-bin");
      mints = path.join(gatewayWork, "mints.log");
      writeFileSync(
        path.join(fakeBin, "hawk-token"),
        `#!/usr/bin/env bash
printf '%s\\n' "$$" >>"$FAKE_MINT_LOG"
payload=$(printf '{"exp":%s}' "$(($(date +%s) + 3600))" | base64 -w0 | tr '+/' '-_' | tr -d '=')
printf 'eyJhbGciOiJub25lIn0.%s.c2ln\\n' "$payload"
`,
        { mode: 0o755 }
      );
    });
    const gatewayJudge = (kind: string, dir: string, env: Record<string, string> = {}) =>
      judge(kind, dir, "answer.json", {
        PATH: `${fakeBin}:${childEnv.PATH}`,
        SKILL_SCENARIOS_WORK: gatewayWork,
        SKILL_SCENARIOS_JUDGE_ENV: `OUTSIDE=${outside} OPERATOR_HOME=${operatorHome} WORK=${gatewayWork}`,
        LEGION_E2E_MODEL_ROUTE: "gateway",
        LEGION_E2E_MODEL_GATEWAY_URL: "https://gateway.internal.example/anthropic",
        DBUS_SESSION_BUS_ADDRESS: "unix:path=/nonexistent/bus",
        FAKE_MINT_LOG: mints,
        ...env,
      });
    const mintCount = () =>
      existsSync(mints) ? readFileSync(mints, "utf8").split("\n").filter(Boolean).length : 0;
    function gatewayDir(): string {
      const dir = path.join(gatewayWork, "judging", `job-${++jobs}`);
      mkdirSync(dir, { recursive: true });
      writeFileSync(path.join(dir, "prompt.txt"), "Write answer.json.\n");
      return dir;
    }

    // Catches: a gateway judge that cannot start in its container, one that gets the operator's
    // home or session bus, one whose models.yml still names the host key command (which the
    // container cannot run), one that mints more than once per judge run, a token the judge can
    // read only by a path outside its mounts, and a token left on disk once the judge ends.
    test.skipIf(skip)(
      "a gateway judge starts in the container on a token the host minted once, and the token file is gone after",
      () => {
        // The first judge-omp call builds both profiles, and each install's preflight mints once.
        // Count only a later call's mints: one judge run is one call of the key command.
        expect(gatewayJudge("bare", gatewayDir()).exitCode).toBe(0);
        const dir = gatewayDir();
        const before = mintCount();
        const result = gatewayJudge("ste", dir);
        expect(judged(result)).toEqual(judgedCleanly);
        expect(mintCount() - before).toBeLessThanOrEqual(1);
        expect(recorded(dir, "env")).toEqual([
          `PWD=${dir}`,
          "HOME=/home/judge",
          "OMP_PROFILE=judge-ste",
        ]);
        expect(Object.fromEntries(recorded(dir, "probe").map((line) => line.split(" ")))).toEqual({
          "read-outside": "denied",
          "read-home-ssh": "denied",
          "list-work": "denied",
          "read-own": "allowed",
          "write-own-dir": "denied",
          "write-sessions": "allowed",
          "read-skill": "allowed",
          "read-models": "allowed",
          "key-command": "allowed",
          "gateway-base-url": "allowed",
          "token-is-jwt": "allowed",
          "dbus-set": "denied",
        });
        expect(existsSync(`${dir}.key`)).toBe(false);
        expect(readFileSync(path.join(dir, "answer.json"), "utf8")).not.toMatch(/eyJ[\w-]*\./);
      },
      judgeBound
    );

    // Catches: a token directory or file that others can read while the judge runs. The modes
    // are the judge's own reading of them from inside the container, recorded while it ran
    // (`recorded(dir, "mode")`), rather than a watcher's from outside: the stand-in omp sleeps
    // for nothing now, so there is no window for a watcher to look through, and a row that
    // waited for one would be waiting on a sleep.
    test.skipIf(skip)(
      "the token file is the owner's alone while the judge runs",
      () => {
        const dir = gatewayDir();
        const result = gatewayJudge("bare", dir);
        expect(judged(result)).toEqual(judgedCleanly);
        expect(recorded(dir, "mode")).toEqual(["key-dir 700", "token 600"]);
        expect(existsSync(`${dir}.key`)).toBe(false);
      },
      judgeBound
    );

    // Catches: a token file left behind when the judge fails, and a failed mint that starts the
    // judge anyway.
    test.skipIf(skip)(
      "removes the token file when the judge fails, and starts no judge when the key command fails",
      () => {
        const dir = gatewayDir();
        writeFileSync(path.join(dir, "exit-status"), "3\n");
        expect(gatewayJudge("bare", dir).exitCode).toBe(3);
        expect(existsSync(`${dir}.key`)).toBe(false);
        const refused = gatewayDir();
        const broken = freshDir("judge-gateway-broken");
        writeFileSync(path.join(broken, "hawk-token"), "#!/usr/bin/env bash\nexit 1\n", {
          mode: 0o755,
        });
        rmSync(path.join(gatewayWork, "judge-profiles/cache"), { recursive: true, force: true });
        for (const kind of ["ste", "bare"])
          rmSync(path.join(gatewayWork, `judge-profiles/${kind}/cache`), {
            recursive: true,
            force: true,
          });
        const result = gatewayJudge("ste", refused, { PATH: `${broken}:${childEnv.PATH}` });
        expect(result.exitCode).toBe(1);
        expect(result.stderr.toString()).toContain(
          "judge-omp: the gateway's key command gave no key"
        );
        expect(existsSync(path.join(refused, "answer.json"))).toBe(false);
      },
      judgeBound
    );
  });

  // Catches: a judge profile whose built-from records an empty commit line when the rig's root is
  // no checkout, so a judge's model run cannot be traced to the code that built it.
  test.skipIf(skip)(
    "fails, naming the rig's root, when the root is no checkout",
    () => {
      const top = freshDir("judge-root");
      const scripts = path.join(top, "packages/pi-legion/scripts/skill-scenarios");
      mkdirSync(scripts, { recursive: true });
      writeFileSync(
        path.join(scripts, "rig.sh"),
        readFileSync(path.join(import.meta.dir, "rig.sh"))
      );
      for (const file of [
        ".omp-pin",
        "skills/asd-ste100/SKILL.md",
        "packages/envoy/scripts/lib/pg-url.sh",
      ]) {
        mkdirSync(path.dirname(path.join(top, file)), { recursive: true });
        writeFileSync(path.join(top, file), readFileSync(path.join(root, file)));
      }
      mkdirSync(path.join(top, "scripts/e2e"), { recursive: true });
      cpSync(path.join(root, "scripts/e2e/lib"), path.join(top, "scripts/e2e/lib"), {
        recursive: true,
      });
      const rootWork = freshDir("judge-root-work");
      const result = spawnChecked(
        ["bash", path.join(scripts, "rig.sh"), "judge-omp", "ste", judgeDir(), "answer.json"],
        {
          env: {
            ...childEnv,
            USER: "rig",
            SKILL_SCENARIOS_WORK: rootWork,
            SKILL_SCENARIOS_JUDGE_OMP: omp,
            LEGION_E2E_MODEL_ROUTE: "key-file",
            LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
            GIT_CEILING_DIRECTORIES: path.dirname(top),
          },
        }
      );
      expect(result.exitCode).toBe(1);
      expect(result.stderr.toString()).toContain(`cannot read the commit of ${top} with jj or git`);
      expect(existsSync(path.join(rootWork, "judge-profiles/ready"))).toBe(false);
    },
    judgeBound
  );
});

// A spec run's plan row reads skill://writing-plans, which only a label built with
// SKILL_SCENARIOS_SUPERPOWERS holds (cmd_profile marks it with a `superpowers` file).
describe("require_superpowers", () => {
  const check = (label: string) =>
    bash(`${fn("fail")}\n${fn("require_superpowers")}\nP=$1\nrequire_superpowers spec-writing`, [
      label,
    ]);

  // Catches: a spec run on a label with no writing-plans, whose plan row would then fail for the
  // rig's fault and not the skill's.
  test("refuses a label built without SKILL_SCENARIOS_SUPERPOWERS, naming the variable", () => {
    const label = freshDir("label");
    const { code, stderr } = check(label);
    expect(code).toBe(1);
    expect(stderr).toContain("SKILL_SCENARIOS_SUPERPOWERS");
    writeFileSync(path.join(label, "superpowers"), "");
    expect(check(label).code).toBe(0);
  });
});

// `rig.sh identity <label>`, the whole script, on a label profile staged as cmd_profile leaves one:
// its directory under <work>/profiles, with its built-from, and its install under <work>/installs.
// Catches: an identity that names no profile tree, or whose digest moves with each model call (the
// route's call record, minted key and key command) or stays put when a plugin changes; a pack
// digest that is not the built-from's `skills` line; a rig revision that is not this checkout's;
// and a label with no built profile answered with three lines anyway.
describe("identity", () => {
  test("prints the label's profile tree digest, its pack digest and the rig's revision", () => {
    const idWork = freshDir("identity-work");
    const P = path.join(idWork, "profiles", "head");
    const I = path.join(idWork, "installs", "0a1b2c3d4e5f");
    mkdirSync(P, { recursive: true });
    const pack = "5".repeat(64);
    for (const [file, text] of Object.entries({
      [path.join(P, "ready")]: "",
      [path.join(P, "install")]: "0a1b2c3d4e5f\n",
      [path.join(P, "pin")]: "omp@1.0.0\n",
      [path.join(P, "built-from")]:
        `checkout /checkouts/head\ncommit ${"6".repeat(40)} parents=\nskills ${pack}\n`,
      [path.join(I, "home", ".omp", "profiles", "legion", "agent", "config.yml")]: "a: 1\n",
      [path.join(I, "pi-envoy", "dist", "index.js")]: "export {};\n",
      [path.join(I, "key", "anthropic-key.calls")]: "",
      [path.join(I, "cache", "hawk-token.key")]: "a token\n",
    })) {
      mkdirSync(path.dirname(file), { recursive: true });
      writeFileSync(file, text);
    }
    const identity = (label = "head") =>
      spawnChecked(["bash", path.join(import.meta.dir, "rig.sh"), "identity", label], {
        env: { ...childEnv, SKILL_SCENARIOS_WORK: idWork },
      });
    const first = identity();
    expect({ code: first.exitCode, stderr: first.stderr.toString() }).toEqual({
      code: 0,
      stderr: "",
    });
    const [tree = "", packLine, rig, ...rest] = first.stdout.toString().split("\n");
    const revision = bash([fn("checkout_commit"), 'checkout_commit "$1"'].join("\n"), [root]);
    expect({ tree: /^profile-tree [0-9a-f]{64}$/.test(tree), packLine, rig, rest }).toEqual({
      tree: true,
      packLine: `pack ${pack}`,
      rig: `rig ${revision.stdout.trim()}`,
      rest: [""],
    });
    // A model call rewrites the route's live files, and the tree digest stays.
    appendFileSync(path.join(I, "key", "anthropic-key.calls"), "a call\n");
    writeFileSync(path.join(I, "cache", "hawk-token.key"), `${fixtureSecret("hawk")}\n`);
    expect(identity().stdout.toString().split("\n")[0]).toBe(tree);
    // A plugin that changes moves it.
    writeFileSync(path.join(I, "pi-envoy", "dist", "index.js"), "export const a = 1;\n");
    expect(identity().stdout.toString().split("\n")[0]).not.toBe(tree);
    const unbuilt = identity("base");
    expect({ code: unbuilt.exitCode, stdout: unbuilt.stdout.toString() }).toEqual({
      code: 1,
      stdout: "",
    });
    expect(unbuilt.stderr.toString()).toContain("no profile for label base");
  });
});

// launch with a stand-in tmux that starts the pane's command a moment later, as a real tmux server
// does, and a stand-in mise that runs the agent it was given, here a script of the row's own in
// place of omp, inside the namespace launch.sh starts it in (world_paths, private_world). Every
// row that starts an agent needs that namespace and skips on a host that refuses it
// (namespaceRefusal); the rows of a refusal run everywhere.
describe("launch", () => {
  const standIns = `
tmux() { case $1 in new-session) (command sleep 0.3 && bash -c "\${@: -1}") & ;; esac; }
sleep() { command sleep 0.05; }`;
  /** The rig's own directory, which every run's world shows again. */
  const RIG = import.meta.dir;
  /** The caller's scratch root: every row's directories are under it, as a Legion pod's are. */
  const SCRATCH = process.env.TMPDIR ?? tmpdir();
  /** An unshare that refuses as a GitHub runner's does. */
  const REFUSAL = "unshare: write failed /proc/self/uid_map: Operation not permitted";
  const refusing = () => {
    const dir = freshDir("refusing-unshare");
    writeFileSync(path.join(dir, "unshare"), `#!/bin/sh\necho "${REFUSAL}" >&2\nexit 1\n`, {
      mode: 0o755,
    });
    return dir;
  };
  interface Launch {
    /** The run's label, whose profile and checkout it gets: head by default. */
    label?: string;
    /** Each label's checkout, as the work directory's profiles name it; the head's alone, a
     * directory of the row's own, by default. */
    checkouts?: Record<string, string>;
    /** The caller's variables over childEnv's; its TMPDIR, the scratch root, is SCRATCH unless
     * one is given. */
    env?: Record<string, string>;
    /** The pane's PATH; the stand-ins' and the caller's by default. */
    panePath?: string;
    /** Lines the pane file carries after its PATH and HOME. */
    pane?: string[];
    /** Whether the run gets rig.sh's stand-ins in $R/bin (standins) before its launch. */
    standins?: boolean;
    /** Files of the run's directory, each at its path under the run, written over launched's own
     * (prompt.txt) before the launch: system-args, bin/gh. */
    files?: Record<string, string>;
    /** Directories ahead of the stand-ins on the caller's PATH. */
    callerPath?: string[];
    /** The rig's own directory (`here`); this one by default. */
    here?: string;
    /** The role boot_agent launches the agent as, relaunching it as it would, in place of one
     * `launch`, the run's own sessions/ its session directory. */
    boot?: string;
  }
  /** The run directory a row's run `name` is made at: <work>/runs/<id>, an id of no meaning as
   * rig.sh's cmd_run gives one (here the name's digest, so a row can name it before the run). */
  const runDirOf = (work: string, name: string) =>
    path.join(work, "runs", new Bun.CryptoHasher("sha256").update(name).digest("hex").slice(0, 12));
  /** Where a label's profile is installed in a row's work directory (rig.sh's cmd_profile). */
  const installOf = (work: string, label: string) =>
    path.join(
      work,
      "installs",
      new Bun.CryptoHasher("sha256").update(label).digest("hex").slice(0, 12)
    );
  /** A work directory holding the run `name` at runDirOf, with its record beside it when the name
   * is one a run's record holds, the run's prompt and pane file, another live run and an archived
   * one, each with a file of its own, each label's profile naming its checkout and its install;
   * `agent` is the body of the shell script that stands in for omp. */
  function launched(
    work: string,
    name: string,
    agent: string,
    args: string[] = [],
    options: Launch = {}
  ) {
    const run = runDirOf(work, name);
    const bin = path.join(work, "bin");
    const label = options.label ?? "head";
    const checkouts = options.checkouts ?? { head: `${work}-checkout` };
    for (const dir of ["cwd", "sessions"]) mkdirSync(path.join(run, dir), { recursive: true });
    const identity = runIdentity(path.join("/", name));
    if (typeof identity !== "string")
      writeFileSync(
        `${run}.json`,
        JSON.stringify({
          run: path.basename(run),
          scenario: identity.scenario,
          case: identity.case,
          label: identity.label,
          n: identity.n,
          name,
          batch: "t-100",
          ran: run,
        })
      );
    writeFileSync(path.join(run, "prompt.txt"), "The person answered your asks on WID-2.\n");
    writeFileSync(
      path.join(run, "pane.env"),
      [
        `PATH=${options.panePath ?? `${bin}:${process.env.PATH ?? ""}`}`,
        `HOME=${run}/home`,
        ...(options.pane ?? []),
        "",
      ].join("\n")
    );
    for (const [file, text] of Object.entries(options.files ?? {})) {
      mkdirSync(path.dirname(path.join(run, file)), { recursive: true });
      writeFileSync(path.join(run, file), text);
    }
    writeFileSync(path.join(run, "out.txt"), "the first turn\nexit=0\n");
    writeFileSync(path.join(run, "daemon.log"), '{"at":"t","opened":"daemon.log"}\n');
    mkdirSync(path.join(bin, "pin", "bin"), { recursive: true });
    writeFileSync(path.join(bin, "pin", "bin", "omp"), `#!/bin/sh\n${agent}\n`, { mode: 0o755 });
    writeFileSync(
      path.join(bin, "mise"),
      `#!/bin/sh\nif [ "$1" = where ]; then echo "${bin}/pin"; exit 0; fi\nshift 3\nexec "$@"\n`,
      { mode: 0o755 }
    );
    for (const [dir, file] of [
      [runDirOf(work, "threads-closed-correct-head-9"), "pointer"],
      [path.join(work, "archive", "runs", "0a0b0c0d0e0f"), "pointer"],
    ] as const) {
      mkdirSync(dir, { recursive: true });
      writeFileSync(path.join(dir, file), `${dir}\n`);
    }
    mkdirSync(path.join(work, "services", "tokens"), { recursive: true });
    for (const [one, checkout] of Object.entries(checkouts)) {
      mkdirSync(path.join(work, "profiles", one), { recursive: true });
      mkdirSync(checkout, { recursive: true });
      mkdirSync(installOf(work, one), { recursive: true });
      writeFileSync(path.join(work, "profiles", one, "checkout"), `${checkout}\n`);
      writeFileSync(
        path.join(work, "profiles", one, "install"),
        `${path.basename(installOf(work, one))}\n`
      );
    }
    const result = bash(
      [
        fn("fail"),
        fn("pinned_omp"),
        fn("pane_var"),
        fn("system_gitconfig"),
        fn("world_paths"),
        fn("launch"),
        assignment("namespace_path"),
        quotedAssignment("private_world"),
        standIns,
        `R=$1 work=$2 tag=t pin=omp@pin P=$work/profiles/${label} I=${installOf(work, label)} co=$3 here=$4`,
        ...(options.standins === true ? [fn("standins"), "standins"] : []),
        ...(options.boot === undefined
          ? [`launch ${name} "\${@:5}"`]
          : [
              fn("note"),
              fn("boot_agent"),
              `sessions_dir=$R/sessions`,
              `boot_agent ${name} ${options.boot}`,
            ]),
      ].join("\n"),
      [run, work, checkouts[label] ?? "", options.here ?? RIG, ...args],
      {
        TMPDIR: SCRATCH,
        ...options.env,
        PATH: [...(options.callerPath ?? []), bin, childEnv.PATH].join(":"),
      }
    );
    return { run, result, out: readFileSync(path.join(run, "out.txt"), "utf8") };
  }
  /** The run's world as launch wrote it for its namespace ($R/namespace.txt). */
  const world = (run: string) =>
    readFileSync(path.join(run, "namespace.txt"), "utf8")
      .split("\n")
      .filter((line) => line !== "");

  // Catches: a later turn that reads the previous turn's `exit=` line as its own and returns at
  // once, and extra omp arguments placed anywhere but after the prompt.
  test.skipIf(withoutNamespace)(
    "a later turn passes its arguments to omp and waits for its own exit line",
    () => {
      const work = freshDir("launch-turn");
      const { result, out } = launched(work, "spec-writing-s1-1", 'echo "$0 $*"', ["--continue"]);
      expect(result.code).toBe(0);
      expect(out).toBe(
        `${work}/bin/pin/bin/omp -p The person answered your asks on WID-2. --continue --session-dir ${runDirOf(work, "spec-writing-s1-1")}/sessions\nexit=0\n`
      );
    }
  );

  // Catches: an agent whose `mise x <pin>` runs a mise other than the one rig_mise installed, or
  // finds none in its namespace, where the work directory that holds the rig's mise is covered.
  test.skipIf(withoutNamespace)(
    "the agent's namespace runs the rig's own mise, which its pane's PATH names and its world shows read-only",
    () => {
      const work = freshDir("mise-launch");
      const archive = miseRelease("v2099.1.2");
      const curl = curlStandIn();
      const installed = rigMise(miseRoot("v2099.1.2", fileSha256(archive)), work, {
        PATH: `${curl}:${childEnv.PATH}`,
        CURL_LOG: path.join(curl, "urls"),
        CURL_SERVES: archive,
      });
      expect({ code: installed.code, stderr: installed.stderr }).toEqual({ code: 0, stderr: "" });
      const tools = path.join(work, "tools", "bin");
      const { run, result, out } = launched(
        work,
        "spec-writing-s1-1",
        'echo "ran by $MISE_RAN"',
        [],
        {
          panePath: `${tools}:${process.env.PATH ?? ""}`,
        }
      );
      expect(result.code).toBe(0);
      expect(out).toBe(`ran by ${tools}/mise\nexit=0\n`);
      expect(world(run)).toContain(`ro ${tools}`);
    }
  );

  /** The line the plugin prints when the Envoy listener refuses the claim of a session that holds
   * no consumer, after its registration (round 23's, at 2926519c). */
  const ENVOY_REFUSAL =
    "claim boot failed at claimRole after claims/register registered legion-fernbrookwidgets0123456789ab-wid-1-reviewer with the daemon at http://127.0.0.1:4321; exiting so the daemon launches it again: the session asked for no session consumer: a session receives only what its own consumer holds, so register with delivery.consumer true";
  /** An omp that stands in for a reviewer phase worker and its plugin, each launch of it doing
   * what its line of `$R/plan` names: `envoy`, registered with the daemon stand-in and refused by
   * the listener at its claim (ENVOY_REFUSAL); `register`, refused 401 by the stand-in's
   * `claims/register`; `late`, refused as `envoy` is once its first turn is in its transcript;
   * `works`, a turn and a completion the stand-in took. Each writes the stand-in's lines of it to
   * $R/daemon.log, as the stand-in does, and its transcript under the session directory it is
   * given, and counts its launches in $R/attempts. */
  const BOOTING = [
    'R=$(dirname "$HOME")',
    'n=$(( $(cat "$R/attempts" 2>/dev/null || echo 0) + 1 ))',
    'echo "$n" >"$R/attempts"',
    'for word; do [ "$previous" = --session-dir ] && sessions=$word; previous=$word; done',
    `logged() { printf '{"at":"%s","method":"POST","path":"/legion/v1/%s","status":%s%s,"role":"reviewer"}\\n' "$(date -u +%FT%T.%3NZ)" "$1" "$2" "\${3:-}" >>"$R/daemon.log"; }`,
    `opened() { printf '{"type":"session","id":"s%s"}\\n{"type":"message","message":{"role":"user","content":[{"type":"text","text":"Review it."}]}}\\n' "$n" >"$sessions/s$n.jsonl"; }`,
    `turned() { printf '{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Reading the pull request."}]}}\\n' >>"$sessions/s$n.jsonl"; }`,
    'case $(sed -n "$n"p "$R/plan") in',
    `envoy) logged claims/register 200; opened; echo "[legion] ${ENVOY_REFUSAL}" >&2; exit 1 ;;`,
    `register) logged claims/register 401 ',"code":"INVALID_BOOT_TOKEN"'; opened; echo "[legion] claims/register registration failed (401): Invalid boot token" >&2; exit 1 ;;`,
    `late) logged claims/register 200; opened; turned; echo "[legion] ${ENVOY_REFUSAL}" >&2; exit 1 ;;`,
    "works) logged claims/register 200; opened; turned; logged handoff/complete 200; echo reviewed ;;",
    "esac",
  ].join("\n");
  /** The reviewer-text-findings run `name` whose reviewer meets `plan`, booted by boot_agent:
   * boot_agent's result, the run, its first attempt's record, how many launches it made, the
   * run's boot record, and the verdict `rig.sh health` gives the run. The run carries the
   * built-from and services-from cmd_run copies into it before its agent starts, naming the
   * label's checkout (launched's), which holds this checkout's plugin claim core (health reads a
   * claim boot refusal by the sentence of the run's own checkout's), the boot record worker_boot
   * leaves before boot_agent launches the agent, and the log of the Dispatch own_dispatch starts
   * for it then, which no request of the stand-in agent reaches. */
  function booted(name: string, plan: string[]) {
    const work = freshDir("boot-agent");
    const run = runDirOf(work, name);
    const checkout = `${work}-checkout`;
    const core = path.join("packages", "pi-shared", "src", "legion-claim-core.ts");
    mkdirSync(path.dirname(path.join(checkout, core)), { recursive: true });
    cpSync(path.join(CHECKOUT, core), path.join(checkout, core));
    mkdirSync(run, { recursive: true });
    writeFileSync(path.join(run, "plan"), `${plan.join("\n")}\n`);
    writeFileSync(path.join(run, "built-from"), `checkout ${checkout}\n`);
    writeFileSync(path.join(run, "services-from"), servicesFrom(checkout));
    writeFileSync(path.join(run, "booted"), "boot reviewer\n");
    mkdirSync(path.join(run, "logs"));
    writeFileSync(path.join(run, "logs", "dispatch.log"), `${LISTENING}\n`);
    writeFileSync(path.join(run, "logs", "github.log"), GITHUB_RECORD);
    writeFileSync(path.join(run, "logs", "heads.log"), HEADS_OPENED);
    const { result } = launched(work, name, BOOTING, [], { boot: "reviewer" });
    const health = spawnChecked(["bash", path.join(RIG, "rig.sh"), "health", run], {
      env: { ...childEnv, SKILL_SCENARIOS_WORK: freshDir("boot-health") },
    });
    return {
      result,
      run,
      id: path.basename(run),
      first: path.join(run, "boot-1", "reviewer"),
      attempts: readFileSync(path.join(run, "attempts"), "utf8").trim(),
      record: readFileSync(path.join(run, "booted"), "utf8"),
      verdict: health.stdout
        .toString()
        .split("\n")
        .find((line) => line.startsWith(`${path.basename(run)}  `)),
    };
  }

  // Catches: an agent whose claim boot was refused before its first turn left as the run's
  // record, where production's daemon launches it again (round 23's base reviewer, which the
  // listener refused); the first attempt dropped, or left where the score and health read the
  // run's own record; and its refusal read as the run's, whether the listener refused the claim
  // after the registration or the stand-in refused the registration itself.
  test.skipIf(withoutNamespace)(
    "a worker refused at its claim boot before its first turn is launched once more, both attempts kept, and the run is healthy",
    () => {
      for (const [refused, kept] of [
        ["envoy", ENVOY_REFUSAL],
        ["register", "claims/register registration failed (401): Invalid boot token"],
      ] as const) {
        const name = "reviewer-text-findings-text-only-head-1";
        const { result, run, id, first, attempts, record, verdict } = booted(name, [
          refused,
          "works",
        ]);
        expect(result).toMatchObject({ code: 0, stderr: "" });
        expect(result.stdout).toContain(
          `skill-scenarios: ${id}: the reviewer's claim boot was refused before its first turn (`
        );
        expect(attempts).toBe("2");
        // Each attempt on the run's boot record, the relaunch's line added before it started; the
        // first, which the reader was asked about, followed by the `since` it was asked with.
        expect(record).toMatch(
          /^boot reviewer\nsince reviewer \d{4}-\d\d-\d\dT[\d:.]+Z\nboot reviewer\n$/
        );
        // The first attempt, whole, beside the second: its output, its transcript and the
        // stand-in's lines of it.
        expect(readFileSync(path.join(first, "out.txt"), "utf8")).toBe(
          `[legion] ${kept}\nexit=1\n`
        );
        expect(readdirSync(path.join(first, "sessions"))).toEqual(["s1.jsonl"]);
        expect(
          readFileSync(path.join(first, "daemon.log"), "utf8")
            .trim()
            .split("\n")
            .map((line) => JSON.parse(line).status)
        ).toEqual([refused === "envoy" ? 200 : 401]);
        // The second where any agent's record stands.
        expect(readFileSync(path.join(run, "out.txt"), "utf8")).toBe("reviewed\nexit=0\n");
        expect(readdirSync(path.join(run, "sessions"))).toEqual(["s2.jsonl"]);
        expect(verdict).toBe(`${id}  healthy  (${name})`);
      }
    }
  );

  // Catches: a relaunch the rig repeats until the agent boots, which is no budget of this rig's;
  // and a run refused on its relaunch too judged healthy on the first refusal's declaration, or
  // named by one attempt alone. The stand-in's refusal of each attempt is told apart by the lines
  // the first attempt's record keeps.
  test.skipIf(withoutNamespace)(
    "a worker refused at both attempts is launched twice and no more, and the run is unhealthy, naming both",
    () => {
      const name = "reviewer-text-findings-text-only-base-1";
      const { result, id, first, attempts, record, verdict } = booted(name, [
        "register",
        "register",
        "works",
      ]);
      expect(result).toMatchObject({ code: 0, stderr: "" });
      expect(attempts).toBe("2");
      expect(record).toMatch(/^boot reviewer\nsince reviewer \S+Z\nboot reviewer\n$/);
      expect(existsSync(path.join(first, "out.txt"))).toBe(true);
      const answered =
        "the daemon stand-in answered the reviewer's POST /legion/v1/claims/register 401 INVALID_BOOT_TOKEN";
      expect(verdict).toBe(
        `${id}  unhealthy: claim boot refused on both attempts: the reviewer's first (<run>/boot-1/reviewer/out.txt): ${answered}; its relaunch (<run>/out.txt): ${answered}  (${name})`
      );
    }
  );

  // Catches: an agent relaunched after its first turn, which the daemon's relaunch would not take
  // back, and such a run read as healthy.
  test.skipIf(withoutNamespace)(
    "a worker refused after its first turn is not relaunched, and the run is unhealthy",
    () => {
      const name = "reviewer-text-findings-defect-head-1";
      const { result, run, id, first, attempts, record, verdict } = booted(name, ["late", "works"]);
      expect(result).toMatchObject({ code: 0, stderr: "" });
      expect(result.stdout).not.toContain("claim boot was refused");
      expect(attempts).toBe("1");
      expect(record).toMatch(/^boot reviewer\nsince reviewer \S+Z\n$/);
      expect(existsSync(first)).toBe(false);
      expect(readFileSync(path.join(run, "out.txt"), "utf8")).toBe(
        `[legion] ${ENVOY_REFUSAL}\nexit=1\n`
      );
      expect(verdict).toBe(
        `${id}  unhealthy: claim boot refused: ${ENVOY_REFUSAL} (<run>/out.txt), and the agent was not relaunched: the rig relaunches an agent once when its claim boot is refused before its first turn, and never after it  (${name})`
      );
    }
  );

  // Catches: an agent that can list and read the other runs of its batch, live or archived, which
  // the 2026-11-14 agents did for a sibling's pointer file; one that writes into the /tmp every
  // run shares; one that runs as uid 0; and a namespace that binds the run's directory in the
  // wrong order, which leaves the host's run directory without a file the agent wrote there, exit
  // 0. The scratch root is a directory elsewhere, so the work directory's own cover is what hides
  // the other runs.
  test.skipIf(withoutNamespace)(
    "the agent sees its own run alone and its own /tmp, as this user, and its writes land in the run",
    () => {
      const work = freshDir("launch-world");
      const marker = `launch-marker-${path.basename(work)}`;
      const name = "threads-closed-correct-head-1";
      const own = runDirOf(work, name);
      const sibling = runDirOf(work, "threads-closed-correct-head-9");
      const others = `${sibling}/pointer ${work}/archive/runs/0a0b0c0d0e0f/pointer`;
      const { run, result, out } = launched(
        work,
        name,
        [
          `echo "uid=$(id -u)"`,
          `echo "runs=$(ls ${work}/runs)"`,
          `echo "archive=$(ls -A ${work}/archive 2>/dev/null)"`,
          `for f in ${others}; do cat "$f" >/dev/null 2>&1 && echo "read $f"; done`,
          `echo "profile=$(cat ${work}/profiles/head/checkout 2>/dev/null)"`,
          `echo "log=$(cat ${own}/daemon.log)"`,
          `echo scratch >/tmp/${marker}`,
          `echo record >${own}/agent-wrote`,
        ].join("\n"),
        [],
        { env: { TMPDIR: freshDir("launch-elsewhere") } }
      );
      expect(result.code).toBe(0);
      expect(out.split("\n")).toEqual([
        `uid=${process.getuid?.()}`,
        `runs=${path.basename(own)}`,
        "archive=",
        // The label's profile is under the work directory's cover, and no path shows it again.
        "profile=",
        'log={"at":"t","opened":"daemon.log"}',
        "exit=0",
        "",
      ]);
      // On the host, after the agent exited: what it wrote is in the run, its /tmp is the run's
      // own, and the machine's /tmp and the other runs are as they were.
      expect(readFileSync(path.join(run, "agent-wrote"), "utf8")).toBe("record\n");
      expect(readFileSync(path.join(run, "daemon.log"), "utf8")).toBe(
        '{"at":"t","opened":"daemon.log"}\n'
      );
      expect(readFileSync(path.join(run, "tmp", marker), "utf8")).toBe("scratch\n");
      expect(existsSync(path.join("/tmp", marker))).toBe(false);
      expect(readdirSync(path.join(work, "runs")).sort()).toEqual(
        [path.basename(own), `${path.basename(own)}.json`, path.basename(sibling)].sort()
      );
    }
  );

  // Catches: a shown path that is a link whose target lies under a cover, bound back at the link
  // itself: the link stays visible, its target is covered, and `mkdir -p` on the dangling link
  // answers `File exists`, so no run starts. Measured by the tester on 2026-11-15 with the rig's
  // own node_modules/zod, a link into the root's .bun store under a covered workspace; a run's
  // world now shows no checkout, and the links it shows are the pane's tool directories, as here
  // one whose target lies under the scratch root. And a gh stand-in that needs the rig's
  // directory or its zod, which the world no longer shows: it runs from its bundle in the run.
  // The covered root sorts after the link, as /legion/workspaces sorts after /legion/scratch, so
  // the link is bound back before its target is.
  test.skipIf(withoutNamespace)(
    "a tool directory that links into a covered root is shown at both paths, and the gh stand-in runs from its bundle",
    () => {
      const work = freshDir("launch-linked");
      const scratch = freshDir("launch-linked-workspace");
      const store = path.join(scratch, "root", "tools");
      mkdirSync(store, { recursive: true });
      writeFileSync(path.join(store, "hello"), "#!/bin/sh\necho hello\n", { mode: 0o755 });
      const tree = freshDir("launch-linked-tree");
      symlinkSync(store, path.join(tree, "bin"));
      const { run, result, out } = launched(
        work,
        "threads-closed-correct-head-1",
        `hello; "$(dirname "$HOME")/bin/gh" --open; echo "gh=$?"`,
        [],
        {
          env: { TMPDIR: scratch },
          panePath: `${path.join(work, "bin")}:${path.join(tree, "bin")}:${process.env.PATH ?? ""}`,
          standins: true,
        }
      );
      expect(out).toBe("hello\ngh=0\nexit=0\n");
      expect(result.code).toBe(0);
      expect(readFileSync(path.join(run, "calls.jsonl"), "utf8")).toContain('"opened"');
      // The world shows the link where the PATH names it and the directory it resolves to.
      expect(world(run)).toContain(`ro ${path.join(tree, "bin")}`);
      expect(world(run)).toContain(`ro ${realpathSync(store)}`);
      expect(world(run).filter((line) => line.includes(RIG))).toEqual([]);
    }
  );

  // Catches: a bun stand-in that records no tree, so no own run of the world's check can be told
  // from a run at another commit (score.ts's `ownRun`): it records the directory bun ran in and
  // that directory's `git rev-parse HEAD`, and an empty head outside a repository.
  test.skipIf(withoutNamespace)(
    "the bun stand-in records the directory it ran in and that directory's commit",
    () => {
      const work = freshDir("launch-bun");
      const name = "threads-closed-correct-head-2";
      const tree = path.join(runDirOf(work, name), "cwd", "tree");
      mkdirSync(tree, { recursive: true });
      const git = (...args: string[]) =>
        spawnChecked(["git", "-C", tree, "-c", "user.name=n", "-c", "user.email=n@n", ...args], {
          env: childEnv,
        })
          .stdout.toString()
          .trim();
      git("init", "-q");
      git("commit", "-q", "--allow-empty", "-m", "a");
      const commit = git("rev-parse", "HEAD");
      const { run, result, out } = launched(
        work,
        name,
        'cd tree && "$(dirname "$HOME")/bin/bun" -e 0 Ada; cd .. && "$(dirname "$HOME")/bin/bun" -e 0; echo ran',
        [],
        { standins: true }
      );
      expect({ code: result.code, out }).toEqual({ code: 0, out: "ran\nexit=0\n" });
      const bunCalls = readFileSync(path.join(run, "calls.jsonl"), "utf8")
        .trim()
        .split("\n")
        .map((line) => JSON.parse(line))
        .filter((call) => call.as === "bun")
        .map(({ argv, cwd, head }) => ({ argv, cwd, head }));
      expect(bunCalls).toEqual([
        { argv: ["-e", "0", "Ada"], cwd: tree, head: commit },
        { argv: ["-e", "0"], cwd: path.join(run, "cwd"), head: "" },
      ]);
    }
  );

  // Catches: a /tmp one run shares with the next: a file the first run's agent writes to /tmp is
  // absent from the second run's.
  test.skipIf(withoutNamespace)("a file one run writes to /tmp is absent in the next run", () => {
    const work = freshDir("launch-next");
    const marker = `next-marker-${path.basename(work)}`;
    const first = launched(work, "threads-closed-correct-head-1", `echo first >/tmp/${marker}`);
    expect(first.result.code).toBe(0);
    const second = launched(
      work,
      "threads-closed-correct-head-2",
      `cat /tmp/${marker} 2>/dev/null || echo absent`
    );
    expect(second.out).toBe("absent\nexit=0\n");
    expect(existsSync(path.join("/tmp", marker))).toBe(false);
  });

  // Catches: a cover of the work directory's runs and archive alone, the namespace before this
  // one, which left every other file of the scratch root readable: on a Legion pod, every other
  // role's scratch and every extraction of a tree.
  test.skipIf(withoutNamespace)(
    "a file in the scratch root outside the run is absent in it",
    () => {
      const work = freshDir("launch-scratch");
      const planted = path.join(freshDir("another-role"), "notes.md");
      writeFileSync(planted, "another role's notes\n");
      const { run, result, out } = launched(
        work,
        "threads-closed-correct-head-1",
        `cat ${planted} 2>/dev/null || echo absent`
      );
      expect(result.code).toBe(0);
      expect(out).toBe("absent\nexit=0\n");
      expect(world(run)).toContain(`cover ${realpathSync(SCRATCH)}`);
    }
  );

  // Catches: a world whose declaration drops a path a run needs (the label's install, the
  // batch's tokens, a tool directory, mise's installs, the pin, the push guard's check), or shows
  // one no run of the new world needs (the label's checkout, the rig's directory, zod, the whole
  // services directory), a namespace that hides a declared path rather than showing it again,
  // and one that shows a read-only path writable. Every fixture path lies under the scratch
  // root, so each is hidden unless the world shows it again.
  test.skipIf(withoutNamespace)(
    "every path the run's world shows is there inside it, writable only where declared rw",
    () => {
      const work = freshDir("launch-declared");
      const mise = freshDir("mise-data");
      const guard = path.join(freshDir("leak-check"), "public-leak-check.sh");
      writeFileSync(guard, "#!/bin/sh\nexit 0\n", { mode: 0o755 });
      const install = installOf(work, "head");
      mkdirSync(path.join(install, "key"), { recursive: true });
      const name = "threads-closed-correct-head-1";
      const R = runDirOf(work, name);
      const needed = [
        `rw ${R}`,
        `ro ${install}`,
        `rw ${install}/key`,
        `rw ${install}/cache`,
        `ro ${work}/services/tokens`,
        `ro ${work}/bin`,
        `ro ${mise}`,
        `ro ${work}/bin/pin`,
        `ro ${path.dirname(guard)}`,
      ];
      // The agent probes the paths this row needs shown, listed in a file of the run: the world's
      // own declaration, namespace.txt, is one of the rig's records the run's agents cannot read.
      const { result, out } = launched(
        work,
        name,
        [
          `while read -r kind p; do`,
          `  [ -d "$p" ] || { echo "$kind $p absent"; continue; }`,
          `  if touch "$p/.probe" 2>/dev/null; then rm -f "$p/.probe"; echo "$kind $p writable"; else echo "$kind $p read-only"; fi`,
          `done <${R}/probe.txt`,
        ].join("\n"),
        [],
        {
          pane: [`MISE_DATA_DIR=${mise}`, `PUBLIC_LEAK_CHECK=${guard}`],
          files: { "probe.txt": `${needed.join("\n")}\n` },
        }
      );
      expect(result.code).toBe(0);
      const declared = world(R).filter((line) => /^r[ow] /.test(line));
      for (const line of needed) expect(declared).toContain(line);
      for (const hidden of [`${work}-checkout`, RIG, `${work}/services`, `${work}/profiles`])
        expect(declared.filter((line) => line === `ro ${hidden}`)).toEqual([]);
      // Inside the run, each needed path the host has is there, writable where it is rw.
      const expected = needed.map((line) => {
        const [kind = "", where = ""] = [line.slice(0, 2), line.slice(3)];
        if (!existsSync(where) || !statSync(where).isDirectory()) return `${kind} ${where} absent`;
        return `${kind} ${where} ${kind === "rw" ? "writable" : "read-only"}`;
      });
      expect(out.split("\n")).toEqual([...expected, "exit=0", ""]);
    }
  );

  // Catches: the services' logs readable by a run's agents in the run's own directory, and a hide
  // that lets the agent write into the host's; and a guard that reads the rig's records as an
  // agent's words, or reads services-from with the rig's own directory in it, which would refuse
  // every run, since services-from names that directory in every one (run-names.ts's
  // `worldFiles`). The records that are files stay readable: this gVisor host refuses a mount onto
  // a file.
  test.skipIf(withoutNamespace)(
    "the services' logs read empty inside a run and stay whole outside it, and the guard reads services-from less the rig's own directory",
    () => {
      const work = freshDir("launch-records");
      const name = "threads-closed-correct-head-1";
      const R = runDirOf(work, name);
      const records = {
        "services-from": `gh-standin.ts shared ${realpathSync(RIG)} -\n`,
        "built-from": "checkout /co\n",
        "logs/dispatch.log": "a line of the server's\n",
      };
      const { result, out } = launched(
        work,
        name,
        [
          `echo "logs=$(ls -A ${R}/logs)"`,
          `{ echo over >${R}/logs/dispatch.log; } 2>/dev/null && echo wrote || echo refused`,
        ].join("\n"),
        [],
        { files: records }
      );
      expect(result).toMatchObject({ code: 0, stderr: "" });
      expect(out).toBe("logs=\nrefused\nexit=0\n");
      for (const [file, text] of Object.entries(records))
        expect(readFileSync(path.join(R, file), "utf8")).toBe(text);
    }
  );

  // Catches: a record of the run's launches an agent of the run can write. The guard tells a run's
  // first launch from a later one by it (run-names.ts's `firstLaunch`), and reads what an agent
  // can write (AGENT_WRITTEN) at the first alone: an agent that could empty it would have the next
  // launch read its own words as the rig's, and one that could add a line would have the next
  // launch pass over what the rig wrote. launch appends its line once the guard has passed, and
  // <run>/logs is under an empty read-only tmpfs in the agent's namespace (world_paths' `hide`).
  // The agent tries the file, the directory, and a user and mount namespace of its own, in which a
  // mount its namespace was made with cannot be taken off. After two launches the record holds the
  // rig's two lines, and none of the agent's.
  test.skipIf(withoutNamespace)(
    "an agent of the run cannot write the run's record of its launches",
    () => {
      const work = freshDir("launch-record");
      const name = "threads-closed-correct-head-1";
      const R = runDirOf(work, name);
      const record = path.join(R, "logs", "launches");
      const agent = [
        `(echo forged >>${record}) 2>/dev/null && echo appended || echo refused`,
        `(: >${record}) 2>/dev/null && echo emptied || echo kept`,
        `mv ${R}/logs ${R}/logs-moved 2>/dev/null && echo moved || echo stayed`,
        `unshare -Urm sh -c 'umount -l ${R}/logs; echo forged >>${record}' 2>/dev/null && echo unshared || echo held`,
      ].join("\n");
      const launches = [launched(work, name, agent), launched(work, name, agent)];
      const line = /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z t-threads-closed-correct-head-1$/;
      expect({
        launches: launches.map(({ result, out }) => [result.code, out]),
        record: existsSync(record) ? readFileSync(record, "utf8").split("\n") : "no record",
      }).toEqual({
        launches: [0, 0].map((code) => [code, "refused\nkept\nstayed\nheld\nexit=0\n"]),
        record: [expect.stringMatching(line), expect.stringMatching(line), ""],
      });
    }
  );

  // Catches: a namespace that hides the push guard's shared check, under which the guard refuses
  // every push (`LEAK CHECK SETUP ERROR`), as tester v21's batch met it. The check here is a copy
  // of the pod's shared one under the scratch root, so only the world's showing it again keeps
  // it; the pod's own, outside the scratch root, must be reachable too.
  const shared = process.env.PUBLIC_LEAK_CHECK ?? "";
  test.skipIf(
    withoutNamespace ||
      !existsSync("/opt/legion/bin/git") ||
      !existsSync("/opt/legion/bin/legion") ||
      !existsSync(shared)
  )("the push guard's check is reachable inside a run, and a push from it passes the guard", () => {
    const work = freshDir("launch-guard");
    const copy = path.join(freshDir("guard-copy"), "leak-check");
    cpSync(path.dirname(shared), copy, { recursive: true });
    const check = path.join(copy, path.basename(shared));
    const name = "threads-closed-correct-head-1";
    const R = runDirOf(work, name);
    const git = "/usr/bin/git -c user.name=A -c user.email=a@example.invalid";
    const { result, out } = launched(
      work,
      name,
      [
        `test -r ${shared} && echo "shared reachable"`,
        `test -r "$PUBLIC_LEAK_CHECK" && echo "copy reachable"`,
        `${git} init -q --bare ${R}/remote.git && ${git} init -q ${R}/src`,
        `echo "The widgets CLI." >${R}/src/README.md`,
        `${git} -C ${R}/src add -A && ${git} -C ${R}/src commit -qm init`,
        `git -C ${R}/src push ${R}/remote.git HEAD:refs/heads/main >${R}/push.txt 2>&1; echo "push=$?"`,
      ].join("\n"),
      [],
      { pane: [`HOME=${R}/home`, `PUBLIC_LEAK_CHECK=${check}`] }
    );
    expect(result.code).toBe(0);
    const push = readFileSync(path.join(R, "push.txt"), "utf8");
    expect({ out, setup: push.includes("LEAK CHECK SETUP ERROR") }).toEqual({
      out: "shared reachable\ncopy reachable\npush=0\nexit=0\n",
      setup: false,
    });
  });

  // Catches: a namespace whose first process inherits the caller's environment: with a PID
  // namespace of its own the run's PID 1 is that process until it execs the agent, and the
  // 2026-11-14 probe read six DISPATCH_* lines of the caller's there. The unshare here records
  // the environment it was started with before it runs the real one; the agent reads PID 1's.
  test.skipIf(withoutNamespace)(
    "the namespace starts with the system PATH alone, and PID 1 names no variable of the caller's",
    () => {
      const work = freshDir("launch-environ");
      const wrapper = freshDir("recording-unshare");
      const seen = path.join(wrapper, "environment");
      writeFileSync(
        path.join(wrapper, "unshare"),
        `#!/bin/sh\nenv >${seen}\nexec ${Bun.which("unshare") ?? "/usr/bin/unshare"} "$@"\n`,
        { mode: 0o755 }
      );
      const marker = `CALLER_MARKER_${path.basename(work).replace(/\W/g, "_")}`;
      const { result, out } = launched(
        work,
        "threads-closed-correct-head-1",
        `echo "marked=$(tr '\\0' '\\n' </proc/1/environ | grep -c ${marker})"`,
        [],
        {
          env: { [marker]: "the caller's", DISPATCH_URL: "http://127.0.0.1:1" },
          callerPath: [wrapper],
        }
      );
      expect(result.code).toBe(0);
      expect(out).toBe("marked=0\nexit=0\n");
      const names = readFileSync(seen, "utf8")
        .split("\n")
        .filter((line) => line.includes("="))
        .map((line) => line.slice(0, line.indexOf("=")));
      expect(names.filter((one) => one === marker || one.startsWith("DISPATCH_"))).toEqual([]);
      expect(readFileSync(seen, "utf8")).toContain(
        `\nPATH=${assignment("namespace_path").slice("namespace_path=".length)}\n`.slice(1)
      );
    }
  );

  // Catches: a run that shares the pod's processes, as one with no PID namespace does: it could
  // read every same-user process's environment and every process's command line, the tester's
  // role prompt among them. The row starts a process of its own outside the run whose command
  // line carries that prompt's words.
  test.skipIf(withoutNamespace)(
    "inside a run /proc lists the run's own processes alone",
    async () => {
      const work = freshDir("launch-pids");
      const outside = Bun.spawn(["sh", "-c", "sleep 30", "Legion Tester Deployment instructions"]);
      try {
        const name = "threads-closed-correct-head-1";
        const R = runDirOf(work, name);
        const { result, out } = launched(
          work,
          name,
          [
            `for d in /proc/[0-9]*; do`,
            `  line=$(tr '\\0' ' ' <"$d/cmdline" 2>/dev/null) || continue`,
            `  case $line in *"Legion Tester"* | *"Deployment instructions"*) echo "marked \${d#/proc/}" ;; esac`,
            `  vars=$(tr '\\0' '\\n' <"$d/environ" 2>/dev/null) || continue`,
            `  printf '%s\\n' "$vars" | grep -qxF "HOME=${R}/home" || echo "foreign \${d#/proc/}"`,
            `done`,
            `echo listed`,
          ].join("\n")
        );
        expect(result.code).toBe(0);
        expect(out).toBe("listed\nexit=0\n");
      } finally {
        outside.kill();
        await outside.exited;
      }
    }
  );

  // Catches: a run that can read the pod's credentials: Legion's role secrets and private state
  // under /var/run/legion and the cluster's service account under /var/run/secrets; one that
  // loses the model route's own key, which every production agent's Oh My Pi reads; and one that
  // can read the issue workspaces of the pod's root, where nothing but what its world shows
  // again (the rig's own directory, which lies in this checkout) is a run's.
  test.skipIf(withoutNamespace)(
    "inside a run the pod's credentials and workspaces are hidden, and the model route's key is not",
    () => {
      const work = freshDir("launch-credentials");
      const name = "threads-closed-correct-head-1";
      const R = runDirOf(work, name);
      const { run, result, out } = launched(
        work,
        name,
        [
          `test -e /var/run/legion/private && echo "legion private" || echo "no legion private"`,
          `echo "secrets=$(ls -A /var/run/secrets 2>/dev/null | tr '\\n' ' ')"`,
          `test -e /run/legion/private && echo "run legion private" || echo "no run legion private"`,
          `echo "run secrets=$(ls -A /run/secrets 2>/dev/null | tr '\\n' ' ')"`,
          `test -e /var/run/model-gateway-key/key && echo "gateway key" || echo "no gateway key"`,
          `find /legion/workspaces -type f >${R}/workspaces.txt 2>/dev/null || true`,
        ].join("\n")
      );
      expect(result.code).toBe(0);
      expect(out.split("\n")).toEqual([
        "no legion private",
        "secrets=",
        "no run legion private",
        "run secrets=",
        existsSync("/var/run/model-gateway-key/key") ? "gateway key" : "no gateway key",
        "exit=0",
        "",
      ]);
      const shown = world(run)
        .filter((line) => /^r[ow] /.test(line))
        .map((line) => line.slice(3));
      const found = readFileSync(path.join(R, "workspaces.txt"), "utf8")
        .split("\n")
        .filter((file) => file !== "");
      expect(found.filter((file) => !shown.some((dir) => file.startsWith(`${dir}/`)))).toEqual([]);
    }
  );

  // Catches: a run that reads the other label's text: from a base run, the head label's install
  // or checkout (outside the scratch root and the issue workspaces, so its own cover alone hides
  // it), a whole-tree extraction under the scratch root, and another issue's workspace under the
  // workspaces root LEGION_WORKSPACE names; and the mirror from a head run. Each run reads its
  // skills from its own label's install alone: no label's checkout is in its world, its own
  // neither, since every path in it would name the rig.
  test.skipIf(withoutNamespace)(
    "a run reads its own label's skills and no other copy, from either label",
    () => {
      const top = freshDir("launch-labels");
      const scratch = path.join(top, "scratch");
      const work = path.join(scratch, "work");
      const skill = "skills/legion-worker/references/review-threads.md";
      const copies = {
        head: path.join(top, "tree-one"),
        base: path.join(scratch, "tree-two"),
        extraction: path.join(scratch, "tree-at-0123456"),
        workspace: path.join(top, "workspaces", "acme", "widgets", "widgets-7"),
      };
      for (const [which, dir] of Object.entries(copies)) {
        mkdirSync(path.dirname(path.join(dir, skill)), { recursive: true });
        writeFileSync(path.join(dir, skill), `${which}\n`);
      }
      const installed = (label: string) => path.join(installOf(work, label), "pi-legion", skill);
      for (const label of ["head", "base"]) {
        mkdirSync(path.dirname(installed(label)), { recursive: true });
        writeFileSync(installed(label), `${label} install\n`);
      }
      const agent = [
        ...Object.values(copies).map(
          (dir) => `cat ${dir}/${skill} 2>/dev/null || echo "absent ${path.basename(dir)}"`
        ),
        ...["head", "base"].map(
          (label) => `cat ${installed(label)} 2>/dev/null || echo "absent ${label} install"`
        ),
      ].join("\n");
      const options = (label: string) => ({
        label,
        checkouts: { head: copies.head, base: copies.base },
        env: {
          TMPDIR: scratch,
          LEGION_WORKSPACE: path.join(top, "workspaces", "acme", "widgets", "widgets-1"),
        },
      });
      const absent = [
        "absent tree-one",
        "absent tree-two",
        "absent tree-at-0123456",
        "absent widgets-7",
      ];
      const base = launched(work, "threads-closed-correct-base-1", agent, [], options("base"));
      expect(base.result).toMatchObject({ code: 0, stderr: "" });
      expect(base.out.split("\n")).toEqual([
        ...absent,
        "absent head install",
        "base install",
        "exit=0",
        "",
      ]);
      const head = launched(work, "threads-closed-correct-head-1", agent, [], options("head"));
      expect(head.result).toMatchObject({ code: 0, stderr: "" });
      expect(head.out.split("\n")).toEqual([
        ...absent,
        "head install",
        "absent base install",
        "exit=0",
        "",
      ]);
    }
  );

  // Catches: a launch that carries on when the namespace cannot be made: the agent never ran, its
  // out.txt holds unshare's refusal and an exit line, and the run would read as an agent that
  // exited 1. The launch fails, naming the refusal, and the agent never starts.
  test("a namespace the host refuses at launch fails the launch, naming the refusal", () => {
    const work = freshDir("launch-refused");
    const name = "threads-closed-correct-head-1";
    const { run, result } = launched(
      work,
      name,
      `: >${path.join(runDirOf(work, name), "agent-ran")}`,
      [],
      {
        callerPath: [refusing()],
      }
    );
    expect(result.code).toBe(1);
    expect(result.stderr).toContain(
      `the agent of ${path.basename(run)} did not start: its namespace could not be made: ${REFUSAL}`
    );
    expect(existsSync(path.join(run, "agent-ran"))).toBe(false);
  });

  // Catches: a world that shows a directory holding one it hides, which shows every file of the
  // hidden one: here a tool directory on the pane's PATH that holds the scratch root.
  test("a world that shows a path holding a hidden one is refused before the agent starts", () => {
    const top = freshDir("launch-holds");
    const work = path.join(top, "scratch", "work");
    const name = "threads-closed-correct-head-1";
    const { run, result } = launched(
      work,
      name,
      `: >${path.join(runDirOf(work, name), "agent-ran")}`,
      [],
      {
        env: { TMPDIR: path.join(top, "scratch") },
        panePath: `${path.join(work, "bin")}:${top}:/usr/bin:/bin`,
      }
    );
    expect(result.code).toBe(1);
    expect(result.stderr).toContain(
      `launch: the run's world shows ${top} (ro), which holds ${path.join(top, "scratch")}, a path it hides (world_paths)`
    );
    expect(existsSync(path.join(run, "agent-ran"))).toBe(false);
  });

  /** The lines an agent printed of what it can read, by place: each `== <place>` line starts one. */
  const places = (out: string) => {
    const found = new Map<string, string[]>();
    let at = "";
    for (const line of out.split("\n")) {
      if (line.startsWith("== ")) {
        at = line.slice(3);
        found.set(at, []);
      } else if (line !== "" && at !== "") found.get(at)?.push(line);
    }
    return found;
  };
  /** An agent that prints everything a process of its run can read about its world: its
   * environment, its mounts (/proc/self/mountinfo, whose bind lines name each host source), its
   * working directory, and every process of its namespace, /proc/1 among them, by its command
   * line and its environment. */
  const DUMP = [
    'echo "== env"; env',
    'echo "== mountinfo"; cat /proc/self/mountinfo',
    'echo "== cwd"; readlink /proc/self/cwd',
    "for d in /proc/[0-9]*; do",
    `  echo "== cmdline \${d#/proc/}"; tr "\\0" "\\n" <"$d/cmdline" 2>/dev/null`,
    `  echo "== environ \${d#/proc/}"; tr "\\0" "\\n" <"$d/environ" 2>/dev/null`,
    "done",
  ].join("\n");
  /** The variables whose value is a name (run-names.ts's NAMED_VARS). */
  const NAMED_VALUE =
    /^(OMP_PROFILE|PI_PROFILE|LEGION_PROJECT|LEGION_ISSUE|LEGION_TREE|LEGION_ROLE|GH_REPO)=/;

  // Catches: a scenario, case or label word, or a name the rig gave its world (run-names.ts's
  // REMOVED_NAMES), anywhere a process of the run can read it from inside its namespace: an
  // environment value or name, a mount (its mount point or the host source mountinfo names),
  // the working directory, /proc/1 and every process's command line and environment, the
  // profile, the session directory, the Dispatch project and issue keys and the role that make
  // its Envoy topics. On 2026-11-16, 16 of the 36 runs of the batch at c273084d reasoned from the
  // run's name that every such place carried. The words are derived (run-names.ts's
  // `vocabulary`, from SCENARIOS and the labels), never typed here, and the pane carries the
  // values a worker pane does, built by rig.sh's own run_project.
  test.skipIf(withoutNamespace)(
    "nothing a run's processes can read inside its namespace names a scenario, case, label or removed name",
    () => {
      const work = freshDir("launch-vocabulary");
      const name = "threads-closed-correct-head-1";
      const project = bash(
        [
          fn("run_project"),
          assignment("worker_repo"),
          "run_project threads-closed correct head 1 t-100",
        ].join("\n")
      ).stdout.trim();
      const role = "reviewer";
      const pane = [
        "OMP_PROFILE=legion",
        `LEGION_PROJECT=${project}`,
        `LEGION_ISSUE=${WORLD_PROJECT}-1`,
        `LEGION_ROLE=${role}`,
        `GH_REPO=${assignment("worker_repo").replace(/^worker_repo=/, "")}`,
        `DISPATCH_TOKEN_FILE=${work}/services/tokens/dispatch-token`,
        `TMPDIR=${runDirOf(work, name)}/tmp`,
      ];
      const { run, result, out } = launched(work, name, DUMP, [], { pane });
      expect(result).toMatchObject({ code: 0, stderr: "" });
      const read = places(out);
      // What the agent read is the world it was given, so the check below reads something.
      expect(read.get("env")).toContain(`HOME=${run}/home`);
      expect(read.get("env")).toContain("OMP_PROFILE=legion");
      expect(read.get("mountinfo")?.join("\n")).toContain(` ${run} `);
      expect(read.get("cwd")).toEqual([`${run}/cwd`]);
      expect(
        [...read].filter(
          ([at, lines]) => at.startsWith("cmdline") && lines.includes("--session-dir")
        )
      ).not.toEqual([]);
      const topic = `notifications.role.legion-${project}-${WORLD_PROJECT.toLowerCase()}-1-${role}`;
      const vocab = vocabulary(["head", "base"]);
      const named: string[] = [];
      for (const [at, lines] of [...read, ["topic", [topic]] as const])
        for (const line of lines) {
          const equals = line.indexOf("=");
          const words =
            at === "env" || at.startsWith("environ")
              ? [
                  nameWords(line.slice(0, Math.max(equals, 0)), vocab),
                  NAMED_VALUE.test(line) || line.slice(equals + 1).includes("/")
                    ? nameWords(line.slice(equals + 1), vocab)
                    : textWords(line.slice(equals + 1), vocab),
                ]
              : at === "mountinfo"
                ? line.split(" ").map((field) => nameWords(field, vocab))
                : [
                    line.includes("/") || at === "topic"
                      ? nameWords(line, vocab)
                      : textWords(line, vocab),
                  ];
          for (const word of words)
            if (word !== undefined) named.push(`${at}: "${word}" in ${line}`);
        }
      expect(named).toEqual([]);
    }
  );

  // Catches: a guard that misses one place of what the agent can read (run-names.ts's
  // worldNames): each row plants one word of the vocabulary in one place, and the launch refuses
  // before the agent starts, naming the word and the place. The host-side work directory is one:
  // every bind's host source is in /proc/self/mountinfo, so a work directory holding a removed
  // name fails the run as the run's own path does.
  test("a scenario, case, label or removed name planted in any one place the agent reads refuses the launch", () => {
    const name = "threads-closed-correct-head-1";
    const plants: {
      what: string;
      word: string;
      place: string;
      work?: string;
      options?: (work: string) => Launch;
    }[] = [
      {
        what: "the profile",
        word: "head",
        place: "the variable OMP_PROFILE",
        options: () => ({ pane: ["OMP_PROFILE=skills-head"] }),
      },
      {
        what: "the issue key",
        word: "lweval",
        place: "the variable LEGION_ISSUE",
        options: () => ({ pane: ["LEGION_ISSUE=LWEVAL-1"] }),
      },
      {
        what: "the project key",
        word: "defect",
        place: "the variable LEGION_PROJECT",
        options: () => ({ pane: ["LEGION_PROJECT=defect"] }),
      },
      {
        what: "the role a topic carries",
        word: "rig-",
        place: "the variable LEGION_ROLE",
        options: () => ({ pane: ["LEGION_ROLE=rig-tester"] }),
      },
      {
        what: "a variable's name",
        word: "skill-scenario",
        place: "the variable name SKILL_SCENARIO_RUN",
        options: () => ({ pane: ["SKILL_SCENARIO_RUN=/somewhere"] }),
      },
      {
        what: "a variable's text",
        word: "text-only",
        place: "the variable NOTE",
        options: () => ({ pane: ["NOTE=the text-only case"] }),
      },
      {
        what: "a mount: a label's checkout the namespace covers",
        word: "threads-closed",
        place: "a mount of its namespace",
        options: () => ({
          checkouts: { head: freshDir("threads-closed") },
          env: { TMPDIR: freshDir("elsewhere") },
        }),
      },
      {
        what: "the host's work directory",
        word: "skill-scenario",
        place: "the variable PATH",
        work: "skill-scenarios",
      },
      {
        what: "the session directory",
        word: "correct",
        place: "its session directory",
        options: (work) => ({
          env: { sessions_dir: `${runDirOf(work, name)}/sessions/correct` },
        }),
      },
      {
        what: "the prompt",
        word: "code-defect",
        place: "<run>/prompt.txt",
        options: () => ({ files: { "prompt.txt": "Review the code-defect run.\n" } }),
      },
      {
        what: "the system prompt",
        word: "reviewer-text-findings",
        place: "<run>/system-args",
        options: () => ({
          files: { "system-args": "--append-system-prompt 'a reviewer-text-findings run'" },
        }),
      },
      {
        // The word is in the prompt file alone, which the agent's Oh My Pi reads at its launch
        // (`--append-system-prompt <file>`, as the daemon hands every pane its prompt since
        // #4190) and which its world shows, inside the run's own directory: <run>/system-args
        // holds only the file's path, which names nothing.
        what: "the system prompt's file, which <run>/system-args names",
        word: "reviewer-text-findings",
        place: "<run>/state/secrets/system-prompt.md",
        options: (work) => ({
          files: {
            "system-args": `--append-system-prompt ${runDirOf(work, name)}/state/secrets/system-prompt.md`,
            "state/secrets/system-prompt.md": "You review a reviewer-text-findings run.\n",
          },
        }),
      },
      {
        what: "the gh stand-in",
        word: "skill-scenario",
        place: "<run>/bin/gh",
        options: () => ({ files: { "bin/gh": "#!/bin/sh\n# skill-scenarios\n" } }),
      },
      {
        what: "the gh bundle",
        word: "skill-scenario",
        place: "<run>/lib/gh.js",
        options: () => ({ files: { "lib/gh.js": 'process.env.SKILL_SCENARIO_RUN = "/x";\n' } }),
      },
      {
        what: "the jj configuration",
        word: "rig-",
        place: "<run>/jj.toml",
        options: () => ({ files: { "jj.toml": '[user]\nname = "rig-worker"\n' } }),
      },
      {
        // The case beside the identifier that holds it, the base rule a reviewer's system prompt
        // names: the identifier names no word of the rig's (the architect's ruling D6 of
        // INFRA-764's plan v9, run-names.ts's `besideWord`), and the case standing alone still does.
        what: "a case standing alone beside a rule's name that holds it",
        word: "last-push",
        place: "<run>/state/secrets/system-prompt.md",
        options: (work) => ({
          files: {
            "system-args": `--append-system-prompt ${runDirOf(work, name)}/state/secrets/system-prompt.md`,
            "state/secrets/system-prompt.md":
              "Read `require_last_push_approval`, then review the last-push round.\n",
          },
        }),
      },
      {
        // A file cmd_profile writes into the label's HOME, which base_env copies into the run:
        // the guard reads it (run-names.ts's RIG_HOME) where it passes over the label's plugins
        // beside it (LABELS_OWN).
        what: "the profile's settings in the HOME the run copies from the label's install",
        word: "threads-closed",
        place: "<run>/home/.omp/profiles/legion/agent/config.yml",
        options: () => ({
          files: { "home/.omp/profiles/legion/agent/config.yml": "# the threads-closed run\n" },
        }),
      },
    ];
    const missed: string[] = [];
    for (const plant of plants) {
      const work = freshDir(plant.work ?? "launch-planted");
      const ran = path.join(runDirOf(work, name), "agent-ran");
      const { run, result } = launched(work, name, `: >${ran}`, [], plant.options?.(work) ?? {});
      const refused = `the world of ${path.basename(run)} names "${plant.word}" in ${plant.place}`;
      if (result.code !== 1 || !result.stderr.includes(refused) || existsSync(ran))
        missed.push(`${plant.what}: exit ${result.code}: ${result.stderr.trim()}`);
    }
    expect(missed).toEqual([]);
  });

  // Catches: a gh stand-in whose source carries a word of the measurement into the bundle every
  // run's world holds. rig.sh's `standins` builds <run>/lib/gh.js from gh-standin.ts and every
  // module it imports, and the launch guard reads that file (run-names.ts's COMPOSED), so one
  // such word refuses every agent of every run. INFRA-764's round 30 at 9083afb5: gh-standin.ts
  // came to import gh-routes.ts's `worldRouteOf`, the bundle took WORLD_ROUTES with each row's
  // `evidence` (`round17-forty/reviewer-text-findings-text-only-head-2/calls.jsonl:26`), and the
  // guard refused all ten runs of the world-health check: "the world of e486953180ff names
  // \"reviewer-text-findings\" in <run>/lib/gh.js". The planted-word row above holds the guard;
  // this one holds what the rig itself puts there, built with standins' own command.
  test("the gh bundle rig.sh builds for a run names no word of the measurement", () => {
    const out = path.join(freshDir("gh-bundle-words"), "gh.js");
    const built = Bun.spawnSync(
      [
        "bun",
        "build",
        path.join(import.meta.dir, "gh-standin.ts"),
        "--target=bun",
        "--minify-whitespace",
        "--outfile",
        out,
      ],
      { stdout: "pipe", stderr: "pipe" }
    );
    expect({ code: built.exitCode, stderr: built.stderr.toString() }).toEqual({
      code: 0,
      stderr: "",
    });
    const text = readFileSync(out, "utf8");
    const word = textWords(text, vocabulary(["head", "base"]));
    const at = word === undefined ? -1 : text.toLowerCase().indexOf(word);
    expect(
      word === undefined ? undefined : `${word}: ${text.slice(Math.max(0, at - 80), at + 80)}`
    ).toBe(undefined);
  });

  // Catches: a guard that reads the label's install as the rig's text. rig.sh's launch copies the
  // label's HOME into the run (`cp -a "$I/home" "$R/home"`), and that HOME holds the label's packed
  // plugins under .omp/profiles/<profile>/plugins/node_modules, the checkout's own code, which a
  // production pane's HOME holds too. INFRA-764's round 31 at c42409bc: the guard read every file
  // of <run>/home at a run's first launch and refused run 4f4d19499cf8 for
  // `var SPEC_WRITING_POINTER` in @sjawhar/pi-envoy/dist/dispatch.js, which folds to the scenario
  // spec-writing; read whole, that HOME took the guard to 1170 MiB, and the pod's memory guard
  // killed it twice before it could refuse.
  test.skipIf(withoutNamespace)(
    "a word in the label's installed plugins, which the run's HOME holds, refuses no launch",
    () => {
      const work = freshDir("launch-installed-plugin");
      const name = "threads-closed-correct-head-1";
      const ran = path.join(runDirOf(work, name), "agent-ran");
      const { result } = launched(work, name, `: >${ran}`, [], {
        files: {
          "home/.omp/profiles/legion/plugins/node_modules/@sjawhar/pi-envoy/dist/dispatch.js":
            'var SPEC_WRITING_POINTER = "Write a spec as skill://dispatch-writing-specs says.";\n',
        },
      });
      expect({ code: result.code, stderr: result.stderr, ran: existsSync(ran) }).toEqual({
        code: 0,
        stderr: "",
        ran: true,
      });
    }
  );

  // Catches: a guard that passes over a link of the run to a path outside it and outside the trees
  // the label and the host provide (the architect's ruling of 2026-11-21T17:22:22Z). The walker
  // reads the path a link names and never what it points to: inside the run the walker reads that
  // path as itself, and the label's install and the host's native module cache and git exec path
  // are the product's and the host's, not read. Anything else is neither the rig's to read nor the
  // label's or the host's to pass over, so the launch is refused, naming the link.
  test("a link of the run to a file outside it and the trees the label and the host provide refuses the launch, naming the link", () => {
    // No path of the run names `link`, a case of askgate's (SCENARIOS).
    const work = freshDir("launch-pointer-outside");
    const name = "threads-closed-correct-head-1";
    const run = runDirOf(work, name);
    const target = path.join(work, "elsewhere", "notes.md");
    mkdirSync(path.dirname(target), { recursive: true });
    writeFileSync(target, "Move the nightly export to 04:00 UTC.\n");
    mkdirSync(path.join(run, "cwd"), { recursive: true });
    symlinkSync(target, path.join(run, "cwd", "notes.md"));
    const ran = path.join(run, "agent-ran");
    const { result } = launched(work, name, `: >${ran}`);
    expect({
      code: result.code,
      refused: result.stderr.split("\n").find((line) => line.includes("cannot be read whole")),
      ran: existsSync(ran),
    }).toEqual({
      code: 1,
      refused: expect.stringContaining(
        `the world of ${path.basename(run)} cannot be read whole: <run>/cwd/notes.md is a link to ${target}, outside the run, the label's install (${installOf(work, "head")}),`
      ),
      ran: false,
    });
  });

  // Catches: a walker that enters a link of the run, or opens what a link points to outside the
  // run. INFRA-764's round 31 at c42409bc: Bun's recursive readdirSync enters a link to a
  // directory, so the walker read the label's plugins through the links of the run's HOME into
  // <work>/installs/<id>/ and the host's native module cache through its .omp/natives link
  // (124 MiB), refused every run on `var SPEC_WRITING_POINTER` in pi-envoy's dist/dispatch.js and
  // reached 1,170 MiB. Here the run holds a link of each kind the rig makes (into the label's
  // install, the host's natives, the host's git exec path, and the run itself), each tree outside
  // holding that word, and its HOME holds the word in a file of the label's Oh My Pi (agent.db),
  // which the rig does not write (run-names.ts's RIG_HOME). The guard, run as launch runs it with
  // the host's two trees read from its environment (HOME, GIT_EXEC_PATH), passes; and every file
  // and directory of the trees outside keeps the access time the row gave it, so the walker opened
  // nothing whose real path is outside the run. The row reads a file of its own after the guard as
  // the control: on a filesystem that keeps no access time it fails rather than seeing nothing.
  test("the guard opens nothing a link of the run points to outside it, and reads nothing of the run's HOME the rig did not write", () => {
    const work = freshDir("guard-trees");
    const run = path.join(work, "runs", "0123456789ab");
    const install = path.join(work, "installs", "ba9876543210");
    const host = path.join(work, "host-home");
    const gitExec = path.join(work, "git-core");
    const word =
      'var SPEC_WRITING_POINTER = "Write a spec as skill://dispatch-writing-specs says.";\n';
    for (const [file, text] of [
      [path.join(install, "pi-envoy", "dist", "dispatch.js"), word],
      [path.join(host, ".omp", "natives", "18.18.6", "pi_natives.linux-x64-modern.node"), word],
      [path.join(gitExec, "git-checkout"), word],
      [path.join(run, "prompt.txt"), "Review the pull request.\n"],
      [path.join(run, "cwd", "notes.md"), "Move the nightly export to 04:00 UTC.\n"],
      [path.join(run, "home", ".omp", "profiles", "legion", "agent", "agent.db"), word],
      [path.join(work, "profiles", "head", "checkout"), `${work}\n`],
    ]) {
      mkdirSync(path.dirname(file), { recursive: true });
      writeFileSync(file, text);
    }
    for (const [link, target] of [
      ["home/.omp/natives", path.join(host, ".omp", "natives")],
      [
        "home/.omp/profiles/legion/plugins/node_modules/@sjawhar/pi-envoy",
        path.join(install, "pi-envoy"),
      ],
      ["lib/git-core/git-checkout", path.join(gitExec, "git-checkout")],
      ["cwd/notes-link.md", "notes.md"],
    ]) {
      mkdirSync(path.dirname(path.join(run, link)), { recursive: true });
      symlinkSync(target, path.join(run, link));
    }
    const outside = [install, path.join(host, ".omp"), gitExec].flatMap((tree) => [
      tree,
      ...runEntries(tree).map(({ file }) => path.join(tree, file)),
    ]);
    const control = path.join(work, "control");
    writeFileSync(control, "read after the guard\n");
    for (const one of [...outside, control]) utimesSync(one, 0, statSync(one).mtime);
    const guard = Bun.spawnSync(
      [
        "bun",
        path.join(import.meta.dir, "run-names.ts"),
        "guard",
        run,
        path.join(run, "sessions"),
        work,
        install,
      ],
      { env: { ...childEnv, HOME: host, GIT_EXEC_PATH: gitExec }, stdout: "pipe", stderr: "pipe" }
    );
    readFileSync(control);
    expect({
      code: guard.exitCode,
      stderr: guard.stderr.toString(),
      opened: outside.filter((one) => statSync(one).atimeMs !== 0),
      control: statSync(control).atimeMs !== 0,
    }).toEqual({ code: 0, stderr: "", opened: [], control: true });
  });

  /** The batch's services and what a run reaches of them, over rig.sh sourced whole: the
   * namespace and tmux probes and the listener, which write nothing into a run; the given
   * Postgres and NATS, which nothing here reaches; the Envoy listener's and the Dispatch server's
   * builds, each a binary outside every run (the label's build of daemon-pane.go, which composes
   * the pane and its system prompt, is built for real); the processes a run starts beside its
   * agent, the daemon stand-in saying it listens as its own log does and opening the run's record
   * of what reached it, which serve the agent and write only their own records; and seed.ts,
   * which files the run's issue on its own Dispatch and writes the ids that Dispatch gave it into
   * the run (fixture.json), here an issue of the world's project with ids of the shape Dispatch
   * gives. */
  const WHOLE_WORLD_SERVICES = [
    "require_namespace() { :; }",
    "tmux_ready() { :; }",
    "until_true() { :; }",
    // A given Postgres that admits exactly what the batch needs, whatever one Dispatch server's
    // count is (services_up's `needs`, from connections_needed).
    'pg_sql() { [ "$2" != "SHOW max_connections" ] || echo "$needs"; }',
    "start_listener() { envoy_port=1; }",
    'eval "rig_$(declare -f go_binary)"',
    "go_binary() {",
    '  if [ "$4" = daemon-pane ]; then rig_go_binary "$@"; else printf \'%s/%s\\n\' "$(build_dir "$1")" "$4"; fi',
    "}",
    "start_dispatch() { dispatch_port=1; }",
    "start_process() {",
    "  printf -v \"$1_pid\" '%s' ''",
    '  [ "$1" != daemon ] || {',
    "    printf 'listening on http://127.0.0.1:4321\\n' >\"$R/logs/daemon.log\"",
    '    printf \'{"at":"2026-11-21T00:00:00.000Z","opened":"daemon.log"}\\n\' >>"$R/daemon.log"',
    "  }",
    "}",
    "await_start() { :; }",
    "seed_standin() {",
    "  local key",
    "  case $1 in",
    "  ask-on-message | measure-before-ask | askgate | tester-proof | spec-writing | spec-rewrite)",
    `    jq -n '{issue:"${WORLD_PROJECT}-1", message:"01a2d000-0000-7000-8000-000000000001", comment_ref:"dispatch://${WORLD_PROJECT}-1/comment/01a2d000-0000-7000-8000-000000000002"}' >"$2"`,
    "    ;;",
    "  plan | implement-handoff | tester-result | review-handoff)",
    "    case $1 in plan) key=plan ;; implement-handoff) key=implement ;; tester-result) key=test ;; *) key=review ;; esac",
    '    jq --arg key "$key" \'.[$key] = {id:"01a2d000-0000-7000-8000-000000000003", version:1}\' "$2" >"$2.next"',
    '    mv "$2.next" "$2"',
    "    ;;",
    "  esac",
    "}",
    'bun() { if [ "$1" = "$here/seed.ts" ]; then shift; seed_standin "$@"; else command bun "$@"; fi; }',
  ];

  /** The directory, on the disk TMPDIR names and never under this file's work directory, of what
   * composeRuns' batches hold that a tmpfs of a pod's size cannot: each batch's work directory,
   * where its label's build of daemon-pane.go goes, and the native module cache its label installs
   * link their homes' .omp/natives to (make_omp_home reads $XDG_DATA_HOME/omp/natives where
   * $XDG_DATA_HOME/omp exists), where Oh My Pi extracts its addon, 123 MiB a version. A pod's
   * /dev/shm of 64 MiB refused both (`ENOSPC`, 2026-11-25). */
  const onDisk = temporaryDirectory("suite");
  /** Composes each run `runs` names (`<scenario>:<case>:<label>:<the case's variable>:<n>`, then
   * `:plant` for a run with PLANT set) as cmd_run composes it, with rig.sh sourced whole, over
   * WHOLE_WORLD_SERVICES and then `stubs`, under `env`: each of `labels` gets a profile on this
   * checkout as cmd_profile records one, the batch's services come up as services_up brings them
   * up, and a run that cannot be composed is named on stderr (`could not compose <run>`). Both
   * labels are built from this checkout, since the base label's own checkout is none this test
   * has. Each label's install is a real one, made as cmd_profile makes it (make_omp_home, then
   * install-plugin-profile.sh for pi-envoy and pi-legion, which packs each plugin and installs it
   * with the pinned Oh My Pi through the caller's mise, then install_model_route), so the HOME
   * every run copies holds the plugins' links into the install and the natives link into a cache
   * (onDisk's), as a batch's does: the route is the provider route, on a provider file of the
   * row's own whose key command no run calls, since the gateway and key-file routes need
   * credentials no test holds; and no superpowers skills are copied, which a model's read probes.
   * Returns bash's result, the batch's work directory (under onDisk) and each run as the rig left
   * it, archived (stop_run), by the name its record gives it. */
  function composeRuns(
    dir: string,
    labels: readonly string[],
    runs: readonly string[],
    stubs: readonly string[],
    env: Record<string, string>
  ) {
    const rigWork = path.join(onDisk, path.basename(dir), "work");
    const natives = path.join(onDisk, "natives");
    const bin = path.join(dir, "bin");
    mkdirSync(path.join(bin, "pin", "bin"), { recursive: true });
    mkdirSync(path.join(natives, "omp"), { recursive: true });
    writeFileSync(path.join(bin, "pin", "bin", "omp"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
    writeFileSync(
      path.join(bin, "mise"),
      `#!/bin/sh\nif [ "$1" = where ]; then echo "${bin}/pin"; exit 0; fi\nshift 3\nexec "$@"\n`,
      { mode: 0o755 }
    );
    // The caller's go caches, which the build of daemon-pane.go compiles with: childEnv's HOME
    // holds none.
    const printed = spawnChecked(
      ["go", "env", "-json", "GOCACHE", "GOMODCACHE", "GOPATH", "GOFLAGS", "GOTOOLCHAIN"],
      { stdout: "pipe", stderr: "pipe" }
    );
    expect(printed.exitCode).toBe(0);
    const go = Object.fromEntries(
      Object.entries(JSON.parse(printed.stdout.toString()) as Record<string, string>).filter(
        ([, value]) => value !== ""
      )
    );
    const providerFile = path.join(dir, "provider.yml");
    writeFileSync(
      providerFile,
      `${JSON.stringify({
        providers: {
          gateway: {
            api: "anthropic-messages",
            baseUrl: "https://gateway.internal.example/anthropic",
            apiKey: "!cat /dev/null",
          },
        },
      })}\n`
    );
    const result = bash(
      [
        `. ${JSON.stringify(path.join(import.meta.dir, "rig.sh"))}`,
        ...WHOLE_WORLD_SERVICES,
        ...stubs,
        // Each label's profile, as cmd_profile records it, on this checkout, and its install as
        // cmd_profile makes it, on the caller's own PATH and mise, which hold the pinned Oh My Pi
        // that the stand-in mise above would answer for.
        "for label in $LABELS; do",
        "  id=$(printf '%s' \"$label\" | sha256sum | cut -c1-12)",
        "  P=$work/profiles/$label I=$work/installs/$id",
        '  mkdir -p "$P" "$I"',
        '  printf \'%s\\n\' "$root" >"$P/checkout"',
        "  printf 'legion\\n' >\"$P/profile\"",
        '  printf \'%s\\n\' "$id" >"$P/install"',
        '  cp "$root/.omp-pin" "$P/pin"',
        '  printf \'checkout %s\\n\' "$root" >"$P/built-from"',
        "  (",
        "    export PATH=$INSTALL_PATH MISE_DATA_DIR=$INSTALL_MISE_DATA_DIR",
        '    . "$root/scripts/e2e/lib/omp-home.sh" && make_omp_home "$I/home" &&',
        '      bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --package pi-envoy --profile legion --home "$I/home" --dest "$I/pi-envoy" &&',
        '      bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --package pi-legion --profile legion --home "$I/home" --dest "$I/pi-legion" &&',
        '      LEGION_E2E_MODEL_ROUTE=provider LEGION_E2E_MODEL_PROVIDER=gateway LEGION_E2E_MODEL_PROVIDER_FILE=$PROVIDER_FILE install_model_route "$root" legion "$I"',
        '  ) >"$P/install.log" 2>&1 || { printf \'could not install label %s: %s\\n\' "$label" "$(tail -n 5 "$P/install.log")" >&2; exit 1; }',
        '  : >"$P/superpowers"',
        '  : >"$P/ready"',
        "done",
        // The source a spec run seeds its issue from (fetch-spec-sources.ts writes one).
        "source=$work/sources/$SKILL_SCENARIOS_SOURCE",
        'mkdir -p "$source"',
        "printf 'Move the nightly export\\n' >\"$source/title.txt\"",
        "printf 'Move the nightly export to 04:00 UTC.\\n' >\"$source/messages.md\"",
        "printf '[]\\n' >\"$source/asks.json\"",
        "printf '# The nightly export\\n\\nIt runs at 02:00 UTC.\\n' >\"$source/old-spec.md\"",
        "lock_services",
        "services_up $LABELS",
        'for one in "$@"; do',
        '  IFS=: read -r scenario case_name label variable n plant <<<"$one"',
        "  (",
        '    [ -z "$variable" ] || export "$variable=$case_name"',
        '    [ -z "$plant" ] || export PLANT=1',
        '    cmd_run "$scenario" "$label" "$n" 1 1700000000',
        "  ) || printf 'could not compose %s\\n' \"$one\" >&2",
        "done",
      ].join("\n"),
      [...runs],
      {
        ...go,
        TMPDIR: SCRATCH,
        PATH: `${bin}:${childEnv.PATH}`,
        INSTALL_PATH: childEnv.PATH,
        // The caller's own mise store, where its mise installed the pin (mise's default under the
        // caller's HOME, which childEnv's HOME is not).
        INSTALL_MISE_DATA_DIR:
          process.env.MISE_DATA_DIR ??
          path.join(process.env.XDG_DATA_HOME ?? path.join(homedir(), ".local", "share"), "mise"),
        // The host's native module cache, as make_omp_home and the launch guard read it
        // (run-names.ts's `hostProvided`): $XDG_DATA_HOME/omp/natives.
        XDG_DATA_HOME: natives,
        PROVIDER_FILE: providerFile,
        LABELS: labels.join(" "),
        SKILL_SCENARIOS_WORK: rigWork,
        SKILL_SCENARIOS_SOURCE: "nightly-export",
        SKILL_SCENARIOS_POSTGRES_URL: "postgres://127.0.0.1:1/postgres",
        SKILL_SCENARIOS_NATS_URL: "nats://127.0.0.1:1",
        ...env,
      }
    );
    const archive = path.join(rigWork, "archive", "runs");
    const composed = new Map<string, string>();
    for (const file of existsSync(archive) ? readdirSync(archive) : [])
      if (file.endsWith(".json"))
        composed.set(
          JSON.parse(readFileSync(path.join(archive, file), "utf8")).name,
          path.join(archive, file.slice(0, -".json".length))
        );
    return { result, composed, rigWork, natives };
  }

  // Catches: a word of the measurement in any file rig.sh writes, copies or builds into a run
  // before its agent starts, the files the guard reads none of included: the fixtures and the
  // world's repositories, the seeds' ids, the rig's records, the pane and its system prompt, the
  // stand-ins and the gh bundle. The row above builds the bundle alone, with standins' own
  // command; this one composes each scenario's run, each case of it, for both labels, as cmd_run
  // composes it, with rig.sh sourced whole, so a file the rig comes to write is read with no edit
  // here: the batch's services as services_up brings them up (WHOLE_WORLD_SERVICES), then each run
  // up to its first agent (SKILL_SCENARIOS_COMPOSE_ONLY), where launch's guard reads it, and then
  // every file the run holds, read whole, as the guard reads a file. INFRA-764's round 30 at
  // 9083afb5: the bundle took the route table's evidence, and the guard refused every reviewer.
  // The files are read through the guard's own walker (run-names.ts's `worldFiles`) as it reads
  // them at a run's first launch, which no launch on the run's record (logs/launches) makes: it
  // passes over AGENTS_OWN and reads what an agent can write (AGENT_WRITTEN) as the rig wrote it,
  // reads services-from less the rig's own directory, a residual of the ruling of
  // 2026-11-18T08:40Z, reads of the HOME the rig's own files alone (RIG_HOME), and reads each link
  // by the path it names, refusing one outside the run and the trees the label and the host
  // provide. Each label is on a real install (composeRuns), so the walker meets the links of the
  // HOME the guard meets: INFRA-764's round 31 at c42409bc, where the row's labels had a HOME of
  // one file, the guard went through those links into the plugins and the host's natives, refused
  // every run and was killed at 1,170 MiB. So the row also names what the walker met of the HOME,
  // and holds the guard's own peak (ru_maxrss), as launch runs it in each run, under
  // GUARD_PEAK_LIMIT.
  test.skipIf(withoutGo || withoutInstall)(
    "no file rig.sh writes, copies or builds into a run before its agent starts names a word of the measurement",
    () => {
      /** The most the guard may hold at once, as launch runs it on a composed run: 256 MiB. The
       * peaks measured on 2026-11-21 in a Legion pod (gVisor, where a bare `bun -e 1` peaks at
       * 139 MiB): 167.9 to 193.5 MiB over this row's 38 runs (193.5 for
       * reviewer-text-findings-defect-base-1), and 182.6 MiB over a copy of round 31's run
       * 4f4d19499cf8 with its services-from named for this checkout. At c42409bc the guard peaked
       * at 1,170 MiB on that run, and the pod's memory guard killed it near 1 GiB. */
      const GUARD_PEAK_LIMIT = 256 * 1024 * 1024;
      const labels = ["head", "base"];
      const runs = labels.flatMap((label) =>
        Object.entries(SCENARIOS).flatMap(([scenario, { variable, cases }]) =>
          (cases.length === 0 ? [""] : cases).map((one) => ({
            name: `${scenario}${one === "" ? "" : `-${one}`}-${label}-1`,
            argument: `${scenario}:${one}:${label}:${variable ?? ""}:1`,
          }))
        )
      );
      const dir = freshDir("whole-world");
      // launch's guard, run through a program that records its peak, `<run> <bytes>` a line.
      const peaks = path.join(dir, "peaks");
      const measure = path.join(dir, "measure.ts");
      writeFileSync(
        measure,
        [
          'import { appendFileSync } from "node:fs";',
          "const argv = Bun.argv.slice(2);",
          'const guard = Bun.spawnSync(["bun", ...argv], { stdio: ["inherit", "inherit", "inherit"] });',
          `appendFileSync(${JSON.stringify(peaks)}, \`\${argv[2]} \${guard.resourceUsage.maxRSS}\\n\`);`,
          "process.exit(guard.exitCode ?? 1);",
          "",
        ].join("\n")
      );
      const { result, composed, rigWork, natives } = composeRuns(
        dir,
        labels,
        runs.map(({ argument }) => argument),
        [
          'eval "unmeasured_$(declare -f bun)"',
          `bun() { if [ "$1" = "$here/run-names.ts" ] && [ "$2" = guard ]; then command bun ${JSON.stringify(measure)} "$@"; else unmeasured_bun "$@"; fi; }`,
        ],
        { SKILL_SCENARIOS_COMPOSE_ONLY: "1" }
      );
      // Every file of each run as the guard reads it at the first launch, through its own walker
      // (run-names.ts's `worldFiles`), by its path and its text, with the trees the run's label
      // and the host provide as the guard reads them (the batch's XDG_DATA_HOME, whose
      // omp/natives make_omp_home linked the install's natives to).
      const vocab = vocabulary(labels);
      const named: string[] = [];
      const first: string[] = [];
      const home = new Set<string>();
      for (const [name, run] of [...composed].sort()) {
        if (!firstLaunch(run)) first.push(name);
        const { label } = JSON.parse(readFileSync(`${run}.json`, "utf8"));
        try {
          for (const { file, text, link } of worldFiles(
            run,
            hostProvided(installOf(rigWork, label), { ...childEnv, XDG_DATA_HOME: natives })
          )) {
            if (file.startsWith("home/")) home.add(`${link ? "link" : "file"} <run>/${file}`);
            const inName = textWords(file, vocab);
            const word = inName ?? textWords(text, vocab);
            if (word === undefined) continue;
            const at = Math.max(0, text.toLowerCase().replaceAll("_", "-").indexOf(word));
            named.push(
              `${name}: <run>/${file} names "${word}"${inName === undefined ? `: ${text.slice(Math.max(0, at - 60), at + 60)}` : ""}`
            );
          }
        } catch (error) {
          named.push(`${name}: ${error instanceof Error ? error.message : String(error)}`);
        }
      }
      const measured = (existsSync(peaks) ? readFileSync(peaks, "utf8") : "")
        .split("\n")
        .filter((line) => line !== "")
        .map((line) => line.split(" "));
      expect({
        code: result.code,
        stderr: result.stderr,
        runs: [...composed.keys()].sort(),
        named,
        notFirst: first,
        home: [...home].sort(),
        guards: measured.length,
        over: measured
          .filter(([, bytes]) => Number(bytes) > GUARD_PEAK_LIMIT)
          .map(
            ([at, bytes]) =>
              `${path.basename(at ?? "")}: ${(Number(bytes) / 1048576).toFixed(1)} MiB`
          ),
      }).toEqual({
        code: 0,
        stderr: "",
        runs: runs.map(({ name }) => name).sort(),
        named: [],
        notFirst: [],
        home: [
          "file <run>/home/.jjconfig-operator-paths.toml",
          "file <run>/home/.omp/profiles/legion/agent/config.yml",
          "file <run>/home/.omp/profiles/legion/agent/models.yml",
          "link <run>/home/.omp/natives",
          "link <run>/home/.omp/profiles/legion/plugins/node_modules/@sjawhar/pi-envoy",
          "link <run>/home/.omp/profiles/legion/plugins/node_modules/@sjawhar/pi-legion",
        ],
        guards: runs.length,
        over: [],
      });
    },
    1_200_000
  );

  /** What a run meets after its first agent, over WHOLE_WORLD_SERVICES, so cmd_run goes on past
   * it and composes every later launch of the run: an agent that starts, writes nothing and exits
   * 0 (tmux's new-session, marking the namespace ready as a started agent's does); a claim boot
   * the daemon stand-in refused before the boot's first turn, so boot_agent launches every worker
   * agent once more (boot-refusal.ts); the person stand-in's answer to each open ask (judge.ts
   * answer); and a spec run's issue as Dispatch holds it after each turn, read by seed.ts's own
   * `state` (STATE_READER): after the first turn the open ask the agent wrote, which names a case of
   * the measurement, and after the second the spec's approval request. A spec run's seed holds the
   * shape seed.ts writes a spec run's fixture in, and with PLANT set its line in fixture.log names
   * that case too. At each launch's guard, rig.sh's own `run-names.ts guard`, the run as the guard
   * reads it is copied to $LAUNCHED/<run id>/<n>, the run's n-th launch counted from 0. */
  const LATER_LAUNCHES = [
    'tmux() { case $1 in new-session) printf \'exit=0\\n\' >>"$R/out.txt" && : >"$R/namespace-ready" ;; esac; }',
    "launched_copy() {",
    "  local into n",
    '  into=$LAUNCHED/$(basename "$1")',
    '  mkdir -p "$into"',
    '  n=$(find "$into" -mindepth 1 -maxdepth 1 | wc -l)',
    '  cp -a "$1" "$into/$n"',
    "}",
    'eval "whole_$(declare -f seed_standin)"',
    "seed_standin() {",
    "  case $1 in",
    "  spec-writing | spec-rewrite)",
    `    jq -n '{issue:"${WORLD_PROJECT}-1", spec_artifact:"01a2d000-0000-7000-8000-000000000004"}' >"$2"`,
    '    printf \'[]\\n\' >"$(dirname "$2")/seeded.json"',
    `    printf 'seeded ${WORLD_PROJECT}-1%s\\n' "\${PLANT:+, a text-only change}"`,
    "    ;;",
    '  *) whole_seed_standin "$@" ;;',
    "  esac",
    "}",
    'eval "whole_$(declare -f bun)"',
    "bun() {",
    "  case $1 in",
    '  "$here/run-names.ts") [ "$2" != guard ] || launched_copy "$3" ;;',
    '  "$here/boot-refusal.ts") echo "claim boot failed before the first turn"; return 0 ;;',
    // judge.ts's `answer` answers the spec run's ask; its `run`, which a worker run's end calls
    // (judge_ts run), judges nothing here, since no agent wrote.
    `  "$here/judge.ts") [ "$2" != answer ] || printf '{"ask":"01a2d000-0000-7000-8000-000000000005","selected":["Text-only"],"text":"It is."}\\n' >"$3/answers-$4.jsonl"; return 0 ;;`,
    '  "$here/seed.ts") [ "$2" != state ] || { shift; command bun "$STATE_READER" "$@"; return; } ;;',
    "  esac",
    '  whole_bun "$@"',
    "}",
  ];
  /** seed.ts's own `state <file> <turn> <run dir>` (STATE_READER's arguments), over a Dispatch
   * that answers the issue's asks: after the first turn the agent's open question, which says
   * `text-only`, and after every later one that question answered and the spec's approval request
   * open. */
  const STATE_READER = `import { state } from ${JSON.stringify(path.join(import.meta.dir, "seed.ts"))};
const [, file = "", turn = "", run = ""] = Bun.argv.slice(2);
const { issue, spec_artifact } = JSON.parse(await Bun.file(file).text());
const author = { kind: "session", id: "01a2d000-0000-7000-8000-000000000006" };
const question = {
  id: "01a2d000-0000-7000-8000-000000000005", issue_key: issue, author, kind: "question",
  question: "Is the change text-only, or does it move the export's code too?",
  options: [{ label: "Text-only", description: "Only the text-only lines change." }, { label: "Code" }],
  multiple: false, state: turn === "1" ? "open" : "answered", answer: null, edited_at: null,
  created_at: "2026-11-21T00:00:00Z",
};
const approval = {
  ...question, id: "01a2d000-0000-7000-8000-000000000007", kind: "approval", question: "Approve?",
  options: [{ label: "Approve" }, { label: "Request changes" }], state: "open",
  approval: { artifact_id: spec_artifact, name: "spec.md", version: 1, requested_version: 1 },
};
globalThis.fetch = (async () => Response.json(turn === "1" ? [question] : [question, approval])) as typeof fetch;
await state(file, Number(turn), run);
`;

  // Catches: the rig writing into a path an agent of the run can write (run-names.ts's
  // AGENT_WRITTEN) after the run's first launch. The guard reads those paths at the first launch
  // alone, as the rig wrote them, and passes over them at every later one, where what is there is
  // an agent's as well (the architect's ruling of 2026-11-21T14:24:55Z), so a word the rig wrote
  // there after the first launch would reach a later agent with no guard reading it. Each worker
  // scenario's case and each spec scenario is composed past its first agent, by an agent that
  // writes nothing (LATER_LAUNCHES), to every later launch it reaches: boot_agent's relaunch of
  // every worker boot, threads-closed-correct's reviewer and its relaunch, and a spec run's
  // second and third turns. Every file of AGENT_WRITTEN outside AGENTS_OWN is held, at each later
  // launch's guard, to the bytes the first launch's guard read; since no agent wrote, any change is
  // the rig's, and the row names the run, the launch and the path. Each launch's guard sees as
  // many launches on the run's record (logs/launches) as went before it, so the guard read the
  // first as the first and every other as a later one. And a spec run's later turn is not refused
  // on the agent's own ask, which says `text-only`: seed.ts's `state` prints no ask's text into
  // <run>/fixture.log, which the guard reads at every turn; the same word in the seed's line of
  // fixture.log, which the rig wrote before the first turn (spec-writing-head-2), refuses that run.
  test.skipIf(withoutGo || withoutInstall)(
    "after a run's first launch the rig writes nothing into what an agent of the run can write",
    () => {
      const dir = freshDir("later-launches");
      const launchedDir = path.join(dir, "launched");
      const reader = path.join(dir, "state-reader.ts");
      writeFileSync(reader, STATE_READER);
      const runs: [string, string][] = [
        ["tester-proof-head-1", "tester-proof::head::1"],
        ...SCENARIOS["reviewer-text-findings"].cases.map((one): [string, string] => [
          `reviewer-text-findings-${one}-head-1`,
          `reviewer-text-findings:${one}:head:SKILL_SCENARIOS_REVIEWER_CASE:1`,
        ]),
        ...SCENARIOS["threads-closed"].cases.map((one): [string, string] => [
          `threads-closed-${one}-head-1`,
          `threads-closed:${one}:head:SKILL_SCENARIOS_THREADS_CASE:1`,
        ]),
        ["spec-writing-head-1", "spec-writing::head::1"],
        ["spec-rewrite-head-1", "spec-rewrite::head::1"],
        ["spec-writing-head-2", "spec-writing::head::2:plant"],
      ];
      const { result, composed } = composeRuns(
        dir,
        ["head"],
        runs.map(([, argument]) => argument),
        LATER_LAUNCHES,
        { LAUNCHED: launchedDir, STATE_READER: reader }
      );
      /** The files of the run copied to `at` that the first launch reads and no later one does:
       * AGENT_WRITTEN's, outside AGENTS_OWN, each a regular file by its bytes and each link by the
       * path it names, by its path under the run. No link is entered (run-names.ts's
       * `runEntries`): the HOME's links reach the label's plugins and the host's natives. */
      const written = (at: string) => {
        const files = new Map<string, Buffer>();
        for (const { file, entry } of runEntries(at))
          if (agentWritten(file) && !agentsOwn(file))
            if (entry.isSymbolicLink())
              files.set(file, Buffer.from(`a link to ${readlinkSync(path.join(at, file))}`));
            else if (entry.isFile()) files.set(file, readFileSync(path.join(at, file)));
        return files;
      };
      const records: Record<string, number[]> = {};
      const changed: string[] = [];
      const held = new Set<string>();
      for (const [name, run] of [...composed].sort()) {
        const copies = path.join(launchedDir, path.basename(run));
        const launches = (existsSync(copies) ? readdirSync(copies) : [])
          .map(Number)
          .sort((a, b) => a - b)
          .map((n) => path.join(copies, String(n)));
        records[name] = launches.map((at) => {
          const record = path.join(at, "logs", "launches");
          return existsSync(record) ? readFileSync(record, "utf8").split("\n").length - 1 : 0;
        });
        const [first, ...later] = launches;
        if (first === undefined) continue;
        const before = written(first);
        for (const file of before.keys())
          held.add(AGENT_WRITTEN.find(({ path: one }) => one.test(file))?.path.source ?? file);
        for (const [index, at] of later.entries()) {
          const after = written(at);
          for (const file of new Set([...before.keys(), ...after.keys()])) {
            const was = before.get(file);
            const is = after.get(file);
            if (was !== undefined && is !== undefined && was.equals(is)) continue;
            const how = was === undefined ? "added" : is === undefined ? "removed" : "changed";
            changed.push(`${name}, launch ${index + 2}: <run>/${file} ${how}`);
          }
        }
      }
      const control = composed.get("spec-writing-head-2") ?? "";
      expect({
        code: result.code,
        refused: result.stderr
          .split("\n")
          .filter((line) => / names "|could not compose/.test(line))
          .map((line) => line.replace(/^(.* in <run>\/[^:]+):.*$/, "$1")),
        records,
        changed,
      }).toEqual({
        code: 0,
        refused: [
          `the world of ${path.basename(control)} names "text-only" in <run>/fixture.log`,
          "could not compose spec-writing::head::2:plant",
        ],
        records: {
          "reviewer-text-findings-defect-head-1": [0, 1],
          "reviewer-text-findings-last-push-head-1": [0, 1],
          "reviewer-text-findings-text-only-head-1": [0, 1],
          "spec-rewrite-head-1": [0, 1, 2],
          "spec-writing-head-1": [0, 1, 2],
          "spec-writing-head-2": [0],
          "tester-proof-head-1": [0, 1],
          "threads-closed-accept-head-1": [0, 1],
          "threads-closed-code-defect-head-1": [0, 1],
          "threads-closed-correct-head-1": [0, 1, 2, 3],
          "threads-closed-crossing-head-1": [0, 1],
        },
        changed: [],
      });
      // What the first launches held of AGENT_WRITTEN, so a walk that found none of it is no
      // path held: the gh answers, the workspace, the HOME and the pane's XDG directories.
      expect([...held]).toEqual(
        expect.arrayContaining(
          [/^fixtures\.json$/, /^ws\//, /^home\//, /^state\/home\//].map(({ source }) => source)
        )
      );
    },
    1_200_000
  );

  // Catches: a guard that reads the reviewer's prompt of a threads-closed run as the rig's own
  // text. run_threads_closed builds that prompt from the implementer's own messages to the
  // reviewer (reviewer_wake, kept whole in <run>/reviewer-wake.json), and launch's own comment says
  // the guard reads "neither the label's install ... nor what the run has written, whose words are
  // its agents' own". INFRA-764's round 26 at 10c26bb8: the implementer of
  // threads-closed-correct-head-1 (run b4124d1ea341) wrote "Pushed one text-only commit" to the
  // reviewer, an ordinary word that 6 of the round's 10 runs' agents used of their own; the guard
  // refused the reviewer's launch on "text-only" in <run>/prompt.txt, the run had no reviewer half
  // (unfinished, so unhealthy), and the world-health check refused the batch. The second launch is
  // the control: the same word in the rig's own line of the prompt, which no message of the run
  // holds, still refuses, so the guard still reads the prompt.
  test.skipIf(withoutNamespace)(
    "a reviewer woken by the implementer's message that says text-only starts; the word in the rig's own line still refuses",
    () => {
      const name = "threads-closed-correct-head-1";
      const wakeOf = (message: string) =>
        JSON.stringify({
          wake: [
            {
              at: "2026-11-20T16:46:07.292Z",
              topic: "notifications.role.legion-fernbrookwidgets0da355854a62-wid-1-reviewer",
              message,
              sender: "01a1f9d6-d286-7165-97d6-fe3f0206e8b7",
              subject:
                "notifications.envoy.kept.role.legion-fernbrookwidgets0da355854a62-wid-1-reviewer",
            },
          ],
        });
      const said =
        "Pushed one text-only commit c9268b02bfc2 fixing the open thread PRRT_kwDOfix002.";
      const agentWork = freshDir("launch-message-word");
      const agentRan = path.join(runDirOf(agentWork, name), "agent-ran");
      const agent = launched(agentWork, name, `: >${agentRan}`, [], {
        files: {
          "prompt.txt": `Implementer → reviewer, WID-1. ${said}\n`,
          "reviewer-wake.json": wakeOf(said),
        },
      });
      const plain = "Pushed one commit c9268b02bfc2 fixing the open thread PRRT_kwDOfix002.";
      const rigWork = freshDir("launch-composed-word");
      const rigRan = path.join(runDirOf(rigWork, name), "agent-ran");
      const rig = launched(rigWork, name, `: >${rigRan}`, [], {
        files: {
          "prompt.txt": `Implementer → reviewer, WID-1, a text-only round. ${plain}\n`,
          "reviewer-wake.json": wakeOf(plain),
        },
      });
      expect({
        agent: { code: agent.result.code, stderr: agent.result.stderr, ran: existsSync(agentRan) },
        rig: {
          code: rig.result.code,
          refused: rig.result.stderr.includes(
            `the world of ${path.basename(rig.run)} names "text-only" in <run>/prompt.txt`
          ),
          ran: existsSync(rigRan),
        },
      }).toEqual({
        agent: { code: 0, stderr: "", ran: true },
        rig: { code: 1, refused: true, ran: false },
      });
    }
  );

  // Catches: a guard that refuses every reviewer for the base rule its system prompt names. The
  // prompt names `require_last_push_approval`, and every world's base rules answer it (rig.sh's
  // base_ruleset_routes); folded, `_` read as `-`, the case `last-push` stands inside it, and a
  // case inside a longer identifier joined by `_` or `-` is that identifier's and not the rig's
  // word (the architect's ruling D6 of INFRA-764's plan v9, run-names.ts's `besideWord`). The
  // second launch is the mutant: the same file naming the case standing alone still refuses.
  test.skipIf(withoutNamespace)(
    "a reviewer whose system prompt names require_last_push_approval starts; the case last-push standing alone still refuses",
    () => {
      const name = "reviewer-text-findings-last-push-head-1";
      /** The launch of the run in a work directory of its own whose system prompt's file holds
       * `text`, and whether its agent ran. */
      const prompted = (text: string) => {
        const work = freshDir("launch-rule-name");
        const run = runDirOf(work, name);
        const { result } = launched(work, name, `: >${run}/agent-ran`, [], {
          files: {
            "system-args": `--append-system-prompt ${run}/state/secrets/system-prompt.md`,
            "state/secrets/system-prompt.md": text,
          },
        });
        return { run, result, ran: existsSync(path.join(run, "agent-ran")) };
      };
      const rule = prompted(
        "Push no fix where the branch's rules answer `require_last_push_approval: true`.\n"
      );
      const alone = prompted("Push no fix in the last-push round.\n");
      expect({
        rule: { code: rule.result.code, stderr: rule.result.stderr, ran: rule.ran },
        alone: {
          code: alone.result.code,
          refused: alone.result.stderr.includes(
            `the world of ${path.basename(alone.run)} names "last-push" in <run>/state/secrets/system-prompt.md`
          ),
          ran: alone.ran,
        },
      }).toEqual({
        rule: { code: 0, stderr: "", ran: true },
        alone: { code: 1, refused: true, ran: false },
      });
    }
  );

  // Catches: a guard that reads a system prompt it cannot read as one with nothing wrong in it.
  // <run>/system-args names a file (run-names.ts's `systemPromptIn`), and a file that is not
  // there, or is empty, or an argument of neither shape the daemon's composer answers with, is a
  // prompt the guard cannot hold to the vocabulary: each refuses the launch, naming the run and
  // what it found, and the agent does not start.
  test("a system prompt the guard cannot read refuses the launch", () => {
    const name = "threads-closed-correct-head-1";
    const rows: {
      what: string;
      refusal: string;
      files: (run: string) => Record<string, string>;
    }[] = [
      {
        what: "a path that names no file",
        refusal: "names <run>/state/secrets/system-prompt.md, which cannot be read",
        files: (run) => ({
          "system-args": `--append-system-prompt ${run}/state/secrets/system-prompt.md`,
        }),
      },
      {
        what: "an empty prompt file",
        refusal: "names <run>/state/secrets/system-prompt.md, which is empty",
        files: (run) => ({
          "system-args": `--append-system-prompt ${run}/state/secrets/system-prompt.md`,
          "state/secrets/system-prompt.md": "\n",
        }),
      },
      {
        what: "an empty prompt carried in the argument",
        refusal: "is empty: <run>/system-args holds",
        files: () => ({ "system-args": "--append-system-prompt ''" }),
      },
      {
        what: "no --append-system-prompt argument",
        refusal: "is no --append-system-prompt argument: <run>/system-args holds",
        files: () => ({ "system-args": "a prompt with no flag\n" }),
      },
      {
        what: "a word that is neither a quoted prompt nor an absolute path",
        refusal: "is neither a prompt nor the path of a prompt's file: <run>/system-args holds",
        files: () => ({ "system-args": "--append-system-prompt system-prompt.md" }),
      },
    ];
    const missed: string[] = [];
    for (const row of rows) {
      const work = freshDir("launch-unreadable-prompt");
      const runDir = runDirOf(work, name);
      const ran = path.join(runDir, "agent-ran");
      const { run, result } = launched(work, name, `: >${ran}`, [], { files: row.files(runDir) });
      const refused = `the world of ${path.basename(run)} cannot be read whole: the system prompt of ${path.basename(run)} ${row.refusal}`;
      if (result.code !== 1 || !result.stderr.includes(refused) || existsSync(ran))
        missed.push(`${row.what}: exit ${result.code}: ${result.stderr.trim()}`);
    }
    expect(missed).toEqual([]);
  });

  // Catches: a guard that reads a <run>/reviewer-wake.json it cannot parse as one that relays no
  // message, or one that dies of it naming nothing. The guard leaves out of the prompt only what
  // that file says the rig relayed from another agent of the run (run-names.ts's `promptOf`), so
  // a file of no shape the rig writes is a prompt whose own text the guard cannot tell from an
  // agent's: each refuses the launch, naming the file, and the agent does not start.
  test("a reviewer-wake.json the guard cannot parse refuses the launch, naming the file", () => {
    const name = "threads-closed-correct-head-1";
    const message = "Pushed one commit c9268b02bfc2 fixing the open thread PRRT_kwDOfix002.";
    const relayed = { at: "2026-11-20T16:46:07.292Z", message, sender: "01a1f9d6" };
    const rows: { what: string; text: string }[] = [
      { what: "no JSON", text: '{"wake": [' },
      { what: "no wake", text: JSON.stringify({ messages: [relayed] }) },
      {
        what: "a message of no text",
        text: JSON.stringify({ wake: [{ ...relayed, message: 1 }] }),
      },
      {
        what: "a time that is none",
        text: JSON.stringify({ wake: [{ ...relayed, at: "today" }] }),
      },
    ];
    const missed: string[] = [];
    for (const row of rows) {
      const work = freshDir("launch-unparsed-wake");
      const ran = path.join(runDirOf(work, name), "agent-ran");
      const { run, result } = launched(work, name, `: >${ran}`, [], {
        files: {
          "prompt.txt": `Implementer → reviewer, WID-1. ${message}\n`,
          "reviewer-wake.json": row.text,
        },
      });
      const refused = `the world of ${path.basename(run)} cannot be read whole: <run>/reviewer-wake.json cannot be parsed: `;
      if (result.code !== 1 || !result.stderr.includes(refused) || existsSync(ran))
        missed.push(`${row.what}: exit ${result.code}: ${result.stderr.trim()}`);
    }
    expect(missed).toEqual([]);
  });

  // Catches: a guard that reads what the rig did not compose, and refuses a run on it. The
  // label's install ships a checkout's own words: every base agent of the preflight at cc09c638
  // was refused on `var SPEC_WRITING_POINTER` in pi-legion's dist/legion.js, a production name,
  // and the head's passed only because its legion.js was over a size bound, so the plant here is
  // small. And what an agent wrote before a later agent's launch holds the agents' own words
  // (run-names.ts's AGENTS_OWN): the first agent's transcript refused
  // threads-closed-correct-head-1's reviewer. Each of those plants a word in one such file, and
  // the agent starts. What an agent of the run can write (run-names.ts's AGENT_WRITTEN) is read at
  // the run's first launch alone (the architect's ruling of 2026-11-21T14:24:55Z): its working
  // directory, and the gh answers the gh stand-in rewrites on its body edit. There the first agent
  // writes the word, and the run's second launch starts; the same word in the same file before
  // the first launch is the rig's, and refuses it, naming the word and the file. The rows start
  // agents, so they need the namespace an agent starts in, as their siblings that start one do.
  test.skipIf(withoutNamespace)(
    "a word in the label's install or in what the run's agents wrote does not refuse a launch, and in what an agent can write it refuses the first",
    () => {
      const name = "threads-closed-correct-head-1";
      const plants: { what: string; options: (work: string) => Launch }[] = [
        {
          what: "the label's packed plugin",
          options: (work) => {
            const dist = path.join(installOf(work, "head"), "pi-legion", "dist");
            mkdirSync(dist, { recursive: true });
            writeFileSync(
              path.join(dist, "legion.js"),
              'var SPEC_WRITING_POINTER = "dispatch://WID/artifact/spec";\n'
            );
            return {};
          },
        },
        {
          what: "the first agent's transcript",
          options: () => ({
            files: {
              "sessions/2026-11-16T00-00-00-000Z_s1.jsonl": `${JSON.stringify({
                type: "message",
                message: {
                  role: "assistant",
                  content: [{ type: "text", text: "These are text-only threads." }],
                },
              })}\n`,
            },
          }),
        },
        {
          what: "the first agent's output",
          options: () => ({
            files: { "out-implementer.txt": "The code-defect is fixed.\nexit=0\n" },
          }),
        },
      ];
      const refused: string[] = [];
      for (const plant of plants) {
        const work = freshDir("launch-unread");
        const ran = path.join(runDirOf(work, name), "agent-ran");
        const { result } = launched(work, name, `: >${ran}`, [], plant.options(work));
        if (result.code !== 0 || !existsSync(ran))
          refused.push(`${plant.what}: exit ${result.code}: ${result.stderr.trim()}`);
      }
      // What an agent of the run can write: written by the first agent, and by the rig.
      const written: { what: string; file: string; line: string; word: string }[] = [
        {
          what: "a file in the working directory",
          file: "cwd/notes.md",
          line: "text-only, both threads",
          word: "text-only",
        },
        {
          what: "the gh answers a body edit rewrote",
          file: "fixtures.json",
          line: '{"routes":[{"stdout":"threads-closed"}]}',
          word: "threads-closed",
        },
      ];
      const unrefused: string[] = [];
      for (const plant of written) {
        const work = freshDir("launch-written");
        const R = runDirOf(work, name);
        const first = launched(work, name, `printf '%s\\n' '${plant.line}' >${R}/${plant.file}`);
        const ran = path.join(R, "agent-ran");
        const later = launched(work, name, `: >${ran}`);
        const wrote = existsSync(path.join(R, plant.file))
          ? readFileSync(path.join(R, plant.file), "utf8")
          : "";
        if (
          first.result.code !== 0 ||
          wrote !== `${plant.line}\n` ||
          later.result.code !== 0 ||
          !existsSync(ran)
        )
          refused.push(
            `${plant.what}, the first agent's: exit ${first.result.code} then ${later.result.code}: ${later.result.stderr.trim()}`
          );
        const rigWork = freshDir("launch-written-before");
        const rigRan = path.join(runDirOf(rigWork, name), "agent-ran");
        const before = launched(rigWork, name, `: >${rigRan}`, [], {
          files: { [plant.file]: `${plant.line}\n` },
        });
        if (
          before.result.code !== 1 ||
          !before.result.stderr.includes(`names "${plant.word}" in <run>/${plant.file}`) ||
          existsSync(rigRan)
        )
          unrefused.push(
            `${plant.what}, the rig's: exit ${before.result.code}: ${before.result.stderr.trim()}`
          );
      }
      expect({ refused, unrefused }).toEqual({ refused: [], unrefused: [] });
    }
  );

  // Catches: a probe that reads a refusal as a namespace, or reports none of the host's words:
  // the skip of every namespace row and the refusal of every run carry the text it prints.
  test("namespace_probe prints the host's refusal and fails where unshare refuses", () => {
    expect(namespaceProbe({ PATH: `${refusing()}:${childEnv.PATH}` })).toEqual({
      code: 1,
      stdout: REFUSAL,
    });
  });
});

// A batch's Postgres and its NATS are the servers SKILL_SCENARIOS_POSTGRES_URL and
// SKILL_SCENARIOS_NATS_URL name when both are set, and otherwise two containers it starts. The
// harness gives services_up a docker that records its command line and answers `docker port`, so
// the container branch runs where there is no docker, a pg_sql that answers every query with
// 100, the max_connections of a server that admits 100, a `go` that answers connections_needed's
// `go run` of store/cmd/connections with 30 connections for each run, the count the rows' numbers
// are worked from, and records nothing of that call, and records the directory each build runs in
// and writes a binary naming it, a `bun` that records each read of a label's daemon facts
// (label_facts) with the work directory and the build it was given, and refuses it when
// FACTS_REFUSE names why, and runs every other call as bun does (services_from's read of the
// programs a run runs, run-programs.ts, and label_contract's reading of the label's own contract,
// standin-record.ts), and a load_label whose label's checkout is a directory of the row's own
// (<work>/checkouts/<label>), holding a contract of the row's own: one that describes every state
// body unless the row gives the label another (`contracts`).
describe("services_up and services_down", () => {
  const stubs = `
until_true() { shift 2; "$@"; }
pg_sql() { printf '%s\\n' 100; }
nats_ready() { :; }
start_listener() { started=$(( \${started:-0} + 1 )); envoy_port=410$started; printf '%s %s\\n' "$1" "$2" >>"$S/listeners"; }
load_label() { P=$work/profiles/$1; co=$work/checkouts/$1; pin=omp@stub; profile=skills-$1; }
pinned_omp() { printf '%s\\n' "$work/omp"; }
go() {
  case "$*" in
  'run ./internal/dispatch/store/cmd/connections -process') printf '%s\\n' 30 ;;
  *) printf '%s go %s\\n' "$PWD" "$*" >>"$S/go-calls"; printf 'built in %s\\n' "$PWD" >"$3" ;;
  esac
}
bun() {
  case $1 in
  */daemon-pane.ts)
    printf '%s|%s|%s\\n' "$SKILL_SCENARIOS_WORK" "$SKILL_SCENARIOS_DAEMON_PANE" "$*" >>"$S/facts-calls"
    [ -z "\${FACTS_REFUSE:-}" ] || { printf 'daemon-pane facts: %s\\n' "$FACTS_REFUSE" >&2; return 1; }
    ;;
  *) command bun "$@" ;;
  esac
}
docker() {
  printf 'docker %s\\n' "$*" >>"$S/docker-calls"
  case $1 in port) case $2 in *-pg) echo 127.0.0.1:55432 ;; *) echo 127.0.0.1:54222 ;; esac ;; esac
}`;
  /** services_up's own definition and every helper of rig.sh's it calls. */
  const servicesUp = [
    ...[
      "note",
      "fail",
      "given_services",
      "connections_needed",
      "safe_url",
      "build_dir",
      "go_binary",
      "daemon_pane_binary",
      "label_facts",
      "label_contract",
      "file_digest",
      "read_nats_image",
      "system_gitconfig",
      "services_from",
      "label_services",
      "label_provenance",
      "services_up",
    ].map(fn),
    assignment("postgres_image"),
    assignment("worker_boots"),
  ].join("\n");
  const given = {
    SKILL_SCENARIOS_POSTGRES_URL: "postgres://rig:secret@127.0.0.1:5432/postgres?sslmode=disable",
    SKILL_SCENARIOS_NATS_URL: "nats://127.0.0.1:4222",
  };
  /** The directory rig.sh's build_dir keeps `checkout`'s binaries in, under `work`: the first 16
   * hex digits of the digest of the checkout's path. */
  const builds = (work: string, checkout: string) =>
    path.join(
      work,
      "builds",
      new Bun.CryptoHasher("sha256").update(checkout).digest("hex").slice(0, 16)
    );
  /** A label's contract that describes every state body: the rows that hold services_up to
   * anything but the contract check give each label this one. */
  const DESCRIBES =
    "export const LegionStateResponse = { safeParse: () => ({ success: true }) };\n";
  /** services_up for `labels`, each with a checkout of its own under the work directory, whose
   * packages/contracts/src/legion-api.ts is `contracts`' source for that label, or `DESCRIBES`, and
   * whose packages/daemon/internal/api state.go, server.go, shaping.go, version.go and
   * updatestate.go are this checkout's: main's shape of the state's pull request and of the
   * contract its answers state, which the contract check builds its bodies by (daemon-pane.ts's
   * `servesFixAttempts` and `statesContract`), as both labels of a batch are main's now. */
  function up(
    env: Record<string, string>,
    labels: string[] = ["head"],
    contracts: Record<string, string> = {}
  ) {
    const work = freshDir("services-up");
    const S = path.join(work, "services");
    mkdirSync(S);
    for (const label of labels) {
      for (const module of ["envoy", "daemon"])
        mkdirSync(path.join(work, "checkouts", label, "packages", module), { recursive: true });
      const contract = path.join(work, "checkouts", label, "packages", "contracts", "src");
      mkdirSync(contract, { recursive: true });
      writeFileSync(path.join(contract, "legion-api.ts"), contracts[label] ?? DESCRIBES);
      const api = path.join(work, "checkouts", label, "packages", "daemon", "internal", "api");
      mkdirSync(api, { recursive: true });
      for (const file of ["state.go", "server.go", "shaping.go", "version.go", "updatestate.go"])
        cpSync(
          path.join(CHECKOUT, "packages", "daemon", "internal", "api", file),
          path.join(api, file)
        );
    }
    const result = bash(
      `${servicesUp}\n${stubs}\nwork=$1 S=$1/services root=$2 here=$3 tag=rig-test\n. "$root/packages/envoy/scripts/lib/pg-url.sh"\n. "$root/scripts/e2e/lib/loopback-dsn.sh"\nservices_postgres=$S/postgres-url tokens=$S/tokens\nshift 3\nservices_up "$@"`,
      [work, root, import.meta.dir, ...labels],
      env
    );
    return { work, S, ...result };
  }

  // Catches: a batch that starts a container although the operator gave it both servers, which is
  // every batch in a Legion pod and in a builds Job, where there is no docker at all.
  test("both URLs set: no container, the Postgres recorded 0600 for each run, the given NATS in the services env", () => {
    const { S, code, stdout, stderr } = up({ ...given, SKILL_SCENARIOS_PARALLEL: "2" });
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    expect(existsSync(path.join(S, "docker-calls"))).toBe(false);
    expect(readFileSync(path.join(S, "postgres-url"), "utf8")).toBe(
      `${given.SKILL_SCENARIOS_POSTGRES_URL}\n`
    );
    expect(statSync(path.join(S, "postgres-url")).mode & 0o777).toBe(0o600);
    expect(readFileSync(path.join(S, "env-head"), "utf8")).toContain(
      "ENVOY_NATS_URL=nats://127.0.0.1:4222\n"
    );
    // The listener is still built.
    expect(readFileSync(path.join(S, "go-calls"), "utf8")).toContain("build -o");
    // No message prints the password.
    expect(stdout).toContain("Postgres postgres://rig@127.0.0.1:5432/postgres?sslmode=disable");
    expect(stdout).not.toContain("secret");
  });

  // Catches: a batch that starts its runs against a given server with too few connections, where
  // each run's Dispatch server takes 30 and the run that cannot get them fails for the rig's fault.
  test("a parallelism the given Postgres cannot serve is refused, naming both numbers", () => {
    const { S, code, stderr } = up({ ...given, SKILL_SCENARIOS_PARALLEL: "3" });
    expect(code).toBe(1);
    expect(stderr).toContain("needs 110 connections");
    expect(stderr).toContain("which admits 100");
    expect(stderr).toContain("set SKILL_SCENARIOS_PARALLEL to 2 or lower");
    // It refuses before it builds anything.
    expect(existsSync(path.join(S, "go-calls"))).toBe(false);
  });

  // Catches: a batch on a box with docker that takes one of the two URLs, or none, and stops
  // starting its containers.
  test("neither URL, or one without the other: the two containers, and no Postgres recorded", () => {
    const none: Record<string, string> = {};
    for (const env of [none, { SKILL_SCENARIOS_NATS_URL: given.SKILL_SCENARIOS_NATS_URL }]) {
      const { S, code, stdout } = up({ ...env, SKILL_SCENARIOS_PARALLEL: "1" });
      expect(code).toBe(0);
      const calls = readFileSync(path.join(S, "docker-calls"), "utf8");
      expect(calls).toContain("--name rig-test-pg");
      expect(calls).toContain("max_connections=50");
      expect(calls).toContain("--name rig-test-nats");
      expect(existsSync(path.join(S, "postgres-url"))).toBe(false);
      expect(readFileSync(path.join(S, "env-head"), "utf8")).toContain(
        "ENVOY_NATS_URL=nats://127.0.0.1:54222\n"
      );
      expect(stdout).toContain(
        "Postgres postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable"
      );
    }
  });

  // Catches: one listener built from the rig's own checkout for every label, which paired main
  // ff2a10ef's pi-envoy, from before #2603, with a listener from after it, and lost the base runs'
  // claim boot to its `409 not_requested`; a listener started from a binary other than its
  // label's; and labels whose runs are pointed at one listener. Each label's Dispatch server and
  // its build of daemon-pane.go are its own checkout's too: a base run on the head's Dispatch
  // server read the head's `dispatch result` lines and had a body field taken that its own server
  // refuses.
  test("each label's listener, Dispatch server and daemon-pane are built in that label's own checkout, and its listener started from that build and named in its services file", () => {
    const { work, S, code, stderr } = up({ ...given, SKILL_SCENARIOS_PARALLEL: "1" }, [
      "head",
      "base",
    ]);
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    const checkout = (label: string) => path.join(work, "checkouts", label);
    const built = (label: string, name: string) => path.join(builds(work, checkout(label)), name);
    const listener = (label: string) => built(label, "envoy-listener");
    expect(readFileSync(path.join(S, "go-calls"), "utf8").split("\n")).toEqual([
      ...["head", "base"].flatMap((label) => [
        `${checkout(label)}/packages/envoy go build -o ${listener(label)} ./cmd/listener`,
        `${checkout(label)}/packages/envoy go build -o ${built(label, "dispatch-e2e")} ./cmd/dispatch`,
        `${checkout(label)}/packages/daemon go build -o ${built(label, "daemon-pane")} -overlay ${built(label, "daemon-pane-overlay.json")} ./cmd/daemon-pane`,
      ]),
      "",
    ]);
    // The overlay puts this directory's program in place of the label's module's main, as
    // daemon-pane.ts's runDaemonPane does for a `go run`.
    expect(JSON.parse(readFileSync(built("base", "daemon-pane-overlay.json"), "utf8"))).toEqual({
      Replace: {
        [`${checkout("base")}/packages/daemon/cmd/daemon-pane/main.go`]: path.join(
          import.meta.dir,
          "daemon-pane.go"
        ),
      },
    });
    expect(readFileSync(listener("base"), "utf8")).toBe(
      `built in ${checkout("base")}/packages/envoy\n`
    );
    expect(readFileSync(path.join(S, "listeners"), "utf8")).toBe(
      `head ${listener("head")}\nbase ${listener("base")}\n`
    );
    expect(readFileSync(path.join(S, "env-head"), "utf8")).toContain(
      "ENVOY_URL=http://127.0.0.1:4101\n"
    );
    expect(readFileSync(path.join(S, "env-base"), "utf8")).toContain(
      "ENVOY_URL=http://127.0.0.1:4102\n"
    );
  });

  // Catches: a provenance that names the rig's checkout, or none, for a service the label's own
  // checkout gives its runs, and one whose digest is not the binary the label's listener runs.
  test("each label's provenance names its own checkout for every service of its own, with its listener's digest", () => {
    const { work, S, code } = up({ ...given, SKILL_SCENARIOS_PARALLEL: "1" }, ["head", "base"]);
    expect(code).toBe(0);
    const checkout = path.join(work, "checkouts", "base");
    const binary = readFileSync(path.join(builds(work, checkout), "envoy-listener"));
    const lines = readFileSync(path.join(S, "services-from-base"), "utf8").split("\n");
    expect(lines).toContain(
      `envoy-listener label ${checkout} sha256:${new Bun.CryptoHasher("sha256").update(binary).digest("hex")}`
    );
    const own = lines.filter((line) => line.split(" ")[1] === "label");
    expect(own.map((line) => line.split(" ")[0])).toEqual([
      "envoy-listener",
      "dispatch",
      "plugins",
      "model-route",
      "daemon-facts",
      "worker-pane",
    ]);
    for (const line of own) expect(line.split(" ")[2]).toBe(checkout);
    expect(lines).toContain("omp pin omp@stub missing");
    const digest = (name: string) =>
      `sha256:${new Bun.CryptoHasher("sha256").update(readFileSync(path.join(builds(work, checkout), name))).digest("hex")}`;
    expect(lines).toContain(`dispatch label ${checkout} ${digest("dispatch-e2e")}`);
    expect(lines).toContain(`daemon-facts label ${checkout} ${digest("daemon-pane")}`);
    expect(lines).toContain(`worker-pane label ${checkout} ${digest("daemon-pane")}`);
    // The Dispatch server is no shared service any more.
    expect(lines.filter((line) => line.startsWith("dispatch "))).toHaveLength(1);
    expect(lines).toContain("nats shared nats://127.0.0.1:4222 -");
    expect(lines).toContain(
      "postgres shared postgres://rig@127.0.0.1:5432/postgres?sslmode=disable -"
    );
  });

  // A stand-in's boot reads every fact of its label's daemon it answers by. Read at the boot, each
  // was a `go run` of daemon-pane.go, a compile of the daemon module: a first boot took ~43 s
  // (head) and ~36 s (base) with a warm Go build cache, past worker_boot's 30 s bound, and the
  // 2026-11-15 preflight lost 6 of 8 runs there. services_up reads them all first (label_facts).
  describe("daemon facts", () => {
    // Catches: a batch whose runs start before the facts of their label's daemon are read, a read
    // made by `go run` or by another label's build, one kept anywhere but the work directory's
    // facts cache, and one that leaves out a stand-in a scenario boots (worker_boots).
    test("each label's facts are read through its own build into the work directory, for every stand-in of worker_boots", () => {
      const { work, S, code, stderr } = up({ ...given, SKILL_SCENARIOS_PARALLEL: "1" }, [
        "head",
        "base",
      ]);
      expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
      const checkout = (label: string) => path.join(work, "checkouts", label);
      expect(readFileSync(path.join(S, "facts-calls"), "utf8")).toBe(
        ["head", "base"]
          .map(
            (label) =>
              `${work}|${path.join(builds(work, checkout(label)), "daemon-pane")}|${import.meta.dir}/daemon-pane.ts facts ${checkout(label)} tester reviewer implementer:reviewing implementer:accepting reviewer:reviewing reviewer:accepting\n`
          )
          .join("")
      );
    });

    // Catches: a fact that cannot be read left for a run's boot to meet, and a batch that starts a
    // label's listener, or writes the provenance a run needs to start, before its facts are read.
    test("a fact that cannot be read fails the batch before the label's listener or any run starts, naming it", () => {
      const why =
        "the reviewer's stand-in in reviewing: the daemon fact `phase reviewer` of /co could not be read";
      const { work, S, code, stderr } = up(
        { ...given, SKILL_SCENARIOS_PARALLEL: "1", FACTS_REFUSE: why },
        ["base"]
      );
      expect(code).toBe(1);
      expect(stderr).toContain(`daemon-pane facts: ${why}\n`);
      expect(stderr).toContain(
        `skill-scenarios: a fact of the daemon of label base's checkout ${path.join(work, "checkouts", "base")} could not be read (above), so no daemon stand-in of the label could boot\n`
      );
      expect(existsSync(path.join(S, "listeners"))).toBe(false);
      expect(existsSync(path.join(S, "services-from-base"))).toBe(false);
      expect(existsSync(path.join(S, "env-base"))).toBe(false);
    });
  });

  // The state a daemon stand-in serves is read by the plugin of the run's own label, with that
  // label's own contract. Catches: a batch that starts a label whose contract does not describe
  // that state, whose agents then meet the refusal in their own turns, every `read_record` of
  // theirs answered `a body the daemon API contract does not describe`, while the other label
  // reads its own: all 30 base runs of 2026-11-09 measured a world of their own so. services_up
  // hands each label's own schema every kind of body a stand-in can serve (label_contract) and
  // stops before that label's listener starts, naming the label and the schema's error. The base
  // label's contract here is a fixture's: one of a daemon that runs its claims in tmux panes
  // alone, which the sandbox locator of the stand-in's claims is no value of.
  describe("each label's contract", () => {
    const tmuxOnly = `
const claims = (issue) => [["architect", issue.architect], ...Object.entries(issue.workers).map(([role, row]) => ["workers." + role + ".claim", row.claim])];
export const LegionStateResponse = {
  safeParse(body) {
    for (const [key, issue] of Object.entries(body.issues))
      for (const [at, claim] of claims(issue))
        if (claim?.locator !== undefined && claim.locator.runtime !== "tmux")
          return { success: false, error: { message: "issues." + key + "." + at + ".locator.runtime: Invalid input: expected \\"tmux\\"" } };
    return { success: true };
  },
};
`;

    test("a label whose own contract does not describe the stand-in's state stops the batch before its listener starts, naming the label and the error", () => {
      const { work, S, code, stderr } = up(
        { ...given, SKILL_SCENARIOS_PARALLEL: "1" },
        ["head", "base"],
        { base: tmuxOnly }
      );
      const contract = path.join(
        work,
        "checkouts",
        "base",
        "packages",
        "contracts",
        "src",
        "legion-api.ts"
      );
      expect({ code, stderr }).toEqual({
        code: 1,
        stderr: [
          `standin-record contract: label base: ${contract} does not describe the state body the stand-in serves (planning, pull request 0): issues.WID-1.architect.locator.runtime: Invalid input: expected "tmux"`,
          `skill-scenarios: label base's own contract (${contract}) does not describe the state the daemon stand-in serves (above), so every read_record of that label's agents would be refused`,
          "",
        ].join("\n"),
      });
      // The head label, whose contract describes the state, was started before the base label
      // was read; nothing of the base label was.
      expect(readFileSync(path.join(S, "listeners"), "utf8")).toMatch(/^head \S+\n$/);
      expect(existsSync(path.join(S, "env-head"))).toBe(true);
      expect(existsSync(path.join(S, "env-base"))).toBe(false);
      expect(existsSync(path.join(S, "services-from-base"))).toBe(false);
    });

    // Catches: a check that reads the rig's own contract for every label, which describes the
    // rig's own stand-in by construction, or one that passes a label it could read no schema
    // of: the base label's checkout here holds a contract that exports no `LegionStateResponse`.
    test("a label whose contract exports no state schema is refused, naming the file, and nothing of it starts", () => {
      const { work, S, code, stderr } = up({ ...given, SKILL_SCENARIOS_PARALLEL: "1" }, ["base"], {
        base: "export const Unrelated = 1;\n",
      });
      const contract = path.join(
        work,
        "checkouts",
        "base",
        "packages",
        "contracts",
        "src",
        "legion-api.ts"
      );
      expect(code).toBe(1);
      expect(stderr).toContain(
        `standin-record contract: label base: ${contract} exports no LegionStateResponse to read a state body with\n`
      );
      expect(existsSync(path.join(S, "listeners"))).toBe(false);
    });
  });

  // A run's own Dispatch is started from its label's build. Catches: a run of the base label
  // started on the server the rig's own checkout built, where the head's `dispatch result` lines
  // and its acceptance of undeclared body fields are what the base agents met.
  describe("start_dispatch", () => {
    /** start_dispatch for a run of the label whose checkout is <work>/checkouts/base, the rig's
     * own checkout elsewhere, with the builds `built` names made: the server it handed
     * run-server.sh, or why it refused. */
    function started(built: ("label" | "rig")[]) {
      const work = freshDir("start-dispatch");
      const co = path.join(work, "checkouts", "base");
      const rigCheckout = path.join(work, "rig-checkout");
      for (const [which, checkout] of [
        ["label", co],
        ["rig", rigCheckout],
      ] as const) {
        if (!built.includes(which)) continue;
        mkdirSync(builds(work, checkout), { recursive: true });
        writeFileSync(path.join(builds(work, checkout), "dispatch-e2e"), "#!/bin/sh\n", {
          mode: 0o755,
        });
      }
      mkdirSync(path.join(work, "logs"));
      const result = bash(
        [
          fn("fail"),
          fn("build_dir"),
          fn("start_dispatch"),
          'start_process() { printf "%s %s\\n" "$DISPATCH_E2E_SERVER_BIN" "$*" >"$work/started"; dispatch_pid=1; }',
          "await_start() { :; }",
          "report_port() { echo 4242; }",
          "work=$1 evidence=$1 co=$2 root=$3",
          'start_dispatch "postgres://rig@127.0.0.1:5432/run_base?sslmode=disable"',
        ].join("\n"),
        [work, co, rigCheckout]
      );
      return { work, co, rigCheckout, result };
    }

    test("a run's Dispatch server is its label's build, under the rig's run-server.sh", () => {
      const { work, co, rigCheckout, result } = started(["label", "rig"]);
      expect(result).toMatchObject({ code: 0, stderr: "" });
      expect(readFileSync(path.join(work, "started"), "utf8")).toBe(
        `${path.join(builds(work, co), "dispatch-e2e")} dispatch bash ${rigCheckout}/packages/dispatch/e2e/run-server.sh\n`
      );
    });

    test("a run whose label's Dispatch server was not built is refused, though the rig's was", () => {
      const { work, co, result } = started(["rig"]);
      expect(result.code).toBe(1);
      expect(result.stderr).toBe(
        `skill-scenarios: no Dispatch server of ${co} at ${path.join(builds(work, co), "dispatch-e2e")}: the run's batch builds it (services_up)\n`
      );
      expect(existsSync(path.join(work, "started"))).toBe(false);
    });
  });

  /** file_digest of `name` in `dir`: `sha256:<hex>` of its bytes, or `missing`. */
  const digestOf = (dir: string, name: string) => {
    const file = path.join(dir, name);
    return existsSync(file)
      ? `sha256:${new Bun.CryptoHasher("sha256").update(readFileSync(file)).digest("hex")}`
      : "missing";
  };
  /** services_from for a label, run with its rig's directory at `here` (this one unless a row
   * gives a copy of it): its exit, its stderr, and the digest of each program of `here` it names,
   * `<program> shared <here> <digest>`, in its order. */
  function provenanceOf(here: string) {
    const result = bash(
      [
        `. ${JSON.stringify(path.join(root, "packages/envoy/scripts/lib/pg-url.sh"))}`,
        ...["fail", "given_services", "safe_url", "file_digest", "system_gitconfig"].map(fn),
        fn("services_from"),
        "pinned_omp() { echo /x/omp; }",
        `here=${here} co=/co pin=omp@pin I=/w/installs/0a1b2c3d4e5f profile=legion`,
        "nats_url=nats://127.0.0.1:4222 pg_url=postgres://127.0.0.1:5432/postgres",
        "services_from /l /d /p",
      ].join("\n"),
      [],
      given
    );
    const programs = new Map<string, string>();
    for (const line of result.stdout.split("\n")) {
      const [program = "", scope, from, digest = "", extra] = line.split(" ");
      if (scope === "shared" && from === here && extra === undefined) programs.set(program, digest);
    }
    return { code: result.code, stderr: result.stderr, programs };
  }

  // Catches: a program this directory runs on a run's path left off the run's provenance, so a
  // change to it changes a run unseen by any digest a batch records. The list services_from kept
  // by hand left out daemon-pane.ts (label_facts, worker_boot), score.ts (reviewer_wake),
  // completion-answers.ts (skip_reason) and judge.ts (judge_ts), and every module of the
  // directory those programs import but github-routes.ts. Catches too: a Go program daemon-pane.ts
  // hands to `go run`, which no function names (daemon-routes.go, daemon-completion.go,
  // gh-selectors.go), left off; a program only the command dispatch names, which no run runs
  // (`rig.sh health`'s health.ts), put on; an order but byte order; and a digest that is not the
  // file's own. The set is read here apart from run-programs.ts, which services_from reads it
  // with: every program a function of rig.sh names (scriptFunctions), every module of this
  // directory bun's bundler takes into one of them (its metafile), and every Go program.
  test("services_from names every program a run runs from this directory, by its own digest, in byte order", async () => {
    const here = realpathSync(import.meta.dir);
    const { code, stderr, programs } = provenanceOf(here);
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    const named = new Set<string>();
    for (const [, name = ""] of rigScript.matchAll(/^([A-Za-z_]\w*)\(\) \{/gm))
      for (const [, program = ""] of fn(name).matchAll(/\$here\/([\w.-]+\.(?:ts|go))\b/g))
        named.add(program);
    const built = await Bun.build({
      entrypoints: [...named]
        .filter((name) => name.endsWith(".ts"))
        .map((name) => path.join(here, name)),
      target: "bun",
      metafile: true,
    });
    expect(built.success).toBe(true);
    const bundled = Object.keys(built.metafile?.inputs ?? {})
      .map((input) => path.resolve(input))
      .filter((file) => path.dirname(file) === here)
      .map((file) => path.basename(file));
    const go = readdirSync(here).filter(
      (name) => name.endsWith(".go") && !name.endsWith("_test.go")
    );
    const expected = [...new Set([...named, ...bundled, ...go])].sort();
    const recorded = [...programs.keys()];
    expect({
      missing: expected.filter((name) => !programs.has(name)),
      unexpected: recorded.filter((name) => !expected.includes(name)),
    }).toEqual({ missing: [], unexpected: [] });
    expect(recorded).toEqual(expected);
    for (const [name, digest] of programs)
      expect(`${name} ${digest}`).toBe(`${name} ${digestOf(here, name)}`);
  });

  /** A directory of its own holding run-programs.ts, `files`, and a copy of rig.sh whose
   * functions name one more program, new-program.ts, and whose command dispatch, when
   * `dispatched` names a program, runs that one as a command of its own. */
  function rigCopy(name: string, files: Record<string, string>, dispatched?: string): string {
    const here = realpathSync(freshDir(name));
    const copy = rigScript.replace(
      /^case "\$\{1:-\}" in$/m,
      (dispatch) =>
        `start_new() { bun "$here/new-program.ts"; }\n${dispatch}${dispatched === undefined ? "" : `\nnew) shift && bun "$here/${dispatched}" ;;`}`
    );
    expect(copy).not.toBe(rigScript);
    writeFileSync(path.join(here, "rig.sh"), copy);
    cpSync(path.join(import.meta.dir, "run-programs.ts"), path.join(here, "run-programs.ts"));
    for (const [file, text] of Object.entries(files)) writeFileSync(path.join(here, file), text);
    return here;
  }

  // Catches: a provenance kept as a list, which a program rig.sh comes to run joins only when
  // someone edits it. A copy of rig.sh whose functions name one more program records it, each
  // module of the directory it imports, exports from or imports dynamically (past its `#!` line),
  // and one more Go program, with no list edited anywhere; it records no program only its command
  // dispatch names, and no Go test; and a program a function names that is not there is recorded
  // `missing`, not refused.
  test("a program rig.sh comes to run, and what it imports, are in services_from's provenance with no list edited", () => {
    const files: Record<string, string> = {
      "new-program.ts": [
        "#!/usr/bin/env bun",
        'import { one } from "./new-module";',
        'export * from "./new-reexport";',
        'const late = await import("./new-dynamic");',
        "console.log(one, late);",
        "",
      ].join("\n"),
      "new-module.ts": "export const one = 1;\n",
      "new-reexport.ts": "export const two = 2;\n",
      "new-dynamic.ts": "export const three = 3;\n",
      "dispatch-only.ts": "console.log(4);\n",
      "new-reader.go": "package main\n",
      "new-reader_test.go": "package main\n",
    };
    const here = rigCopy("services-from-copy", files, "dispatch-only.ts");
    const { code, stderr, programs } = provenanceOf(here);
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    const added = [
      "new-program.ts",
      "new-module.ts",
      "new-reexport.ts",
      "new-dynamic.ts",
      "new-reader.go",
    ];
    const asked = [...added, "dispatch-only.ts", "new-reader_test.go", "daemon-standin.ts"];
    expect(
      Object.fromEntries(asked.map((name) => [name, programs.get(name) ?? "unrecorded"]))
    ).toEqual({
      ...Object.fromEntries(added.map((name) => [name, digestOf(here, name)])),
      "dispatch-only.ts": "unrecorded",
      "new-reader_test.go": "unrecorded",
      "daemon-standin.ts": "missing",
    });
  });

  // Catches: a provenance written with the programs a run runs unread, which names none of them,
  // as a run of a rig that runs none would: where the reading fails (an import of a program that
  // does not resolve), services_from fails, naming why, and names no program. So it does for a
  // rig.sh whose command dispatch it cannot find, where a reading of the whole script would take
  // in the programs only a command runs (`rig.sh health`'s health.ts).
  test("services_from fails, naming why, where the programs a run runs cannot be read", () => {
    const unresolved = rigCopy("services-from-unread", {
      "new-program.ts": 'import { gone } from "./gone";\nconsole.log(gone);\n',
    });
    const undispatched = realpathSync(freshDir("services-from-undispatched"));
    const copy = rigScript.replace(/^case "\$\{1:-\}" in$/m, 'case "$1" in');
    expect(copy).not.toBe(rigScript);
    writeFileSync(path.join(undispatched, "rig.sh"), copy);
    cpSync(
      path.join(import.meta.dir, "run-programs.ts"),
      path.join(undispatched, "run-programs.ts")
    );
    for (const [here, why] of [
      [unresolved, /^run-programs: Cannot find module '\.\/gone'/],
      [undispatched, /^run-programs: \S+\/rig\.sh holds no command dispatch, so which programs/],
    ] as const) {
      const { code, stderr, programs } = provenanceOf(here);
      expect(code).toBe(1);
      expect(stderr).toMatch(why);
      expect(stderr).toContain(
        `\nskill-scenarios: services_from: the programs a run of ${here}/rig.sh runs could not be read (above), so no run's provenance can name them\n`
      );
      expect([...programs.keys()]).toEqual([]);
    }
  });

  // Catches: two labels' listeners started as one service (one log, one port file, one pid kept)
  // or under one machine id, whose durable NATS consumer the two would share and take each
  // other's deliveries from; a machine id that names the label, which an agent's `envoy_whoami`
  // answers (it is the batch's tag and the label's install id, load_label's I); and a listener
  // started from any binary but the one it is given.
  test("start_listener starts the label's binary as a service and a machine of its own", () => {
    const S = freshDir("start-listener");
    writeFileSync(path.join(S, "envoy-token"), "token\n");
    const result = bash(
      [
        fn("start_listener"),
        `start_held() { printf '%s|%s|%s\\n' "$1" "$ENVOY_MACHINE_ID" "\${@: -1}" >>"$S/held"; printf -v "$2" %s 4100; printf -v "\${1}_pid" %s "$((4200 + \${#listener_pids[@]}))"; }`,
        "await_start() { :; }",
        "S=$1 tokens=$1 tag=rig-test nats_url=nats://127.0.0.1:4222 listener_pids=()",
        "I=/w/installs/0a1b2c3d4e5f start_listener head /builds/a/envoy-listener",
        "I=/w/installs/9f8e7d6c5b4a start_listener ablate-x /builds/b/envoy-listener",
        `echo "\${listener_pids[*]}"`,
      ].join("\n"),
      [S]
    );
    expect(result).toMatchObject({ code: 0, stdout: "4200 4201\n" });
    expect(readFileSync(path.join(S, "held"), "utf8")).toBe(
      "listener_head|rig-test-0a1b2c3d4e5f|/builds/a/envoy-listener\nlistener_ablate_x|rig-test-9f8e7d6c5b4a|/builds/b/envoy-listener\n"
    );
  });

  /** The files a batch's services make for label head (rig.sh's services_files), under S. */
  const SERVICES_FILES = [
    "env-head",
    "services-from-head",
    "postgres-url",
    "tokens/envoy-token",
    "tokens/dispatch-token",
  ];
  /** services_up and the stubs its rows run it on, for a row that tears down after it. */
  const upStubs = `${servicesUp}\n${stubs}\nroot=${root}`;
  /** A work directory whose services directory holds every file a batch made (SERVICES_FILES,
   * the listener's port file and the lock), then services_down, with `prelude` run first. */
  function down(env: Record<string, string>, prelude = "") {
    const work = freshDir("services-down");
    const S = path.join(work, "services");
    mkdirSync(path.join(S, "tokens"), { recursive: true });
    for (const file of [...SERVICES_FILES, "up"]) writeFileSync(path.join(S, file), "");
    writeFileSync(path.join(work, "listener_head.port"), "4100\n");
    const result = bash(
      `${fn("given_services")}\n${fn("services_files")}\n${fn("services_down")}\nstop_tree() { printf 'stop_tree %s\\n' "$*" >>"$S/stopped"; }\ndocker() { printf 'docker %s\\n' "$*" >>"$S/docker-calls"; }\nwork=$1 S=$1/services tokens=$1/services/tokens tag=rig-test containers=()\nservices_postgres=$S/postgres-url\nlistener_pids=(4242 4343)\n${prelude}\nservices_down`,
      [work],
      env
    );
    return { S, work, ...result };
  }

  // Catches: a teardown that removes a server it was given, or the databases on it, one that
  // leaves a label's listener running, and one that leaves a file of the batch's services there,
  // which the next batch's lock would then refuse.
  test("both URLs set: every label's listener stops, the batch's own files go, and no docker runs", () => {
    const { S, work, code } = down(given);
    expect(code).toBe(0);
    expect(existsSync(path.join(S, "docker-calls"))).toBe(false);
    expect(readFileSync(path.join(S, "stopped"), "utf8")).toBe("stop_tree 4242\nstop_tree 4343\n");
    expect(readdirSync(S)).toEqual(["stopped"]);
    expect(existsSync(path.join(work, "listener_head.port"))).toBe(false);
  });

  // Catches: a teardown that leaves the batch's own containers running.
  test("neither URL: the two containers services_up started are the two services_down removes", () => {
    const { S, code } = down({}, `${upStubs}\nservices_up >/dev/null`);
    expect(code).toBe(0);
    expect(
      readFileSync(path.join(S, "docker-calls"), "utf8")
        .split("\n")
        .filter((line) => line.startsWith("docker rm"))
    ).toEqual(["docker rm -fv rig-test-pg rig-test-nats"]);
  });

  // Catches: a teardown that removes containers by their names alone, which removed an earlier
  // batch's container whose name the batch's own `docker run` could not take.
  test("a container docker refused to start under the batch's name is not removed", () => {
    const { S, code } = down(
      {},
      `${upStubs}\ndocker() { printf 'docker %s\\n' "$*" >>"$S/docker-calls"; case "$1 $*" in run*rig-test-pg*) echo 'Conflict. The container name "/rig-test-pg" is already in use' >&2; return 125 ;; esac; }\nset -e\ntrap services_down EXIT\nservices_up`
    );
    expect(code).toBe(125);
    const calls = readFileSync(path.join(S, "docker-calls"), "utf8").split("\n");
    expect(calls.filter((line) => line.startsWith("docker run")).length).toBe(1);
    expect(calls.filter((line) => line.startsWith("docker rm"))).toEqual([]);
  });

  // Catches: a batch that takes the lock over the files a batch killed outright left, and then
  // overwrites or removes them (services_up writes each, services_down removes each), a label's
  // own among them.
  test("a file an earlier batch left is refused, named, and kept, and the lock is not held", () => {
    const work = freshDir("services-lock");
    const S = path.join(work, "services");
    mkdirSync(path.join(S, "tokens"), { recursive: true });
    const earlier = fixtureSecret("envoy");
    writeFileSync(path.join(S, "tokens", "envoy-token"), `${earlier}\n`);
    writeFileSync(path.join(S, "services-from-base"), "envoy-listener label /co -\n");
    writeFileSync(path.join(work, "listener_base.port"), "4100\n");
    const result = bash(
      `${fn("fail")}\n${fn("services_files")}\n${fn("lock_services")}\nwork=$1 tag=rig-test tokens=$1/services/tokens\nservices_postgres=$work/services/postgres-url\nlock_services`,
      [work]
    );
    expect(result.code).toBe(1);
    expect(result.stderr).toContain(
      `an earlier batch left ${S}/tokens/envoy-token ${S}/services-from-base ${work}/listener_base.port; a batch replaces no earlier batch's file`
    );
    expect(readFileSync(path.join(S, "tokens", "envoy-token"), "utf8")).toBe(`${earlier}\n`);
    expect(readFileSync(path.join(work, "listener_base.port"), "utf8")).toBe("4100\n");
    expect(readdirSync(S).sort()).toEqual(["logs", "services-from-base", "tokens"]);
  });
});

// The probes services_up waits on: one for a given server as for a started container.
describe("nats_ready and the URL helpers", () => {
  /** A server on 127.0.0.1 that writes TEXT to every connection, as a process of its own: bash()
   * runs synchronously, so a listener in this process would never reach its accept. */
  async function greeting(text: string) {
    const child = Bun.spawn(
      [
        "bun",
        "-e",
        'const server = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { open: (socket) => void socket.write(process.env.GREETING ?? ""), data: () => {} } });\nconsole.log(server.port);',
      ],
      { cwd: childCwd, env: { ...childEnv, GREETING: text }, stdout: "pipe", stderr: "inherit" }
    );
    const reader = child.stdout.getReader();
    const { value } = await reader.read();
    reader.releaseLock();
    return {
      port: Number(new TextDecoder().decode(value).trim()),
      stop: async () => {
        child.kill();
        await child.exited;
      },
    };
  }

  // Catches: a probe that takes any process holding the port for a NATS, so a batch starts against
  // a dead one.
  test("the INFO line a NATS sends on connect, and nothing else, is ready", async () => {
    const probe = (port: number) =>
      bash(`${fn("nats_ready")}\nnats_ready "nats://127.0.0.1:$1"`, [String(port)]).code;
    const nats = await greeting('INFO {"server_id":"rig"}\r\n');
    const other = await greeting("+OK\r\n");
    try {
      expect(probe(nats.port)).toBe(0);
      expect(probe(other.port)).toBe(1);
    } finally {
      await nats.stop();
      await other.stop();
    }
    // Port 1, where nothing listens, is the rig's own address for that.
    expect(probe(1)).toBe(1);
  });
});

// scripts/e2e/plan-pointer-drive.ts composes the implementer's pane through this rig's own
// composer (daemon-pane.ts's `workerPane`), and no tsconfig of this repository covers
// scripts/e2e: the drive's `compose` step passed a claim with no `repository` and no
// `defaultBranch` — the two fields that composer requires and daemon-pane.go refuses a request
// without — and nothing typechecked it, so the drive failed where it ran and nowhere else.
// This row is that typecheck, over the one file of that directory this rig's modules reach, and
// the pi-legion job of .github/workflows/ci.yaml runs it with every other row of this file
// (`bun run test` in packages/pi-legion), on a pull request that changes the drive and nothing
// else: .github/ci-gates/pi-legion.py declares this file a read of that job's `pi_legion` gate,
// whose filter the changes job computes from it, and check-ci-filters.sh holds the two together.
// The program is written here rather than committed
// because the file belongs to no package: the compiler options are packages/pi-legion's own,
// with the paths and type roots a configuration outside the tree needs.
describe("the e2e drives that compose a pane through this rig", () => {
  test("plan-pointer-drive.ts typechecks, claim and all", () => {
    const dir = freshDir("e2e-typecheck");
    const config = path.join(dir, "tsconfig.json");
    writeFileSync(
      config,
      JSON.stringify({
        compilerOptions: {
          target: "ES2022",
          lib: ["ES2023"],
          module: "ESNext",
          moduleResolution: "Bundler",
          strict: true,
          noEmit: true,
          types: ["bun"],
          typeRoots: [path.join(root, "node_modules", "@types")],
          skipLibCheck: true,
          baseUrl: root,
          paths: { "@legion/contracts": ["packages/contracts/src/index.ts"] },
        },
        files: [path.join(root, "scripts", "e2e", "plan-pointer-drive.ts")],
      })
    );
    const tsc = spawnChecked(["bunx", "tsc", "-p", config], { cwd: root, env: childEnv });
    expect(`${tsc.stdout.toString()}${tsc.stderr.toString()}`).toBe("");
    expect(tsc.exitCode).toBe(0);
  });
});

// Each worker world as the rig builds and seeds it, read back through its own Dispatch and out of
// its own repository, against the issue state a Legion issue holds at that point of its life and
// against every concern a reviewer of round 24 and the rehearsal put on record about the fixture
// (dispatch://INFRA-764/artifact/infra-764-reviewer-concerns-txt: 46 reviews, 8 concerns). Each
// world runs rig.sh's own worker_run_world against a real Dispatch server on a Postgres of the
// caller's (LEGION_TEST_PG_DSN), as `rig.sh run` does — own_dispatch, seed.ts tester-proof, the
// fixture, plan, pull-request and implement-handoff — and then the seeds its runner adds
// (tester-result for a review round, review-handoff after an approval); only the issue workspace's
// jj clone, which nothing here reads, is left out.
//
// What a row asserts is named, so a world that drifts fails naming the row. In every world the
// issue resolves to the world's repository, has left triage for the status its phase shows, holds
// its approved plan.md, and `dispatch results` compares the check with the head of the world's pull
// request and says a pass stands there (concern 7, the defect this row was written for: the
// reviewer was handed a `before_merge` check with no current pass, which the merge gate counts as a
// behavioural finding). Of the eight concerns each is false in every world but one: the check that
// misses the blank name (6) is true in the two worlds whose CLI greets a blank name, where it is
// part of their one intended defect, with the blank-name test their code's author also left out.
const testPostgres = process.env.LEGION_TEST_PG_DSN;
describe.skipIf(testPostgres === undefined || withoutGo)(
  "each worker world, as the rig builds and seeds it",
  () => {
    /** The Dispatch server of this checkout, built once for every world here, as services_up
     * builds a label's (go_binary), on the disk TMPDIR names and never under this file's work
     * directory: the binary is larger than a pod's /dev/shm of 64 MiB holds (`no space left on
     * device`, 2026-11-25). */
    const serverDir = temporaryDirectory("suite-dispatch");
    let server = "";
    /** The caller's go caches, which a stand-in's read of its daemon's facts compiles with
     * (health's `goSettings` says why a HOME of childEnv's holds none), and the one work
     * directory whose facts cache every stand-in of this block reads, so one world pays for the
     * read (daemon-pane.ts's `factsCache`). */
    let go: Record<string, string> = {};
    let factsWork = "";
    beforeAll(() => {
      server = path.join(serverDir, "dispatch-e2e");
      const built = spawnChecked(["go", "build", "-o", server, "./cmd/dispatch"], {
        cwd: path.join(root, "packages", "envoy"),
        env: process.env,
      });
      expect({ code: built.exitCode, err: built.stderr.toString() }).toEqual({ code: 0, err: "" });
      const printed = spawnChecked(
        ["go", "env", "-json", "GOCACHE", "GOMODCACHE", "GOPATH", "GOFLAGS", "GOTOOLCHAIN"],
        { stdout: "pipe", stderr: "pipe" }
      );
      expect(printed.exitCode).toBe(0);
      go = Object.fromEntries(
        Object.entries(JSON.parse(printed.stdout.toString()) as Record<string, string>).filter(
          ([, value]) => value !== ""
        )
      );
      factsWork = freshDir("world-daemon-facts");
    }, 900_000);
    // The databases this block makes, each named with a prefix of its own and never rig.sh's
    // `run_`: in CI LEGION_TEST_PG_DSN names the shared test Postgres, and the acceptance batch's
    // runner refuses to start while that server holds a `run_` database its own work directory
    // does not name, so a world row's database left there would stop it. own_dispatch is rig.sh's
    // own but for that one word. Each is dropped by the world's own exit trap and again here,
    // whatever the rows did, so a row killed before its trap ran leaves nothing behind.
    const WORLD_DATABASE_PREFIX = "worldrow_";
    const worldDatabases = new Set<string>();
    afterAll(async () => {
      const sql = new Bun.SQL({ url: testPostgres ?? "", max: 1 });
      try {
        for (const name of worldDatabases)
          await sql.unsafe(`DROP DATABASE IF EXISTS ${name} WITH (FORCE)`);
      } finally {
        await sql.close();
      }
    });
    const ownDispatch = fn("own_dispatch");

    /** One world built and seeded: its run directory, the issue, the pull request's number and
     * head, and what its Dispatch answered (seededWorld). */
    interface SeededWorld {
      run: string;
      issue: string;
      pr: string;
      head: string;
      issueRead: string;
      planRead: string;
      verification: string;
      results: string;
      /** The state the daemon stand-in served the booted agent, `dispatch read` of the issue after
       * it, and the issue's role holders its Dispatch held then, each "" for a world no stand-in
       * was booted on. */
      state: string;
      readAfter: string;
      holders: string;
    }

    /** The session the agent booted on a world registers under: a UUIDv7, as a session's is. */
    const BOOT_SESSION = "01a2d000-0000-7000-8000-00000000f006";

    /** The world CASE names (worker_fixture's), built and seeded by rig.sh's own functions, and
     * what its Dispatch answers about its issue: `dispatch read` of the issue and of its plan.md,
     * `dispatch doc-read` of its Verification document and `dispatch results`, each through the
     * real `dispatch` entry an agent runs. With `pushed`, the implementer's one commit of text
     * fixes is pushed to the pull request's branch first and the head moved as run_threads_closed
     * moves it between its halves (seed.ts pull-request), which is the state the reviewer reads.
     * With `boot`, the daemon stand-in is started on the world as worker_boot starts it for that
     * role, phase and completed roles, on a built-from naming this checkout; the agent's claim
     * boot is made against it (its registration under BOOT_SESSION, then its ready); and the
     * state it serves is read, as the agent's first `read_record` reads it, with `dispatch read`
     * of the issue after it.
     * The server is stopped and the run's database dropped before this returns. */
    function seededWorld(
      caseName: string,
      pushed = false,
      boot?: { role: string; phase: string; completed: string }
    ): SeededWorld {
      const dir = freshDir("seeded-world");
      const run = path.join(dir, `${Date.now().toString(16)}${dirs.toString(16).padStart(2, "0")}`);
      worldDatabases.add(`${WORLD_DATABASE_PREFIX}${path.basename(run)}`);
      expect(ownDispatch).toContain("database=run_");
      const script = [
        "set -euo pipefail",
        "R=$1 work=$2 co=$3 server=$4 case_name=$5 pushed=$6 boot_role=$7 boot_phase=$8 boot_completed=$9 root=$3 here=$3/packages/pi-legion/scripts/skill-scenarios",
        `. ${JSON.stringify(path.join(root, "scripts/e2e/lib/rig.sh"))}`,
        // pg_sql and safe_url take a URL's password out with pg-url.sh, which rig.sh sources.
        `. ${JSON.stringify(path.join(root, "packages/envoy/scripts/lib/pg-url.sh"))}`,
        ...[
          "fail",
          "note",
          "build_dir",
          "start_dispatch",
          ...(rigScript.includes("\ndispatch_whoami() {") ? ["dispatch_whoami"] : []),
          "pg_sql",
          "conninfo_url",
          "safe_url",
          "seed_ts",
          "start_heads",
          "heads_stored",
          "worker_run_world",
          "case_guard",
          "case_check",
          "worker_fixture",
          "tester_proof_world",
          "review_world",
          "approved_world",
          "round_world",
          "fixture_reviewer_commit",
          "fixture_open",
          "fixture_git",
          "fixture_docs_before",
          "fixture_first_commit",
          "fixture_round_files",
          "fixture_head_commit",
          "fixture_implement",
          "fixture_test_handoff",
          "fixture_checks",
          "fixture_body",
          "fixture_write",
          "concluded_checks",
          "base_ruleset_routes",
          "resolution_protection_routes",
          "role_identity",
          "role_env",
        ].map(fn),
        ownDispatch.replace("database=run_", `database=${WORLD_DATABASE_PREFIX}`),
        definition("fixture_cli"),
        "fixture_workspace() { :; }",
        ...[
          "worker_repo",
          "worker_base",
          "world_author_name",
          "world_author_email",
          "world_login",
          ...WORLD_APPS,
        ].map(assignment),
        quotedAssignment("text_finding_doc_line"),
        quotedAssignment("text_finding_trigger"),
        quotedAssignment("cli_example_line"),
        'evidence=$R tokens=$work/tokens services_postgres=$work/postgres worker_key=""',
        'mkdir -p "$R/logs" "$R/cli" "$tokens" "$(build_dir "$co")"',
        "printf 'e2e-token\\n' >\"$tokens/dispatch-token\"",
        'printf "%s\\n" "$TEST_POSTGRES" >"$services_postgres"',
        'ln -s "$server" "$(build_dir "$co")/dispatch-e2e"',
        `database=${WORLD_DATABASE_PREFIX}$(basename "$R")`,
        'trap \'stop_pid "$standin_pid"; stop_pid "$watcher_pid"; stop_pid "$dispatch_pid"; stop_pid "$github_pid"; pg_sql "$TEST_POSTGRES" "DROP DATABASE IF EXISTS $database WITH (FORCE)" >/dev/null 2>&1 || true\' EXIT',
        "dispatch_pid='' github_pid='' watcher_pid='' standin_pid=''",
        'worker_run_world 1 1700000000 "$case_name"',
        'case $case_name in text-only | defect | last-push) seed_ts tester-result "$R/fixture.json" "$R/test.json" "$head" ;; esac',
        'case $case_name in accept | correct | crossing | code-defect) seed_ts tester-result "$R/fixture.json" "$R/test.json" "$implemented"; seed_ts review-handoff "$R/fixture.json" "$R/review.json" ;; esac',
        'if [ "$pushed" = 1 ]; then',
        '  src=$R/src when="@1700000101 +0000"',
        "  printf 'It trims the spaces around the name.\\n' >>\"$src/docs/cli.md\"",
        '  fixture_by=implementer fixture_git commit -qam "docs: say that the name is trimmed"',
        '  fixture_git push -q "$R/remote.git" "legion/$worker_key"',
        '  head=$(git -C "$R/remote.git" rev-parse "legion/$worker_key")',
        '  await_start watcher "$watcher_pid" 30 "the head watcher to store the push" heads_stored',
        '  cp "$R/logs/heads.log" "$R/heads.txt"',
        "fi",
        'cli() { env -i PATH="$PATH" HOME="$R/cli" DISPATCH_STATE_DIR="$R/cli" DISPATCH_HOST=omp DISPATCH_SESSION_ID=world-reader DISPATCH_URL="http://127.0.0.1:$dispatch_port" DISPATCH_TOKEN=e2e-token bun "$root/packages/envoy-client/bin/dispatch.ts" "$@"; }',
        'cli read --issue "$worker_key" >"$R/read.txt"',
        'cli read --issue "$worker_key" --artifact plan-md >"$R/plan.txt"',
        'cli doc-read --issue "$worker_key" --artifact verification-md >"$R/verification.txt"',
        'cli results --issue "$worker_key" >"$R/results.txt"',
        'printf "%s %s %s\\n" "$worker_key" "$pr" "$head" >"$R/seeded.txt"',
        'if [ -n "$boot_role" ]; then',
        '  printf "checkout %s\\n" "$co" >"$R/built-from"',
        '  printf "rig-boot-token\\n" >"$R/boot-token"',
        '  SKILL_SCENARIOS_WORK="$FACTS_WORK" DISPATCH_URL="http://127.0.0.1:$dispatch_port" DISPATCH_TOKEN_FILE="$tokens/dispatch-token" bun "$here/daemon-standin.ts" 0 "$R/daemon.log" "$R/boot-token" "worldrow$(basename "$R")" "$worker_key" "$R" "$boot_role" "$boot_phase" "$boot_completed" >"$R/logs/daemon.log" 2>&1 &',
        "  standin_pid=$!",
        '  for _ in $(seq 1500); do grep -q "^listening on " "$R/logs/daemon.log" && break; kill -0 "$standin_pid" 2>/dev/null || break; sleep 0.2; done',
        '  at=$(sed -n "s|^listening on ||p" "$R/logs/daemon.log")',
        '  [ -n "$at" ] || { cat "$R/logs/daemon.log" >&2; exit 1; }',
        '  contract=$(curl -fsS -D - -o /dev/null "$at/healthz" | tr -d "\\r" | awk \'tolower($1) == "legion-daemon-contract:" { print $2 }\')',
        '  [ -n "$contract" ] || { echo "the stand-in states no contract on /healthz" >&2; exit 1; }',
        '  secret=$(curl -fsS -d "{\\"bootToken\\":\\"rig-boot-token\\",\\"sessionId\\":\\"$BOOT_SESSION\\",\\"pluginContract\\":$contract}" "$at/legion/v1/claims/register" | jq -r .secret)',
        '  curl -fsS -o /dev/null -d "{\\"secret\\":\\"$secret\\"}" "$at/legion/v1/claims/ready"',
        '  curl -fsS -o "$R/state.json" "$at/legion/v1/state"',
        '  cli read --issue "$worker_key" >"$R/read-after.txt"',
        `  printf 'Authorization: Bearer %s' e2e-token | curl -fsS -H @- "http://127.0.0.1:$dispatch_port/api/v1/issues/$worker_key/role-holders" >"$R/holders.json"`,
        "fi",
      ].join("\n");
      const seeded = spawnChecked(
        [
          "bash",
          "-c",
          script,
          "-",
          run,
          dir,
          root,
          server,
          caseName,
          pushed ? "1" : "",
          boot?.role ?? "",
          boot?.phase ?? "",
          boot?.completed ?? "",
        ],
        {
          env: {
            ...childEnv,
            ...go,
            TEST_POSTGRES: testPostgres ?? "",
            FACTS_WORK: factsWork,
            BOOT_SESSION,
          },
        }
      );
      const read = (name: string) =>
        existsSync(path.join(run, name)) ? readFileSync(path.join(run, name), "utf8") : "";
      expect({
        code: seeded.exitCode,
        err: seeded.stderr.toString(),
        fixture: read("fixture.log").slice(-4000),
      }).toMatchObject({ code: 0 });
      writeGhSelectors(run);
      const [issue = "", pr = "", head = ""] = read("seeded.txt").trim().split(" ");
      return {
        run,
        issue,
        pr,
        head,
        issueRead: read("read.txt"),
        planRead: read("plan.txt"),
        verification: read("verification.txt"),
        results: read("results.txt"),
        state: read("state.json"),
        readAfter: read("read-after.txt"),
        holders: read("holders.json"),
      };
    }

    /** The issue as its Dispatch answers for it, in the facts a Legion issue under review holds:
     * the repository it resolves to, its status and labels, its approved plan, the coverage of its
     * requirements, and the k1 line of `dispatch results`, which names the pull request it
     * compares with and whether a pass stands at that head. */
    function issueState(world: SeededWorld) {
      const line = (text: string, prefix: string) =>
        text.split("\n").find((candidate) => candidate.startsWith(prefix)) ?? `no ${prefix} line`;
      const k1 = line(world.results, "- k1 (before_merge). ");
      return {
        repository: line(world.issueRead, "Repository: "),
        status: line(world.issueRead, "Status: "),
        labels: line(world.issueRead, "Labels: "),
        planDocument: world.issueRead.split("\n").some((text) => /^- .*plan\.md/.test(text)),
        planApproval: /^Approval: (approved v\d+) by /m.exec(world.planRead)?.[1],
        coverageGaps: world.issueRead.split("\n").filter((text) => text.startsWith("Coverage: ")),
        comparedWith: /Compared with (the head of pull request #\d+ at [0-9a-f]{40})/.exec(k1)?.[1],
        passStands: / A pass stands: ([^.]*)\.$/.exec(k1)?.[1],
      };
    }

    /** What `bun test` in a copy of the world's repository answers with greet.ts's guard
     * replaced by GUARD (left as the world wrote it when GUARD is undefined): whether the suite
     * passed. A guard whose loss the suite does not notice is a requirement no test pins. */
    function suitePasses(world: SeededWorld, guard?: string): boolean {
      const copy = freshDir("world-suite");
      cpSync(path.join(world.run, "src"), copy, { recursive: true });
      rmSync(path.join(copy, ".git"), { recursive: true, force: true });
      if (guard !== undefined) {
        const file = path.join(copy, "greet.ts");
        const code = readFileSync(file, "utf8");
        const usage = /if \(name === undefined[^)]*\) \{/;
        expect(code).toMatch(usage);
        const mutated = code.replace(usage, `if (${guard}) {`);
        writeFileSync(file, mutated);
      }
      return spawnChecked(["bun", "test"], { cwd: copy, env: childEnv }).exitCode === 0;
    }

    /** `bun greet.ts ARGS…` in the world's repository: its exit code and what it printed. */
    function greet(world: SeededWorld, ...args: string[]) {
      const made = spawnChecked(["bun", "greet.ts", ...args], {
        cwd: path.join(world.run, "src"),
        env: childEnv,
      });
      return { code: made.exitCode, out: made.stdout.toString(), err: made.stderr.toString() };
    }

    /** Each concern of the sweep, true where the world gives a reviewer that concern to raise,
     * and the world's intended findings beside them. A key names its concern, so a fixture that
     * drifts fails naming the row. */
    function concerns(world: SeededWorld) {
      const src = (file: string) =>
        existsSync(path.join(world.run, "src", file))
          ? readFileSync(path.join(world.run, "src", file), "utf8")
          : "";
      const workflow = src(".github/workflows/greet.yml");
      const docs = src("docs/cli.md");
      const greetTs = src("greet.ts");
      const uses = [...workflow.matchAll(/^\s*- uses: (\S+)/gm)].map((match) => match[1] ?? "");
      const runs = [...workflow.matchAll(/^\s*- run: (.*)$/gm)].map((match) => match[1] ?? "");
      const named = [...docs.matchAll(/`bun ([\w.-]+\.ts)\b/g)].map((match) => match[1] ?? "");
      const check = /:::check\{#k1 [^\n]*\}\n[^\n]*/.exec(world.verification)?.[0] ?? "";
      // The required set as the merge gate reads it: the base's rulesets' required checks and
      // its classic protection's, which a base a ruleset alone protects names none of.
      const required = [
        [
          "repos/fernbrook/widgets/rules/branches/main",
          '.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context',
        ],
        [
          "repos/fernbrook/widgets/branches/main",
          ".protection.required_status_checks.checks[].context",
        ],
      ].map(([endpoint = "", jq = ""]) =>
        spawnChecked(["bun", GH_STANDIN, "gh", "api", endpoint, "--jq", jq], {
          env: { ...childEnv, GH_DATA_DIR: world.run },
        })
      );
      const blank = greet(world, "   ");
      const trimmed = greet(world, "  Ada  ");
      const onKeys = /^on:\n((?: {2}\S[^\n]*\n)+)/m
        .exec(workflow)?.[1]
        ?.split("\n")
        .filter(Boolean)
        .map((key) => key.trim());
      return {
        "1 an action pinned by tag, or no permissions block":
          workflow !== "" &&
          (uses.some((use) => !/@[0-9a-f]{40}$/.test(use)) ||
            !/^permissions:\n {2}contents: read$/m.test(workflow)),
        "2 R2 has no test (the suite passes with the usage guard gone)": suitePasses(
          world,
          "false"
        ),
        "2 R2 has no test of the no-name case (the suite passes when no name greets)": suitePasses(
          world,
          'name === ""'
        ),
        "3 the name is trimmed in two places": (greetTs.match(/\.trim\(/g) ?? []).length > 1,
        "4 the workflow runs one named test file": runs.some((run) => /^bun test \S/.test(run)),
        "5 docs/cli.md names a command the repository has not got": named.some(
          (file) => src(file) === ""
        ),
        "6 check k1 covers the no-name case only, not R2's blank name":
          check !== "" && !/blank|spaces/.test(check),
        "7 Dispatch says no pass stands for k1":
          issueState(world).passStands?.startsWith("yes") !== true,
        "intended check: k1 compares the output of `bun greet.ts Ada` with docs/cli.md's example line":
          check.includes("compared with the example line that `docs/cli.md` gives"),
        "8 the base branch requires no status check": required.every(
          (read) => read.exitCode !== 0 || read.stdout.toString().trim() === ""
        ),
        "the suite passes at the head, as the head's checks report": suitePasses(world),
        "intended defect: the CLI greets a blank name": blank.code === 0,
        "intended defect: no test holds the blank name (the suite passes without its guard)":
          suitePasses(world, "name === undefined"),
        "intended text finding: docs/cli.md's sentence on spaces contradicts R3, which the CLI meets":
          docs.includes("It prints the name exactly as it was given, spaces and all.") &&
          WORLD_SPEC.includes("**R3.** Surrounding whitespace in the name is trimmed") &&
          trimmed.out === "Hello, Ada!\n",
        "intended text finding: the workflow's comment names a push trigger the spec's Context rules out, which `on:` meets":
          /^# Runs the greet tests on every pull request and on every push to main\.$/m.test(
            workflow
          ) &&
          WORLD_SPEC.includes("no workflow of the repository runs on a push to `main`") &&
          JSON.stringify(onKeys) === JSON.stringify(["pull_request:"]),
        "intended code defect: docs/cli.md says a blank name exits 2, false only because the CLI greets it":
          docs.includes(
            "A blank name, like no name at all, prints the usage line on stderr and exits 2."
          ) && blank.code === 0,
        "intended text finding: the reviewer's commit says the CLI strips every space from the name, which its trim does not":
          docs.includes("It strips every space from the name.") &&
          greet(world, "Ada Lovelace").out === "Hello, Ada Lovelace!\n",
      };
    }

    /** What every world gives: its issue past triage on the world's repository and plan, and k1's
     * pass standing at the head of the world's own pull request. */
    function expectedState(world: SeededWorld, status: string, passStands: string) {
      return {
        repository: `Repository: ${WORLD_REPO} (inherited or project default)`,
        status: `Status: ${status}`,
        labels: "Labels: legion",
        planDocument: true,
        planApproval: "approved v1",
        coverageGaps: [],
        comparedWith: `the head of pull request #${world.pr} at ${world.head}`,
        passStands,
      };
    }
    /** The concerns of a world that intends none of them, with its intended findings as given:
     * the defect, the document's false sentence on spaces, the workflow's false comment, the
     * sentence the defect makes false, the sentence the reviewer's own commit makes false, the
     * passes standing at the head below the reviewer's commit and none at it (Verification v47's
     * k2: accept, correct and crossing), and the check that compares the command's output with
     * docs/cli.md's example line (k3's crossing). */
    function noConcern(intended: {
      defect: boolean;
      doc: boolean;
      workflow: boolean;
      codeDefectSentence: boolean;
      reviewerSentence: boolean;
      passBelow: boolean;
      comparesExample: boolean;
    }) {
      return {
        "1 an action pinned by tag, or no permissions block": false,
        "2 R2 has no test (the suite passes with the usage guard gone)": false,
        "2 R2 has no test of the no-name case (the suite passes when no name greets)": false,
        "3 the name is trimmed in two places": false,
        "4 the workflow runs one named test file": false,
        "5 docs/cli.md names a command the repository has not got": false,
        "6 check k1 covers the no-name case only, not R2's blank name": intended.defect,
        "7 Dispatch says no pass stands for k1": intended.passBelow,
        "intended check: k1 compares the output of `bun greet.ts Ada` with docs/cli.md's example line":
          intended.comparesExample,
        "8 the base branch requires no status check": false,
        "the suite passes at the head, as the head's checks report": true,
        "intended defect: the CLI greets a blank name": intended.defect,
        "intended defect: no test holds the blank name (the suite passes without its guard)":
          intended.defect,
        "intended text finding: docs/cli.md's sentence on spaces contradicts R3, which the CLI meets":
          intended.doc,
        "intended text finding: the workflow's comment names a push trigger the spec's Context rules out, which `on:` meets":
          intended.workflow,
        "intended code defect: docs/cli.md says a blank name exits 2, false only because the CLI greets it":
          intended.codeDefectSentence,
        "intended text finding: the reviewer's commit says the CLI strips every space from the name, which its trim does not":
          intended.reviewerSentence,
      };
    }
    /** The intended findings of a world that holds none. */
    const none = {
      defect: false,
      doc: false,
      workflow: false,
      codeDefectSentence: false,
      reviewerSentence: false,
      passBelow: false,
      comparesExample: false,
    };
    /** The implementer's commit, the head below the reviewer's where the world has one (rig.sh's
     * fixture_implement, `$R/implement.json`), and docs/cli.md at `commit`. */
    const implementedOf = (world: SeededWorld) =>
      z
        .object({ head: z.string() })
        .parse(JSON.parse(readFileSync(path.join(world.run, "implement.json"), "utf8"))).head;
    const docsAt = (world: SeededWorld, commit: string) =>
      spawnChecked([
        "git",
        "-C",
        path.join(world.run, "remote.git"),
        "show",
        `${commit}:docs/cli.md`,
      ]).stdout.toString();

    test("the tester's world: its issue is in testing with the implementer's pass standing, and no concern is true", () => {
      const world = seededWorld("");
      expect(issueState(world)).toEqual(expectedState(world, "testing", "yes, from implementer"));
      expect(concerns(world)).toEqual(noConcern(none));
    }, 300_000);

    test("text-only: the review round's issue is in needs_review with both passes standing, and only its two text findings are true", () => {
      const world = seededWorld("text-only");
      expect(issueState(world)).toEqual(
        expectedState(world, "needs_review", "yes, from tester, implementer")
      );
      expect(concerns(world)).toEqual(noConcern({ ...none, doc: true, workflow: true }));
    }, 300_000);

    test("defect: the check that missed the blank name and the defect are true, beside the two text findings, and nothing else", () => {
      const world = seededWorld("defect");
      expect(issueState(world)).toEqual(
        expectedState(world, "needs_review", "yes, from tester, implementer")
      );
      expect(concerns(world)).toEqual(
        noConcern({ ...none, defect: true, doc: true, workflow: true })
      );
    }, 300_000);

    test("last-push: text-only's world on a base that asks the last push approved by another, both passes standing at the head and only its two text findings true", () => {
      const world = seededWorld("last-push");
      expect(issueState(world)).toEqual(
        expectedState(world, "needs_review", "yes, from tester, implementer")
      );
      expect(concerns(world)).toEqual(noConcern({ ...none, doc: true, workflow: true }));
      // The case reaches the base's rules through worker_fixture (review_world's CASE).
      const asked = spawnChecked(
        [
          "bun",
          GH_STANDIN,
          "gh",
          "api",
          "repos/fernbrook/widgets/rules/branches/main",
          "--jq",
          '.[] | select(.type=="pull_request") | .parameters.require_last_push_approval',
        ],
        { env: { ...childEnv, GH_DATA_DIR: world.run } }
      );
      expect([asked.exitCode, asked.stdout.toString()]).toEqual([0, "true\n"]);
    }, 300_000);

    /** The states of the reviews the world's pull request holds, as gh answers the reviews list. */
    const reviewStates = (world: SeededWorld) => {
      const listed = spawnChecked(
        [
          "bun",
          GH_STANDIN,
          "gh",
          "api",
          `repos/fernbrook/widgets/pulls/${world.pr}/reviews`,
          "--jq",
          "[.[] | .state] | @json",
        ],
        { env: { ...childEnv, GH_DATA_DIR: world.run } }
      );
      expect(listed.exitCode).toBe(0);
      return JSON.parse(listed.stdout.toString()) as string[];
    };

    // Verification v47's k2: in accept, correct and crossing the reviewer has not approved yet,
    // the issue is in `reviewing`, the tester's and the implementer's passes stand at the head
    // below the reviewer's commit, and no result stands at that commit, so Dispatch says no pass
    // stands at the pull request's head. correct's workflow thread is an inline comment of a
    // `COMMENT` review at the reviewer's commit.
    test("correct: the reviewer's commit is the head, its sentence false another way, the workflow's comment the one other finding, and no approval or pass stands at it", () => {
      const world = seededWorld("correct");
      expect(issueState(world)).toEqual(expectedState(world, "needs_review", "no"));
      expect(concerns(world)).toEqual(
        noConcern({ ...none, workflow: true, reviewerSentence: true, passBelow: true })
      );
      expect(reviewStates(world)).toEqual(["COMMENTED"]);
    }, 300_000);

    test("accept: the reviewer's commit is the head and makes the document true, no finding is left, and no approval or pass stands at it", () => {
      const world = seededWorld("accept");
      expect(issueState(world)).toEqual(expectedState(world, "needs_review", "no"));
      expect(concerns(world)).toEqual(noConcern({ ...none, passBelow: true }));
      expect(reviewStates(world)).toEqual([]);
      // The reviewer's commit rewords the example line, and the check reads no line of the
      // document, so the tester's earlier pass holds at it.
      expect(docsAt(world, world.head)).toContain(
        "Run `bun greet.ts Ada`, and it prints `Hello, Ada!`"
      );
    }, 300_000);

    // Verification v47's k3 crossing: accept's world but for the check, which compares the
    // command's output with docs/cli.md's example line. The reviewer's commit rewords that line
    // and keeps its greeting, so the line stays true, the implementer's own run there passes, and
    // the tester's earlier pass, of the line as it stood below that commit, does not hold there.
    test("crossing: accept's world with a check that compares the output with docs/cli.md's example line, which the reviewer's commit rewords and keeps true", () => {
      const world = seededWorld("crossing");
      expect(issueState(world)).toEqual(expectedState(world, "needs_review", "no"));
      expect(concerns(world)).toEqual(
        noConcern({ ...none, passBelow: true, comparesExample: true })
      );
      expect(reviewStates(world)).toEqual([]);
      expect(docsAt(world, implementedOf(world))).toContain(
        "`bun greet.ts Ada` prints `Hello, Ada!`"
      );
      const atHead = docsAt(world, world.head);
      expect(atHead).toContain("Run `bun greet.ts Ada`, and it prints `Hello, Ada!`");
      expect(atHead).not.toContain("\n`bun greet.ts Ada` prints");
      expect(greet(world, "Ada").out).toBe("Hello, Ada!\n");
    }, 300_000);

    test("code-defect: the check that missed the blank name, the defect and the sentence it makes false are true, and nothing else", () => {
      const world = seededWorld("code-defect");
      expect(issueState(world)).toEqual(
        expectedState(world, "needs_review", "yes, from tester, implementer")
      );
      expect(concerns(world)).toEqual(
        noConcern({ ...none, defect: true, codeDefectSentence: true })
      );
    }, 300_000);

    // After the implementer's push, the first `dispatch results` an agent can run reads what
    // production gives it: the pushed commit is the head k1 compares with, and no pass stands
    // there, since each pass's commit is another commit (results_comparison.go, commitStands,
    // through nothingReason, which takes the repository as readable once GitHub answered its
    // default branch: delivery/branch.go, branchOf). Nothing of the rig's seeds the head here: the
    // run's head watcher stores it on the push (rig.sh's start_heads, head-watcher.ts), and the row
    // waits only for its record to answer the push, as an agent's next call does. Catches: a
    // missing watcher, with which the read compares with the head the world was built at, the
    // reviewer's commit, so `comparedWith` names that old head and not the pushed one. No pass
    // stands at either head, since the passes are seeded at the implementer's commit below both.
    test("correct after the implementer's push: k1 compares with the pushed head the watcher stored, and no pass stands there, as production answers", () => {
      const world = seededWorld("correct", true);
      expect(issueState(world)).toEqual(expectedState(world, "needs_review", "no"));
      expect(readFileSync(path.join(world.run, "heads.txt"), "utf8")).toContain(
        `"pushed":"${world.head}","ok":true`
      );
    }, 300_000);

    /** The record of the world's issue the stand-in booted on it served (seededWorld's `boot`). */
    function servedRecord(world: SeededWorld) {
      const record = z
        .looseObject({
          issues: z.record(
            z.string(),
            z.looseObject({
              status: z.string(),
              workers: z.record(z.string(), z.record(z.string(), z.unknown())),
              pullRequest: z.record(z.string(), z.unknown()),
              designGate: z.looseObject({
                currentVersion: z.number(),
                approvedVersion: z.number().nullable(),
              }),
            })
          ),
        })
        .parse(JSON.parse(world.state)).issues[world.issue];
      if (record === undefined) throw new Error(`the state holds no record of ${world.issue}`);
      return record;
    }
    /** The role holders the world's Dispatch held after the boot (`GET /api/v1/issues/{key}/
     * role-holders`, in the workflow's order, as Dispatch answers them), and the ones the record
     * the stand-in served names in that order: the session of each live claim, a registered,
     * ready, working or idle one that records a session (record's `LiveRoleHolders`). */
    function roleHolders(world: SeededWorld) {
      const Holder = z.looseObject({ role: z.string(), session: z.string() });
      const held = z
        .looseObject({ issue: z.literal(world.issue), holders: z.array(Holder) })
        .parse(JSON.parse(world.holders))
        .holders.map(({ role, session }) => ({ role, session }));
      const Claim = z.looseObject({ session: z.string(), state: z.string() });
      const issue = z
        .looseObject({
          issues: z.record(
            z.string(),
            z.looseObject({
              architect: Claim,
              workers: z.record(z.string(), z.looseObject({ claim: Claim })),
            })
          ),
        })
        .parse(JSON.parse(world.state)).issues[world.issue];
      const claims = new Map([
        ["architect", issue?.architect],
        ...Object.entries(issue?.workers ?? {}).map(([role, row]) => [role, row.claim] as const),
      ]);
      const named = ["architect", "planner", "implementer", "tester", "reviewer", "merger"].flatMap(
        (role) => {
          const claim = claims.get(role);
          return claim !== undefined &&
            claim.session !== "" &&
            ["registered", "ready", "working", "idle"].includes(claim.state)
            ? [{ role, session: claim.session }]
            : [];
        }
      );
      return { held, named };
    }
    /** The handoff records the world's seeds wrote, as the run keeps them (fixture.json): each
     * the id and version the run's Dispatch holds it under. */
    function seededRecords(world: SeededWorld) {
      const record = z.object({ id: z.string(), version: z.number() });
      return z
        .looseObject({ plan: record, implement: record, test: record, review: record.optional() })
        .parse(JSON.parse(readFileSync(path.join(world.run, "fixture.json"), "utf8")));
    }
    /** The world's pull request as its gh answers `gh pr view`, in the record's own members and
     * the daemon's lower-case spelling: the decision absent where gh names none. */
    function ghSays(world: SeededWorld) {
      const fields = "number,headRefOid,state,mergeStateStatus,reviewDecision";
      const view = spawnChecked(
        ["bun", GH_STANDIN, "gh", "pr", "view", world.pr, "--repo", WORLD_REPO, "--json", fields],
        { env: { ...childEnv, GH_DATA_DIR: world.run } }
      );
      expect({ code: view.exitCode, err: view.stderr.toString() }).toEqual({ code: 0, err: "" });
      const said = z
        .object({
          number: z.number(),
          headRefOid: z.string(),
          state: z.string(),
          mergeStateStatus: z.string(),
          reviewDecision: z.string(),
        })
        .parse(JSON.parse(view.stdout.toString()));
      return {
        repo: WORLD_REPO,
        number: said.number,
        head: said.headRefOid,
        state: said.state.toLowerCase(),
        mergeState: said.mergeStateStatus.toLowerCase(),
        ...(["", "REVIEW_REQUIRED"].includes(said.reviewDecision)
          ? {}
          : { reviewDecision: said.reviewDecision.toLowerCase() }),
      };
    }

    // The record an agent's first `read_record` answers on a world the rig built, held to what
    // that world's own seeds, Dispatch and gh say of the issue. Catches: a record that says less
    // of the issue than its world does — no row of the implementer's standing round (INFRA-764
    // round 25: `workers: {}` in every counted threads-closed run, read before any write), a
    // handoff record under another id or version than the run's Dispatch holds, a pull request
    // other than the one gh answers, a design gate the spec's approval does not give, or a
    // status the run's Dispatch does not show; a stand-in that writes a status to the run's
    // Dispatch at a read that moved nothing; and a world whose Dispatch holds no session of a role
    // the record names live (the daemon's role-holder publication: daemon-standin.ts's
    // `queueHolders`), which the real Dispatch of the world refuses or the stand-in never sent; and
    // a row of the implementer or the tester naming the reviewer's commit, where each names the
    // head its own record names, the implementer's commit below it.
    test("threads-closed's correct world: the record its implementer reads holds each seeded round at its own head, gh's pull request with no decision and the approved spec", () => {
      const world = seededWorld("correct", false, {
        role: "implementer",
        phase: "reviewing",
        completed: "implementer",
      });
      const record = servedRecord(world);
      const seeds = seededRecords(world);
      const implemented = implementedOf(world);
      expect(implemented).not.toBe(world.head);
      expect(Object.keys(record.workers)).toEqual(["implementer", "planner", "reviewer", "tester"]);
      expect(record).toMatchObject({
        key: world.issue,
        phase: "reviewing",
        status: "needs_review",
        repo: WORLD_REPO,
        defaultBranch: "main",
        architect: { state: "idle" },
        workers: {
          planner: { claim: { state: "idle" }, handoffRecord: seeds.plan },
          implementer: {
            claim: { session: BOOT_SESSION, state: "working" },
            handoffCommit: implemented,
            handoffRecord: seeds.implement,
            pass: 0,
            rounds: 0,
          },
          tester: {
            claim: { state: "idle" },
            handoffCommit: implemented,
            handoffRecord: seeds.test,
          },
          reviewer: { claim: { state: "idle" }, handoffRecord: seeds.review },
        },
        gatedSteps: [{ step: "spec", status: "approved" }],
        slot: { index: 0 },
      });
      // No approval stands, so the round waits on no merge state of GitHub's.
      expect("mergeWait" in record).toBe(false);
      expect(record.pullRequest).toEqual(ghSays(world));
      expect("reviewDecision" in record.pullRequest).toBe(false);
      expect(record.designGate.approvedVersion).toBe(record.designGate.currentVersion);
      expect(issueState({ ...world, issueRead: world.readAfter }).status).toBe(
        `Status: ${record.status}`
      );
      const holders = roleHolders(world);
      expect(holders.held).toEqual(holders.named);
      expect(holders.named.map(({ role }) => role)).toEqual([
        "architect",
        "planner",
        "implementer",
        "tester",
        "reviewer",
      ]);
    }, 300_000);

    test("reviewer-text-findings' text-only world: the record its reviewer reads holds the rounds before its own, its own task and no decision", () => {
      const world = seededWorld("text-only", false, { role: "reviewer", phase: "", completed: "" });
      const record = servedRecord(world);
      const seeds = seededRecords(world);
      expect(Object.keys(record.workers)).toEqual(["implementer", "planner", "reviewer", "tester"]);
      expect(record).toMatchObject({
        key: world.issue,
        phase: "reviewing",
        status: "needs_review",
        workers: {
          planner: { handoffRecord: seeds.plan },
          implementer: { handoffCommit: world.head, handoffRecord: seeds.implement },
          tester: { handoffCommit: world.head, handoffRecord: seeds.test },
          reviewer: {
            claim: { session: BOOT_SESSION, state: "working", pendingTask: { phase: "reviewing" } },
            pass: 0,
            rounds: 0,
          },
        },
        gatedSteps: [{ step: "spec", status: "approved" }],
        slot: { index: 0 },
        phaseEntry: 1,
        givenEntry: 1,
      });
      expect(
        ["handoffRecord", "handoffCommit"].filter((key) => key in (record.workers.reviewer ?? {}))
      ).toEqual([]);
      expect("mergeWait" in record).toBe(false);
      expect(record.pullRequest).toEqual(ghSays(world));
      expect("reviewDecision" in record.pullRequest).toBe(false);
      expect(issueState({ ...world, issueRead: world.readAfter }).status).toBe(
        `Status: ${record.status}`
      );
      const holders = roleHolders(world);
      expect(holders.held).toEqual(holders.named);
      expect(holders.named.map(({ role }) => role)).toEqual([
        "architect",
        "planner",
        "implementer",
        "tester",
        "reviewer",
      ]);
    }, 300_000);
  }
);

// The run's two readers that open its own database with Bun.SQL, seed.ts pull-request and the
// head watcher (seed-handoffs.ts's `pullRequestHead`), given its URL as rig.sh gives it on a given
// server: own_dispatch's dispatch_database_url, the URL conninfo_url rebuilds from the keywords
// libpq read of the given URL (run_database_url), handed to seed.ts by seed_ts and to the watcher
// by start_heads. Each row runs rig.sh's own own_dispatch, seed_ts, start_heads and heads_stored,
// with no Dispatch server (start_dispatch's port is 1) and no seed.ts project, which no reader
// needs, then a push the watcher answers. Catches: a reader given run_database_url's keyword/value
// string, which Bun.SQL refuses (`Invalid URL`), so no pull request's head is ever stored on a
// given server; and a reader given the given URL's own text, of which Bun reads another host than
// libpq read: of `postgres://evil.example?@127.0.0.1:5433/db`, Bun 1.4.4 read evil.example and
// libpq 127.0.0.1, the user `evil.example?` (measured 2026-11-25).
describe("the two Bun.SQL readers of a run's database, on a given server", () => {
  const READER_DATABASE_PREFIX = "bunsqlrow_";
  const readerDatabases = new Set<string>();
  afterAll(async () => {
    if (testPostgres === undefined || readerDatabases.size === 0) return;
    const sql = new Bun.SQL({ url: testPostgres, max: 1 });
    try {
      for (const name of readerDatabases)
        await sql.unsafe(`DROP DATABASE IF EXISTS ${name} WITH (FORCE)`);
    } finally {
      await sql.close();
    }
  });

  /** rig.sh's own_dispatch on the given Postgres GIVEN (the batch's $services_postgres), its
   * database named with READER_DATABASE_PREFIX, then the run's pull request seeded (seed_ts
   * pull-request, whose failure is kept, not fatal) and its head watcher started (start_heads),
   * and one push to the pull request's branch, which the row waits for the watcher to answer
   * (heads_stored). `stubs` replace rig.sh functions, `setup` runs after own_dispatch and
   * `afterSeed` after the seed; the database is dropped as the script exits. Returns the run
   * directory, the pushed commit and bash's result. */
  function readersOn(
    given: string,
    options: {
      stubs?: string[];
      setup?: string[];
      afterSeed?: string[];
      env?: Record<string, string>;
    }
  ) {
    const run = path.join(
      freshDir("bun-sql-readers"),
      `${Date.now().toString(16)}${dirs.toString(16).padStart(2, "0")}`
    );
    mkdirSync(path.join(run, "logs"), { recursive: true });
    readerDatabases.add(`${READER_DATABASE_PREFIX}${path.basename(run)}`);
    writeFileSync(path.join(run, "fixture.json"), `${JSON.stringify({ issue: "WID-1" })}\n`);
    const view = {
      number: 1001,
      url: `https://github.com/${WORLD_REPO}/pull/1001`,
      title: "feat: greet() (WID-1)",
      state: "OPEN",
      headRefName: "legion/WID-1",
      baseRefName: "main",
    };
    writeFileSync(
      path.join(run, "fixtures.json"),
      `${JSON.stringify({ routes: [{ match: "^gh pr view", stdout: view }] })}\n`
    );
    const own = fn("own_dispatch");
    expect(own).toContain("database=run_");
    const script = [
      "set -euo pipefail",
      "R=$1 work=$2 root=$3 here=$4",
      `. ${JSON.stringify(path.join(root, "scripts/e2e/lib/rig.sh"))}`,
      `. ${JSON.stringify(path.join(root, "packages/envoy/scripts/lib/pg-url.sh"))}`,
      ...["fail", "note", "safe_url", "pg_sql", "conninfo_url", "seed_ts", "start_heads"].map(fn),
      fn("heads_stored"),
      own.replace("database=run_", `database=${READER_DATABASE_PREFIX}`),
      "start_dispatch() { dispatch_port=1; }",
      'bun() { if [ "$#" = 2 ] && [ "$1" = "$here/seed.ts" ] && [ "$2" = project ]; then return 0; fi; command bun "$@"; }',
      ...(options.stubs ?? []),
      'evidence=$R services_postgres=$R/postgres-url watcher_pid=""',
      `database=${READER_DATABASE_PREFIX}$(basename "$R")`,
      'printf "%s\\n" "$GIVEN" >"$services_postgres"',
      'trap \'stop_pid "$watcher_pid"; pg_sql "$GIVEN" "DROP DATABASE IF EXISTS $database WITH (FORCE)" >/dev/null 2>&1 || true\' EXIT',
      "own_dispatch project",
      ...(options.setup ?? []),
      // The run's remote, its pull request's branch at a first commit.
      'git init -q --bare "$R/remote.git"',
      'g() { GIT_AUTHOR_NAME=n GIT_AUTHOR_EMAIL=n@n GIT_COMMITTER_NAME=n GIT_COMMITTER_EMAIL=n@n git -C "$R/remote.git" "$@"; }',
      "tree=$(g hash-object -t tree -w --stdin </dev/null)",
      'first=$(g commit-tree "$tree" -m first </dev/null)',
      'g update-ref refs/heads/legion/WID-1 "$first"',
      '( seed_ts pull-request "$R/fixture.json" "$R/fixtures.json" "$R/remote.git" ) || :',
      ...(options.afterSeed ?? []),
      ': >"$R/pushes.log"',
      "start_heads",
      // A push of a second commit, as the remote's post-receive hook logs it.
      'second=$(g commit-tree "$tree" -p "$first" -m second </dev/null)',
      'g update-ref refs/heads/legion/WID-1 "$second"',
      'printf \'%s refs/heads/legion/WID-1 %s %s\\n\' "$(date -u +%FT%T.%3NZ)" "$first" "$second" >>"$R/pushes.log"',
      'await_start watcher "$watcher_pid" 30 "the head watcher to answer the push" heads_stored',
      'printf "%s\\n" "$second" >"$R/pushed.txt"',
      // The database's head row, read through the same URL, by psql, which is libpq.
      'pg_sql "$dispatch_database_url" "SELECT head_sha FROM pull_request_heads" >"$R/stored.txt" 2>&1 || :',
    ].join("\n");
    const result = bash(script, [run, path.dirname(run), root, import.meta.dir], {
      GIVEN: given,
      ...options.env,
    });
    const read = (name: string) =>
      existsSync(path.join(run, name)) ? readFileSync(path.join(run, name), "utf8") : "";
    return { run, result, read, pushed: read("pushed.txt").trim() };
  }

  // The hostile URL above, at a port of the row's own where a stand-in Postgres listens on the
  // loopback: it records the startup message of every connection made to it, the user and the
  // database each names, and refuses each login, so each reader fails there, having reached it.
  test("a URL whose text Bun reads another host from reaches the loopback host libpq read, from both readers", () => {
    const dir = freshDir("bun-sql-hostile");
    const program = path.join(dir, "postgres.ts");
    const portFile = path.join(dir, "port");
    const record = path.join(dir, "startups.jsonl");
    writeFileSync(
      program,
      [
        'import { appendFileSync, writeFileSync } from "node:fs";',
        "const [portFile, record] = Bun.argv.slice(2);",
        "const held = new Map<unknown, Buffer>();",
        "const listener = Bun.listen({",
        '  hostname: "127.0.0.1",',
        "  port: 0,",
        "  socket: {",
        "    data(socket, chunk) {",
        "      let buffer = Buffer.concat([held.get(socket) ?? Buffer.alloc(0), Buffer.from(chunk)]);",
        "      while (buffer.length >= 8 && buffer.length >= buffer.readInt32BE(0)) {",
        "        const length = buffer.readInt32BE(0);",
        "        const code = buffer.readInt32BE(4);",
        '        const pairs = buffer.subarray(8, length).toString().split("\\0");',
        "        buffer = buffer.subarray(length);",
        '        if (code === 80877103 || code === 80877104) { socket.write("N"); continue; }',
        "        const startup: Record<string, string> = {};",
        '        for (let i = 0; i + 1 < pairs.length; i += 2) if (pairs[i]) startup[pairs[i] ?? ""] = pairs[i + 1] ?? "";',
        '        appendFileSync(record ?? "", JSON.stringify({ user: startup.user, database: startup.database }) + "\\n");',
        '        const fields = Buffer.from("SFATAL\\0VFATAL\\0C28000\\0Mthe stand-in refuses every login\\0\\0");',
        "        const head = Buffer.alloc(5);",
        '        head.write("E");',
        "        head.writeInt32BE(4 + fields.length, 1);",
        "        socket.write(Buffer.concat([head, fields]));",
        "        socket.end();",
        "      }",
        "      held.set(socket, buffer);",
        "    },",
        "  },",
        "});",
        'writeFileSync(portFile ?? "", String(listener.port));',
        "",
      ].join("\n")
    );
    writeFileSync(record, "");
    const server = Bun.spawn(["bun", program, portFile, record], {
      stdout: "ignore",
      stderr: "ignore",
    });
    try {
      for (let waited = 0; !existsSync(portFile) && waited < 600; waited++) Bun.sleepSync(50);
      const port = readFileSync(portFile, "utf8");
      const hostile = `postgres://evil.example?@127.0.0.1:${port}/db`;
      const { run, result, read, pushed } = readersOn(hostile, {
        // The stand-in takes no CREATE DATABASE: psql is no reader of Bun.SQL.
        stubs: ["pg_sql() { :; }"],
        afterSeed: ['wc -l <"$RECORD" >"$R/after-seed.txt"'],
        env: { RECORD: record },
      });
      expect({ code: result.code, err: result.stderr }).toMatchObject({ code: 0 });
      const database = `${READER_DATABASE_PREFIX}${path.basename(run)}`;
      const startups = readFileSync(record, "utf8")
        .split("\n")
        .filter(Boolean)
        .map((line) => JSON.parse(line));
      // The seed's connection, then the watcher's: each named the user and the database libpq
      // read, at the loopback host libpq read, the only listener they reached.
      expect(Number(read("after-seed.txt").trim())).toBeGreaterThan(0);
      expect(startups.length).toBeGreaterThan(Number(read("after-seed.txt").trim()));
      expect(new Set(startups.map((startup) => JSON.stringify(startup)))).toEqual(
        new Set([JSON.stringify({ user: "evil.example?", database })])
      );
      expect(read("fixture.log")).toContain("the stand-in refuses every login");
      const answer = read("logs/heads.log")
        .split("\n")
        .filter(Boolean)
        .map((line) => JSON.parse(line))
        .find((line) => line.pushed === pushed);
      expect(answer).toMatchObject({
        ok: false,
        error: expect.stringContaining("the stand-in refuses every login"),
      });
    } finally {
      server.kill();
    }
  }, 120_000);

  // The pod's own Postgres (LEGION_TEST_PG_DSN), as the file's other Postgres rows take it: the
  // run's database holds the two tables the head's write reads, as Dispatch's migration makes them
  // (store/migrations/20261102122300_pull_request_heads.up.sql, as far as the write goes) and the
  // issue it is attributed to; the seed stores the first head and the watcher the pushed one.
  test.skipIf(testPostgres === undefined)(
    "a seed and the head watcher store the pull request's head in the run's own database through its URL",
    () => {
      const { result, read, pushed } = readersOn(testPostgres ?? "", {
        setup: [
          `pg_sql "$dispatch_database_url" "CREATE TABLE issues (key text PRIMARY KEY); CREATE TABLE pull_request_heads (repo text NOT NULL, number integer NOT NULL, head_sha text NOT NULL, head_ref text NOT NULL DEFAULT '', base_ref text NOT NULL DEFAULT '', state text NOT NULL, issue_key text REFERENCES issues (key), attribution_source text NOT NULL DEFAULT 'none', title text NOT NULL DEFAULT '', url text NOT NULL DEFAULT '', checked_at timestamptz, PRIMARY KEY (repo, number)); INSERT INTO issues VALUES ('WID-1')" >/dev/null`,
        ],
      });
      expect({ code: result.code, err: result.stderr }).toMatchObject({ code: 0 });
      expect(read("fixture.log")).toMatch(/seeded WID-1's pull request #1001 at [0-9a-f]{40}/);
      expect(read("logs/heads.log")).toContain(`"pushed":"${pushed}","ok":true`);
      expect(read("stored.txt")).toBe(`${pushed}\n`);
    },
    120_000
  );
});

// Health's "head" rule (rule 19) over a run's push log, its head watcher's record and its own
// remote. Catches: a push whose head the run's Dispatch was never given read as healthy, whether
// the watcher wrote no line for it or wrote that the write failed; a worker world whose record is
// missing, empty or not opened read as healthy because nothing was pushed; a line the watcher does
// not write passed over; a branch of the run's own remote that moved with no line storing it read
// as healthy because pushes.log missed the push; and a world rig.sh starts no watcher for held to
// a record.
describe("health's head rule", () => {
  const push = (commit: string) =>
    `2026-11-16T10:00:00.000Z refs/heads/legion/WID-1 ${"a".repeat(40)} ${commit}\n`;
  const opened = '{"at":"2026-11-16T09:59:00.000Z","opened":"heads.log"}\n';
  /** A run of `scenario` holding PUSHES as its push log unless undefined and, unless undefined,
   * HEADS as its record. */
  function pushed(
    pushes: string | undefined,
    heads?: string,
    scenario = "reviewer-text-findings"
  ): { dir: string; scenario: string } {
    const dir = freshDir("head-rule");
    mkdirSync(path.join(dir, "logs"));
    if (pushes !== undefined) writeFileSync(path.join(dir, "pushes.log"), pushes);
    if (heads !== undefined) writeFileSync(path.join(dir, "logs", "heads.log"), heads);
    return { dir, scenario };
  }
  const b = "b".repeat(40);
  const line = (ok: boolean, error?: string, commit = b) =>
    `${JSON.stringify({ at: "2026-11-16T10:00:01.000Z", ref: "refs/heads/legion/WID-1", pushed: commit, ok, ...(error === undefined ? {} : { error }) })}\n`;

  test("a push the watcher stored is healthy, and a world with no watcher that pushed nothing needs no record", () => {
    expect(headUnmoved(pushed(push(b), opened + line(true)))).toBeUndefined();
    expect(headUnmoved(pushed(undefined, opened))).toBeUndefined();
    for (const scenario of ["ask-on-message", "brainstorm", "askgate"])
      expect(headUnmoved(pushed("", undefined, scenario))).toBeUndefined();
  });

  test("a push with no line or a failed line is unhealthy, naming the push", () => {
    expect(headUnmoved(pushed(push(b), opened))).toBe(
      `head: pushes.log:1 pushed ${b} to refs/heads/legion/WID-1, and logs/heads.log holds no line storing it, so the run's Dispatch went on comparing results with the head before it`
    );
    expect(headUnmoved(pushed(push(b), opened + line(false, "POST refused")))).toBe(
      `head: pushes.log:1 pushed ${b} to refs/heads/legion/WID-1, and the head watcher could not store it: POST refused`
    );
  });

  test("a worker world's record missing, empty or not opened is unhealthy with no push, as any pushing run's is", () => {
    const missing =
      "head: logs/heads.log is missing, so the run kept no record of the heads its Dispatch was given";
    for (const scenario of ["tester-proof", "reviewer-text-findings", "threads-closed"])
      expect(headUnmoved(pushed(undefined, undefined, scenario))).toBe(missing);
    expect(headUnmoved(pushed(push(b), undefined, "ask-on-message"))).toBe(missing);
    expect(headUnmoved(pushed("", ""))).toBe(
      "head: logs/heads.log is empty, so the run kept no record of the heads its Dispatch was given"
    );
    expect(headUnmoved(pushed("", line(true)))).toBe(
      "head: logs/heads.log does not open with the head watcher's opened line, so it is no record of the heads its Dispatch was given"
    );
  });

  test("a line of the record the watcher does not write is unhealthy", () => {
    expect(headUnmoved(pushed(push(b), `${opened + line(true)}garbage\n`))).toBe(
      "head: logs/heads.log:3 is no line the head watcher writes: garbage"
    );
  });

  test("the run's own remote moved off the seeded head with no line storing it is unhealthy, whatever pushes.log holds", () => {
    const run = pushed("", opened);
    const src = path.join(run.dir, "src");
    const git = (...args: string[]) => {
      const ran = spawnChecked(
        [
          "git",
          "-c",
          "user.name=t",
          "-c",
          "user.email=t@example.invalid",
          "-c",
          "commit.gpgsign=false",
          ...args,
        ],
        { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" } }
      );
      if (ran.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${ran.stderr.toString()}`);
      return ran.stdout.toString().trim();
    };
    git("init", "-q", "-b", "legion/WID-1", src);
    git("-C", src, "commit", "-q", "--allow-empty", "-m", "seeded");
    const seeded = git("-C", src, "rev-parse", "HEAD");
    git("clone", "-q", "--bare", src, path.join(run.dir, "remote.git"));
    writeFileSync(
      path.join(run.dir, "world.json"),
      JSON.stringify({
        key: "WID-1",
        repo: "fernbrook/widgets",
        pr: 1001,
        branch: "legion/WID-1",
        base: "main",
        head: seeded,
      })
    );
    expect(headUnmoved(run)).toBeUndefined();
    git("-C", src, "commit", "-q", "--allow-empty", "-m", "pushed");
    const moved = git("-C", src, "rev-parse", "HEAD");
    git("-C", src, "push", "-q", path.join(run.dir, "remote.git"), "legion/WID-1");
    expect(headUnmoved(run)).toBe(
      `head: remote.git's refs/heads/legion/WID-1 stands at ${moved}, not world.json's seeded head ${seeded}, and logs/heads.log holds no line storing it, so a push the record missed left the run's Dispatch comparing results with the head before it`
    );
    writeFileSync(path.join(run.dir, "logs", "heads.log"), opened + line(true, undefined, moved));
    expect(headUnmoved(run)).toBeUndefined();
  });
});

// The tester-proof world's gh routes (worker_routes), answered by the run's gh stand-in. A tester
// finds its pull request with the worker prompt's lookup, `gh pr list --repo <the issue's
// repository> --head legion/<issue> --state open --json number --jq '.[0].number'`
// (packages/daemon/internal/prompts/go/worker-common.md).
describe("worker_routes", () => {
  const pr = {
    number: 1001,
    url: "https://github.com/fernbrook/widgets/pull/1001",
    headRefName: "legion/LWEVAL-1",
    headRefOid: "099b05dc19f7f7e5058e791d16ae05f0b123a2cc",
    body: "Dispatch: LWEVAL-1",
  };
  /** The run directory whose fixtures.json holds worker_routes' routes for `pr`: made by the first
   * call, inside a test, since the file's work directory is made in a hook, and read by every call
   * after it, since no call here edits a body. */
  let run: string | undefined;
  /** `gh <args>` answered by the gh stand-in from worker_routes' fixtures.json for `pr`. */
  function gh(...args: string[]) {
    if (run === undefined) {
      run = freshDir("worker-routes");
      const routes = bash(`${fn("worker_routes")}\nworker_routes "$1" >"$2/fixtures.json"`, [
        JSON.stringify(pr),
        run,
      ]);
      expect(routes).toEqual({ code: 0, stdout: "", stderr: "" });
    }
    const result = spawnChecked(
      [process.execPath, path.join(import.meta.dir, "gh-standin.ts"), "gh", ...args],
      { env: { ...childEnv, GH_DATA_DIR: run } }
    );
    return {
      code: result.exitCode,
      stdout: result.stdout.toString(),
      stderr: result.stderr.toString(),
    };
  }

  // Catches: a world whose stand-in answers the prompt's lookup of the pull request with a 404,
  // after which a tester took the pull request to be unreachable and left its body unedited; and a
  // list of the open pull requests with no head that answers none, which tells a tester that lists
  // the repository's pull requests that none exists.
  test("a list of the PR's branch, or of no head, lists the PR, picked to the fields asked", () => {
    expect(
      gh(
        "pr",
        "list",
        "--repo",
        "fernbrook/widgets",
        "--head",
        "legion/LWEVAL-1",
        "--state",
        "open",
        "--json",
        "number",
        "--jq",
        ".[0].number"
      )
    ).toEqual({ code: 0, stdout: "1001\n", stderr: "" });
    for (const filter of [
      ["--head", "legion/LWEVAL-1", "--state", "open"],
      ["--head=legion/LWEVAL-1", "--state", "open"],
      ["-H", "legion/LWEVAL-1", "--state", "open"],
      ["--state", "open"],
    ]) {
      expect(gh("pr", "list", ...filter, "--json", "number,headRefOid")).toEqual({
        code: 0,
        stdout: `[{"number":1001,"headRefOid":"${pr.headRefOid}"}]\n`,
        stderr: "",
      });
    }
  });

  // Catches: a list of another branch, or of the closed or merged pull requests (on the PR's own
  // branch too), that names the fixture's open PR, or that fails where GitHub answers an empty
  // list.
  test("a list of any other head, or of closed or merged pull requests, is empty", () => {
    for (const filter of [
      ["--head", "legion/LWEVAL-10"],
      ["--head", "legion/LWEVAL-2"],
      ["--state", "closed"],
      ["-s", "merged"],
      ["--head", "legion/LWEVAL-1", "--state", "closed"],
      ["--state", "merged", "--head", "legion/LWEVAL-1"],
    ]) {
      expect(gh("pr", "list", ...filter, "--json", "number")).toEqual({
        code: 0,
        stdout: "[]\n",
        stderr: "",
      });
    }
  });
});
