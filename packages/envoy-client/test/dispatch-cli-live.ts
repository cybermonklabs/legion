import { spawn } from "node:child_process";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { type DispatchStack, useDispatchStackDirectory } from "./dispatch-stack";

// What every file that runs the real `dispatch` CLI of this checkout as a child process against a
// real Dispatch server of this checkout works out of. `dispatchLive` holds the calls of the
// server's own routes that make what a row writes about, as a person and over the deployment's
// credential, and one run of the CLI entry in the environment written out below, which answers how
// the run ended. `dispatchCliLive` builds on it what design-gate-advice.live.test.ts,
// moved-approval-advice.live.test.ts and comment-mention.live.test.ts share: an issue in the
// workflow, a document, and the CLI's `doc-edit` and `read`, which fail the row that called them
// on a refusal. artifact-dry-run.live.test.ts runs on `dispatchLive` alone: its rows make a plain
// issue, and each holds a refusal of the command to what Dispatch answers. Each file keeps the
// constants, the writes and the sentences that are its own, and reads the harness they share from
// here.

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const { PATH: SHELL_PATH = "" } = process.env;

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

/** One run of the CLI entry: its exit code and what it printed. */
export interface CliRun {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
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
 * Every file that runs on `dispatchCliLive` calls this once, at its own module scope, as
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

/** What a live file hands `dispatchLive`, which is everything it cannot know on its own. */
export interface DispatchLiveOptions {
  /** The running stack, read at every call: the file's `beforeAll` starts it, which is after this
   *  factory has answered, and a helper called before that throws rather than reaching nothing. */
  readonly stack: () => DispatchStack | undefined;
  /** The session the CLI acts as, which every child process below is run as. */
  readonly session: string;
  /** The person whose own calls these are, as the server's header identity names them. */
  readonly person: string;
}

/** The calls of the server and the run of the CLI a live file makes what it proves out of. */
export interface DispatchLive {
  /** A person's own call, which the server's header identity takes as that person. A refusal
   *  throws. */
  asPerson<T>(method: string, route: string, body?: unknown): Promise<T>;
  /** A machine's call, over the credential the deployment configured: the credential a
   *  deployment's daemon publishes a gate record with, an agent records a plan review or opens an
   *  approval request with, and the CLI itself reaches the server with. A refusal throws. */
  asMachine<T>(method: string, route: string, body?: unknown): Promise<T>;
  /**
   * The real `dispatch` entry, its own process, run with `args` against the stack's Dispatch as an
   * agent's shell runs it, and how the run ended: its exit code and what it printed, a refusal as
   * much as a success.
   *
   * `home` is the run's `HOME`, its `DISPATCH_STATE_DIR` and the directory it runs in. `stdin` is
   * the bytes the command reads on its standard input, as a pipe hands them to a
   * `--content-file -`, and then the end of that input; without it the command's standard input
   * is ignored. A command that exits before it has read the whole input fails the row: the write's
   * error rejects the run.
   */
  runDispatch(args: readonly string[], home: string, stdin?: Uint8Array): Promise<CliRun>;
}

/**
 * The calls of the server and the run of the CLI entry a live file's rows make, over the stack the
 * calling file started.
 *
 * A file calls this, or `dispatchCliLive`, once, in the body of its `describe`.
 */
export function dispatchLive(options: DispatchLiveOptions): DispatchLive {
  const { session, person } = options;

  /** The running server, or the error that says why no row can run. */
  function server(): DispatchStack {
    const stack = options.stack();
    if (stack === undefined) throw new Error("the Dispatch stack did not start");
    return stack;
  }

  /** One call of the server's own routes, its answer's JSON, and a throw on any refusal. */
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

  function runDispatch(args: readonly string[], home: string, stdin?: Uint8Array): Promise<CliRun> {
    const argv = [ENTRY, ...args];
    // The child's environment is written here rather than inherited: a Legion pane exports
    // `DISPATCH_TOKEN_FILE`, which wins over `DISPATCH_TOKEN` in this command
    // (`src/dispatch-config.ts`), and a `DISPATCH_URL` or `ENVOY_CONFIG_FILE` of its own, either of
    // which would point the command at another Dispatch. `HOME` and `DISPATCH_STATE_DIR` are the
    // row's own directory, so the command reads no developer's `envoy.json` and writes its session
    // state nowhere but there.
    const run = {
      cwd: home,
      env: {
        PATH: SHELL_PATH,
        HOME: home,
        DISPATCH_STATE_DIR: home,
        DISPATCH_HOST: "omp",
        DISPATCH_SESSION_ID: session,
        DISPATCH_URL: server().url,
        DISPATCH_TOKEN: server().token,
      },
    };
    const child =
      stdin === undefined
        ? spawn(process.execPath, argv, { ...run, stdio: ["ignore", "pipe", "pipe"] })
        : spawn(process.execPath, argv, { ...run, stdio: ["pipe", "pipe", "pipe"] });
    const finished = Promise.withResolvers<CliRun>();
    if (child.stdin !== null) {
      // A command that exits before it has read the whole input fails the row here rather than
      // leaving the write's EPIPE unhandled.
      child.stdin.once("error", finished.reject);
      child.stdin.end(stdin);
    }
    let stdout = "";
    let stderr = "";
    child.stdout.setEncoding("utf8");
    child.stdout.on("data", (chunk: string) => {
      stdout += chunk;
    });
    child.stderr.setEncoding("utf8");
    child.stderr.on("data", (chunk: string) => {
      stderr += chunk;
    });
    child.once("error", finished.reject);
    child.once("close", (code) => finished.resolve({ code: code ?? -1, stdout, stderr }));
    return finished.promise;
  }

  return { asPerson, asMachine, runDispatch };
}

/** What a file hands `dispatchCliLive`: `dispatchLive`'s options, and what its rows write with. */
export interface DispatchCliLiveOptions extends DispatchLiveOptions {
  /** The spec every issue below is created with, which each file words for the rows it writes.
   *  No row asserts the text, and every row needs one. */
  readonly spec: string;
  /** The directories the file reserved at its module scope. */
  readonly directories: DispatchCliDirectories;
}

/** `dispatchLive`'s calls and run, and the writes and the CLI commands built on them. */
export interface DispatchCliLive extends DispatchLive {
  /** An issue in the workflow, with a spec of its own: an issue Dispatch still shows in `triage`
   *  is out of the workflow, and neither the advice's gates nor its approval requests name one. */
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
}

/**
 * The harness the files whose rows write an issue with a spec run on, over the stack the calling
 * file started: `dispatchLive`'s, with the CLI's state directory the file reserved as the `HOME`
 * of each command it runs.
 *
 * A file calls this once, in the body of its `describe`, and keeps for itself the project keys,
 * the writes and the sentences that are its own.
 */
export function dispatchCliLive(options: DispatchCliLiveOptions): DispatchCliLive {
  const live = dispatchLive(options);
  const { asPerson } = live;

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

  /** The real CLI entry run as an agent's shell runs it, `dispatch <command> --issue <issue>` and
   *  `args`, and the output it printed. A refusal, or anything on standard error, throws. */
  async function cli(command: string, issue: string, args: readonly string[]): Promise<string> {
    const { code, stdout, stderr } = await live.runDispatch(
      [command, "--issue", issue, ...args],
      options.directories.state
    );
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

  return { ...live, createIssue, upload, docEdit, read };
}
