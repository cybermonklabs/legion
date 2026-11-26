import { afterEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { mkdir, mkdtemp, rm, utimes, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { staleGitLockNote } from "./git-lock";

const scratch: string[] = [];
afterEach(async () => {
  for (const directory of scratch.splice(0)) await rm(directory, { recursive: true, force: true });
});

const NOW = Date.parse("2026-10-13T21:53:36Z");

/** How long ago most locks here were last written, and how the note prints it: a Go duration
 * rounded to the second, as gitlocknote_test.go's noteAge and ageWords. */
const NOTE_AGE_MS = (31 * 60 + 4) * 1000;
const AGE_WORDS = "31m4s";

/** A git lock file at `name` (which may name directories under it) in a scratch directory, last
 * written `ageMs` before NOW. */
async function staleLock(name: string, ageMs: number): Promise<string> {
  const dir = await mkdtemp(path.join(tmpdir(), "legion-git-lock-"));
  scratch.push(dir);
  const lock = path.join(dir, name);
  await mkdir(path.dirname(lock), { recursive: true });
  await writeFile(lock, "");
  const written = new Date(NOW - ageMs);
  await utimes(lock, written, written);
  return lock;
}

/** How git's own lockfile API names a lock it could not take, and how jj's git backend does: the
 * two the note is read out of, as the daemon's gitlocknote_test.go words them. */
function gitLockFailure(lock: string): string {
  return `error: cannot lock ref 'refs/heads/main': Unable to create '${lock}': File exists.`;
}

function jjLockFailure(lock: string): string {
  return `2: The lock for resource '${lock.slice(0, -5)}' could not be obtained after 1.00s after 16 attempt(s). The lockfile at '${lock}' might need manual deletion.`;
}

/** The note for one lock NOTE_AGE_MS old, which most rows here read. */
async function oneNote(): Promise<{ lock: string; text: string }> {
  const lock = await staleLock("index.lock", NOTE_AGE_MS);
  return {
    lock,
    text: await staleGitLockNote(`fatal: Unable to create '${lock}': File exists.`, NOW),
  };
}

test("names each git lock the output reports that is still there, with how long ago it was written", async () => {
  const index = await staleLock("index.lock", NOTE_AGE_MS);
  const ref = await staleLock("main.lock", (2 * 3600 + 5) * 1000);
  const output = [
    "Error: Failed to reset Git HEAD state",
    jjLockFailure(index),
    gitLockFailure(ref),
  ].join("\n");
  expect(await staleGitLockNote(output, NOW)).toBe(
    `${daemonNote(index, AGE_WORDS)}\n${daemonNote(ref, "2h0m5s")}`
  );
});

test("says nothing of output that names no lock, or names one already gone", async () => {
  const gone = path.join(tmpdir(), "legion-git-lock-never-made", "index.lock");
  expect(await staleGitLockNote('Error: There is no jj repo in "."', NOW)).toBe("");
  expect(await staleGitLockNote(`fatal: Unable to create '${gone}': File exists.`, NOW)).toBe("");
});

// git and jj print the lock's path between single quotes and do not escape it, so a path holding a
// quote of its own still names its lock. The same two rules as the daemon's StaleGitLockNote, held
// by the same rows as gitlocknote_test.go's: the path ends at the first closing text after its
// opening, so two locks one report names on one line are two paths; and a path stays on its line,
// so an opening cut off at the end of a line takes nothing from the next line's report.
type LockPair = { index: string; ref: string };
const quotedCases: [string, (locks: LockPair) => string, (locks: LockPair) => string[]][] = [
  ["git's report", ({ index }) => gitLockFailure(index), ({ index }) => [index]],
  ["jj's report", ({ index }) => jjLockFailure(index), ({ index }) => [index]],
  [
    "both reports of one lock, two lines",
    ({ index }) => `${jjLockFailure(index)}\n${gitLockFailure(index)}`,
    ({ index }) => [index],
  ],
  [
    "two locks git names on one line",
    ({ index, ref }) => `${gitLockFailure(index)} ${gitLockFailure(ref)}`,
    ({ index, ref }) => [index, ref],
  ],
  [
    "two locks jj names on one line",
    ({ index, ref }) => `${jjLockFailure(index)} ${jjLockFailure(ref)}`,
    ({ index, ref }) => [index, ref],
  ],
  [
    "a cut-off git opening before git's report",
    ({ index }) => `fatal: Unable to create '/cut off\n${gitLockFailure(index)}`,
    ({ index }) => [index],
  ],
  [
    "a cut-off jj opening before jj's report",
    ({ index }) => `Error: The lockfile at '/cut off\n${jjLockFailure(index)}`,
    ({ index }) => [index],
  ],
];
for (const [name, output, named] of quotedCases) {
  test(`names a lock whose path holds a single quote: ${name}`, async () => {
    const locks = {
      index: await staleLock("tmp-sq'x/it's here/index.lock", NOTE_AGE_MS),
      ref: await staleLock("tmp-sq'x/it's here/main.lock", NOTE_AGE_MS),
    };
    expect(await staleGitLockNote(output(locks), NOW)).toBe(
      named(locks)
        .map((lock) => daemonNote(lock, AGE_WORDS))
        .join("\n")
    );
  });
}

/**
 * The daemon's own wording, read out of the data file the two implementations share: one line
 * whose two `%s` are the lock's path and its age, in that order, and the newline that ends it,
 * which the note itself does not carry — the Go side renders the same text with that newline
 * trimmed (`staleGitLockNoteText`, jjgit.go), so the two are equal byte for byte.
 * `packages/contracts/src/dispatch-tools.test.ts` reads a shipped file the same way.
 */
function daemonNote(lock: string, age: string): string {
  const repoRoot = path.resolve(import.meta.dir, "../../..");
  const file = path.join(repoRoot, "packages/daemon/internal/workspace/stale-git-lock-note.txt");
  const text = readFileSync(file, "utf8").replace(/\n$/u, "");
  const parts = text.split("%s");
  if (parts.length !== 3) {
    throw new Error(
      `${file} takes ${parts.length - 1} arguments, want the lock's path and its age`
    );
  }
  return `${parts[0]}${lock}${parts[1]}${age}${parts[2]}`;
}

// One text in two implementations: a role reads whichever of them its command went through, and a
// wording that drifted would tell two roles of one pod two different things. The daemon's is the
// original (`StaleGitLockNote`, packages/daemon/internal/workspace/jjgit.go) and this one follows
// it word for word.
test("is word for word the note the daemon renders for the same lock and age", async () => {
  const { lock, text } = await oneNote();
  expect(text).toBe(daemonNote(lock, AGE_WORDS));
});

test("names the lock and tells the role to run the command again", async () => {
  const { lock, text } = await oneNote();
  expect(
    text.startsWith(`git lock ${lock} was last written ${AGE_WORDS} ago and blocks this command.`)
  ).toBe(true);
  expect(text).toContain("Run the command again in about 16 minutes.");
});

test("names how long the launcher's sweep takes", async () => {
  const { text } = await oneNote();
  expect(text).toContain(
    "The launcher of the issue's pod removes a git lock that no process of the pod holds open and that is more than 15 minutes old, and it looks again every minute."
  );
  expect(text).toContain("Run the command again in about 16 minutes.");
});

// The last sentence is what keeps the note true in a pod whose image predates the launcher's
// sweep: this plugin can be newer than the launcher the pod runs, and there the wait the note
// names passes with the lock still there.
test("ends with telling the issue's architect", async () => {
  const { text } = await oneNote();
  expect(
    text.endsWith("If the lock is still there after that time, tell the issue's architect.")
  ).toBe(true);
});

// Replacing the issue's pod takes its disk and every role in it, and the role is not to keep the
// lock where it is either: its answer is to run the command again.
test("offers no pod replacement and no advice to keep the lock", async () => {
  const { text } = await oneNote();
  for (const forbidden of [
    "replacing the issue's pod",
    "replace the pod",
    "replace the issue's pod",
    "until the pod is replaced",
    "a new pod",
    "do not remove it",
    "leave it where it is",
  ]) {
    expect(text.toLowerCase()).not.toContain(forbidden);
  }
});

// Under tmux there is no pod and no launcher, so this sentence is the whole of what a lock costs a
// host: the rewrite kept it word for word.
test("keeps its tmux sentence", async () => {
  const { text } = await oneNote();
  expect(text).toContain(
    "Under the tmux runtime nothing removes it: remove it by hand only once no git or jj process on the host is using the repository."
  );
});
