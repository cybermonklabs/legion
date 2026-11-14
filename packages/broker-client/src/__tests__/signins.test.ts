// packages/broker-client/src/__tests__/signins.test.ts
//
// A person's sign-in on this machine, read as the Go client reads it: the directory it lives in,
// which record a session acts under, the refusals that stop a session acting as one colleague
// under another's name, and the sign-in client a deployment configures.

import { expect, test } from "bun:test";
import { mkdir, writeFile } from "node:fs/promises";
import { basename, join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { signedIn, signInPath, writeSignIn } from "../../test/sign-ins";
import {
  chooseSignIn,
  DEFAULT_SIGNIN_ISSUER,
  listSignIns,
  PERSON_VAR,
  SIGNIN_CLIENT_ID_VAR,
  SIGNIN_CLIENT_SECRET_FILE_VAR,
  SIGNIN_COMMAND,
  SIGNIN_DIR_VAR,
  SIGNIN_ISSUER_VAR,
  type SignInRecord,
  signInDirectory,
  signInFlow,
  signInSessionName,
} from "../signins";
import { refusalOf } from "./stub";

const makeDirectory = temporaryDirectories("broker-signins");

test("the directory is the variable's, else the XDG state directory's, else HOME's", () => {
  const everything = {
    [SIGNIN_DIR_VAR]: "/srv/example/signins",
    XDG_STATE_HOME: "/home/example/.state",
    HOME: "/home/example",
  };
  expect(signInDirectory(everything)).toBe("/srv/example/signins");
  // XDG_STATE_HOME comes before HOME: a Legion tmux pane's points under the daemon's own state,
  // which keeps the pane off the operator's sign-ins.
  expect(signInDirectory({ ...everything, [SIGNIN_DIR_VAR]: "" })).toBe(
    "/home/example/.state/agent-secrets/signins"
  );
  expect(signInDirectory({ HOME: "/home/example" })).toBe(
    "/home/example/.local/state/agent-secrets/signins"
  );
  expect(signInDirectory({ XDG_STATE_HOME: "", HOME: "/home/example" })).toBe(
    "/home/example/.local/state/agent-secrets/signins"
  );
});

test("a directory that is not there is nobody signed in, and no error", async () => {
  const missing = join(makeDirectory(), "signins");
  expect(await listSignIns(missing)).toEqual([]);
  expect(await chooseSignIn({ [SIGNIN_DIR_VAR]: missing, [PERSON_VAR]: "" })).toBeUndefined();
  // A name for nobody on a machine where nobody signed in is no refusal either: the machine
  // trades as itself.
  expect(
    await chooseSignIn({ [SIGNIN_DIR_VAR]: missing, [PERSON_VAR]: "ada@example.com" })
  ).toBeUndefined();
});

test("one sign-in is the one a session acts under, read as Go wrote it", async () => {
  const dir = join(makeDirectory(), "signins");
  const ada = await signedIn("ada@example.com", { enrollment_id: "enrollment-1" });
  await writeSignIn(dir, ada);
  expect(await chooseSignIn({ [SIGNIN_DIR_VAR]: dir })).toEqual(ada);
  expect(await listSignIns(dir)).toEqual([ada]);
  // The XDG state directory finds the same record.
  const state = makeDirectory();
  await writeSignIn(join(state, "agent-secrets", "signins"), ada);
  expect(await chooseSignIn({ XDG_STATE_HOME: state, HOME: "/nonexistent" })).toEqual(ada);
});

test("a member Go leaves out reads as empty", async () => {
  const dir = join(makeDirectory(), "signins");
  // Go's omitempty leaves no enrollment_id in a record that has none.
  const ada = await signedIn("ada@example.com");
  const path = await writeSignIn(dir, ada);
  expect(await Bun.file(path).text()).not.toContain("enrollment_id");
  expect((await chooseSignIn({ [SIGNIN_DIR_VAR]: dir }))?.enrollment_id).toBe("");
});

test("two sign-ins and nobody named is refused naming both, until one is named", async () => {
  const dir = join(makeDirectory(), "signins");
  const grace = await signedIn("grace@example.com");
  const ada = await signedIn("ada@example.com");
  await writeSignIn(dir, grace);
  await writeSignIn(dir, ada);
  // By person, whatever the order of the files.
  expect((await listSignIns(dir)).map((record) => record.person)).toEqual([
    "ada@example.com",
    "grace@example.com",
  ]);
  const refusal = await refusalOf(() => chooseSignIn({ [SIGNIN_DIR_VAR]: dir }));
  expect(refusal?.message).toBe(
    `2 people are signed in on this machine (ada@example.com, grace@example.com), so this session must say which one it acts for: set ${PERSON_VAR} to one of them`
  );
  // Each named, whatever the case and the whitespace of the variable.
  expect(
    await chooseSignIn({ [SIGNIN_DIR_VAR]: dir, [PERSON_VAR]: " GRACE@example.com " })
  ).toEqual(grace);
  expect(await chooseSignIn({ [SIGNIN_DIR_VAR]: dir, [PERSON_VAR]: "ada@example.com" })).toEqual(
    ada
  );
});

test("a person named who is not signed in here is refused naming who is", async () => {
  const dir = join(makeDirectory(), "signins");
  await writeSignIn(dir, await signedIn("ada@example.com"));
  const refusal = await refusalOf(() =>
    chooseSignIn({ [SIGNIN_DIR_VAR]: dir, [PERSON_VAR]: "Mallory@example.com" })
  );
  expect(refusal?.message).toBe(
    `${PERSON_VAR} names mallory@example.com, who is not signed in on this machine; signed in here: ada@example.com; run: ${SIGNIN_COMMAND}`
  );
});

test("a record missing a person, a refresh token or a box key is refused naming its file", async () => {
  const required = [
    "person",
    "refresh_token",
    "box_key",
  ] as const satisfies readonly (keyof SignInRecord)[];
  for (const member of required) {
    const dir = join(makeDirectory(), "signins");
    const ada = await signedIn("ada@example.com");
    const path = await writeSignIn(dir, { ...ada, [member]: "" });
    const refusal = await refusalOf(() => chooseSignIn({ [SIGNIN_DIR_VAR]: dir }));
    expect(refusal?.message, member).toBe(
      `the sign-in at ${path} names no person, no refresh token or no box key; run: ${SIGNIN_COMMAND}`
    );
    // The refusal carries nothing the record keeps.
    expect(refusal?.message).not.toContain(ada.refresh_token);
  }
});

test("a record that is no JSON is refused naming its file, and quotes none of it", async () => {
  const dir = join(makeDirectory(), "signins");
  const ada = await signedIn("ada@example.com");
  const path = signInPath(dir, ada.person);
  await mkdir(dir, { recursive: true, mode: 0o700 });
  // A record cut short in the middle of its refresh token.
  const whole = JSON.stringify(ada, null, 2);
  await writeFile(path, whole.slice(0, whole.indexOf(ada.refresh_token) + 8), { mode: 0o600 });
  const refusal = await refusalOf(() => listSignIns(dir));
  expect(refusal?.message).toBe(
    `read the sign-in at ${path}: it is not JSON; run: ${SIGNIN_COMMAND}`
  );
  expect(refusal?.message).not.toContain(ada.refresh_token.slice(0, 8));
  // A member of the wrong type is refused naming it.
  await writeFile(path, JSON.stringify({ ...ada, person: 7 }), { mode: 0o600 });
  expect((await refusalOf(() => listSignIns(dir)))?.message).toBe(
    `read the sign-in at ${path}: its person is not a string; run: ${SIGNIN_COMMAND}`
  );
});

test("only the directory's .json files that are not directories are records", async () => {
  const dir = join(makeDirectory(), "signins");
  const ada = await signedIn("ada@example.com");
  const path = await writeSignIn(dir, ada);
  await writeFile(join(dir, "notes.txt"), "not a sign-in\n");
  // The temporary file Go's writeFileAtomic renames into place, caught half written.
  await writeFile(join(dir, `.${basename(path)}.2718281828`), "half written\n");
  await mkdir(join(dir, "old.json"));
  expect(await listSignIns(dir)).toEqual([ada]);
});

test("a sign-in's session name is Go's SessionFor", () => {
  // Go's own sample (packages/envoy/cmd/agent-secrets/login_test.go).
  const thumbprint = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG";
  expect(signInSessionName({ person: "ada@example.com", thumbprint })).toBe(
    "signin-ada@example.com-abcdefghijkl"
  );
  expect(signInSessionName({ person: "ada@example.com", thumbprint: "abc" })).toBe(
    "signin-ada@example.com-abc"
  );
  expect(signInSessionName({ person: "ada@example.com", thumbprint: "" })).toBe(
    "signin-ada@example.com"
  );
});

test("a sign-in client is both variables or neither", async () => {
  expect(await signInFlow({})).toBeUndefined();
  const both = `a person's sign-in needs both ${SIGNIN_CLIENT_ID_VAR} and ${SIGNIN_CLIENT_SECRET_FILE_VAR}; this machine has one of them`;
  expect(
    (await refusalOf(() => signInFlow({ [SIGNIN_CLIENT_ID_VAR]: "example-client" })))?.message
  ).toBe(both);
  expect(
    (await refusalOf(() => signInFlow({ [SIGNIN_CLIENT_SECRET_FILE_VAR]: "/run/secret" })))?.message
  ).toBe(both);

  const secretFile = join(makeDirectory(), "client-secret");
  await writeFile(secretFile, "example-client-not-a-secret\n", { mode: 0o600 });
  const configured = {
    [SIGNIN_CLIENT_ID_VAR]: " example-client ",
    [SIGNIN_CLIENT_SECRET_FILE_VAR]: secretFile,
  };
  expect(await signInFlow(configured)).toEqual({
    issuer: DEFAULT_SIGNIN_ISSUER,
    clientId: "example-client",
    clientSecret: "example-client-not-a-secret",
  });
  expect(
    (await signInFlow({ ...configured, [SIGNIN_ISSUER_VAR]: " http://127.0.0.1:5556// " }))?.issuer
  ).toBe("http://127.0.0.1:5556");
});

test("a secret file that cannot be read is refused naming the variable, never the secret", async () => {
  const dir = makeDirectory();
  // The variable names the directory the secret is in rather than the file: no read of it
  // answers anything, and no refusal may carry what the directory holds.
  await writeFile(join(dir, "client-secret"), "example-client-not-a-secret\n", { mode: 0o600 });
  for (const unreadable of [dir, join(dir, "missing")]) {
    const refusal = await refusalOf(() =>
      signInFlow({
        [SIGNIN_CLIENT_ID_VAR]: "example-client",
        [SIGNIN_CLIENT_SECRET_FILE_VAR]: unreadable,
      })
    );
    expect(refusal?.message).toStartWith(
      `read the sign-in client's secret (${SIGNIN_CLIENT_SECRET_FILE_VAR}): `
    );
    expect(refusal?.message).not.toContain("example-client-not-a-secret");
  }
});
