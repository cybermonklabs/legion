// lib/github-apps.sh run as the stage proofs run it: the input checks, the github-apps.yaml block
// they write for lib/app-logins.ts, check_github_apps over it, and the token files the sandbox
// proof's refresher mints under bridge, each driven under a stage script's shell options against
// the stand-in GitHub of stand-in-github-fixtures.ts.
import { beforeAll, describe, expect, test } from "bun:test";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { appCredentialSources } from "./app-logins";
import { expectNoMissingHelpers, nameMissingHelpers, scriptFunctions } from "./script-functions";
import {
  appPrivateKey,
  holdsNoKey,
  holdsNoToken,
  release,
  standInGitHub,
  tokenOf,
} from "./stand-in-github-fixtures";

const { url: standIn, seen, dir } = standInGitHub("github-apps-test");
// The home every child of this file runs under, so none of them reads the caller's.
const home = join(dir, "home");

// lib/github-apps.sh as a stage proof uses it: sourced under the stage scripts' shell options with
// their check helpers, the inputs in the environment, and `then` run after. The seams are bun,
// handed the stand-in GitHub after the helper's own arguments, and a stand-in `agent-secrets` first
// on PATH, which, as the real CLI does, puts the release the github-app secret it names gives
// (STORE/<name>) into the environment of the command after `--` under that name. It appends its
// argv to ARGV_LOG, fails with exit 3 when the `<owner>/<read-only>` it was asked for matches
// FAIL_MINT, refuses the names UNKNOWN_SECRETS lists as the broker refuses a name it does not
// serve (`UNKNOWN_SECRET`, exit 1, under the line UNKNOWN_PREFACE gives when it gives one, as the
// client's own progress lines put it), writes WAIT_NOTICE to its stderr when that is set, as the
// client writes the line a grant waiting on a person is approved by, releases nothing when
// EMPTY_MINT is set, and streams stdout when OVERSIZED_MINT is set.
const store = join(dir, "store");
const fakeBin = join(dir, "bin");
// A directory holding one stand-in `awk`, which the row that reads the relay's own silence puts
// in front of fakeBin on PATH and no other row sees: it writes a warning when it is given `-W`,
// as gawk and busybox awk do, and is otherwise the host's own awk. It is what makes that row
// able to fail here, where awk is a mawk that takes `-W interactive` silently: the lib's probe
// must refuse a flag whose run says anything at all, and a probe that accepted this one would
// put a warning in the channel the row reads.
const warningAwk = join(dir, "warning-awk");
// Resolved here, before that directory is first on PATH, so the stand-in runs the real awk
// rather than itself.
const hostAwk = Bun.which("awk") ?? "/usr/bin/awk";
// The review App's own private key, the one the stand-in GitHub verifies an App JWT against, read
// in the hook below rather than here: appPrivateKey generates that key at its first read, and a
// run that selects no row of this file must generate none. The rig drives the key as a deployment
// does — through a command that prints it, never through a variable or an argument holding it —
// and keyIDs is the App whose key holdsNoKey is told to look for wherever it must not be.
const keyIDs = [202];
let reviewKey = "";
const reviewKeyFile = join(dir, "review-key.pem");
// The key command the rig gives the review role, which has no broker secret to name: a `cat` of
// the file above, as an operator's own command is one. The lib never puts this text in a block:
// it runs the command once, here on the driver, and the block carries a `cat` of the 0600 file it
// wrote (write_github_apps --key-dir, or mint_app_tokens under --token-dir).
const reviewKeyCommand = `cat -- ${reviewKeyFile}`;
// Every directory and stand-in this file's children read, staged before the first row: `dir` is
// reserved at module scope and made by the hook that reservation registered, so a run that
// selects no row of this file stages nothing and leaves nothing behind.
beforeAll(() => {
  mkdirSync(home);
  mkdirSync(store);
  mkdirSync(fakeBin);
  writeFileSync(join(store, "IMPLEMENT_APP"), release({ sjawhar: tokenOf(101) }));
  writeFileSync(join(store, "OTHER_APP"), release({ sjawhar: tokenOf(404) }));
  // The review App's key, behind the key command below: the file an operator's own command would
  // read, written where every child of this file can read it.
  reviewKey = appPrivateKey(202);
  writeFileSync(reviewKeyFile, reviewKey, { mode: 0o600 });
  writeFileSync(
    join(fakeBin, "agent-secrets"),
    `#!/bin/sh
printf '%s\\n' "$*" >>"\${ARGV_LOG:-/dev/null}"
[ -z "\${WAIT_NOTICE:-}" ] || echo "$WAIT_NOTICE" >&2
name=$1
shift
owner=
ro=
while [ "$1" != -- ]; do
  case $1 in --github-owner) owner=$2; shift ;; --github-read-only) ro=1 ;; esac
  shift
done
shift
if [ -n "\${FAIL_MINT:-}" ]; then
  case "$owner/$ro" in $FAIL_MINT) echo "grant for $name denied" >&2; exit 3 ;; esac
fi
if [ -n "\${UNKNOWN_SECRETS:-}" ]; then
  case " $UNKNOWN_SECRETS " in
    *" $name "*)
      [ -z "\${UNKNOWN_PREFACE:-}" ] || echo "$UNKNOWN_PREFACE" >&2
      echo "agent-secrets: no agent secret has this name (UNKNOWN_SECRET); asked the broker AGENT_SECRETS_URL names" >&2
      exit 1 ;;
  esac
fi
if [ -n "\${EMPTY_MINT:-}" ]; then export "$name="; exec "$@"; fi
if [ -n "\${OVERSIZED_MINT:-}" ]; then exec yes x; fi
value=$(cat "\${STORE:-${store}}/$name") || exit 9
export "$name=$value"
exec "$@"
`,
    { mode: 0o755 }
  );
  mkdirSync(warningAwk);
  writeFileSync(
    join(warningAwk, "awk"),
    `#!/bin/sh
case " $* " in *" -W "*) echo "awk: warning: -W is not a supported option" >&2 ;; esac
exec ${hostAwk} "$@"
`,
    { mode: 0o755 }
  );
});
const appInputs = {
  LEGION_E2E_IMPLEMENT_APP_ID: "101",
  LEGION_E2E_IMPLEMENT_APP_SECRET: "IMPLEMENT_APP",
  LEGION_E2E_REVIEW_APP_ID: "202",
  LEGION_E2E_REVIEW_APP_KEY_COMMAND: reviewKeyCommand,
};
// The sh the bridge daemon's container runs a token command with is busybox's; where the host has
// none, the host's POSIX sh stands in for it.
const busyboxSh = Bun.which("busybox") === null ? ["sh"] : ["busybox", "sh"];

function spawnGithubApps(inputs: Record<string, string | undefined>, then: string) {
  const work = mkdtempSync(join(dir, "apps."));
  // The child gets PATH with the stand-in first, a home of the test's own, the two seams and the
  // case's own inputs, never the caller's whole environment. A devbox shell exports this
  // deployment's own credential variables, and one of those left in would be swept up by the
  // lib's own `compgen -e` strip list and hide whether the lib's default entries are swept: the
  // child starts with no App secret of any kind, and only a case's inputs put one back.
  const env: Record<string, string | undefined> = {
    PATH: `${fakeBin}:${process.env.PATH}`,
    HOME: home,
    LIB: join(import.meta.dir, "github-apps.sh"),
    work,
    API: standIn,
    ...inputs,
  };
  for (const name of Object.keys(env)) if (env[name] === undefined) delete env[name];
  const shell = `set -Eeuo pipefail
check=prerequisites
note() { printf '   %s\\n' "$*"; }
fail() { printf 'FAIL %s: %s\\n' "$check" "$*" >&2; exit 1; }
bun() { command bun "$@" "$API"; }
. "$LIB"
${then}`;
  return { child: Bun.spawn(["bash", "-c", shell], { env, stdout: "pipe", stderr: "pipe" }), work };
}

async function githubApps(inputs: Record<string, string | undefined>, then: string) {
  const { child, work } = spawnGithubApps(inputs, then);
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code, stdout: stdout.trimEnd(), stderr: stderr.trim(), work };
}
const line = (stdout: string, prefix: string) =>
  (stdout.split("\n").find((l) => l.startsWith(prefix)) ?? "").slice(prefix.length);
const tokenCommand = (name: string) =>
  `agent-secrets ${name} --ttl 12h --reason=legion-live-proof-github-app-token \${LEGION_GITHUB_OWNER:+--github-owner "$LEGION_GITHUB_OWNER"} \${LEGION_GITHUB_REPOS:+$(printf ' --github-repo %s' $LEGION_GITHUB_REPOS)} \${LEGION_GITHUB_READ_ONLY:+--github-read-only} -- sh -c 'printf %s "$${name}"'`;
