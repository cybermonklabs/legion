import { expect, test } from "bun:test";
import { callerMiseEnvironment } from "./mise-environment";

/** The caller's home in every row: a path no host has, so nothing here reads the real one. */
const home = "/caller-home";

test("a caller that names nothing gets mise's defaults under its own home", () => {
  expect(callerMiseEnvironment({}, home)).toEqual({
    MISE_DATA_DIR: "/caller-home/.local/share/mise",
    MISE_CONFIG_DIR: "/caller-home/.config/mise",
  });
});

test("the XDG base directories move both directories", () => {
  expect(
    callerMiseEnvironment({ XDG_DATA_HOME: "/data", XDG_CONFIG_HOME: "/config" }, home)
  ).toEqual({ MISE_DATA_DIR: "/data/mise", MISE_CONFIG_DIR: "/config/mise" });
});

test("mise's own variables win over the XDG base directories", () => {
  expect(
    callerMiseEnvironment(
      {
        XDG_DATA_HOME: "/data",
        XDG_CONFIG_HOME: "/config",
        MISE_DATA_DIR: "/mise-data",
        MISE_CONFIG_DIR: "/mise-config",
      },
      home
    )
  ).toEqual({ MISE_DATA_DIR: "/mise-data", MISE_CONFIG_DIR: "/mise-config" });
});

// mise reads an empty variable as unset (var_path): `XDG_CONFIG_HOME=` is not the relative `mise`.
test("an empty variable is read as unset", () => {
  expect(
    callerMiseEnvironment(
      { MISE_DATA_DIR: "", XDG_DATA_HOME: "", MISE_CONFIG_DIR: "", XDG_CONFIG_HOME: "" },
      home
    )
  ).toEqual({
    MISE_DATA_DIR: "/caller-home/.local/share/mise",
    MISE_CONFIG_DIR: "/caller-home/.config/mise",
  });
});

// mise expands a leading `~/` against its HOME (replace_path). The child's HOME is not the
// caller's, so the caller's `~/` is expanded here, where it still means the caller's home.
test("a leading ~/ is the caller's home, not the child's", () => {
  expect(
    callerMiseEnvironment(
      { MISE_DATA_DIR: "~/.mise", XDG_CONFIG_HOME: "~/cfg", MISE_GLOBAL_CONFIG_FILE: "~/m.toml" },
      home
    )
  ).toEqual({
    MISE_DATA_DIR: "/caller-home/.mise",
    MISE_CONFIG_DIR: "/caller-home/cfg/mise",
    MISE_GLOBAL_CONFIG_FILE: "/caller-home/m.toml",
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
