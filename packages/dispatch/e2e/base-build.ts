/** The build of the release before, booted beside a stack's own server, for the rows that read
 *  both builds at once (the check c-inbox-unchanged): the commit `BASELINE_SHA`
 *  names, built once per run, its dashboard where a row opens its pages, and this checkout's own
 *  build, which such a row boots beside it.
 *
 *  The base is `BASELINE_BINARY` where the run gives one (a prebuilt `./cmd/dispatch` of that
 *  commit), and otherwise the commit's tree taken out of the repository with `git archive` - no
 *  worktree, no checkout - and built with the stamp the image build gives a server
 *  (`-ldflags "-X main.buildCommit=<commit>"`), as `e2e/acceptance/lib-baseline.sh` builds it. The
 *  build goes to `$TMPDIR/dispatch-e2e-base-<commit>/dispatch` under `flock`, written whole and
 *  renamed, so the workers of a run build it once between them and a later run reuses it. A
 *  machine with neither that binary nor `go`, `git`, `tar` and `flock` cannot build it, and the row
 *  says so (`baseBuild`'s `skip`): CI's browser lane runs in a container with no Go. The base's
 *  dashboard is built the same way, from the same commit's tree, where a row names a directory for
 *  it (`baseWebBuild`).
 *
 *  A build serves the stack's own database unless the row names one of its own, so both builds
 *  answer with one set of ids, and it signs its cookies with the stack's dev sign-in key: a
 *  server process refuses to start beside a live one whose keys it does not hold, and
 *  `e2e/run-server.sh` gives every one-process stack `DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY`, or the
 *  fixed `e2e-dev-signin` where the run names none. It gets the stack's fake Envoy, so a delivery
 *  it attempts meets the listener the stack's server meets, and none of the caller's own
 *  environment. */
import { type ChildProcess, execFile, spawn } from "node:child_process";
import { generateKeyPairSync } from "node:crypto";
import { accessSync, constants, existsSync, mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join, resolve } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import { harnessVariables, stackValue } from "./harness-ports";

/** Whether `tool` is an executable on PATH. Playwright runs this module under Node, which has no
 *  `Bun.which`. */
function onPath(tool: string): boolean {
  return (process.env.PATH ?? "").split(delimiter).some((directory) => {
    try {
      accessSync(join(directory, tool), constants.X_OK);
      return true;
    } catch {
      return false;
    }
  });
}

const execFileAsync = promisify(execFile);

const repository = fileURLToPath(new URL("../../..", import.meta.url));
const dispatchPackage = fileURLToPath(new URL("..", import.meta.url));
const envoyPackage = fileURLToPath(new URL("../../envoy", import.meta.url));

/** The dev sign-in key `e2e/run-server.sh` gives a one-process stack when the run names none. */
const harnessDevSignInKey = "e2e-dev-signin";

/** How long the base gets to say it serves and answer `/healthz`. */
const bootMs = 180_000;

/** The base build, or why this machine cannot build it. */
export type BaseBuild =
  | { readonly binary: string; readonly commit: string | undefined }
  | { readonly skip: string };

/** The commit `BASELINE_SHA` names, in full: the release this branch lands after. It has no
 *  default, as lib-baseline.sh's has none: main's merge base is older than any sibling branch this
 *  one is stacked on, so a row built on it could pass without being able to fail. */
async function baselineCommit(): Promise<string> {
  const revision = (process.env.BASELINE_SHA ?? "").trim();
  if (revision === "")
    throw new Error(
      "BASELINE_SHA is unset: set it to the commit of the release this branch lands after, " +
        "or set BASELINE_BINARY to a server already built from it; it has no default"
    );
  const { stdout } = await execFileAsync("git", [
    "-C",
    repository,
    "rev-parse",
    "--verify",
    "--quiet",
    `${revision}^{commit}`,
  ]);
  return stdout.trim();
}

