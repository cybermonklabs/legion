import { type ReactNode, type RefCallback, useEffect, useId, useState } from "react";
import type { IssueDetails } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { NoTooltips } from "../../components/tooltips";
import {
  borderDefault,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useMarginDrawsSlot } from "../margin/margin-slot";
import { AgentName } from "../refs/AgentName";
import { ReferencedBy, ReferencedByToggle } from "../refs/ReferencedBy";
import { buildDispatchReference } from "../refs/routes";
import { BlockedByControl } from "./BlockedByControl";
import { ConversationStates } from "./conversation-states";
import { FocusedNodeFacts } from "./FocusedNodeFacts";
import type { FocusedNode } from "./focused-node";
import { GitHubLink } from "./GitHubLink";
import { IssueCollaboratorList } from "./IssueCollaboratorList";
import { IssueComponentsLine } from "./IssueComponentsLine";
import {
  authorText,
  MARGIN_SECTION_NAMES,
  type MarginSection,
  PROPERTY_LABELS,
  pageChrome,
} from "./page-structure";
import type { ConversationState } from "./page-words";
import { RepoControl } from "./RepoControl";
import { SubscribedAgents } from "./SubscribedAgents";
import { useIssueSubscribers } from "./useIssueSubscribers";
import { useMarginSections } from "./useMarginSections";

/**
 * The margin's own side of the issue page's panel (dispatch://INFRA-1196 R12): the margin's
 * cards, and - where the margin keeps a column for that panel - the host the panel is moved
 * into, under them.
 *
 * The panel is drawn once, by the page, and moved between this host and the page's own column
 * rather than drawn afresh in each (`IssuePage`'s `panelStage`): a reader who has typed a
 * message, opened a blocked-by edit or opened one of the panel's lists still has it when the
 * margin is collapsed and shown again. Whether the margin keeps that column is read here as
 * the page reads it (`features/margin/margin-slot.tsx`), so both hosts answer one change of
 * it in one commit and the panel is never between hosts for a frame.
 */
export function IssueMarginCards({
  conversation,
  panelHost,
}: {
  /** The margin's own content, which this wraps where the panel sits inside the shell's
   *  margin. */
  conversation?: ReactNode;
  /** Takes the host the page's panel is drawn in, where the margin keeps a column for it; it
   *  is handed no host wherever the page draws the panel in its own column instead, and the
   *  margin draws its own cards alone. The wrapper is the same element in both states on
   *  purpose: the margin holds the open composer inside it, and a wrapper that changed shape
   *  as the margin collapsed would remount that composer and lose an unsent comment
   *  (`features/margin/MarginSheet.tsx`). */
  panelHost: RefCallback<HTMLElement>;
}): ReactNode {
  const drawsPanel = useMarginDrawsSlot();
  return (
    // The panel is the page's, wherever the shell mounts it, and the page draws no tooltips
    // (`components/tooltips.tsx`): the margin's own cards reach that reading through this
    // wrapper, because they are rendered inside it (`features/margin/margin-slot.tsx`).
    <NoTooltips>
      <div
        className={drawsPanel ? "flex min-w-0 flex-col gap-3" : "contents"}
        data-testid={drawsPanel ? "issue-margin" : undefined}
        ref={drawsPanel ? panelHost : undefined}
      >
        {/* The margin's own cards first, and the panel's own sections under them, because the
            page's panel is appended to this wrapper (`features/margin/margin-slot.tsx`): the
            issue's asks, comments, messages and decisions, in the margin's words and the
            authors' own, and not the page's declared vocabulary (dispatch://INFRA-1196 R21,
            `page-structure.ts`). They are first because the margin places an anchored card at
            a `top` it measures down from the top of its own content (`margin/ThreadList.tsx`'s
            `regionOrigin`), so anything drawn over them moves that anchor and a card can no
            longer reach the line its mark is on. */}
        <div className="contents" {...authorText}>
          {conversation}
        </div>
      </div>
    </NoTooltips>
  );
}

