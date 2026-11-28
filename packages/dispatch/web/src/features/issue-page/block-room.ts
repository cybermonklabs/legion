import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { createElement, type ReactNode, useEffect, useRef, useState } from "react";
import type { Awareness } from "y-protocols/awareness";
import type { Doc, XmlElement } from "yjs";

import { primarySpec } from "../../api/issue-cache";
import { issueTreeKey, whoAmIQuery } from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { type ConnectionCallbacks, colorForLogin, loadDocumentTransport } from "../doc/connection";
import { createBlockEditor, type EditorHandle } from "../doc/editor";
import { composerKindFor, selectionBarKindFor } from "../doc/marks";
import type { PendingState } from "../doc/pending-sync";
import { createPendingWaiter } from "../doc/pending-waiter";
import type { QuoteAnchor } from "../doc/quote-anchor";
import { fragmentOf } from "../doc/roots";
import { compiledBlockSchema, compiledSchemaCreatable, editorBlockSchema } from "../doc/schema";
import { useMargin } from "../margin/margin-context";
import { watchPicturePath } from "../refs/reference-target";
import { buildDispatchReference, documentRoute } from "../refs/routes";
import { importWhenOnline } from "../shell/DeploymentResilience";
import { IssueBlockRoomContext, type KeptBind, watchKeptBinds } from "./block-editor";
import { useReportAdmission } from "./document-admission";
import { askForTask } from "./drawn-in-slices";
import { useOwnedIssueTree } from "./issue-tree-owner";
import { KeptEditorParts, type ReferencesPress } from "./kept-editor-parts";
import {
  BLOCK_EDITOR_START_MARK,
  observeEntryEnds,
  onRowsAndFontsChange,
  rowsAndFontsReady,
  watchLoadEnd,
} from "./load-end";
import { KEY_REPORTS } from "./page-structure";
import { blockNodeId } from "./tree-drafts";
import { nodesInOrder } from "./tree-keys";
import type { TreeAnswer } from "./tree-read/tree-read";

const READY_MARK = "block-editor-ready";
const TASK_MEASURE = "block-editor-task";

export interface RoomArtifact {
  readonly id: string;
  readonly fragment: string;
}

interface RoomConnection {
  readonly awareness?: Awareness;
  readonly doc: Doc;
  destroy(): void;
}

type Connect = (id: string, callbacks: ConnectionCallbacks) => RoomConnection;

interface KeptEditor {
  readonly view: { readonly dom: HTMLElement };
  destroy(): void | Promise<void>;
}

export interface BindTarget<Editor> {
  readonly artifact: RoomArtifact;
  readonly connection: RoomConnection;
  readonly editor: Editor;
  readonly hidden: HTMLElement;
  /** The element the kept editor is built in, inside `hidden`. Milkdown wraps the editable in a
   *  box inside this element, and again at each change of the editor's plugins (its
   *  `MILKDOWN_VIEW_CLEAR` view), so a bind moves this element into a row, never the editable
   *  alone, which the next change of plugins would put back here. */
  readonly root: HTMLElement;
  readonly scratch: XmlElement;
  /** The admission when handed over; `watchAdmission` reports later changes. */
  readonly readOnly: boolean;
  readonly issueClosed: () => boolean;
  watchAdmission(listener: (readOnly: boolean) => void): () => void;
  watchPending(listener: (state: PendingState) => void): () => void;
  whenSent(timeoutMs: number): Promise<void>;
}

interface BindRequest<Editor> {
  onBind(target: BindTarget<Editor>): void;
  onBindRefused(words: string): void;
}

export interface BlockRoom<Editor extends KeptEditor> {
  start(): void;
  requestBind(artifact: RoomArtifact, request: BindRequest<Editor>): () => void;
  destroy(): void;
}

interface BuildContext {
  readonly root: HTMLElement;
  readonly queryClient: QueryClient;
  readonly boundArtifact: () => RoomArtifact | undefined;
  /** Answers once this browser's edits of the bound block's room have reached the server, and
   *  at `timeoutMs` whatever it reports. */
  readonly whenSent: (timeoutMs: number) => Promise<void>;
}

interface RoomOptions<Editor extends KeptEditor> {
  readonly spec: RoomArtifact;
  readonly converted: boolean | (() => boolean);
  readonly queryClient: QueryClient;
  readonly host: HTMLElement;
  readonly issueClosed?: () => boolean;
  readonly loadTransport: () => Promise<Connect>;
  readonly loadEditor: () => Promise<unknown>;
  readonly loadStyles: () => Promise<unknown>;
  readonly buildEditor: (element: XmlElement, context: BuildContext) => Promise<Editor>;
  readonly rowWidth?: () => number;
  readonly schedule?: (run: () => void) => void;
  readonly mark?: (name: string) => void;
  readonly measure?: (name: string, start: number) => void;
  readonly report?: (words: string) => void;
  readonly onAdmission?: (artifactId: string, readOnly: boolean) => void;
}

