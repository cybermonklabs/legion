import { expect, test } from "bun:test";
import { callerMiseEnvironment } from "./mise-environment";

/** The caller's home in every row: a path no host has, so nothing here reads the real one. */
const home = "/caller-home";

/** The three directories mise derives from the caller's home when nothing names them. */
const defaults = {
  MISE_DATA_DIR: "/caller-home/.local/share/mise",
  MISE_CONFIG_DIR: "/caller-home/.config/mise",
  MISE_STATE_DIR: "/caller-home/.local/state/mise",
};

test("a caller that names nothing gets mise's defaults under its own home", () => {
  expect(callerMiseEnvironment({}, home)).toEqual(defaults);
});

test("the XDG base directories move all three directories", () => {
  expect(
    callerMiseEnvironment(
      { XDG_DATA_HOME: "/data", XDG_CONFIG_HOME: "/config", XDG_STATE_HOME: "/state" },
      home
    )
  ).toEqual({
    MISE_DATA_DIR: "/data/mise",
    MISE_CONFIG_DIR: "/config/mise",
    MISE_STATE_DIR: "/state/mise",
  });
});

test("mise's own variables win over the XDG base directories", () => {
  expect(
    callerMiseEnvironment(
      {
        XDG_DATA_HOME: "/data",
        XDG_CONFIG_HOME: "/config",
        XDG_STATE_HOME: "/state",
        MISE_DATA_DIR: "/mise-data",
        MISE_CONFIG_DIR: "/mise-config",
        MISE_STATE_DIR: "/mise-state",
      },
      home
    )
  ).toEqual({
    MISE_DATA_DIR: "/mise-data",
    MISE_CONFIG_DIR: "/mise-config",
    MISE_STATE_DIR: "/mise-state",
  });
});

// mise reads an empty variable as unset (var_path): `XDG_CONFIG_HOME=` is not the relative `mise`.
test("an empty variable is read as unset", () => {
  expect(
    callerMiseEnvironment(
      {
        MISE_DATA_DIR: "",
        XDG_DATA_HOME: "",
        MISE_CONFIG_DIR: "",
        XDG_CONFIG_HOME: "",
        MISE_STATE_DIR: "",
        XDG_STATE_HOME: "",
        MISE_GLOBAL_CONFIG_FILE: "",
        MISE_SYSTEM_CONFIG_DIR: "",
        MISE_SYSTEM_CONFIG_FILE: "",
        MISE_TRUSTED_CONFIG_PATHS: "",
      },
      home
    )
  ).toEqual(defaults);
});

// mise expands a leading `~/` against its HOME (replace_path). The child's HOME is not the
// caller's, so the caller's `~/` is expanded here, where it still means the caller's home.
test("a leading ~/ is the caller's home, not the child's", () => {
  expect(
    callerMiseEnvironment(
      {
        MISE_DATA_DIR: "~/.mise",
        XDG_CONFIG_HOME: "~/cfg",
        XDG_STATE_HOME: "~/st",
        MISE_GLOBAL_CONFIG_FILE: "~/m.toml",
        MISE_SYSTEM_CONFIG_DIR: "~/sys",
        MISE_SYSTEM_CONFIG_FILE: "~/sys.toml",
        MISE_TRUSTED_CONFIG_PATHS: "~/work:/srv",
      },
      home
    )
  ).toEqual({
    MISE_DATA_DIR: "/caller-home/.mise",
    MISE_CONFIG_DIR: "/caller-home/cfg/mise",
    MISE_STATE_DIR: "/caller-home/st/mise",
    MISE_GLOBAL_CONFIG_FILE: "/caller-home/m.toml",
    MISE_SYSTEM_CONFIG_DIR: "/caller-home/sys",
    MISE_SYSTEM_CONFIG_FILE: "/caller-home/sys.toml",
    MISE_TRUSTED_CONFIG_PATHS: "/caller-home/work:/srv",
  });
});

// replace_path matches `~` as a whole first component, so a bare `~` is the home too, and
// `~user/...` is not expanded at all.
test("a bare ~ is the caller's home, and ~user is left as written", () => {
  expect(
    callerMiseEnvironment(
      { MISE_DATA_DIR: "~", XDG_CONFIG_HOME: "~", MISE_TRUSTED_CONFIG_PATHS: "~:~other/work" },
      home
    )
  ).toEqual({
    ...defaults,
    MISE_DATA_DIR: "/caller-home",
    MISE_CONFIG_DIR: "/caller-home/mise",
    MISE_TRUSTED_CONFIG_PATHS: "/caller-home:~other/work",
  });
});

