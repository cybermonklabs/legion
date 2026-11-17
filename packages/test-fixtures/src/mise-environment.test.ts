import { expect, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { callerMiseEnvironment, seedMiseState } from "./mise-environment";
import { temporaryDirectories } from "./temporary-directories";

/** The caller's home in the rows that read no file: a path no host has. */
const home = "/caller-home";
/** The child's private state directory in those rows. */
const state = "/child-state";

/** What the helper gives a child, for a caller whose environment is `env`. */
const environment = (env: Record<string, string>) => callerMiseEnvironment(state, env, home);

/** The directories mise derives from the caller's home when nothing names them, and the child's
 *  own state directory. */
const defaults = {
  MISE_DATA_DIR: "/caller-home/.local/share/mise",
  MISE_CONFIG_DIR: "/caller-home/.config/mise",
  MISE_STATE_DIR: state,
};

test("a caller that names nothing gets mise's defaults under its own home", () => {
  expect(environment({})).toEqual(defaults);
});

test("the XDG base directories move the data and config directories", () => {
  expect(environment({ XDG_DATA_HOME: "/data", XDG_CONFIG_HOME: "/config" })).toEqual({
    MISE_DATA_DIR: "/data/mise",
    MISE_CONFIG_DIR: "/config/mise",
    MISE_STATE_DIR: state,
  });
});

test("mise's own variables win over the XDG base directories", () => {
  expect(
    environment({
      XDG_DATA_HOME: "/data",
      XDG_CONFIG_HOME: "/config",
      MISE_DATA_DIR: "/mise-data",
      MISE_CONFIG_DIR: "/mise-config",
    })
  ).toEqual({
    MISE_DATA_DIR: "/mise-data",
    MISE_CONFIG_DIR: "/mise-config",
    MISE_STATE_DIR: state,
  });
});

// The child's mise writes into its state directory, so the caller's is never passed: seedMiseState
// copies what the child must read of it (the rows at the end).
test("the child's state directory is its own, whatever the caller's is", () => {
  expect(environment({ MISE_STATE_DIR: "/mise-state", XDG_STATE_HOME: "/state" })).toEqual(
    defaults
  );
});

// mise reads an empty path variable as unset (var_path): `XDG_CONFIG_HOME=` is not the relative
// `mise`. MISE_TRUSTED_CONFIG_PATHS is a list, not a path: its own rows cover it.
test("an empty path variable is read as unset", () => {
  expect(
    environment({
      MISE_DATA_DIR: "",
      XDG_DATA_HOME: "",
      MISE_CONFIG_DIR: "",
      XDG_CONFIG_HOME: "",
      MISE_GLOBAL_CONFIG_FILE: "",
      MISE_SYSTEM_CONFIG_DIR: "",
      MISE_SYSTEM_CONFIG_FILE: "",
    })
  ).toEqual(defaults);
});

// mise expands a leading `~/` against its HOME (replace_path). The child's HOME is not the
// caller's, so the caller's `~/` is expanded here, where it still means the caller's home.
test("a leading ~/ is the caller's home, not the child's", () => {
  expect(
    environment({
      MISE_DATA_DIR: "~/.mise",
      XDG_CONFIG_HOME: "~/cfg",
      MISE_GLOBAL_CONFIG_FILE: "~/m.toml",
      MISE_SYSTEM_CONFIG_DIR: "~/sys",
      MISE_SYSTEM_CONFIG_FILE: "~/sys.toml",
      MISE_TRUSTED_CONFIG_PATHS: "~/work:/srv",
    })
  ).toEqual({
    MISE_DATA_DIR: "/caller-home/.mise",
    MISE_CONFIG_DIR: "/caller-home/cfg/mise",
    MISE_STATE_DIR: state,
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
    environment({
      MISE_DATA_DIR: "~",
      XDG_CONFIG_HOME: "~",
      MISE_TRUSTED_CONFIG_PATHS: "~:~other/work",
    })
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
    environment({
      XDG_CONFIG_HOME: "~/link/../config",
      XDG_DATA_HOME: "/x/link/../data/",
      MISE_GLOBAL_CONFIG_FILE: "~//a/./b/../m.toml",
    })
  ).toEqual({
    MISE_DATA_DIR: "/x/link/../data/mise",
    MISE_CONFIG_DIR: "/caller-home/link/../config/mise",
    MISE_STATE_DIR: state,
    MISE_GLOBAL_CONFIG_FILE: "/caller-home/a/b/../m.toml",
  });
});

test("a global config file is passed only when the caller names one", () => {
  const named = (env: Record<string, string>) => environment(env).MISE_GLOBAL_CONFIG_FILE;
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
  const named = (env: Record<string, string>) => environment(env).MISE_SYSTEM_CONFIG_DIR;
  expect(named({})).toBeUndefined();
  expect(named({ MISE_SYSTEM_CONFIG_DIR: "/sys" })).toBe("/sys");
  // MISE_SYSTEM_DIR is the older name, read only when MISE_SYSTEM_CONFIG_DIR is unset.
  expect(named({ MISE_SYSTEM_DIR: "/old-sys" })).toBe("/old-sys");
  expect(named({ MISE_SYSTEM_CONFIG_DIR: "/sys", MISE_SYSTEM_DIR: "/old-sys" })).toBe("/sys");
  expect(named({ MISE_SYSTEM_CONFIG_DIR: "", MISE_SYSTEM_DIR: "/old-sys" })).toBe("/old-sys");
});

test("a system config file is passed only when the caller names one", () => {
  expect(environment({}).MISE_SYSTEM_CONFIG_FILE).toBeUndefined();
  expect(environment({ MISE_SYSTEM_CONFIG_FILE: "/etc/m.toml" }).MISE_SYSTEM_CONFIG_FILE).toBe(
    "/etc/m.toml"
  );
});

/** The MISE_TRUSTED_CONFIG_PATHS the helper passes for a caller that sets it to `list`. */
const trusted = (list: string) =>
  environment({ MISE_TRUSTED_CONFIG_PATHS: list }).MISE_TRUSTED_CONFIG_PATHS;

// mise splits the list on the path separator and drops empty entries (list_by_os_path_separator),
// then expands each entry's `~` against its HOME (Settings::trusted_config_paths).
test("trusted config paths are passed entry by entry, without empty entries", () => {
  expect(environment({}).MISE_TRUSTED_CONFIG_PATHS).toBeUndefined();
  expect(trusted("/a")).toBe("/a");
  expect(trusted(":/a::~/b:")).toBe("/a:/caller-home/b");
});

// An empty list parses: confique keeps a successful parse of "" or ":" as an empty set, and the
// environment outranks the config files' trusted_config_paths (src/config/settings.rs:1415-1427).
// Dropping it would hand the child the paths the caller's config trusts, which the caller turned
// off.
test("an empty trusted path list the caller sets is passed, empty", () => {
  expect(trusted("")).toBe("");
  expect(trusted(":")).toBe("");
  expect(trusted("::")).toBe("");
});

// seedMiseState reads and writes real directories, each row's own.
const directory = temporaryDirectories("mise-environment");

/** A mise state directory at `dir` holding a trust record, its content hash, an ignore record and
 *  a tracking link, each naming `tag`, as mise writes them: symlinks to the config, and a file. */
function callerState(dir: string, tag: string) {
  for (const records of ["trusted-configs", "ignored-configs", "tracked-configs"])
    mkdirSync(path.join(dir, records), { recursive: true });
  symlinkSync(`/configs/${tag}-trusted.toml`, path.join(dir, "trusted-configs", `${tag}-trusted`));
  writeFileSync(path.join(dir, "trusted-configs", `${tag}-trusted.hash`), `${tag} hash\n`);
  symlinkSync(`/configs/${tag}-ignored.toml`, path.join(dir, "ignored-configs", `${tag}-ignored`));
  symlinkSync(`/configs/${tag}-tracked.toml`, path.join(dir, "tracked-configs", `${tag}-tracked`));
}

/** Every entry under `dir`'s record directories, as `<records>/<name> -> <link>` or
 *  `<records>/<name> = <content>`. */
function records(dir: string): string[] {
  return ["trusted-configs", "ignored-configs", "tracked-configs"].flatMap((name) =>
    existsSync(path.join(dir, name))
      ? readdirSync(path.join(dir, name))
          .sort()
          .map((entry) => {
            const file = path.join(dir, name, entry);
            return entry.endsWith(".hash")
              ? `${name}/${entry} = ${readFileSync(file, "utf8").trim()}`
              : `${name}/${entry} -> ${readlinkSync(file)}`;
          })
      : []
  );
}

test("seeding copies the caller's trust and ignore records as they are, and no tracking link", () => {
  const caller = directory("caller");
  const child = path.join(directory("child"), "state");
  callerState(path.join(caller, ".local", "state", "mise"), "default");
  seedMiseState(child, {}, caller);
  expect(records(child)).toEqual([
    "trusted-configs/default-trusted -> /configs/default-trusted.toml",
    "trusted-configs/default-trusted.hash = default hash",
    "ignored-configs/default-ignored -> /configs/default-ignored.toml",
  ]);
});

test("seeding reads the caller's state directory where mise finds it", () => {
  const caller = directory("caller");
  callerState(path.join(caller, ".local", "state", "mise"), "default");
  callerState(path.join(caller, "xdg", "mise"), "xdg");
  callerState(path.join(caller, "named"), "named");
  const seeded = (env: Record<string, string>) => {
    const child = path.join(directory("child"), "state");
    seedMiseState(child, env, caller);
    return records(child)[0];
  };
  expect(seeded({})).toBe("trusted-configs/default-trusted -> /configs/default-trusted.toml");
  expect(seeded({ XDG_STATE_HOME: path.join(caller, "xdg") })).toBe(
    "trusted-configs/xdg-trusted -> /configs/xdg-trusted.toml"
  );
  expect(
    seeded({ XDG_STATE_HOME: path.join(caller, "xdg"), MISE_STATE_DIR: path.join(caller, "named") })
  ).toBe("trusted-configs/named-trusted -> /configs/named-trusted.toml");
  expect(seeded({ MISE_STATE_DIR: "~/named" })).toBe(
    "trusted-configs/named-trusted -> /configs/named-trusted.toml"
  );
});

// A record directory reached through a symlink is copied, not linked: a link would let the child's
// trust records land in the caller's directory.
test("what the child writes into its seeded state never reaches the caller's", () => {
  const caller = directory("caller");
  const elsewhere = path.join(caller, "elsewhere");
  callerState(elsewhere, "linked");
  mkdirSync(path.join(caller, "state"));
  symlinkSync(
    path.join(elsewhere, "trusted-configs"),
    path.join(caller, "state", "trusted-configs")
  );
  const child = path.join(directory("child"), "state");
  seedMiseState(child, { MISE_STATE_DIR: path.join(caller, "state") }, caller);
  writeFileSync(path.join(child, "trusted-configs", "written-by-the-child"), "x");
  expect(readdirSync(path.join(elsewhere, "trusted-configs")).sort()).toEqual([
    "linked-trusted",
    "linked-trusted.hash",
  ]);
});

test("a caller with no records gets an empty state directory", () => {
  const child = path.join(directory("child"), "state");
  seedMiseState(child, {}, directory("caller"));
  expect(readdirSync(child)).toEqual([]);
});

// The directory is made on its own, never with `recursive`, so a state directory whose parent is
// missing is an error rather than a tree nothing removes.
test("seeding makes the state directory and nothing above it", () => {
  const child = path.join(directory("child"), "missing", "state");
  expect(() => seedMiseState(child, {}, directory("caller"))).toThrow("ENOENT");
  expect(existsSync(path.dirname(child))).toBe(false);
});
