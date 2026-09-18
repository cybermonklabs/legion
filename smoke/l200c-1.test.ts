import { expect, test } from "bun:test";
import { kindSmokeValue } from "./l200c-1";

test("kindSmokeValue returns the l200c smoke string", () => {
  expect(kindSmokeValue()).toBe("kind smoke l200c 1");
});
