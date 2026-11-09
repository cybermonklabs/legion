import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { createHash, randomBytes } from "node:crypto";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  utimesSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, resolve } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { keepStore, MARKER_MAX_AGE_MS, mintMarker, setRecordMode } from "../askgate-marker";
import { cliRefusals, runDispatchCli } from "../dispatch-cli";
import { resetAdviceMemory } from "../dispatch-execute";
import { pruneSessions } from "../dispatch-session-state";

// AskGate's enforcement at the posting CLI. A live Oh My Pi process whose gate is bound leaves an
// owner file in the session's state directory (`askgate/<pid>-<start>.json`) that names it and
// its marker store; the gate mints one single-use marker for each literal call it lets run. The
// CLI consumes a marker before it posts a gated write, and refuses one with no marker, whatever
// form ran it. These tests write the record and the markers by hand, in the format the gate
// writes, so they hold the CLI to that format and to nothing of the gate's.

interface Recorded {
  readonly method: string;
  readonly path: string;
  /** The session's ledger as it stood when the request arrived. */
  readonly ledger: string;
  /** What a JSON request posted, or an upload's file as text. */
  readonly sent?: unknown;
}

const posts: Recorded[] = [];
const server = Bun.serve({
  port: 0,
  async fetch(request) {
    const { pathname } = new URL(request.url);
    const ledgerFile = join(stateDir, "sessions", "ses-1", "results.jsonl");
    // One row puts a directory where the ledger file goes, so that the CLI cannot write its
    // line: reading it here must not fail the request the CLI is making.
    let ledgerText = "";
    try {
      ledgerText = readFileSync(ledgerFile, "utf-8");
    } catch {
      ledgerText = "";
    }
    const record = (sent?: unknown) =>
      posts.push({ method: request.method, path: pathname, ledger: ledgerText, sent });
    if (request.method === "POST" && pathname === ARTIFACTS) {
      // An upload sends its file as form data; one with inline `--content` sends JSON.
      if (request.headers.get("content-type")?.includes("multipart/form-data") === true) {
        const file = (await request.formData()).get("file");
        record(file instanceof Blob ? await file.text() : undefined);
      } else {
        record(await request.json());
      }
      return Response.json({
        artifact: { id: "artifact-1", issue_key: "DSP-1", name: "brief.md", slug: "brief-md" },
        version: { number: 1 },
      });
    }
    if (request.method === "POST" && (pathname === MESSAGES || pathname === COMMENTS)) {
      record(await request.json());
      return Response.json({
        id: "message-1",
        issue_key: "DSP-1",
        advice: { issue_status: "in_progress", session_writes_since_human: 0, your_open_asks: [] },
      });
    }
    record();
    return Response.json({ error: `unexpected ${request.method} ${pathname}` }, { status: 500 });
  },
});
afterAll(() => server.stop(true));

const MESSAGES = "/api/v1/issues/DSP-1/messages";
const COMMENTS = "/api/v1/issues/DSP-1/comments";
const ARTIFACTS = "/api/v1/issues/DSP-1/artifacts";
const postsTo = (route: string) => posts.filter((p) => p.method === "POST" && p.path === route);
const messagePosts = () => postsTo(MESSAGES);

const makeDirectory = temporaryDirectories("dispatch-askgate");
let stateDir = "";
beforeEach(() => {
  posts.length = 0;
  stateDir = makeDirectory();
  resetAdviceMemory();
});

const sha = (value: unknown) => createHash("sha256").update(JSON.stringify(value)).digest("hex");

/** Field 22 of /proc/<pid>/stat: the process's start time, in clock ticks since boot. */
function startOf(pid: number): string {
  const stat = readFileSync(`/proc/${pid}/stat`, "utf-8");
  return stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19] ?? "";
}

const store = () => join(stateDir, "askgate", String(process.pid));
const markers = () => join(store(), "markers");

/** The owner file of `owner` in session `id`'s record, as the gate names it. */
function ownerFile(id: string, owner: { pid: number; start: string | null }): string {
  const name = owner.start === null ? String(owner.pid) : `${owner.pid}-${owner.start}`;
  return join(stateDir, "sessions", id, "askgate", `${name}.json`);
}

/**
 * The record a live gate writes for session `id`: this test process stands in for Oh My Pi. `mode`
 * is the check mode the owner file carries for the CLI's marker checks: `block`, as a session that
 * chose block does (most tests here), `warn`, or null for a record whose owner file carries no
 * mode, which the CLI reads as warn. An owner with no `ns` is one written before the pid
 * namespace was kept.
 */
function writeRecord(
  id = "ses-1",
  owner: { pid: number; start: string | null; ns?: string | null; store: string } = {
    pid: process.pid,
    start: startOf(process.pid),
    store: store(),
  },
  mode: "warn" | "block" | null = "block"
): void {
  const file = ownerFile(id, owner);
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, JSON.stringify({ v: 2, ...owner, ...(mode === null ? {} : { mode }) }));
  mkdirSync(join(owner.store, "markers"), { recursive: true });
}

/** The sha256 of a file's bytes, as a marker binds each file it names. */
const fileSha = (file: string) => createHash("sha256").update(readFileSync(file)).digest("hex");

/**
 * One marker as the gate mints it: keyed by the argv's digest, bound to the arguments' digest and
 * to each file the argv names, with the digest of its bytes as the gate read them. `into` is the
 * minting process's store, this test process's by default.
 */
