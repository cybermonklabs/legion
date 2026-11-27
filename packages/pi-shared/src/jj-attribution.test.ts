import { afterEach, describe, expect, it } from "bun:test";
import { chmod, mkdir, mkdtemp, readdir, readFile, realpath, rm, symlink } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { LEGION_ROLES } from "@legion/contracts";
import {
  exportJjSessionAttribution,
  JJ_CONFIG_PATH_TIMEOUT_MS,
  paneAttributionOptions,
  resolvedRepository,
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
  /** A real directory to stand for the repository an overlay is scoped to, under a scratch
   * directory the afterEach removes. */
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

  it("scopes the pair, then the Dispatch line, to the pane's repository", async () => {
    const dir = await stateDir();
    const ws = await workspace();
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      repository: ws,
      role: "tester",
      env: {},
      resolveBase: async () => "/u/base.toml",
    });
    const [unscoped, scope] = await overlayTables(dir);
    expect(unscoped.split("\n")).toContain(
      `commit_trailers = '"Omp-Session: ${ROOT_ID}\\nLegion-Role: tester"'`
    );
    expect(unscoped).not.toContain("Dispatch");
    // The scope, written out once: the pair, then the issue line, for this repository alone.
    expect(scope).toBe(
      [
        "[[--scope]]",
        `--when.repositories = ["${ws}"]`,
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

  // Entry 20261125180539 (deployment-roles): a role of the deployment, `checker`, is a claim's role as Legion's six are, and
  // its commits carry it as theirs do.
  it("writes Legion-Role: checker for a role of the deployment", async () => {
    const dir = await stateDir();
    await exportJjSessionAttribution(ROOT_ID, dir, {
      ...paneAttributionOptions({ LEGION_ROLE: "checker" }),
      env: {},
      resolveBase: async () => "/u/base.toml",
    });
    const lines = (await overlayText(dir)).split("\n");
    expect(lines).toContain(`commit_trailers = '"Omp-Session: ${ROOT_ID}\\nLegion-Role: checker"'`);
    expect(lines).toContain(`  "Legion-Role: checker\\n",`);
  });

  // The role is read from LEGION_ROLE in one place, and only a role name passes it on: a role of
  // the step grammar (`isStepRole`) or the controller's.
  it.each([
    ["unset", undefined],
    ["empty", ""],
    ["outside the role grammar", "Bogus"],
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
    ["a value outside the role grammar", "Bogus"],
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

  it("scopes the Dispatch line to the pane's repository for a key of the accepted shape", async () => {
    const dir = await stateDir();
    const ws = await workspace();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      repository: ws,
      env,
      resolveBase: async () => "/u/base.toml",
    });
    const [unscoped, scope] = await overlayTables(dir);
    expect(unscoped).not.toContain("Dispatch");
    const scoped = scope?.split("\n");
    expect(scoped).toContain(`--when.repositories = ["${ws}"]`);
    expect(scoped).toContain(`commit_trailers = '"Omp-Session: ${ROOT_ID}\\nDispatch: ACME-7"'`);
  });

  // Without a repository there is nothing to scope the key to, and an unscoped key would stamp
  // this deployment's issue into every jj repository the pane's shell commits in.
  it("writes no scope for an accepted key with no repository", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      env,
      resolveBase: async () => "/u/base.toml",
    });
    await expectSessionLineAlone(dir);
  });

  // jj matches a scope against the real path of the repository it loaded, so a workspace reached
  // through a symlink, whose `.jj/repo` names the clone's by a relative path as `jj workspace add`
  // writes it, scopes to the clone's real store.
  it("scopes to the clone's real store when the workspace is reached through a symlink", async () => {
    const dir = await stateDir();
    const root = await workspace("layout");
    const store = path.join(root, "repos", "widgets", ".jj", "repo");
    await mkdir(store, { recursive: true });
    const ws = path.join(root, "workspaces", "acme-7");
    await mkdir(path.join(ws, ".jj"), { recursive: true });
    await Bun.write(path.join(ws, ".jj", "repo"), "../../../repos/widgets/.jj/repo");
    const link = path.join(root, "link-ws");
    await symlink(ws, link);
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      ...paneAttributionOptions({ LEGION_ISSUE_TRAILER: "ACME-7", LEGION_WORKSPACE: link }),
      env,
      resolveBase: async () => "/u/base.toml",
    });
    const [, scope] = await overlayTables(dir);
    expect(scope?.split("\n")).toContain(`--when.repositories = ["${await realpath(store)}"]`);
  });

  it("writes no scope for a workspace path that does not exist", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      repository: resolvedRepository(path.join(dir, "no-such-workspace")),
      env,
      resolveBase: async () => "/u/base.toml",
    });
    await expectSessionLineAlone(dir);
  });

  // The path goes into a TOML basic string inside an array; a raw quote would end that string
  // and a raw backslash would open an escape, leaving jj a file it refuses.
  it("escapes a repository path holding a double quote and a backslash", async () => {
    const dir = await stateDir();
    const ws = await workspace('ws "quoted\\odd');
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      repository: ws,
      env,
      resolveBase: async () => "/u/base.toml",
    });
    // The last name of the path, `ws "quoted\odd`, with its `"` and its `\` escaped for TOML.
    const [, scope] = await overlayTables(dir);
    expect(scope?.split("\n")).toContain(
      `--when.repositories = ["${path.dirname(ws)}/ws \\"quoted\\\\odd"]`
    );
  });

  // No escape of ours is worth guessing at here: a scope jj cannot parse breaks every `jj`
  // command in the pane, which is far worse than a missing trailer.
  it.each([
    ["a newline", "/ws/line\nbreak"],
    ["a tab", "/ws/tab\there"],
    ["a DEL", "/ws/del\u007fhere"],
  ])("writes no scope for a repository path holding %s", async (_name, ws) => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      issueKey: "ACME-7",
      repository: ws,
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
      repository: await workspace(),
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
      repository: ws,
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

  /** The jj commands the templates row below runs, counted: the row's bound is that count times
   *  each command's own hang guard, so a loaded machine running jj slowly fails nothing, and the
   *  row holds the count to the commands it ran. A fixed 60 s for the whole row once decided it at
   *  a load of about 1,400, where its jj work took 46 s and more. */
  const TEMPLATE_ROW_JJ_COMMANDS = 34;
  /** Each jj command's hang guard in that row: far above one command on a loaded machine, so a
   *  jj that never ends fails naming its command. */
  const JJ_COMMAND_MS = 60_000;

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
      let ran = 0;
      const jj = async (...args: string[]): Promise<string> => {
        ran += 1;
        if (ran > TEMPLATE_ROW_JJ_COMMANDS) {
          throw new Error(
            `jj ${args.join(" ")} is past the ${TEMPLATE_ROW_JJ_COMMANDS} commands this row counts`
          );
        }
        const child = Bun.spawn([JJ, ...args], {
          cwd: repo,
          env,
          stdin: "ignore",
          stdout: "pipe",
          stderr: "pipe",
          timeout: JJ_COMMAND_MS,
          killSignal: "SIGKILL",
        });
        const [code, stdout, stderr] = await Promise.all([
          child.exited,
          new Response(child.stdout).text(),
          new Response(child.stderr).text(),
        ]);
        if (child.signalCode === "SIGKILL") {
          throw new Error(`jj ${args.join(" ")} ran past its ${JJ_COMMAND_MS} ms guard`);
        }
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
      // The count the row's bound is made of is the commands it ran.
      expect(ran).toBe(TEMPLATE_ROW_JJ_COMMANDS);
    },
    // Each counted command's guard, and room for the row's own file work around them.
    TEMPLATE_ROW_JJ_COMMANDS * JJ_COMMAND_MS + 30_000
  );

  // Each task of an issue works in a jj workspace of its own beside the issue's, all of one clone,
  // and the pane's LEGION_WORKSPACE names the issue's alone. So the issue key belongs in a commit
  // of every working copy of that clone, and in no commit of another repository. Real jj, as the
  // case above runs it; the workspaces are made before the overlay, as provisioning makes them.
  it.skipIf(JJ === null && !ON_ACTIONS)(
    "the issue key is in a commit of each working copy of the issue's clone",
    async () => {
      if (JJ === null) throw new Error("no jj on PATH on GitHub Actions (install-jj installs one)");
      const root = await realpath(await stateDir("jj-attribution-copies-"));
      const base = path.join(root, "base.toml");
      await Bun.write(base, 'ui.color = "never"\n');
      const env: NodeJS.ProcessEnv = {
        PATH: process.env.PATH ?? "",
        HOME: root,
        XDG_CONFIG_HOME: path.join(root, "config"),
        JJ_CONFIG: base,
        JJ_USER: "Attribution Test",
        JJ_EMAIL: "attribution-test@example.invalid",
      };
      const jj = async (cwd: string, ...args: string[]): Promise<string> => {
        const child = Bun.spawn([JJ, ...args], {
          cwd,
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
        if (code !== 0) throw new Error(`jj ${args.join(" ")} in ${cwd} exited ${code}: ${stderr}`);
        return stdout;
      };
      const clone = path.join(root, "repos", "widgets");
      const own = path.join(root, "workspaces", "acme-7");
      const task = path.join(root, "workspaces", "acme-7.b");
      const unrelated = path.join(root, "other");
      await mkdir(clone, { recursive: true });
      await mkdir(unrelated);
      await mkdir(path.dirname(own));
      await jj(clone, "git", "init");
      await Bun.write(path.join(clone, "a"), "a\n");
      await jj(clone, "commit", "-m", "base");
      await jj(clone, "workspace", "add", own, "--name", "acme-7");
      await jj(clone, "workspace", "add", task, "--name", "acme-7.b");
      await jj(unrelated, "git", "init");

      const chain = await exportJjSessionAttribution(ROOT_ID, path.join(root, "state"), {
        ...paneAttributionOptions({ LEGION_ISSUE_TRAILER: "ACME-7", LEGION_WORKSPACE: own }),
        env: { ...env, JJ_CONFIG: undefined },
        resolveBase: async () => base,
      });
      if (chain === undefined) throw new Error("the writer exported no chain");
      env.JJ_CONFIG = chain;
      const pair = `Omp-Session: ${ROOT_ID}\n`;
      for (const [name, dir, trailers] of [
        ["a task's own working copy", task, `${pair}Dispatch: ACME-7\n`],
        ["the issue's working copy", own, `${pair}Dispatch: ACME-7\n`],
        ["an unrelated repository", unrelated, pair],
      ] as const) {
        await Bun.write(path.join(dir, "work"), `${name}\n`);
        await jj(dir, "commit", "-m", "work");
        expect([name, await jj(dir, "log", "--no-graph", "-r", "@-", "-T", "description")]).toEqual(
          [name, `work\n\n${trailers}`]
        );
      }
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

describe("resolvedRepository", () => {
  const dirs: string[] = [];
  afterEach(async () => {
    await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
  });
  /** A clone at `repos/widgets` whose own `.jj/repo` is a directory, under a scratch directory. */
  async function clone(): Promise<{ root: string; store: string }> {
    const root = await realpath(await mkdtemp(path.join(tmpdir(), "jj-attribution-repo-")));
    dirs.push(root);
    const store = path.join(root, "repos", "widgets", ".jj", "repo");
    await mkdir(store, { recursive: true });
    return { root, store };
  }
  /** A workspace at `workspaces/<name>` whose `.jj/repo` file holds `named`. */
  async function workspaceNaming(root: string, name: string, named: string): Promise<string> {
    const ws = path.join(root, "workspaces", name);
    await mkdir(path.join(ws, ".jj"), { recursive: true });
    await Bun.write(path.join(ws, ".jj", "repo"), named);
    return ws;
  }

  it("is the clone's own store for the clone's workspace", async () => {
    const { root, store } = await clone();
    expect(resolvedRepository(path.join(root, "repos", "widgets"))).toBe(store);
  });

  // `jj workspace add` writes the store's path relative to the workspace's `.jj`.
  it("is the clone's store for a workspace whose .jj/repo names it by a relative path", async () => {
    const { root, store } = await clone();
    expect(
      resolvedRepository(
        await workspaceNaming(root, "acme-7.b", "../../../repos/widgets/.jj/repo\n")
      )
    ).toBe(store);
  });

  it("is the clone's store for a workspace whose .jj/repo names it by an absolute path", async () => {
    const { root, store } = await clone();
    expect(resolvedRepository(await workspaceNaming(root, "acme-7", store))).toBe(store);
  });

  it.each([
    ["an empty file", ""],
    ["a store that does not exist", "../../../repos/gone/.jj/repo"],
  ])("is undefined for a workspace whose .jj/repo names %s", async (_name, named) => {
    const { root } = await clone();
    expect(resolvedRepository(await workspaceNaming(root, "acme-7", named))).toBeUndefined();
  });

  it("is undefined for a directory that is no jj workspace, and for none", async () => {
    const { root } = await clone();
    expect(resolvedRepository(root)).toBeUndefined();
    expect(resolvedRepository(path.join(root, "no-such-workspace"))).toBeUndefined();
    expect(resolvedRepository(undefined)).toBeUndefined();
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
  /** How long each wait below gives its fact before it fails naming it: a hang guard far above
   *  what a loaded machine takes to write a pid or reap a child, inside each row's own timeout. */
  const WAIT_MS = 30_000;
  /** How long a wrapper's child sleeps: far past every wait and row timeout here, so a child the
   *  group kill missed is still running when `reaped` or `afterEach` looks. A `sleep 30` ended by
   *  itself inside the 30 s `reaped` gives it, which read as a kill. */
  const CHILD_SLEEP_S = 3_600;
  /** Every child pid a row read. Bun runs `afterEach` after a row's timeout, where it skips the
   *  row's own `finally`, so the end of a child that outlived its row is here. */
  const children: number[] = [];
  afterEach(async () => {
    const survivors: number[] = [];
    for (const pid of children.splice(0)) {
      if (!(await running(pid))) continue;
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
  // are external OS state, which Bun's fake timers cannot advance.
  /** The wrapper's child pid, once it has written it, kept for `afterEach`. */
  async function pidFrom(pidFile: string): Promise<number> {
    const deadline = performance.now() + WAIT_MS;
    while (performance.now() < deadline) {
      const pid = await readFile(pidFile, "utf8")
        .then((value) => Number(value.trim()))
        .catch(() => 0);
      if (Number.isInteger(pid) && pid > 0) {
        children.push(pid);
        return pid;
      }
      await Bun.sleep(10);
    }
    throw new Error(`the jj wrapper did not write ${pidFile} within ${WAIT_MS} ms`);
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

  /** Resolves once the child is gone, and forgets it then, so `afterEach` never signals a pid the
   *  kernel has since given another process; fails after `WAIT_MS` naming it, so a child that
   *  lives on reads as that, not as the row's timeout. */
  async function reaped(pid: number): Promise<void> {
    const deadline = performance.now() + WAIT_MS;
    while (await running(pid)) {
      if (performance.now() > deadline) {
        throw new Error(`the wrapper's child ${pid} still ran ${WAIT_MS} ms after the group kill`);
      }
      await Bun.sleep(10);
    }
    const at = children.indexOf(pid);
    if (at >= 0) children.splice(at, 1);
  }

  it("forgets a child once it is seen gone, so afterEach signals no pid reaped already", async () => {
    const child = Bun.spawn(["true"]);
    await child.exited;
    children.push(child.pid);
    await reaped(child.pid);
    expect(children).not.toContain(child.pid);
  });

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

  // The give-up comes once the wrapper has forked its child and written the child's pid: a bound
  // of the row's own raced the wrapper, and a 200 ms one killed the group before the pid existed
  // on a loaded machine. `giveUp` takes the same path the bound does.
  it(
    "kills a wrapper's forked child when jj times out",
    async () => {
      const pidFile = path.join(await mkdtemp(path.join(tmpdir(), "jj-attribution-child-")), "pid");
      dirs.push(path.dirname(pidFile));
      const env = await withJj(
        `sleep ${CHILD_SLEEP_S} & echo $! > ${JSON.stringify(pidFile)}; wait`
      );
      const giveUp = new AbortController();
      const chain = userConfigChain(JJ_CONFIG_PATH_TIMEOUT_MS, env, giveUp.signal);
      const pid = await pidFrom(pidFile);
      giveUp.abort();
      expect(await chain).toBeUndefined();
      await reaped(pid);
    },
    2 * WAIT_MS + 5_000
  );

  // What the row reads is two facts: the wrapper's answer and its detached child's death. The
  // wrapper is given the production bound (JJ_CONFIG_PATH_TIMEOUT_MS, 10 s), so a loaded machine
  // running it slowly fails nothing; a 200 ms bound here once decided the row, answering undefined
  // for a wrapper that had not yet printed its path. Each wait fails naming its fact, inside the
  // row's timeout.
  it(
    "kills a wrapper's detached child after jj returns a config path",
    async () => {
      const pidFile = path.join(await mkdtemp(path.join(tmpdir(), "jj-attribution-child-")), "pid");
      dirs.push(path.dirname(pidFile));
      const env = await withJj(
        `sleep ${CHILD_SLEEP_S} </dev/null >/dev/null 2>&1 & echo $! > ${JSON.stringify(pidFile)}; printf '/u/a.toml\\n'`
      );
      const paths = await userConfigChain(JJ_CONFIG_PATH_TIMEOUT_MS, env);
      const pid = await pidFrom(pidFile);
      if (paths === undefined) {
        throw new Error(`the wrapper gave no config path within ${JJ_CONFIG_PATH_TIMEOUT_MS} ms`);
      }
      expect(paths).toBe("/u/a.toml");
      await reaped(pid);
    },
    JJ_CONFIG_PATH_TIMEOUT_MS + 2 * WAIT_MS + 5_000
  );

  it("bounds jj at 10 s, inside the 30 s Oh My Pi gives the session_start handler", () => {
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBe(10_000);
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBeLessThan(30_000);
  });
});
