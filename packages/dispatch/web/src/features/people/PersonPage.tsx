import { useInfiniteQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import { api } from "../../api/client";
import { nextOffsetPage } from "../../api/queries";
import type { PersonNode } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import { ShowMore } from "../../components/ShowMore";
import {
  borderDefault,
  card,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { PostPreview } from "../refs/PostBody";
import { EditedMark } from "../refs/PostMarks";
import { postDeletion } from "../refs/post-marks";
import { parseIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceKind, textSourceOf } from "../refs/text-references";
import { useDocumentTitle } from "../shell/useDocumentTitle";

const PAGE_SIZE = 50;

/** A row of the person nodes read as a server of either release answers it: this build's carries
 *  the line as `first_line` and again as `title`, and a server of the release before answers it as
 *  `title` alone, which the page reads while a roll or a rollback leaves such a pod serving.
 *
 *  kept for one release: person-node-title
 */
type PersonNodeRead = Omit<PersonNode, "first_line"> & { readonly first_line?: string };

/** What each tie reads as on the page. A row carries one or both: a comment a person wrote that
 *  was then delivered back to a session of theirs is authored and received at once. */
const RELATION_LABELS: Record<string, string> = {
  authored_by: "Authored",
  delivered_to: "Received",
};

/** `/people/<login>`: one person's page, from one `GET /api/v1/people/{login}/nodes` — what they
 *  wrote and what reached them, newest first, read from the node and tuple store. Every row is
 *  re-read from that route: the page holds no list of its own and walks no edge, because the
 *  route takes no kind, filter or traversal. A comment or an issue message its author deleted
 *  keeps its row, drawn as the deleted line in place of its first line (`DeletedBody`), and one
 *  its author edited says ` · edited` after its kind in the row's byline (`EditedMark`), as every
 *  surface that prints a post marks it (dispatch://INFRA-1523, R2 and R3). A row with neither
 *  mark draws exactly what it drew before the marks. */
export function PersonPage(): ReactNode {
  const { login = "" } = useParams();
  useDocumentTitle(`${login} · Dispatch`);
  const nodes = useInfiniteQuery({
    getNextPageParam: nextOffsetPage,
    initialPageParam: 0,
    queryFn: ({ pageParam }) => api.listPersonNodes(login, { limit: PAGE_SIZE, offset: pageParam }),
    queryKey: ["people", login, "nodes"],
  });

  if (nodes.isPending) return <LoadingSkeleton label="Loading this person's nodes" />;
  if (nodes.isError) {
    return <p className={dangerText}>Could not load this page: {nodes.error.message}</p>;
  }
  const rows: PersonNodeRead[] = nodes.data.pages.flatMap((page) => page.rows ?? []);

  return (
    <section aria-label="Person">
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to="/">
          ← Inbox
        </Link>
        <h1 className={`mt-1 text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}>
          {nodes.data.pages[0]?.person ?? login}
        </h1>
        <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>
          What this person wrote and what reached them, newest first.
        </p>
      </header>
      {rows.length === 0 ? (
        <EmptyState
          label="Person empty state"
          message="Nothing this person wrote and nothing delivered to them is recorded yet."
        />
      ) : (
        <ul aria-label="Person nodes" className="space-y-3">
          {rows.map((row) => {
            const deleted = postDeletion(row);
            // kept for one release: person-node-title
            const line = row.first_line ?? row.title;
            return (
              <li
                className={`rounded-xl border p-3 ${card} ${borderDefault}`}
                data-person-node={`${row.kind}:${row.id}`}
                key={`${row.kind}:${row.id}`}
              >
                <div className={`flex flex-wrap items-center gap-2 text-xs ${textMutedOnCanvas}`}>
                  <Timestamp at={row.created_at} />
                  <span>
                    {row.kind}
                    <EditedMark at={row.edited_at} deleted={deleted} />
                  </span>
                  {(row.relations ?? []).map((relation) => (
                    <LabelPill key={relation}>{RELATION_LABELS[relation] ?? relation}</LabelPill>
                  ))}
                </div>
                {/* The record's first line as the server cut it, drawn from the server's reading
                    of it (`first_line_tree`); a row a server of the release before answers
                    carries no tree, and the preview parses its line. */}
                <PostPreview
                  className="mt-2 text-sm"
                  deleted={deleted}
                  fullTitle
                  lines={2}
                  markdown={line}
                  source={textSourceOf(
                    textSourceKind(row.kind),
                    row.id,
                    // The row carries no issue key; a comment on an issue links to that issue's
                    // conversation, so a role reference in its line is read by that issue.
                    parseIssuePath(row.href)?.key
                  )}
                  surface="person-node-title"
                  tree={row.first_line_tree}
                />
                <Link
                  className={`mt-2 inline-block min-h-11 py-2 text-sm ${linkText} ${linkHoverText}`}
                  to={row.href}
                >
                  Open
                </Link>
              </li>
            );
          })}
        </ul>
      )}
      <ShowMore
        fetching={nodes.isFetchingNextPage}
        hasMore={nodes.hasNextPage}
        onFetch={() => void nodes.fetchNextPage()}
      />
    </section>
  );
}
