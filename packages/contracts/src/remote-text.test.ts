import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import {
  callLibrary,
  LibraryError,
  RemoteText,
  readRemoteText,
  redactRemoteText,
  WORDS_NOT_READABLE,
} from "./remote-text";

// The TypeScript half of one rule: this replays packages/envoy/internal/redact/testdata/remote.json,
// that file itself read by relative path and never a copy, as the Go suite's remote_test.go
// replays it against redact.Remote. The required categories are Go's remoteCategories.
const CORPUS_PATH = new URL("../../envoy/internal/redact/testdata/remote.json", import.meta.url);

const REQUIRED_CATEGORIES = [
  "probe",
  "text-after-query",
  "value-split",
  "fragment",
  "scheme-relative",
  "backslash",
  "quote",
  "space",
  "tab",
  "line-feed",
  "angle",
  "backtick",
  "percent",
  "percent-encoded-url",
  "percent-encoded-twice",
  "mixed-encoding",
  "unicode-escape",
  "hex-escape",
  "equals-in-token",
  "question-in-token",
  "line-separator",
  "control",
  "whitespace-split",
  "known-residual",
  "non-ascii",
  "at-after-authority",
  "two-at",
  "port-not-digits",
  "authority-without-slashes",
  "colon-at-before-start",
  "at-after-url",
  "trailing-punctuation",
  "trailing-punctuation-in-value",
  "json-body",
  "two-urls",
  "bare-segment",
  "ipv6",
  "scheme-case",
  "other-scheme",
  "user-only",
  "r4-no-url",
  "r4-clean-url",
  "r4-prose-after-url",
  "r4-percent",
  "r4-email-without-url",
  "r4-not-a-url",
  "r4-empty",
  "r4-unreadable-clean",
];

interface RemoteRow {
  readonly id: string;
  readonly category: string;
  readonly field: string;
  readonly readable: boolean;
  readonly printed?: string;
  readonly secrets: readonly string[];
}

const corpus = JSON.parse(readFileSync(CORPUS_PATH, "utf8")) as {
  readonly not_readable: string;
  readonly rows: readonly RemoteRow[];
};

test("remote fixture: the marker is WORDS_NOT_READABLE, every required category has a row, and no id repeats", () => {
  expect(corpus.not_readable).toBe(WORDS_NOT_READABLE);
  const ids = corpus.rows.map((row) => row.id);
  expect(ids.filter((id, index) => ids.indexOf(id) !== index)).toEqual([]);
  const categories = new Set(corpus.rows.map((row) => row.category));
  expect(REQUIRED_CATEGORIES.filter((category) => !categories.has(category))).toEqual([]);
  expect([...categories].filter((category) => !REQUIRED_CATEGORIES.includes(category))).toEqual([]);
  for (const row of corpus.rows) {
    expect(row.readable, `${row.id}: a readable row names what it prints`).toBe(
      row.printed !== undefined
    );
    for (const secret of row.secrets) expect(row.field, `${row.id}`).toContain(secret);
  }
});

test.each(
  corpus.rows.map((row) => [row.id, row] as const)
)("remote fixture row %s reads as the fixture says", (_id, row) => {
  const read = readRemoteText(row.field);
  const printed = redactRemoteText(row.field);
  // The designated assertion of cmd/redact-gate's control (1): the output the rule gave,
  // captured, and a failure naming fixture/<id> where it holds one of the row's secrets.
  const captured = `${read ?? ""}\n${printed}`;
  console.log(
    `REDACT-CAPTURE fixture/${row.id} ${Buffer.from(captured, "utf8").toString("base64")}`
  );
  for (const secret of row.secrets) {
    expect(
      captured.includes(secret),
      `REDACT-ASSERT fixture/${row.id}: the rule's output holds ${secret}`
    ).toBe(false);
  }
  expect(read).toBe(row.readable ? row.printed : undefined);
  expect(printed).toBe(row.readable ? (row.printed as string) : WORDS_NOT_READABLE);
});

test("a RemoteText compares ignoring case and parses a time, and answers no text", () => {
  const probe = corpus.rows.find((row) => row.id === "probe");
  if (probe === undefined) throw new Error("the fixture has no probe row");
  expect(new RemoteText("dpop").equalsIgnoringCase("DPoP")).toBe(true);
  expect(new RemoteText("Bearer").equalsIgnoringCase("DPoP")).toBe(false);
  expect(new RemoteText("a.b").equalsIgnoringCase("a?b")).toBe(false);
  expect(new RemoteText("axb").equalsIgnoringCase("a.b")).toBe(false);
  expect(new RemoteText("2026-12-01T00:00:00Z").date()?.toISOString()).toBe(
    "2026-12-01T00:00:00.000Z"
  );
  expect(new RemoteText(probe.field).date()).toBeUndefined();
  expect(new RemoteText(probe.field).equalsIgnoringCase(probe.field.toUpperCase())).toBe(true);
});

test("callLibrary turns what a library throws or rejects with into a LibraryError that keeps no words", async () => {
  const probe = corpus.rows.find((row) => row.id === "probe");
  if (probe === undefined) throw new Error("the fixture has no probe row");
  const description = "the issuer refused this request";
  /** An error shaped as oauth4webapi's ResponseBodyError: the answer's words and its body. */
  const answered = () =>
    Object.assign(new Error("server responded with an error in the response body"), {
      name: "ResponseBodyError",
      status: 400,
      error: "invalid_grant",
      error_description: description,
      cause: { error: "invalid_grant", error_description: description, access_token: probe.field },
    });
  const thrown = (() => {
    try {
      callLibrary(() => {
        throw answered();
      });
    } catch (error) {
      return error;
    }
  })();
  const rejected = await callLibrary(() => Promise.reject(answered())).catch(
    (error: unknown) => error
  );
  for (const error of [thrown, rejected]) {
    expect(error).toBeInstanceOf(LibraryError);
    if (!(error instanceof LibraryError)) continue;
    expect([error.name, error.status, error.code.equals("invalid_grant")]).toEqual([
      "ResponseBodyError",
      400,
      true,
    ]);
    expect(error.description.equals(description)).toBe(true);
    for (const text of [
      String(error.code),
      String(error.description),
      error.message,
      JSON.stringify(error),
      Bun.inspect(error),
    ]) {
      for (const privateWord of ["invalid_grant", description]) {
        expect(text).not.toContain(privateWord);
      }
      for (const secret of probe.secrets) expect(text).not.toContain(secret);
    }
  }
  expect(callLibrary(() => 7)).toBe(7);
  expect(await callLibrary(() => Promise.resolve("answered"))).toBe("answered");
});