/** The base build's binary: `BASELINE_BINARY`, or the commit's build, made once per run. */
export async function baseBuild(): Promise<BaseBuild> {
  const given = (process.env.BASELINE_BINARY ?? "").trim();
  if (given !== "") {
    const binary = resolve(dispatchPackage, given);
    if (!existsSync(binary))
      throw new Error(`BASELINE_BINARY names ${binary}, which does not exist`);
    return { binary, commit: undefined };
  }
  for (const tool of ["go", "git", "tar", "flock"]) {
    if (!onPath(tool)) {
      return {
        skip:
          `no ${tool} on PATH, and no BASELINE_BINARY: this row boots the build of BASELINE_SHA ` +
          "beside the stack's server, and this machine can neither build it nor was given one",
      };
    }
  }
  const commit = await baselineCommit();
  const directory = join(tmpdir(), `dispatch-e2e-base-${commit}`);
  mkdirSync(directory, { recursive: true });
  const binary = join(directory, "dispatch");
  const build = [
    "set -euo pipefail",
    'directory="$1" commit="$2" repository="$3"',
    '[ -x "$directory/dispatch" ] && exit 0',
    'rm -rf "$directory/tree" && mkdir "$directory/tree"',
    'git -C "$repository" archive "$commit" | tar -x -C "$directory/tree"',
    'go -C "$directory/tree/packages/envoy" build -ldflags "-X main.buildCommit=$commit" ' +
      '-o "$directory/dispatch.partial" ./cmd/dispatch',
    'mv "$directory/dispatch.partial" "$directory/dispatch"',
    'rm -rf "$directory/tree"',
  ].join("\n");
  await execFileAsync(
    "flock",
    [`${directory}.lock`, "bash", "-c", build, "base-build", directory, commit, repository],
    { maxBuffer: 64 * 1024 * 1024 }
  );
  return { binary, commit };
}

/** The dashboard of the commit `BASELINE_SHA` names, built in `directory`, or why this machine
 *  cannot build it. */
export type BaseWebBuild = { readonly webDist: string } | { readonly skip: string };

/** Builds the dashboard (`packages/dispatch/web/dist`) of the commit `BASELINE_SHA` names, in
 *  `directory`, once per commit: the commit's tree taken out with `git archive`, its packages
 *  installed from the lockfile it carries, and its contracts and dashboard built with its own
 *  scripts, under `flock`, with a stamp naming the commit written last, so the workers of a run
 *  build it once between them and a later run reuses it. A machine with no `git`, `tar`, `bun` or
 *  `flock` cannot build it, and the row says so. */
export async function baseWebBuild(directory: string): Promise<BaseWebBuild> {
  for (const tool of ["git", "tar", "bun", "flock"]) {
    if (!onPath(tool)) {
      return {
        skip:
          `no ${tool} on PATH: this row opens the dashboard of BASELINE_SHA beside this ` +
          "checkout's, and this machine cannot build it",
      };
    }
  }
  const commit = await baselineCommit();
  mkdirSync(directory, { recursive: true });
  const build = [
    "set -euo pipefail",
    'directory="$1" commit="$2" repository="$3"',
    '[ -f "$directory/built-$commit" ] && exit 0',
    'rm -rf "$directory/tree" "$directory"/built-* && mkdir "$directory/tree"',
    'git -C "$repository" archive "$commit" | tar -x -C "$directory/tree"',
    'cd "$directory/tree" && bun install --frozen-lockfile',
    "cd packages/dispatch && bun run build:contracts && bun run build:web",
    'touch "$directory/built-$commit"',
  ].join("\n");
  await execFileAsync(
    "flock",
    [`${directory}.lock`, "bash", "-c", build, "base-web-build", directory, commit, repository],
    { maxBuffer: 64 * 1024 * 1024 }
  );
  return { webDist: join(directory, "tree/packages/dispatch/web/dist") };
}

/** A build serving on a loopback port. */
export interface BaseServer {
  readonly url: string;
  stop(): Promise<void>;
}

/** Boots `binary` - the base build, or this checkout's own (`stackBinary`) - on a port
 *  `scripts/e2e/lib/held-port.py` holds from the kernel's choice until the server serves on it,
 *  and returns once the server has said it listens and `/healthz` answers 200. It serves
 *  `databaseUrl`, the stack's own database unless a row names one of its own, and the dashboard
 *  in `webDist`, or none: a row that reads only the API boots a server with an empty one. */
