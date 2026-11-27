import { afterEach, describe, expect, it } from "bun:test";
import { chmod, mkdir, mkdtemp, readdir, readFile, realpath, rm, symlink } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { LEGION_ROLES } from "@legion/contracts";
import { settled, until } from "@legion/test-fixtures/wait";
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
  /** The real jj command the templates row below is running, if any: its words and its process.
   *  A row Bun's timeout ends inside a command never clears this, so the afterEach right below
   *  finds the command here. */
  let jjRunning: { command: string; child: Bun.Subprocess<"ignore", "pipe", "pipe"> } | undefined;
  // The one process waited on here is a jj this hook has just sent SIGKILL. The body kills a jj
  // command a row left running when Bun's timeout ended it, waits for it to exit, and removes the
  // rows' directories. The kill and its exit come first, so the removal never races a jj still
  // working in its tree, and the error that names the command comes last, so it skips no removal.
  afterEach(async () => {
    const running = jjRunning;
    jjRunning = undefined;
    running?.child.kill("SIGKILL");
    await running?.child.exited;
    await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
    if (running !== undefined) {
      throw new Error(`${running.command} still ran when its row ended, killed here`);
    }
  });
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

  /** The jj commands the templates row below runs. The row checks the commands it ran (`ran`)
   *  against this count. A command added past the count fails the row naming that command. A
   *  command lost fails the count check at the row's end. */
  const TEMPLATE_ROW_JJ_COMMANDS = 34;

  // The one case here that runs the real jj, which is not on every host: it skips where PATH has
  // none, except on GitHub Actions, where the pi-shared job installs one and a skip would hide
  // the only run of the check (as `ompNatsGuard` does for Oh My Pi, `test/omp-harness.ts`). What
  // can hang in it is a real jj command, which runs with no deadline of its own: the command's
  // guard alone ends a jj that never ends, and the describe's first afterEach kills the command
  // and names it.
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
      let ran = 0;
      const jj = async (...args: string[]): Promise<string> => {
        const command = `jj ${args.join(" ")}`;
        ran += 1;
        if (ran > TEMPLATE_ROW_JJ_COMMANDS) {
          throw new Error(
            `${command} is past the ${TEMPLATE_ROW_JJ_COMMANDS} commands this row counts`
          );
        }
        const child = Bun.spawn([JJ, ...args], {
          cwd: repo,
          env,
          stdin: "ignore",
          stdout: "pipe",
          stderr: "pipe",
        });
        jjRunning = { command, child };
        try {
          const [code, stdout, stderr] = await Promise.all([
            child.exited,
            new Response(child.stdout).text(),
            new Response(child.stderr).text(),
          ]);
          if (code !== 0) throw new Error(`${command} exited ${code}: ${stderr}`);
          return stdout;
        } finally {
          if (jjRunning?.child === child) jjRunning = undefined;
        }
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
      // A command lost from the row fails this check. One added past the count failed at its call.
      expect(ran).toBe(TEMPLATE_ROW_JJ_COMMANDS);
    }
  );
});

describe("paneAttributionOptions", () => {
  it.each([...LEGION_ROLES, "controller"])("passes LEGION_ROLE=%s on as the role", (role) => {
    expect(paneAttributionOptions({ LEGION_ROLE: role })).toEqual({ role });
  });
});

