import { describe, expect, test } from "bun:test";
import {
  errorClass,
  isCleanUrl,
  isHttpUrl,
  requestForText,
  URL_NOT_PRINTED,
  urlForText,
} from "./log-text";

const TAIL = "TAIL7c1e0b";
const phrase = (setting: string) => `${setting} ${URL_NOT_PRINTED}`;

// Only a plain http(s) URL prints, as written; every other configured value prints as its
// setting's fixed phrase. A rule that took the user info, the query or the fragment out would print
// a piece of a credential wherever the URL parser splits the text otherwise than its writer meant.
describe("urlForText", () => {
  test.each([
    "https://dispatch.example",
    "https://dispatch.example/",
    "http://127.0.0.1:8766/api",
    "HTTPS://Dispatch.Example:443/base_path/v1-x.y~z",
    "http://[::1]:8080",
    "http://localhost",
  ])("%p prints as written", (url) => {
    expect(isCleanUrl(url)).toBe(true);
    expect(urlForText(url, "THE_URL")).toBe(url);
  });

  // Each is a credential the parser would split differently from its writer, or a place one sits.
  test.each([
    // A password holding an '@' and naming no host: the parser reads the password's tail as one.
    [`https://user:p@ssw0rd${TAIL}`, "an '@' in a password, no host"],
    // A token holding a '?': the parser reads its head as the host and the rest as a query.
    [`https://tok?en${TAIL}@broker.example`, "a '?' in user info"],
    [`https://tok#en${TAIL}@broker.example`, "a '#' in user info"],
    [`https://user:p%40ss${TAIL}@broker.example`, "an escape in user info"],
    [`https://user\\p${TAIL}@broker.example`, "a backslash in user info"],
    [`https://user:pw${TAIL}@broker.example`, "user info"],
    [`https://broker.example/v1/token?access_token=${TAIL}`, "a query"],
    [`https://broker.example/callback#access_token=${TAIL}`, "a fragment"],
    [`https://broker.example/a%2F${TAIL}`, "an escape in the path"],
    [`https://broker.example/a b${TAIL}`, "a space"],
    [`ya29.a0-pasted-${TAIL}`, "no URL at all"],
    [`https://broker.example:99999${TAIL}`, "a port past five digits"],
    [`nats://broker.example/${TAIL}`, "another scheme"],
    [`https://${TAIL}_x.example`, "a host that is no DNS name"],
    [`https://[fe80::1%25eth0${TAIL}]`, "an IPv6 zone"],
  ])("%p (%s) prints as its setting's phrase alone", (url) => {
    expect(isCleanUrl(url)).toBe(false);
    const shown = urlForText(url, "AGENT_SECRETS_URL");
    expect(shown).toBe(phrase("AGENT_SECRETS_URL"));
    expect(shown.toLowerCase()).not.toContain(TAIL.toLowerCase());
  });
});

// A Postgres URL prints as its scheme, host, port and database, with no user, password or query,
// where it reads one way to every parser and its host passes the http(s) host rule; otherwise as
// its setting's phrase. The password is made for this run.
describe("urlForText of a Postgres URL", () => {
  const password = `pw${crypto.randomUUID()}`;
  test.each([
    [
      `postgres://made-up-user:${password}@127.0.0.1:5432/acme_local?sslmode=disable`,
      "postgres://127.0.0.1:5432/acme_local",
    ],
    ["postgresql://made-up-user@db.internal/app", "postgresql://db.internal/app"],
    ["postgres://127.0.0.1:5432/acme_local", "postgres://127.0.0.1:5432/acme_local"],
    [`postgres://made-up-user:${password}@[::1]:5432`, "postgres://[::1]:5432"],
  ])("%p prints as %p", (url, shown) => {
    expect(isCleanUrl(url)).toBe(false);
    expect(urlForText(url, "DATABASE_URL")).toBe(shown);
  });

  // Each is a value whose host fails the host rule, or one a parser can split otherwise than its
  // writer meant.
  test.each([
    [`postgres://made-up-user:${TAIL}@${TAIL}_x.example/db`, "a host that is no DNS name"],
    [`postgres://made-up-user:p@ss${TAIL}@127.0.0.1/db`, "a second '@'"],
    [`postgres://made-up-user:p/x${TAIL}@127.0.0.1/db`, "a '/' in a password"],
    [`postgres://made-up-user:p?x${TAIL}@127.0.0.1/db`, "a '?' in a password"],
    [`postgres://made-up-user:p#x${TAIL}@127.0.0.1/db`, "a '#' in a password"],
    [`postgres://made-up-user:p\\x${TAIL}@127.0.0.1/db`, "a backslash in a password"],
    [`POSTGRES://made-up-user:${TAIL}@127.0.0.1/db`, "an upper-case scheme"],
    [` postgres://made-up-user:${TAIL}@127.0.0.1/db`, "a leading space"],
    [`postgres://made-up-user:${TAIL}@h1:5432,h2:5432/db`, "two hosts"],
    [`postgres://made-up-user:${TAIL}@127.0.0.1:99999${TAIL}/db`, "a port past five digits"],
    [`postgres://127.0.0.1/db%2F${TAIL}`, "an escape in the database"],
    [`postgres://127.0.0.1/db?user=x@${TAIL}`, "an '@' in the query"],
    [`postgres:///db?host=/run/${TAIL}`, "no host"],
  ])("%p (%s) prints as its setting's phrase alone", (url) => {
    const shown = urlForText(url, "DATABASE_URL");
    expect(shown).toBe(phrase("DATABASE_URL"));
    expect(shown.toLowerCase()).not.toContain(TAIL.toLowerCase());
  });
});