// The review role's private_key_command in every block the lib writes: a `cat` of the key file it
// resolved the operator's command into, under the credential directory the caller named.
const keyFileCommand = (dir: string) => `cat -- '${join(dir, "review", "key.pem")}'`;
// The teardown of the App credential directory the sandbox proof runs, taken from that script by
// name rather than written again here: claude-backend-sandbox.sh's remove_app_tokens, which stops
// the refresher this lib starts and removes the directory the proof set app_tokens_dir to. A row
// that drives it puts `sandboxTeardown` in its shell after setting app_tokens_dir, so a change to
// that function reaches the rows below.
const sandboxScript = join(import.meta.dir, "..", "claude-backend-sandbox.sh");
// nameMissingHelpers names a helper remove_app_tokens calls that neither this harness's `. "$LIB"`
// nor the extraction defines, rather than leaving the run a bare exit 127.
const sandboxTeardown = `${nameMissingHelpers(sandboxScript)}
${scriptFunctions(sandboxScript)("remove_app_tokens")}`;

describe("lib/github-apps.sh", () => {
  const proveBoth = `require_app_inputs implement review
write_github_apps --key-dir "$work/app-key" implement review
check_github_apps implement review
echo "logins $github_app_logins"`;

  test("the default pair is the lib's documented one, and an input left unset takes it", async () => {
    seen.length = 0;
    const { code, stdout, stderr, work } = await githubApps(
      { LEGION_E2E_REVIEW_APP_ID: "202", LEGION_E2E_REVIEW_APP_KEY_COMMAND: reviewKeyCommand },
      `require_app_inputs implement review
write_github_apps --key-dir "$work/app-key" implement review
echo "defaults $default_implement_app_id $default_implement_app_secret $default_review_app_id"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    // The review App has no default credential of its own: its key command is required and
    // default_review_app_secret is gone. The three below are read outside this file — the
    // implement pair out of the lib's text by packages/daemon/internal/api/real_github_test.go,
    // all three by scripts/e2e/README.md — so each is pinned here.
    expect(line(stdout, "defaults ")).toBe("5341240 LEGION_IMPLEMENT_APP 5341243");
    const sources = appCredentialSources(readFileSync(join(work, "github-apps.yaml"), "utf8"));
    expect(sources.implement).toEqual({
      appId: "5341240",
      kind: "token_command",
      command: tokenCommand("LEGION_IMPLEMENT_APP"),
    });
    expect(sources.review).toEqual({
      appId: "202",
      kind: "private_key_command",
      command: keyFileCommand(join(work, "app-key")),
    });
    // The default pair is never asked of GitHub by this test: check_github_apps did not run.
    expect(seen).toEqual([]);
  });

  test("an input set to an empty value is refused, not read as a choice of the default", async () => {
    const { code, stderr } = await githubApps(
      { ...appInputs, LEGION_E2E_IMPLEMENT_APP_SECRET: "" },
      proveBoth
    );
    expect(code).toBe(1);
    expect(stderr).toContain("LEGION_E2E_IMPLEMENT_APP_SECRET must be");
  });

  test("write_github_apps refuses a role require_app_inputs did not read, and writes nothing", async () => {
    const { code, stdout, stderr, work } = await githubApps(
      appInputs,
      `require_app_inputs implement
write_github_apps implement review
echo "wrote the block"`
    );
    expect(code).toBe(1);
    expect(stdout).not.toContain("wrote the block");
    expect(stderr).toContain(
      "the review App's inputs are unread: require_app_inputs review has not run"
    );
    expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
  });

  test("app_key_unsets strips the implement secret, the lib's default name, the two retired review names, the review App's key command and every PRIVATE_KEY variable", async () => {
    // Each name is exported first, so the strip list has something to remove: without it the
    // child would print them all. The three exported after the call are the lib's own literal
    // entries: its `compgen -e` sweep never saw them, so only the literals can strip them, and a
    // stale export of either retired review name reaches no process the run starts. The review
    // role's own input is in the list for a reason of its own: a value that holds the key itself
    // and still runs is taken (a one-line `printf` of a PEM), and no child of the run reads that
    // input.
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, SOME_OTHER_APP_PRIVATE_KEY_B64: "irrelevant" },
      `export IMPLEMENT_APP=x
require_app_inputs implement review
export "$default_implement_app_secret=x" LEGION_REVIEW_APP=x GH_REVIEW_APP_PRIVATE_KEY_B64=x
names="IMPLEMENT_APP|SOME_OTHER_APP_PRIVATE_KEY_B64|$default_implement_app_secret|LEGION_REVIEW_APP|GH_REVIEW_APP_PRIVATE_KEY_B64|LEGION_E2E_REVIEW_APP_KEY_COMMAND"
echo "set $(env | grep -c -E "^($names)=")"
env "\${app_key_unsets[@]}" sh -c "echo \\"left \\$(env | grep -c -E '^($names)=' || true)\\""`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("set 6");
    expect(stdout).toContain("left 0");
  });

  test("an id that is not a number, a secret name unsafe in a shell, or one App id for both is refused", async () => {
    const cases: [Record<string, string>, string][] = [
      [{ LEGION_E2E_IMPLEMENT_APP_ID: "12a" }, "LEGION_E2E_IMPLEMENT_APP_ID must be"],
      [{ LEGION_E2E_IMPLEMENT_APP_SECRET: "APP;true" }, "LEGION_E2E_IMPLEMENT_APP_SECRET must be"],
      [{ LEGION_E2E_REVIEW_APP_ID: "101" }, "are both 101: the workflow needs two Apps"],
    ];
    for (const [inputs, want] of cases) {
      const { code, stderr, work } = await githubApps({ ...appInputs, ...inputs }, proveBoth);
      expect(code).toBe(1);
      expect(stderr).toContain(want);
      expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
    }
  });

  test("a malformed input is refused without printing its value, a token-shaped one by its length", async () => {
    const pasted = `ghs_${"A1b2C3d4".repeat(10)}`;
    for (const [name, value] of [
      ["LEGION_E2E_IMPLEMENT_APP_SECRET", pasted],
      ["LEGION_E2E_REVIEW_APP_ID", "1234567890123"],
    ]) {
      const { code, stderr } = await githubApps({ ...appInputs, [name]: value }, proveBoth);
      expect(code).toBe(1);
      expect(stderr).toContain(`${name} must be`);
      expect(stderr).not.toContain(value.slice(0, 13));
    }
  });

  test("the review App's key command is required, and an unset, empty or blank one is refused by name", async () => {
    for (const value of [undefined, "", "   "]) {
      const { code, stdout, stderr, work } = await githubApps(
        { ...appInputs, LEGION_E2E_REVIEW_APP_KEY_COMMAND: value },
        proveBoth
      );
      expect(code).toBe(1);
      expect(stderr).toContain(
        "LEGION_E2E_REVIEW_APP_KEY_COMMAND must be set to shell text that prints the review App's PEM private key on stdout"
      );
      // The refusal says what to set and prints nothing an operator pasted.
      expect(stderr).toContain("its value is not printed");
      expect(stdout).not.toContain("logins");
      expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
    }
  });

  test("a run that still exports the retired review secret is refused by name, key command or not", async () => {
    // The retired input named a broker secret for the review App, which that App no longer has:
    // a run that still sets it is told so rather than quietly reading the key command instead.
    for (const inputs of [
      { LEGION_E2E_REVIEW_APP_SECRET: "REVIEW_APP" },
      { LEGION_E2E_REVIEW_APP_SECRET: "REVIEW_APP", LEGION_E2E_REVIEW_APP_KEY_COMMAND: undefined },
    ]) {
      const { code, stdout, stderr, work } = await githubApps(
        { ...appInputs, ...inputs },
        proveBoth
      );
      expect(code).toBe(1);
      expect(stderr).toContain(
        "LEGION_E2E_REVIEW_APP_SECRET is retired: the review App's credential is its key, not a secrets broker release"
      );
      expect(stderr).toContain("LEGION_E2E_REVIEW_APP_KEY_COMMAND");
      expect(stdout).not.toContain("logins");
      expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
    }
  });

  test("a key command that is the key itself is refused by name, with no part of the key printed", async () => {
    const { code, stdout, stderr, work } = await githubApps(
      { ...appInputs, LEGION_E2E_REVIEW_APP_KEY_COMMAND: reviewKey },
      proveBoth
    );
    expect(code).toBe(1);
    expect(stderr).toContain(
      "LEGION_E2E_REVIEW_APP_KEY_COMMAND must name a command that prints the review App's private key, not the key itself"
    );
    expect(stdout).not.toContain("logins");
    expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
    holdsNoKey(stdout + stderr, keyIDs);
  });

  test("app_token_command prints the broker client's command, its owner, repository and read-only words left to the daemon's sh", async () => {
    const { code, stdout, stderr } = await githubApps(
      appInputs,
      `require_app_inputs implement review
printf 'command %s\\n' "$(app_token_command "$implement_app_secret")"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(line(stdout, "command ")).toBe(tokenCommand("IMPLEMENT_APP"));
  });

  test("the token command asks agent-secrets for the owner, repositories and read-only flag the daemon names, and prints the release", async () => {
    const argvLog = join(dir, "argv-command");
    const cases: [Record<string, string>, string][] = [
      [
        {},
        'IMPLEMENT_APP --ttl 12h --reason=legion-live-proof-github-app-token -- sh -c printf %s "$IMPLEMENT_APP"',
      ],
      [
        { LEGION_GITHUB_OWNER: "Acme-Org" },
        'IMPLEMENT_APP --ttl 12h --reason=legion-live-proof-github-app-token --github-owner Acme-Org -- sh -c printf %s "$IMPLEMENT_APP"',
      ],
      [
        { LEGION_GITHUB_OWNER: "Acme-Org", LEGION_GITHUB_REPOS: "legion" },
        'IMPLEMENT_APP --ttl 12h --reason=legion-live-proof-github-app-token --github-owner Acme-Org --github-repo legion -- sh -c printf %s "$IMPLEMENT_APP"',
      ],
      [
        {
          LEGION_GITHUB_OWNER: "Acme-Org",
          LEGION_GITHUB_REPOS: "legion nats-server",
          LEGION_GITHUB_READ_ONLY: "1",
        },
        'IMPLEMENT_APP --ttl 12h --reason=legion-live-proof-github-app-token --github-owner Acme-Org --github-repo legion --github-repo nats-server --github-read-only -- sh -c printf %s "$IMPLEMENT_APP"',
      ],
      [
        { LEGION_GITHUB_OWNER: "Acme-Org", LEGION_GITHUB_READ_ONLY: "1" },
        'IMPLEMENT_APP --ttl 12h --reason=legion-live-proof-github-app-token --github-owner Acme-Org --github-read-only -- sh -c printf %s "$IMPLEMENT_APP"',
      ],
      [
        { LEGION_GITHUB_OWNER: "", LEGION_GITHUB_REPOS: "", LEGION_GITHUB_READ_ONLY: "" },
        'IMPLEMENT_APP --ttl 12h --reason=legion-live-proof-github-app-token -- sh -c printf %s "$IMPLEMENT_APP"',
      ],
    ];
    for (const [daemonEnv, argv] of cases) {
      rmSync(argvLog, { force: true });
      const child = Bun.spawnSync(["sh", "-c", tokenCommand("IMPLEMENT_APP")], {
        env: { PATH: `${fakeBin}:${process.env.PATH}`, ARGV_LOG: argvLog, ...daemonEnv },
        stdout: "pipe",
        stderr: "pipe",
      });
      expect(child.stderr.toString()).toBe("");
      expect(child.exitCode).toBe(0);
      expect(child.stdout.toString()).toBe(readFileSync(join(store, "IMPLEMENT_APP"), "utf8"));
      expect(readFileSync(argvLog, "utf8")).toBe(`${argv}\n`);
    }
  });

  test("matching Apps pass through their own credential commands, and the block written is the daemon's", async () => {
    const { code, stdout, stderr, work } = await githubApps(appInputs, proveBoth);
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(line(stdout, "logins "))).toEqual({
      implement: "example-implement[bot]",
      review: "example-review[bot]",
    });
    // One credential field a role, and the whole text of the block: the implement App's token
    // command, the review App's `cat` of the key file this call resolved its key command into,
    // and neither role carrying both.
    expect(readFileSync(join(work, "github-apps.yaml"), "utf8")).toBe(
      `github_apps:\n  implement:\n    app_id: "101"\n    token_command: ${JSON.stringify(tokenCommand("IMPLEMENT_APP"))}\n  review:\n    app_id: "202"\n    private_key_command: ${JSON.stringify(keyFileCommand(join(work, "app-key")))}\n`
    );
    const sources = appCredentialSources(readFileSync(join(work, "github-apps.yaml"), "utf8"));
    expect(sources.implement?.kind).toBe("token_command");
    expect(sources.review?.kind).toBe("private_key_command");
    holdsNoToken(stdout);
    holdsNoKey(stdout, keyIDs);
  });

  test("a secret that releases another App's tokens fails the check, naming both ids and no token", async () => {
    const { code, stdout, stderr } = await githubApps(
      {
        ...appInputs,
        LEGION_E2E_IMPLEMENT_APP_ID: "404",
        LEGION_E2E_IMPLEMENT_APP_SECRET: "OTHER_APP",
      },
      proveBoth
    );
    expect(code).toBe(1);
    expect(stdout).not.toContain("logins");
    expect(stderr).toContain("answered App id 405, want 404");
    expect(stderr).toMatch(/^FAIL prerequisites: lib\/app-logins.ts could not prove/m);
    holdsNoToken(stderr);
  });

  // A deployment whose broker holds no credential for the implement App (its secret deleted) must
  // set that role's input: app-logins.ts reads the broker's own refusal out of the run it already
  // makes and names the role and the input, rather than its generic reason alone. It reads it out
  // of the whole of the command's stderr, so a refusal the client prints under a line of its own
  // is named the same way. The implement App is the only role this can happen to: the review
  // App's credential is its key, which the broker is never asked for.
  const refusal =
    /^app-logins: the implement App \(app_id 101\): the secrets broker serves no github-app secret under the name github_apps\.implement\.token_command gives \(UNKNOWN_SECRET\): set LEGION_E2E_IMPLEMENT_APP_SECRET to the name of a github-app secret this deployment's broker holds for that App$/m;

  test("a secret the broker does not serve fails the check, naming the role and the input", async () => {
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, UNKNOWN_SECRETS: "IMPLEMENT_APP" },
      proveBoth
    );
    expect(code).toBe(1);
    expect(stdout).not.toContain("logins");
    // The broker's own line still reaches the run's stderr as it is written.
    expect(stderr).toContain("no agent secret has this name (UNKNOWN_SECRET)");
    expect(stderr).toMatch(refusal);
    holdsNoToken(stderr);
  });

  test("a broker refusal below the first line of the command's stderr names the role and the input too", async () => {
    const preface = "agent-secrets: asking the broker AGENT_SECRETS_URL names for IMPLEMENT_APP";
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, UNKNOWN_SECRETS: "IMPLEMENT_APP", UNKNOWN_PREFACE: preface },
      proveBoth
    );
    expect(code).toBe(1);
    expect(stdout).not.toContain("logins");
    const relayed = stderr.split("\n");
    expect(relayed.indexOf(preface)).toBeLessThan(
      relayed.findIndex((l) => l.includes("no agent secret has this name (UNKNOWN_SECRET)"))
    );
    expect(stderr).toMatch(refusal);
    holdsNoToken(stderr);
  });

  test("one role proves that App alone, as Stage 4a runs it", async () => {
    const { code, stdout, stderr } = await githubApps(
      { LEGION_E2E_IMPLEMENT_APP_ID: "101", LEGION_E2E_IMPLEMENT_APP_SECRET: "IMPLEMENT_APP" },
      `require_app_inputs implement
write_github_apps implement
check_github_apps implement
echo "logins $github_app_logins"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain('logins {"implement":"example-implement[bot]"}');
  });

  // The refresher as the sandbox proof runs it under bridge: the releases of the one role whose
  // tokens come from the broker minted here, into owner-only files of a directory the caller
  // names, and the block's token commands then read those files alone.
  test("mint_app_tokens writes each release to an owner-only file atomically, one an owner and flag", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const argvLog = join(dir, "argv-mint");
    rmSync(argvLog, { force: true });
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, ARGV_LOG: argvLog },
      `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" SJawhar acme-org
echo minted`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("minted");
    const files = [
      "_all.json",
      "acme-org.json",
      "acme-org.read.json",
      "sjawhar.json",
      "sjawhar.read.json",
    ];
    expect(statSync(join(tokenDir, "implement")).mode & 0o777).toBe(0o700);
    // No temporary file is left beside them.
    expect(readdirSync(join(tokenDir, "implement")).sort()).toEqual(files);
    for (const file of files) {
      const path = join(tokenDir, "implement", file);
      expect(statSync(path).mode & 0o777).toBe(0o600);
      expect(readFileSync(path, "utf8")).toBe(readFileSync(join(store, "IMPLEMENT_APP"), "utf8"));
    }
    const flags = "--ttl 12h --reason=legion-live-proof-github-app-token";
    const asked = readFileSync(argvLog, "utf8")
      .trim()
      .split("\n")
      .map((argv) => argv.replace(/ -- sh -c .*$/, ""));
    expect(asked).toEqual([
      `IMPLEMENT_APP ${flags}`,
      `IMPLEMENT_APP ${flags} --github-owner sjawhar`,
      `IMPLEMENT_APP ${flags} --github-owner sjawhar --github-read-only`,
      `IMPLEMENT_APP ${flags} --github-owner acme-org`,
      `IMPLEMENT_APP ${flags} --github-owner acme-org --github-read-only`,
    ]);
    holdsNoToken(stdout + stderr);
  });

  test("mint_app_tokens writes the review App's key once to an owner-only file of its own, and asks the broker for the implement role alone", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const argvLog = join(dir, "argv-key-mint");
    rmSync(argvLog, { force: true });
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, ARGV_LOG: argvLog },
      `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar
echo minted`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("minted");
    // One file, the key, owner-only in an owner-only directory, and no temporary file beside it.
    expect(readdirSync(join(tokenDir, "review"))).toEqual(["key.pem"]);
    expect(statSync(join(tokenDir, "review")).mode & 0o777).toBe(0o700);
    expect(statSync(join(tokenDir, "review", "key.pem")).mode & 0o777).toBe(0o600);
    // What the key command printed, byte for byte.
    expect(readFileSync(join(tokenDir, "review", "key.pem"), "utf8")).toBe(reviewKey);
    // The broker was asked for the implement App's releases and for nothing else: the review
    // App's credential never passes through it.
    const asked = readFileSync(argvLog, "utf8").trim().split("\n");
    expect([...new Set(asked.map((argv) => argv.split(" ")[0]))]).toEqual(["IMPLEMENT_APP"]);
    holdsNoKey(stdout + stderr, keyIDs);
    holdsNoToken(stdout + stderr);
  });

  test("a key command that fails, or prints nothing, fails the run naming the review App and the file", async () => {
    // The failing command prints the key on stdout before it fails, so the row also says what
    // becomes of that: no file, and no byte of it in any message.
    const cases: [string, RegExp, string][] = [
      [
        `cat -- ${reviewKeyFile}; echo 'the key command refused' >&2; exit 4`,
        /^FAIL prerequisites: the review App \(App 202, LEGION_E2E_REVIEW_APP_KEY_COMMAND\) gave no private key for .*\/review\/key\.pem: the key command exited 4 \(124 is its 35-minute timeout\); no key is printed$/m,
        "the key command refused",
      ],
      [
        "true",
        /^FAIL prerequisites: the review App \(App 202, LEGION_E2E_REVIEW_APP_KEY_COMMAND\) gave no private key for .*\/review\/key\.pem: the key command printed nothing$/m,
        "",
      ],
    ];
    for (const [command, named, relayed] of cases) {
      const tokenDir = mkdtempSync(join(dir, "tokens."));
      const { code, stdout, stderr } = await githubApps(
        { ...appInputs, TOKEN_DIR: tokenDir, LEGION_E2E_REVIEW_APP_KEY_COMMAND: command },
        `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar
echo minted`
      );
      expect(code).toBe(1);
      expect(stdout).not.toContain("minted");
      expect(stderr).toMatch(named);
      // The command's own stderr reaches the run's, where an operator reads what went wrong.
      if (relayed !== "") expect(stderr).toContain(relayed);
      expect(readdirSync(join(tokenDir, "review"))).toEqual([]);
      holdsNoKey(stdout + stderr, keyIDs);
      holdsNoToken(stdout + stderr);
    }
  });

  test("a mint that fails fails the run naming the App, leaves no file of it and keeps the last good release", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    mkdirSync(join(tokenDir, "implement"), { mode: 0o700 });
    writeFileSync(join(tokenDir, "implement", "sjawhar.read.json"), "last good", { mode: 0o600 });
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, FAIL_MINT: "sjawhar/1" },
      `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar
echo minted`
    );
    expect(code).toBe(1);
    expect(stdout).not.toContain("minted");
    expect(stderr).toContain("grant for IMPLEMENT_APP denied");
    expect(stderr).toMatch(
      /^FAIL prerequisites: the implement App \(App 101, github-app secret IMPLEMENT_APP\) gave no release for .*sjawhar\.read\.json: agent-secrets exited 3; no token is printed$/m
    );
    expect(readdirSync(join(tokenDir, "implement")).sort()).toEqual([
      "_all.json",
      "sjawhar.json",
      "sjawhar.read.json",
    ]);
    expect(readFileSync(join(tokenDir, "implement", "sjawhar.read.json"), "utf8")).toBe(
      "last good"
    );
    // The run stopped at the first failure: the review App was never minted.
    expect(existsSync(join(tokenDir, "review"))).toBe(false);
    holdsNoToken(stdout + stderr);
  });

  test("a mint that prints nothing is refused, leaving no file", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const { code, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, EMPTY_MINT: "1" },
      `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar`
    );
    expect(code).toBe(1);
    expect(stderr).toMatch(
      /^FAIL prerequisites: the implement App \(App 101, .*_all\.json: agent-secrets printed nothing$/m
    );
    expect(readdirSync(join(tokenDir, "implement"))).toEqual([]);
  });

  test("an oversized release returns the documented overflow status and leaves no file", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, OVERSIZED_MINT: "1" },
      `set +e
app_token_mint_file "$TOKEN_DIR/release.json" "" "" IMPLEMENT_APP
status=$?
set -e
printf 'status %s\n' "$status"`
    );
    expect(code).toBe(0);
    expect(stdout).toBe("status 126");
    expect(stderr).toBe("");
    expect(existsSync(join(tokenDir, "release.json"))).toBe(false);
  });

  test("a release whose rename fails leaves no temporary file", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const failingMv = mkdtempSync(join(dir, "mv."));
    writeFileSync(join(failingMv, "mv"), "#!/bin/sh\nexit 1\n", { mode: 0o755 });
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, FAILING_MV: failingMv },
      `require_app_inputs implement review
PATH="$FAILING_MV:$PATH"
mint_app_tokens "$TOKEN_DIR" sjawhar`
    );
    expect(code).toBe(1);
    expect(readdirSync(join(tokenDir, "implement"))).toEqual([]);
    holdsNoToken(stdout + stderr);
  });

  test("the refresher mints the releases again on its period, never the key, and stops when told", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const refreshStore = mkdtempSync(join(dir, "store."));
    // The one secret the refresher may ask for: a round that asked for any other would print a
    // refresh failure on stderr, which this row refuses.
    copyFileSync(join(store, "IMPLEMENT_APP"), join(refreshStore, "IMPLEMENT_APP"));
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, STORE: refreshStore },
      `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar
key=$TOKEN_DIR/review/key.pem
echo "key before $(stat -c '%i %s %Y' -- "$key")"
app_token_refresh_seconds=1
start_app_token_refresher "$TOKEN_DIR"
printf '%s' '${release({ sjawhar: "ghs_rotated" })}' >"$STORE/IMPLEMENT_APP"
for _ in $(seq 50); do
  grep -q ghs_rotated "$TOKEN_DIR/implement/sjawhar.read.json" && break
  sleep 0.1
done
pid=$app_token_refresher
stop_app_token_refresher
kill -0 "$pid" 2>/dev/null && echo "still running" || echo "stopped"
echo "read $(cat "$TOKEN_DIR/implement/sjawhar.read.json")"
echo "key after $(stat -c '%i %s %Y' -- "$key")"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("stopped");
    expect(line(stdout, "read ")).toBe(release({ sjawhar: "ghs_rotated" }));
    expect(readdirSync(join(tokenDir, "implement")).sort()).toEqual([
      "_all.json",
      "sjawhar.json",
      "sjawhar.read.json",
    ]);
    // The key file the refresher left alone: a private key does not expire, and that App has no
    // broker secret to ask for. A mint of it would have renamed a temporary file over this one,
    // so the inode says it was never minted again.
    expect(line(stdout, "key after ")).toBe(line(stdout, "key before "));
    expect(readdirSync(join(tokenDir, "review"))).toEqual(["key.pem"]);
    expect(readFileSync(join(tokenDir, "review", "key.pem"), "utf8")).toBe(reviewKey);
  });

  test("write_github_apps --token-dir writes commands that read the file of the owner and flag asked, and the key file, in busybox sh", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    mkdirSync(join(tokenDir, "implement"));
    for (const file of ["_all", "sjawhar", "sjawhar.read"]) {
      writeFileSync(join(tokenDir, "implement", `${file}.json`), `file ${file}`);
    }
    mkdirSync(join(tokenDir, "review"), { mode: 0o700 });
    writeFileSync(join(tokenDir, "review", "key.pem"), reviewKey, { mode: 0o600 });
    const { code, stderr, work } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir },
      `require_app_inputs implement review
write_github_apps --token-dir "$TOKEN_DIR" implement review`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    const sources = appCredentialSources(readFileSync(join(work, "github-apps.yaml"), "utf8"));
    expect(sources.implement?.appId).toBe("101");
    expect(sources.implement?.kind).toBe("token_command");
    // The review App's key command reads the key file mint_app_tokens wrote, and nothing else.
    expect(sources.review).toEqual({
      appId: "202",
      kind: "private_key_command",
      command: `cat -- '${tokenDir}/review/key.pem'`,
    });
    const command = sources.implement?.command ?? "";
    for (const [daemonEnv, want] of [
      [{ LEGION_GITHUB_OWNER: "", LEGION_GITHUB_READ_ONLY: "" }, "file _all"],
      [{ LEGION_GITHUB_OWNER: "SJawhar", LEGION_GITHUB_READ_ONLY: "" }, "file sjawhar"],
      [{ LEGION_GITHUB_OWNER: "sjawhar", LEGION_GITHUB_READ_ONLY: "1" }, "file sjawhar.read"],
    ] as const) {
      const ran = Bun.spawnSync([...busyboxSh, "-c", command], {
        env: { PATH: process.env.PATH, ...daemonEnv },
        stdout: "pipe",
        stderr: "pipe",
      });
      expect(ran.stderr.toString()).toBe("");
      expect(ran.exitCode).toBe(0);
      expect(ran.stdout.toString()).toBe(want);
    }
    const key = Bun.spawnSync([...busyboxSh, "-c", sources.review?.command ?? ""], {
      env: { PATH: process.env.PATH },
      stdout: "pipe",
      stderr: "pipe",
    });
    expect(key.stderr.toString()).toBe("");
    expect(key.exitCode).toBe(0);
    expect(key.stdout.toString()).toBe(reviewKey);
    // An owner the file layout could not name reads no file outside the directory.
    const outside = Bun.spawnSync([...busyboxSh, "-c", command], {
      env: { PATH: process.env.PATH, LEGION_GITHUB_OWNER: "../review/x" },
      stdout: "pipe",
      stderr: "pipe",
    });
    expect(outside.exitCode).toBe(2);
    expect(outside.stdout.toString()).toBe("");
  });

  test("the refresher's files and the --token-dir block are proved alone, agent-secrets broken after the mint", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const brokenBin = mkdtempSync(join(dir, "broken-bin."));
    writeFileSync(join(brokenBin, "agent-secrets"), "#!/bin/sh\nexit 7\n", { mode: 0o755 });
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, BROKEN_BIN: brokenBin },
      `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar
PATH="$BROKEN_BIN:$PATH"
write_github_apps --token-dir "$TOKEN_DIR" implement review
check_github_apps implement review
echo "logins $github_app_logins"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(line(stdout, "logins "))).toEqual({
      implement: "example-implement[bot]",
      review: "example-review[bot]",
    });
  });

  // What this issue is for: the review App's key reaches the one file the daemon's container
  // reads it from, and nothing else of the run — no shell variable, no child's environment, no
  // argv the broker client was given, no message — and the run's own teardown takes that file
  // with it.
  test("the key reaches its file alone, and the run's teardown leaves neither it nor its directory", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const argvLog = join(dir, "argv-key-leak");
    rmSync(argvLog, { force: true });
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, ARGV_LOG: argvLog, KEY_FILE: reviewKeyFile },
      `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar
