import type { BlockElementVersions, BlockSchema } from "../../api/types";

/**
 * R for `schema`: the highest `since` among the elements an editor of `schema.version` may create
 * (`creatable_since` at most that version), each number 1 where the manifest names none. A build
 * whose version is R or more reads everything this editor writes. It is Go's
 * `pmdoc.SchemaCreatable()` for the same manifest.
 *
 * It is a module of its own, apart from `schema.ts`, because that module imports the manifest JSON
 * with no import attribute: Vite and Bun take that, and Node, which loads the browser suite's spec
 * files, refuses it. A spec that computes R for another build's manifest imports this one.
 */
export function schemaCreatable(schema: BlockSchema): number {
  let creatable = 0;
  const count = (element: BlockElementVersions | undefined) => {
    if ((element?.creatable_since ?? 1) <= schema.version) {
      creatable = Math.max(creatable, element?.since ?? 1);
    }
  };
  for (const element of [...(schema.nodes ?? []), ...(schema.marks ?? [])]) {
    count(element);
  }
  for (const type of schema.types) {
    count(type);
    for (const attribute of Object.values(type.attributes)) {
      count(attribute);
      for (const choice of attribute.choices ?? []) {
        count(attribute.choice_versions?.[choice]);
      }
      for (const item of attribute.items ?? []) {
        count(attribute.item_versions?.[item]);
      }
    }
  }
  return creatable;
}
