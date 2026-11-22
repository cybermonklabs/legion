// The red-at-the-head worlds the rig's tests build with the library red-at-the-head.sh, and what
// the tests know of them: red-at-the-head.test.ts, which tests the library, builds them, and so
// does rig.test.ts, whose run_red_at_the_head rows build each reviewer's variant beside the world
// it varies. This is no test file, so bun runs no suite of it. It registers no hook and makes no
// directory: bun evaluates a module once per process, so a hook here would land in the scope of
// the first file that imported it (@legion/test-fixtures/temporary-directories says so), and each
// importer hands redWorlds the directory maker and the environment of its own.
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { scriptFunctions } from "../../../../scripts/e2e/lib/script-functions";

/** The issue of every built world's pull request, whose key names its branch. */
export const RED_KEY = "LWEVAL-9";

/** The reviewer's variants (red_world's, `m` to `t`), each by the world it varies: that world's red
 * and every answer of it, with the reviewer booted in `reviewing` in place of the tester, as `i`
 * and `j` are `a` and `b`'s, and the facts of RED_VARIANT_FACTS. */
export const RED_VARIANTS = {
  m: "c",
  n: "d",
  o: "e",
  p: "f",
  q: "g",
  r: "h",
  s: "k",
  t: "l",
} as const;
type RedVariant = keyof typeof RED_VARIANTS;
export const RED_VARIANT_WORLDS = Object.keys(RED_VARIANTS) as RedVariant[];
/** The facts a variant carries over those of the world it varies (red_world), so that the red the
 * reviewer meets is on a check the base requires: the aggregate check `ci`, red over the red lane
 * as in `i` and `j`, or in `p`, whose red is the title check in a run of its own, green over lint
 * and typecheck with the title check required beside it. `q`'s red is that aggregate itself,
 * `g`'s, so it carries no other. */
export const RED_VARIANT_FACTS: Record<RedVariant, Record<string, unknown>> = {
  m: { aggregate: true },
  n: { aggregate: true },
  o: { aggregate: true },
  p: { aggregate: true, required: ["ci", "title"] },
  q: {},
  r: { aggregate: true },
  s: { aggregate: true },
  t: { aggregate: true },
};
/** The world a variant varies, and any other world itself. */
export const baseOf = (world: string): string =>
  world in RED_VARIANTS ? RED_VARIANTS[world as RedVariant] : world;
/** The twelve worlds `a` to `l`: the worlds of the rig's one table (jq/worlds.json) less the
 * reviewer's variants of eight of them. */
export const RED_WORLDS = Object.keys(
  JSON.parse(readFileSync(path.join(import.meta.dir, "jq", "worlds.json"), "utf8")).worlds
).filter((world) => !(world in RED_VARIANTS));

/** rig.sh's note and fail, which the library leaves to its caller. */
const rig = scriptFunctions(path.join(import.meta.dir, "rig.sh"));
/** What a script over the library runs first: the library, rig.sh's note and fail, and jj stood in
 * for. */
export const sourced = `. ${JSON.stringify(path.join(import.meta.dir, "red-at-the-head.sh"))}\n${rig("note")}\n${rig("fail")}\njj() { :; }`;

/** The variables every child process of a rig test file gets, rig.test.ts's and
 * red-at-the-head.test.ts's alike: the caller's PATH, `home` as HOME (each file's own), the
 * caller's mise data and config directories when the caller names them, and PUBLIC_LEAK_CHECK and
 * LEGION_LEAK_CHECK_TAG when the caller has them. A tool on PATH can be a mise shim (jq and jj
 * are, on a devbox), and under a fresh HOME such a shim finds neither its installs nor its global
 * config, so it fails: make_omp_home keeps the operator's two directories for the processes it
 * starts for the same reason, and base_env passes MISE_DATA_DIR to every agent. A caller that
 * names neither gets neither, so the developer's own home decides nothing. A row that builds a
 * world through worker_fixture pushes into its bare remote, and on a Legion pod that push goes
 * through the pod's git wrapper, whose pre-push leak guard reads the two leak-check variables. */
