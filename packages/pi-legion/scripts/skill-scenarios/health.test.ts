import { describe, expect, setDefaultTimeout, test } from "bun:test";
import { appendFileSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { ANSWERS, labelOf, SKIPS, scenarioOf } from "./completion-answers";
import {
  credentialPrinted,
  ghUndeclared,
  loadRun,
  mounted,
  namesOf,
  namesSaid,
  offWorldAllowed,
  offWorldReach,
  ownNames,
  serviceProvenance,
  unfinished,
  unmergedHead,
  unmodelledWrite,
  untaken,
  writtenBy,
} from "./health";
import { labelPersonRoutes } from "./person-routes";
import {
  casesOf,
  REMOVED_NAMES,
  systemPromptOf,
  vocabulary,
  WORLD_REMOTE_NAMES,
  WORLD_REMOTE_VCS,
} from "./run-names";
import { writeGhSelectors } from "./test-programs";
import { WORLD_REPO } from "./world-issue";

// The rows run the rig's programs as real processes, which a loaded pod slows past bun's 5 s
// default, so each gets a minute, as rig.test.ts's do.
setDefaultTimeout(60_000);

/** The world of a run whose pane names only the loopback. */
const LOOPBACK = new Set(["127.0.0.1", "localhost", "::1"]);
const SESSION = "sessions/2026-11-14T00-00-00-000Z_s1.jsonl";
const API = "https://api.github.com/repos/cybermonklabs/legion";
const LINK = "https://github.com/cybermonklabs/legion/pull/2827";
/** The run's own directory, whose bin/gh is its gh stand-in. */
const RUN = "/work/runs/threads-closed-correct-head-1";

/** Rule 13's reason for an agent of the run RUN that made `steps`, one tool call each, in that
 * order. */
function reach(...steps: [tool: string, args: Record<string, unknown>][]): string | undefined {
  return offWorldReach(
    steps.map(([tool, args], at) => ({ file: SESSION, tool, args, at })),
    LOOPBACK,
    [RUN]
  );
}
const bash = (command: string): [string, Record<string, unknown>] => ["bash", { command }];
const write = (path: string, content: string): [string, Record<string, unknown>] => [
  "write",
  { path, content },
];
const FETCH_PY = `import urllib.request\nprint(urllib.request.urlopen('${API}/pulls/2827').read())\n`;
/** `git` in `dir`, under no configuration but the identity a commit needs. */
const git = (dir: string, ...args: string[]) => {
  const ran = Bun.spawnSync(
    ["git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-C", dir, ...args],
    { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_NOSYSTEM: "1" } }
  );
  return { code: ran.exitCode, out: ran.stdout.toString().trim(), err: ran.stderr.toString() };
};

// Catches: a here-document body read as text whatever reads it, so a Python or a shell program
// given on stdin reaches GitHub unread; and the body of a command that writes text (a file, a
// comment's --body-file, a commit message) or reads data (jq, `python3 -c`) read as a reach.
describe("health: rule 13, off-world network, here-documents (door 2)", () => {
  test("a here-document python3 runs as its program is off-world", () => {
    expect(
      reach(bash(`python3 - <<'EOF'\nimport urllib.request; urllib.request.urlopen('${API}')\nEOF`))
    ).toBe(
      `off-world network: a bash call in ${SESSION} reaches api.github.com (${API}) in a here-document python3 runs, no host of the run's world (127.0.0.1, localhost, ::1)`
    );
  });

  test("a here-document a bare cat pipes into node is off-world", () => {
    expect(reach(bash(`cat <<'EOF' | node\nawait fetch("${API}");\nEOF`))).toContain(
      "reaches api.github.com"
    );
  });

  test("a here-document a shell reads on stdin is read as a shell command", () => {
    expect(reach(bash(`bash -s 2827 <<'EOF'\ncurl -sS ${API}\nEOF`))).toContain(
      `reaches api.github.com (${API}) in a here-document bash runs`
    );
    expect(reach(bash(`bash <<'EOF'\ngh pr comment 1 --body 'see ${LINK}'\nEOF`))).toBeUndefined();
  });

  test("a here-document a command writes as text is no reach", () => {
    expect(reach(bash(`cat > body.md <<'EOF'\nsee ${LINK}\nEOF`))).toBeUndefined();
    expect(reach(bash(`gh pr comment 1 --body-file - <<'EOF'\nsee ${LINK}\nEOF`))).toBeUndefined();
    expect(reach(bash(`git commit -F - <<'EOF'\nAnswer ${LINK}\nEOF`))).toBeUndefined();
  });

  test("a here-document an interpreter reads as data is no reach", () => {
    expect(
      reach(bash(`python3 -c 'import sys; print(sys.stdin.read())' <<'EOF'\n${LINK}\nEOF`))
    ).toBeUndefined();
    expect(reach(bash(`jq -r .url <<'EOF'\n{"url": "${API}"}\nEOF`))).toBeUndefined();
  });
});

// Catches: a script the run wrote and then ran read as text, so its fetch of GitHub goes unread;
// and a file written and never run, or handed to a command as text, read as a reach.
describe("health: rule 13, off-world network, files written then run (door 3)", () => {
  test("a write of fetch.py that python3 then runs is off-world", () => {
    expect(reach(write("fetch.py", FETCH_PY), bash("python3 fetch.py"))).toBe(
      `off-world network: a bash call in ${SESSION} reaches api.github.com (${API}/pulls/2827) in fetch.py, which a write call in ${SESSION} wrote and it runs, no host of the run's world (127.0.0.1, localhost, ::1)`
    );
  });

  test("a written file run by its absolute path, an interpreter's subcommand or source is off-world", () => {
    expect(
      reach(
        write("/run/ws/fetch.ts", `await fetch("${API}");`),
        bash("cd /run/ws && bun run fetch.ts")
      )
    ).toContain("in /run/ws/fetch.ts, which a write call");
    expect(
      reach(write("get.sh", `curl -sS ${API}\n`), bash("chmod +x get.sh && ./get.sh"))
    ).toContain("reaches api.github.com");
    expect(reach(write("env.sh", `curl -sS ${API}\n`), bash("source env.sh"))).toContain(
      "reaches api.github.com"
    );
    expect(reach(write("fetch.py", FETCH_PY), bash("python3 /run/ws/fetch.py"))).toContain(
      "in fetch.py, which a write call"
    );
  });

  test("calls are read in the order they were made, whatever transcript holds them", () => {
    const [script, run] = [write("fetch.py", FETCH_PY), bash("python3 fetch.py")];
    const at = (step: [string, Record<string, unknown>], when: number, file: string) => ({
      file,
      tool: step[0],
      args: step[1],
      at: when,
    });
    // The subagent's transcript is read after its parent's, but its write came first.
    expect(
      offWorldReach([at(run, 2, SESSION), at(script, 1, `${SESSION}/sub.jsonl`)], LOOPBACK)
    ).toContain(`in fetch.py, which a write call in ${SESSION}/sub.jsonl`);
  });

  test("a written file a program a shell is given inline runs is off-world", () => {
    expect(
      reach(write("fetch.py", FETCH_PY), bash(`bash -lc 'cd ws && python3 fetch.py'`))
    ).toContain("in fetch.py, which a write call");
  });

  test("a written file a command's quoted text names as a command is no reach", () => {
    const script = write("fetch.sh", `curl -sS ${API}\n`);
    expect(
      reach(
        script,
        bash(`dispatch result --issue X-1 --run "bash fetch.sh; ./fetch.sh" --verdict pass`)
      )
    ).toBeUndefined();
    expect(reach(script, bash(`git commit -m "Fix it\n./fetch.sh\nmore"`))).toBeUndefined();
  });

  test("a written reply a command substitution reads is no reach", () => {
    // Round 17's reviewer-text-findings-text-only-base-5 and threads-closed-code-defect-base-3.
    const reply = write("/tmp/reply.txt", `see ${LINK}\n`);
    for (const read of ["body=$(cat /tmp/reply.txt)", "body=`cat /tmp/reply.txt`"])
      expect(
        reach(reply, bash(`${read} && gh api graphql -F id=PRRT_1 -f body="$body"`))
      ).toBeUndefined();
  });

  test("a here-document a bash command writes to a file that it then runs is off-world", () => {
    expect(
      reach(bash(`cat > fetch.py <<'EOF'\n${FETCH_PY}EOF`), bash("python3 fetch.py"))
    ).toContain("in fetch.py, which a bash call");
    expect(
      reach(bash(`tee get.sh <<'EOF' >/dev/null\ncurl -sS ${API}\nEOF`), bash("bash get.sh"))
    ).toContain("in get.sh, which a bash call");
  });

  test("an edit that puts a URL in a file the run then runs is off-world", () => {
    expect(
      reach(
        write("fetch.py", "import urllib.request\n"),
        ["edit", { input: `[fetch.py#1A2B]\nPUT >1:\n+urllib.request.urlopen('${API}')\n` }],
        bash("python3 fetch.py")
      )
    ).toContain("in fetch.py, which a edit call");
  });

  test("what a call appends to a written file keeps the text the file held", () => {
    const tail = "print('done')\n";
    for (const append of [
      bash(`cat >> fetch.py <<'EOF'\n${tail}EOF`),
      bash(`tee -a fetch.py <<'EOF'\n${tail}EOF`),
      ["edit", { input: `[fetch.py#1A2B]\nPUT >2:\n+${tail}` }] as [
        string,
        Record<string, unknown>,
      ],
    ])
      expect(reach(write("fetch.py", FETCH_PY), append, bash("python3 fetch.py"))).toContain(
        "reaches api.github.com"
      );
    expect(
      reach(
        write("fetch.py", FETCH_PY),
        bash(`cat > fetch.py <<'EOF'\n${tail}EOF`),
        bash("python3 fetch.py")
      )
    ).toBeUndefined();
    // Within one call: the append keeps what the same command wrote before it.
    expect(
      reach(
        bash(`cat > fetch.py <<'EOF'\n${FETCH_PY}EOF\ncat >> fetch.py <<'EOF'\n${tail}EOF`),
        bash("python3 fetch.py")
      )
    ).toContain("reaches api.github.com");
  });

  test("a bash command that writes a here-document to a file and runs it is off-world", () => {
    expect(reach(bash(`cat > fetch.py <<'EOF'\n${FETCH_PY}EOF\npython3 fetch.py`))).toContain(
      "in fetch.py, which a bash call"
    );
  });

  test("a written file a shell runs is read as a shell script, a link it writes as text", () => {
    const post = `gh pr comment 1 --body 'see ${LINK}'\n`;
    expect(reach(write("post", `#!/bin/sh\n${post}`), bash("./post"))).toBeUndefined();
    expect(reach(write("post.sh", post), bash("./post.sh"))).toBeUndefined();
    expect(reach(write("post", post), bash("bash post"))).toBeUndefined();
    expect(reach(write("post.py", `# ${post}`), bash("python3 post.py"))).toContain(
      "reaches github.com"
    );
  });

  test("an eval cell that loads a written file as code is off-world", () => {
    const evalOf = (language: string, code: string): [string, Record<string, unknown>] => [
      "eval",
      { language, code },
    ];
    for (const code of ['import runpy\nrunpy.run_path("fetch.py")', "import fetch"])
      expect(reach(write("fetch.py", FETCH_PY), evalOf("py", code))).toContain(
        "in fetch.py, which a write call"
      );
    expect(
      reach(write("/run/ws/fetch.js", `await fetch("${API}");`), evalOf("js", 'require("./fetch")'))
    ).toContain("in /run/ws/fetch.js, which a write call");
  });

  test("an eval cell that runs a written file through a subprocess is off-world", () => {
    expect(
      reach(write("fetch.py", FETCH_PY), [
        "eval",
        { language: "py", code: 'import subprocess\nsubprocess.run(["python3", "fetch.py"])' },
      ])
    ).toContain("in fetch.py, which a write call");
    expect(
      reach(write("fetch.ts", `await fetch("${API}");`), [
        "eval",
        {
          language: "js",
          code: 'const { execSync } = require("child_process");\nexecSync("bun fetch.ts");',
        },
      ])
    ).toContain("in fetch.ts, which a write call");
  });

  test("a written file never run, run before it was written, or handed on as text is no reach", () => {
    expect(reach(write("fetch.py", FETCH_PY))).toBeUndefined();
    expect(reach(bash("python3 fetch.py"), write("fetch.py", FETCH_PY))).toBeUndefined();
    expect(
      reach(write("reply.md", `see ${LINK}\n`), bash("gh pr comment 1 --body-file reply.md"))
    ).toBeUndefined();
  });

  test("an eval cell that names a written file without running it is no reach", () => {
    const doc = write("docs/cli.md", `See ${LINK}.\n`);
    for (const code of [
      'print(open("docs/cli.md").read())',
      'import subprocess\nsubprocess.run(["git", "diff", "docs/cli.md"])',
      'const { execSync } = require("child_process");\nexecSync("git diff docs/cli.md");',
    ])
      expect(reach(doc, ["eval", { language: "py", code }])).toBeUndefined();
  });

  test("an eval cell whose message quotes a written file's command is no reach", () => {
    const code = `await tool.envoy_publish({ topic: "t", message: "ran \`python3 fetch.py\` and it passed" });`;
    expect(reach(write("fetch.py", FETCH_PY), ["eval", { language: "js", code }])).toBeUndefined();
  });
});

// Catches: a jj revision that names a remote-tracking bookmark with a path (`main@origin:<path>`)
// read as a git remote in scp form, so a run that read a file of main from its own repository was
// called off-world (round 23's world-health run reviewer-text-findings-defect-base-1, which ran
// this command verbatim); and a remote in scp form where a command does take one read as no reach.
describe("health: rule 13, off-world network, a remote in scp form", () => {
  test("a jj command naming a bookmark of a remote with a path reaches no host", () => {
    expect(
      reach(
        bash(
          `cd /tmp/ws && git rev-parse --short=8 HEAD 2>/dev/null; echo "---base cli.md---"; jj -R . file show main@origin:docs/cli.md 2>&1`
        )
      )
    ).toBeUndefined();
    expect(reach(bash("jj file show -r main@origin docs/cli.md"))).toBeUndefined();
    expect(reach(bash("jj diff --from main@origin --to legion/LWEVAL-1@origin"))).toBeUndefined();
  });

  test("a remote in scp form that git or jj clones, fetches or adds is off-world", () => {
    const remote = "git@github.com:cybermonklabs/legion.git";
    for (const command of [
      `git clone ${remote} src`,
      `git fetch ${remote} main`,
      `git push ${remote} HEAD:main`,
      `git remote add upstream ${remote}`,
      `jj git clone ${remote} src`,
      `jj git remote add upstream ${remote}`,
      `jj git remote set-url origin ${remote}`,
    ])
      expect(reach(bash(command))).toBe(
        `off-world network: a bash call in ${SESSION} reaches github.com (${remote}), no host of the run's world (127.0.0.1, localhost, ::1)`
      );
  });
});

// Catches: a position that takes a repository's address (git-clone(1), git-fetch(1) and the other
// subcommands health.ts's GIT_TAKERS and JJ_TAKERS cite) read as no address, past the options
// before it, so a remote in scp form there goes unread; and a word of git or jj where the command
// takes no address (a revset, a regex of URLs to delete, a value a mode matches, a `--repo` that
// git push's own repository argument overrides) read as a reach.
describe("health: rule 13, off-world network, where git and jj take a repository's address", () => {
  const remote = "git@github.com:cybermonklabs/legion.git";
  const off = (named: string) =>
    `off-world network: a bash call in ${SESSION} reaches github.com (${named}), no host of the run's world (127.0.0.1, localhost, ::1)`;

  for (const [command, named] of [
    [`git clone --depth 1 -b main --single-branch ${remote} src`, remote],
    [`git -C ws -c core.quotepath=off pull ${remote} main`, remote],
    [`git fetch --multiple origin ${remote}`, remote],
    [`git push --repo=${remote}`, remote],
    [`git ls-remote --sort version:refname ${remote}`, remote],
    [`git remote -v set-url --push origin ${remote}`, remote],
    [`git submodule add -b main ${remote} vendor/legion`, remote],
    [`git submodule set-url vendor/legion ${remote}`, remote],
    [`git archive --remote ${remote} HEAD`, remote],
    [`git config --add remote.upstream.url ${remote}`, remote],
    [`git config set --comment mirror remote.upstream.pushurl ${remote}`, remote],
    [`git -c remote.origin.url=${remote} fetch origin`, remote],
    ["git config --global url.git@github.com:.insteadOf gh:", "git@github.com:"],
    [`jj -R ws git remote add upstream ../legion.git --push-url ${remote}`, remote],
    [`jj git remote set-url origin --fetch ${remote}`, remote],
    [`jj --config ui.color=never git clone --remote upstream ${remote} src`, remote],
  ])
    test(`\`${command}\` is off-world`, () => {
      expect(reach(bash(command))).toBe(off(named));
    });

  for (const command of [
    "jj log -r 'main@origin::@'",
    `git remote set-url --delete origin ${remote}`,
    `git config --unset remote.origin.url ${remote}`,
    `git push --repo=${remote} origin main`,
  ])
    test(`\`${command}\` reaches no host`, () => {
      expect(reach(bash(command))).toBeUndefined();
    });
});

