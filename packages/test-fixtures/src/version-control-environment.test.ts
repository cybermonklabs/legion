import { expect, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { captureEnvironment } from "./environment";
import { temporaryDirectory } from "./temporary-directories";
import {
  sharedPublicLeakCheck,
  versionControlEnvironment,
  versionControlIdentity,
  versionControlNames,
} from "./version-control-environment";

const root = temporaryDirectory("test-fixtures-version-control");

test("names the home it was given, a config home under it and a jj configuration file in it", () => {
  const home = join(root, "named");
  const environment = versionControlEnvironment(home);
  expect(environment["HOME"]).toBe(home);
  expect(environment["XDG_CONFIG_HOME"]).toBe(join(home, ".config"));
  expect(environment["JJ_CONFIG"]).toBe(join(home, "jj-config.toml"));
  expect(statSync(join(home, ".config")).isDirectory()).toBe(true);
  expect(readFileSync(join(home, "jj-config.toml"), "utf8")).toStartWith("#");
});

test("leaves git no configuration to read and no prompt to raise, and names the identity", () => {
  const environment = versionControlEnvironment(join(root, "git"));
  expect(environment["GIT_CONFIG_GLOBAL"]).toBe("/dev/null");
  expect(environment["GIT_CONFIG_NOSYSTEM"]).toBe("1");
  expect(environment["GIT_TERMINAL_PROMPT"]).toBe("0");
  expect(environment["JJ_USER"]).toBe(versionControlIdentity.JJ_USER);
  expect(environment["JJ_EMAIL"]).toBe(versionControlIdentity.JJ_EMAIL);
});

test("sets exactly the names it publishes, and nothing on this process", () => {
  const home = join(root, "untouched");
  const before = versionControlNames.map((name) => process.env[name]);
  const environment = versionControlEnvironment(home);
  // PUBLIC_LEAK_CHECK is set only where this machine resolves a check, so the names a run sees
  // are the published ones without it where it resolves none.
  const published = versionControlNames.filter(
    (name) => name !== "PUBLIC_LEAK_CHECK" || sharedPublicLeakCheck() !== undefined
  );
  expect(Object.keys(environment).sort()).toEqual([...published].sort());
  expect(versionControlNames.map((name) => process.env[name])).toEqual(before);
});

test("makes a home under a parent that does not exist yet", () => {
  const home = join(root, "deep", "under", "here");
  expect(existsSync(home)).toBe(false);
  versionControlEnvironment(home);
  expect(statSync(home).isDirectory()).toBe(true);
});

/** Writes `body` as a program at `path`, making the directories above it. */
function program(path: string, body: string): string {
  mkdirSync(join(path, ".."), { recursive: true });
  writeFileSync(path, body, { mode: 0o755 });
  return path;
}

test("resolves the shared public leak check in the leak guard's own three places, in its order", () => {
  const machine = join(root, "resolve");
  const named = program(join(machine, "named", "public-leak-check.sh"), "#!/bin/sh\nexit 0\n");
  const onPath = program(join(machine, "bin", "public-leak-check.sh"), "#!/bin/sh\nexit 0\n");
  const pointed = program(join(machine, "pointed", "public-leak-check.sh"), "#!/bin/sh\nexit 0\n");
  const home = join(machine, "home");
  const configHome = join(home, ".config");
  mkdirSync(join(configHome, "public-leak-check"), { recursive: true });
  writeFileSync(join(configHome, "public-leak-check", "path"), `${pointed}\n`);
  const onNoPath = join(machine, "nothing");
  mkdirSync(onNoPath, { recursive: true });

  const all = { PUBLIC_LEAK_CHECK: named, PATH: join(machine, "bin"), HOME: home };
  expect(sharedPublicLeakCheck(all)).toBe(named);
  expect(sharedPublicLeakCheck({ ...all, PUBLIC_LEAK_CHECK: "" })).toBe(onPath);
  expect(sharedPublicLeakCheck({ PATH: join(machine, "bin") })).toBe(onPath);
  expect(sharedPublicLeakCheck({ PATH: onNoPath, HOME: home })).toBe(pointed);
  // The configuration home is XDG_CONFIG_HOME where there is one, and `.config` under HOME
  // otherwise, which is the guard's own reading of it.
  expect(sharedPublicLeakCheck({ XDG_CONFIG_HOME: configHome, HOME: onNoPath })).toBe(pointed);
  expect(sharedPublicLeakCheck({ PATH: onNoPath, HOME: onNoPath })).toBeUndefined();
  expect(sharedPublicLeakCheck({})).toBeUndefined();
});

test("takes the first source that names a check, as the guard does, even where the file is gone", () => {
  const machine = join(root, "gone");
  const onPath = program(join(machine, "bin", "public-leak-check.sh"), "#!/bin/sh\nexit 0\n");
  const missing = join(machine, "removed", "public-leak-check.sh");
  expect(sharedPublicLeakCheck({ PUBLIC_LEAK_CHECK: missing, PATH: join(machine, "bin") })).toBe(
    missing
  );
  // A file on PATH with no execute bit is no check, as the guard's own PATH search has it.
  const notExecutable = join(machine, "plain", "public-leak-check.sh");
  mkdirSync(join(machine, "plain"), { recursive: true });
  writeFileSync(notExecutable, "#!/bin/sh\nexit 0\n", { mode: 0o644 });
  expect(sharedPublicLeakCheck({ PATH: `${join(machine, "plain")}:${join(machine, "bin")}` })).toBe(
    onPath
  );
  // A relative entry is skipped there too, even where it would name a check: `bin` under this
  // row's own directory holds one, and a search from there finds none.
  const was = process.cwd();
  try {
    process.chdir(machine);
    expect(sharedPublicLeakCheck({ PATH: "bin" })).toBeUndefined();
  } finally {
    process.chdir(was);
  }
});

test("calls a broken setup by the path the guard reports it at, and reads the pointer file first", () => {
  const machine = join(root, "broken");
  const good = program(join(machine, "good", "public-leak-check.sh"), "#!/bin/sh\nexit 0\n");
  const danglingDir = join(machine, "dangling");
  mkdirSync(danglingDir, { recursive: true });
  const dangling = join(danglingDir, "public-leak-check.sh");
  symlinkSync(join(machine, "moved", "public-leak-check.sh"), dangling);
  const plainDir = join(machine, "plain");
  mkdirSync(plainDir, { recursive: true });
  const plain = join(plainDir, "public-leak-check.sh");
  writeFileSync(plain, "#!/bin/sh\nexit 0\n", { mode: 0o644 });
  const configWith = (body: string | "directory"): string => {
    const config = mkdtempSync(join(machine, "config-"));
    const pointer = join(config, "public-leak-check", "path");
    if (body === "directory") {
      mkdirSync(pointer, { recursive: true });
    } else {
      mkdirSync(join(config, "public-leak-check"), { recursive: true });
      writeFileSync(pointer, body);
    }
    return config;
  };
  const empty = join(machine, "empty");
  mkdirSync(empty, { recursive: true });

  // A file of the check's name on PATH that does not run is the answer where nothing else names
  // a check: a dangling link, or a file with no execute bit.
  expect(sharedPublicLeakCheck({ PATH: danglingDir })).toBe(dangling);
  expect(sharedPublicLeakCheck({ PATH: plainDir, XDG_CONFIG_HOME: empty })).toBe(plain);
  // The pointer file is read first, as the guard and the check's own pre-push hook read it.
  expect(
    sharedPublicLeakCheck({ PATH: danglingDir, XDG_CONFIG_HOME: configWith(`${good}\n`) })
  ).toBe(good);
  // A pointer file that names nothing, or is a directory, is answered as its own path.
  const blank = configWith("\n");
  expect(sharedPublicLeakCheck({ PATH: empty, XDG_CONFIG_HOME: blank })).toBe(
    join(blank, "public-leak-check", "path")
  );
  const directory = configWith("directory");
  expect(sharedPublicLeakCheck({ PATH: empty, XDG_CONFIG_HOME: directory })).toBe(
    join(directory, "public-leak-check", "path")
  );
  // Nothing of the check anywhere is no check.
  expect(sharedPublicLeakCheck({ PATH: empty, XDG_CONFIG_HOME: empty })).toBeUndefined();
});

test("carries a check the machine's pointer file names, which the home it builds would hide", () => {
  const machine = join(root, "pointer");
  const pointed = program(join(machine, "check", "public-leak-check.sh"), "#!/bin/sh\nexit 0\n");
  const configHome = join(machine, "config");
  mkdirSync(join(configHome, "public-leak-check"), { recursive: true });
  writeFileSync(join(configHome, "public-leak-check", "path"), `${pointed}\n`);
  const empty = join(machine, "empty");
  mkdirSync(empty, { recursive: true });

  const restore = captureEnvironment(["PUBLIC_LEAK_CHECK", "XDG_CONFIG_HOME", "PATH"]);
  try {
    delete process.env["PUBLIC_LEAK_CHECK"];
    process.env["XDG_CONFIG_HOME"] = configHome;
    process.env["PATH"] = empty;
    const home = join(root, "pointer-home");
    const environment = versionControlEnvironment(home);
    expect(environment["PUBLIC_LEAK_CHECK"]).toBe(pointed);
    // The isolation stands: the config home a child reads is the fixture's own, so the machine's
    // pointer file is reachable to nothing the test starts but through the carry above.
    expect(environment["XDG_CONFIG_HOME"]).toBe(join(home, ".config"));
  } finally {
    restore();
  }
});