export async function bootBuild(
  binary: string,
  {
    databaseUrl = stackValue(process.env, "DATABASE_URL"),
    webDist,
  }: { readonly databaseUrl?: string; readonly webDist?: string } = {}
): Promise<BaseServer> {
  if (databaseUrl === undefined || databaseUrl.trim() === "") {
    throw new Error("the stack names no DATABASE_URL, which the build must serve");
  }
  const fakeEnvoy = stackValue(process.env, harnessVariables.fakeEnvoy);
  if (fakeEnvoy === undefined) throw new Error("the stack names no fake Envoy port");
  const home = mkdtempSync(join(tmpdir(), "dispatch-e2e-base-home-"));
  mkdirSync(join(home, "web-dist"));
  const child: ChildProcess = spawn(
    "python3",
    [
      join(repository, "scripts/e2e/lib/held-port.py"),
      "exec",
      "--fd-env",
      "DISPATCH_LISTEN_FD",
      "--env",
      "DISPATCH_PORT={port}",
      "--env",
      "DISPATCH_SERVER_URL=http://127.0.0.1:{port}",
      "--",
      binary,
    ],
    {
      cwd: home,
      env: {
        DATABASE_URL: databaseUrl,
        DISPATCH_AGENT_TOKEN: "e2e-token",
        // The fake GitHub App `e2e/run-server.sh` gives the stack's server: without an App's
        // credentials a server answers its dashboard 503 until one is configured.
        DISPATCH_APP_CLIENT_ID: "Iv1.e2efake",
        DISPATCH_APP_PEM_B64: generateKeyPairSync("rsa", {
          modulusLength: 2048,
          privateKeyEncoding: { format: "pem", type: "pkcs1" },
          publicKeyEncoding: { format: "pem", type: "spki" },
        }).privateKey,
        DISPATCH_GITHUB_API_BASE: `http://127.0.0.1:${stackValue(process.env, harnessVariables.fakeGithub) ?? ""}`,
        DISPATCH_DEV_SIGNIN: "1",
        DISPATCH_DEV_SIGNIN_SHARED_KEY:
          stackValue(process.env, "DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY") || harnessDevSignInKey,
        DISPATCH_IDENTITY: "cookie",
        DISPATCH_INSECURE_COOKIE: "1",
        DISPATCH_LISTEN_HOST: "127.0.0.1",
        DISPATCH_NATS_DISABLED: "1",
        DISPATCH_WEB_DIST: webDist ?? join(home, "web-dist"),
        ENVOY_URL: `http://127.0.0.1:${fakeEnvoy}`,
        HOME: home,
        PATH: process.env.PATH ?? "",
        TMPDIR: home,
        XDG_CACHE_HOME: home,
        XDG_CONFIG_HOME: home,
        XDG_DATA_HOME: home,
      },
      stdio: ["ignore", "pipe", "pipe"],
    }
  );
  let output = "";
  child.stdout?.on("data", (chunk: Buffer) => {
    output += chunk.toString();
  });
  child.stderr?.on("data", (chunk: Buffer) => {
    output += chunk.toString();
  });
  const exit = Promise.withResolvers<void>();
  child.once("exit", () => exit.resolve());
  const stop = async () => {
    if (child.exitCode === null && child.signalCode === null) {
      child.kill("SIGTERM");
      const killed = setTimeout(() => child.kill("SIGKILL"), 10_000);
      await exit.promise;
      clearTimeout(killed);
    }
    rmSync(home, { force: true, recursive: true });
  };
  const deadline = Date.now() + bootMs;
  try {
    while (Date.now() < deadline) {
      if (child.exitCode !== null || child.signalCode !== null) {
        throw new Error(`the build exited before it served:\n${output}`);
      }
      const listening = /msg="dispatch: listening" addr=(127\.0\.0\.1:\d+)/.exec(output);
      if (listening !== null) {
        const url = `http://${listening[1]}`;
        const health = await fetch(`${url}/healthz`).catch(() => undefined);
        if (health?.status === 200) return { stop, url };
      }
      // A server process, not a page, is booting: nothing to poll but its output and /healthz, so
      // the loop reads both four times a second until the boot deadline above.
      await sleep(250);
    }
    throw new Error(`the build did not answer /healthz within ${bootMs / 1000} s:\n${output}`);
  } catch (error) {
    await stop();
    throw error;
  }
}

/** This checkout's own `envoy-dispatch`: the binary `e2e/run-server.sh` built for the stack's
 *  server, or the one `DISPATCH_E2E_SERVER_BIN` names. */
export function stackBinary(): string {
  const given = (process.env.DISPATCH_E2E_SERVER_BIN ?? "").trim();
  return given !== "" ? resolve(dispatchPackage, given) : join(envoyPackage, "dispatch-e2e");
}
