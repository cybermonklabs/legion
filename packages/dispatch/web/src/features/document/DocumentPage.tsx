import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { api, isSubscriberNotFound, removedEvenIfGone } from "../../api/client";
import type { Artifact } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { dangerText } from "../../theme/classes";
import { ArtifactDocument } from "../artifacts/ArtifactDocument";
import { ArtifactBlobView, ArtifactHeader } from "../artifacts/ArtifactHeader";
import { ArtifactQueryStates } from "../artifacts/ArtifactQueryStates";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { SubscribedAgents } from "../issue/SubscribedAgents";
import { useAgentText } from "../refs/AgentName";
import { ReferencedBy } from "../refs/ReferencedBy";
import {
  buildDispatchReference,
  buildProjectPath,
  documentRoute as documentReferenceRoute,
  parseProjectPath,
} from "../refs/routes";
import { firstHighlightTerm } from "../search/search-model";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { useProjectArtifact } from "./useProjectArtifact";

/**
 * The document's subscribed agents, with the unsubscribe the reader makes from that list and the
 * refusal and Retry of that unsubscribe. All of it is the document's alone, and the page stays
 * mounted as the reader moves to another document - by a link in its text, or by Referenced by -
 * so the page keys this by the document (`DocumentPage`): each document is drawn with an
 * unsubscribe of its own, never under another document's refusal with that document's
 * unsubscribe as its Retry.
 */
function DocumentSubscribers({ artifact }: { readonly artifact: Artifact }): ReactNode {
  const queryClient = useQueryClient();
  const subscribers = useQuery({
    queryKey: ["subscribers", artifact.id],
    queryFn: () => api.getArtifactSubscribers(artifact.id),
  });
  const agentText = useAgentText();
  // The press carries the document it unsubscribes from, its id and its name: the route takes the
  // id, and the sentence of a stopped unsubscribe the name, from the press rather than from the
  // render the write runs under.
  const unsubscribe = useKeyedWrite({
    label: ({ name, sessionId }: { artifactId: string; name: string; sessionId: string }) =>
      `${agentText.sessionText(sessionId, subscribers.data?.find((row) => row.session_id === sessionId)?.title)} unsubscribed from ${name}`,
    // An unsubscribe made again after one that landed finds the session no longer subscribed
    // (`404 SUBSCRIBER_NOT_FOUND`), which is the unsubscribe done (`removedEvenIfGone`).
    method: "DELETE",
    write: async ({ artifactId, sessionId }, bounds) => {
      await removedEvenIfGone(
        api.unsubscribeArtifactSession(artifactId, sessionId, bounds),
        isSubscriberNotFound
      );
    },
    onSuccess: (_result, { artifactId }) =>
      void queryClient.invalidateQueries({ queryKey: ["subscribers", artifactId] }),
  });

  return (
    <>
      <SubscribedAgents
        onUnsubscribe={(sessionId) =>
          unsubscribe.mutate({ artifactId: artifact.id, name: artifact.name, sessionId })
        }
        ownerLabel={`${artifact.project}/${artifact.slug}`}
        subscribers={subscribers.isError ? [] : (subscribers.data ?? [])}
      />
      {subscribers.isError ? (
        <QueryError
          error={subscribers.error}
          message="Subscribed agents unavailable — Envoy listener unreachable."
          onRetry={() => subscribers.refetch()}
          retrying={subscribers.isFetching}
        />
      ) : null}
      {unsubscribe.isError ? (
        <QueryError
          message={
            stoppedNote(
              unsubscribe,
              `this agent's unsubscribe from “${(unsubscribe.variables ?? artifact).name}”`,
              "made"
            ) ?? "Could not unsubscribe this agent."
          }
          onRetry={unsubscribe.retry}
          retrying={unsubscribe.isPending}
        />
      ) : null}
    </>
  );
}

export function DocumentPage(): ReactNode {
  const location = useLocation();
  const navigate = useNavigate();
  const route = parseProjectPath(location.pathname, location.search);
  const documentRoute = route?.kind === "document" ? route : undefined;
  const [showDiff, setShowDiff] = useState(false);
  const [toolbar, setToolbar] = useState<DocumentToolbar | undefined>(undefined);
  const handleToolbarChange = useCallback((next: DocumentToolbar | undefined) => {
    setToolbar(next);
    if (next === undefined) {
      setShowDiff(false);
    }
  }, []);
  const artifact = useProjectArtifact(documentRoute);
  const query = new URLSearchParams(location.search);
  const highlightTerm = firstHighlightTerm(query.get("q") ?? "");
  const project = documentRoute?.project;
  // The title bar holds no markup, so a document's name goes in as the characters it is; the
  // reading list's `document-page-title` (`DISPATCH_READING_SURFACES`) says why.
  useDocumentTitle(
    artifact.data === undefined || project === undefined
      ? "Document · Dispatch"
      : `${artifact.data.name} · ${project} · Dispatch`
  );

  if (documentRoute === undefined) {
    return null;
  }
  if (artifact.isPending || artifact.isError) {
    return (
      <ArtifactQueryStates
        errorMessage="Could not load this document."
        loadingLabel="Loading document…"
        notFoundLabel="Document not found"
        notFoundLinkText={`Back to ${documentRoute.project} documents`}
        notFoundTo={buildProjectPath({ kind: "documents", project: documentRoute.project })}
        query={artifact}
      />
    );
  }
  if (artifact.data === undefined) {
    return <p className={dangerText}>Could not load this document.</p>;
  }

  const owner = {
    artifactId: artifact.data.id,
    kind: "document" as const,
    project: documentRoute.project,
    slug: documentRoute.slug,
  };
  const version = documentRoute.version;
  const item = documentRoute.item;
  const selectVersion = (nextVersion: number | null) => {
    setShowDiff(false);
    navigate(
      buildProjectPath({
        kind: "document",
        project: documentRoute.project,
        slug: documentRoute.slug,
        ...(nextVersion === null ? {} : { version: nextVersion }),
      })
    );
  };

  return (
    <section className="space-y-6">
      <ArtifactHeader
        artifact={artifact.data}
        highlight={false}
        isClosed={false}
        onShowDiffChange={setShowDiff}
        showDiff={showDiff}
        showVersionPicker
        toolbar={artifact.data.kind === "doc" ? toolbar : undefined}
        version={version}
      >
        <DocumentSubscribers artifact={artifact.data} key={artifact.data.id} />
        {artifact.data.kind === "doc" ? (
          <ArtifactDocument
            artifact={artifact.data}
            askId={item?.kind === "ask" ? item.id : undefined}
            commentId={item?.kind === "comment" ? item.id : undefined}
            highlightTerm={highlightTerm}
            isClosed={false}
            onToolbarChange={handleToolbarChange}
            onVersionChange={selectVersion}
            owner={owner}
            showDiff={showDiff}
            version={version}
          />
        ) : (
          <ArtifactBlobView artifact={artifact.data} version={version} />
        )}
      </ArtifactHeader>
      <ReferencedBy reference={buildDispatchReference(documentReferenceRoute(artifact.data))} />
    </section>
  );
}
