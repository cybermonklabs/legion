// What gh 2.98.0 prints for a pull request — `gh pr view`, `gh pr view --comments` and `gh pr
// list`, with `--json` and without — from the pull request GitHub holds: the fields `--json`
// takes, its refusal of one it does not, the objects it reads a review and a comment as, and its
// text views. The gh stand-in reads the world's pull request and answers each call with these
// (gh-standin.ts's `prRead`); production's answers to the same calls are the measure, each named
// where it is used. A module of its own, since the gh stand-in is a program that answers a call
// when it is loaded.

type Json = Record<string, unknown>;
const isObject = (value: unknown): value is Json =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/** The fields `gh pr view --json` and `gh pr list --json` take, in the order gh lists them:
 * measured on 2026-11-17 with gh 2.98.0, `gh pr view <n> --repo <owner>/<repo> --json bogus` and
 * `gh pr list --repo <owner>/<repo> --json bogus` each listed these 46 after its refusal. */
export const PR_FIELDS = [
  "additions",
  "assignees",
  "author",
  "autoMergeRequest",
  "baseRefName",
  "baseRefOid",
  "body",
  "changedFiles",
  "closed",
  "closedAt",
  "closingIssuesReferences",
  "comments",
  "commits",
  "createdAt",
  "deletions",
  "files",
  "fullDatabaseId",
  "headRefName",
  "headRefOid",
  "headRepository",
  "headRepositoryOwner",
  "id",
  "isCrossRepository",
  "isDraft",
  "labels",
  "latestReviews",
  "maintainerCanModify",
  "mergeCommit",
  "mergeStateStatus",
  "mergeable",
  "mergedAt",
  "mergedBy",
  "milestone",
  "number",
  "potentialMergeCommit",
  "projectCards",
  "projectItems",
  "reactionGroups",
  "reviewDecision",
  "reviewRequests",
  "reviews",
  "state",
  "statusCheckRollup",
  "title",
  "updatedAt",
  "url",
] as const;
export type PrField = (typeof PR_FIELDS)[number];

/** gh's refusal of a `--json` that names a field it does not take, for the first such field of
 * `fields`, or undefined where it takes every one: measured on 2026-11-17, `--json bogus` and
 * `--json number,bogus,other` each printed `Unknown JSON field: "bogus"`, `Available fields:` and
 * each of PR_FIELDS on a line of its own, indented two spaces, on stderr and nothing on stdout,
 * and exited 1. */
export function unknownField(fields: readonly string[]): string | undefined {
  const unknown = fields.find((field) => !(PR_FIELDS as readonly string[]).includes(field));
  if (unknown === undefined) return undefined;
  return `Unknown JSON field: ${JSON.stringify(unknown)}\nAvailable fields:\n${PR_FIELDS.map((field) => `  ${field}\n`).join("")}`;
}

/** What the gh stand-in's record names a call it refused as gh refuses it (gh-standin.ts's
 * `handler`): one naming a pull request, a branch or a repository GitHub has not got
 * (`pullRefusal`), and one whose `--json` names a field gh does not take (`unknownField`). The
 * record keeps no refusal's text, so `rig.sh health` reads which refusal it was by this name and
 * declares both for every role (health.ts's GH_DECLARED_EVERY_ROLE). */
export const PULL_NOT_FOUND = "pr not found";
export const FIELD_REFUSED = "pr json field refused";

/** The login GitHub's GraphQL names an account by, which gh reads every author by: a user's own,
 * and an App's bot without the `[bot]` its REST login carries — measured on 2026-11-17, a review
 * the REST API names `<app>[bot]` is `<app>` in `gh pr view --json reviews`, and so is the pull
 * request's author in `gh pr view`'s text. */
export function graphqlLogin(login: string): string {
  return login.replace(/\[bot\]$/, "");
}

