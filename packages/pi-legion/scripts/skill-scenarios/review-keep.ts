// The reviews a role sends through the gh stand-in (gh-standin.ts), as GitHub records them, and
// every read of them, as functions of what they are given alone: the stand-in parses the call and
// hands each the call's parts and what the run has kept so far.
// A review the role sends is kept: a POST of `.../pulls/<n>/reviews` with an `--input` JSON object
// or `-f`/`-F` fields, or `gh pr review` with `--approve`, `--request-changes` or `--comment` and
// `-b/--body` or `-F/--body-file`, through a route that carries `review`, the template of each
// review it keeps. GitHub records it so: the state its event names (APPROVE is APPROVED,
// REQUEST_CHANGES is CHANGES_REQUESTED, COMMENT is COMMENTED, and no event is PENDING), the body
// exactly as sent, and the template's commit (the head), user and pull request, under the next id
// counted from the template's. A route that carries `reviews` answers from what was kept: `list`
// every review in the order the role sent them, `one` the review whose id the match's named group
// `id` took, or GitHub's 404 for an id no review the role sent has, and `pr` the object its own
// answer is with `reviews` and `latestReviews` beside it, in the fields gh 2.98.0 prints for
// `gh pr view --json` (measured): the review's node id, author's login, association, body,
// submission time, state and commit, the latest review with no id and no commit, since gh asks
// none for it. A pending review is in the list: GitHub shows a pending review to its author alone,
// and the rig's reader is the login that sent it. Which review GitHub answers as the latest once a
// pending or comment-only review is kept is not measured, so a call that asks `latestReviews` then
// is refused as unmodeled. A `pr` route's `comments` names the route that posts a comment on the
// pull request: its `comments` field is none until that route has answered a call, and since the
// stand-in keeps no comment, a call that asks it after is refused as unmodeled. The inline
// `comments` a review carries are kept with it, each as GitHub records a review comment: its path,
// line and side, its body exactly as sent, the head as its commit, the reviewer, and its review's
// id, under the next id counted from the template's `comment_id`. `comments` reads every one in
// the order sent, and `review_comments` those of the review whose id the match's `id` took, or
// GitHub's 404 for an id no review the role sent has. A review whose answer the rig cannot state
// (an event GitHub refuses, REQUEST_CHANGES or COMMENT with no body, a second pending review, a
// commit other than the template's, flags gh refuses, an inline comment on a path or line the
// template's `added_lines` does not hold, or on the left side) is kept nowhere and refused as
// unmodeled.
// A route that carries `review_edit` changes a kept review, the one whose id the match's `id`
// took, and answers it as GitHub answers it, the review object with the change made (GitHub's
// REST docs, "Update a review for a pull request" and "Dismiss a review for a pull request": each
// answers 200 with the review; the docs' example of each answers the review's every other field
// as the others do): `update` sets its body to the `body` the call sends (required, the docs),
// and `dismiss`, with the `message` the call sends (required) and an `event` that is DISMISS or
// none (the docs), sets its state to DISMISSED (the docs' example). A COMMENTED review cannot be
// dismissed: GitHub answers 422 (the docs list the 422) with the message "Can not dismiss a
// commented pull request review", in the shape of its measured errors
// (github-answers/review-dismiss-422). The message is not measured: a dismissal is a write, which
// the rig's read-only token cannot make, and no archived call dismisses a COMMENTED review (the
// one archived dismissal, trial-i-head-4's, dismisses review 6002, which is APPROVED), so the
// text decides no run. What GitHub answers for a review the role did not
// send, a pending or dismissed one, an empty body, or a dismissal with no message or another
// event is not measured, so each is refused as unmodeled. gh prints a review whose body was
// edited with `includesCreatedEdit` (GraphQL's mark of an edited comment), which is not
// measured, and which review is the latest once one is dismissed is not either: a `pr` read that
// asks `reviews` after an update, or `latestReviews` after either, is refused as unmodeled.
// The review threads of a GraphQL world (`{"$keep": "threads"}`, gh-graphql.ts) are the kept
// inline comments, each a thread of its own, unresolved, with that comment as its last, in the
// order sent, on one page whose end cursor is an opaque one of the rig's: threads() builds them
// whole, and gh-graphql.ts answers the query's own selection of them.
import { z } from "zod";