// A request URL a client built from a clean base prints with the path and query the client wrote;
// from any other base, as the base's phrase.
describe("requestForText", () => {
  test.each([
    [
      "http://dispatch.test",
      "http://dispatch.test/api/v1/issues?project=DSP&limit=2",
      "http://dispatch.test/api/v1/issues?project=DSP&limit=2",
    ],
    [
      "HTTP://Dispatch.Test/base",
      "http://dispatch.test/base/api/v1/issues/resolve?ref=owner%2Frepo%2312",
      "HTTP://Dispatch.Test/base/api/v1/issues/resolve?ref=owner%2Frepo%2312",
    ],
  ])("from %p, %p prints as %p", (base, url, shown) => {
    expect(requestForText(base, url, "THE_URL")).toBe(shown);
  });

  test.each([
    [`http://user:pw${TAIL}@dispatch.test`, `http://user:pw${TAIL}@dispatch.test/api/v1/x`],
    [`http://user:p@ssw0rd${TAIL}`, `http://user:p@ssw0rd${TAIL}/api/v1/x`],
    ["http://dispatch.test", `http://elsewhere.example/${TAIL}`],
    ["http://host", `http://hostile.example/${TAIL}`],
  ])("from %p, %p prints as the base's phrase", (base, url) => {
    const shown = requestForText(base, url, "the configured Dispatch URL");
    expect(shown).toBe(phrase("the configured Dispatch URL"));
    expect(shown.toLowerCase()).not.toContain(TAIL.toLowerCase());
  });
});

describe("isHttpUrl", () => {
  test.each([
    ["https://broker.example", true],
    ["http://127.0.0.1:8080/base", true],
    ["https://user:pw@broker.example", true],
    ["nats://host.example:4222", false],
    ["ya29.a0-pasted-token", false],
    ["", false],
  ] as const)("%p is %p", (value, is) => {
    expect(isHttpUrl(value)).toBe(is);
  });
});

// A library's or the runtime's message repeats what it was handed: a resolver's names the host, a
// URL parser's error holds the URL, a file read's names the path. Only the class is named.
describe("errorClass", () => {
  const canary = "canary-7c1e0b";

  test("a runtime error is named by its syscall and code", () => {
    const error = Object.assign(new Error(`getaddrinfo ENOTFOUND ${canary}.invalid`), {
      code: "ENOTFOUND",
      syscall: "getaddrinfo",
    });
    expect(errorClass(error)).toBe("getaddrinfo ENOTFOUND");
  });

  test("a library error is named by its code", () => {
    const error = Object.assign(new TypeError(`fetch ${canary} failed`), { code: "ECONNREFUSED" });
    expect(errorClass(error)).toBe("ECONNREFUSED");
  });

  test("an error with no code is named by its type, its message withheld", () => {
    expect(errorClass(new TypeError(`"${canary}" cannot be parsed as a URL`))).toBe(
      "TypeError (message withheld)"
    );
    const odd = Object.assign(new Error(canary), { name: `Error ${canary}`, code: `${canary} x` });
    expect(errorClass(odd)).toBe("Error (message withheld)");
    expect(errorClass(canary)).toBe("a thrown string (message withheld)");
  });
});
