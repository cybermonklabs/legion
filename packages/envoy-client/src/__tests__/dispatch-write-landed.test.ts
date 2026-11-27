import { describe, expect, test } from "bun:test";
import { writeMayHaveLanded } from "../dispatch-commands/failures";
import { DispatchGatewayError, DispatchServiceError } from "../dispatch-http";

// Whether a failed write may have taken effect follows the one rule for a status after which the
// request may have reached the server (`mayHaveReachedServer`): a 5xx or a 408 leaves it possibly
// applied, whether Dispatch or a gateway in its place answered, and any other status of a refusal
// proves it was not.
describe("writeMayHaveLanded", () => {
  test("a 5xx or a 408 in Dispatch's own shape may have landed, and its other refusals did not", () => {
    for (const [status, landed] of [
      [408, true],
      [500, true],
      [503, true],
      [400, false],
      [404, false],
      [409, false],
      [429, false],
    ] as const) {
      const refusal = new DispatchServiceError(`HTTP_${status}`, status, `refused ${status}`);
      expect({ status, landed: writeMayHaveLanded(refusal) }).toEqual({ status, landed });
    }
  });

  test("a gateway's answer may have landed after the same statuses", () => {
    for (const [status, landed] of [
      [408, true],
      [502, true],
      [429, false],
      [404, false],
    ] as const) {
      const page = new DispatchGatewayError(status, `answered ${status}`, "a retry may succeed");
      expect({ status, landed: writeMayHaveLanded(page) }).toEqual({ status, landed });
    }
  });

  test("a failure with no status may have landed", () => {
    expect(writeMayHaveLanded(new Error("The operation timed out."))).toBe(true);
  });
});
