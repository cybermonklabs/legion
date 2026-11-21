import { beforeAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import {
  appendFileSync,
  chmodSync,
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { argMaxForceHolds } from "../../../../scripts/e2e/lib/arg-max-force";
import { ownGroup, untilRunning } from "../../../../scripts/e2e/lib/own-group";
import { procRootOf } from "../../../../scripts/e2e/lib/proc-root";
import { scriptFunctions } from "../../../../scripts/e2e/lib/script-functions";
import {
  baseOf,
  RED_KEY,
  RED_VARIANT_FACTS,
  RED_VARIANT_WORLDS,
  RED_VARIANTS,
  RED_WORLDS,
  redWorlds,
} from "./red-at-the-head.fixtures";
import { RIG_FILES, stageRig } from "./rig-files";

// Every test here runs rig.sh and its stand-ins as real processes. On a loaded machine one test
// takes longer than bun's 5 s default (the eight-turn conversation took 11 s), so each test gets
// one minute.
setDefaultTimeout(60_000);

// The judge runs in a Docker container; every other row here needs no container. Read once, so
// one warning names docker for the whole file.
const withoutDocker = skipWithout("docker");

// rig.sh's install_model_route, run with the checkout's real installers: the route a label's
// profile reaches Anthropic by, picked by LEGION_E2E_MODEL_ROUTE.
const root = path.resolve(import.meta.dir, "../../../..");
const fn = scriptFunctions(path.join(import.meta.dir, "rig.sh"));
const work = temporaryDirectory("skill-scenarios-rig");
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
 * decides nothing here. PUBLIC_LEAK_CHECK and LEGION_LEAK_CHECK_TAG go through when the caller
 * has them: a row that builds a world through worker_fixture pushes into its bare remote, and on a
 * Legion pod that push goes through the pod's git wrapper, whose pre-push leak guard reads both. */
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
  ...(process.env.PUBLIC_LEAK_CHECK === undefined
    ? {}
    : { PUBLIC_LEAK_CHECK: process.env.PUBLIC_LEAK_CHECK }),
  ...(process.env.LEGION_LEAK_CHECK_TAG === undefined
    ? {}
    : { LEGION_LEAK_CHECK_TAG: process.env.LEGION_LEAK_CHECK_TAG }),
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
  const result = runScript(
    `route_model_id=claude-opus-4-8\n${fn("install_model_route")}\n${fn("install_provider_route")}\ninstall_model_route "$1" skills-test "$2" "$3"`,
    [root, dir, model],
    { ...childEnv, ...env }
  );
  return { code: result.code, stderr: result.stderr };
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
    const served = Bun.spawnSync([path.join(dir, "model-key", "anthropic-key")], { env: childEnv });
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

/** A label's checkout whose plugin installer and home maker do nothing, with the real key-file
 * route. */
function labelCheckout(): string {
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

// The key-file route's key command reads the operator's key for as long as it exists
// (install-key-file-model-route.sh: the caller removes --key-dir on every exit that keeps no
// profile). A setup that stops after the route is installed leaves no profile to use it.
describe("a setup that fails after install_model_route", () => {
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
        "given_omp",
        "cmd_profile",
      ].map(fn),
      "checkout_commit() { printf 'fixture parents=base\\n'; }",
      "install_superpowers() { fail 'the superpowers probe failed'; }",
      'work=$1; cmd_profile probe "$2"',
    ].join("\n");
    const failed = bash(script, [work, labelCheckout()], {
      ...routeEnv(),
      SKILL_SCENARIOS_SUPERPOWERS: "set",
    });
    expect(failed.code).toBe(1);
    expect(failed.stderr).toContain("the superpowers probe failed");
    const P = path.join(work, "profiles/probe");
    expect(existsSync(path.join(P, "home/.omp/profiles/skills-probe/agent/models.yml"))).toBe(true);
    expect(existsSync(path.join(P, "model-key"))).toBe(false);
    expect(existsSync(path.join(P, "ready"))).toBe(false);

    // The same setup that succeeds keeps its key command: a ready profile uses it.
    const kept = freshDir("route-work");
    const ok = bash(
      script.replace("fail 'the superpowers probe failed'", ":"),
      [kept, labelCheckout()],
      {
        ...routeEnv(),
        SKILL_SCENARIOS_SUPERPOWERS: "set",
      }
    );
    expect({ code: ok.code, stderr: ok.stderr }).toEqual({ code: 0, stderr: "" });
    expect(existsSync(path.join(kept, "profiles/probe/model-key/anthropic-key"))).toBe(true);
    expect(existsSync(path.join(kept, "profiles/probe/ready"))).toBe(true);
  });

  // Catches: a judge-profile build that fails at its second profile and keeps the first one's key
  // command.
  test("judge_profiles removes both profiles' model-key and marks nothing ready", () => {
    const work = freshDir("judge-route-work");
    const co = labelCheckout();
    const script = [
      "set -euo pipefail",
      ...["fail", "on_exit", "drop_unready_keys", "install_model_route", "judge_profiles"].map(fn),
      "checkout_commit() { printf 'fixture parents=base\\n'; }",
      "root=$1; work=$2; judge_model=fixture; judge_thinking=high; judge_profiles",
    ].join("\n");
    // The bare profile's home cannot be made, after the ste profile's route is installed.
    appendFileSync(
      path.join(co, "scripts/e2e/lib/omp-home.sh"),
      'make_omp_home() { [ "$(basename "$(dirname "$1")")" != bare ] || { echo "no home for bare" >&2; return 1; }; mkdir -p "$1/.omp"; }\n'
    );
    const failed = bash(script, [co, work], routeEnv());
    expect(failed.code).toBe(1);
    expect(failed.stderr).toContain("make_omp_home failed for the judge's bare profile");
    const dir = path.join(work, "judge-profiles");
    expect(existsSync(path.join(dir, "ste/home/.omp/profiles/judge-ste/agent/models.yml"))).toBe(
      true
    );
    expect(existsSync(path.join(dir, "ste/model-key"))).toBe(false);
    expect(existsSync(path.join(dir, "bare/model-key"))).toBe(false);
    expect(existsSync(path.join(dir, "ready"))).toBe(false);
  });
});

// cmd_profile hands both plugin installs the Oh My Pi the label runs: .omp-pin's, through mise, or
// the binary SKILL_SCENARIOS_OMP names, which install-plugin-profile.sh takes as --omp-binary alone
// (its --omp is a mise tool spec, whatever it looks like).
describe("cmd_profile's Oh My Pi", () => {
  /** cmd_profile of the label `probe` under `env`, with an installer that records its arguments. */
  function profile(env: Record<string, string>) {
    const co = labelCheckout();
    const installs = path.join(co, "installs");
    writeFileSync(
      path.join(co, "scripts/e2e/lib/install-plugin-profile.sh"),
      `printf '%s\\n' "$*" >>${JSON.stringify(installs)}\n`
    );
    const work = freshDir("omp-profile-work");
    const script = [
      "set -euo pipefail",
      ...[
        "fail",
        "label_dir",
        "on_exit",
        "drop_unready_keys",
        "install_model_route",
        "given_omp",
        "cmd_profile",
      ].map(fn),
      "checkout_commit() { printf 'fixture parents=base\\n'; }",
      'work=$1; cmd_profile probe "$2"',
    ].join("\n");
    const result = bash(script, [work, co], { ...routeEnv(), ...env });
    const P = path.join(work, "profiles/probe");
    return {
      code: result.code,
      stderr: result.stderr,
      installs: readFileSync(installs, "utf8").trimEnd().split("\n"),
      flags: ["pi-envoy", "pi-legion"].map(
        (pkg) => `--package ${pkg} --profile skills-probe --home ${P}/home --dest ${P}/${pkg}`
      ),
    };
  }

  // Catches: the binary handed on as --omp, which the installer reads as a mise tool spec, so a
  // pod with no mise fails at `mise install <path>`.
  test("a set SKILL_SCENARIOS_OMP goes to both installs as --omp-binary, and an unset one as nothing", () => {
    const omp = path.join(freshDir("omp-given"), "omp");
    writeFileSync(omp, "#!/bin/sh\n", { mode: 0o755 });
    const given = profile({ SKILL_SCENARIOS_OMP: omp });
    expect({ code: given.code, stderr: given.stderr }).toEqual({ code: 0, stderr: "" });
    expect(given.installs).toEqual(given.flags.map((flags) => `${flags} --omp-binary ${omp}`));
    const pinned = profile({});
    expect({ code: pinned.code, stderr: pinned.stderr }).toEqual({ code: 0, stderr: "" });
    expect(pinned.installs).toEqual(pinned.flags);
  });
});

