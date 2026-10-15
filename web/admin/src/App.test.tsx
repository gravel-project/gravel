import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";

import { demoNotice } from "./App";
import { member, renderShell } from "./test/rig";

describe("the shell", () => {
  it("shows a member with no capabilities only their access, and says so", async () => {
    const { container } = renderShell({ me: member(), path: "/access" });
    expect(await screen.findByRole("heading", { level: 1, name: "Your access" })).toBeTruthy();
    const nav = screen.getByRole("navigation", { name: "Admin" });
    expect(within(nav).getAllByRole("link").map((a) => a.textContent)).toEqual(["Your access"]);
    expect(screen.getByText(/no role is granted to you/)).toBeTruthy();
    expect(screen.getByText(/Nothing beyond what every visitor may/)).toBeTruthy();
    const account = within(screen.getByRole("navigation", { name: "Account" }));
    expect(account.getByRole("link", { name: "Account" }).getAttribute("href")).toBe("/account");
    expect(await axe(container)).toHaveNoViolations();
  });

  it("sends a visitor without a session to the login, back to where they were", async () => {
    const { left } = renderShell({ path: "/people" });
    await waitFor(() => {
      expect(left).toEqual(["/login?return_to=%2Fadmin%2Fpeople"]);
    });
    expect(screen.getByRole("status").textContent).toMatch(/login page/);
  });

  it("lists an owner's roles, grants and capabilities", async () => {
    const { container } = renderShell({
      path: "/access",
      me: member({
        owner: true,
        roles: ["admin"],
        capabilities: ["members:manage", "servers:read"],
        grants: [{ userId: "u1", role: "moderator", gameId: "cs2" }],
      }),
    });
    const roles = within((await screen.findByRole("heading", { name: "Your roles" })).closest("section") as HTMLElement);
    expect(roles.getAllByRole("listitem").map((li) => li.textContent)).toEqual(["owner, everywhere", "admin, everywhere", "moderator, for cs2"]);
    expect(screen.getByText("members:manage")).toBeTruthy();
    const nav = screen.getByRole("navigation", { name: "Admin" });
    expect(within(nav).getAllByRole("link").map((a) => a.textContent)).toEqual(["Fleet", "Events", "People", "Metrics", "Stats", "Activity", "Settings", "Apps", "Your access"]);
    expect(await axe(container)).toHaveNoViolations();
  });

  it("says a demo hub's data is made up", async () => {
    const { container } = renderShell({ me: member(), demo: true });
    expect(await screen.findByText(demoNotice)).toBeTruthy();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("answers an unknown route inside the shell", async () => {
    const { container } = renderShell({ me: member(), path: "/nowhere" });
    expect(await screen.findByRole("heading", { level: 1, name: "Not found" })).toBeTruthy();
    expect(await axe(container)).toHaveNoViolations();
  });
});
