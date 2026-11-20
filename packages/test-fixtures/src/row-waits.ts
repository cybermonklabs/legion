/**
 * The limit of a row of a test file that starts processes and waits for them, read from what the
 * row waits for, and the row's count of those waits as it makes them.
 *
 * A row that states no limit runs under Bun's own 5 s, or under its lane's `--timeout`: a guess,
 * which the next load runs past. On a shared Linux machine on 2026-11-19, under a load of 618 to
 * 703, the `--cli` row of `scripts/e2e/lib/dispatch-keyed-comment.test.ts` ran past Bun's 5 s in 4
 * of 20 runs, for no defect of the check or of the command it runs: the row starts the check, which
 * starts the `dispatch` command, which asks again after a lost answer. So such a row states
 * `rowMs` of its waits: each wait named, with the bound of the code that ends it, read from that
 * code's own constant (`Wait`), and room above them for the row's own work (`rowRoomMs`). And it
 * counts each wait as it makes it (`rowOf`, `Row.charge`): a wait its limit does not count fails
 * the row there, naming that wait, whatever the load, so a limit below what the row waits for is
 * a red run on every machine and not only on a loaded one.
 *
 * `scripts/e2e/lib/row-waits.test.ts` reads the source of the files whose rows start processes: a
 * row or a hook there that starts one and states no limit is refused, and so is a row that counts
 * `rowOf(<waits>)` and states anything but `rowMs(<waits>)`, or that states `rowMs` of waits it
 * does not count.
 */

/** One thing a row waits for: what it is, as the row's failure names it, and how long it may take,
 *  in ms: the bound of the code that ends it, read from that code's own constant, or `commandMs`
 *  for a process nothing else bounds. */
export interface Wait {
  readonly what: string;
  readonly ms: number;
}

/** How long a row gives one process it starts and waits for, which starts nothing that waits
 *  itself: a help, a refusal, a probe, a script against stand-ins, its start included. It is the
 *  `short` bound of `packages/dispatch/e2e/__tests__/bounded-subprocess.ts`, a bound of the same
 *  kind, whose header holds the reading behind it: at least three times the 11.95 s of one
 *  `playwright test --help` under nine busy loops on 3 CPUs. The slowest row of
 *  `scripts/e2e/lib/dispatch-keyed-comment.test.ts` that starts one such process took 1.43 s, in
 *  60 runs on a shared Linux machine under loads of 640 to 2,535, on 2026-11-19. */
export const commandMs = 60_000;

/** A process `what` of the kind `commandMs` bounds. */
export function command(what: string): Wait {
  return { what, ms: commandMs };
}

/** What a row's limit leaves above its waits: the row's own work between them, its stand-ins'
 *  answers, its reads and its checks, each of which takes milliseconds; and the moment between
 *  the end of a wait and the row reading it, so that a wait's own failure ends the row, and not
 *  the row's clock. It is the room the rows of `packages/dispatch/e2e/__tests__` get above the
 *  children they wait for (`rowLimit` of `bounded-subprocess.ts`). */
export const rowRoomMs = 30_000;

/** A row's limit: its waits, one after another, and `rowRoomMs` above them. */
export function rowMs(waits: readonly Wait[]): number {
  return waits.reduce((sum, wait) => sum + wait.ms, rowRoomMs);
}

/** What a row has waited for so far, against the waits its limit counts (`rowOf`). */
export interface Row {
  /** Counts `wait`, which the row is about to make, against the waits of its limit: one of the
   *  same kind (`Wait.what`) whose bound reaches `wait`'s, which no earlier wait of the row used.
   *  Where none is left it throws, naming `wait`: the row's limit is below what it waits for. */
  charge(wait: Wait): void;
}

/** The count of a row whose limit is `rowMs(waits)`. */
export function rowOf(waits: readonly Wait[]): Row {
  const left = [...waits];
  return {
    charge(wait) {
      const fits = left.findIndex((each) => each.what === wait.what && each.ms >= wait.ms);
      if (fits === -1) {
        const counted = waits.filter((each) => each.what === wait.what).length;
        throw new Error(
          `the row waits for ${wait.what}, for up to ${wait.ms / 1000} s, and its limit counts ${counted === 0 ? "none" : `${counted}, each used by an earlier wait or shorter`}: its limit is below what it waits for, so state rowMs of every wait the row makes`
        );
      }
      left.splice(fits, 1);
    },
  };
}