/** SCRIPT in a file of its own, for `bash FILE ARGS`, never `bash -c SCRIPT`: Linux refuses any
 * one argument over 131,072 bytes (MAX_ARG_STRLEN), and a script of rig.sh's functions grows with
 * rig.sh. builtWorld's reached 133,179 bytes at 2af25c16, and CI's pi-legion job refused it with
 * E2BIG (run 44728976046); a pod cannot show that refusal, since gVisor enforces no such limit.
 * (builtWorld, now red-at-the-head.fixtures.ts's, sources red-at-the-head.sh.) The file is in a
 * directory of its own under tmpdir(), and the caller removes it once bash has exited
 * (removeScript). */
function scriptFile(script: string): string {
  const file = path.join(mkdtempSync(path.join(tmpdir(), "skill-scenarios-script-")), "script.sh");
  writeFileSync(file, script);
  return file;
}
const removeScript = (file: string) => rmSync(path.dirname(file), { recursive: true, force: true });
/** Runs SCRIPT under bash with ARGS as $1... and ENV as its environment, from a file
 * (scriptFile) removed once bash has exited. `wrap` turns the argv into the one spawned
 * (ownGroup). */
function runScript(
  script: string,
  args: string[],
  env: Record<string, string>,
  wrap = (argv: string[]) => argv
) {
  const file = scriptFile(script);
  try {
    const result = Bun.spawnSync(wrap(["bash", file, ...args]), { env });
    return {
      code: result.exitCode,
      stdout: result.stdout.toString(),
      stderr: result.stderr.toString(),
    };
  } finally {
    removeScript(file);
  }
}
/** Runs SCRIPT (rig.sh functions and a call of them) under bash with ARGS as $1..., in an
 * environment of PATH and the test's own HOME. */
function bash(script: string, args: string[] = [], env: Record<string, string> = {}) {
  return runScript(script, args, { ...childEnv, ...env });
}
let dirs = 0;
/** A new directory under the test's work directory. */
function freshDir(name: string): string {
  const dir = path.join(work, `${name}-${++dirs}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}

// INFRA-1303: catches a helper that hands bash its script as one argument, which Linux refuses
// over 131,072 bytes (MAX_ARG_STRLEN): CI run 44728976046 failed builtWorld's 133,179-byte script
// with E2BIG. gVisor enforces no such limit, so this row fails only on a real kernel, as on
// GitHub's runner; a pod cannot show it red.
test("the test's bash runs a script larger than one argument may be", () => {
  const script = `${"# padding\n".repeat(14_000)}printf '%s:%s' "$1" "$2"`;
  expect(Buffer.byteLength(script)).toBeGreaterThan(131_072);
  expect(bash(script, ["a", "b"])).toEqual({ code: 0, stdout: "a:b", stderr: "" });
});

// cmd_profile records the label's commit in built-from with checkout_commit. A git worktree holds
// no .jj, and jj alone printed an empty commit line for it.
describe("checkout_commit", () => {
  const git = (dir: string, ...args: string[]) => {
    const result = Bun.spawnSync(
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
    const result = Bun.spawnSync(["bash", path.join(import.meta.dir, "rig.sh"), "score"], {
      env: { ...childEnv, SKILL_SCENARIOS_WORK: work },
    });
    expect(result.exitCode).toBe(1);
    expect(result.stderr.toString()).toContain(`${top}/.omp`);
    expect(existsSync(work)).toBe(false);
  });
});

// A spec-writing or spec-rewrite batch takes one run, numbered one above the highest run of its
// scenario and label, so a later batch never removes an earlier run (cmd_run starts from rm -rf).
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
  const groupBash = (script: string, args: string[], procRoot: string) =>
    runScript(script, args, { ...childEnv, proc_root: procRoot }, ownGroup);

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
    const run = () =>
      bash(
        `${fn("fail")}\n${fn("given_omp")}\n${fn("pinned_command")}\n${fn("install_superpowers")}\nsuperpowers_skills=(writing-plans brainstorming)\ninstall_superpowers "$1" skills-s1 omp@pin`,
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

// Catches: a copy of the rig a test stages that lacks a file rig.sh sources or its library loads
// at its start, as the copies here and in rig-isolation.test.ts lacked red-at-the-head.sh once
// rig.sh came to source it, so every row on such a copy failed on the missing file and not on
// what it tests. The list is rig-files.ts's, the one every staged copy takes.
test("a staged rig carries every file rig.sh and its library load", () => {
  const loaded = (file: string, dir: string) =>
    [
      ...readFileSync(path.join(import.meta.dir, file), "utf8").matchAll(
        new RegExp(`(?:^\\. |-L )"\\$${dir}/([^"/]+)`, "gm")
      ),
    ].map((match) => match[1] as string);
  const named = [...loaded("rig.sh", "here"), ...loaded("red-at-the-head.sh", "red_dir")];
  expect(named).toEqual(["red-at-the-head.sh", "jq"]);
  for (const file of named) expect(RIG_FILES as readonly string[]).toContain(file);
  const top = freshDir("staged-rig");
  const scripts = path.join(top, "packages/pi-legion/scripts/skill-scenarios");
  mkdirSync(scripts, { recursive: true });
  stageRig(scripts);
  cpSync(path.join(root, "scripts/e2e/lib"), path.join(top, "scripts/e2e/lib"), {
    recursive: true,
  });
  // With no command the rig prints its usage and exits 2, once every file it sources is read.
  const usage = Bun.spawnSync(["bash", path.join(scripts, "rig.sh")], { env: childEnv });
  expect({ code: usage.exitCode, stderr: usage.stderr.toString() }).toEqual({
    code: 2,
    stderr: "",
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
  const judge = (
    kind: string,
    dir: string,
    output = "answer.json",
    env: Record<string, string> = {}
  ) =>
    Bun.spawnSync(["bash", path.join(import.meta.dir, "rig.sh"), "judge-omp", kind, dir, output], {
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
    }
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
    }
  );

  // Catches: a bare judge that still loads skills, or a judge-omp that hides omp's failure.
  test.skipIf(skip)("bare loads no skill, and judge-omp exits with omp's status", () => {
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
  test.skipIf(skip)("one judge cannot leave a file that a second judge can read", () => {
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
      }
    );

    // Catches: a token directory or file that others can read while the judge runs. The modes
    // are the judge's own reading of them from inside the container, recorded while it ran
    // (`recorded(dir, "mode")`), rather than a watcher's from outside: the stand-in omp sleeps
    // for nothing now, so there is no window for a watcher to look through, and a row that
    // waited for one would be waiting on a sleep.
    test.skipIf(skip)("the token file is the owner's alone while the judge runs", () => {
      const dir = gatewayDir();
      const result = gatewayJudge("bare", dir);
      expect(judged(result)).toEqual(judgedCleanly);
      expect(recorded(dir, "mode")).toEqual(["key-dir 700", "token 600"]);
      expect(existsSync(`${dir}.key`)).toBe(false);
    });

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
      }
    );
  });

  // Catches: a judge profile whose built-from records an empty commit line when the rig's root is
  // no checkout, so a judge's model run cannot be traced to the code that built it.
  test.skipIf(skip)("fails, naming the rig's root, when the root is no checkout", () => {
    const top = freshDir("judge-root");
    const scripts = path.join(top, "packages/pi-legion/scripts/skill-scenarios");
    mkdirSync(scripts, { recursive: true });
    stageRig(scripts);
    for (const file of [".omp-pin", "skills/asd-ste100/SKILL.md"]) {
      mkdirSync(path.dirname(path.join(top, file)), { recursive: true });
      writeFileSync(path.join(top, file), readFileSync(path.join(root, file)));
    }
    mkdirSync(path.join(top, "scripts/e2e"), { recursive: true });
    cpSync(path.join(root, "scripts/e2e/lib"), path.join(top, "scripts/e2e/lib"), {
      recursive: true,
    });
    const rootWork = freshDir("judge-root-work");
    const result = Bun.spawnSync(
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

// launch with a stand-in tmux that starts the pane's command a moment later, as a real tmux server
// does, a stand-in start_process that starts it at once in the background, both recording each
// call, and a stand-in mise that prints the omp command line it was given.
describe("launch", () => {
  const standIns = `
