import {
  type DefaultError,
  type UseMutationOptions,
  type UseMutationResult,
  useMutation as useLibraryMutation,
} from "@tanstack/react-query";

/**
 * A mutation's options and result in the cache layer's own terms: TanStack's, but every callback
 * TanStack hands its function context - which carries the `QueryClient` - is handed the rest
 * alone. The write engine that makes every write a surface presses (`useKeyedWrite`) is built on
 * this hook, so no feature imports TanStack's.
 */

/** The options of TanStack's whose callbacks are handed the client. */
type HandsOver = "mutationFn" | "onError" | "onMutate" | "onSettled" | "onSuccess";

/** What a mutation does as it is made, and once Dispatch answers it or refuses it. */
export type MutationOptions<TData, TError, TVariables, TContext> = Omit<
  UseMutationOptions<TData, TError, TVariables, TContext>,
  HandsOver
> & {
  readonly mutationFn?: (variables: TVariables) => Promise<TData>;
  readonly onMutate?: (variables: TVariables) => Promise<TContext> | TContext;
  readonly onSuccess?: (data: TData, variables: TVariables, context: TContext) => unknown;
  readonly onError?: (
    error: TError,
    variables: TVariables,
    context: TContext | undefined
  ) => unknown;
  readonly onSettled?: (
    data: TData | undefined,
    error: TError | null,
    variables: TVariables,
    context: TContext | undefined
  ) => unknown;
};

/** What one call of `mutate` is told besides the mutation's own options. */
export interface MutateCallbacks<TData, TError, TVariables, TContext> {
  readonly onSuccess?: (data: TData, variables: TVariables, context: TContext | undefined) => void;
  readonly onError?: (error: TError, variables: TVariables, context: TContext | undefined) => void;
  readonly onSettled?: (
    data: TData | undefined,
    error: TError | null,
    variables: TVariables,
    context: TContext | undefined
  ) => void;
}

/** `mutate`'s arguments: the variables, optional where they may be undefined. */
type MutateArguments<TData, TError, TVariables, TContext> = undefined extends TVariables
  ? [variables?: TVariables, callbacks?: MutateCallbacks<TData, TError, TVariables, TContext>]
  : [variables: TVariables, callbacks?: MutateCallbacks<TData, TError, TVariables, TContext>];

/** `T` without `K`, taken from each member of a union apart, so a result told apart by its
 *  `status` still is. */
type OmitEach<T, K extends PropertyKey> = T extends unknown ? Omit<T, K> : never;

/** A mutation as a component holds it. */
export type MutationResult<TData, TError, TVariables, TContext> = OmitEach<
  UseMutationResult<TData, TError, TVariables, TContext>,
  "mutate" | "mutateAsync"
> & {
  readonly mutate: (...rest: MutateArguments<TData, TError, TVariables, TContext>) => void;
  readonly mutateAsync: (
    ...rest: MutateArguments<TData, TError, TVariables, TContext>
  ) => Promise<TData>;
};

/** TanStack's `useMutation`, in the terms above. */
export function useMutation<
  TData = unknown,
  TError = DefaultError,
  TVariables = void,
  TContext = unknown,
>(
  options: MutationOptions<TData, TError, TVariables, TContext>
): MutationResult<TData, TError, TVariables, TContext> {
  return useLibraryMutation(options as UseMutationOptions<TData, TError, TVariables, TContext>);
}
