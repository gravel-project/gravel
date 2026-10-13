// The gate: who is calling, from GetMe with the session cookie. A visitor without a session is
// sent to the hub's login page, which brings them back here (/login?return_to=, ADR-0015's guard
// on the path); the dashboard has no login of its own.
import { Code, ConnectError } from "@connectrpc/connect";
import { useQuery } from "@tanstack/react-query";
import { createContext, type ReactNode, useContext, useEffect } from "react";

import { IdentityService, type User } from "./gen/gravel/hub/v1/identity_pb";
import { type Organization, OrganizationService } from "./gen/gravel/hub/v1/organization_pb";
import { useClient } from "./transport";

// Browser is what the shell needs from the window, so a test can stand in for it.
export interface Browser {
  // here is the current path and query, /admin included.
  here(): string;
  // leave loads another page of the hub (a full navigation, not a route of the dashboard).
  leave(to: string): void;
}

export const windowBrowser: Browser = {
  here: () => window.location.pathname + window.location.search,
  leave: (to) => {
    window.location.assign(to);
  },
};

const BrowserContext = createContext<Browser>(windowBrowser);

export function BrowserProvider({ browser, children }: { browser: Browser; children: ReactNode }) {
  return <BrowserContext value={browser}>{children}</BrowserContext>;
}

export function useBrowser(): Browser {
  return useContext(BrowserContext);
}

export function loginPath(here: string): string {
  return "/login?return_to=" + encodeURIComponent(here);
}

export type MeState =
  | { state: "loading" }
  | { state: "anonymous" }
  | { state: "error"; message: string }
  | { state: "ready"; me: User };

// useMe is the caller, polled like everything else: a revoked grant changes the nav within a poll,
// and an ended session sends the viewer to log in.
export function useMe(): MeState {
  const identity = useClient(IdentityService);
  const browser = useBrowser();
  const q = useQuery({
    queryKey: ["me"],
    queryFn: async ({ signal }) => (await identity.getMe({}, { signal })).user ?? null,
  });
  const anonymous = q.error !== null && ConnectError.from(q.error).code === Code.Unauthenticated;
  useEffect(() => {
    if (anonymous) {
      browser.leave(loginPath(browser.here()));
    }
  }, [anonymous, browser]);
  if (anonymous) {
    return { state: "anonymous" };
  }
  if (q.data) {
    return { state: "ready", me: q.data };
  }
  if (q.error !== null) {
    return { state: "error", message: ConnectError.from(q.error).rawMessage };
  }
  if (q.isSuccess) {
    return { state: "error", message: "the hub answered without a member" };
  }
  return { state: "loading" };
}

// useOrganization is the hub's name, and whether it is a demo; anyone may read it.
export function useOrganization(): Organization | undefined {
  const org = useClient(OrganizationService);
  const q = useQuery({
    queryKey: ["organization"],
    queryFn: async ({ signal }) => (await org.getOrganization({}, { signal })).organization ?? null,
  });
  return q.data ?? undefined;
}