tmux() {
  printf 'tmux %s\\n' "$1" >>"$R/calls"
  case $1 in new-session) (command sleep 0.3 && bash -c "\${@: -1}") & ;; esac
}
start_process() {
  printf 'start_process %s\\n' "$1" >>"$R/calls"
  shift
  "$@" &
  agent_pid=$!
}
stop_tree() { :; }
sleep() { command sleep 0.05; }`;
  /** One later turn, `launch spec-writing-s1-1 --continue`, in a run directory of its own: the
   * pane's out.txt as its wait read it, and the tmux and start_process calls it made. `tmux` puts
   * a tmux binary on PATH, which is what launch looks for: the row's own tmux function takes
   * every call either way. With none, PATH holds only the tools launch and its pane run, so a tmux
   * elsewhere on the machine is not on it. `given` sets SKILL_SCENARIOS_OMP to an omp of the run's
   * own, which prints its path and its arguments. */
  function laterTurn({ tmux, given }: { tmux: boolean; given: boolean }) {
    const run = freshDir("launch-run");
    const bin = freshDir("launch-bin");
    let PATH = `${bin}:${childEnv.PATH}`;
    if (tmux) {
      writeFileSync(path.join(bin, "tmux"), '#!/bin/sh\necho "tmux ran: $*" >&2\nexit 1\n', {
        mode: 0o755,
      });
    } else {
      const tools = freshDir("launch-tools");
      for (const tool of ["bash", "cat", "chmod", "cp", "env", "grep", "sleep", "tail"]) {
        const found = Bun.which(tool);
        if (found === null) throw new Error(`launch's row needs ${tool} on PATH`);
        symlinkSync(found, path.join(tools, tool));
      }
      PATH = `${bin}:${tools}`;
    }
    for (const dir of ["cwd", "sessions"]) mkdirSync(path.join(run, dir));
    writeFileSync(path.join(run, "prompt.txt"), "The person answered your asks on LWEVAL-2.\n");
    writeFileSync(path.join(run, "pane.env"), `PATH=${PATH}\n`);
    writeFileSync(path.join(run, "out.txt"), "the first turn\nexit=0\n");
    mkdirSync(path.join(bin, "pin", "bin"), { recursive: true });
    writeFileSync(path.join(bin, "pin", "bin", "omp"), "#!/bin/sh\n", { mode: 0o755 });
    writeFileSync(
      path.join(bin, "mise"),
      `#!/bin/sh\nif [ "$1" = where ]; then echo "${bin}/pin"; exit 0; fi\nshift 3\necho "$@"\n`,
      { mode: 0o755 }
    );
    const omp = path.join(bin, "given", "omp");
    mkdirSync(path.dirname(omp));
    writeFileSync(omp, '#!/bin/sh\necho "$0 $*"\n', { mode: 0o755 });
    const result = bash(
      `${fn("fail")}\n${fn("given_omp")}\n${fn("pinned_command")}\n${fn("launch")}\n${standIns}\nR=$1 tag=t pin=omp@pin\nlaunch spec-writing-s1-1 --continue\ncp "$R/out.txt" "$R/seen"`,
      [run],
      given ? { PATH, SKILL_SCENARIOS_OMP: omp } : { PATH }
    );
    return {
      run,
      bin,
      omp,
      code: result.code,
      stderr: result.stderr,
      seen: existsSync(path.join(run, "seen")) ? readFileSync(path.join(run, "seen"), "utf8") : "",
      calls: readFileSync(path.join(run, "calls"), "utf8").trimEnd().split("\n"),
    };
  }
  const prompt = "-p The person answered your asks on LWEVAL-2. --continue --session-dir";
  const inTmux = ["tmux kill-session", "tmux new-session", "tmux kill-session"];

  // Catches: a later turn that reads the previous turn's `exit=` line as its own and returns at
  // once, and extra omp arguments placed anywhere but after the prompt.
  test("a later turn passes its arguments to omp and waits for its own exit line", () => {
    const { run, bin, code, seen } = laterTurn({ tmux: true, given: false });
    expect(code).toBe(0);
    expect(seen).toBe(
      `${bin}/pin/bin/omp -p The person answered your asks on LWEVAL-2. --continue --session-dir ${run}/sessions\nexit=0\n`
    );
  });

  // Catches: SKILL_SCENARIOS_OMP deciding whether tmux runs as well as which omp does, so a box
  // that has tmux and tries an Oh My Pi build of its own loses its panes.
  test("with tmux on PATH, a set SKILL_SCENARIOS_OMP still runs in a tmux session, as it is", () => {
    const turn = laterTurn({ tmux: true, given: true });
    expect({ code: turn.code, stderr: turn.stderr }).toEqual({ code: 0, stderr: "" });
    expect(turn.seen).toBe(`${turn.omp} ${prompt} ${turn.run}/sessions\nexit=0\n`);
    expect(turn.calls).toEqual(inTmux);
  });

  // Catches: a box with no tmux that sets no SKILL_SCENARIOS_OMP, where launch would start no
  // agent at all, and a pod with no tmux that sets it, where the agent would be the pin's omp
  // through a mise the pod does not carry.
  test.each([
    ["mise's install of the pin", false],
    ["the binary SKILL_SCENARIOS_OMP names", true],
  ] as const)("with no tmux on PATH, the agent is a process group of the run's own shell, whichever omp runs: %s", (_omp, given) => {
    const turn = laterTurn({ tmux: false, given });
    expect({ code: turn.code, stderr: turn.stderr }).toEqual({ code: 0, stderr: "" });
    const omp = given ? turn.omp : `${turn.bin}/pin/bin/omp`;
    expect(turn.seen).toBe(`${omp} ${prompt} ${turn.run}/sessions\nexit=0\n`);
    expect(turn.calls).toEqual(["start_process agent"]);
  });
});

// pinned_command: the command a label's agents run omp by, the binary SKILL_SCENARIOS_OMP names
// or mise's install of the pin, each refused in words of its own when it is not executable.
describe("pinned_command", () => {
  /** pinned_command of the pin omp@pin under `env`, and the command it set, one word a line. */
  const pinned = (env: Record<string, string>) =>
    bash(
      `${fn("fail")}\n${fn("given_omp")}\n${fn("pinned_command")}\npinned_command omp@pin\nprintf '%s\\n' "\${omp_command[@]}"`,
      [],
      env
    );

  // Catches: the two branches' failure texts run together, which read "the Oh My Pi binary
  // SKILL_SCENARIOS_OMP names/x/omp, is not executable".
  test("a SKILL_SCENARIOS_OMP that is not executable is refused by name, as is a pin's install", () => {
    const dir = freshDir("pinned");
    const given = path.join(dir, "omp");
    writeFileSync(given, "#!/bin/sh\n", { mode: 0o644 });
    expect(pinned({ SKILL_SCENARIOS_OMP: given })).toEqual({
      code: 1,
      stdout: "",
      stderr: `skill-scenarios: ${given}, the Oh My Pi binary SKILL_SCENARIOS_OMP names, is not executable\n`,
    });
    chmodSync(given, 0o755);
    expect(pinned({ SKILL_SCENARIOS_OMP: given })).toEqual({
      code: 0,
      stdout: `${given}\n`,
      stderr: "",
    });

    const bin = freshDir("pinned-bin");
    mkdirSync(path.join(bin, "pin", "bin"), { recursive: true });
    writeFileSync(path.join(bin, "pin", "bin", "omp"), "#!/bin/sh\n", { mode: 0o644 });
    writeFileSync(path.join(bin, "mise"), `#!/bin/sh\necho "${bin}/pin"\n`, { mode: 0o755 });
    expect(pinned({ PATH: `${bin}:${childEnv.PATH}` })).toEqual({
      code: 1,
      stdout: "",
      stderr: `skill-scenarios: ${bin}/pin/bin/omp, the Oh My Pi binary of the pin omp@pin, is not executable\n`,
    });
  });
});