// Catches: a git or jj call that names the world's own repository by its https URL read as on the
// world. The issue workspace's origin is that URL (rig.sh's fixture_workspace), and the world's
// helper takes a call of origin by its name alone (world_remote): a call naming the URL is taken
// to the live github.com, as it was before the origin named it, so the run reached off its world.
describe("health: rule 13, off-world network, the world's own repository by its URL", () => {
  const url = `https://github.com/${WORLD_REPO}`;
  for (const [command, named] of [
    [`git ls-remote ${url}`, url],
    [`git fetch ${url}.git main`, `${url}.git`],
    [`git clone ${url} src`, url],
    [`jj git clone ${url} src`, url],
  ])
    test(`\`${command}\` is off-world`, () => {
      expect(reach(bash(command))).toBe(offWorld("github.com", named));
    });

  for (const command of [
    "git ls-remote origin",
    "git fetch origin main",
    "jj git fetch --remote origin",
  ])
    test(`\`${command}\` reaches no host`, () => {
      expect(reach(bash(command))).toBeUndefined();
    });
});

// Catches: a word of curl or wget where the program takes no host read as one (the file a value
// option takes, apart or at the end of a word of short options, or a redirection's target); one
// program's option read by the other's (wget's `-c` takes no cookie jar, curl's `-O` no file), so
// the URL after it goes unread; and the host an option of theirs connects to (`--url`, a proxy,
// wget's `--base`) read as none.
describe("health: rule 13, off-world network, where curl and wget take a host", () => {
  const api = "api.github.com/repos/cybermonklabs/legion";
  const whoami = "http://127.0.0.1:4101/api/v1/whoami";
  for (const [command, host, named] of [
    [`curl -O ${api}/tarball`, "api.github.com", `${api}/tarball`],
    [`curl -sS --url ${api}`, "api.github.com", api],
    [`curl -sS -x proxy.example.com:3128 ${whoami}`, "proxy.example.com", "proxy.example.com:3128"],
    [`wget -c ${api}/tarball`, "api.github.com", `${api}/tarball`],
    ["wget -F -i links.html -B api.github.com/", "api.github.com", "api.github.com/"],
  ])
    test(`\`${command}\` is off-world`, () => {
      expect(reach(bash(command))).toBe(
        `off-world network: a bash call in ${SESSION} reaches ${host} (${named}), no host of the run's world (127.0.0.1, localhost, ::1)`
      );
    });

  for (const command of [
    `curl -sS -D headers.txt -o report.json ${whoami}`,
    `curl -sSo report.json ${whoami}`,
    `curl -sS ${whoami} > report.json`,
    `wget -a fetch.log -O page.html ${whoami}`,
  ])
    test(`\`${command}\` reaches no host`, () => {
      expect(reach(bash(command))).toBeUndefined();
    });
});

/** Rule 13's reason for a bash call that reaches `host`, named `named`, held to the run's world. */
const offWorld = (host: string, named: string, via = "") =>
  `off-world network: a bash call in ${SESSION} reaches ${host} (${named})${via}, no host of the run's world (127.0.0.1, localhost, ::1)`;

// Catches: an address in scp form with no user (`host:path`, which git, jj, scp, rsync and sftp
// read as a remote host when no slash comes before its first colon) read as no reach where one
// of them takes an address; and that form read anywhere else, where `HEAD:docs/cli.md`,
// `main@origin:docs/cli.md` and `docs/cli.md:12` are a revision and a path, or a file and a line,
// and a review body or a commit message that names a command after a `;` or a bracket is text.
describe("health: rule 13, off-world network, a remote in scp form with no user", () => {
  const remote = "github.com:cybermonklabs/legion.git";
  for (const command of [
    `git clone ${remote} src`,
    `git -C ws remote add upstream ${remote}`,
    `jj git clone ${remote} src`,
    `scp notes.md ${remote}`,
    `rsync -az ./ ${remote}`,
    `sftp ${remote}`,
  ])
    test(`\`${command}\` is off-world`, () => {
      expect(reach(bash(command))).toBe(offWorld("github.com", remote));
    });

  for (const command of [
    "git show HEAD:docs/cli.md",
    "jj file show main@origin:docs/cli.md",
    "grep -n greet docs/cli.md:12",
    "git push origin HEAD:main",
    "git clone localhost:legion.git src",
    "scp ./notes:v1.md docs/cli.md:12 backup/",
    `gh pr comment 1001 --body "Checked; git clone ${remote} still works"`,
    'git commit -m "Fix the typo (ssh keys are unchanged)"',
  ])
    test(`\`${command}\` reaches no host`, () => {
      expect(reach(bash(command))).toBeUndefined();
    });
});

// Catches: a command a wrapper runs read as the wrapper's words, so `sudo git clone …` or
// `env GIT_SSH_COMMAND=… git fetch …` reaches GitHub unread; and a wrapper read as a reach of its
// own, or the words of one that runs nothing (`command -v`, `sudo -l`) read as a command.
describe("health: rule 13, off-world network, a command a wrapper runs", () => {
  const remote = "git@github.com:cybermonklabs/legion.git";
  for (const [command, named, via] of [
    [`sudo -u ada -E git clone ${remote} src`, remote, ""],
    [`env -i GIT_SSH_COMMAND='ssh -i key' git fetch ${remote}`, remote, ""],
    [`timeout -s KILL 30 git ls-remote ${remote}`, remote, ""],
    [
      "nice -n 10 curl -sS api.github.com/repos/cybermonklabs/legion",
      "api.github.com/repos/cybermonklabs/legion",
      "",
    ],
    ["nohup ssh git@github.com &", "git@github.com", ""],
    [`echo main | xargs -n1 git clone ${remote}`, remote, ""],
    [`command git clone ${remote}`, remote, ""],
    ["exec ssh -T git@github.com", "git@github.com", ""],
    [`time git fetch ${remote}`, remote, ""],
    [`sh -c 'git clone ${remote} src'`, remote, " in the program sh -c runs"],
    ["bash -lc 'cd ws && ssh -T git@github.com'", "git@github.com", " in the program bash -c runs"],
    [`sudo env FOO=1 timeout 5 git clone ${remote}`, remote, ""],
  ])
    test(`\`${command}\` is off-world`, () => {
      expect(reach(bash(command))).toBe(
        offWorld(named.startsWith("api.") ? "api.github.com" : "github.com", named, via)
      );
    });

  for (const command of [
    "env FOO=1 git status",
    "timeout 30 bun test",
    "sudo -u ada git log --oneline",
    "nice -n 10 bun test",
    "nohup bun run serve.ts &",
    "git ls-files | xargs -I{} ssh {}",
    "command -v ssh",
    "sudo -l ssh git@github.com",
    "exec bun test",
    "time bun test",
    "bash -c 'git show HEAD:docs/cli.md'",
  ])
    test(`\`${command}\` reaches no host`, () => {
      expect(reach(bash(command))).toBeUndefined();
    });
});

// Catches: a command that follows an assignment whose value holds a space
// (`GIT_SSH_COMMAND='ssh -i key' git clone …`) read as a program named by the rest of that value,
// so no rule that reads a command by its grammar reads it: score.ts's AT_COMMAND takes an
// assignment's value as `\S*`, and SHELL_COMMAND and DISPATCH_COMMAND are both built on it. And
// an address inside such a value, or a command after an assignment that stands alone, read as a
// reach.
describe("health: rule 13, off-world network, a command after an assignment whose value holds a space", () => {
  const remote = "git@github.com:cybermonklabs/legion.git";
  for (const [command, host, named] of [
    [`GIT_SSH_COMMAND='ssh -i key' git clone ${remote} src`, "github.com", remote],
    [`GIT_SSH_COMMAND="ssh -i key" git fetch ${remote} main`, "github.com", remote],
    [`GIT_SSH_COMMAND=ssh\\ -i\\ key git push ${remote} HEAD:main`, "github.com", remote],
    [`cd ws && NOTE='two words' LANG=C git ls-remote ${remote}`, "github.com", remote],
    [`env NOTE='two words' git clone ${remote} src`, "github.com", remote],
    [
      "NOTE='two words' curl -sS api.github.com/repos/cybermonklabs/legion",
      "api.github.com",
      "api.github.com/repos/cybermonklabs/legion",
    ],
    [`NOTE="two words" ssh -T git@github.com`, "github.com", "git@github.com"],
  ])
    test(`\`${command}\` is off-world`, () => {
      expect(reach(bash(command))).toBe(offWorld(host, named));
    });

  for (const command of [
    `NOTE='clone ${remote} later' git status`,
    `NOTE="see git@github.com for the key" bun test`,
    "NOTE='two words' git show HEAD:docs/cli.md",
    `NOTE='two words'; git status`,
  ])
    test(`\`${command}\` reaches no host`, () => {
      expect(reach(bash(command))).toBeUndefined();
    });
});

// Each reader of a command's words reads them through score.ts's one shell-word reader
// (SHELL_WORD, `shellTokens`): its first word, its words, its prefix assignments, its
// redirections' targets, a text flag's value and a list a subprocess runs. Catches, one row each:
// a quoted first word read up to its first blank (`"/opt/my tools/ssh"` named `my`), or read with
// its quotes (`'/usr/bin/ssh'` named `ssh'`); env's `-i` read with no options after it; a
// backslash-escaped blank ending a word; a word quoted in part read with its quotes; a `$'…'`
// assignment read with its quotes; a redirection's quoted target read up to its first blank, the
// rest of it read as words; a quoted file a here-document is written to read up to its first
// blank; a text flag's or a field's value read up to an escaped quote, the rest of it read as a
// reach; and a subprocess list's word holding a blank read as two.
describe("health: rule 13, off-world network, words read as the shell reads them", () => {
  const remote = "git@github.com:cybermonklabs/legion.git";
  for (const [command, host, named] of [
    ['"/opt/my tools/ssh" -T git@github.com', "github.com", "git@github.com"],
    ["'/usr/bin/ssh' -T git@github.com", "github.com", "git@github.com"],
    [`env -i -u HOME git clone ${remote} src`, "github.com", remote],
    [
      "curl -sS 'api.github.com'/repos/cybermonklabs/legion",
      "api.github.com",
      "api.github.com/repos/cybermonklabs/legion",
    ],
  ])
    test(`\`${command}\` is off-world`, () => {
      expect(reach(bash(command))).toBe(offWorld(host, named));
    });

  test("a gh whose GH_HOST is given in ANSI-C quotes, an escape among them, reaches that host", () => {
    expect(reach(bash("GH_HOST=$'ghe\\x2eexample.com' gh api user"))).toBe(
      `off-world network: a bash call in ${SESSION} reaches ghe.example.com (GH_HOST=ghe.example.com), no host the run's gh serves (github.com, 127.0.0.1, localhost, ::1)`
    );
  });

  for (const command of [
    "curl -sS http://127.0.0.1:9/x -H X-Note:\\ see\\ example.com/",
    "curl -sS http://127.0.0.1:9/x > 'notes for example.com'",
    `gh pr comment 1 --body "a \\"b c\\" ${LINK}"`,
    `gh api repos/o/r/issues/1/comments -f body="a \\"b c\\" ${LINK}"`,
  ])
    test(`\`${command}\` reaches no host`, () => {
      expect(reach(bash(command))).toBeUndefined();
    });

  test("a file a here-document is written to under a quoted name, then run, is off-world", () => {
    expect(
      reach(
        bash(`cat > '/run/ws/get it.sh' <<'EOF'\ncurl -sS ${API}\nEOF`),
        bash("bash /run/ws/get\\ it.sh")
      )
    ).toContain("in /run/ws/get it.sh, which a bash call");
  });

  test("a written file a subprocess list runs by a name holding a blank is off-world", () => {
    expect(
      reach(write("/run/ws/my fetch.py", FETCH_PY), [
        "eval",
        {
          language: "py",
          code: 'import subprocess\nsubprocess.run(["python3", "/run/ws/my fetch.py"])',
        },
      ])
    ).toContain("in /run/ws/my fetch.py, which a write call");
  });

  // An assignment's value keeps an unquoted `$(` in it, so a command substitution there is read
  // where it starts and runs the written file.
  test("a written file a command substitution in an assignment's value runs is off-world", () => {
    expect(reach(write("fetch.py", FETCH_PY), bash("out=$(python3 fetch.py)"))).toContain(
      "in fetch.py, which a write call"
    );
  });
});

// The four operators that trim a variable's value, `${word%p}`, `${word%%p}`, `${word#p}` and
// `${word##p}`, as rule 13 reads them where a call sets the variable to a literal (score.ts's
// `expandedWord`), each row held to bash's own answer for it, which bash prints here, so the
// reader's shortest and longest matches cannot drift from the shell's. Catches: `%%` read as `%`
// or `##` as `#` (the shortest match taken for the longest), a prefix taken off for a suffix, a
// `*` matched as itself, an empty pattern or one with no match read as taking something off, and
// a character of the pattern a regular expression reads as more than itself (`.`, `+`).
describe("health: rule 13, off-world network, a variable's value trimmed as bash trims it", () => {
  const rows: [value: string, operator: string, pattern: string][] = [
    ["a:b:c", "%", ":*"],
    ["a:b:c", "%%", ":*"],
    ["a:b:c", "#", "*:"],
    ["a:b:c", "##", "*:"],
    ["a/b/c/d", "%", "/*/*"],
    ["a/b/c/d", "%%", "/*/*"],
    ["a/b/c/d", "#", "*/*/"],
    ["a/b/c/d", "##", "*/*/"],
    ["aXbYb", "#", "a*b"],
    ["aXbYb", "##", "a*b"],
    ["aXbYb", "%", "b*b"],
    ["aXbYb", "%%", "X*"],
    ["abc", "%", "*"],
    ["abc", "%%", "*"],
    ["abc", "#", "*"],
    ["abc", "##", "*"],
    ["abc", "%", ""],
    ["abc", "#", "z*"],
    ["v7.0.1", "%", ".1"],
    ["v7.0.1", "#", "v"],
    ["a.b+c", "%", "+c"],
    ["a.b+c", "#", "a.b"],
    ["axb", "#", "a.b"],
    ["actions/checkout:v7.0.1:3d3c42e5", "%%", ":*"],
    ["v7.0.1:3d3c42e5", "#", "*:"],
  ];
  test("each row reads to what bash prints for it", () => {
    const expansion = ([, operator, pattern]: (typeof rows)[number]) =>
      `\${word${operator}${pattern}}`;
    const read = rows.map((row) => [
      ...row,
      reach(bash(`word=${row[0]}; curl -s https://example.com/${expansion(row)}`)),
    ]);
    const printed = rows.map((row) => {
      const shell = Bun.spawnSync(["bash", "-c", `word=${row[0]}; printf %s "${expansion(row)}"`]);
      expect(shell.exitCode).toBe(0);
      return [...row, offWorld("example.com", `https://example.com/${shell.stdout.toString()}`)];
    });
    expect(read).toEqual(printed);
  });
});

// The paths a `bash` command writes, read through the same shell-word reader. Catches: a write
// after an assignment whose value holds a space read as no command's (AT_COMMAND's `\S*`); a
// writer's operand with an escaped blank read as two words; a quoted redirection target read as
// none; and a `>` inside a double-quoted word that holds an escaped quote read as a redirection.
describe("health: the paths a bash command writes, words read as the shell reads them", () => {
  for (const [command, paths] of [
    ["NOTE='two words' cp a.txt /var/tmp/b.md", ["/var/tmp/b.md"]],
    ["cp a.txt /var/tmp/my\\ notes.md", ["/var/tmp/my notes.md"]],
    ["echo x > '/var/tmp/my notes.md'", ["/var/tmp/my notes.md"]],
    ['echo "a \\" > /var/tmp/x"', []],
  ] as const)
    test(`\`${command}\` writes ${paths.join(", ") || "nothing"}`, () => {
      expect(writtenBy("bash", { command })).toEqual([...paths]);
    });
});

// Catches: the host ssh, scp, sftp, rsync, nc, ncat or telnet connects to read as no reach: an
// operand that takes a host by the tool's own grammar, or an option that names one (`-J`,
// `-o ProxyJump`, `-o HostName`, a proxy); and the words of those tools that name no host (an
// option's value, a local path, a listener's port, an ssh alias `HostName` points at the
// loopback) read as one.
describe("health: rule 13, off-world network, ssh, scp, sftp, rsync, nc, ncat and telnet", () => {
  for (const [command, host, named] of [
    ["ssh -i key -p 22 git@github.com", "github.com", "git@github.com"],
    ["ssh -J bastion.example.com localhost", "bastion.example.com", "bastion.example.com"],
    [
      "ssh -o ProxyJump=bastion.example.com 127.0.0.1",
      "bastion.example.com",
      "ProxyJump=bastion.example.com",
    ],
    ["ssh -o 'HostName github.com' localhost", "github.com", "HostName github.com"],
    ["scp -P 22 notes.md git@github.com:notes.md", "github.com", "git@github.com:notes.md"],
    [
      "scp -J bastion.example.com notes.md localhost:notes.md",
      "bastion.example.com",
      "bastion.example.com",
    ],
    ["sftp -b batch git@github.com", "github.com", "git@github.com"],
    ["rsync -avz --exclude .git ./ git@github.com:legion/", "github.com", "git@github.com:legion/"],
    ["rsync -av github.com::legion/ ./mirror/", "github.com", "github.com::legion/"],
    [
      "rsync -e 'ssh -J bastion.example.com' ./ localhost:backup/",
      "bastion.example.com",
      "bastion.example.com",
    ],
    ["nc -zv github.com 22", "github.com", "github.com"],
    ["nc -x proxy.example.com:1080 localhost 4100", "proxy.example.com", "proxy.example.com:1080"],
    [
      "ncat --proxy proxy.example.com:3128 127.0.0.1 4100",
      "proxy.example.com",
      "proxy.example.com:3128",
    ],
    ["telnet github.com 22", "github.com", "github.com"],
  ])
    test(`\`${command}\` is off-world`, () => {
      expect(reach(bash(command))).toBe(offWorld(host, named));
    });

  for (const command of [
    "ssh -p 2222 -i key localhost",
    "ssh -o HostName=127.0.0.1 github",
    "ssh -G github.com",
    "ssh-keygen -t ed25519 -f key",
    "scp notes.md backup/notes.md",
    "sftp localhost",
    "rsync -e ssh -av ./ localhost:backup/",
    "rsync -av ./src/ ./dst/",
    "nc -lvp 4100",
    "nc -U /tmp/agent.sock",
    "ncat -l 127.0.0.1 4100",
    "telnet 127.0.0.1 4100",
  ])
    test(`\`${command}\` reaches no host`, () => {
      expect(reach(bash(command))).toBeUndefined();
    });
});