/** The pull request's author as gh writes it (`gh pr view --json author`), from the account as
 * GitHub's REST API answers it (rest-objects.ts's `userJson`): an App's bot as `app/<slug>`, with
 * no id or name, and a user by its login, node id and name — the world holds no account's name, so
 * none. Measured on 2026-11-17: #2827's author, a role App's bot, was `{"is_bot":true,"login":
 * "app/legion-implementer"}`. */
export function authorOf(user: Json): Json {
  const login = String(user.login ?? "");
  return user.type === "Bot"
    ? { is_bot: true, login: `app/${graphqlLogin(login)}` }
    : { id: user.node_id, is_bot: false, login, name: "" };
}

/** A review as GitHub's GraphQL holds it and gh reads it (`gh pr view --json reviews`), from the
 * review as GitHub's REST API answers it (gh-standin.ts's `reviewJson`): its node id, its author
 * by its GraphQL login, its association, body, state and the commit it stands at, when it was
 * submitted (null for one still pending, which GitHub has not submitted), and no reaction and no
 * edit, which the world holds none of. Production's reviews of #2827 carried these fields
 * (production-shapes.json's `gh pr view comments,reviews`), and `url` too on the 37 of its 137
 * reviews gh read past the first page of 100: gh asks for a review's `url` only on the pages after
 * the first (measured on 2026-11-17, every review past the 100th held one and no review before it
 * did, and a pull request of 2 reviews held none), and no pull request of the world holds 100. */
export function reviewNode(review: Json): Json {
  const user = isObject(review.user) ? review.user : {};
  return {
    id: review.node_id,
    author: { login: graphqlLogin(String(user.login ?? "")) },
    authorAssociation: review.author_association,
    body: review.body,
    submittedAt: review.submitted_at ?? null,
    includesCreatedEdit: false,
    reactionGroups: [],
    state: review.state,
    commit: { oid: review.commit_id },
  };
}

/** A comment on the pull request's conversation as GitHub's GraphQL holds it, from the comment
 * as GitHub's REST API answers it (gh-standin.ts's `issueComment`): its node id, its author by
 * its GraphQL login, its association, body, link and when it was made, whether `viewer` (the
 * login the call's token acts as) wrote it, and none of the edits, minimizing and reactions the
 * world holds none of. GraphQL answers a comment no one minimized with `minimizedReason` null,
 * which gh reads as "" (`commentsOf`). */
export function commentNode(comment: Json, viewer: string): Json {
  const user = isObject(comment.user) ? comment.user : {};
  return {
    id: comment.node_id,
    author: { login: graphqlLogin(String(user.login ?? "")) },
    authorAssociation: comment.author_association,
    body: comment.body,
    createdAt: comment.created_at,
    includesCreatedEdit: false,
    isMinimized: false,
    minimizedReason: null,
    reactionGroups: [],
    url: comment.html_url,
    viewerDidAuthor: user.login === viewer,
  };
}

/** The pull request's latest review of each reviewer, as GitHub answers `latestReviews` from
 * `reviews`, each a review of `commentNode`'s and `reviewNode`'s shape: the last submitted review
 * of each author other than the pull request's own (`author`, a GraphQL login), whatever its state,
 * ordered by login. Measured on 2026-11-17 against cybermonklabs/legion's pull requests: #4340,
 * whose reviewer requested changes and then commented, listed the comment; the pull request's
 * author's own replies were in none of #2827, #4340 or #4303; and #2827 and #4134 each listed
 * `github-advanced-security` before `legion-reviewer`, the order of their logins (and of their
 * ids: the two orders were not told apart). */
export function latestReviews(reviews: readonly Json[], author: string): Json[] {
  const latest = new Map<string, Json>();
  for (const review of reviews) {
    const login = isObject(review.author) ? String(review.author.login ?? "") : "";
    if (review.state !== "PENDING" && login !== author) latest.set(login, review);
  }
  return [...latest.entries()]
    .sort(([one], [other]) => (one < other ? -1 : one > other ? 1 : 0))
    .map(([, review]) => review);
}

