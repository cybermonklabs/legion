/** Whether a process with id `pid` runs now: signal 0 reaches it, or reaches it and is refused
 *  (`EPERM`, a process of another user). It imports nothing, so the Codex tool hook's bundle, which
 *  asks it of the hooks that took messages before it (`../src/codex/codex-inbox`), stays one that
 *  needs nothing but the filesystem. */
export function processExists(pid: number): boolean {
  try {
    process.kill(pid, 0)
    return true
  } catch (error) {
    return (error as NodeJS.ErrnoException).code === "EPERM"
  }
}