describe("resolvedWorkspace", () => {
  const dirs: string[] = [];
  // Nothing here waits on another process: the body removes the rows' directories.
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
  /** The pidfiles a row's fake jj writes its pid to, each recorded when the row makes it, before
   *  anything the row awaits can hang: the afterEach below kills every pid it finds in them, so a
   *  child the code under test fails to kill is collected whether the row returns or the runner
   *  abandons it at the command's guard — a `userConfigChain` that never returns included. A
   *  pidfile leaves this list once `reaped` has seen its pid gone, so a green row leaves the
   *  afterEach no pid to kill. */
  const pidFiles: string[] = [];
  /** A pidfile for one fake jj, in a directory of its own, recorded for the afterEach. */
  async function childPidFile(): Promise<string> {
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-child-"));
    dirs.push(dir);
    const pidFile = path.join(dir, "pid");
    pidFiles.push(pidFile);
    return pidFile;
  }
  /** Advanced by the afterEach below. Each row that waits on its fake jj takes it at its first
   *  line, before any await, and hands it to `onRow`, whose probe ends that row's wait once the
   *  row has ended: a wait that a body Bun's timeout abandoned starts only after that, inside the
   *  next row, and ends there at once. */
  let rowGeneration = 0;
  /** The pid in `pidFile`, or undefined while the wrapper has not written one. */
  async function pidIn(pidFile: string): Promise<number | undefined> {
    const pid = await readFile(pidFile, "utf8")
      .then((value) => Number(value.trim()))
      .catch(() => 0);
    return Number.isInteger(pid) && pid > 0 ? pid : undefined;
  }
  // Nothing here waits on another process: the body reads the rows' pidfiles, kills what they name
  // and removes the rows' directories, which a loaded host stretches.
  afterEach(async () => {
    rowGeneration += 1;
    const survivors: number[] = [];
    for (const pidFile of pidFiles.splice(0)) {
      const pid = await pidIn(pidFile);
      if (pid === undefined || !(await running(pid))) continue;
      survivors.push(pid);
      try {
        process.kill(pid, "SIGKILL");
      } catch {
        // It ended between the look and the kill.
      }
    }
    await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
    if (survivors.length > 0) {
      throw new Error(`the wrapper's child ${survivors.join(", ")} outlived its row, killed here`);
    }
  });
  // Real-time polling is required here: the wrapper's pidfile and the kernel reaping its child
  // are external OS state, which Bun's fake timers cannot advance. Each such wait is the shared
  // `until`, which ends at the row's deadline and names what it waited for: a pidfile that is
  // never written, or a child the kill misses, fails the row there, and the afterEach above kills
  // the fake jj's `sleep 3600` either way instead of leaving it for an hour. Bun's timeout can
  // still end a row inside an await that is no such wait, and the body it abandons may reach a
  // wait after its row has ended. So each probe first checks `rowGeneration`, which the afterEach
  // advances, against the generation its row took at its first line, and throws once they differ:
  // that wait ends at once rather than run on inside the next row.
  /** A probe for `until` that throws once the row that took generation `row` has ended, and
   *  otherwise answers what `read` answers: a truthy value once the fact holds, else undefined. */
  function onRow<T>(row: number, what: string, read: () => Promise<T>): () => Promise<T> {
    return async () => {
      if (row !== rowGeneration) throw new Error(`stopped waiting for ${what}: the row ended`);
      return await read();
    };
  }

  /** Whether `pid` runs: signal 0 reaches it and `/proc` shows it no zombie, which nothing will
   *  run again. */
  async function running(pid: number): Promise<boolean> {
    const stat = await readFile(`/proc/${pid}/stat`, "utf8").catch(() => undefined);
    if (stat !== undefined && stat[stat.lastIndexOf(")") + 2] === "Z") return false;
    try {
      process.kill(pid, 0);
      return true;
    } catch {
      return false;
    }
  }

  /** Returns once `pid` no longer runs (`running`), and drops `pidFile` from the afterEach's list,
   *  so the afterEach never signals a pid the kernel has since given another process. */
  async function reaped(row: number, pidFile: string, pid: number): Promise<void> {
    const what = `pid ${pid} to end`;
    await until(
      what,
      onRow(row, what, async () => ((await running(pid)) ? undefined : true))
    );
    const at = pidFiles.indexOf(pidFile);
    if (at >= 0) pidFiles.splice(at, 1);
  }

  it("forgets a child once it is seen gone, so afterEach signals no pid reaped already", async () => {
    const row = rowGeneration;
    const pidFile = await childPidFile();
    const child = Bun.spawn(["true"]);
    await child.exited;
    await Bun.write(pidFile, `${child.pid}\n`);
    await reaped(row, pidFile, child.pid);
    expect(pidFiles).not.toContain(pidFile);
    // What can hang here is the spawn of `true`, its exit, or the reaping `reaped` waits for.
  });

  it("chains the user config files jj prints, in order", async () => {
    const env = await withJj("printf '/u/a.toml\\n/u/b.toml\\n'");
    // The bound, 120 s, is far past what a loaded host takes to start the fake jj, which answers
    // at once: how long that start takes is no part of this row's subject. A `jj` that never
    // prints ends at the bound, inside the command's guard, and fails the row on its undefined.
    expect(await userConfigChain(120_000, env)).toBe("/u/a.toml:/u/b.toml");
  });

  // Each of the three rows below runs a fake jj that sleeps 3600 s, far past the guard this
  // file's command sets. So a `userConfigChain` that stopped bounding its wait, or a kill that
  // leaves the forked child behind, cannot return before the row's deadline, and the shared wait
  // that holds there, failing by its own message, is what tells a correct run from the defect,
  // not a reading of the clock. The bound each row passes `userConfigChain` is named at its own
  // call below.
  it("gives up on a jj that never answers, so attribution never holds bootstrap", async () => {
    const row = rowGeneration;
    const pidFile = await childPidFile();
    const env = await withJj(`echo $$ > ${JSON.stringify(pidFile)} && exec sleep 3600`);
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-"));
    dirs.push(dir);
    const exported: NodeJS.ProcessEnv = {};
    await settled(
      "the attribution to give up on a jj that never answers",
      exportJjSessionAttribution(ROOT_ID, dir, {
        env: exported,
        resolveBase: () => userConfigChain(200, env),
      })
    );
    expect(exported.JJ_CONFIG).toBeUndefined();
    // The resolve gave up, so its bound killed the fake jj's group. A pid the jj wrote before that
    // is waited out here, which drops its pidfile; a jj killed before it wrote one is gone and left
    // nothing to collect, so this row never waits on a pidfile that may not come.
    const pid = await pidIn(pidFile);
    if (pid !== undefined) await reaped(row, pidFile, pid);
    // What can hang here is a resolve that waits the whole sleep out, which `settled` fails at
    // the row's deadline.
  });

  it("kills a wrapper's forked child when jj times out", async () => {
    const row = rowGeneration;
    const pidFile = await childPidFile();
    // The child records itself: `sh -c` writes its own pid and only then becomes the `sleep`, so a
    // kill that reaches the wrapper at any moment leaves no `sleep` unrecorded. A wrapper that
    // wrote `$!` after forking left a window in which a kill narrowed to the wrapper missed the
    // child and no pid was written.
    const env = await withJj(
      `sh -c 'echo $$ > "$1" && exec sleep 3600' sh ${JSON.stringify(pidFile)} & wait`
    );
    // The give-up comes once the wrapper has forked its child and the child has written its pid
    // (`giveUp`, which takes the same path the bound does), so how long the host takes to fork
    // decides nothing: a bound raced the fork, and a fork took up to 33478 ms at 240 busy loops on
    // one pod, past the production 10 s. The bound passed, 120 s, sits far past that fork and
    // inside the command's guard: it fires only for a wrapper that has not forked by then, and
    // the wait for its pidfile below then fails naming it. Undefined is the give-up, which is this
    // row's premise.
    const giveUp = new AbortController();
    const chain = userConfigChain(120_000, env, giveUp.signal);
    const written = `the jj wrapper to write ${pidFile}`;
    const pid = await until(
      written,
      onRow(row, written, () => pidIn(pidFile))
    );
    giveUp.abort();
    expect(await chain).toBeUndefined();
    // The check: this returns only once the child is gone, and the child's own sleep outlives the
    // row, so a kill that never reached it fails the row at the wait's deadline, naming the pid,
    // and the afterEach above collects the child.
    await reaped(row, pidFile, pid);
    // What can hang here is an unbounded wait on the wrapper, a pidfile that is never written, or
    // a forked child the kill misses.
  });

  it("kills a wrapper's detached child after jj returns a config path", async () => {
    const row = rowGeneration;
    const pidFile = await childPidFile();
    // The child records itself, as in the row above: `sh -c` writes its own pid and only then
    // becomes the `sleep`, so no `sleep` runs before its pid is on disk. The wrapper answers only
    // once the pidfile holds that pid as a whole line, for which `read` needs the newline. If the
    // row ends first, the afterEach removes the pidfile's directory. A child that has not written
    // by then fails its write, and `&&` keeps it from the `sleep`. The wrapper's wait sees the
    // directory gone and exits, and `userConfigChain` kills its group on that exit.
    const quotedDir = JSON.stringify(path.dirname(pidFile));
    const quotedFile = JSON.stringify(pidFile);
    const env = await withJj(
      [
        `sh -c 'echo $$ > "$1" && exec sleep 3600' sh ${quotedFile} </dev/null >/dev/null 2>&1 &`,
        `until read -r pid 2>/dev/null <${quotedFile}; do`,
        `  [ -d ${quotedDir} ] || exit 1`,
        "  sleep 0.01",
        "done",
        "printf '/u/a.toml\\n'",
      ].join("\n")
    );
    // This row's subject is that the detached child is killed after jj answers, and `killGroup`
    // runs off the wrapper's own exit, so the bound is no part of it. The production default of
    // 10 s was tried here: a starved host fired it before the fake jj printed, failing a correct
    // run 1 time in 20 at 240 busy loops on this two-CPU pod, and reproducibly when the fake jj
    // answers one second late. So the budget below, 120 s, sits far past that, inside the
    // command's guard: it ends a `jj` that never prints, and the row then fails on the undefined
    // it answers. The row above passes such a bound too, and gives up through its signal once the
    // wrapper's child has written its pid.
    const paths = await userConfigChain(120_000, env);
    expect(paths).toBe("/u/a.toml");
    // The wrapper answered, so the pidfile holds its child's pid: one read finds it.
    const pid = await pidIn(pidFile);
    if (pid === undefined) throw new Error(`the jj wrapper answered with no pid in ${pidFile}`);
    // The check, as above: the detached child's 3600 s sleep outlives the row unless it is killed.
    await reaped(row, pidFile, pid);
    // What can hang here is a pidfile that is never written, which the wrapper waits on, or a
    // detached child the kill misses.
  });

  it("bounds jj at 10 s, inside the 30 s Oh My Pi gives the session_start handler", () => {
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBe(10_000);
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBeLessThan(30_000);
  });
});
