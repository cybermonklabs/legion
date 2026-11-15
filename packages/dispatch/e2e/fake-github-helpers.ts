import { test } from "@playwright/test";

import { harnessPorts } from "./harness-ports";

interface FakeGithubRepo {
  readonly contents: string;
  readonly actions?: string;
  readonly pull_requests?: string;
  readonly installation_id: number;
  readonly files?: Record<string, string>;
  readonly pulls?: Record<
    string,
    {
      head_sha: string;
      merged?: boolean;
      state: string;
      title: string;
      body?: string;
      head_ref?: string;
      base_ref?: string;
      updated_at?: string;
    }
  >;
  readonly check_runs?: Record<
    string,
    readonly {
      app_id?: number;
      check_suite_id: number;
      completed_at?: string | null;
      conclusion: string | null;
      html_url?: string;
      id: number;
      name: string;
      output?: { title: string | null; summary: string | null };
      status: string;
    }[]
  >;
  readonly check_run_faults?: Record<string, { status: number } | { hold_ms: number }>;
  readonly workflow_runs?: Record<
    string,
    readonly { check_suite_id: number; event: string; id: number; name: string }[]
  >;
  readonly default_branch?: string;
  readonly commit_pulls?: Record<string, readonly number[]>;
  readonly branch_commits?: Record<string, readonly string[]>;
  readonly commits?: readonly string[];
  readonly installation_missing?: boolean;
}

/** Replaces the fake GitHub's installations wholesale: repositories present are
 * "App installed" with the given Contents permission; absent ones answer 404.
 * `files` (keyed by file name) populate .dispatch/architecture/, and the fake
 * derives the branch commit from their content, so a re-seed with different
 * files moves the commit exactly like a push. `pulls` (keyed by number), and
 * `check_runs` and `workflow_runs` (keyed by commit sha), answer the issue
 * header's GitHub link; `check_runs` also answers the results read of a check
 * CI proves, which takes a run's `completed_at`, `html_url` and `output` (each
 * answered as GitHub answers a run that has none when unset), and
 * `check_run_faults` (keyed by commit sha) makes that commit's check-runs read
 * answer `{status}` or wait `{hold_ms}` before answering. `commits` names the
 * commits the repository holds with nothing else attached to them, beside the
 * commits those fields imply: a result is refused `400 RESULT_COMMIT_UNKNOWN` at
 * any other 40-hex commit, so a row recording one names it here. A deployed server
 * (`PLAYWRIGHT_BASE_URL`) talks to no fake, so the test that seeds it there is
 * skipped at that point rather than failed. */
export async function seedFakeGithub(repos: Record<string, FakeGithubRepo>): Promise<void> {
  test.skip(
    Boolean(process.env.PLAYWRIGHT_BASE_URL),
    "the fake GitHub is unavailable with PLAYWRIGHT_BASE_URL"
  );
  await putFakeGithubRepos(repos);
}

/** How many times the server read each commit's check runs from the fake GitHub since the last
 * seed, keyed by "owner/repo" and then by commit sha, from `GET /__fixture/requests`, whose
 * answer is `{check_runs: {"owner/repo": {"<sha>": <count>}}}`. Each page is one read, a
 * faulted read is counted as well, and every seed (`seedFakeGithub`, `resetFakeGithub`) starts
 * the counts again from none. Only a row that seeded the fake reads it. */
export async function fakeGithubCheckRunReads(): Promise<Record<string, Record<string, number>>> {
  const response = await fetch(
    `http://127.0.0.1:${harnessPorts().fakeGithub.port}/__fixture/requests`
  );
  if (!response.ok) {
    throw new Error(`read fake github requests: ${response.status}`);
  }
  const answer = (await response.json()) as {
    check_runs: Record<string, Record<string, number>>;
  };
  return answer.check_runs;
}

/** Makes the local delivery reconcile succeed against an empty acme/widgets history, so a pass that
 * runs inside a test leaves the freshness row Live rather than red with the reconcile's error. Only
 * a test that asserts the row says Live needs it. Its opposite is delivery.e2e.ts's "the freshness
 * row shows a named reconcile failure" row, which seeds the same repository without `actions` from
 * its own body, so that every pass inside its window fails with the one error it asserts. A
 * deployed server has its own GitHub and is left as it is. */
export async function seedDeliveryReconcileGithub(): Promise<void> {
  if (process.env.PLAYWRIGHT_BASE_URL) return;
  await putFakeGithubRepos({
    "acme/widgets": {
      actions: "read",
      contents: "read",
      installation_id: 690,
      pull_requests: "read",
    },
  });
}

/** Clears every fake GitHub repository before the next scenario. A deployed run has no fake. */
export async function resetFakeGithub(): Promise<void> {
  if (process.env.PLAYWRIGHT_BASE_URL) return;
  await putFakeGithubRepos({});
}

async function putFakeGithubRepos(repos: Record<string, FakeGithubRepo>): Promise<void> {
  const response = await fetch(
    `http://127.0.0.1:${harnessPorts().fakeGithub.port}/__fixture/repos`,
    {
      body: JSON.stringify(repos),
      headers: { "Content-Type": "application/json" },
      method: "PUT",
    }
  );
  if (!response.ok) {
    throw new Error(`seed fake github: ${response.status}`);
  }
}
