import { beforeAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import {
  appendFileSync,
  cpSync,
  existsSync,
  lstatSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { fileLimit, spawnWithin } from "@legion/test-fixtures/row-clock";
import { memoryRoot, temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { argMaxForceHolds } from "../../../../scripts/e2e/lib/arg-max-force";
import { ownGroup, untilRunning } from "../../../../scripts/e2e/lib/own-group";
import { procRootOf } from "../../../../scripts/e2e/lib/proc-root";
import { scriptFunctions } from "../../../../scripts/e2e/lib/script-functions";
import { DAEMON_MODULE, workerPane } from "./daemon-pane";
import { headsCases } from "./heads-scenarios";
import { RIG_WORLD } from "./rig-world";
import {
  JUDGE_OMP,
  JUDGE_OMP_ENVIRONMENT,
  JUDGE_PULLS,
  RIG_STEP,
  rowLimit,
  SCRIPT_RUN,
  VCS_COMMAND,
} from "./test-limit";

/** rig.sh's world variables as its header sets them from rig-world.json, for a row that runs one
 * of its functions alone, and `here`, the directory whose rig-world.json they come from. */
const WORLD_VARS = `here=${import.meta.dir} worker_repo=${RIG_WORLD.repository} worker_base=${RIG_WORLD.defaultBranch}`;

// Every test here runs rig.sh and its stand-ins as real processes, which a loaded machine
// stretches with no bound, so each test gets the limit test-limit.ts computes from the bounds of
// what they wait for: the most of a row of judge-omp's describe (JUDGE_OMP: the row's measured
// normal work and one Docker call's bound, since a stalled call ends the row at once) and four
// rig.sh steps, the limit of every other row.
setDefaultTimeout(
  fileLimit(Math.max(rowLimit(RIG_STEP, RIG_STEP, RIG_STEP, RIG_STEP), rowLimit(JUDGE_OMP)))
);

// The judge runs in a Docker container; every other row here needs no container. Read once, so
// one warning names docker for the whole file.
const withoutDocker = skipWithout("docker");

// rig.sh's install_model_route, run with the checkout's real installers: the route a label's
// profile reaches Anthropic by, picked by LEGION_E2E_MODEL_ROUTE.
const root = path.resolve(import.meta.dir, "../../../..");
const fn = scriptFunctions(path.join(import.meta.dir, "rig.sh"));
/**
 * The directory this file's work directory goes in: memoryRoot, so /dev/shm where the machine has
 * a tmpfs there, unless its real path holds a character other than letters, digits and `/._-`, and
 * then /tmp. A plain path, because the model route installers and judge-omp write the path of each
 * key command, and of the key file, into a profile as one plain `!command` word, and refuse any
 * other character in it (install-model-gateway.sh and install-key-file-model-route.sh at `--dest`,
 * rig.sh's judge_keys at the key file): under a TMPDIR with a space or a quote in its name, every
 * row that installs a route failed on that refusal. A tmpfs, because the file's afterAll removes
 * the whole directory under the file's 60 s: on the disk TMPDIR names, that removal of about 1,600
 * entries took 70 to 90 s at a five-minute load of about 2,200, and failed the file with "a
 * beforeEach/afterEach hook timed out" after every row had passed. `need` is about sixteen times
 * the 1.8 MiB a run held there at its largest (1,392 entries).
 */
const memory = memoryRoot({ need: 32 * 1024 * 1024 });
const workRoot = /^[A-Za-z0-9/._-]+$/.test(realpathSync(memory)) ? memory : "/tmp";
if (workRoot !== memory) {
  console.error(
    `rig.test.ts: ${memory} holds a character a plain \`!command\` word cannot, so the work directory is under /tmp`
  );
}
const work = temporaryDirectory("skill-scenarios-rig", { root: workRoot });
const operatorHome = path.join(work, "operator-home");
// The home the probes must not reach, with the one thing they look for in it: a judge that could
// read it would list this directory. It is staged in a hook, below the one the reservation
// registered: a run that selects no row of this file makes neither. Each level is made on its
// own, never with `recursive`, so this hook makes nothing above the reserved root and a root the
// fixture did not make is an error here rather than a directory nothing removes.
beforeAll(() => {
  mkdirSync(operatorHome);
  mkdirSync(path.join(operatorHome, ".ssh"));
  writeFileSync(path.join(operatorHome, ".ssh", "id_ed25519"), "not a key\n", { mode: 0o600 });
});
/** The variables every child process of this file gets: the caller's PATH, a HOME of the test's
 * own, and the caller's mise data and config directories when the caller names them. A tool on
 * PATH can be a mise shim (jq and jj are, on a devbox). Under a fresh HOME such a shim finds
 * neither its installs nor its global config, so it fails. make_omp_home keeps the operator's two
 * directories for the processes it starts for the same reason, and base_env passes MISE_DATA_DIR
 * to every agent. A caller that names neither variable gets neither: the developer's own home
 * decides nothing here. */
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
};

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
  const result = spawnWithin(
    { ...RIG_STEP, part: "rig.sh install_model_route" },
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
    const served = spawnWithin({ ...RIG_STEP, part: "the profile's model-key command" }, [
      path.join(dir, "model-key", "anthropic-key"),
    ]);
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
      "install_superpowers() { fail 'the superpowers probe failed'; }",
      'work=$1 maps=$1/maps; cmd_profile probe "$2"',
    ].join("\n");
    const failed = bash(script, [work, checkout()], {
      ...routeEnv(),
      SKILL_SCENARIOS_SUPERPOWERS: "set",
    });
    expect(failed.code).toBe(1);
    expect(failed.stderr).toContain("the superpowers probe failed");
    // The profile's directory has a random name, and the map's link names the label.
    const P = realpathSync(path.join(work, "maps/profiles/probe"));
    expect(path.relative(realpathSync(work), P)).toMatch(/^profiles\/[0-9a-f]{16}$/);
    expect(existsSync(path.join(P, "home/.omp/profiles/skills/agent/models.yml"))).toBe(true);
    expect(existsSync(path.join(P, "model-key"))).toBe(false);
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
    const keptProfile = realpathSync(path.join(kept, "maps/profiles/probe"));
    expect(existsSync(path.join(keptProfile, "model-key/anthropic-key"))).toBe(true);
    expect(existsSync(path.join(keptProfile, "ready"))).toBe(true);
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

/** Runs SCRIPT (rig.sh functions and a call of them) under bash with ARGS as $1..., in an
 * environment of PATH and the test's own HOME. */
function bash(script: string, args: string[] = [], env: Record<string, string> = {}) {
  const result = spawnWithin(
    { ...RIG_STEP, part: `rig.sh ${script.split("\n").at(-1)}` },
    ["bash", "-c", script, "-", ...args],
    {
      env: { ...childEnv, ...env },
    }
  );
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

// cmd_profile records the label's commit in built-from with checkout_commit. A git worktree holds
// no .jj, and jj alone printed an empty commit line for it.
describe("checkout_commit", () => {
  const git = (dir: string, ...args: string[]) => {
    const result = spawnWithin(
      { ...VCS_COMMAND, part: `git ${args.join(" ")}` },
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
    const result = spawnWithin(
      { ...RIG_STEP, part: "rig.sh score" },
      ["bash", path.join(import.meta.dir, "rig.sh"), "score"],
      {
        env: { ...childEnv, SKILL_SCENARIOS_WORK: work },
      }
    );
    expect(result.exitCode).toBe(1);
    expect(result.stderr.toString()).toContain(`${top}/.omp`);
    expect(existsSync(work)).toBe(false);
  });
});

// A spec-writing or spec-rewrite batch takes one run, numbered one above the highest run of its
// scenario and label in the map, so a later batch never replaces an earlier run (cmd_run replaces a
// run of its name).
describe("next_run", () => {
  const next = (work: string, scenario: string, label: string) =>
    bash(`${fn("next_run")}\nwork=$1 maps=$1/maps\nnext_run "$2" "$3"`, [work, scenario, label]);

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
      mkdirSync(path.join(work, "maps", "runs", run), { recursive: true });
    writeFileSync(path.join(work, "maps", "runs", "spec-writing-s1-99"), "");
    expect(next(work, "spec-writing", "s1")).toEqual({ code: 0, stdout: "11\n", stderr: "" });
  });

  // Catches: a first batch that fails because $work/maps/runs does not exist yet.
  test("1 when the label has no run of the scenario", () => {
    expect(next(freshDir("numbering-empty"), "spec-rewrite", "s1").stdout).toBe("1\n");
  });
});

// run_env_pids names each process whose environment carries a run under DIR, with its start time,
// for the exit handlers' stop_env_processes. It walks every process's environment, so a machine
// whose /proc/<pid>/environ entries together pass ARG_MAX must not hand them to grep as its
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
      env: { PATH: process.env.PATH ?? "", SKILL_SCENARIO_RUN: path.join(dir, "run-1") },
      stdout: "ignore",
      stderr: "ignore",
    });
  const groupBash = (script: string, args: string[], procRoot: string) => {
    const result = Bun.spawnSync(ownGroup(["bash", "-c", script, "-", ...args]), {
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
        const old = 'grep -lzsF -e "SKILL_SCENARIO_RUN=$2" "$proc_root"/[0-9]*/environ';
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
    await untilRunning(run.pid, "sleep");
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
  printf 'launch%s | %s\\n' "\${*:+ $*}" "$(<"$R/prompt.txt")" >>"$R/calls"
  [ -n "\${SECOND:-}" ] && [ $# -gt 0 ] && : >"$R/sessions/fresh.jsonl"
  printf x >>"$R/launches"
  status=0
  [ "$(wc -c <"$R/launches")" != "\${FAIL_AT:-0}" ] || status=1
  printf 'output of turn %s%s\\nexit=%s\\n' "$(wc -c <"$R/launches")" "\${*:+ $*}" "$status" >"$R/out.txt"
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
      "launch | Design it.",
      "seed state <R>/fixture.json 1 <R>",
      "judge answer <R> 1",
      "seed answer <R>/fixture.json <R>/answers-1.jsonl",
      `launch --continue | ${answered}`,
      "seed state <R>/fixture.json 2 <R>",
      "seed approve <R>/fixture.json",
      `launch --continue | ${approved}`,
      "seed state <R>/fixture.json 3 <R>",
    ]);
    expect(read("turns.txt")).toBe("1 first exit=0\n2 answered exit=0\n3 approved exit=0\n");
    expect(read("answers.jsonl")).toBe(read("answers-1.jsonl"));
    expect(read("out-2.txt")).toBe("output of turn 2 --continue\nexit=0\n");
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
    expect(calls).toEqual(["launch | Design it.", "seed state <R>/fixture.json 1 <R>"]);
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
    const run = () =>
      bash(
        `${fn("fail")}\n${fn("pinned_omp")}\n${fn("install_superpowers")}\nsuperpowers_skills=(writing-plans brainstorming)\ninstall_superpowers "$1" skills-s1 omp@pin`,
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
    const skills = path.join(label, "home/.omp/profiles/skills-s1/agent/skills");
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
    expect(env.get("OMP_PROFILE")).toBe("skills-s1");
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
  // A Docker daemon that stalls is the machine's, not one row's. Once rig.sh refuses a judge run at
  // the rows' own Docker bound (JUDGE_OMP_ENVIRONMENT) for a call that bound ended, every later row
  // that needs Docker would wait on the same stalled daemon, call after call, and a daemon that
  // stalls every call would add those bounds up past the CI job's timeout. So that judge's row
  // fails at once with the rig's refusal and starts no judge after it, and each later row that
  // needs Docker (dockerTest) fails at once with the same refusal, starting none. The rows that
  // plant a `docker` of their own with bounds of seconds (stalledDocker, SHORT) wait on no daemon
  // of the machine, so they neither set this nor stop for it.
  let daemonStall: string | undefined;
  /** rig.sh's refusal of a judge for a Docker call its bound ended (judge_docker's docker_late). */
  const LATE_CALL = /^skill-scenarios: judge-omp: docker .+ did not end within its bound of \d+ s/;
  /** A row that reaches the container: skipped without Docker, and failed at once after a stall. */
  const dockerTest = (name: string, row: () => void) =>
    test.skipIf(skip)(name, () => {
      if (daemonStall !== undefined) {
        throw new Error(
          `${daemonStall}: the judge of an earlier row met that stall, and this row starts no judge on the stalled daemon`
        );
      }
      row();
    });
  // Each judge waits for the bounds rig.sh judge-omp runs under (JUDGE_OMP), which the row hands it
  // (JUDGE_OMP_ENVIRONMENT): a Docker daemon that stalls less than a call's bound costs the row
  // time, and one that stalls longer ends the judge with the rig's refusal naming the call, and,
  // at the rows' own Docker bound, the row with it.
  const judge = (
    kind: string,
    dir: string,
    output = "answer.json",
    env: Record<string, string> = {}
  ) => {
    const result = spawnWithin(
      { ...JUDGE_OMP, part: `rig.sh judge-omp ${kind}` },
      ["bash", path.join(import.meta.dir, "rig.sh"), "judge-omp", kind, dir, output],
      {
        env: {
          ...childEnv,
          ...JUDGE_OMP_ENVIRONMENT,
          USER: "rig",
          SKILL_SCENARIOS_WORK: work,
          SKILL_SCENARIOS_JUDGE_OMP: omp,
          SKILL_SCENARIOS_JUDGE_ENV: `OUTSIDE=${outside} OPERATOR_HOME=${operatorHome} WORK=${work}`,
          LEGION_E2E_MODEL_ROUTE: "key-file",
          LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile(0o600),
          ...env,
        },
      }
    );
    if (env.SKILL_SCENARIOS_DOCKER_TIMEOUT === undefined) {
      const late = result.stderr
        .toString()
        .split("\n")
        .find((line) => LATE_CALL.test(line));
      if (late !== undefined) {
        daemonStall = late;
        throw new Error(late);
      }
    }
    return result;
  };
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
  dockerTest(
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
    }
  );

  // Catches: a judge that reads the run's transcript, other runs or the operator's files beside its
  // own directory (as a findings judge did, given no check), or that writes anything but its one
  // answer file and its sessions.
  dockerTest(
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
    }
  );

  // Catches: a bare judge that still loads skills, or a judge-omp that hides omp's failure.
  dockerTest("bare loads no skill, and judge-omp exits with omp's status", () => {
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
      existsSync(path.join(work, "judge-profiles/bare/home/.omp/profiles/judge-bare/agent/skills"))
    ).toBe(false);
  });

  // Catches: a mount two judges share that one can write and the next can read, such as the key
  // command's call record, a path one judge could leave its private input in for another to read.
  // The first judge writes a marker into every directory it can write outside its own directory;
  // the second, of the same kind, looks for one anywhere it can read.
  dockerTest("one judge cannot leave a file that a second judge can read", () => {
    const first = judgeDir();
    const env = (word: string) => ({
      SKILL_SCENARIOS_JUDGE_ENV: `OUTSIDE=${outside} OPERATOR_HOME=${operatorHome} WORK=${work} ${word}`,
    });
    expect(judge("ste", first, "answer.json", env("DROP=one")).exitCode).toBe(0);
    const second = judgeDir();
    expect(judge("ste", second, "answer.json", env("SEEK=1")).exitCode).toBe(0);
    expect(recorded(second, "seek")).toEqual([""]);
  });

  // Catches: a judge kind the rig has no profile for, a directory with no prompt, and an answer
  // file name that escapes the judge's directory or names another of its files.
  test("refuses another kind, a directory with no prompt.txt and a bad answer name", () => {
    expect(judge("plugins", judgeDir()).stderr.toString()).toContain("is ste or bare");
    expect(judge("ste", freshDir("judging-empty")).stderr.toString()).toContain(
      "has no prompt.txt"
    );
    for (const output of ["../answer.json", "sessions", "prompt.txt", "a/b.json", ""]) {
      const result = judge("ste", judgeDir(), output);
      expect(result.exitCode).not.toBe(0);
      expect(result.stderr.toString()).toContain("is not the name of an answer file");
    }
  });

  /**
   * A `docker` first on PATH for a daemon that never answers one kind of call, `hang` (it sleeps
   * far past any bound, as a stalled daemon's client waits on; `none` stalls no call), answers an
   * image's inspect with `inspect` (1: the daemon lacks the image), fails each pull at once with a
   * line of its own on stderr, and answers a run at once. It writes each call's first two words to
   * calls.txt beside it. No row of it needs Docker itself.
   */
  function stalledDocker(hang: "image" | "pull" | "run" | "none", inspect = 0) {
    const bin = freshDir("judge-stalled-docker");
    const calls = path.join(bin, "calls.txt");
    writeFileSync(
      path.join(bin, "docker"),
      `#!/bin/bash\nprintf '%s %s\\n' "$1" "$2" >>${JSON.stringify(calls)}\n[ "$1" != ${hang} ] || exec sleep 600\ncase $1 in image) exit ${inspect} ;; pull) echo 'the registry refused the pull' >&2; exit 1 ;; esac\n`,
      { mode: 0o755 }
    );
    return {
      PATH: `${bin}:${childEnv.PATH}`,
      calls: () => readFileSync(calls, "utf8").split("\n").filter(Boolean),
    };
  }
  /** Bounds that end a stalled call in seconds: the daemon's 2 s, and omp's own 1 s. */
  const SHORT = { SKILL_SCENARIOS_DOCKER_TIMEOUT: "2", SKILL_SCENARIOS_TIMEOUT: "1" };
  /** The rig's refusal lines in a judge's stderr. */
  const refusals = (result: { stderr: Buffer }) =>
    result.stderr
      .toString()
      .split("\n")
      .filter((line) => line.startsWith("skill-scenarios: "));

  // Catches: a Docker call that waits on a stalled daemon with no bound of its own, so only the
  // row's clock ends it and nothing names the call (on 20 November the seven judge rows failed so,
  // at a bound measured on another step), and an inspect the daemon never answered taken for a missing
  // image, which then pulls.
  test("an inspect the daemon does not answer within its bound ends the judge, naming the call", () => {
    const docker = stalledDocker("image");
    const dir = judgeDir();
    const result = judge("bare", dir, "answer.json", { ...SHORT, PATH: docker.PATH });
    expect({ code: result.exitCode, refusals: refusals(result) }).toEqual({
      code: 1,
      refusals: [
        expect.stringMatching(
          /^skill-scenarios: judge-omp: docker image inspect \S+ did not end within its bound of 2 s \(SKILL_SCENARIOS_DOCKER_TIMEOUT\)$/
        ),
      ],
    });
    expect(docker.calls()).toEqual(["image inspect"]);
    expect(existsSync(`${dir}.key`)).toBe(false);
  });

  // Catches: a pull that waits on a stalled daemon with no bound, a refusal that names no call, and
  // a pull its bound ended asked again, each attempt another bound on a daemon that stalled.
  test("a pull the daemon does not answer within its bound ends the judge at once, naming the call", () => {
    const docker = stalledDocker("pull", 1);
    const result = judge("bare", judgeDir(), "answer.json", { ...SHORT, PATH: docker.PATH });
    expect({ code: result.exitCode, refusals: refusals(result) }).toEqual({
      code: 1,
      refusals: [
        expect.stringMatching(
          /^skill-scenarios: judge-omp: docker pull \S+ did not end within its bound of 2 s \(SKILL_SCENARIOS_DOCKER_TIMEOUT\)$/
        ),
      ],
    });
    expect(docker.calls()).toEqual(["image inspect", "pull --quiet"]);
  });

  // Catches: a registry's one bad answer, which the next pull makes good, refusing the judge, and a
  // refusal after the last attempt that drops that attempt's own words.
  test("a pull that fails within its bound is asked again, and the judge is refused after the last", () => {
    const docker = stalledDocker("none", 1);
    const result = judge("bare", judgeDir(), "answer.json", { ...SHORT, PATH: docker.PATH });
    expect({ code: result.exitCode, refusals: refusals(result) }).toEqual({
      code: 1,
      refusals: [
        expect.stringMatching(
          new RegExp(
            `^skill-scenarios: judge-omp: docker could not pull \\S+ in ${JUDGE_PULLS} attempts: the registry refused the pull$`
          )
        ),
      ],
    });
    expect(docker.calls()).toEqual([
      "image inspect",
      ...Array.from({ length: JUDGE_PULLS }, () => "pull --quiet"),
    ]);
  });

  // Catches: a judge's run that waits on a stalled daemon past omp's own bound, and one whose
  // refusal leaves its key directory behind or no exit line in out.txt.
  test("a run that does not end within the daemon's bound and omp's own ends the judge, naming both", () => {
    const docker = stalledDocker("run");
    const dir = judgeDir();
    const result = judge("bare", dir, "answer.json", { ...SHORT, PATH: docker.PATH });
    expect({ code: result.exitCode, refusals: refusals(result) }).toEqual({
      code: 1,
      refusals: [
        expect.stringMatching(
          /^skill-scenarios: judge-omp: docker run \S+ did not end within its bound of 3 s: the daemon's 2 s \(SKILL_SCENARIOS_DOCKER_TIMEOUT\) and omp's own 1 s \(SKILL_SCENARIOS_TIMEOUT\)$/
        ),
      ],
    });
    expect(docker.calls()).toEqual(["image inspect", "run --rm"]);
    expect(readFileSync(path.join(dir, "out.txt"), "utf8")).toBe("exit=137\n");
    expect(existsSync(`${dir}.key`)).toBe(false);
  });

  // Catches: a bound that is no whole number of seconds handed to `timeout` as it stands.
  test("refuses a Docker bound or an omp bound that is no whole number of seconds", () => {
    for (const [name, value] of [
      ["SKILL_SCENARIOS_DOCKER_TIMEOUT", "5s"],
      ["SKILL_SCENARIOS_DOCKER_TIMEOUT", "0"],
      ["SKILL_SCENARIOS_TIMEOUT", "1e3"],
    ] as const) {
      const result = judge("bare", judgeDir(), "answer.json", { [name]: value });
      expect({ name, value, code: result.exitCode, refusals: refusals(result) }).toEqual({
        name,
        value,
        code: 1,
        refusals: [
          `skill-scenarios: judge-omp: ${name} is '${value}', not a whole number of seconds`,
        ],
      });
    }
  });

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
    dockerTest(
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
      }
    );

    // Catches: a token directory or file that others can read while the judge runs. The modes
    // are the judge's own reading of them from inside the container, recorded while it ran
    // (`recorded(dir, "mode")`), rather than a watcher's from outside: the stand-in omp sleeps
    // for nothing now, so there is no window for a watcher to look through, and a row that
    // waited for one would be waiting on a sleep.
    dockerTest("the token file is the owner's alone while the judge runs", () => {
      const dir = gatewayDir();
      const result = gatewayJudge("bare", dir);
      expect(judged(result)).toEqual(judgedCleanly);
      expect(recorded(dir, "mode")).toEqual(["key-dir 700", "token 600"]);
      expect(existsSync(`${dir}.key`)).toBe(false);
    });

    // Catches: a token file left behind when the judge fails, and a failed mint that starts the
    // judge anyway.
    dockerTest(
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
      }
    );
  });

  // Catches: a judge profile whose built-from records an empty commit line when the rig's root is
  // no checkout, so a judge's model run cannot be traced to the code that built it.
  dockerTest("fails, naming the rig's root, when the root is no checkout", () => {
    const top = freshDir("judge-root");
    const scripts = path.join(top, "packages/pi-legion/scripts/skill-scenarios");
    mkdirSync(scripts, { recursive: true });
    for (const file of ["rig.sh", "rig-world.json"])
      writeFileSync(path.join(scripts, file), readFileSync(path.join(import.meta.dir, file)));
    for (const file of [".omp-pin", "skills/asd-ste100/SKILL.md"]) {
      mkdirSync(path.dirname(path.join(top, file)), { recursive: true });
      writeFileSync(path.join(top, file), readFileSync(path.join(root, file)));
    }
    mkdirSync(path.join(top, "scripts/e2e"), { recursive: true });
    cpSync(path.join(root, "scripts/e2e/lib"), path.join(top, "scripts/e2e/lib"), {
      recursive: true,
    });
    const rootWork = freshDir("judge-root-work");
    const result = spawnWithin(
      { ...JUDGE_OMP, part: "rig.sh judge-omp ste" },
      ["bash", path.join(scripts, "rig.sh"), "judge-omp", "ste", judgeDir(), "answer.json"],
      {
        env: {
          ...childEnv,
          ...JUDGE_OMP_ENVIRONMENT,
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
  });
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

// launch with a stand-in tmux that records its session's name and starts the pane's command a
// moment later, as a real tmux server does, a stand-in systemd-run that records the scope it was
// asked for and runs the command after `--`, a stand-in bwrap that records its arguments and runs
// the command after `--`, a stand-in mise that prints the omp command line it was given, and a
// stand-in bun that names its binary. The run's directory and the label's profile sit in a work
// directory, as rig.sh lays them out. The pane's PATH holds what an operator's does: a wrapper of
// git and jj in the operator's home first, then mise's shims (jq's is a link to mise, as mise
// makes them), then two mise installs, jq's and jj's, under the pane's MISE_DATA_DIR, then the
// system. The run's HOME links to a native module cache, and its models.yml reads its key with
// `!cat`, as the provider route writes it; the label's key-file command names a key file.
describe("launch", () => {
  type Dirs = { run: string; profile: string; bin: string; tools: string };
  function launchRun(
    args: string,
    env: Record<string, string> = {},
    before?: (dirs: Dirs) => void
  ) {
    const work = freshDir("launch-work");
    const run = path.join(work, "runs", "0123456789abcdef");
    const profile = path.join(work, "profiles", "fedcba9876543210");
    const bin = freshDir("launch-bin");
    const tools = freshDir("launch-tools");
    for (const dir of ["cwd", "sessions", "tmp", "home/.omp/profiles/skills/agent"])
      mkdirSync(path.join(run, dir), { recursive: true });
    for (const dir of ["pi-envoy", "pi-legion", "model-key"])
      mkdirSync(path.join(profile, dir), { recursive: true });
    // The label's checkout, which no agent's view holds, and the map's link to its profile.
    const checkout = freshDir("launch-checkout");
    writeFileSync(path.join(profile, "checkout"), `${checkout}\n`);
    mkdirSync(path.join(work, "services"));
    mkdirSync(path.join(work, "maps", "profiles"), { recursive: true });
    symlinkSync(
      "../../profiles/fedcba9876543210",
      path.join(work, "maps", "profiles", "ablate-head")
    );
    const data = path.join(tools, "mise");
    const wrappers = path.join(tools, "home", ".dotfiles", "shims");
    for (const dir of [wrappers, path.join(data, "shims"), path.join(data, "installs/jq/1.0")])
      mkdirSync(dir, { recursive: true });
    mkdirSync(path.join(data, "installs/jj/0.1/bin"), { recursive: true });
    for (const file of [
      path.join(wrappers, "git"),
      path.join(wrappers, "jj"),
      path.join(data, "installs/jq/1.0/jq"),
      path.join(data, "installs/jj/0.1/bin/jj"),
    ])
      writeFileSync(file, "#!/bin/sh\n", { mode: 0o755 });
    symlinkSync(path.join(bin, "mise"), path.join(data, "shims", "jq"));
    const panePath = [
      bin,
      wrappers,
      path.join(data, "shims"),
      path.join(data, "installs/jq/1.0"),
      path.join(data, "installs/jj/0.1/bin"),
      "/usr/bin",
      "/bin",
    ].join(":");
    mkdirSync(path.join(tools, "natives"));
    symlinkSync(path.join(tools, "natives"), path.join(run, "home/.omp/natives"));
    mkdirSync(path.join(tools, "keys"));
    writeFileSync(path.join(tools, "keys", "token"), "a key\n");
    writeFileSync(
      path.join(run, "home/.omp/profiles/skills/agent/models.yml"),
      `${JSON.stringify({ providers: { rig: { apiKey: `!cat ${tools}/keys/token` } } })}\n`
    );
    writeFileSync(path.join(tools, "anthropic-key"), "a key\n");
    writeFileSync(
      path.join(profile, "model-key", "anthropic-key"),
      `#!/bin/sh\nkey_file=${tools}/anthropic-key\n`,
      { mode: 0o755 }
    );
    writeFileSync(path.join(run, "prompt.txt"), "The person answered your asks on LWEVAL-2.\n");
    writeFileSync(path.join(run, "pane.env"), `PATH=${panePath}\nMISE_DATA_DIR=${data}\n`);
    writeFileSync(path.join(run, "out.txt"), "the first turn\nexit=0\n");
    mkdirSync(path.join(bin, "pin", "bin"), { recursive: true });
    writeFileSync(path.join(bin, "pin", "bin", "omp"), "#!/bin/sh\n", { mode: 0o755 });
    writeFileSync(
      path.join(bin, "mise"),
      `#!/bin/sh\nif [ "$1" = where ]; then echo "${bin}/pin"; exit 0; fi\nshift 3\necho "$@"\n`,
      { mode: 0o755 }
    );
    writeFileSync(path.join(bin, "bun-real"), "", { mode: 0o755 });
    writeFileSync(path.join(bin, "bun"), `#!/bin/sh\necho "${bin}/bun-real"\n`, { mode: 0o755 });
    writeFileSync(
      path.join(bin, "systemd-run"),
      `#!/bin/bash\nscope=()\nwhile [ "$1" != -- ]; do scope+=("$1"); shift; done\nshift\necho "\${scope[*]}" >"${run}/scope"\nexec "$@"\n`,
      { mode: 0o755 }
    );
    writeFileSync(
      path.join(bin, "bwrap"),
      `#!/bin/bash\n: >"${run}/view"\nif [ "$1" = --args ]; then tr '\\0' '\\n' <&"$2" >>"${run}/view"; shift 2; fi\nwhile [ "$1" != -- ]; do printf '%s\\n' "$1" >>"${run}/view"; shift; done\nshift\nexec "$@"\n`,
      { mode: 0o755 }
    );
    before?.({ run, profile, bin, tools });
    const standIns = `
tmux() { case $1 in new-session) printf '%s\\n' "$4" >"$R/session"; (command sleep 0.3 && bash -c "\${@: -1}") & ;; esac; }
sleep() { command sleep 0.05; }`;
    const functions = [
      "fail",
      "pinned_omp",
      "view_bind",
      "view_tool",
      "provider_key_file",
      "key_command_file",
      "fresh_gateway_key",
      "agent_view",
      "launch",
    ];
    const result = bash(
      `${functions.map(fn).join("\n")}\n${standIns}\nR=$1 P=$2 work=$3 maps=$3/maps tag=t pin=omp@pin profile=skills\nlaunch${args}\ncp "$R/out.txt" "$R/seen"`,
      [run, profile, work],
      { PATH: `${bin}:${childEnv.PATH}`, ...env }
    );
    const read = (name: string) => readFileSync(path.join(run, name), "utf8");
    return { ...result, run, bin, read, work, profile, checkout, tools, data };
  }

  /** The view's bubblewrap arguments, read back: each bind (`--bind` or `--ro-bind`, a source
   *  and a destination) and, apart, every other argument with its operands. */
  function viewOf(lines: string[]) {
    const binds: { mode: string; source: string; dest: string }[] = [];
    const others: string[] = [];
    const operands: Record<string, number> = {
      "--symlink": 2,
      "--dev": 1,
      "--proc": 1,
      "--tmpfs": 1,
      "--chdir": 1,
    };
    for (let at = 0; at < lines.length; ) {
      const option = lines[at] ?? "";
      if (option === "--bind" || option === "--ro-bind" || option === "--dev-bind") {
        binds.push({ mode: option, source: lines[at + 1] ?? "", dest: lines[at + 2] ?? "" });
        at += 3;
      } else {
        const n = operands[option] ?? 0;
        others.push(lines.slice(at, at + n + 1).join(" "));
        at += n + 1;
      }
    }
    return { binds, others };
  }

  /** What launch binds of the system: /usr, /etc, each directory at / that is no link, and the
   *  directory of each file outside /etc and /usr that a link of /etc names. */
  const systemSources = new Set(["/usr", "/etc"]);
  for (const dir of ["/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32"])
    if (existsSync(dir) && !lstatSync(dir).isSymbolicLink()) systemSources.add(dir);
  for (const name of readdirSync("/etc")) {
    const link = path.join("/etc", name);
    if (!lstatSync(link).isSymbolicLink()) continue;
    let target: string;
    try {
      target = realpathSync(link);
    } catch {
      continue;
    }
    if (!/^\/(etc|usr|proc|dev)\//.test(target)) systemSources.add(path.dirname(target));
  }

  // Catches: a later turn that reads the previous turn's `exit=` line as its own and returns at
  // once, and extra omp arguments placed anywhere but after the prompt.
  test("a later turn passes its arguments to omp and waits for its own exit line", () => {
    const { code, read, bin, run } = launchRun(" --continue");
    expect(code).toBe(0);
    expect(read("seen")).toBe(
      `${bin}/pin/bin/omp -p The person answered your asks on LWEVAL-2. --continue --session-dir ${run}/sessions\nexit=0\n`
    );
  });

  // Catches: an agent that runs in the caller's own scope with no memory cap. On 2026-11-18 one
  // reviewer's shell held an endless command substitution (an unquoted here-document whose
  // backticks ran `yes`) and its omp reached 79 GiB in the session's scope before the kernel
  // killed it. In a scope of its own, capped with no swap, such an agent is killed alone.
  test("the agent runs in a user scope of its own, capped at 8G with no swap, or at SKILL_SCENARIOS_MEMORY_MAX", () => {
    const capped = launchRun("");
    expect(capped.code).toBe(0);
    expect(capped.read("scope")).toBe(
      "--user --scope --quiet -p MemoryMax=8G -p MemorySwapMax=0\n"
    );
    const named = launchRun("", { SKILL_SCENARIOS_MEMORY_MAX: "2G" });
    expect(named.read("scope")).toBe("--user --scope --quiet -p MemoryMax=2G -p MemorySwapMax=0\n");
  });

  // Catches: an agent whose view is the whole machine with holes in it (`--dev-bind / /`), where a
  // search from / reads another label's skills in an older work directory's profile or in the
  // operator's installed plugins; a view that binds a label's checkout, the map, another run or
  // label, the profile's own records, or a tool's wrapper in the operator's home or mise's shims in
  // place of the tool's install; another run's scratch files in the machine's /tmp; and a tmux
  // session named after the run.
  test("the agent's view is an allowlist: the system, its run with its own /tmp, its label's plugins and keys, the services, and its tools' installs, in a PID namespace of its own", () => {
    const { code, read, run, work, profile, checkout, bin, tools, data } = launchRun("");
    expect(code).toBe(0);
    const { binds, others } = viewOf(read("view").split("\n").filter(Boolean));
    expect(binds.filter((bind) => bind.mode === "--dev-bind" || bind.source === "/")).toEqual([]);
    const ro = (source: string, dest = source) => ({ mode: "--ro-bind", source, dest });
    const real = (file: string) => realpathSync(file);
    expect(binds.filter((bind) => !systemSources.has(bind.source))).toEqual([
      { mode: "--bind", source: run, dest: run },
      { mode: "--bind", source: `${run}/tmp`, dest: "/tmp" },
      ro(`${profile}/pi-envoy`),
      ro(`${profile}/pi-legion`),
      ro(`${profile}/model-key`),
      ro(`${work}/services`),
      ro(`${tools}/keys`),
      ro(`${tools}/anthropic-key`),
      ro(real(`${bin}/pin`), `${bin}/pin`),
      ro(real(`${bin}/mise`)),
      ro(real(`${bin}/bun-real`)),
      { mode: "--bind", source: real(`${tools}/natives`), dest: `${tools}/natives` },
      ro(real(`${data}/installs/jj/0.1`), `${data}/installs/jj/0.1`),
      ro(real(`${data}/installs/jq/1.0`), `${data}/installs/jq/1.0`),
    ]);
    for (const bind of binds) expect(bind.source.startsWith(checkout)).toBe(false);
    // `mise x` takes every mise install but the pin's off the agent's PATH, so the run's bin,
    // first on it, links each tool a mise install holds; git is the system's.
    expect(readlinkSync(path.join(run, "bin", "jj"))).toBe(`${data}/installs/jj/0.1/bin/jj`);
    expect(readlinkSync(path.join(run, "bin", "jq"))).toBe(`${data}/installs/jq/1.0/jq`);
    expect(existsSync(path.join(run, "bin", "git"))).toBe(false);
    expect(others.filter((other) => !other.startsWith("--symlink "))).toEqual([
      "--dev /dev",
      "--tmpfs /dev/shm",
      "--proc /proc",
      "--tmpfs /var/tmp",
      "--unshare-pid",
      "--die-with-parent",
      `--chdir ${run}/cwd`,
    ]);
    for (const other of others.filter((one) => one.startsWith("--symlink ")))
      expect(other).toMatch(/^--symlink usr\/\S+ \/\S+$/);
    expect(read("session")).toBe("t-0123456789abcdef\n");
    // bwrap reads the view from a file under <work>, which the view does not hold, and the agent
    // runs mise by the path the view binds.
    expect(read("launch.sh")).not.toContain(checkout);
    expect(read("launch.sh")).toContain(
      ` --args 3 -- env -i "\${vars[@]}" ${real(`${bin}/mise`)} x omp@pin -- `
    );
  });

  // Catches: a gateway route whose key command must mint inside the agent's view, which holds
  // neither hawk-token nor the operator's HOME and session bus; and a kept key that expires
  // before the run's deadline. The stand-in key command serves the kept key while it is good, as
  // install-model-gateway.sh's does, and mints one good for 60 s first and for a day after.
  test("on the gateway route the host mints a key good past the run's deadline, and the view holds the kept key read-only", () => {
    const keyCommand = (dirs: Dirs, lives: string) => {
      mkdirSync(path.join(dirs.profile, "key"));
      mkdirSync(path.join(dirs.profile, "cache"));
      writeFileSync(
        path.join(dirs.profile, "key", "hawk-token"),
        [
          "#!/bin/bash",
          `cache=${dirs.profile}/cache/hawk-token.key calls=${dirs.profile}/key/calls`,
          'echo call >>"$calls"',
          'if [ -s "$cache" ]; then read -r until key <"$cache"; [ "$EPOCHSECONDS" -ge "$until" ] || { echo "$key"; exit 0; }; fi',
          `lives=(${lives})`,
          'n=$(wc -l <"$calls")',
          'printf "%s key%s\\n" $((EPOCHSECONDS + lives[n - 1])) "$n" >"$cache"',
          'echo "key$n"',
          "",
        ].join("\n"),
        { mode: 0o755 }
      );
    };
    const fresh = launchRun("", {}, (dirs) => keyCommand(dirs, "60 86400"));
    expect(fresh.code).toBe(0);
    expect(readFileSync(path.join(fresh.profile, "key", "calls"), "utf8")).toBe("call\ncall\n");
    const [until] = readFileSync(path.join(fresh.profile, "cache", "hawk-token.key"), "utf8").split(
      " "
    );
    expect(Number(until)).toBeGreaterThan(Date.now() / 1000 + 1500);
    const { binds } = viewOf(fresh.read("view").split("\n").filter(Boolean));
    expect(binds).toContainEqual({
      mode: "--ro-bind",
      source: `${fresh.profile}/key`,
      dest: `${fresh.profile}/key`,
    });
    expect(binds).toContainEqual({
      mode: "--ro-bind",
      source: `${fresh.profile}/cache`,
      dest: `${fresh.profile}/cache`,
    });
    const short = launchRun("", {}, (dirs) => keyCommand(dirs, "60 60"));
    expect(short.code).not.toBe(0);
    expect(short.stderr).toContain("before the run's deadline");
  });
});

// The heads scenarios wait for the run's Dispatch to know #702's head before any agent starts:
// fixture_heads with a stand-in for the label checkout's `dispatch` command (its
// packages/envoy-client/bin/dispatch.ts), which records each call and prints the line of #702 from
// the call HEADS_LINE_AT on, beside a line of #703 on every call, and a stand-in sleep that records
// each wait. Like the real command, the stand-in refuses a call that names no host or no session
// (`DISPATCH_HOST is not set`), and prints no line of a head then.
describe("fixture_heads", () => {
  function waitFor(lineAt: number) {
    const run = freshDir("heads-run");
    const co = freshDir("heads-checkout");
    const rigWork = freshDir("heads-work");
    const bin = path.join(co, "packages", "envoy-client", "bin");
    mkdirSync(bin, { recursive: true });
    writeFileSync(
      path.join(bin, "dispatch.ts"),
      `import { appendFileSync, existsSync, readFileSync } from "node:fs";
const calls = process.env.HEADS_CALLS ?? "";
const n = (existsSync(calls) ? readFileSync(calls, "utf8").split("\\n").filter(Boolean).length : 0) + 1;
appendFileSync(calls, \`\${JSON.stringify({ argv: process.argv.slice(2), url: process.env.DISPATCH_URL, token: process.env.DISPATCH_TOKEN_FILE, host: process.env.DISPATCH_HOST, session: process.env.DISPATCH_SESSION_ID })}\\n\`);
if (!process.env.DISPATCH_HOST || !process.env.DISPATCH_SESSION_ID) {
  console.log("dispatch: DISPATCH_HOST is not set.");
  process.exit(1);
}
console.log(\`- k1 (before_merge). Compared with the heads of 2 open pull requests. Read \${n}.\`);
console.log("  A pass stands: no, as 0 of 2 heads read yes");
console.log("  - #703 at ${"3".repeat(40)}: no, as fail f1 counts there.");
if (n >= Number(process.env.HEADS_LINE_AT)) console.log("  - #702 at ${"2".repeat(40)}: yes, from implementer, tester. Passes p1, p2 decide it.");
`
    );
    const calls = path.join(run, "calls.jsonl");
    const result = bash(
      [
        fn("fail"),
        fn("fixture_heads"),
        `sleep() { printf '%s\\n' "$1" >>"$R/sleeps"; }`,
        "R=$1 co=$2 work=$3 dispatch_port=4567 worker_key=LWEVAL-9",
        "fixture_heads 702",
      ].join("\n"),
      [run, co, rigWork],
      { HEADS_CALLS: calls, HEADS_LINE_AT: String(lineAt) }
    );
    const read = (name: string) =>
      existsSync(path.join(run, name)) ? readFileSync(path.join(run, name), "utf8") : "";
    const made = read("calls.jsonl").split("\n").filter(Boolean);
    return { ...result, run, rigWork, read, made: made.map((line) => JSON.parse(line)) };
  }

  // Catches: a wait that starts the agent before Dispatch learned #702's head, one that takes the
  // line of #703 for it, one that reads another Dispatch than the run's own, one that names no host
  // or session (the real command refuses it and prints no line of a head), and one that keeps no
  // record of what it read.
  test("a line of #702 on the third read passes, and that read's output is results-before.txt", () => {
    const { code, stderr, read, made, rigWork } = waitFor(3);
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    expect(made).toHaveLength(3);
    expect(made[0]).toEqual({
      argv: ["results", "--issue", "LWEVAL-9"],
      url: "http://127.0.0.1:4567",
      token: path.join(rigWork, "services", "dispatch-token"),
      host: "omp",
      session: "rig-fixture-heads",
    });
    expect(read("sleeps")).toBe("2\n2\n");
    expect(read("results-before.txt")).toContain("Read 3.");
    expect(read("results-before.txt")).toContain(`  - #702 at ${"2".repeat(40)}: yes`);
  });

  // Catches: a wait with no deadline, one shorter than 60 s, a failure that does not name the head
  // or the file, and one that leaves no record of the last read.
  test("no line of #702 in 60 s fails, naming #702 and the last read's file", () => {
    const { code, stderr, read, made, run } = waitFor(1000);
    expect(code).toBe(1);
    expect(stderr).toContain("#702");
    expect(stderr).toContain(path.join(run, "results-before.txt"));
    expect(made).toHaveLength(31);
    const waited = read("sleeps")
      .split("\n")
      .filter(Boolean)
      .reduce((sum, seconds) => sum + Number(seconds), 0);
    expect(waited).toBe(60);
    expect(read("results-before.txt")).toContain("Read 31.");
    expect(read("results-before.txt")).not.toContain("#702");
  });
});

// A run of a -hidden label turns its Dispatch's switch on after the rig's own reads and off once
// its agent has ended (hide_state_on, hide_state_off). Stand-ins for the label checkout's
// `dispatch` (its packages/envoy-client/bin/dispatch.ts) and for curl print whether the switch's
// file existed when each ran, and a stand-in agent records whether it saw the switch on and
// results-before.txt in its run's directory.
describe("hide_state_on and hide_state_off", () => {
  function hiddenRun(hidden: boolean, ending: "ends" | "fails") {
    const run = freshDir("hide-run");
    const co = freshDir("hide-checkout");
    const rigWork = freshDir("hide-work");
    const quiet = path.join(rigWork, "maps", "quiet");
    mkdirSync(quiet, { recursive: true });
    mkdirSync(path.join(rigWork, "services"));
    writeFileSync(path.join(rigWork, "services", "dispatch-token"), "e2e-token\n");
    const bin = path.join(co, "packages", "envoy-client", "bin");
    mkdirSync(bin, { recursive: true });
    writeFileSync(
      path.join(bin, "dispatch.ts"),
      `import { existsSync } from "node:fs";
const on = (process.env.SWITCH ?? "") !== "" && existsSync(process.env.SWITCH ?? "");
console.log(\`dispatch \${process.argv.slice(2).join(" ")} at \${process.env.DISPATCH_URL} as \${process.env.DISPATCH_HOST}: switch \${on ? "on" : "off"}\`);
`
    );
    writeFileSync(path.join(run, "results-before.txt"), `  - #702 at ${"2".repeat(40)}: yes\n`);
    const sw = hidden ? path.join(quiet, path.basename(run)) : "";
    const result = bash(
      [
        fn("quiet_reads"),
        fn("hide_state_on"),
        fn("hide_state_off"),
        fn("stop_run"),
        "stop_tree() { :; }",
        "stop_panes() { :; }",
        "stop_env_processes() { :; }",
        `curl() { printf 'curl %s [stdin %s]: switch %s\\n' "$*" "$(cat)" "$([ -e "$SWITCH" ] && echo on || echo off)"; }`,
        `agent() { { [ -e "$SWITCH" ] && echo switch=on || echo switch=off; [ -e "$R/results-before.txt" ] && echo before=seen || echo before=unseen; } >"$R/agent-saw"; }`,
        "R=$1 co=$2 work=$3 maps=$3/maps dispatch_port=4567 worker_key=ACME-1 hide_state_file=$SWITCH",
        "session='' daemon_pid='' dispatch_pid='' natsd_pid='' fake_github_pid=''",
        "trap stop_run EXIT",
        "hide_state_on",
        "agent",
        ending === "ends" ? "hide_state_off" : "exit 1",
      ].join("\n"),
      [run, co, rigWork],
      { SWITCH: sw }
    );
    const read = (file: string) => (existsSync(file) ? readFileSync(file, "utf8") : "");
    return { ...result, run, sw, quiet, read };
  }

  // Catches: a switch turned on before the rig's own reads, or after the agent starts; one left on
  // after the agent; a results-before.txt the agent's view still holds, or one not put back for
  // score.ts; and reads of the results the rig does not keep.
  test("a hidden run's agent sees the switch on and no results-before.txt, between the rig's reads before and after", () => {
    const { code, stderr, run, sw, read } = hiddenRun(true, "ends");
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    expect(read(path.join(run, "agent-saw"))).toBe("switch=on\nbefore=unseen\n");
    expect(read(`${sw}.before-text`)).toBe(
      "dispatch results --issue ACME-1 at http://127.0.0.1:4567 as omp: switch off\n"
    );
    expect(read(`${sw}.after-text`)).toBe(
      "dispatch results --issue ACME-1 at http://127.0.0.1:4567 as omp: switch on\n"
    );
    expect(read(`${sw}.before-json`)).toBe(
      "dispatch results --issue ACME-1 --json at http://127.0.0.1:4567 as omp: switch off\n"
    );
    expect(read(`${sw}.after-json`)).toContain("--json at http://127.0.0.1:4567 as omp: switch on");
    // The token reaches curl on its standard input, never in its arguments, which another process
    // can read (packages/daemon/internal/textcheck).
    expect(read(`${sw}.before-curl`)).toBe(
      "curl -sS -H @- http://127.0.0.1:4567/api/v1/issues/ACME-1/results [stdin Authorization: Bearer e2e-token]: switch off\n"
    );
    expect(read(`${sw}.after-curl`)).toContain(
      "/results [stdin Authorization: Bearer e2e-token]: switch on"
    );
    expect(existsSync(sw)).toBe(false);
    expect(existsSync(`${sw}.results-before.txt`)).toBe(false);
    expect(read(path.join(run, "results-before.txt"))).toContain("#702");
  });

  // Catches: a run that fails with the switch on and leaves it on, or keeps results-before.txt in
  // the map, where score.ts does not read it.
  test("a hidden run whose agent fails ends with the switch off and results-before.txt back", () => {
    const { code, run, sw, read } = hiddenRun(true, "fails");
    expect(code).toBe(1);
    expect(read(path.join(run, "agent-saw"))).toBe("switch=on\nbefore=unseen\n");
    expect(existsSync(sw)).toBe(false);
    expect(read(path.join(run, "results-before.txt"))).toContain("#702");
  });

  // Catches: a switch that a run of any other label turns on.
  test("a run of another label reads nothing more, and its agent sees what it saw before", () => {
    const { code, stderr, run, quiet } = hiddenRun(false, "ends");
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    expect(readFileSync(path.join(run, "agent-saw"), "utf8")).toBe("switch=off\nbefore=seen\n");
    expect(readdirSync(quiet)).toEqual([]);
  });

  // Catches: a switch the run's Dispatch never learns of.
  test("start_dispatch names the run's switch to run-server.sh, and an empty one for any other run", () => {
    const started = (sw: string) => {
      const run = freshDir("hide-dispatch");
      mkdirSync(path.join(run, "logs"));
      const result = bash(
        [
          fn("start_dispatch"),
          `start_process() { printf '%s|%s\\n' "\${DISPATCH_TEST_HIDE_STATE_FILE-unset}" "$*" >"$evidence/started"; }`,
          "await_start() { :; }",
          "report_port() { echo 4567; }",
          "evidence=$1 root=/checkout hide_state_file=$2",
          "start_dispatch postgres://db",
        ].join("\n"),
        [run, sw]
      );
      return { code: result.code, started: readFileSync(path.join(run, "started"), "utf8") };
    };
    expect(started("/w/maps/quiet/0123456789abcdef")).toEqual({
      code: 0,
      started:
        "/w/maps/quiet/0123456789abcdef|dispatch bash /checkout/packages/dispatch/e2e/run-server.sh\n",
    });
    expect(started("").started).toBe(
      "|dispatch bash /checkout/packages/dispatch/e2e/run-server.sh\n"
    );
  });
});

// services_up, then services_down, with a stand-in docker, which records each call, answers each
// `run` with an id and `logs` with the lines NATS logs on a port it chose, and refuses a `run` that
// publishes a host port, as Docker does when another process holds the port it chose.
describe("services_up", () => {
  const TAKEN =
    "docker: Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint t-nats (bb7e): failed to bind host port 127.0.0.1:41026/tcp: address already in use";
  function up(owner: string | undefined) {
    const dir = freshDir("services-up");
    mkdirSync(path.join(dir, "services"));
    mkdirSync(path.join(dir, "root", "packages", "envoy"), { recursive: true });
    mkdirSync(path.join(dir, "root", "scripts", "e2e", "lib"), { recursive: true });
    writeFileSync(
      path.join(dir, "root", "scripts", "e2e", "lib", "nats-image.sh"),
      "nats_image=nats:rig\n"
    );
    const result = bash(
      [
        "set -euo pipefail",
        fn("fail"),
        fn("services_up"),
        fn("services_down"),
        `docker() {
  printf '%s\\n' "$*" >>"$S/calls"
  case $1 in
  run)
    case " $* " in *" -p 127.0.0.1:"* | *" --publish "*) echo "${TAKEN}" >&2 && return 125 ;; esac
    n=$((n + 1)) && echo "id-$n"
    ;;
  logs)
    echo "[1] 2026/11/21 01:56:40.269703 [INF] Listening for client connections on 127.0.0.1:4567"
    echo "[1] 2026/11/21 01:56:40.269710 [INF] Server is ready"
    ;;
  esac
}`,
        "until_true() { :; }",
        "go() { :; }",
        "start_listener() { envoy_port=1; }",
        "stop_tree() { :; }",
        "note() { :; }",
        "n=0 S=$1/services root=$1/root tag=t services_env=$1/services/env",
        "services_up",
        `cp "$services_env" "$S/env-seen" && printf '%s' "$pg_socket" >"$S/socket-seen"`,
        `[ -d "$pg_socket" ] && echo made >"$S/socket-made"`,
        "services_down",
      ].join("\n"),
      [dir],
      owner === undefined ? {} : { SKILL_SCENARIOS_OWNER: owner }
    );
    const read = (name: string) =>
      existsSync(path.join(dir, "services", name))
        ? readFileSync(path.join(dir, "services", name), "utf8")
        : "";
    const socket = read("socket-seen");
    return {
      code: result.code,
      stderr: result.stderr,
      containers: read("containers"),
      runs: read("calls")
        .split("\n")
        .filter((line) => line.startsWith("run ")),
      env: read("env-seen"),
      socket,
      socketMade: read("socket-made"),
      socketLeft: socket !== "" && existsSync(socket),
    };
  }

  // Catches: a container no record names, so nothing can remove it by its id; and an owner label
  // missing from one of the two.
  test("each container's id is recorded, and SKILL_SCENARIOS_OWNER labels each one", () => {
    const labelled = up("rig-agent");
    expect({ code: labelled.code, stderr: labelled.stderr }).toEqual({ code: 0, stderr: "" });
    expect(labelled.containers).toBe("id-1\nid-2\n");
    expect(labelled.runs.map((line) => line.split(" ").slice(0, 6).join(" "))).toEqual([
      "run -d --name t-pg --label owner=rig-agent",
      "run -d --name t-nats --label owner=rig-agent",
    ]);
    const plain = up(undefined);
    expect(plain.containers).toBe("id-1\nid-2\n");
    expect(plain.runs.every((line) => !line.includes("--label"))).toBe(true);
  });

  // Catches: a container that publishes a host port, which Docker chooses among the ports it has
  // not handed out itself and another process of a shared host can hold; a batch of
  // reviewer-pass-and-fail ended at that refusal, before any run. And a NATS the listener would
  // refuse as another machine's, or a service the runs' Dispatch servers would look for on a host
  // port.
  test("no container publishes a host port: NATS binds its own loopback port, Postgres is reached through its socket", () => {
    const { code, stderr, runs, env, socket, socketMade, socketLeft } = up(undefined);
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    expect(runs.filter((line) => / (-p 127\.0\.0\.1:|--publish )/.test(line))).toEqual([]);
    const nats = runs.find((line) => line.startsWith("run -d --name t-nats ")) ?? "";
    expect(nats).toEndWith(
      " --network host --cpus 1 --memory 512m nats:rig -js -a 127.0.0.1 -p -1"
    );
    expect(env).toContain("ENVOY_NATS_URL=nats://127.0.0.1:4567\n");
    const postgres = runs.find((line) => line.startsWith("run -d --name t-pg ")) ?? "";
    expect(postgres).toContain(
      `--mount type=bind,source=${socket},destination=/var/run/postgresql`
    );
    expect(postgres).toMatch(/ --user \d+:\d+ /);
    // The socket's directory is a new one of /tmp's, short enough for a socket's path, and
    // services_down removes it.
    expect({
      name: socket.replace(/\.[A-Za-z0-9]{6}$/, ".XXXXXX"),
      socketMade,
      socketLeft,
    }).toEqual({
      name: "/tmp/rig-pg.XXXXXX",
      socketMade: "made\n",
      socketLeft: false,
    });
  });
});

// cmd_batch of reviewer-own-head runs each of its cases <runs> times for each label, each as a
// run of its own. A stand-in rig.sh records each run's arguments, which cmd_batch hands to
// `rig.sh run` through xargs, and a stand-in go records the build of the runs' natsd. The cases a
// batch runs are the scenario's rows of heads-scenarios.txt, the catalog rig.sh's heads_cases
// reads, so each row below takes its cases from that catalog (headsCases): a case the catalog
// gains is a case the rows expect.
describe("cmd_batch", () => {
  // Catches: a batch that runs one case alone, gives two runs one index (and so one PR number and
  // one commit time), drops the case from a run's arguments, or builds no natsd for the runs.
  test("reviewer-own-head runs each case for each label and run number, each with its own index", () => {
    const here = freshDir("batch-here");
    const rigWork = freshDir("batch-work");
    writeFileSync(path.join(here, "rig.sh"), `printf '%s\\n' "$*" >>"${here}/runs"\n`);
    const result = bash(
      [
        fn("fail"),
        fn("rig_scenario"),
        fn("heads_cases"),
        fn("heads_scenario"),
        fn("hidden_needs_heads"),
        fn("cmd_batch"),
        "load_label() { :; }",
        'lock_services() { S=$work/services; mkdir -p "$S"; }',
        "on_exit() { :; }",
        "services_up() { :; }",
        "date() { echo 1700000000; }",
        `go() { printf 'go %s\\n' "$*" >>"$here/builds"; }`,
        `here=$1 work=$2 root=${root}`,
        "cmd_batch reviewer-own-head 2 head ablate-head",
      ].join("\n"),
      [here, rigWork]
    );
    expect({ code: result.code, stderr: result.stderr }).toEqual({ code: 0, stderr: "" });
    const cases = headsCases("reviewer-own-head");
    expect(cases).toContain("sibling-fails");
    expect(cases).not.toContain("");
    let index = 0;
    const want = ["head", "ablate-head"].flatMap((label) =>
      cases.flatMap((which) =>
        [1, 2].map((n) => `run reviewer-own-head ${label} ${n} ${++index} 1700000000 ${which}`)
      )
    );
    const runs = readFileSync(path.join(here, "runs"), "utf8").split("\n").filter(Boolean);
    expect(runs.toSorted()).toEqual(want.toSorted());
    expect(readFileSync(path.join(here, "builds"), "utf8")).toBe(
      `go -C ${root}/packages/envoy build -o ${rigWork}/services/bin/natsd ./internal/testnats/cmd/natsd\n`
    );
  });

  // Catches: a run whose name leaves out its case, so the cases' batches of one label, each
  // numbered from 1, replace each other's runs; a case outside the catalog's; and a run directory
  // whose own name tells its agent the scenario, the case or the label (each is in the map alone).
  test("cmd_run names a reviewer-own-head run by its case in the map, in a directory of a random name, and refuses another case", () => {
    // rig.sh resolves its work directory's path once (realpath), as this does.
    const rigWork = realpathSync(freshDir("run-work"));
    mkdirSync(path.join(rigWork, "services"), { recursive: true });
    writeFileSync(path.join(rigWork, "services", "env"), "");
    const script = [
      fn("fail"),
      fn("rig_scenario"),
      fn("heads_cases"),
      fn("heads_scenario"),
      fn("hidden_needs_heads"),
      fn("one_of"),
      fn("listed_in"),
      fn("cmd_run"),
      'load_label() { P=$work/profiles/0000000000000001; mkdir -p "$P"; echo "checkout /c/$1" >"$P/built-from"; }',
      "on_exit() { :; }",
      `run_reviewer_own_head() { printf '%s\\n' "$*" >"$R/ran"; echo exit=0 >"$R/out.txt"; }`,
      `work=$1 maps=$1/maps services_env=$1/services/env root=${root}`,
      'cmd_run reviewer-own-head head 1 3 1700000000 "$2"',
    ].join("\n");
    const link = path.join(rigWork, "maps", "runs", "reviewer-own-head-ci-only-head-1");
    const ran = bash(script, [rigWork, "ci-only"]);
    expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
    const first = realpathSync(link);
    expect(path.relative(rigWork, first)).toMatch(/^runs\/[0-9a-f]{16}$/);
    expect(readFileSync(path.join(first, "ran"), "utf8")).toBe("3 1700000000 ci-only\n");
    expect(existsSync(path.join(first, "case.txt"))).toBe(false);
    expect(existsSync(path.join(first, "built-from"))).toBe(false);
    expect(
      readFileSync(
        path.join(rigWork, "maps", "built-from", "reviewer-own-head-ci-only-head-1"),
        "utf8"
      )
    ).toBe("checkout /c/head\n");
    // A second run of the name replaces the first: its directory and its link.
    expect(bash(script, [rigWork, "ci-only"]).code).toBe(0);
    expect(realpathSync(link)).not.toBe(first);
    expect(existsSync(first)).toBe(false);
    const refused = bash(script, [rigWork, "frobnicate"]);
    expect(refused.code).toBe(1);
    const cases = headsCases("reviewer-own-head");
    expect(refused.stderr).toContain(
      `'frobnicate' is not ${cases.slice(0, -1).join(", ")} or ${cases.at(-1)}`
    );
  });

  // Catches: a -hidden label whose run has no switch (its agent then sees all Dispatch computes,
  // under the hidden label's name), a switch inside the run's directory, which the agent's view
  // holds, and a switch for a run of another label.
  test("cmd_run gives a run of a -hidden label a switch in the map named after its directory, and no other run one", () => {
    const rigWork = realpathSync(freshDir("hidden-run-work"));
    mkdirSync(path.join(rigWork, "services"), { recursive: true });
    writeFileSync(path.join(rigWork, "services", "env"), "");
    const script = [
      fn("fail"),
      fn("rig_scenario"),
      fn("heads_cases"),
      fn("heads_scenario"),
      fn("hidden_needs_heads"),
      fn("listed_in"),
      fn("cmd_run"),
      'load_label() { P=$work/profiles/0000000000000001; mkdir -p "$P"; echo "checkout /c/$1" >"$P/built-from"; }',
      "on_exit() { :; }",
      `run_architect_pass_and_fail() { printf '%s\\n' "$hide_state_file" >"$R/switch"; echo exit=0 >"$R/out.txt"; }`,
      `work=$1 maps=$1/maps services_env=$1/services/env root=${root} hide_state_file=''`,
      'cmd_run architect-pass-and-fail "$2" 1 1 1700000000',
    ].join("\n");
    expect(bash(script, [rigWork, "head-hidden"])).toMatchObject({ code: 0, stderr: "" });
    const hiddenRun = realpathSync(
      path.join(rigWork, "maps", "runs", "architect-pass-and-fail-head-hidden-1")
    );
    expect(readFileSync(path.join(hiddenRun, "switch"), "utf8")).toBe(
      `${path.join(rigWork, "maps", "quiet", path.basename(hiddenRun))}\n`
    );
    expect(bash(script, [rigWork, "head"])).toMatchObject({ code: 0, stderr: "" });
    const run = realpathSync(path.join(rigWork, "maps", "runs", "architect-pass-and-fail-head-1"));
    expect(readFileSync(path.join(run, "switch"), "utf8")).toBe("\n");
  });

  // Catches: an architect-pass-and-fail batch that runs its own world alone, or its cases alone; a
  // case run whose name leaves out its case, so it replaces the scenario's own run of that number;
  // and a case outside the catalog's.
  test("architect-pass-and-fail runs its own world and each case the catalog lists, each case run named by its case", () => {
    const here = freshDir("architect-batch-here");
    const rigWork = freshDir("architect-batch-work");
    writeFileSync(path.join(here, "rig.sh"), `printf '%s\\n' "$*" >>"${here}/runs"\n`);
    const batch = bash(
      [
        fn("fail"),
        fn("rig_scenario"),
        fn("heads_cases"),
        fn("heads_scenario"),
        fn("hidden_needs_heads"),
        fn("cmd_batch"),
        "load_label() { :; }",
        'lock_services() { S=$work/services; mkdir -p "$S"; }',
        "on_exit() { :; }",
        "services_up() { :; }",
        "date() { echo 1700000000; }",
        "go() { :; }",
        `here=$1 work=$2 root=${root}`,
        "cmd_batch architect-pass-and-fail 1 head ablate-states-read",
      ].join("\n"),
      [here, rigWork]
    );
    expect({ code: batch.code, stderr: batch.stderr }).toEqual({ code: 0, stderr: "" });
    const worlds = headsCases("architect-pass-and-fail");
    expect(worlds).toContain("");
    expect(worlds).toContain("no-verdict-fail");
    let index = 0;
    const want = ["head", "ablate-states-read"].flatMap((label) =>
      worlds.map(
        (which) =>
          `run architect-pass-and-fail ${label} 1 ${++index} 1700000000${which ? ` ${which}` : ""}`
      )
    );
    const runs = readFileSync(path.join(here, "runs"), "utf8").split("\n").filter(Boolean);
    expect(runs.toSorted()).toEqual(want.toSorted());

    const work = realpathSync(freshDir("architect-run-work"));
    mkdirSync(path.join(work, "services"), { recursive: true });
    writeFileSync(path.join(work, "services", "env"), "");
    const script = [
      fn("fail"),
      fn("rig_scenario"),
      fn("heads_cases"),
      fn("heads_scenario"),
      fn("hidden_needs_heads"),
      fn("one_of"),
      fn("listed_in"),
      fn("cmd_run"),
      'load_label() { P=$work/profiles/0000000000000001; mkdir -p "$P"; echo "checkout /c/$1" >"$P/built-from"; }',
      "on_exit() { :; }",
      `run_architect_pass_and_fail() { printf '%s\\n' "$*" >"$R/ran"; echo exit=0 >"$R/out.txt"; }`,
      `work=$1 maps=$1/maps services_env=$1/services/env root=${root} hide_state_file=''`,
      'if [ -n "$2" ]; then cmd_run architect-pass-and-fail head 1 4 1700000000 "$2"; else cmd_run architect-pass-and-fail head 1 4 1700000000; fi',
    ].join("\n");
    expect(bash(script, [work, "no-verdict-fail"])).toMatchObject({ code: 0, stderr: "" });
    expect(bash(script, [work, ""])).toMatchObject({ code: 0, stderr: "" });
    const cased = realpathSync(
      path.join(work, "maps", "runs", "architect-pass-and-fail-no-verdict-fail-head-1")
    );
    const own = realpathSync(path.join(work, "maps", "runs", "architect-pass-and-fail-head-1"));
    expect(cased).not.toBe(own);
    expect(readFileSync(path.join(cased, "ran"), "utf8")).toBe("4 1700000000 no-verdict-fail\n");
    expect(readFileSync(path.join(own, "ran"), "utf8")).toBe("4 1700000000 \n");
    const refused = bash(script, [work, "frobnicate"]);
    expect(refused.code).toBe(1);
    const cases = worlds.filter(Boolean);
    expect(refused.stderr).toContain(
      `'frobnicate' is not ${cases.slice(0, -1).join(", ")} or ${cases.at(-1)}`
    );
  });

  // Catches: a writer of a task that reads the task words inside another command's argument, where
  // a failure of that read ends only its own subshell, so the run writes the task without the words
  // and launches its role as if the label had none; a reader that takes a words file it cannot
  // read, or one that holds no words, for a label with none; and a writer that answers success with
  // a task no agent could work from, an empty file or one naming no issue, or with the label's
  // words left out. The label's checkout holds the file (task-words.txt) only where the label's
  // patch adds it: a link to nothing and a file of blanks each fail both writers, naming the label,
  // with no task written; a checkout with no file and one whose file holds words each give a task
  // that carries the issue key the run passes in, a value of this run's own, and the task given the
  // words is longer than the one given none. The test reads none of the task's words.
  test("a label's task words that cannot be read fail the reviewer's and the architect's task writers", () => {
    const run = freshDir("task-words-fail-run");
    writeFileSync(
      path.join(run, "fixture.json"),
      JSON.stringify({ title: "A usage page", test: { version: 3 }, review: { version: 4 } })
    );
    const key = `LWEVAL-${process.pid}`;
    const prompt = path.join(run, "prompt.txt");
    const checkout = (name: string, words?: (file: string) => void) => {
      const co = freshDir(`task-words-${name}`);
      const dir = path.join(co, "packages/pi-legion/scripts/skill-scenarios");
      mkdirSync(dir, { recursive: true });
      words?.(path.join(dir, "task-words.txt"));
      return co;
    };
    const write = (fnName: string, co: string) => {
      rmSync(prompt, { force: true });
      return bash(
        [
          fn("fail"),
          fn("task_words"),
          fn(fnName),
          `R=$1 co=$2 worker_key=$3 label=ablate-states-read`,
          fnName,
          `echo "${fnName} returned"`,
        ].join("\n"),
        [run, co, key]
      );
    };
    // written is the task fnName writes for co, which must answer success and leave a task that
    // carries the issue key this run passed in.
    const written = (fnName: string, co: string) => {
      expect(write(fnName, co)).toMatchObject({ code: 0, stderr: "" });
      const task = readFileSync(prompt, "utf8");
      expect(task).toContain(key);
      return task;
    };
    const unreadable = [
      checkout("dangling", (file) => symlinkSync(path.join(path.dirname(file), "gone.txt"), file)),
      checkout("blank", (file) => writeFileSync(file, " \n\t\n")),
    ];
    const none = checkout("none");
    const worded = checkout("worded", (file) =>
      writeFileSync(file, "Tester's verdict:\n  pass.\n")
    );
    for (const fnName of ["reviewer_prompt", "architect_prompt"]) {
      for (const co of unreadable) {
        const result = write(fnName, co);
        expect(result.code).toBe(1);
        expect(result.stdout).not.toContain(`${fnName} returned`);
        expect(result.stderr).toContain("ablate-states-read");
        expect(existsSync(prompt)).toBe(false);
      }
      const bare = written(fnName, none);
      expect(written(fnName, worded).length).toBeGreaterThan(bare.length);
    }
  });

  // Catches: a -hidden label run in a scenario that never turns the switch on, so its agent sees
  // all that Dispatch computes; and a refusal that comes only after the batch started its services.
  test("a -hidden label is refused by any scenario but the heads scenarios, before a batch starts anything", () => {
    const here = freshDir("hidden-batch-here");
    const rigWork = freshDir("hidden-batch-work");
    const result = bash(
      [
        fn("fail"),
        fn("rig_scenario"),
        fn("heads_cases"),
        fn("heads_scenario"),
        fn("hidden_needs_heads"),
        fn("cmd_batch"),
        "load_label() { :; }",
        `lock_services() { echo started >>"$here/started"; }`,
        "on_exit() { :; }",
        "services_up() { :; }",
        `here=$1 work=$2 root=${root}`,
        "cmd_batch measure-before-ask 1 head ablate-x-hidden",
      ].join("\n"),
      [here, rigWork]
    );
    expect(result.code).toBe(1);
    expect(result.stderr).toContain(
      "the label ablate-x-hidden hides Dispatch's computed state, but the heads-scenario catalog does not list measure-before-ask"
    );
    expect(existsSync(path.join(here, "started"))).toBe(false);
  });

  // Catches: a ci-pass-and-fail batch that drives one role alone, or a run whose name leaves out its
  // role, so the roles' runs of one label replace each other.
  test("ci-pass-and-fail runs each role for each label, and cmd_run names the run by its role", () => {
    const here = freshDir("ci-batch-here");
    const rigWork = freshDir("ci-batch-work");
    writeFileSync(path.join(here, "rig.sh"), `printf '%s\\n' "$*" >>"${here}/runs"\n`);
    const batch = bash(
      [
        fn("fail"),
        fn("rig_scenario"),
        fn("heads_cases"),
        fn("heads_scenario"),
        fn("hidden_needs_heads"),
        fn("cmd_batch"),
        "load_label() { :; }",
        'lock_services() { S=$work/services; mkdir -p "$S"; }',
        "on_exit() { :; }",
        "services_up() { :; }",
        "date() { echo 1700000000; }",
        "go() { :; }",
        `here=$1 work=$2 root=${root}`,
        "cmd_batch ci-pass-and-fail 1 head head-hidden",
      ].join("\n"),
      [here, rigWork]
    );
    expect({ code: batch.code, stderr: batch.stderr }).toEqual({ code: 0, stderr: "" });
    let index = 0;
    const want = ["head", "head-hidden"].flatMap((label) =>
      ["reviewer", "tester", "architect"].map(
        (role) => `run ci-pass-and-fail ${label} 1 ${++index} 1700000000 ${role}`
      )
    );
    const runs = readFileSync(path.join(here, "runs"), "utf8").split("\n").filter(Boolean);
    expect(runs.toSorted()).toEqual(want.toSorted());

    const work = realpathSync(freshDir("ci-run-work"));
    mkdirSync(path.join(work, "services"), { recursive: true });
    writeFileSync(path.join(work, "services", "env"), "");
    const script = [
      fn("fail"),
      fn("rig_scenario"),
      fn("heads_cases"),
      fn("heads_scenario"),
      fn("hidden_needs_heads"),
      fn("one_of"),
      fn("listed_in"),
      fn("cmd_run"),
      'load_label() { P=$work/profiles/0000000000000001; mkdir -p "$P"; echo "checkout /c/$1" >"$P/built-from"; }',
      "on_exit() { :; }",
      `run_ci_pass_and_fail() { printf '%s\\n' "$*" >"$R/ran"; echo exit=0 >"$R/out.txt"; }`,
      `work=$1 maps=$1/maps services_env=$1/services/env root=${root} hide_state_file=''`,
      'cmd_run ci-pass-and-fail head 1 2 1700000000 "$2"',
    ].join("\n");
    expect(bash(script, [work, "tester"])).toMatchObject({ code: 0, stderr: "" });
    const run = realpathSync(path.join(work, "maps", "runs", "ci-pass-and-fail-tester-head-1"));
    expect(readFileSync(path.join(run, "ran"), "utf8")).toBe("2 1700000000 tester\n");
    const refused = bash(script, [work, "merger"]);
    expect(refused.code).toBe(1);
    expect(refused.stderr).toContain(
      "the ci-pass-and-fail case 'merger' is not reviewer, tester or architect"
    );
  });
});

// ci_job_results with a stand-in docker, which records the psql call and its SQL and answers the
// count of the rows written: 0 while the run's Dispatch has not indexed the block k1 yet, then 2.
describe("ci_job_results", () => {
  function written(answers: string[]) {
    const run = freshDir("ci-job-run");
    writeFileSync(
      path.join(run, "dispatch.json"),
      JSON.stringify({ database: "run_0123456789abcdef" })
    );
    writeFileSync(
      path.join(run, "fixture.json"),
      JSON.stringify({ verification: "11111111-2222-3333-4444-555555555555" })
    );
    writeFileSync(path.join(run, "heads.json"), JSON.stringify({ "702": "7".repeat(40) }));
    writeFileSync(path.join(run, "answers"), `${answers.join("\n")}\n`);
    const result = bash(
      [
        fn("fail"),
        fn("ci_job_results"),
        `docker() { printf '%s\\n' "$*" >>"$R/calls"; cat >"$R/sql"; head -n 1 "$R/answers"; sed -i 1d "$R/answers"; }`,
        `sleep() { printf '%s\\n' "$1" >>"$R/sleeps"; }`,
        `R=$1 tag=t worker_key=LWEVAL-9 ${WORLD_VARS}`,
        "ci_job_results",
      ].join("\n"),
      [run]
    );
    const read = (name: string) =>
      existsSync(path.join(run, name)) ? readFileSync(path.join(run, name), "utf8") : "";
    return { ...result, read };
  }

  // Catches: rows written before the block exists (none, and no retry), a call that names another
  // database, head, branch, block or job than the run's (the job is rig-world.json's, which
  // Dispatch's GitHub stand-in serves too), and a statement that writes one verdict.
  test("writes the job's pass and fail of k1 at #702's head once the block is indexed", () => {
    const { code, stderr, read } = written(["0", "2"]);
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    expect(read("sleeps")).toBe("2\n");
    const job = RIG_WORLD.ciJobs["ci-pass-and-fail"].name;
    const call = `exec -i t-pg psql -U postgres -d run_0123456789abcdef -v ON_ERROR_STOP=1 -qAt -v issue=LWEVAL-9 -v artifact=11111111-2222-3333-4444-555555555555 -v head=${"7".repeat(40)} -v branch=legion/LWEVAL-9-702 -v job=${job} -v session=ci-results:example/widgets/.github/workflows/ci-results.yaml`;
    expect(read("calls").split("\n").filter(Boolean)).toEqual([call, call]);
    const sql = read("sql");
    expect(sql).toContain("insert into ci_check_results");
    expect(sql).toContain("where b.artifact_id = :'artifact'::uuid and b.block_id = 'k1'");
    expect(sql).toMatch(
      /\('pass', 'Attempt 1 of ' \|\| :'job' \|\| ' passed[^']*', interval '2 minutes'\)/
    );
    expect(sql).toMatch(
      /\('fail', 'Attempt 2 of ' \|\| :'job' \|\| ' failed[^']*', interval '0 seconds'\)/
    );
  });

  // Catches: a world built with no CI result, which would read as the not_completed case alone.
  test("a block that is never indexed fails the run after 60 s, naming the issue", () => {
    const { code, stderr, read } = written(Array.from({ length: 31 }, () => "0"));
    expect(code).toBe(1);
    expect(stderr).toContain("held no block k1 within 60 s");
    expect(read("sleeps").split("\n").filter(Boolean)).toHaveLength(30);
  });
});

// The world of the heads scenarios: fixture_repo's bare remote, source repository and workspace,
// and heads_fixture's two pull request branches of one issue on it. worker_fixture, the
// tester-proof world, builds on the same fixture_repo, and keeps its world as it was.
describe("heads_fixture", () => {
  function build(fixture: "heads_fixture" | "worker_fixture") {
    const run = freshDir(`${fixture}-run`);
    const result = bash(
      [
        "set -euo pipefail",
        ...["fail", "g", "fixture_repo", "workspace_on", fixture].map(fn),
        `R=$1 worker_key=LWEVAL-9 ${WORLD_VARS}`,
        `${fixture} 3 1700000000`,
      ].join("\n"),
      [run]
    );
    return { ...result, run };
  }
  const git = (dir: string, ...args: string[]) => {
    const result = spawnWithin(
      { ...VCS_COMMAND, part: `git ${args.join(" ")}` },
      ["git", "-C", dir, ...args],
      {
        env: childEnv,
      }
    );
    if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr}`);
    return result.stdout.toString().trim();
  };
  /** The commit the workspace's working copy stands on, as jj reads it with the fixture's own
   * configuration. */
  const workspaceParent = (run: string) =>
    spawnWithin(
      { ...VCS_COMMAND, part: "jj log" },
      ["jj", "log", "--no-graph", "-r", "@-", "-T", "commit_id"],
      {
        cwd: path.join(run, "ws"),
        env: { ...childEnv, JJ_CONFIG: path.join(run, "jj-setup.toml") },
      }
    )
      .stdout.toString()
      .trim();

  // Catches: one branch where the case needs two heads, a head that is no commit of the remote, a
  // commit that is not one above main or that changes code (the reviewer then runs its
  // thermonuclear pair), a fixture that ignores the run's index and start, a workspace left on
  // main, and a world that names another pull request than #702.
  test("two branches of the issue, each one docs-only commit above main, and the world of #702", () => {
    const { code, stderr, run } = build("heads_fixture");
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    const remote = path.join(run, "remote.git");
    const heads = JSON.parse(readFileSync(path.join(run, "heads.json"), "utf8")) as Record<
      string,
      string
    >;
    expect(Object.keys(heads).toSorted()).toEqual(["702", "703"]);
    expect(heads["702"]).not.toBe(heads["703"]);
    const main = git(remote, "rev-parse", "main");
    for (const [pr, head] of Object.entries(heads)) {
      expect(head).toMatch(/^[0-9a-f]{40}$/);
      expect(git(remote, "rev-parse", `legion/LWEVAL-9-${pr}`)).toBe(head);
      expect(git(remote, "rev-parse", `${head}^`)).toBe(main);
      expect(git(remote, "rev-list", "--count", `main..${head}`)).toBe("1");
      const changed = git(remote, "diff", "--name-only", main, head).split("\n");
      expect(changed.length).toBeGreaterThan(0);
      expect(changed.filter((file) => !file.endsWith(".md"))).toEqual([]);
      expect(git(remote, "log", "-1", "--format=%at %ct", head)).toBe("1700000003 1700000003");
    }
    expect(JSON.parse(readFileSync(path.join(run, "world.json"), "utf8"))).toEqual({
      key: "LWEVAL-9",
      repo: "example/widgets",
      pr: 702,
      branch: "legion/LWEVAL-9-702",
      head: heads["702"],
    });
    expect(workspaceParent(run)).toBe(heads["702"]);
    expect(readFileSync(path.join(run, "pushes.log"), "utf8")).toBe("");
  });

  // Catches: a move of the shared part into fixture_repo that changes the tester's world: its
  // branch, its code commit, its PR number or its workspace.
  test("worker_fixture with the same inputs keeps legion/LWEVAL-9 one commit above main with greet.ts, and pr 1003", () => {
    const { code, stderr, run } = build("worker_fixture");
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    const remote = path.join(run, "remote.git");
    const head = git(remote, "rev-parse", "legion/LWEVAL-9");
    expect(git(remote, "rev-parse", `${head}^`)).toBe(git(remote, "rev-parse", "main"));
    expect(git(remote, "rev-list", "--count", `main..${head}`)).toBe("1");
    expect(git(remote, "diff", "--name-only", "main", head).split("\n")).toEqual([
      "greet.test.ts",
      "greet.ts",
    ]);
    expect(git(remote, "show", `${head}:greet.ts`)).toContain(
      "export function greet(name: string): string {"
    );
    expect(JSON.parse(readFileSync(path.join(run, "world.json"), "utf8"))).toEqual({
      key: "LWEVAL-9",
      repo: "example/widgets",
      pr: 1003,
      branch: "legion/LWEVAL-9",
      head,
    });
    expect(workspaceParent(run)).toBe(head);
    expect(readFileSync(path.join(run, "pushes.log"), "utf8")).toBe("");
  });

  /** fix_fixture's world of `world`, then `lines` run in it, with the capture functions loaded. */
  function fixWorld(world: string, lines: string[] = []) {
    const run = freshDir(`fix-fixture-${world}`);
    const result = bash(
      [
        "set -euo pipefail",
        ...[
          "fail",
          "g",
          "fixture_repo",
          "workspace_on",
          "beside_branch",
          "fix_fixture",
          "workspace_state",
          "fix_commits",
        ].map(fn),
        `R=$1 worker_key=LWEVAL-9 ${WORLD_VARS}`,
        `fix_fixture 3 1700000000 ${world}`,
        ...lines,
      ].join("\n"),
      [run]
    );
    return { ...result, run };
  }
  /** The commit the working copy at `dir` stands on, as jj reads it with the fixture's config. */
  const parentIn = (run: string, dir: string) =>
    spawnWithin(
      { ...VCS_COMMAND, part: "jj log" },
      ["jj", "log", "--no-graph", "-r", "@-", "-T", "commit_id"],
      { cwd: path.join(run, dir), env: { ...childEnv, JJ_CONFIG: path.join(run, "jj-setup.toml") } }
    )
      .stdout.toString()
      .trim();

  // Catches: a fix's world whose change is not one commit on the issue's own branch over a base
  // that holds the package it does not touch, a fix's own working copy that is missing or not at
  // the head, a tester's world that gets one, and a world that names no branch for the role beside
  // the fix.
  test("fix_fixture: the change on legion/<issue> over a base with packages/notify, and the fix's own copy at the head", () => {
    for (const [world, role] of [
      ["unrelated-test", "implementer"],
      ["touched-test", "implementer"],
      ["tester-beside", "tester"],
    ] as const) {
      const { code, stderr, run } = fixWorld(world);
      expect({ world, code, stderr }).toEqual({ world, code: 0, stderr: "" });
      const remote = path.join(run, "remote.git");
      const head = git(remote, "rev-parse", "legion/LWEVAL-9");
      expect(git(remote, "rev-parse", `${head}^`)).toBe(git(remote, "rev-parse", "main"));
      expect(git(remote, "diff", "--name-only", "main", head).split("\n")).toEqual([
        "packages/greet/greet.test.ts",
        "packages/greet/greet.ts",
      ]);
      expect(git(remote, "ls-tree", "-r", "--name-only", "main").split("\n")).toContain(
        "packages/notify/notify.test.ts"
      );
      expect(JSON.parse(readFileSync(path.join(run, "heads.json"), "utf8"))).toEqual({
        "702": head,
      });
      expect(JSON.parse(readFileSync(path.join(run, "world.json"), "utf8"))).toEqual({
        key: "LWEVAL-9",
        repo: "example/widgets",
        pr: 702,
        branch: "legion/LWEVAL-9",
        head,
        own: `legion-beside/LWEVAL-9/1/default/fix-1/${role}`,
      });
      expect(workspaceParent(run)).toBe(head);
      if (role === "implementer") expect(parentIn(run, "ws-fix")).toBe(head);
      else expect(existsSync(path.join(run, "ws-fix"))).toBe(false);
      expect(readFileSync(path.join(run, "pushes.log"), "utf8")).toBe("");
    }
  });

  // Catches: a record of the shared working copy that a write in it leaves unchanged, a read that
  // itself changes it, and a list of the fix's commits that leaves out a commit made in its copy or
  // counts its empty working copy.
  test("workspace_state shows a write in the shared copy, and fix_commits each commit of the fix's copy", () => {
    const { code, stderr, stdout, run } = fixWorld("touched-test", [
      'head=$(jq -r .head "$R/world.json")',
      'first=$(workspace_state) again=$(workspace_state) none=$(fix_commits "$head")',
      'printf "fix\\n" >"$R/ws-fix/packages/greet/fix.txt"',
      '(cd "$R/ws-fix" && JJ_CONFIG=$R/jj-setup.toml jj commit -m fix) >/dev/null 2>&1',
      'made=$(fix_commits "$head")',
      'printf "x\\n" >"$R/ws/scratch.txt"',
      "written=$(workspace_state)",
      'jq -cn --argjson first "$first" --argjson again "$again" --argjson none "$none" --argjson made "$made" --argjson written "$written" \'{$first, $again, $none, $made, $written}\'',
    ]);
    expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
    const read = JSON.parse(stdout);
    expect(read.again).toEqual(read.first);
    expect(read.none).toEqual([]);
    expect(read.made).toEqual([parentIn(run, "ws-fix")]);
    expect(read.written.commit).toBe(read.first.commit);
    expect(read.written.files).not.toBe(read.first.files);
  });

  // Catches: a ci-pass-and-fail world whose GitHub shows k1's job as passed (the agent would then
  // see CI green beside Dispatch's not_completed), a body line that is not the one Dispatch printed,
  // a second pull request, and an approval on #702 for any role but the architect.
  test("heads_routes of ci-pass-and-fail serve #702 alone, its go-checks run in progress, and the architect's approval", () => {
    const { code, run } = build("heads_fixture");
    expect(code).toBe(0);
    const head = JSON.parse(readFileSync(path.join(run, "heads.json"), "utf8"))["702"];
    writeFileSync(
      path.join(run, "fixture.json"),
      JSON.stringify({ issue: "LWEVAL-9", title: "A usage page" })
    );
    const prLine = "- Check k1 (before_merge): CI run go-checks has no result (not_completed)";
    writeFileSync(
      path.join(run, "results-before.txt"),
      `Checks of LWEVAL-9:\n\n- k1 (before_merge): its CI job's results below.\n  A pass stands: not known, as 0 of 1 heads read yes.\n  - #702 at ${head}: not known, as pass p1 and fail f1 count at this commit.\n    Pull request line: ${prLine}\n`
    );
    const routes = (role: string) => {
      const result = bash(
        [
          "set -euo pipefail",
          fn("fail"),
          fn("pr_line"),
          fn("heads_routes"),
          `R=$1 worker_key=LWEVAL-9 ${WORLD_VARS}`,
          'heads_routes ci-pass-and-fail "$2"',
        ].join("\n"),
        [run, role]
      );
      expect({ code: result.code, stderr: result.stderr }).toEqual({ code: 0, stderr: "" });
      const all: { match: string; stdout?: unknown }[] = JSON.parse(
        readFileSync(path.join(run, "fixtures.json"), "utf8")
      ).routes;
      const answer = (match: string) => all.find((route) => route.match === match)?.stdout;
      return { all, answer };
    };
    const reviewer = routes("reviewer");
    // gh prints a check run's own link, the job's page in its workflow run (rig-world.json's run 41
    // and check run 9101 for go-checks, as Dispatch's stand-in serves them).
    expect(reviewer.answer("^gh pr checks")).toBe(
      "lint\tpass\t10s\thttps://github.com/example/widgets/actions/runs/1\ntest\tpass\t10s\thttps://github.com/example/widgets/actions/runs/1\ngo-checks\tpending\t0\thttps://github.com/example/widgets/actions/runs/41/job/9101"
    );
    expect(reviewer.answer(`^gh api .*commits/${head}/check-runs`)).toMatchObject({
      total_count: 3,
      check_runs: [
        { name: "lint" },
        { name: "test" },
        { name: "go-checks", status: "in_progress", conclusion: null },
      ],
    });
    const pr = reviewer.answer("^gh pr view") as { body: string; statusCheckRollup: unknown[] };
    expect(pr.body).toContain(`**Checks:**\n${prLine}\n`);
    expect(pr.statusCheckRollup).toContainEqual({
      name: "go-checks",
      status: "IN_PROGRESS",
      conclusion: "",
    });
    expect((reviewer.answer("^gh pr list") as unknown[]).length).toBe(1);
    expect(reviewer.answer("^gh api .*pulls/702/reviews")).toEqual([]);
    expect(routes("tester").answer("^gh api .*pulls/702/reviews")).toEqual([]);
    expect(routes("architect").answer("^gh api .*pulls/702/reviews")).toMatchObject([
      { state: "APPROVED", commit_id: head },
    ]);
  });

  // Catches: a world of a failed CI check whose GitHub shows k2's job passed where Dispatch reads it
  // failed (or failed where the case says it passed), a job whose log no read answers, a body that
  // carries k1's line alone, and an architect's world of the other head with #703 left out.
  test("heads_routes of the ci-check-fail worlds serve k2's job where it failed, its log, and both checks' lines", () => {
    const { code, run } = build("heads_fixture");
    expect(code).toBe(0);
    const heads = JSON.parse(readFileSync(path.join(run, "heads.json"), "utf8"));
    const k1Line = (pr: string) =>
      `- Check k1 (before_merge): pass at ${String(heads[pr]).slice(0, 12)} (tester) on #${pr}`;
    const k2Line = (pr: string, verdict: string) =>
      `- Check k2 (before_merge): ${verdict} at ${String(heads[pr]).slice(0, 12)} (CI run usage-test) on #${pr}`;
    writeFileSync(
      path.join(run, "fixture.json"),
      JSON.stringify({
        issue: "LWEVAL-9",
        title: "A usage page",
        lines: { "702": k1Line("702"), "703": k1Line("703") },
      })
    );
    const routes = (scenario: string, scenarioCase: string, k2At: Record<string, string>) => {
      writeFileSync(
        path.join(run, "results-before.txt"),
        [
          "Checks of LWEVAL-9:",
          "",
          "- k1 (before_merge). A pass stands: yes.",
          ...["702", "703"].flatMap((pr) => [
            `  - #${pr} at ${heads[pr]}: yes, from implementer, tester.`,
            `    Pull request line: ${k1Line(pr)}`,
          ]),
          "- k2 (before_merge): its CI job's results below.",
          ...Object.entries(k2At).flatMap(([pr, verdict]) => [
            `  - #${pr} at ${heads[pr]}: ${verdict === "pass" ? "yes" : "no, as fail f9 counts at this commit."}`,
            `    Pull request line: ${k2Line(pr, verdict)}`,
          ]),
          "",
        ].join("\n")
      );
      const result = bash(
        [
          "set -euo pipefail",
          fn("fail"),
          fn("pr_line"),
          fn("heads_routes"),
          `R=$1 worker_key=LWEVAL-9 ${WORLD_VARS}`,
          'heads_routes "$2" "$3"',
        ].join("\n"),
        [run, scenario, scenarioCase]
      );
      expect({ code: result.code, stderr: result.stderr }).toEqual({ code: 0, stderr: "" });
      const all: { match: string; stdout?: unknown; exit?: number }[] = JSON.parse(
        readFileSync(path.join(run, "fixtures.json"), "utf8")
      ).routes;
      const route = (match: string) => all.find((each) => each.match === match);
      return { all, route };
    };
    const failedJob = RIG_WORLD.ciJobs["ci-check-fail"];
    const passedJob = RIG_WORLD.ciJobs["ci-check-pass"];
    const jobLine = (job: { name: string; run: number; checkRun: number }, state: string) =>
      `${job.name}\t${state}\t10s\thttps://github.com/example/widgets/actions/runs/${job.run}/job/${job.checkRun}`;
    const logOf = (id: number) => `^gh run view (?=.*--job[ =]${id}\\b)(?=.*--log)`;

    const reviewer = routes("reviewer-own-head", "ci-check-fail", { "702": "fail" });
    expect(String(reviewer.route("^gh pr checks")?.stdout)).toContain(jobLine(failedJob, "fail"));
    expect(reviewer.route("^gh pr checks")?.exit).toBe(1);
    expect(reviewer.route(logOf(failedJob.checkRun))?.stdout).toBe(failedJob.log);
    const body = (reviewer.route("^gh pr view")?.stdout as { body: string }).body;
    expect(body).toContain(`**Checks:**\n${k1Line("702")}\n${k2Line("702", "fail")}\n`);

    const other = routes("reviewer-own-head", "ci-check-fail-other-head", {
      "702": "pass",
      "703": "fail",
    });
    expect(String(other.route("^gh pr checks")?.stdout)).toContain(jobLine(passedJob, "pass"));
    expect(other.route("^gh pr checks")?.exit).toBe(0);
    expect(String(other.route("^gh pr checks .*\\b703\\b")?.stdout)).toContain(
      jobLine(failedJob, "fail")
    );
    expect(other.route(logOf(failedJob.checkRun))?.stdout).toBe(failedJob.log);

    const architect = routes("architect-pass-and-fail", "ci-check-fail-other-head", {
      "702": "pass",
      "703": "fail",
    });
    expect((architect.route("^gh pr list")?.stdout as unknown[]).length).toBe(2);
    expect(architect.route("^gh api .*pulls/702/reviews")?.stdout).toMatchObject([
      { state: "APPROVED", commit_id: heads["702"] },
    ]);
    const alone = routes("architect-pass-and-fail", "ci-check-fail", { "702": "fail" });
    expect((alone.route("^gh pr list")?.stdout as unknown[]).length).toBe(1);
    expect(String(alone.route("^gh pr checks")?.stdout)).toContain(jobLine(failedJob, "fail"));
  });

  // Each GitHub read the heads scenarios' agents made in the fifth round of runs and since, one
  // call of each endpoint in the form an agent wrote it, and the reads the role prompts and skills
  // name. The round's agents named two repositories, since the pane's addressing named another than
  // the world's; the addressing now reads rig-world.json, so each call names the world's.
  const ROUND_CALLS = [
    "gh --version",
    "gh auth status",
    "gh repo view example/widgets --json defaultBranchRef --jq .defaultBranchRef.name",
    "gh api repos/example/widgets --jq {full_name,default_branch}",
    "gh api repos/example/widgets/branches --jq .[].name",
    "gh api -i repos/example/widgets/branches/main",
    "gh api repos/example/widgets/branches/main/protection/required_status_checks",
    "gh api repos/example/widgets/rulesets",
    'gh api --paginate repos/example/widgets/rules/branches/main --jq .[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context',
    "gh api --paginate repos/example/widgets/issues/702/comments --jq .[] | [.user.login,.body] | @tsv",
    "gh api repos/example/widgets/pulls/702/comments",
    "gh api --paginate repos/example/widgets/pulls/703/reviews --jq .[] | [.id,.state,.user.login] | @tsv",
    "gh api repos/example/widgets/pulls/702 --jq {head:.head.sha,base:.base.ref,mergeable_state}",
    "gh api repos/example/widgets/pulls/702/commits --jq .[] | [.sha, .commit.message] | @tsv",
    "gh api repos/example/widgets/actions/runs?head_sha={702}&per_page=100 --jq .workflow_runs[] | [.id, .name, .event, .status, .conclusion] | @tsv",
    "gh api repos/example/widgets/actions/runs/1/jobs --jq .jobs[] | [.id, .name, .status, .conclusion] | @tsv",
    "gh api repos/example/widgets/commits/{703}/check-runs --paginate --jq .check_runs[] | [.name, .status, .conclusion, .started_at] | @tsv",
    "gh api --paginate repos/example/widgets/commits/{702}/status?per_page=100 --jq .statuses[] | [.context, .state] | @tsv",
    "gh api repos/example/widgets/git/ref/heads/legion/LWEVAL-9-702 --jq .object.sha",
    "gh api repos/example/widgets/compare/main...legion/LWEVAL-9-703 --jq .ahead_by",
    'gh api graphql -f query=query{repository(owner:"example",name:"widgets"){pullRequest(number:703){mergeStateStatus mergeable baseRef{branchProtectionRule{requiredStatusCheckContexts}}}}}',
    "gh api graphql --paginate -f query=query($o:String!,$r:String!,$n:Int!,$endCursor:String){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved}}}}} -F o=example -F r=widgets -F n=702",
    "gh pr view 703 --repo example/widgets --json number,headRefOid,mergeStateStatus,statusCheckRollup,isCrossRepository",
    "gh pr diff 702 --repo example/widgets",
    "gh pr checks 702 -R example/widgets --required",
    "gh pr list --repo example/widgets --state open --json number,headRefName,headRefOid",
  ];
  // Catches: a world with no route for a GitHub read an agent makes, which the stand-in used to
  // answer `gh: Not Found (HTTP 404)`, as if GitHub held no such thing: the fifth round's reviewers
  // read the base's branch so, took it for a refusal and waited, and that measured the rig. And a
  // stand-in that answers a read no route names in any way an agent can take for GitHub's.
  test("heads_routes answer each GitHub read of the round as GitHub does, and a read they lack fails the run loudly", () => {
    const { code, run } = build("heads_fixture");
    expect(code).toBe(0);
    const heads = JSON.parse(readFileSync(path.join(run, "heads.json"), "utf8"));
    writeFileSync(
      path.join(run, "fixture.json"),
      JSON.stringify({
        issue: "LWEVAL-9",
        title: "A usage page",
        lines: { "702": "- Check k1 (before_merge): pass at the head by implementer, tester" },
      })
    );
    const made = bash(
      [
        "set -euo pipefail",
        fn("fail"),
        fn("pr_line"),
        fn("heads_routes"),
        `R=$1 worker_key=LWEVAL-9 ${WORLD_VARS}`,
        "heads_routes reviewer-own-head sibling-fails",
      ].join("\n"),
      [run]
    );
    expect({ code: made.code, stderr: made.stderr }).toEqual({ code: 0, stderr: "" });
    const gh = (line: string) => {
      const argv =
        line
          .replace(/\{(70[23])\}/g, (_, pr: string) => heads[pr])
          .match(/'[^']*'|\S+/g)
          ?.slice(1)
          .map((arg) => arg.replace(/^'|'$/g, "")) ?? [];
      // A jq program of several words is one argument, as the agent's shell quoted it.
      const jq = argv.findIndex((arg) => arg === "--jq" || arg === "-q");
      const args = jq < 0 ? argv : [...argv.slice(0, jq + 1), argv.slice(jq + 1).join(" ")];
      const result = spawnWithin(
        { ...SCRIPT_RUN, part: "gh-standin.ts" },
        ["bun", path.join(import.meta.dir, "gh-standin.ts"), "gh", ...args],
        { env: { ...childEnv, SKILL_SCENARIO_RUN: run } }
      );
      return {
        code: result.exitCode,
        stdout: result.stdout.toString(),
        stderr: result.stderr.toString(),
      };
    };
    for (const call of ROUND_CALLS) {
      const { code, stderr } = gh(call);
      expect({ call, noRoute: code === 97 || stderr.includes("STAND-IN HAS NO ROUTE") }).toEqual({
        call,
        noRoute: false,
      });
    }
    expect(existsSync(path.join(run, "no-route.log"))).toBe(false);
    // A pull request's commits, as GitHub lists them: its one commit, the head on the base. A
    // reviewer of reviewer-own-head read #702's so, where the stand-in had no route for it.
    expect(
      gh("gh api repos/example/widgets/pulls/702/commits --jq .[] | [.sha, .parents[0].sha] | @tsv")
    ).toEqual({
      code: 0,
      stdout: `${heads["702"]}\t${git(path.join(run, "remote.git"), "rev-parse", "main")}\n`,
      stderr: "",
    });
    // A pull request's files, as GitHub lists them: each path, its status and its patch, the hunks
    // of that path from the first @@ line. A reviewer of reviewer-own-head read #703's so, where the
    // stand-in had no route for it.
    const remote = path.join(run, "remote.git");
    const changed = git(remote, "diff", "--name-status", "main", heads["703"])
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => line.split("\t"));
    expect(changed.length).toBeGreaterThan(0);
    const statuses: Record<string, string> = { A: "added", M: "modified", D: "removed" };
    expect(
      gh("gh api repos/example/widgets/pulls/703/files --jq .[] | [.filename, .status] | @tsv")
    ).toEqual({
      code: 0,
      stdout: changed.map(([status, file]) => `${file}\t${statuses[status ?? ""]}\n`).join(""),
      stderr: "",
    });
    for (const [, file] of changed) {
      const diff = git(remote, "diff", "main", heads["703"], "--", file ?? "");
      const patch = diff.slice(diff.indexOf("\n@@") + 1).replace(/\n$/, "");
      expect(
        gh(
          `gh api repos/example/widgets/pulls/703/files --jq .[] | select(.filename == "${file}") | .patch`
        )
      ).toEqual({ code: 0, stdout: `${patch}\n`, stderr: "" });
    }
    expect(
      gh("gh api repos/example/widgets/pulls/703/files --jq .[] | .filename, .patch").code
    ).toBe(0);
    // A check run by its id, as GitHub answers it, and GitHub's 404 for an id no head has a run of.
    // A reviewer of the case ci-only read its named run so, where the stand-in had no route for it.
    const runID = gh(
      "gh api repos/example/widgets/commits/{702}/check-runs --jq .check_runs[0].id"
    ).stdout.trim();
    expect(
      gh(
        `gh api repos/example/widgets/check-runs/${runID} --jq [.id, .name, .head_sha, .conclusion] | @tsv`
      )
    ).toEqual({ code: 0, stdout: `${runID}\tlint\t${heads["702"]}\tsuccess\n`, stderr: "" });
    expect(gh("gh api repos/example/widgets/check-runs/1 --jq .name")).toMatchObject({
      code: 1,
      stderr: "gh: Not Found (HTTP 404)\n",
    });
    // A compare with a commit the repository does not hold: GitHub's 404. An architect of
    // architect-pass-and-fail compared the base with such a commit, where the stand-in had no route
    // for it. The control: the compare of the base with a head still answers.
    expect(
      gh(
        "gh api repos/example/widgets/compare/main...cc14cd790a47ea73926f227bad9e00374e3424d1 --jq {status,ahead_by}"
      )
    ).toMatchObject({ code: 1, stderr: "gh: Not Found (HTTP 404)\n" });
    expect(gh("gh api repos/example/widgets/compare/main...{702} --jq .status")).toEqual({
      code: 0,
      stdout: "ahead\n",
      stderr: "",
    });
    // Each body holds every line of the template's brief that the implementer writes, and the
    // Thermo line's row for each half, as skills/legion-worker/references/pr-body.md has them. A
    // reviewer of reviewer-own-head requested changes on a body that lacked the brief and had one
    // Thermo row, instead of judging the gate.
    const template = readFileSync(
      path.join(root, "skills/legion-worker/references/pr-body.md"),
      "utf8"
    );
    const brief = template.slice(
      template.indexOf("## For the reviewer"),
      template.indexOf("## Verification")
    );
    const briefLines = brief.split("\n");
    const implementerLabels = briefLines.flatMap((line, index) => {
      const label = /^\*\*([^*]+):\*\*/.exec(line)?.[1];
      const reviewers = `${line}\n${briefLines[index + 1] ?? ""}`.includes(
        "the reviewer writes this line"
      );
      return label === undefined || reviewers ? [] : [label];
    });
    expect(implementerLabels).toEqual(["Outcome", "Why", "Change", "Proven by", "Size"]);
    const halves = [...template.matchAll(/^- (`thermonuclear-[a-z-]+`):/gm)].map((m) => m[1]);
    expect(halves.length).toBe(2);
    for (const pr of ["702", "703"]) {
      const body = gh(`gh pr view ${pr} --repo example/widgets --json body --jq .body`).stdout;
      expect({
        pr,
        brief: body.includes("\n## For the reviewer\n"),
        missing: implementerLabels.filter(
          (label) => !body.includes(`\n**${label}:** `) && !body.includes(`\n**${label}:**\n`)
        ),
        rows: halves.map((half) =>
          body.includes(`\n- ${half}: not run (the whole change touches no runtime code)\n`)
        ),
      }).toEqual({ pr, brief: true, missing: [], rows: [true, true] });
    }
    expect(gh("gh pr view 702 --repo example/widgets --json body --jq .body").stdout).toContain(
      "\n**Size:** 1 file changed, +3/−0\n"
    );
    // The reads a reviewer makes of the base, as GitHub answers them for a base with no protection.
    expect(gh("gh api repos/example/widgets/branches/main --jq .protected")).toEqual({
      code: 0,
      stdout: "false\n",
      stderr: "",
    });
    expect(
      gh(
        "gh api repos/example/widgets/branches/main --jq .protection.required_status_checks.checks[].context"
      )
    ).toEqual({ code: 0, stdout: "", stderr: "" });
    expect(gh("gh api repos/example/widgets/branches/main/protection")).toEqual({
      code: 1,
      stdout:
        '{"message":"Resource not accessible by integration","documentation_url":"https://docs.github.com/rest/branches/branch-protection#get-branch-protection","status":"403"}',
      stderr: "gh: Resource not accessible by integration (HTTP 403)\n",
    });
    expect(gh("gh api repos/example/widgets/branches/no-such-branch --jq .name")).toMatchObject({
      code: 1,
      stderr: "gh: Branch not found (HTTP 404)\n",
    });
    expect(gh("gh api -i repos/example/widgets/branches/main").stdout).toStartWith(
      "HTTP/2.0 200 OK\r\n"
    );
    expect(gh("gh pr checks 702 -R example/widgets --required")).toEqual({
      code: 1,
      stdout: "",
      stderr: "no required checks reported on the 'legion/LWEVAL-9-702' branch\n",
    });
    // A review the run posts is GitHub's answer, then in the list and readable by its id.
    writeFileSync(
      path.join(run, "review.json"),
      JSON.stringify({ event: "APPROVE", body: "ok", commit_id: heads["702"] })
    );
    const posted = gh(
      `gh api --method POST repos/example/widgets/pulls/702/reviews --input ${path.join(run, "review.json")} --jq .id`
    );
    expect(posted).toEqual({ code: 0, stdout: "7002\n", stderr: "" });
    expect(
      gh("gh api repos/example/widgets/pulls/702/reviews --jq .[] | [.id,.state,.commit_id] | @tsv")
        .stdout
    ).toBe(`7002\tAPPROVED\t${heads["702"]}\n`);
    expect(gh("gh api repos/example/widgets/pulls/702/reviews/7002 --jq .state").stdout).toBe(
      "APPROVED\n"
    );
    expect(gh("gh api repos/example/widgets/pulls/702/reviews/99 --jq .state")).toMatchObject({
      code: 1,
      stderr: "gh: Not Found (HTTP 404)\n",
    });
    expect(gh("gh pr view 702 --json reviews --jq .reviews[].state").stdout).toBe("APPROVED\n");
    // A repository other than the world's, which the App's token cannot read: GitHub's 404.
    expect(gh("gh api repos/skills-rig/project/branches/main --jq .protected")).toMatchObject({
      code: 1,
      stderr: "gh: Not Found (HTTP 404)\n",
    });
    // A read no route names: the stand-in's own failure, named in the run's log, never an answer.
    expect(gh("gh api repos/example/widgets/planted-unknown-endpoint --jq .")).toEqual({
      code: 97,
      stdout: "",
      stderr:
        "STAND-IN HAS NO ROUTE: gh api repos/example/widgets/planted-unknown-endpoint --jq .\n",
    });
    expect(readFileSync(path.join(run, "no-route.log"), "utf8")).toBe(
      "STAND-IN HAS NO ROUTE: gh api repos/example/widgets/planted-unknown-endpoint --jq .\n"
    );
    const calls = readFileSync(path.join(run, "calls.jsonl"), "utf8").trim().split("\n");
    expect(JSON.parse(calls.at(-1) ?? "{}")).toMatchObject({
      route: null,
      no_route: true,
      exit: 97,
    });
  });

  // Catches: a pane whose addressing names another repository or default branch than the world
  // the stand-ins serve, as the fifth round's did (`skills-rig/project`, `trunk`, where GitHub's
  // stand-in held `example/widgets` on `main`): an agent that reads the repository it was told
  // then reads a world that does not exist. The addressing comes from daemon-pane.go, the world
  // from rig.sh, and both read rig-world.json, so each side is read here and they must agree.
  test("the pane's addressing names the repository and default branch the stand-in's world serves", () => {
    const { code, run } = build("heads_fixture");
    expect(code).toBe(0);
    writeFileSync(
      path.join(run, "fixture.json"),
      JSON.stringify({
        issue: "LWEVAL-9",
        title: "A usage page",
        lines: { "702": "- Check k1 (before_merge): pass at the head by implementer, tester" },
      })
    );
    const made = bash(
      [
        "set -euo pipefail",
        fn("fail"),
        fn("pr_line"),
        fn("heads_routes"),
        `R=$1 worker_key=LWEVAL-9 ${WORLD_VARS}`,
        "heads_routes reviewer-own-head sibling-fails",
      ].join("\n"),
      [run]
    );
    expect({ code: made.code, stderr: made.stderr }).toEqual({ code: 0, stderr: "" });
    const pane = workerPane(
      { rig: run, port: 13399, profile: "rig-profile" },
      { ...childEnv, HOME: path.join(run, "pane-home") },
      { project: "skillsrig", issue: "LWEVAL-9", role: "reviewer" },
      { daemonModule: DAEMON_MODULE, systemPrompt: true, ghPath: process.execPath }
    );
    const promptFile = (pane.systemPromptArgument ?? "")
      .replace(/^--append-system-prompt /, "")
      .replace(/^'(.*)'$/, "$1");
    const told = /Your issue's repository is (\S+) \(default branch ([^)]+)\)/.exec(
      readFileSync(promptFile, "utf8")
    );
    const routes: { match: string; stdout?: unknown }[] = JSON.parse(
      readFileSync(path.join(run, "fixtures.json"), "utf8")
    ).routes;
    const repoRead = routes.find((route) => route.match.includes("/?repos/[^/ ]+/[^/ ?]+([ ?]|$)"))
      ?.stdout as { full_name?: string; default_branch?: string } | undefined;
    const remoteHead = spawnWithin(
      { ...VCS_COMMAND, part: "git symbolic-ref" },
      ["git", "-C", path.join(run, "remote.git"), "symbolic-ref", "--short", "HEAD"],
      { env: childEnv }
    )
      .stdout.toString()
      .trim();
    const world = JSON.parse(readFileSync(path.join(run, "world.json"), "utf8"));
    const named = { repository: RIG_WORLD.repository, defaultBranch: RIG_WORLD.defaultBranch };
    expect({
      addressing: { repository: told?.[1], defaultBranch: told?.[2] },
      githubStandIn: { repository: repoRead?.full_name, defaultBranch: repoRead?.default_branch },
      remote: { repository: world.repo, defaultBranch: remoteHead },
    }).toEqual({ addressing: named, githubStandIn: named, remote: named });
  }, 600_000);

  // Catches: a named CI job that the two GitHub worlds of one run number apart. Dispatch's stand-in
  // (heads-world.ts) served docs as check run 9001 of run 40, the agent's gh stand-in served it as
  // 8002 of run 1, and no route answered a read of job 9001: a ci-only reviewer read the id that
  // `dispatch results` printed, and the run was a rig error.
  for (const [scenario, scenarioCase, world] of [
    ["reviewer-own-head", "ci-only", "ci-only"],
    ["ci-pass-and-fail", "reviewer", "ci-pass-and-fail"],
  ] as const) {
    test(`${scenario} ${scenarioCase}: the named CI job is the check run, workflow run and suite rig-world.json names, and a read of it by its id answers`, () => {
      const { code, run } = build("heads_fixture");
      expect(code).toBe(0);
      writeFileSync(
        path.join(run, "fixture.json"),
        JSON.stringify({
          issue: "LWEVAL-9",
          title: "A usage page",
          lines: { "702": "- Check k1 (before_merge): pass at the head by implementer, tester" },
        })
      );
      // The rig's read of the results before the agent starts: a named job's Pull request line.
      const head702 = JSON.parse(readFileSync(path.join(run, "heads.json"), "utf8"))["702"];
      writeFileSync(
        path.join(run, "results-before.txt"),
        `Checks of LWEVAL-9:\n\n- k1 (before_merge): its CI job's results below.\n  - #702 at ${head702}: yes\n    Pull request line: - Check k1 (before_merge): pass at ${head702} by ci\n`
      );
      const made = bash(
        [
          "set -euo pipefail",
          fn("fail"),
          fn("pr_line"),
          fn("heads_routes"),
          `R=$1 worker_key=LWEVAL-9 ${WORLD_VARS}`,
          `heads_routes ${scenario} ${scenarioCase}`,
        ].join("\n"),
        [run]
      );
      expect({ code: made.code, stderr: made.stderr }).toEqual({ code: 0, stderr: "" });
      const job = RIG_WORLD.ciJobs[world];
      const heads = JSON.parse(readFileSync(path.join(run, "heads.json"), "utf8"));
      const gh = (...args: string[]) => {
        const result = spawnWithin(
          { ...SCRIPT_RUN, part: "gh-standin.ts" },
          ["bun", path.join(import.meta.dir, "gh-standin.ts"), "gh", ...args],
          { env: { ...childEnv, SKILL_SCENARIO_RUN: run } }
        );
        return {
          code: result.exitCode,
          stdout: result.stdout.toString(),
          stderr: result.stderr.toString(),
        };
      };
      const done = job.status === "completed";
      expect(
        gh(
          "api",
          `repos/example/widgets/actions/jobs/${job.checkRun}`,
          "--jq",
          "[.id,.run_id,.name,.status]|@tsv"
        )
      ).toEqual({
        code: 0,
        stdout: `${job.checkRun}\t${job.run}\t${job.name}\t${job.status}\n`,
        stderr: "",
      });
      expect(
        gh(
          "api",
          `repos/example/widgets/commits/${heads["702"]}/check-runs`,
          "--jq",
          `.check_runs[] | select(.name == "${job.name}") | [.id,.run_id,.check_suite.id,.html_url]|@tsv`
        ).stdout
      ).toBe(
        `${job.checkRun}\t${job.run}\t${job.suite}\thttps://github.com/example/widgets/actions/runs/${job.run}/job/${job.checkRun}\n`
      );
      expect(
        gh("api", `repos/example/widgets/actions/runs/${job.run}`, "--jq", "[.name,.status]|@tsv")
          .stdout
      ).toBe(`${job.name}\t${job.status}\n`);
      expect(gh("pr", "checks", "702").code).toBe(done ? 0 : 8);
      expect(gh("api", "repos/example/widgets/actions/jobs/1234567", "--jq", ".id")).toMatchObject({
        code: 1,
        stderr: "gh: Not Found (HTTP 404)\n",
      });
      expect(existsSync(path.join(run, "no-route.log"))).toBe(false);
    }, 600_000);
  }

  // Catches: a world with no route for a read of a file at a head, which this round's architects
  // made with Dispatch's state hidden (`contents/docs/usage.md?ref=<head>`, `git/trees/<head>`),
  // and which the stand-in failed loudly as a rig error: each read answers as GitHub answers it, a
  // path no commit holds is GitHub's 404, and each file's bytes are the commit's own.
  test("heads_routes answer the reads of a head's files as GitHub does", () => {
    const { code, run } = build("heads_fixture");
    expect(code).toBe(0);
    writeFileSync(
      path.join(run, "fixture.json"),
      JSON.stringify({
        issue: "LWEVAL-9",
        title: "A usage page",
        lines: { "702": "- Check k1 (before_merge): pass at the head by implementer, tester" },
      })
    );
    const made = bash(
      [
        "set -euo pipefail",
        fn("fail"),
        fn("pr_line"),
        fn("heads_routes"),
        `R=$1 worker_key=LWEVAL-9 ${WORLD_VARS}`,
        "heads_routes architect-pass-and-fail",
      ].join("\n"),
      [run]
    );
    expect({ code: made.code, stderr: made.stderr }).toEqual({ code: 0, stderr: "" });
    const head = JSON.parse(readFileSync(path.join(run, "heads.json"), "utf8"))["702"];
    const gitShow = (spec: string) =>
      spawnWithin(
        { ...VCS_COMMAND, part: "git show" },
        ["git", "-C", path.join(run, "remote.git"), "show", spec],
        {
          env: childEnv,
        }
      ).stdout.toString();
    const usage = gitShow(`${head}:docs/usage.md`);
    expect(usage.length).toBeGreaterThan(0);
    const gh = (...args: string[]) => {
      const result = spawnWithin(
        { ...SCRIPT_RUN, part: "gh-standin.ts" },
        ["bun", path.join(import.meta.dir, "gh-standin.ts"), "gh", ...args],
        { env: { ...childEnv, SKILL_SCENARIO_RUN: run } }
      );
      return {
        code: result.exitCode,
        stdout: result.stdout.toString(),
        stderr: result.stderr.toString(),
      };
    };
    const content = gh(
      "api",
      `repos/example/widgets/contents/docs/usage.md?ref=${head}`,
      "--jq",
      ".content"
    );
    expect(content.code).toBe(0);
    expect(Buffer.from(content.stdout.replace(/\n/g, ""), "base64").toString()).toBe(usage);
    expect(
      gh(
        "api",
        "-H",
        "Accept: application/vnd.github.raw",
        `repos/example/widgets/contents/docs/usage.md?ref=${head}`
      ).stdout
    ).toBe(usage);
    expect(
      gh("api", "repos/example/widgets/contents/docs?ref=legion/LWEVAL-9-702", "--jq", ".[].name")
        .stdout
    ).toBe("usage.md\n");
    expect(
      gh(
        "api",
        `repos/example/widgets/git/trees/${head}?recursive=1`,
        "--jq",
        ".tree[].path"
      ).stdout.split("\n")
    ).toContain("docs/usage.md");
    expect(gh("api", `repos/example/widgets/commits/${head}`, "--jq", ".sha").stdout).toBe(
      `${head}\n`
    );
    expect(
      gh("api", `repos/example/widgets/contents/no/such/file.md?ref=${head}`, "--jq", ".content")
    ).toMatchObject({
      code: 1,
      stderr: "gh: Not Found (HTTP 404)\n",
    });
    expect(existsSync(path.join(run, "no-route.log"))).toBe(false);
  }, 600_000);
});