// services_up and a run's start_dispatch, with every process they start stood in for: the Go
// toolchain (which records each build and writes the binary it names), the Postgres and NATS a
// batch was given, the listener, and run-server.sh, whose environment the row reads.
describe("the batch's Dispatch server build", () => {
  // Catches: a run whose Dispatch start compiles the server, as run-server.sh does when it is given
  // no binary: the first runs of a batch, compiling at once, overran the daemon stand-in's start
  // bound, then 30 s (trial-a-head-1 left an empty daemon.log).
  test("services_up builds the Dispatch server once, and each run's start_dispatch runs that build", () => {
    const work = freshDir("dispatch-build-work");
    const bin = freshDir("dispatch-build-bin");
    const builds = path.join(work, "builds");
    writeFileSync(
      path.join(bin, "go"),
      `#!/usr/bin/env bash
printf '%s %s\\n' "$PWD" "$*" >>${JSON.stringify(builds)}
while [ $# -gt 0 ]; do
  if [ "$1" = -o ]; then printf '#!/bin/sh\\n' >"$2" && chmod +x "$2"; fi
  shift
done
`,
      { mode: 0o755 }
    );
    const globals =
      "work=$1 root=$2 tag=t services_env=$1/services/env " +
      "services_postgres=$1/services/postgres-url services_dispatch=$1/services/dispatch-server";
    const up = bash(
      [
        ...["fail", "note", "safe_url", "given_services", "lock_services", "services_up"].map(fn),
        "until_true() { :; }",
        "pg_sql() { echo 100; }",
        "start_listener() { envoy_port=9020; }",
        globals,
        "lock_services",
        "services_up",
      ].join("\n"),
      [work, root],
      {
        PATH: `${bin}:${childEnv.PATH}`,
        SKILL_SCENARIOS_POSTGRES_URL: "postgres://rig@127.0.0.1:5432/postgres",
        SKILL_SCENARIOS_NATS_URL: "nats://127.0.0.1:4222",
        SKILL_SCENARIOS_PARALLEL: "1",
      }
    );
    expect({ code: up.code, stderr: up.stderr }).toEqual({ code: 0, stderr: "" });
    const envoy = path.join(root, "packages/envoy");
    expect(readFileSync(builds, "utf8")).toBe(
      `${envoy} build -o ${work}/services/envoy-listener ./cmd/listener\n` +
        `${envoy} build -o ${work}/services/dispatch-server ./cmd/dispatch\n`
    );

    // A run's start, in a process of its own as `rig.sh run` is: run-server.sh is given the build.
    const run = freshDir("dispatch-build-run");
    mkdirSync(path.join(run, "logs"));
    const start = bash(
      [
        fn("start_dispatch"),
        'start_process() { env >"$evidence/run-server.env"; dispatch_pid=1; }',
        "await_start() { :; }",
        "report_port() { echo 5555; }",
        globals,
        "evidence=$3",
        "start_dispatch postgres://rig@127.0.0.1:5432/run_x",
      ].join("\n"),
      [work, root, run]
    );
    expect({ code: start.code, stderr: start.stderr }).toEqual({ code: 0, stderr: "" });
    expect(readFileSync(path.join(run, "run-server.env"), "utf8").split("\n")).toContain(
      `DISPATCH_E2E_SERVER_BIN=${work}/services/dispatch-server`
    );
  });
});

// own_dispatch on a Postgres the batch was given (SKILL_SCENARIOS_POSTGRES_URL): the run's own
// database there is scripts/e2e/lib/loopback-dsn.sh's run_database_url, which reads the URL with
// libpq. The server's CREATE DATABASE, the Dispatch server and the seed are stood in for, so a row
// reads what the server was asked and the connection string the run's Dispatch was given.
const withoutPython = skipWithout("python3");
const withoutLibpq =
  !withoutPython &&
  Bun.spawnSync([
    "python3",
    "-c",
    'import ctypes.util, sys; sys.exit(0 if ctypes.util.find_library("pq") else 1)',
  ]).exitCode !== 0;
if (withoutLibpq) {
  console.warn(
    `Skipped the own_dispatch tests in ${import.meta.path}. run_database_url reads each URL with ` +
      `libpq, and ctypes.util.find_library("pq") finds none on this host; install libpq5.`
  );
}
describe.skipIf(withoutPython || withoutLibpq)(
  "own_dispatch on a Postgres the batch was given",
  () => {
    /** One run's own_dispatch on the server `url` names: the run's database, its exit, what the
     * server was asked (`sql`) and the connection string the run's Dispatch was given. */
    function ownDispatch(url: string) {
      const run = freshDir("own-dispatch");
      const result = bash(
        [
          fn("fail"),
          fn("safe_url"),
          fn("own_dispatch"),
          '. "$2/scripts/e2e/lib/loopback-dsn.sh"',
          'pg_sql() { printf \'%s\\n\' "$2" >>"$R/sql"; }',
          'start_dispatch() { printf \'%s\\n\' "$1" >"$R/database-url"; dispatch_port=5555; }',
          "bun() { :; }",
          "R=$1 root=$2 here=/rig services_postgres=$1/postgres-url",
          'printf \'%s\\n\' "$3" >"$services_postgres"',
          "own_dispatch project",
        ].join("\n"),
        [run, root, url]
      );
      const [sql, databaseUrl] = ["sql", "database-url"].map((file) =>
        existsSync(path.join(run, file)) ? readFileSync(path.join(run, file), "utf8") : ""
      );
      return {
        database: `run_${path.basename(run).replaceAll("-", "_")}`,
        code: result.code,
        stderr: result.stderr,
        sql,
        databaseUrl,
      };
    }

    // The URLs scripts/e2e/lib/loopback-dsn.test.ts pins run_database_url with (its first row is
    // the one a shell rewrite of the URL got wrong: a `?` inside the user name, which libpq reads
    // as part of it), each with the connection string the helper rebuilds for the run's database.
    // Catches: a run's Dispatch given a string a text join of the URL's parts would make, which
    // names another database, another user or another host.
    test.each([
      [
        "postgres://u?x:p@localhost:5433/db",
        "host='localhost' password='p' port='5433' user='u?x'",
      ],
      ["postgres://evil.example?x=@localhost", "host='localhost' user='evil.example?x='"],
      ["postgres://localhost/db?", "host='localhost'"],
      [
        "postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable",
        "host='127.0.0.1' port='5433' sslmode='disable' user='legion_test'",
      ],
      ["postgres://u@localhost:5432@10.1.2.3/db", "host='localhost' port='5432@10.1.2.3' user='u'"],
      ["postgres://evil.example?@localhost/db", "host='localhost' user='evil.example?'"],
      [
        "postgres://u@127.0.0.1:5433/postgres?dbname=other&sslmode=disable",
        "host='127.0.0.1' port='5433' sslmode='disable' user='u'",
      ],
    ])("%s gives the run's Dispatch its own database there", (url, rest) => {
      const { database, ...made } = ownDispatch(url);
      expect(made).toEqual({
        code: 0,
        stderr: "",
        sql: `CREATE DATABASE ${database}\n`,
        databaseUrl: `dbname='${database}' ${rest}\n`,
      });
    });

    // Catches: a run that makes its database on a server that is not this machine's, which the
    // run's Dispatch would refuse only after the database was made.
    test("a server that is not this machine's is refused by name, before any database is made", () => {
      expect(ownDispatch("postgres://legion_test:secret@db.prod.example/db")).toEqual({
        database: expect.any(String),
        code: 1,
        stderr:
          "skill-scenarios: SKILL_SCENARIOS_POSTGRES_URL must name 127.0.0.1, localhost or ::1: " +
          "libpq reads the host db.prod.example, and each run makes a database of its own there\n",
        sql: "",
        databaseUrl: "",
      });
    });
  }
);

