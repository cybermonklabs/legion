import { lstat } from "node:fs/promises";

/** The ways a git or jj failure names the lock file it could not take: jj's git backend
 * (gitoxide), and git's own lockfile API, which every git jj runs reports through. */
const GIT_LOCK_REPORTS = [
  /The lockfile at '([^']+)' might need manual deletion/g,
  /Unable to create '([^']+)': File exists/g,
];

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
 * held to by `git-lock.test.ts`, which reads that source. Empty when `output` names no lock, or
 * each one it names is already gone.
 */
export async function staleGitLockNote(output: string, now = Date.now()): Promise<string> {
  const locks = new Set<string>();
  for (const report of GIT_LOCK_REPORTS) {
    for (const match of output.matchAll(report)) locks.add(match[1] as string);
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