interface Session {
  readonly artifactId: string;
  readonly admissions: Set<(readOnly: boolean) => void>;
  readonly pending: Set<(state: PendingState) => void>;
  connection?: RoomConnection;
  readOnly: boolean;
  synced: boolean;
}

interface Waiting<Editor> {
  readonly artifact: RoomArtifact;
  readonly session: Session;
  readonly request: BindRequest<Editor>;
}

function hiddenBox(host: HTMLElement, width: number): HTMLElement {
  const box = host.ownerDocument.createElement("div");
  box.setAttribute("inert", "");
  box.setAttribute("aria-hidden", "true");
  Object.assign(box.style, {
    position: "fixed",
    inset: "0 auto auto 0",
    visibility: "hidden",
    contain: "strict",
    width: `${width}px`,
    height: "0",
  });
  return host.appendChild(box);
}

/** The editor belongs to the issue page, not to a view or a connection. A converted issue's
 *  documents use the primary spec's connection. Other documents on an unconverted issue each
 *  connect on first use, and still use the same kept editor. */
export function createBlockRoom<Editor extends KeptEditor>({
  spec,
  converted,
  queryClient,
  host,
  issueClosed = () => false,
  loadTransport,
  loadEditor,
  loadStyles,
  buildEditor,
  rowWidth = () => host.clientWidth,
  schedule = askForTask,
  mark = (name) => performance.mark(name),
  measure = (name, start) => performance.measure(name, { start, end: performance.now() }),
  report,
  onAdmission,
}: RoomOptions<Editor>): BlockRoom<Editor> {
  const sessions = new Map<string, Session>();
  const waiting: Waiting<Editor>[] = [];
  const pendingWaiter = createPendingWaiter();
  let alive = true;
  let started = false;
  let box: HTMLElement | undefined;
  let editorRoot: HTMLElement | undefined;
  let scratch: Doc | undefined;
  let scratchBlock: XmlElement | undefined;
  let editor: Editor | undefined;
  let building = false;
  let currentArtifact: RoomArtifact | undefined;
  let currentSession: Session | undefined;

  function runTask<T>(run: () => T | PromiseLike<T>): Promise<T | undefined> {
    const task = Promise.withResolvers<T | undefined>();
    schedule(() => {
      if (!alive) {
        task.resolve(undefined);
        return;
      }
      const start = performance.now();
      try {
        task.resolve(run());
      } catch (error) {
        task.reject(error);
      } finally {
        // Do not charge the task for time spent awaiting network responses.
        measure(TASK_MEASURE, start);
      }
    });
    return task.promise;
  }

  async function loadCode() {
    const transport = await runTask(() =>
      Promise.all([loadTransport(), importWhenOnline(() => import("yjs"))])
    );
    if (!alive || transport === undefined) return;
    await runTask(loadEditor);
    if (!alive) return;
    await runTask(loadStyles);
    return transport;
  }
  let code: ReturnType<typeof loadCode> | undefined;

  function drainBinds() {
    while (alive) {
      const next = waiting[0];
      if (next === undefined) return;
      const { artifact, request, session } = next;
      // A room that admits the page read-only still binds the block, read-only (decision 572).
      if (
        !session.synced ||
        session.connection === undefined ||
        editor === undefined ||
        box === undefined ||
        editorRoot === undefined ||
        scratchBlock === undefined
      ) {
        return;
      }
      waiting.shift();
      currentArtifact = artifact;
      currentSession = session;
      request.onBind({
        artifact,
        connection: session.connection,
        editor,
        hidden: box,
        root: editorRoot,
        scratch: scratchBlock,
        get readOnly() {
          return session.readOnly;
        },
        issueClosed,
        watchAdmission(listener) {
          session.admissions.add(listener);
          return () => session.admissions.delete(listener);
        },
        watchPending(listener) {
          session.pending.add(listener);
          return () => session.pending.delete(listener);
        },
        whenSent: (timeoutMs) => pendingWaiter.whenSent(session.artifactId, timeoutMs),
      });
    }
  }

  function refuse(predicate: (bind: Waiting<Editor>) => boolean) {
    const refused = waiting.filter(predicate);
    for (const bind of refused) waiting.splice(waiting.indexOf(bind), 1);
    for (const bind of refused) bind.request.onBindRefused(KEY_REPORTS.editor_closed);
  }

  async function makeEditor(Y: typeof import("yjs")) {
    if (building || editor !== undefined) return;
    building = true;
    try {
      const made = await runTask(async () => {
        const doc = new Y.Doc();
        const paragraph = new Y.XmlElement("paragraph");
        doc.getXmlFragment("scratch").insert(0, [paragraph]);
        const hidden = hiddenBox(host, rowWidth());
        box = hidden;
        const root = hidden.appendChild(host.ownerDocument.createElement("div"));
        try {
          return {
            doc,
            handle: await buildEditor(paragraph, {
              boundArtifact: () => currentArtifact,
              queryClient,
              root,
              whenSent: (timeoutMs) =>
                currentSession === undefined
                  ? Promise.resolve()
                  : pendingWaiter.whenSent(currentSession.artifactId, timeoutMs),
            }),
            hidden,
            paragraph,
            root,
          };
        } catch (error) {
          hidden.remove();
          doc.destroy();
          throw error;
        }
      });
      if (made === undefined) return;
      if (!alive) {
        void made.handle.destroy();
        made.doc.destroy();
        return;
      }
      if (!made.hidden.contains(made.handle.view.dom)) made.hidden.append(made.handle.view.dom);
      scratch = made.doc;
      scratchBlock = made.paragraph;
      editorRoot = made.root;
      editor = made.handle;
      mark(READY_MARK);
    } catch (error) {
      if (alive) {
        console.error("Could not build the issue's block editor", error);
        refuse(() => true);
      }
    } finally {
      building = false;
    }
  }

  function ready(Y: typeof import("yjs")) {
    void makeEditor(Y).then(drainBinds);
  }

  async function open(artifact: RoomArtifact, session: Session) {
    code ??= loadCode().catch((error: unknown) => {
      code = undefined;
      throw error;
    });
    const active = () => alive && sessions.get(artifact.id) === session;
    const failed = (showReport: boolean) => {
      if (!active()) return;
      sessions.delete(artifact.id);
      session.connection?.destroy();
      if (showReport) report?.(KEY_REPORTS.editor_closed);
      refuse((bind) => bind.session === session);
      drainBinds();
    };
    try {
      const loaded = await code;
      if (loaded === undefined || !active()) return;
      const [connect, Y] = loaded;
      await runTask(() => {
        const connection = connect(artifact.id, {
          fragment: artifact.fragment,
          schemaVersion: compiledBlockSchema.version,
          schemaCreatable: compiledSchemaCreatable,
          onAdmission(readOnly) {
            if (!active()) return;
            session.readOnly = readOnly;
            onAdmission?.(artifact.id, readOnly);
            for (const listener of session.admissions) listener(readOnly);
            // A bind waiting on a read-only admission is made read-only (`drainBinds`).
            drainBinds();
          },
          onOutsideSchema: () => failed(true),
          onPending(state) {
            if (!active()) return;
            pendingWaiter.report(artifact.id, state);
            for (const listener of session.pending) listener(state);
          },
          onStatus(status) {
            if (status === "failed") failed(true);
          },
          onSynced() {
            if (!active()) return;
            session.synced = true;
            ready(Y);
          },
        });
        if (active()) session.connection = connection;
        else connection.destroy();
      });
    } catch (error) {
      if (!active()) return;
      console.error("Could not open the issue's block editor room", error);
      failed(false);
    }
  }

  function sessionFor(artifact: RoomArtifact): Session {
    const existing = sessions.get(artifact.id);
    if (existing !== undefined) return existing;
    const session: Session = {
      artifactId: artifact.id,
      admissions: new Set(),
      pending: new Set(),
      readOnly: false,
      synced: false,
    };
    sessions.set(artifact.id, session);
    void open(artifact, session);
    return session;
  }

  function begin() {
    if (started) return;
    started = true;
    mark(BLOCK_EDITOR_START_MARK);
  }

  return {
    start() {
      if (!alive) return;
      begin();
      sessionFor(spec);
    },
    requestBind(artifact, request) {
      if (!alive) {
        request.onBindRefused(KEY_REPORTS.editor_closed);
        return () => {};
      }
      begin();
      const shared = typeof converted === "function" ? converted() : converted;
      const session = sessionFor(shared ? spec : artifact);
      const bind = { artifact, request, session };
      waiting.push(bind);
      drainBinds();
      return () => {
        const at = waiting.indexOf(bind);
        if (at !== -1) waiting.splice(at, 1);
        drainBinds();
      };
    },
    destroy() {
      if (!alive) return;
      alive = false;
      waiting.length = 0;
      for (const session of sessions.values()) session.connection?.destroy();
      sessions.clear();
      void editor?.destroy();
      scratch?.destroy();
      box?.remove();
    },
  };
}

