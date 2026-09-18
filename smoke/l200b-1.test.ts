import { expect, test } from "bun:test";
import { kindSmokeValue } from "./l200b-1";

test("kindSmokeValue returns the fixed smoke string", () => {
  expect(kindSmokeValue()).toBe("kind smoke l200b 1");
});
