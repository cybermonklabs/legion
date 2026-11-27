// The two code cards of the Evals tab (dispatch://INFRA-3206/artifact/plan-md, T10; spec R7 to
// R11 and R14; checks k6, k9, k11 and k16), on the real stack. Two projects, CODEA and CODEB,
// share one default repository and so one seeded branch: CODEA reads its deploys from a GitHub
// workflow and CODEB from posted events, since two github_workflow sources cannot read one
// repository (DELIVERY_REPOSITORY_SHARED). The seed writes the code survival run's stored results
// (code_repos, code_days, code_changes, code_pairs) in the schema the migrations make, then each
// project's delivery settings with its code groups; that write notifies
// dispatch_delivery_settings, which wakes the server's regroup (codesurvival.Regroup), and the
// regroup builds both evals' day rows from the results with no GitHub request. Every window starts
// on the first measured day, since a window before a series' first day reads "no data".
//
// The seed's counts, which the rows read back. Six changes in acme/widgets, measured on the ten
// days 2026-09-01 to 09-10:
//
//   c1  09-02  k9seed/first    octo-dev          no issue  100 added, 80 alive  (prefix alone)
//   c2  09-03  feature/second  k9-seed-app[bot]  no issue   50 added, 30 alive  (login alone)
//   c3  09-04  k9seed/third    octo-dev          CODEA-1    40 added, 36 alive
//   c4  09-05  people/fourth   octo-person       CODEB-1    60 added, 45 alive
//   c5  09-06  people/fifth    octo-person       no issue   20 added, 10 alive
//   c6  09-08  k9seed/sixth    octo-dev          CODEA-2    10 added, 10 alive
//
// and one pair: c6 removed 4 lines of c3 four days after it, a change of the same group for a
// different issue, so rework. A change with no issue counts for both projects, and a change of
// an issue for that issue's project alone (R11). CODEA names one group, `agents`, whose only
// prefix is `k9seed/` and whose only login is `k9-seed-app[bot]`: c1, c2, c3 and c6, 4 changes,
// 156 of 200 lines alive (78.0%), 4 of them removed as rework (2.0%), c1 and c2 with no issue.
// CODEB names `bots`, the login alone, and a last group with no rule, `rest`: bots holds c2
// (30 of 50, 60.0%), rest c1, c4 and c5 (135 of 180, 75.0%), and c1, c2 and c5 have no issue.
// The window, 2026-09-01 to 09-15, holds five days after the newest measured day.
//
// acme-shaped placeholders throughout (AGENTS.md: "This repository is public").

import type {
  CodeGroup,
  EvalItemsResponse,
  EvalListItem,
  ProjectDeploySourceInput,
} from "../web/src/api/types";
import {
  createProject,
  getProjectDeliverySettings,
  listProjectEvals,
  putProjectDefaultRepository,
  putProjectDeliverySettings,
} from "./api";
import { SEED_ACTOR } from "./delivery-seed";
import { saveDeliverySettings } from "./delivery-settings-steps";
import { seedFakeGithub } from "./fake-github-helpers";
import { sql } from "./psql";
import { sqlLiteral } from "./seed";
import { expect, type Locator, type Page, test } from "./test";

const SURVIVAL = "code-survival-30d";
const REWORK = "post-merge-rework-30d";

/** The repository both projects name as their default, the branch the seed's results are of. */
const REPO = "acme/widgets";

/** The window every row reads: the first measured day to the fifth day after the last. */
const WINDOW = { from: "2026-09-01T00:00:00Z", to: "2026-09-16T00:00:00Z" } as const;
const WINDOW_QUERY = `from=${encodeURIComponent(WINDOW.from)}&to=${encodeURIComponent(WINDOW.to)}`;

/** How long a row waits for the regroup a settings write wakes to build the day rows: one pass
 *  at most two seconds after the last (regroupGap), a rebuild and the drain. */
const REGROUP_MS = 30_000;

/** A seed commit's 40-hex SHA: `pair` twenty times. No repository holds it. */
function sha(pair: string): string {
  return pair.repeat(20);
}

