import { jest } from "bun:test";

// Bun's own per-test default is 5,000 ms, and nothing in this package wrote it, sized it or
// explained it. On a busy machine it is a limit on a row's work rather than on a hang: measured in
// a 2-CPU pod under 16 busy loops (8 x nproc), `dispatch-cwd.test.ts`'s row that kills a child
// ignoring SIGTERM reached 5,000.89 ms and failed, where it costs 0.14 s idle and 0.31 s with its
// own file alone, a stretch of 35. Pooled over 43 whole-suite runs and the isolated runs of each
// file, 28 of the 1,541 rows reach a third of that default, so no list of per-row limits closes it.
//
// 30 s is this suite's hang guard, written once. It is three and a half times the 8.78 s of the
// most expensive row that relies on it, the two concurrent calls of `dispatch-askgate.test.ts`,
// and more than ten times every other. A row that hangs still fails the suite; it now takes 30 s
// to say so rather than 5 s. A row whose own claim needs another number gives `test` a third
// argument, which wins over this default in both directions, and says beside it whether that
// number is a hang guard or a bound on work.
//
// `bunfig.toml` names this file as the test preload, so `bun test` and `bun run test` both read it.
jest.setTimeout(30_000);
