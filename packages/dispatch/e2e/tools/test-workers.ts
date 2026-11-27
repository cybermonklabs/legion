/** `bun e2e/tools/test-workers.ts`: prints the worker count that package.json's `test:web` and
 *  `test:harness` pass to Bun as `--parallel=$(bun e2e/tools/test-workers.ts)`. That is one
 *  worker per CPU that `availableParallelism()` reports, up to eight. It reads the count through
 *  Bun's `node:os` and not `nproc`, so a machine without GNU coreutils reads it too.
 *
 *  Why eight at most: a bare `--parallel` starts one worker per CPU, and on a 180-CPU machine it
 *  failed every file of `test:web` (README.md, the paragraph that starts "The Bun test scripts
 *  that run a package's files side by side"). Why fewer below eight CPUs: a Legion issue pod has
 *  2 CPUs and shares its memory with every agent in it, and there eight workers saved at most
 *  22 s over two and held up to 2,681 MiB more. Measured at fef6c148 in a gVisor Job of 2 CPUs
 *  (2026-11-26; the memory is the peak of MemTotal - MemAvailable, sampled each second), each
 *  run with 0 failures:
 *
 *    | script                       | 8 workers        | 2 workers        |
 *    | ---------------------------- | ---------------- | ---------------- |
 *    | `test:web web/src`           | 439 s, 4,774 MiB | 438 s, 2,093 MiB |
 *    | `test:harness e2e/__tests__` | 520 s, 3,985 MiB | 542 s, 2,715 MiB |
 *
 *  A Job of 2 CPUs and 6 GiB ran out of memory in its first `bun run test` under eight workers.
 *  On CI's 4-CPU runners the count is four, which costs `test:harness` about 35 s against eight
 *  (README.md). */
import { availableParallelism } from "node:os";

console.log(Math.min(8, availableParallelism()));