/** One stored change of acme/widgets (code_changes). */
interface SeedChange {
  readonly sha: string;
  readonly day: string;
  readonly pr: number;
  readonly headRef: string;
  readonly author: string;
  readonly issue: string | null;
  readonly added: number;
  readonly alive: number;
}

const CHANGES: readonly SeedChange[] = [
  {
    added: 100,
    alive: 80,
    author: "octo-dev",
    day: "2026-09-02",
    headRef: "k9seed/first",
    issue: null,
    pr: 101,
    sha: sha("c1"),
  },
  {
    added: 50,
    alive: 30,
    author: "k9-seed-app[bot]",
    day: "2026-09-03",
    headRef: "feature/second",
    issue: null,
    pr: 102,
    sha: sha("c2"),
  },
  {
    added: 40,
    alive: 36,
    author: "octo-dev",
    day: "2026-09-04",
    headRef: "k9seed/third",
    issue: "CODEA-1",
    pr: 103,
    sha: sha("c3"),
  },
  {
    added: 60,
    alive: 45,
    author: "octo-person",
    day: "2026-09-05",
    headRef: "people/fourth",
    issue: "CODEB-1",
    pr: 104,
    sha: sha("c4"),
  },
  {
    added: 20,
    alive: 10,
    author: "octo-person",
    day: "2026-09-06",
    headRef: "people/fifth",
    issue: null,
    pr: 105,
    sha: sha("c5"),
  },
  {
    added: 10,
    alive: 10,
    author: "octo-dev",
    day: "2026-09-08",
    headRef: "k9seed/sixth",
    issue: "CODEA-2",
    pr: 106,
    sha: sha("c6"),
  },
];

/** The ten measured days, 2026-09-01 to 09-10. */
const MEASURED_DAYS = Array.from(
  { length: 10 },
  (_, index) => `2026-09-${String(index + 1).padStart(2, "0")}`
);

/** The commit whose patch GitHub gave neither whole nor through files[], blocking a day. */
const BLOCKED_SHA = sha("b7");

/** The state the seed gives a day of acme/widgets: measured, or marked after a force-push, or
 *  blocked by `BLOCKED_SHA`, which withheld three files. */
type DayState = "measured" | "marked" | "blocked";

/** The statements that write acme/widgets' stored results: the repository as the run reads it,
 *  each of `days` in its state, and the changes. */
function resultsSql(
  days: readonly string[],
  states: Readonly<Record<string, DayState>> = {},
  changes: readonly SeedChange[] = CHANGES
): string[] {
  const dayRow = (day: string) => {
    const state = states[day] ?? "measured";
    const digest = `decode(repeat(${sqlLiteral(day.slice(-2))}, 32), 'hex')`;
    return state === "blocked"
      ? `('${REPO}', '${day}', 6, ${digest}, 'blocked', null, null, '${BLOCKED_SHA}', 3,
          'GitHub refused the diff, and files[] withheld the patches of 3 files', '2026-10-20T00:00:00Z')`
      : `('${REPO}', '${day}', 6, ${digest}, '${state}', '2026-10-15T00:00:00Z',
          ${state === "marked" ? "'2026-10-18T00:00:00Z'" : "null"}, null, null, null, '2026-10-15T00:00:00Z')`;
  };
  return [
    `INSERT INTO code_repos (repo, branch, start_day, state, checked_on)
     VALUES ('${REPO}', 'main', '2026-09-01', 'readable', current_date)
     ON CONFLICT (repo) DO NOTHING`,
    `INSERT INTO code_days (repo, day, commits, digest, state, measured_at, marked_at, blocked_sha,
       blocked_files, blocked_reason, tried_at) VALUES ${days.map(dayRow).join(", ")}`,
    `INSERT INTO code_changes (repo, sha, day, committed_at, pr_number, head_ref, author, issue_key,
       added, alive, removed_by_non_pr) VALUES ${changes
         .map(
           (change) =>
             `('${REPO}', '${change.sha}', '${change.day}', '${change.day}T12:00:00Z', ${change.pr},
               ${sqlLiteral(change.headRef)}, ${sqlLiteral(change.author)},
               ${change.issue === null ? "null" : sqlLiteral(change.issue)}, ${change.added}, ${change.alive}, 0)`
         )
         .join(", ")}`,
  ];
}

