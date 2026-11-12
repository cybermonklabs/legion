import type { z } from "zod";
import type { components, SplitOwnedSchemaName } from "./dispatch-api.generated";
import {
  AskAnchorArtifactSchema,
  AskBlockArtifactSchema,
  AskOptionSchema,
  InboxDocumentSchema,
} from "./dispatch-api-asks";
import { SuggestionSchema } from "./dispatch-api-comments";
import {
  ActorOriginSchema,
  AnchorSchema,
  BlockPathEntrySchema,
  ExternalLinkSchema,
} from "./dispatch-api-core";
import { AskApprovalDocumentSchema } from "./dispatch-api-documents";
import { ReplyBlockSchema } from "./dispatch-api-refs";

/**
 * What holds the eleven shapes the split owns to the shapes the server's document declares.
 *
 * Those eleven are exported as `z.output` of a zod schema rather than as the generated alias,
 * because the two spellings are the same type. Before that, each name *was* the document's type
 * and could not drift from it; after it, the equality is a fact about today's two definitions.
 * A probe run once says nothing about the next regeneration: a field whose Go type changes is
 * rendered into `components` by `bun run gen:api` and leaves the schema beside it alone, and the
 * split would then read a shape the route table no longer answers with, with nothing to say so.
 *
 * This file is what says so. `scripts/gen-api.ts` renders the same list it filters the aliases by
 * into the generated module as `SplitOwnedSchemaName`, the mapping below answers that union with
 * the schema reading each name, and `Mutual` compares the two readings in both directions. The
 * assertion is compiled, not run, so `bunx tsc --noEmit` and every package that builds against
 * `@legion/contracts` reds on a regeneration that moves one of these shapes, naming it.
 *
 * What the union cannot miss: a name the generator's table holds and the document declares is in
 * it by construction, since one list renders both the filtered aliases and the union. What it
 * misses is a name the table does not hold - a type written here beside a schema the document
 * also declares, with no entry in `OWNED_BY_THE_SPLIT` - which `dispatch-api.ts` refuses in its
 * own way, because the generated alias is then rendered too and one name cannot come from two
 * modules (`TS2308`). A name the table holds that the document does not declare is in neither the
 * aliases nor the union, and the mapping below then has a key the union does not carry, which
 * reds here.
 */
const OWNED_SCHEMAS = {
  ActorOrigin: ActorOriginSchema,
  Anchor: AnchorSchema,
  AskAnchorArtifact: AskAnchorArtifactSchema,
  AskApprovalDocument: AskApprovalDocumentSchema,
  AskBlockArtifact: AskBlockArtifactSchema,
  AskOption: AskOptionSchema,
  BlockPathEntry: BlockPathEntrySchema,
  ExternalLink: ExternalLinkSchema,
  InboxDocument: InboxDocumentSchema,
  ReplyBlock: ReplyBlockSchema,
  Suggestion: SuggestionSchema,
} satisfies Record<SplitOwnedSchemaName, z.ZodType>;

/** `true` when each type carries the other, which is what makes two spellings one type here: a
 *  field one side alone declares, and a field whose type moves on one side, each break it. The
 *  tuples keep the comparison from distributing over a union member. */
type Mutual<Written, Declared> = [Written] extends [Declared]
  ? [Declared] extends [Written]
    ? true
    : false
  : false;

/** Each owned name against the shape the document declares for it. */
type OwnedAgreements = {
  [Name in SplitOwnedSchemaName]: Mutual<
    z.output<(typeof OWNED_SCHEMAS)[Name]>,
    components["schemas"][Name]
  >;
};

/** The assertion. A name whose two readings have drifted makes its property `false`, which this
 *  constraint refuses with the name in the error. */
type EveryOwnedNameAgrees<Agreements extends Record<SplitOwnedSchemaName, true>> = Agreements;

export type OwnedNamesAgreeWithTheDocument = EveryOwnedNameAgrees<OwnedAgreements>;
