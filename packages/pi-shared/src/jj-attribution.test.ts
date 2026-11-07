import { afterEach, describe, expect, it } from "bun:test";
import { chmod, mkdir, mkdtemp, readdir, readFile, realpath, rm, symlink } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import {
  exportJjSessionAttribution,
  JJ_CONFIG_PATH_TIMEOUT_MS,
  resolvedWorkspace,
  sessionIdFromTranscript,
  userConfigChain,
} from "./jj-attribution";

const ROOT_TRANSCRIPT =
  "/sessions/-legion-default/2026-08-30T03-47-40-329Z_01a050c7-b4a9-7000-a539-240e20271441.jsonl";
const ROOT_ID = "01a050c7-b4a9-7000-a539-240e20271441";

describe("sessionIdFromTranscript", () => {
  it("extracts the resumable id from a top-level transcript", () => {
    expect(sessionIdFromTranscript(ROOT_TRANSCRIPT)).toBe(ROOT_ID);
  });

  it("rejects subagent transcripts named after their agent", () => {
    expect(
      sessionIdFromTranscript("/sessions/-x/2026-08-30T03-47-40-329Z_abc/EnvProbe.jsonl")
    ).toBeUndefined();
  });

  it("rejects timestamped basenames whose id is not a UUID", () => {
    expect(
      sessionIdFromTranscript("/sessions/-x/2026-08-30T03-47-40-329Z_bad'id.jsonl")
    ).toBeUndefined();
  });
});