function mint(
  argv: readonly string[],
  args: Record<string, unknown> | null,
  createdAt = Date.now(),
  files: ReadonlyArray<{ flag: string; operand: string; sha256: string }> = [],
  into = store()
): string {
  const file = join(into, "markers", `${sha(argv)}.${randomBytes(16).toString("hex")}`);
  writeFileSync(
    file,
    JSON.stringify({
      v: 2,
      argv: sha(argv),
      args: args === null ? null : sha(args),
      files,
      toolCallId: "call-1",
      outcome: "verdict",
      createdAt,
    }),
    { mode: 0o600 }
  );
  return file;
}

function environment(
  extra: Record<string, string | undefined>
): Record<string, string | undefined> {
  return {
    HOME: stateDir,
    DISPATCH_URL: `http://127.0.0.1:${server.port}`,
    DISPATCH_TOKEN: "test-token",
    DISPATCH_STATE_DIR: stateDir,
    ...extra,
  };
}

async function run(
  argv: readonly string[],
  extra: Record<string, string | undefined> = omp,
  stdin = "",
  fetchImpl?: typeof fetch
): Promise<{ code: number; stdout: string; stderr: string }> {
  let stdout = "";
  let stderr = "";
  const code = await runDispatchCli(argv, environment(extra), {
    stdout: (text) => {
      stdout += text;
    },
    stderr: (text) => {
      stderr += text;
    },
    readText: (path) => (path === "-" ? stdin : readFileSync(resolve(stateDir, path), "utf-8")),
    cwd: stateDir,
    ...(fetchImpl === undefined ? {} : { fetchImpl }),
  });
  return { code, stdout, stderr };
}

/** The arguments the gate binds a reviewed marker to: what the call's own dry run prints. */
async function dryArgs(argv: readonly string[], stdin = ""): Promise<Record<string, unknown>> {
  const [command, ...rest] = argv;
  const { code, stdout } = await run([command ?? "", "--dry-run", ...rest], omp, stdin);
  expect(code).toBe(0);
  return JSON.parse(stdout);
}

