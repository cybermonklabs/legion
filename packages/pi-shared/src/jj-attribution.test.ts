import { afterEach, describe, expect, it } from "bun:test";
import { chmod, mkdir, mkdtemp, readdir, readFile, realpath, rm, symlink } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { LEGION_ROLES } from "@legion/contracts";
import {
  exportJjSessionAttribution,
  JJ_CONFIG_PATH_TIMEOUT_MS,
  paneAttributionOptions,
  resolvedWorkspace,
  sessionIdFromTranscript,
  userConfigChain,
} from "./jj-attribution";

const ROOT_TRANSCRIPT =
  "/sessions/-legion-default/2026-08-30T03-47-40-329Z_01a050c7-b4a9-7000-a539-240e20271441.jsonl";
const ROOT_ID = "01a050c7-b4a9-7000-a539-240e20271441";
/** Another session's id, for a commit that already carries that session's pair. */
const OTHER_ID = ROOT_ID.replace("b4a9", "dead");
/** The real jj on PATH, if any, and whether this run is on GitHub Actions (`GITHUB_ACTIONS`, not
 * `CI`, which agent harnesses on the devbox export), as `ompNatsGuard` reads them. */
const JJ = Bun.which("jj");
const ON_ACTIONS = process.env.GITHUB_ACTIONS === "true";
/** The real-jj case's bound on all its jj commands together, in milliseconds. Each command waits
 * for its jj to exit, and the one still running when the bound passes is killed and named. The 34
 * commands took 10 to 13 s on the devbox at a 1-minute load of 340, and passed 60 s in 4 of 20
 * runs of this file at 1-minute loads of 508 to 704. */
const JJ_COMMANDS_BOUND_MS = 240_000;
/** What bun gives the real-jj case past JJ_COMMANDS_BOUND_MS, in milliseconds: time to kill the
 * command that passed it and for the case's own file writes. bun's bound on a case must never be
 * the one that passes first, because bun runs afterEach while a case it timed out still runs:
 * here, jj in the repository that afterEach removes. */
const JJ_CASE_MARGIN_MS = 30_000;
/** Each afterEach's bound on removing its case's directories, in milliseconds. A hook that states
 * no bound gets bun's 5 s, and this file's afterEach passed that in 16 of 20 runs on the devbox at
 * a 1-minute load of 466 to 703, removing the real-jj case's repository (about 230 entries). */
const REMOVE_BOUND_MS = 30_000;
/** bun's bound on each afterEach, in milliseconds: past REMOVE_BOUND_MS, so that the removal's own
 * bound passes first and names the directories it left. */
const AFTER_EACH_BOUND_MS = REMOVE_BOUND_MS + 5_000;

/** Whether `work` settles within `ms` milliseconds; a rejection of `work` rejects. Real time is
 * required: a jj process and the filesystem are outside bun, and its fake timers cannot advance
 * them. */
async function settlesWithin(work: Promise<unknown>, ms: number): Promise<boolean> {
  const passed = Promise.withResolvers<false>();
  const timer = setTimeout(() => passed.resolve(false), ms);
  try {
    return await Promise.race([work.then(() => true), passed.promise]);
  } finally {
    clearTimeout(timer);
  }
}

/** Removes `dirs`, or fails naming them when the removal has not finished within
 * REMOVE_BOUND_MS. */