/**
 * The issue page's one right-hand panel (dispatch://INFRA-1196 R12): the properties that are not
 * the issue's identity, in three named collapsible sections; the facts of whatever the reader's
 * focus is on; and the conversation itself - its four state controls and the one list they
 * drive, which lives here and in no column of the page.
 *
 * It is one panel, not two: the properties are not a second column beside the conversation, and
 * no property of the issue is drawn in both this and the identity block.
 *
 * The page draws it once and moves it into whichever host is on the screen - the margin's
 * column above, or the page's own under the identity block, wherever the margin keeps no
 * column (at the width it is a sheet, and while the reader has collapsed it to its rail). So
 * the properties are read at every width and under either preference without opening
 * anything, and nothing in the panel is thrown away as the reader moves between them (R11).
 */
export function IssueMarginPanel({
  conversationList,
  conversationState,
  focused,
  isClosed,
  issue,
  onConversationState,
  openCollaborators = false,
}: {
  /** The conversation's one list, which the state controls above it drive. It reads at the
   *  panel's own width (384 px by default, 280 px at its narrowest). */
  conversationList?: ReactNode;
  /** Which state the conversation is in, which the address names and these controls move. */
  conversationState: ConversationState;
  /** What the reader's focus is on, which the page reads and hands over (`focused-node.tsx`). */
  focused: FocusedNode;
  isClosed: boolean;
  issue: IssueDetails;
  /** Takes the state a reader picked, which the page holds for the address they are on, or
   *  opens the address of where that state is one (`IssuePage`'s `pickState`). */
  onConversationState: (state: ConversationState) => void;
  /** Whether the People section opens with the issue's collaborators drawn, which is what
   *  `/issues/<key>/collaborators` - the address the Collaborators tab answered - names
   *  (R19). Every other address leaves the list closed and reads nothing. */
  openCollaborators?: boolean;
}): ReactNode {
  const sections = useMarginSections();
  const subscribers = useIssueSubscribers(issue.key);
  const [subscribersOpen, setSubscribersOpen] = useState(false);
  const [collaboratorsOpen, setCollaboratorsOpen] = useState(openCollaborators);
  const [referencesOpen, setReferencesOpen] = useState(false);
  const referencesPanelId = useId();
  const issueReference = buildDispatchReference({ key: issue.key, kind: "issue" });
  // The address opens the list; closing it again is the reader's, so this only ever opens.
  // The panel outlives a move between the issue's addresses, so seeding the state is not
  // enough: a link to `/collaborators` followed from another of the issue's addresses arrives
  // under a panel that is already mounted.
  useEffect(() => {
    if (openCollaborators) setCollaboratorsOpen(true);
  }, [openCollaborators]);

  return (
    <div className="flex min-w-0 flex-col gap-3">
      <PropertySection name="development" sections={sections}>
        <RepoControl
          disabled={isClosed}
          issueKey={issue.key}
          project={issue.project}
          repo={issue.repo}
          resolvedRepo={issue.resolved_repo}
        />
        <IssueComponentsLine issue={issue} />
        <BlockedByControl issue={issue} />
        {(issue.external_links ?? []).length === 0 ? null : (
          <Property label={PROPERTY_LABELS.external_links}>
            {(issue.external_links ?? []).map((link) => (
              <GitHubLink key={link.url} link={link} />
            ))}
          </Property>
        )}
      </PropertySection>
      <PropertySection name="people" sections={sections}>
        {issue.created_by == null ? null : (
          <Property label={PROPERTY_LABELS.opened_by}>
            <span {...authorText}>
              <AgentName actor={issue.created_by} inline />
            </span>
          </Property>
        )}
        <button
          aria-expanded={subscribersOpen}
          className={`flex min-h-11 items-center gap-2 rounded-lg px-2 py-1 text-sm font-medium ${textSecondaryOnSurface}`}
          onClick={() => setSubscribersOpen((open) => !open)}
          type="button"
          {...pageChrome}
        >
          <span>
            {subscribers.counted ? `${PROPERTY_LABELS.subscribers}:` : PROPERTY_LABELS.subscribers}
          </span>
          {subscribers.counted ? <span {...authorText}>{subscribers.rows.length}</span> : null}
        </button>
        {subscribersOpen ? (
          <SubscribedAgents
            onUnsubscribe={(sessionId) => subscribers.unsubscribe.mutate(sessionId)}
            ownerLabel={issue.key}
            subscribers={subscribers.rows}
          />
        ) : null}
        {subscribers.error ? (
          <QueryError
            error={subscribers.failure}
            message="Subscribed agents unavailable — Envoy listener unreachable."
            onRetry={subscribers.refetch}
            retrying={subscribers.fetching}
          />
        ) : null}
        {subscribers.unsubscribe.isError ? (
          <QueryError
            message="Could not unsubscribe this agent."
            onRetry={() => subscribers.unsubscribe.mutate(subscribers.unsubscribe.variables ?? "")}
            retrying={subscribers.unsubscribe.isPending}
          />
        ) : null}
        {/* Everyone who worked the issue, running or not, which the nine-tab page gave a tab
              of its own. It is read only once a reader opens it, so the panel costs the page no
              request on an address that does not name it (R20). */}
        <button
          aria-expanded={collaboratorsOpen}
          className={`flex min-h-11 items-center gap-2 rounded-lg px-2 py-1 text-sm font-medium ${textSecondaryOnSurface}`}
          onClick={() => setCollaboratorsOpen((open) => !open)}
          type="button"
          {...pageChrome}
        >
          <span>{PROPERTY_LABELS.collaborators}</span>
        </button>
        {collaboratorsOpen ? (
          <div {...authorText}>
            <IssueCollaboratorList issueKey={issue.key} />
          </div>
        ) : null}
      </PropertySection>
      <PropertySection name="references" sections={sections}>
        <ReferencedByToggle
          controls={referencesPanelId}
          count={issue.referenced_by_count}
          expanded={referencesOpen}
          onToggle={() => setReferencesOpen((open) => !open)}
        />
        {referencesOpen ? (
          <ReferencedBy
            className="w-full"
            reference={issueReference}
            toggle={{ id: referencesPanelId }}
          />
        ) : null}
      </PropertySection>
      <FocusedNodeFacts node={focused} />
      {/* Under the margin's own content, never above it, for the same reason the sections
          above are (the panel's head): a section drawn over the margin's cards moves the
          anchor their placement measures against. */}
      <ConversationStates onPick={onConversationState} state={conversationState} />
      {/* The issue's one conversation panel, under the state controls that drive it. It is
            the box every spec and every link reaches the conversation by
            (`#issue-conversation-panel`), which is this panel wherever the shell mounts it. */}
      <div className="flex min-w-0 flex-col" id="issue-conversation-panel" {...authorText}>
        {conversationList}
      </div>
    </div>
  );
}