/** The template of each review a route keeps: the first review's id (each next review takes the
 * next number) and the first inline comment's, the commit every review is on, the reviewer and its
 * association with the repository, the pull request's page (a review's own is it with
 * `#pullrequestreview-<id>`, a comment's with `#discussion_r<id>`) and its API URL, and the lines
 * of each file the diff adds whole, which are the lines an inline comment can be on. */
export const ReviewTemplate = z.object({
  id: z.number(),
  comment_id: z.number(),
  commit_id: z.string(),
  user: z.looseObject({ login: z.string() }),
  author_association: z.string(),
  html_url: z.string(),
  pull_request_url: z.string(),
  added_lines: z.record(z.string(), z.number()),
});
export type ReviewTemplate = z.infer<typeof ReviewTemplate>;
/** A review as the stand-in keeps it: GitHub's REST object for it, its fields in GitHub's order. */
export const KeptReview = z.looseObject({
  id: z.number(),
  node_id: z.string(),
  user: z.looseObject({ login: z.string() }),
  body: z.string(),
  state: z.string(),
  html_url: z.string(),
  pull_request_url: z.string(),
  author_association: z.string(),
  _links: z.unknown(),
  submitted_at: z.string().optional(),
  commit_id: z.string(),
});
export type KeptReview = z.infer<typeof KeptReview>;
/** An inline comment of a kept review: GitHub's REST object for a review comment. */
export const KeptComment = z.looseObject({
  id: z.number(),
  pull_request_review_id: z.number(),
  user: z.looseObject({ login: z.string() }),
  body: z.string(),
});
export type KeptComment = z.infer<typeof KeptComment>;
/** How a route reads what the role sent (the header). */
export const ReviewMode = z.enum(["list", "one", "pr", "comments", "review_comments"]);
export type ReviewMode = z.infer<typeof ReviewMode>;
/** How a route changes a review the role sent (the header): its body, or its dismissal. */
export const ReviewEdit = z.enum(["update", "dismiss"]);
export type ReviewEdit = z.infer<typeof ReviewEdit>;
/** What the run has kept: the reviews the role sent and their inline comments, in the order sent,
 * and the ids of the reviews whose body it updated since (`edited`, none where absent). */
export interface Kept {
  reviews: KeptReview[];
  comments: KeptComment[];
  edited?: number[];
}

/** The inline comments a review sends, each as GitHub records a review comment, under the next
 * ids; `null` where one is on a path or line the template's `added_lines` does not hold, on the
 * left side, or has no body, which GitHub refuses or the rig cannot state. */
export function inlineComments(
  template: ReviewTemplate,
  reviewId: number,
  comments: unknown[],
  kept: Kept,
  sentAt: string
): KeptComment[] | null {
  const out: KeptComment[] = [];
  for (const [index, comment] of comments.entries()) {
    if (typeof comment !== "object" || comment === null || Array.isArray(comment)) return null;
    const {
      path: file,
      line,
      side = "RIGHT",
      start_line: start = null,
      start_side: startSide = "RIGHT",
      body: text,
    } = comment as Record<string, unknown>;
    const lines = typeof file === "string" ? template.added_lines[file] : undefined;
    if (lines === undefined || typeof text !== "string" || text === "") return null;
    if (side !== "RIGHT" || startSide !== "RIGHT") return null;
    if (typeof line !== "number" || !Number.isInteger(line) || line < 1 || line > lines)
      return null;
    if (
      start !== null &&
      (typeof start !== "number" || !Number.isInteger(start) || start < 1 || start >= line)
    )
      return null;
    const id = template.comment_id + kept.comments.length + index;
    const url = template.pull_request_url.replace(/\/\d+$/, `/comments/${id}`);
    const html_url = `${template.html_url}#discussion_r${id}`;
    out.push({
      url,
      pull_request_review_id: reviewId,
      id,
      node_id: `PRRC_${id}`,
      path: file,
      commit_id: template.commit_id,
      original_commit_id: template.commit_id,
      user: template.user,
      body: text,
      created_at: sentAt,
      updated_at: sentAt,
      html_url,
      pull_request_url: template.pull_request_url,
      author_association: template.author_association,
      _links: {
        self: { href: url },
        html: { href: html_url },
        pull_request: { href: template.pull_request_url },
      },
      start_line: start,
      original_start_line: start,
      start_side: start === null ? null : "RIGHT",
      line,
      original_line: line,
      side: "RIGHT",
      subject_type: "line",
    });
  }
  return out;
}

