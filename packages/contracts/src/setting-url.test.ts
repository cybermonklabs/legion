import { describe, expect, test } from "bun:test";
import * as settingUrl from "./setting-url";
import { shownSettingUrl, unshownSettingUrl } from "./setting-url";

// One function decides what a line may show of a setting's URL. A second export that strips a
// user part without that judgement is how a value the parser misread (`user:password@host`) was
// printed whole, so the module exports that function, and beside it only the words a refusal
// names a value by when the function shows none of it, which hold no part of the value.
test("the module exports shownSettingUrl and unshownSettingUrl alone", () => {
  expect(Object.keys(settingUrl)).toEqual(["shownSettingUrl", "unshownSettingUrl"]);
});

// A setting's value is shown only when the URL parser reads it as an http or https URL with a
// host, and then with no user and no password. Any other reading cannot be trusted to have found
// the password (`user:password@host` parses, with `user:` as its scheme and the password in its
// path), so such a value is shown not at all: a line names its length alone.
describe("shownSettingUrl", () => {
  const user = "user-7f2a";
  const secret = "pw-4c7e19";

  // A configured Dispatch or Envoy URL may carry a user and a password, and a user alone can be a
  // token, so a line that names the URL drops both and keeps the rest as it was written.
  test.each([
    [`https://${user}:${secret}@host.example/a`, "https://host.example/a"],
    [`HTTPS://${user}:${secret}@Host.Example/a`, "https://host.example/a"],
    [`http://${user}@127.0.0.1:8766`, "http://127.0.0.1:8766"],
    ["http://dispatch.test", "http://dispatch.test"],
    [`https://${user}:${secret}@host.example`, "https://host.example"],
    [`https://${user}:${secret}@host.example/`, "https://host.example/"],
    [
      `http://${user}:${secret}@host.example:8766/api/v1/issues?project=DSP`,
      "http://host.example:8766/api/v1/issues?project=DSP",
    ],
    [`https://${user}@host.example/dispatch`, "https://host.example/dispatch"],
    [`https://:${secret}@host.example`, "https://host.example"],
    // An `@` that the user part holds percent-encoded is part of that user part.
    [`https://${user}%40corp.example:${secret}@host.example/a`, "https://host.example/a"],
  ])("%p is shown as %p", (value, shown) => {
    expect(shownSettingUrl(value)).toBe(shown);
  });

  test("a URL with no user or password comes back as written", () => {
    for (const url of ["http://dispatch.test", "HTTP://Host.Example"]) {
      expect(shownSettingUrl(url)).toBe(url);
    }
  });

  // The parser ends a user part at the first `/`, `?`, `#` or `\` before it: a token that holds
  // one (base64 does) reads as a host taken from the token, and the rest of it as the path, the
  // query or the fragment. So no reading of the parser is trusted to have found the user part:
  // a value that still holds a raw `@` once the user part the parser found is taken out is shown
  // not at all, since even its host can be a part of the token. That holds for an `@` in a path,
  // a query or a fragment too (`https://host.example/a@b`), which a setting's URL has no need of.
  test.each([
    [`https://MadeUpTok3n/${secret}+Qw==@host.example`],
    [`https://MadeUpTok3n#${secret}@host.example`],
    [`https://MadeUpTok3n?${secret}@host.example`],
    [`https://MadeUpTok3n\\${secret}@host.example`],
    [`https://${user}:4821/${secret}@host.example`],
    [`https://${user}:4821#${secret}@host.example`],
    [`https://${user}@MadeUpTok3n/${secret}@host.example`],
    ["https://host.example/a@b"],
    ["https://host.example/?to=a@b"],
    ["https://host.example/#a@b"],
    [`https://${user}:${secret}@host.example/?to=a@b`],
    // A scheme written twice: the parser reads the host `http`, and the rest as the path.
    [`http://http://${user}:${secret}@host.example`],
  ])("%p, which holds an @ that no user part ends with, is shown not at all", (value) => {
    expect(shownSettingUrl(value)).toBeUndefined();
  });

  test.each([
    [`${user}:${secret}@host.example/a`],
    [`${user}:${secret}@host.example:8443`],
    [`mailto:${user}:${secret}@host.example`],
    [`//${user}:${secret}@host.example/a`],
    ["   "],
    [""],
    ["not a url"],
    [`ftp://${user}:${secret}@host.example/a`],
    [`file://${user}:${secret}@host.example/a`],
    [`https://${user}:${secret}@bad host/a`],
    [`https://${user}:${secret}/x@host.example`],
    [`https://${user}:${secret}@host.example:99999`],
  ])("%p is shown not at all", (value) => {
    expect(shownSettingUrl(value)).toBeUndefined();
  });

  // The parser drops a tab and a line feed where it reads them, so a value that holds one, or any
  // other control character, would carry it into a line that showed the value as written.
  test.each([
    ["https://host.example/a\tb"],
    ["https://host.example/a\nb"],
    ["https://host.example\r/a"],
    ["https://host.example/a\u0000"],
    ["https://host.example/a\u007f"],
    ["\thttps://host.example/a"],
  ])("%p, which holds a control character, is shown not at all", (value) => {
    expect(shownSettingUrl(value)).toBeUndefined();
  });

  // A caller whose setting names a URL of another scheme (a NATS server, a Postgres database)
  // says which schemes it takes, and the same rule holds for each: no user and no password, and
  // a value that still holds a raw `@` once its user part is taken out is shown not at all. Such
  // a value is shown without its query and fragment too, since this function cannot tell which
  // parameters of another protocol are secrets (libpq reads a password from `?password=`).
  test.each([
    [`nats://${user}:${secret}@127.0.0.1:4222`, ["nats:"], "nats://127.0.0.1:4222"],
    [`nats://${secret}@nats.example:4222`, ["nats:"], "nats://nats.example:4222"],
    [
      `postgres://${user}:${secret}@127.0.0.1:5432/db?sslmode=disable&password=${secret}`,
      ["postgres:", "postgresql:"],
      "postgres://127.0.0.1:5432/db",
    ],
    ["postgres://127.0.0.1:5432/db#frag", ["postgres:"], "postgres://127.0.0.1:5432/db"],
    [
      `https://${user}:${secret}@host.example/a?b=c`,
      ["http:", "https:"],
      "https://host.example/a?b=c",
    ],
  ] as const)("%p, of the schemes %p, is shown as %p", (value, schemes, shown) => {
    expect(shownSettingUrl(value, schemes)).toBe(shown);
  });

  test.each([
    [`nats://${user}:${secret}@127.0.0.1:4222`, undefined],
    [`https://${user}:${secret}@host.example`, ["nats:"]],
    [`nats://MadeUpTok3n/${secret}@nats.example:4222`, ["nats:"]],
    [`postgres://${user}:4821/${secret}@127.0.0.1/db`, ["postgres:"]],
    [`postgres:///db?host=/run/postgresql`, ["postgres:"]],
  ] as const)("%p, of the schemes %p, is shown not at all", (value, schemes) => {
    expect(shownSettingUrl(value, schemes)).toBeUndefined();
  });
});

