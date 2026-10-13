// The dashboard's query defaults. Everything it shows is live: a query refetches every 5 s while
// the tab is visible and stops in a background tab (TanStack Query pauses refetchInterval when the
// document is hidden and refetches on focus). A refusal is not retried: it will not change.
import { Code, ConnectError } from "@connectrpc/connect";
import { QueryClient } from "@tanstack/react-query";

export const pollInterval = 5_000;

const final = new Set([Code.Unauthenticated, Code.PermissionDenied, Code.InvalidArgument, Code.NotFound, Code.FailedPrecondition]);

// isFinal reports whether an error is the hub's answer, not a failure to reach it.
export function isFinal(err: unknown): boolean {
  return final.has(ConnectError.from(err).code);
}

export function makeQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        refetchInterval: pollInterval,
        refetchIntervalInBackground: false,
        refetchOnWindowFocus: true,
        staleTime: pollInterval - 1_000,
        retry: (failures, err) => !isFinal(err) && failures < 2,
      },
    },
  });
}