// red-at-the-head's runs (rig.sh's run_red_at_the_head, cmd_run and cmd_batch): the one role booted
// on a world, the turns it takes, and the world a run's name carries. The world itself, the
// library red-at-the-head.sh, is red-at-the-head.test.ts's, the scorer's half of the scenario
// score.test.ts's, and the stand-in's sequencing gh-standin.test.ts's. A row reaches the library's
// functions by sourcing it, and rig.sh's own by name; the rows of the reviewer's variants build
// their worlds as red-at-the-head.test.ts does (red-at-the-head.fixtures.ts).
const library = `. ${JSON.stringify(path.join(import.meta.dir, "red-at-the-head.sh"))}`;

/** The rig's one table of the worlds (jq/worlds.json, which red_world reads): each world, with the
 * name of its red's lane, and each lane's check. */
const RED_TABLE: {
  worlds: Record<string, { lane: string }>;
  lanes: Record<string, { check: string }>;
} = JSON.parse(readFileSync(path.join(import.meta.dir, "jq", "worlds.json"), "utf8"));
/** Each world's red check, its lane's, which red_fixture writes to world.json as `check`: in a
 * reviewer's variant the check of the world it varies, whose lane it has. */
const RED_CHECK = Object.fromEntries(
  Object.entries(RED_TABLE.worlds).map(([world, { lane }]) => [world, RED_TABLE.lanes[lane]?.check])
);
const RED_REPO = "example/widgets";
const RED_PR = 1003;
/** The head the stand-in for worker_fixture below writes to world.json, which every prompt names. */
const RED_HEAD = "3ad0b8c1f2e4d6a8b0c2e4f6a8b0c2d4e6f8a0b2";