// replace_path rebuilds the rest of a `~/` path component by component, which folds repeated
// separators and `.` but keeps `..`, and mise appends `mise` to an XDG base with PathBuf::join,
// which normalizes nothing. A `..` after a symlink names another directory than the collapsed
// path does, so it stays.
test("a .. is kept as written, in a ~/ path and under an XDG base", () => {
  expect(
    callerMiseEnvironment(
      {
        XDG_CONFIG_HOME: "~/link/../config",
        XDG_DATA_HOME: "/x/link/../data/",
        MISE_STATE_DIR: "/y/link/../state",
        MISE_GLOBAL_CONFIG_FILE: "~//a/./b/../m.toml",
      },
      home
    )
  ).toEqual({
    MISE_DATA_DIR: "/x/link/../data/mise",
    MISE_CONFIG_DIR: "/caller-home/link/../config/mise",
    MISE_STATE_DIR: "/y/link/../state",
    MISE_GLOBAL_CONFIG_FILE: "/caller-home/a/b/../m.toml",
  });
});

test("a global config file is passed only when the caller names one", () => {
  const named = (env: Record<string, string>) =>
    callerMiseEnvironment(env, home).MISE_GLOBAL_CONFIG_FILE;
  expect(named({})).toBeUndefined();
  expect(named({ MISE_GLOBAL_CONFIG_FILE: "" })).toBeUndefined();
  expect(named({ MISE_GLOBAL_CONFIG_FILE: "/g.toml" })).toBe("/g.toml");
  // MISE_CONFIG_FILE is the older name of the same setting, read only when the new one is unset.
  expect(named({ MISE_CONFIG_FILE: "/old.toml" })).toBe("/old.toml");
  expect(named({ MISE_GLOBAL_CONFIG_FILE: "/g.toml", MISE_CONFIG_FILE: "/old.toml" })).toBe(
    "/g.toml"
  );
  expect(named({ MISE_GLOBAL_CONFIG_FILE: "", MISE_CONFIG_FILE: "/old.toml" })).toBe("/old.toml");
});

// The system config directory does not depend on HOME (/etc/mise unless a variable moves it), so
// the child finds the same one by itself unless the caller moved it.
test("a system config directory is passed only when the caller names one", () => {
  const named = (env: Record<string, string>) =>
    callerMiseEnvironment(env, home).MISE_SYSTEM_CONFIG_DIR;
  expect(named({})).toBeUndefined();
  expect(named({ MISE_SYSTEM_CONFIG_DIR: "/sys" })).toBe("/sys");
  // MISE_SYSTEM_DIR is the older name, read only when MISE_SYSTEM_CONFIG_DIR is unset.
  expect(named({ MISE_SYSTEM_DIR: "/old-sys" })).toBe("/old-sys");
  expect(named({ MISE_SYSTEM_CONFIG_DIR: "/sys", MISE_SYSTEM_DIR: "/old-sys" })).toBe("/sys");
  expect(named({ MISE_SYSTEM_CONFIG_DIR: "", MISE_SYSTEM_DIR: "/old-sys" })).toBe("/old-sys");
});

test("a system config file is passed only when the caller names one", () => {
  expect(callerMiseEnvironment({}, home).MISE_SYSTEM_CONFIG_FILE).toBeUndefined();
  expect(
    callerMiseEnvironment({ MISE_SYSTEM_CONFIG_FILE: "/etc/m.toml" }, home).MISE_SYSTEM_CONFIG_FILE
  ).toBe("/etc/m.toml");
});

// mise splits the list on the path separator and drops empty entries (list_by_os_path_separator),
// then expands each entry's `~/` against its HOME (Settings::trusted_config_paths).
test("trusted config paths are passed entry by entry, without empty entries", () => {
  const named = (list: string) =>
    callerMiseEnvironment({ MISE_TRUSTED_CONFIG_PATHS: list }, home).MISE_TRUSTED_CONFIG_PATHS;
  expect(callerMiseEnvironment({}, home).MISE_TRUSTED_CONFIG_PATHS).toBeUndefined();
  expect(named("/a")).toBe("/a");
  expect(named(":/a::~/b:")).toBe("/a:/caller-home/b");
  expect(named(":")).toBeUndefined();
});
