import { join } from "node:path";
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { type DispatchStack, useDispatchStackDirectory } from "./dispatch-stack";

// What every file that proves a clause of what the `dispatch` CLI prints over the whole path it is
// printed through works out of: the real `dispatch` CLI, run as a child process against a real
// Dispatch server of this checkout, and the writes over the server's own routes that make what a
// row writes about. The files are design-gate-advice.live.test.ts and
// moved-approval-advice.live.test.ts, on a document write's advice, comment-mention.live.test.ts,
// on a comment's --mention, and result-role.live.test.ts, on the role a recorded result stores;
// each keeps the constants, the writes and the sentences that are its own, and reads the harness
// they share from here.

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const { PATH: SHELL_PATH, DISPATCH_CLI_ENTRY } = process.env;

/**
 * The command a row runs the CLI with, as an agent's shell reaches it. DISPATCH_CLI_ENTRY, where
 * it is set, is the absolute path of a built `dispatch` (a host plugin's bundle, or the build of
 * another tree for a negative control), which is spawned directly; unset, it is this checkout's
 * own source entry, run by the Bun that runs the file.
 */
const COMMAND: readonly string[] =
  DISPATCH_CLI_ENTRY === undefined || DISPATCH_CLI_ENTRY === ""
    ? [process.execPath, join(import.meta.dir, "..", "bin", "dispatch.ts")]
    : [DISPATCH_CLI_ENTRY];

/** An issue as its creation answers it. */
export interface CreatedIssue {
  readonly key: string;
  readonly primary_artifact_id: string;
}

/** A document as its upload answers it. */
export interface UploadedDocument {
  readonly artifact: { readonly id: string };
  readonly version: { readonly number: number };
}

/** The two directories one live file works in. */
export interface DispatchCliDirectories {
  /** The directory the file's stack works in, as `startDispatchStack` takes it. */
  readonly stack: string;
  /** The CLI's own `HOME` and state directory, which every row's child process is given. */
  readonly state: string;
}

/**
 * Reserves both directories one live file works in — the stack's own and the CLI's state — and
 * returns their paths.
 *
 * Every live file calls this once, at its own module scope, as
 * `const directories = useDispatchCliDirectories()`. The call cannot live in this module's body:
 * `useDispatchStackDirectory` and `temporaryDirectory` each register the `beforeAll` that makes a
 * directory and the `afterAll` that removes it (the stack's through a removal it does not wait
 * for) where they are called, and `bun:test` takes the scope of a hook from the point of the call,
 * so a call made here would reserve one pair for whichever file loaded this module first.
 */
export function useDispatchCliDirectories(): DispatchCliDirectories {
  return {
    stack: useDispatchStackDirectory(),
    state: temporaryDirectory("dispatch-cli-live-state"),
  };
}

/** What a live file hands the harness, which is everything the harness cannot know on its own. */
export interface DispatchCliLiveOptions {
  /** The running stack, read at every call: the file's `beforeAll` starts it, which is after this
   *  factory has answered, and a helper called before that throws rather than reaching nothing. */
  readonly stack: () => DispatchStack | undefined;
  /** The session the CLI acts as in `docEdit` and `read`; a row that calls `runAs` names its
   *  own. */
  readonly session: string;
  /** The person whose own writes these are, as the server's header identity names them. */
  readonly person: string;
  /** The spec every issue below is created with, which each file words for the rows it writes.
   *  No row asserts the text, and every row needs one. */
  readonly spec: string;
  /** The directories the file reserved at its module scope. */
  readonly directories: DispatchCliDirectories;
}

