// Why a worker world answers each route of gh-routes.ts's WORLD_ROUTES that no role text of either
// label names, kept apart from the table itself. A route a text names that the world does not
// answer is a refusal production never gives, and a route the world answers that nothing names is
// a world that tells an agent more than it was asked about, so such a route stays only with its
// evidence: a recorded run whose agent called it, or why GitHub serves it to a role.
//
// The evidence names recorded runs by the names they ran under, which carry the measurement's own
// words (`round17-forty/reviewer-text-findings-text-only-head-2/calls.jsonl:26`), so it is in no
// module the gh stand-in imports: rig.sh's `standins` bundles gh-standin.ts and every module it
// imports into each run's <run>/lib/gh.js, which the agent can read and launch's guard reads
// (run-names.ts's `worldFiles`). At 9083afb5 the stand-in came to import WORLD_ROUTES with its evidence
// in it, and the guard refused every reviewer of INFRA-764's round 30. The stand-in reads the
// routes and their answers alone (gh-routes.ts); the evidence is read by the rows of
// gh-standin.test.ts that cite a route's run. No test reads a role text, so none holds a route of
// the table to a text that names it or to its evidence here.

/** Why a world answers a route no role text of either label names: a recorded run whose agent
 * called it (`run`, `<batch>/<run>/calls.jsonl:<line>`), or, where no recorded run called it, why
 * GitHub serves it to a role (`world`). A world that stopped answering such a route would give a
 * refusal production does not, so it stays, and says why. */
export type Evidence =
  | { readonly run: string; readonly reason: string }
  | { readonly world: string };

/** The evidence of each route of gh-routes.ts's WORLD_ROUTES that carries one, by the route's key
 * as the table writes it. A route a role text names needs none. */
export const ROUTE_EVIDENCE: Readonly<Partial<Record<string, Evidence>>> = {
  "GET repos/{owner}/{repo}/pulls/{number}/commits": {
    world:
      "GitHub's REST list of the pull request's commits, the read `gh pr view --json commits` makes through GraphQL",
  },
  "GET repos/{owner}/{repo}/pulls/{number}/files": {
    world:
      "GitHub's REST list of the pull request's files, the read `gh pr view --json files` and `gh pr diff --name-only` make",
  },
  "GET repos/{owner}/{repo}/pulls/{number}/reviews/{id}": {
    run: "round17-forty/threads-closed-text-fixes-base-2/calls.jsonl:33",
    reason: "a reviewer reads one review back by its id",
  },
  "GET repos/{owner}/{repo}/pulls/{number}/reviews/{id}/comments": {
    run: "round17-forty/reviewer-text-findings-text-only-head-2/calls.jsonl:26",
    reason: "a reviewer reads back the inline comments of the review it created",
  },
  "GET repos/{owner}/{repo}/pulls/{number}/comments": {
    run: "round17-forty/reviewer-text-findings-text-only-head-2/calls.jsonl:25",
    reason: "a reviewer lists the pull request's inline comments beside its threads",
  },
  "GET repos/{owner}/{repo}/issues/{number}/comments": {
    run: "round17-forty/reviewer-text-findings-defect-base-2/calls.jsonl:29",
    reason: "a reviewer reads the pull request's conversation before it reviews",
  },
  "GET repos/{owner}/{repo}/commits/{ref+}": {
    run: "round29-world-health/94caf2e09844/calls.jsonl:8",
    reason: "a reviewer reads the commit the implementer pushed, for its author and parents",
  },
  "GET repos/{owner}/{repo}/commits": {
    world:
      "GitHub's list of the commits a branch reaches, the REST form of the history beside the read of one commit",
  },
  "GET repos/{owner}/{repo}/rulesets": {
    world:
      "the list of the rulesets whose id each rule of the base branch names, which a reviewer reaches from those rules",
  },
  "GET repos/{owner}/{repo}/rulesets/{id}": {
    world:
      "the ruleset a rule of the base branch names by its id, which a reviewer reads for the rules it holds",
  },
  "GET repos/{owner}/{repo}/branches/{branch}/protection": {
    world:
      "GitHub's protection read of the base branch the merge gate reads, which a reviewer reaches for its required checks",
  },
  "GET repos/{owner}/{repo}/branches": {
    world: "GitHub's list of the branches, beside the read of one branch the world serves",
  },
  "GET repos/{owner}/{repo}/tags": {
    world: "GitHub's list of the tags, which it answers empty for a repository with none",
  },
  "GET repos/{owner}/{repo}/git/refs": {
    world: "GitHub's list of every ref, the plural form of the ref read the world serves",
  },
  "GET repos/{owner}/{repo}/git/refs/{ref+}": {
    world:
      "GitHub's read of one ref or of the refs under a name, the plural form of the ref read the world serves",
  },
  "GET repos/{owner}/{repo}/git/commits/{sha}": {
    world: "GitHub's git-data read of one commit, which it serves beside the read of a commit",
  },
  "GET repos/{owner}/{repo}/git/trees/{sha+}": {
    world: "GitHub's read of a commit's tree, the files of a head listed without a checkout",
  },
  "GET repos/{owner}/{repo}/contents/{path+}": {
    run: "not-counted-r24/threads-closed-text-fixes-head-3/calls.jsonl:16",
    reason: "a head reviewer re-reads the fixed file at the head through the API",
  },
  "GET user": { world: "the identity read every gh session can make, which GitHub refuses an App" },
  "GH auth status": { world: "the identity read every gh session can make" },
  "GH pr diff": {
    world:
      "gh's read of the pull request's diff, which round 25's agents made in two spellings no role text named, `gh pr diff <n>` and `gh pr diff <n> --color never` (production-shapes.json's `gh pr diff -` and `gh pr diff ---color`, their readings again of 2026-11-18); the one text that named the read, conflicts-and-rewrites.md's `gh pr diff <n> --name-only`, left with #4464",
  },
  "POST repos/{owner}/{repo}/pulls/{number}/reviews/{id}/events": {
    world: "submits the pending review the world's own create leaves",
  },
  "PUT repos/{owner}/{repo}/pulls/{number}/reviews/{id}": {
    run: "round25-runs/1e2a0aeced4b/calls.jsonl:23",
    reason: "a reviewer rewrites its approval's body after it submitted it",
  },
  "POST repos/{owner}/{repo}/pulls/{number}/comments/{id}/replies": {
    world: "the REST form of the review-thread reply",
  },
  "POST repos/{owner}/{repo}/pulls/{number}/comments": {
    run: "round17-forty/reviewer-text-findings-text-only-head-5/calls.jsonl:21",
    reason: "a reviewer posts an inline comment on its own",
  },
  "POST repos/{owner}/{repo}/issues/{number}/comments": {
    world: "the REST form of the conversation comment gh pr comment posts",
  },
  "GH pr edit": {
    run: "round17-forty/reviewer-text-findings-defect-base-4/calls.jsonl:32",
    reason: "a role writes the body's sections with gh pr edit",
  },
  "GRAPHQL mutation addPullRequestReviewThreadReply": {
    run: "round17-forty/reviewer-text-findings-defect-base-5/calls.jsonl:34",
    reason: "a reviewer replies on a review thread through GraphQL",
  },
};
