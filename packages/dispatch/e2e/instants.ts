import { expect, type Locator } from "@playwright/test";

/** `ms` brought down to a millisecond that ends in zero. Go drops that zero when it spells the
 *  time (`…:32.15Z` for 150 ms), where `Date.toISOString` always writes three digits, so a moment
 *  the server writes again is spelled two ways on every run, and not only one run in ten. A row
 *  that rounds its moment so compares it with expectInstant. */
export function endsInZeroMs(ms: number): number {
  return Math.floor(ms / 10) * 10;
}

/** Asserts that the `<time>` element at `locator` names the moment `want` in its `datetime`. It
 *  compares the moment and not the string: the server writes the moment as Go spells it, which
 *  drops a trailing zero of the milliseconds (`…:32.15Z` for 150 ms), where `Date.toISOString`
 *  always writes three digits, so a comparison of the strings fails whenever the milliseconds end
 *  in zero. */
export async function expectInstant(locator: Locator, want: number, what: string) {
  await expect(locator).toBeVisible();
  await expect
    .poll(async () => Date.parse((await locator.getAttribute("datetime")) ?? ""), {
      message: `${what} names the moment ${new Date(want).toISOString()}`,
    })
    .toBe(want);
}
