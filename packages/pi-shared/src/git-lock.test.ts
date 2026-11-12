import { afterEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { mkdtemp, rm, utimes, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { staleGitLockNote } from "./git-lock";

const scratch: string[] = [];
afterEach(async () => {
  for (const directory of scratch.splice(0)) await rm(directory, { recursive: true, force: true });
});

const NOW = Date.parse("2026-10-13T21:53:36Z");

/** A git lock file at `name` in a scratch directory, last written `ageMs` before NOW. */
async function staleLock(name: string, ageMs: number): Promise<string> {
  const dir = await mkdtemp(path.join(tmpdir(), "legion-git-lock-"));
  scratch.push(dir);
  const lock = path.join(dir, name);
  await writeFile(lock, "");
  const written = new Date(NOW - ageMs);
  await utimes(lock, written, written);
  return lock;
}

const note = (lock: string, age: string) =>
  `git lock ${lock} was last written ${age} ago and blocks this command. The launcher of the issue's pod removes a git lock that no process of the pod holds open and that is more than 15 minutes old, and it looks again every minute. Under the tmux runtime nothing removes it: remove it by hand only once no git or jj process on the host is using the repository. Run the command again in about 16 minutes. If the lock is still there after that time, tell the issue's architect.`;

/** The note for one lock 31m4s old, which most rows here read. */
async function oneNote(): Promise<{ lock: string; text: string }> {
  const lock = await staleLock("index.lock", (31 * 60 + 4) * 1000);
  return {
    lock,
    text: await staleGitLockNote(`fatal: Unable to create '${lock}': File exists.`, NOW),
  };
}

test("names each git lock the output reports that is still there, with how long ago it was written", async () => {
  const index = await staleLock("index.lock", (31 * 60 + 4) * 1000);
  const ref = await staleLock("main.lock", (2 * 3600 + 5) * 1000);
  const output = [
    "Error: Failed to reset Git HEAD state",
    `2: The lock for resource '${index.slice(0, -5)}' could not be obtained after 1.00s after 16 attempt(s). The lockfile at '${index}' might need manual deletion.`,
    `error: cannot lock ref 'refs/heads/main': Unable to create '${ref}': File exists.`,
  ].join("\n");
  expect(await staleGitLockNote(output, NOW)).toBe(
    `${note(index, "31m4s")}\n${note(ref, "2h0m5s")}`
  );
});

test("says nothing of output that names no lock, or names one already gone", async () => {
  const gone = path.join(tmpdir(), "legion-git-lock-never-made", "index.lock");
  expect(await staleGitLockNote('Error: There is no jj repo in "."', NOW)).toBe("");
  expect(await staleGitLockNote(`fatal: Unable to create '${gone}': File exists.`, NOW)).toBe("");
});

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
  expect(text).toBe(daemonNote(lock, "31m4s"));
});

test("names the lock and tells the role to run the command again", async () => {
  const { lock, text } = await oneNote();
  expect(
    text.startsWith(`git lock ${lock} was last written 31m4s ago and blocks this command.`)
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