/** What a call hands GitHub for a review: a `gh pr review`'s event flags (each long name it gave
 * of `--approve`, `--request-changes` and `--comment`) and its body, or a `gh api` call's
 * `--input` file text, else its `-f`/`-F` fields. */
export interface ReviewCall {
  prReview: boolean;
  events: string[];
  body: string | undefined;
  input: string | undefined;
  fields: Record<string, unknown>;
}

/** What a `gh api` call hands GitHub as its JSON body: its `--input` file's JSON object, else its
 * `-f`/`-F` fields; `null` for an input that is no JSON object. */
function apiBody(
  input: string | undefined,
  fields: Record<string, unknown>
): Record<string, unknown> | null {
  if (input === undefined) return fields;
  let parsed: unknown;
  try {
    parsed = JSON.parse(input);
  } catch {
    return null;
  }
  return typeof parsed === "object" && parsed !== null && !Array.isArray(parsed)
    ? { ...parsed }
    : null;
}

/** The review a call sends, as GitHub records it under `template`, with its inline comments, at
 * `sentAt` (GitHub's stamp, to the second); `null` where the rig cannot state GitHub's answer to
 * it (the header). A `gh pr review` takes its event from its one flag and its body from `-b` or
 * `-F`; a `gh api` call takes them from its `--input` JSON object, else from its fields. */
export function sentReview(
  template: ReviewTemplate,
  call: ReviewCall,
  kept: Kept,
  sentAt: string
): { review: KeptReview; comments: KeptComment[] } | null {
  let sent: Record<string, unknown>;
  if (call.prReview) {
    const events = (
      [
        ["--approve", "APPROVE"],
        ["--request-changes", "REQUEST_CHANGES"],
        ["--comment", "COMMENT"],
      ] as const
    ).filter(([long]) => call.events.includes(long));
    if (events.length !== 1) return null;
    sent = { event: events[0]?.[1], body: call.body };
  } else {
    const body = apiBody(call.input, call.fields);
    if (body === null) return null;
    sent = body;
  }
  const { event, body: text = "", commit_id: commit, comments = [] } = sent;
  const states: Record<string, string> = {
    APPROVE: "APPROVED",
    REQUEST_CHANGES: "CHANGES_REQUESTED",
    COMMENT: "COMMENTED",
  };
  const state = event === undefined || event === "" ? "PENDING" : states[String(event)];
  if (state === undefined || typeof text !== "string") return null;
  if ((state === "CHANGES_REQUESTED" || state === "COMMENTED") && text === "") return null;
  if (state === "PENDING" && kept.reviews.some((review) => review.state === "PENDING")) return null;
  if (commit !== undefined && commit !== template.commit_id) return null;
  // A pending review's comments are pending too, which the rig does not state.
  if (!Array.isArray(comments) || (state === "PENDING" && comments.length > 0)) return null;
  const id = template.id + kept.reviews.length;
  const inline = inlineComments(template, id, comments, kept, sentAt);
  if (inline === null) return null;
  const html_url = `${template.html_url}#pullrequestreview-${id}`;
  return {
    review: {
      id,
      node_id: `PRR_${id}`,
      user: template.user,
      body: text,
      state,
      html_url,
      pull_request_url: template.pull_request_url,
      author_association: template.author_association,
      _links: { html: { href: html_url }, pull_request: { href: template.pull_request_url } },
      ...(state === "PENDING" ? {} : { submitted_at: sentAt }),
      commit_id: template.commit_id,
    },
    comments: inline,
  };
}

/** The change a call makes, through a route of `edit`, to the review the match's `id` names (the
 * header): that review as GitHub answers the change, every field but the one changed as kept;
 * GitHub's answer where it refuses the change (`github`, the name of its files under
 * github-answers/); or `null` where the rig cannot state GitHub's answer. The call hands GitHub
 * what a `gh api` call hands it for a review: its `--input` JSON object, else its fields. */
export function changedReview(
  edit: ReviewEdit,
  kept: Kept,
  call: { id: string | undefined; input: string | undefined; fields: Record<string, unknown> }
): { review: KeptReview } | { github: string } | null {
  const one = kept.reviews.find((review) => String(review.id) === call.id);
  const sent = apiBody(call.input, call.fields);
  if (one === undefined || sent === null || one.state === "PENDING" || one.state === "DISMISSED")
    return null;
  if (edit === "update") {
    const { body } = sent;
    return typeof body === "string" && body !== "" ? { review: { ...one, body } } : null;
  }
  const { message, event = "DISMISS" } = sent;
  if (typeof message !== "string" || message === "" || event !== "DISMISS") return null;
  return one.state === "COMMENTED"
    ? { github: "review-dismiss-422" }
    : { review: { ...one, state: "DISMISSED" } };
}