describe("exportJjSessionAttribution", () => {
  const dirs: string[] = [];
  async function stateDir(prefix = "jj-attribution-"): Promise<string> {
    const dir = await mkdtemp(path.join(tmpdir(), prefix));
    dirs.push(dir);
    return dir;
  }
  afterEach(async () => {
    await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
  });
  const sessionLine = `[templates]\ncommit_trailers = '"Omp-Session: ${ROOT_ID}"'\n`;
  /** A real directory to scope an overlay to, under a scratch directory the afterEach removes. */
  async function workspace(name = "ws"): Promise<string> {
    const dir = path.join(await stateDir("jj-attribution-ws-"), name);
    await mkdir(dir, { recursive: true });
    return await realpath(dir);
  }
  /** The overlay the writer composes: the unscoped session line, then the scope carrying both.
   * `inToml` is the workspace path as a TOML basic string's body, escapes included. */
  function scopedOverlay(inToml: string, issueKey: string): string {
    return (
      `${sessionLine}\n[[--scope]]\n--when.workspaces = ["${inToml}"]\n` +
      `[--scope.templates]\ncommit_trailers = '"Omp-Session: ${ROOT_ID}\\nDispatch: ${issueKey}"'\n`
    );
  }

  it("writes the trailer overlay and chains it after the resolved user config", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    const chain = await exportJjSessionAttribution(ROOT_ID, dir, {
      env,
      resolveBase: async () => "/u/a.toml:/u/b.toml",
    });

    const overlay = path.join(dir, `omp-attribution-${ROOT_ID}.toml`);
    expect(chain).toBe(`/u/a.toml:/u/b.toml:${overlay}`);
    expect(env.JJ_CONFIG).toBe(chain);
    expect(await readFile(overlay, "utf8")).toBe(sessionLine);
  });

  it("scopes the Dispatch line to the pane's workspace for a key of the accepted shape", async () => {
    const dir = await stateDir();
    const ws = await workspace();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      workspace: ws,
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      scopedOverlay(ws, "ACME-7")
    );
  });

  // Without a workspace there is nothing to scope the key to, and an unscoped key would stamp
  // this deployment's issue into every jj repository the pane's shell commits in.
  it("writes no scope for an accepted key with no workspace", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      sessionLine
    );
  });

  // jj resolves a workspace root before it matches a scope, so the link's own path would match
  // nothing — and say nothing about it.
  it("scopes to the real path when the workspace is reached through a symlink", async () => {
    const dir = await stateDir();
    const real = await workspace("real-ws");
    const link = path.join(path.dirname(real), "link-ws");
    await symlink(real, link);
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      workspace: resolvedWorkspace(link),
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      scopedOverlay(real, "ACME-7")
    );
  });

  it("writes no scope for a workspace path that does not exist", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      workspace: resolvedWorkspace(path.join(dir, "no-such-workspace")),
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      sessionLine
    );
  });

  // The path goes into a TOML basic string inside an array; a raw quote would end that string
  // and a raw backslash would open an escape, leaving jj a file it refuses.
  it("escapes a workspace path holding a double quote and a backslash", async () => {
    const dir = await stateDir();
    const ws = await workspace('ws "quoted\\odd');
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      workspace: ws,
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      scopedOverlay(ws.replaceAll("\\", "\\\\").replaceAll('"', '\\"'), "ACME-7")
    );
  });

  // No escape of ours is worth guessing at here: a scope jj cannot parse breaks every `jj`
  // command in the pane, which is far worse than a missing trailer.
  it.each([
    ["a newline", "/ws/line\nbreak"],
    ["a tab", "/ws/tab\there"],
    ["a DEL", "/ws/del\u007fhere"],
  ])("writes no scope for a workspace path holding %s", async (_name, ws) => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      workspace: ws,
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      sessionLine
    );
  });

  // Containment as much as correctness: the value is interpolated into a jj template inside a
  // TOML literal, so anything but a whole key writes the session line alone.
  it.each([
    ["no key", undefined],
    ["an empty key", ""],
    ["a key with a prefix", "legion/ACME-7"],
    ["a key with a trailing space", "ACME-7 "],
    ["a value holding a single quote", "ACME-7'"],
    ["a value holding a backslash", "ACME\\-7"],
    ["a value holding a newline", "ACME-7\nDispatch: OTHER-1"],
    ["a project part over ten characters", "ABCDEFGHIJK-7"],
    ["a lowercase key", "acme-7"],
  ])("writes the session line alone for %s", async (_name, issueKey) => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey,
      workspace: await workspace(),
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      sessionLine
    );
  });

  it("carries a state directory holding a space and a single quote whole", async () => {
    const dir = path.join(await stateDir(), "state 'dir");
    const ws = await workspace();
    const env: NodeJS.ProcessEnv = {};
    const chain = await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      workspace: ws,
      env,
      resolveBase: async () => "/u/base.toml",
    });
    const overlay = path.join(dir, `omp-attribution-${ROOT_ID}.toml`);
    expect(chain).toBe(`/u/base.toml:${overlay}`);
    expect(await readFile(overlay, "utf8")).toBe(scopedOverlay(ws, "ACME-7"));
  });

  it("strips stale attribution overlays from an inherited JJ_CONFIG, keeping user layers", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {
      JJ_CONFIG: `/u/base.toml:/home/x/.cache/omp/jj/omp-attribution-${ROOT_ID.replace("b4a9", "dead")}.toml`,
    };
    await exportJjSessionAttribution(ROOT_ID, dir, {
      env,
      resolveBase: async () => {
        throw new Error("must not resolve");
      },
    });
    expect(env.JJ_CONFIG).toBe(`/u/base.toml:${path.join(dir, `omp-attribution-${ROOT_ID}.toml`)}`);
  });

  it("re-resolves the base when the inherited chain was only stale overlays", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = { JJ_CONFIG: "/tmp/omp-attribution-old.toml" };
    await exportJjSessionAttribution(ROOT_ID, dir, {
      env,
      resolveBase: async () => "/u/base.toml",
    });
    expect(env.JJ_CONFIG).toBe(`/u/base.toml:${path.join(dir, `omp-attribution-${ROOT_ID}.toml`)}`);
  });

  it("returns the live chain and leaves the file alone when the overlay is already chained", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    const first = await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      env,
      resolveBase: async () => "/u/base.toml",
    });
    const overlay = path.join(dir, `omp-attribution-${ROOT_ID}.toml`);
    const written = await readFile(overlay, "utf8");

    const again = await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "OTHER-9",
      env,
      resolveBase: async () => {
        throw new Error("must not resolve");
      },
    });
    expect(again).toBe(first);
    expect(env.JJ_CONFIG).toBe(first);
    expect(await readFile(overlay, "utf8")).toBe(written);
  });

  it("publishes the clean base and exports nothing when the overlay cannot be written", async () => {
    const dir = await stateDir();
    const blocked = path.join(dir, "not-a-directory");
    await Bun.write(blocked, "occupied");
    const env: NodeJS.ProcessEnv = { JJ_CONFIG: "/u/base.toml:/tmp/omp-attribution-old.toml" };
    const chain = await exportJjSessionAttribution(ROOT_ID, path.join(blocked, "nested"), {
      issueKey: "ACME-7",
      env,
    });
    expect(chain).toBeUndefined();
    expect(env.JJ_CONFIG).toBe("/u/base.toml"); // stale overlay gone, no new one
    expect(await readFile(blocked, "utf8")).toBe("occupied");
  });

  it("leaves the environment untouched when jj is unavailable", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    expect(
      await exportJjSessionAttribution(ROOT_ID, dir, { env, resolveBase: async () => undefined })
    ).toBeUndefined();
    expect(env.JJ_CONFIG).toBeUndefined();
  });

  // The id reaches both the TOML literal and the overlay's file name, so an id that is not one
  // (`SESSION_ID_PATH_SEGMENT`) passes nothing on: a single quote would end the literal and break
  // every `jj` command in the pane, and a `/` would move the overlay out of the state directory.
  it.each([
    ["an empty session id", ""],
    ["a session id holding a single quote", "s'1"],
    ["a session id holding a slash", "../s-1"],
    ["a session id holding a backslash", "s\\1"],
    ["a session id over 128 characters", "s".repeat(129)],
  ])("leaves the environment untouched and writes nothing for %s", async (_name, sessionId) => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    expect(
      await exportJjSessionAttribution(sessionId, dir, {
        env,
        resolveBase: async () => "/u/base.toml",
      })
    ).toBeUndefined();
    expect(env.JJ_CONFIG).toBeUndefined();
    expect(await readdir(dir)).toEqual([]);
  });
});

