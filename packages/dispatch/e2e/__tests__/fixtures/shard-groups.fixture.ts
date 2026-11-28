/** The rows of `shard-groups.config.ts`: eight rows Playwright may run anywhere, three in a
 *  `serial` `describe` and three in a `default`-mode one, which Playwright runs in order in one
 *  worker, and two `serial` `describe`s of one title with four rows each, which Playwright allows
 *  since their rows' titles differ and which the placement's key cannot tell apart. The rows do
 *  nothing: the config is only listed. */
import { test } from "@playwright/test";

for (const row of [1, 2, 3, 4, 5, 6, 7, 8]) {
  test(`free row ${row}`, () => {});
}

test.describe("serial rows", () => {
  test.describe.configure({ mode: "serial" });
  for (const row of [1, 2, 3]) {
    test(`serial row ${row}`, () => {});
  }
});

test.describe("default-mode rows", () => {
  test.describe.configure({ mode: "default" });
  for (const row of [1, 2, 3]) {
    test(`default-mode row ${row}`, () => {});
  }
});

for (const twin of ["first", "second"]) {
  test.describe("twin rows", () => {
    test.describe.configure({ mode: "serial" });
    for (const row of [1, 2, 3, 4]) {
      test(`${twin} twin row ${row}`, () => {});
    }
  });
}
