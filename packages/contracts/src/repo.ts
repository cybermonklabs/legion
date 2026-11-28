import { goTrimSpace } from "./go-blank";

export function canonicalRepo(owner: string, repo: string): string {
  return `${goTrimSpace(owner).toLowerCase()}/${goTrimSpace(repo)
    .toLowerCase()
    .replace(/\.git$/, "")}`;
}
