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

// stage3-devbox-workflow.sh prints no word a gh call answered or said: each gh site of the proof,
// run as the script writes it with a stand-in gh whose words hold the marker (lib/stage3-gh-words.ts
// says how a row runs and what it asserts). Where the site prints what a helper of lib/workflow.sh
// handed it, the helper runs as that library writes it, against the same stand-in.
const siteRows = siteHarness("stage3-devbox-gh-words-test");
const devbox = "stage3-devbox-workflow.sh";
const devboxSpan = scriptSpan(devbox);
const implement = "legion-implement[bot]";
// The run's project and its root issue's key, which the proof writes as the project key and a
// number.
const project = "S312345678";
const rootIssue = `${project}-1`;
const devboxSetup = `project=${project} root_issue=$project-1 pr_number=21 smoke_file=README.md
implement_app_login='${implement}' review_app_login='legion-review[bot]' real_gh=$bin/gh state=$work/state from=held
unset STAGE3_PR
until_true() { shift 2; "$@" >/dev/null 2>&1 || fail "timed out"; }
require_proof_human() { :; }
send_agent() { :; }
wait_for_phase() { :; }
wait_for_worker() { :; }
assert_round_handoff() { :; }
assert_handoff_record() { :; }
set_status() { :; }
await_tree_suspended() { :; }
sleep() { :; }`;