function ledger(id = "ses-1"): Array<Record<string, unknown>> {
  return readFileSync(join(stateDir, "sessions", id, "results.jsonl"), "utf-8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line));
}

const omp = { DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-1" };
const MESSAGE = ["message", "--issue", "DSP-1", "--body", "Deliverable landed."];
const REFUSAL = "dispatch message was not called: AskGate is on for this session";

describe("AskGate at the dispatch CLI", () => {
  // A live record and no marker is a call AskGate never reviewed, whatever the session's mode:
  // the gate did not read the command that ran it. Refusing it would be a check that never
  // happened stopping the work; the write posts with its warning instead (INFRA-2079, row 3.5),
  // and the ledger carries what went out unreviewed. The rows of the last describe hold that in
  // full; this one holds that nothing of it reads as a refusal.
  test("a gated write with a live record and no marker posts, and is logged, not refused", async () => {
    writeRecord();
    const { code, stdout } = await run(MESSAGE);
    expect(code).toBe(0);
    expect(stdout).toContain("carries no AskGate marker");
    expect(cliRefusals(stdout)).toEqual([]);
    expect(messagePosts()).toHaveLength(1);
    expect(ledger()[0]).toMatchObject({
      tool: "dispatch_message",
      askgate: { mode: "warn" },
    });
  });

  test("a matching marker lets the write post once, and is consumed", async () => {
    writeRecord();
    const marker = mint(MESSAGE, await dryArgs(MESSAGE));
    const { code } = await run(MESSAGE);
    expect(code).toBe(0);
    expect(messagePosts()).toHaveLength(1);
    expect(existsSync(marker)).toBe(false);
  });

  // The marker is single use: the second call of the same argv finds none. The gate reads a
  // literal resend and reviews it again, so the write that posts a second time unreviewed is one
  // AskGate never saw, which posts with its warning like any other.
  test("a marker is single use: the same call again finds none and warns", async () => {
    writeRecord();
    mint(MESSAGE, await dryArgs(MESSAGE));
    const first = await run(MESSAGE);
    expect(first.code).toBe(0);
    expect(first.stdout).not.toContain("AskGate");
    const again = await run(MESSAGE);
    expect(again.code).toBe(0);
    expect(again.stdout).toContain("posted without review");
    expect(messagePosts()).toHaveLength(2);
  });

  test("a marker bound to other arguments is refused and left for the call it was minted for", async () => {
    writeRecord();
    const argv = ["message", "--issue", "DSP-1", "--body-file", "-"];
    const marker = mint(argv, await dryArgs(argv, "the reviewed text"));
    const { code, stdout } = await run(argv, omp, "other text");
    expect(code).toBe(1);
    expect(stdout).toStartWith(REFUSAL);
    expect(stdout).toContain("arguments differ from the ones AskGate reviewed");
    expect(messagePosts()).toHaveLength(0);
    expect(existsSync(marker)).toBe(true);
  });

  // A marker is keyed by the argv, so one minted for another command is no permission for this
  // one: this call has none, and AskGate reviewed it not at all, so it posts with its warning
  // and the other call's marker stays where it is.
  test("a marker for another argument vector is no marker for this one", async () => {
    writeRecord();
    const other = mint(["message", "--issue", "DSP-1", "--body", "other"], null);
    const { code, stdout } = await run(MESSAGE);
    expect(code).toBe(0);
    expect(stdout).toContain("this call carries no AskGate marker");
    expect(existsSync(other)).toBe(true);
    expect(messagePosts()).toHaveLength(1);
  });

  test("an argv-only marker, minted for a call no review stands behind, takes any arguments", async () => {
    writeRecord();
    const argv = ["message", "--issue", "DSP-1", "--body-file", "-"];
    const marker = mint(argv, null);
    expect((await run(argv, omp, "whatever text")).code).toBe(0);
    expect(messagePosts()).toHaveLength(1);
    expect(existsSync(marker)).toBe(false);
  });

  // The review of the marker design at cf3f1e021, C1, and its attack, B1 and B2: the gate reviews
  // each file a write posts in place, and its marker binds the digest of the bytes it read. The CLI
  // reads each file the argv names once before it posts, and a file that changed since the review
  // is refused with what to do, since its call is literal already.
  describe("a file the write posts", () => {
    const CHANGED = "a file this write posts changed after AskGate read it";
    /** The marker the gate mints for `argv` once it reviewed `file`, which `flag` names. */
    const reviewed = async (argv: readonly string[], flag: string, file: string) =>
      mint(argv, await dryArgs(argv), Date.now(), [
        { flag, operand: file, sha256: fileSha(resolve(stateDir, file)) },
      ]);
    const note = () => join(stateDir, "note.md");
    const comment = () => ["comment", "--issue", "DSP-1", "--body-file", note()];
    const upload = (file: string) => [
      "artifact",
      "--issue",
      "DSP-1",
      "--name",
      "brief.md",
      "--path",
      file,
    ];

    test("a --body-file that AskGate reviewed posts once", async () => {
      writeRecord();
      writeFileSync(note(), "The reviewed text.\n");
      const marker = await reviewed(comment(), "--body-file", note());
      expect((await run(comment())).code).toBe(0);
      expect(postsTo(COMMENTS).map((p) => p.sent)).toMatchObject([
        { body: "The reviewed text.\n" },
      ]);
      expect(existsSync(marker)).toBe(false);
    });

    test("a --body-file changed after the review is refused with what to do, and nothing posts", async () => {
      writeRecord();
      writeFileSync(note(), "The reviewed text.\n");
      const marker = await reviewed(comment(), "--body-file", note());
      writeFileSync(note(), "Text an edit of the same message wrote.\n");
      const { code, stdout } = await run(comment());
      expect(code).toBe(1);
      expect(stdout).toStartWith("dispatch comment was not called: AskGate is on for this session");
      expect(stdout).toContain(`${CHANGED} (\`--body-file ${note()}\`)`);
      expect(stdout).toContain(
        "Send the call again in a later message than the one that changes the file."
      );
      expect(stdout).not.toContain("literal words");
      expect(posts).toHaveLength(0);
      expect(existsSync(marker)).toBe(true);
      expect(ledger()).toMatchObject([
        {
          tool: "dispatch_comment",
          askgate: { mode: "block", why: expect.stringContaining(CHANGED) },
        },
      ]);
    });

    test("a markdown upload by a relative --path that AskGate reviewed posts its bytes once", async () => {
      writeRecord();
      writeFileSync(join(stateDir, "brief.md"), "# Brief\n");
      await reviewed(upload("brief.md"), "--path", "brief.md");
      expect((await run(upload("brief.md"))).code).toBe(0);
      expect(postsTo(ARTIFACTS).map((p) => p.sent)).toEqual(["# Brief\n"]);
    });

    test("an upload by --path changed after the review is refused, and nothing posts", async () => {
      writeRecord();
      const brief = join(stateDir, "brief.md");
      writeFileSync(brief, "# Brief\n");
      await reviewed(upload(brief), "--path", brief);
      writeFileSync(brief, "# Brief, edited in the same message\n");
      const { code, stdout } = await run(upload(brief));
      expect(code).toBe(1);
      expect(stdout).toContain(`${CHANGED} (\`--path ${brief}\`)`);
      expect(stdout).toContain(
        "Send the call again in a later message than the one that changes the file."
      );
      expect(posts).toHaveLength(0);
    });

    test("an upload by --path sends the bytes the check read, though the file changes before the send", async () => {
      writeRecord();
      const brief = join(stateDir, "brief.md");
      writeFileSync(brief, "# Brief\n");
      await reviewed(upload(brief), "--path", brief);
      // The file changes after the check, as the request goes out: a fresh read would send it.
      const changing = (async (url: string | URL | Request, init?: RequestInit) => {
        writeFileSync(brief, "# Brief, changed after the check\n");
        return fetch(url, init);
      }) as typeof fetch;
      expect((await run(upload(brief), omp, "", changing)).code).toBe(0);
      expect(postsTo(ARTIFACTS).map((p) => p.sent)).toEqual(["# Brief\n"]);
    });

    test("in warn mode a changed file posts with a warning and is logged first", async () => {
      writeRecord("ses-1", undefined, "warn");
      writeFileSync(note(), "The reviewed text.\n");
      await reviewed(comment(), "--body-file", note());
      writeFileSync(note(), "Text an edit of the same message wrote.\n");
      const { code, stdout } = await run(comment());
      expect(code).toBe(0);
      expect(stdout).toContain(
        `dispatch: warning: AskGate is on for this session in warn mode, and ${CHANGED}`
      );
      expect(stdout).toContain(
        "Send a write in a later message than the call that changes its file."
      );
      expect(postsTo(COMMENTS)).toHaveLength(1);
      expect(ledger()[0]).toMatchObject({
        tool: "dispatch_comment",
        askgate: { mode: "warn", why: expect.stringContaining(CHANGED) },
      });
    });

    test("with block not chosen, the default, a changed file posts with its warning", async () => {
      writeRecord("ses-1", undefined, null);
      writeFileSync(note(), "The reviewed text.\n");
      await reviewed(comment(), "--body-file", note());
      writeFileSync(note(), "Text an edit of the same message wrote.\n");
      const { code, stdout } = await run(comment());
      expect(code).toBe(0);
      expect(stdout).toContain(
        `dispatch: warning: AskGate is on for this session in warn mode, and ${CHANGED}`
      );
      expect(postsTo(COMMENTS)).toHaveLength(1);
      expect(ledger()[0]).toMatchObject({
        tool: "dispatch_comment",
        askgate: { mode: "warn", why: expect.stringContaining(CHANGED) },
      });
    });
  });

  // The attack on the marker design at cf3f1e021, N1: the gate mints a marker when it prepares the
  // calls of a message, and a call can wait there behind an exclusive call of the same message for
  // up to that call's limit (an eval cell's is 3,600 s). The age limit bounds only a marker its call
  // left behind, since the gate removes each marker when its call ends.
  test("a marker that waited an hour behind another call of its message still posts", async () => {
    writeRecord();
    const marker = mint(MESSAGE, await dryArgs(MESSAGE), Date.now() - 61 * 60_000);
    expect((await run(MESSAGE)).code).toBe(0);
    expect(messagePosts()).toHaveLength(1);
    expect(existsSync(marker)).toBe(false);
  });

  test("a marker older than the longest tool limit is refused and removed", async () => {
    writeRecord();
    const marker = mint(MESSAGE, null, Date.now() - 121 * 60_000);
    const { code, stdout } = await run(MESSAGE);
    expect(code).toBe(1);
    expect(stdout).toContain("expired");
    expect(existsSync(marker)).toBe(false);
    expect(messagePosts()).toHaveLength(0);
  });

  test("the gate's next mint keeps a marker an hour old, and sweeps one older than the age limit", () => {
    writeRecord();
    const waiting = mint(MESSAGE, null);
    const left = mint(["message", "--issue", "DSP-1", "--body", "left behind"], null);
    const hour = (Date.now() - 61 * 60_000) / 1000;
    const old = (Date.now() - 121 * 60_000) / 1000;
    utimesSync(waiting, hour, hour);
    utimesSync(left, old, old);
    mintMarker(
      store(),
      { argv: MESSAGE, args: null, files: [], toolCallId: "call-2", outcome: "verdict" },
      Date.now()
    );
    expect(existsSync(waiting)).toBe(true);
    expect(existsSync(left)).toBe(false);
  });

  // The attack on the marker design at cf3f1e021, N4: the ledger line that tells pi-envoy a write
  // went out unreviewed was written after the post, so a ledger that failed then hid the write.
  // In warn mode the CLI now writes that line on its own before it sends, and the result's line
  // after it carries no `askgate` field, so the report counts the write once.
  test("in warn mode a write with no marker is logged first, then posts and warns", async () => {
    writeRecord("ses-1", undefined, "warn");
    const { code, stdout } = await run(MESSAGE);
    expect(code).toBe(0);
    expect(messagePosts()).toHaveLength(1);
    const [line] = messagePosts()[0]?.ledger.split("\n") ?? [];
    expect(JSON.parse(line ?? "")).toMatchObject({
      tool: "dispatch_message",
      askgate: { mode: "warn", why: "this call carries no AskGate marker" },
    });
    expect(stdout).toContain("dispatch: warning: dispatch message posted without review:");
    expect(cliRefusals(stdout)).toEqual([]);
    const [before, after, ...rest] = ledger();
    expect(rest).toEqual([]);
    expect(before).not.toHaveProperty("details");
    expect(after).toMatchObject({ tool: "dispatch_message", details: expect.any(Object) });
    expect(after).not.toHaveProperty("askgate");
  });

  test("with block not chosen, the default, a write with no marker posts with its warning and an entry", async () => {
    writeRecord("ses-1", undefined, null);
    const { code, stdout } = await run(MESSAGE);
    expect(code).toBe(0);
    expect(messagePosts()).toHaveLength(1);
    expect(stdout).toContain("dispatch: warning: dispatch message posted without review:");
    expect(cliRefusals(stdout)).toEqual([]);
    expect(ledger()[0]).toMatchObject({
      tool: "dispatch_message",
      askgate: { mode: "warn", why: "this call carries no AskGate marker" },
    });
  });

  // pi-envoy records an unmarked write on Dispatch, where a session with no access to this
  // machine counts the asks that went out, or were refused, unreviewed. The count is the CLI's:
  // it alone holds the write's own text. The line carries the count and nothing of the write.
  // The write is not held back, so what it does at the server is this fixture's business and not
  // the gate's: the row reads the line AskGate wrote before the request went out.
  test("the ledger line of an unmarked write counts the asks it carries, and holds no arguments", async () => {
    writeRecord();
    const ask = ["ask", "--issue", "DSP-1", "--question", "Which deployment window?"];
    const { stdout } = await run(ask);
    expect(stdout).toContain("dispatch: warning: dispatch ask posted without review:");
    expect(ledger()[0]).toMatchObject({
      tool: "dispatch_ask",
      askgate: { mode: "warn", why: "this call carries no AskGate marker", asksCarried: 1 },
    });
    expect(Object.keys(ledger()[0] ?? {}).sort()).toEqual(["askgate", "at", "tool"]);
    expect(JSON.stringify(ledger()[0])).not.toContain("deployment window");
  });

  test("an unmarked write that opens no ask carries a count of none", async () => {
    writeRecord();
    expect((await run(MESSAGE)).code).toBe(0);
    expect(ledger()[0]).toMatchObject({
      tool: "dispatch_message",
      askgate: { mode: "warn", asksCarried: 0 },
    });
  });

  // An upload's asks are in its file, which the dry run's arguments name only by path: the count
  // reads the bytes the CLI already read for the marker check.
  test("an unmarked markdown upload counts the ask blocks its own file carries", async () => {
    writeRecord("ses-1", undefined, "warn");
    const brief = join(stateDir, "asks.md");
    writeFileSync(
      brief,
      ":::ask{#window}\nWhich deployment window?\n:::\n\n:::ask{#owner}\nWho owns it?\n:::\n"
    );
    const upload = ["artifact", "--issue", "DSP-1", "--name", "asks.md", "--path", brief];
    expect((await run(upload)).code).toBe(0);
    expect(ledger()[0]).toMatchObject({
      tool: "dispatch_artifact",
      askgate: { mode: "warn", asksCarried: 2 },
    });
    expect(JSON.stringify(ledger()[0])).not.toContain("deployment window");
  });

  test("with no record the write posts and nothing is required", async () => {
    const { code } = await run(MESSAGE);
    expect(code).toBe(0);
    expect(messagePosts()).toHaveLength(1);
  });

  test("a record of a dead process, of a reused pid, of another session or of another host is inert", async () => {
    const child = Bun.spawn(["true"]);
    await child.exited;
    const dead = { pid: child.pid, start: "1", store: store() };
    const reused = { pid: process.pid, start: "1", store: store() };
    for (const owner of [dead, reused]) {
      writeRecord("ses-1", owner);
      expect((await run(MESSAGE)).code).toBe(0);
    }
    writeRecord("ses-other");
    expect((await run(MESSAGE)).code).toBe(0);
    // Claude Code's session: host claude, its own session id, which no Oh My Pi record names.
    writeRecord("ses-1");
    expect(
      (
        await run(MESSAGE, {
          DISPATCH_HOST: "claude",
          CLAUDE_CODE_SESSION_ID: "cc-1",
          DISPATCH_SESSION_ID: "ses-1",
        })
      ).code
    ).toBe(0);
    expect(messagePosts()).toHaveLength(4);
  });

  test("help, a dry run and every ungated command need no marker under a live record", async () => {
    writeFileSync(join(stateDir, "shot.png"), "image bytes");
    writeRecord();
    expect((await run(["message", "--help"])).code).toBe(0);
    expect((await run(["--help"])).code).toBe(0);
    expect((await run([...MESSAGE, "--dry-run"])).code).toBe(0);
    for (const argv of [
      ["search", "--query", "landed"],
      ["issue-update", "--issue", "DSP-1", "--status", "in_progress"],
      ["issue", "--project", "DSP", "--title", "No spec"],
      ["artifact", "--issue", "DSP-1", "--name", "shot.png", "--path", "shot.png"],
    ]) {
      posts.length = 0;
      const { stdout } = await run(argv);
      expect(stdout).not.toContain("AskGate");
      expect(posts.length).toBeGreaterThan(0);
    }
  });

  // Gated, which is what the warning shows: an ungated command prints no AskGate line at all.
  test("an issue with a spec and a markdown upload are gated", async () => {
    writeRecord();
    for (const [argv, command] of [
      [["issue", "--project", "DSP", "--title", "t", "--spec", "# Spec"], "issue"],
      [["artifact", "--issue", "DSP-1", "--name", "brief.md", "--content", "# Brief"], "artifact"],
      [["artifact", "--issue", "DSP-1", "--name", "notes", "--path", "notes.markdown"], "artifact"],
    ] as const) {
      const { stdout } = await run(argv);
      expect(stdout).toContain(`dispatch: warning: dispatch ${command} posted without review:`);
      expect(stdout).not.toContain("was not called: AskGate");
    }
  });

  // No review can run for this session at all, and the call is not at fault: the write posts,
  // and the warning names the record or the store it could not read.
  test("a record that cannot be read, or that names a missing store, posts with the cause named", async () => {
    const self = { pid: process.pid, start: startOf(process.pid) };
    const file = ownerFile("ses-1", self);
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, "{not json");
    const broken = await run(MESSAGE);
    expect(broken.code).toBe(0);
    expect(broken.stdout).toContain(
      "dispatch: warning: dispatch message posted without review: AskGate's record"
    );
    expect(broken.stdout).toContain(file);
    expect(cliRefusals(broken.stdout)).toEqual([]);
    writeFileSync(
      file,
      JSON.stringify({ v: 2, ...self, store: join(stateDir, "gone"), mode: "warn" })
    );
    const missing = await run(MESSAGE);
    expect(missing.code).toBe(0);
    expect(missing.stdout).toContain(join(stateDir, "gone"));
    expect(messagePosts()).toHaveLength(2);
  });

  test("pruning keeps an idle session whose record is live, and removes it once the process is gone", () => {
    writeRecord();
    const dir = join(stateDir, "sessions", "ses-1");
    const old = (Date.now() - 15 * 86_400_000) / 1000;
    utimesSync(dir, old, old);
    pruneSessions(stateDir, Date.now());
    expect(existsSync(dir)).toBe(true);
    rmSync(ownerFile("ses-1", { pid: process.pid, start: startOf(process.pid) }));
    writeRecord("ses-1", { pid: process.pid, start: "1", store: store() });
    // The gone process's store went with it (at its exit, or by the store sweep).
    rmSync(store(), { recursive: true });
    utimesSync(dir, old, old);
    writeFileSync(join(stateDir, ".pruned"), "");
    utimesSync(join(stateDir, ".pruned"), old, old);
    pruneSessions(stateDir, Date.now());
    expect(existsSync(dir)).toBe(false);
  });

  // A session moved into an agent box: its Oh My Pi process runs in the box's own pid namespace and
  // keeps its record in the state directory the box shares with the host. The host's CLI, and
  // every other box's, cannot look that process up by its pid, which there is free or another
  // process's. Judged by pid, the host's daily prune removed the box process's store once no marker
  // had changed it for two hours, and the box's CLI then refused every gated write as a record that
  // names a missing store. This test process stands in for the host: the owner names another pid
  // namespace and a pid no process of this one holds.
  test("a record of another pid namespace's process stays live while its process touches its store", async () => {
    const child = Bun.spawn(["true"]);
    await child.exited;
    const boxStore = join(stateDir, "askgate", `${child.pid}-15039164-1`);
    writeRecord("ses-1", { pid: child.pid, start: "15039164", ns: "1", store: boxStore });
    const dir = join(stateDir, "sessions", "ses-1");
    // The box process minted its last marker three hours ago and has been idle since, as the
    // session has for fifteen days; only its beat (`keepStore`) has touched the store.
    const idle = (Date.now() - MARKER_MAX_AGE_MS - 3_600_000) / 1000;
    utimesSync(join(boxStore, "markers"), idle, idle);
    utimesSync(boxStore, idle, idle);
    keepStore(boxStore, Date.now());
    const longIdle = (Date.now() - 15 * 86_400_000) / 1000;
    utimesSync(dir, longIdle, longIdle);
    pruneSessions(stateDir, Date.now());
    expect(existsSync(join(boxStore, "markers"))).toBe(true);
    expect(existsSync(dir)).toBe(true);
    // The record is live here too: a write from this namespace that the box process minted no
    // marker for posts with AskGate's warning, and one it did mint for posts silently.
    const unreviewed = await run(MESSAGE);
    expect(unreviewed.code).toBe(0);
    expect(unreviewed.stdout).toContain("posted without review");
    expect(unreviewed.stdout).toContain("carries no AskGate marker");
    mint(MESSAGE, null, Date.now(), [], boxStore);
    const marked = await run(MESSAGE);
    expect(marked.code).toBe(0);
    expect(marked.stdout).not.toContain("AskGate");
    expect(messagePosts()).toHaveLength(2);

    // The process ended without removing its store: its beat stops, so after MARKER_MAX_AGE_MS the
    // next prune takes the store, and the one after it the session directory, whose record then
    // names no live owner and no store that is still there.
    utimesSync(join(boxStore, "markers"), idle, idle);
    utimesSync(boxStore, idle, idle);
    utimesSync(join(stateDir, ".pruned"), longIdle, longIdle);
    utimesSync(dir, longIdle, longIdle);
    pruneSessions(stateDir, Date.now());
    expect(existsSync(boxStore)).toBe(false);
    expect(existsSync(dir)).toBe(true);
    utimesSync(join(stateDir, ".pruned"), longIdle, longIdle);
    utimesSync(dir, longIdle, longIdle);
    pruneSessions(stateDir, Date.now());
    expect(existsSync(dir)).toBe(false);
    expect((await run(MESSAGE)).code).toBe(0);
  });

  // A box process on an earlier pi-envoy writes a record with no pid namespace. Here its pid reads
  // gone, so the record reads off, but its store is still there: the prune keeps the session
  // directory with it, so that process's own CLI never finds its record gone. Once the store sweep
  // has taken the store (14 days with no marker), the next prune takes the directory.
  test("pruning keeps an idle session whose record names no namespace while its store is there", async () => {
    const child = Bun.spawn(["true"]);
    await child.exited;
    const oldStore = join(stateDir, "askgate", `${child.pid}-15039164`);
    writeRecord("ses-1", { pid: child.pid, start: "15039164", store: oldStore });
    const dir = join(stateDir, "sessions", "ses-1");
    const minted = (Date.now() - 3 * 3_600_000) / 1000;
    utimesSync(join(oldStore, "markers"), minted, minted);
    utimesSync(oldStore, minted, minted);
    const longIdle = (Date.now() - 15 * 86_400_000) / 1000;
    utimesSync(dir, longIdle, longIdle);
    pruneSessions(stateDir, Date.now());
    expect(existsSync(oldStore)).toBe(true);
    expect(existsSync(dir)).toBe(true);
    utimesSync(join(oldStore, "markers"), longIdle, longIdle);
    utimesSync(oldStore, longIdle, longIdle);
    utimesSync(join(stateDir, ".pruned"), longIdle, longIdle);
    utimesSync(dir, longIdle, longIdle);
    pruneSessions(stateDir, Date.now());
    expect(existsSync(oldStore)).toBe(false);
    utimesSync(join(stateDir, ".pruned"), longIdle, longIdle);
    utimesSync(dir, longIdle, longIdle);
    pruneSessions(stateDir, Date.now());
    expect(existsSync(dir)).toBe(false);
  });

  // The owner remakes its store before each write of its own owner file, so a store another
  // process removed is back, empty, by the owner's next call: a write it never reviewed still finds
  // no marker.
  test("a record whose store another process removed is whole again at the owner's next record write", async () => {
    const self = { pid: process.pid, start: startOf(process.pid), ns: null, store: store() };
    writeRecord("ses-1", self);
    mint(MESSAGE, null);
    rmSync(store(), { recursive: true });
    const broken = await run(MESSAGE);
    expect(broken.code).toBe(0);
    expect(broken.stdout).toContain(`names the marker store ${store()}`);
    setRecordMode(join(stateDir, "sessions", "ses-1"), self, "block");
    expect(readdirSync(markers())).toEqual([]);
    const unmarked = await run(MESSAGE);
    expect(unmarked.code).toBe(0);
    expect(unmarked.stdout).toContain("carries no AskGate marker");
    expect(messagePosts()).toHaveLength(2);
  });
});

// INFRA-2079, row 3.5: a refusal is only ever the verdict of a review that ran. Where no review
// stands behind a call at all — the gate never saw the command that ran it, its record cannot be
// read, the marker check itself failed, the ledger line cannot be written — the write posts with a
// warning that names the call and why no review ran, and the CLI refuses nothing. A review that
// ran and does not cover this write (other arguments, a file changed since it was read, a marker
// past its age) still refuses where the session chose block: the rows above hold that.
describe("a write no review could run for posts with its warning", () => {
  /** The start of the warning the CLI prints for a write it posted with no review behind it. */
  const POSTED = "dispatch: warning: dispatch message posted without review:";

  test("a live record and no marker: no review ever saw this call, so nothing refuses it", async () => {
    writeRecord();
    const { code, stdout } = await run(MESSAGE);
    expect(code).toBe(0);
    expect(cliRefusals(stdout)).toEqual([]);
    expect(stdout).toContain(POSTED);
    expect(stdout).toContain("this call carries no AskGate marker");
    expect(messagePosts()).toHaveLength(1);
    expect(ledger()[0]).toMatchObject({
      tool: "dispatch_message",
      askgate: {
        mode: "warn",
        why: expect.stringContaining("this call carries no AskGate marker"),
        asksCarried: 0,
      },
    });
  });

  test("a record that cannot be read: whether a review ran cannot be told, so nothing refuses it", async () => {
    const self = { pid: process.pid, start: startOf(process.pid) };
    const file = ownerFile("ses-1", self);
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, "{not json");
    const { code, stdout } = await run(MESSAGE);
    expect(code).toBe(0);
    expect(cliRefusals(stdout)).toEqual([]);
    expect(stdout).toContain(POSTED);
    expect(stdout).toContain(file);
    expect(messagePosts()).toHaveLength(1);
  });

  test("a record naming a store that is gone: nothing refuses it either", async () => {
    const self = { pid: process.pid, start: startOf(process.pid) };
    const file = ownerFile("ses-1", self);
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, JSON.stringify({ v: 2, ...self, store: join(stateDir, "gone") }));
    const { code, stdout } = await run(MESSAGE);
    expect(code).toBe(0);
    expect(stdout).toContain(POSTED);
    expect(stdout).toContain(join(stateDir, "gone"));
    expect(messagePosts()).toHaveLength(1);
  });

  // The store passes the record's own check and then cannot be listed: the check throws, which is
  // no review's verdict.
  test("a marker store that cannot be listed: the check itself failed, so nothing refuses it", async () => {
    writeRecord();
    chmodSync(markers(), 0o000);
    try {
      const { code, stdout } = await run(MESSAGE);
      expect(code).toBe(0);
      expect(cliRefusals(stdout)).toEqual([]);
      expect(stdout).toContain(POSTED);
      expect(stdout).toContain("could not be checked");
      expect(messagePosts()).toHaveLength(1);
    } finally {
      chmodSync(markers(), 0o700);
    }
  });

  // The ledger line that records an unreviewed write is written before the write goes out. A
  // ledger that cannot take it is not a verdict either: the write posts, and says the record
  // failed, so the call's own output is the only account of it.
  test("a ledger that cannot take the line: the write posts and the failure is said", async () => {
    writeRecord();
    mkdirSync(join(stateDir, "sessions", "ses-1", "results.jsonl"), { recursive: true });
    const { code, stdout } = await run(MESSAGE);
    expect(code).toBe(0);
    expect(cliRefusals(stdout)).toEqual([]);
    expect(stdout).toContain(POSTED);
    expect(stdout).toContain("could not be recorded");
    expect(messagePosts()).toHaveLength(1);
  });

  // The one outcome that must never change: a review that ran and does not cover this write still
  // refuses where the session chose block.
  test("a review that ran and does not cover this write still refuses", async () => {
    writeRecord();
    const argv = ["message", "--issue", "DSP-1", "--body-file", "-"];
    mint(argv, await dryArgs(argv, "the reviewed text"));
    const { code, stdout } = await run(argv, omp, "other text");
    expect(code).toBe(1);
    expect(stdout).toStartWith(REFUSAL);
    expect(stdout).toContain("arguments differ from the ones AskGate reviewed");
    expect(messagePosts()).toHaveLength(0);
  });
});

