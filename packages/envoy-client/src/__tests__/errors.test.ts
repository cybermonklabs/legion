import { describe, expect, test } from "bun:test";
import { DispatchGatewayError, DispatchServiceError } from "../dispatch-http";
import { messageFor, withoutUserinfo } from "../errors";

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

// A configured Dispatch or Envoy URL may carry a user and a password, and a user alone can be a
// token, so text that names the URL drops both and keeps the rest as it was written.
describe("withoutUserinfo", () => {
  const user = "user-7f2a";
  const secret = "pw-4c7e19";

  test.each([
    [`https://${user}:${secret}@host.example`, "https://host.example"],
    [`https://${user}:${secret}@host.example/`, "https://host.example/"],
    [
      `http://${user}:${secret}@host.example:8766/api/v1/issues?project=DSP`,
      "http://host.example:8766/api/v1/issues?project=DSP",
    ],
    [`https://${user}@host.example/dispatch`, "https://host.example/dispatch"],
    [`https://:${secret}@host.example`, "https://host.example"],
  ])("%p is named as %p", (url, shown) => {
    expect(withoutUserinfo(url)).toBe(shown);
  });

  test("a URL with no user or password comes back as written", () => {
    for (const url of ["http://dispatch.test", "https://host.example/a@b", "HTTP://Host.Example"]) {
      expect(withoutUserinfo(url)).toBe(url);
    }
  });

  test("a string the URL parser refuses keeps only what follows its last @", () => {
    expect(withoutUserinfo(`https://${user}:${secret}@host.example:99999`)).toBe(
      "host.example:99999"
    );
    expect(withoutUserinfo("not a url")).toBe("not a url");
  });
});
