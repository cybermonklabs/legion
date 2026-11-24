import { lstat } from "node:fs/promises";

/** The ways a git or jj failure names the lock file it could not take: jj's git backend
 * (gitoxide), and git's own lockfile API, which every git jj runs reports through. Both print the
 * path between single quotes without escaping it, so a path may hold a quote of its own. Each
 * report is the fixed text before the path and the fixed text after it: the path runs from the one
 * up to the first of the other on that line, at least one character long, as the daemon's
 * gitLockReports (`'(.+?)'`) read it. They are read with indexOf, not a pattern: a lazy pattern
 * here costs time quadratic in a line that holds many openings and no fixed tail. A path stays on
 * its line, which is a trade: `'([^']+)'`, which this replaced, matched a newline, so a lock whose
 * path holds a newline got its note then and gets none now. */
type LockReport = readonly [opening: string, closing: string];
const GIT_LOCK_REPORTS: readonly LockReport[] = [
  ["The lockfile at '", "' might need manual deletion"],
  ["Unable to create '", "': File exists"],
];

/** Each path `report` names on `line`, in order: from each opening to the first closing at least
 * one character after it. A line with no closing after its first opening has none after any later
 * opening either, so the read ends there. */
function lockPaths(line: string, report: LockReport): string[] {
  const [opening, closing] = report;
  const paths: string[] = [];
  for (let from = 0; ; ) {
    const start = line.indexOf(opening, from);
    if (start < 0) return paths;
    const end = line.indexOf(closing, start + opening.length + 1);
    if (end < 0) return paths;
    paths.push(line.slice(start + opening.length, end));
    from = end + closing.length;
  }
}

/** `ms` as Go prints a time.Duration rounded to the second: `31m4s`, `2h0m5s`, `45s`. */
function age(ms: number): string {
  const total = Math.round(Math.abs(ms) / 1000);
  const sign = ms < 0 && total > 0 ? "-" : "";
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  if (hours > 0) return `${sign}${hours}h${minutes}m${seconds}s`;
  if (minutes > 0) return `${sign}${minutes}m${seconds}s`;
  return `${sign}${seconds}s`;
}

/**
 * One line for each git lock file `output` names that is still on disk: its path, how long ago it
 * was last written, and what becomes of it. A git or jj interrupted mid-write leaves its lock, and
 * every later write of what it locks fails on it. In an issue pod the launcher of the roles
 * container removes such a lock once no process of the container holds it open and it is more than
 * 15 minutes old, and looks again every minute, so the role's answer is to run its command again,
 * never to remove the file, which another role's live git may hold. Under tmux nothing removes it,
 * and the last sentence is what stands where a pod's image predates that sweep, since this plugin
 * can be newer than the launcher the pod runs. The daemon words the same note for its own jj calls
 * (`StaleGitLockNote`, packages/daemon/internal/workspace/jjgit.go), whose wording this text is
 * held to by `git-lock.test.ts`, which reads the data file that note is rendered from
 * (packages/daemon/internal/workspace/stale-git-lock-note.txt). Empty when `output` names no
 * lock, or each one it names is already gone.
 */
export async function staleGitLockNote(output: string, now = Date.now()): Promise<string> {
  const locks = new Set<string>();
  const lines = output.split("\n");
  for (const report of GIT_LOCK_REPORTS) {
    for (const line of lines) for (const lock of lockPaths(line, report)) locks.add(lock);
  }
  const notes: string[] = [];
  for (const lock of locks) {
    const written = await lstat(lock).then(
      (info) => info.mtimeMs,
      () => undefined
    );
    if (written === undefined) continue;
    notes.push(
      `git lock ${lock} was last written ${age(now - written)} ago and blocks this command. The launcher of the issue's pod removes a git lock that no process of the pod holds open and that is more than 15 minutes old, and it looks again every minute. Under the tmux runtime nothing removes it: remove it by hand only once no git or jj process on the host is using the repository. Run the command again in about 16 minutes. If the lock is still there after that time, tell the issue's architect.`
    );
  }
  return notes.join("\n");
}
