// The client reads whether the broker answered by the workspace's one statement of the outage set,
// `outage` of `@legion/contracts/http-status`: `refusalCode` (oauth-refusal.ts) reads an OAuth
// error only in a 4xx that is no outage, and a broker refusal at an outage is never final
// (`BrokerRefusedError.permanent`, token-errors.ts). The first two rows hold each reader to that
// statement at every status a fetch Response can carry, so a copy of the set written into either
// file again, which can drift from it, fails a row. The last row pins what the set is here.

import { expect, test } from "bun:test";
import { outage } from "@legion/contracts/http-status";
import { ResponseBodyError } from "openid-client";
import { refusalCode } from "../oauth-refusal";
import { BrokerRefusedError } from "../token-errors";

/** Every status a fetch Response can carry: 200 to 599. */
const STATUSES = Array.from({ length: 400 }, (_, index) => 200 + index);

/** The statuses at which `refusalCode` reads the OAuth error a token endpoint's answer names, as
 *  the library throws that answer. */
const CODE_READ = STATUSES.filter(
  (status) =>
    refusalCode(
      new ResponseBodyError("the token endpoint refused the request", {
        cause: { error: "access_denied" },
        response: new Response(null, { status }),
      })
    ) !== ""
);

test("refusalCode reads an OAuth error only in a 4xx that is no outage, at every status", () => {
  expect(CODE_READ).toEqual(
    STATUSES.filter((status) => status >= 400 && status < 500 && !outage(status))
  );
});

test("a broker refusal is final at every status that is no outage, and at no outage", () => {
  const final = STATUSES.filter(
    (status) =>
      new BrokerRefusedError(status, "invalid_request", "", "this machine's identity").permanent
  );
  expect(final).toEqual(STATUSES.filter((status) => !outage(status)));
});

test("the 4xx that name no refusal are the 408 and the 429, and no 5xx names one", () => {
  const fourHundreds = STATUSES.filter((status) => status >= 400 && status < 500);
  expect(fourHundreds.filter((status) => !CODE_READ.includes(status))).toEqual([408, 429]);
  expect(CODE_READ.filter((status) => status >= 500)).toEqual([]);
});
