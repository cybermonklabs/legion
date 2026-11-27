/** Type-level rows of the per-row keys (R8 of dispatch://INFRA-983) and of the logins a row signs
 *  in as. docs/testing.md, "Where a Dispatch test belongs", says a real-stack row makes its
 *  projects under `rowKey(...)`, that `createProject` takes only the `RowKey` type `rowKey`
 *  answers, and that "a helper that makes a project under a key its caller passes takes a `RowKey`
 *  too"; it says too that `signIn`, `asUser`, the `signedIn` option and the API client's `login`
 *  (e2e/api.ts) take only alice, bob and the `RowLogin` type that `rowPerson` and `fixedPerson`
 *  answer, and that `fixedLogin` (e2e/users.ts) answers for a fixed login that leaves no person
 *  a later row reads. The same restriction applies to direct sign-ins through `devSignInPath`.
 *  Each call below writes a literal key or a literal login other than alice and bob and must
 *  fail the typecheck: `bun run typecheck` reads this file (packages/dispatch's tsconfig.json
 *  includes `e2e/**\/*.ts`), and a directive above a call that compiles is unused, which tsc
 *  reports as an error. The first call is the control: `createProject` refuses the key today.
 *  Nothing runs this file. */
import type { Browser, BrowserContext } from "@playwright/test";

import { createIssue, createProject, devSignInPath, userHeaders } from "../api";
import { deliveryPlace } from "../delivery-seed";
import { seedLegionDaemon, seedLegionProject } from "../legion-seed";
import { test } from "../test";
import { asUser, signIn } from "../users";

export function literalProjectKeysFailTheTypecheck(): void {
  // @ts-expect-error a literal key is no RowKey
  void createProject({ key: "CORE", name: "Core" });
  // @ts-expect-error a literal key is no RowKey: the helper inserts the project itself
  void seedLegionProject("ACME");
  // @ts-expect-error a literal key is no RowKey: the helper inserts the project itself
  void seedLegionDaemon("ACME", "http://daemon.test/state");
  // @ts-expect-error a literal key is no RowKey: the seed of the place inserts the project
  deliveryPlace("ACME", 10_000, 1, 1);
}

export function literalLoginsFailTheTypecheck(browser: Browser, context: BrowserContext): void {
  // @ts-expect-error a literal login other than alice and bob is no RowLogin
  void asUser(browser, "carol");
  // @ts-expect-error a literal login other than alice and bob is no RowLogin
  void signIn(context, "carol");
  // @ts-expect-error a literal login other than alice and bob is no RowLogin
  devSignInPath("carol");
  // @ts-expect-error a literal login other than alice and bob is no RowLogin
  test.use({ signedIn: "carol" });
  // @ts-expect-error a literal login other than alice and bob is no RowLogin: the API signs it in
  void createIssue({ project: "CORE", title: "Who answers this?" }, { login: "carol" });
  // @ts-expect-error a literal login other than alice and bob is no RowLogin: the API signs it in
  void userHeaders("carol");
}