describe("resolvedWorkspace", () => {
  const dirs: string[] = [];
  afterEach(async () => {
    await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
  });
  async function scratch(): Promise<string> {
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-resolve-"));
    dirs.push(dir);
    return await realpath(dir);
  }

  it("answers the real path of an existing directory", async () => {
    const dir = await scratch();
    expect(resolvedWorkspace(dir)).toBe(dir);
  });

  it("follows a symlink to the path jj would resolve the workspace to", async () => {
    const dir = await scratch();
    const real = path.join(dir, "real");
    await mkdir(real);
    const link = path.join(dir, "link");
    await symlink(real, link);
    expect(resolvedWorkspace(link)).toBe(real);
  });

  // A relative path matches no scope and says nothing about it, so the absolute one is written.
  it("answers an absolute path for a relative one", async () => {
    const dir = await scratch();
    const relative = path.relative(process.cwd(), dir);
    expect(path.isAbsolute(relative)).toBe(false);
    expect(resolvedWorkspace(relative)).toBe(dir);
  });

  it.each([
    ["an unset workspace", undefined],
    ["an empty workspace", ""],
  ])("answers undefined for %s", (_name, workspace) => {
    expect(resolvedWorkspace(workspace)).toBeUndefined();
  });

  it("answers undefined for a path that does not exist", async () => {
    expect(resolvedWorkspace(path.join(await scratch(), "gone"))).toBeUndefined();
  });
});

