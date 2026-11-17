import { cpSync, existsSync, mkdirSync, realpathSync } from "node:fs";
import { homedir } from "node:os";
import path from "node:path";

/** The mise variables `callerMiseEnvironment` gives a child: the three directories always, and
 *  each of the others only when the caller names it. */
export type CallerMiseEnvironment = {
  MISE_DATA_DIR: string;
  MISE_CONFIG_DIR: string;
  MISE_STATE_DIR: string;
  MISE_GLOBAL_CONFIG_FILE?: string;
  MISE_SYSTEM_CONFIG_DIR?: string;
  MISE_SYSTEM_CONFIG_FILE?: string;
  MISE_TRUSTED_CONFIG_PATHS?: string;
};

/**
 * The caller's mise configuration, as variables for a child process that runs under a HOME of its
 * own: where its installs and its config are, the system config and trusted paths it names, and
 * `stateDir`, the child's own state directory, which `seedMiseState` fills with copies of the
 * caller's trust decisions.
 *
 * A tool on PATH can be a mise shim, and a shim needs the caller's installs (the data directory)
 * and the caller's global config, which pins the version it runs. mise finds both through HOME
 * unless a variable names them, so under a fresh HOME a shim fails with
 * `No version is set for shim: <tool>`. Passing only the variables the caller happens to set is
 * not enough: a caller that leaves XDG_CONFIG_HOME unset keeps its config in ~/.config/mise, which
 * the child's HOME does not reach. A project config outside the global one loads only when it is
 * trusted, and its trust record is in the caller's state directory, which the child's HOME does
 * not reach either.
 *
 * Each value is resolved as mise 2026.10.11 resolves it (crates/mise-util/src/env.rs unless named):
 *   - MISE_DATA_DIR, else $XDG_DATA_HOME/mise, else ~/.local/share/mise (lines 92-93, 157-158);
 *   - MISE_CONFIG_DIR, else $XDG_CONFIG_HOME/mise, else ~/.config/mise (lines 88-89, 145-146);
 *   - MISE_GLOBAL_CONFIG_FILE, else its older name MISE_CONFIG_FILE (lines 306-307), which
 *     replaces the config directory's files as the global config (src/config/mod.rs:3346-3354);
 *   - MISE_SYSTEM_CONFIG_DIR, else its older name MISE_SYSTEM_DIR, else /etc/mise (lines
 *     192-196);
 *   - MISE_SYSTEM_CONFIG_FILE (lines 312-313), which replaces the system config directory's files
 *     (src/config/mod.rs:3364-3375);
 *   - MISE_TRUSTED_CONFIG_PATHS, split on the path separator with its empty entries dropped
 *     (settings.toml:4364-4379, crates/mise-settings/src/lib.rs:504-511), each entry's leading `~`
 *     expanded (src/config/settings.rs:1103-1109), and read by is_trusted
 *     (src/config/config_file/mod.rs:688). A set but empty list ("" or ":") is an empty set to
 *     mise, and it outranks the config files' trusted_config_paths (src/config/settings.rs:
 *     1415-1427), so it is passed empty.
 * The last four are passed only when the caller names them: the system config directory does not
 * depend on HOME, so the child finds the same /etc/mise by itself.
 * mise reads an empty path variable as unset, and expands a first path component of `~` (a bare
 * `~` too) against its HOME, keeping every `..` of the rest (var_path, env.rs:724-729, and
 * replace_path, crates/mise-util/src/file.rs:878-909). It appends `mise` to an XDG base with
 * PathBuf::join, which folds nothing either. The `~` is expanded here, against the caller's home:
 * in the child it would name the child's.
 *
 * MISE_STATE_DIR is `stateDir`, never the caller's: mise writes there whenever it runs, a link for
 * each config file it loads (src/config/mod.rs:3876) and, for `mise exec`, `install`, `run` and
 * `watch`, the trust record of the active config (src/cli/mod.rs:484-490,
 * src/config/config_file/mod.rs:614-622). A child given the caller's would leave those records in
 * the caller's state. Call this where the child's environment is built, and `seedMiseState` with
 * the same `stateDir` in the hook that stages the child's HOME.
 *
 * The caller's config carries its `[env]` too, which a shim applies over the child's own
 * variables (src/cli/exec.rs:374-441). Nothing here stops that, so a child that must keep its own
 * values sets MISE_NO_ENV=1 beside these, as both rig tests do: mise then picks the tool version
 * and loads no config env (src/config/mod.rs:1554-1557).
 */