async function removeWithin(dirs: readonly string[]): Promise<void> {
  const removed = Promise.all(dirs.map((dir) => rm(dir, { recursive: true, force: true })));
  if (!(await settlesWithin(removed, REMOVE_BOUND_MS))) {
    throw new Error(`removing ${dirs.join(" ")} did not finish within ${REMOVE_BOUND_MS} ms`);
  }
}

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
  afterEach(() => removeWithin(dirs.splice(0)), AFTER_EACH_BOUND_MS);
  /** The overlay the writer wrote under `dir`. */
  async function overlayText(dir: string): Promise<string> {
    return await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8");
  }
  /** The overlay the writer wrote under `dir`, cut where its `[[--scope]]` table starts: the
   * unscoped `[templates]` table, then the scope, or undefined when the overlay has none. */
  async function overlayTables(
    dir: string
  ): Promise<[unscoped: string, scope: string | undefined]> {
    const text = await overlayText(dir);
    const at = text.indexOf("[[--scope]]");
    return at < 0 ? [text, undefined] : [text.slice(0, at), text.slice(at)];
  }
  /** Checks that the overlay under `dir` gives the session line alone: each of the three templates
   * holds the session line, the `commit_trailers` line holds nothing more, no line names
   * `Legion-Role` or `Dispatch`, and there is no scope. */
  async function expectSessionLineAlone(dir: string): Promise<void> {
    const [unscoped, scope] = await overlayTables(dir);
    expect(scope).toBeUndefined();
    const lines = unscoped.split("\n");
    expect(lines).toContain(`commit_trailers = '"Omp-Session: ${ROOT_ID}"'`);
    expect(lines).toContain(`  "Omp-Session: ${ROOT_ID}\\n",`);
    expect(lines).toContain(
      `    if(!trailers.any(|t| t.key() == "Omp-Session" && t.value() == "${ROOT_ID}"), "Omp-Session: ${ROOT_ID}\\n"),`
    );
    expect(unscoped).not.toContain("Legion-Role");
    expect(unscoped).not.toContain("Dispatch");
  }
  /** A real directory to scope an overlay to, under a scratch directory the afterEach removes. */
  async function workspace(name = "ws"): Promise<string> {
    const dir = path.join(await stateDir("jj-attribution-ws-"), name);
    await mkdir(dir, { recursive: true });
    return await realpath(dir);
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
    await expectSessionLineAlone(dir);
  });

  // The whole text, written out once: the session line, then the role line, in each template.
  it("writes the pair in order into the three templates of the unscoped table", async () => {
    const dir = await stateDir();
    await exportJjSessionAttribution(ROOT_ID, dir, {
      role: "tester",
      env: {},
      resolveBase: async () => "/u/base.toml",
    });
    const text = await overlayText(dir);
    expect(text).toBe(
      [
        "[templates]",
        `commit_trailers = '"Omp-Session: ${ROOT_ID}\\nLegion-Role: tester"'`,
        "revert_description = '''",
        "concat(",
        `  'Revert "' ++ description.first_line() ++ '"' ++ "\\n",`,
        `  "\\n",`,
        `  "This reverts commit " ++ commit_id ++ ".\\n",`,
        `  "\\n",`,
        `  "Omp-Session: ${ROOT_ID}\\n",`,
        `  "Legion-Role: tester\\n",`,
        ")",
        "'''",
        "duplicate_description = '''",
        "concat(",
        "  description,",
        "  if(description, concat(",
        `    if(!trailers, "\\n"),`,
        `    if(!trailers.any(|t| t.key() == "Omp-Session" && t.value() == "${ROOT_ID}"), "Omp-Session: ${ROOT_ID}\\n"),`,
        `    if(!trailers.any(|t| t.key() == "Legion-Role" && t.value() == "tester"), "Legion-Role: tester\\n"),`,
        "  )),",
        ")",
        "'''",
        "",
      ].join("\n")
    );
  });

  it("scopes the pair, then the Dispatch line, to the pane's workspace", async () => {
    const dir = await stateDir();
    const ws = await workspace();
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      workspace: ws,
      role: "tester",
      env: {},
      resolveBase: async () => "/u/base.toml",
    });
    const [unscoped, scope] = await overlayTables(dir);
    expect(unscoped.split("\n")).toContain(
      `commit_trailers = '"Omp-Session: ${ROOT_ID}\\nLegion-Role: tester"'`
    );
    expect(unscoped).not.toContain("Dispatch");
    // The scope, written out once: the pair, then the issue line, for this workspace alone.
    expect(scope).toBe(
      [
        "[[--scope]]",
        `--when.workspaces = ["${ws}"]`,
        "[--scope.templates]",
        `commit_trailers = '"Omp-Session: ${ROOT_ID}\\nLegion-Role: tester\\nDispatch: ACME-7"'`,
        "",
      ].join("\n")
    );
  });

  // `legion controller start` sets LEGION_ROLE=controller, which is no LegionRole.
  it("writes Legion-Role: controller for the controller", async () => {
    const dir = await stateDir();
    await exportJjSessionAttribution(ROOT_ID, dir, {
      ...paneAttributionOptions({ LEGION_ROLE: "controller" }),
      env: {},
      resolveBase: async () => "/u/base.toml",
    });
    const lines = (await overlayText(dir)).split("\n");
    expect(lines).toContain(
      `commit_trailers = '"Omp-Session: ${ROOT_ID}\\nLegion-Role: controller"'`
    );
    expect(lines).toContain(`  "Legion-Role: controller\\n",`);
    expect(lines).toContain(
      `    if(!trailers.any(|t| t.key() == "Legion-Role" && t.value() == "controller"), "Legion-Role: controller\\n"),`
    );
  });

  // The role is read from LEGION_ROLE in one place, and only a role name passes it on.
  it.each([
    ["unset", undefined],
    ["empty", ""],
    ["bogus", "bogus"],
    ['x"y', 'x"y'],
  ])("writes the session line alone for LEGION_ROLE %s", async (_name, role) => {
    const dir = await stateDir();
    const options = paneAttributionOptions(role === undefined ? {} : { LEGION_ROLE: role });
    expect(options).not.toHaveProperty("role");
    await exportJjSessionAttribution(ROOT_ID, dir, {
      ...options,
      env: {},
      resolveBase: async () => "/u/base.toml",
    });
    await expectSessionLineAlone(dir);
  });

  // Containment, as for the issue key: the role is interpolated into a jj template inside a TOML
  // literal, so the writer checks a caller's value again.
  it.each([
    ["a value that is no role", "bogus"],
    ["a value holding a double quote", 'x"y'],
    ["a role with a trailing newline", "tester\n"],
  ])("writes the session line alone for %s passed as the role", async (_name, role) => {
    const dir = await stateDir();
    await exportJjSessionAttribution(ROOT_ID, dir, {
      role,
      env: {},
      resolveBase: async () => "/u/base.toml",
    });
    await expectSessionLineAlone(dir);
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
    const [unscoped, scope] = await overlayTables(dir);
    expect(unscoped).not.toContain("Dispatch");
    const scoped = scope?.split("\n");
    expect(scoped).toContain(`--when.workspaces = ["${ws}"]`);
    expect(scoped).toContain(`commit_trailers = '"Omp-Session: ${ROOT_ID}\\nDispatch: ACME-7"'`);
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
    await expectSessionLineAlone(dir);
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
    const [, scope] = await overlayTables(dir);
    expect(scope?.split("\n")).toContain(`--when.workspaces = ["${real}"]`);
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
    await expectSessionLineAlone(dir);
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
    // The last name of the path, `ws "quoted\odd`, with its `"` and its `\` escaped for TOML.
    const [, scope] = await overlayTables(dir);
    expect(scope?.split("\n")).toContain(
      `--when.workspaces = ["${path.dirname(ws)}/ws \\"quoted\\\\odd"]`
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
    await expectSessionLineAlone(dir);
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
    ["a project part over ten ASCII characters", "ABCDEFGHIJK-7"],
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
    await expectSessionLineAlone(dir);
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
    expect(await readFile(overlay, "utf8")).toStartWith("[templates]\n");
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
    ["a session id over 128 ASCII characters", "s".repeat(129)],
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

  // The one case here that runs the real jj, which is not on every host: it skips where PATH has
  // none, except on GitHub Actions, where the pi-shared job installs one and a skip would hide
  // the only run of the check (as `ompNatsGuard` does for Oh My Pi, `test/omp-harness.ts`).
  it.skipIf(JJ === null && !ON_ACTIONS)(
    "the overlay's three templates give each jj command the running pair",
    async () => {
      if (JJ === null) throw new Error("no jj on PATH on GitHub Actions (install-jj installs one)");
      const root = await stateDir("jj-attribution-jj-");
      // A base chain and a home of the test's own, so no jj or git configuration of the host
      // reaches the repository, and the identity jj needs comes from JJ_USER and JJ_EMAIL.
      const base = path.join(root, "base.toml");
      await Bun.write(base, 'ui.color = "never"\n');
      const env: NodeJS.ProcessEnv = {
        PATH: process.env.PATH ?? "",
        HOME: root,
        XDG_CONFIG_HOME: path.join(root, "config"),
        JJ_USER: "Attribution Test",
        JJ_EMAIL: "attribution-test@example.invalid",
      };
      const state = path.join(root, "state");
      const chain = await exportJjSessionAttribution(ROOT_ID, state, {
        role: "tester",
        env,
        resolveBase: async () => base,
      });
      expect(chain).toBe(`${base}:${path.join(state, `omp-attribution-${ROOT_ID}.toml`)}`);
      const repo = path.join(root, "repo");
      await mkdir(repo);
      // Each command waits for its jj to exit, within what is left of the case's bound on its jj
      // commands; the one still running when that bound passes is killed, and the case fails
      // naming it, before bun's own bound on the case can pass.
      const deadline = Date.now() + JJ_COMMANDS_BOUND_MS;
      const jj = async (...args: string[]): Promise<string> => {
        const child = Bun.spawn([JJ, ...args], {
          cwd: repo,
          env,
          stdin: "ignore",
          stdout: "pipe",
          stderr: "pipe",
        });
        const done = Promise.all([
          child.exited,
          new Response(child.stdout).text(),
          new Response(child.stderr).text(),
        ]);
        if (!(await settlesWithin(done, deadline - Date.now()))) {
          child.kill("SIGKILL");
          await child.exited;
          throw new Error(
            `jj ${args.join(" ")} (pid ${child.pid}) had not exited when the case's ${JJ_COMMANDS_BOUND_MS} ms bound on its jj commands passed; the case killed it`
          );
        }
        const [code, stdout, stderr] = await done;
        if (code !== 0) throw new Error(`jj ${args.join(" ")} exited ${code}: ${stderr}`);
        return stdout;
      };
      const description = (revision: string) =>
        jj("log", "--no-graph", "-r", revision, "-T", "description");
      const pair = `Omp-Session: ${ROOT_ID}\nLegion-Role: tester\n`;

      await jj("git", "init");
      await Bun.write(path.join(repo, "a"), "a\n");
      await jj("commit", "-m", "commit message");
      expect(await description("@-")).toBe(`commit message\n\n${pair}`);
      await jj("describe", "-m", "describe message");
      expect(await description("@")).toBe(`describe message\n\n${pair}`);
      await Bun.write(path.join(repo, "b"), "b\n");
      await Bun.write(path.join(repo, "c"), "c\n");
      await jj("split", "-m", "split message", "b");
      expect(await description("@-")).toBe(`split message\n\n${pair}`);
      const split = (await jj("log", "--no-graph", "-r", "@-", "-T", "commit_id")).trim();
      await jj("revert", "-r", "@-", "--insert-before", "@");
      expect(await description("@-")).toBe(
        `Revert "split message"\n\nThis reverts commit ${split}.\n\n${pair}`
      );

      // Each source is made with no trailer template, so its description is the one given, and
      // its duplicate is the one commit the duplicate adds.
      const changes = async () =>
        (await jj("log", "--no-graph", "-r", "all()", "-T", 'change_id ++ "\\n"'))
          .split("\n")
          .filter(Boolean);
      const duplicateOf = async (message: string | undefined): Promise<string> => {
        const given = message === undefined ? [] : ["-m", message];
        await jj("new", "--config", 'templates.commit_trailers=""', ...given, "root()");
        expect(await description("@")).toBe(message === undefined ? "" : `${message}\n`);
        const before = new Set(await changes());
        await jj("duplicate", "@");
        const [duplicate, ...more] = (await changes()).filter((change) => !before.has(change));
        expect(more).toEqual([]);
        if (duplicate === undefined) throw new Error("jj duplicate added no commit");
        return await description(duplicate);
      };
      const other = `Omp-Session: ${OTHER_ID}\nLegion-Role: implementer\n`;
      expect(await duplicateOf("no trailer")).toBe(`no trailer\n\n${pair}`);
      expect(await duplicateOf(`another pair\n\n${other.trimEnd()}`)).toBe(
        `another pair\n\n${other}${pair}`
      );
      expect(await duplicateOf(`the running pair\n\n${pair.trimEnd()}`)).toBe(
        `the running pair\n\n${pair}`
      );
      expect(await duplicateOf(undefined)).toBe("");
    },
    JJ_COMMANDS_BOUND_MS + JJ_CASE_MARGIN_MS
  );
});

describe("paneAttributionOptions", () => {
  it.each([...LEGION_ROLES, "controller"])("passes LEGION_ROLE=%s on as the role", (role) => {
    expect(paneAttributionOptions({ LEGION_ROLE: role })).toEqual({ role });
  });
});

describe("resolvedWorkspace", () => {
  const dirs: string[] = [];
  afterEach(() => removeWithin(dirs.splice(0)), AFTER_EACH_BOUND_MS);
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
  /**
   * A closed environment whose PATH finds a `jj` running `body` first: PATH and a home of the
   * test's own, so nothing the developer's environment holds reaches the stub.
   */
  async function withJj(body: string): Promise<NodeJS.ProcessEnv> {
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-bin-"));
    dirs.push(dir);
    await Bun.write(path.join(dir, "jj"), `#!/bin/sh\n${body}\n`);
    await chmod(path.join(dir, "jj"), 0o755);
    return { PATH: `${dir}:${process.env.PATH ?? ""}`, HOME: dir };
  }
  afterEach(() => removeWithin(dirs.splice(0)), AFTER_EACH_BOUND_MS);
  /** The child pid the wrapper wrote to `pidFile`, or undefined when it wrote none. Read once
   * userConfigChain has returned: by then the wrapper has exited, or its group has been sent
   * SIGKILL, so there is no later write to wait for, and a pid it had not written counts as none. */
  async function writtenPid(pidFile: string): Promise<number | undefined> {
    const pid = Number((await readFile(pidFile, "utf8").catch(() => "")).trim());
    return Number.isInteger(pid) && pid > 0 ? pid : undefined;
  }
  /** The largest bound the forked-child case tries, in milliseconds. */
  const FORK_BOUND_MAX_MS = 6_400;
  /** bun's bound on the forked-child case, in milliseconds: the bounds it tries add up to 12.6 s,
   * and `stops` waits up to 3 s. */
  const FORK_CASE_BOUND_MS = 30_000;

  // Real-time polling is required here: the kernel reaping the child is external OS state, which
  // Bun's fake timers cannot advance.
  /** The kernel has reaped the child once signal 0 says it no longer exists. */
  async function stops(pid: number): Promise<boolean> {
    const deadline = Date.now() + 3_000;
    while (Date.now() < deadline) {
      try {
        process.kill(pid, 0);
      } catch {
        return true;
      }
      await Bun.sleep(10);
    }
    return false;
  }

  it("chains the user config files jj prints, in order", async () => {
    const env = await withJj("printf '/u/a.toml\\n/u/b.toml\\n'");
    expect(await userConfigChain(5_000, env)).toBe("/u/a.toml:/u/b.toml");
  });

  it("gives up on a jj that never answers, so attribution never holds bootstrap", async () => {
    const env = await withJj("exec sleep 30");
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-"));
    dirs.push(dir);
    const exported: NodeJS.ProcessEnv = {};
    const started = Date.now();
    await exportJjSessionAttribution(ROOT_ID, dir, {
      env: exported,
      resolveBase: () => userConfigChain(200, env),
    });
    expect(Date.now() - started).toBeLessThan(5_000);
    expect(exported.JJ_CONFIG).toBeUndefined();
  });

  // The bound must pass after the wrapper has forked its child and written the child's pid, or the
  // group it kills holds no child to check, and a loaded host can take longer than a short bound to
  // get the wrapper there. So the case doubles the bound until a run has written the pid.
  it(
    "kills a wrapper's forked child when jj times out",
    async () => {
      for (let bound = 200; bound <= FORK_BOUND_MAX_MS; bound *= 2) {
        const pidFile = path.join(
          await mkdtemp(path.join(tmpdir(), "jj-attribution-child-")),
          "pid"
        );
        dirs.push(path.dirname(pidFile));
        const env = await withJj(`sleep 30 & echo $! > ${JSON.stringify(pidFile)}; wait`);
        const started = Date.now();
        await userConfigChain(bound, env);
        const returnedAfter = Date.now() - started;
        const pid = await writtenPid(pidFile);
        if (pid === undefined) continue;
        try {
          expect(returnedAfter).toBeLessThan(bound + 5_000);
          expect(await stops(pid)).toBe(true);
        } finally {
          try {
            process.kill(pid, "SIGKILL");
          } catch {
            // The expected path: the process group killed it first.
          }
        }
        return;
      }
      throw new Error(
        `jj wrapper wrote no child pid before a bound of up to ${FORK_BOUND_MAX_MS} ms passed`
      );
    },
    FORK_CASE_BOUND_MS
  );

  it(
    "kills a wrapper's detached child after jj returns a config path",
    async () => {
      const pidFile = path.join(await mkdtemp(path.join(tmpdir(), "jj-attribution-child-")), "pid");
      dirs.push(path.dirname(pidFile));
      const env = await withJj(
        `sleep 30 </dev/null >/dev/null 2>&1 & echo $! > ${JSON.stringify(pidFile)}; printf '/u/a.toml\\n'`
      );
      // The code's own bound: the case waits for jj to return the path, and the wrapper writes
      // the pid before it prints the path.
      const paths = await userConfigChain(JJ_CONFIG_PATH_TIMEOUT_MS, env);
      const pid = await writtenPid(pidFile);
      try {
        expect(paths).toBe("/u/a.toml");
        if (pid === undefined) throw new Error(`jj wrapper returned without writing ${pidFile}`);
        expect(await stops(pid)).toBe(true);
      } finally {
        if (pid !== undefined) {
          try {
            process.kill(pid, "SIGKILL");
          } catch {
            // The expected path: the process group killed it first.
          }
        }
      }
    },
    // Past the code's bound on jj, by the 3 s `stops` waits and some.
    JJ_CONFIG_PATH_TIMEOUT_MS + 5_000
  );

  it("bounds jj at 10 s, inside the 30 s Oh My Pi gives the session_start handler", () => {
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBe(10_000);
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBeLessThan(30_000);
  });
});