describe("userConfigChain", () => {
  const dirs: string[] = [];
  /** An environment whose PATH finds a `jj` running `body` first. */
  async function withJj(body: string): Promise<NodeJS.ProcessEnv> {
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-bin-"));
    dirs.push(dir);
    await Bun.write(path.join(dir, "jj"), `#!/bin/sh\n${body}\n`);
    await chmod(path.join(dir, "jj"), 0o755);
    return { ...process.env, PATH: `${dir}:${process.env.PATH ?? ""}` };
  }
  // The same hang guard the rows below carry, for the same reason: removing a handful of
  // temporary directories is not work this suite measures, but it runs at bun's implicit 5 s
  // hook limit, which a loaded machine exceeds — at 240 busy loops on 2 CPUs this hook timed out
  // once in 20 runs and failed a row that had already passed its assertion.
  afterEach(async () => {
    await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
  }, 60_000);
  // Real-time polling is required here: the wrapper's pidfile and the kernel reaping its child
  // are external OS state, which Bun's fake timers cannot advance. Neither helper carries a
  // deadline of its own any more: each one's 3 s deadline sat below this file's 5 s per-test
  // limit, and the two run in series inside one row, so on a loaded machine the row hit the
  // runner's limit before either helper could report what it saw. The per-test limit on each row
  // below is the single bound, and the fake jj sleeps long past it, so a wait that is never
  // bounded and a kill that misses the child both end the row at that limit.
  /** The wrapper's child pid, once it has written it. */
  async function pidFrom(pidFile: string): Promise<number> {
    for (;;) {
      const pid = await readFile(pidFile, "utf8")
        .then((value) => Number(value.trim()))
        .catch(() => 0);
      if (Number.isInteger(pid) && pid > 0) return pid;
      await Bun.sleep(10);
    }
  }

  /** Returns once signal 0 says the kernel has reaped the child. */
  async function reaped(pid: number): Promise<void> {
    for (;;) {
      try {
        process.kill(pid, 0);
      } catch {
        return;
      }
      await Bun.sleep(10);
    }
  }

  it("chains the user config files jj prints, in order", async () => {
    const env = await withJj("printf '/u/a.toml\\n/u/b.toml\\n'");
    expect(await userConfigChain(5_000, env)).toBe("/u/a.toml:/u/b.toml");
  });

  // Each of the three rows below runs a fake jj that sleeps 300 s, far above every per-test limit
  // in this file. So a `userConfigChain` that stopped bounding its wait, or a kill that leaves the
  // forked child behind, cannot return inside the row's limit, and that limit — not a reading of
  // the clock — is what tells a correct run from the defect. The bound each row passes
  // `userConfigChain` is named at its own call below.
  it("gives up on a jj that never answers, so attribution never holds bootstrap", async () => {
    const env = await withJj("exec sleep 300");
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-"));
    dirs.push(dir);
    const exported: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      env: exported,
      resolveBase: () => userConfigChain(200, env),
    });
    expect(exported.JJ_CONFIG).toBeUndefined();
    // A hang guard, not a measurement: a resolve that waits the whole 300 s sleep out hangs the
    // row, and a real jj subprocess leaves a loaded machine far more room than bun's 5 s default.
  }, 60_000);

  it("kills a wrapper's forked child when jj times out", async () => {
    const pidFile = path.join(await mkdtemp(path.join(tmpdir(), "jj-attribution-child-")), "pid");
    dirs.push(path.dirname(pidFile));
    const env = await withJj(`sleep 300 & echo $! > ${JSON.stringify(pidFile)}; wait`);
    // The bound has to outlast the host's own process start: a group killed before the wrapper
    // has forked writes no pid, and the row then waits for something that cannot arrive. Measured
    // in this pod at 240 busy loops on 2 CPUs, the wrapper takes up to 3.1 s (p50 995 ms) to
    // write its pid, against 14 ms unloaded; 10 s is the production default, about three times
    // that worst case and six times below this row's 60 s limit, so it cannot decide a healthy
    // run. `sleep 300` means jj still times out, so the row's subject — the bound fires, the
    // group is killed, the forked child dies — is unchanged. The row above keeps 200 ms because
    // it needs no fork: its subject is only that the resolve gives up and exports nothing.
    // Undefined is the bound firing rather than jj answering, which is this row's premise.
    expect(await userConfigChain(JJ_CONFIG_PATH_TIMEOUT_MS, env)).toBeUndefined();
    const pid = await pidFrom(pidFile);
    try {
      // The check: this returns only once the child is gone, and the child's own sleep outlives
      // the row, so a kill that never reached it ends the row at the limit below.
      await reaped(pid);
    } finally {
      try {
        process.kill(pid, "SIGKILL");
      } catch {
        // The expected path: the process group killed it first.
      }
    }
    // A hang guard, not a measurement: an unbounded wait on the wrapper, a pidfile that is never
    // written, or a forked child the kill misses each hangs the row rather than passing it.
  }, 60_000);

  it("kills a wrapper's detached child after jj returns a config path", async () => {
    const pidFile = path.join(await mkdtemp(path.join(tmpdir(), "jj-attribution-child-")), "pid");
    dirs.push(path.dirname(pidFile));
    const env = await withJj(
      `sleep 300 </dev/null >/dev/null 2>&1 & echo $! > ${JSON.stringify(pidFile)}; printf '/u/a.toml\\n'`
    );
    // The production default, as in the row above: this row's subject is that the detached child
    // is killed after jj answers, and `killGroup` runs off the child's own exit, so the bound
    // decides nothing the row is about. It is a hang guard on jj itself — what can hang is a
    // `jj` that never prints — far above the work a loaded machine has to do here and far below
    // this row's 60 s limit, so the runner's limit stays the discriminator. Only the row that
    // gives up on jj keeps 200 ms, because its subject is that bound firing and it needs no fork.
    const paths = await userConfigChain(JJ_CONFIG_PATH_TIMEOUT_MS, env);
    const pid = await pidFrom(pidFile);
    try {
      expect(paths).toBe("/u/a.toml");
      // The check, as above: the detached child's 300 s sleep outlives the row unless it is killed.
      await reaped(pid);
    } finally {
      try {
        process.kill(pid, "SIGKILL");
      } catch {
        // The expected path: the process group killed it first.
      }
    }
    // A hang guard, not a measurement: a pidfile that is never written, or a detached child the
    // kill misses, hangs the row rather than passing it.
  }, 60_000);

  it("bounds jj at 10 s, inside the 30 s Oh My Pi gives the session_start handler", () => {
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBe(10_000);
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBeLessThan(30_000);
  });
});
