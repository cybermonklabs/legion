import type { DocumentJSON } from "@legion/proof-editor";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { Doc } from "yjs";

import { isText } from "../../api/artifact-version";
import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { Ask } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { watchPicturePath } from "../refs/reference-target";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { AskBlockCard } from "./AskBlockCard";
import { type ChangeBaseline, changeMarksAgainst } from "./change-marks";
import { colorForLogin } from "./connection";
import { DocumentReferences, useReferenceClick } from "./document-references";
import { createDoc, type EditorHandle } from "./editor";
import type { Highlight } from "./highlight";
import { embedHighlight } from "./highlight";
import { refocusWhenSettled } from "./refocus-when-settled";
import { DocumentRuntime } from "./runtime";
import { compiledBlockSchema } from "./schema";
import type { AskBlockHost } from "./typed-block-drawing";

export interface VersionViewProps {
  artifactId: string;
  /** The document's block-indexed asks, so a historical decision block still names its asker. */
  asks: readonly Ask[];
  /** The version this one is marked against - the last approved one - with which the view draws
   *  what changed since it: the words each changed block gained, and each block removed since
   *  then where it stood. It carries the reading Go made of that version, in its version mode,
   *  which keeps the attributes the server wrote into that text, since a decision answered in a
   *  version is part of what that version said; and its markdown, which the marks are computed
   *  from where the answer carried no reading (`changeMarksAgainst`). Without a baseline nothing
   *  is marked. */
  changeBaseline?: ChangeBaseline;
  createdAt: string | undefined;
  highlight: Highlight | undefined;
  /** The project document shown, for block asks with no issue to be referenced under. */
  owner: { project: string; slug: string } | undefined;
  version: number;
}

