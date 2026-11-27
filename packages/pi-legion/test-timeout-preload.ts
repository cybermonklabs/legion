import { setDefaultTimeout } from "bun:test";
import { declareGuard, isParallelWorker } from "@legion/test-fixtures/deadline";

// This package's per-test guard, for its `bun run test` (`bun test --parallel=4`). Bun 1.4.4 reads
// no [test] timeout from bunfig.toml, so the guard comes from here, not a --timeout flag. It is set
// only inside a --parallel worker, because a non-parallel `bun test` runs every file in one process
// where setDefaultTimeout reaches only the first file (Bun resets the default to 5000 ms between
// files); declaring there would raise the shared wait's deadline for every file while Bun kills the
// later ones at 5000 ms. Under --parallel each file is its own worker and reloads this preload, so
// the guard reaches them all. A plain `bun test` (or a --parallel run of a single file) keeps Bun's
// 5000 ms default, as on main. setDefaultTimeout arms Bun's per-test kill; declareGuard gives the
// shared wait (@legion/test-fixtures/wait) the same length, winning over the command line.
const GUARD_MS = 300_000;
if (isParallelWorker()) {
  setDefaultTimeout(GUARD_MS);
  declareGuard(GUARD_MS);
}
