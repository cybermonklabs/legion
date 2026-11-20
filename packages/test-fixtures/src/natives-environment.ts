/**
 * The number of worker threads each thread pool of `@oh-my-pi/pi-natives` gets in a process a test
 * starts, where the caller states none: one. The processes these tests start run a handful of native
 * calls, which one worker serves.
 */
export const nativesWorkerThreads = "1";

/**
 * The environment for a process a test starts that loads `@oh-my-pi/pi-natives`: a Bun process
 * whose modules import `@oh-my-pi/pi-utils` or `@oh-my-pi/pi-tui` (every extension of pi-envoy,
 * pi-legion and pi-shared does), or the `omp` binary, which embeds the addon. It is `environment`
 * with `TOKIO_WORKER_THREADS` and `RAYON_NUM_THREADS` each set to {@link nativesWorkerThreads},
 * unless `environment` already holds a value for it, which it keeps.
 *
 * The addon's Tokio runtime starts a worker for each CPU the machine has as it loads, unless
 * `TOKIO_WORKER_THREADS` says how many; its Rayon pool does the same at its first parallel call
 * (`countTokens` of an array, among others), unless `RAYON_NUM_THREADS` says how many. On a shared
 * Linux machine of 288 CPUs, on 2026-11-19, a Bun process that imported `@oh-my-pi/pi-utils` held
 * 288 `tokio-rt-worker` threads (1 with the variable set to 1, none without the import), and so
 * did `omp config get`; a call of `countTokens` of an array added 288 more (1 with
 * `RAYON_NUM_THREADS` set to 1). Under a limit of 512 tasks, a second such process could not
 * start its threads: the row that loads pi-envoy's entry in a child, beside its own test process,
 * saw the child exit 1 having printed nothing.
 *
 * `environment` is not changed; the returned object is a new one.
 */
export function nativesEnvironment(
  environment: Record<string, string | undefined>
): Record<string, string | undefined> {
  return {
    ...environment,
    TOKIO_WORKER_THREADS: environment["TOKIO_WORKER_THREADS"] ?? nativesWorkerThreads,
    RAYON_NUM_THREADS: environment["RAYON_NUM_THREADS"] ?? nativesWorkerThreads,
  };
}
