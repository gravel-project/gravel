// How the dashboard reaches the hub: Connect over fetch to this origin, the session cookie sent
// with each call (ADR-0029: no bearer token in JavaScript). Tests hand the shell a router transport
// with fake services instead.
import type { DescService } from "@bufbuild/protobuf";
import { type Client, createClient, type Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { createContext, type ReactNode, useContext, useMemo } from "react";

export function hubTransport(): Transport {
  return createConnectTransport({ baseUrl: window.location.origin });
}

const TransportContext = createContext<Transport | null>(null);

export function TransportProvider({ transport, children }: { transport: Transport; children: ReactNode }) {
  return <TransportContext value={transport}>{children}</TransportContext>;
}

// useClient is a Connect client for one of the hub's services.
export function useClient<T extends DescService>(service: T): Client<T> {
  const transport = useContext(TransportContext);
  if (transport === null) {
    throw new Error("useClient outside a TransportProvider");
  }
  return useMemo(() => createClient(service, transport), [service, transport]);
}
