import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";

import { member, renderShell } from "../test/rig";

const owner = member({ id: "jo", displayName: "Jo", owner: true });
const bot = { clientId: "gravel_bot", name: "HTG Bot", scopes: ["identity:read", "roles:grant"], createdAt: timestampFromDate(new Date("2026-10-01T12:00:00Z")) };

describe("apps", () => {
  it("registers an app and shows its secret once", async () => {
    const { container, actions } = renderShell({ me: owner, path: "/apps", apps: [bot] });
    expect(await screen.findByText("HTG Bot")).toBeTruthy();
    expect(await axe(container)).toHaveNoViolations();
    const reg = screen.getByRole("form", { name: "Register an app" });
    fireEvent.change(within(reg).getByLabelText("Name"), { target: { value: "Muster" } });
    fireEvent.click(within(reg).getByLabelText(/stats:read/));
    fireEvent.click(within(reg).getByLabelText(/participation:write/));
    fireEvent.change(within(reg).getByLabelText(/Its own scopes/), { target: { value: "host:muster:read" } });
    fireEvent.change(within(reg).getByLabelText(/Where its pages receive/), { target: { value: "https://muster.example/cb\n" } });
    fireEvent.click(within(reg).getByRole("button", { name: "Register" }));
    const shown = await screen.findByText("s3cret-shown-once");
    expect(actions[0]).toEqual({
      method: "createApp",
      req: expect.objectContaining({ name: "Muster", scopes: ["stats:read", "participation:write", "host:muster:read"], redirectUris: ["https://muster.example/cb"] }) as unknown,
    });
    expect(await screen.findByText("Muster")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    await waitFor(() => { expect(shown.isConnected).toBe(false); });
    expect(screen.queryByText("s3cret-shown-once")).toBeNull();
    expect(screen.getByRole("form", { name: "Register an app" })).toBeTruthy();
  });

  it("revokes an app, asked first", async () => {
    const { actions } = renderShell({ me: owner, path: "/apps", apps: [bot] });
    fireEvent.click(await screen.findByRole("button", { name: "Revoke" }));
    expect(screen.getByText(/Its tokens stop working at once/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Yes, revoke it" }));
    expect(await screen.findByText("revoked")).toBeTruthy();
    expect(actions).toEqual([{ method: "revokeApp", req: expect.objectContaining({ clientId: "gravel_bot" }) as unknown }]);
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
  });

  it("is in the nav for whoever manages apps only", async () => {
    renderShell({ me: member({ capabilities: ["servers:read", "apps:manage"] }) });
    expect(await screen.findByRole("link", { name: "Apps" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Settings" })).toBeNull();
  });
});
