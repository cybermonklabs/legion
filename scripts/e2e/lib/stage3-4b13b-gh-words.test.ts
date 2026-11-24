import { describe } from "bun:test";
import {
  type Answer,
  answered,
  marker,
  type Rule,
  said,
  scriptSpan,
  sha,
  siteHarness,
} from "./stage3-gh-words";

// stage3-4b13b-acceptance.sh prints no word a gh call answered or said: each gh site of the proof,
// run as the script writes it with a stand-in gh whose words hold the marker (lib/stage3-gh-words.ts
// says how a row runs and what it asserts).
const siteRows = siteHarness("stage3-4b13b-gh-words-test");
const accept = "stage3-4b13b-acceptance.sh";
const acceptSpan = scriptSpan(accept);
// The run's project and its issues' keys, which the proof writes as the project key and a number.
const project = "AC12345678";
const root1 = `${project}-1`;
const child = `${project}-4`;
const acceptSetup = `project=${project}
scratch_base=l208-4b13b-accept-1 merge_base=l208-4b13b-accept-1-merge gate_base=l208-4b13b-accept-1-gate
root1=$project-1 root2=$project-2 root3=$project-3 child=$project-4
declare -A pr_of=([$root1]=11 [$root2]=12 [$root3]=13 [$child]=14) gate_artifacts=([$root3]=a3)
main_sha=${sha("a")} review_app='legion-review[bot]'
soft_failures=$evidence/soft-failures.txt first_soft_check= first_soft_since=
: >"$soft_failures"
until_true() { shift 2; "$@" >/dev/null 2>&1 || fail "timed out"; }
role_app() { printf 'legion-review[bot]'; }
require_proof_human() { :; }
send_agent() { :; }
sleep() { :; }`;

