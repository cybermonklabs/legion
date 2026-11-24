import * as Y from "yjs";

/**
 * The attributes the editor derives from the document rather than takes from a person, by the node
 * that carries them. Milkdown's `syncHeadingIdPlugin` slugs every heading's id from its text as it
 * loads the document, and its `syncListOrderPlugin` labels every ordered list item with its number
 * and types it as ordered on the first caret move or edit. No rendering carries either, so the
 * server settles an update that changes only these without a version (envoy's
 * `TestAttributesTheEditorDerivesSettleWithoutAVersion`), and the next load derives them again.
 * The pending-edit store (`pending-sync.ts`) and the browser suite's visual write guard
 * (`e2e/visual-guard.ts`) both read an update by this table.
 */
const DERIVED_ATTRIBUTES: Record<string, readonly string[]> = {
  heading: ["id"],
  list_item: ["label", "listType"],
};

/**
 * Whether a transaction changed nothing but `DERIVED_ATTRIBUTES`. Yjs lists each type that existed
 * before the transaction and that it changed, with the attribute keys it set or removed and `null`
 * for a change to the type's children or text. A type the transaction created is inserted into one
 * that existed, so it shows as that parent's `null`.
 */
export function changesOnlyDerived(transaction: Y.Transaction): boolean {
  if (transaction.changed.size === 0) {
    return false;
  }
  for (const [type, keys] of transaction.changed) {
    // `Object.hasOwn`, not indexing alone: the table inherits `Object.prototype`, so a node named
    // `constructor` would read a function.
    const attributes =
      type instanceof Y.XmlElement && Object.hasOwn(DERIVED_ATTRIBUTES, type.nodeName)
        ? DERIVED_ATTRIBUTES[type.nodeName]
        : undefined;
    if (attributes === undefined) {
      return false;
    }
    for (const key of keys) {
      if (key === null || !attributes.includes(key)) {
        return false;
      }
    }
  }
  return true;
}
