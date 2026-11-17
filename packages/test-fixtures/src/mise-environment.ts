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
 * own: where its installs, its config and its trust records are, and the system config and
 * trusted paths it names.
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
 *   - MISE_STATE_DIR, else $XDG_STATE_HOME/mise, else ~/.local/state/mise (lines 102-103,
 *     160-161). The trust records are its trusted-configs directory
 *     (crates/mise-util/src/dirs.rs:14, 27), which is_trusted reads for every config that is not
 *     global (src/config/config_file/mod.rs:663-746, trust_path at 1019-1022);
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
 * A child's mise writes into the caller's state directory what any mise run there writes: a link
 * for each config file it loads (src/config/mod.rs:3876), and the trust record of the active config
 * for `mise exec`, `install`, `run` and `watch` (src/cli/mod.rs:484-490,
 * src/config/config_file/mod.rs:614-622).
 */
export function callerMiseEnvironment(
  env: Record<string, string | undefined> = process.env,
  home: string = homedir()
): CallerMiseEnvironment {
  const expand = (entry: string) => {
    // replace_path: a first component of `~` (bare `~` included) is the home, and the rest is
    // pushed component by component, which drops empty and `.` components and keeps `..`.
    if (entry !== "~" && !entry.startsWith("~/")) return entry;
    return entry
      .slice(1)
      .split("/")
      .filter((component) => component !== "" && component !== ".")
      .reduce(under, home);
  };
  const value = (name: string): string | undefined => {
    const set = env[name];
    return set === undefined || set === "" ? undefined : expand(set);
  };
  // mise's Windows data home (LOCALAPPDATA, env.rs:95-100) is not copied: rig.sh, which these
  // variables are for, is bash, and its tests run only on Linux and macOS.
  const dataHome = value("XDG_DATA_HOME") ?? under(under(home, ".local"), "share");
  const configHome = value("XDG_CONFIG_HOME") ?? under(home, ".config");
  const stateHome = value("XDG_STATE_HOME") ?? under(under(home, ".local"), "state");
  const globalConfigFile = value("MISE_GLOBAL_CONFIG_FILE") ?? value("MISE_CONFIG_FILE");
  const systemConfigDir = value("MISE_SYSTEM_CONFIG_DIR") ?? value("MISE_SYSTEM_DIR");
  const systemConfigFile = value("MISE_SYSTEM_CONFIG_FILE");
  // Unlike a path variable, a set but empty list is an answer: mise parses "" and ":" as no
  // trusted paths, which outranks the config files' trusted_config_paths. So whether the caller
  // sets it decides whether it is passed, not whether the list has an entry.
  const trustedConfigPaths = env["MISE_TRUSTED_CONFIG_PATHS"]
    ?.split(path.delimiter)
    .filter((entry) => entry !== "")
    .map(expand)
    .join(path.delimiter);
  return {
    MISE_DATA_DIR: value("MISE_DATA_DIR") ?? under(dataHome, "mise"),
    MISE_CONFIG_DIR: value("MISE_CONFIG_DIR") ?? under(configHome, "mise"),
    MISE_STATE_DIR: value("MISE_STATE_DIR") ?? under(stateHome, "mise"),
    ...(globalConfigFile === undefined ? {} : { MISE_GLOBAL_CONFIG_FILE: globalConfigFile }),
    ...(systemConfigDir === undefined ? {} : { MISE_SYSTEM_CONFIG_DIR: systemConfigDir }),
    ...(systemConfigFile === undefined ? {} : { MISE_SYSTEM_CONFIG_FILE: systemConfigFile }),
    ...(trustedConfigPaths === undefined ? {} : { MISE_TRUSTED_CONFIG_PATHS: trustedConfigPaths }),
  };
}

/** `name` under `dir` as PathBuf::push puts it: one separator between them, and nothing of
 *  `dir` normalized. path.join would fold a `..` away, and after a symlink the folded path names
 *  another directory. */
function under(dir: string, name: string): string {
  return dir.endsWith("/") ? `${dir}${name}` : `${dir}/${name}`;
}