// run_red_at_the_head with every process it starts stood in for: the run's Dispatch, the seeds,
// both fixtures, the rig's own report of world `l`'s check (red_report), the daemon stand-in
// (start_process), the worker pane (`bun`) and the turns (launch). Each stand-in records its
// call, so a row reads what the run passed on. The red_fixture stand-in writes what the real one
// writes to world.json that the run reads: the world's facts, from the rig's own red_world, and
// its check.
describe("run_red_at_the_head", () => {
  const standIns = `
own_dispatch() { dispatch_port=5555; }
seed_ts() {
  printf 'seed %s\\n' "$*" >>"$R/calls"
  [ "$1" != tester-proof ] || printf '{"issue":"${RED_KEY}","checks":[],"implement":{"version":4}}\\n' >"$2"
}
worker_fixture() {
  printf 'worker_fixture %s\\n' "$*" >>"$R/calls"
  jq -n '{pr:${RED_PR}, head:"${RED_HEAD}", branch:"legion/${RED_KEY}"}' >"$R/world.json"
}
write_facts() {
  jq --argjson facts "$(red_world "$1")" --arg world "$1" \\
    '. + $facts + {check:(${JSON.stringify(RED_CHECK)})[$world]}' \\
    "$R/world.json" >"$R/world.red.json"
  mv "$R/world.red.json" "$R/world.json"
}
red_fixture() { printf 'red_fixture %s\\n' "$*" >>"$R/calls"; write_facts "$1"; }
start_process() {
  printf 'start_process %s\\n' "$*" >>"$R/calls"
  daemon_pid=4242
  printf 'listening on http://127.0.0.1:4321\\n' >"$R/logs/daemon.log"
}
await_start() { printf 'await_start %s\\n' "$1" >>"$R/calls"; }
base_env() { : >"$R/worker.env"; }
standins() { :; }
bun() { printf 'bun %s\\n' "$*" >>"$R/calls"; }
launch() { printf 'launch %s\\n' "$*" >>"$R/calls"; printf 'exit=0\\n' >"$R/out.txt"; }
rerun_pressed() { printf 'rerun_pressed %s\\n' "$*" >>"$R/calls"; }
rerun_settle() { printf 'rerun_settle\\n' >>"$R/calls"; }
red_report() { printf 'red_report\\n' >>"$R/calls"; }`;

  /** The script of one run of world $2 in the run directory $1: run_red_at_the_head over the
   * stand-ins above and then `extra`, where a row stands in for something more, with `here` as the
   * rig's own directory. */
  const redRunScript = (here: string, extra: string[] = []) =>
    [
      // The library first: the world's repository and its check (world_repository,
      // world_check_when) and the world's facts (red_world), which worker_world seeds the issue
      // by; the stand-ins below take the place of the rest of it that the run calls.
      library,
      fn("fail"),
      fn("note"),
      // The helpers run_red_at_the_head shares with run_tester_proof: the seeds and the world's
      // read (worker_world), the architect's assignment (architect_assignment), the pane and the
      // daemon stand-in (boot_worker), and each turn's line of turns.txt (record_turn).
      fn("worker_world"),
      fn("architect_assignment"),
      fn("boot_worker"),
      fn("record_turn"),
      fn("run_red_at_the_head"),
      standIns,
      ...extra,
      "R=$1",
      "work=$R/work",
      "P=$work/profiles/s1",
      'mkdir -p "$P"',
      "profile=$P/home",
      "co=/checkout",
      `here=${here}`,
      "services_env=$R/services.env",
      ': >"$services_env"',
      'run_red_at_the_head "trial-$2-s1-2" 2 3 1740000000 "$2"',
    ].join("\n");

  /** One run of `world`, and what it took: the world it wrote down, the role each worker it
   * started was given, the turns it ran and what woke each, whether the rig reported the check
   * and whether it settled the re-run between them. */
  function redRun(world: string) {
    const run = freshDir(`red-run-${world}`);
    mkdirSync(path.join(run, "logs"));
    const result = bash(redRunScript("/rig"), [run, world]);
    if (result.code !== 0) throw new Error(`run_red_at_the_head ${world}: ${result.stderr}`);
    const calls = readFileSync(path.join(run, "calls"), "utf8").trimEnd().split("\n");
    return {
      // The world the run hands red_fixture, which writes it to world.json as `world`.
      world: calls.find((call) => call.startsWith("red_fixture "))?.split(" ")[1],
      // The daemon stand-in takes the role before the world file, the worker pane before its
      // checkout.
      daemons: calls
        .filter((call) => call.includes("daemon-standin.ts"))
        .map((call) => call.split(" ").at(-2)),
      worlds: calls
        .filter((call) => call.includes("daemon-standin.ts"))
        .map((call) => call.split(" ").at(-1)),
      panes: calls
        .filter((call) => call.includes("worker-pane.ts"))
        .map((call) => call.split(" ").at(-2)),
      turns: readFileSync(path.join(run, "turns.txt"), "utf8").trimEnd().split("\n"),
      outs: ["out-1.txt", "out-2.txt", "out-3.txt"].filter((out) =>
        existsSync(path.join(run, out))
      ),
      reported: calls.includes("red_report"),
      settled: calls.includes("rerun_settle"),
    };
  }

  /** One line of turns.txt: the turn, what woke it, the moment it ended and its exit line. */
  const turnLine = (turn: number, wake: string) =>
    expect.stringMatching(
      new RegExp(`^${turn} ${wake} \\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}Z exit=0$`)
    );

  /** The role that holds the phase in `world`: the reviewer in `i`, `j` and the variants. */
  const roleOf = (world: string) =>
    world === "i" || world === "j" || world in RED_VARIANTS ? "reviewer" : "tester";
  /** Every world, the twelve and the reviewer's variants of eight of them. */
  const allWorlds = [...RED_WORLDS, ...RED_VARIANT_WORLDS];

  // Catches: a run that boots the tester in a reviewer's world, one that boots both roles, a
  // world.json the scorer reads as another world than the one the run ran, a daemon stand-in told
  // no world, a world that takes a turn it owes no wake for, a world `l` or `t` whose second turn
  // is a re-run's settlement rather than the rig's own report of the check, a settlement the rig
  // gives before the turn that pressed the re-run has ended, or in a world that re-runs nothing,
  // and a reviewer's variant that takes other turns than the world it varies.
  test("boots one worker, takes each world's turns, and settles a re-run only after its turn", () => {
    expect(allWorlds.map(redRun)).toEqual(
      allWorlds.map((world) => {
        const role = roleOf(world);
        // `d`, `e` and `f` and their variants expect no re-run and take one turn; `l` and `t` take
        // their second on the rig's report of the check and their third on the settlement of the
        // re-run; every other world takes its second on the settlement of the re-run it pressed
        // (the stand-in for rerun_pressed answers that it did).
        const base = baseOf(world);
        const one = base === "d" || base === "e" || base === "f";
        return {
          world,
          daemons: [role],
          worlds: [expect.stringMatching(/\/world\.json$/)],
          panes: [role],
          turns: one
            ? [turnLine(1, "assignment")]
            : base === "l"
              ? [turnLine(1, "assignment"), turnLine(2, "report"), turnLine(3, "settlement")]
              : [turnLine(1, "assignment"), turnLine(2, "settlement")],
          outs: one
            ? ["out-1.txt"]
            : base === "l"
              ? ["out-1.txt", "out-2.txt", "out-3.txt"]
              : ["out-1.txt", "out-2.txt"],
          reported: base === "l",
          settled: !one,
        };
      })
    );
  });

  /** Every prompt a run of `world` gives its agent, one after another. */
  function redPrompts(world: string) {
    const run = freshDir(`red-prompts-${world}`);
    mkdirSync(path.join(run, "logs"));
    const launch = `launch() { cat "$R/prompt.txt" >>"$R/prompts"; printf 'exit=0\\n' >"$R/out.txt"; }`;
    const result = bash(redRunScript("/rig", [launch]), [run, world]);
    if (result.code !== 0) throw new Error(`run_red_at_the_head ${world}: ${result.stderr}`);
    return readFileSync(path.join(run, "prompts"), "utf8");
  }

  // The text each turn of a world gives its agent, which no fact of the world may change. Catches:
  // an assignment of another role's phase, a told line in a world whose counted set reports its
  // red, and a resume task of another phase.
  test("each world's prompts are the architect's assignment and the resume task, word for word", () => {
    const prompts = allWorlds.map(redPrompts);
    const pr = `PR #${RED_PR} (https://github.com/${RED_REPO}/pull/${RED_PR}, repository ${RED_REPO}) is at ${RED_HEAD}.`;
    const handoff = `Handoff: implement version 4 (dispatch handoff-read --issue ${RED_KEY} --phase implement --version 4).`;
    expect(prompts).toEqual(
      allWorlds.map((world) => {
        const reviewer = roleOf(world) === "reviewer";
        const base = baseOf(world);
        const told = base === "l" ? " The daemon reports test (notify) red at that head." : "";
        const assignment = reviewer
          ? `Architect → reviewer, ${RED_KEY}. The tester has finished: ${pr}${told} ${handoff} Tester's verdict: pass. Review it and finish your phase.\n`
          : `Architect → tester, ${RED_KEY}. The implementer has finished: ${pr}${told} ${handoff} Test it and finish your phase.\n`;
        const resume = `Continue greet(): a greeting helper for the widgets CLI. Issue: ${RED_KEY}. Phase: ${reviewer ? "reviewing" : "testing"}. Resume the existing phase work.\n`;
        return base === "d" || base === "e" || base === "f"
          ? assignment
          : base === "l"
            ? assignment + resume + resume
            : assignment + resume;
      })
    );
  });

  /** score.ts's output over one staged run of each world.json of `runs`, by the run's name: a run
   * whose agent got its one model turn and did nothing more, no gh call, no record, no completion,
   * so the row shows which rule the scorer read the world by. */
  function scoredRuns(runs: Record<string, string>): string {
    const runsDir = path.join(freshDir("red-scored"), "runs");
    for (const [name, world] of Object.entries(runs)) {
      const dir = path.join(runsDir, name);
      mkdirSync(path.join(dir, "sessions"), { recursive: true });
      const files: Record<string, string> = {
        "dispatch.json": JSON.stringify({
          postgres: "pg-1",
          database: `run_${name.replaceAll("-", "_")}`,
          port: 20000,
          url: "http://127.0.0.1:20000",
        }),
        "out.txt": "exit=0\n",
        "world.json": world,
        "calls.jsonl": "",
        "handoffs.json": "[]",
        "daemon.log": "",
        "turns.txt": "1 assignment 2026-09-30T17:30:00Z exit=0\n",
        // The rig's report of the check, which a world whose counted set reports the red late
        // (`report`) is scored against.
        ...(JSON.parse(world).report ? { "reported-at": "2026-09-30T17:29:00Z\n" } : {}),
      };
      for (const [file, text] of Object.entries(files)) writeFileSync(path.join(dir, file), text);
      writeFileSync(
        path.join(dir, "sessions", "2026-09-30T17-29-00-000Z_01a0f2cc.jsonl"),
        [
          { type: "session", id: "01a0f2cc", cwd: path.join(dir, "cwd") },
          { type: "message", message: { role: "user", content: [{ type: "text", text: "Go." }] } },
          {
            type: "message",
            message: { role: "assistant", content: [{ type: "text", text: "Done." }] },
          },
        ]
          .map((entry) => `${JSON.stringify(entry)}\n`)
          .join("")
      );
    }
    const scored = Bun.spawnSync(["bun", path.join(import.meta.dir, "score.ts"), "runs", runsDir], {
      env: childEnv,
    });
    if (scored.exitCode !== 0) throw new Error(`score.ts: ${scored.stderr.toString()}`);
    return scored.stdout.toString();
  }

  /** The worlds the variant rows below build, as red-at-the-head.test.ts builds its own
   * (red-at-the-head.fixtures.ts), each in a directory of this file's own. */
  const { builtWorld, ownFixture } = redWorlds(freshDir, childEnv);

  // The reviewer's variants (`m` to `t`, red_world's): no archived run is of one, so each
  // is proven by its own row, built and scored. Catches: a variant built on another world than the
  // one it varies (another red, another workflow, another answer of any route), facts that differ
  // from that world's in more than the role and RED_VARIANT_FACTS, one that boots the tester,
  // takes other turns than that world, and one the scorer cannot read or scores by the tester's
  // rule. A variant carries facts of its own beside its role (RED_VARIANT_FACTS: the aggregate the
  // base requires, and in `p` the title check required beside it), so the row builds the world it
  // varies with those facts over its own, and the variant's world must be that world in every
  // answer. The one other difference its build may carry is the pull request body's line for the
  // issue's check: a reviewer whose round goes on reads k1 as `after_merge`, not yet run, as `i`
  // does beside `a` (world_check_when). Both worlds are built for this row alone (ownFixture) and
  // compared as parsed JSON: the gh stand-in writes a world's fixtures.json back in its own form at
  // every call it answers (gh-standin.ts, compact, each route's keys in its schema's order), and in
  // a whole-file run the rows then above them had called it on the worlds redFixture shares,
  // which failed all eight of these rows in CI at 7bf70a93 (run 44797028466, job 136499245509: the
  // base's file compact, the variant's as red_fixture's jq wrote it) while they passed run alone.
  // Each builds two worlds and runs run_red_at_the_head twice and the scorer once, under the
  // file's minute.
  for (const variant of RED_VARIANT_WORLDS) {
    const base = baseOf(variant);
    test(`world ${variant} is world ${base} with the reviewer booted in reviewing and its own facts: its red, its answers, the reviewer's rule`, () => {
      const built = ownFixture(variant);
      const of = builtWorld(
        base,
        freshDir(`built-${base}-as-${variant}`),
        RED_VARIANT_FACTS[variant]
      );
      const read = (run: string, file: string) => readFileSync(path.join(run, file), "utf8");
      const head = JSON.parse(read(of, "world.json")).head;
      /** The pull request body's line for the issue's check, JSON-escaped as fixtures.json holds it. */
      const checkLine = (run: string) =>
        JSON.stringify(
          bash(`${library}\nR=$1\nchecks_lines "$2"`, [run, head]).stdout.trimEnd()
        ).slice(1, -1);
      // The facts that differ from the world it varies: the reviewer's role and RED_VARIANT_FACTS.
      const facts = (world: string) =>
        JSON.parse(bash(`${library}\nred_world "$1"`, [world]).stdout);
      expect(facts(variant)).toEqual({
        ...facts(base),
        role: "reviewer",
        phase: "reviewing",
        handoff: "review",
        ...RED_VARIANT_FACTS[variant],
      });
      expect(JSON.parse(read(built, "world.json"))).toEqual({
        ...JSON.parse(read(of, "world.json")),
        world: variant,
        role: "reviewer",
        phase: "reviewing",
        handoff: "review",
      });
      expect(JSON.parse(read(built, "fixtures.json"))).toEqual(
        JSON.parse(read(of, "fixtures.json").replaceAll(checkLine(of), checkLine(built)))
      );
      // The run boots the reviewer where the world it varies boots the tester, and takes the same
      // turns on the same wakes: each run's own directory and clock read out.
      const ran = (world: string) => {
        const run = redRun(world);
        return {
          ...run,
          worlds: run.worlds.map((file) => path.basename(file ?? "")),
          turns: run.turns.map((line) => line.replace(/ \S+Z /, " ")),
        };
      };
      expect(ran(variant)).toEqual({
        ...ran(base),
        world: variant,
        daemons: ["reviewer"],
        panes: ["reviewer"],
      });
      // The scorer reads the built world by the reviewer's rule, where it reads the world it
      // varies by the tester's: a reviewer's run that read no CI never met the red and is left
      // out of its key's counts, and a tester's is counted.
      const out = scoredRuns({
        [`trial-${variant}-s1-1`]: read(built, "world.json"),
        [`trial-${base}-s1-1`]: read(of, "world.json"),
      });
      expect(out).toMatch(
        new RegExp(
          `^trial-${variant}-s1-1\\tpass=false\\tref=false\\tworld=${variant} record=none wrote=\\w+ before=\\w+ pressed=\\w+ waited=\\w+ outcome=false told=\\w+ declined=false reported=\\w+ readCI=false `,
          "m"
        )
      );
      expect(out).toContain(
        `trial-${base}-s1-1\tpass=false\tref=false\tworld=${base} rule=tester ciReads=0 pressed=true outcome=false readCI=false `
      );
      expect(out).toContain(`red-at-the-head:${variant}\ts1\t0/0\t0/0\t0\t0\n`);
      expect(out).toContain(`red-at-the-head:${base}\ts1\t0/1\t0/1\t0\t0\n`);
    });
  }

  // Catches: a run whose fixtures.json the gh stand-in refuses, which would boot its worker and
  // answer each of the role's `gh` calls with the refusal, a head that never turns scored as the
  // role's failure. worker_world runs the stand-in's own parse once the fixtures are written, so
  // that run stops before it boots anything and leaves no exit= line in out.txt, which the scorer
  // reports as a rig error. The same run with its `follows` spelled as the route's match boots.
  test("a fixture the gh stand-in refuses stops the run before it boots a worker", () => {
    // red_fixture as a typo in its jq would leave the routes, and the real `bun` for the
    // stand-in's own parse; every other `bun` call is recorded, as above.
    const extra = [
      `red_fixture() { printf 'red_fixture %s\\n' "$*" >>"$R/calls"; cp "$R/red.json" "$R/fixtures.json"; write_facts "$1"; }`,
      `bun() { if [ "$2" = --check ]; then command bun "$@"; else printf 'bun %s\\n' "$*" >>"$R/calls"; fi; }`,
    ];
    const seen = ["^gh run rerunn", "^gh run rerun"].map((follows) => {
      const run = freshDir("red-run-checked");
      mkdirSync(path.join(run, "logs"));
      writeFileSync(
        path.join(run, "red.json"),
        JSON.stringify({
          routes: [
            { match: "^gh pr checks", replies: [{ stdout: "test\tfail", exit: 1 }, {}], follows },
            { match: "^gh run rerun", stdout: "✓ Requested rerun of failed jobs" },
          ],
        })
      );
      const result = bash(redRunScript(import.meta.dir, extra), [run, "a"]);
      const calls = readFileSync(path.join(run, "calls"), "utf8");
      return {
        code: result.code,
        stderr: result.stderr,
        log: readFileSync(path.join(run, "fixture.log"), "utf8"),
        fixtured: calls.includes("red_fixture a\n"),
        booted: calls.includes("start_process daemon") || calls.includes("launch "),
        finished: existsSync(path.join(run, "out.txt")),
        run,
      };
    });
    const [refused, spelled] = seen;
    expect(refused).toEqual({
      code: 1,
      stderr:
        `skill-scenarios: the gh stand-in refuses ${refused?.run}/fixtures.json; ` +
        `see ${refused?.run}/fixture.log\n`,
      log:
        `gh-standin: ${refused?.run}/fixtures.json is refused:\n` +
        `✖ route "^gh pr checks" follows "^gh run rerunn", which is no route's match\n` +
        "  → at routes[0].follows\n",
      fixtured: true,
      booted: false,
      finished: false,
      run: expect.any(String),
    });
    expect(spelled).toEqual({
      code: 0,
      stderr: "",
      log: "",
      fixtured: true,
      booted: true,
      finished: true,
      run: expect.any(String),
    });
  });
});