// Catches: a host a `gh` names (`--hostname`, `-R HOST/OWNER/REPO`, GH_HOST, set before it or on
// it) other than github.com, which the run's stand-in plays, and the loopback read as in the
// world; github.com read as off it; and a gh other than the run's own stand-in (<run>/bin/gh,
// which a bare `gh` resolves to on the pane's PATH) read as the world's, whatever it names.
describe("health: rule 13, off-world network, the host a gh names, and a gh that is not the run's", () => {
  const served = (host: string, named: string) =>
    `off-world network: a bash call in ${SESSION} reaches ${host} (${named}), no host the run's gh serves (github.com, 127.0.0.1, localhost, ::1)`;
  const other = (named: string) =>
    `off-world network: a bash call in ${SESSION} reaches github.com (${named}), a gh that is not the run's own stand-in (${RUN}/bin/gh), and no world of this rig serves another`;
  for (const [command, reason] of [
    ["gh api --hostname ghe.example.com user", served("ghe.example.com", "ghe.example.com")],
    [
      "gh pr view 1001 -R ghe.example.com/example/widgets",
      served("ghe.example.com", "ghe.example.com/example/widgets"),
    ],
    [
      "gh pr view 1001 --repo=ghe.example.com/example/widgets",
      served("ghe.example.com", "ghe.example.com/example/widgets"),
    ],
    [
      "gh pr view 1001 -R https://ghe.example.com/example/widgets",
      served("ghe.example.com", "https://ghe.example.com/example/widgets"),
    ],
    [
      "GH_HOST=ghe.example.com gh pr view 1001",
      served("ghe.example.com", "GH_HOST=ghe.example.com"),
    ],
    [
      "env GH_HOST=ghe.example.com gh api user",
      served("ghe.example.com", "GH_HOST=ghe.example.com"),
    ],
    [
      "export GH_HOST=ghe.example.com && gh api user",
      served("ghe.example.com", "GH_HOST=ghe.example.com"),
    ],
    ["GH_HOST=ghe.example.com; gh api user", served("ghe.example.com", "GH_HOST=ghe.example.com")],
    ["GH_HOST=ghe.example.com\ngh api user", served("ghe.example.com", "GH_HOST=ghe.example.com")],
    ["/usr/bin/gh pr view 1", other("/usr/bin/gh")],
    ["cd ws && /opt/legion/bin/gh pr view 1", other("/opt/legion/bin/gh")],
    ["legion gh pr view 1", other("legion gh")],
    ["./gh pr view 1", other("./gh")],
  ])
    test(`\`${command}\` is off-world`, () => {
      expect(reach(bash(command))).toBe(reason);
    });

  for (const command of [
    "gh pr view 1",
    "gh api --hostname github.com repos/example/widgets/pulls/1001",
    "gh pr view 1001 -R example/widgets",
    "gh pr view 1001 -R github.com/example/widgets",
    "gh pr view -R https://github.com/example/widgets 1001",
    "GH_HOST=github.com gh api user",
    "GH_HOST=127.0.0.1 gh api user",
    `${RUN}/bin/gh pr view 1`,
    "../bin/gh pr view 1",
    "command -v /usr/bin/gh",
  ])
    test(`\`${command}\` reaches no host`, () => {
      expect(reach(bash(command))).toBeUndefined();
    });
});

// Catches: a tool rule 13 names no reading for read as reaching nothing, so a URL handed to a
// tool nobody listed goes unread; and the text a writing tool sends (an Envoy message, a Dispatch
// comment, a subagent's prompt) read as a reach.
describe("health: rule 13, off-world network, every other tool (class default)", () => {
  test("an unknown tool given a URL is off-world, called by name or through its xd:// device", () => {
    expect(reach(["frobnicate", { target: `${API}/pulls` }])).toBe(
      `off-world network: a frobnicate call in ${SESSION} reaches api.github.com (${API}/pulls), no host of the run's world (127.0.0.1, localhost, ::1)`
    );
    expect(reach(["frobnicate", { options: [{ url: API }] }])).toContain("reaches api.github.com");
    expect(reach(write("xd://frobnicate", JSON.stringify({ target: API })))).toContain(
      "a write xd://frobnicate call"
    );
  });

  test("an unknown tool whose only URL is its intent is no reach", () => {
    expect(reach(["frobnicate", { i: `Reading ${LINK}` }])).toBeUndefined();
  });

  test("a grep given a URL as its path is off-world, and a URL it searches for is not", () => {
    expect(reach(["grep", { pattern: "legion", path: `src;${API}/readme` }])).toContain(
      "a grep call"
    );
    expect(reach(["grep", { pattern: LINK, path: "src" }])).toBeUndefined();
  });

  for (const [tool, args] of [
    ["envoy_publish", { topic: "notifications.role.x", message: `see ${LINK}` }],
    ["envoy_send", { session_id: "s", message: `see ${LINK}` }],
    ["dispatch_comment", { issue: "LWEVAL-1", body: `see ${LINK}` }],
    ["task", { tasks: [{ id: "a", assignment: `Review ${LINK}` }] }],
    ["todo", { op: "add", items: [`answer ${LINK}`] }],
    ["legion", { op: "handoff_complete", summary: `Approved ${LINK}` }],
    ["report_issue", { content: `read: ${LINK} rendered wrong` }],
    ["edit", { input: `[reply.md#1A2B]\nPUT >1:\n+see ${LINK}\n` }],
  ] as const)
    test(`a ${tool} call whose text names a URL is no reach`, () => {
      expect(reach([tool, args])).toBeUndefined();
    });

  test("an envoy_publish through its xd:// device whose message names a URL is no reach", () => {
    expect(
      reach(write("xd://envoy_publish", JSON.stringify({ topic: "t", message: `see ${LINK}` })))
    ).toBeUndefined();
  });
});

// Catches: an eval cell's call of a session tool read as code, so a message an agent publishes
// through eval (round 17's code-defect-head-5 ran `await tool.envoy_publish(…)`) that quotes the
// world's pull request voids a correct run; and that reading made so wide that a fetch beside it,
// a bash or read call inside the cell, or a call the reader cannot balance goes unread.
describe("health: rule 13, off-world network, an eval cell's tool calls (door 1)", () => {
  const WORLD_PR = "https://github.com/example/widgets/pull/1001";
  const cell = (language: string, code: string): [string, Record<string, unknown>] => [
    "eval",
    { language, code },
  ];
  const PY_PUBLISH = `await tool.envoy_publish(topic="t", message="""see ${LINK}""")`;

  test("a text tool an eval cell calls is no reach, in either language", () => {
    expect(
      reach(cell("js", `await tool.envoy_publish({topic:"t", message:\`see ${WORLD_PR}\`})`))
    ).toBeUndefined();
    expect(reach(cell("py", PY_PUBLISH))).toBeUndefined();
    expect(
      reach(cell("py", `await tool.envoy_publish(topic="t", message="""see\n${LINK}\n(the PR)""")`))
    ).toBeUndefined();
    expect(
      reach(
        cell("js", `await tool.envoy_publish({message: \`see \${\`PR \${n}\`} at ${WORLD_PR}\`})`)
      )
    ).toBeUndefined();
  });

  test("a fetch beside a text tool's call is off-world", () => {
    expect(reach(cell("py", `${PY_PUBLISH}\nawait fetch("${API}")`))).toContain(
      `reaches api.github.com (${API})`
    );
    expect(
      reach(cell("js", `await tool.envoy_publish({message: \`\${await fetch("${API}")}\`})`))
    ).toContain("reaches api.github.com");
  });

  test("a bash, read or unknown tool an eval cell calls is read as its own call", () => {
    expect(reach(cell("js", `await tool.bash({command:"curl -s ${API}/x"})`))).toContain(
      `reaches api.github.com (${API}/x) in the cell's tool.bash call`
    );
    expect(
      reach(cell("js", `await tool.bash({command:"gh pr comment 1 --body 'see ${LINK}'"})`))
    ).toBeUndefined();
    expect(
      reach(cell("js", `await tool.read({path:"https://github.com/cybermonklabs/legion"})`))
    ).toContain("in the cell's tool.read call");
    expect(reach(cell("py", `await tool.grep(pattern="${LINK}", path="src")`))).toBeUndefined();
    expect(reach(cell("py", `await tool.frobnicate("${API}")`))).toContain(
      "in the cell's tool.frobnicate call"
    );
  });

  test("a tool call an eval cell does not balance reads the cell whole as code", () => {
    expect(reach(cell("js", `await tool.envoy_publish({message: "see ${API}"`))).toContain(
      "reaches api.github.com"
    );
    // A one-line literal that does not end on its line is no literal: two apostrophes of
    // comments would otherwise hide the fetch between them inside the text tool's call.
    const comments = [
      "await tool.envoy_publish({",
      `  message: "ok", // don't`,
      `  topic: await (await fetch("${API}")).text(), // it's`,
      "})",
    ].join("\n");
    expect(reach(cell("js", comments))).toContain("reaches api.github.com");
  });

  test("a Python argument with a string prefix is read by its key", () => {
    expect(reach(cell("py", `await tool.bash(command=f"curl -s ${API}/{n}")`))).toContain(
      "in the cell's tool.bash call"
    );
  });

  test("a Python f-string's expressions are read as code, its text and doubled braces are not", () => {
    expect(
      reach(
        cell(
          "py",
          `await tool.envoy_publish(message=f"{urllib.request.urlopen('https://api.github.com/x').read()}")`
        )
      )
    ).toContain("reaches api.github.com");
    expect(
      reach(
        cell(
          "py",
          `url = "http://127.0.0.1:4100/x"\nawait tool.envoy_publish(message=f"see {url}")`
        )
      )
    ).toBeUndefined();
    expect(
      reach(cell("py", `await tool.envoy_publish(message=f"see {{https://api.github.com/x}}")`))
    ).toBeUndefined();
  });

  test("a file an eval cell's tool.write or tool.edit writes, then runs, is off-world", () => {
    const fetchPy = `await tool.write({path:"f.py", content:"import urllib.request\\nurllib.request.urlopen('https://api.github.com/x')"})`;
    expect(reach(cell("js", fetchPy), bash("python3 f.py"))).toContain(
      "in f.py, which a eval call"
    );
    expect(reach(cell("js", fetchPy))).toBeUndefined();
    expect(
      reach(
        write("f.py", "import urllib.request\n"),
        cell(
          "js",
          `await tool.edit({input:"[f.py#1A2B]\\nPUT >1:\\n+urllib.request.urlopen('https://api.github.com/x')"})`
        ),
        bash("python3 f.py")
      )
    ).toContain("in f.py, which a eval call");
    // In one cell, in the cell's order.
    expect(reach(cell("js", `${fetchPy}\nawait tool.bash({command:"python3 f.py"})`))).toContain(
      "in f.py, which a eval call"
    );
  });
});

