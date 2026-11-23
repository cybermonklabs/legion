/** The stress layer the layout rows read every page through (`e2e/layout-rule.stub.e2e.ts`):
 *  `e2e/review-seed.ts`'s corpus, and on top of it the data the layout
 *  defects of dispatch://INFRA-5200 need to show. The review corpus is small on purpose - a visual
 *  baseline reads it - so a row of chips that scrolls sideways at forty entries draws three there
 *  and passes; this layer writes the lengths production holds, so a page that breaks the layout
 *  rule on production breaks it here.
 *
 *  It changes nothing `seedReviewRoutes` writes that a route's readiness reads: the issue the
 *  issue routes open keeps its spec, documents and asks, and its new title still begins with the
 *  words the priority page's ready locator matches (`e2e/review-routes.ts`).
 *
 *  What it adds, each for the defect the INFRA-5200 audit measured on production
 *  (dispatch://INFRA-5200/spec, "Problem"):
 *  - on the issue the issue routes open: a 170-character title, twelve labels, a `role:` route
 *    nobody holds, a claim by a session with a 64-character title, a parent, two priorities,
 *    fifteen GitHub pull requests with checks, five subscribers, thirteen components of a
 *    fifteen-component architecture source, four more children of four pull requests each, a
 *    recorded result whose run command is 200 characters with no space, and a check whose words
 *    hold an 80-character path (the header, its state row, the Children tab, the Verification
 *    tab);
 *  - an answered ask whose three options are 160 characters each (`/answers`);
 *  - six open asks for the reader with long questions and long options, two of them replied to
 *    by an agent (the Inbox's reply chip), and a mention reply from a session with a long title;
 *  - a project with an 80-character name, the server's cap (the Settings forms' project pickers);
 *  - twelve sessions on the machine `/machines/:machine` opens, each in a tmux window of its own
 *    (the machine page's window strip, the Agents page's rows and its chosen agents);
 *  - a live session with a replayed conversation and an uploaded picture, for the two agent
 *    pages the review layers exclude (`agentSession`, `agentArtifactSlug`).
 *
 *  Every write goes through `e2e/api.ts`, as `e2e/workspace.ts` writes: a person's as alice, a
 *  session's under the agent bearer with the session's title stamped in `origin`. The fakes are
 *  seeded as the suite seeds them elsewhere: the fake GitHub's repositories
 *  (`e2e/fake-github-helpers.ts`), the fake Envoy's sessions and interests (`e2e/agents.ts`). */

import { expect } from "@playwright/test";
import type { Actor, ArtifactUploadResponse } from "../web/src/api/types";

