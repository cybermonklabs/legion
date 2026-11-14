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
  /** The unscoped table the writer composes, written out line by line: the pair is the session
   * line, then the role line when `role` is given. */
  function unscopedTable(role?: string): string {
    const pair: [string, string][] = [["Omp-Session", ROOT_ID]];
    if (role !== undefined) pair.push(["Legion-Role", role]);
    return [
      "[templates]",
      `commit_trailers = '"${pair.map(([key, value]) => `${key}: ${value}`).join("\\n")}"'`,
      "revert_description = '''",
      "concat(",
      `  'Revert "' ++ description.first_line() ++ '"' ++ "\\n",`,
      `  "\\n",`,
      `  "This reverts commit " ++ commit_id ++ ".\\n",`,
      `  "\\n",`,
      ...pair.map(([key, value]) => `  "${key}: ${value}\\n",`),
      ")",
      "'''",
      "duplicate_description = '''",
      "concat(",
      "  description,",
      "  if(description, concat(",
      `    if(!trailers, "\\n"),`,
      ...pair.map(
        ([key, value]) =>
          `    if(!trailers.any(|t| t.key() == "${key}" && t.value() == "${value}"), "${key}: ${value}\\n"),`
      ),
      "  )),",
      ")",
      "'''",
      "",
    ].join("\n");
  }
  /** The overlay with the session line alone: no role and no scope. */
  const sessionLine = unscopedTable();
  /** A real directory to scope an overlay to, under a scratch directory the afterEach removes. */
  async function workspace(name = "ws"): Promise<string> {
    const dir = path.join(await stateDir("jj-attribution-ws-"), name);
    await mkdir(dir, { recursive: true });
    return await realpath(dir);
  }
  /** The overlay the writer composes: the unscoped table, then the scope carrying the pair and the
   * issue line. `inToml` is the workspace path as a TOML basic string's body, escapes included. */
  function scopedOverlay(inToml: string, issueKey: string, role?: string): string {
    const roleLine = role === undefined ? "" : `\\nLegion-Role: ${role}`;
    return (
      `${unscopedTable(role)}\n[[--scope]]\n--when.workspaces = ["${inToml}"]\n[--scope.templates]\n` +
      `commit_trailers = '"Omp-Session: ${ROOT_ID}${roleLine}\\nDispatch: ${issueKey}"'\n`
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

  // The whole text, written out once: the session line, then the role line, in each template.
  it("writes the pair in order into the three templates of the unscoped table", async () => {
    const dir = await stateDir();
    await exportJjSessionAttribution(ROOT_ID, dir, {
      role: "tester",
      env: {},
      resolveBase: async () => "/u/base.toml",
    });
    const text = await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8");
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
    expect(text).toBe(unscopedTable("tester"));
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
    const text = await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8");
    expect(text).toBe(scopedOverlay(ws, "ACME-7", "tester"));
    expect(text).toEndWith(
      `\n[--scope.templates]\ncommit_trailers = '"Omp-Session: ${ROOT_ID}\\nLegion-Role: tester\\nDispatch: ACME-7"'\n`
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
    const text = await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8");
    expect(text).toBe(unscopedTable("controller"));
    expect(text).toContain(
      `commit_trailers = '"Omp-Session: ${ROOT_ID}\\nLegion-Role: controller"'`
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
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      sessionLine
    );
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
    expect(await readFile(path.join(dir, `omp-attribution-${ROOT_ID}.toml`), "utf8")).toBe(
      sessionLine
    );
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
      const jj = async (...args: string[]): Promise<string> => {
        const child = Bun.spawn([JJ, ...args], {
          cwd: repo,
          env,
          stdin: "ignore",
          stdout: "pipe",
          stderr: "pipe",
        });
        const [code, stdout, stderr] = await Promise.all([
          child.exited,
          new Response(child.stdout).text(),
          new Response(child.stderr).text(),
        ]);
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
    60_000
  );
});

describe("paneAttributionOptions", () => {
  it.each([...LEGION_ROLES, "controller"])("passes LEGION_ROLE=%s on as the role", (role) => {
    expect(paneAttributionOptions({ LEGION_ROLE: role })).toEqual({ role });
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
  afterEach(async () => {
    await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
  });
  /** The wrapper's child pid, once it has written it. */
  async function pidFrom(pidFile: string): Promise<number> {
    const deadline = Date.now() + 3_000;
    while (Date.now() < deadline) {
      const pid = await readFile(pidFile, "utf8")
        .then((value) => Number(value.trim()))
        .catch(() => 0);
      if (Number.isInteger(pid) && pid > 0) return pid;
      await Bun.sleep(10);
    }
    throw new Error(`jj wrapper did not write ${pidFile}`);
  }

  // Real-time polling is required here: the wrapper's pidfile and the kernel reaping its child
  // are external OS state, which Bun's fake timers cannot advance.
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

  it("kills a wrapper's forked child when jj times out", async () => {
    const pidFile = path.join(await mkdtemp(path.join(tmpdir(), "jj-attribution-child-")), "pid");
    dirs.push(path.dirname(pidFile));
    const env = await withJj(`sleep 30 & echo $! > ${JSON.stringify(pidFile)}; wait`);
    const started = Date.now();
    await userConfigChain(200, env);
    const pid = await pidFrom(pidFile);
    try {
      expect(Date.now() - started).toBeLessThan(5_000);
      expect(await stops(pid)).toBe(true);
    } finally {
      try {
        process.kill(pid, "SIGKILL");
      } catch {
        // The expected path: the process group killed it first.
      }
    }
  });

  it("kills a wrapper's detached child after jj returns a config path", async () => {
    const pidFile = path.join(await mkdtemp(path.join(tmpdir(), "jj-attribution-child-")), "pid");
    dirs.push(path.dirname(pidFile));
    const env = await withJj(
      `sleep 30 </dev/null >/dev/null 2>&1 & echo $! > ${JSON.stringify(pidFile)}; printf '/u/a.toml\\n'`
    );
    const paths = await userConfigChain(200, env);
    const pid = await pidFrom(pidFile);
    try {
      expect(paths).toBe("/u/a.toml");
      expect(await stops(pid)).toBe(true);
    } finally {
      try {
        process.kill(pid, "SIGKILL");
      } catch {
        // The expected path: the process group killed it first.
      }
    }
  });

  it("bounds jj at 10 s, inside the 30 s Oh My Pi gives the session_start handler", () => {
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBe(10_000);
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBeLessThan(30_000);
  });
});