// Round 32's c8ef305c3eab (reviewer-text-findings, text-only, base), as the tester's evidence
// holds it (dispatch://INFRA-764/artifact/infra-764-r32-world-health-tgz-b64): a subagent of the
// reviewer searched the web for Bun's spawnSync and read Bun's documentation (its transcript
// MammothLungfish.jsonl, the call at line 10 and the result answering it at line 14). The world
// here is that run's: its record, its world.json (but the head this remote holds), its workflow
// file at that head, and two of its fixtures.json routes; the call and its result are the
// transcript's, three of the result's ten sources kept verbatim. Each row reads the run as
// `rig.sh health` reads it (`loadRun`), so a search's result is the one its transcript pairs with
// it by `toolCallId`. The architect's ruling of 2026-11-22T01:44:54Z, read as the tester's
// world-health.py reads it (tools v73), whose checks on copies of this run these rows repeat.
describe("health: rule 13, off-world network, a web search as a run's transcript records it", () => {
  const runs = temporaryDirectories("health-web-search");
  const RECORD = {
    run: "c8ef305c3eab",
    scenario: "reviewer-text-findings",
    case: "text-only",
    label: "base",
    n: 1,
    name: "reviewer-text-findings-text-only-base-1",
    batch: "legion-5e633a2f-1795306843",
    ran: "/legion/scratch/tmp/b32/w/runs/c8ef305c3eab",
  };
  const WORKFLOW = [
    "# Runs the greet tests on every pull request and on every push to main.",
    "name: greet",
    "on:",
    "  pull_request:",
    "permissions:",
    "  contents: read",
    "jobs:",
    "  test:",
    "    runs-on: ubuntu-latest",
    "    steps:",
    "      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1",
    "      - uses: oven-sh/setup-bun@0c5077e51419868618aeaa5fe8019c62421857d6 # v2.2.0",
    "      - run: bun test",
    "",
  ].join("\n");
  const rule = (type: string) => ({
    type,
    ruleset_source_type: "Repository",
    ruleset_source: "fernbrook/widgets",
    ruleset_id: 1,
  });
  const ROUTES = [
    {
      match: "^gh api .*/rules/branches/main",
      methods: ["GET"],
      stdout: [rule("deletion"), rule("non_fast_forward"), rule("required_linear_history")],
    },
    { match: "^gh pr edit( |$)", stdout: "https://github.com/fernbrook/widgets/pull/1002" },
    {
      match: "^gh api .*/branches/main/protection",
      methods: ["GET"],
      exit: 1,
      stdout: {
        message: "Resource not accessible by integration",
        documentation_url:
          "https://docs.github.com/rest/branches/branch-protection#get-branch-protection",
        status: "403",
      },
    },
    {
      match: "^gh api .*/rulesets([ ?]|$)",
      methods: ["GET"],
      stdout: [
        {
          id: 1,
          name: "main",
          target: "branch",
          source_type: "Repository",
          source: "fernbrook/widgets",
          enforcement: "active",
          node_id: "RRS_lACqUmVwb3NpdG9yec4AAAABzgAAAAE",
          _links: {
            self: { href: "https://api.github.com/repos/fernbrook/widgets/rulesets/1" },
            html: { href: "https://github.com/fernbrook/widgets/rules/1" },
          },
          created_at: "2026-01-01T00:00:00.000Z",
          updated_at: "2026-01-01T00:00:00.000Z",
        },
      ],
    },
  ];
  const TRANSCRIPT =
    "sessions/2026-11-22T00-32-40-999Z_01a200ab-6027-757c-b4c1-577fcb6be0ef/MammothLungfish.jsonl";
  const QUERY = "Bun.spawnSync default stderr stdout pipe inherit";
  const INTENT = "Checking Bun spawnSync default stdio";
  const SOURCES = [
    '[2] Spawn | Bun Docs\n    https://bun.com/docs/runtime/child-process\n    Output streams #\n\nRead the subprocess\'s output from the stdout and stderr properties. By default stdout is an instance of ReadableStream ; stderr is inherited from the parent process, so proc.stderr is undefined . Pass stderr: "pipe" to ge…',
    '[7] bun/docs/runtime/child-process.mdx at bun-v1.3.14 · oven-sh/bun · GitHub (154mo ago)\n    https://github.com/oven-sh/bun/blob/bun-v1.3.14/docs/runtime/child-process.mdx\n    You can read results from the subprocess via the stdout and stderr properties. By default these are instances of ReadableStream.\nconst proc = Bun.spawn(["bun", "--version"]);\nconst text = await proc.stdout.text();\nconsole.log(text); // => …',
    '[8] bun.com\n    https://bun.com/docs/runtime/child-process.md\n    * Value | Description\n* "pipe" | Default for stdout. Pipe the output to a ReadableStream on the returned Subprocess object\n* "inherit" | Default for stderr. Inherit from the parent process\n* "ignore" | Discard the output\n* Bun.file() | Wri…',
  ].join("\n");

  /** c8ef305c3eab's world in a directory of its own, and the head its remote holds. */
  function world(): { dir: string; head: string } {
    const dir = path.join(runs(), RECORD.run);
    const src = path.join(dir, "src");
    mkdirSync(path.join(src, ".github", "workflows"), { recursive: true });
    writeFileSync(path.join(src, ".github", "workflows", "greet.yml"), WORKFLOW);
    git(src, "init", "-q", "-b", "legion/WID-1");
    git(src, "add", "-A");
    git(src, "commit", "-qm", "feat: greet(), a greeting helper for the widgets CLI (WID-1)");
    const head = git(src, "rev-parse", "HEAD").out;
    git(dir, "clone", "-q", "--bare", src, "remote.git");
    writeFileSync(`${dir}.json`, JSON.stringify(RECORD));
    writeFileSync(path.join(dir, "fixtures.json"), JSON.stringify({ routes: ROUTES }));
    writeFileSync(
      path.join(dir, "world.json"),
      JSON.stringify({
        key: "WID-1",
        repo: "fernbrook/widgets",
        pr: 1002,
        branch: "legion/WID-1",
        base: "main",
        head,
        logins: { implement: "widgets-implementer[bot]", review: "widgets-reviewer[bot]" },
      })
    );
    return { dir, head };
  }

  /** Rule 13's reason for the run under `dir` whose subagent made one call of `tool` with `args`,
   * answered with `text`, in the shape MammothLungfish.jsonl records them. */
  function judged(dir: string, tool: string, args: Record<string, unknown>, text: string) {
    const id = "toolu_01J5qoztEHbUpgGWmLEoM9F7";
    const entries = [
      {
        type: "session",
        version: 3,
        id: "01a200ad-4e6f-727e-a871-b00fa9e329cd",
        timestamp: "2026-11-22T00:34:47.535Z",
        cwd: `${RECORD.ran}/cwd`,
      },
      {
        type: "message",
        id: "334d375b",
        parentId: "b688fb87",
        timestamp: "2026-11-22T00:35:25.367Z",
        message: {
          role: "assistant",
          content: [
            { type: "toolCall", id, name: tool, arguments: { ...args, i: INTENT }, intent: INTENT },
          ],
        },
      },
      {
        type: "custom",
        customType: "tool_execution_start",
        data: { toolCallId: id, toolName: tool, startedAt: "2026-11-22T00:35:25.388Z" },
        id: "cab48c02",
        parentId: "4e412d0f",
        timestamp: "2026-11-22T00:35:25.388Z",
      },
      {
        type: "message",
        id: "6ec4fcdf",
        parentId: "0cda8208",
        timestamp: "2026-11-22T00:35:26.490Z",
        message: {
          role: "toolResult",
          toolCallId: id,
          toolName: tool,
          isError: false,
          details: { response: { provider: "parallel", sources: [] } },
          content: [{ type: "text", text }],
          timestamp: 1795307726489,
        },
      },
    ];
    mkdirSync(path.dirname(path.join(dir, TRANSCRIPT)), { recursive: true });
    writeFileSync(
      path.join(dir, TRANSCRIPT),
      entries.map((entry) => `${JSON.stringify(entry)}\n`).join("")
    );
    const run = loadRun(dir, RECORD.scenario, RECORD.label);
    return offWorldReach(run.calls, LOOPBACK, run.own, offWorldAllowed(run));
  }
  const searched = `off-world network: a web_search call in ${TRANSCRIPT} searches the web, which no host of the run's world serves`;

  // Catches: a health that holds a web search to the world's workflow, as rule 13 did before the
  // ruling, or reads the GitHub page among its sources (oven-sh/bun's) as the rig's own code.
  test("round 32's search, as c8ef305c3eab recorded it, is part of its world", () => {
    const { dir } = world();
    expect(judged(dir, "web_search", { query: QUERY }, SOURCES)).toBeUndefined();
  });

  // Catches: a health that lets a query carry a word of the measurement off the world: the run's
  // own case word, which no ordinary sentence holds.
  test("a copy whose query names a case word is off-world", () => {
    const { dir } = world();
    expect(judged(dir, "web_search", { query: `${QUERY} text-only` }, SOURCES)).toBe(
      `${searched}, and its query names "text-only", a word of the measurement`
    );
  });

  // Catches (the architect's ruling of 2026-11-22T04:06:48Z): a health that reads a message of
  // GitHub's own, which the world answers with as GitHub does and the rig's GitHub sources hold
  // (production-shapes.json's stderr `gh: Resource not accessible by integration (HTTP 403)`), as
  // a string of the world's; and one that excepts more, a value the world itself made.
  test("a copy whose query carries GitHub's own message is part of its world, and one carrying a value of the world's answers is not", () => {
    const { dir } = world();
    const copy = (query: string) => judged(dir, "web_search", { query }, SOURCES);
    expect([
      copy("Resource not accessible by integration github app"),
      copy("RRS_lACqUmVwb3NpdG9yec4AAAABzgAAAAE ruleset"),
    ]).toEqual([
      undefined,
      `${searched}, and its query carries "rrs_lacqumvwb3npdg9yec4aaaabzgaaaae", a string the world's gh stand-in answers with (fixtures.json)`,
    ]);
  });

  // Catches: a health that reads one of the ruling's five things, or its exceptions, otherwise
  // than the tester's reader: these are the tester's own checks on copies of c8ef305c3eab, the
  // remote's commit named by this remote's head. `non_fast_forward`, a code of GitHub's own, is no
  // string of the world's since the ruling of 2026-11-22T04:06:48Z.
  test("the tester's copies: a scenario, a world word, a remote commit or a page of the rig's code is off-world, and GitHub's own code or a search about a pinned action is not", () => {
    const { dir, head } = world();
    const copy = (query: string, text = SOURCES) => judged(dir, "web_search", { query }, text);
    const project =
      "[11] legion · cybermonklabs/legion\n    https://github.com/cybermonklabs/legion\n";
    expect([
      copy("threads-closed review rules"),
      copy("fernbrook widgets greet"),
      copy(`${head.slice(0, 12)} bun`),
      copy("non_fast_forward"),
      copy(QUERY, `${SOURCES}\n${project}`),
      copy("actions/checkout 3d3c42e5aac5 v7.0.1 release"),
    ]).toEqual([
      `${searched}, and its query names "threads-closed", a word of the measurement`,
      `${searched}, and its query names "fernbrook", a word of the world's own`,
      `${searched}, and its query names ${head.slice(0, 12)}, a commit of the run's own remote`,
      undefined,
      `${searched}, and it came back with https://github.com/cybermonklabs/legion, a page of the repository under test`,
      undefined,
    ]);
  });

  // Catches: a health that lets an agent read a page of the repository under test, which holds
  // this rig's code, by its blob or its raw form, as a GET of a repository the world's workflow
  // names is allowed; and one that refuses that GET.
  test("a fetch of a page of the repository under test, or its raw form, is off-world, and one of a pinned action is not", () => {
    const { dir } = world();
    const blob = "https://github.com/cybermonklabs/legion/blob/main/README.md";
    const raw = "https://raw.githubusercontent.com/cybermonklabs/legion/main/README.md";
    const refused = (tool: string, host: string, url: string) =>
      `off-world network: a ${tool} call in ${TRANSCRIPT} reaches ${host} (${url}), no host of the run's world (127.0.0.1, localhost, ::1), and no GET or HEAD with no body or ls-remote of a repository the world's workflow names (actions/checkout, oven-sh/setup-bun)`;
    expect([
      judged(dir, "read", { path: blob }, "# Legion"),
      judged(dir, "read", { path: raw }, "# Legion"),
      judged(dir, "fetch", { url: blob }, "# Legion"),
      judged(dir, "read", { path: "https://github.com/actions/checkout/releases/tag/v7.0.1" }, ""),
    ]).toEqual([
      refused("read", "github.com", blob),
      refused("read", "raw.githubusercontent.com", raw),
      refused("fetch", "github.com", blob),
      undefined,
    ]);
  });

  // Catches (the architect's rulings of 2026-11-22T16:32Z, after INFRA-764's round 33 at
  // 25ce8a43): a health that loses a bodyless GET of a pinned repository where the shell runs it
  // inside `bash -c` (with or without `timeout`), where a literal loop's variable is named by the
  // URL's own path (`/ref/` names `ref`), or inside a command substitution with a literal URL, and
  // so voided runs 7b36798f96dd, a6125589ac1b and 2c18007abee3 for reads the rulings allow; and
  // one that, to allow them, allows the same shapes reading a repository the workflow names none
  // of. Each command is the round's own, as its transcript recorded it.
  test("round 33's reads of the pinned actions inside `bash -c`, in a loop whose variable the URL names, and inside `$(…)` are part of the world, and the same of another repository is not", () => {
    const { dir } = world();
    const shapes = [
      `timeout 15 bash -c 'curl -s -o /dev/null -w "%{http_code}" https://api.github.com/repos/actions/checkout/git/refs/tags/v7.0.1' 2>&1; echo " checkout-net"`,
      [
        'for ref in "actions/checkout:3d3c42e5aac5ba805825da76410c181273ba90b1:v7.0.1" "oven-sh/setup-bun:0c5077e51419868618aeaa5fe8019c62421857d6:v2.2.0"; do',
        '  repo="${ref%%:*}"; rest="${ref#*:}"; sha="${rest%%:*}"; tag="${rest#*:}"',
        '  echo "=== $repo $tag ($sha) ==="',
        `  curl -s -m 15 "https://api.github.com/repos/$repo/git/ref/tags/$tag" 2>/dev/null | grep -E '"sha"|message|Not Found' | head -5`,
        "done",
      ].join("\n"),
      [
        `SHA=$(curl -s -m 8 https://api.github.com/repos/oven-sh/setup-bun/git/refs/tags/v2.2.0 | grep -m1 '"sha"' | grep -oE '[0-9a-f]{40}')`,
        'echo "tagobj=$SHA"',
        `curl -s -m 8 "https://api.github.com/repos/oven-sh/setup-bun/git/tags/$SHA" 2>/dev/null | grep -E '"sha"' | head`,
      ].join("\n"),
    ];
    const elsewhere = (command: string) =>
      command
        .replaceAll("repos/actions/checkout", "repos/someone/elsewhere")
        .replaceAll('"actions/checkout:', '"someone/elsewhere:')
        .replaceAll("repos/oven-sh/setup-bun", "repos/someone/elsewhere")
        .replaceAll('"oven-sh/setup-bun:', '"someone/elsewhere:');
    expect(shapes.map((command) => judged(dir, "bash", { command }, ""))).toEqual([
      undefined,
      undefined,
      undefined,
    ]);
    for (const command of shapes)
      expect(judged(dir, "bash", { command: elsewhere(command) }, "")).toStartWith(
        `off-world network: a bash call in ${TRANSCRIPT} reaches api.github.com (`
      );
  });
});