write_github_apps --token-dir "$TOKEN_DIR" implement review
check_github_apps implement review
echo "logins $github_app_logins"
# Every variable and function this shell holds, and the environment a child of it would take,
# written where the grep below reads them.
set >"$work/shell-variables"
env >"$work/child-environment"
echo "key file mode $(stat -c %a -- "$TOKEN_DIR/review/key.pem")"
cmp -s -- "$TOKEN_DIR/review/key.pem" "$KEY_FILE" && echo "the key file holds the key"
# Every file of the run's work and token directories holding any line of the key, by name alone.
grep -r -l -F -f "$KEY_FILE" -- "$work" "$TOKEN_DIR" |
  sed "s|^$TOKEN_DIR|TOKEN_DIR|; s|^|holds the key: |"
# The teardown the sandbox proof runs over this directory, that script's own function rather than
# a copy of it here: claude-backend-sandbox.sh's remove_app_tokens, over the directory its caller
# names in app_tokens_dir.
app_tokens_dir=$TOKEN_DIR
${sandboxTeardown}
remove_app_tokens && echo "the teardown returned 0" || echo "the teardown returned $?"
[ -e "$TOKEN_DIR" ] && echo "the App credential files remain" ||
  echo "the key file and its directory are gone"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(line(stdout, "logins "))).toEqual({
      implement: "example-implement[bot]",
      review: "example-review[bot]",
    });
    expect(line(stdout, "key file mode ")).toBe("600");
    expect(stdout).toContain("the key file holds the key");
    expect(stdout.split("\n").filter((l) => l.startsWith("holds the key: "))).toEqual([
      "holds the key: TOKEN_DIR/review/key.pem",
    ]);
    expect(stdout).toContain("the key file and its directory are gone");
    expect(stdout).toContain("the teardown returned 0");
    expectNoMissingHelpers(stdout, stderr);
    expect(existsSync(tokenDir)).toBe(false);
    holdsNoKey(stdout, keyIDs);
    holdsNoKey(stderr, keyIDs);
    holdsNoKey(readFileSync(argvLog, "utf8"), keyIDs);
    holdsNoToken(stdout + stderr);
  });

  // The other half of that teardown: a directory it could not empty is a key left on the devbox,
  // which the run must say out loud rather than pass over.
  test("the teardown fails, naming the directory, when the App credential files cannot be removed", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const review = join(tokenDir, "review");
    mkdirSync(review, { mode: 0o700 });
    writeFileSync(join(review, "key.pem"), reviewKey, { mode: 0o600 });
    // A key file `rm -rf` cannot unlink, since its directory admits no write: what a credential
    // directory of another owner, or one on a read-only mount, leaves the teardown with.
    chmodSync(review, 0o500);
    try {
      const { code, stdout, stderr } = await githubApps(
        { ...appInputs, TOKEN_DIR: tokenDir },
        `app_tokens_dir=$TOKEN_DIR
${sandboxTeardown}
remove_app_tokens && echo "the teardown returned 0" || echo "the teardown returned $?"`
      );
      // The shell around it ends well: the teardown's refusal is its return value and its FAIL
      // line, never an exit that would hide the rest of a run's cleanup.
      expect(code).toBe(0);
      expect(stdout).toContain("the teardown returned 1");
      expect(stderr).toContain(`FAIL cleanup: the App credential files remain in ${tokenDir}`);
      expectNoMissingHelpers(stdout, stderr);
      // The key is still there, which is what that FAIL line is about.
      expect(readFileSync(join(review, "key.pem"), "utf8")).toBe(reviewKey);
      holdsNoKey(stdout, keyIDs);
      holdsNoKey(stderr, keyIDs);
    } finally {
      // The mode given back, so the directory this file reserved can be removed when the run
      // ends.
      chmodSync(review, 0o700);
    }
  });

  // The check every proof's teardown ends with, over the directory write_github_apps --key-dir
  // wrote the review App's key into: one helper of the lib rather than a copy of the same `if`
  // in each of the six, printing the line the proof that called it is written in.
  test("app_key_left names the key directory a teardown left behind, and says nothing once it is gone", async () => {
    const { code, stdout, stderr, work } = await githubApps(
      appInputs,
      `require_app_inputs implement review
write_github_apps --key-dir "$work/app-key" implement review
app_key_left "$work/app-key" && echo "left: returned 0" || echo "left: returned $?"
app_key_left --check-line "$work/app-key" || true
rm -rf -- "$work/app-key"
app_key_left "$work/app-key" && echo "gone: returned 0"
app_key_left --check-line "$work/app-key" && echo "gone under --check-line: returned 0"`
    );
    expect(code).toBe(0);
    // While the key is there: the proof's own FAIL line on stderr, or the teardown check line
    // stage4b-sandbox-tree.sh writes on stdout, and a return of 1 for the caller to act on.
    expect(stdout).toContain("left: returned 1");
    expect(stderr).toBe(
      `FAIL cleanup: the review App's private key remains in ${join(work, "app-key")}`
    );
    expect(stdout).toContain(
      `CHECK app-key: FAIL: the review App's private key remains in ${join(work, "app-key")}`
    );
    // Once it is gone: nothing printed either way, and a return of 0.
    expect(stdout).toContain("gone: returned 0");
    expect(stdout).toContain("gone under --check-line: returned 0");
    holdsNoKey(stdout + stderr, keyIDs);
  });

  test("a token directory the credential commands could not carry in their shell text is refused, and nothing is written", async () => {
    for (const name of ["tokens 'x", "tokens$x", "tokens;x"]) {
      const tokenDir = join(mkdtempSync(join(dir, "tokens.")), name);
      mkdirSync(tokenDir);
      for (const then of [
        `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar
echo "wrote"`,
        `require_app_inputs implement review
write_github_apps --token-dir "$TOKEN_DIR" implement review
echo "wrote"`,
      ]) {
        const { code, stdout, stderr, work } = await githubApps(
          { ...appInputs, TOKEN_DIR: tokenDir },
          then
        );
        expect(code).toBe(1);
        expect(stdout).not.toContain("wrote");
        expect(stderr).toContain("holds a character other than letters, digits and _ . / -");
        expect(readdirSync(tokenDir)).toEqual([]);
        expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
      }
    }
  });

  // The operator's key command runs once, here on the driver, in the caller's own environment,
  // and the daemon's own field is a `cat` of the 0600 file that run wrote. So a command that
  // reads the operator's HOME is fine, and the check and the daemon cannot disagree about it,
  // because both run that same `cat`.
  test("write_github_apps --key-dir runs the key command once, here, and writes a block that reads the file it wrote", async () => {
    const counter = join(dir, "key-command-runs");
    rmSync(counter, { force: true });
    const { code, stdout, stderr, work } = await githubApps(
      {
        ...appInputs,
        // The command counts its own calls, a byte a call, beside the key it prints.
        LEGION_E2E_REVIEW_APP_KEY_COMMAND: `printf r >>${counter}; cat -- ${reviewKeyFile}`,
      },
      `require_app_inputs implement review
write_github_apps --key-dir "$work/app-key" implement review
check_github_apps implement review
echo "logins $github_app_logins"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    const keyDir = join(work, "app-key");
    // The key file alone, owner-only in an owner-only directory, holding what the command printed
    // byte for byte, with no temporary file beside it.
    expect(readdirSync(join(keyDir, "review"))).toEqual(["key.pem"]);
    expect(statSync(join(keyDir, "review")).mode & 0o777).toBe(0o700);
    expect(statSync(join(keyDir, "review", "key.pem")).mode & 0o777).toBe(0o600);
    expect(readFileSync(join(keyDir, "review", "key.pem"), "utf8")).toBe(reviewKey);
    // The block: the review App reads that file, the implement App still asks the broker.
    const sources = appCredentialSources(readFileSync(join(work, "github-apps.yaml"), "utf8"));
    expect(sources.review).toEqual({
      appId: "202",
      kind: "private_key_command",
      command: keyFileCommand(keyDir),
    });
    expect(sources.implement).toEqual({
      appId: "101",
      kind: "token_command",
      command: tokenCommand("IMPLEMENT_APP"),
    });
    // Once for the whole run, the check's own proof of that App included: the check runs the
    // block's command, which is the `cat`, never the operator's.
    expect(readFileSync(counter, "utf8")).toBe("r");
    // And that command needs nothing of the operator's environment, which is the whole point: it
    // is run under the HOME the daemon's own environment would give it.
    const ran = Bun.spawnSync([...busyboxSh, "-c", sources.review?.command ?? ""], {
      env: { PATH: process.env.PATH, HOME: "/nonexistent" },
      stdout: "pipe",
      stderr: "pipe",
    });
    expect(ran.stderr.toString()).toBe("");
    expect(ran.exitCode).toBe(0);
    expect(ran.stdout.toString()).toBe(reviewKey);
    expect(JSON.parse(line(stdout, "logins "))).toEqual({
      implement: "example-implement[bot]",
      review: "example-review[bot]",
    });
    holdsNoKey(stdout + stderr, keyIDs);
    holdsNoToken(stdout + stderr);
  });

  test("a call that writes the review role with neither directory flag is refused, naming both, and writes nothing", async () => {
    const counter = join(dir, "key-command-unrun");
    rmSync(counter, { force: true });
    const { code, stdout, stderr, work } = await githubApps(
      {
        ...appInputs,
        LEGION_E2E_REVIEW_APP_KEY_COMMAND: `printf r >>${counter}; cat -- ${reviewKeyFile}`,
      },
      `require_app_inputs implement review
write_github_apps implement review
echo "wrote the block"`
    );
    expect(code).toBe(1);
    expect(stdout).not.toContain("wrote the block");
    // The refusal says what the directory must be, and names both ways of giving one.
    expect(stderr).toContain(
      "the review App's key must be resolved into a directory the caller's cleanup removes unconditionally, on every exit"
    );
    expect(stderr).toContain("--key-dir");
    expect(stderr).toContain("--token-dir");
    expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
    // Nothing was resolved either: the refusal comes before the key command would have run.
    expect(existsSync(counter)).toBe(false);
  });

  test("--token-dir and --key-dir together are refused: they name one key file two ways", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const { code, stdout, stderr, work } = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir },
      `require_app_inputs implement review
write_github_apps --token-dir "$TOKEN_DIR" --key-dir "$work/app-key" implement review
echo "wrote the block"`
    );
    expect(code).toBe(1);
    expect(stdout).not.toContain("wrote the block");
    expect(stderr).toContain("name the review App's key file two ways; pass one");
    expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
    expect(existsSync(join(work, "app-key"))).toBe(false);
  });

  // A role's credential comes from the role, which never changes, not from whether a variable of
  // some name is set: a devbox shell that still exports the retired review_app_secret must not
  // make the broker the review App's source again, nor put that stale value in the strip list.
  test("a stale review_app_secret in the environment asks the broker nothing and reaches no strip list", async () => {
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const argvLog = join(dir, "argv-stale-secret");
    rmSync(argvLog, { force: true });
    const { code, stdout, stderr, work } = await githubApps(
      {
        ...appInputs,
        // No release of this name is in the store, so a mint that asked for it would fail loudly.
        review_app_secret: "STALE_REVIEW_SECRET",
        TOKEN_DIR: tokenDir,
        ARGV_LOG: argvLog,
      },
      `require_app_inputs implement review
echo "unsets \${app_key_unsets[*]}"
mint_app_tokens "$TOKEN_DIR" sjawhar
write_github_apps --token-dir "$TOKEN_DIR" implement review
echo wrote`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("wrote");
    // The broker was asked for the implement App's releases and for nothing else.
    const asked = readFileSync(argvLog, "utf8").trim().split("\n");
    expect([...new Set(asked.map((argv) => argv.split(" ")[0]))]).toEqual(["IMPLEMENT_APP"]);
    // The strip list carries the implement App's secret name and no stale review value.
    expect(line(stdout, "unsets ")).toContain("-u IMPLEMENT_APP");
    expect(line(stdout, "unsets ")).not.toContain("STALE_REVIEW_SECRET");
    // And the review App's credential is still its key.
    const sources = appCredentialSources(readFileSync(join(work, "github-apps.yaml"), "utf8"));
    expect(sources.review).toEqual({
      appId: "202",
      kind: "private_key_command",
      command: keyFileCommand(tokenDir),
    });
  });

  test("a key command that reads the key between its PEM markers is a command, and is taken", async () => {
    const { code, stdout, stderr, work } = await githubApps(
      {
        ...appInputs,
        // The guard refuses a pasted key, never a command that names a PEM header.
        LEGION_E2E_REVIEW_APP_KEY_COMMAND: `awk '/-----BEGIN/,/-----END/' ${reviewKeyFile}`,
      },
      proveBoth
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(line(stdout, "logins "))).toEqual({
      implement: "example-implement[bot]",
      review: "example-review[bot]",
    });
    expect(readFileSync(join(work, "app-key", "review", "key.pem"), "utf8")).toBe(reviewKey);
  });

  test("a key command carrying a long base64 word is a command, and is taken", async () => {
    // The guard refuses the common pastes of a key where its command goes — a PEM, base64 of a
    // PEM, and a value with three or more lines of 40 or more base64 characters alone — and takes
    // every other value: a command that names a deployment's own secret version, an opaque base64
    // word of its own, is one of them, and a guard that refused it would be in the way of the
    // operators it is for.
    const version = "QUJDREVGR0hJSg".repeat(25);
    const { code, stdout, stderr, work } = await githubApps(
      {
        ...appInputs,
        LEGION_E2E_REVIEW_APP_KEY_COMMAND: `printf '%s' ${version} >/dev/null; cat -- ${reviewKeyFile}`,
      },
      proveBoth
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(JSON.parse(line(stdout, "logins "))).toEqual({
      implement: "example-implement[bot]",
      review: "example-review[bot]",
    });
    expect(readFileSync(join(work, "app-key", "review", "key.pem"), "utf8")).toBe(reviewKey);
  });

  test("a key pasted as one base64 word is refused by name, with no part of it printed", async () => {
    // The shape an older recipe's GH_REVIEW_APP_PRIVATE_KEY_B64 held. Left to `sh`, it would come
    // back in a `not found` error, which the relay then masks; refused here, it never gets that
    // far.
    const pasted = Buffer.from(reviewKey, "utf8").toString("base64");
    const { code, stdout, stderr, work } = await githubApps(
      { ...appInputs, LEGION_E2E_REVIEW_APP_KEY_COMMAND: pasted },
      proveBoth
    );
    expect(code).toBe(1);
    expect(stderr).toContain(
      "LEGION_E2E_REVIEW_APP_KEY_COMMAND is base64 characters and whitespace alone, which is a key pasted where its command goes"
    );
    expect(stderr).toContain("its value is not printed");
    expect(stderr).not.toContain(pasted.slice(0, 40));
    expect(stdout).not.toContain("logins");
    expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
    holdsNoKey(stdout + stderr, keyIDs);
  });

  test("a key pasted as the base64 `base64` prints, wrapped at 76 columns, is refused with no line of it printed", async () => {
    // `base64 key.pem` wraps at 76 columns by default, and `sh` would echo each of those lines.
    // The value is that command's own output over a real 2048-bit key, not a spelling of it
    // written here.
    const made = Bun.spawnSync(["base64", reviewKeyFile]);
    expect(made.exitCode).toBe(0);
    const pasted = made.stdout.toString();
    const lines = pasted.trimEnd().split("\n");
    expect(lines.length).toBeGreaterThan(20);
    expect(lines[0]?.length).toBe(76);
    const { code, stdout, stderr, work } = await githubApps(
      { ...appInputs, LEGION_E2E_REVIEW_APP_KEY_COMMAND: pasted },
      proveBoth
    );
    expect(code).toBe(1);
    expect(stderr).toContain(
      "LEGION_E2E_REVIEW_APP_KEY_COMMAND is base64 characters and whitespace alone, which is a key pasted where its command goes"
    );
    expect(stderr).toContain("its value is not printed");
    // No line of the wrapped key is anywhere in the run's output, neither whole nor begun.
    for (const wrapped of lines) expect(stdout + stderr).not.toContain(wrapped.slice(0, 20));
    expect(stdout).not.toContain("logins");
    expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
    holdsNoKey(stdout + stderr, keyIDs);
  });

  test("a PEM pasted behind a leading newline or space is refused by name, with no part of it printed", async () => {
    // The prefix test reads the value with its whitespace taken out, so the blank line or the
    // indent a paste carries changes nothing: it is still the key itself.
    for (const pasted of [`\n${reviewKey}`, `   ${reviewKey}`]) {
      const { code, stdout, stderr, work } = await githubApps(
        { ...appInputs, LEGION_E2E_REVIEW_APP_KEY_COMMAND: pasted },
        proveBoth
      );
      expect(code).toBe(1);
      expect(stderr).toContain(
        "LEGION_E2E_REVIEW_APP_KEY_COMMAND must name a command that prints the review App's private key, not the key itself"
      );
      expect(stderr).toContain("its value is not printed");
      expect(stdout).not.toContain("logins");
      expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
      holdsNoKey(stdout + stderr, keyIDs);
    }
  });
  // The three refusals the guard has, each named by the message it prints.
  const pemRefused =
    "LEGION_E2E_REVIEW_APP_KEY_COMMAND must name a command that prints the review App's private key, not the key itself";
  const base64Refused =
    "LEGION_E2E_REVIEW_APP_KEY_COMMAND is base64 characters and whitespace alone, which is a key pasted where its command goes";
  const linesRefused =
    "LEGION_E2E_REVIEW_APP_KEY_COMMAND has three or more lines that are 40 or more base64 characters alone";
  // The pastes of a key an operator makes where its command goes, each as the shell would hold
  // it, with the refusal it earns. The value is the key this run generated every time, never a
  // spelling of one written here.
  const pastedKeys = (): [string, string, string][] => [
    ["a PEM in quotes", `'${reviewKey}'`, pemRefused],
    ["a PEM after a byte-order mark", `\uFEFF${reviewKey}`, pemRefused],
    [
      "the PEM body and its END line, with no header line",
      reviewKey.trimEnd().split("\n").slice(1).join("\n"),
      linesRefused,
    ],
    [
      "openssl pkcs12 output, which opens with its Bag Attributes",
      `Bag Attributes\n    localKeyID: 54 69 6D 65\nKey Attributes: <No Attributes>\n${reviewKey}`,
      linesRefused,
    ],
    ["a PEM behind an export", `export K='${reviewKey}'`, linesRefused],
    [
      "base64 of the key in one quoted word",
      `'${Buffer.from(reviewKey, "utf8").toString("base64")}'`,
      base64Refused,
    ],
  ];
  // The first 20 characters of every line of the key and of the base64 `base64` makes of it. A
  // value that reaches `sh` is echoed a line at a time and each line may be cut, so a leak shows
  // as the head of a line where holdsNoKey, which reads whole lines, would miss it.
  const holdsNoKeyLine = (text: string) => {
    const wrapped = Bun.spawnSync(["base64", reviewKeyFile]);
    expect(wrapped.exitCode).toBe(0);
    for (const written of [...reviewKey.split("\n"), ...wrapped.stdout.toString().split("\n")]) {
      if (written.length >= 20) expect(text).not.toContain(written.slice(0, 20));
    }
  };

  test("each shape a pasted key arrives in is refused by name, with no line of it printed", async () => {
    for (const [shape, pasted, refusal] of pastedKeys()) {
      const { code, stdout, stderr, work } = await githubApps(
        { ...appInputs, LEGION_E2E_REVIEW_APP_KEY_COMMAND: pasted },
        proveBoth
      );
      expect(code, shape).toBe(1);
      expect(stderr, shape).toContain(refusal);
      expect(stderr, shape).toContain("its value is not printed");
      expect(stdout, shape).not.toContain("logins");
      expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
      holdsNoKey(stdout + stderr, keyIDs);
      holdsNoKeyLine(stdout + stderr);
    }
  });

  test("a key quoted with `>` before each line is refused, and no line of it is written to the working directory", async () => {
    // A mail client's quoting slips past the shapes above: with the `> ` in front, no line is
    // base64 alone. `sh` reads each of those lines as a redirection and creates an empty file
    // named after it in whatever directory the proof runs from — key text on disk under a name
    // no relay can mask — so the guard takes a leading `>` off each line before it counts, and
    // the paste is turned back before the command runs.
    const quoted = reviewKey
      .trimEnd()
      .split("\n")
      .map((written) => `> ${written}`)
      .join("\n");
    const { code, stdout, stderr, work } = await githubApps(
      { ...appInputs, LEGION_E2E_REVIEW_APP_KEY_COMMAND: quoted },
      `mkdir -p "$work/run-from"
cd "$work/run-from"
require_app_inputs implement review
write_github_apps --key-dir "$work/app-key" implement review
echo taken`
    );
    expect(code).toBe(1);
    expect(stderr).toContain(linesRefused);
    expect(stderr).toContain("its value is not printed");
    expect(stdout).not.toContain("taken");
    // The directory the run ran from is as it was: `sh` never saw the paste.
    expect(readdirSync(join(work, "run-from"))).toEqual([]);
    expect(existsSync(join(work, "github-apps.yaml"))).toBe(false);
    holdsNoKey(stdout + stderr, keyIDs);
    holdsNoKeyLine(stdout + stderr);
  });

  test("the commands operators write are taken, whatever they mention, however many lines they have and whatever they redirect", async () => {
    // The guard's tolerance, which costs as much as its refusals: each of these reads a key out
    // of a deployment's own store, and a guard that refused one would be in the way of the
    // operator it is for. They are read for their shape alone here, so none of them runs. A
    // redirection is part of a command, the one that opens the last line here included: the
    // guard takes a leading `>` off a line before it counts that line's base64, and nothing else.
    for (const command of [
      "gcloud secrets versions access latest --secret=legion-review-app-key --project=legion-prod",
      "kubectl -n legion get secret review-app -o jsonpath='{.data.key}' | base64 -d",
      "cat ~/.config/legion/review-key.pem",
      "pass show legion/review-app-key",
      "op read op://legion/review-app/key.pem",
      "awk '/-----BEGIN/,/-----END/' ~/.config/legion/review-key.pem",
      'project=legion-prod\ngcloud secrets versions access latest \\\n  --secret=review-key --project="$project"',
      "gpg --quiet --decrypt ~/.legion/review-key.pem.gpg 2>/dev/null",
      'kubectl -n legion get secret review-app -o jsonpath="{.data.key}" | base64 -d \\\n  > /dev/stdout',
    ]) {
      const { code, stdout, stderr } = await githubApps(
        { ...appInputs, LEGION_E2E_REVIEW_APP_KEY_COMMAND: command },
        "require_app_inputs implement review\necho taken"
      );
      expect(stderr, command).toBe("");
      expect(code, command).toBe(0);
      expect(stdout, command).toContain("taken");
    }
  });

  test("a key left where its command goes has its armour and its long lines masked at the sink, caught by the guard or not", async () => {
    // The sink, which is what holds when a shape the guard does not know is pasted: the value is
    // put straight into the variable app_token_mint_key reads, as a guard that missed it would
    // leave it, so every line of it reaches `sh -c`, which echoes back what it cannot run. What
    // that echo costs the run is a masked line apiece for the lines a key is wrapped at — the
    // relay passes a line of 16 characters or fewer, and the key read below is wrapped at 64.
    for (const [shape, pasted] of pastedKeys()) {
      const { code, stdout, stderr, work } = await githubApps(
        { ...appInputs, PASTED: pasted },
        `review_app_id=202
review_app_key_command=$PASTED
app_token_mint_key "$work/app-key" || echo "refused: $app_token_mint_failed"
echo done`
      );
      expect(code, shape).toBe(0);
      expect(stdout, shape).toContain("done");
      // No key was minted out of a key: the file is absent, or holds nothing of it either.
      expect(stdout, shape).toContain("refused: the review App (App 202");
      const keyFile = join(work, "app-key", "review", "key.pem");
      const written = existsSync(keyFile) ? readFileSync(keyFile, "utf8") : "";
      holdsNoKey(stdout + stderr + written, keyIDs);
      holdsNoKeyLine(stdout + stderr + written);
    }
  });

  test("the relay masks the key material of a line and passes the rest of it on", async () => {
    // The other half of the sink: the operator reads why their own command failed — this script
    // names no secret store of theirs, so that stderr is all they have — while the key material
    // the same line carries is masked.
    // The command writes one line of the key's base64 and one of its armour into a message of
    // its own, as a command that reads the wrong file does.
    const body = reviewKey.trimEnd().split("\n")[1];
    const armour = reviewKey.split("\n")[0];
    const { code, stdout, stderr } = await githubApps(
      {
        ...appInputs,
        MIXED: `printf 'ERROR: secret legion-review-app-key not found; read %s\\n' '${body}' >&2; printf '%s\\n' '${armour}' >&2; exit 4`,
      },
      `review_app_id=202
review_app_key_command=$MIXED
app_token_mint_key "$work/app-key" || echo "refused: $app_token_mint_failed"
echo done`
    );
    expect(code).toBe(0);
    expect(stdout).toContain("done");
    // The command's own words, which the operator acts on.
    expect(stderr).toContain("ERROR: secret legion-review-app-key not found; read ");
    // And the key material of those two lines, masked: the body line the message quoted, and the
    // PEM armour of the line after it.
    expect(stderr).toContain("read [redacted key]");
    expect(stderr.split("\n")).toContain("[redacted key]");
    holdsNoKey(stdout + stderr, keyIDs);
    holdsNoKeyLine(stdout + stderr);
  });

  test("the key command's first line reaches the run's stderr while that command is still running", async () => {
    // What the relay is for: an operator whose key command prints progress — a line before a slow
    // read of their own store, or a prompt — reads it while the command runs, not after it ends.
    // Every stage between that command and the run's stderr can hold a line it has read, and
    // each of them held it until the command exited before the lib's two probes released them
    // (github-apps.sh, above app_token_flag_taken, says which stage holds a line and which flag
    // releases it).
    // So the row reads the run's stderr as it arrives rather than after the exit, and asserts the
    // gap between the two: the line is due about 100 ms in, and the command exits about 2100 ms
    // in, so a line that arrives 1.5 s before the exit cannot be a line that arrived with it,
    // and a host loaded enough to spend a second of that margin on starting bash still passes.
    const progress = "reading the review App key from the deployment store";
    const started = performance.now();
    const { child, work } = spawnGithubApps(
      {
        ...appInputs,
        LEGION_E2E_REVIEW_APP_KEY_COMMAND: `printf '%s\\n' '${progress}' >&2; sleep 2; cat -- ${reviewKeyFile}`,
      },
      `require_app_inputs implement review
write_github_apps --key-dir "$work/app-key" implement review
echo written`
    );
    let firstLineAt = 0;
    let stderr = "";
    const decoder = new TextDecoder();
    const reading = (async () => {
      for await (const chunk of child.stderr as ReadableStream<Uint8Array>) {
        stderr += decoder.decode(chunk, { stream: true });
        if (firstLineAt === 0 && stderr.includes("\n")) firstLineAt = performance.now();
      }
    })();
    const stdout = await new Response(child.stdout).text();
    const code = await child.exited;
    const exitedAt = performance.now();
    await reading;
    expect(code).toBe(0);
    expect(stdout).toContain("written");
    expect(stderr.trim()).toBe(progress);
    // The line arrived, and it arrived while the command was still in its sleep.
    expect(firstLineAt).toBeGreaterThan(0);
    expect(exitedAt - firstLineAt).toBeGreaterThan(1500);
    expect(firstLineAt - started).toBeLessThan(1000);
    // And the key the command printed after the sleep still reached its file alone.
    expect(readFileSync(join(work, "app-key", "review", "key.pem"), "utf8")).toBe(reviewKey);
    holdsNoKey(stdout + stderr, keyIDs);
  });

  test("the relay writes nothing of its own: the run's stderr holds the command's line, masked, and nothing else", async () => {
    // The failure the lib's two probes exist to prevent. Each probe takes a flag of one
    // implementation (github-apps.sh, above app_token_flag_taken), and an awk or a stdbuf that
    // answers such a flag with a warning would write that warning into this very channel, the
    // one an operator reads their own command's words out of. So the run's stderr must hold what
    // the command wrote, masked, and nothing besides: no warning, no usage line, nothing of the
    // relay's own. The stand-in awk goes first on PATH to make that failure reachable here: this
    // pod's awk is a mawk that takes `-W interactive` without a word, so a probe that wrongly
    // accepted a warning would pass this row against the real awk. The stand-in warns on `-W`
    // and is the real awk otherwise, so the probe must refuse its flag — and if it does not, the
    // warning lands in the stderr this row reads.
    const body = reviewKey.trimEnd().split("\n")[1];
    const { code, stdout, stderr, work } = await githubApps(
      {
        ...appInputs,
        PATH: `${warningAwk}:${fakeBin}:${process.env.PATH}`,
        LEGION_E2E_REVIEW_APP_KEY_COMMAND: `printf 'reading %s\\n' '${body}' >&2; cat -- ${reviewKeyFile}`,
      },
      `require_app_inputs implement review
write_github_apps --key-dir "$work/app-key" implement review
echo written`
    );
    expect(code).toBe(0);
    expect(stdout).toContain("written");
    expect(stderr.split("\n")).toEqual(["reading [redacted key]"]);
    expect(readFileSync(join(work, "app-key", "review", "key.pem"), "utf8")).toBe(reviewKey);
    holdsNoKey(stdout + stderr, keyIDs);
    holdsNoKeyLine(stdout + stderr);
  });

  test("a key command whose stderr holds NUL bytes is still bounded at a megabyte and refused", async () => {
    // The bound counts the lines awk reads, and mawk under `-W interactive` — the flag the lib's
    // probe takes here — drops everything from a NUL byte to the end of its line and joins the
    // rest to the next line. Without the NUL replacement the lib puts before the bound, these
    // 1024 lines of 1024 bytes count as one short line: the command writes a megabyte of stderr,
    // the overflow refusal never fires, and the key is written from a command that overran. With
    // it the bound sees the bytes that arrived, refuses with 123, and the token the command
    // wrote after them — the line that crosses the limit, which `head -c` had already cut — is
    // dropped rather than relayed in pieces.
    const nulLines = `awk 'BEGIN { s = sprintf("%1022s", ""); gsub(/ /, "a", s); for (i = 0; i < 1024; i++) printf "@%s\\n", s }' | tr '@' '\\000' >&2`;
    const { code, stdout, stderr, work } = await githubApps(
      {
        ...appInputs,
        NUL_STDERR: `${nulLines}; printf 'ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789\\n' >&2; cat -- ${reviewKeyFile}`,
      },
      `review_app_id=202
review_app_key_command=$NUL_STDERR
set +e
app_token_mint_command "$work/direct-key.pem" "$NUL_STDERR" key
printf 'status %s\n' "$?"
set -e
app_token_mint_key "$work/app-key" || echo "refused: $app_token_mint_failed"
echo done`
    );
    expect(code).toBe(0);
    expect(stdout.split("\n")).toContain("status 123");
    expect(stdout).toContain(
      "refused: the review App (App 202, LEGION_E2E_REVIEW_APP_KEY_COMMAND) gave no private key for"
    );
    expect(stdout).toContain("the key command wrote more than 1048576 bytes to stderr");
    expect(stdout).toContain("done");
    // Neither call left a key behind, and no NUL byte reached the run's stderr.
    expect(existsSync(join(work, "direct-key.pem"))).toBe(false);
    expect(existsSync(join(work, "app-key", "review", "key.pem"))).toBe(false);
    expect(stderr).not.toContain("\0");
    // The lines that fitted were relayed, masked, and the token after them was not relayed at
    // all: it is in the line the bound drops. The cut leaves only the token's first byte past the
    // limit, so this absence cannot fail by itself; the row's lock is the status 123 above.
    expect(stderr.split("\n")).toContain("?[redacted key]");
    expect(stdout + stderr).not.toContain("ghp_ABCDEFGHIJKLMNO");
    holdsNoToken(stdout + stderr);
    holdsNoKey(stdout + stderr, keyIDs);
  });

  test("a request waiting on a person reaches the operator with its approval link whole, and the key command's relay still masks a key", async () => {
    // The two relays, one a channel. agent-secrets is the broker's own voice: the line it writes
    // while a human-tier grant waits on a person carries the link that person approves it by,
    // and it is the only place that link appears, so the release mint relays the line as it was
    // written. Nothing of the operator's is echoed back out of that channel.
    const approval =
      "agent-secrets: waiting for approval: http://127.0.0.1:40925/approve/4a99c1e2-7b0d-4f6e-9a51-0d3c2e8fb274";
    const tokenDir = mkdtempSync(join(dir, "tokens."));
    const minted = await githubApps(
      { ...appInputs, TOKEN_DIR: tokenDir, WAIT_NOTICE: approval },
      `require_app_inputs implement review
mint_app_tokens "$TOKEN_DIR" sjawhar
echo minted`
    );
    expect(minted.code).toBe(0);
    expect(minted.stdout).toContain("minted");
    // Whole: the operator opens the link and approves the request the run waits on.
    expect(minted.stderr.split("\n")).toContain(approval);
    holdsNoToken(minted.stdout + minted.stderr);
    // The key command's own stderr is the other channel, the one a key can be pasted into, and
    // there a key's body line is masked.
    const body = reviewKey.trimEnd().split("\n")[1];
    const relayed = await githubApps(
      { ...appInputs, PASTED: `printf '%s\\n' '${body}' >&2; exit 7` },
      `review_app_id=202
review_app_key_command=$PASTED
app_token_mint_key "$work/app-key" || echo "refused: $app_token_mint_failed"
echo done`
    );
    expect(relayed.code).toBe(0);
    expect(relayed.stdout).toContain("done");
    expect(relayed.stderr.split("\n")).toContain("[redacted key]");
    holdsNoKey(relayed.stdout + relayed.stderr, keyIDs);
    holdsNoKeyLine(relayed.stdout + relayed.stderr);
  });
});
