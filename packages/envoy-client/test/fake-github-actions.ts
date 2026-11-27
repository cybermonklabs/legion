import { createHash, generateKeyPairSync, type KeyObject, sign } from "node:crypto";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";

// The stand-in of GitHub that a CI job's result path meets twice: once as the issuer of the
// job's OIDC token, which the secrets broker verifies (BROKER_EXCHANGE_GITHUB_ISSUER), and once
// as the GitHub API, which Dispatch reads the job's check run and the spec files at the run's
// commit from (DISPATCH_GITHUB_API_BASE). It serves:
//
//   - the OIDC discovery document and the key set of its test issuer;
//   - the Actions token endpoint a runner gives a job granted `id-token: write`
//     (ACTIONS_ID_TOKEN_REQUEST_URL): GET /_actions/token?audience=…, under the bearer
//     ACTIONS_ID_TOKEN_REQUEST_TOKEN names, which mints the claims of the run that bearer was
//     added for, as GitHub mints a run's own;
//   - GET /repos/{owner}/{repo}/check-runs/{id}, the one check run Dispatch reads by its id;
//   - GET /repos/{owner}/{repo}/git/trees/{sha}:{dir} and GET .../git/blobs/{sha}, the reads of
//     githubapp.Client.Dir and DirFiles;
//   - GET /repos/{owner}/{repo}/contents/{path}?ref={sha}, the read of githubapp.Client.File, by
//     which Dispatch reads the run's workflow file to know which job makes the run's check run;
//   - the App installation of a repository and the installation token Dispatch mints there.
//
// Every route answers what this process set and nothing else, as GitHub's 404 for the rest.

/** The `iss` of a token GitHub's own hosts mint for an Actions run. */
export const GITHUB_ACTIONS_ISSUER = "https://token.actions.githubusercontent.com";

/** The key id both of the stand-in's signing keys carry, so a forged token differs from a real
 *  one in its signature alone. */
const KEY_ID = "actions-local-key";

/** How long a run's token lasts, as GitHub's own last a few minutes. */
const TOKEN_SECONDS = 300;

/** One job of one workflow run, as the claims of GitHub's token for it describe it. */
export interface ActionsRun {
  /** `<owner>/<name>`, the repository the run belongs to. */
  readonly repository: string;
  /** The workflow file's path in the repository, as `.github/workflows/<name>.yaml`. */
  readonly workflow: string;
  /** The ref the run was pushed to, as `refs/heads/<branch>`. */
  readonly ref: string;
  /** The commit the run tested, 40 lowercase hex characters. */
  readonly sha: string;
  readonly runId: number;
  readonly runAttempt: number;
  /** The check run of the job, which GitHub names after the job. */
  readonly checkRunId: number;
  /** The check run's name: the job's name, then its matrix values in parentheses. */
  readonly checkRunName: string;
  /** Whether each token of the run is signed by a key the issuer's key set does not hold. */
  readonly forged?: boolean;
}

/** The two variables a runner sets in a job granted `id-token: write`. */
export interface ActionsTokenRequest {
  /** ACTIONS_ID_TOKEN_REQUEST_URL. It carries a query of its own, as GitHub's does. */
  readonly requestUrl: string;
  /** ACTIONS_ID_TOKEN_REQUEST_TOKEN. */
  readonly requestToken: string;
}

/** The stand-in, once it serves. */
export interface FakeGitHubActions {
  /** Its base URL, which is both the issuer and the API base. */
  readonly url: string;
  /** The run's token endpoint, and the check run Dispatch reads for it. */
  addRun(run: ActionsRun): ActionsTokenRequest;
  /** The files of `dir` at `sha`: the whole directory, by file name. */
  setFiles(
    repository: string,
    sha: string,
    dir: string,
    files: Readonly<Record<string, string>>
  ): void;
  /** The one file at `path` at `sha`, as the contents read answers it. */
  setFile(repository: string, sha: string, path: string, content: string): void;
  stop(): Promise<void>;
}

/** What a caller may change about the stand-in. */
export interface FakeGitHubActionsOptions {
  /** The `iss` of each token it mints; its own URL by default. */
  readonly issuer?: string;
  /** The repositories the App is installed on, by `<owner>/<name>`, each with its installation. */
  readonly installations?: Readonly<Record<string, number>>;
}

