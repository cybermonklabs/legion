import { api } from "../../api/client";
import type { BlockSchema } from "../../api/types";

/**
 * Loads one server schema per session while retaining parsed schemas by their version. A
 * failed fetch is not retained: the next `load` fetches again, so a transient error on page
 * load does not leave every document surface without a schema until reload.
 */
export class BlockSchemaCache {
  private readonly schemas = new Map<number, BlockSchema>();
  private loading: Promise<BlockSchema> | undefined;

  load(fetchSchema: () => Promise<BlockSchema>): Promise<BlockSchema> {
    if (this.loading !== undefined) {
      return this.loading;
    }
    const loading = fetchSchema().then(
      (schema) => {
        this.schemas.set(schema.version, schema);
        return schema;
      },
      (error: unknown) => {
        if (this.loading === loading) {
          this.loading = undefined;
        }
        throw error;
      }
    );
    this.loading = loading;
    return loading;
  }

  /**
   * Reads the schema again, whatever this cache holds, and retains what it reads by version.
   * Its one caller is a document whose socket the server closed because the schema it serves
   * moved: the session's first load is no answer there. A read that fails leaves every surface
   * on the schema the cache held, and the next `load` fetches again.
   */
  reload(fetchSchema: () => Promise<BlockSchema>): Promise<BlockSchema> {
    this.loading = undefined;
    return this.load(fetchSchema);
  }

  forVersion(version: number): BlockSchema | undefined {
    return this.schemas.get(version);
  }
}

export const blockSchemaCache = new BlockSchemaCache();

/** Fetches the current schema once, then retains it by version for every document surface. */
export function loadBlockSchema(): Promise<BlockSchema> {
  return blockSchemaCache.load(() => api.getBlockSchema());
}

/** Fetches the schema the server has installed now, in place of the one this session loaded. */
export function reloadBlockSchema(): Promise<BlockSchema> {
  return blockSchemaCache.reload(() => api.getBlockSchema());
}
