import { DELIVERY_CAPABILITIES, dispatchWritingBox } from "@legion/contracts";
import { type ReactNode, useId, useRef, useState } from "react";

import type { Agent, MessageDeliveryMode } from "../../api/types";
import { heldPress } from "../../components/held-press";
import { MultiSelect } from "../../components/MultiSelect";
import { RefusableButton } from "../../components/RefusableButton";
import { useFootInset } from "../../hooks/useFootInset";
import {
  borderDefault,
  card,
  dangerText,
  inputClasses,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { capabilityLabel, deliveryMode } from "../conversation/delivery";
import { MentionField } from "../conversation/MentionField";
import { AgentName, type AgentPart, AgentParts, useAgentText } from "../refs/AgentName";
import { broadcastSendState, type ComposedBroadcast } from "./broadcast-plan";
import type { BroadcastSend } from "./broadcast-queue";

/** The composer's one notice slot: the recipient limit, the exclusions, or the mode the selection
 *  sends in, one at a time. In the compact grid it is one line that scrolls sideways like the
 *  chips: between the mode and Send on a narrow screen, adding no height, and the third and last
 *  row on a short one. A refusal or an exclusion reads as a warning; the mode alone does not. */
const composerLineLayout =
  "mt-2 text-sm narrow-or-short:order-2 narrow-or-short:col-start-2 narrow-or-short:mt-0 narrow-or-short:min-w-0 narrow-or-short:overflow-x-auto narrow-or-short:text-xs narrow-or-short:whitespace-nowrap short:order-3 short:col-span-4 short:col-start-1";
const composerLine = `${composerLineLayout} ${dangerText}`;

/**
 * The broadcast a selection is waiting to become: one body, one mode, and one message per
 * recipient. Selection is what the human ticked, so a recipient that a later filter hides is
 * still listed here rather than silently dropped; every selected session is named, including
 * the ones this mode leaves out. It follows the list and sticks to the viewport's bottom, so
 * appearing costs no layout above the rows: the checkbox that summoned it stays under the
 * pointer. A long recipient list scrolls inside it rather than growing it past half the screen.
 * Send hands the request to the page's queue (`onSend`) at the press: `composed`, which
 * `composedBroadcast` decides - the restored request word for word while one is kept, else the
 * live plan under the composition's key - with the plan the composer shows for it, so a session
 * that has come back since a refused send is shown as not in the request being re-sent. A
 * selected session the agent list could not vouch for (`unchecked`: missing from a list that is
 * not current) keeps its place in the send, and its chip says its state is unknown, not gone.
 *
 * One definition for the Agents page and the Inbox's compose dialog. The dialog holds its Send
 * (`held`) while its own send is out; the Agents page holds nothing here, since its Send hands
 * the request to its queue at the press and the composer goes with the selection.
 */
export function BroadcastComposer({
  agents,
  body,
  composed,
  defaulted,
  delivery,
  held = false,
  onBody,
  onDelivery,
  onSend,
  pageFoot = false,
  selected,
  onDeselect,
}: {
  agents: readonly Agent[];
  body: string;
  composed: ComposedBroadcast;
  /** Whether `delivery` is the selection's own mode rather than one the sender picked. */
  defaulted: boolean;
  delivery: MessageDeliveryMode;
  /** Holds Send by `aria-disabled` while the host's send is out, rather than disabling it, so the
   *  button the reader pressed keeps the focus (`RefusableButton`'s `busy`); the message box, the
   *  mode and the recipients' chips are held with it, so what is shown is what went out. */
  held?: boolean;
  onBody: (body: string) => void;
  onDelivery: (delivery: MessageDeliveryMode) => void;
  onDeselect: (sessionID: string) => void;
  onSend: (send: BroadcastSend) => void;
  /** Registers this section in the foot's page layer (`useFootInset`), for the Agents page's narrow
   *  and short screens where `BroadcastFoot` has no box (`contents`). Elsewhere - the Inbox's
   *  compose dialog - the composer is not a page foot. */
  pageFoot?: boolean;
  selected: ReadonlySet<string>;
}): ReactNode {
  const { plan } = composed;
  const { excluded, recipients, unchecked } = plan;
  const [modeListOpen, setModeListOpen] = useState(false);
  // The selection's own mode, named when it is not the broadcast's usual BTW, so the reader knows
  // what their words become: an Aside, because a selected session takes Aside and not BTW.
  const asideOnly =
    defaulted && delivery === "aside"
      ? agents.filter((agent) => {
          const capabilities = agent.capabilities ?? [];
          return (
            selected.has(agent.session_id) &&
            capabilities.includes("aside") &&
            !capabilities.includes("btw")
          );
        })
      : [];
  const asideLine: AgentPart[] = [
    "Sending as Aside: ",
    ...asideOnly.flatMap((agent, index): AgentPart[] => [
      ...(index === 0 ? [] : [" and "]),
      { sessionId: agent.session_id, storedTitle: agent.title },
    ]),
    ` ${asideOnly.length > 1 ? "do" : "does"} not advertise BTW.`,
  ];
  const text = useAgentText();
  // The server counts the `session_ids` it is sent - these recipients - against the shared limit
  // and refuses a send over it; saying so before Send saves the round trip. The notice line is
  // Send's reason when it refuses for the limit or for nobody to reach; otherwise it still bears
  // on the send, so Send is described by it.
  const sendState = broadcastSendState(plan, delivery, body);
  // The button carries its refusal as text (its `title`, and its own hidden reason line where no
  // line on screen shows it), so a refusal naming agents names them as `AgentName` does.
  const refusal = sendState.refusal === null ? null : sendState.refusal.map(text.part).join("");
  const noticeId = useId();
  // On a narrow or short screen this section is what sticks to the foot of the page's scroll
  // (`BroadcastFoot` has no box there), in the foot's page layer (`hooks/useFootInset.ts` lists
  // the foot's layers and the elements in each). Elsewhere on the Agents page that wrapper holds
  // it and sticks instead, and the wrapper's band holds the section's, so the section's band
  // changes no inset and no ceiling there: it registers on every screen while `pageFoot` is set.
  const sectionRef = useRef<HTMLElement>(null);
  useFootInset(sectionRef, pageFoot, "page");

  // On a narrow or short screen (`narrow-or-short`, styles.css) the composer is a compact grid,
  // so it takes about a third of a phone screen: the heading on one line, the recipients in one
  // sideways-scrolling line, the message sized to its text (`field-sizing`) from two rows up to
  // `max-h-32`, and the mode beside Send. A short screen (a phone in landscape) folds that into
  // two lines - heading beside the recipients, then the message beside the mode and Send - with
  // the message held at two rows. `min-h-16` is important because styles.css's unlayered
  // below-1280 touch floor (`min-height: 44px`) would otherwise override it and shrink the
  // message box on its first character. Elsewhere none of these classes apply.
  // The line under the box saying what a pick does (`DISPATCH_WRITING_BOXES`) is the box's
  // description there rather than a line of its own: this panel is held to 40% of a phone
  // empty and 45% filled (`e2e/agents.e2e.ts`), and a hint of its own costs a line of that
  // budget, which the recipients and the message are owed. Every wider screen shows it.
  return (
    <section
      aria-label="Broadcast"
      className={`max-h-[50vh] overflow-y-auto rounded-xl border p-3 narrow-or-short:sticky narrow-or-short:bottom-0 narrow-or-short:z-10 narrow-or-short:mt-3 narrow-or-short:grid narrow-or-short:grid-cols-[auto_minmax(0,1fr)_auto] narrow-or-short:items-center narrow-or-short:gap-2 short:grid-cols-[auto_minmax(0,1fr)_auto_auto] ${card} ${borderDefault}`}
      ref={sectionRef}
    >
      <h2
        className={`text-sm font-semibold narrow-or-short:col-span-full narrow-or-short:truncate short:col-span-1 ${textPrimaryOnCanvas}`}
      >
        Broadcast to {recipients.length} of {selected.size} selected
      </h2>
      <ul
        aria-label="Selected agents"
        className="mt-2 flex flex-wrap gap-2 narrow-or-short:col-span-full narrow-or-short:mt-0 narrow-or-short:flex-nowrap narrow-or-short:overflow-x-auto short:col-span-3"
      >
        {[...selected].map((sessionID) => {
          const agent = agents.find((candidate) => candidate.session_id === sessionID);
          const left = excluded.find((item) => item.sessionID === sessionID);
          const note = left ?? unchecked.find((item) => item.sessionID === sessionID);
          return (
            <li className="narrow-or-short:shrink-0" key={sessionID}>
              {/* The deselect button keeps the name; the agent's live link sits beside it. */}
              <AgentName
                className="flex items-center"
                control={(name) => (
                  <button
                    className={`min-h-8 rounded-full border px-2 py-1 text-xs narrow-or-short:min-h-11 narrow-or-short:whitespace-nowrap ${secondaryButtonBorder} ${left === undefined ? secondaryButtonText : dangerText} ${secondaryButtonHoverBorder}`}
                    {...heldPress(held, () => onDeselect(sessionID))}
                    title={`Remove ${text.sessionText(sessionID, agent?.title)} from this broadcast`}
                    type="button"
                  >
                    {name}
                    {/* not stored prose: the dashboard's own word for why it gets none */}
                    {note === undefined ? "" : ` · ${note.reason}`} ✕
                  </button>
                )}
                kind="control"
                sessionId={sessionID}
                storedTitle={agent?.title}
              />
            </li>
          );
        })}
      </ul>
      <div className="mt-2 flex flex-wrap items-center gap-2 narrow-or-short:order-2 narrow-or-short:mt-0">
        {/* The shared list's single pick of the modes, whose text box narrows them as the reader
            types; its trigger shows the mode the broadcast sends in. While `held` it is held by
            `aria-disabled`: it keeps the focus, opens on nothing, and a pick changes no mode. */}
        <MultiSelect
          ariaDisabled={held}
          emptyMessage="No mode matches that."
          label="Delivery mode"
          mode="single"
          onOpenChange={(next) => setModeListOpen(next && !held)}
          onPick={(picked) => {
            if (held) return;
            const mode = deliveryMode(picked);
            // A pick of the mode the trigger shows changes nothing, as a select set to the value it
            // holds fires nothing: a restored broadcast stays the one refused, and a mode the
            // selection takes stays unpicked.
            if (mode !== undefined && mode !== delivery) onDelivery(mode);
          }}
          open={modeListOpen}
          optionLabel={capabilityLabel}
          options={DELIVERY_CAPABILITIES}
          searchLabel="Search delivery modes"
          size="comfortable"
          triggerClassName={`rounded-lg border text-left text-sm ${inputClasses(true)}`}
          value={delivery}
        />
      </div>
      {/* The box is one grid cell of the short-screen layout, so the label the field wraps it
          in carries that placement and the box itself only its own size. */}
      <MentionField
        body={body}
        className="text-sm narrow-or-short:mt-0 narrow-or-short:max-h-32 narrow-or-short:min-h-16! narrow-or-short:field-sizing-content short:max-h-16"
        held={held}
        hintClassName={`text-xs narrow-or-short:sr-only ${textMutedOnSurface}`}
        label="Broadcast message"
        labelClassName="mt-2 block w-full narrow-or-short:order-1 narrow-or-short:col-span-full narrow-or-short:mt-0 short:col-span-2"
        labelHidden
        onChange={(change) => onBody(change.body)}
        placeholder="One message, sent to each selected agent"
        readOnly={held}
        rows={3}
        writing={dispatchWritingBox("broadcast", "broadcast")}
      />
      {sendState.notice !== null ? (
        <p className={composerLine} id={noticeId}>
          <AgentParts parts={sendState.notice} />
        </p>
      ) : asideOnly.length > 0 ? (
        <p className={`${composerLineLayout} ${textMutedOnSurface}`}>
          <AgentParts parts={asideLine} />
        </p>
      ) : null}
      <RefusableButton
        busy={held ? "Sending…" : undefined}
        className="mt-2 narrow-or-short:order-2 narrow-or-short:col-start-3 narrow-or-short:mt-0 narrow-or-short:justify-self-end short:col-start-4"
        describedBy={sendState.notice === null || sendState.refusalOnNotice ? undefined : noticeId}
        onPress={() =>
          onSend({
            input: composed.input,
            key: composed.key,
            picked: !defaulted,
            selected: [...selected],
          })
        }
        refusal={refusal}
        refusalShownBy={sendState.refusalOnNotice ? noticeId : undefined}
      >
        {sendState.label}
      </RefusableButton>
    </section>
  );
}
