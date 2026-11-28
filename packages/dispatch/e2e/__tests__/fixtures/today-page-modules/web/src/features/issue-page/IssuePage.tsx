import { blockRef } from "@fixture/contracts";
import { lazy, type ReactNode } from "react";

const IssueOutlinePage = lazy(() =>
  import("./IssueOutlinePage").then((module) => ({ default: module.IssueOutlinePage }))
);

export function IssuePage({ outline }: { outline: boolean }): ReactNode {
  return outline ? <IssueOutlinePage /> : <p>{blockRef("today")}</p>;
}
