import {
  hasControlCharacter,
  MAX_BLOCK_ID_BYTES,
  referenceTargetFits,
  utf8Bytes,
} from "@legion/contracts";

import { artifactSlugPattern } from "./artifact-slug";
import { blockFragment, parseBlockReference } from "./block-reference";

/**
 * The kinds of node a reference names by the node's own id in the store, with no issue key or
 * project in the address: where such a node stands is a tuple, so a move changes no address. An
 * interaction, a lens, a risk and a constraint are `dispatch://<kind>/<uuid>`, the uuid as the
 * store writes it, and a lens kind is `dispatch://lens_kind/<slug>`, its slug as an artifact's is
 * written. A finding of a lens is a block of the lens, `dispatch://lens/<uuid>#b-<block id>`, and a
 * lens records no revision of its findings, so a pinned one names nothing. This is the twin of the
 * server's `text.parseStoreAddress` (`internal/dispatch/text/refs.go`), and both are tested
 * against the shared text table (`DISPATCH_TEXT_REFERENCES`).
 *
 * No route of `routes.ts` names one, so a reference to one is a reference without a route, which
 * text cites as the server indexes it, and which today's page links nowhere. The new issue page
 * writes one in a row's words as its node's name, linked to that node's row, where its tree read
 * holds the node, and as its kind's word otherwise (`issue-page/tree-drafts.ts`'s
 * `addressesAsShortForms`, `issue-page/lenses/store-words.ts`).
 */
const STORE_ADDRESS = {
  /** The kinds addressed by a uuid. */
  uuidKinds: ["interaction", "lens", "risk", "constraint"],
  /** The kind addressed by its slug. */
  slugKind: "lens_kind",
  /** The kind whose blocks a reference names: a finding of a lens. */
  blockKind: "lens",
} as const;

/** A uuid as the store writes one: lowercase hex in five hyphenated groups. */
const storeUuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** A lens kind's slug, written as an artifact's slug is. */
const storeSlug = new RegExp(`^${artifactSlugPattern}$`);

/** Whether `id` is the whole id of a node of `kind` addressed by its store id alone. */
function isStoreId(kind: string, id: string): boolean {
  if ((STORE_ADDRESS.uuidKinds as readonly string[]).includes(kind)) {
    return storeUuid.test(id);
  }
  return kind === STORE_ADDRESS.slugKind && storeSlug.test(id);
}

/**
 * The reference `value` names when it is a `dispatch://` reference to a node addressed by its
 * store id alone, written canonically (a finding's block id as `encodeBlockId` writes it), or
 * undefined when it names no such node. Text cites one only within the bounds the graph's index
 * keys, as the server's `text.inBound` reads them: a finding's block id of at most
 * MAX_BLOCK_ID_BYTES, and a target id (`<kind id>`, or `lens/<uuid>#<block id>` for a finding)
 * that fits.
 */
export function storeAddressReference(value: string): string | undefined {
  if (!value.startsWith("dispatch://") || hasControlCharacter(value)) {
    return undefined;
  }
  const rest = value.slice("dispatch://".length);
  const hash = rest.indexOf("#");
  const address = hash === -1 ? rest : rest.slice(0, hash);
  const slash = address.indexOf("/");
  if (slash === -1) {
    return undefined;
  }
  const kind = address.slice(0, slash);
  const id = address.slice(slash + 1);
  if (!isStoreId(kind, id)) {
    return undefined;
  }
  if (hash === -1) {
    return referenceTargetFits(id) ? `dispatch://${kind}/${id}` : undefined;
  }
  if (kind !== STORE_ADDRESS.blockKind) {
    return undefined;
  }
  const target = parseBlockReference(address, rest.slice(hash + 1));
  if (
    target === undefined ||
    target.pin !== undefined ||
    utf8Bytes(target.block) > MAX_BLOCK_ID_BYTES ||
    !referenceTargetFits(`${kind}/${id}#${target.block}`)
  ) {
    return undefined;
  }
  return `dispatch://${kind}/${id}${blockFragment(target.block)}`;
}