// cmd_run and cmd_batch read SKILL_SCENARIOS_RED_WORLD and take it only where red_world prints its
// facts: the run function is stood in for, so a row reads the name the command made and what it
// refused, and a row may stand in for red_world too, with `worlds` after the library's own.
describe("a red-at-the-head run's world", () => {
  const runStandIns = `
load_label() { P=$work/profiles/$1; mkdir -p "$P"; printf 'commit\\n' >"$P/built-from"; }
on_exit() { :; }
run_red_at_the_head() {
  printf 'run_red_at_the_head %s\\n' "$*" >>"$work/calls"
  printf 'exit=0\\n' >"$R/out.txt"
}`;
  const run = (work: string, world: string | undefined, n: string, worlds = "") =>
    bash(
      [
        library,
        fn("fail"),
        fn("cmd_run"),
        fn("run_prefix"),
        worlds,
        runStandIns,
        "work=$1",
        'mkdir -p "$work"',
        "services_env=$work/services.env",
        ': >"$services_env"',
        "shift",
        'cmd_run "$@"',
      ].join("\n"),
      [work, "red-at-the-head", "s1", n, "3", "1740000000"],
      world === undefined ? {} : { SKILL_SCENARIOS_RED_WORLD: world }
    );

  // Catches: a run named after its scenario and label alone, whose directory a second world's
  // batch would remove (cmd_run starts from rm -rf), and a world the run passes on by another
  // name than the one it was named for.
  test("the run's name and directory carry its world, so two worlds keep their own runs", () => {
    const work = freshDir("red-run-work");
    expect(run(work, "a", "1").stdout).toBe("trial-a-s1-1: exit=0\n");
    expect(run(work, "b", "1").stdout).toBe("trial-b-s1-1: exit=0\n");
    expect(readFileSync(path.join(work, "calls"), "utf8")).toBe(
      "run_red_at_the_head trial-a-s1-1 1 3 1740000000 a\n" +
        "run_red_at_the_head trial-b-s1-1 1 3 1740000000 b\n"
    );
    expect(existsSync(path.join(work, "runs/trial-a-s1-1"))).toBe(true);
    expect(existsSync(path.join(work, "runs/trial-b-s1-1"))).toBe(true);
  });

  // A red-at-the-head run's pane.env (base_env) holds every agent-visible path — HOME, TMPDIR,
  // PATH and SKILL_SCENARIO_RUN — and each lives under the run directory, named by run_prefix.
  // The prefix is `trial`, off the page's own name, so no such path names the page under test.
  // Catches: a run directory, or a pane.env value, that names skills/legion-worker/references/
  // red-at-the-head.md — which, in an agent-visible path, cues the change arm (119 of 120
  // record-batch runs opened the page before any CI read).
  test("no agent-visible path of a red-at-the-head run names the page it tests", () => {
    const work = freshDir("red-pane-work");
    const result = bash(
      [
        library,
        fn("fail"),
        fn("base_env"),
        fn("cmd_run"),
        fn("run_prefix"),
        'load_label() { P=$work/profiles/$1; mkdir -p "$P/home"; printf \'commit\\n\' >"$P/built-from"; }',
        "on_exit() { :; }",
        "run_red_at_the_head() { base_env; printf 'exit=0\\n' >\"$R/out.txt\"; }",
        "work=$1",
        'mkdir -p "$work"',
        "services_env=$work/services.env",
        ': >"$services_env"',
        "shift",
        'cmd_run "$@"',
      ].join("\n"),
      [work, "red-at-the-head", "s1", "1", "3", "1740000000"],
      { SKILL_SCENARIOS_RED_WORLD: "a" }
    );
    expect(result.code).toBe(0);
    const made = readdirSync(path.join(work, "runs"));
    expect(made).toHaveLength(1);
    const runName = made[0] ?? "";
    expect(runName).not.toContain("red-at-the-head");
    const env = readFileSync(path.join(work, "runs", runName, "pane.env"), "utf8").trimEnd();
    for (const line of env.split("\n")) {
      expect(line.slice(line.indexOf("=") + 1)).not.toContain("red-at-the-head");
    }
  });

  // Catches: a run of a world red_fixture has no red for, which would fail inside the run with
  // its services up, and a run named `trial--s1-1` when the variable is unset.
  test("cmd_run refuses a world that is not one of the twenty, naming them, and makes no run", () => {
    const work = freshDir("red-run-refused");
    const named = "not a, b, c, d, e, f, g, h, i, j, k, l, m, n, o, p, q, r, s or t";
    const unknown = run(work, "z", "1");
    expect(unknown.code).toBe(1);
    expect(unknown.stderr).toBe(`skill-scenarios: SKILL_SCENARIOS_RED_WORLD is 'z', ${named}\n`);
    const unset = run(work, undefined, "1");
    expect(unset.code).toBe(1);
    expect(unset.stderr).toBe(`skill-scenarios: SKILL_SCENARIOS_RED_WORLD is '', ${named}\n`);
    expect(existsSync(path.join(work, "runs"))).toBe(false);
  });

  const batchStandIns = `
load_label() { printf 'load_label %s\\n' "$1" >>"$work/calls"; }
on_exit() { :; }
lock_services() { printf 'lock_services\\n' >>"$work/calls"; }
services_up() { printf 'services_up\\n' >>"$work/calls"; }
warm_daemon_panes() { printf 'warm_daemon_panes\\n' >>"$work/calls"; }`;
  /** cmd_batch of two runs of label s1 in `scenario` over the stand-ins above, with
   * SKILL_SCENARIOS_RED_WORLD set to `world`, and the calls it made. */
  function batch(world: string | undefined, worlds = "", scenario = "red-at-the-head") {
    const work = freshDir("red-batch-work");
    const here = freshDir("red-batch-here");
    writeFileSync(
      path.join(here, "rig.sh"),
      `#!/usr/bin/env bash\nprintf 'run %s\\n' "$*" >>"${path.join(work, "calls")}"\n`
    );
    const result = bash(
      [
        library,
        fn("fail"),
        fn("cmd_batch"),
        worlds,
        batchStandIns,
        "work=$1",
        "here=$2",
        "shift 2",
        'cmd_batch "$@"',
      ].join("\n"),
      [work, here, scenario, "2", "s1"],
      world === undefined ? {} : { SKILL_SCENARIOS_RED_WORLD: world }
    );
    const calls = existsSync(path.join(work, "calls"))
      ? readFileSync(path.join(work, "calls"), "utf8").trimEnd().split("\n")
      : [];
    // The batch's start time is the clock's, so each run line is read without it. cmd_batch hands
    // the runs to `xargs -P`, which starts them at once, so the order of their lines is the
    // scheduler's: they are sorted, and the calls before them keep the order the batch made them in.
    const lines = calls.map((line) => line.split(" ").slice(0, 5).join(" "));
    const runs = lines.filter((line) => line.startsWith("run run "));
    return {
      ...result,
      calls: [...lines.filter((line) => !line.startsWith("run run ")), ...runs.sort()],
    };
  }

  // Catches: a batch that starts its services and only then finds it has no world, and a gate
  // that refuses the twenty worlds too.
  test("cmd_batch refuses such a world before it locks or starts a service", () => {
    const refused = batch("world-a");
    expect(refused.code).toBe(1);
    expect(refused.stderr).toBe(
      "skill-scenarios: a red-at-the-head batch needs SKILL_SCENARIOS_RED_WORLD set to " +
        "a, b, c, d, e, f, g, h, i, j, k, l, m, n, o, p, q, r, s or t (got 'world-a')\n"
    );
    expect(refused.calls).toEqual([]);
    const ran = batch("l");
    expect(ran.code).toBe(0);
    expect(ran.calls).toEqual([
      "load_label s1",
      "lock_services",
      "services_up",
      "warm_daemon_panes",
      "run run red-at-the-head s1 1",
      "run run red-at-the-head s1 2",
    ]);
  });

  // Catches: a batch that compiles daemon-pane.go in every daemon module for a scenario whose
  // runs boot no worker (boot_worker), and a tester-proof batch that skips the build its runs'
  // daemon stand-ins need. Askgate and the two spec scenarios need settings of their own before a
  // batch of them starts, so the row takes the scenarios a batch runs with nothing more.
  test("cmd_batch warms the daemon panes for tester-proof and red-at-the-head alone", () => {
    const scenarios = [
      "ask-on-message",
      "measure-before-ask",
      "tester-proof",
      "brainstorm",
      "red-at-the-head",
    ];
    const boots = ["tester-proof", "red-at-the-head"];
    expect(
      scenarios.map((scenario) => {
        const { code, calls } = batch("a", undefined, scenario);
        return { scenario, code, calls };
      })
    ).toEqual(
      scenarios.map((scenario) => ({
        scenario,
        code: 0,
        calls: [
          "load_label s1",
          "lock_services",
          "services_up",
          ...(boots.includes(scenario) ? ["warm_daemon_panes"] : []),
          `run run ${scenario} s1 1`,
          `run run ${scenario} s1 2`,
        ],
      }))
    );
  });

  // Catches: a list of worlds in cmd_run or cmd_batch beside red_world's table, which a world added
  // to the table would be missing from: here red_world knows `m` alone.
  test("cmd_run and cmd_batch take the worlds red_world knows, and keep no list of their own", () => {
    const worlds = `red_world() { [ "$1" = m ] && echo '{"role":"tester"}'; }\nred_worlds() { echo m; }`;
    const work = freshDir("red-run-catalogue");
    expect(run(work, "m", "1", worlds)).toEqual({
      code: 0,
      stdout: "trial-m-s1-1: exit=0\n",
      stderr: "",
    });
    const refused = run(work, "a", "1", worlds);
    expect(refused.code).toBe(1);
    // The refusal names the worlds red_worlds reads from the table, not a list of its own.
    expect(refused.stderr).toBe("skill-scenarios: SKILL_SCENARIOS_RED_WORLD is 'a', not m\n");
    expect(readFileSync(path.join(work, "calls"), "utf8")).toBe(
      "run_red_at_the_head trial-m-s1-1 1 3 1740000000 m\n"
    );
    const ran = batch("m", worlds);
    expect({ code: ran.code, calls: ran.calls }).toEqual({
      code: 0,
      calls: [
        "load_label s1",
        "lock_services",
        "services_up",
        "warm_daemon_panes",
        "run run red-at-the-head s1 1",
        "run run red-at-the-head s1 2",
      ],
    });
    expect(batch("a", worlds)).toMatchObject({ code: 1, calls: [] });
  });
});
