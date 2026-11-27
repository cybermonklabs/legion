import { expect } from "@playwright/test";

/** What a clause of the form "none of them is" needs to mean anything: the items it is read
 *  over, what makes one of them the thing that must not be there, and how many items the row
 *  had to measure before the clause says anything at all. */
export interface NoneOf<T> {
  /** The fewest items this clause is worth reading over. A row that measured fewer measured
   *  the wrong thing - a renamed test id, a part not yet mounted, an observer that was never
   *  given anything - and the clause below it is then true of an empty set. */
  readonly amongAtLeast: number;
  /** What must be true of none of them. */
  readonly are: (item: T, index: number) => boolean;
  /** The items, as a plural noun phrase: "diagrams within a viewport height of the reader". */
  readonly measured: string;
  /** What an item that matches is, as a plural noun phrase: "diagrams the margin reached and
   *  left undrawn". It is what both failures name, so a reader of the report learns which
   *  clause bit without opening the file. */
  readonly matched: string;
}

/**
 * None of `items` is the thing `matched` names, and there were at least `amongAtLeast` of them.
 *
 * Every universal clause of a check takes this pairing (dispatch://INFRA-341). "Every near
 * diagram drew" is true of a body with no near diagram; "no box of the chrome is on the
 * reader's screen" is true of a page that drew no chrome; "no main-thread task is over half a
 * second" is true of a reading whose observer collected nothing. In each case the row goes
 * green while measuring nothing of its subject, and goes on going green after the thing it
 * watches has been renamed or has stopped being drawn at all - which is the one failure a
 * suite cannot report, because nothing fails.
 *
 * Writing the count by hand works and was how the rows that needed it were written, but it is
 * two assertions a reader has to see the connection between, and the connection is what gets
 * dropped when a clause is copied into the next row. Here the count cannot be dropped: the
 * clause does not compile without it.
 *
 * Both failures name the count, because the count is what tells a reader which of the two went
 * wrong - a row that measured nothing has a different cause than a row that measured plenty and
 * found one bad.
 */
export function expectNoneOf<T>(items: readonly T[], clause: NoneOf<T>): void {
  const { amongAtLeast, are, matched, measured } = clause;
  if (amongAtLeast < 1) {
    throw new Error(
      `expectNoneOf over ${measured} asks for at least ${amongAtLeast} of them, which is the ` +
        "empty measurement this helper exists to refuse: name the count the clause needs."
    );
  }
  expect(
    items.length,
    `only ${items.length} ${measured} were measured, under the ${amongAtLeast} this clause ` +
      `needs: "no ${matched}" is met by an empty set rather than by the page doing its job`
  ).toBeGreaterThanOrEqual(amongAtLeast);
  const matches = items.filter((item, index) => are(item, index));
  expect(matches, `${matches.length} of ${items.length} ${measured}: ${matched}`).toEqual([]);
}
