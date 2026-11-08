import { beforeAll, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// claude-backend-tmux.sh's prerequisites on a box whose own `secrets` store holds no App credential
// (the devbox once its local copies of another deployment's keys were removed): the run must reach
// its two role Apps' credentials through the secrets broker's client, `agent-secrets`, and the
// operator's own key command alone, and never ask the local store, or it can never pass there.
// Every tool the prerequisites ask for is a stand-in, so the run touches nothing outside this
// test's directory, and the stand-ins answer as far as read_app_logins, the last prerequisite that
// reaches an App credential: a run that stopped before it would say nothing about where those
// credentials came from. It stops soon after: `mise` resolves no real binary for the run's own
// PATH, and then the stand-in `gh` acts as no account for the proof human, which ends the run.
const script = join(import.meta.dir, "..", "claude-backend-tmux.sh");
const dir = temporaryDirectory("claude-backend-key-runner-test");

const bin = join(dir, "bin");
const scratch = join(dir, "tmp");
// Where the stand-in `secrets` records that it was asked.
const asked = join(dir, "secrets-asked");
const keyFile = join(dir, "anthropic-key");
// The release prerequisites admits: it refuses any other Bun before it runs lib/app-logins.ts, so
// the stand-in `bun` below answers `--version` with the pin out of the file that check reads.
const pinned = readFileSync(join(import.meta.dir, "..", "..", "..", ".bun-version"), "utf8").trim();
// What the operator's own key command prints here, which is the review App's whole credential:
// prerequisites runs that command once, on the driver, into the run's own 0600 key file, and the
// block it writes carries a `cat` of that file. It holds no apostrophe, since the row below gives
// the command as a single-quoted `printf` argument.
const keyCommandPrints = "a stand-in review App key, printed by the key command of this run";
// Where the stand-in lib/app-logins.ts below leaves each App's credential as the block hands it to
// the daemon: the review App's key from its private_key_command, the implement App's token from
// its token_command.
const keyRead = join(dir, "review-key-the-block-reads");
const tokenRead = join(dir, "implement-token-the-block-reads");
function stub(name: string, body: string) {
  writeFileSync(join(bin, name), `#!/bin/sh\n${body}\n`);
  chmodSync(join(bin, name), 0o755);
}
// The real programs the run needs on the PATH it is given: each is linked into the run's own bin
// below. A host missing one of them would run the script against a PATH without it, which fails
// at whichever line first calls it, so the row is skipped and names what is missing instead.
const tools = [
  "bash",
  "sh",
  "jq",
  "awk",
  "sed",
  "tr",
  "cat",
  "mktemp",
  "date",
  "getconf",
  "readlink",
  "realpath",
  "dirname",
  "basename",
  "grep",
  "env",
  "rm",
  "mkdir",
  "chmod",
  "ls",
  "head",
  "tail",
  "cut",
  "sort",
  "uniq",
  "wc",
  "sleep",
  "kill",
  "id",
  "stat",
  "seq",
  "timeout",
  "touch",
  "cp",
  "mv",
  "ln",
  "find",
  "xargs",
  "tee",
  "od",
  "base64",
  "flock",
  "ps",
  "sha256sum",
];
// The gate is given every name: it walks each one and names the missing ones in its warning.
// `found` is the run's own walk, whose real binaries the stand-in PATH below is built from.
const found = new Map(tools.map((name) => [name, hostTool(name)] as const));
const skip = skipWithout(...tools);

// Nothing below is read by a run that will not happen, so a host that skips builds none of it;
// and it is built in a hook, below the one the reservation registered, so a run that selects no
// row of this file builds none of it either.
beforeAll(() => {
  if (skip) return;
  mkdirSync(bin);
  mkdirSync(scratch);
  // The stand-ins the prerequisites need only find: each prints nothing and exits 0.
  const quiet = ["go", "docker", "ss", "tmux", "mise", "gh", "jj", "pgrep", "claude", "python3"];
  for (const name of quiet) stub(name, "exit 0");
  // Anthropic accepts the model key.
  stub("curl", "printf 200");
  // The box's own store holds no App credential, and records that it was asked.
  stub("secrets", `printf '%s\\n' "$*" >> '${asked}'\necho "no secret named $2" >&2\nexit 1`);
  // The broker's client the implement App's token comes through: it runs the command after `--`
  // with that secret's release in its environment, under whatever flags the token command gave.
  stub(
    "agent-secrets",
    'name=$1; shift; while [ "$1" != -- ]; do shift; done; shift; export "$name={}"; exec "$@"'
  );
  // The pinned Bun, and lib/app-logins.ts with it: prerequisites holds the version to the pin
  // before it runs that script, and the script runs each App's credential command out of the
  // block the lib wrote, as the daemon runs it. This stand-in does that much, leaves what each
  // command printed where the row below reads it, and answers with the logins its caller parses.
  stub(
    "bun",
    `case $1 in --version) echo ${pinned}; exit 0 ;; esac
for arg in "$@"; do case $arg in *github-apps.yaml) yaml=$arg ;; esac; done
sh -c "$(sed -n 's/^    private_key_command: //p' "$yaml" | jq -r .)" >'${keyRead}'
sh -c "$(sed -n 's/^    token_command: //p' "$yaml" | jq -r .)" >'${tokenRead}'
printf '{"implement":"example-implement[bot]","review":"example-review[bot]"}'`
  );
  for (const [name, path] of found) {
    if (path !== null) symlinkSync(path, join(bin, name));
  }
  writeFileSync(keyFile, "sk-test\n", { mode: 0o600 });
});

test.skipIf(skip)(
  "the prerequisites reach both Apps' credentials and never ask the box's own secrets store",
  () => {
    const run = Bun.spawnSync(["bash", script], {
      env: {
        HOME: dir,
        PATH: bin,
        TMPDIR: scratch,
        LEGION_E2E_MODEL_ROUTE: "key-file",
        LEGION_E2E_ANTHROPIC_KEY_FILE: keyFile,
        // The review App's key command, which prerequisites requires and has no default for
        // (lib/github-apps.sh). It runs once, here on the driver, before the Bun pin check and
        // lib/app-logins.ts, and what it prints is the key the block's `cat` then reads.
        // `printf` is the shell's own builtin, so it answers under this run's stand-in PATH.
        LEGION_E2E_REVIEW_APP_KEY_COMMAND: `printf %s '${keyCommandPrints}'`,
      },
      stdout: "pipe",
      stderr: "pipe",
      timeout: 60_000,
    });
    const output = run.stdout.toString() + run.stderr.toString();
    // read_app_logins passed, so every assertion below is about a run that resolved both App
    // credentials rather than one a stand-in stopped before them.
    expect(output).toContain(
      "the implement App acts on GitHub as example-implement[bot] and the review App as " +
        "example-review[bot]"
    );
    expect(output).not.toMatch(/^FAIL prerequisites: secrets\b/m);
    // Each credential as the block hands it to the daemon: the review App's key is what the
    // operator's command printed, through the run's own key file, and the implement App's token
    // is the broker client's release. A prerequisites that reached either through the box's own
    // store would leave another value here and a line in `asked`.
    expect(readFileSync(keyRead, "utf8")).toBe(keyCommandPrints);
    expect(readFileSync(tokenRead, "utf8")).toBe("{}");
    expect(existsSync(asked) ? readFileSync(asked, "utf8") : "").toBe("");
  },
  90_000
);