// Rule 1, finished, read on the run's own boot record (<run>/booted) held to the scenario's one
// definition of the roles it boots (completion-answers.ts's ANSWERS' `boots` and SKIPS). Each row
// builds a run directory as rig.sh leaves it, named as cmd_run names a run.
describe("health: rule 1, finished, on the run's boot record", () => {
  const runs = temporaryDirectories("health-boot-record");
  /** A transcript with its session's header and one model turn. */
  const TRANSCRIPT = [
    { type: "session", id: "s1" },
    { type: "message", message: { role: "assistant", content: [{ type: "text", text: "Done." }] } },
  ]
    .map((entry) => `${JSON.stringify(entry)}\n`)
    .join("");
  /** The implementer's last words in the measured run, threads-closed-correct-head-1 at
   * 7bf20811: its out.txt and its out-implementer.txt, byte for byte the same. */
  const IMPLEMENTER_OUT =
    "WAITING on the reviewer to read the fix, resolve PRRT_kwDOfix001 and PRRT_kwDOfix002, approve head `a0ce49ee301f9d20205337743fb0a88df6480e8c`, and notify the architect, whose acceptance is the gate on the fix head.\nexit=0\n";
  const FIRST = "sessions/2026-11-15T20-59-48-435Z_01a1e102.jsonl";
  /** A run directory named `name` holding `files`, each a path under the run and its text; a path
   * ending in `/` is a directory. */
  function runOf(name: string, files: Record<string, string>): string {
    const dir = path.join(runs(), name);
    mkdirSync(dir);
    for (const [file, text] of Object.entries(files)) {
      const full = path.join(dir, file);
      mkdirSync(file.endsWith("/") ? full : path.dirname(full), { recursive: true });
      if (!file.endsWith("/")) writeFileSync(full, text);
    }
    return dir;
  }
  const judge = (dir: string) =>
    unfinished({ dir, scenario: scenarioOf(dir) ?? "", label: labelOf(dir) });
  /** A threads-closed run both of whose agents ran to their exit, with `booted` its record. */
  const both = (name: string, booted: string) =>
    runOf(name, {
      booted,
      "out-implementer.txt": IMPLEMENTER_OUT,
      "out.txt": "Approved the fix head.\nexit=0\n",
      [FIRST]: TRANSCRIPT,
      "sessions/2026-11-15T20-59-48-435Z_01a1e102/": "",
      "sessions/reviewer/2026-11-15T21-06-10-000Z_01a1e103.jsonl": TRANSCRIPT,
    });
  const SKIPPED = `skip reviewer ${SKIPS["threads-closed"]?.["code-defect"]?.reviewer}\n`;

  // Catches: the measured run read healthy. Its reviewer's stand-in timed out, so the reviewer
  // never launched: sessions/reviewer/ is empty and out.txt is still the implementer's, its exit=
  // line included, which a reader of out.txt's last line took for the run's end.
  test("the measured run whose reviewer was booted and never ran is unfinished", () => {
    const files = {
      "out-implementer.txt": IMPLEMENTER_OUT,
      "out.txt": IMPLEMENTER_OUT,
      [FIRST]: TRANSCRIPT,
      "sessions/2026-11-15T20-59-48-435Z_01a1e102/": "",
      "sessions/reviewer/": "",
    };
    expect(
      judge(
        runOf("threads-closed-correct-head-1", {
          ...files,
          booted: "boot implementer\nboot reviewer\n",
        })
      )
    ).toBe(
      "unfinished: the reviewer left no transcript: <run>/sessions/reviewer holds no .jsonl of its own with an entry"
    );
    // As it was recorded, before a run kept a boot record.
    expect(judge(runOf("threads-closed-correct-head-1", files))).toBe(
      "no boot record: the run has no <run>/booted, so which roles of threads-closed it booted, and whether each finished, is unknown"
    );
    // The same run where worker_boot moves the implementer's output aside rather than copying
    // it: the reviewer, booted last, has no output of its own at all.
    const { "out.txt": _, ...moved } = files;
    expect(
      judge(
        runOf("threads-closed-correct-head-1", {
          ...moved,
          booted: "boot implementer\nboot reviewer\n",
        })
      )
    ).toBe("unfinished: the reviewer's output, <run>/out.txt, is missing");
  });

  // Catches: a reader that holds a run to more than it booted, or to less.
  test("a run whose every booted role finished is finished, a relaunch's repeated line one boot", () => {
    expect(judge(both("threads-closed-correct-head-2", "boot implementer\nboot reviewer\n"))).toBe(
      undefined
    );
    expect(
      judge(
        both("threads-closed-correct-base-2", "boot implementer\nboot implementer\nboot reviewer\n")
      )
    ).toBe(undefined);
    // As boot_agent writes it now: each launch it asks the boot's reader about followed by the
    // `since` it asked with, and a relaunch's repeated `boot` line, asked nothing, with none.
    expect(
      judge(
        both(
          "threads-closed-correct-head-11",
          "boot implementer\nsince implementer 2026-11-16T09:00:00.000Z\nboot implementer\nboot reviewer\nsince reviewer 2026-11-16T09:10:00.000Z\n"
        )
      )
    ).toBe(undefined);
    expect(
      judge(
        runOf("reviewer-text-findings-defect-base-1", {
          booted: "boot reviewer\n",
          "out.txt": "Requested changes.\nexit=0\n",
          [FIRST]: TRANSCRIPT,
        })
      )
    ).toBe(undefined);
  });

  // Catches: the code-defect run that boots no reviewer, because its implementer sent the reviewer
  // nothing and so already failed the message the case asks of it, and the accept and crossing
  // runs, which never boot one, read as unfinished against a definition that names the reviewer;
  // and a skip the scenario and case do not declare read as one they do, accept's reason in
  // code-defect and code-defect's old reason, from before the implementer told the reviewer too,
  // among them.
  test("a code-defect, accept or crossing run that skipped the reviewer for its declared reason is finished, and no other skip is", () => {
    const skipped = (name: string, booted: string) =>
      judge(
        runOf(name, {
          booted,
          "out.txt": "Told the architect the thread's sentence is false.\nexit=0\n",
          [FIRST]: TRANSCRIPT,
        })
      );
    const declared = SKIPS["threads-closed"]?.["code-defect"]?.reviewer;
    expect(declared).toBe(
      "the implementer sent the reviewer nothing, which fails the message the case asks"
    );
    expect(skipped("threads-closed-code-defect-head-1", `boot implementer\n${SKIPPED}`)).toBe(
      undefined
    );
    const accepted = "skip reviewer the accept case scores the implementer's answer alone\n";
    expect(skipped("threads-closed-accept-head-1", `boot implementer\n${accepted}`)).toBe(
      undefined
    );
    const crossed = "skip reviewer the crossing case scores the implementer's answer alone\n";
    expect(skipped("threads-closed-crossing-head-1", `boot implementer\n${crossed}`)).toBe(
      undefined
    );
    expect(skipped("threads-closed-crossing-head-2", `boot implementer\n${accepted}`)).toBe(
      'undeclared skip: <run>/booted skips the reviewer because "the accept case scores the implementer\'s answer alone", where threads-closed crossing declares "the crossing case scores the implementer\'s answer alone" (completion-answers.ts\'s SKIPS)'
    );
    expect(skipped("threads-closed-code-defect-head-3", `boot implementer\n${accepted}`)).toBe(
      `undeclared skip: <run>/booted skips the reviewer because "the accept case scores the implementer's answer alone", where threads-closed code-defect declares "${declared}" (completion-answers.ts's SKIPS)`
    );
    expect(
      skipped(
        "threads-closed-code-defect-head-4",
        "boot implementer\nskip reviewer the implementer sent the reviewer nothing\n"
      )
    ).toBe(
      `undeclared skip: <run>/booted skips the reviewer because "the implementer sent the reviewer nothing", where threads-closed code-defect declares "${declared}" (completion-answers.ts's SKIPS)`
    );
    expect(skipped("threads-closed-correct-head-3", `boot implementer\n${SKIPPED}`)).toBe(
      `undeclared skip: <run>/booted skips the reviewer because "${declared}", and threads-closed correct declares no skip of it (completion-answers.ts's SKIPS)`
    );
    expect(
      skipped("threads-closed-code-defect-head-2", "boot implementer\nskip reviewer it was late\n")
    ).toBe(
      `undeclared skip: <run>/booted skips the reviewer because "it was late", where threads-closed code-defect declares "${declared}" (completion-answers.ts's SKIPS)`
    );
    // A run named by its id reads its case from its record (run-names.ts's `runIdentity`), never
    // from its directory's name, which names nothing.
    const recorded = (id: string, runCase: string) => {
      const dir = runOf(id, {
        booted: `boot implementer\n${SKIPPED}`,
        "out.txt": "Told the architect the thread's sentence is false.\nexit=0\n",
        [FIRST]: TRANSCRIPT,
      });
      writeFileSync(
        `${dir}.json`,
        JSON.stringify({
          run: id,
          scenario: "threads-closed",
          case: runCase,
          label: "head",
          n: 3,
          name: `threads-closed-${runCase}-head-3`,
          batch: "legion-00000000-1",
          ran: dir,
        })
      );
      return judge(dir);
    };
    expect(recorded("0123456789ab", "code-defect")).toBe(undefined);
    expect(recorded("0123456789ac", "correct")).toBe(
      `undeclared skip: <run>/booted skips the reviewer because "${declared}", and threads-closed correct declares no skip of it (completion-answers.ts's SKIPS)`
    );
  });

  // Catches: a run with no record read on the scenario's definition in its place, and a record
  // that does not decide each role of the definition once, in its order, read as one that does.
  test("a missing record, a role outside the definition, a role decided twice or out of order, or one left out is unfinished", () => {
    const definition =
      "threads-closed boots the implementer, then the reviewer (completion-answers.ts's ANSWERS)";
    expect(judge(both("threads-closed-correct-head-4", "boot reviewer\nboot implementer\n"))).toBe(
      `boot record: <run>/booted starts with \`boot reviewer\`, not \`boot implementer\`; ${definition}`
    );
    expect(judge(both("threads-closed-correct-head-5", `${SKIPPED}boot reviewer\n`))).toBe(
      `boot record: <run>/booted starts with \`skip reviewer\`, not \`boot implementer\`; ${definition}`
    );
    expect(judge(both("threads-closed-correct-head-6", ""))).toBe(
      `boot record: <run>/booted starts with no line, not \`boot implementer\`; ${definition}`
    );
    expect(
      judge(both("threads-closed-correct-head-7", "boot implementer\nboot reviewer\nboot tester\n"))
    ).toBe(
      `boot record: <run>/booted decides the tester, a role threads-closed boots none of; ${definition}`
    );
    expect(
      judge(
        both("threads-closed-correct-head-8", "boot implementer\nboot reviewer\nboot implementer\n")
      )
    ).toBe(`boot record: <run>/booted decides the implementer twice; ${definition}`);
    expect(judge(both("threads-closed-correct-head-9", "boot implementer\n"))).toBe(
      `boot record: <run>/booted neither boots nor skips the reviewer; ${definition}`
    );
    expect(() => judge(both("threads-closed-correct-head-10", "launched implementer\n"))).toThrow(
      "booted:1 is none of `boot <role>`, `since <role> <time>` and `skip <role> <reason>`: launched implementer"
    );
    expect(() =>
      judge(
        both(
          "threads-closed-correct-head-12",
          "boot implementer\nsince reviewer 2026-11-16T09:00:00.000Z\nboot reviewer\n"
        )
      )
    ).toThrow(
      "booted:2 is a `since` line that does not follow its role's `boot` line: since reviewer 2026-11-16T09:00:00.000Z"
    );
  });

  // Catches: a role's record that is another's standing in for it: the first role's transcript
  // read from a subagent's directory or a later role's, and an output that is missing or never
  // ended read as one that did.
  test("a booted role finishes on its own output and its own top-level transcript alone", () => {
    const booted = "boot implementer\nboot reviewer\n";
    expect(
      judge(
        runOf("threads-closed-correct-head-11", {
          booted,
          "out-implementer.txt": IMPLEMENTER_OUT,
          "out.txt": "Approved the fix head.\nexit=0\n",
          "sessions/2026-11-15T20-59-48-435Z_01a1e102/task-1.jsonl": TRANSCRIPT,
          "sessions/reviewer/2026-11-15T21-06-10-000Z_01a1e103.jsonl": TRANSCRIPT,
        })
      )
    ).toBe(
      "unfinished: the implementer left no transcript: <run>/sessions holds no .jsonl of its own with an entry"
    );
    expect(
      judge(
        runOf("threads-closed-correct-head-12", {
          booted,
          "out-implementer.txt": IMPLEMENTER_OUT,
          "out.txt": "Approved the fix head.\nexit=0\n",
          [FIRST]: TRANSCRIPT,
          "sessions/reviewer/2026-11-15T21-06-10-000Z_01a1e103/task-1.jsonl": TRANSCRIPT,
        })
      )
    ).toBe(
      "unfinished: the reviewer left no transcript: <run>/sessions/reviewer holds no .jsonl of its own with an entry"
    );
    expect(
      judge(
        runOf("threads-closed-correct-head-13", {
          booted,
          "out-implementer.txt": IMPLEMENTER_OUT,
          "out.txt": "Approved the fix head.\nexit=0\n",
          [FIRST]: "",
          "sessions/reviewer/2026-11-15T21-06-10-000Z_01a1e103.jsonl": TRANSCRIPT,
        })
      )
    ).toBe(
      "unfinished: the implementer left no transcript: <run>/sessions holds no .jsonl of its own with an entry"
    );
    expect(
      judge(
        runOf("threads-closed-correct-head-14", {
          booted,
          "out.txt": "Approved the fix head.\nexit=0\n",
          [FIRST]: TRANSCRIPT,
          "sessions/reviewer/2026-11-15T21-06-10-000Z_01a1e103.jsonl": TRANSCRIPT,
        })
      )
    ).toBe("unfinished: the implementer's output, <run>/out-implementer.txt, is missing");
    expect(
      judge(
        runOf("threads-closed-correct-head-15", {
          booted,
          "out-implementer.txt": IMPLEMENTER_OUT,
          "out.txt": "Reading the fix head.\n",
          [FIRST]: TRANSCRIPT,
          "sessions/reviewer/2026-11-15T21-06-10-000Z_01a1e103.jsonl": TRANSCRIPT,
        })
      )
    ).toBe("unfinished: the reviewer's output, <run>/out.txt, has no exit= line");
  });

  // Catches: a reader that keeps its own list of the roles a scenario boots. A definition that
  // gains a role holds every run to it, with no edit to health: a run that neither boots nor
  // skips it, and one that decides it out of the definition's order, are unfinished.
  test("a definition that gains a role makes a run without it unfinished", () => {
    const entry = ANSWERS["threads-closed"] as unknown as { boots: readonly string[] };
    const defined = entry.boots;
    entry.boots = ["implementer", "tester", "reviewer"];
    try {
      const definition =
        "threads-closed boots the implementer, then the tester, then the reviewer (completion-answers.ts's ANSWERS)";
      expect(
        judge(both("threads-closed-correct-head-16", "boot implementer\nboot reviewer\n"))
      ).toBe(
        `boot record: <run>/booted decides the reviewer where the tester comes; ${definition}`
      );
      expect(judge(both("threads-closed-correct-head-17", "boot implementer\n"))).toBe(
        `boot record: <run>/booted neither boots nor skips the tester; ${definition}`
      );
    } finally {
      entry.boots = defined;
    }
    expect(judge(both("threads-closed-correct-head-18", "boot implementer\nboot reviewer\n"))).toBe(
      undefined
    );
  });
});

// Rule 3, taken, read for the run's case (completion-answers.ts's ANSWERS' `takes`): a text-only
// reviewer that pushed its fix records no pass at it, approves nothing and waits for the
// implementer's last chance (Verification v47's k1), so it completes nothing while it waits, a
// last-push reviewer that waits instead of approving completes nothing either (what it does there
// is k3's measurement, which a void run would hide), and a defect reviewer completes its round.
// Each row is a run directory named as cmd_run named a run before the names left the world, which
// is its record of its scenario, case and label.
// Catches: a world that calls a waiting reviewer unhealthy where the case does not end its round
// with a completion, and one that requires no completion of the case that does.
describe("health: rule 3, taken, by the run's case", () => {
  const runs = temporaryDirectories("health-taken");
  const COMPLETED = JSON.stringify({
    at: "2026-11-14T00:05:00.000Z",
    path: "/legion/v1/handoff/complete",
    status: 200,
    role: "reviewer",
    grantId: "g-1",
  });
  /** Rule 3's reason for the run `name` whose daemon stand-in logged its own opening line and, with
   * `completed`, the reviewer's completion taken 200. */
  const taken = (name: string, completed: boolean) => {
    const dir = path.join(runs(), name);
    mkdirSync(dir, { recursive: true });
    writeFileSync(
      path.join(dir, "daemon.log"),
      [
        JSON.stringify({ at: "2026-11-14T00:00:00.000Z", opened: "daemon.log" }),
        ...(completed ? [COMPLETED] : []),
        "",
      ].join("\n")
    );
    return untaken({ dir, scenario: scenarioOf(dir) ?? "", label: labelOf(dir), repos: [] });
  };
  const NONE =
    "no completion taken: no completion of the reviewer was answered as the head label's daemon answers one it takes";

  test("a text-only reviewer that completed nothing is no unhealthy run", () => {
    expect(taken("reviewer-text-findings-text-only-head-1", false)).toBe(undefined);
    expect(taken("reviewer-text-findings-text-only-head-2", true)).toBe(undefined);
  });

  test("a defect reviewer whose completion the world took none of is unhealthy", () => {
    expect(taken("reviewer-text-findings-defect-head-1", false)).toBe(NONE);
    expect(taken("reviewer-text-findings-defect-head-2", true)).toBe(undefined);
  });

  test("a last-push reviewer that completed nothing is no unhealthy run", () => {
    expect(taken("reviewer-text-findings-last-push-head-1", false)).toBe(undefined);
    expect(taken("reviewer-text-findings-last-push-head-2", true)).toBe(undefined);
  });

  // Catches: a `takes` keyed by a word that is no case of its scenario, which would require a
  // completion of no run at all.
  test("each case `takes` names is a case of its scenario", () => {
    for (const [scenario, { takes }] of Object.entries(ANSWERS))
      for (const named of Object.keys(takes ?? {}))
        expect([scenario, named, casesOf(scenario).includes(named)]).toEqual([
          scenario,
          named,
          true,
        ]);
    expect(ANSWERS["reviewer-text-findings"]?.takes).toEqual({ defect: "reviewer" });
  });
});

// Rule 15, service provenance, on the run's Dispatch server: the server a run's agents call is
// the one built from the run's label's own checkout (rig.sh's services_up), since what an agent
// reads of it differs across versions. Catches: a base run on a server the head checkout built,
// and a provenance that names the server shared, as the rig's own, where the rule would hold it
// to no checkout at all.
describe("health: rule 15, service provenance, the run's own Dispatch server", () => {
  const runs = temporaryDirectories("health-dispatch-provenance");
  const BASE = "/checkouts/base-ff2a10ef";
  const HEAD = "/checkouts/head";
  /** A base run whose built-from names BASE and whose provenance names every service of its
   * label's own from BASE but the Dispatch server, which `dispatch` names. */
  function baseRun(name: string, dispatch: string): string {
    const dir = path.join(runs(), name);
    mkdirSync(dir);
    writeFileSync(path.join(dir, "built-from"), `checkout ${BASE}\ncommit ff2a10ef parents=\n`);
    writeFileSync(
      path.join(dir, "services-from"),
      [
        `envoy-listener label ${BASE} sha256:${"0".repeat(64)}`,
        dispatch,
        `plugins label ${BASE} -`,
        `model-route label ${BASE} -`,
        `daemon-facts label ${BASE} sha256:${"2".repeat(64)}`,
        `worker-pane label ${BASE} sha256:${"2".repeat(64)}`,
        "omp pin omp@stub missing",
        "",
      ].join("\n")
    );
    return dir;
  }
  const judge = (dir: string) => serviceProvenance({ dir, label: labelOf(dir) });

  test("a base run whose Dispatch server the head checkout built is unhealthy", () => {
    expect(
      judge(
        baseRun("threads-closed-correct-base-1", `dispatch label ${HEAD} sha256:${"1".repeat(64)}`)
      )
    ).toBe(
      `foreign service: the run's dispatch came from ${HEAD}, not from ${BASE}, the checkout of the base label its built-from names`
    );
  });

  test("a base run whose provenance names its Dispatch server shared is unhealthy", () => {
    expect(
      judge(
        baseRun("threads-closed-correct-base-2", `dispatch shared ${HEAD} sha256:${"1".repeat(64)}`)
      )
    ).toBe(
      `service provenance: <run>/services-from names no dispatch of the base label's own checkout (${BASE})`
    );
  });

  test("a base run on the Dispatch server its own checkout built holds", () => {
    expect(
      judge(
        baseRun("threads-closed-correct-base-3", `dispatch label ${BASE} sha256:${"1".repeat(64)}`)
      )
    ).toBeUndefined();
  });
});

// Rule 16's 403 that every agent of every run may meet, read out of a label's own source: each
// route Dispatch's routes table gives the person scope (routes_table.go's `routeScopes`) that the
// agents' client sends (envoy-client's dispatch-http.ts), person-routes.ts's `labelPersonRoutes`.
// Catches: a reading that holds a list in place of the class, so `dispatch inbox`'s three reads
// or `dispatch review`'s post go undeclared, as the tester's row of 605f3c38 measured; one that
// takes a route that needs no person scope, or a person-scoped one no command of the client sends;
// and a source of another shape read as a Dispatch that refuses nothing, where the architect's
// ruling of 2026-11-21 has a marker that stops matching refuse, naming the file and the marker.
describe("health: rule 16, Dispatch's 403 to the person-scoped routes a label's agents call", () => {
  const CHECKOUT = path.resolve(import.meta.dir, "../../../..");
  const SCOPES = "packages/envoy/internal/dispatch/api/routes_table.go";
  const CLIENT = "packages/envoy-client/src/dispatch-http.ts";
  const OPENER = "var routeScopes = map[string]routeScopeSet{";
  const PERSON = "model.ScopePerson";
  const copies = temporaryDirectories("health-person-routes");
  /** A checkout holding this checkout's routes table and client, each given through its edit, a
   * source of null left out. */
  function copyOf(
    scopes: ((text: string) => string) | null,
    client: ((text: string) => string) | null
  ): string {
    const dir = copies();
    for (const [file, edit] of [
      [SCOPES, scopes],
      [CLIENT, client],
    ] as const) {
      if (edit === null) continue;
      mkdirSync(path.dirname(path.join(dir, file)), { recursive: true });
      writeFileSync(path.join(dir, file), edit(readFileSync(path.join(CHECKOUT, file), "utf8")));
    }
    return dir;
  }
  const same = (text: string) => text;

  test("this checkout's person-scoped routes its agents' client calls hold dispatch inbox's, review's and send-to-agent's, and only routes routeScopes gives the person scope", () => {
    const routes = labelPersonRoutes(CHECKOUT);
    expect(routes).toEqual(
      expect.arrayContaining([
        "GET /api/v1/inbox",
        "GET /api/v1/me/replies",
        "GET /api/v1/credential-requests",
        "POST /api/v1/artifacts/{id}/versions/{number}/reviews",
        "POST /api/v1/agents/{session_id}/messages",
      ])
    );
    const table = readFileSync(path.join(CHECKOUT, SCOPES), "utf8");
    const open = table.indexOf(OPENER);
    const person = table
      .slice(open, table.indexOf("\n}\n", open))
      .split("\n")
      .filter((row) => row.includes(PERSON));
    // Each route read is a row of routeScopes whose set holds the person scope.
    const scoped = person.map((row) => row.trim().split('":')[0]?.slice(1));
    expect(routes.filter((route) => !scoped.includes(route))).toEqual([]);
    // The client's own read of a handoff needs no person scope, and routeScopes gives the person
    // scope to routes no command of the client sends, so the reading holds fewer than its rows.
    expect(routes).not.toContain("GET /api/v1/issues/{key}/handoffs/{phase}/latest");
    expect(routes.length).toBeLessThan(person.length);
  });

  test("a Dispatch source whose routeScopes or route paths are of no shape this reads is refused, naming the file and the marker", () => {
    const why = (dir: string) =>
      `the Dispatch sources of ${dir} are of no shape its person-scoped routes are read by: `;
    const renamed = "var routeScopesByRoute = map[string]routeScopeSet{";
    const untabled = copyOf((text) => text.replace(OPENER, renamed), same);
    expect(() => labelPersonRoutes(untabled)).toThrow(
      `${why(untabled)}${SCOPES} holds no ${JSON.stringify(OPENER)}, the line that opens the table of the scopes each route needs`
    );
    const table = readFileSync(path.join(CHECKOUT, SCOPES), "utf8");
    const row = table.split("\n").indexOf(OPENER) + 2;
    const inserted = `${OPENER}\n\tinboxRoute: {${PERSON}},`;
    const unrowed = copyOf((text) => text.replace(OPENER, inserted), same);
    expect(() => labelPersonRoutes(unrowed)).toThrow(
      `${why(unrowed)}${SCOPES}:${row} is no row of routeScopes (\`"<METHOD> <pattern>": {<scope>, …},\`): inboxRoute: {${PERSON}},`
    );
    const personless = copyOf((text) => text.replaceAll(PERSON, "model.ScopeSelf"), same);
    expect(() => labelPersonRoutes(personless)).toThrow(
      `${why(personless)}${SCOPES}'s routeScopes gives no route model.ScopePerson, the person scope`
    );
    const tableless = copyOf(null, same);
    expect(() => labelPersonRoutes(tableless)).toThrow(
      `${why(tableless)}${SCOPES} is not there, which holds the scopes each route needs (routeScopes)`
    );
    const pathless = copyOf(same, (text) => text.replace(/"api",(\s*)"v1"/g, '"api",$1"v2"'));
    expect(() => labelPersonRoutes(pathless)).toThrow(
      `${why(pathless)}${CLIENT} spells no route path ["api", "v1", …], which is how it names each route it sends`
    );
    const sent = 'this.#json("GET", ["api", "v1", "inbox"]';
    const client = readFileSync(path.join(CHECKOUT, CLIENT), "utf8");
    const line = client.slice(0, client.indexOf(sent)).split("\n").length;
    const unsent = copyOf(same, (text) => text.replace(sent, 'this.#get(["api", "v1", "inbox"]'));
    expect(() => labelPersonRoutes(unsent)).toThrow(
      `${why(unsent)}${CLIENT}:${line} spells the route path ["api", "v1", "inbox"], which no call this reads sends`
    );
  });
});