/** The state gh prints for a pull request: `DRAFT` for an open draft, its state otherwise. */
function stateOf(view: Json): string {
  return view.isDraft === true && view.state === "OPEN" ? "DRAFT" : String(view.state ?? "");
}

/** What gh's `reviewers:` line names (its `prReviewerList`): each author's last review but the
 * pull request's author's (`author`, a GraphQL login), each review request over it, none pending,
 * the reviews first and then the requests, each by name, as `<login> (<state>)` — `Approved`,
 * `Changes requested`, `Commented` for a comment or a dismissed review, `Requested` — joined by
 * `, `. Measured on 2026-11-17: `gh pr view <n>` printed `reviewers:\tgithub-advanced-security
 * (Commented), legion-reviewer (Changes requested)` for #2827, whose author's own replies are
 * among its reviews. */
function reviewersOf(view: Json, author: string): string {
  const states = new Map<string, string>();
  for (const review of Array.isArray(view.reviews) ? view.reviews : []) {
    if (!isObject(review) || !isObject(review.author)) continue;
    const login = String(review.author.login ?? "") || "ghost";
    if (login !== author) states.set(login, String(review.state ?? ""));
  }
  for (const request of Array.isArray(view.reviewRequests) ? view.reviewRequests : [])
    if (isObject(request)) states.set(String(request.login ?? request.slug ?? ""), "REQUESTED");
  const shown: Readonly<Record<string, string>> = {
    REQUESTED: "Requested",
    APPROVED: "Approved",
    CHANGES_REQUESTED: "Changes requested",
    COMMENTED: "Commented",
    DISMISSED: "Commented",
  };
  return [...states.entries()]
    .filter(([, state]) => state !== "PENDING")
    .sort(([one, a], [other, b]) =>
      (a === "REQUESTED") !== (b === "REQUESTED")
        ? a === "REQUESTED"
          ? 1
          : -1
        : one < other
          ? -1
          : one > other
            ? 1
            : 0
    )
    .map(
      ([login, state]) =>
        `${login} (${shown[state] ?? `${state.slice(0, 1)}${state.slice(1).toLowerCase()}`})`
    )
    .join(", ");
}

/** The names a list of `field`'s objects holds, joined as gh joins them. */
const named = (list: unknown, field: string) =>
  (Array.isArray(list) ? list : [])
    .map((item) => (isObject(item) ? String(item[field] ?? "") : ""))
    .join(", ");

/** `gh pr view <n>` as gh prints it when stdout is no terminal (its `printRawPrPreview`): one
 * `<name>:\t<value>` line for each of the title, the state, the author's GraphQL login, the labels,
 * the assignees, the reviewers (`reviewersOf`), the projects, the milestone, the number, the URL,
 * the lines added and removed and auto-merge, then `--` and the body. `view` holds every field of
 * PR_FIELDS these read, and `author` is the author's GraphQL login. Measured on 2026-11-17 (the
 * production reads' prod-pr-view.txt, `gh pr view 2827` with stdout no terminal): those thirteen
 * lines in that order, the empty ones a name, a tab and nothing, `auto-merge:\tdisabled`, `--`,
 * and the body and a line end. */
