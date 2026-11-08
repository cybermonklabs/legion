import { beforeAll, describe, expect, test } from "bun:test";
import {
  constants,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { appTokenSources } from "./app-logins";
import { holdsNoToken, release, standInGitHub, tokenOf } from "./stand-in-github-fixtures";

// lib/app-logins.ts run as the stage proofs run it, against the stand-in GitHub of
// stand-in-github-fixtures.ts, which answers GraphQL's viewer and GET /apps/<slug> under an
// installation token of one of its Apps and refuses every other request, the JWT routes
// (GET /app, the installation routes) among them.
const { url: standIn, seen, dir } = standInGitHub("app-logins-test");
const script = join(import.meta.dir, "app-logins.ts");
// The home every child of this file runs under, so none of them reads the caller's.
const home = join(dir, "home");

// A GitHub that never answers: the port of a server stopped before any run.
const closed = Bun.serve({ port: 0, fetch: () => new Response() });
const unreachable = `http://127.0.0.1:${closed.port}/`;
await closed.stop(true);

const releaseIDs = [101, 202, 303, 404];
// The home and the releases this file's children read, staged before the first row: `dir` is
// reserved where `standInGitHub` was called and made by the hook that reservation registered, so
// a run that selects no row of this file stages nothing and leaves nothing behind.
beforeAll(() => {
  mkdirSync(home);
  for (const id of releaseIDs) {
    writeFileSync(join(dir, `release-${id}.json`), release({ sjawhar: tokenOf(id) }));
  }
});

let configs = 0;
function config(implement: string, review: string): string {
  const path = join(dir, `legion-${++configs}.yaml`);
  writeFileSync(
    path,
    `project: ACME\ngithub_apps:\n  implement:\n${implement}\n  review:\n${review}\n`
  );
  return path;
}
const app = (id: string, command = `cat ${join(dir, `release-${id}.json`)}`) =>
  `    app_id: "${id}"\n    token_command: ${JSON.stringify(command)}`;

// The run is asynchronous: the stand-in GitHub answers from this test's own event loop. The child
// gets PATH, a home of the test's own and `env`, never the caller's whole environment. The helper
// is given stdin, so a token command that inherited it would read `stdin-leak` where the
// closed-stdin case expects nothing.
async function run(path: string, env: Record<string, string> = {}, api = standIn) {
  const child = Bun.spawn(["bun", script, path, api], {
    env: { PATH: process.env.PATH ?? "", HOME: home, ...env },
    stdin: new Blob(["stdin-leak"]),
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code, stdout: stdout.trim(), stderr: stderr.trim() };
}

// read_app_logins as a stage proof calls it: lib/workflow.sh sourced under the stage scripts' shell
// options and check helpers (the lib reads repo as it is sourced), with the config as
// $work/github-apps.yaml, the file write_github_apps writes, then `then`. The one seam is bun,
// which is handed `api` after the helper's own arguments, the GitHub API URL the helper takes.
async function readAppLogins(path: string, then: string, api = standIn) {
  const work = mkdtempSync(join(dir, "work."));
  copyFileSync(path, join(work, "github-apps.yaml"));
  const shell = `set -Eeuo pipefail
check=boot repo=sjawhar/legion-smoke
note() { printf '   %s\\n' "$*"; }
fail() { printf 'FAIL %s: %s\\n' "$check" "$*" >&2; exit 1; }
bun() { command bun "$@" "$API"; }
. "$LIB"
read_app_logins
${then}`;
  const child = Bun.spawn(["bash", "-c", shell], {
    env: {
      PATH: process.env.PATH ?? "",
      HOME: home,
      LIB: join(import.meta.dir, "workflow.sh"),
      work,
      API: api,
    },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code, stdout: stdout.trimEnd(), stderr: stderr.trim() };
}

// A failure names the App it could not read by its role and app id, on one line of stderr.
const namesApp = (role: string, id: string) => new RegExp(`\\b${role} App\\b.*\\bapp_id ${id}\\b`);

/** Ends the process whose pid the file holds, and answers whether that process was still
 * running. */
function endRecordedProcess(pidFile: string): boolean {
  if (!existsSync(pidFile)) return false;
  const pid = Number.parseInt(readFileSync(pidFile, "utf8").trim(), 10);
  rmSync(pidFile, { force: true });
  if (!Number.isInteger(pid) || pid <= 1) return false;
  try {
    process.kill(pid, "SIGKILL");
    return true;
  } catch {
    // Already gone: nothing to end.
    return false;
  }
}

/**
 * Runs `body` against a pid file of its own, `name` under `dir`, ends whatever process that file
 * names once `body` is over, and asserts that the process was still running. A command that
 * escapes its caller's session outlives the run that started it, so a row that starts one has it
 * write down the pid it ends up with and ends it here; a descendant that ended before the run
 * did would leave the row proving nothing.
 */
async function withEscapedDescendant(
  name: string,
  body: (pidFile: string) => Promise<void>
): Promise<void> {
  const pidFile = join(dir, name);
  let wasRunning = false;
  try {
    await body(pidFile);
  } finally {
    wasRunning = endRecordedProcess(pidFile);
  }
  expect(wasRunning).toBe(true);
}

describe("app-logins.ts", () => {
  // The token command the two rows below both run, and the failure line the helper ends on when
  // it fails: its whole stderr is quoted there, so that line is about a megabyte long, written
  // in one go before the exit.
  const noisy = "printf '%01000000d' 0 >&2; printf 'END-OF-STDERR\\n' >&2; exit 3";
  const failure = `app-logins: the implement App (app_id 101): github_apps.implement.token_command failed (exit 3): ${"0".repeat(1_000_000)}END-OF-STDERR`;
  /** Asserts that the last line of `printed` is the whole of that failure, by length and by both
   * ends: a mismatch between two megabyte-long lines prints no diff anyone can read. */
  const expectFailureLine = (printed: string) => {
    const last = printed.trimEnd().split("\n").at(-1) ?? "";
    expect(last.length).toBe(failure.length);
    expect(last.slice(0, 120)).toBe(failure.slice(0, 120));
    expect(last.slice(-40)).toBe(failure.slice(-40));
  };
  /** What one shell group prints when its own stdout and stderr meet a reader that sleeps
   * `stall` seconds before it reads a byte: `body`'s output, in the order the group wrote it. The
   * group gets PATH and a home of the test's own, as every other child of this file does. */
  const behindReader = async (body: string, stall: number) => {
    const shell = `{\n${body}\n} 2>&1 | { sleep ${stall}; cat; }`;
    const child = Bun.spawn(["bash", "-c", shell], {
      env: { PATH: process.env.PATH ?? "", HOME: home },
      stdout: "pipe",
      stderr: "pipe",
    });
    const [printed] = await Promise.all([new Response(child.stdout).text(), child.exited]);
    return printed;
  };

  test("prints each App's bot login from GraphQL's viewer, proved by GET /apps/<slug> under its own token", async () => {
    seen.length = 0;
    const { code, stdout, stderr } = await run(config(app("101"), app("202")));
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(stdout)).toEqual({
      implement: "example-implement[bot]",
      review: "example-review[bot]",
    });
    // No JWT route is asked: the proof holds no key.
    expect(seen).toEqual([
      "POST /graphql",
      "GET /apps/example-implement",
      "POST /graphql",
      "GET /apps/example-review",
    ]);
    holdsNoToken(stdout);
  });

  test("an App whose id is not the configured one is refused, naming both ids and no token", async () => {
    const { code, stdout, stderr } = await run(config(app("101"), app("404")));
    expect(code).toBe(1);
    expect(stdout).toBe("");
    expect(stderr).toMatch(namesApp("review", "404"));
    expect(stderr).toContain("answered App id 405, want 404");
    holdsNoToken(stderr);
  });

  test("an App without its role's write permission is refused, the two Apps swapped whole too", async () => {
    const { code, stdout, stderr } = await run(
      config(app("202", `cat ${join(dir, "release-202.json")}`), app("101"))
    );
    expect(code).toBe(1);
    expect(stdout).toBe("");
    // The implement role on the review App passes (contents: write); the review role on the
    // implement App does not.
    expect(stderr).toMatch(namesApp("review", "101"));
    expect(stderr).toContain('answered permissions.checks "read", want write');
    holdsNoToken(stderr);
  });

  test("a token command that fails is named by its field, exit code and stderr, its stdout never quoted", async () => {
    const { code, stderr } = await run(
      config(app("101", `echo ${tokenOf(101)}; echo grant denied >&2; exit 3`), app("202"))
    );
    expect(code).toBe(1);
    // The stderr reaches the operator as it is written, then again in the failure.
    expect(stderr).toBe(
      "grant denied\napp-logins: the implement App (app_id 101): github_apps.implement.token_command failed (exit 3): grant denied"
    );
    holdsNoToken(stderr);
  });

  test("a token command runs as the daemon runs it: no NATS seed, stdin closed, owner, repositories and read-only set empty", async () => {
    // An unset variable prints `unset`, so a set-empty owner, repository list and read-only flag
    // each print nothing.
    const probe =
      // biome-ignore lint/suspicious/noTemplateCurlyInString: a shell parameter expansion, not a template
      'printf "%s|%s|%s|%s|%s|%s|%s" "$NATS_NKEY_SEED" "$NATS_DAEMON_NKEY_SEED" "$NATS_NKEY_SEED_FILE" "${LEGION_GITHUB_OWNER-unset}" "${LEGION_GITHUB_REPOS-unset}" "${LEGION_GITHUB_READ_ONLY-unset}" "$(cat)" >&2; exit 4';
    const { code, stderr } = await run(config(app("101", probe), app("202")), {
      NATS_NKEY_SEED: "pane-seed",
      NATS_DAEMON_NKEY_SEED: "daemon-seed",
      NATS_NKEY_SEED_FILE: "/seed-file",
      LEGION_GITHUB_OWNER: "leaked-owner",
      LEGION_GITHUB_REPOS: "leaked-repo",
      LEGION_GITHUB_READ_ONLY: "1",
    });
    expect(code).toBe(1);
    expect(stderr).toBe(
      "||/seed-file||||app-logins: the implement App (app_id 101): github_apps.implement.token_command failed (exit 4): ||/seed-file||||"
    );
  });

  test("a token command's stderr reaches the operator on success too", async () => {
    const pending = `printf 'request req-42 pending\\n' >&2; cat ${join(dir, "release-101.json")}`;
    const { code, stdout, stderr } = await run(config(app("101", pending), app("202")));
    expect(code).toBe(0);
    expect(stderr).toBe("request req-42 pending");
    expect(JSON.parse(stdout).implement).toBe("example-implement[bot]");
  });

  test("a token command whose newline-delimited stderr exceeds the cap is killed", async () => {
    const overCap =
      "i=0; while [ \"$i\" -lt 257 ]; do printf '%04095d\\n' 0 >&2; i=$((i + 1)); done; exit 3";
    const { code, stderr } = await run(config(app("101", overCap), app("202")));
    expect(code).toBe(1);
    expect(stderr.slice(-1000)).toContain("wrote more than 1048576 bytes to stderr");
  });

  // An exit that does not wait for that megabyte-long write discards most of it, and the caller
  // is left with the command's noise and no reason for the run's failure.
  test("the failure a token command's stderr is quoted in reaches the caller whole, and last", async () => {
    const { code, stderr } = await run(config(app("101", noisy), app("202")));
    expect(code).toBe(1);
    expectFailureLine(stderr);
  });

  // The relay write sits inside the `for await` over the child's stderr, so an `EPIPE` it let out
  // would reject that loop's promise, leave runTokenCommand through its `Promise.all`, and reach
  // the `catch` that writes its diagnosis to the same dead descriptor and throws again: a caller
  // that stopped reading stderr would get a stack trace and the wrong exit code where the helper
  // promises the logins and exit 0, and lib/github-apps.sh's check_github_apps reads both. What
  // the row guards is the writer: no `EPIPE` may leave it.
  test("a caller that stopped reading stderr still gets the logins and exit 0", async () => {
    const relayed = `printf 'relayed\\n' >&2; cat ${join(dir, "release-101.json")}`;
    const child = Bun.spawn(["bun", script, config(app("101", relayed), app("202")), standIn], {
      env: { PATH: process.env.PATH ?? "", HOME: home },
      stdout: "pipe",
      stderr: "pipe",
    });
    // The read end is closed before the child has finished starting, let alone written: every
    // write the helper makes to fd 2 from here on meets a reader that is gone.
    await child.stderr.cancel();
    const [stdout, code] = await Promise.all([new Response(child.stdout).text(), child.exited]);
    expect(code).toBe(0);
    expect(JSON.parse(stdout).implement).toBe("example-implement[bot]");
  });

  // A stage proof runs many bun commands in one shell group, and bun leaves `O_NONBLOCK` set on
  // the pipe that fd 2 names when it exits, so a later writer on that pipe meets `EAGAIN` where
  // a blocking descriptor waits (writeAll's comment in app-logins.ts holds the mechanism and
  // what letting the `EAGAIN` out cost). What this row adds: the megabyte-long failure reaches
  // such a caller whole, rather than ending after 66,381 bytes with a stack in its place.
  test("the failure reaches a caller whose fd 2 an earlier bun left non-blocking, whole", async () => {
    // A bound of its own: the row holds the reader for a second on purpose.
    const path = config(app("101", noisy), app("202"));
    // Only the first bun's stdout is redirected: a `2>/dev/null` on it would flip a descriptor
    // of its own and leave this pipe blocking, and the row would then prove nothing (measured).
    // Both runs share the group, so the flag the first bun leaves is the one the helper writes
    // under, and the printed flags refuse the row when a later bun stops setting them.
    const printed = await behindReader(
      `  bun -e 'process.stderr' >/dev/null
  sed -n 's/^flags:/fd2 flags:/p' /proc/self/fdinfo/2
  bun "${script}" --roles implement "${path}" "${standIn}" >/dev/null`,
      1
    );
    const flags =
      printed.split("\n").find((line) => line.startsWith("fd2 flags:")) ?? "fd2 flags: none";
    const bits = Number.parseInt(flags.slice("fd2 flags:".length).trim(), 8);
    // The `O_NONBLOCK` bit alone, since another flag on the pipe is no business of this row's.
    expect(bits & constants.O_NONBLOCK, flags).toBe(constants.O_NONBLOCK);
    expectFailureLine(printed);
  }, 30_000);

  test("a token command whose escaped descendant holds its output open fails within the drain bound", async () => {
    // The pid the descendant records is the one it ends up with: `sh` prints its own, then
    // becomes the `sleep` through `exec`, so what the row ends is what held the output open for
    // the whole drain.
    await withEscapedDescendant("escaped.pid", async (pidFile) => {
      const escaped = `setsid sh -c 'echo $$ >${pidFile}; exec sleep 60' >&2 & exit 0`;
      const started = Date.now();
      const { code, stderr } = await run(config(app("101", escaped), app("202")));
      expect(code).toBe(1);
      expect(stderr).toContain("still held its output open");
      expect(Date.now() - started).toBeLessThan(30_000);
    });
  }, 30_000);

  // The same shape with a descendant that prints while it holds the pipes open: its output is
  // not the command's progress and must not push the bound out, which the silent row above
  // cannot catch.
  test("a token command whose escaped descendant writes while it holds its output open fails within the drain bound too", async () => {
    // The descendant is a loop, so it cannot become its payload through `exec` as the silent
    // row's does: the `sh` that prints `$$` is the `sh` that then runs the loop, and the pid
    // therefore names the writer itself. Killing it ends the writing and leaves the `sleep` of
    // the iteration it stood in, which holds the inherited stderr for the rest of its second and
    // then exits. `setsid` puts the loop outside every process group the helper kills, and the
    // helper's own exit does not stop it (measured: it was still writing into the socket
    // `Bun.spawn` gave the command ten seconds later). It counts its writes rather than looping
    // for ever, so a run that never reaches the cleanup still ends it, as the silent row's
    // `sleep 60` ends itself; thirty seconds is six times the bound it must not push out.
    await withEscapedDescendant("escaped-writing.pid", async (pidFile) => {
      const escaped =
        `setsid sh -c 'echo $$ >${pidFile}; i=0; while [ $i -lt 30 ]; do printf x >&2; sleep 1;` +
        " i=$((i + 1)); done' >&2 & exit 3";
      const started = Date.now();
      const { code, stderr } = await run(config(app("101", escaped), app("202")));
      expect(code).toBe(1);
      expect(stderr).toContain("still held its output open");
      expect(Date.now() - started).toBeLessThan(20_000);
    });
  }, 25_000);

  // The drain bound must not count this process's own blocked writes, which the relay's
  // synchronous writer holds for as long as a caller stays behind. Each shape here is one group
  // whose stdout and stderr meet a reader that sleeps eight seconds, three past the bound, and
  // whose token command writes one 300,008-byte line, its newline left to a descendant half a
  // second later: the relay issues that line in a single write, far larger than the 64 KiB pipe,
  // and it blocks until the reader wakes. The row checks that each shape still ends on the token
  // command's own verdict: the failing one on its exit 3 and the stderr quoted in it, the
  // succeeding one on the release it printed and its exit 0.
  test("a reader that falls behind still gets the token command's own verdict", async () => {
    // A bound of its own: the row holds a reader for eight seconds on purpose.
    const held = "printf 'denied-%0300000d' 0 >&2; (sleep 0.5; printf '\\n' >&2) &";
    const succeeding = join(dir, "behind-succeeding.json");
    // Both shapes at once, so the row costs one stall rather than two.
    const stalled = (command: string, out: string) => {
      const path = config(app("101", command), app("202"));
      return behindReader(
        `  bun "${script}" --roles implement "${path}" "${standIn}" >"${out}"
  echo "exit=$?"`,
        8
      );
    };
    const [bad, good] = await Promise.all([
      stalled(`${held} exit 3`, "/dev/null"),
      stalled(`${held} cat ${join(dir, "release-101.json")}`, succeeding),
    ]);
    expect(bad).toContain("failed (exit 3): denied-");
    expect(bad).not.toContain("still held its output open");
    expect(bad.split("\n")).toContain("exit=1");
    expect(good.split("\n")).toContain("exit=0");
    expect(JSON.parse(readFileSync(succeeding, "utf8")).implement).toBe("example-implement[bot]");
  }, 30_000);

  // The same eight-second stall, with a whole 300,002-byte line written before the command
  // exits rather than after it: the relay blocks on that line, the exit is read only once the
  // reader wakes, and the stall has therefore already pushed the exit out. The bound counts the
  // blocked time after the exit alone, so a bound that counted this block too would hand the
  // escaped descendant a second stall to hold the pipes in, and hide its diagnosis for it.
  test("a reader that falls behind before the exit does not buy an escaped descendant a second stall", async () => {
    // A bound of its own: the row holds a reader for eight seconds on purpose and then waits out
    // the five the drain gives the descendant, with room for the twenty-one this row refuses.
    // The pid the descendant records is the one it ends up with: `sh` prints its own, then
    // becomes the `sleep` through `exec`.
    await withEscapedDescendant("escaped-before-exit.pid", async (pidFile) => {
      const escaped =
        `printf 'a%0300000d\\n' 0 >&2; sleep 3; setsid sh -c 'echo $$ >${pidFile}; exec sleep 40'` +
        " >&2 & exit 3";
      const path = config(app("101", escaped), app("202"));
      const started = Date.now();
      const printed = await behindReader(
        `  bun "${script}" --roles implement "${path}" "${standIn}" >/dev/null
  echo "exit=$?"`,
        8
      );
      const elapsed = Date.now() - started;
      expect(printed).toContain("still held its output open");
      expect(printed.split("\n")).toContain("exit=1");
      // Thirteen seconds is the stall the exit waits out plus the five the drain bound gives;
      // twenty-one is that stall counted a second time, as a bound that gave back every block
      // since the spawn counts it. The window takes the first and refuses the second.
      expect(elapsed, `${elapsed} ms`).toBeGreaterThan(11_000);
      expect(elapsed, `${elapsed} ms`).toBeLessThan(17_000);
    });
  }, 30_000);

  test("output that is no release, an expired token, or no installation is refused without quoting it", async () => {
    const cases: [string, string][] = [
      [`echo '{"token":"${tokenOf(101)}"'`, "printed no release"],
      [`echo '${release({ sjawhar: tokenOf(101) }, "2000-01-01T00:00:00Z")}'`, "expired at"],
      [`echo '{"installations":{}}'`, "released no installation"],
    ];
    for (const [command, want] of cases) {
      const { code, stderr } = await run(config(app("101", command), app("202")));
      expect(code).toBe(1);
      expect(stderr).toMatch(namesApp("implement", "101"));
      expect(stderr).toContain(want);
      holdsNoToken(stderr);
    }
  });

  test("the first installation by login is the one read, as the daemon's AppLogin reads it", async () => {
    const two = join(dir, "release-two.json");
    writeFileSync(two, release({ zeta: "ghs_unknown_token", Alpha: tokenOf(101) }));
    const { code, stdout, stderr } = await run(config(app("101", `cat ${two}`), app("202")));
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(stdout).implement).toBe("example-implement[bot]");
  });

  test("a token GitHub refuses is named by the App and GitHub's status, its echo of the token masked", async () => {
    const unknown = join(dir, "release-unknown.json");
    writeFileSync(unknown, release({ sjawhar: "ghs_unknown_token" }));
    const { code, stderr } = await run(config(app("101", `cat ${unknown}`), app("202")));
    expect(code).toBe(1);
    expect(stderr).toMatch(namesApp("implement", "101"));
    expect(stderr).toContain("401");
    expect(stderr).not.toContain("ghs_unknown_token");
  });

  test("a token that acts as a person, not an App's bot, is refused", async () => {
    const { code, stderr } = await run(config(app("101"), app("303")));
    expect(code).toBe(1);
    expect(stderr).toMatch(namesApp("review", "303"));
    expect(stderr).toContain("acts as a-person, not an App's bot");
  });

  test("a GitHub that never answers is named by the App's role and app id", async () => {
    const { code, stdout, stderr } = await run(config(app("101"), app("202")), {}, unreachable);
    expect(code).toBe(1);
    expect(stdout).toBe("");
    expect(stderr).toMatch(namesApp("implement", "101"));
  });

  test("two roles on one App are refused, as the daemon refuses them at boot", async () => {
    const { code, stderr } = await run(config(app("202"), app("202")));
    expect(code).toBe(1);
    expect(stderr).toContain("both github_apps roles are the App example-review[bot]");
  });

  test("--roles reads only the roles it names, and refuses one it does not know", async () => {
    const path = join(dir, `legion-${++configs}.yaml`);
    writeFileSync(path, `github_apps:\n  implement:\n${app("101")}\n`);
    const one = Bun.spawn(["bun", script, "--roles", "implement", path, standIn], {
      env: { PATH: process.env.PATH ?? "", HOME: home },
      stdout: "pipe",
      stderr: "pipe",
    });
    const [stdout, stderr, code] = await Promise.all([
      new Response(one.stdout).text(),
      new Response(one.stderr).text(),
      one.exited,
    ]);
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(stdout)).toEqual({ implement: "example-implement[bot]" });
    for (const roles of ["merger", "implement,implement", ""]) {
      const bad = Bun.spawnSync(["bun", script, "--roles", roles, path], {
        env: { PATH: process.env.PATH ?? "", HOME: home },
        stderr: "pipe",
      });
      expect(bad.exitCode).toBe(2);
      expect(bad.stderr.toString()).toContain("usage:");
    }
  });

  test("an App configured with no token_command is refused by name", () => {
    expect(() =>
      appTokenSources(
        'github_apps:\n  implement: { app_id: "1", private_key_command: "true" }\n  review: { app_id: "2", token_command: "true" }\n'
      )
    ).toThrow("github_apps.implement names its credential without a token_command");
    expect(() => appTokenSources("project: ACME\n")).toThrow("the config has no github_apps");
  });
});