// A path a tool call names under /tmp, read as rig.sh's private_world mounts it from the run's
// world (world_paths, which the run recorded in its namespace.txt): the run's own <run>/tmp, bound
// over the machine's, but under a path the world shows at or under /tmp, which the namespace
// binds back onto its own path. Catches: a write into a run's own /tmp read as one into the
// machine's, which the tester measured on 2026-11-16 in 2 of 8 runs and in 4 of 8 at c273084d
// (/tmp/body.md, /tmp/reply-body.txt, /tmp/lweval1-approve.json, /tmp/lweval1-body.md, each a
// run's own); and, on a host whose work directory is under /tmp (no TMPDIR), a path the world
// shows there read as the run's own, or the run's own directory read as the machine's /tmp.
describe("health: a path under /tmp, as the run's namespace mounts it", () => {
  /** A world that covers, and hides again, nothing. */
  const bare = { covers: [], credentials: [], hidden: [] };
  test("a path under /tmp is the run's own <run>/tmp, and every other path is itself", () => {
    const pod = {
      ...bare,
      own: RUN,
      shown: [RUN, "/work/profiles/head", "/work/profiles/head/key"],
    };
    expect(mounted(pod, "/tmp/body.md")).toBe(`${RUN}/tmp/body.md`);
    expect(mounted(pod, "/tmp/reply/x.json")).toBe(`${RUN}/tmp/reply/x.json`);
    expect(mounted(pod, "/tmp")).toBe(`${RUN}/tmp`);
    for (const named of [
      "/legion/tmp/placeholder",
      "/tmpfile",
      `${RUN}/tmp/body.md`,
      "/work/profiles/head/key/minted",
    ])
      expect(mounted(pod, named)).toBe(named);
  });

  test("a path the world shows under /tmp is that path, and the rest of /tmp the run's own", () => {
    const work = "/tmp/skill-scenarios";
    const own = `${work}/runs/threads-closed-correct-head-1`;
    const head = `${work}/profiles/head`;
    const world = {
      ...bare,
      own,
      shown: [own, head, `${head}/key`, `${work}/services`, "/usr/bin"],
    };
    for (const named of [
      own,
      `${own}/ws/README.md`,
      `${head}/key/minted`,
      `${work}/services/dispatch-token`,
    ])
      expect(mounted(world, named)).toBe(named);
    expect(mounted(world, "/tmp/body.md")).toBe(`${own}/tmp/body.md`);
    // What the world hides under /tmp is not there: a sibling's directory, the other label's
    // profile and a path whose name only starts as a shown one's does are names in the run's own
    // /tmp.
    for (const named of [
      `${work}/runs/threads-closed-correct-base-1/out.txt`,
      `${work}/profiles/base/built-from`,
      `${head}less/notes.md`,
    ])
      expect(mounted(world, named)).toBe(`${own}${named}`);
  });

  test("a world that shows /tmp itself keeps the machine's", () => {
    expect(mounted({ ...bare, own: RUN, shown: [RUN, "/tmp"] }, "/tmp/body.md")).toBe(
      "/tmp/body.md"
    );
  });

  // The rest of private_world's mounts, each in its order (rig.sh's private_world), on the world
  // round 25's threads-closed-correct-head-1 (6f38c7583a6d) recorded: its covers, its
  // credentials, its own directory with its logs hidden again, and a path shown under a cover.
  // Catches: `mounted` reading /tmp alone, so a name under /dev/shm, a covered path, a credentials
  // path or the hidden logs reads as the machine's own path, which another run's read of the same
  // name then matches (rule 9) and the line of writes outside the run lists; and the other way, a
  // path the world shows under a cover read as covered.
  test("a name under /dev/shm, a cover, a credentials path or a hide reaches no path of the machine's", () => {
    const scratch = "/legion/scratch/tmp";
    const own = `${scratch}/b1/w/runs/6f38c7583a6d`;
    const recorded = {
      own,
      covers: [scratch, "/legion/workspaces", `${scratch}/b1/w`],
      credentials: ["/var/run/legion", "/run/legion", "/var/run/secrets", "/run/secrets"],
      shown: [own, `${scratch}/b1/w/installs/4c0768d2ae4b`, `${scratch}/bin`, "/usr/bin"],
      hidden: [`${own}/logs`],
    };
    for (const named of [
      "/dev/shm/LWEVAL-1-review.json",
      "/dev/shm",
      `${scratch}/another-role/notes.md`,
      `${scratch}/b1/w/runs/2ba32fad2738/out.txt`,
      "/legion/workspaces/sjawhar/legion/infra-764/README.md",
      "/run/secrets/kubernetes.io/token",
      `${own}/logs/dispatch.log`,
    ])
      expect([named, mounted(recorded, named)]).toEqual([named, undefined]);
    for (const named of [
      `${own}/ws/README.md`,
      `${scratch}/b1/w/installs/4c0768d2ae4b/skills/x/SKILL.md`,
      `${scratch}/bin/jq`,
      "/usr/bin/git",
      "/etc/hostname",
    ])
      expect(mounted(recorded, named)).toBe(named);
    expect(mounted(recorded, "/tmp/body.md")).toBe(`${own}/tmp/body.md`);
  });

  // A world whose work directory is under /dev/shm, as rig.test.ts's is (memoryRoot): each path it
  // shows there is bound back into the launch's own /dev/shm (rig.sh's private_world). Catches:
  // `mounted` reading every name under /dev/shm as the launch's tmpfs, so a write into the run's
  // own directory there reads as kept nowhere; and the other way, the hidden logs, a sibling's
  // directory or another name of the machine's /dev/shm read as a path of the machine's.
  test("a path the world shows under /dev/shm is that path, and the rest of /dev/shm the launch's own", () => {
    const work = "/dev/shm/suite-a1b2c3/work";
    const own = `${work}/runs/threads-closed-correct-head-1`;
    const world = {
      ...bare,
      own,
      covers: [work],
      shown: [own, `${work}/profiles/head`, "/usr/bin"],
      hidden: [`${own}/logs`],
    };
    for (const named of [own, `${own}/ws/README.md`, `${work}/profiles/head/key/minted`])
      expect(mounted(world, named)).toBe(named);
    for (const named of [
      `${own}/logs/dispatch.log`,
      `${work}/runs/threads-closed-correct-base-1/out.txt`,
      `${own}less/notes.md`,
      "/dev/shm/LWEVAL-1-review.json",
    ])
      expect([named, mounted(world, named)]).toEqual([named, undefined]);
    expect(mounted(world, "/tmp/body.md")).toBe(`${own}/tmp/body.md`);
  });
});

