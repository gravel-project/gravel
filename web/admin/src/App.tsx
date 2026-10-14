// The shell: the demo banner, the header with the pages this member may use, and the page. Links
// out of the dashboard (the account, the member pages) are plain links to the hub's own pages.
import type { Transport } from "@connectrpc/connect";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, Route, Router, Switch, useRoute } from "wouter";
import type { BaseLocationHook } from "wouter";

import type { User } from "./gen/gravel/hub/v1/identity_pb";
import type { Organization } from "./gen/gravel/hub/v1/organization_pb";
import { type NavItem, visible } from "./nav";
import { NotFound } from "./pages/NotFound";
import { Access } from "./pages/Access";
import { Events } from "./pages/Events";
import { Fleet } from "./pages/Fleet";
import { People } from "./pages/People";
import { type Browser, BrowserProvider, useMe, useOrganization, windowBrowser } from "./session";
import { TransportProvider } from "./transport";

// The member pages say the same (internal/web/templates DemoNotice).
export const demoNotice = "Demo data: these members, matches and stats are made up";

export interface AppProps {
  transport: Transport;
  queryClient: QueryClient;
  browser?: Browser;
  // location is wouter's location hook: the browser's history unless a test passes a memory one.
  location?: BaseLocationHook;
}

export function App({ transport, queryClient, browser = windowBrowser, location }: AppProps) {
  return (
    <TransportProvider transport={transport}>
      <QueryClientProvider client={queryClient}>
        <BrowserProvider browser={browser}>
          <Router base="/admin" {...(location ? { hook: location } : {})}>
            <Shell />
          </Router>
        </BrowserProvider>
      </QueryClientProvider>
    </TransportProvider>
  );
}

function Shell() {
  const me = useMe();
  const org = useOrganization();
  switch (me.state) {
    case "loading":
    case "anonymous":
      return (
        <Frame org={org}>
          <p role="status" className="text-muted">
            {me.state === "loading" ? "Loading…" : "Taking you to the login page…"}
          </p>
        </Frame>
      );
    case "error":
      return (
        <Frame org={org}>
          <p role="alert">The hub did not answer: {me.message}. Trying again shortly.</p>
        </Frame>
      );
    case "ready":
      return (
        <Frame org={org} me={me.me}>
          <Switch>
            <Route path="/">
              <Fleet />
            </Route>
            <Route path="/events">
              <Events />
            </Route>
            <Route path="/people">
              <People me={me.me} org={org} />
            </Route>
            <Route path="/access">
              <Access me={me.me} org={org} />
            </Route>
            <Route>
              <NotFound />
            </Route>
          </Switch>
        </Frame>
      );
  }
}

function Frame({ org, me, children }: { org: Organization | undefined; me?: User; children: ReactNode }) {
  return (
    <>
      <a href="#main" className="absolute -left-[999px] top-0 bg-ground px-4 py-2 focus:left-4">
        Skip to content
      </a>
      {org?.demo && (
        <section aria-label="Demo hub" className="border-b-[3px] border-accent px-4 py-2 text-center font-bold">
          <p>{demoNotice}</p>
        </section>
      )}
      <header className="flex flex-wrap items-center justify-between gap-4 border-b border-line px-4 py-3">
        <div className="flex flex-wrap items-center gap-4">
          <span className="font-bold">
            {org?.name ?? "gravel"} <span className="font-normal text-muted">admin</span>
          </span>
          {me && <Nav items={visible(me, undefined, org)} />}
        </div>
        <nav aria-label="Account" className="flex flex-wrap items-center gap-4">
          {me && <span className="text-muted">{me.displayName}</span>}
          <a href="/servers">Member pages</a>
          <a href="/account">Account</a>
        </nav>
      </header>
      <main id="main" tabIndex={-1} className="mx-auto max-w-6xl p-4 focus:outline-none">
        {children}
      </main>
    </>
  );
}

function Nav({ items }: { items: NavItem[] }) {
  return (
    <nav aria-label="Admin">
      <ul className="flex flex-wrap gap-4">
        {items.map((item) => (
          <li key={item.to}>
            <NavLink item={item} />
          </li>
        ))}
      </ul>
    </nav>
  );
}

function NavLink({ item }: { item: NavItem }) {
  const [active] = useRoute(item.to);
  return (
    <Link href={item.to} aria-current={active ? "page" : undefined} className={active ? "font-bold" : undefined}>
      {item.label}
    </Link>
  );
}
