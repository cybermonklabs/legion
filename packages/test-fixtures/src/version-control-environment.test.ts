import { expect, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
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

/** Runs `git` as the `PATH` of `environment` names it, and reports its status and standard error. */
function git(
  cwd: string,
  environment: Record<string, string>,
  ...args: string[]
): { code: number | null; stderr: string } {
  const done = Bun.spawnSync(["git", ...args], {
    cwd,
    env: environment,
    stdout: "pipe",
    stderr: "pipe",
  });
  return { code: done.exitCode, stderr: done.stderr.toString() };
}

/** Throws unless the command was clean, so a broken fixture is named rather than read as a result. */
function clean(what: string, done: { code: number | null; stderr: string }): void {
  if (done.code !== 0) throw new Error(`${what} exited ${done.code}:\n${done.stderr}`);
}

/**
 * A work tree under `root` with one commit in it and a bare origin of its own beside it, both made
 * with `environment`, and the path of the work tree. The identity goes on the command line because
 * the fixture leaves git no configuration file to read one from.
 */
function repositoryWithACommit(root: string, environment: Record<string, string>): string {
  const origin = join(root, "origin.git");
  const work = join(root, "work");
  mkdirSync(work, { recursive: true });
  clean("git init --bare", git(root, environment, "init", "--bare", "--quiet", origin));
  clean("git init", git(work, environment, "init", "--quiet", "-b", "main"));
  writeFileSync(join(work, "a.txt"), "hello\n");
  clean("git add", git(work, environment, "add", "a.txt"));
  clean(
    "git commit",
    git(
      work,
      environment,
      "-c",
      `user.name=${versionControlIdentity.JJ_USER}`,
      "-c",
      `user.email=${versionControlIdentity.JJ_EMAIL}`,
      "commit",
      "--quiet",
      "-m",
      "add a"
    )
  );
  clean("git remote add", git(work, environment, "remote", "add", "origin", origin));
  return work;
}

/**
 * Whether a push under this fixture's environment, with the leak check carry taken back out, is
 * refused by a leak guard that resolves no check. That refusal is what the row below pins the cure
 * for, and it is the only state in which that row says anything.
 *
 * On a rolled Legion worker pod the `git` first on `PATH` is the wrapper that runs the guard
 * before a push, the guard finds a check in none of its three places once the carry is gone — the
 * variable is not set, the check is on no `PATH` entry, and the fixture's own `XDG_CONFIG_HOME`
 * hides the machine's pointer file — and it refuses the push with a setup error.
 *
 * Where this push passes, either no wrapper is on `PATH` (an unrolled pod, a developer's machine,
 * CI) or the environment this fixture builds still resolves a check some other way. Either way a
 * push under the fixture passes whatever the fixture carries, so the row below proves nothing and
 * skips rather than passing silently.
 *
 * It is here at module scope, with a scratch directory of its own that it removes again, because
 * `bun:test` takes a skip from a value: a row cannot skip itself from its own body.
 */
function pushIsRefusedWithoutTheCarry(): boolean {
  const scratch = mkdtempSync(join(tmpdir(), "test-fixtures-leak-guard-"));
  try {
    const environment: Record<string, string> = {
      PATH: process.env["PATH"] ?? "",
      ...versionControlEnvironment(join(scratch, "home")),
    };
    delete environment["PUBLIC_LEAK_CHECK"];
    const work = repositoryWithACommit(scratch, environment);
    const pushed = git(work, environment, "push", "--quiet", "origin", "main");
    return pushed.code !== 0 && pushed.stderr.includes("LEAK CHECK");
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
}

/**
 * Why the row below says nothing on this host, or `undefined` where it says something: a host with
 * no check to carry, and a host where nothing runs the guard, are the two states in which the row
 * passes whatever the fixture does.
 */
function whyTheRowIsVacuous(): string | undefined {
  if (sharedPublicLeakCheck() === undefined) {
    return "this host resolves no shared public leak check at all, so the fixture has none to carry";
  }
  if (!pushIsRefusedWithoutTheCarry()) {
    return "a push under this fixture with the carry taken out was not refused, so no leak guard stands in front of a push here";
  }
  return undefined;
}

const vacuous = whyTheRowIsVacuous();

if (vacuous !== undefined) {
  console.warn(
    "Skipped the leak guard row in packages/test-fixtures/src/version-control-environment.test.ts: " +
      `${vacuous}.`
  );
}

// This row drives the real thing: the `git` on `PATH`, which on a rolled Legion worker pod is the
// wrapper that runs Legion's leak guard before a push (`/opt/legion/bin/git` and the root-owned
// pre-push hook it points git at). No stand-in is spawned, so what passes here is the carry
// itself: with the check taken back out of the fixture the probe above shows this same push
// refused, and the live row of packages/pi-shared/src/handoff-record.test.ts is refused with it.
test.skipIf(vacuous !== undefined)(
  "carries the machine's leak check, so a real push under the fixture passes the guard on PATH",
  () => {
    const scratch = join(root, "leak-guard");
    mkdirSync(scratch, { recursive: true });
    const environment: Record<string, string> = {
      PATH: process.env["PATH"] ?? "",
      ...versionControlEnvironment(join(scratch, "home")),
    };
    const work = repositoryWithACommit(scratch, environment);
    const pushed = git(work, environment, "push", "--quiet", "origin", "main");
    expect(pushed.stderr).not.toInclude("LEAK CHECK");
    expect(pushed.code).toBe(0);
    // And the check it ran is the machine's own, not one the fixture made up.
    expect(environment["PUBLIC_LEAK_CHECK"]).toBe(sharedPublicLeakCheck());
  },
  60_000
);
