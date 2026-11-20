// packages/broker-client/src/github-actions.ts

import { readFile } from "node:fs/promises";
import type { SubjectToken, TokenSource } from "./source";
import { SUBJECT_TOKEN_TYPE_GITHUB } from "./wire";

/** The runner's OIDC token endpoint, which GitHub sets in a job granted `id-token: write`. */
export const ACTIONS_TOKEN_URL_VAR = "ACTIONS_ID_TOKEN_REQUEST_URL";
/** The bearer that endpoint takes, which GitHub sets beside it. */
export const ACTIONS_TOKEN_VAR = "ACTIONS_ID_TOKEN_REQUEST_TOKEN";

/** Bounds one token fetch, as the metadata reader bounds its own. */
const TIMEOUT_MS = 10_000;

/** The runner's endpoint, the bearer it takes, and the audience the minted token carries. */
export interface GitHubActionsIdentityOptions {
  readonly requestUrl: string;
  readonly requestToken: string;
  readonly audience: string;
  /** The fetch every read goes through; the global one by default. */
  readonly fetchImpl?: typeof fetch | undefined;
}

/**
 * The identity of a GitHub Actions job: the OIDC token its runner mints for `audience`, asked of
 * the runner's endpoint with the bearer the runner gave the job, and presented as a workflow
 * run's token. The broker takes such a token once, so every read asks the runner for a new one.
 * No refusal names the bearer or the token, and nothing here reads a claim: that is the broker's
 * work. The Go client's `GitHubActionsIdentity` reads the same endpoint the same way.
 */
export function githubActionsIdentity(options: GitHubActionsIdentityOptions): TokenSource {
  const fetchImpl = options.fetchImpl ?? fetch;
  const where = `the runner's OIDC token endpoint (${ACTIONS_TOKEN_URL_VAR})`;
  return {
    async read(signal?: AbortSignal): Promise<SubjectToken> {
      // The runner's URL carries a query of its own, so the audience joins it. A value that is no
      // URL is refused by its variable, as every other refusal here is, and never quoted.
      let url: URL;
      try {
        url = new URL(options.requestUrl);
      } catch {
        throw new Error(`${where} is no URL`);
      }
      url.searchParams.set("audience", options.audience);
      const signals = [AbortSignal.timeout(TIMEOUT_MS), ...(signal === undefined ? [] : [signal])];
      let answer: Response;
      try {
        // No redirect is followed: fetch would send the runner's bearer on to a target on the
        // runner's own origin, and take a token another origin minted. A 3xx is then the
        // endpoint's refusal, named by its status as every other one is, its body unread.
        answer = await fetchImpl(url, {
          redirect: "manual",
          headers: { Authorization: `Bearer ${options.requestToken}`, Accept: "application/json" },
          signal: AbortSignal.any(signals),
        });
      } catch (error) {
        throw new Error(`${where} could not be reached: ${(error as Error).message}`);
      }
      if (!answer.ok) throw new Error(`${where} answered HTTP ${answer.status}`);
      const body: unknown = await answer.json().catch(() => undefined);
      const value =
        typeof body === "object" && body !== null && "value" in body ? body.value : undefined;
      const token = typeof value === "string" ? value.trim() : "";
      if (token === "") throw new Error(`${where} answered no token`);
      return { token, tokenType: SUBJECT_TOKEN_TYPE_GITHUB };
    },
    describe: () =>
      `this GitHub Actions job's OIDC token (${ACTIONS_TOKEN_URL_VAR} names the endpoint)`,
  };
}

/**
 * A workflow run's OIDC token held in the file at `path`, for a job that reads its token some
 * other way than the runner's endpoint. It is read at every trade, since the broker takes one
 * token once, and an empty file is refused rather than presented.
 */
export function workflowRunTokenFile(path: string): TokenSource {
  return {
    async read(signal?: AbortSignal): Promise<SubjectToken> {
      let contents: string;
      try {
        contents = await readFile(path, {
          encoding: "utf8",
          ...(signal === undefined ? {} : { signal }),
        });
      } catch (error) {
        throw new Error(`read the workflow run's token ${path}: ${(error as Error).message}`);
      }
      const token = contents.trim();
      if (token === "") throw new Error(`the workflow run's token ${path} is empty`);
      return { token, tokenType: SUBJECT_TOKEN_TYPE_GITHUB };
    },
    describe: () => `the workflow run's token ${path}`,
  };
}
