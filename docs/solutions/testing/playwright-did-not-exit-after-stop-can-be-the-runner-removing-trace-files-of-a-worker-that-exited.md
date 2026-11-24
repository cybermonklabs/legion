---
title: "Playwright's \"did not exit after stop\" can be the runner removing trace files of a worker that exited"
category: testing
tags:
  - playwright
  - patchedDependencies
  - libuv-threadpool
  - xfs
date: 2026-11-22
status: active
module: packages/dispatch/e2e
---

# Playwright's "did not exit after stop" can be the runner removing trace files of a worker that exited

- `Error: worker-<n> process did not exit within 300000ms after stop, force-killed it` does not
  prove the worker was alive. In Playwright 1.64.1 the runner sends the 'exit' a worker's stop
  waits for only after `WorkerHost.onExit` has removed the worker's artifacts directory
  (`playwright/lib/runner/index.js`, the process 'exit' handler at 1934-1938 and `onExit` at
  5665-5666), and the stop times that wait by heartbeats, which end when the worker exits
  (2019-2046).
- With `trace: "retain-on-first-failure"` every row records a trace, and its files stay in
  `<outputDir>/.playwright-artifacts-<n>/traces` until the worker exits: 4,744 to 5,963 entries,
  127 to 205 MB, after a hundred rows. Four workers that stop together at a project switch leave
  about 22,000 files for `fs.promises.rm`, which queues every entry on the runner's four libuv
  threads at once.
- On a disk where unlinks wait on a lock, that removal took 316 s, the next worker's `mkdir` waited
  behind it, and the run printed the line for workers that had exited in under a second.
  `patches/playwright@1.64.1.patch` sends 'exit' when the process exits, removes the directory in
  the background one call at a time, and stops applying the deadline once the process has exited;
  `e2e/__tests__/playwright-worker-exit.test.ts` holds both and the kill of a worker that really
  does not exit.
- To tell the two apart, log in the runner (a `NODE_OPTIONS=--require` preload) when it sends
  `__stop__` and when the child process exits, and in the worker when it calls `process.reallyExit`.
  An exited worker with no next fork means the runner, not the worker, is waiting.
- Bun ignores a `patchedDependencies` entry for a version that is not installed. Moving
  `@playwright/test` to another Playwright means redoing the patch, or dropping it once upstream
  ships the fix.

## Evidence

- 2026-11-22, shard 3 of 4, local lane, 4 workers, box load about 800: 2 of 10 runs (one at main
  93541fc13f, one at 32ee3e060b) printed the line for three workers each, at the same minute. A
  preload timeline showed each of those workers' `reallyExit` 0.1 to 1.1 s after its `__stop__`,
  and the runner forked the next worker 316 s later.
- A dump of the runner 13 s after four workers stopped: 9,262 file requests in flight, and all four
  libuv threads in D state in `unlink` of `.playwright-artifacts-6/traces/screencast/*.jpeg`,
  kernel stack `xfs_buf_lock`, `xfs_read_agi`, `xfs_iunlink`, `xfs_remove`.
