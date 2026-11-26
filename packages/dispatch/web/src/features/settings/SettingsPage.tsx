import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { textPrimaryOnCanvas, textSecondaryOnCanvas } from "../../theme/classes";
import { GrantsSection } from "../credentials/GrantsSection";
import { credentialPendingQuery } from "../credentials/pending";
import { SignInsSection } from "../credentials/SignInsSection";
import { IssuePagePreference } from "../issue-page/IssuePagePreference";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { AgentTokensSection } from "./AgentTokensSection";
import { ArchitectureSourcesSection } from "./ArchitectureSourcesSection";
import { settingsSectionGap } from "./classes";
import { LegionDaemonsSection } from "./LegionDaemonsSection";
import { ProjectsSection } from "./ProjectsSection";

export function SettingsPage(): ReactNode {
  useDocumentTitle("Settings · Dispatch");
  // `null` is a Dispatch with no secrets broker, where Live grants has nothing to list.
  const credentialsAvailable = useQuery(credentialPendingQuery());

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
        <IssuePagePreference />
        <LegionDaemonsSection />
      </div>
    </section>
  );
}