describe("read_app_logins", () => {
  test("gives each role its App's login as GitHub answered it, and GraphQL the bare slug", async () => {
    const roles = [
      "implementer",
      "merger",
      "planner",
      "tester",
      "reviewer",
      "architect",
      "controller",
    ];
    const { code, stdout, stderr } = await readAppLogins(
      config(app("101"), app("202")),
      `for role in ${roles.join(" ")}; do echo "$role $(role_app "$role") $(role_app_slug "$role")"; done`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    const answers = stdout.split("\n").filter((line) => roles.includes(line.split(" ")[0]));
    expect(answers).toEqual([
      "implementer example-implement[bot] example-implement",
      "merger example-implement[bot] example-implement",
      "planner example-review[bot] example-review",
      "tester example-review[bot] example-review",
      "reviewer example-review[bot] example-review",
      "architect example-review[bot] example-review",
      "controller example-review[bot] example-review",
    ]);
  });

  test("a GitHub that never answers fails the run, naming the App it could not read", async () => {
    const { code, stdout, stderr } = await readAppLogins(
      config(app("101"), app("202")),
      "echo after-read",
      unreachable
    );
    expect(code).toBe(1);
    expect(stdout).not.toContain("after-read");
    expect(stderr).toMatch(namesApp("implement", "101"));
    expect(stderr).toMatch(/^FAIL boot: /m);
  });
});