// Rule 17, names: a scenario, case or label word an agent says, by the vocabulary run-names.ts
// derives from the rig's scenario table and the labels (never typed here), is a leak only where
// an input of the run held it before any agent of the run wrote it. Catches: the batch at
// c273084d, 16 of whose 36 runs reasoned from the run's name, read healthy; and the preflight at
// cc09c638, three of whose head runs were called unhealthy for "text-only", a word their agents
// made: no skill file, user message or tool result held it before they wrote it.
describe("health: rule 17, names, traced to where an agent got them", () => {
  const runs = temporaryDirectories("health-names");
  const header = { type: "session", id: "s1" };
  const at = (minute: number) => `2026-11-16T01:${String(minute).padStart(2, "0")}:00.000Z`;
  const user = (text: string, minute = 0) => ({
    type: "message",
    timestamp: at(minute),
    message: { role: "user", content: [{ type: "text", text }] },
  });
  const result = (tool: string, text: string, minute = 0) => ({
    type: "message",
    timestamp: at(minute),
    message: {
      role: "toolResult",
      toolName: tool,
      toolCallId: "c1",
      content: [{ type: "text", text }],
    },
  });
  const says = (kind: "text" | "thinking", text: string, minute = 0) => ({
    type: "message",
    timestamp: at(minute),
    message: {
      role: "assistant",
      content: [kind === "text" ? { type: "text", text } : { type: "thinking", thinking: text }],
    },
  });
  const call = (tool: string, args: object, minute = 0) => ({
    type: "message",
    timestamp: at(minute),
    message: {
      role: "assistant",
      content: [{ type: "toolCall", id: "c1", name: tool, arguments: args }],
    },
  });
  const delivered = (text: string, minute = 0) => ({
    type: "custom_message",
    customType: "envoy-message",
    timestamp: at(minute),
    content: text,
  });
  /** A run holding `transcripts` (each a path under the run and its entries, after a header: the
   * session `s1`'s, unless the first entry is a header of its own) and `files` (each a path under
   * the run and its text), at `dir`, read as `rig.sh health` reads it. */
  function runWith(
    transcripts: Record<string, object[]>,
    files: Record<string, string> = {},
    dir = path.join(runs(), "0123456789ab")
  ) {
    for (const [file, entries] of Object.entries(transcripts)) {
      const own = (entries[0] as { type?: unknown } | undefined)?.type === "session";
      mkdirSync(path.dirname(path.join(dir, file)), { recursive: true });
      writeFileSync(
        path.join(dir, file),
        [...(own ? [] : [header]), ...entries].map((entry) => `${JSON.stringify(entry)}\n`).join("")
      );
    }
    for (const [file, text] of Object.entries(files)) {
      mkdirSync(path.dirname(path.join(dir, file)), { recursive: true });
      writeFileSync(path.join(dir, file), text);
    }
    return loadRun(dir, "threads-closed", "head");
  }
  const FILE = "sessions/s1.jsonl";

  test("a word a tool result held before the agent wrote it makes the run unhealthy, naming the result", () => {
    const thought =
      "The run name points to a text-only case, so I expect the findings to be wording rather than behavioural.";
    const run = runWith({
      [FILE]: [
        user("Review the pull request."),
        call("read", { path: "docs/cli.md" }),
        result("read", "These are the text-only findings."),
        says("thinking", `Let me look again. ${thought} Next.`),
      ],
    });
    expect(namesSaid(run)).toBe(
      `names: a thinking part in ${FILE} says "text-only": "${thought}", which the result of read in ${FILE} held before any agent of the run wrote it`
    );
    // A case word beside a label word, as a run's name puts them, that a message held.
    expect(
      namesSaid(
        runWith({
          [FILE]: [
            delivered("The defect head world is yours."),
            says("text", "This is the defect head run, so the verdict is CHANGES."),
          ],
        })
      )
    ).toBe(
      `names: a text part in ${FILE} says "defect head": "This is the defect head run, so the verdict is CHANGES.", which the envoy-message message in ${FILE} held before any agent of the run wrote it`
    );
    // Every distinctive word of the derived vocabulary, a tool result's before the agent's.
    const missed = vocabulary(["head", "base"]).distinctive.filter(
      (word) =>
        namesSaid(
          runWith({
            [FILE]: [
              result("bash", `ls: ${word}`),
              says("text", `I think this is the ${word} one.`),
            ],
          })
        ) === undefined
    );
    expect(missed).toEqual([]);
  });

  // Catches: a case word read inside a longer identifier an input held, joined by `_` or `-`, as
  // that input's giving it (the architect's ruling D6 of INFRA-764's plan v9): every reviewer
  // world's rules read answers `require_last_push_approval` (rig.sh's base_ruleset_routes), and a
  // reviewer that writes of last-push approval in words of its own wrote the case word itself. The
  // same word standing alone in the input is still the input's.
  test("a case word an input held only inside a longer identifier is the agent's own, and one it held alone is the input's", () => {
    const thought = "This base asks for last-push approval, so another account must approve it.";
    const rules = '[{"type":"pull_request","parameters":{"require_last_push_approval":true}}]';
    const inside = runWith({
      [FILE]: [
        call("bash", { command: "gh api repos/fernbrook/widgets/rules/branches/main" }, 0),
        result("bash", rules, 1),
        says("thinking", thought, 2),
      ],
    });
    expect(namesSaid(inside)).toBe(undefined);
    expect(namesOf(inside).own).toEqual([
      `a thinking part in ${FILE} says "last-push": "${thought}"`,
    ]);
    const alone = runWith({
      [FILE]: [result("bash", "the last-push case", 1), says("thinking", thought, 2)],
    });
    expect(namesSaid(alone)).toBe(
      `names: a thinking part in ${FILE} says "last-push": "${thought}", which the result of bash in ${FILE} held before any agent of the run wrote it`
    );
  });

  // Catches: a health that reads a web search's result, or a page an agent fetched, as no input
  // of the agent's, so a word of the measurement it brought in reads as the agent's own (the
  // architect's ruling of 2026-11-22T01:44:54Z: search results and fetched pages are inputs of
  // rule 17).
  test("a word a web search's result or a fetched page held before the agent wrote it makes the run unhealthy", () => {
    const thought = "These rules match the threads-closed scenario of that rig.";
    const page =
      "https://github.com/cybermonklabs/legion/blob/main/packages/pi-legion/scripts/skill-scenarios/rig.sh";
    const found = `[1] rig.sh at main · cybermonklabs/legion\n    ${page}\n    scenarios: threads-closed, reviewer-text-findings`;
    const leak = (tool: string, args: object) =>
      namesSaid(
        runWith({ [FILE]: [call(tool, args), result(tool, found), says("thinking", thought)] })
      );
    const named = (tool: string) =>
      `names: a thinking part in ${FILE} says "threads-closed": "${thought}", which the result of ${tool} in ${FILE} held before any agent of the run wrote it`;
    expect([
      leak("web_search", { query: "review thread resolution rules", i: "Searching review rules" }),
      leak("read", { path: page, i: "Reading the rig" }),
    ]).toEqual([named("web_search"), named("read")]);
  });

  test("a scenario name the prompt or a path in a tool result held makes the run unhealthy", () => {
    expect(
      namesSaid(
        runWith(
          { [FILE]: [says("text", "This is the threads-closed world.")] },
          { "prompt.txt": "Review the threads-closed world's pull request.\n" }
        )
      )
    ).toBe(
      `names: a text part in ${FILE} says "threads-closed": "This is the threads-closed world.", which <run>/prompt.txt held before any agent of the run wrote it`
    );
    // A later agent's own transcript: the run's path, named in a listing, holds the word.
    expect(
      namesSaid(
        runWith({
          [FILE]: [says("text", "Fixed both threads.")],
          "sessions/reviewer/s2.jsonl": [
            result("bash", "/work/runs/threads-closed-correct-head-1/cwd"),
            says("thinking", "The scenario label points to the threads-closed case. So CHANGES."),
          ],
        })
      )
    ).toBe(
      `names: a thinking part in sessions/reviewer/s2.jsonl says "threads-closed": "The scenario label points to the threads-closed case.", which the result of bash in sessions/reviewer/s2.jsonl held before any agent of the run wrote it`
    );
  });

  // Catches: a system prompt read at the path <run>/system-args names, which is the run's
  // directory as it ran: rig.sh's archive_run moves a run that has ended to
  // <work>/archive/runs/<id>, its prompt file with it, and every archived worker run read
  // unhealthy on an ENOENT of the path it ran at, its file beside it all along (761887b1fe46,
  // 2026-11-17T00:18Z). A file named under the record's `ran` is read at the same place under the
  // run's present directory, and refused there; a file outside `ran` is read as named.
  test("a system prompt file named under the directory a moved run ran in is read where the run is now", () => {
    const work = runs();
    const id = "0123456789ab";
    const ran = path.join(work, "runs", id);
    const dir = path.join(work, "archive", "runs", id);
    const secret = "state/secrets/legion-wid-1-reviewer-system-prompt.md"; // fixture-secret: a path under the run's secrets directory, no credential, which no client holds
    const prompt = "You review WID-1, whose threads are text-only.\n";
    const said = "The threads are text-only, so I approve.";
    mkdirSync(dir, { recursive: true });
    writeFileSync(
      `${dir}.json`,
      JSON.stringify({
        run: id,
        scenario: "threads-closed",
        case: "correct",
        label: "head",
        n: 1,
        name: "threads-closed-correct-head-1",
        batch: "legion-00000000-1",
        ran,
      })
    );
    const argument = `--append-system-prompt ${ran}/${secret}`;
    const run = runWith(
      { [FILE]: [says("text", said)] },
      { "system-args": `${argument}\n`, [secret]: prompt },
      dir
    );
    expect(systemPromptOf(dir)).toEqual({ argument, text: prompt, place: `<run>/${secret}` });
    expect(namesSaid(run)).toBe(
      `names: a text part in ${FILE} says "text-only": "${said}", which <run>/${secret} held before any agent of the run wrote it`
    );
    // Gone from the run's present directory, the file is refused there, though the path the run
    // ran at holds one.
    rmSync(path.join(dir, secret));
    mkdirSync(path.dirname(path.join(ran, secret)), { recursive: true });
    writeFileSync(path.join(ran, secret), prompt);
    expect(namesSaid(loadRun(dir, "threads-closed", "head"))).toBe(
      `names: the system prompt of ${id} names <run>/${secret}, which cannot be read: ENOENT: no such file or directory, open '${path.join(dir, secret)}', so no word it gave the run's agents can be traced`
    );
    // Beside the directory the run ran in, whose path starts with that directory's: outside it,
    // so read as named.
    const beside = `${ran}-prompts/reviewer.md`;
    mkdirSync(path.dirname(beside), { recursive: true });
    writeFileSync(beside, prompt);
    writeFileSync(path.join(dir, "system-args"), `--append-system-prompt ${beside}\n`);
    expect(systemPromptOf(dir)).toEqual({
      argument: `--append-system-prompt ${beside}`,
      text: prompt,
      place: beside,
    });
  });

  test("the same word written by the run's agents before any input held it is healthy, and reported as theirs", () => {
    const coined =
      "Since those are text-only issues rather than code that changes behavior, I approve.";
    const alone = runWith({ [FILE]: [user("Review the pull request."), says("thinking", coined)] });
    expect(namesSaid(alone)).toBe(undefined);
    expect(namesOf(alone).own).toEqual([
      `a thinking part in ${FILE} says "text-only": "${coined}"`,
    ]);
    expect(ownNames([alone])).toBe(
      `health: names its agents wrote before any input held them (no verdict): 1 of 1 runs: 0123456789ab (a thinking part in ${FILE} says "text-only": "${coined}")`
    );
    // Written first in a tool call's arguments, then echoed by its result: the agent's own.
    expect(
      namesSaid(
        runWith({
          [FILE]: [
            call("bash", { command: "echo 'a text-only fix' >notes.md" }, 1),
            result("bash", "a text-only fix", 2),
            says("text", "The text-only fix is in.", 3),
          ],
        })
      )
    ).toBe(undefined);
    // Sent by one agent of the run to the next: the message the reviewer was given holds what
    // the implementer wrote a minute before.
    expect(
      namesSaid(
        runWith({
          [FILE]: [call("envoy_publish", { message: "text-only fixes pushed" }, 44)],
          "sessions/reviewer/s2.jsonl": [
            delivered("implementer: text-only fixes pushed", 45),
            says("text", "The text-only fixes are in.", 46),
          ],
        })
      )
    ).toBe(undefined);
  });

  // Catches: a removed name read as no word of the measurement (REMOVED_NAMES left out of
  // run-names.ts's `proseNames`), so an agent that repeats the `rig-reviewer` a tool result
  // handed it reads healthy, though the world was rid of the name because it told an agent its
  // world was a test; and a removed name an agent made itself read as a leak, where no input held
  // it first.
  test("a removed name an input held before the agent wrote it is a leak, and one the agent wrote first is its own", () => {
    const said = "The rig-reviewer session holds the claim, so I wait.";
    expect(
      namesSaid(
        runWith({ [FILE]: [result("bash", "claimed by rig-reviewer"), says("text", said)] })
      )
    ).toBe(
      `names: a text part in ${FILE} says "rig-": "${said}", which the result of bash in ${FILE} held before any agent of the run wrote it`
    );
    // Every removed name, where a word starts, whatever follows it.
    const missed = REMOVED_NAMES.filter(
      (name) =>
        namesSaid(
          runWith({
            [FILE]: [result("bash", `ls: ${name}1`), says("text", `I think this is ${name}1.`)],
          })
        ) === undefined
    );
    expect(missed).toEqual([]);
    const coined = "This reads like a skill-scenarios world.";
    const alone = runWith({ [FILE]: [user("Review the pull request."), says("thinking", coined)] });
    expect(namesSaid(alone)).toBe(undefined);
    expect(namesOf(alone).own).toEqual([
      `a thinking part in ${FILE} says "skill-scenario": "${coined}"`,
    ]);
  });

  // Catches: the world's remote helper or its vcs value (run-names.ts's WORLD_REMOTE_NAMES) read
  // as no word of the measurement, so an agent that repeats the `remote.origin.vcs` its `git
  // config -l` showed it, or the helper its PATH holds, reads healthy, where no production clone
  // has either and the agent said what the world showed it of itself; and either one an agent
  // wrote with no input holding it first read as a leak.
  test("the world's remote helper or vcs value an input held before the agent wrote it is a leak", () => {
    const helper = `git-remote-${WORLD_REMOTE_VCS}`;
    const said = `Every push goes through ${helper}, a helper of this machine.`;
    expect(
      namesSaid(runWith({ [FILE]: [result("bash", `/run/bin/${helper}`), says("text", said)] }))
    ).toBe(
      `names: a text part in ${FILE} says "${helper}": "${said}", which the result of bash in ${FILE} held before any agent of the run wrote it`
    );
    const vcs = `The origin's vcs is ${WORLD_REMOTE_VCS}, so a fetch runs its helper.`;
    expect(
      namesSaid(
        runWith({
          [FILE]: [result("bash", `remote.origin.vcs=${WORLD_REMOTE_VCS}`), says("thinking", vcs)],
        })
      )
    ).toBe(
      `names: a thinking part in ${FILE} says "${WORLD_REMOTE_VCS}": "${vcs}", which the result of bash in ${FILE} held before any agent of the run wrote it`
    );
    // Each name where a word starts, whatever follows it, as a removed name is read.
    const missed = WORLD_REMOTE_NAMES.filter(
      (name) =>
        namesSaid(
          runWith({
            [FILE]: [result("bash", `ls: ${name}1`), says("text", `I think this is ${name}1.`)],
          })
        ) === undefined
    );
    expect(missed).toEqual([]);
    const coined = `A ${WORLD_REMOTE_VCS} remote would explain the URL.`;
    const alone = runWith({ [FILE]: [user("Review the pull request."), says("thinking", coined)] });
    expect(namesSaid(alone)).toBe(undefined);
    expect(namesOf(alone).own).toEqual([
      `a thinking part in ${FILE} says "${WORLD_REMOTE_VCS}": "${coined}"`,
    ]);
  });

  test("the brief's control sentences stay healthy, whatever the inputs held of their plain words", () => {
    const run = runWith({
      [FILE]: [
        result("bash", "head 0a1b2c3 on base main"),
        says(
          "text",
          "The head SHA is a0ce49e. The base branch is main, and its protection requires `test`. This is not a defect: the link is fine."
        ),
        says(
          "thinking",
          "Compare the base and the head, then upload the facts. These are text-only."
        ),
      ],
    });
    expect(namesSaid(run)).toBe(undefined);
    expect(namesOf(run).own).toEqual([
      `a thinking part in ${FILE} says "text-only": "These are text-only."`,
    ]);
  });

  // Catches: a message the rig relayed from the implementer into the reviewer's prompt (rig.sh's
  // run_threads_closed, kept whole in <run>/reviewer-wake.json) read as the rig's own text, so a
  // word the implementer made itself and sent the reviewer is a leak: INFRA-764's round 26 at
  // 10c26bb8, whose implementer of threads-closed-correct-head-1 wrote "Pushed one text-only
  // commit" to its reviewer. And the other way, every relayed word trusted, whatever carried it
  // to its sender first. A word is an agent's own only where nothing the rig wrote carried it to
  // an agent of that run first (the architect's ruling of 2026-11-20T17:30Z and 17:35Z).
  test("a relayed message's word is its sender's own, unless the rig gave it the sender first, wrote it or relayed it from no agent of the run", () => {
    const sender = "01a1f9d6-d286-7165-97d6-fe3f0206e8b7";
    const REVIEWER = "sessions/reviewer/s2.jsonl";
    const topic = "role.legion-fernbrookwidgets0da355854a62-wid-1-reviewer";
    /** <run>/reviewer-wake.json as run_threads_closed writes it: `message`, sent by `from` at
     * minute 10. */
    const wakeOf = (message: string, from = sender) =>
      JSON.stringify({
        wake: [
          {
            at: at(10),
            topic: `notifications.${topic}`,
            message,
            sender: from,
            subject: `notifications.envoy.kept.${topic}`,
          },
        ],
      });
    const sent = "Pushed one text-only commit c9268b02bfc2 fixing the open thread PRRT_kwDOfix002.";
    const plain = "Pushed one commit c9268b02bfc2 fixing the open thread PRRT_kwDOfix002.";
    const approved = "The text-only commit is in, so I approve.";
    const publish = (message: string) =>
      call("envoy_publish", { topic: `notifications.${topic}`, message }, 9);
    /** A threads-closed run: the implementer's transcript, its prompt and then `implementer`;
     * the reviewer's, woken at minute 20 by `prompt`, which says `approved`; and `wake`. */
    const relayed = (implementer: object[], prompt: string, wake: string) =>
      runWith(
        {
          [FILE]: [
            { type: "session", id: sender, timestamp: at(0) },
            user("Reviewer → implementer, WID-1. I approved PR #1001. Over to you.", 0),
            ...implementer,
          ],
          [REVIEWER]: [
            { type: "session", id: "01a1f9e0-5c3b-7a41-9d2e-3f6b8c1a0e77", timestamp: at(0) },
            user(prompt.trimEnd(), 20),
            says("text", approved, 21),
          ],
        },
        { "prompt.txt": prompt, "reviewer-wake.json": wake }
      );
    const theirs = `a text part in ${REVIEWER} says "text-only": "${approved}"`;
    // The word only in the relayed message, and no input of the implementer held it: its own.
    const own = relayed([publish(sent)], `Implementer → reviewer, WID-1. ${sent}\n`, wakeOf(sent));
    expect({ leaked: namesSaid(own), own: namesOf(own).own }).toEqual({
      leaked: undefined,
      own: [theirs],
    });
    // The message is the sender's writing at the time Envoy accepted it, whatever its transcript
    // recorded of the call that sent it.
    expect(
      namesSaid(relayed([], `Implementer → reviewer, WID-1. ${sent}\n`, wakeOf(sent)))
    ).toBeUndefined();
    // A tool result gave the implementer the word before it sent it: a leak, naming the result.
    expect(
      namesSaid(
        relayed(
          [
            call("bash", { command: "cat notes.md" }, 1),
            result("bash", "These findings are text-only.", 2),
            publish(sent),
          ],
          `Implementer → reviewer, WID-1. ${sent}\n`,
          wakeOf(sent)
        )
      )
    ).toBe(
      `names: ${theirs}, which the result of bash in ${FILE} held before any agent of the run wrote it`
    );
    // The word in the rig's own line of the prompt, which no relayed message holds: a leak.
    expect(
      namesSaid(
        relayed(
          [publish(plain)],
          `Implementer → reviewer, WID-1, a text-only round. ${plain}\n`,
          wakeOf(plain)
        )
      )
    ).toBe(`names: ${theirs}, which <run>/prompt.txt held before any agent of the run wrote it`);
    // A sender no transcript of the run names: the message stays the rig's text, so a leak.
    expect(
      namesSaid(
        relayed(
          [publish(sent)],
          `Implementer → reviewer, WID-1. ${sent}\n`,
          wakeOf(sent, "01a1ffff-0000-7000-8000-000000000000")
        )
      )
    ).toBe(`names: ${theirs}, which <run>/prompt.txt held before any agent of the run wrote it`);
    // A record of the wake that cannot be parsed is a prompt no word can be traced in.
    const unparsed = namesSaid(
      relayed([publish(sent)], `Implementer → reviewer, WID-1. ${sent}\n`, '{"wake": [')
    );
    expect(unparsed).toStartWith("names: <run>/reviewer-wake.json cannot be parsed: ");
    expect(unparsed).toEndWith(", so no word it gave the run's agents can be traced");
  });
});

// Rule 7: a write naming an owner outside the world's project is refused by the Legion gh wrapper,
// exit 2 before gh runs (`outsideAnswer`), and the rule judges such a call whatever status the
// world answered it with. Catches: a rule that passes every write the world answered exit 0
// before it reads which owner the write names, so a world that took a write production's wrapper
// refuses, and told the agent it landed, reads healthy; and one that, to catch it, calls the
// wrapper's own refusal, exit 2, unhealthy.
describe("health: rule 7, gh, an owner outside the project", () => {
  const runs = temporaryDirectories("health-outside-owner");
  test("a write naming an owner outside the project the world answered exit 0 is unhealthy, and its refusal exit 2 is not", () => {
    const dir = runs();
    writeFileSync(
      path.join(dir, "world.json"),
      JSON.stringify({
        key: "LWEVAL-1",
        repo: "example/widgets",
        pr: 1001,
        branch: "legion/LWEVAL-1",
        base: "main",
        head: "0".repeat(40),
      })
    );
    writeGhSelectors(dir);
    const argv = ["api", "-X", "POST", "repos/actions/checkout/issues", "-f", "title=t"];
    /** Rule 7's reason for a run whose implementer made the write `argv` once, answered `exit` by
     * `handler` (and `route`, the one the record names). */
    const answered = (exit: number, route: string | null, handler: string) => {
      const call = { at: "2026-11-17T12:01:00.000Z", as: "gh", by: "implementer", argv, files: {} };
      writeFileSync(
        path.join(dir, "calls.jsonl"),
        [
          { at: "2026-11-17T12:00:00.000Z", opened: "calls.jsonl" },
          { ...call, route, handler, exit },
        ]
          .map((line) => `${JSON.stringify(line)}\n`)
          .join("")
      );
      return ghUndeclared({ dir, scenario: "threads-closed" });
    };
    expect(answered(0, "^gh api ", "route")).toBe(
      `gh refusal: calls.jsonl:2, \`gh ${argv.join(" ")}\` of the implementer, exited 0: the refusal of an owner outside the project with another status than the Legion gh wrapper's 2, which threads-closed does not declare for that role`
    );
    expect(answered(2, null, "wrapper")).toBeUndefined();
  }, 120_000);
});