export function preview(view: Json, author: string): string {
  const auto = isObject(view.autoMergeRequest)
    ? `enabled\t${isObject(view.autoMergeRequest.enabledBy) ? String(view.autoMergeRequest.enabledBy.login ?? "") : ""}\t${String(view.autoMergeRequest.mergeMethod ?? "").toLowerCase()}`
    : "disabled";
  const projects = [
    ...(Array.isArray(view.projectCards) ? view.projectCards : []).map((card) =>
      isObject(card) && isObject(card.project) ? String(card.project.name ?? "") : ""
    ),
    ...(Array.isArray(view.projectItems) ? view.projectItems : []).map((item) =>
      isObject(item) ? String(item.title ?? "") : ""
    ),
  ].join(", ");
  return [
    `title:\t${String(view.title ?? "")}`,
    `state:\t${stateOf(view)}`,
    `author:\t${author}`,
    `labels:\t${named(view.labels, "name")}`,
    `assignees:\t${named(view.assignees, "login")}`,
    `reviewers:\t${reviewersOf(view, author)}`,
    `projects:\t${projects}`,
    `milestone:\t${isObject(view.milestone) ? String(view.milestone.title ?? "") : ""}`,
    `number:\t${String(view.number ?? "")}`,
    `url:\t${String(view.url ?? "")}`,
    `additions:\t${String(view.additions ?? 0)}`,
    `deletions:\t${String(view.deletions ?? 0)}`,
    `auto-merge:\t${auto}`,
    "--",
    `${String(view.body ?? "")}\n`,
  ].join("\n");
}

/** `gh pr view <n> --comments` as gh prints it when stdout is no terminal (its `RawCommentList`):
 * the conversation's comments and the reviews gh shows — every review but a pending one and a
 * `COMMENTED` one with no body, which is how GitHub files a comment made outside a review — in the
 * order each was made (a review by when it was submitted), each as its author's GraphQL login, its
 * association in lower case, whether it was edited and its status (a review's state in lower case
 * with a space for each `_`, `none` for a comment), then `--`, its body and `--`; a minimized one
 * is left out. Measured on 2026-11-17 (prod-pr-view-comments.txt, `gh pr view 2827 --comments`
 * with stdout no terminal): 18 such entries for the 6 comments and the 12 of 137 reviews that have
 * a body, `status:\tnone` on each comment, and no line of the pull request's own. */
export function commentList(view: Json): string {
  const reviews = (Array.isArray(view.reviews) ? view.reviews : []).filter(
    (review) =>
      isObject(review) &&
      review.state !== "PENDING" &&
      !(review.state === "COMMENTED" && review.body === "")
  );
  const status: Readonly<Record<string, string>> = {
    APPROVED: "approved",
    CHANGES_REQUESTED: "changes requested",
    COMMENTED: "commented",
  };
  const entries = [
    ...(Array.isArray(view.comments) ? view.comments : []).map((comment) => ({
      at: isObject(comment) ? String(comment.createdAt ?? "") : "",
      entry: comment,
      status: "none",
    })),
    ...reviews.map((review) => ({
      at: isObject(review) ? String(review.submittedAt ?? "") : "",
      entry: review,
      status: isObject(review) ? (status[String(review.state)] ?? "none") : "none",
    })),
  ].sort((one, other) => (one.at < other.at ? -1 : one.at > other.at ? 1 : 0));
  return entries
    .flatMap(({ entry, status: shown }) => {
      if (!isObject(entry) || entry.isMinimized === true) return [];
      const login = isObject(entry.author) ? String(entry.author.login ?? "") : "";
      return [
        `author:\t${login}\nassociation:\t${String(entry.authorAssociation ?? "").toLowerCase()}\nedited:\t${String(entry.includesCreatedEdit === true)}\nstatus:\t${shown}\n--\n${String(entry.body ?? "")}\n--\n`,
      ];
    })
    .join("");
}

/** One pull request's line of `gh pr list` as gh prints it when stdout is no terminal: its
 * number, title, head branch, state and when it was opened, tab-separated. Measured on 2026-11-17:
 * `gh pr list --repo <owner>/<repo> --limit 2` printed `<n>\t<title>\t<branch>\tOPEN\t<created>`
 * for each, and nothing at all, exit 0, where no pull request matched. */
export function listLine(view: Json): string {
  return [view.number, view.title, view.headRefName, stateOf(view), view.createdAt]
    .map((value) => String(value ?? ""))
    .join("\t");
}
