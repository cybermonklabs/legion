/**
 * The cache layer's vocabulary that features build with: the keys a read and a mutation are named
 * by, compared as TanStack compares them, and the shapes a read hands back. Nothing here reads a
 * cache: a value shared by deep equality (`replaceEqualDeep`) is the caller's own.
 */
export type {
  DataTag,
  DefinedUseQueryResult,
  InfiniteData,
  MutationKey,
  QueryKey,
  UseInfiniteQueryResult,
  UseQueryResult,
} from "@tanstack/react-query";
export {
  hashKey,
  partialMatchKey,
  replaceEqualDeep,
} from "@tanstack/react-query";