/** One named section of the panel's properties, open or closed by its own one control. A closed
 *  section draws none of its values, so what a reader sees is what the page holds. */
function PropertySection({
  children,
  name,
  sections,
}: {
  children: ReactNode;
  name: MarginSection;
  sections: ReturnType<typeof useMarginSections>;
}): ReactNode {
  const open = sections.isOpen(name);
  return (
    <section className={`border-b pb-2 ${borderDefault}`} data-margin-section={name}>
      <h2>
        <button
          aria-expanded={open}
          className={`flex min-h-11 w-full items-center justify-between gap-2 text-sm font-semibold ${textPrimaryOnSurface}`}
          onClick={() => sections.toggle(name)}
          type="button"
          {...pageChrome}
        >
          <span>{MARGIN_SECTION_NAMES[name]}</span>
          <span aria-hidden="true">{open ? "−" : "+"}</span>
        </button>
      </h2>
      {open ? <div className="flex min-w-0 flex-col gap-2">{children}</div> : null}
    </section>
  );
}

/** One property of the issue inside a section: its name, then its value. */
function Property({ children, label }: { children: ReactNode; label: string }): ReactNode {
  return (
    <div className={`flex min-w-0 flex-wrap items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
      <span className={`font-medium ${textMutedOnSurface}`} {...pageChrome}>
        {label}:
      </span>
      {children}
    </div>
  );
}
