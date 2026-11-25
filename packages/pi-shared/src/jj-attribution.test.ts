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

// The hang guard of every row of this file that awaits a file, a temporary directory or a fake jj
// and names no other limit below: what can hang is that wait, an event with no deadline of its own.
// This package's `bun test` sets no `--timeout`, so these rows ran at bun's 5 s default. Worst
// measured cost among them: 3 389 ms over 20 runs of this file whole at the 240 rung in the Job
// infra-1580-r27jw-c468r of the cluster's `builds` namespace, two CPUs and 8 GiB (483 busy loops
// for the 240 rung, row A 32.0 s), at 82306880, where bun's 5 s was 1.5 times it. 30_000 is 8.9
// times that worst.
const JJ_ROW_LIMIT_MS = 30_000;

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
  // A bound on work, not a hang guard: nothing here waits on another process. The body removes the
  // rows' directories, a part of the work of the `userConfigChain` afterEach below, and it carries
  // that hook's 60_000 for the reading recorded there.
  afterEach(async () => {
    await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
  }, 60_000);
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

  it(
    "writes the trailer overlay and chains it after the resolved user config",
    async () => {
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
    },
    JJ_ROW_LIMIT_MS
  );

  // The whole text, written out once: the session line, then the role line, in each template.
  it(
    "writes the pair in order into the three templates of the unscoped table",
    async () => {
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
    },
    JJ_ROW_LIMIT_MS
  );

  it(
    "scopes the pair, then the Dispatch line, to the pane's workspace",
    async () => {
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
    },
    JJ_ROW_LIMIT_MS
  );

  // `legion controller start` sets LEGION_ROLE=controller, which is no LegionRole.
  it(
    "writes Legion-Role: controller for the controller",
    async () => {
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
    },
    JJ_ROW_LIMIT_MS
  );

  // The role is read from LEGION_ROLE in one place, and only a role name passes it on.
  it.each([
    ["unset", undefined],
    ["empty", ""],
    ["bogus", "bogus"],
    ['x"y', 'x"y'],
  ])(
    "writes the session line alone for LEGION_ROLE %s",
    async (_name, role) => {
      const dir = await stateDir();
      const options = paneAttributionOptions(role === undefined ? {} : { LEGION_ROLE: role });
      expect(options).not.toHaveProperty("role");
      await exportJjSessionAttribution(ROOT_ID, dir, {
        ...options,
        env: {},
        resolveBase: async () => "/u/base.toml",
      });
      await expectSessionLineAlone(dir);
    },
    JJ_ROW_LIMIT_MS
  );

  // Containment, as for the issue key: the role is interpolated into a jj template inside a TOML
  // literal, so the writer checks a caller's value again.
  it.each([
    ["a value that is no role", "bogus"],
    ["a value holding a double quote", 'x"y'],
    ["a role with a trailing newline", "tester\n"],
  ])(
    "writes the session line alone for %s passed as the role",
    async (_name, role) => {
      const dir = await stateDir();
      await exportJjSessionAttribution(ROOT_ID, dir, {
        role,
        env: {},
        resolveBase: async () => "/u/base.toml",
      });
      await expectSessionLineAlone(dir);
    },
    JJ_ROW_LIMIT_MS
  );

  it(
    "scopes the Dispatch line to the pane's workspace for a key of the accepted shape",
    async () => {
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
    },
    JJ_ROW_LIMIT_MS
  );

  // Without a workspace there is nothing to scope the key to, and an unscoped key would stamp
  // this deployment's issue into every jj repository the pane's shell commits in.
  it(
    "writes no scope for an accepted key with no workspace",
    async () => {
      const dir = await stateDir();
      const env: NodeJS.ProcessEnv = {};
      await exportJjSessionAttribution(ROOT_ID, dir, {
        issueKey: "ACME-7",
        env,
        resolveBase: async () => "/u/base.toml",
      });
      await expectSessionLineAlone(dir);
    },
    JJ_ROW_LIMIT_MS
  );

  // jj resolves a workspace root before it matches a scope, so the link's own path would match
  // nothing — and say nothing about it.
  it(
    "scopes to the real path when the workspace is reached through a symlink",
    async () => {
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
    },
    JJ_ROW_LIMIT_MS
  );

  it(
    "writes no scope for a workspace path that does not exist",
    async () => {
      const dir = await stateDir();
      const env: NodeJS.ProcessEnv = {};
      await exportJjSessionAttribution(ROOT_ID, dir, {
        issueKey: "ACME-7",
        workspace: resolvedWorkspace(path.join(dir, "no-such-workspace")),
        env,
        resolveBase: async () => "/u/base.toml",
      });
      await expectSessionLineAlone(dir);
    },
    JJ_ROW_LIMIT_MS
  );

  // The path goes into a TOML basic string inside an array; a raw quote would end that string
  // and a raw backslash would open an escape, leaving jj a file it refuses.
  it(
    "escapes a workspace path holding a double quote and a backslash",
    async () => {
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
    },
    JJ_ROW_LIMIT_MS
  );

  // No escape of ours is worth guessing at here: a scope jj cannot parse breaks every `jj`
  // command in the pane, which is far worse than a missing trailer.
  it.each([
    ["a newline", "/ws/line\nbreak"],
    ["a tab", "/ws/tab\there"],
    ["a DEL", "/ws/del\u007fhere"],
  ])(
    "writes no scope for a workspace path holding %s",
    async (_name, ws) => {
      const dir = await stateDir();
      const env: NodeJS.ProcessEnv = {};
      await exportJjSessionAttribution(ROOT_ID, dir, {
        issueKey: "ACME-7",
        workspace: ws,
        env,
        resolveBase: async () => "/u/base.toml",
      });
      await expectSessionLineAlone(dir);
    },
    JJ_ROW_LIMIT_MS
  );

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
  ])(
    "writes the session line alone for %s",
    async (_name, issueKey) => {
      const dir = await stateDir();
      const env: NodeJS.ProcessEnv = {};
      await exportJjSessionAttribution(ROOT_ID, dir, {
        issueKey,
        workspace: await workspace(),
        env,
        resolveBase: async () => "/u/base.toml",
      });
      await expectSessionLineAlone(dir);
    },
    JJ_ROW_LIMIT_MS
  );

  it(
    "carries a state directory holding a space and a single quote whole",
    async () => {
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
    },
    JJ_ROW_LIMIT_MS
  );

  it(
    "strips stale attribution overlays from an inherited JJ_CONFIG, keeping user layers",
    async () => {
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
      expect(env.JJ_CONFIG).toBe(
        `/u/base.toml:${path.join(dir, `omp-attribution-${ROOT_ID}.toml`)}`
      );
    },
    JJ_ROW_LIMIT_MS
  );

  it(
    "re-resolves the base when the inherited chain was only stale overlays",
    async () => {
      const dir = await stateDir();
      const env: NodeJS.ProcessEnv = { JJ_CONFIG: "/tmp/omp-attribution-old.toml" };
      await exportJjSessionAttribution(ROOT_ID, dir, {
        env,
        resolveBase: async () => "/u/base.toml",
      });
      expect(env.JJ_CONFIG).toBe(
        `/u/base.toml:${path.join(dir, `omp-attribution-${ROOT_ID}.toml`)}`
      );
    },
    JJ_ROW_LIMIT_MS
  );

  it(
    "returns the live chain and leaves the file alone when the overlay is already chained",
    async () => {
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
    },
    JJ_ROW_LIMIT_MS
  );

  it(
    "publishes the clean base and exports nothing when the overlay cannot be written",
    async () => {
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
    },
    JJ_ROW_LIMIT_MS
  );

  it(
    "leaves the environment untouched when jj is unavailable",
    async () => {
      const dir = await stateDir();
      const env: NodeJS.ProcessEnv = {};
      expect(
        await exportJjSessionAttribution(ROOT_ID, dir, { env, resolveBase: async () => undefined })
      ).toBeUndefined();
      expect(env.JJ_CONFIG).toBeUndefined();
    },
    JJ_ROW_LIMIT_MS
  );

  // The id reaches both the TOML literal and the overlay's file name, so an id that is not one
  // (`SESSION_ID_PATH_SEGMENT`) passes nothing on: a single quote would end the literal and break
  // every `jj` command in the pane, and a `/` would move the overlay out of the state directory.
  it.each([
    ["an empty session id", ""],
    ["a session id holding a single quote", "s'1"],
    ["a session id holding a slash", "../s-1"],
    ["a session id holding a backslash", "s\\1"],
    ["a session id over 128 characters", "s".repeat(129)],
  ])(
    "leaves the environment untouched and writes nothing for %s",
    async (_name, sessionId) => {
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
    },
    JJ_ROW_LIMIT_MS
  );

  /** The jj commands the templates row below runs. The row checks the commands it ran (`ran`)
   *  against this count. A command added past the count fails the row naming that command. A
   *  command lost fails the count check at the row's end. */
  const TEMPLATE_ROW_JJ_COMMANDS = 34;
  /** The templates row's own limit. A hang guard: what can hang is a real jj command, which runs
   *  with no deadline of its own. This limit alone ends a jj that never ends, and it names the
   *  row. The afterEach below names the command and kills it. Worst measured cost: over 20 runs
   *  of this file at the 240 rung, a run charged up to 442 299 ms, all of it this row's but the
   *  22.9 s its other rows took together, in the Job infra-1580-t27-j3-99kcl of the cluster's
   *  `builds` namespace, two CPUs and 8 GiB (417 busy loops for the 240 rung, row A 35.3 s), at
   *  06d187e7. The row took 300 338 ms in the Job infra-1580-t27-j15-tx7cm at that commit. The
   *  pi-shared job's room sizes the limit: its `timeout-minutes: 10` less its 45 s at 06d187e7
   *  (CI run 45437325102) leaves 555 s. Six times either reading is past that room. So the room
   *  decides 540_000, 1.2 times 442 299 ms. */
  const TEMPLATE_ROW_LIMIT_MS = 540_000;
  /** The real jj command the templates row below is running, if any: its words and its process.
   *  A row its limit ends inside a command never clears this, so the afterEach below finds the
   *  command here. */
  let jjRunning: { command: string; child: Bun.Subprocess<"ignore", "pipe", "pipe"> } | undefined;
  // Nothing here waits: the kill is a signal, and the error names the command the row was in.
  afterEach(() => {
    const running = jjRunning;
    jjRunning = undefined;
    if (running === undefined) return;
    running.child.kill("SIGKILL");
    throw new Error(`${running.command} still ran when its row ended, killed here`);
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
    },
    TEMPLATE_ROW_LIMIT_MS
  );
});