/** The statement that writes the seed's one pair: c6 removed 4 lines of c3 four days after it, a
 *  change of the same group for a different issue. */
const PAIR_SQL = `INSERT INTO code_pairs (repo, adder_sha, remover_sha, remover_pr, remover_head_ref,
    remover_author, remover_issue_key, lines, days_between)
  VALUES ('${REPO}', '${sha("c3")}', '${sha("c6")}', 106, 'k9seed/sixth', 'octo-dev', 'CODEA-2', 4, 4)`;

/** CODEA's one group: the k9 seed's prefix and its login. */
const CODEA_GROUPS: CodeGroup[] = [
  { author_logins: ["k9-seed-app[bot]"], branch_prefixes: ["k9seed/"], name: "agents" },
];

/** CODEB's groups: the login alone, then a last group with no rule. */
const CODEB_GROUPS: CodeGroup[] = [
  { author_logins: ["k9-seed-app[bot]"], branch_prefixes: [], name: "bots" },
  { author_logins: [], branch_prefixes: [], name: "rest" },
];

/** CODEA's one deploy source, a GitHub workflow reading its default repository, as a PUT names
 *  it. */
const CODEA_SOURCE: ProjectDeploySourceInput = {
  deploy_workflow_path: ".github/workflows/deploy.yml",
  kind: "github_workflow",
  pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
  production_job_names: ["widgets-release"],
  repo: null,
};

/** CODEB's one deploy source: the production events posted to Dispatch. */
const EVENTS_SOURCE: ProjectDeploySourceInput = { kind: "events" };

/** `value` as an SQL literal, null where it holds none. */
function sqlText(value: string | null | undefined): string {
  return value === undefined || value === null ? "null" : sqlLiteral(value);
}

/** The statements that give `project` its delivery settings with delivery on, its code groups and
 *  its one deploy source, `source`, each column as `source` names it. The settings row's insert
 *  notifies dispatch_delivery_settings, which wakes the regroup. */
function settingsSql(
  project: string,
  groups: readonly CodeGroup[],
  source: ProjectDeploySourceInput
): string[] {
  const jobs = source.production_job_names ?? null;
  return [
    `INSERT INTO project_delivery_settings (project_key, delivery, population_authors,
       excluded_repos, code_groups, updated_by)
     VALUES ('${project}', true, ARRAY[]::text[], ARRAY[]::text[],
       ${sqlLiteral(JSON.stringify(groups))}::jsonb, ${SEED_ACTOR})`,
    `INSERT INTO project_deploy_sources (project_key, position, kind, repo, deploy_workflow_path,
       production_job_names, pr_checks_workflow_path)
     VALUES ('${project}', 0, '${source.kind}', ${sqlText(source.repo)},
       ${sqlText(source.deploy_workflow_path)},
       ${jobs === null ? "null" : `ARRAY[${jobs.map(sqlLiteral).join(", ")}]`},
       ${sqlText(source.pr_checks_workflow_path)})`,
  ];
}

/** The code survival eval of `project` over the window, as the list read answers it. */
async function codeSurvival(project: string): Promise<EvalListItem | undefined> {
  const answer = await listProjectEvals(project, {
    from: WINDOW.from,
    kind: "delivery",
    limit: 200,
    to: WINDOW.to,
  });
  return answer.items?.find((item) => item.definition.slug === SURVIVAL);
}

/** A statistic of one series of `item` over the window; undefined where it holds no such series. */
function statistic(
  item: EvalListItem | undefined,
  series: string,
  name: string
): number | null | undefined {
  const found = item?.series?.find((each) => each.series === series);
  return found === undefined ? undefined : (found.window?.[name] ?? null);
}

/** Seeds the stack: the App reading acme/widgets, both projects made through the API (so their
 *  evals are seeded) with it as their default repository, the issues of the changes, the results,
 *  and then each project's settings; and waits until the regroup has built each project's rows,
 *  read back as each project's first group holding its count of changes. `states` gives a day
 *  other than measured. */