describe("AskGate at the dispatch entry, across processes", () => {
  const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");
  // The 30 s limit is a hang guard, not a budget: the row spawns two child processes that each
  // start the dispatch entry, and bun's unwritten 5,000 ms per-test default is a limit on that
  // work rather than on a hang. Under 16 busy loops (8 x nproc) in a 2-CPU pod it timed out,
  // 3 runs of 3, at 5,104 to 5,202 ms with this file alone. Measured there: 0.71 s idle in a
  // whole-suite run and 3.00 s with this file alone; under those loops, 8.78 s at most in a
  // whole-suite run and 5.20 s with this file alone. It fires only if a child never exits.
  test("one marker and two concurrent calls of its argv: exactly one is the reviewed one", async () => {
    writeRecord();
    mint(MESSAGE, null);
    const spawnOne = () =>
      Bun.spawn([process.execPath, ENTRY, ...MESSAGE], {
        env: {
          PATH: process.env.PATH,
          ...(environment(omp) as Record<string, string>),
        },
        stdout: "pipe",
        stderr: "pipe",
      });
    const children = [spawnOne(), spawnOne()];
    const results = await Promise.all(
      children.map(async (child) => ({
        code: await child.exited,
        stdout: await new Response(child.stdout).text(),
      }))
    );
    // The marker is single use, so exactly one of the two carries the review; the other is a
    // write no review stands behind, which posts with its warning rather than being refused.
    expect(results.map((r) => r.code)).toEqual([0, 0]);
    expect(results.filter((r) => r.stdout.includes("posted without review"))).toHaveLength(1);
    expect(messagePosts()).toHaveLength(2);
    expect(readdirSync(markers())).toEqual([]);
  }, 30_000);

  // Every form AskGate's gate does not read, run as an agent would run it: each reaches the CLI as
  // the session and finds the live record and no marker. The gate never read the command, so no
  // review ran and nothing refuses the write: it posts, with the warning that names the call and
  // says why (INFRA-2079, row 3.5). With no record the same form posts silently: the check is
  // inert outside a gated Oh My Pi session.
  //
  // The 240 s limit is a hang guard, not a budget, and sits above the suite's 30 s default. The row
  // runs twelve child process trees, six caller forms twice, and none of that is spare: each form
  // is a caller kind the row covers, and the second pass is the inertness claim, so the work cannot
  // shrink without dropping an assertion. Measured in a 2-CPU pod: 3.67 s idle in a whole-suite run
  // and 23.90 s with this file alone; under 16 busy loops (8 x nproc), 58.37 s at most in a
  // whole-suite run and 56.71 s with this file alone, which left its old 60 s limit 1.03x in hand.
  // It fires only if a child never exits.
  test("a gated write from xargs, python3 -c, node -e, bun -e, an alias or a script posts with its warning; with no record it posts silently", async () => {
    const bin = join(stateDir, "bin");
    mkdirSync(bin);
    writeFileSync(
      join(bin, "dispatch"),
      `#!/bin/sh\nexec "${process.execPath}" "${ENTRY}" "$@"\n`,
      { mode: 0o755 }
    );
    const script = join(stateDir, "post.sh");
    writeFileSync(script, "dispatch message --issue DSP-1 --body 'from a script'\n");
    const argv = '"message", "--issue", "DSP-1", "--body", "hi"';
    const forms: Array<[string, string[]]> = [
      ["xargs", ["bash", "-c", "printf '%s\\n' message --issue DSP-1 --body hi | xargs dispatch"]],
      ["python3 -c", ["python3", "-c", `import subprocess; subprocess.run(["dispatch", ${argv}])`]],
      [
        "node -e",
        [
          "node",
          "-e",
          `require("node:child_process").spawnSync("dispatch", [${argv}], { stdio: "inherit" })`,
        ],
      ],
      [
        "bun -e",
        [process.execPath, "-e", `Bun.spawnSync(["dispatch", ${argv}], { stdout: "inherit" })`],
      ],
      [
        "an alias",
        [
          "bash",
          "-c",
          "shopt -s expand_aliases\nalias d=dispatch\nd message --issue DSP-1 --body hi",
        ],
      ],
      ["a script", ["bash", script]],
    ];
    const runForm = async (command: string[]) => {
      const child = Bun.spawn(command, {
        cwd: stateDir,
        env: {
          ...(environment(omp) as Record<string, string>),
          PATH: `${bin}:${process.env.PATH ?? ""}`,
        },
        stdout: "pipe",
        stderr: "pipe",
      });
      await child.exited;
      return new Response(child.stdout).text();
    };
    writeRecord();
    for (const [form, command] of forms) {
      const stdout = await runForm(command);
      expect({ form, warned: stdout.includes("posted without review") }).toEqual({
        form,
        warned: true,
      });
    }
    expect(messagePosts()).toHaveLength(forms.length);
    rmSync(join(stateDir, "sessions", "ses-1", "askgate"), { recursive: true });
    for (const [, command] of forms) {
      const stdout = await runForm(command);
      expect(stdout).not.toContain("AskGate");
    }
    expect(messagePosts()).toHaveLength(forms.length * 2);
  }, 240_000);
});