import { type FakeSession, setInterests, setLiveSessions } from "./agents";
import {
  answerAsk,
  baseUrl,
  claimIssue,
  createAsk,
  createComment,
  createIssue,
  createPriority,
  createProject,
  editArtifact,
  getIssue,
  listIssueArtifacts,
  patchIssue,
  publishAgentStreamFrame,
  putArchitectureSource,
  recordCheckResult,
  replyToCommentDelivery,
  syncArchitectureSource,
  userHeaders,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import {
  AGENT_PICTURE,
  AGENT_REPLY_OPENING,
  ARCHITECTURE_REPO,
  type LayoutSeed,
} from "./layout-routes";
import { seedReviewRoutes } from "./review-seed";
import { workspaceSessions } from "./workspace";

/** The repository the issue's pull requests and its recorded result stand in. Used by no other
 *  spec: the server remembers which installation covers a repository it has read. */
const PULLS_REPO = "acme/layout-stress";
/** How many pull requests the issue links, and how many each new child links. A pull request is
 *  linked from one issue at most (`409 EXTERNAL_LINK_TAKEN`), so the children's follow the
 *  issue's: the issue links 1-15 and child `n` links the four after `15 + 4(n-1)`. */
const PULLS = 15;
const CHILDREN = 4;
const CHILD_PULLS = 4;
/** A pull request's head commit: 40 hex digits, one commit per pull request. */
const headOf = (pull: number): string => pull.toString(16).padStart(40, "a");

/** The fifteen components of the architecture source, by the file each is read from; the issue
 *  names the first thirteen. */
const COMPONENTS = [
  "issue-header",
  "issue-tabs",
  "inbox-list",
  "inbox-reading-pane",
  "answers-page",
  "settings-forms",
  "agent-live-view",
  "machine-page",
  "agents-page",
  "project-issues-table",
  "priorities-table",
  "sidebar-navigation",
  "verification-tab",
  "children-tab",
  "artifacts-tab",
];
const ISSUE_COMPONENTS = COMPONENTS.slice(0, 13);

const LABELS = [
  "dispatch",
  "layout",
  "issue-header",
  "inbox",
  "accessibility",
  "regression",
  "phone-width",
  "needs-design-review",
  "customer-visible",
  "performance-budget",
  "browser-test",
  "priority-escalation",
];

/** Words a production title or question holds, repeated to the length a case needs. */
function prose(opening: string, length: number): string {
  const filler =
    " - measured on production at every width from 360 to 1920 pixels, where a row of " +
    "elements either scrolls sideways or wraps wherever the width happens to fall";
  let text = opening;
  while (text.length < length) text += filler;
  return text.slice(0, length).trimEnd();
}

/** A session id as Envoy writes one: a UUID, which Dispatch's agent routes require. */
function stressSessionId(index: number): string {
  return `019a5200-${index.toString(16).padStart(4, "0")}-7a00-8000-${index.toString(16).padStart(12, "0")}`;
}

/** The machine `/machines/:machine` opens: the first workspace session's. */
const MACHINE = workspaceSessions[0]?.machine_id ?? "";

/** Twelve sessions on that machine, each in a tmux window of its own, with titles of the length
 *  a Legion lane writes. The first holds the claim, the next five subscribe to the issue, the
 *  seventh replies in the Inbox, and the eighth is the live session the agent pages open. */
const stressSessions: FakeSession[] = Array.from({ length: 12 }, (_, index) => ({
  capabilities: ["aside", "btw", "steer"],
  dir: `/workspaces/legion/INFRA-5200-layout-rule-worktree-${index + 1}`,
  machine_id: MACHINE,
  roles: [`legion-architect-core-${index + 1}`],
  session_id: stressSessionId(index + 1),
  title:
    index === 0
      ? "Legion implementer · INFRA-5200 · the layout rule's browser test"
      : `Legion architect · INFRA-${5200 + index} · Dispatch layout rule and its stress seed`,
  tmux_place: {
    layout: `b262,80x24,0,0,${index}`,
    pane_id: `%${index}`,
    pane_index: 0,
    server_pid: "5200",
    session_name: "legion",
    socket: "/tmp/tmux-1000/default",
    window_id: `@${index + 1}`,
    window_index: index + 1,
    window_name: `infra-${5200 + index}-layout-rule-lane`,
    zoomed: false,
  },
}));

function sessionActor(session: FakeSession): { actor: Actor; as: "agent" } {
  return {
    actor: { id: session.session_id, kind: "session", origin: { session_title: session.title } },
    as: "agent",
  };
}

function stressSession(index: number): FakeSession {
  const session = stressSessions[index];
  if (session === undefined) throw new Error(`seedLayoutStress: no stress session ${index}`);
  return session;
}

/** A picture uploaded to `sessionID`'s conversation as alice, as a paste on the Agents page
 *  does (`e2e/two-builds.multiprocess.e2e.ts` uploads one the same way); answers its slug. */
async function uploadAgentPicture(sessionID: string): Promise<string> {
  const png =
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL0eAAAAABJRU5ErkJggg==";
  const form = new FormData();
  form.append("name", AGENT_PICTURE);
  form.append("file", new Blob([Buffer.from(png, "base64")], { type: "image/png" }));
  const response = await fetch(
    new URL(`/api/v1/agents/${encodeURIComponent(sessionID)}/artifacts`, baseUrl),
    { body: form, headers: await userHeaders("alice"), method: "POST" }
  );
  if (!response.ok) {
    throw new Error(`agent picture upload: ${response.status} ${await response.text()}`);
  }
  return ((await response.json()) as ArtifactUploadResponse).artifact.slug;
}

/** The pull requests, their checks and the architecture model, in the fake GitHub's one seed:
 *  a seed replaces every installation, so both repositories are written together. Every third
 *  pull request has a failing check, so the header draws both states. */
async function seedGithub(issue: string): Promise<void> {
  const pulls: Record<string, { head_sha: string; state: string; title: string }> = {};
  const checkRuns: Record<
    string,
    { check_suite_id: number; conclusion: string; id: number; name: string; status: string }[]
  > = {};
  for (let pull = 1; pull <= PULLS + CHILDREN * CHILD_PULLS; pull += 1) {
    pulls[String(pull)] = {
      head_sha: headOf(pull),
      state: pull % 5 === 0 ? "closed" : "open",
      title: prose(`${issue} part ${pull}: the layout rule on one more surface`, 90),
    };
    checkRuns[headOf(pull)] = [
      {
        check_suite_id: pull * 10,
        conclusion: "success",
        id: pull * 10,
        name: "lint",
        status: "completed",
      },
      {
        check_suite_id: pull * 10 + 1,
        conclusion: pull % 3 === 0 ? "failure" : "success",
        id: pull * 10 + 1,
        name: "dispatch-browser",
        status: "completed",
      },
    ];
  }
  const files = Object.fromEntries(
    COMPONENTS.map((id) => [
      `${id}.md`,
      `---\ntitle: ${prose(`The ${id.replaceAll("-", " ")}`, 48)}\n---\nOne part of the dashboard.\n`,
    ])
  );
  await seedFakeGithub({
    [ARCHITECTURE_REPO]: { contents: "read", files, installation_id: 5201 },
    [PULLS_REPO]: {
      check_runs: checkRuns,
      commit_pulls: { [headOf(1)]: [1] },
      contents: "read",
      installation_id: 5202,
      pull_requests: "read",
      pulls,
    },
  });
}

/** Seeds the review corpus and the stress layer on top of it into an empty database; the caller
 *  resets first. `boundMs` is the bound of the review corpus's own waits (`seedReviewRoutes`). */
export async function seedLayoutStress({
  boundMs,
}: {
  readonly boundMs: number;
}): Promise<LayoutSeed> {
  const review = await seedReviewRoutes({ boundMs });
  const issue = review.issue;
  await setLiveSessions([...workspaceSessions, ...stressSessions]);
  await seedGithub(issue);
  await putArchitectureSource("CORE", { branch: "main", repo: ARCHITECTURE_REPO });
  const source = await syncArchitectureSource("CORE", ARCHITECTURE_REPO);
  if (source.last_error !== null) {
    throw new Error(
      `seedLayoutStress: the architecture source did not import: ${source.last_error}`
    );
  }

  // The issue's header: every property production's longest headers hold.
  const parent = await createIssue({
    force: true,
    project: "CORE",
    title: "Dispatch's surfaces lay out by one rule, at every width a reader sets",
  });
  const secondPriority = await createPriority({
    owner: "alice",
    title: "Every surface of Dispatch reads at every width from a phone to a wide screen",
  });
  const pullUrl = (pull: number) => `https://github.com/${PULLS_REPO}/pull/${pull}`;
  await patchIssue(issue, {
    advances: [review.priority, secondPriority.id],
    components: { ids: ISSUE_COMPONENTS, mode: "explicit" },
    external_links: [
      ...Array.from({ length: PULLS }, (_, index) => ({
        kind: "github_pr",
        url: pullUrl(index + 1),
      })),
      // A link that is no pull request, which today's header draws beside them and the
      // redrawn Details lists apart (`links`).
      {
        kind: "url",
        url: "https://docs.example.com/dispatch/the-layout-rule-and-its-browser-test",
      },
    ],
    labels: LABELS,
    parent: parent.key,
    repo: PULLS_REPO,
    route: "role:layout-stress-reviewer",
    // The ready locator of `/priorities/:id` matches the title's first words.
    title: prose(
      "Local testing workflow for Dispatch, with every header property production holds",
      170
    ),
  });
  await claimIssue(issue, sessionActor(stressSession(0)));
  await setInterests(
    stressSessions.slice(1, 6).map((session) => ({
      session_id: session.session_id,
      topics: [`notifications.dispatch.issue.${issue}.>`],
    }))
  );
  // One after the other, so the Children tab lists them in one order on every run.
  for (let child = 1; child <= CHILDREN; child += 1) {
    const created = await createIssue({
      force: true,
      parent: issue,
      project: "CORE",
      title: prose(`Child ${child}: one surface of the layout rule`, 110),
    });
    await patchIssue(created.key, {
      external_links: Array.from({ length: CHILD_PULLS }, (_, index) => ({
        kind: "github_pr",
        url: pullUrl(PULLS + (child - 1) * CHILD_PULLS + index + 1),
      })),
      status: child % 2 === 0 ? "in_progress" : "todo",
    });
  }
  // The Verification tab's run command: one unbroken path of 200 characters.
  await recordCheckResult(issue, {
    check: "c-routes",
    commit: headOf(1),
    negative_control: "Dropped a row of the route table and the test named the route it lost.",
    observed: "every route of the router reviewed or excluded",
    role: "tester",
    run: `packages/dispatch/e2e/__tests__/review-routes.test.ts::${"covers-every-route-of-the-router-reviewed-or-excluded-with-a-reason/".repeat(3)}`.slice(
      0,
      200
    ),
    surface: "cli",
    verdict: "pass",
  });

  // A check whose words hold an 80-character path with no space: on production at 400 px a
  // check of this shape made the Verification tab 428 px wide.
  const verification = (await listIssueArtifacts(issue)).find(
    (artifact) => artifact.name === "verification.md"
  );
  if (verification === undefined) {
    throw new Error(`seedLayoutStress: ${issue} has no verification.md to add a check to`);
  }
  await editArtifact(verification.id, {
    ops: [
      {
        after: "block:c-ready",
        markdown:
          `:::check{#c-long-path serves="dispatch://${issue}/artifact/${review.issueArtifactSlug}#b-r2" surface="cli" run="bash packages/dispatch/e2e/acceptance/converted-verification-check-with-a-long-path.sh" negative_control="Point it at a skeleton and it fails."}\n` +
          "Before READY, against production, bash packages/dispatch/e2e/acceptance/converted-verification-check-with-a-long-path.sh\n:::",
        op: "insert",
      },
    ],
  });

  // The Answers page: an answered ask whose three options are 160 characters each.
  const answersIssue = await createIssue({
    force: true,
    project: "CORE",
    title: prose("Which header groups a reader acts on stay on the first line", 120),
  });
  const options = ["first", "second", "third"].map((which) => ({
    label: prose(
      `Keep the ${which} group on the first line and move every other property into Details`,
      160
    ),
  }));
  const answered = await createAsk(
    answersIssue.key,
    {
      options,
      question: prose(
        "Which group of the issue header stays on its first line at a phone's width?",
        260
      ),
    },
    sessionActor(stressSession(1))
  );
  await answerAsk(
    answered.id,
    { expected_edited_at: null, selected: [options[1]?.label ?? ""] },
    { login: "alice" }
  );

  // The Inbox: six open asks for alice with long questions, two with long options, and a reply
  // from a session with a long title. Low and medium urgency, so the blocking ask the review
  // corpus writes stays the row the reading pane opens on.
  for (let ask = 1; ask <= 6; ask += 1) {
    const askIssue = await createIssue({
      force: true,
      project: "CORE",
      title: prose(`Inbox stress ${ask}: a question production holds`, 140),
    });
    const asked = await createAsk(
      askIssue.key,
      {
        options:
          ask % 3 === 0
            ? [
                {
                  label: prose(
                    "Move the claim chip into Details and keep its warning in the header",
                    150
                  ),
                },
                {
                  label: prose(
                    "Keep the claim chip in the header and fold the route beside it",
                    150
                  ),
                },
              ]
            : undefined,
        question: prose(
          `Question ${ask}: should the header fold its labels after ten entries`,
          420
        ),
        urgency: ask % 2 === 0 ? "med" : "low",
      },
      sessionActor(stressSession(ask))
    );
    // The Inbox row's reply chip ("<agent> replied", `InboxRowChip` in `features/inbox/Inbox.tsx`):
    // an agent's reply under the ask, which hands the turn back to the reader, as
    // `dispatch comment --reply-to-ask` writes it (`ask_id`, `envoy-client`'s conversation tool).
    // From the session with the 64-character title, on the first two asks.
    if (ask <= 2) {
      await createComment(
        askIssue.key,
        {
          ask_id: asked.id,
          body: prose("The header still scrolls sideways at 400 pixels on production", 200),
          turn: "human",
        },
        sessionActor(stressSession(0))
      );
    }
  }
  // On an issue with no priority, so the reply's row lists under the Inbox's last band and the
  // spec decision the review corpus writes stays the row the reading pane opens on (the inbox
  // route's ready locator, `e2e/review-routes.ts`).
  const replier = stressSession(6);
  const mention = await createComment(answersIssue.key, {
    body: `@${replier.title} please re-run the layout test at 360 pixels`,
    delivery: "btw",
    mentions: [{ target: `session:${replier.session_id}` }],
  });
  await replyToCommentDelivery(
    mention.id,
    {
      attempt: 1,
      body: prose("Re-ran at 360 pixels: the header's rail still scrolls sideways", 300),
    },
    sessionActor(replier).actor
  );

  // The Settings page: a project whose name is 80 characters, the server's cap and production's
  // longest.
  await createProject({
    key: "STRESS",
    name: prose(
      "Layout stress project whose name is as long as the longest name a deployment holds",
      80
    ),
  });

  // The agent pages: a replayed conversation and a picture of the live session.
  const live = stressSession(7);
  await publishAgentStreamFrame(
    live.session_id,
    {
      frames: [
        {
          kind: "message",
          message: {
            at: 1_000,
            id: "u1000",
            parts: [
              { text: prose("Read the layout audit and fix the issue header", 300), type: "text" },
            ],
            role: "user",
            streaming: false,
          },
          seq: 1,
          v: 1,
        },
        {
          kind: "message",
          message: {
            at: 2_000,
            id: "a2000",
            parts: [
              {
                text: `${AGENT_REPLY_OPENING} The rail lives in packages/dispatch/web/src/features/issue/IssueHeader.tsx/and/its/rail/component/at/one/hundred/and/twenty/characters.tsx\n\n${prose("Next", 600)}`,
                type: "text",
              },
            ],
            role: "assistant",
            streaming: false,
          },
          seq: 2,
          v: 1,
        },
      ],
      session_id: live.session_id,
      v: 1,
    },
    "replay"
  );
  const agentArtifactSlug = await uploadAgentPicture(live.session_id);

  // The route nobody holds is drawn as a warning only once Dispatch has judged it `no_holder`
  // against the listener's session list, which it re-reads every few seconds: before that it
  // answers `unknown` and every page draws no warning. A page read in that gap drew the warning on
  // one run and not on the next, so the seed is done only once the verdict stands.
  await expect
    .poll(async () => (await getIssue(issue)).route_status, {
      message: `${issue}'s route reads no_holder`,
      timeout: boundMs,
    })
    .toBe("no_holder");

  return {
    ...review,
    agentArtifactSlug,
    agentSession: live.session_id,
    // A clock read, after every write: the rows pin the page clock a minute past it.
    seededAt: new Date().toISOString(),
  };
}
