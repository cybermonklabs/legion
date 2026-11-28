/** Codex thread IDs can be used as literal path segments and glob fragments. */
export function threadIdShape(id: string): boolean {
  return /^[0-9A-Za-z-]+$/.test(id)
}
