/** The project an issue key belongs to. A project key holds no hyphen (`^[A-Z][A-Z0-9]{1,9}$`,
 *  the server's `ProjectKeyPattern`), so the part of an issue key before the first one is its
 *  project: `CORE-12` belongs to `CORE`. It is the one rule the web application reads a key's
 *  project by. */
export function issueProject(key: string): string {
  return key.split("-")[0] ?? key;
}