export function childEnvironment(home: string): Record<string, string> {
  const miseData =
    process.env.MISE_DATA_DIR ??
    (process.env.XDG_DATA_HOME === undefined
      ? undefined
      : path.join(process.env.XDG_DATA_HOME, "mise"));
  return {
    PATH: process.env.PATH ?? "",
    HOME: home,
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
}

/** Runs SCRIPT under bash after `sourced`, with ARGS as $1... and ENV as its environment. */
export function runSourced(script: string, args: string[], env: Record<string, string>) {
  const result = Bun.spawnSync(["bash", "-c", `${sourced}\n${script}`, "-", ...args], { env });
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

/** The worlds one test file builds: each in a directory `freshDir` makes under the file's own
 * work directory, the library run by bash in `env`, the file's child environment. */
export function redWorlds(freshDir: (name: string) => string, env: Record<string, string>) {
  /** A run directory whose world is built as a run builds it: the world's repository and base
   * (world_repository), the one check of its Verification document as seed.ts tester-proof writes
   * it to fixture.json for that world (world_check_when; the implementer's result beside a
   * `before_merge` check), worker_fixture's green pull request, its repository, its workflows and
   * its routes, and, where `world` names one, red_fixture's red over it, all the library's own,
   * with jj stood in for; in `run` where the caller gives one. `facts`, where given, stands over
   * the world's own facts in the table, so every reader of the table builds the world with them (a
   * reviewer's variant's facts over the world it varies). */
  function builtWorld(
    world: string,
    run = freshDir(`built-${world}`),
    facts?: Record<string, unknown>
  ): string {
    const result = runSourced(
      [
        "R=$1 worker_key=$2",
        'world=$3; [ "$world" != green ] || world=',
        // The facts that stand over the world's own: the table answers the world's entry with them
        // added. That table is written to a directory of the run's own, which red_jq then puts
        // ahead of the library's jq/ on jq's library path, so worlds.jq imports it in place of
        // jq/worlds.json, and red_world and red_workflows, the table's readers, read the facts.
        "rig_test_world=$3 rig_test_facts=$4",
        'if [ -n "$rig_test_facts" ]; then',
        '  mkdir "$R/rig-test-jq" || exit 1',
        `  jq --arg world "$rig_test_world" --argjson facts "$rig_test_facts" '.worlds[$world] += $facts' "$red_dir/jq/worlds.json" >"$R/rig-test-jq/worlds.json" || exit 1`,
        '  red_jq() { jq -L "$R/rig-test-jq" -L "$red_dir/jq" "$@"; }',
        "fi",
        "world_repository",
        'when=$(world_check_when "$world")',
        `if [ "$when" = before_merge ]; then result=',"result":{"role":"implementer","verdict":"pass","observed":"prints Hello, Ada! and exits 0"}'; else result=; fi`,
        `printf '{"issue":"%s","checks":[{"id":"k1","when":"%s"%s}]}\\n' "$worker_key" "$when" "$result" >"$R/fixture.json"`,
        'worker_fixture 3 1740000000 "$world" && { [ -z "$world" ] || red_fixture "$world"; }',
      ].join("\n"),
      [run, RED_KEY, world, facts === undefined ? "" : JSON.stringify(facts)],
      env
    );
    if (result.code !== 0) throw new Error(`the world ${world} failed: ${result.stderr}`);
    return run;
  }
  /** A run directory whose fixtures.json and world.json red_fixture has turned red over `world`,
   * over the world worker_fixture really writes, built for the one row that calls it: the world of
   * a row that writes into it, which presses the re-run, posts a review, a comment or an edit,
   * names a listener or has the rig report the check, or reads what its own calls leave behind
   * (calls.jsonl, fixtures.json's counts). */
  const ownFixture = (world: string) => builtWorld(world);
  /** The same world for the rows that only read it, built once per world for the whole file and
   * shared, so the file builds a world it reads once rather than once a row. A shared world is
   * marked (the file <run>/shared), and the gh stand-in refuses any call that would write it,
   * naming the world (gh-standin.ts, `writes`): a read writes nothing, and a press, a post, an edit
   * or a read of a route whose count something reads would change what a later row reads, so the
   * order of rows would decide results, as it did at 7bf70a93, where every call wrote the file back
   * and a row that only read a shared world changed the file a later row compared (the eight
   * variant rows, CI run 44797028466). A row that writes builds a world of its own (ownFixture). */
  const sharedWorlds = new Map<string, string>();
  const redFixture = (world: string) => {
    let built = sharedWorlds.get(world);
    if (built === undefined) {
      built = builtWorld(world);
      writeFileSync(path.join(built, "shared"), "");
      sharedWorlds.set(world, built);
    }
    return built;
  };
  return { builtWorld, ownFixture, redFixture };
}