/** The login GraphQL gives the account REST names `login`: a bot's bare name, without the [bot]
 * REST adds to an App's account (rig.sh's worker_fixture names where that is recorded). */
export const graphqlLogin = (login: string) => login.replace(/\[bot\]$/, "");

/** A kept review in the fields gh 2.98.0 prints for `gh pr view --json reviews` (measured), which
 * gh reads through GraphQL. */
export function ghReview(review: KeptReview) {
  return {
    id: review.node_id,
    author: { login: graphqlLogin(review.user.login) },
    authorAssociation: review.author_association,
    body: review.body,
    submittedAt: review.submitted_at ?? null,
    includesCreatedEdit: false,
    reactionGroups: [],
    state: review.state,
    commit: { oid: review.commit_id },
  };
}

/** `latestReviews` as gh prints it: the latest review of each author, and the rig's reviewer is
 * the one author; undefined, which the rig cannot state, once a pending, comment-only or
 * dismissed review is kept (the header). */
export function latestReviews(reviews: KeptReview[]) {
  return reviews.some((review) => ["PENDING", "COMMENTED", "DISMISSED"].includes(review.state))
    ? undefined
    : reviews.slice(-1).map((review) => ({ ...ghReview(review), id: "", commit: { oid: "" } }));
}

/** A read of what the role sent through a route of `mode` (the header): the reviews, the review
 * the match's `id` names, the inline comments, or those of the review the `id` names, each with
 * GitHub's measured 404 for a review id no review the role sent has; or, for `pr`, the route's
 * own `answer` with the reviews beside its pull request, `null` where the call's `--json` fields
 * ask what the rig cannot state (`reviews` or `latestReviews` once a review's body was updated,
 * `latestReviews` it cannot name, or `comments` after the role posted one). Undefined for a
 * route that reads nothing the role sent. */
export function keptRead<A extends { stdout?: unknown }>(
  mode: ReviewMode | undefined,
  kept: Kept,
  call: { id: string | undefined; fields: string[] | undefined; commented: boolean },
  answer: A
): A | { stdout: unknown } | { github: string } | null | undefined {
  if (mode === "list") return { stdout: kept.reviews };
  if (mode === "comments") return { stdout: kept.comments };
  if (mode === "pr") {
    const edited = (kept.edited ?? []).length > 0;
    const latest = edited ? undefined : latestReviews(kept.reviews);
    if (
      call.fields !== undefined &&
      ((call.fields.includes("reviews") && edited) ||
        (call.fields.includes("latestReviews") && !latest) ||
        (call.fields.includes("comments") && call.commented))
    )
      return null;
    const pull = answer.stdout;
    if (typeof pull !== "object" || pull === null || Array.isArray(pull)) return answer;
    return {
      ...answer,
      stdout: {
        ...pull,
        reviews: kept.reviews.map(ghReview),
        latestReviews: latest ?? [],
        comments: [],
      },
    };
  }
  if (mode !== "one" && mode !== "review_comments") return undefined;
  const one = kept.reviews.find((review) => String(review.id) === call.id);
  if (one === undefined) return { github: mode === "one" ? "review-404" : "review-comments-404" };
  return mode === "one"
    ? { stdout: one }
    : { stdout: kept.comments.filter((comment) => comment.pull_request_review_id === one.id) };
}

/** The review threads the kept inline comments open (the header), whole: gh-graphql.ts answers a
 * query's own selection of them. */
export function threads(comments: KeptComment[]) {
  const last = comments.at(-1);
  return {
    pageInfo: {
      hasNextPage: false,
      endCursor: last === undefined ? null : btoa(`cursor:v2:${last.id}`),
    },
    totalCount: comments.length,
    nodes: comments.map((comment) => ({
      id: `PRRT_${comment.id}`,
      isResolved: false,
      comments: {
        totalCount: 1,
        nodes: [{ author: { login: graphqlLogin(comment.user.login) }, body: comment.body }],
      },
    })),
  };
}
