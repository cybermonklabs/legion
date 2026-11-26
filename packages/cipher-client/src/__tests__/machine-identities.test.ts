// packages/cipher-client/src/__tests__/machine-identities.test.ts
//
// The TypeScript half of the one list of machine identities:
// `packages/envoy/internal/cipherclient/testdata/machine-identities.json` names each kind
// AGENT_SECRETS_MACHINE_IDENTITY takes and the variables it needs, in the order a client reads
// them, and the Go suite's `machine_identities_test.go` holds the Go client to the same file.

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { IDENTITY_VAR, machineIdentity } from "../environment";

/** The corpus, as both suites read it. */
interface MachineIdentities {
  readonly identities: readonly {
    readonly kind: string;
    readonly clients: readonly string[];
    readonly variables: readonly { readonly name: string; readonly example: string }[];
  }[];
}

const CORPUS = JSON.parse(
  readFileSync(
    new URL(
      "../../../envoy/internal/cipherclient/testdata/machine-identities.json",
      import.meta.url
    ),
    "utf8"
  )
) as MachineIdentities;

/** The kinds the corpus says this client takes, in its order. */
const KINDS = CORPUS.identities.filter((identity) => identity.clients.includes("typescript"));

// For each kind, machineIdentity given none of its variables refuses the first, given that one
// refuses the next, and given all of them builds the identity; and the refusal of a kind it does
// not know lists exactly the corpus's kinds. Every process has a HOME, where a device login's file
// is kept unless a variable names another.
test("every machine identity reads the corpus's variables in order", () => {
  expect(KINDS.length).toBeGreaterThan(0);
  for (const { kind, variables } of KINDS) {
    const env: Record<string, string> = { HOME: "/home/example", [IDENTITY_VAR]: kind };
    for (const { name, example } of variables) {
      expect(() => machineIdentity(env)).toThrow(`${name} is required by ${IDENTITY_VAR}=${kind}`);
      env[name] = example;
    }
    expect(machineIdentity(env)).toBeDefined();
  }
  const kinds = KINDS.map(({ kind }) => kind);
  expect(() => machineIdentity({ [IDENTITY_VAR]: "no-such-kind" })).toThrow(
    `${IDENTITY_VAR} names no identity this machine can prove; it takes ${kinds.slice(0, -1).join(", ")} or ${kinds.at(-1)}`
  );
});
