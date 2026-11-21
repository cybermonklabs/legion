/** Type-level rows of the per-row keys (R8 of dispatch://INFRA-983). docs/testing.md, "Where a
 *  Dispatch test belongs", says a real-stack row makes its projects under `rowKey(...)`, that
 *  `createProject` takes only the `RowKey` type `rowKey` answers, and that "a helper that makes a
 *  project under a key its caller passes takes a `RowKey` too". Each call below writes a literal
 *  key and must fail the typecheck: `bun run typecheck` reads this file (packages/dispatch's
 *  tsconfig.json includes `e2e/**\/*.ts`), and a directive above a call that compiles is unused,
 *  which tsc reports as an error. The first call is the control: `createProject` refuses the key
 *  today. Nothing runs this file. */
import { createProject } from "../api";
import { deliveryPlace } from "../delivery-seed";
import { seedLegionDaemon, seedLegionProject } from "../legion-seed";

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
