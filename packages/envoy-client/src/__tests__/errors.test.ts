import { describe, expect, test } from "bun:test";
import { DispatchGatewayError, DispatchServiceError } from "../dispatch-http";
import { messageFor } from "../errors";

describe("messageFor", () => {
  test("returns the message from Error values", () => {
    expect(messageFor(new Error("network unavailable"))).toBe("network unavailable");
  });

  test("coerces non-Error thrown values for diagnostic output", () => {
    expect(messageFor("plain failure")).toBe("plain failure");
    expect(messageFor(401)).toBe("401");
    expect(messageFor(null)).toBe("null");
  });

  // The tree of dispatch://INFRA-1523 read three refusals through curl because the command printed
  // no code, while skill://dispatch has every agent branch on the code. A Node errno error carries
  // a `code` of its own and is none of Dispatch's business, which is why `fromDispatch` decides.
  test("a refusal Dispatch decided prints its code before its message", () => {
    expect(
      messageFor(new DispatchServiceError("NOT_AUTHOR", 403, "only the author may edit"))
    ).toBe("NOT_AUTHOR: only the author may edit");
    expect(messageFor(new DispatchGatewayError(502, "answered 502", "ask again"))).toBe(
      "HTTP_502: answered 502, so ask again."
    );
  });

  test("an error carrying no Dispatch code prints as it always did", () => {
    const errno = Object.assign(new Error("no such file"), { code: "ENOENT" });
    expect(messageFor(errno)).toBe("no such file");
  });

  test("a message that already opens with its code is not prefixed twice", () => {
    const refusal = new DispatchServiceError(
      "ISSUE_CLOSED",
      409,
      "ISSUE_CLOSED: the issue is closed"
    );
    expect(messageFor(refusal)).toBe("ISSUE_CLOSED: the issue is closed");
  });
});
