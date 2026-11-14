import { mkdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { delimiter, isAbsolute, join } from "node:path";

/** The identity jj commits as, which no configuration of the host supplies. */
export const versionControlIdentity = {
  JJ_USER: "Legion Test",
  JJ_EMAIL: "legion-test@example.invalid",
} as const;

/**
 * The names {@link versionControlEnvironment} sets, for a caller that puts them on its own process
 * and hands them to `restoreEnvironment` so the next file finds its own again.
 *
 * `PUBLIC_LEAK_CHECK` is the one name of these the fixture sets only sometimes: it carries the
 * shared public leak check where this machine resolves one and is left out where it resolves none
 * ({@link sharedPublicLeakCheck}). A restore puts an absent name back to absent, so a caller hands
 * this whole list over either way.
 */
export const versionControlNames = [
  "HOME",
  "XDG_CONFIG_HOME",
  "JJ_CONFIG",
  "JJ_USER",
  "JJ_EMAIL",
  "GIT_CONFIG_GLOBAL",
  "GIT_CONFIG_NOSYSTEM",
  "GIT_TERMINAL_PROMPT",
  "PUBLIC_LEAK_CHECK",
] as const;

/** The variable that names the shared public leak check: the first place the guard looks. */
const leakCheckVariable = "PUBLIC_LEAK_CHECK";

/** The check's file name, which is how it is found on `PATH`: the second place. */
const leakCheckName = "public-leak-check.sh";

/** The file under the configuration home whose content names the check: the third place. */
const leakCheckPointer = "public-leak-check/path";

/**
 * The shared public leak check this environment resolves — `process.env` by default — or
 * `undefined` where it resolves none.
 *
 * On a Legion worker pod the `git`, `jj` and `gh` first on `PATH` are wrappers that run Legion's
 * leak guard before a byte reaches a remote: `/opt/legion/bin/git` runs a push with
 * `-c core.hooksPath=/opt/legion/githooks`, whose root-owned `pre-push` hands the update lines to
 * `legion leak-check pre-push` (the sources are `packages/daemon/docker/`). That guard resolves
 * the check in three places, in this order (`packages/daemon/internal/leakguard/check.go`,
 * `Resolve`): `PUBLIC_LEAK_CHECK`, then `public-leak-check.sh` on `PATH`, then the pointer file
 * `public-leak-check/path` under the configuration home — `XDG_CONFIG_HOME`, or `.config` under
 * `HOME` — whose content is the path of the check. A push whose environment names the check in
 * none of the three is refused with a setup error rather than made.
 *
 * The first source that names something wins, even where the file it names is not there: that is
 * the guard's own order, and the guard reports such a name as the missing check it is. A fixture
 * that fell through to the next source instead would hand a spawned git a check this machine's own
 * push would never run.
 *
 * The `PATH` search is the guard's own (`packages/daemon/internal/lookpath`, `On`): the entries in
 * order, absolute ones only, and the first that holds a file with an execute bit.
 */
export function sharedPublicLeakCheck(
  environment: Record<string, string | undefined> = process.env
): string | undefined {
  const named = environment[leakCheckVariable];
  if (named !== undefined && named !== "") return named;
  for (const entry of (environment["PATH"] ?? "").split(delimiter)) {
    if (entry === "" || !isAbsolute(entry)) continue;
    const candidate = join(entry, leakCheckName);
    try {
      const found = statSync(candidate);
      if (found.isFile() && (found.mode & 0o111) !== 0) return candidate;
    } catch {
      // No check on this entry: the next one.
    }
  }
  const configHome = configurationHome(environment);
  if (configHome === undefined) return undefined;
  let pointed: string;
  try {
    pointed = readFileSync(join(configHome, leakCheckPointer), "utf8").trim();
  } catch {
    return undefined;
  }
  return pointed === "" ? undefined : pointed;
}

/** The configuration home of an environment, the way the leak guard reads it. */
function configurationHome(environment: Record<string, string | undefined>): string | undefined {
  const config = environment["XDG_CONFIG_HOME"];
  if (config !== undefined && config !== "") return config;
  const home = environment["HOME"];
  if (home !== undefined && home !== "") return join(home, ".config");
  return undefined;
}

/**
 * The environment a jj or git this suite starts runs in: the given directory as its home, with a
 * `.config` under it and a jj configuration file of its own in it, no git configuration at all,
 * and the identity. So neither the developer's `~/.jjconfig.toml`, nor their `~/.gitconfig`, nor
 * the system gitconfig, nor a prompt for a credential decides what a test sees.
 *
 * The one thing of the machine's it does carry is the shared public leak check
 * ({@link sharedPublicLeakCheck}), under `PUBLIC_LEAK_CHECK`, and only where the machine resolves
 * one. On a Legion worker pod every `git`, `jj` and `gh` on `PATH` runs Legion's leak guard before
 * a push, and the guard refuses a push whose environment names no check — so a fixture that left
 * the check out would have every real push a test makes refused, on a pod and nowhere else. The
 * carry names the check this machine's own push would run, which is no configuration, no
 * repository and no credential of the developer's: the isolation above stands whole.
 *
 * All three of the guard's sources are carried, and as one name: the check found on `PATH` or
 * through the pointer file is carried under `PUBLIC_LEAK_CHECK`, which the guard reads first. The
 * other two cannot be carried as themselves — this fixture does not own the `PATH` its caller
 * hands a child, and it rewrites `XDG_CONFIG_HOME` on purpose, which hides the machine's pointer
 * file from everything it starts.
 *
 * It makes the directories and writes the file, and sets nothing on this process: a caller whose
 * code under test starts jj with the process's own environment puts the names on `process.env`
 * itself, and registers their restore.
 *
 * Give it a temporary directory of the suite's own — `temporaryDirectory(prefix)` — so the home is
 * removed with it.
 */
export function versionControlEnvironment(home: string): Record<string, string> {
  const configHome = join(home, ".config");
  mkdirSync(configHome, { recursive: true });
  const config = join(home, "jj-config.toml");
  writeFileSync(config, "# The test's own jj configuration: nothing of the developer's.\n");
  const leakCheck = sharedPublicLeakCheck();
  return {
    HOME: home,
    XDG_CONFIG_HOME: configHome,
    JJ_CONFIG: config,
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_TERMINAL_PROMPT: "0",
    ...versionControlIdentity,
    ...(leakCheck === undefined ? {} : { [leakCheckVariable]: leakCheck }),
  };
}
