// The shell over fake services: a router transport (no network) answering as the test says, a
// memory location under /admin, and a browser that records where the shell sent the viewer.
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { render } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { memoryLocation } from "wouter/memory-location";

import { App } from "../App";
import { IdentityService, UserSchema } from "../gen/gravel/hub/v1/identity_pb";
import { OrganizationSchema, OrganizationService } from "../gen/gravel/hub/v1/organization_pb";
import type { Browser } from "../session";

export interface RigOptions {
  // me is the caller; undefined means no session.
  me?: Member;
  demo?: boolean;
  // path is the route under /admin.
  path?: string;
}

type Member = MessageInitShape<typeof UserSchema>;

export function member(fields: Member = {}): Member {
  return { id: "u1", displayName: "Jo", ...fields };
}

export function renderShell({ me, demo = false, path = "/" }: RigOptions = {}) {
  const transport = createRouterTransport(({ service }) => {
    service(IdentityService, {
      getMe: () => {
        if (me === undefined) {
          throw new ConnectError("log in first", Code.Unauthenticated);
        }
        return { user: create(UserSchema, me) };
      },
    });
    service(OrganizationService, {
      getOrganization: () => ({ organization: create(OrganizationSchema, { id: "o1", name: "Hidden Token Gaming", owned: true, demo }) }),
    });
  });
  const left: string[] = [];
  const here = "/admin" + (path === "/" ? "" : path);
  const browser: Browser = { here: () => here, leave: (to) => left.push(to) };
  // No retries and no polling: a test sees the first answer.
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, refetchInterval: false } } });
  const { hook } = memoryLocation({ path: here });
  const view = render(<App transport={transport} queryClient={queryClient} browser={browser} location={hook} />);
  return { ...view, left };
}