export function callerMiseEnvironment(
  stateDir: string,
  env: Record<string, string | undefined> = process.env,
  home: string = homedir()
): CallerMiseEnvironment {
  const value = (name: string) => pathVariable(env, name, home);
  // mise's Windows data home (LOCALAPPDATA, env.rs:95-100) is not copied: rig.sh, which these
  // variables are for, is bash, and its tests run only on Linux and macOS.
  const dataHome = value("XDG_DATA_HOME") ?? under(under(home, ".local"), "share");
  const configHome = value("XDG_CONFIG_HOME") ?? under(home, ".config");
  const globalConfigFile = value("MISE_GLOBAL_CONFIG_FILE") ?? value("MISE_CONFIG_FILE");
  const systemConfigDir = value("MISE_SYSTEM_CONFIG_DIR") ?? value("MISE_SYSTEM_DIR");
  const systemConfigFile = value("MISE_SYSTEM_CONFIG_FILE");
  // Unlike a path variable, a set but empty list is an answer: mise parses "" and ":" as no
  // trusted paths, which outranks the config files' trusted_config_paths. So whether the caller
  // sets it decides whether it is passed, not whether the list has an entry.
  const trustedConfigPaths = env["MISE_TRUSTED_CONFIG_PATHS"]
    ?.split(path.delimiter)
    .filter((entry) => entry !== "")
    .map((entry) => expand(entry, home))
    .join(path.delimiter);
  return {
    MISE_DATA_DIR: value("MISE_DATA_DIR") ?? under(dataHome, "mise"),
    MISE_CONFIG_DIR: value("MISE_CONFIG_DIR") ?? under(configHome, "mise"),
    MISE_STATE_DIR: stateDir,
    ...(globalConfigFile === undefined ? {} : { MISE_GLOBAL_CONFIG_FILE: globalConfigFile }),
    ...(systemConfigDir === undefined ? {} : { MISE_SYSTEM_CONFIG_DIR: systemConfigDir }),
    ...(systemConfigFile === undefined ? {} : { MISE_SYSTEM_CONFIG_FILE: systemConfigFile }),
    ...(trustedConfigPaths === undefined ? {} : { MISE_TRUSTED_CONFIG_PATHS: trustedConfigPaths }),
  };
}

/** The mise state records a child must read: trust decisions, the only part of the state that
 *  decides whether a config loads (src/config/config_file/mod.rs:663-746). */
const trustRecords = ["trusted-configs", "ignored-configs"];

/**
 * Makes `stateDir`, the child's own mise state directory, and copies into it the caller's trust
 * and ignore records, so the child decides as the caller would whether a project config loads, and
 * what it writes stays in `stateDir`.
 *
 * The caller's state directory is MISE_STATE_DIR, else $XDG_STATE_HOME/mise, else
 * ~/.local/state/mise (crates/mise-util/src/env.rs:102-103, 160-161), and its records are its
 * trusted-configs and ignored-configs directories (crates/mise-util/src/dirs.rs:14, 27-28). Each
 * record, a symlink to the config it names or a file beside one, is copied as it is. A record
 * directory reached through a symlink is copied from where the link leads, so no write of the
 * child's goes through a link into the caller's. Tracking links (tracked-configs) are read by
 * nothing that decides a run, and are left out.
 *
 * `stateDir` is made on its own, never with `recursive`, so a missing parent is an error rather
 * than a tree nothing removes. Call it in a hook, never at module scope: a `bun test -t <pattern>`
 * that selects no row of the file runs the module body and no hook.
 */
export function seedMiseState(
  stateDir: string,
  env: Record<string, string | undefined> = process.env,
  home: string = homedir()
): void {
  const callerState =
    pathVariable(env, "MISE_STATE_DIR", home) ??
    under(
      pathVariable(env, "XDG_STATE_HOME", home) ?? under(under(home, ".local"), "state"),
      "mise"
    );
  mkdirSync(stateDir);
  for (const records of trustRecords) {
    const from = under(callerState, records);
    if (!existsSync(from)) continue;
    cpSync(realpathSync(from), under(stateDir, records), {
      recursive: true,
      verbatimSymlinks: true,
      errorOnExist: true,
      force: false,
    });
  }
}

/** The caller's value of the path variable `name` as mise reads it (var_path): unset when it is
 *  unset or empty, and its `~` expanded against the caller's home. */
function pathVariable(
  env: Record<string, string | undefined>,
  name: string,
  home: string
): string | undefined {
  const set = env[name];
  return set === undefined || set === "" ? undefined : expand(set, home);
}

/** `entry` with a first component of `~` (a bare `~` included) replaced by `home`, as replace_path
 *  does: the rest is pushed component by component, which drops empty and `.` components and
 *  keeps `..`. */
function expand(entry: string, home: string): string {
  if (entry !== "~" && !entry.startsWith("~/")) return entry;
  return entry
    .slice(1)
    .split("/")
    .filter((component) => component !== "" && component !== ".")
    .reduce(under, home);
}

/** `name` under `dir` as PathBuf::push puts it: one separator between them, and nothing of
 *  `dir` normalized. path.join would fold a `..` away, and after a symlink the folded path names
 *  another directory. */
function under(dir: string, name: string): string {
  return dir.endsWith("/") ? `${dir}${name}` : `${dir}/${name}`;
}
