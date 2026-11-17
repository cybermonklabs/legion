import { homedir } from "node:os";
import path from "node:path";

/**
 * The caller's mise data and config directories, and its global config file when it names one, as
 * variables for a child process that runs under a HOME of its own.
 *
 * A tool on PATH can be a mise shim, and a shim needs both the caller's installs (the data
 * directory) and the caller's global config, which pins the version it runs. mise finds both
 * through HOME unless a variable names them, so under a fresh HOME a shim fails with
 * `No version is set for shim: <tool>`. Passing only the variables the caller happens to set is
 * not enough: a caller that leaves XDG_CONFIG_HOME unset keeps its config in ~/.config/mise, which
 * the child's HOME does not reach.
 *
 * Each value is resolved as mise 2026.10.11 resolves it (crates/mise-util/src/env.rs):
 *   - MISE_DATA_DIR, else $XDG_DATA_HOME/mise, else ~/.local/share/mise (lines 92-93, 157-158);
 *   - MISE_CONFIG_DIR, else $XDG_CONFIG_HOME/mise, else ~/.config/mise (lines 88-89, 145-146);
 *   - MISE_GLOBAL_CONFIG_FILE, else its older name MISE_CONFIG_FILE (lines 306-307), which
 *     replaces the config directory's files as the global config (src/config/mod.rs:3346-3354),
 *     and is passed only when the caller names one.
 * mise reads an empty variable as unset and expands a leading `~/` against its HOME (var_path,
 * env.rs:724-729, and replace_path, crates/mise-util/src/file.rs:897-909). The `~/` is expanded
 * here, against the caller's home: in the child it would name the child's.
 */
export function callerMiseEnvironment(
  env: Record<string, string | undefined> = process.env,
  home: string = homedir()
): { MISE_DATA_DIR: string; MISE_CONFIG_DIR: string; MISE_GLOBAL_CONFIG_FILE?: string } {
  const value = (name: string): string | undefined => {
    const set = env[name];
    if (set === undefined || set === "") return undefined;
    return set.startsWith("~/") ? path.join(home, set.slice(2)) : set;
  };
  const dataHome = value("XDG_DATA_HOME") ?? path.join(home, ".local", "share");
  const configHome = value("XDG_CONFIG_HOME") ?? path.join(home, ".config");
  const globalConfigFile = value("MISE_GLOBAL_CONFIG_FILE") ?? value("MISE_CONFIG_FILE");
  return {
    MISE_DATA_DIR: value("MISE_DATA_DIR") ?? path.join(dataHome, "mise"),
    MISE_CONFIG_DIR: value("MISE_CONFIG_DIR") ?? path.join(configHome, "mise"),
    ...(globalConfigFile === undefined ? {} : { MISE_GLOBAL_CONFIG_FILE: globalConfigFile }),
  };
}
