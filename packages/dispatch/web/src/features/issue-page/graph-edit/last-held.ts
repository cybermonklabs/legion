import { blockFragment } from "../../refs/block-reference";
import { buildIssuePath, parseNodeReference } from "../../refs/routes";

/**
 * The address on the new issue page that a citation of a deleted node opens (R9 of INFRA-5705's
 * spec): the version that last held the node, as the server names it in `last_held` of the
 * batched reference read's member for a gone block (`GET /api/v1/reference-targets`).
 *
 * The server writes `last_held` in the reference forms its own parser reads (`parseDispatch` in
 * `text/refs.go`): `dispatch://<KEY>/spec#b-<id>@v<N>` for a node the spec query held,
 * `dispatch://<KEY>/artifact/verification-md#b-<id>@v<N>` or `…/artifact/plan-md#b-<id>@v<N>` for
 * one the Verification or the plan query held, and `dispatch://<KEY>/artifact/<slug>#b-<id>@v<N>`
 * for any other block. It is read here by the page's reader of a node's reference
 * (`parseNodeReference`), never followed as written and never through `buildReferencePath`, which
 * opens a pinned block of a document at its old page.
 *
 * Imports nothing but the reference readers, so a row's words (`RowText.tsx`) read it with no
 * import of the model.
 */

/** The filter that draws a query's document at a version, by the slug the model gives that
 *  document (`model.ArtifactSlug*`), which `last_held` names it by as `artifact/<slug>`. The spec
 *  is named as `spec`, and every other slug is a document of its own: a slug is read as the
 *  table's own key alone (`Object.hasOwn`), so `constructor` is a document too. */
const QUERY_FILTERS: Readonly<Record<string, "plan" | "verification">> = {
  "plan-md": "plan",
  "verification-md": "verification",
};

/**
 * The page's address of the version a `last_held` address names: for a node of a query, that
 * query's filter at version N (`/issues/<KEY>/<spec | verification | plan>?v=<N>#b-<id>`), and for
 * any other block its document at version N (`/issues/<KEY>/artifacts/<slug>?v=<N>#b-<id>`), a
 * plain `artifact/spec` among them, with the slug and the block id each written by the page's own
 * encoder (`buildIssuePath`, `blockFragment`) and N written as the reference's digits. Undefined
 * for anything else: no address (`undefined`), an address that pins no version, one of no issue's
 * spec or artifact, and one the reader does not read.
 */
export function lastHeldPath(lastHeld: string | undefined): string | undefined {
  if (lastHeld === undefined) return undefined;
  const route = parseNodeReference(lastHeld);
  if (route === undefined || (route.kind !== "spec" && route.kind !== "artifact")) return undefined;
  const block = route.block;
  if (block?.pin === undefined) return undefined;
  const filter =
    route.kind === "artifact" && Object.hasOwn(QUERY_FILTERS, route.slug)
      ? QUERY_FILTERS[route.slug]
      : undefined;
  const page =
    route.kind === "spec"
      ? buildIssuePath({ key: route.key, kind: "spec" })
      : filter === undefined
        ? buildIssuePath({ key: route.key, kind: "artifact", slug: route.slug })
        : buildIssuePath({ key: route.key, kind: filter });
  return `${page}?v=${block.pin}${blockFragment(block.id)}`;
}
