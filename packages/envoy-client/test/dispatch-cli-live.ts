import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { type DispatchStack, useDispatchStackDirectory } from "./dispatch-stack";

// What every file that proves a clause of a document write's advice over the whole path it is
// printed through works out of: the real `dispatch` CLI of this checkout, run as a child process
// against a real Dispatch server of this checkout, and the writes over the server's own routes
// that make what a row writes about. The files are design-gate-advice.live.test.ts and
// moved-approval-advice.live.test.ts; each keeps the constants, the writes and the sentences that
// are its own, and reads the harness the two share from here.

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const { PATH: SHELL_PATH } = process.env;

/** The CLI entry a row runs, as an agent's shell reaches it: this checkout's own `dispatch`. */
const ENTRY = join(import.meta.dir, "..", "bin", "dispatch.ts");

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
 * directory and the `afterAll` that removes it where they are called, and `bun:test` takes the
 * scope of a hook from the point of the call, so a call made here would reserve one pair for
 * whichever file loaded this module first.
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
  /** The session the CLI acts as, which every child process below is run as. */
  readonly session: string;
  /** The person whose own writes these are, as the server's header identity names them. */
  readonly person: string;
  /** The spec every issue below is created with, which each file words for the rows it writes:
   *  the design one file's gates record, and the design the other's approval requests are opened
   *  on. No row asserts the text, and every row needs one. */
  readonly spec: string;
  /** The directories the file reserved at its module scope. */
  readonly directories: DispatchCliDirectories;
}

/** The writes and the child process a live file makes what it proves out of. */
export interface DispatchCliLive {
  /** A person's own write, which the server's header identity takes as that person. */
  asPerson<T>(method: string, route: string, body?: unknown): Promise<T>;
  /** A machine's write, over the credential the deployment configured: the credential a
   *  deployment's daemon publishes a gate record with, an agent records a plan review or opens an
   *  approval request with, and the CLI itself reaches the server with. */
  asMachine<T>(method: string, route: string, body?: unknown): Promise<T>;
  /** An issue in the workflow, with a spec of its own: an issue Dispatch still shows in `triage`
   *  is out of the workflow, and neither the advice's gates nor its approval requests name one. */
  createIssue(project: string, title: string, parent?: string): Promise<CreatedIssue>;
  /** A document of the issue, uploaded as the person who owns it. */
  upload(issue: string, name: string, content: string): Promise<UploadedDocument>;
  /**
   * One insert at the end of a document, run through the real CLI entry as an agent's shell runs
   * it, and the output it printed. A refusal, or anything on standard error, fails the row that
   * called it: every row of both files is about the line a write that went through printed.
   */
  docEdit(issue: string, artifact: string, sentence: string): Promise<string>;
}

/**
 * The harness both live files run their rows on, over the stack the calling file started.
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

  async function docEdit(issue: string, artifact: string, sentence: string): Promise<string> {
    const child = Bun.spawn(
      [
        process.execPath,
        ENTRY,
        "doc-edit",
        "--issue",
        issue,
        "--artifact",
        artifact,
        "--ops-json",
        JSON.stringify([{ op: "insert", markdown: sentence, after: "end" }]),
      ],
      {
        env: {
          PATH: SHELL_PATH,
          HOME: directories.state,
          DISPATCH_STATE_DIR: directories.state,
          DISPATCH_HOST: "omp",
          DISPATCH_SESSION_ID: session,
          DISPATCH_URL: server().url,
          DISPATCH_TOKEN: server().token,
        },
        stdout: "pipe",
        stderr: "pipe",
      }
    );
    const [stdout, stderr, code] = await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ]);
    if (code !== 0 || stderr !== "") {
      throw new Error(`dispatch doc-edit on ${issue} exited ${code}: ${stderr}${stdout}`);
    }
    return stdout.trimEnd();
  }

  return { asPerson, asMachine, createIssue, upload, docEdit };
}
