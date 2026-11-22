const MiB = 1024 * 1024;

/**
 * The bytes each suite below asks the memory root to have free before its scratch directory goes
 * there (`memoryRoot({ need })`, @legion/test-fixtures/temporary-directories): twice what the
 * suite's scratch tree held when its file's `afterAll` removed it, rounded up to a whole MiB, so a
 * second run of the same suite on the host fits too. A tmpfs with less free gives `tmpdir()`, the
 * scratch disk, where a nearly full one would fail the suite's writes with ENOSPC: a Legion pod's
 * `/dev/shm` is 64 MiB, and every role in the pod and the browser suites' Chromium share it.
 *
 * Measured on /dev/shm at 91714391ae, by a preload that walked each tree before its removal and
 * added up the bytes allocated to every entry (st_blocks x 512). The tree at the `afterAll` is the
 * most each suite holds at once: none of them removes anything of it before then.
 */
export const scratchNeed = {
  /** multi-repo-acceptance-harness.ts's `useScratchDirectory`, for the most of its three files:
   *  multi-repo-second-owner's 1,716 entries, 3,747,840 bytes. */
  multiRepo: 8 * MiB,
  /** stage4b-session-text.test.ts: 870 entries, 1,773,568 bytes. */
  stage4bSessionText: 4 * MiB,
  /** run-processes.test.ts: 897 entries, 65,536 bytes (proc roots of links and directories). */
  runProcesses: 1 * MiB,
  /** claude-backend-guards.test.ts: 31 entries, 3,043,328 bytes (copies of bash and sleep). */
  claudeBackendGuards: 6 * MiB,
  /** controller-start-guards.test.ts: 6 entries, 1,523,712 bytes (copies of bash and sleep). */
  controllerStartGuards: 3 * MiB,
} as const;