export function VersionView({
  artifactId,
  asks,
  changeBaseline,
  createdAt,
  highlight,
  owner,
  version,
}: VersionViewProps): ReactNode {
  const root = useRef<HTMLDivElement>(null);
  const [askBlockHosts, setAskBlockHosts] = useState<readonly AskBlockHost[]>([]);
  // The handle the version's references are drawn through: the titles are a render of their
  // own, so the surface that draws them re-renders with the editor it draws into.
  const [editorHandle, setEditorHandle] = useState<EditorHandle | undefined>(undefined);
  const [highlightMissing, setHighlightMissing] = useState(false);
  const [renderError, setRenderError] = useState<string | undefined>(undefined);
  const { createEditor } = useContext(DocumentRuntime);
  const versionQuery = useQuery({
    queryKey: ["artifact", artifactId, "version", version],
    queryFn: () => api.getArtifactVersion(artifactId, version),
  });
  // The signed-in person reaches the editor as their login alone, and the effect below depends
  // on that string rather than on the whoami answer: the answer carries `event_head`, so it is
  // a new object after every committed event, and keyed on it the editor is destroyed and
  // built again while the reader reads. This view reads the person at build time only, so the
  // login - equal to itself across re-reads - is both what the editor needs and the signal
  // that it has arrived.
  const login = useQuery(whoAmIQuery()).data?.login;
  const queryClient = useQueryClient();
  // The server's reading of this version's text, in its version mode: what the editor draws,
  // with no parse of its own. `DisplayTree` is the same JSON `DocumentJSON` describes - one
  // ProseMirror node - typed openly because a response carries whatever the server's schema
  // wrote.
  const tree = (isText(versionQuery.data) ? versionQuery.data.markdown_tree : undefined) as
    | DocumentJSON
    | undefined;
  // The version's own markdown, which draws it where no reading arrived: a pod of the build
  // before this one answers a version without one for the length of a rollout, and a stored
  // text with no tree is parsed rather than refused, as every other view of this change does
  // (`MarkdownBody`, `DecisionsPanel`). Telling a reader the version could not be loaded, where
  // the document is right there in the answer, is the one reading this view must not make.
  const markdown = isText(versionQuery.data) ? versionQuery.data.markdown : undefined;
  // Where a `#id` in this version's text resolves: the document it is a version of, whose
  // latest version's question blocks declare the asks it names, as the server reads them.
  const textSource = useMemo(() => textSourceOf("artifact", artifactId), [artifactId]);
  const followReference = useReferenceClick();

  // `ProofDocument` renders this view and forwards `highlight`; `ArtifactDocument` above it
  // builds that object afresh on every render. So the editor effect below keys on what the
  // highlight says rather than on the object: keyed on the object, every render of
  // `ArtifactDocument` destroys the editor, builds a new one and scrolls again, so the page
  // collapses between mounts and the last scroll can run before the last editor has laid out,
  // leaving the reader at the top. Keyed on the fields, the editor mounts once per highlighted
  // comment.
  const highlightBy = highlight?.by;
  const highlightId = highlight?.id;
  const highlightQuote = highlight?.quote;
  const highlighted = useMemo(
    () =>
      highlightBy === undefined || highlightId === undefined || highlightQuote === undefined
        ? undefined
        : { by: highlightBy, id: highlightId, quote: highlightQuote },
    [highlightBy, highlightId, highlightQuote]
  );
  // The baseline is keyed on what it says for the same reason the highlight is: the reading pane
  // builds this object from its own query's answer on every render, and keyed on the object the
  // editor would be destroyed and built again under a reader who has moved nothing.
  const baselineMarkdown = changeBaseline?.markdown;
  const baselineTree = changeBaseline?.tree;
  const baseline = useMemo(
    () =>
      baselineMarkdown === undefined
        ? undefined
        : { markdown: baselineMarkdown, tree: baselineTree },
    [baselineMarkdown, baselineTree]
  );
  useEffect(() => {
    const parent = root.current;
    // `markdown` is a required field of a text version, read under the same check as the
    // reading beside it, so an answer that is a text has it and one that is not (a file
    // version) has neither: this one test stands for both.
    if (parent === null || markdown === undefined || login === undefined) {
      return;
    }

    let ydoc: Doc | undefined;
    let handle: EditorHandle | undefined;
    let mounted = true;
    let disposeRefocus = () => {};
    setRenderError(undefined);
    const mount = async () => {
      const doc = await createDoc();
      if (!mounted) {
        doc.destroy();
        return;
      }
      ydoc = doc;
      const editor = await createEditor(parent, {
        access: "read",
        awareness: null,
        blockSchema: compiledBlockSchema,
        heatMapMode: "hidden",
        onAskBlockHosts: setAskBlockHosts,
        user: { color: colorForLogin(login), name: login },
        watchPicture: (route, onPicture) => watchPicturePath(queryClient, route, onPicture),
        ydoc: doc,
      });
      handle = editor;
      if (!mounted) {
        editor.destroy();
        return;
      }
      // The reading where the answer carried one, and the markdown itself where it carried
      // none. A highlight is embedded in a tree, so a version drawn from its markdown is drawn
      // first and the editor's own reading of it is what the quote is embedded in: the reader
      // who followed a comment's deep link into a past version sees the quote marked either
      // way, and is told the range is gone only where the text really no longer holds it. That
      // reading is read back for a highlight alone: a version nobody deep-linked into is drawn
      // already, and serializing the whole document again would buy nothing.
      if (tree === undefined) {
        editor.setMarkdown(markdown);
      }
      const embedded =
        highlighted === undefined
          ? undefined
          : embedHighlight(tree ?? (editor.view.state.doc.toJSON() as DocumentJSON), highlighted);
      if (embedded !== undefined) {
        editor.setDocumentJSON(embedded);
      } else if (tree !== undefined) {
        editor.setDocumentJSON(tree);
      }
      setHighlightMissing(highlighted !== undefined && embedded === undefined);
      // Once the version's text is in: its references are drawn over what it reads now.
      setEditorHandle(editor);
      if (embedded !== undefined && highlighted !== undefined) {
        editor.focusMark(highlighted.id);
        // A diagram or a card above the quote that draws after this focus pushes it down, so the
        // focus runs again once the drawing has settled; the cleanup drops it once unmounted.
        disposeRefocus = refocusWhenSettled(
          () => editor.focusMark(highlighted.id),
          editor.view.dom
        );
      }
      if (baseline === undefined) return;
      // The baseline is drawn - or parsed, where its answer carried no reading - by the shared
      // headless engine, which the shell warms after the first paint: awaiting that load marks a
      // version opened straight by its own URL too, rather than racing the warm-up and marking
      // nothing.
      await warmMarkdownRenderer().catch(() => undefined);
      if (!mounted) return;
      editor.setChangeDecorations(changeMarksAgainst(baseline, editor.view.state.doc));
    };
    mount().catch((error: unknown) => {
      // A version the editor cannot build the server's tree into must say so; an empty
      // read-only editor looks like an empty document.
      handle?.destroy();
      handle = undefined;
      setEditorHandle(undefined);
      if (mounted) {
        setRenderError(error instanceof Error ? error.message : String(error));
      }
    });

    return () => {
      mounted = false;
      disposeRefocus();
      handle?.destroy();
      ydoc?.destroy();
      setEditorHandle(undefined);
    };
  }, [baseline, createEditor, highlighted, login, markdown, queryClient, tree]);

  if (versionQuery.isPending) {
    return <p>Loading version…</p>;
  }
  // The message is for a version there is no text of: a read that failed, or an answer that is
  // no text at all (a file version). An answer that carried its markdown and no reading is not
  // one of those, and is drawn above by parsing it.
  if (versionQuery.isError || markdown === undefined) {
    return <p>Could not load this document version.</p>;
  }

  return (
    <section aria-label={`Document version ${version}`} className="space-y-3">
      <h2>
        Version {version}
        {createdAt === undefined ? null : (
          <span>
            {" · "}
            <Timestamp at={createdAt} />
          </span>
        )}
      </h2>
      {highlightMissing ? (
        <p role="status">Text changed. The selected range no longer exists in this document.</p>
      ) : null}
      {renderError === undefined ? null : (
        <p role="alert">This version could not be rendered: {renderError}</p>
      )}
      {/* biome-ignore lint/a11y/useKeyWithClickEvents: dispatch:// link clicks bubble here; the
      editor already handles keyboard activation of its own links. */}
      <article
        aria-label="Document"
        className="dispatch-doc"
        data-read-only="true"
        onClick={followReference}
      >
        <div ref={root} />
        <DocumentReferences editor={editorHandle} source={textSource} />
        {askBlockHosts.map((host) => {
          const blockId = String(host.node.attrs.blockId);
          return (
            <AskBlockCard
              ask={asks.find((candidate) => candidate.block_id === blockId)}
              host={host}
              inBody={false}
              key={host.key}
              owner={owner}
              readOnly
              rowResolution={false}
            />
          );
        })}
      </article>
    </section>
  );
}
