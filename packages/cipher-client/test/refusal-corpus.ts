// packages/cipher-client/test/refusal-corpus.ts
//
// The broker's refusals as its own OAuth library writes them on the wire: the library's fixed
// sentence, then the hint, or the broker's `<CODE>: ` ahead of its message. The Go broker's
// refusal-corpus test writes this file from the broker's library and checks each hint stands in
// its source, so a row built from it says what the broker sends.

import { readFileSync } from "node:fs";
import { join } from "node:path";

/** Every refusal of the broker's corpus, as the broker sends it. */
export const refusalCorpus: readonly string[] = JSON.parse(
  readFileSync(
    join(import.meta.dir, "../../envoy/internal/cipher/oauth/testdata/refusal-descriptions.json"),
    "utf8"
  )
);

/** The one refusal of the corpus that ends with `hint`, as the broker sends it. */
export function sentAs(hint: string): string {
  const found = refusalCorpus.filter((one) => one.endsWith(hint));
  if (found.length !== 1) {
    throw new Error(`the refusal corpus holds ${found.length} refusals ending "${hint}"`);
  }
  return found[0] as string;
}