describe("paneAttributionOptions", () => {
  it.each([...LEGION_ROLES, "controller"])("passes LEGION_ROLE=%s on as the role", (role) => {
    expect(paneAttributionOptions({ LEGION_ROLE: role })).toEqual({ role });
  });
});

describe("resolvedWorkspace", () => {
  const dirs: string[] = [];
  // A bound on work, not a hang guard: nothing here waits on another process. The body removes the
  // rows' directories, a part of the work of the `userConfigChain` afterEach below, and it carries
  // that hook's 60_000 for the reading recorded there.
  afterEach(async () => {
    await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
  }, 60_000);
  async function scratch(): Promise<string> {
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-resolve-"));
    dirs.push(dir);
    return await realpath(dir);
  }

  it(
    "answers the real path of an existing directory",
    async () => {
      const dir = await scratch();
      expect(resolvedWorkspace(dir)).toBe(dir);
    },
    JJ_ROW_LIMIT_MS
  );

  it(
    "follows a symlink to the path jj would resolve the workspace to",
    async () => {
      const dir = await scratch();
      const real = path.join(dir, "real");
      await mkdir(real);
      const link = path.join(dir, "link");
      await symlink(real, link);
      expect(resolvedWorkspace(link)).toBe(real);
    },
    JJ_ROW_LIMIT_MS
  );

  // A relative path matches no scope and says nothing about it, so the absolute one is written.
  it(
    "answers an absolute path for a relative one",
    async () => {
      const dir = await scratch();
      const relative = path.relative(process.cwd(), dir);
      expect(path.isAbsolute(relative)).toBe(false);
      expect(resolvedWorkspace(relative)).toBe(dir);
    },
    JJ_ROW_LIMIT_MS
  );

  it.each([
    ["an unset workspace", undefined],
    ["an empty workspace", ""],
  ])("answers undefined for %s", (_name, workspace) => {
    expect(resolvedWorkspace(workspace)).toBeUndefined();
  });

  it(
    "answers undefined for a path that does not exist",
    async () => {
      expect(resolvedWorkspace(path.join(await scratch(), "gone"))).toBeUndefined();
    },
    JJ_ROW_LIMIT_MS
  );
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
   *  abandons it at its limit — a `userConfigChain` that never returns included. A pidfile leaves
   *  this list once `reaped` has seen its pid gone, so a green row leaves the afterEach no pid to
   *  kill. */
  const pidFiles: string[] = [];
  /** A pidfile for one fake jj, in a directory of its own, recorded for the afterEach. */
  async function childPidFile(): Promise<string> {
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-child-"));
    dirs.push(dir);
    const pidFile = path.join(dir, "pid");
    pidFiles.push(pidFile);
    return pidFile;
  }
  /** Advanced by the afterEach below. Each row that polls takes it at its first line, before any
   *  await, and hands it to `polled`, so a poll stops once that row has ended: a poll that a body
   *  its limit abandoned starts only after that, inside the next row, stops there at once. */
  let rowGeneration = 0;
  /** The pid in `pidFile`, or undefined while the wrapper has not written one. */
  async function pidIn(pidFile: string): Promise<number | undefined> {
    const pid = await readFile(pidFile, "utf8")
      .then((value) => Number(value.trim()))
      .catch(() => 0);
    return Number.isInteger(pid) && pid > 0 ? pid : undefined;
  }
  // A rung is a load: that many busy loops on the pods where dispatch://INFRA-1580 first measured
  // its loads, and where `packages/pi-envoy/extensions/envoy.test.ts`'s row "session_shutdown
  // resolves within its budget when drain hangs on a dead connection" ran 20 times in about
  // 10.4 min at 96. A slower pod runs the same load as fewer loops, matched by that row's run time
  // there, so a figure written "N busy loops (the R rung)" is one pod's count at the time of its
  // reading.
  //
  // A bound on work, not a hang guard: nothing here waits on another process. The body reads the
  // rows' pidfiles, kills what they name and removes the rows' directories, which a loaded host
  // stretches. 60_000. Worst measured cost: 5 104.35 ms over 15 invocations in 3 runs of this
  // describe at 75 busy loops (the 240 rung) on a two-CPU, 8 GiB pod. The reading is of the body
  // at 0ad78f05, which killed without asking `running` first, in a describe of five rows.
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
  }, 60_000);
  // Real-time polling is required here: the wrapper's pidfile and the kernel reaping its child
  // are external OS state, which Bun's fake timers cannot advance. The poll carries no deadline of
  // its own, so what decides a row that waits here is that row's own per-test limit, which names
  // it: a pidfile that is never written, or a child the kill misses, ends the row there, and the
  // afterEach above kills the fake jj's `sleep 300` either way instead of leaving it for five
  // minutes. Once the row has ended, the afterEach advances `rowGeneration` and the poll stops,
  // since the generation it checks is the one its row took at its first line. At the base,
  // 67dcae63, these polls gave up at `WAIT_MS`, 30 s, inside row limits of 65 s and 75 s, and two
  // of them ran in series in one row. A give-up on real time inside the row's own limit is a bound
  // on real time deciding the row. A wrapper's fork costing 33478 ms at 240 busy loops, past that
  // 30 s, shows it.
  /** Polls `read` until it answers, warning once at 2 s, which names what it waits for and decides
   *  nothing. */
  async function polled<T>(
    row: number,
    what: string,
    read: () => Promise<T | undefined>
  ): Promise<T> {
    const start = performance.now();
    let warned = false;
    for (;;) {
      const value = await read();
      if (value !== undefined) return value;
      if (row !== rowGeneration) {
        throw new Error(`stopped waiting for ${what}: the row ended`);
      }
      if (!warned && performance.now() - start > 2_000) {
        warned = true;
        console.warn(`still waiting for ${what} after 2 s`);
      }
      await Bun.sleep(10);
    }
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
    await polled(row, `pid ${pid} to end`, async () => ((await running(pid)) ? undefined : true));
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
    // A hang guard: what can hang is the spawn of `true`, its exit, or the reaping `reaped` waits
    // for, each an event the row waits on with no deadline of its own (`polled` stops only once
    // the row has ended). It ran at bun's 5 s default, which this package's `bun test` sets no flag
    // over, and that default ended a correct run at the 240 rung, charged 6 697.61 ms, in the
    // tester's Job infra-1580-t26-k4-p99sv of the cluster's `builds` namespace, two CPUs and 8 GiB
    // (350 busy loops for the 240 rung, row A 36.6 s), at fe0e876b. With this limit it is 20 of 20
    // at that rung, charged 7 / 1 381 / 5 182 ms (min / median / max), in the Job
    // infra-1580-r27jf-lswkw (600 busy loops for the 240 rung, row A 38.0 s), at 82306880.
    // 100_000 is 14.9 times the worst of them.
  }, 100_000);

  it("chains the user config files jj prints, in order", async () => {
    const env = await withJj("printf '/u/a.toml\\n/u/b.toml\\n'");
    // The bound sits above this row's own limit, as the detached-child row's does, so it can
    // never fire inside a run the runner allows: the fake jj answers at once, and how long a
    // loaded host takes to start it is no part of this row's subject.
    expect(await userConfigChain(120_000, env)).toBe("/u/a.toml:/u/b.toml");
    // A hang guard: what can hang is a `jj` that never prints. 60_000. Worst measured cost:
    // 3 494.54 ms at 75 busy loops (the 240 rung) on a two-CPU, 8 GiB pod.
  }, 60_000);

  // Each of the three rows below runs a fake jj that sleeps 300 s, far above every per-test limit
  // in this file. So a `userConfigChain` that stopped bounding its wait, or a kill that leaves the
  // forked child behind, cannot return inside the row's limit, and that limit — not a reading of
  // the clock — is what tells a correct run from the defect. The bound each row passes
  // `userConfigChain` is named at its own call below.
  it("gives up on a jj that never answers, so attribution never holds bootstrap", async () => {
    const row = rowGeneration;
    const pidFile = await childPidFile();
    const env = await withJj(`echo $$ > ${JSON.stringify(pidFile)} && exec sleep 300`);
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-"));
    dirs.push(dir);
    const exported: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_ID, dir, {
      env: exported,
      resolveBase: () => userConfigChain(200, env),
    });
    expect(exported.JJ_CONFIG).toBeUndefined();
    // The resolve gave up, so its bound killed the fake jj's group. A pid the jj wrote before that
    // is waited out here, which drops its pidfile; a jj killed before it wrote one is gone and left
    // nothing to collect, so this row never waits on a pidfile that may not come.
    const pid = await pidIn(pidFile);
    if (pid !== undefined) await reaped(row, pidFile, pid);
    // A hang guard: what can hang is a resolve that waits the whole 300 s sleep out. Worst measured
    // cost: 16 109 ms at the 240 rung in the Job infra-1580-t25-j1-gtjcb of the cluster's `builds`
    // namespace, two CPUs and 8 GiB (555 busy loops for the 240 rung, row A 39.5 s), at b5a8b94f.
    // 100_000 is 6.2 times that.
  }, 100_000);

  it("kills a wrapper's forked child when jj times out", async () => {
    const row = rowGeneration;
    const pidFile = await childPidFile();
    // The child records itself: `sh -c` writes its own pid and only then becomes the `sleep`, so a
    // kill that reaches the wrapper at any moment leaves no `sleep` unrecorded. A wrapper that
    // wrote `$!` after forking left a window in which a kill narrowed to the wrapper missed the
    // child and no pid was written.
    const env = await withJj(
      `sh -c 'echo $$ > "$1" && exec sleep 300' sh ${JSON.stringify(pidFile)} & wait`
    );
    // The give-up comes once the wrapper has forked its child and the child has written its pid
    // (`giveUp`, which takes the same path the bound does), so how long the host takes to fork
    // decides nothing: a bound raced the fork, and a fork took up to 33478 ms at 240 busy loops on
    // one pod, past the production 10 s. The bound passed sits above the row's own limit, so it
    // can never fire inside a run the runner allows. Undefined is the give-up, which is this row's
    // premise.
    const giveUp = new AbortController();
    const chain = userConfigChain(120_000, env, giveUp.signal);
    const pid = await polled(row, `the jj wrapper to write ${pidFile}`, () => pidIn(pidFile));
    giveUp.abort();
    expect(await chain).toBeUndefined();
    // The check: this returns only once the child is gone, and the child's own sleep outlives the
    // row, so a kill that never reached it ends the row at the limit below, and the afterEach above
    // collects the child.
    await reaped(row, pidFile, pid);
    // A hang guard: what can hang is an unbounded wait on the wrapper, a pidfile that is never
    // written, or a forked child the kill misses. Worst measured cost of the row's form before its
    // give-up: 12515.71 ms at 96 busy loops (48 per CPU), this row's acceptance rung, on a two-CPU
    // pod, with a script in which the wrapper wrote `$!`; 15 692 ms at the 240 rung in the Job
    // infra-1580-t25-j1-gtjcb of the cluster's `builds` namespace, two CPUs and 8 GiB (555 busy
    // loops for the 240 rung, row A 39.5 s), at b5a8b94f. 100_000 is 6.4 times that.
  }, 100_000);

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
        `sh -c 'echo $$ > "$1" && exec sleep 300' sh ${quotedFile} </dev/null >/dev/null 2>&1 &`,
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
    // answers one second late. So the budget below sits above this row's own 100 s limit, where
    // it can never fire inside a run the runner allows, and the runner's limit is what ends a
    // `jj` that never prints. The row above passes such a bound too, and gives up through its
    // signal once the wrapper's child has written its pid.
    const paths = await userConfigChain(120_000, env);
    expect(paths).toBe("/u/a.toml");
    // The wrapper answered, so the pidfile holds its child's pid: one read finds it.
    const pid = await pidIn(pidFile);
    if (pid === undefined) throw new Error(`the jj wrapper answered with no pid in ${pidFile}`);
    // The check, as above: the detached child's 300 s sleep outlives the row unless it is killed.
    await reaped(row, pidFile, pid);
    // A hang guard: what can hang is a pidfile that is never written, which the wrapper waits on,
    // or a detached child the kill misses. Worst measured cost of the row's form before its child
    // recorded itself: 14 098 ms over 20 runs at the 240 rung in the Job infra-1580-jj-vkdjb of the
    // cluster's `builds` namespace, two CPUs and 8 GiB (350 busy loops for the 240 rung, row A
    // 33.7 s), at 7025b6c8, with a script in which the wrapper wrote `$!`. 100_000 is 7.1 times
    // that.
  }, 100_000);

  it("bounds jj at 10 s, inside the 30 s Oh My Pi gives the session_start handler", () => {
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBe(10_000);
    expect(JJ_CONFIG_PATH_TIMEOUT_MS).toBeLessThan(30_000);
  });
});
