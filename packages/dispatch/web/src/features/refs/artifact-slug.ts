/** The slug Dispatch gives a document, the one a reference can name: words of lowercase letters
 *  and digits joined by single hyphens. A lens kind's id is written the same way. It stands in a
 *  module of its own because routes.ts and store-address.ts both read it, and routes.ts imports
 *  store-address.ts. */
export const artifactSlugPattern = "[a-z0-9]+(?:-[a-z0-9]+)*";
