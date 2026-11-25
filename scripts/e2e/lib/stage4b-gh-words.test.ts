import { describe, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { holdsMarker, marker } from "./gh-words";
import {
  expectNoMissingHelpers,
  nameMissingHelpers,
  scriptAssignments,
  scriptFunctions,
  sourcedLibraries,
} from "./script-functions";

// Stage 4b prints no word a gh call answered or said, but a value it expects. Whatever answers a
// gh call in GitHub's place holds gh's token and writes all gh hands on: gh's stderr (`gh:
// <message> (HTTP 502)`, or the client's error) and its answer, and a tool's error over that answer
// quotes it. Each row below runs a site of stage4b-sandbox-tree.sh, the function taken from the
// script by name with every function of the script and of the libraries it sources that it calls
// (lib/workflow.sh's readers among them), under a stand-in gh first on PATH whose stderr or answer
// holds the marker not-a-secret-token, and asserts that no run of eight of its characters is in
// what the site printed, stdout and stderr together, beside the values it sets that the script
// prints after it. The twin of each row shows the marker is there to print: the file the site keeps
// gh's words or answer in holds it. A control runs each site on GitHub's ordinary answers and reads
// the stand-in's log, so the stand-in is the gh the site called. CI runs this file with the rest
// of scripts/e2e/lib (`bun test scripts/e2e/lib`, .github/workflows/ci-job-test.yaml).
const root = join(import.meta.dir, "..", "..", "..");
const script = join(root, "scripts", "e2e", "stage4b-sandbox-tree.sh");
const assignment = scriptAssignments(script);
const makeDir = temporaryDirectories("stage4b-gh-words");

// expectNoMarker fails the row, quoting what the site printed, when that holds the marker.
function expectNoMarker(printed: string): void {
  if (holdsMarker(printed)) {
    throw new Error(
      `MARKER CHECK: the site printed a run of eight characters of ${marker}:\n${printed}`
    );
  }
}

// Every function the script and the libraries it sources define, the script's own last, as bash
// holds them once the script has sourced its libraries.
const sources = [...sourcedLibraries(script).filter((path) => path !== script), script];
const defined = new Map<string, string>();
for (const path of sources) {
  const fn = scriptFunctions(path);
  for (const found of readFileSync(path, "utf8").matchAll(/^([A-Za-z_][A-Za-z0-9_]*)\(\) \{/gm)) {
    defined.set(found[1], fn(found[1]));
  }
}

// extract gives the definitions of ENTRY, of every function the harness's own text (HARNESS: its
// stubs and the row's body) names, and of every function those name, transitively, but the stubbed
// ones, which HARNESS defines. A name in a string or a comment brings its definition too, never
// called.
function extract(entry: string[], harness: string): string {
  const stubbed = new Set(
    [...harness.matchAll(/^([A-Za-z_][A-Za-z0-9_]*)\(\) \{/gm)].map((found) => found[1])
  );
  const taken: string[] = [];
  const seen = new Set<string>();
  const queue = [
    ...entry,
    ...[...harness.matchAll(/[A-Za-z_][A-Za-z0-9_]*/g)].map((word) => word[0]),
  ];
  for (let name = queue.shift(); name !== undefined; name = queue.shift()) {
    if (seen.has(name) || stubbed.has(name) || (!entry.includes(name) && !defined.has(name)))
      continue;
    seen.add(name);
    const text = defined.get(name);
    if (text === undefined)
      throw new Error(`stage4b-sandbox-tree.sh and its libraries define no ${name}()`);
    taken.push(text);
    for (const word of text.matchAll(/[A-Za-z_][A-Za-z0-9_]*/g)) {
      if (defined.has(word[0]) && !seen.has(word[0])) queue.push(word[0]);
    }
  }
  return taken.join("\n");
}

// The stand-in gh logs each call, then answers it from the first line of $GH_PLAN whose pattern, a
// bash glob, matches the call as it names it: the role the pod_exec stub ran it for, the token and
// the config directory gh was handed, then its arguments. A line's fields, split by \x1f, are the
// pattern, the exit status, stdout and stderr (each printed with printf %b), and `once` for a line
// that answers one call alone. A call no line answers fails, naming no word of its own.
const standIn = `#!/usr/bin/env bash
subject="\${GH_ROLE:+$GH_ROLE: }\${GH_TOKEN:+[$GH_TOKEN] }\${GH_CONFIG_DIR:+<$GH_CONFIG_DIR> }$*"
printf '%s\\n' "\${subject//$'\\n'/ }" >>"$GH_LOG"
case " $* " in *" --input - "*) cat >/dev/null ;; esac
n=0
while IFS=$'\\x1f' read -r pattern status out err once; do
  n=$((n + 1))
  if [ -n "$once" ] && grep -qx "$n" "$GH_PLAN.used" 2>/dev/null; then continue; fi
  # shellcheck disable=SC2053 # the pattern is a glob
  if [[ $subject == $pattern ]]; then
    [ -z "$once" ] || echo "$n" >>"$GH_PLAN.used"
    [ -z "$out" ] || printf '%b\\n' "$out"
    [ -z "$err" ] || printf '%b\\n' "$err" >&2
    exit "$status"
  fi
done <"$GH_PLAN"
echo "stand-in gh: no line of the plan answers this call (its log names it)" >&2
exit 97
`;

interface Plan {
  call: string;
  status?: number;
  out?: string;
  err?: string;
  once?: boolean;
}

interface Ran {
  exitCode: number;
  // printed is what the site printed, stdout then stderr.
  printed: string;
  // calls are the stand-in gh's log, one call a line.
  calls: string[];
  dir: string;
  // kept is the file the site kept gh's words (err) or answer (out) in, as keep_gh names it.
  kept: (name: string, stream: "out" | "err") => string;
}

// The run's own values, as the script sets them before these sites run: the checkout (root, whose
// lib/*.jq the session readers include), its repository, project and trees, the two Apps' logins
// (read_app_logins), the role gh bundle's directories, its evidence and work directories, and the
// real gh (mise's), here the stand-in.
const values = [
  `root=${JSON.stringify(root)}`,
  "check=gh-words",
  "timeout_hook=",
  "capacity_subject=",
  "repo=acme/legion-smoke",
  "project=LEGSMOKE",
  "run_label=legsmoke",
  "tree1=LEGSMOKE-one tree2=LEGSMOKE-two tree3=LEGSMOKE-three tree4=LEGSMOKE-four child3=LEGSMOKE-seven child4=LEGSMOKE-eight",
  "pr_number=42",
  "implement_app_login='acme-implement[bot]' review_app_login='acme-review[bot]'",
  "implement_app=$implement_app_login review_app=$review_app_login",
  "service_hosts=()",
  "dispatch_default_assignee=",
  "evidence=$DIR/evidence work=$DIR/work real_gh=$DIR/bin/gh",
  assignment("gh_owners_dir"),
  assignment("gh_config_dir"),
  assignment("gh_wrapper_env"),
  assignment("listener_release_repo"),
].join("\n");

// The stubs every row runs with. A wait polls once, with its command's stdout and stderr on
// /dev/null as lib/rig.sh's until_true runs it (`"$@" >/dev/null 2>&1`), and names what it waits for
// in a note, as that one does. pod_exec runs its command here, as kubectl exec runs it in the pod:
// the pod's real gh, /usr/local/bin/gh, and the image's wrapper on PATH are both the stand-in, told
// the role. assert_gh_files is stage4b-gh-files.test.ts's.
const commonStubs = `until_true() {
  local limit=$1 what=$2
  shift 2
  note "waiting up to \${limit}s for $what"
  "$@" >/dev/null 2>&1 || fail "timed out after \${limit}s waiting for $what"
}
pod_exec() {
  local role=$2
  shift 2
  local argv=("$@")
  GH_ROLE=$role "\${argv[@]//\\/usr\\/local\\/bin\\/gh/gh}"
}
assert_gh_files() { :; }`;

interface Site {
  // entry: the script's functions the body calls.
  entry: string[];
  body: string;
  stubs?: string;
  setup?: string;
  // errexit: false for a site the teardown runs, under its `set +e`.
  errexit?: boolean;
}

function run(site: Site, plan: Plan[], files: Record<string, string> = {}): Ran {
  const dir = makeDir();
  for (const sub of ["bin", "evidence/github", "work"])
    mkdirSync(join(dir, sub), { recursive: true });
  writeFileSync(join(dir, "bin", "gh"), standIn);
  chmodSync(join(dir, "bin", "gh"), 0o755);
  const lines = plan.map((line) =>
    [line.call, line.status ?? 0, line.out ?? "", line.err ?? "", line.once ? "once" : ""].join(
      "\x1f"
    )
  );
  writeFileSync(join(dir, "plan"), `${lines.join("\n")}\n`);
  for (const [name, text] of Object.entries(files))
    writeFileSync(join(dir, "evidence", name), text);
  const stubs = `${commonStubs}\n${site.stubs ?? ""}`;
  const shell = [
    site.errexit === false ? "set -Euo pipefail" : "set -Eeuo pipefail",
    nameMissingHelpers(script),
    extract(site.entry, `${stubs}\n${site.body}`),
    stubs,
    values,
    site.setup ?? "",
    site.body,
  ].join("\n");
  const result = Bun.spawnSync(["bash", "-c", shell], {
    env: {
      PATH: `${join(dir, "bin")}:${process.env.PATH ?? ""}`,
      HOME: dir,
      TMPDIR: dir,
      DIR: dir,
      GH_PLAN: join(dir, "plan"),
      GH_LOG: join(dir, "gh.log"),
    },
  });
  const out = result.stdout.toString();
  const err = result.stderr.toString();
  expectNoMissingHelpers(out, err);
  const log = join(dir, "gh.log");
  return {
    exitCode: result.exitCode,
    printed: out + err,
    calls: existsSync(log) ? readFileSync(log, "utf8").split("\n").filter(Boolean) : [],
    dir,
    kept: (name, stream) => {
      const path = join(dir, "evidence", "github", `${name}.${stream}`);
      return existsSync(path) ? readFileSync(path, "utf8") : "";
    },
  };
}

// markerFiles lists each file of the run's evidence directory that holds a run of the marker.
function markerFiles(ran: Ran): string[] {
  const evidence = join(ran.dir, "evidence");
  return readdirSync(evidence, { recursive: true, encoding: "utf8" })
    .map((path) => join(evidence, path))
    .filter((path) => statSync(path).isFile() && holdsMarker(readFileSync(path, "utf8")));
}

// leaked asserts the row's three parts on a run whose stand-in held the marker: the site printed
// no run of it (first, so that a run before the fix fails here); the twin, a file of the evidence
// directory holds it (the one keep_gh keeps as NAME, or the one a reader of lib/workflow.sh keeps
// gh's words in itself); and what the site printed names NAMED, a file of the evidence directory
// that holds it, or by default the files keep_gh keeps as NAME.
function leaked(ran: Ran, name: string, stream: "out" | "err", named?: string): void {
  expectNoMarker(ran.printed);
  expect(markerFiles(ran)).not.toEqual([]);
  if (named === undefined) {
    expect(ran.printed).toContain(join(ran.dir, "evidence", "github", `${name}.`));
    expect(ran.kept(name, stream)).not.toBe("");
  } else {
    expect(markerFiles(ran)).toContain(join(ran.dir, "evidence", named));
    expect(ran.printed).toContain(join(ran.dir, "evidence", named));
  }
}

const said = (where: string) => `gh: HTTP 502: ${marker} (https://api.github.com/${where})`;
// Commit ids: the pull request's head, another head, and a commit the proof human made.
const head = "1".repeat(40);
const otherHead = "a".repeat(40);
const made = "c".repeat(40);

// threadsAnswer is GraphQL's answer to review_threads' query (lib/workflow.sh), one page.
interface Comment {
  author: string;
  body: string;
  review?: string;
}
function threadsAnswer(
  threads: { id: string; isResolved: unknown; comments: Comment[] }[]
): string {
  return JSON.stringify({
    data: {
      repository: {
        pullRequest: {
          reviewThreads: {
            pageInfo: { hasNextPage: false },
            nodes: threads.map((thread) => ({
              id: thread.id,
              isResolved: thread.isResolved,
              comments: {
                pageInfo: { hasNextPage: false },
                nodes: thread.comments.map((comment) => ({
                  author: { login: comment.author },
                  body: comment.body,
                  state: "SUBMITTED",
                  pullRequestReview: { state: comment.review ?? "COMMENTED" },
                })),
              },
            })),
          },
        },
      },
    },
  });
}
// threadRecord is one thread as review_thread prints it into the evidence.
function threadRecord(id: string, isResolved: unknown, comments: Comment[]): string {
  return `${JSON.stringify({
    id,
    isResolved,
    comments: comments.map((comment) => ({
      author: comment.author,
      body: comment.body,
      state: "SUBMITTED",
      reviewState: comment.review ?? "COMMENTED",
    })),
  })}\n`;
}
const reviewThreadsCall = "*pageInfo { hasNextPage } nodes { id isResolved*";

describe("read_pr_number, tree 1's pull request", () => {
  const site: Site = {
    entry: ["read_pr_number"],
    body: 'read_pr_number\necho "pull request #$pr_number"',
  };
  const call = "*pr list --head legion/LEGSMOKE-one --state open --json number*";

  test("prints no run of the marker gh's stderr holds when the list fails", () => {
    const ran = run(site, [{ call, status: 1, err: said("graphql") }]);
    leaked(ran, "pr-number", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as the number, which every later line names", () => {
    const ran = run(site, [{ call, out: marker }]);
    leaked(ran, "pr-number", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("control: reads the stand-in's number and prints it", () => {
    const ran = run(site, [{ call, out: "42" }]);
    expect(ran).toMatchObject({ exitCode: 0, printed: "pull request #42\n" });
    expect(ran.calls).toEqual([
      "-R acme/legion-smoke pr list --head legion/LEGSMOKE-one --state open --json number --jq .[0].number",
    ]);
  });
});

describe("read_smoke_file and note_tree1_moved, the file tree 1's pull request changes", () => {
  const site: Site = {
    entry: ["read_smoke_file", "note_tree1_moved"],
    body: "read_smoke_file\nnote_tree1_moved",
  };
  const call = "*--paginate repos/acme/legion-smoke/pulls/42/files*";

  test("prints no run of the marker gh's stderr holds when the read fails", () => {
    const ran = run(site, [
      { call, status: 1, err: said("repos/acme/legion-smoke/pulls/42/files") },
    ]);
    leaked(ran, "product-files", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as the file's name", () => {
    const ran = run(site, [{ call, out: marker }]);
    leaked(ran, "product-files", "out");
    expect(ran.exitCode).toBe(0);
  });

  test("control: the stand-in's file is read and named by the file that keeps it", () => {
    const ran = run(site, [{ call, out: "README.md" }]);
    expect(ran.exitCode).toBe(0);
    expect(ran.kept("product-files", "out")).toBe("README.md\n");
    expect(ran.calls).toEqual([
      "api --paginate repos/acme/legion-smoke/pulls/42/files --jq .[].filename",
    ]);
  });
});

describe("role_gh_answers, the implementer's and the reviewer's gh in their pod", () => {
  const site: Site = {
    entry: ["role_gh_answers"],
    setup: "gh_pod=pod-a",
    body: "role_gh_answers\necho OK",
  };
  const status = {
    call: "implementer: *auth status",
    out: "github.com\\n  Logged in to github.com account x-access-token (GH_CONFIG_DIR)",
  };
  const implementer = { call: "implementer: *api graphql*", out: "acme-implement[bot]" };
  const reviewer = { call: "reviewer: *api graphql*", out: "acme-review[bot]" };

  test("prints no run of the marker gh auth status says when it fails", () => {
    const ran = run(site, [{ call: status.call, status: 1, out: said("user") }]);
    leaked(ran, "auth-status", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh auth status says when it names no x-access-token account", () => {
    const ran = run(site, [
      { call: status.call, out: `Logged in to github.com account ${marker}` },
    ]);
    leaked(ran, "auth-status", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh's stderr holds when the viewer read fails", () => {
    const ran = run(site, [status, { call: implementer.call, status: 1, err: said("graphql") }]);
    leaked(ran, "viewer-pod-a-implementer", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as the viewer", () => {
    const ran = run(site, [status, implementer, { call: reviewer.call, out: marker }]);
    leaked(ran, "viewer-pod-a-reviewer", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("control: each role's gh answers its own App, and the stand-in is the gh run in each role", () => {
    const ran = run(site, [status, implementer, reviewer]);
    expect(ran).toMatchObject({ exitCode: 0, printed: "OK\n" });
    expect(ran.calls).toEqual([
      "implementer: </var/run/legion/gh/acme> auth status",
      "implementer: </var/run/legion/gh/acme> api graphql -f query={viewer{login}} --jq .data.viewer.login",
      "reviewer: </var/run/legion/gh/acme> api graphql -f query={viewer{login}} --jq .data.viewer.login",
    ]);
  });
});

describe("implementer_head_on_github, the implementer's head as GitHub records it", () => {
  const site: Site = {
    entry: ["implementer_head_on_github"],
    stubs: 'assert_handoff_record() { :; }\nrole_handoff() { printf "%s\\n" "$pr_head"; }',
    body: "implementer_head_on_github\necho OK",
  };
  const prHead = { call: "*pr view 42 --json headRefOid*", out: head };
  const committer = { call: `*commits/${head} --jq .committer.login*`, out: "acme-implement[bot]" };
  const commits = { call: "*--paginate repos/acme/legion-smoke/pulls/42/commits*", out: head };
  const files = { call: `*commits/${head} --jq .files*`, out: "README.md" };
  const message = {
    call: `*commits/${head} --jq .commit.message*`,
    out: "Append the round's line",
  };
  const checks = { call: "*check-runs --jq .total_count*", out: "3" };
  const good = [prHead, committer, commits, files, message, checks];
  const without = (left: Plan) => good.filter((line) => line !== left);

  test("prints no run of the marker gh's stderr holds when the head read fails", () => {
    const ran = run(site, [
      { call: prHead.call, status: 1, err: said("graphql") },
      ...without(prHead),
    ]);
    leaked(ran, "pr-head", "err");
  });

  test("prints no run of the marker gh answers as the head, which every later line names", () => {
    const ran = run(site, [{ call: prHead.call, out: marker }, ...without(prHead)]);
    leaked(ran, "pr-head", "out");
  });

  test("prints no run of the marker gh's stderr holds when the committer read fails", () => {
    const ran = run(site, [
      { call: committer.call, status: 1, err: said("repos") },
      ...without(committer),
    ]);
    leaked(ran, "head-committer", "err");
  });

  test("prints no run of the marker gh answers as the committer", () => {
    const ran = run(site, [{ call: committer.call, out: marker }, ...without(committer)]);
    leaked(ran, "head-committer", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh's stderr holds when the commits read fails", () => {
    const ran = run(site, [
      { call: commits.call, status: 1, err: said("pulls") },
      ...without(commits),
    ]);
    leaked(ran, "handoff-pushes", "err");
  });

  test("prints no run of the marker gh answers as a commit that changes only .legion/", () => {
    const ran = run(site, [
      { call: commits.call, out: marker },
      { call: `*commits/${marker} --jq .files*`, out: ".legion/handoff.md" },
      ...without(commits),
    ]);
    leaked(ran, "handoff-pushes", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as a message with a CI-skipping line", () => {
    const ran = run(site, [
      { call: message.call, out: `${marker} [skip ci]` },
      ...without(message),
    ]);
    leaked(ran, "head-message", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh's stderr holds when the message read fails", () => {
    const ran = run(site, [
      { call: message.call, status: 1, err: said("repos") },
      ...without(message),
    ]);
    leaked(ran, "head-message", "err");
  });

  test("prints no run of the marker gh's stderr holds when the count of check runs fails", () => {
    const ran = run(site, [
      { ...checks, once: true },
      { call: checks.call, status: 1, err: said("check-runs") },
      ...without(checks),
    ]);
    expectNoMarker(ran.printed);
    expect(holdsMarker(ran.kept("head-check-runs", "err"))).toBe(true);
  });

  test("control: GitHub's record passes, and the stand-in answered every read", () => {
    const ran = run(site, good);
    expect(ran).toMatchObject({
      exitCode: 0,
      printed: `   waiting up to 300s for a check run on the implementer's head ${head}\nOK\n`,
    });
    expect(readFileSync(join(ran.dir, "evidence", "github-credential-pushes.txt"), "utf8")).toBe(
      `${head} acme-implement[bot] the implementer's head\nimplementer head ${head}: 3 check runs; no commit of pull request #42 changes only .legion/\n`
    );
    expect(ran.calls.length).toBe(7);
  });
});

describe("gh_negative_control, gh with no config directory in the implementer's namespaces", () => {
  const site: Site = {
    entry: ["gh_negative_control"],
    setup: "gh_pod=pod-a",
    body: "gh_negative_control\necho OK",
  };
  const call = "implementer: </nonexistent> api graphql*";

  test("prints no run of the marker gh says when its words are not gh's not-logged-in message", () => {
    const ran = run(site, [{ call, status: 1, err: said("graphql") }]);
    expectNoMarker(ran.printed);
    expect(
      readFileSync(join(ran.dir, "evidence", "github-credential-negative.txt"), "utf8")
    ).toContain(marker);
    expect(ran.printed).toContain(join(ran.dir, "evidence", "github-credential-negative.txt"));
    expect(ran.exitCode).toBe(1);
  });

  test("control: gh's not-logged-in message passes, from the stand-in run as /usr/local/bin/gh", () => {
    const ran = run(site, [
      {
        call,
        status: 4,
        err: "gh: To use GitHub CLI in automation, set the GH_TOKEN environment variable.",
      },
    ]);
    expect(ran).toMatchObject({ exitCode: 0, printed: "OK\n" });
    expect(ran.calls).toEqual([
      "implementer: </nonexistent> api graphql -f query={viewer{login}} --jq .data.viewer.login",
    ]);
  });
});

describe("commit_conflict, the proof human's conflicting commit on the base", () => {
  const site: Site = { entry: ["commit_conflict"], body: "commit_conflict" };
  const base = { call: "*pulls/42 --jq .base.ref", out: "main" };
  const files = { call: "*pulls/42/files --jq*", out: "README.md" };
  const blob = { call: "*contents/*--jq .sha", out: "b".repeat(40) };
  const prHead = { call: "*pulls/42 --jq .head.sha", out: head };
  const put = { call: "*--method PUT*", out: made };
  const good = [base, files, blob, prHead, put];
  const without = (left: Plan) => good.filter((line) => line !== left);

  for (const [what, line, name] of [
    ["the base's name", base, "pr-base"],
    ["the file's name", files, "conflict-path"],
    ["the head", prHead, "conflict-head"],
    ["the commit it made", put, "conflict-commit"],
  ] as const) {
    test(`prints no run of the marker gh answers as ${what}`, () => {
      const ran = run(site, [{ call: line.call, out: marker }, ...without(line)]);
      leaked(ran, name, "out");
    });
  }

  test("prints no run of the marker gh's stderr holds when the base read fails", () => {
    const ran = run(site, [
      { call: base.call, status: 1, err: said("pulls/42") },
      ...without(base),
    ]);
    leaked(ran, "pr-base", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh's stderr holds when the commit fails", () => {
    const ran = run(site, [{ call: put.call, status: 1, err: said("contents") }, ...without(put)]);
    leaked(ran, "conflict-commit", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("control: the commit is made on the base the stand-in named and printed by its id", () => {
    const ran = run(site, good);
    expect(ran.exitCode).toBe(0);
    expect(ran.printed).toContain(
      `as ${made} while the tester's witness job ran, pull request #42 at ${head}`
    );
    expect(ran.calls).toContain(
      "api --method PUT repos/acme/legion-smoke/contents/README.md --input - --jq .commit.sha"
    );
    expect(ran.calls).toContain(
      `api repos/acme/legion-smoke/contents/README.md?ref=main --jq .sha`
    );
  });
});

describe("await_merge_forward, the head the merge forward left", () => {
  const site: Site = {
    entry: ["await_merge_forward"],
    // pr_base: the base's name, which the note before this change printed.
    setup: `conflict_head=${head} pr_base=main`,
    body: "await_merge_forward",
  };
  const call = "*pulls/42 --jq .head.sha";

  test("prints no run of the marker gh answers as the moved head", () => {
    const ran = run(site, [{ call, out: marker }]);
    leaked(ran, "head-moved", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("control: a head other than the conflicting one is the moved head, printed by its id", () => {
    const ran = run(site, [{ call, out: otherHead }]);
    expect(ran.exitCode).toBe(0);
    expect(ran.printed).toContain(`pull request #42 stands at ${otherHead}`);
    expect(ran.calls).toEqual(["api repos/acme/legion-smoke/pulls/42 --jq .head.sha"]);
  });
});

describe("no_early_approval, the review App's approvals before the round's decision", () => {
  const site: Site = { entry: ["no_early_approval"], body: "no_early_approval\necho OK" };
  const call = "*--paginate repos/acme/legion-smoke/pulls/42/reviews*";

  test("prints no run of the marker gh's stderr holds when the reviews read fails", () => {
    const ran = run(site, [{ call, status: 1, err: said("pulls/42/reviews") }]);
    leaked(ran, "early-approvals", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("control: no approval passes, and an approval fails", () => {
    expect(run(site, [{ call }])).toMatchObject({ exitCode: 0, printed: "OK\n" });
    const approved = run(site, [{ call, out: "123" }]);
    expect(approved.exitCode).toBe(1);
    expect(approved.printed).toContain(
      "the reviewer approved pull request #42 before anyone asked it"
    );
  });
});

describe("stuck_head_of_pr, the review-stuck head among the pull request's commits", () => {
  const site: Site = {
    entry: ["stuck_head_of_pr"],
    setup: `stuck_head=${head}`,
    body: "stuck_head_of_pr\necho OK",
  };
  const call = "*--paginate repos/acme/legion-smoke/pulls/42/commits*";

  test("prints no run of the marker gh's stderr holds when the commits read fails", () => {
    const ran = run(site, [{ call, status: 1, err: said("pulls/42/commits") }]);
    leaked(ran, "pr-commits", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("control: the head among the stand-in's commits passes", () => {
    const ran = run(site, [{ call, out: `${otherHead}\\n${head}` }]);
    expect(ran).toMatchObject({ exitCode: 0, printed: "OK\n" });
  });
});

describe("read_reviewer_thread, the thread the reviewer's round 1 review opened", () => {
  const site: Site = {
    entry: ["read_reviewer_thread"],
    body: 'read_reviewer_thread\necho "thread read"',
  };
  const opened = (id: string) =>
    threadsAnswer([
      {
        id,
        isResolved: false,
        comments: [
          { author: "acme-review", body: "Append the line.", review: "CHANGES_REQUESTED" },
        ],
      },
    ]);

  test("prints no run of the marker gh's stderr holds when the threads read fails", () => {
    const ran = run(site, [{ call: reviewThreadsCall, status: 1, err: said("graphql") }]);
    leaked(ran, "reviewer-thread", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as the thread's id", () => {
    const ran = run(site, [{ call: reviewThreadsCall, out: opened(marker) }]);
    leaked(ran, "reviewer-thread", "out");
    expect(ran.exitCode).toBe(0);
  });

  test("control: the one thread's id is kept, and two threads fail by their count", () => {
    const ran = run(site, [{ call: reviewThreadsCall, out: opened("PRRT_one") }]);
    expect(ran.exitCode).toBe(0);
    expect(ran.kept("reviewer-thread", "out")).toBe("PRRT_one\n");
    const two = threadsAnswer([
      {
        id: "PRRT_one",
        isResolved: false,
        comments: [{ author: "acme-review", body: "a", review: "CHANGES_REQUESTED" }],
      },
      {
        id: "PRRT_two",
        isResolved: false,
        comments: [{ author: "acme-review", body: "b", review: "CHANGES_REQUESTED" }],
      },
    ]);
    const failed = run(site, [{ call: reviewThreadsCall, out: two }]);
    expect(failed.exitCode).toBe(1);
    expect(failed.kept("reviewer-thread", "err")).toContain(
      "opened 2 CHANGES_REQUESTED review threads"
    );
  });
});

describe("thread_after_correction, the reviewer's thread after the correction round", () => {
  const site: Site = {
    entry: ["thread_after_correction"],
    setup: "thread_id=PRRT_one",
    body: "thread_after_correction",
  };
  const answered = (isResolved: unknown, newest: string) =>
    threadsAnswer([
      {
        id: "PRRT_one",
        isResolved,
        comments: [
          { author: "acme-review", body: "Append the line.", review: "CHANGES_REQUESTED" },
          { author: newest, body: "Fixed in 1234567." },
        ],
      },
    ]);

  test("prints no run of the marker gh's stderr holds when the thread read fails", () => {
    const ran = run(site, [{ call: reviewThreadsCall, status: 1, err: said("graphql") }]);
    leaked(ran, "review-thread-after-correction", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as the newest comment's author", () => {
    const ran = run(site, [{ call: reviewThreadsCall, out: answered(true, marker) }]);
    leaked(ran, "review-thread-after-correction", "out", "review-thread-after-correction.json");
  });

  test("prints no run of the marker gh answers as the thread's isResolved", () => {
    const ran = run(site, [{ call: reviewThreadsCall, out: answered(marker, "acme-implement") }]);
    leaked(ran, "review-thread-after-correction", "out", "review-thread-after-correction.json");
  });

  test("control: a resolved thread the implementer answered last is named so", () => {
    const ran = run(site, [{ call: reviewThreadsCall, out: answered(true, "acme-implement") }]);
    expect(ran.exitCode).toBe(0);
    expect(ran.printed).toBe(
      "   after the correction round the reviewer's thread reads isResolved true, newest comment by the implementer's App acme-implement\n"
    );
  });
});

describe("open_bot_thread, the proof bot's review thread", () => {
  const site: Site = {
    entry: ["open_bot_thread"],
    setup: "smoke_file=README.md",
    body: "open_bot_thread",
  };
  const prHead = { call: "*pulls/42 --jq .head.sha", out: head };
  const post = {
    call: "*--method POST repos/acme/legion-smoke/pulls/42/comments*",
    out: "C_one proof-bot[bot] Bot",
  };
  const nodeId = { call: "*nodes { id comments(first: 1)*", out: "PRRT_bot" };
  const good = [prHead, post, nodeId];
  const without = (left: Plan) => good.filter((line) => line !== left);

  test("prints no run of the marker gh answers as the thread's author, which post_bot_thread names", () => {
    const ran = run(site, [{ call: post.call, out: `C_one ${marker} User` }, ...without(post)]);
    leaked(ran, "bot-thread", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh's stderr holds when the post fails", () => {
    const ran = run(site, [
      { call: post.call, status: 1, err: said("pulls/42/comments") },
      ...without(post),
    ]);
    leaked(ran, "bot-thread", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as the first comment's id", () => {
    const ran = run(site, [
      { call: post.call, out: `${marker} proof-bot[bot] Bot` },
      ...without(post),
    ]);
    leaked(ran, "bot-thread", "out");
  });

  test("prints no run of the marker gh answers as the thread's id", () => {
    const ran = run(site, [{ call: nodeId.call, out: marker }, ...without(nodeId)]);
    leaked(ran, "bot-thread-id", "out");
  });

  test("prints no run of the marker gh's stderr holds when the thread's id read fails", () => {
    const ran = run(site, [
      { call: nodeId.call, status: 1, err: said("graphql") },
      ...without(nodeId),
    ]);
    leaked(ran, "bot-thread-id", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("control: the stand-in posts the thread and both ids are kept", () => {
    const ran = run(site, good);
    expect(ran.exitCode).toBe(0);
    expect([ran.kept("bot-thread", "out"), ran.kept("bot-thread-id", "out")]).toEqual([
      "C_one\n",
      "PRRT_bot\n",
    ]);
    expect(ran.calls).toContain(
      'api --method POST repos/acme/legion-smoke/pulls/42/comments --input - --jq "\\(.node_id) \\(.user.login) \\(.user.type)"'
    );
  });
});

// The threads as the re-review's decision leaves them and as the review-thread checkpoint reads
// them: thread_after_correction, threads_at_approval, threads_resolved and note_review_threads in
// the script's order, each thread answering its own reads. The reviewer's thread reads as the
// correction left it on its first read and as the approval left it on every later one.
const reviewerThread = (isResolved: unknown, newest: string, body = "Fixed in 1234567.") => ({
  id: "PRRT_one",
  isResolved,
  comments: [
    { author: "acme-review", body: "Append the line.", review: "CHANGES_REQUESTED" },
    { author: newest, body },
  ],
});
const botThread = (
  id: string,
  isResolved: unknown,
  body = "The change is needed: the spec asks for it."
) => ({
  id,
  isResolved,
  comments: [
    { author: "proof-bot", body: "Is this file change needed?" },
    { author: "acme-review", body },
  ],
});
const repliesCall = "*nodes { isResolved comments(first: 50)*";
const replies = { call: repliesCall, out: "true\\nacme-review\\tThe change is needed" };
const threadStubs =
  'notice_delivered() { :; }\nnotice_line() { echo \'{"customType":"envoy-message"}\'; }';

describe("threads_at_approval, both threads when the re-review decided", () => {
  const site: Site = {
    entry: ["threads_at_approval"],
    setup: "thread_id=PRRT_one bot_thread_id=PRRT_bot decision=approve",
    body: "threads_at_approval",
  };
  const both = (reviewer: unknown, bot: unknown) =>
    threadsAnswer([reviewerThread(reviewer, "acme-review"), botThread("PRRT_bot", bot)]);

  test("prints no run of the marker gh answers in a thread when the decision was not an approval", () => {
    const ran = run(
      { ...site, setup: "thread_id=PRRT_one bot_thread_id=PRRT_bot decision=changes" },
      [
        {
          call: reviewThreadsCall,
          out: threadsAnswer([
            reviewerThread(true, "acme-review", marker),
            botThread("PRRT_bot", true),
          ]),
        },
      ]
    );
    expectNoMarker(ran.printed);
    expect(
      readFileSync(join(ran.dir, "evidence", "review-thread-at-approval.json"), "utf8")
    ).toContain(marker);
    expect(ran.printed).toContain(join(ran.dir, "evidence", "review-thread-at-approval.json"));
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as a thread's isResolved", () => {
    const ran = run(site, [{ call: reviewThreadsCall, out: both(marker, true) }]);
    leaked(ran, "review-thread-at-approval", "out", "review-thread-at-approval.json");
  });

  test("prints no run of the marker gh's stderr holds when a thread read fails", () => {
    const ran = run(site, [{ call: reviewThreadsCall, status: 1, err: said("graphql") }]);
    leaked(ran, "review-thread-at-approval", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("control: an approval with both threads resolved notes both", () => {
    const ran = run(site, [{ call: reviewThreadsCall, out: both(true, true) }]);
    expect(ran).toMatchObject({
      exitCode: 0,
      printed:
        "   the reviewer approved pull request #42 at its head; its thread then read isResolved true, the bot's isResolved true\n",
    });
  });
});

describe("threads_resolved, the review-thread checkpoint's three thread records", () => {
  const site: Site = {
    entry: ["threads_resolved"],
    stubs: threadStubs,
    setup: "thread_id=PRRT_one bot_thread_id=PRRT_bot bot_thread=C_one",
    body: "threads_resolved\necho OK",
  };
  const records = (afterCorrection: string, atApproval: string) => ({
    "review-thread-after-correction.json": afterCorrection,
    "review-thread-at-approval.json": atApproval,
  });
  const goodRecords = records(
    threadRecord("PRRT_one", true, reviewerThread(true, "acme-implement").comments),
    threadRecord("PRRT_one", true, reviewerThread(true, "acme-review").comments)
  );
  const botAnswer = (id = "PRRT_bot", isResolved: unknown = true, body?: string) => ({
    call: reviewThreadsCall,
    out: threadsAnswer([reviewerThread(true, "acme-review"), botThread(id, isResolved, body)]),
  });

  test("prints no run of the marker gh answered in the thread after the correction when it fails the rule", () => {
    const ran = run(
      site,
      [replies, botAnswer()],
      records(
        threadRecord("PRRT_one", false, reviewerThread(false, "acme-implement", marker).comments),
        goodRecords["review-thread-at-approval.json"]
      )
    );
    expectNoMarker(ran.printed);
    expect(ran.printed).toContain(join(ran.dir, "evidence", "review-thread-after-correction.json"));
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answered in the thread at the approval when it fails the rule", () => {
    const ran = run(
      site,
      [replies, botAnswer()],
      records(
        goodRecords["review-thread-after-correction.json"],
        threadRecord("PRRT_one", true, reviewerThread(true, "acme-implement", marker).comments)
      )
    );
    expectNoMarker(ran.printed);
    expect(ran.printed).toContain(join(ran.dir, "evidence", "review-thread-at-approval.json"));
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answered as the bot's thread's id", () => {
    const ran = run(
      { ...site, setup: `thread_id=PRRT_one bot_thread_id=${marker} bot_thread=C_one` },
      [replies, botAnswer(marker)],
      goodRecords
    );
    expectNoMarker(ran.printed);
    expect(readFileSync(join(ran.dir, "evidence", "bot-thread-resolved.json"), "utf8")).toContain(
      marker
    );
    expect(ran.exitCode).toBe(0);
  });

  test("prints no run of the marker gh's stderr holds when the bot's thread read fails", () => {
    const ran = run(
      site,
      [replies, { call: reviewThreadsCall, status: 1, err: said("graphql") }],
      goodRecords
    );
    leaked(ran, "bot-thread-resolved", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers in the bot's thread when it fails the rule", () => {
    const ran = run(site, [replies, botAnswer("PRRT_bot", false, marker)], goodRecords);
    leaked(ran, "bot-thread-resolved", "out", "bot-thread-resolved.json");
    expect(ran.exitCode).toBe(1);
  });

  test("control: three records that keep the rule pass, the bot's read from the stand-in", () => {
    const ran = run(site, [replies, botAnswer()], goodRecords);
    expect(ran.exitCode).toBe(0);
    expect(ran.printed).toEndWith("OK\n");
    expect(ran.calls.length).toBe(2);
  });
});

describe("note_review_threads after the checkpoint's reads, in the script's order", () => {
  const site: Site = {
    entry: [
      "thread_after_correction",
      "threads_at_approval",
      "threads_resolved",
      "note_review_threads",
    ],
    stubs: threadStubs,
    setup:
      "thread_id=PRRT_one bot_thread_id=PRRT_bot bot_thread=C_one decision=approve threads_code=404",
    body: "thread_after_correction\nthreads_at_approval\nthreads_resolved\nnote_review_threads",
  };
  const plan = (body: string) => [
    replies,
    {
      call: reviewThreadsCall,
      once: true,
      out: threadsAnswer([
        reviewerThread(true, "acme-implement", body),
        botThread("PRRT_bot", false),
      ]),
    },
    {
      call: reviewThreadsCall,
      out: threadsAnswer([
        reviewerThread(true, "acme-review", body),
        botThread("PRRT_bot", true, body),
      ]),
    },
  ];

  test("prints no run of the marker gh answers as the threads' comments", () => {
    const ran = run(site, plan(`Fixed in 1234567: ${marker}`));
    expectNoMarker(ran.printed);
    expect(readFileSync(join(ran.dir, "evidence", "bot-thread-resolved.json"), "utf8")).toContain(
      marker
    );
    expect(ran.exitCode).toBe(0);
  });

  test("control: the checkpoint's reads pass on the stand-in's threads and the note names their files", () => {
    const ran = run(site, plan("Fixed in 1234567."));
    expect(ran.exitCode).toBe(0);
    expect(ran.printed).toContain(
      `the reviewer's thread: resolved by the implementer after the correction (${join(ran.dir, "evidence", "review-thread-after-correction.json")})`
    );
    expect(ran.calls.length).toBe(5);
  });
});

describe("refreshed_token_answers, the architect's gh and tokens after the refresh", () => {
  const site: Site = {
    entry: ["refreshed_token_answers"],
    setup: [
      "gh_pod=pod-a gh_before_token=ghs_oldtoken gh_after_token=ghs_newtoken before_expiry_s=0",
      "before_expiry=2026-11-24T09:00:00Z refreshed_at=2026-11-24T08:50:00Z propagated_at=2026-11-24T08:51:00Z propagation=60",
    ].join("\n"),
    body: "refreshed_token_answers",
  };
  const viewer = { call: "architect: *api graphql*", out: "acme-review[bot]" };
  const old = {
    call: "*ghs_oldtoken*api graphql*",
    status: 1,
    err: "gh: Bad credentials (HTTP 401)",
  };
  const fresh = { call: "*ghs_newtoken*api graphql*", out: "acme-review[bot]" };
  const good = [viewer, old, fresh];
  const without = (left: Plan) => good.filter((line) => line !== left);

  test("prints no run of the marker gh's stderr holds when the viewer read fails", () => {
    const ran = run(site, [
      { call: viewer.call, status: 1, err: said("graphql") },
      ...without(viewer),
    ]);
    leaked(ran, "viewer-pod-a-architect", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as the viewer", () => {
    const ran = run(site, [{ call: viewer.call, out: marker }, ...without(viewer)]);
    leaked(ran, "viewer-pod-a-architect", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers when the expired token still answers", () => {
    const ran = run(site, [{ call: old.call, out: marker }, ...without(old)]);
    leaked(ran, "old-token", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh says when it refuses the expired token for another reason", () => {
    const ran = run(site, [{ call: old.call, status: 1, err: said("graphql") }, ...without(old)]);
    leaked(ran, "old-token", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh's stderr holds when the refreshed token's read fails", () => {
    const ran = run(site, [
      { call: fresh.call, status: 1, err: said("graphql") },
      ...without(fresh),
    ]);
    leaked(ran, "new-token", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers for the refreshed token", () => {
    const ran = run(site, [{ call: fresh.call, out: marker }, ...without(fresh)]);
    leaked(ran, "new-token", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("control: each token reaches the stand-in in gh's environment, and the run says what each answered", () => {
    const ran = run(site, good);
    expect(ran.exitCode).toBe(0);
    expect(ran.printed).toContain(
      "the token read at issue-independence answers 401 and the refreshed one acme-review[bot]"
    );
    expect(ran.calls.slice(1)).toEqual([
      `[ghs_oldtoken] <${join(ran.dir, "work", "no-gh")}> api graphql -f query={viewer{login}}`,
      `[ghs_newtoken] <${join(ran.dir, "work", "no-gh")}> api graphql -f query={viewer{login}} --jq .data.viewer.login`,
    ]);
  });
});

describe("operator_controller_gh, the operator-launched controller's gh", () => {
  const site: Site = {
    entry: ["operator_controller_gh"],
    setup: "controller_gh=$DIR/controller-gh",
    body: 'operator_controller_gh\necho "state=$state viewer=$viewer"',
  };
  const state = { call: "<*/controller-gh> pr view 42*", out: "OPEN" };
  const viewer = { call: "<*/controller-gh> api graphql*", out: "acme-review[bot]" };
  const nowhere = {
    call: "</nonexistent> pr view 42*",
    status: 4,
    err: "To get started with GitHub CLI, please run:  gh auth login",
  };
  const good = [state, viewer, nowhere];
  const without = (left: Plan) => good.filter((line) => line !== left);

  test("prints no run of the marker gh's stderr holds when the state read fails", () => {
    const ran = run(site, [
      { call: state.call, status: 1, err: said("graphql") },
      ...without(state),
    ]);
    leaked(ran, "controller-state", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as the pull request's state", () => {
    const ran = run(site, [{ call: state.call, out: marker }, ...without(state)]);
    leaked(ran, "controller-state", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh's stderr holds when the viewer read fails", () => {
    const ran = run(site, [
      { call: viewer.call, status: 1, err: said("graphql") },
      ...without(viewer),
    ]);
    leaked(ran, "controller-viewer", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as the viewer", () => {
    const ran = run(site, [{ call: viewer.call, out: marker }, ...without(viewer)]);
    leaked(ran, "controller-viewer", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers from no config directory", () => {
    const ran = run(site, [{ call: nowhere.call, out: marker }, ...without(nowhere)]);
    leaked(ran, "controller-nonexistent", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("control: the stand-in answers from the controller's directory and not from none", () => {
    const ran = run(site, good);
    expect(ran).toMatchObject({ exitCode: 0, printed: "state=OPEN viewer=acme-review[bot]\n" });
    expect(ran.calls).toEqual([
      `<${join(ran.dir, "controller-gh")}> pr view 42 -R acme/legion-smoke --json state -q .state`,
      `<${join(ran.dir, "controller-gh")}> api graphql -f query={viewer{login}} --jq .data.viewer.login`,
      "</nonexistent> pr view 42 -R acme/legion-smoke --json state -q .state",
    ]);
  });
});

describe("daemon_controller_gh, the daemon-launched controller's gh", () => {
  const site: Site = {
    entry: ["daemon_controller_gh"],
    setup: `pod=pod-c claim='{"locator":{"sandbox":{"container":"controller"}}}'`,
    body: 'daemon_controller_gh\necho "state=$state viewer=$viewer container=$container"',
  };
  const state = { call: "controller: </var/run/legion/gh/acme> pr view 42*", out: "MERGED" };
  const viewer = {
    call: "controller: </var/run/legion/gh/acme> api graphql*",
    out: "acme-review[bot]",
  };
  const nowhere = {
    call: "controller: </nonexistent> pr view 42*",
    status: 4,
    err: "To get started with GitHub CLI, please run:  gh auth login",
  };
  const good = [state, viewer, nowhere];
  const without = (left: Plan) => good.filter((line) => line !== left);

  test("prints no run of the marker gh's stderr holds when the state read fails", () => {
    const ran = run(site, [
      { call: state.call, status: 1, err: said("graphql") },
      ...without(state),
    ]);
    leaked(ran, "daemon-controller-state", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as the pull request's state", () => {
    const ran = run(site, [{ call: state.call, out: marker }, ...without(state)]);
    leaked(ran, "daemon-controller-state", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh's stderr holds when the viewer read fails", () => {
    const ran = run(site, [
      { call: viewer.call, status: 1, err: said("graphql") },
      ...without(viewer),
    ]);
    leaked(ran, "viewer-pod-c-controller", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as the viewer", () => {
    const ran = run(site, [{ call: viewer.call, out: marker }, ...without(viewer)]);
    leaked(ran, "viewer-pod-c-controller", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers from no config directory", () => {
    const ran = run(site, [{ call: nowhere.call, out: marker }, ...without(nowhere)]);
    leaked(ran, "daemon-controller-nonexistent", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("control: the stand-in, run in the controller's container, answers from its owner's directory alone", () => {
    const ran = run(site, good);
    expect(ran).toMatchObject({
      exitCode: 0,
      printed: "state=MERGED viewer=acme-review[bot] container=controller\n",
    });
    expect(ran.calls).toEqual([
      "controller: </var/run/legion/gh/acme> pr view 42 -R acme/legion-smoke --json state -q .state",
      "controller: </var/run/legion/gh/acme> api graphql -f query={viewer{login}} --jq .data.viewer.login",
      "controller: </nonexistent> pr view 42 -R acme/legion-smoke --json state -q .state",
    ]);
  });
});

describe("threads_at_merge, every thread resolved before the merger acts", () => {
  const site: Site = {
    entry: ["threads_at_merge"],
    setup: "thread_id=PRRT_one bot_thread_id=PRRT_bot",
    body: "threads_at_merge",
  };
  const mergeHead = { call: "*pulls/42 --jq .head.sha", out: head };
  const threads = (
    reviewer = reviewerThread(true, "acme-review"),
    bot = botThread("PRRT_bot", true)
  ) => ({
    call: reviewThreadsCall,
    out: threadsAnswer([reviewer, bot]),
  });

  test("prints no run of the marker gh answers as the head", () => {
    const ran = run(site, [{ call: mergeHead.call, out: marker }, threads()]);
    leaked(ran, "merge-head", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh's stderr holds when the head read fails", () => {
    const ran = run(site, [{ call: mergeHead.call, status: 1, err: said("pulls/42") }, threads()]);
    leaked(ran, "merge-head", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh's stderr holds when the threads read fails", () => {
    const ran = run(site, [
      mergeHead,
      { call: reviewThreadsCall, status: 1, err: said("graphql") },
    ]);
    leaked(ran, "review-threads-at-merge", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as an unresolved thread's newest author", () => {
    const ran = run(site, [mergeHead, threads(reviewerThread(false, marker))]);
    leaked(ran, "review-threads-at-merge", "out", "review-threads-at-merge.json");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as the threads' ids", () => {
    const ran = run({ ...site, setup: `thread_id=${marker} bot_thread_id=PRRT_bot` }, [
      mergeHead,
      threads({ ...reviewerThread(true, "acme-review"), id: marker }),
    ]);
    leaked(ran, "review-threads-at-merge", "out", "review-threads-at-merge.json");
    expect(ran.exitCode).toBe(0);
  });

  test("control: two resolved threads at the head pass, and the control of an unresolved one fails", () => {
    const ran = run(site, [mergeHead, threads()]);
    expect(ran.exitCode).toBe(0);
    expect(ran.printed).toBe(
      "   negative control unresolved-thread-at-merge rejected the deliberately broken observation\n" +
        `   pull request #42 stands at ${head} with 2 review threads, 0 unresolved, the reviewer's and the bot's among them (${join(ran.dir, "evidence", "review-threads-at-merge.json")})\n`
    );
  });
});

describe("merged_pr_commits, the merged pull request's commits", () => {
  const site: Site = { entry: ["merged_pr_commits"], body: "merged_pr_commits" };
  const commits = { call: "*--paginate repos/acme/legion-smoke/pulls/42/commits*", out: head };
  const files = { call: `*commits/${head} --jq .files*`, out: "README.md" };

  test("prints no run of the marker gh's stderr holds when the commits read fails", () => {
    const ran = run(site, [
      { call: commits.call, status: 1, err: said("pulls/42/commits") },
      files,
    ]);
    leaked(ran, "merged-handoff-pushes", "err");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh answers as a commit that changes only .legion/", () => {
    const ran = run(site, [
      { call: commits.call, out: marker },
      { call: `*commits/${marker} --jq .files*`, out: ".legion/handoff.md" },
    ]);
    leaked(ran, "merged-handoff-pushes", "out");
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker gh's stderr holds when the count of commits fails", () => {
    const ran = run(site, [
      { ...commits, once: true },
      files,
      { call: commits.call, status: 1, err: said("pulls/42/commits") },
    ]);
    expectNoMarker(ran.printed);
    expect(holdsMarker(ran.kept("merged-commits", "err"))).toBe(true);
  });

  test("control: the stand-in's one commit is counted", () => {
    const ran = run(site, [commits, files]);
    expect(ran).toMatchObject({
      exitCode: 0,
      printed: "   merged pull request #42 holds 1 commits, none changing only .legion/\n",
    });
  });
});

// The teardown's own reads. Its cleanup pull request's (close_smoke_cleanup) is lib/workflow.sh's
// to keep, so smoke_cleanup_branch is empty here and that function calls no gh.
describe("remove_run_branches, the teardown's pull requests and branches", () => {
  const site: Site = {
    entry: ["remove_run_branches"],
    errexit: false,
    setup: "proof_human='proof-bot[bot]' smoke_cleanup_branch= smoke_cleanup_url=",
    body: "remove_run_branches",
  };
  const others = { call: "*pr list --head legion/*" };
  const close = { call: "*pr close*" };
  const deleteRef = { call: "*api -X DELETE*", status: 1 };

  test("prints no run of the marker gh answers as the number of the pull request it closes", () => {
    const ran = run(site, [
      { call: "*pr list --head legion/LEGSMOKE-one *", out: marker },
      others,
      close,
      deleteRef,
    ]);
    leaked(ran, "teardown-pr-LEGSMOKE-one", "out");
  });

  test("control: the stand-in's open pull request is closed and named by its number", () => {
    const ran = run(site, [
      { call: "*pr list --head legion/LEGSMOKE-one *", out: "7" },
      others,
      close,
      deleteRef,
    ]);
    expect(ran.exitCode).toBe(0);
    expect(ran.printed).toBe(
      "   closed the run's open pull request acme/legion-smoke#7 (legion/LEGSMOKE-one)\n"
    );
    expect(ran.calls).toContain(
      "-R acme/legion-smoke pr close 7 --comment Closed by the Stage 4b run that opened it, at its teardown."
    );
    expect(ran.calls.length).toBe(9);
  });
});

describe("note_listener_restarts, the listener release that spans a restart", () => {
  const site: Site = {
    entry: ["note_listener_restarts"],
    stubs:
      'listener_restarts() { echo \'[{"start":"2026-11-24T10:00:00Z","end":"2026-11-24T10:01:00Z","session":"s1","samples":3}]\'; }',
    body: "note_listener_restarts",
  };
  const call = "*run list -R cybermonklabs/legion --workflow Release Envoy Listener*";
  const release = (databaseId: unknown) =>
    JSON.stringify([
      {
        databaseId,
        headSha: head,
        createdAt: "2026-11-24T09:00:00Z",
        updatedAt: "2026-11-24T11:00:00Z",
      },
    ]);

  test("prints no run of the marker gh answers as a run's id", () => {
    const ran = run(site, [{ call, out: release(marker) }]);
    leaked(ran, "listener-releases", "out");
  });

  test("prints no run of the marker jq would quote from an answer it cannot read", () => {
    const ran = run(site, [{ call, out: JSON.stringify(marker) }]);
    expectNoMarker(ran.printed);
    expect(markerFiles(ran)).toEqual([
      join(ran.dir, "evidence", "github", "listener-releases.out"),
    ]);
    expect(ran.printed).toContain("release: none known");
  });

  test("control: the stand-in's run is named by its id and commit", () => {
    const ran = run(site, [{ call, out: release(123) }]);
    expect(ran.printed).toContain(`release: run 123 (${head.slice(0, 10)})`);
    expect(ran.calls).toEqual([
      "run list -R cybermonklabs/legion --workflow Release Envoy Listener --limit 30 --json databaseId,headSha,createdAt,updatedAt",
    ]);
  });
});

// production_audit reads the run's pull requests (run_pull_request_numbers) to judge its sessions'
// interests (interests_outside, whose jq is handed that answer), and the run prints its verdict as
// the teardown prints it (cleanup), naming the audit's files (audit_failure). The audit's other
// reads, of Dispatch and of the interest samples' outcomes, are stubbed to find nothing outside the
// run; the samples themselves are the evidence's interests.jsonl.
describe("production_audit's read of the run's pull requests (run_pull_request_numbers)", () => {
  const site: Site = {
    entry: ["production_audit"],
    stubs:
      "touched_outside() { echo '[]'; }\ninterests_unanswered() { echo '[]'; }\nlistener_restarts() { echo '[]'; }",
    setup: [
      assignment("registered_jq"),
      "daemon_log=$evidence/daemon.log dispatch_actor=proof-human audited= optree=LEGSMOKE-optree opchild=LEGSMOKE-opchild",
    ].join("\n"),
    body: 'production_audit || echo "CHECK production-audit: FAIL: $(audit_failure)"',
  };
  const call = "*pr list --state all --limit 1000*";
  // A sample of a session following a pull request on the smoke repository's GitHub subjects, which
  // interests_outside judges by the pull request numbers.
  const following = {
    "daemon.log": "",
    "interests.jsonl": `${JSON.stringify({ session_id: "s1", topics: ["notifications.github.acme.legion-smoke.pr.5"] })}\n`,
  };
  const outside = (ran: Ran) => join(ran.dir, "evidence", "production-interests-outside.json");
  // reported: the audit's failure names its file, which names the file gh's words are kept in.
  const reported = (ran: Ran, kept: string) => {
    expect(ran.printed).toContain(
      "CHECK production-audit: FAIL: the run wrote outside LEGSMOKE or subscribed outside it: "
    );
    expect(ran.printed).toContain(outside(ran));
    expect(readFileSync(outside(ran), "utf8")).toContain(join(ran.dir, "evidence", "github", kept));
  };

  test("prints no run of the marker gh's stderr holds when the list fails", () => {
    const ran = run(site, [{ call, status: 1, err: said("graphql") }], following);
    expectNoMarker(ran.printed);
    expect(markerFiles(ran)).toEqual([
      join(ran.dir, "evidence", "github", "run-pull-requests.err"),
    ]);
    reported(ran, "run-pull-requests.err");
  });

  // The answer goes nowhere but interests_outside's jq, whose error over a value it cannot index
  // names the value's type and the key, never the value: this row holds at the base too, and is the
  // evidence that the answer cannot reach the output. The twin: jq did fail over it.
  test("an answer that is no array of numbers reaches jq alone, whose error prints no run of it", () => {
    const ran = run(site, [{ call, out: JSON.stringify(marker) }], following);
    expectNoMarker(ran.printed);
    expect(ran.printed).toContain("Cannot index string with number");
    expect(readFileSync(outside(ran), "utf8")).toBe('"the interest samples could not be read"\n');
  });

  test("control: the stand-in's pull requests are read, and a sample of one of them passes the audit", () => {
    const ran = run(site, [{ call, out: "[1,5]" }], following);
    expect(ran).toMatchObject({ exitCode: 0, printed: "" });
    expect(ran.calls).toEqual([
      '-R acme/legion-smoke pr list --state all --limit 1000 --json number,headRefName --jq [.[] | select(.headRefName | startswith("legion/LEGSMOKE-")) | .number]',
    ]);
    const other = run(site, [{ call, out: "[1,2]" }], following);
    expect(other.printed).toContain("CHECK production-audit: FAIL");
    expect(readFileSync(outside(other), "utf8")).toContain(
      "notifications.github.acme.legion-smoke.pr.5"
    );
  });
});

// full-agent-tools: each role runs `gh api graphql` in its own bash, and its FullAgentGh subagent
// runs it in its bash and answers in its task result, so the role's session records gh's answer.
// Whatever answers gh in GitHub's place holds the role's token. The session a row hands the site is
// that record, the answer holding the marker where gh's answer stands: no stand-in gh answers here,
// since the gh call is the agent's and the script reads only its session. full_agent_role_checks
// takes the role's excerpt from its claim's session, from the instruction's delivery on, into
// $evidence/full-agent-tools/<role>.jsonl, and the subagent's session beside it.
interface ToolCall {
  name: string;
  args: Record<string, unknown>;
  text: string;
}
// sessionOf is a session as Oh My Pi records it: the instruction's delivery carrying NONCE, then each
// call and the result that answers it.
function sessionOf(nonce: string, calls: ToolCall[]): string {
  const user = {
    role: "user",
    content: [{ type: "text", text: `full-agent-tools operation ${nonce}` }],
  };
  const lines = [JSON.stringify({ type: "message", message: user })];
  calls.forEach((call, i) => {
    const id = `toolu_${i}`;
    const content = [{ type: "toolCall", id, name: call.name, arguments: call.args }];
    lines.push(JSON.stringify({ type: "message", message: { role: "assistant", content } }));
    const result = { role: "toolResult", toolName: call.name, toolCallId: id, isError: false };
    lines.push(
      JSON.stringify({
        type: "message",
        message: { ...result, content: [{ type: "text", text: call.text }] },
      })
    );
  });
  return `${lines.join("\n")}\n`;
}

describe("full_agent_role_checks, full_agent_common_asserts and full_agent_summary, a role's own gh", () => {
  const nonce = "fa-architect";
  // The architect acts as the review App, and the implement App is the other one.
  const app = "acme-review[bot]";
  const other = "acme-implement[bot]";
  const graphql = "gh api graphql -f query='{ viewer { login } }' --jq .data.viewer.login";
  const chain =
    "go version && bun --version && python3 --version && curl --version | head -1 && gh --version | head -1 && git --version";
  const versions =
    "go version go1.25.1 linux/amd64\n1.4.4\nPython 3.12.3\ncurl 8.5.0\ngh version 2.98.0\ngit version 2.45.2";
  // role is the role's calls as the operation asks for them, ANSWER gh's answer to its graphql call,
  // EXTRA calls before the rest, and TOOLCHAIN and TASK the chain's and the subagent's results.
  const role = ({ answer = app, extra = [] as ToolCall[], toolchain = versions, task = "" } = {}) =>
    sessionOf(nonce, [
      ...extra,
      { name: "eval", args: { language: "py" }, text: `${nonce} py 42` },
      { name: "eval", args: { language: "js" }, text: `${nonce} js 42` },
      { name: "eval", args: { language: "js" }, text: `title: full-agent-tools ${nonce}` },
      {
        name: "web_search",
        args: { query: "Jujutsu version control" },
        text: "https://jj-vcs.github.io/jj/",
      },
      { name: "bash", args: { command: chain }, text: toolchain },
      {
        name: "edit",
        args: { input: "/tmp/full-agent-tools-architect.html" },
        text: `edited ${nonce}`,
      },
      {
        name: "read",
        args: { path: "/tmp/full-agent-tools-architect.html" },
        text: `<title>edited ${nonce}</title>`,
      },
      { name: "bash", args: { command: graphql }, text: answer },
      {
        name: "task",
        args: { agent: "FullAgentGh" },
        text: task || `<task-result id="FullAgentGh">${app}</task-result>`,
      },
    ]);
  // subagent is the FullAgentGh subagent's session: its bash answered ANSWER and its yield was taken.
  const subagent = (answer = app) =>
    sessionOf(nonce, [
      { name: "bash", args: { command: graphql }, text: answer },
      { name: "yield", args: {}, text: "accepted" },
    ]);
  const site: Site = {
    entry: ["full_agent_role_checks"],
    stubs: [
      'claim_session_text() { cat "$evidence/claim-$2.jsonl"; }',
      "claim_session_file() { printf '%s\\n' \"$evidence/sessions/$2.jsonl\"; }",
      'session_text() { cat "$evidence/subagent.jsonl"; }',
    ].join("\n"),
    setup: [
      'mkdir -p "$evidence/full-agent-tools"',
      `declare -A fa_nonce=([architect]=${nonce})`,
      // The loop's variable, which the role's checks read before they were a function of their own.
      "role=architect",
    ].join("\n"),
    body: "full_agent_role_checks architect",
  };
  const files = (claim: string, sub = subagent()) => ({
    "claim-architect.jsonl": claim,
    "subagent.jsonl": sub,
  });
  const excerpt = "full-agent-tools/architect.jsonl";

  test("prints no run of the marker gh answers the role's graphql call in place of its App", () => {
    const ran = run(site, [], files(role({ answer: marker })));
    leaked(ran, "", "out", excerpt);
    expect(ran.exitCode).toBe(1);
  });

  test("prints no run of the marker a bash result carries beside the other App", () => {
    const ran = run(
      site,
      [],
      files(
        role({
          extra: [{ name: "bash", args: { command: "gh pr view 42" }, text: `${other} ${marker}` }],
        })
      )
    );
    leaked(ran, "", "out", excerpt);
    expect(ran.printed).toContain(`a bash result carries ${other}, the other App`);
  });

  test("prints no run of the marker gh answered when the toolchain's result lacks a version", () => {
    const ran = run(
      site,
      [],
      files(role({ answer: `${app} ${marker}`, toolchain: "go version go1.25.1" }))
    );
    leaked(ran, "", "out", excerpt);
    expect(ran.printed).toContain("carries 'Python 3'");
  });

  test("prints no run of the marker the subagent's task result answers in place of the App", () => {
    const ran = run(
      site,
      [],
      files(role({ task: `<task-result id="FullAgentGh">${marker}</task-result>` }))
    );
    leaked(ran, "", "out", excerpt);
    expect(ran.printed).toContain("no task result carries");
  });

  test("prints no run of the marker a task result carries beside the App but without its block", () => {
    const ran = run(site, [], files(role({ task: `${app} ${marker}` })));
    leaked(ran, "", "out", excerpt);
    expect(ran.printed).toContain("holds no <task-result id=…> block");
  });

  test("prints no run of the marker the subagent's own gh answered beside the other App", () => {
    const ran = run(site, [], files(role(), subagent(`${other} ${marker}`)));
    leaked(ran, "", "out", "full-agent-tools/architect-FullAgentGh.jsonl");
    expect(ran.printed).toContain('{"login":false,"yield":true,"other":true}');
  });

  test("prints no run of the marker gh answered beside the App in the role's summary", () => {
    const ran = run(site, [], files(role({ answer: `${app} ${marker}` })));
    expectNoMarker(ran.printed);
    expect(markerFiles(ran)).toContain(join(ran.dir, "evidence", excerpt));
    expect(ran.printed).toContain(`login ${app}; browser`);
    expect(ran.exitCode).toBe(0);
  });

  test("prints no run of the marker a bash result before the toolchain's carries beside its first word", () => {
    const extra = [
      { name: "bash", args: { command: "gh api user" }, text: `go version ${marker}` },
    ];
    const ran = run(site, [], files(role({ extra })));
    expectNoMarker(ran.printed);
    expect(markerFiles(ran)).toContain(join(ran.dir, "evidence", excerpt));
    expect(ran.printed).toContain("toolchain go version go1.25.1 linux/amd64 1.4.4 Python 3.12.3");
  });

  test("control: the role's operation passes, and its summary names its App and its toolchain", () => {
    const ran = run(site, [], files(role()));
    expect(ran).toMatchObject({
      exitCode: 0,
      printed:
        `   architect as ${app}: login ${app}; browser title: full-agent-tools ${nonce}; toolchain ` +
        "go version go1.25.1 linux/amd64 1.4.4 Python 3.12.3 curl 8.5.0 gh version 2.98.0 git version 2.45.2; " +
        `search https://jj-vcs.github.io/jj/; its subagent FullAgentGh answered ${app} and yielded\n`,
    });
    // The control's twin: the role's graphql answer without the App fails the role.
    expect(run(site, [], files(role({ answer: other }))).exitCode).toBe(1);
  });
});