async function seed(states: Readonly<Record<string, DayState>> = {}): Promise<void> {
  await seedFakeGithub({ [REPO]: { contents: "read", installation_id: 971 } });
  for (const [key, name] of [
    ["CODEA", "Code A"],
    ["CODEB", "Code B"],
  ] as const) {
    await createProject({ key, name });
    await putProjectDefaultRepository(key, REPO);
  }
  await sql(
    `INSERT INTO issues (key, project_key, number, title, status, priority, created_by, rank) VALUES
       ('CODEA-1', 'CODEA', 1, 'Parse the widget feed', 'todo', 2, ${SEED_ACTOR}, 'U'),
       ('CODEA-2', 'CODEA', 2, 'Cache the widget feed', 'todo', 2, ${SEED_ACTOR}, 'V'),
       ('CODEB-1', 'CODEB', 1, 'Draw the gadgets', 'todo', 2, ${SEED_ACTOR}, 'U')`,
    ...resultsSql(MEASURED_DAYS, states),
    PAIR_SQL,
    ...settingsSql("CODEA", CODEA_GROUPS, CODEA_SOURCE),
    ...settingsSql("CODEB", CODEB_GROUPS, EVENTS_SOURCE)
  );
  const built = Object.keys(states).length === 0;
  await expect
    .poll(
      async () => {
        const [a, b] = [await codeSurvival("CODEA"), await codeSurvival("CODEB")];
        return built
          ? [statistic(a, "survival:agents", "changes"), statistic(b, "survival:bots", "changes")]
          : [statistic(a, "status", "marked_days"), statistic(b, "status", "marked_days")];
      },
      { message: "the regroup builds both projects' day rows", timeout: REGROUP_MS }
    )
    .toEqual(built ? [4, 1] : [1, 1]);
}

/** `project`'s Evals tab over the window, once its header has drawn. */
async function openTab(page: Page, project: string): Promise<void> {
  await page.goto(`/projects/${project}/evals?${WINDOW_QUERY}`);
  await expect(page.getByRole("tab", { name: "Evals" })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByRole("group", { name: "Window and filters" })).toBeVisible();
}

/** The code card of `slug` in Deploys and lead time. */
function card(page: Page, slug: string): Locator {
  return page
    .getByRole("region", { name: "Deploys and lead time" })
    .locator(`[data-code-card="${slug}"]`);
}

/** The headline share and the counts the card of `slug` draws for `group`. */
function group(page: Page, slug: string, name: string) {
  const line = card(page, slug).locator(`[data-group="${name}"]`);
  return { counts: line.locator("[data-counts]"), share: line.locator("[data-share]") };
}

/** What each card's face and method text must say (spec R14; k11): the reading and the diff on
 *  the face, and in the method what the number is not, in the method document's words; the
 *  rework card's, how it differs from the title-match number and that the 8.2 per cent is
 *  another reading. */
const NOT_STATEMENTS = [
  "It is the replay's reading, not blame.",
  "It reads replacement and never whether the work was right.",
  "An edited line is a dead line.",
  "The replay reads 1.2 points above blame for Legion and 4.8 for the other agent at 30 days, and the gap is uneven.",
  "The diff is GitHub's, with three lines of context, no whitespace rule and no copy detection.",
] as const;

const REWORK_STATEMENTS = [
  "**How it differs from Rework share.** Rework share counts pull requests whose title starts with fix, revert or hotfix, over the merged pull requests.",
  "The 8.2 per cent of dispatch://INFRA-3117/artifact/analysis-md is read at the tip with no age limit, over Legion's merges of any age, and is not this number.",
] as const;

const CARDS = [
  { slug: SURVIVAL, statements: NOT_STATEMENTS, title: "Code survival at 30 days" },
  {
    slug: REWORK,
    statements: [...NOT_STATEMENTS, ...REWORK_STATEMENTS],
    title: "Post-merge rework at 30 days",
  },
] as const;