/** A JWT signed with RS256 by `key`, whose header names KEY_ID. */
function signJwt(key: KeyObject, claims: Readonly<Record<string, unknown>>): string {
  const input = [{ alg: "RS256", typ: "JWT", kid: KEY_ID }, claims]
    .map((part) => Buffer.from(JSON.stringify(part)).toString("base64url"))
    .join(".");
  return `${input}.${sign("sha256", Buffer.from(input), key).toString("base64url")}`;
}

/** GitHub's answer for a resource it does not have. */
function notFound(what: string): Response {
  return Response.json({ message: `Not Found: ${what}` }, { status: 404 });
}

/** Starts the stand-in on a loopback port the system picks; it serves once this returns. */
export function startFakeGitHubActions(options: FakeGitHubActionsOptions = {}): FakeGitHubActions {
  const published = generateKeyPairSync("rsa", { modulusLength: 2048 });
  // A key of the same kind and the same key id, which the key set never names: what it signs
  // reads as a run's token until the broker checks the signature.
  const forger = generateKeyPairSync("rsa", { modulusLength: 2048 });
  const installations = new Map(Object.entries(options.installations ?? {}));
  const installationTokens = new Map(
    [...installations.values()].map((id) => [id, fixtureSecret("install")])
  );
  const runs = new Map<string, ActionsRun>();
  const checkRuns = new Map<string, ActionsRun>();
  // "<owner>/<name>@<sha>:<dir>" → file name → blob id, and blob id → content.
  const trees = new Map<string, Map<string, string>>();
  const blobs = new Map<string, string>();
  // "<owner>/<name>@<sha>:<path>" → content, for the contents read.
  const contents = new Map<string, string>();

  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request): Response {
      const url = new URL(request.url);
      const base = url.origin;
      const issuer = options.issuer ?? base;

      if (request.method === "GET" && url.pathname === "/.well-known/openid-configuration") {
        return Response.json({
          issuer,
          jwks_uri: `${base}/keys`,
          response_types_supported: ["id_token"],
          subject_types_supported: ["public"],
          id_token_signing_alg_values_supported: ["RS256"],
        });
      }
      if (request.method === "GET" && url.pathname === "/keys") {
        return Response.json({
          keys: [
            {
              ...published.publicKey.export({ format: "jwk" }),
              kid: KEY_ID,
              alg: "RS256",
              use: "sig",
            },
          ],
        });
      }

      if (request.method === "GET" && url.pathname === "/_actions/token") {
        const bearer = request.headers.get("Authorization")?.replace(/^Bearer /i, "") ?? "";
        const run = runs.get(bearer);
        if (run === undefined) {
          return Response.json({ message: "Bad credentials" }, { status: 401 });
        }
        const now = Math.floor(Date.now() / 1000);
        const owner = run.repository.split("/")[0] ?? "";
        const workflowRef = `${run.repository}/${run.workflow}@${run.ref}`;
        const claims = {
          iss: issuer,
          aud: url.searchParams.get("audience") ?? `https://github.com/${owner}`,
          sub: `repo:${run.repository}:ref:${run.ref}`,
          iat: now,
          nbf: now,
          exp: now + TOKEN_SECONDS,
          // A fresh one for each request, as GitHub mints one: the broker spends each once.
          jti: crypto.randomUUID(),
          repository: run.repository,
          repository_owner: owner,
          workflow_ref: workflowRef,
          // A job written directly in its workflow, as each job here is, gets job_workflow_ref
          // equal to workflow_ref (measured on real runs on 2026-11-16), though GitHub's OIDC
          // reference describes the claim for a reusable workflow. The broker refuses a token
          // without it.
          job_workflow_ref: workflowRef,
          ref: run.ref,
          ref_type: "branch",
          sha: run.sha,
          event_name: "push",
          run_id: String(run.runId),
          run_attempt: String(run.runAttempt),
          runner_environment: "github-hosted",
          check_run_id: String(run.checkRunId),
        };
        const key = run.forged === true ? forger.privateKey : published.privateKey;
        return Response.json({ count: 1, value: signJwt(key, claims) });
      }

      const installation = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/installation$/);
      if (request.method === "GET" && installation !== null) {
        const repository = `${installation[1]}/${installation[2]}`;
        const id = installations.get(repository);
        if (id === undefined) return notFound(`the App is not installed on ${repository}`);
        return Response.json({
          id,
          app_slug: "dispatch-local",
          account: { login: installation[1] },
          permissions: { checks: "read", contents: "read" },
        });
      }
      const mint = url.pathname.match(/^\/app\/installations\/(\d+)\/access_tokens$/);
      if (request.method === "POST" && mint !== null) {
        return Response.json(
          {
            token: installationTokens.get(Number(mint[1])),
            expires_at: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
          },
          { status: 201 }
        );
      }

      // Every read of a repository's contents and checks takes an installation token.
      const repositoryRead = url.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/(.+)$/);
      if (request.method === "GET" && repositoryRead !== null) {
        const repository = `${repositoryRead[1]}/${repositoryRead[2]}`;
        const rest = repositoryRead[3] ?? "";
        const id = installations.get(repository);
        if (id === undefined) return notFound(repository);
        if (request.headers.get("Authorization") !== `Bearer ${installationTokens.get(id)}`) {
          return Response.json({ message: "Requires authentication" }, { status: 401 });
        }

        const checkRun = rest.match(/^check-runs\/(\d+)$/);
        if (checkRun !== null) {
          const run = checkRuns.get(`${repository}#${checkRun[1]}`);
          if (run === undefined) return notFound(`check run ${checkRun[1]}`);
          return Response.json({
            id: run.checkRunId,
            name: run.checkRunName,
            head_sha: run.sha,
            status: "in_progress",
            app: { slug: "github-actions" },
          });
        }

        // `git/trees/{sha}:{dir}`, the ":" kept and the "/" of the directory percent-encoded, as
        // the Go client escapes it.
        const tree = rest.match(/^git\/trees\/([^/]+)$/);
        if (tree !== null) {
          const [sha = "", dir = ""] = decodeURIComponent(tree[1] ?? "").split(":", 2);
          const files = trees.get(`${repository}@${sha}:${dir}`);
          if (files === undefined) return notFound(`${dir} at ${sha}`);
          return Response.json({
            sha: createHash("sha1").update(`${repository}@${sha}:${dir}`).digest("hex"),
            truncated: false,
            tree: [...files].map(([name, blob]) => ({
              path: name,
              mode: "100644",
              type: "blob",
              sha: blob,
              size: Buffer.byteLength(blobs.get(blob) ?? ""),
            })),
          });
        }
        const blob = rest.match(/^git\/blobs\/([0-9a-f]{40})$/);
        if (blob !== null) {
          const content = blobs.get(blob[1] ?? "");
          if (content === undefined) return notFound(`blob ${blob[1]}`);
          return Response.json({
            content: Buffer.from(content).toString("base64"),
            encoding: "base64",
          });
        }
        // `contents/{path}?ref={sha}`, each part of the path percent-encoded, as the Go client
        // escapes it; GitHub's 404 both for a path the commit does not hold and for no such ref.
        const file = rest.match(/^contents\/(.+)$/);
        if (file !== null) {
          const path = (file[1] ?? "").split("/").map(decodeURIComponent).join("/");
          const sha = url.searchParams.get("ref") ?? "";
          const content = contents.get(`${repository}@${sha}:${path}`);
          if (content === undefined) return notFound(`${path} at ${sha}`);
          return Response.json({
            type: "file",
            size: Buffer.byteLength(content),
            encoding: "base64",
            content: Buffer.from(content).toString("base64"),
          });
        }
      }

      return notFound(`${request.method} ${url.pathname}`);
    },
  });
  const url = `http://127.0.0.1:${server.port}`;

  return {
    url,
    addRun(run: ActionsRun): ActionsTokenRequest {
      const requestToken = fixtureSecret("actions");
      runs.set(requestToken, run);
      checkRuns.set(`${run.repository}#${run.checkRunId}`, run);
      return { requestUrl: `${url}/_actions/token?api-version=2.0`, requestToken };
    },
    setFiles(repository, sha, dir, files): void {
      const listing = new Map<string, string>();
      for (const [name, content] of Object.entries(files)) {
        const id = createHash("sha1").update(content).digest("hex");
        blobs.set(id, content);
        listing.set(name, id);
      }
      trees.set(`${repository}@${sha}:${dir}`, listing);
    },
    setFile(repository, sha, path, content): void {
      contents.set(`${repository}@${sha}:${path}`, content);
    },
    async stop(): Promise<void> {
      await server.stop(true);
    },
  };
}
