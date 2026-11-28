/**
 * A typed block's `block[]` attribute: the blocks it points at, as canonical block references.
 * Which values it may hold, and why one is refused, is decided by `@legion/contracts`
 * (`parseBlockPointerValue`, `blockPointerValueProblem`, `parseCanonicalBlockReference`), the
 * parsers the document service's twins are tested against with the same table, so the editor and
 * the service never disagree on a value. A pointer to a block of the editor's own document is
 * also refused when the block's type is not one the attribute's `targets` allow; a block of
 * another document is the service's to check, since only it knows that document's blocks. The
 * refusals follow the service's wording (pmdoc `ParseBlockPointerValue` and `CheckBlockPointers`)
 * but are not byte for byte the same: a value is quoted with `JSON.stringify` where Go uses `%q`,
 * which escapes a control or non-printing character differently, and a type refusal carries no
 * `block pointer refused:` prefix.
 */

import {
  type BlockItemKind,
  blockPointerValueProblem,
  hasControlCharacter,
  PARENT_ATTRIBUTE,
  PROJECT_KEY_PATTERN,
  parseBlockPointerValue,
  parseCanonicalBlockReference,
} from "@legion/contracts";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type { BlockTypeSchema } from "./block-schema";
import { blockIdOf } from "./editor/schema/block-ids";

// An issue key, as Go's `text` grammar reads one: a project key, `-` and a number from 1.
const PROJECT_KEY = PROJECT_KEY_PATTERN.source.slice(1, -1);
const ISSUE_KEY = `${PROJECT_KEY}-[1-9][0-9]*`;
const ISSUE_KEY_ONLY = new RegExp(`^${ISSUE_KEY}$`);
// An ask of an issue, `dispatch://<ISSUE>/ask/<id>`, and a part of one, which a `#b-` fragment
// follows. The id holds no `/`, `?`, `#` or whitespace, as Go's `itemSegment` reads it, and no
// character a reference ends at (`referenceEnd`: quotes, angle and square brackets, a backtick,
// `|`).
const ASK_REFERENCE = new RegExp(`^dispatch://(${ISSUE_KEY})/ask/([^/?#\\s'"<>\`\\[\\]|]+)$`);
const ASK_PART_REFERENCE = new RegExp(`^dispatch://${ISSUE_KEY}/ask/[^/?#\\s]+#b-`);
// What Go's `trimReference` takes off a reference's end: trailing punctuation, and a `)` no `(`
// opened.
const TRIMMED_END = /[.,;:!?*_~]$/;
// The owner key of a document's unversioned reference: an issue's or a project's.
const DOCUMENT_OWNER = new RegExp(`^dispatch://(${PROJECT_KEY}(?:-[1-9][0-9]*)?)/`);

/**
 * The issue key of `value` when it is an `ask` attribute's value as the document service takes
 * one (pmdoc `validateAskReference`): one ask of an issue, `dispatch://<ISSUE>/ask/<id>`, written
 * whole as every writer writes its address, the id needing no decoding. Undefined otherwise.
 */
export function askReferenceIssue(value: string): string | undefined {
  const match = ASK_REFERENCE.exec(value);
  const id = match?.[2] ?? "";
  const unopened =
    id.endsWith(")") && (id.match(/\(/g)?.length ?? 0) < (id.match(/\)/g)?.length ?? 0);
  if (match === null || hasControlCharacter(value) || TRIMMED_END.test(id) || unopened) {
    return undefined;
  }
  let decoded: string;
  try {
    decoded = decodeURIComponent(id);
  } catch {
    return undefined;
  }
  return decoded === id ? match[1] : undefined;
}

/**
 * Why `value` is no `ask` attribute's value (`askReferenceIssue`), in the document service's
 * wording (pmdoc `validateAskReference`, the value quoted with `JSON.stringify` where Go uses
 * `%q`), or undefined when it is one. A part of an ask is refused with a reason of its own: it
 * names a block of the ask's text, not the ask a decision settles.
 */
export function askReferenceProblem(value: string): string | undefined {
  if (ASK_PART_REFERENCE.test(value)) {
    return `${JSON.stringify(value)} is a part of an ask: an ask attribute names the ask itself, dispatch://<ISSUE>/ask/<id>`;
  }
  if (askReferenceIssue(value) === undefined) {
    return `${JSON.stringify(value)} is not an ask of an issue as Dispatch writes one: dispatch://<ISSUE>/ask/<id>`;
  }
  return undefined;
}

/**
 * The first block of `doc` whose id is `id`, and its position, or null when no block has it. The
 * first holder of a repeated id is the block, as the document service reads it.
 */
export function findBlock(
  doc: ProseMirrorNode,
  id: string
): { node: ProseMirrorNode; pos: number } | null {
  let found: { node: ProseMirrorNode; pos: number } | null = null;
  doc.descendants((node, pos) => {
    if (found !== null) {
      return false;
    }
    if (blockIdOf(node) !== id) {
      return true;
    }
    found = { node, pos };
    return false;
  });
  return found;
}

/**
 * The pointers the typed block `typeName`'s `block[]` attribute `name`, declaring the item kinds
 * `items`, holds when markdown or a pasted block writes it as `value`: none for the empty string,
 * else canonical references joined by exactly one space. Any other value is refused, never
 * normalised, and so is a parent holding more than one (`parentProblem`).
 */
export function parseBlockPointers(
  typeName: string,
  name: string,
  value: string,
  items?: readonly BlockItemKind[]
): string[] {
  const pointers = parseBlockPointerValue(value, items);
  const problem =
    pointers === undefined ? blockPointerValueProblem(value, items) : parentProblem(name, pointers);
  if (problem !== undefined || pointers === undefined) {
    throw new Error(
      `typed block ${JSON.stringify(typeName)} attribute ${JSON.stringify(name)}: ${problem}`
    );
  }
  return pointers;
}