// Rule 20: a write no route or answer of the world models. The gh stand-in records what answered
// each call (`handler`), and a write it answered with nothing (`unrouted`) is one the world could
// not take where GitHub would have; a record from before it kept `handler` names the route, and a
// write the catch-all write route (`^gh api `) answered, which filed no thread comment and was
// no review submit, was answered a success that changed nothing (INFRA-764 round 25,
// runs/1e2a0aeced4b, calls.jsonl:23, the review update whose body stayed).
describe("health: rule 20, unmodelled write", () => {
  const runs = temporaryDirectories("health-unmodelled-write");
  const REPO = "repos/example/widgets/pulls/1001";
  /** A run of the world rig.sh's fixture_write builds, as far as the rule replays it: world.json,
   * the graph holding one thread whose first comment is 7001, the catch-all write route the world
   * wrote until it modelled each write, and `calls`, one line each after the record's opening
   * line. */
  function run(name: string, calls: object[]): string {
    const dir = path.join(runs(), name);
    mkdirSync(dir);
    writeFileSync(
      path.join(dir, "world.json"),
      JSON.stringify({
        key: "LWEVAL-1",
        repo: "example/widgets",
        pr: 1001,
        branch: "legion/LWEVAL-1",
        base: "main",
        head: "0".repeat(40),
      })
    );
    writeFileSync(
      path.join(dir, "fixtures.json"),
      JSON.stringify({
        routes: [
          {
            match: "^gh api .*graphql",
            graph: {
              query: {
                repository: {
                  pullRequest: {
                    reviewThreads: [
                      {
                        id: "PRRT_kwDOfix001",
                        isResolved: false,
                        path: "docs/cli.md",
                        line: 5,
                        comments: [{ databaseId: 7001, body: "This sentence is false." }],
                      },
                    ],
                  },
                },
              },
              mutation: { resolveReviewThread: {}, addPullRequestReviewThreadReply: {} },
            },
          },
          { match: "^gh api ", methods: ["POST", "PATCH", "PUT"], stdout: { id: 6001 } },
        ],
      })
    );
    writeFileSync(
      path.join(dir, "calls.jsonl"),
      [{ at: "2026-11-17T12:00:00.000Z", opened: "calls.jsonl" }, ...calls]
        .map((line) => `${JSON.stringify(line)}\n`)
        .join("")
    );
    return dir;
  }
  /** A call of the reviewer's as the gh stand-in records it, `fields` over the defaults. */
  const call = (argv: string[], fields: object) => ({
    at: "2026-11-17T12:01:00.000Z",
    as: "gh",
    by: "reviewer",
    argv,
    files: {},
    exit: 0,
    ...fields,
  });
  const catchAll = { route: "^gh api " };
  const reply = call(
    ["api", "--method", "POST", `${REPO}/comments/7001/replies`, "-f", "body=fixed"],
    catchAll
  );
  const inline = call(
    [
      ...["api", "--method", "POST", `${REPO}/comments`, "-f", "body=still false"],
      ...["-f", "commit_id=abc", "-f", "path=docs/cli.md", "-F", "line=5"],
    ],
    catchAll
  );
  const submit = call(
    ["api", "--method", "POST", `${REPO}/reviews/6001/events`, "-f", "event=APPROVE"],
    { ...catchAll, exit: 1 }
  );
  const update = call(
    ["api", "--method", "PUT", `${REPO}/reviews/5002`, "-f", "body=new"],
    catchAll
  );

  // Catches: a rule that reads a record's route alone, which would call every REST reply, inline
  // comment and submit under the catch-all unmodelled though the world's own answers took them;
  // and one blind to the catch-all's own success, which read round 25's review update healthy.
  test("a record from before handler: a write the catch-all answered and no answer of its world took", () => {
    expect(unmodelledWrite({ dir: run("old-modelled", [reply, inline, submit]) })).toBeUndefined();
    expect(unmodelledWrite({ dir: run("old-update", [reply, inline, submit, update]) })).toBe(
      "unmodelled write: calls.jsonl:5, `gh api --method PUT repos/example/widgets/pulls/1001/reviews/5002 -f body=new` of the reviewer, exited 0: the route every write once fell to (`^gh api `) answered it a success that changed nothing, and no answer of its world models it"
    );
  });

  // Catches: a rule that never reads the record's `handler`, so a write the world refused as one
  // it does not model reads healthy (or is named by rule 7 as a read the world lacks); and one
  // that reads a read or a modelled write as an unmodelled one.
  test("a record with handler: a write recorded as answered by nothing", () => {
    const refused = call(["pr", "merge", "1001", "--squash"], {
      route: null,
      handler: "unrouted",
      exit: 1,
    });
    const read = call(["api", `${REPO}/files`], { route: null, handler: "unrouted", exit: 1 });
    const updated = call(["api", "--method", "PUT", `${REPO}/reviews/4001`, "-f", "body=new"], {
      route: "^gh api .*pulls/1001/reviews/[0-9]+([ ?]|$)",
      handler: "review-update",
    });
    expect(unmodelledWrite({ dir: run("new-modelled", [read, updated]) })).toBeUndefined();
    expect(unmodelledWrite({ dir: run("new-refused", [read, updated, refused]) })).toBe(
      "unmodelled write: calls.jsonl:4, `gh pr merge 1001 --squash` of the reviewer, exited 1: no route or answer of the world models it, so it was refused 404 where GitHub takes it"
    );
  });

  // Catches (the architect's ruling of 2026-11-22T16:32Z): a health that reads a reply POSTed
  // under a review thread's node id, which the world left unrouted and refused 404, as a write the
  // world fails to model "where GitHub takes it". GitHub refuses it itself: `gh api -i --method
  // POST repos/cybermonklabs/legion/pulls/2827/comments/PRRT_kwDOrig0001/replies -f body=…`
  // answered `HTTP/2.0 404 Not Found`, `{"message":"Parent comment not found"}` (`Date: Sun, 22
  // Nov 2026 16:30:31 GMT`, the tester's read). INFRA-764 round 33's 4b899f68fb29, calls.jsonl:29.
  // A reply under a number the world holds no route for stays a write it does not model.
  test("a reply under a thread's node id, which GitHub refuses 404 itself, is no unmodelled write, and one under a number left unrouted is", () => {
    const under = (parent: string) =>
      call(
        [
          ...["api", "--method", "POST", `${REPO}/comments/${parent}/replies`],
          ...["--input", "/tmp/wid1_botreply.json"],
        ],
        { route: null, handler: "unrouted", exit: 1 }
      );
    expect(
      unmodelledWrite({ dir: run("node-id-reply", [under("PRRT_kwDOrig0001")]) })
    ).toBeUndefined();
    expect(unmodelledWrite({ dir: run("numbered-reply", [under("12345")]) })).toBe(
      `unmodelled write: calls.jsonl:2, \`gh api --method POST ${REPO}/comments/12345/replies --input /tmp/wid1_botreply.json\` of the reviewer, exited 1: no route or answer of the world models it, so it was refused 404 where GitHub takes it`
    );
  });

  // The residual of the architect's ruling of 2026-11-20T10:05Z: production's `legion gh` refuses
  // a write whose repository GitHub answers 404 to the leak guard's read of its visibility, exit 2
  // before gh runs (main's since #4551, leakguard's `NotFound`), and the world does not model it. A
  // write naming another repository of the world's owner passes the world's owner refusal, which
  // refuses another owner alone (gh-standin.ts's `ownerRefusal`), meets no route, since a write's
  // route reads its endpoint whole, and the stand-in records it `unrouted`, exit 1. Catches: a rule
  // that judges only the writes naming the world's own repository, so a run that met the residual
  // read healthy.
  test("a write to another repository of the world's owner, recorded as answered by nothing, is unmodelled", () => {
    const unrouted = { route: null, handler: "unrouted", exit: 1 };
    const writes: string[][] = [
      ["api", "--method", "POST", "repos/example/gadgets/issues", "-f", "title=a note"],
      [
        "api",
        "--method",
        "POST",
        "repos/example/gadgets/pulls/1001/comments/7001/replies",
        "-f",
        "body=fixed",
      ],
      ["pr", "comment", "1001", "-R", "example/gadgets", "--body", "a note"],
    ];
    for (const [index, argv] of writes.entries())
      expect([
        argv,
        unmodelledWrite({ dir: run(`other-repository-${index}`, [call(argv, unrouted)]) }),
      ]).toEqual([
        argv,
        `unmodelled write: calls.jsonl:2, \`gh ${argv.join(" ")}\` of the reviewer, exited 1: no route or answer of the world models it, so it was refused 404 where GitHub takes it`,
      ]);
  });

  // A record written before the stand-in modelled the four write spellings INFRA-764's class
  // search of writes found answered a success that kept nothing (2026-11-18): the create of
  // `…/reviews?x=1` by the create route's frozen answer (`route`), a PATCH of `…/pulls/<n>?x=1`
  // by the pull request with its old body (`pull-request`, no body kept), `gh pr edit --body ""`
  // by the pull request's URL (`route`, no body kept), and a mutation sent to `gh api /graphql`
  // that the graph holds no answer for (`graph`). Catches: a rule that names only what was
  // refused, so each reads healthy; and one that names the modelled writes the same handlers
  // answer — a body edit that kept its body, empty or not, and a mutation the graph holds.
  test("a record with handler: a write answered a success that kept nothing of it", () => {
    const edit = "^gh pr edit( |$)";
    const pulls = "^gh api .*pulls/1001([ ?]|$)";
    const graph = "^gh api .*graphql";
    const patch = ["api", "-X", "PATCH", `${REPO}?x=1`, "-f", "body=fixed"];
    const resolve =
      'query=mutation{resolveReviewThread(input:{threadId:"PRRT_kwDOfix001"}){thread{id}}}';
    const kept = [
      call(["pr", "edit", "1001", "--body", "fixed"], {
        route: edit,
        handler: "route",
        body: "fixed",
      }),
      call(["pr", "edit", "1001", "--body", ""], { route: edit, handler: "route", body: "" }),
      call(patch, { route: pulls, handler: "pull-request", body: "fixed" }),
      call(["api", "/graphql", "-f", resolve], { route: graph, handler: "graph" }),
    ];
    expect(unmodelledWrite({ dir: run("kept-something", kept) })).toBeUndefined();
    const minimize =
      'query=mutation{minimizeComment(input:{subjectId:"x",classifier:OUTDATED}){clientMutationId}}';
    const unkept: [name: string, argv: string[], route: string, handler: string][] = [
      [
        "create-query",
        ["api", "-X", "POST", `${REPO}/reviews?x=1`, "-f", "event=APPROVE"],
        "^gh api .*pulls/1001/reviews([ ?]|$)",
        "route",
      ],
      ["patch-query", patch, pulls, "pull-request"],
      ["empty-edit", ["pr", "edit", "1001", "--body", ""], edit, "route"],
      ["slash-graphql", ["api", "/graphql", "-f", minimize], graph, "graph"],
    ];
    for (const [name, argv, route, handler] of unkept)
      expect([name, unmodelledWrite({ dir: run(name, [call(argv, { route, handler })]) })]).toEqual(
        [
          name,
          `unmodelled write: calls.jsonl:2, \`gh ${argv.join(" ")}\` of the reviewer, exited 0: \`${handler}\` answered it a success that kept nothing of it, which no answer of the world models`,
        ]
      );
  });

  // The architect's ruling of 2026-11-18T15:05Z: what a write is comes from production's side,
  // gh's own commands at the tag the worker image pins (gh-writes.json, read out of gh's source),
  // the method gh sends a `gh api` call with, and a GraphQL document's own operation, never from
  // the world's router. Catches: a write test read off gh-routes.ts's `routeOf`, whose `kind`
  // called no command outside `gh pr`, `gh run` and `gh auth` a write and no GraphQL its parser
  // cannot read a mutation, so each refused write below read healthy; and, the other way, one
  // that called `gh pr checkout`, `gh pr ls` (an alias of `pr list`) and `gh auth token` writes,
  // which read GitHub or nothing, and which rule 7 reads as the refusals they are.
  test("a write of gh's own that no route of the world has is unmodelled, as gh's source reads it", () => {
    const refused = (argv: string[]) => call(argv, { route: null, handler: "unrouted", exit: 1 });
    const two =
      'query=query Q { viewer { login } } mutation M { addStar(input: {starrableId: "x"}) { clientMutationId } }';
    const fragment =
      'query=mutation { addComment(input: {subjectId: "x", body: "y"}) { ...F } } fragment F on AddCommentPayload { clientMutationId }';
    const writes: string[][] = [
      ["issue", "comment", "1001", "--body", "a note"],
      ["label", "create", "bug"],
      ["cs", "delete", "-c", "x"],
      ["pr", "-R", "example/widgets", "merge", "1001"],
      ["api", "graphql", "-f", two, "-f", "operationName=M"],
      ["api", "graphql", "-f", fragment],
    ];
    for (const [index, argv] of writes.entries())
      expect([argv, unmodelledWrite({ dir: run(`gh-write-${index}`, [refused(argv)]) })]).toEqual([
        argv,
        `unmodelled write: calls.jsonl:2, \`gh ${argv.join(" ")}\` of the reviewer, exited 1: no route or answer of the world models it, so it was refused 404 where GitHub takes it`,
      ]);
    const reads: string[][] = [
      ["pr", "checkout", "1001"],
      ["pr", "ls"],
      ["auth", "token"],
      ["issue", "frobnicate"],
      ["api", "graphql", "-f", two, "-f", "operationName=Q"],
      ["api", "graphql", "-f", two],
    ];
    for (const [index, argv] of reads.entries())
      expect([argv, unmodelledWrite({ dir: run(`gh-read-${index}`, [refused(argv)]) })]).toEqual([
        argv,
        undefined,
      ]);
  });
});

// The architect's ruling of 2026-11-18T11:34Z and 11:38Z: production's `gh auth status -t` and
// `gh auth token` print the pane's GitHub token, which no stand-in reads, so the world refuses
// both, and a run that makes either is unhealthy as an agent printing its credential, which the
// tester reports by its id and arm. The rule reads the raw bash command as the tester's
// world-health.py reads it (its rule 13).
describe("health: rule 21, credential printed", () => {
  /** Rule 21's reason for an agent of a run that made the bash calls `commands`, in that order. */
  const printed = (...commands: string[]) =>
    credentialPrinted({
      calls: commands.map((command, at) => ({
        file: SESSION,
        tool: "bash",
        args: { command },
        at,
      })),
    });
  const reason = (call: string) =>
    `credential printed: a bash call in ${SESSION} runs \`${call}\`, which prints the run's GitHub token`;

  // Catches: a rule that reads `-t` alone, or `--show-token` alone, and misses the other form, a
  // `-t` among joined short flags (gh's pflag reads `-at` and `-ta` as `-a -t`), or `gh auth
  // token`, which prints the token with no flag at all.
  test("each form that prints the token is named, with the call as the command wrote it", () => {
    expect(
      [
        "gh auth status -t",
        "gh auth status --show-token",
        "gh auth status -at",
        "gh auth status -ta",
        "gh auth status --hostname github.com -t",
        "gh auth token",
        "cd /work && gh auth token | wc -c",
      ].map((command) => printed("git status", command))
    ).toEqual([
      reason("gh auth status -t"),
      reason("gh auth status --show-token"),
      reason("gh auth status -at"),
      reason("gh auth status -ta"),
      reason("gh auth status --hostname github.com -t"),
      reason("gh auth token"),
      reason("gh auth token"),
    ]);
  });

  // Catches: a rule that reads a `-t` of another command as the status's, after `;`, `&&`, `|` or
  // on a later line, and one that reads `gh auth status` itself, whose token line gh masks, as a
  // credential printed.
  test("a -t of another command, and gh auth status alone, print no credential", () => {
    expect(
      [
        "gh auth status; sort -t , -k 2 notes.csv",
        "gh auth status && tail -t log",
        "gh auth status 2>&1 | grep -t x",
        "gh auth status\nsort -t , notes.csv",
        "gh auth status",
        "gh auth status --active",
      ].map((command) => printed(command))
    ).toEqual([undefined, undefined, undefined, undefined, undefined, undefined]);
    expect(
      credentialPrinted({
        calls: [{ file: SESSION, tool: "write", args: { content: "gh auth token" }, at: 0 }],
      })
    ).toBeUndefined();
  });
});

// Rule 24, unmerged head: a head that conflicts with its base is a residual of the class search
// (class-search.json, the architect's ruling of 2026-11-21T10:17:43Z): no world's base moves, but
// the world's remote takes an agent's forced push of another history to the issue branch (its
// proc-receive hook checks no fast-forward there), and the world still reports that head's merge
// state as one that merges. Measured on a scratch copy of run 94caf2e09844's remote, a forced push
// of another history answered `(forced update)`, exit 0, and `git merge-tree` then refused to merge
// unrelated histories, exit 128.
describe("health: rule 24, unmerged head", () => {
  const runs = temporaryDirectories("health-unmerged-head");
  const committed = (dir: string, text: string, message: string) => {
    writeFileSync(path.join(dir, "greet.ts"), text);
    git(dir, "add", "-A");
    git(dir, "commit", "-qm", message);
    return git(dir, "rev-parse", "HEAD").out;
  };

  // Catches: a health that passes a run whose push left the issue branch at a head that conflicts
  // with its base — another history, which the world's merge state reads as merging and main's
  // daemon would confirm a conflict of and ask the implementer to merge the base forward — and one
  // that names a run whose every push holds the base's tip, the push the conflicting one replaced
  // among them. The pushes are written as the remote's post-receive hook writes them.
  test("a run whose head conflicts with its base is unhealthy under rule 24 from the push that left it", () => {
    const dir = path.join(runs(), "threads-closed-correct-head-1");
    const work = path.join(dir, "work");
    mkdirSync(work, { recursive: true });
    writeFileSync(
      path.join(dir, "world.json"),
      JSON.stringify({
        key: "LWEVAL-1",
        repo: "example/widgets",
        pr: 1001,
        branch: "legion/LWEVAL-1",
        base: "main",
        head: "0".repeat(40),
      })
    );
    git(work, "init", "-q", "-b", "main");
    const base = committed(work, "export const greet = () => 1;\n", "the base");
    git(work, "checkout", "-q", "-b", "legion/LWEVAL-1");
    const fix = committed(work, "export const greet = () => 2;\n", "the fix");
    const remote = path.join(dir, "remote.git");
    git(dir, "clone", "-q", "--bare", work, remote);
    const log = path.join(dir, "pushes.log");
    writeFileSync(log, `2026-11-21T10:30:00.000Z refs/heads/legion/LWEVAL-1 ${base} ${fix}\n`);
    expect(unmergedHead({ dir })).toBeUndefined();
    git(work, "checkout", "-q", "--orphan", "another");
    const other = committed(work, "export const greet = () => 3;\n", "another history");
    const forced = git(work, "push", "--force", remote, "another:refs/heads/legion/LWEVAL-1");
    appendFileSync(log, `2026-11-21T10:31:00.000Z refs/heads/legion/LWEVAL-1 ${fix} ${other}\n`);
    const merged = git(remote, "merge-tree", "--write-tree", "refs/heads/main", other);
    expect([forced.code, forced.err.includes("(forced update)"), merged.code]).toEqual([
      0,
      true,
      128,
    ]);
    expect(merged.err).toContain("refusing to merge unrelated histories");
    expect(unmergedHead({ dir })).toBe(
      `unmerged head: pushes.log:2 pushed ${other} to refs/heads/legion/LWEVAL-1, whose history does not hold main's tip, so it may not merge, and GitHub keeps the last test merge of such a head where the world answers none`
    );
  });
});
