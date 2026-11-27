import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef } from "react";
import { useLocation } from "react-router-dom";

import { textPrimaryOnCanvas, textSecondaryOnCanvas } from "../../theme/classes";
import { GrantsSection } from "../credentials/GrantsSection";
import { credentialPendingQuery } from "../credentials/pending";
import { SignInsSection } from "../credentials/SignInsSection";
import { DeliverySettingsForm } from "../delivery/DeliverySettingsForm";
import { IssuePagePreference } from "../issue-page/IssuePagePreference";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { AgentTokensSection } from "./AgentTokensSection";
import { ArchitectureSourcesSection } from "./ArchitectureSourcesSection";
import { settingsSectionGap } from "./classes";
import { KeyboardShortcutsSection } from "./KeyboardShortcutsSection";
import { LegionDaemonsSection } from "./LegionDaemonsSection";
import { ProjectsSection } from "./ProjectsSection";

/** The id an address's `#` part names, or `undefined` for none and for a part that is no valid
 *  percent-encoding (`#%E0%A4%A`), which opens Settings at its top rather than throwing. */
function fragmentId(hash: string): string | undefined {
  if (hash.length <= 1) return undefined;
  try {
    return decodeURIComponent(hash.slice(1));
  } catch {
    return undefined;
  }
}

/**
 * Settings. Its groups, in this order, one under another in a single column at every width
 * (the layout rule of `packages/dispatch/web/src/AGENTS.md`, dispatch://INFRA-5200):
 *
 * 1. Heading: the page's title and the one line that says what it holds.
 * 2. Projects (`ProjectsSection`).
 * 3. Agent tokens (`AgentTokensSection`).
 * 4. Live grants and Sign-ins (`GrantsSection`, `SignInsSection`), drawn only on a Dispatch with
 *    a secrets broker; a Dispatch without one has nothing for them to list.
 * 5. Architecture sources (`ArchitectureSourcesSection`).
 * 6. Delivery timeline: its heading, its line, and its form (`DeliverySettingsForm`).
 * 7. The issue page (`IssuePagePreference`).
 * 8. Legion daemons (`LegionDaemonsSection`).
 *
 * Each section lays out its own rows; none sits beside another. An address that names a section
 * by its `#` part (`/settings#architecture-sources-heading`, which an issue's Components line
 * links to while its project has no source) opens the page at that section.
 */
export function SettingsPage(): ReactNode {
  useDocumentTitle("Settings · Dispatch");
  // `null` is a Dispatch with no secrets broker, where Live grants has nothing to list.
  const credentialsAvailable = useQuery(credentialPendingQuery());
  // The app routes in the page, so the browser's own scroll to a `#` part never runs: the page
  // scrolls to the section once the address names it. On a Dispatch with a broker, Live grants
  // and Sign-ins are drawn above the later sections once the broker's read settles, which moves
  // the section the address names, so the page scrolls to it once more then - unless the reader
  // has scrolled since, by wheel, touch or keys or by the scroll position moving from where the
  // first scroll left it: where the reader has gone is theirs.
  const { hash } = useLocation();
  const brokerSettled = !credentialsAvailable.isPending;
  const correction = useRef<{ id: string; readerMoved: boolean; scrollY: number } | null>(null);
  useEffect(() => {
    correction.current = null;
    const id = fragmentId(hash);
    const target = id === undefined ? null : document.getElementById(id);
    if (id === undefined || target === null) return;
    target.scrollIntoView();
    const pending = { id, readerMoved: false, scrollY: window.scrollY };
    correction.current = pending;
    const moved = () => {
      pending.readerMoved = true;
    };
    const scrolled = () => {
      if (Math.abs(window.scrollY - pending.scrollY) > 1) pending.readerMoved = true;
    };
    window.addEventListener("wheel", moved, { passive: true });
    window.addEventListener("touchmove", moved, { passive: true });
    window.addEventListener("keydown", moved);
    window.addEventListener("scroll", scrolled, { passive: true });
    return () => {
      window.removeEventListener("wheel", moved);
      window.removeEventListener("touchmove", moved);
      window.removeEventListener("keydown", moved);
      window.removeEventListener("scroll", scrolled);
    };
  }, [hash]);
  useEffect(() => {
    const pending = correction.current;
    if (!brokerSettled || pending === null) return;
    correction.current = null;
    if (!pending.readerMoved) document.getElementById(pending.id)?.scrollIntoView();
  }, [brokerSettled]);

  return (
    <section className="max-w-4xl">
      <header className="mb-6">
        <h1 className={`text-2xl font-semibold ${textPrimaryOnCanvas}`}>Settings</h1>
        <p className={`mt-1 ${textSecondaryOnCanvas}`}>
          Manage the projects, the agent tokens and the settings of this Dispatch.
        </p>
      </header>
      <div className={settingsSectionGap}>
        <ProjectsSection />
        <AgentTokensSection />
        {credentialsAvailable.isPending || credentialsAvailable.data === null ? null : (
          <>
            <GrantsSection />
            <SignInsSection />
          </>
        )}
        <ArchitectureSourcesSection />
        <section aria-labelledby="delivery-timeline-heading">
          <h2
            className={`text-xl font-semibold ${textPrimaryOnCanvas}`}
            id="delivery-timeline-heading"
          >
            Delivery timeline
          </h2>
          <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
            Which repository deploys, and whose merged pull requests the Delivery page counts.
            Saving verifies the Dispatch GitHub App can read the deploy repository.
          </p>
          <DeliverySettingsForm />
        </section>
        <IssuePagePreference />
        <KeyboardShortcutsSection />
        <LegionDaemonsSection />
      </div>
    </section>
  );
}