/**
 * Why `pointers`, the references attribute `name` holds, are no value of it, in the document
 * service's wording (pmdoc `validateAttributeValue`): the parent (PARENT_ATTRIBUTE) holds at most
 * one, because a block has one parent. Undefined for every other attribute and for one parent.
 */
export function parentProblem(name: string, pointers: readonly string[]): string | undefined {
  if (name !== PARENT_ATTRIBUTE || pointers.length <= 1) {
    return undefined;
  }
  return `holds ${pointers.length} references; a block has one parent, so it holds at most one`;
}

/**
 * Why a command may not leave `doc` with a typed block of `type` holding the attribute values
 * `attrs`, or undefined when it may. `prior` is the block the command replaces or changes (null
 * for a new one); its id names the block in the refusal, and its attributes are the pointers
 * already held when it is of `type` too, since a retype from another type that declares the same
 * name may narrow its `targets`. A `block[]` value the command introduces - one the prior block
 * does not already hold in that attribute - is refused when it names a block of `doc` whose type
 * the attribute's `targets` do not allow. A pointer names a block of `doc` when its document is
 * `documentRef`, the document's unversioned reference; without one nothing is refused here. A
 * pointer to a block `doc` lacks is accepted, as the service accepts one to an absent block.
 */
export function blockPointerRefusal(
  doc: ProseMirrorNode,
  type: BlockTypeSchema,
  prior: ProseMirrorNode | null,
  attrs: Readonly<Record<string, unknown>>,
  documentRef: string | undefined
): string | undefined {
  if (documentRef === undefined) {
    return undefined;
  }
  const block = prior === null ? null : blockIdOf(prior);
  const held: Readonly<Record<string, unknown>> =
    prior !== null && prior.type.name === type.name ? prior.attrs : {};
  for (const name of Object.keys(attrs).sort()) {
    const targets = type.attributes[name]?.targets ?? [];
    const values = attrs[name];
    if (
      type.attributes[name]?.kind !== "block[]" ||
      targets.length === 0 ||
      !Array.isArray(values)
    ) {
      continue;
    }
    const heldValues: unknown = held[name];
    for (const value of values as readonly string[]) {
      const reference = parseCanonicalBlockReference(value);
      if (
        reference === undefined ||
        value.slice(0, value.indexOf("#")) !== documentRef ||
        (Array.isArray(heldValues) && heldValues.includes(value))
      ) {
        continue;
      }
      const target = findBlock(doc, reference.block)?.node.type.name;
      if (target !== undefined && !targets.includes(target)) {
        const at = block === null ? "" : ` #${block}`;
        return `typed block ${JSON.stringify(type.name)}${at} attribute ${JSON.stringify(name)} points at ${value}, a ${JSON.stringify(target)} block; it points only at ${targets.join(", ")}`;
      }
    }
  }
  return undefined;
}

/**
 * Why a typed block of `type` holding the attribute values `attrs` may not stand in the document
 * `documentRef` (its unversioned reference), or undefined when it may: it names something of
 * another issue than the document's own. The document service refuses such a tie on every write
 * that takes markdown - an upload, an editor operation, an issue's first document, a rebuild -
 * quoting the attribute and the value (pmdoc.CheckIssueTies); a browser's write it never refuses,
 * and settlement stores the block with no tuple and no row for the tie to the other issue, while
 * a tie of the same block inside the issue stands. A `block[]` attribute with no `targets` and no
 * `items` (a decision's or an ask's `shaped`) points at a block of any document of the document's
 * own issue, and an `ask` attribute (a decision's `from_ask`) names an ask of that issue. One
 * declaring `targets` or `items` (a parent) points at a block of an issue above as well, which
 * only the document service can read, so nothing here refuses it. A command and a paste are both
 * held to it, so neither builds a document the service refuses or a tie it drops. Without
 * `documentRef`, or for a document no issue owns, nothing is refused here.
 */
export function otherIssueRefusal(
  type: BlockTypeSchema,
  attrs: Readonly<Record<string, unknown>>,
  documentRef: string | undefined
): string | undefined {
  const owner = documentRef === undefined ? undefined : DOCUMENT_OWNER.exec(documentRef)?.[1];
  if (owner === undefined || !ISSUE_KEY_ONLY.test(owner)) {
    return undefined;
  }
  for (const name of Object.keys(attrs).sort()) {
    const attribute = type.attributes[name];
    const value = attrs[name];
    if (attribute?.kind === "ask" && typeof value === "string") {
      const issue = askReferenceIssue(value);
      if (issue !== undefined && issue !== owner) {
        return `typed block ${JSON.stringify(type.name)} attribute ${JSON.stringify(name)} names ${value}, an ask of ${issue}; it names only an ask of this document's issue, ${owner}`;
      }
      continue;
    }
    if (
      attribute?.kind !== "block[]" ||
      (attribute.targets?.length ?? 0) > 0 ||
      (attribute.items?.length ?? 0) > 0 ||
      !Array.isArray(value)
    ) {
      continue;
    }
    for (const pointer of value as readonly string[]) {
      const reference = parseCanonicalBlockReference(pointer);
      const issue = reference?.issue ?? reference?.project;
      if (reference !== undefined && issue !== owner) {
        return `typed block ${JSON.stringify(type.name)} attribute ${JSON.stringify(name)} points at ${pointer}, a block of ${issue}; it points only at a block of this document's issue, ${owner}`;
      }
    }
  }
  return undefined;
}
