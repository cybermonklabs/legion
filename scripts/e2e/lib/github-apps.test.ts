// lib/github-apps.sh run as the stage proofs run it: the input checks, the github-apps.yaml block
// they write for lib/app-logins.ts, check_github_apps over it, and the token files the sandbox
// proof's refresher mints under bridge, each driven under a stage script's shell options against
// the stand-in GitHub of stand-in-github-fixtures.ts.
import { beforeAll, describe, expect, test } from "bun:test";
import {
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
import { appTokenSources } from "./app-logins";
import {
  appPrivateKey,
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
// client's own progress lines put it), releases nothing when EMPTY_MINT is set, and streams
// stdout when OVERSIZED_MINT is set.
const store = join(dir, "store");
const fakeBin = join(dir, "bin");
// The review App's own private key, the one the stand-in GitHub verifies an App JWT against. The
// rig drives it as a deployment does — through a command that prints it, never through a variable
// or an argument holding it — and the base64 body lines of that key are this file's sentinel:
// 64 characters apiece of an RSA key this run generated, which no other fixture here could print.
// holdsNoKey looks for them, and for the armor line, wherever the key must not be.
const reviewKey = appPrivateKey(202);
const reviewKeyFile = join(dir, "review-key.pem");
const keySentinel = reviewKey.split("\n").filter((pem) => pem.length >= 40 && pem[0] !== "-");
const holdsNoKey = (text: string) => {
  expect(text).not.toContain("BEGIN PRIVATE KEY");
  for (const pem of keySentinel) expect(text).not.toContain(pem);
};
// The key command the rig gives the review role, which has no broker secret to name: a `cat` of
// the file above, the shape write_github_apps --token-dir writes and an operator's own has.
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
  writeFileSync(reviewKeyFile, reviewKey, { mode: 0o600 });
  writeFileSync(
    join(fakeBin, "agent-secrets"),
    `#!/bin/sh
printf '%s\\n' "$*" >>"\${ARGV_LOG:-/dev/null}"
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

async function githubApps(inputs: Record<string, string | undefined>, then: string) {
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
  const child = Bun.spawn(["bash", "-c", shell], { env, stdout: "pipe", stderr: "pipe" });
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

describe("lib/github-apps.sh", () => {
  const proveBoth = `require_app_inputs implement review
write_github_apps implement review
check_github_apps implement review
echo "logins $github_app_logins"`;

  test("the default pair is the lib's documented one, and an input left unset takes it", async () => {
    seen.length = 0;
    const { code, stdout, stderr, work } = await githubApps(
      { LEGION_E2E_REVIEW_APP_ID: "202", LEGION_E2E_REVIEW_APP_KEY_COMMAND: reviewKeyCommand },
      `require_app_inputs implement review
write_github_apps implement review
echo "defaults $default_implement_app_id $default_implement_app_secret $default_review_app_id"`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    // The review App has no default credential of its own: its key command is required and
    // default_review_app_secret is gone. The three below are read outside this file — the
    // implement pair out of the lib's text by packages/daemon/internal/api/real_github_test.go,
    // all three by scripts/e2e/README.md — so each is pinned here.
    expect(line(stdout, "defaults ")).toBe("5341240 LEGION_IMPLEMENT_APP 5341243");
    const sources = appTokenSources(readFileSync(join(work, "github-apps.yaml"), "utf8"));
    expect(sources.implement).toEqual({
      appId: "5341240",
      source: "token_command",
      command: tokenCommand("LEGION_IMPLEMENT_APP"),
    });
    expect(sources.review).toEqual({
      appId: "202",
      source: "private_key_command",
      command: reviewKeyCommand,
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

  test("app_key_unsets strips the implement secret, the lib's default name, the two retired review names and every PRIVATE_KEY variable", async () => {
    // Each name is exported first, so the strip list has something to remove: without it the
    // child would print them all. The three exported after the call are the lib's own literal
    // entries: its `compgen -e` sweep never saw them, so only the literals can strip them, and a
    // stale export of either retired review name reaches no process the run starts.
    const { code, stdout, stderr } = await githubApps(
      { ...appInputs, SOME_OTHER_APP_PRIVATE_KEY_B64: "irrelevant" },
      `export IMPLEMENT_APP=x
require_app_inputs implement review
export "$default_implement_app_secret=x" LEGION_REVIEW_APP=x GH_REVIEW_APP_PRIVATE_KEY_B64=x
names="IMPLEMENT_APP|SOME_OTHER_APP_PRIVATE_KEY_B64|$default_implement_app_secret|LEGION_REVIEW_APP|GH_REVIEW_APP_PRIVATE_KEY_B64"
echo "set $(env | grep -c -E "^($names)=")"
env "\${app_key_unsets[@]}" sh -c "echo \\"left \\$(env | grep -c -E '^($names)=' || true)\\""`
    );
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("set 5");
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
    holdsNoKey(stdout + stderr);
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
    // command, the review App's key command verbatim, and neither role carrying both.
    expect(readFileSync(join(work, "github-apps.yaml"), "utf8")).toBe(
      `github_apps:\n  implement:\n    app_id: "101"\n    token_command: ${JSON.stringify(tokenCommand("IMPLEMENT_APP"))}\n  review:\n    app_id: "202"\n    private_key_command: ${JSON.stringify(reviewKeyCommand)}\n`
    );
    const sources = appTokenSources(readFileSync(join(work, "github-apps.yaml"), "utf8"));
    expect(sources.implement?.source).toBe("token_command");
    expect(sources.review?.source).toBe("private_key_command");
    holdsNoToken(stdout);
    holdsNoKey(stdout);
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
    holdsNoKey(stdout + stderr);
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
      holdsNoKey(stdout + stderr);
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
    const sources = appTokenSources(readFileSync(join(work, "github-apps.yaml"), "utf8"));
    expect(sources.implement?.appId).toBe("101");
    expect(sources.implement?.source).toBe("token_command");
    // The review App's key command reads the key file mint_app_tokens wrote, and nothing else.
    expect(sources.review).toEqual({
      appId: "202",
      source: "private_key_command",
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
# The teardown the sandbox proof runs over this directory (claude-backend-sandbox.sh's
# remove_app_tokens): the refresher stopped, then the directory removed, the key with it.
stop_app_token_refresher
rm -rf -- "$TOKEN_DIR"
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
    expect(existsSync(tokenDir)).toBe(false);
    holdsNoKey(stdout);
    holdsNoKey(stderr);
    holdsNoKey(readFileSync(argvLog, "utf8"));
    holdsNoToken(stdout + stderr);
  });

  test("a token directory the token commands could not carry in their shell text is refused, and nothing is written", async () => {
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
});