describe(`${accept} prints no word of gh's`, () => {
  describe("prerequisites: the devbox gh reads the smoke repository", () => {
    const code = () => acceptSpan(/^require_proof_human$/, /^head_commit=/);
    const read = (answer: Answer): Rule[] => [
      { match: "api repos/sjawhar/legion-smoke --jq .name", answers: [answer] },
    ];
    siteRows(
      accept,
      code,
      acceptSetup,
      [
        {
          name: "a refused read",
          gh: read(said()),
          called: "api repos/sjawhar/legion-smoke --jq .name",
          code: 1,
          says: ["the devbox's ordinary gh cannot read sjawhar/legion-smoke"],
          kept: "gh/repo-readable.err",
        },
      ],
      [
        {
          name: "a read that answers",
          gh: read(answered("legion-smoke")),
          called: "--jq .name",
          code: 0,
        },
      ]
    );
  });

  describe("rig: the scratch base is cut from main", () => {
    const code = () => acceptSpan(/^main_sha=\$\(gh/, /^start_daemon$/);
    const created: Rule = {
      match: "api repos/sjawhar/legion-smoke/git/refs -f",
      answers: [answered("{}")],
    };
    const main = (answer: Answer): Rule => ({ match: "git/ref/heads/main", answers: [answer] });
    siteRows(
      accept,
      code,
      acceptSetup,
      [
        {
          name: "main's commit answered as no commit id",
          gh: [main(answered(marker)), created],
          called: "git/ref/heads/main",
          code: 1,
          says: ["gh answered no commit id for sjawhar/legion-smoke's main"],
          kept: "gh/main-sha.answer",
        },
        {
          name: "main's commit refused",
          gh: [main(said()), created],
          called: "git/ref/heads/main",
          code: 1,
          kept: "gh/main-sha.err",
        },
        {
          name: "the scratch base's creation refused",
          gh: [main(answered(sha("b"))), { match: "git/refs -f", answers: [said()] }],
          called: "git/refs -f ref=refs/heads/l208-4b13b-accept-1",
          code: 1,
          kept: "gh/scratch-base.err",
        },
      ],
      [
        {
          name: "the base is cut at main's commit, which the note names",
          gh: [main(answered(sha("b"))), created],
          called: `sha=${sha("b")}`,
          code: 0,
          says: [`scratch base sjawhar/legion-smoke:l208-4b13b-accept-1 at main ${sha("b")}`],
        },
      ]
    );
  });

  describe("retarget_pr", () => {
    const code = () => `retarget_pr ${root1}`;
    const rules = (number: Answer, views: Answer[], edit = answered("")): Rule[] => [
      { match: "--json number --jq .[0].number", answers: [number] },
      {
        match: `pr list --head legion/${root1} --state open --json number`,
        answers: [answered('[{"number":11}]')],
      },
      { match: "pr view 11 --json baseRefName", answers: views },
      { match: "pr edit 11 --base", answers: [edit] },
      // A view of a pull request named by another answer than its number.
      { match: "pr view", answers: [answered("l208-4b13b-accept-1")] },
    ];
    siteRows(
      accept,
      code,
      acceptSetup,
      [
        {
          name: "the pull request's number answered as no number",
          gh: rules(answered(marker), [answered("l208-4b13b-accept-1")]),
          called: "--jq .[0].number",
          code: 1,
          says: [`gh answered no pull request number for legion/${root1}`],
          kept: `gh/pr-of-${root1}.answer`,
        },
        {
          name: "the pull request list refused",
          gh: rules(said(), [answered("l208-4b13b-accept-1")]),
          called: "--jq .[0].number",
          code: 1,
          kept: `gh/pr-of-${root1}.err`,
        },
        {
          name: "its base answered as neither main nor the scratch base",
          gh: rules(answered("11"), [answered(marker), answered("l208-4b13b-accept-1")]),
          called: "pr edit 11 --base l208-4b13b-accept-1",
          code: 0,
          says: ["its base read neither main nor l208-4b13b-accept-1 here"],
          kept: `gh/base-of-${root1}.answer`,
        },
        {
          name: "its base's read refused",
          gh: rules(answered("11"), [said()]),
          called: "pr view 11 --json baseRefName",
          code: 1,
          kept: `gh/base-of-${root1}.err`,
        },
        {
          name: "its retarget refused",
          gh: rules(answered("11"), [answered("main")], said()),
          called: "pr edit 11 --base l208-4b13b-accept-1",
          code: 1,
          kept: `gh/retarget-${root1}.err`,
        },
        {
          name: "its base's read after the retarget refused",
          gh: rules(answered("11"), [answered("main"), said()]),
          called: "pr edit 11 --base l208-4b13b-accept-1",
          code: 1,
          says: ["sjawhar/legion-smoke#11 did not retarget to l208-4b13b-accept-1"],
          kept: `gh/rebased-${root1}.err`,
        },
      ],
      [
        {
          name: "a pull request on main is retargeted, and the note names main",
          gh: rules(answered("11"), [answered("main"), answered("l208-4b13b-accept-1")]),
          called: "pr edit 11 --base l208-4b13b-accept-1",
          code: 0,
          says: [
            `${root1} opened sjawhar/legion-smoke#11; its base read main here, and is l208-4b13b-accept-1`,
          ],
        },
      ]
    );
  });

  describe("pr-closed-unmerged: the child's pull request is closed", () => {
    const code = () =>
      acceptSpan(/^gh.* pr close "\$\{pr_of\[\$child\]\}"/, /^# The notice is queued on the child/);
    const close = (answer: Answer): Rule[] => [{ match: "pr close 14", answers: [answer] }];
    siteRows(
      accept,
      code,
      acceptSetup,
      [
        {
          name: "the close refused",
          gh: close(said()),
          called: "pr close 14",
          code: 1,
          kept: `gh/close-${child}.err`,
        },
      ],
      [{ name: "the close", gh: close(answered("")), called: "pr close 14", code: 0 }]
    );
  });

  describe("reviewers: the approved commits and the head", () => {
    const code = () => acceptSpan(/^review_app=\$\(role_app reviewer\)$/, /^pass$/);
    const setup = `${acceptSetup}
until_true() { :; }
issue_phase() { return 1; }
wait_for_phase_at_least() { :; }`;
    const rules = (reviews: Answer, head: Answer): Rule[] => [
      { match: "/reviews --jq", answers: [reviews] },
      { match: "--jq .head.sha", answers: [head] },
    ];
    siteRows(
      accept,
      code,
      setup,
      [
        {
          name: "an approval's commit answered as no commit id",
          gh: rules(answered(marker), answered(sha("c"))),
          called: "api --paginate repos/sjawhar/legion-smoke/pulls/11/reviews",
          code: 0,
          says: ["approved <no commit id: gh's answer is in"],
          kept: "gh/reviews-of-11.answer",
        },
        {
          name: "the reviews refused",
          gh: rules(said(), answered(sha("c"))),
          called: "pulls/11/reviews",
          code: 0,
          kept: "gh/reviews-of-11.err",
        },
        {
          name: "the head answered as no commit id",
          gh: rules(answered(sha("b")), answered(marker)),
          called: "api repos/sjawhar/legion-smoke/pulls/11 --jq .head.sha",
          code: 0,
          says: ["the head is now <no commit id: gh's answer is in"],
          kept: `gh/head-of-${root1}.answer`,
        },
        {
          name: "the head's read refused",
          gh: rules(answered(sha("b")), said()),
          called: "pulls/11 --jq .head.sha",
          code: 0,
          kept: `gh/head-of-${root1}.err`,
        },
      ],
      [
        {
          name: "commit ids are named by their first eight digits",
          gh: rules(answered(sha("b")), answered(sha("c"))),
          called: "pulls/13 --jq .head.sha",
          code: 0,
          says: [`${root1}: legion-review[bot] approved bbbbbbbb; the head is now cccccccc`],
        },
      ]
    );
  });

  describe("merger-merges-with-its-own-squash-message", () => {
    const code = () => acceptSpan(/^begin merger-merges-with-its-own-squash-message$/, /^pass$/);
    const setup = `${acceptSetup}
await_merger_merge() { merge_when_clean_commit=${sha("d")}; }
wait_for_phase() { :; }
notice_needle() { echo needle; }
notice_deliveries() { echo 0; }`;
    const rules = (
      title: Answer,
      message: Answer,
      refs = answered("{}"),
      edit = answered("")
    ): Rule[] => [
      { match: "git/refs -f ref=refs/heads/l208-4b13b-accept-1-merge", answers: [refs] },
      { match: "pr edit 11 --base l208-4b13b-accept-1-merge", answers: [edit] },
      { match: "pulls/11 --jq .title", answers: [title] },
      { match: `commits/${sha("d")} --jq .commit.message`, answers: [message] },
    ];
    const file = `merge-commit-message-${root1}.txt`;
    siteRows(
      accept,
      code,
      setup,
      [
        {
          name: "a squash message that is not the title and number",
          gh: rules(answered("Proof change"), answered(marker)),
          called: `commits/${sha("d")}`,
          code: 0,
          says: ["is not the pull request's title and number alone, which it should be"],
          kept: file,
        },
        {
          name: "a squash message that is the title and number, the title gh's answer",
          gh: rules(answered(marker), answered(`${marker} (#11)`)),
          called: "pulls/11 --jq .title",
          code: 0,
          says: ["under its own message, the pull request's title and number alone"],
          kept: file,
        },
        {
          name: "a squash message that carries the branch's trailers",
          gh: rules(
            answered("Proof change"),
            answered(`Proof change (#11)\n\nOmp-Session: ${marker}`)
          ),
          called: `commits/${sha("d")}`,
          code: 0,
          says: ["carries the branch's own commit messages; it is in"],
          kept: file,
        },
        {
          name: "the merge base's creation refused",
          gh: rules(answered("Proof change"), answered("Proof change (#11)"), said()),
          called: "refs/heads/l208-4b13b-accept-1-merge",
          code: 1,
          kept: "gh/merge-base.err",
        },
        {
          name: "the retarget to the merge base refused",
          gh: rules(
            answered("Proof change"),
            answered("Proof change (#11)"),
            answered("{}"),
            said()
          ),
          called: "pr edit 11 --base l208-4b13b-accept-1-merge",
          code: 1,
          kept: `gh/merge-base-of-${root1}.err`,
        },
        {
          name: "the title's read refused",
          gh: rules(said(), answered("Proof change (#11)")),
          called: "pulls/11 --jq .title",
          code: 1,
          kept: `gh/title-of-${root1}.err`,
        },
        {
          name: "the squash message's read refused",
          gh: rules(answered("Proof change"), said()),
          called: `commits/${sha("d")}`,
          code: 1,
          kept: `gh/merge-message-of-${root1}.err`,
        },
      ],
      [
        {
          name: "a squash message that is the title and number passes, its merge commit named",
          gh: rules(answered("Proof change"), answered("Proof change (#11)")),
          called: `commits/${sha("d")}`,
          code: 0,
          says: [
            `the merger merged sjawhar/legion-smoke#11 at ${sha("d")} under its own message, the pull request's title and number alone`,
          ],
        },
      ]
    );
  });

  describe("a-reclosed-gate-holds-the-merge: the gate base", () => {
    const code = () => acceptSpan(/^begin a-reclosed-gate-holds-the-merge$/, /^gate_artifact=/);
    const rules = (refs: Answer, edit: Answer): Rule[] => [
      { match: "git/refs -f ref=refs/heads/l208-4b13b-accept-1-gate", answers: [refs] },
      { match: "pr edit 13 --base l208-4b13b-accept-1-gate", answers: [edit] },
    ];
    siteRows(
      accept,
      code,
      acceptSetup,
      [
        {
          name: "the gate base's creation refused",
          gh: rules(said(), answered("")),
          called: "refs/heads/l208-4b13b-accept-1-gate",
          code: 1,
          kept: "gh/gate-base.err",
        },
        {
          name: "the retarget to the gate base refused",
          gh: rules(answered("{}"), said()),
          called: "pr edit 13 --base l208-4b13b-accept-1-gate",
          code: 1,
          kept: `gh/gate-base-of-${project}-3.err`,
        },
      ],
      [
        {
          name: "the gate base cut and the pull request on it",
          gh: rules(answered("{}"), answered("")),
          called: "pr edit 13",
          code: 0,
        },
      ]
    );
  });

  describe("a-reclosed-gate-holds-the-merge: the held pull request", () => {
    const code = () => acceptSpan(/^held=\$\(gh/, /^note "\$root3's merger ended its turn/);
    const held = (answer: Answer): Rule[] => [
      { match: "pr view 13 --json state,autoMergeRequest,isInMergeQueue", answers: [answer] },
    ];
    siteRows(
      accept,
      code,
      acceptSetup,
      [
        {
          name: "the pull request's state answered in none of its fields' forms",
          gh: held(answered(marker)),
          called: "pr view 13",
          code: 1,
          says: ["reads 'an answer in none of their forms, kept in"],
          kept: `gh/held-${project}-3.answer`,
        },
        {
          name: "the pull request's read refused",
          gh: held(said()),
          called: "pr view 13",
          code: 1,
          kept: `gh/held-${project}-3.err`,
        },
      ],
      [
        {
          name: "a merged pull request is named by its fields",
          gh: held(answered("MERGED\tfalse\tfalse")),
          called: "pr view 13",
          code: 1,
          says: [
            "sjawhar/legion-smoke#13 reads 'MERGED\tfalse\tfalse' (state, auto-merge armed, in a merge queue)",
          ],
        },
        {
          name: "an open pull request with nothing armed or queued passes",
          gh: held(answered("OPEN\tfalse\tfalse")),
          called: "pr view 13",
          code: 0,
        },
      ]
    );
  });
});