// SLOW(>=2s): each row makes two projects through the API, writes the code results and the
// settings, waits for the regroup a settings write wakes to build both projects' day rows, and
// opens the tab.
test.describe("the code cards", () => {
  test("both cards are in Deploys and lead time beside Rework share", async ({ page }) => {
    await seed();
    await openTab(page, "CODEA");
    const section = page.getByRole("region", { name: "Deploys and lead time" });
    for (const { slug, title } of CARDS) {
      await expect(card(page, slug)).toBeVisible();
      await expect(card(page, slug)).toHaveAttribute("aria-label", title);
    }
    // The title-match number still shows, under its own name, and the two cards follow the
    // section's charts it is one of.
    await expect(section.getByRole("article", { name: "Rework share" })).toBeVisible();
    await expect(section.getByRole("article", { name: "Rework share" })).toHaveAttribute(
      "data-eval",
      "rework-share"
    );
    const order = await section
      .locator("[data-eval]")
      .evaluateAll((drawn) => drawn.map((each) => each.getAttribute("data-eval")));
    expect(order.indexOf("rework-share")).toBeGreaterThanOrEqual(0);
    expect(order.indexOf(SURVIVAL)).toBeGreaterThan(order.indexOf("rework-share"));
    expect(order.indexOf(REWORK)).toBe(order.indexOf(SURVIVAL) + 1);
    // Nowhere else on the tab.
    await expect(page.locator("[data-code-card]")).toHaveCount(2);
  });

  test("each card face names the replay reading and the GitHub diff", async ({ page }) => {
    await seed();
    await openTab(page, "CODEA");
    for (const { slug } of CARDS) {
      // Read on the face, beside the value, with no method or panel opened.
      await expect(group(page, slug, "agents").share).toBeVisible();
      await expect(card(page, slug).locator("[data-reading]")).toHaveText(
        "replay reading, not blame \u00b7 GitHub diff"
      );
    }
    await expect(page.getByRole("dialog")).toHaveCount(0);
  });

  test("each method text states what the number is not", async ({ page }) => {
    await seed();
    await openTab(page, "CODEA");
    for (const { slug, statements, title } of CARDS) {
      await card(page, slug).getByRole("button", { name: title }).click();
      const panel = page.getByRole("dialog", { name: title });
      const method = panel.locator("[data-method-text]");
      await expect(method).toBeVisible();
      const text = (await method.textContent()) ?? "";
      const found = statements.filter((statement) => text.includes(statement));
      test.info().annotations.push({
        description: `${found.length} of ${statements.length}`,
        type: `${slug} statements`,
      });
      expect(found).toEqual([...statements]);
      await panel.getByRole("button", { name: "Close the panel" }).click();
      await expect(panel).toHaveCount(0);
    }
  });

  test("each repository note counts the changes with no issue", async ({ page }) => {
    await seed();
    for (const [project, count] of [
      ["CODEA", 2],
      ["CODEB", 3],
    ] as const) {
      await openTab(page, project);
      // The items rows of the same window: the survival eval's changes, each with whether it
      // reached the project through its repository with no issue.
      const response = await page.request.get(
        `/api/v1/projects/${project}/evals/${SURVIVAL}/items?${WINDOW_QUERY}&row_kind=change&limit=200`
      );
      expect(response.status()).toBe(200);
      const body = (await response.json()) as EvalItemsResponse;
      const withNoIssue = (body.items ?? []).filter(
        (row) => row.cols?.via_repository === true
      ).length;
      expect(withNoIssue).toBe(count);
      for (const { slug } of CARDS) {
        await expect(card(page, slug).locator('[data-note="repository"]')).toHaveText(
          `${withNoIssue} changes with no issue reached this project through ${REPO}`
        );
      }
    }
  });

  test("a project whose default repository is cleared counts no change of it", async ({ page }) => {
    await seed();
    // Before the move: CODEB's bots group holds c2 and its rest group c1, c4 and c5; c1, c2 and
    // c5 have no issue and count for CODEB only through its default repository (R11).
    const before = await codeSurvival("CODEB");
    expect([
      statistic(before, "survival:bots", "share"),
      statistic(before, "survival:rest", "changes"),
      statistic(before, "survival:rest", "via_repository"),
    ]).toEqual([0.6, 3, 2]);

    await putProjectDefaultRepository("CODEB", null);
    // With no default repository CODEB counts the changes of its own issues alone (R11): c4
    // (CODEB-1) in rest, and no change in bots, whose window reads no data and never 0 (R7).
    // The number and the items rows behind it agree (R13).
    await expect
      .poll(
        async () => {
          const after = await codeSurvival("CODEB");
          const response = await page.request.get(
            `/api/v1/projects/CODEB/evals/${SURVIVAL}/items?${WINDOW_QUERY}&row_kind=change&limit=200`
          );
          expect(response.status()).toBe(200);
          const items = ((await response.json()) as EvalItemsResponse).items ?? [];
          return [
            statistic(after, "survival:bots", "share"),
            statistic(after, "survival:rest", "changes"),
            statistic(after, "survival:rest", "via_repository"),
            items.filter((row) => row.cols?.via_repository === true).length,
            items.filter((row) => row.cols?.issue === "CODEB-1").length,
          ];
        },
        { message: "CODEB's code evals follow its cleared default repository", timeout: REGROUP_MS }
      )
      .toEqual([null, 1, 0, 0, 1]);
  });

  test("each card names its waiting days", async ({ page }) => {
    await seed();
    await openTab(page, "CODEA");
    for (const { slug } of CARDS) {
      await expect(group(page, slug, "agents").share).toBeVisible();
      await expect(card(page, slug).locator("[data-waiting]")).toHaveText("5 days still waiting");
    }
  });

  test("a marked day and a blocked commit are named", async ({ page }) => {
    await seed({ "2026-09-04": "marked", "2026-09-07": "blocked" });
    await openTab(page, "CODEA");
    for (const { slug } of CARDS) {
      const noData = card(page, slug).locator("[data-no-data]");
      await expect(noData).toHaveText([
        "No data: rows marked after a force-push found on 2026-09-04",
        `No data: GitHub did not give the whole patch of commit ${BLOCKED_SHA.slice(0, 7)} (3 files)`,
      ]);
      await expect(
        card(page, slug).getByRole("link", { name: BLOCKED_SHA.slice(0, 7) })
      ).toHaveAttribute("href", `https://github.com/${REPO}/commit/${BLOCKED_SHA}`);
      // Never 0, and never a part of the lines read as the whole.
      await expect(card(page, slug).locator("[data-share]")).toHaveCount(0);
    }
  });

  test("a project with no group is told to name them", async ({ page }) => {
    await seed();
    await putProjectDeliverySettings("CODEB", {
      code_groups: [],
      delivery: true,
      sources: [EVENTS_SOURCE],
    });
    // The regroup unlists the groups' series: the evals hold no group's number.
    await expect
      .poll(
        async () => {
          const survival = await codeSurvival("CODEB");
          return (survival?.series ?? []).filter(
            (series) => series.listed && series.series !== "status"
          ).length;
        },
        { timeout: REGROUP_MS }
      )
      .toBe(0);
    await openTab(page, "CODEB");
    for (const { slug } of CARDS) {
      await expect(card(page, slug).locator("[data-no-data]")).toHaveText([
        "No data: name this project's code groups",
      ]);
      await expect(
        card(page, slug).getByRole("link", { name: "name this project's code groups" })
      ).toHaveAttribute("href", "/projects/CODEB/settings");
    }
  });

  test("two projects' groups read two numbers, each holding the seed's count", async ({ page }) => {
    await seed();
    await openTab(page, "CODEA");
    // c1 by its prefix alone and c2 by its login alone, with c3 and c6: agents holds four.
    await expect(group(page, SURVIVAL, "agents").share).toHaveText("78.0%");
    await expect(group(page, SURVIVAL, "agents").counts).toHaveText(
      "156 of 200 lines alive \u00b7 4 changes"
    );
    await expect(group(page, REWORK, "agents").share).toHaveText("2.0%");
    await expect(group(page, REWORK, "agents").counts).toHaveText(
      "4 of 200 lines removed \u00b7 4 changes"
    );
    await openTab(page, "CODEB");
    // c2 by its login alone; the group with no rule takes every other change of CODEB's.
    await expect(group(page, SURVIVAL, "bots").share).toHaveText("60.0%");
    await expect(group(page, SURVIVAL, "bots").counts).toHaveText(
      "30 of 50 lines alive \u00b7 1 change"
    );
    await expect(group(page, SURVIVAL, "rest").share).toHaveText("75.0%");
    await expect(group(page, SURVIVAL, "rest").counts).toHaveText(
      "135 of 180 lines alive \u00b7 3 changes"
    );
    expect(
      await card(page, SURVIVAL)
        .locator("[data-group]")
        .evaluateAll((lines) => lines.map((line) => line.getAttribute("data-group")))
    ).toEqual(["bots", "rest"]);
  });

  test("a save of the form keeps code_groups, and so does a PUT that leaves it out", async ({
    page,
  }) => {
    await seed();
    await page.goto("/projects/CODEA/settings");
    await expect(page.getByRole("switch", { name: "Delivery" })).toBeChecked();
    await saveDeliverySettings(page, "CODEA");
    expect((await getProjectDeliverySettings("CODEA"))?.code_groups).toEqual(CODEA_GROUPS);

    await putProjectDeliverySettings("CODEA", { delivery: true, sources: [CODEA_SOURCE] });
    expect((await getProjectDeliverySettings("CODEA"))?.code_groups).toEqual(CODEA_GROUPS);
    // And the other project's groups stand as seeded.
    expect((await getProjectDeliverySettings("CODEB"))?.code_groups).toEqual(CODEB_GROUPS);
  });

  test("with delivery off the tab shows neither card", async ({ page }) => {
    await seed();
    await openTab(page, "CODEB");
    for (const { slug } of CARDS) await expect(card(page, slug)).toBeVisible();

    await putProjectDeliverySettings("CODEB", { delivery: false, sources: [EVENTS_SOURCE] });
    await openTab(page, "CODEB");
    // CODEB has no Legion daemon either: one line names both settings, and no section shows.
    await expect(page.locator("[data-missing-sources]")).toBeVisible();
    await expect(page.getByRole("region", { name: "Deploys and lead time" })).toHaveCount(0);
    await expect(page.locator("[data-code-card]")).toHaveCount(0);
  });

  test("a measurement reaches the open tab with no reload", async ({ page }) => {
    await seed();
    await openTab(page, "CODEA");
    await expect(group(page, SURVIVAL, "agents").share).toHaveText("78.0%");
    await expect(card(page, SURVIVAL).locator("[data-waiting]")).toHaveText("5 days still waiting");
    let navigations = 0;
    page.on("framenavigated", (frame) => {
      if (frame === page.mainFrame()) navigations += 1;
    });
    // A mark on the page's own document, which a reload or a navigation would drop.
    await page.evaluate(() => {
      document.documentElement.dataset.e2eStillOpen = "yes";
    });

    // Two newly measured days, one holding a change of agents with no issue, 100 lines all
    // alive. No route or test hook runs the measurement on the stack (the run is a leased duty,
    // and a stack of this suite holds no lease), so the row writes what the run's results
    // transaction writes and wakes the regroup, the run's other writer of these rows: the stored
    // digest of CODEA's groups goes, so its next pass rebuilds CODEA's rows from the results, as
    // the run's writer does, and the eval_days change reaches the open view.
    await sql(
      ...resultsSql(["2026-09-11", "2026-09-12"], {}, [
        {
          added: 100,
          alive: 100,
          author: "octo-dev",
          day: "2026-09-11",
          headRef: "k9seed/seventh",
          issue: null,
          pr: 107,
          sha: sha("c7"),
        },
      ]),
      `DELETE FROM code_projects WHERE project_key = 'CODEA'`,
      `SELECT pg_notify('dispatch_delivery_settings', 'CODEA')`
    );
    // 256 of 300 lines alive, and the waiting days fall from five to three.
    await expect(group(page, SURVIVAL, "agents").share).toHaveText("85.3%", {
      timeout: REGROUP_MS,
    });
    await expect(card(page, SURVIVAL).locator("[data-waiting]")).toHaveText("3 days still waiting");
    await expect(group(page, REWORK, "agents").share).toHaveText("1.3%");
    await expect(card(page, SURVIVAL).locator('[data-note="repository"]')).toHaveText(
      `3 changes with no issue reached this project through ${REPO}`
    );
    expect(navigations).toBe(0);
    expect(await page.evaluate(() => document.documentElement.dataset.e2eStillOpen)).toBe("yes");
  });
});
