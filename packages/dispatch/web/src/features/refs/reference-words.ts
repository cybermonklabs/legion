/** The words an element the `RefLink` portal draws into held before `collectReferenceAnchors`
 *  cleared them (`ReferenceAnchor.text`): a bare reference as its author wrote it, or a link's own
 *  words, none for the link of a picture whose reference names no artifact. `RefLink` may draw a
 *  name in their place; a surface that counts its words as their author wrote them reads these
 *  (`issue-page/row-references.ts`'s `markDrawnAtoms`). It stands in a module of its own, which
 *  imports nothing, so a reader of the mark loads none of `RefLink.tsx`'s rendering. */
export const REFERENCE_WORDS = "data-dispatch-words";
