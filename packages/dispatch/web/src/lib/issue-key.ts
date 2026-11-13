/** The project an issue key belongs to. A project key holds no hyphen (`^[A-Z][A-Z0-9]{1,9}$`,
 *  the server's `ProjectKeyPattern`), so the part of an issue key before the first one is its
 *  project: `CORE-12` belongs to `CORE`. The one rule every page reads a key's project by: the
 *  Inbox's rows, the event feed's rules, an issue's assignee write and the Project column of an
 *  issues table that lists every project's work. */
export function issueProject(key: string): string {
  return key.split("-")[0] ?? key;
}
