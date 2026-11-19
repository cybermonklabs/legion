import { describe, expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret, wordBuiltFixtureSecrets } from "./fixture-secret";
import { temporaryDirectories } from "./temporary-directories";

describe("fixtureSecret", () => {
  test("is its prefix, a dash and 24 random upper-case letters, new at every call", () => {
    const one = fixtureSecret("boot");
    expect(one).toMatch(/^boot-[A-Z]{24}$/);
    expect(fixtureSecret("boot")).not.toBe(one);
  });

  // Every 10-character window holds a random upper-case letter, so no window reads as a word:
  // with the longest prefix the first window holds one random character, so 200 secrets test it.
  test("leaves no 10-character window without an upper-case letter", () => {
    for (let made = 0; made < 200; made += 1) {
      const secret = fixtureSecret("abcdefgh");
      for (let at = 0; at + 10 <= secret.length; at += 1) {
        expect(secret.slice(at, at + 10)).toMatch(/[A-Z]/);
      }
    }
  });

  test("refuses a prefix that could hold a word of 10 characters", () => {
    expect(() => fixtureSecret("controller")).toThrow("1 to 8 lower-case letters or digits");
    expect(() => fixtureSecret("")).toThrow();
  });
});

describe("wordBuiltFixtureSecrets", () => {
  const makeDirectory = temporaryDirectories("fixture-secret");
  const packageWith = (source: string): string => {
    const root = makeDirectory();
    writeFileSync(path.join(root, "probe.test.ts"), source);
    return root;
  };

  // A check that read nothing has not passed: a root with no test source is an error, not [].
  test("refuses a root that holds no test source", () => {
    const root = makeDirectory();
    writeFileSync(
      path.join(root, "index.ts"),
      'export const greeting = "a file the check does not read";\n'
    );
    expect(() => wordBuiltFixtureSecrets(root)).toThrow(`no test source under ${root}`);
  });

  // The control: the fixture that hid the operator's claim refusal (#4140) is found.
  test.each([
    [
      "a word-built capability in a 0600 file",
      'await writeFile(secretFile, "controller-start-capability\\n", { mode: 0o600 });',
    ],
    [
      "a word-built token in an environment variable",
      'env: { DISPATCH_TOKEN: "environment-token" },',
    ],
    // biome-ignore lint/suspicious/noTemplateCurlyInString: source text the scanner reads
    ["a secret built around a name", "const BOOT = { LEGION_BOOT_TOKEN: `${name}-boot` };"],
    ["a camel-case token", 'const bootToken = "launch-boot-token";'],
    ["a token file written with no mode", 'writeFileSync(tokenFile, "pane-listener-token");'],
    ["a plain bearer", 'bearerCredential("listener-token")'],
    ["a stand-in's registration secret", 'Response.json({ secret: "registration-secret" })'],
    // Built from parts, so the push's credential check reads no key and value in this file.
    ["a stand-in broker's access token", `Response.json({ ${"access"}_token: "broker-answer" })`],
    ["a quoted token key", `const config = { "${"token"}": "profile-file-token" };`],
    ["a camel-case secret key", `Response.json({ client${"Secret"}: "device-client-secret" })`],
    ["a token file Bun writes", 'await Bun.write(tokenFile, "projected-token-value");'],
    [
      "a token file whose path a call joins",
      'writeFileSync(path.join(dir, "LEGION_BOOT_TOKEN"), "launch-boot-token");',
    ],
    ["a password key", `const login = { pass${"word"}: "operator-password" };`],
    ["a capability key", `const held = { capa${"bility"}: "controller-capability" };`],
    ["an API key", `const config = { api${"Key"}: "stand-in-api-key" };`],
  ])("finds %s", (_, source) => {
    expect(wordBuiltFixtureSecrets(packageWith(source))).toHaveLength(1);
  });

  test.each([
    ["tests", "stand-in.ts"],
    ["e2e", "rig.ts"],
    ["scripts", "rig.mts"],
    ["test", "stand-in.tsx"],
    ["tests", "stand-in.js"],
  ])("reads the helpers under %s/%s", (directory, file) => {
    const root = makeDirectory();
    mkdirSync(path.join(root, directory));
    writeFileSync(path.join(root, directory, file), 'bearerCredential("listener-token")');
    expect(wordBuiltFixtureSecrets(root)).toEqual([`${directory}/${file}:1: "listener-token"`]);
  });

  test("classifies a file by its path under the root, not by the root's own", () => {
    // A package whose checkout sits under a directory named `tests` reads its tests alone.
    const root = path.join(makeDirectory(), "tests", "package");
    mkdirSync(path.join(root, "src"), { recursive: true });
    writeFileSync(path.join(root, "src", "client.ts"), 'bearerCredential("production-token")');
    writeFileSync(path.join(root, "src", "client.test.ts"), 'const ok = "short";');
    expect(wordBuiltFixtureSecrets(root)).toEqual([]);
  });

  // The forms the scanner's own comment says it cannot see. A row here that starts finding one
  // means the code changed, and that comment is to change with it.
  test.each([
    ["a call whose literal is on a line of its own", 'bearerCredential(\n  "listener-token"\n)'],
    [
      "a secret handed through a variable that names none",
      'const value = "listener-token";\nconst config = { token: value };',
    ],
    [
      "a header written whole",
      `fetch(url, { headers: { Authorization: "${"Bearer"} listener-token" } });`,
    ],
    ["a write whose path names no secret", 'writeFileSync(file, "projected-token-value");'],
  ])("does not see %s, as its comment says", (_, source) => {
    expect(wordBuiltFixtureSecrets(packageWith(source))).toEqual([]);
  });

  test("does not read a committed vector, as its comment says", () => {
    const root = packageWith('const ignored = "a test file the scanner reads";');
    mkdirSync(path.join(root, "testdata"));
    writeFileSync(
      path.join(root, "testdata", "vector.json"),
      JSON.stringify({ [`access${"_token"}`]: "vector-access-token" })
    );
    expect(wordBuiltFixtureSecrets(root)).toEqual([]);
  });

  test.each([
    ["the helper", 'const SECRET = fixtureSecret("ctl");'],
    ["a short value, never held", 'process.env.LEGION_BOOT_TOKEN = "decoy";'],
    // biome-ignore lint/suspicious/noTemplateCurlyInString: source text the scanner reads
    ["the helper's value in a file", "await writeFile(file, `${SECRET}\\n`, { mode: 0o600 });"],
    // biome-ignore lint/suspicious/noTemplateCurlyInString: source text the scanner reads
    ["a random call", "const token = `${crypto.randomUUID()}`;"],
    // biome-ignore lint/suspicious/noTemplateCurlyInString: source text the scanner reads
    ["a claim token, which names a role", "const claimToken = `legion-${name}-implementer`;"],
    [
      "a refused value, exempted",
      'ENVOY_TOKEN: "S3CRET\\nnotes", // fixture-secret: refused for its newline, never held',
    ],
    [
      "a value exempted on the line above",
      `// fixture-secret-next-line: a field's name, never held\nconst NAMES = { access${"Token"}: "field_name_here" };`,
    ],
    [
      "a ternary's branch, which is no key",
      `const shown = ok ? access${"Token"} : "no token in hand";`,
    ],
    [
      "a list's cursor",
      `const page = { page${"Token"}: "next-page-cursor", next${"Token"}: "the-next-cursor" };`,
    ],
    [
      "a claim token in snake case",
      `const claim = { claim_${"token"}: "legion-implementer-claim" };`,
    ],
  ])("leaves %s", (_, source) => {
    expect(wordBuiltFixtureSecrets(packageWith(source))).toEqual([]);
  });
});