describe(`${devbox} prints no word of gh's`, () => {
  describe("prerequisites: the devbox gh reads the smoke repository", () => {
    const code = () =>
      devboxSpan(/^require_proof_human$/, /^mkdir -p "\$evidence\/logs" "\$state"/);
    const read = (answer: Answer): Rule[] => [
      { match: "repo view sjawhar/legion-smoke --json name", answers: [answer] },
    ];
    siteRows(
      devbox,
      code,
      devboxSetup,
      [
        {
          name: "a refused read",
          gh: read(said()),
          called: "repo view sjawhar/legion-smoke",
          code: 1,
          says: ["the devbox's ordinary gh cannot read sjawhar/legion-smoke"],
          kept: "gh/repo-readable.err",
        },
      ],
      [
        {
          name: "a read that answers",
          gh: read(answered('{"name":"legion-smoke"}')),
          called: "repo view",
          code: 0,
        },
      ]
    );
  });

  describe("primary-implementer-pull-request-and-handoff", () => {
    const code = () =>
      devboxSpan(/^ {2}pr_number=\$\(gh.*pr list --head "legion\/\$root_issue"/, /^ {2}pass$/);
    const rules = (
      number = answered("21"),
      authors = answered(`${implement}|${implement}`),
      files = answered("README.md")
    ): Rule[] => [
      { match: "--json number --jq .[0].number", answers: [number] },
      { match: "/commits --jq", answers: [authors] },
      { match: "/files --jq", answers: [files] },
    ];
    const opened = `implementer opened sjawhar/legion-smoke#21 on legion/${rootIssue} changing one file`;
    siteRows(
      devbox,
      code,
      devboxSetup,
      [
        {
          name: "the pull request's number answered as no number",
          gh: rules(answered(marker)),
          called: "--jq .[0].number",
          code: 1,
          says: [`gh answered no pull request number for legion/${rootIssue}`],
          kept: `gh/pr-of-${rootIssue}.answer`,
        },
        {
          name: "the pull request list refused",
          gh: rules(said()),
          called: "--jq .[0].number",
          code: 1,
          kept: `gh/pr-of-${rootIssue}.err`,
        },
        {
          name: "the commits' authors answered with another name",
          gh: rules(undefined, answered(`${marker}|${implement}`)),
          called: "pulls/21/commits",
          code: 1,
          says: [
            "sjawhar/legion-smoke#21 commits are not authored and committed by the implementer's App bot alone",
          ],
          kept: "gh/authors-of-21.answer",
        },
        {
          name: "the commits' read refused",
          gh: rules(undefined, said()),
          called: "pulls/21/commits",
          code: 1,
          kept: "gh/authors-of-21.err",
        },
        {
          name: "the one file the pull request changes, its path gh's answer",
          gh: rules(undefined, undefined, answered(`${marker}.md`)),
          called: "pulls/21/files",
          code: 0,
          says: [opened],
          kept: "pull-request-files.txt",
        },
        {
          name: "two files the pull request changes",
          gh: rules(undefined, undefined, answered(`README.md\n${marker}.md`)),
          called: "pulls/21/files",
          code: 1,
          says: ["does not change the spec's one product file alone"],
          kept: "pull-request-files.txt",
        },
      ],
      [
        {
          name: "the implementer's one-file pull request",
          gh: rules(),
          called: "pulls/21/files",
          code: 0,
          says: [opened],
        },
        {
          name: "commits of another author fail the check",
          gh: rules(undefined, answered("someone|someone")),
          called: "pulls/21/commits",
          code: 1,
          says: ["commits are not authored and committed by the implementer's App bot alone"],
        },
      ]
    );
  });

  describe("the bot's review thread, rounds 1 and 2", () => {
    // The loop body from the round's bot note to its handoff checks, run for round 1, which opens
    // the thread, then for round 2, which reads it again, as the review loop runs them.
    const code = () => {
      const body = devboxSpan(
        /^ {4}bot_note=""$/,
        /^ {4}assert_round_handoff "\$root_issue" "\$round"$/
      );
      return `round=1\n${body}\nround=2\n${body}`;
    };
    const setup = `${devboxSetup}
round_correction_pushed() { :; }
bot_thread_resolved() { :; }
round_line() { printf 'round %s' "$1"; }`;
    const rules = (head: Answer, posted = answered("PRRC_1 sjawhar-agent[bot] Bot")): Rule[] => [
      { match: "pulls/21 --jq .head.sha", answers: [head] },
      { match: "--method POST repos/sjawhar/legion-smoke/pulls/21/comments", answers: [posted] },
    ];
    siteRows(
      devbox,
      code,
      setup,
      [
        {
          name: "the pull request's head read refused",
          gh: rules(said()),
          called: "pulls/21 --jq .head.sha",
          code: 1,
          says: ["the proof human could not open a bot review thread on pull request #21"],
          names: "bot-thread.err",
        },
        {
          name: "the thread opened by an account that is no bot",
          gh: rules(answered(sha("e")), answered(`PRRC_1 ${marker} User`)),
          called: "--method POST",
          code: 1,
          names: "bot-thread.err",
        },
        {
          name: "the thread's first comment id, gh's answer",
          gh: rules(answered(sha("e")), answered(`${marker} sjawhar-agent[bot] Bot`)),
          called: "--method POST",
          code: 0,
          says: [
            "the implementer answered the bot's review thread",
            "carries the Legion reviewer's reply and is resolved",
          ],
          kept: "bot-thread.id",
        },
      ],
      [
        {
          name: "a bot's thread opened and read again",
          gh: rules(answered(sha("e"))),
          called: "--method POST",
          code: 0,
          says: ["the implementer answered the bot's review thread"],
        },
      ]
    );
  });

  describe("smoke-main-clean: the merged files and main's leftovers", () => {
    const code = () =>
      devboxSpan(
        /^ {2}handoff_pushes=\$\(pull_request_legion_only_commits\)/,
        /^ {2}release_smoke_main$/
      );
    const setup = `${devboxSetup}
pull_request_legion_only_commits() { :; }
clean_smoke_main() { :; }`;
    const rules = (files: Answer, leftovers = answered("")): Rule[] => [
      { match: "pulls/21/files --jq", answers: [files] },
      { match: "git/trees/main", answers: [leftovers] },
    ];
    siteRows(
      devbox,
      code,
      setup,
      [
        {
          name: "a merged handoff file, its path gh's answer",
          gh: rules(answered(`README.md\n.legion/${marker}/plan.json`)),
          called: "pulls/21/files",
          code: 1,
          says: ["sjawhar/legion-smoke#21 merged 1 handoff files"],
          kept: "merged-handoff-files.txt",
        },
        {
          name: "a leftover on main, its path gh's answer",
          gh: rules(answered("README.md"), answered(`docs/solutions/${marker}.md`)),
          called: "git/trees/main",
          code: 1,
          says: ["sjawhar/legion-smoke main still carries 1 proof leftovers"],
          kept: "smoke-main-leftovers.txt",
        },
      ],
      [
        {
          name: "no handoff merged and nothing left on main",
          gh: rules(answered("README.md")),
          called: "git/trees/main",
          code: 0,
          says: ["merged no commit that changes only .legion/ and no file under .legion/"],
        },
      ]
    );
  });

  describe("a development run's credential check names the newest pull request", () => {
    const code = () => devboxSpan(/^ {2}pr_number=\$\{STAGE3_PR:-\$\(gh/, /^ {2}pass$/);
    const newest = (answer: Answer): Rule[] => [
      { match: "pr list --state all --limit 1", answers: [answer] },
    ];
    siteRows(
      devbox,
      code,
      devboxSetup,
      [
        {
          name: "the newest pull request answered as no number",
          gh: newest(answered(marker)),
          called: "--state all --limit 1",
          code: 1,
          says: ["gh answered no pull request number for the newest smoke pull request"],
          kept: "gh/newest-pr.answer",
        },
        {
          name: "the list refused",
          gh: newest(said()),
          called: "--state all --limit 1",
          code: 1,
          kept: "gh/newest-pr.err",
        },
      ],
      [
        {
          name: "the newest pull request's number",
          gh: newest(answered("41")),
          called: "--state all --limit 1",
          code: 0,
          says: ["credential check against sjawhar/legion-smoke#41"],
        },
      ]
    );
  });

  describe("in-agent-credentials: gh with an empty GH_CONFIG_DIR", () => {
    const code = () =>
      devboxSpan(
        /^ {2}if GH_CONFIG_DIR="\$work\/empty-gh"/,
        /^ {2}note "real implementer pane ran/
      );
    const viewer = (answer: Answer): Rule[] => [{ match: "api graphql", answers: [answer] }];
    siteRows(
      devbox,
      code,
      devboxSetup,
      [
        {
          name: "a refusal that does not name GH_TOKEN",
          gh: viewer(said()),
          called: "api graphql",
          code: 1,
          says: ["gh with an empty GH_CONFIG_DIR failed without naming GH_TOKEN"],
          kept: "cred-negative.txt",
        },
      ],
      [
        {
          name: "gh's own refusal, naming GH_TOKEN",
          gh: viewer({
            err: "gh: To use GitHub CLI, set the GH_TOKEN environment variable\n",
            code: 4,
          }),
          called: "api graphql",
          code: 0,
        },
        {
          name: "an answer is a failure",
          gh: viewer(answered('{"data":{}}')),
          called: "api graphql",
          code: 1,
          says: ["gh with an empty GH_CONFIG_DIR and no token still answered"],
        },
      ]
    );
  });

  describe("cleanup-is-complete: this run's open pull requests", () => {
    const code = () => devboxSpan(/^open=\$\(run_pull_requests\)/, /^note "\$\{closed:-/);
    const open = (answer: Answer): Rule[] => [
      { match: "pr list --state open --limit 1000", answers: [answer] },
    ];
    siteRows(
      devbox,
      code,
      devboxSetup,
      [
        {
          name: "an open pull request answered as no number",
          gh: open(answered(marker)),
          called: "--limit 1000",
          code: 1,
          says: ["this run's pull requests are still open on sjawhar/legion-smoke"],
          kept: "open-pull-requests.txt",
        },
      ],
      [
        {
          name: "an open pull request is named by its number",
          gh: open(answered("31")),
          called: "--limit 1000",
          code: 1,
          says: ["this run's pull requests are still open on sjawhar/legion-smoke: 31"],
        },
        { name: "none open", gh: open(answered("")), called: "--limit 1000", code: 0 },
      ]
    );
  });
});