/** What one run of the CLI ended with: its exit status and everything it printed, unchanged. */
export interface DispatchRun {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

/** The writes and the child process a live file makes what it proves out of. */
export interface DispatchCliLive {
  /** A person's own write, which the server's header identity takes as that person. */
  asPerson<T>(method: string, route: string, body?: unknown): Promise<T>;
  /** A machine's write, over the credential the deployment configured: the credential a
   *  deployment's daemon publishes a gate record with, an agent records a plan review or opens an
   *  approval request with, and the CLI itself reaches the server with. */
  asMachine<T>(method: string, route: string, body?: unknown): Promise<T>;
  /** An issue in the workflow, with a spec of its own, which is where a running tree's gate
   *  stands. */
  createIssue(project: string, title: string, parent?: string): Promise<CreatedIssue>;
  /** A document of the issue, uploaded as the person who owns it. */
  upload(issue: string, name: string, content: string): Promise<UploadedDocument>;
  /**
   * One insert at the end of a document, run through the real CLI entry as an agent's shell runs
   * it, and the output it printed. A refusal, or anything on standard error, fails the row that
   * called it: every row that calls it is about the line a write that went through printed.
   */
  docEdit(issue: string, artifact: string, sentence: string): Promise<string>;
  /**
   * A read of one of the issue's documents, run through the real CLI entry as `docEdit` is, and
   * the summary it printed: the document's `Approval:` line among it. A refusal, or anything on
   * standard error, fails the row that called it.
   */
  read(issue: string, artifact: string): Promise<string>;
  /**
   * The real CLI entry run as an agent's shell runs it in that session, `dispatch <command>` and
   * `args` (DISPATCH_SESSION_ID is `session`), and what the run ended with. A refusal is an
   * answer here, not an error: the row reads the exit status and both streams itself.
   */
  runAs(session: string, command: string, args: readonly string[]): Promise<DispatchRun>;
  /**
   * Publishes which session holds each Legion role of the issue, `holders` from role to session,
   * as the Legion daemon of the issue's project publishes it: PUT /api/v1/issues/{key}/role-holders
   * over the machine credential, as session `legion-daemon:<project>`, where `<project>` is the
   * key up to its first `-`. The map is the issue's whole set: a session it does not name holds no
   * role of the issue afterwards, and `{}` clears every role.
   */
  holdRoles(issue: string, holders: Readonly<Record<string, string>>): Promise<void>;
}

/**
 * The harness every live file runs its rows on, over the stack the calling file started.
 *
 * A file calls this once, in the body of its `describe`, and keeps for itself the project keys,
 * the writes and the sentences that are its own.
 */
export function dispatchCliLive(options: DispatchCliLiveOptions): DispatchCliLive {
  const { session, person, directories } = options;

  /** The running server, or the error that says why no row can run. */
  function server(): DispatchStack {
    const stack = options.stack();
    if (stack === undefined) throw new Error("the Dispatch stack did not start");
    return stack;
  }

  async function request<T>(
    method: string,
    route: string,
    body: unknown,
    headers: Readonly<Record<string, string>>
  ): Promise<T> {
    const response = await fetch(`${server().url}${route}`, {
      method,
      headers: { "Content-Type": "application/json", ...headers },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    const text = await response.text();
    if (!response.ok) throw new Error(`${method} ${route}: ${response.status} ${text}`);
    return JSON.parse(text) as T;
  }

  const asPerson = <T>(method: string, route: string, body?: unknown): Promise<T> =>
    request<T>(method, route, body, { "X-Dispatch-User": person });

  const asMachine = <T>(method: string, route: string, body?: unknown): Promise<T> =>
    request<T>(method, route, body, { Authorization: `Bearer ${server().token}` });

  const createIssue = async (
    project: string,
    title: string,
    parent?: string
  ): Promise<CreatedIssue> => {
    const issue = await asPerson<CreatedIssue>("POST", "/api/v1/issues", {
      project,
      title,
      force: true,
      spec: options.spec,
      ...(parent === undefined ? {} : { parent }),
    });
    await asPerson("PATCH", `/api/v1/issues/${issue.key}`, { status: "in_progress" });
    return issue;
  };

  const upload = (issue: string, name: string, content: string): Promise<UploadedDocument> =>
    asPerson<UploadedDocument>("POST", `/api/v1/issues/${issue}/artifacts`, { name, content });

  const runAs = async (
    actingSession: string,
    command: string,
    args: readonly string[]
  ): Promise<DispatchRun> => {
    const child = Bun.spawn([...COMMAND, command, ...args], {
      env: {
        ...bunCacheOff,
        PATH: SHELL_PATH,
        HOME: directories.state,
        DISPATCH_STATE_DIR: directories.state,
        DISPATCH_HOST: "omp",
        DISPATCH_SESSION_ID: actingSession,
        DISPATCH_URL: server().url,
        DISPATCH_TOKEN: server().token,
      },
      stdout: "pipe",
      stderr: "pipe",
    });
    const [stdout, stderr, code] = await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ]);
    return { code, stdout, stderr };
  };

  /** `runAs` in the file's own session, `dispatch <command> --issue <issue>` and `args`, and the
   *  output it printed. A refusal, or anything on standard error, throws. */
  async function cli(command: string, issue: string, args: readonly string[]): Promise<string> {
    const { code, stdout, stderr } = await runAs(session, command, ["--issue", issue, ...args]);
    if (code !== 0 || stderr !== "") {
      throw new Error(`dispatch ${command} on ${issue} exited ${code}: ${stderr}${stdout}`);
    }
    return stdout.trimEnd();
  }

  const docEdit = (issue: string, artifact: string, sentence: string): Promise<string> =>
    cli("doc-edit", issue, [
      "--artifact",
      artifact,
      "--ops-json",
      JSON.stringify([{ op: "insert", markdown: sentence, after: "end" }]),
    ]);

  const read = (issue: string, artifact: string): Promise<string> =>
    cli("read", issue, ["--artifact", artifact]);

  const holdRoles = async (
    issue: string,
    holders: Readonly<Record<string, string>>
  ): Promise<void> => {
    const project = issue.split("-", 1)[0];
    await asMachine("PUT", `/api/v1/issues/${issue}/role-holders`, {
      holders: Object.entries(holders).map(([role, holder]) => ({ role, session: holder })),
      actor: { kind: "session", id: `legion-daemon:${project}` },
    });
  };

  return { asPerson, asMachine, createIssue, upload, docEdit, read, runAs, holdRoles };
}
