// packages/envoy-client/src/__tests__/excerpt.test.ts
//
// The tokens a client presented lately (`SentTokens`, excerpt.ts), which every excerpt of an
// answer redacts beside the credential's secret now.

import { expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { EXCERPT_SCAN_LIMIT, excerpt, SentTokens } from "../excerpt";

// F2 (the review of be17859eca): the token the credential holds now is one of the tokens it
// presented, so it is scanned once, not once as `secret()` and again as a kept token.
test("the token held now and the same token presented are one token to redact", () => {
  const live = fixtureSecret("live");
  const sent = new SentTokens();
  sent.note({ Authorization: `DPoP ${live}`, DPoP: "proof" });
  expect(sent.with(live)).toEqual([live]);
  expect(sent.with("")).toEqual([live]);
});

// The newest first, each once, and at most four besides the one held now.
test("a client keeps the tokens it presented, newest first, each once, at most four", () => {
  const tokens = Array.from({ length: 6 }, (_, index) => fixtureSecret(`t${index}`));
  const sent = new SentTokens();
  for (const token of tokens) sent.note({ Authorization: `Bearer ${token}` });
  sent.note({ authorization: `Bearer ${tokens[4]}` });
  const current = fixtureSecret("current");
  expect(sent.with(current)).toEqual([current, tokens[4], tokens[5], tokens[3], tokens[2]]);
});

// Every kept token is redacted from an answer, the one held now included.
test("an excerpt redacts every token a client presented and the one it holds now", () => {
  const old = fixtureSecret("old");
  const live = fixtureSecret("live");
  const sent = new SentTokens();
  sent.note({ Authorization: `DPoP ${old}` });
  const shown = excerpt(`echo ${old} and ${live}`, sent.with(live));
  expect(shown).toBe("echo [redacted] and [redacted]");
});

// h1 (the review of 2fa6fbf7cf): a gateway that echoes the request echoes its `DPoP` header too,
// the proof the credential signed for that request, which is none of the tokens a client keeps.
// It is redacted as an echoed `Authorization` header is: as a header line and as a JSON member,
// in any case, the header's name kept.
test.each([
  ["a header line", (proof: string) => `DPoP: ${proof}`, "DPoP: [redacted]"],
  ["a header line in lower case", (proof: string) => `dpop:${proof}`, "dpop:[redacted]"],
  ["a JSON member", (proof: string) => `{"Dpop":"${proof}"}`, '{"Dpop":"[redacted]"}'],
  [
    "a spaced JSON member",
    (proof: string) => `{ "DPOP" : "${proof}" }`,
    '{ "DPOP" : "[redacted]" }',
  ],
  // A gateway that echoes Go's header JSON (`http.Header`) writes each value in a list (the review
  // of c19c0722bf, R2).
  ["a JSON member's list", (proof: string) => `{"Dpop":["${proof}"]}`, '{"Dpop":["[redacted]"]}'],
  [
    "a spaced JSON member's list",
    (proof: string) => `{ "Dpop" : [ "${proof}" ] }`,
    '{ "Dpop" : [ "[redacted]" ] }',
  ],
  [
    "an Authorization member's list",
    (proof: string) => `{"Authorization":["DPoP ${proof}"]}`,
    '{"Authorization":["DPoP [redacted]"]}',
  ],
  // A body that quotes the echo inside a JSON string, or as HTML, writes its quotes escaped; and
  // a list may hold the header twice (the deep review of dfa2ea1f59, H1).
  [
    "a JSON-escaped member's list",
    (proof: string) => `{\\"Dpop\\":[\\"${proof}\\"]}`,
    '{\\"Dpop\\":[\\"[redacted]\\"]}',
  ],
  [
    "an HTML-escaped member's list",
    (proof: string) => `&quot;Dpop&quot;:[&quot;${proof}&quot;]`,
    "&quot;Dpop&quot;:[&quot;[redacted]&quot;]",
  ],
  [
    "the second value of a list",
    (proof: string) => `{"Dpop":["${proof}","${proof}"]}`,
    '{"Dpop":["[redacted]","[redacted]"]}',
  ],
] as const)("an excerpt redacts an echoed DPoP proof as %s", (_as, echo, shown) => {
  const proof = `eyJ0eXAiOiJkcG9wK2p3dCJ9.${fixtureSecret("claims")}.${fixtureSecret("sig")}`;
  const quoted = excerpt(`echo ${echo(proof)} Authorization: DPoP ${fixtureSecret("token")}`, []);
  expect(quoted).toBe(`echo ${shown} Authorization: DPoP [redacted]`);
  expect(quoted).not.toContain(proof.slice(0, 12));
});

// The JWS pattern runs on the host's event loop over up to `EXCERPT_SCAN_LIMIT` characters of
// text a gateway chose, so it starts a match only at the start of a run of base64url characters.
// Started anywhere, it scans a run to its end from each `eyJ` inside it: measured on this
// repository's dev box, 0.53 s for one 64 KiB page of `eyJ-`, against 0.3 ms with the lookbehind.
// Twenty pages put the defect at about 10 s idle, past the row's 5 s limit, while the row itself
// takes milliseconds even on a loaded box: the limit is a bound on work, not a hang guard.
test("the JWS pattern does bounded work on twenty pages that start a JWS at every fourth character", () => {
  for (let page = 0; page < 20; page += 1) {
    const hostile = `${page}${"eyJ-".repeat(EXCERPT_SCAN_LIMIT / 4)}`;
    expect(excerpt(hostile, [])).toStartWith(`${page}`);
  }
}, 5_000);

// The same bound as every pattern of `excerpt`: a proof cut by the first `EXCERPT_SCAN_LIMIT`
// characters is redacted whole, its piece inside the bound included, and nothing past it is read.
test("an echoed DPoP proof cut by the excerpt's scan bound leaves none of it behind", () => {
  const proof = fixtureSecret("proof");
  const cut = `<style>${"s".repeat(EXCERPT_SCAN_LIMIT - 40)}</style>DPoP: ${proof} after the limit`;
  expect(excerpt(cut, [])).toBe("DPoP: [redacted]");
});
