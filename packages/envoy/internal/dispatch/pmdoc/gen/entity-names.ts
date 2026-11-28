// differential.ts calls engineEntityNames in --entity-names mode to test every name the editor
// decodes. Resolve each dependency from the package that imports it so the test reads the same
// copy of character-entities that the editor uses.
import { createRequire } from "node:module";

/** Loads the names from the same dependency that the engine uses. */
export async function engineEntityNames(): Promise<string[]> {
  let from = createRequire(import.meta.path).resolve("@legion/proof-editor");
  for (const name of [
    "remark-parse",
    "mdast-util-from-markdown",
    "micromark",
    "decode-named-character-reference",
    "character-entities",
  ]) {
    from = createRequire(from).resolve(name);
  }
  // The path is known only at run time, so a static import cannot name it.
  const { characterEntities } = (await import(from)) as {
    characterEntities: Record<string, string>;
  };
  return Object.keys(characterEntities);
}