// A refusal names a value that shownSettingUrl shows none of by its length, and says the cause that
// shownSettingUrl's checks found, each in its own words, with what to correct; the words never hold
// a part of the value. A value shownSettingUrl shows is named by its length alone.
describe("unshownSettingUrl", () => {
  const secret = "pw-4c7e19";
  const control =
    "it holds a control character (a line ending or a tab, often one left from a file); remove it";
  const unparsed = "it does not parse as a URL";
  const userPart =
    "if it carries a user part, write each @, /, ?, # and \\ in that part percent-encoded (@ as %40, / as %2F), or remove the user part";
  const atOutside =
    "it holds an @ outside its user part (in its path, query or fragment); if its user part holds a /, ?, # or \\, write each of them in it percent-encoded (/ as %2F, ? as %3F, # as %23, \\ as %5C), else write that @ as %40";

  test.each([
    ["a line ending at its end", "http://127.0.0.1:9020\n", undefined, control],
    [
      "a carriage return and a line feed at its end",
      "http://127.0.0.1:9020\r\n",
      undefined,
      control,
    ],
    ["a tab inside", `https://made-up-user:${secret}@host.example/a\tb`, undefined, control],
    ["no URL at all", "not a url", undefined, unparsed],
    ["nothing", "", undefined, unparsed],
    ["a host with a space", "https://bad host/a", undefined, unparsed],
    [
      "a / in the password, whose rest reads as a port",
      `https://made-up-user:pa/${secret}@host.example`,
      undefined,
      `${unparsed}; ${userPart}`,
    ],
    [
      "another scheme",
      "ftp://host.example/a",
      undefined,
      "it does not start with http:// or https://",
    ],
    [
      "no scheme, its user read as one",
      `made-up-user:${secret}@host.example`,
      undefined,
      "it does not start with http:// or https://",
    ],
    [
      "another scheme, of the NATS schemes",
      "https://nats.example:4222",
      ["nats:", "tls:"],
      "it does not start with nats:// or tls://",
    ],
    [
      "no host, of the Postgres schemes",
      "postgres:///db?host=/run/postgresql",
      ["postgres:", "postgresql:"],
      "it names no host",
    ],
    [
      "a / in a token that is the user part",
      `https://MadeUpTok3n/${secret}@host.example`,
      undefined,
      atOutside,
    ],
    [
      "a # in the password",
      `https://made-up-user:4821#${secret}@host.example`,
      undefined,
      atOutside,
    ],
    ["an @ in the query", "https://host.example/?to=a@b", undefined, atOutside],
  ] as const)("a value with %s is named by its length and that cause", (_, value, schemes, cause) => {
    const words = unshownSettingUrl(value, schemes);
    expect(words).toBe(`its value is ${value.length} characters long; ${cause}`);
    expect(words).not.toContain(secret);
    expect(words).not.toContain("MadeUpTok3n");
    expect(words).not.toContain("made-up-user");
  });

  test("a value shownSettingUrl shows is named by its length alone", () => {
    const value = `https://made-up-user:${secret}@host.example/a`;
    expect(unshownSettingUrl(value)).toBe(`its value is ${value.length} characters long`);
  });

  // The Go check (redact.Misread, misread_test.go's TestEachCauseIsTheOneTheTypeScriptCheckNames)
  // reads the same values, each with an @, and names the same cause for each.
  test.each([
    [`https://made-up-user:${secret}@host.example/a\tb`, control],
    [`https://made-up-user:pa/${secret}@host.example`, `${unparsed}; ${userPart}`],
    [`made-up-user:${secret}@host.example`, "it does not start with http:// or https://"],
    [
      `made-up-user:${secret}@host.example/?next=http://x.example`,
      "it does not start with http:// or https://",
    ],
    [`https://MadeUpTok3n/${secret}@host.example`, atOutside],
    [`https://made-up-user:4821#${secret}@host.example`, atOutside],
    ["https://host.example/?to=a@b", atOutside],
  ] as const)("the cause of each value the Go check reads too: %s", (value, cause) => {
    expect(unshownSettingUrl(value)).toBe(`its value is ${value.length} characters long; ${cause}`);
  });
});
