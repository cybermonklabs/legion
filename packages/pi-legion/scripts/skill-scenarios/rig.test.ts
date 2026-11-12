import { beforeAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import { cpSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptFunctions } from "../../../../scripts/e2e/lib/script-functions";

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
  const result = Bun.spawnSync(
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
    const served = Bun.spawnSync([path.join(dir, "model-key", "anthropic-key")]);
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
    // The real key-file route, with the lib it sources for its base URL's check.
    for (const file of ["install-key-file-model-route.sh", "model-endpoint.sh"]) {
      cpSync(path.join(root, "scripts/e2e/lib", file), path.join(co, "scripts/e2e/lib", file));
    }
    writeFileSync(
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
      'work=$1; cmd_profile probe "$2"',
    ].join("\n");
    const failed = bash(script, [work, checkout()], {
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
      [kept, checkout()],
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
    const co = checkout();
    const script = [
      "set -euo pipefail",
      ...["fail", "on_exit", "drop_unready_keys", "install_model_route", "judge_profiles"].map(fn),
      "checkout_commit() { printf 'fixture parents=base\\n'; }",
      "root=$1; work=$2; judge_model=fixture; judge_thinking=high; judge_profiles",
    ].join("\n");
    // The bare profile's home cannot be made, after the ste profile's route is installed.
    writeFileSync(
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

/** Runs SCRIPT (rig.sh functions and a call of them) under bash with ARGS as $1..., in an
 * environment of PATH and the test's own HOME. */
function bash(script: string, args: string[] = [], env: Record<string, string> = {}) {
  const result = Bun.spawnSync(["bash", "-c", script, "-", ...args], {
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

// cmd_live_read reads every path skill_reads prints. asd-ste100 holds examples/ beside
// references/, and a read of SKILL.md and references/ alone left examples/ unread.
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
    const env = readFileSync(path.join(bin, "env.txt"), "utf8");
    expect(env).toContain("ENVOY_URL=http://127.0.0.1:1\n");
    expect(env).toContain("OMP_PROFILE=skills-s1\n");
    expect(env).toContain(`HOME=${label}/home\n`);
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
  env | grep -q LEGION_E2E && echo "env LEGION_E2E set"
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
    writeFileSync(path.join(scripts, "rig.sh"), readFileSync(path.join(import.meta.dir, "rig.sh")));
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
// does, and a stand-in mise that prints the omp command line it was given.
describe("launch", () => {
  // Catches: a later turn that reads the previous turn's `exit=` line as its own and returns at
  // once, and extra omp arguments placed anywhere but after the prompt.
  test("a later turn passes its arguments to omp and waits for its own exit line", () => {
    const run = freshDir("launch-run");
    const bin = freshDir("launch-bin");
    for (const dir of ["cwd", "sessions"]) mkdirSync(path.join(run, dir));
    writeFileSync(path.join(run, "prompt.txt"), "The person answered your asks on LWEVAL-2.\n");
    writeFileSync(path.join(run, "pane.env"), `PATH=${bin}:${process.env.PATH ?? ""}\n`);
    writeFileSync(path.join(run, "out.txt"), "the first turn\nexit=0\n");
    mkdirSync(path.join(bin, "pin", "bin"), { recursive: true });
    writeFileSync(path.join(bin, "pin", "bin", "omp"), "#!/bin/sh\n", { mode: 0o755 });
    writeFileSync(
      path.join(bin, "mise"),
      `#!/bin/sh\nif [ "$1" = where ]; then echo "${bin}/pin"; exit 0; fi\nshift 3\necho "$@"\n`,
      { mode: 0o755 }
    );
    const standIns = `
tmux() { case $1 in new-session) (command sleep 0.3 && bash -c "\${@: -1}") & ;; esac; }
sleep() { command sleep 0.05; }`;
    const result = bash(
      `${fn("fail")}\n${fn("pinned_omp")}\n${fn("launch")}\n${standIns}\nR=$1 tag=t pin=omp@pin\nlaunch spec-writing-s1-1 --continue\ncp "$R/out.txt" "$R/seen"`,
      [run],
      { PATH: `${bin}:${childEnv.PATH}` }
    );
    expect(result.code).toBe(0);
    expect(readFileSync(path.join(run, "seen"), "utf8")).toBe(
      `${bin}/pin/bin/omp -p The person answered your asks on LWEVAL-2. --continue --session-dir ${run}/sessions\nexit=0\n`
    );
  });
});

// red-at-the-head (rig.sh's header, red_fixture, run_red_at_the_head, cmd_run and cmd_batch): the
// world's red at the head of worker_fixture's pull request, the one role booted on it, and the
// world a run's name and world.txt carry. The scorer's half of the scenario is score.test.ts's,
// and the stand-in's sequencing is gh-standin.test.ts's.

/** The twelve worlds, as rig.sh's header lists them and cmd_run and cmd_batch take them. */
const RED_WORLDS = ["a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"] as const;
type RedWorld = (typeof RED_WORLDS)[number];
/** Each world's red check, the job that carries it, and what the head settles at once the re-run
 * is pressed (red_fixture). The reviewer holds the phase in i and j, the tester in the rest. */
const RED_WORLD: Record<RedWorld, { check: string; job: string; after: "green" | "red" }> = {
  a: { check: "test (notify)", job: "test (notify)", after: "green" },
  b: { check: "test (notify)", job: "test (notify)", after: "red" },
  c: { check: "build", job: "build (linux-x64)", after: "green" },
  d: { check: "test (widgets)", job: "test (widgets)", after: "red" },
  e: { check: "test (notify)", job: "test (notify)", after: "red" },
  f: { check: "title", job: "title", after: "red" },
  g: { check: "ci", job: "test (notify)", after: "green" },
  h: { check: "test (notify)", job: "test (notify)", after: "green" },
  i: { check: "test (notify)", job: "test (notify)", after: "green" },
  j: { check: "test (notify)", job: "test (notify)", after: "red" },
  k: { check: "test (notify)", job: "test (notify)", after: "green" },
  l: { check: "test (notify)", job: "test (notify)", after: "green" },
};

const RED_KEY = "LWEVAL-9";
const RED_REPO = "example/widgets";
const RED_PR = 1003;
const RED_HEAD = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678";
const RED_TITLE = `feat: greet() (${RED_KEY})`;
/** red_fixture's ids for this pull request: the run that failed, its job, and the check runs —
 * lint, typecheck, the world's red, and the attempt the re-run makes. */
const RED_RUN_ID = 7_000_000 + RED_PR;
const RED_JOB_ID = 8_000_000 + RED_PR;
const RED_ID = { lint: 9_000_000 + RED_PR - 2, typecheck: 9_000_000 + RED_PR - 1 };
const RED_RED_ID = 9_000_000 + RED_PR;
const RED_RUN_URL = `https://github.com/${RED_REPO}/actions/runs/${RED_RUN_ID}`;
const ghStandin = path.join(import.meta.dir, "gh-standin.ts");

/** worker_fixture's green head as red_fixture reads it: the `gh pr view` answer with its rollup,
 * the `gh pr checks` rows and the generic check-runs read, which red_fixture rewrites, and the
 * catch-all past them, which it leaves alone. The repository it also builds, and the handoff it
 * writes, are no part of what red_fixture reads. */
function greenRun(name: string): string {
  const run = freshDir(name);
  const url = `https://github.com/${RED_REPO}/pull/${RED_PR}`;
  const pullRequest = {
    number: RED_PR,
    url,
    title: RED_TITLE,
    state: "OPEN",
    headRefName: `legion/${RED_KEY}`,
    headRefOid: RED_HEAD,
    body: `Dispatch: ${RED_KEY}`,
    head: { sha: RED_HEAD, ref: `legion/${RED_KEY}` },
    base: { ref: "main" },
    statusCheckRollup: [
      { name: "test", status: "COMPLETED", conclusion: "SUCCESS" },
      { name: "lint", status: "COMPLETED", conclusion: "SUCCESS" },
    ],
  };
  writeFileSync(
    path.join(run, "fixtures.json"),
    JSON.stringify({
      routes: [
        { match: "^gh pr view", stdout: pullRequest },
        {
          match: "^gh pr checks",
          stdout:
            "lint\tpass\t10s\thttps://example.invalid/1\ntest\tpass\t20s\thttps://example.invalid/2",
        },
        {
          match: "^gh api .*(check-runs|status)",
          stdout: {
            total_count: 2,
            check_runs: [
              { name: "test", status: "completed", conclusion: "success" },
              { name: "lint", status: "completed", conclusion: "success" },
            ],
          },
        },
        { match: "^gh (api user|auth status)", stdout: { login: "rig[bot]" } },
      ],
    })
  );
  writeFileSync(
    path.join(run, "world.json"),
    JSON.stringify({
      key: RED_KEY,
      repo: RED_REPO,
      pr: RED_PR,
      branch: `legion/${RED_KEY}`,
      head: RED_HEAD,
    })
  );
  return run;
}

/** A run directory whose fixtures.json and world.json red_fixture has turned red over `world`. */
function redFixture(world: string): string {
  const run = greenRun(`red-${world}`);
  const result = bash(
    `${fn("fail")}\n${fn("red_fixture")}\nR=$1 worker_key=$2 worker_repo=$3\nred_fixture "$4"`,
    [run, RED_KEY, RED_REPO, world]
  );
  if (result.code !== 0) throw new Error(`red_fixture ${world} failed: ${result.stderr}`);
  return run;
}

/** One `gh` call of the role's, through the stand-in rig.sh puts on its PATH. The environment is
 * the test's own, which names no Envoy, so a route that publishes records that it could not and
 * nothing leaves this machine; the row that reads a settlement names a listener of its own. */
async function gh(run: string, args: string[], env: Record<string, string> = {}) {
  const child = Bun.spawn(["bun", ghStandin, "gh", ...args], {
    env: { ...childEnv, SKILL_SCENARIO_RUN: run, ...env },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, exit] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { stdout, stderr, exit };
}
const outputLines = (stdout: string) => stdout.split("\n").filter((line) => line !== "");
/** What the stand-in recorded of each call of a run, in the order it answered them. */
function ghCalls(run: string): { argv: string[]; route: string | null; exit: number }[] {
  return readFileSync(path.join(run, "calls.jsonl"), "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as { argv: string[]; route: string | null; exit: number });
}

/** The query the page gives for the counted set at a named head, as a role sends it. */
const RED_ROLLUP_QUERY =
  'query($oid:GitObjectID!){repository(owner:"example",name:"widgets"){object(oid:$oid){' +
  "... on Commit{statusCheckRollup{state contexts(first:100){totalCount pageInfo{hasNextPage} " +
  "nodes{__typename ... on CheckRun{name databaseId status conclusion " +
  "checkSuite{app{databaseId} workflowRun{event databaseId}}} " +
  "... on StatusContext{context state}}}}}}}}";
/** One GraphQL read of the counted set at the head, with `jq` over the answer. */
const rollupRead = (run: string, filter: string) =>
  gh(run, [
    "api",
    "graphql",
    "-F",
    `oid=${RED_HEAD}`,
    "-f",
    `query=${RED_ROLLUP_QUERY}`,
    "--jq",
    filter,
  ]);

/** The four reads of the head the page has a role make, each as a verdict per check: the three
 * reads of the counted set — the rollup of `gh pr view`, the rows of `gh pr checks` and the
 * GraphQL rollup of the head — and the check runs of the commit, which answer every run at it,
 * the latest id of a name being the one the counted set shows. */
async function readHead(run: string) {
  const rollup = await gh(run, [
    "pr",
    "view",
    String(RED_PR),
    "--json",
    "statusCheckRollup",
    "--jq",
    ".statusCheckRollup[] | [.name,.conclusion] | @tsv",
  ]);
  const graphql = await rollupRead(
    run,
    ".data.repository.object.statusCheckRollup.contexts.nodes[] | [.name,.conclusion] | @tsv"
  );
  const checks = await gh(run, ["pr", "checks"]);
  const runs = await gh(run, [
    "api",
    "--paginate",
    `repos/${RED_REPO}/commits/${RED_HEAD}/check-runs?filter=all&per_page=100`,
    "--jq",
    ".check_runs[] | [.id,.name,.conclusion] | @tsv",
  ]);
  const latest: Record<string, { id: number; verdict: string }> = {};
  for (const line of outputLines(runs.stdout)) {
    const [id, name, conclusion] = line.split("\t");
    const seen = latest[name];
    const entry = { id: Number(id), verdict: conclusion === "success" ? "pass" : "fail" };
    if (seen === undefined || seen.id < entry.id) latest[name] = entry;
  }
  const verdict = (stdout: string, pass: string) =>
    Object.fromEntries(
      outputLines(stdout).map((line) => {
        const [name, answer] = line.split("\t");
        return [name, answer === pass ? "pass" : "fail"];
      })
    );
  return {
    rollup: verdict(rollup.stdout, "SUCCESS"),
    graphql: verdict(graphql.stdout, "SUCCESS"),
    checks: verdict(checks.stdout, "pass"),
    checksExit: checks.exit,
    checkRuns: Object.fromEntries(
      Object.entries(latest).map(([name, { verdict: answer }]) => [name, answer])
    ),
    checkRunRows: outputLines(runs.stdout),
  };
}
/** The verdict the head carries under each check, before the re-run and after it. */
function headIs(world: RedWorld, when: "before" | "after"): Record<string, string> {
  const { check, after } = RED_WORLD[world];
  const other: Record<string, string> = { lint: "pass", typecheck: "pass" };
  // The spent head's e2e check: an earlier run of it failed, and the one that replaced it passed.
  if (world === "e") other.e2e = "pass";
  return { ...other, [check]: when === "after" && after === "green" ? "pass" : "fail" };
}
/** What the commit's check runs answer, which is headIs but for world `k`: the by-hand run's
 * failure of the same check name is the latest run of it there, before the re-run and after it,
 * and the counted set holds neither. */
function runsAre(world: RedWorld, when: "before" | "after"): Record<string, string> {
  if (world !== "k") return headIs(world, when);
  return { ...headIs(world, when), [RED_WORLD[world].check]: "fail" };
}
/** The settlement the Envoy listener renders for a state of the head, as red_fixture writes it
 * for the re-run to publish and for the rig's own report of the check: `notify` is the id of the
 * run of `test (notify)` that state holds, the red's own or the attempt the re-run made. */
const settlement = (notify: number, passed: string[], failed: string[]) => ({
  kind: "checks",
  repo: RED_REPO,
  number: String(RED_PR),
  sha: RED_HEAD,
  check_runs: [
    { name: "lint", id: RED_ID.lint },
    { name: "test (notify)", id: notify },
    { name: "typecheck", id: RED_ID.typecheck },
  ],
  generation: 2,
  snapshot: RED_HEAD.slice(0, 8),
  failed: { count: failed.length, checks: failed },
  running: { count: 0, checks: [] },
  passed: { count: passed.length, checks: passed },
  queued: { count: 0, checks: [] },
  cancelled: { count: 0, checks: [] },
  skipped: { count: 0, checks: [] },
  failing_checks: failed.map((name) => ({ name, url: RED_RUN_URL })),
});
/** That settlement as the envelope a listener receives it in. */
const envelope = (notify: number, passed: string[], failed: string[]) => ({
  authorization: "Bearer rig-envoy-token",
  url: "/v1/messages/publish",
  body: {
    topic: `notifications.github.example.widgets.pr.${RED_PR}.checks`,
    source: "github",
    message:
      `checks settled on ${RED_REPO}#${RED_PR} @ ${RED_HEAD.slice(0, 7)}: ` +
      `${passed.length} passed, ${failed.length} failed, 0 cancelled, 0 skipped`,
    payload: JSON.stringify(settlement(notify, passed, failed)),
  },
});

/** The rig reporting world `l`'s check between the two turns (red_report), with `listener` as
 * the Envoy the batch's services_env names. The shell runs as a child this test awaits, never a
 * synchronous one, so the listener of the row answers the publish while it runs. */
async function redReport(run: string, listener: string) {
  const token = path.join(run, "envoy-token");
  writeFileSync(token, "rig-envoy-token\n");
  const services = path.join(run, "services.env");
  writeFileSync(services, `ENVOY_URL=${listener}\nENVOY_TOKEN_FILE=${token}\n`);
  const child = Bun.spawn(
    [
      "bash",
      "-c",
      `${fn("fail")}\n${fn("note")}\n${fn("red_report")}\nR=$1 services_env=$2\nred_report`,
      "-",
      run,
      services,
    ],
    { env: childEnv, stdout: "pipe", stderr: "pipe" }
  );
  const [stderr, exit] = await Promise.all([new Response(child.stderr).text(), child.exited]);
  if (exit !== 0) throw new Error(`red_report: ${stderr}`);
}

describe("red_fixture", () => {
  // One row per world, because each world is a seed of its own and a disagreement must name the
  // world it is in. Catches: a world whose reads of the head disagree (a rollup that stays green
  // under a red `gh pr checks`, or a GraphQL rollup that answers the review threads of
  // worker_fixture), a read that answers the same state twice, a settlement that never lands, and
  // a `gh pr checks` whose status does not follow its rows. The counted set and the commit
  // endpoint answer the same checks in every world but `k`, whose commit endpoint holds the
  // by-hand run as well (runsAre). World `l` reports no red at all until the rig reports it, so
  // it has rows of its own below rather than a place in this one.
  const settling = RED_WORLDS.filter((world) => world !== "l");
  // Each world's head is read twice before the re-run, which is both that every read answers the
  // world's own red and the defect the first live run of world a measured: the head's second read
  // in a turn answered the settlement although the role had pressed nothing, so the role read the
  // head as already re-run and pressed no re-run of its own. The head turns on the act, never on
  // the read (gh-standin.ts, `follows`), and the re-run the role presses after its reads still
  // turns it.
  test.each([
    ...settling,
  ])("world %s: reading the head again, with no re-run between, answers red again", async (world) => {
    const run = redFixture(world);

    const first = await readHead(run);
    const again = await readHead(run);
    for (const read of [first, again]) {
      expect({
        rollup: read.rollup,
        graphql: read.graphql,
        checks: read.checks,
        runs: read.checkRuns,
      }).toEqual({
        rollup: headIs(world, "before"),
        graphql: headIs(world, "before"),
        checks: headIs(world, "before"),
        runs: runsAre(world, "before"),
      });
      expect(read.checksExit).toBe(1);
    }

    const rerun = await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    expect([rerun.stdout, rerun.exit]).toEqual(["✓ Requested rerun of failed jobs\n", 0]);
    const after = await readHead(run);
    expect({
      rollup: after.rollup,
      graphql: after.graphql,
      checks: after.checks,
      runs: after.checkRuns,
    }).toEqual({
      rollup: headIs(world, "after"),
      graphql: headIs(world, "after"),
      checks: headIs(world, "after"),
      runs: runsAre(world, "after"),
    });
    expect(after.checksExit).toBe(RED_WORLD[world].after === "green" ? 0 : 1);
  });

  // Catches: a world `e` whose head's runs carry no second attempt, which is where the page
  // reads the spend, and a second attempt in every world, which would spend the re-run of the
  // eleven others.
  test("the head's runs answer attempt 1 in every world, and the spent head's attempt 2", async () => {
    const seen: Record<string, string[]> = {};
    for (const world of RED_WORLDS) {
      const runs = await gh(redFixture(world), [
        "api",
        `repos/${RED_REPO}/actions/runs?head_sha=${RED_HEAD}&event=pull_request&per_page=100`,
        "--jq",
        ".workflow_runs[] | [.id,.name,.run_attempt,.conclusion] | @tsv",
      ]);
      seen[world] = outputLines(runs.stdout);
    }
    const red = `${RED_RUN_ID}\tci\t1\tfailure`;
    expect(seen).toEqual({
      ...Object.fromEntries(RED_WORLDS.map((world) => [world, [red]])),
      e: [red, `${RED_RUN_ID - 1}\te2e\t2\tsuccess`],
    });
  });

  // The commit endpoint of world `e` keeps the superseded e2e runs, which the page lets no role
  // judge the spend on. Catches: a seed that moved them out with the spend, which would leave the
  // world unable to tell a role judging there from one judging on the head's runs, and one that
  // carries them in every world.
  test("the spent head alone carries the superseded runs the commit endpoint answers", async () => {
    const spent = await readHead(redFixture("e"));
    const fresh = await readHead(redFixture("a"));
    expect(spent.checkRunRows).toEqual([
      `${RED_ID.lint}\tlint\tsuccess`,
      `${RED_ID.typecheck}\ttypecheck\tsuccess`,
      `${RED_RED_ID - 4}\te2e\tfailure`,
      `${RED_RED_ID - 3}\te2e\tsuccess`,
      `${RED_RED_ID}\ttest (notify)\tfailure`,
    ]);
    expect(fresh.checkRunRows).toEqual([
      `${RED_ID.lint}\tlint\tsuccess`,
      `${RED_ID.typecheck}\ttypecheck\tsuccess`,
      `${RED_RED_ID}\ttest (notify)\tfailure`,
    ]);
  });

  // Catches: a title check whose output names another title than the pull request's own, which
  // the role reads as a red of some other change.
  test("the title check's output names the pull request's own title", async () => {
    const run = redFixture("f");
    const title = await gh(run, [
      "pr",
      "view",
      String(RED_PR),
      "--json",
      "title",
      "--jq",
      ".title",
    ]);
    const summary = await gh(run, [
      "api",
      `repos/${RED_REPO}/commits/${RED_HEAD}/check-runs?filter=all`,
      "--jq",
      '.check_runs[] | select(.name == "title") | .output.summary',
    ]);
    const log = await gh(run, ["run", "view", String(RED_RUN_ID), "--log-failed"]);
    const said = `The title "${RED_TITLE}" has no scope: the pattern is <type>(<scope>): <subject>.`;
    expect(title.stdout).toBe(`${RED_TITLE}\n`);
    expect(summary.stdout).toBe(`${said}\n`);
    expect(log.stdout).toBe(`${said}\n`);
  });

  // Catches: a world h whose log read answers a line like every other world's, which the page's
  // "a read that fails is recorded as failed" has nothing to record.
  test("world h's log read fails with its error, where world a's answers the failing line", async () => {
    const failed = await gh(redFixture("h"), ["run", "view", String(RED_RUN_ID), "--log-failed"]);
    const read = await gh(redFixture("a"), ["run", "view", String(RED_RUN_ID), "--log-failed"]);
    expect([failed.exit, failed.stdout]).toEqual([1, ""]);
    expect(failed.stderr).toBe(
      "failed to get run log: HTTP 410: Gone " +
        "(https://api.github.com/repos/example/widgets/actions/runs)\n"
    );
    expect([read.exit, read.stderr]).toEqual([0, ""]);
    expect(read.stdout).toBe("(fail) notify > retries a refused send [3.00ms]\n");
  });

  // Catches: a counted set in a shape of the rig's own, which a role that reads it as GitHub
  // answers one — the state, the one page of contexts, and a CheckRun node per check — would
  // read as nothing at all, and a node that names no run, which leaves the role nothing to
  // re-run.
  test("the GraphQL rollup answers the counted set in the shape GitHub answers", async () => {
    const run = redFixture("a");
    const answer = await rollupRead(run, ".data.repository.object.statusCheckRollup");
    expect(JSON.parse(answer.stdout)).toEqual({
      state: "FAILURE",
      contexts: {
        totalCount: 3,
        pageInfo: { hasNextPage: false },
        nodes: [
          {
            __typename: "CheckRun",
            name: "lint",
            databaseId: RED_ID.lint,
            status: "COMPLETED",
            conclusion: "SUCCESS",
            checkSuite: { workflowRun: { event: "pull_request", databaseId: RED_RUN_ID } },
          },
          {
            __typename: "CheckRun",
            name: "test (notify)",
            databaseId: RED_RED_ID,
            status: "COMPLETED",
            conclusion: "FAILURE",
            checkSuite: { workflowRun: { event: "pull_request", databaseId: RED_RUN_ID } },
          },
          {
            __typename: "CheckRun",
            name: "typecheck",
            databaseId: RED_ID.typecheck,
            status: "COMPLETED",
            conclusion: "SUCCESS",
            checkSuite: { workflowRun: { event: "pull_request", databaseId: RED_RUN_ID } },
          },
        ],
      },
    });
  });

  // Catches: a world k whose by-hand failure is of another check name, which no role would read
  // as the head's one re-run already spent, and one the counted set holds too, which would make
  // the two reads agree and leave the world measuring nothing.
  test("world k: the commit endpoint answers a by-hand failure the counted set does not hold", async () => {
    const run = redFixture("k");
    const before = await readHead(run);
    // Two failing runs of the one check name at the commit, the second a by-hand run's, against
    // one counted run of it.
    expect(before.checkRunRows).toEqual([
      `${RED_ID.lint}\tlint\tsuccess`,
      `${RED_ID.typecheck}\ttypecheck\tsuccess`,
      `${RED_RED_ID}\ttest (notify)\tfailure`,
      `${RED_RED_ID + 2}\ttest (notify)\tfailure`,
    ]);
    expect(before.graphql).toEqual(headIs("k", "before"));
    expect(before.checks).toEqual(headIs("k", "before"));
    const counted = await rollupRead(
      run,
      ".data.repository.object.statusCheckRollup.contexts.nodes " +
        '| map(select(.name == "test (notify)")) | [length, .[0].databaseId]'
    );
    expect(JSON.parse(counted.stdout)).toEqual([1, RED_RED_ID]);
    // The by-hand run is a run of its own at that head, which is what puts it outside the set
    // GitHub counts for the pull request.
    const byHand = await gh(run, [
      "api",
      `repos/${RED_REPO}/commits/${RED_HEAD}/check-runs?filter=all`,
      "--jq",
      `.check_runs[] | select(.id == ${RED_RED_ID + 2}) | [.html_url, .output.summary] | @tsv`,
    ]);
    expect(byHand.stdout).toBe(
      `https://github.com/${RED_REPO}/actions/runs/${RED_RUN_ID + 1}\t` +
        "A by-hand workflow_dispatch run of test (notify) failed.\n"
    );
    // The re-run clears the counted run and leaves the by-hand one, which is why a role that
    // reads the commit endpoint for the head's state reads it red after a green re-run.
    await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    const after = await readHead(run);
    expect({ graphql: after.graphql, runs: after.checkRuns }).toEqual({
      graphql: headIs("k", "after"),
      runs: runsAre("k", "after"),
    });
  });

  // Catches: a world l whose counted set reports the red from the start, which the role could
  // decide on in its first turn, and one whose commit endpoint holds no failing run, which would
  // leave the role nothing to wait on. red_report is the rig reporting the check between the
  // turns: the counted set reports it, the run learns when, and the settlement wakes the role.
  test("world l: the counted set reports the red only once the rig reports it", async () => {
    const run = redFixture("l");
    const unreported = await readHead(run);
    expect({
      rollup: unreported.rollup,
      graphql: unreported.graphql,
      checks: unreported.checks,
    }).toEqual({
      rollup: { lint: "pass", typecheck: "pass" },
      graphql: { lint: "pass", typecheck: "pass" },
      checks: { lint: "pass", typecheck: "pass" },
    });
    expect(unreported.checksExit).toBe(0);
    // The commit endpoint holds the failing run all along, which is the read the page forbids
    // deciding on.
    expect(unreported.checkRunRows).toEqual([
      `${RED_ID.lint}\tlint\tsuccess`,
      `${RED_ID.typecheck}\ttypecheck\tsuccess`,
      `${RED_RED_ID}\ttest (notify)\tfailure`,
    ]);

    const received: { authorization: string | null; url: string; body: unknown }[] = [];
    const server = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      fetch: async (request) => {
        received.push({
          authorization: request.headers.get("authorization"),
          url: new URL(request.url).pathname,
          body: await request.json(),
        });
        return Response.json({ ok: true });
      },
    });
    const at = new Date();
    try {
      await redReport(run, `http://127.0.0.1:${server.port}`);
    } finally {
      server.stop(true);
    }
    expect(received).toEqual([envelope(RED_RED_ID, ["lint", "typecheck"], ["test (notify)"])]);
    const reportedAt = readFileSync(path.join(run, "reported-at"), "utf8");
    expect(reportedAt).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\n$/);
    expect(new Date(reportedAt.trim()).getTime()).toBeGreaterThanOrEqual(
      Math.floor(at.getTime() / 1000) * 1000
    );

    const reported = await readHead(run);
    expect({ rollup: reported.rollup, graphql: reported.graphql, checks: reported.checks }).toEqual(
      {
        rollup: headIs("l", "before"),
        graphql: headIs("l", "before"),
        checks: headIs("l", "before"),
      }
    );
    expect(reported.checksExit).toBe(1);

    const rerun = await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    expect(rerun.exit).toBe(0);
    const after = await readHead(run);
    expect({
      rollup: after.rollup,
      graphql: after.graphql,
      checks: after.checks,
      runs: after.checkRuns,
    }).toEqual({
      rollup: headIs("l", "after"),
      graphql: headIs("l", "after"),
      checks: headIs("l", "after"),
      runs: runsAre("l", "after"),
    });
    expect(after.checksExit).toBe(0);
  });

  // The routes are the world's in their content alone, so one row runs the page's six calls in
  // every world. Catches: a call of the page that falls through to the stand-in's 404, a read of
  // the counted set answered by worker_fixture's generic graphql route, which holds a review
  // thread list, a check-runs read whose `filter=all` query misses the head's own route and takes
  // the generic one, a jobs read answered by the route for the head's runs, and an aggregate
  // world whose jobs name no failed lane.
  test("every call the page names is answered by its own route, in every world", async () => {
    const seen: unknown[] = [];
    for (const world of RED_WORLDS) {
      const run = redFixture(world);
      await gh(run, [
        "api",
        `repos/${RED_REPO}/actions/runs?head_sha=${RED_HEAD}&event=pull_request`,
        "--jq",
        ".workflow_runs[] | [.id,.event,.conclusion] | @tsv",
      ]);
      await rollupRead(run, ".data.repository.object.statusCheckRollup.state");
      const jobs = await gh(run, [
        "api",
        `repos/${RED_REPO}/actions/runs/${RED_RUN_ID}/jobs`,
        "--jq",
        ".jobs[] | [.id,.name,.conclusion] | @tsv",
      ]);
      await gh(run, ["run", "view", String(RED_RUN_ID), "--log-failed"]);
      await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
      await gh(run, [
        "api",
        "--paginate",
        `repos/${RED_REPO}/commits/${RED_HEAD}/check-runs?filter=all&per_page=100`,
        "--jq",
        ".check_runs[] | [.id,.name,.conclusion] | @tsv",
      ]);
      seen.push({ world, routes: ghCalls(run).map((call) => call.route), jobs: jobs.stdout });
    }
    expect(seen).toEqual(
      RED_WORLDS.map((world) => ({
        world,
        routes: [
          "^gh api .*actions/runs",
          "^gh api .*graphql.*statusCheckRollup",
          `^gh api .*actions/runs/${RED_RUN_ID}/jobs`,
          "^gh run view .*--log-failed",
          "^gh run rerun",
          `^gh api .*commits/${RED_HEAD}/check-runs`,
        ],
        jobs:
          `${RED_JOB_ID}\t${RED_WORLD[world].job}\tfailure\n` +
          `${RED_JOB_ID + 1}\tlint\tsuccess\n`,
      }))
    );
  });

  // Catches: a re-run that publishes nothing, which leaves a waiting role asleep, and a
  // settlement whose payload names another head, another pull request or another verdict than
  // the one the role pressed the re-run at.
  test("the re-run publishes the settlement of the head, as the fixture wrote it", async () => {
    const received: { authorization: string | null; url: string; body: unknown }[] = [];
    const server = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      fetch: async (request) => {
        received.push({
          authorization: request.headers.get("authorization"),
          url: new URL(request.url).pathname,
          body: await request.json(),
        });
        return Response.json({ ok: true });
      },
    });
    try {
      for (const world of ["a", "b"] as const) {
        const run = redFixture(world);
        const token = path.join(run, "envoy-token");
        writeFileSync(token, "rig-envoy-token\n");
        const rerun = await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"], {
          ENVOY_URL: `http://127.0.0.1:${server.port}`,
          ENVOY_TOKEN_FILE: token,
        });
        expect(rerun.exit).toBe(0);
      }
    } finally {
      server.stop(true);
    }
    expect(received).toEqual([
      envelope(RED_RED_ID + 1, ["lint", "test (notify)", "typecheck"], []),
      envelope(RED_RED_ID + 1, ["lint", "typecheck"], ["test (notify)"]),
    ]);
  });

  // Catches: a world.json that loses the world, its check, its settlement, the failing line or
  // the failing test's name, each of which the scorer reads the run's answers against (score.ts,
  // redAtTheHead: the record is judged against `log` and the role's `dispatch search` against
  // `test`).
  test("world.json carries the world, its check, its job, its settlement and its failing line", () => {
    const run = redFixture("g");
    expect(JSON.parse(readFileSync(path.join(run, "world.json"), "utf8"))).toEqual({
      key: RED_KEY,
      repo: RED_REPO,
      pr: RED_PR,
      branch: `legion/${RED_KEY}`,
      head: RED_HEAD,
      world: "g",
      check: "ci",
      job: "test (notify)",
      after: "green",
      run_id: RED_RUN_ID,
      job_id: RED_JOB_ID,
      log: "(fail) notify > retries a refused send [3.00ms]",
      test: "notify > retries a refused send",
    });
  });
});

// run_red_at_the_head with every process it starts stood in for: the run's Dispatch, the seeds,
// both fixtures, the rig's own report of world `l`'s check (red_report), the daemon stand-in
// (start_process), the worker pane (`bun`) and the turns (launch). Each stand-in records its
// call, so a row reads what the run passed on.
describe("run_red_at_the_head", () => {
  const standIns = `
own_dispatch() { dispatch_port=5555; }
seed_ts() {
  printf 'seed %s\\n' "$*" >>"$R/calls"
  [ "$1" != tester-proof ] || printf '{"issue":"${RED_KEY}","implement":{"version":4}}\\n' >"$2"
}
worker_fixture() {
  printf 'worker_fixture %s\\n' "$*" >>"$R/calls"
  jq -n '{pr:${RED_PR}, head:"${RED_HEAD}", branch:"legion/${RED_KEY}"}' >"$R/world.json"
}
red_fixture() { printf 'red_fixture %s\\n' "$*" >>"$R/calls"; }
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
red_report() { printf 'red_report\\n' >>"$R/calls"; }`;

  /** One run of `world`, and what it took: the world it wrote down, the role each worker it
   * started was given, the turns it ran and what woke each, and whether the rig reported the
   * check between them. */
  function redRun(world: RedWorld) {
    const run = freshDir(`red-run-${world}`);
    mkdirSync(path.join(run, "logs"));
    const result = bash(
      [
        fn("fail"),
        fn("note"),
        fn("run_red_at_the_head"),
        standIns,
        "R=$1",
        "work=$R/work",
        "P=$work/profiles/s1",
        'mkdir -p "$P"',
        "profile=$P/home",
        "co=/checkout",
        "here=/rig",
        `worker_repo=${RED_REPO}`,
        "services_env=$R/services.env",
        ': >"$services_env"',
        'run_red_at_the_head "red-at-the-head-$2-s1-2" 2 3 1740000000 "$2"',
      ].join("\n"),
      [run, world]
    );
    if (result.code !== 0) throw new Error(`run_red_at_the_head ${world}: ${result.stderr}`);
    const calls = readFileSync(path.join(run, "calls"), "utf8").trimEnd().split("\n");
    return {
      world: readFileSync(path.join(run, "world.txt"), "utf8").trim(),
      // The daemon stand-in takes the role last, the worker pane before its checkout.
      daemons: calls
        .filter((call) => call.includes("daemon-standin.ts"))
        .map((call) => call.split(" ").at(-1)),
      panes: calls
        .filter((call) => call.includes("worker-pane.ts"))
        .map((call) => call.split(" ").at(-2)),
      turns: readFileSync(path.join(run, "turns.txt"), "utf8").trimEnd().split("\n"),
      outs: ["out-1.txt", "out-2.txt"].filter((out) => existsSync(path.join(run, out))),
      reported: calls.includes("red_report"),
    };
  }

  /** One line of turns.txt: the turn, what woke it, the moment it ended and its exit line. */
  const turnLine = (turn: number, wake: string) =>
    expect.stringMatching(
      new RegExp(`^${turn} ${wake} \\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}Z exit=0$`)
    );

  // Catches: a run that boots the tester in a reviewer's world, one that boots both roles, a
  // world.txt the scorer reads as another world than the one the run ran, a world that takes a
  // turn it owes no wake for, and a world `l` whose second turn is a re-run's settlement rather
  // than the rig's own report of the check.
  test("boots one worker, takes each world's turns, and reports world l's check between them", () => {
    expect(RED_WORLDS.map(redRun)).toEqual(
      RED_WORLDS.map((world) => {
        const role = world === "i" || world === "j" ? "reviewer" : "tester";
        // `d`, `e` and `f` expect no re-run and take one turn; `l` takes its second on the rig's
        // report of the check, and every other world on the settlement of the re-run it pressed.
        const one = world === "d" || world === "e" || world === "f";
        return {
          world,
          daemons: [role],
          panes: [role],
          turns: one
            ? [turnLine(1, "assignment")]
            : [turnLine(1, "assignment"), turnLine(2, world === "l" ? "report" : "settlement")],
          outs: one ? ["out-1.txt"] : ["out-1.txt", "out-2.txt"],
          reported: world === "l",
        };
      })
    );
  });
});

// cmd_run and cmd_batch read SKILL_SCENARIOS_RED_WORLD: the run function is stood in for, so a
// row reads the name the command made and what it refused.
describe("a red-at-the-head run's world", () => {
  const runStandIns = `
load_label() { P=$work/profiles/$1; mkdir -p "$P"; printf 'commit\\n' >"$P/built-from"; }
on_exit() { :; }
run_red_at_the_head() {
  printf 'run_red_at_the_head %s\\n' "$*" >>"$work/calls"
  printf 'exit=0\\n' >"$R/out.txt"
}`;
  const run = (work: string, world: string | undefined, n: string) =>
    bash(
      [
        fn("fail"),
        fn("cmd_run"),
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
    expect(run(work, "a", "1").stdout).toBe("red-at-the-head-a-s1-1: exit=0\n");
    expect(run(work, "b", "1").stdout).toBe("red-at-the-head-b-s1-1: exit=0\n");
    expect(readFileSync(path.join(work, "calls"), "utf8")).toBe(
      "run_red_at_the_head red-at-the-head-a-s1-1 1 3 1740000000 a\n" +
        "run_red_at_the_head red-at-the-head-b-s1-1 1 3 1740000000 b\n"
    );
    expect(existsSync(path.join(work, "runs/red-at-the-head-a-s1-1"))).toBe(true);
    expect(existsSync(path.join(work, "runs/red-at-the-head-b-s1-1"))).toBe(true);
  });

  // Catches: a run of a world red_fixture has no red for, which would fail inside the run with
  // its services up, and a run named `red-at-the-head--s1-1` when the variable is unset.
  test("cmd_run refuses a world that is not one of the twelve, naming them, and makes no run", () => {
    const work = freshDir("red-run-refused");
    const named = "not a, b, c, d, e, f, g, h, i, j, k or l";
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
services_up() { printf 'services_up\\n' >>"$work/calls"; }`;
  function batch(world: string | undefined) {
    const work = freshDir("red-batch-work");
    const here = freshDir("red-batch-here");
    writeFileSync(
      path.join(here, "rig.sh"),
      `#!/usr/bin/env bash\nprintf 'run %s\\n' "$*" >>"${path.join(work, "calls")}"\n`
    );
    const result = bash(
      [
        fn("fail"),
        fn("cmd_batch"),
        batchStandIns,
        "work=$1",
        "here=$2",
        "shift 2",
        'cmd_batch "$@"',
      ].join("\n"),
      [work, here, "red-at-the-head", "2", "s1"],
      world === undefined ? {} : { SKILL_SCENARIOS_RED_WORLD: world }
    );
    const calls = existsSync(path.join(work, "calls"))
      ? readFileSync(path.join(work, "calls"), "utf8").trimEnd().split("\n")
      : [];
    // The batch's start time is the clock's, so each run line is read without it.
    return { ...result, calls: calls.map((line) => line.split(" ").slice(0, 5).join(" ")) };
  }

  // Catches: a batch that starts its services and only then finds it has no world, and a gate
  // that refuses the twelve worlds too.
  test("cmd_batch refuses such a world before it locks or starts a service", () => {
    const refused = batch("world-a");
    expect(refused.code).toBe(1);
    expect(refused.stderr).toBe(
      "skill-scenarios: a red-at-the-head batch needs SKILL_SCENARIOS_RED_WORLD set to " +
        "a, b, c, d, e, f, g, h, i, j, k or l (got 'world-a')\n"
    );
    expect(refused.calls).toEqual([]);
    const ran = batch("l");
    expect(ran.code).toBe(0);
    expect(ran.calls).toEqual([
      "load_label s1",
      "lock_services",
      "services_up",
      "run run red-at-the-head s1 1",
      "run run red-at-the-head s1 2",
    ]);
  });
});