/** Take the newest tree header already held by the page. No query is sent. */
function heldTree(queryClient: QueryClient, issueKey: string): TreeAnswer | undefined {
  let newest: TreeAnswer | undefined;
  let newestAt = -1;
  for (const query of queryClient.getQueryCache().findAll({ queryKey: issueTreeKey(issueKey) })) {
    const answer = query.state.data as TreeAnswer | undefined;
    const at = query.state.dataUpdatedAt;
    if (typeof answer?.converted === "boolean" && at > newestAt) {
      newest = answer;
      newestAt = at;
    }
  }
  return newest;
}

/** Mount under the keyed issue page, not under a view that comes and goes. */
export function IssueBlockRoom({
  children,
  issue,
}: {
  readonly children: ReactNode;
  readonly issue: IssueDetails;
}): ReactNode {
  const queryClient = useQueryClient();
  const tree = useOwnedIssueTree(issue);
  const roomRef = useRef<BlockRoom<EditorHandle> | null>(null);
  const margin = useMargin();
  const marginRef = useRef(margin);
  marginRef.current = margin;
  const reportAdmission = useReportAdmission();
  // The kept editor once it is built, for what the page draws beside the block it binds
  // (`KeptEditorParts`), and what a press on a block's references button there does.
  const [keptEditor, setKeptEditor] = useState<EditorHandle | undefined>(undefined);
  const referencesPress = useRef<ReferencesPress>(() => {});
  const latest = useRef({ issue, root: tree.root });
  latest.current = { issue, root: tree.root };
  const { key } = issue;
  const spec = primarySpec(issue);
  const specId = spec?.id;
  const specFragment = spec === undefined ? undefined : fragmentOf(spec);
  const documentRef = spec === undefined ? undefined : buildDispatchReference(documentRoute(spec));
  useEffect(() => {
    if (specId === undefined || specFragment === undefined) return;
    // The watcher and the room both support immediate starts. They share the start mark.
    const marked = new Set<string>();
    const mark = (name: string) => {
      if (marked.has(name)) return;
      marked.add(name);
      performance.mark(name);
    };
    let alive = true;
    // The margin draws a document's threads, and the composer a comment on a selection opens,
    // only while a document's editor is registered with it (`registerDocument`, which the whole
    // document's editor does as it binds, `ProofDocument.tsx`). The kept editor registers itself
    // once the reader asks for one of them - a comment on its selection, a block's references -
    // so a bind reads nothing more, and takes itself back as it leaves the row.
    let registered = false;
    const unregister = () => {
      if (!registered) return;
      registered = false;
      marginRef.current.registerDocument(undefined);
    };
    let showDocument = (_artifactId: string) => {};
    const room = createBlockRoom({
      spec: { id: specId, fragment: specFragment },
      converted: () => heldTree(queryClient, key)?.converted === true,
      queryClient,
      host: document.body,
      issueClosed: () => latest.current.issue.closed_at !== null,
      onAdmission(artifactId, readOnly) {
        if (latest.current.issue.closed_at !== null) return;
        if (heldTree(queryClient, key)?.converted === true) {
          for (const artifact of latest.current.issue.artifacts ?? []) {
            reportAdmission(artifact.id, readOnly);
          }
        } else {
          reportAdmission(artifactId, readOnly);
        }
      },
      rowWidth: () =>
        document
          .querySelector("[data-issue-page] [data-node-row] [data-node-words]")
          ?.getBoundingClientRect().width ||
        document.querySelector('[data-testid="issue-column"]')?.clientWidth ||
        document.documentElement.clientWidth,
      loadTransport: async () => {
        const [connect, { splitSyncInWorker }] = await Promise.all([
          loadDocumentTransport(),
          importWhenOnline(() => import("./sync-parts")),
        ]);
        return (id, callbacks) =>
          connect(id, { ...callbacks, firstSyncInParts: splitSyncInWorker });
      },
      loadEditor: () =>
        importWhenOnline(() =>
          Promise.all([import("@legion/proof-editor"), import("../doc/ask-block")])
        ),
      loadStyles: () => importWhenOnline(() => import("@legion/proof-editor/style.css")),
      buildEditor: async (element, { boundArtifact, queryClient: client, root, whenSent }) => {
        const login = client.getQueryData(whoAmIQuery().queryKey)?.login;
        if (login === undefined) throw new Error("The issue has no signed-in reader.");
        const plugins = importWhenOnline(() => import("./block-editor-plugins"));
        // A build that fails first leaves this load's own failure, if any, unread.
        plugins.catch(() => undefined);
        let bound: KeptBind | undefined;
        let quoteForSelection: (from: number, to: number) => QuoteAnchor | undefined = () =>
          undefined;
        const handle = await createBlockEditor(root, {
          askStateOf(blockId) {
            const id = boundArtifact()?.id;
            const artifact = latest.current.issue.artifacts?.find((entry) => entry.id === id);
            if (artifact === undefined) return undefined;
            const nodeId = blockNodeId(artifact.slug, blockId);
            return nodesInOrder(latest.current.root).find(
              (node) => node.type === "ask" && node.id === nodeId
            )?.state;
          },
          onMarkAction(action) {
            const artifact = boundArtifact()?.id;
            if (artifact === undefined) return;
            if (action.kind === "quote-comment") {
              const anchor = quoteForSelection(action.from, action.to);
              if (anchor === undefined) return;
              showDocument(artifact);
              return marginRef.current.composeForMark({
                anchor: { artifact, ...anchor },
                kind: "comment",
              });
            }
            if (action.kind === "comment" || action.kind === "ask" || action.kind === "suggest") {
              showDocument(artifact);
              return marginRef.current.composeForMark({
                anchor: {
                  artifact,
                  mark_id: action.markId,
                  placement: action.placement ?? undefined,
                  quote: action.quote,
                },
                kind: composerKindFor(action.kind),
              });
            }
          },
          onMarkClick: (id) => marginRef.current.focusItemForMark(id),
          onMarkHover: (id) => marginRef.current.hoverItemForMark(id),
          blockSchema: editorBlockSchema(heldTree(client, key)?.view_of),
          documentRef,
          element,
          user: { name: login, color: colorForLogin(login) },
          watchPicture: (route, onPicture) => watchPicturePath(client, route, onPicture),
        });
        // The page's own plugins of the kept editor, loaded with the editor's code and put on
        // before its first bind (`block-editor-plugins.ts`).
        try {
          const { boundQuoteAnchor, installKeptEditorPlugins } = await plugins;
          installKeptEditorPlugins(handle.view);
          quoteForSelection = (from, to) =>
            bound === undefined
              ? undefined
              : boundQuoteAnchor(handle.view.state.schema, bound.element, from, to);
        } catch (error) {
          void handle.destroy();
          throw error;
        }
        showDocument = (artifactId) => {
          registered = true;
          marginRef.current.registerDocument({
            artifactId,
            focusBlock: (blockId) => handle.focusBlock(blockId),
            focusMark: (markId) => handle.focusMark(markId),
            removeMark: (markId) => handle.removeMark(markId),
            remarkMark: (markId, kind, quote, placement) =>
              handle.remarkMark(markId, selectionBarKindFor(kind), quote, placement ?? null),
            retypeMark: (markId, kind) => handle.retypeMark(markId, selectionBarKindFor(kind)),
            setActiveBlocks: (blockIds) => handle.setActiveBlocks(blockIds),
            setActiveMarks: (markIds) => handle.setActiveMarks(markIds),
            whenSent,
          });
        };
        watchKeptBinds(handle, {
          bound: (next) => {
            bound = next;
          },
          parked: () => {
            bound = undefined;
            unregister();
          },
        });
        referencesPress.current = (artifactId, blockId, blocks) => {
          showDocument(artifactId);
          marginRef.current.filterToBlocks(blockId, blocks);
          // The block is the view's root; focusBlock searches below it.
          handle.view.dom.focus({ preventScroll: true });
        };
        if (alive) setKeptEditor(handle);
        return handle;
      },
      mark,
    });
    roomRef.current = room;
    const load = watchLoadEnd({
      queryClient,
      ready: rowsAndFontsReady,
      subscribeReady: onRowsAndFontsChange,
      now: () => performance.now(),
      observe: observeEntryEnds,
      mark,
      start: room.start,
    });
    return () => {
      alive = false;
      roomRef.current = null;
      load.destroy();
      room.destroy();
      unregister();
      referencesPress.current = () => {};
      setKeptEditor(undefined);
    };
  }, [documentRef, key, queryClient, reportAdmission, specFragment, specId]);
  return createElement(
    IssueBlockRoomContext.Provider,
    { value: roomRef },
    children,
    keptEditor === undefined
      ? null
      : createElement(KeptEditorParts, {
          editor: keptEditor,
          issueKey: key,
          onReferencesPress: (...args: Parameters<ReferencesPress>) =>
            referencesPress.current(...args),
        })
  );
}
