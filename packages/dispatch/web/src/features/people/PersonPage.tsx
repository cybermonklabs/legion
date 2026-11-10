import { useInfiniteQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import { api } from "../../api/client";
import type { PersonNodes } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import {
  borderDefault,
  card,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { Timestamp } from "../refs/Timestamp";
import { useDocumentTitle } from "../shell/useDocumentTitle";

const PAGE_SIZE = 50;

/** What each tie reads as on the page. A row carries one or both: a comment a person wrote that
 *  was then delivered back to a session of theirs is authored and received at once. */
const RELATION_LABELS: Record<string, string> = {
  authored_by: "Authored",
  delivered_to: "Received",
};

/** `/people/<login>`: one person's page, from one `GET /api/v1/people/{login}/nodes` — what they
 *  wrote and what reached them, newest first, read from the node and tuple store. Every row is
 *  re-read from that route: the page holds no list of its own and walks no edge, because the
 *  route takes no kind, filter or traversal. */
export function PersonPage(): ReactNode {
  const { login = "" } = useParams();
  useDocumentTitle(`${login} · Dispatch`);
  const nodes = useInfiniteQuery({
    getNextPageParam: (last: PersonNodes) =>
      last.offset + (last.rows ?? []).length < last.total
        ? last.offset + (last.rows ?? []).length
        : undefined,
    initialPageParam: 0,
    queryFn: ({ pageParam }) => api.listPersonNodes(login, { limit: PAGE_SIZE, offset: pageParam }),
    queryKey: ["people", login, "nodes"],
  });

  if (nodes.isPending) return <LoadingSkeleton label="Loading this person's nodes" />;
  if (nodes.isError) {
    return <p className={dangerText}>Could not load this page: {nodes.error.message}</p>;
  }
  const rows = nodes.data.pages.flatMap((page) => page.rows ?? []);

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
          {rows.map((row) => (
            <li
              className={`rounded-xl border p-3 ${card} ${borderDefault}`}
              key={`${row.kind}:${row.id}`}
            >
              <div className={`flex flex-wrap items-center gap-2 text-xs ${textMutedOnCanvas}`}>
                <Timestamp at={row.created_at} />
                <span>{row.kind}</span>
                {(row.relations ?? []).map((relation) => (
                  <LabelPill key={relation}>{RELATION_LABELS[relation] ?? relation}</LabelPill>
                ))}
              </div>
              <MarkdownPreview className="mt-2 text-sm" fullTitle lines={2} markdown={row.title} />
              <Link
                className={`mt-2 inline-block min-h-11 py-2 text-sm ${linkText} ${linkHoverText}`}
                to={row.href}
              >
                Open
              </Link>
            </li>
          ))}
        </ul>
      )}
      {nodes.hasNextPage ? (
        <button
          className={`mt-4 min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${linkText} ${linkHoverText}`}
          disabled={nodes.isFetchingNextPage}
          onClick={() => void nodes.fetchNextPage()}
          type="button"
        >
          {nodes.isFetchingNextPage ? "Loading…" : "Show more"}
        </button>
      ) : null}
    </section>
  );
}
