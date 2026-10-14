import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";

import { member, renderShell } from "../test/rig";

const games = [
  { id: "cs2", name: "Counter-Strike 2" },
  { id: "wardogs", name: "War Dogs" },
];

const owner = member({ id: "jo", displayName: "Jo", owner: true, identities: [{ provider: "local", subject: "jo" }] });
const admin = member({ id: "ana", displayName: "Ana", roles: ["admin"], capabilities: ["identity:read", "roles:grant", "members:manage"], grants: [{ role: "admin" }] });
const users = [
  owner,
  admin,
  member({ id: "sam", displayName: "Sam", grants: [{ role: "operator", gameId: "cs2" }], identities: [{ provider: "local", subject: "sam" }],
    createdAt: timestampFromDate(new Date("2026-09-01T10:00:00Z")) }),
  member({ id: "kim", displayName: "Kim", identities: [{ provider: "discord", subject: "42", displayName: "kim#0" }] }),
];

// row is a member's entry in the list.
async function row(name: string): Promise<HTMLElement> {
  const link = await screen.findByRole("link", { name });
  return link.closest("li") as HTMLElement;
}

describe("People", () => {
  it("lists the members with their grants, by game name, and passes axe", async () => {
    const { container } = renderShell({ me: owner, path: "/people", users, games });
    const sam = await row("Sam");
    expect(within(sam).getByRole("list", { name: "Sam's roles" }).textContent).toContain("operator · Counter-Strike 2");
    expect(within(sam).getByText(/local · sam/)).toBeTruthy();
    expect(within(sam).getByText(/Joined 2026-09-01 10:00 UTC · last login never/)).toBeTruthy();
    expect(within(await row("Jo")).getByText("you")).toBeTruthy();
    expect(screen.getByRole("link", { name: "People" }).getAttribute("aria-current")).toBe("page");
    expect(await axe(container)).toHaveNoViolations();
  });

  it("grants and revokes as the granter rules allow, and says what changed", async () => {
    const { actions } = renderShell({ me: owner, path: "/people", users, games });
    const sam = await row("Sam");
    const form = within(sam).getByRole("form", { name: "Grant Sam a role" });
    // The owner may grant admin; every game and each game are offered.
    expect(within(form).getAllByRole("option").map((o) => o.textContent)).toEqual(["admin", "operator", "moderator", "Every game", "Counter-Strike 2", "War Dogs"]);
    fireEvent.change(within(form).getByLabelText("For"), { target: { value: "wardogs" } });
    fireEvent.click(within(form).getByRole("button", { name: "Grant" }));
    expect(await within(sam).findByRole("status")).toHaveProperty("textContent", "Sam is now moderator · War Dogs.");
    await waitFor(() => { expect(within(sam).getByRole("list", { name: "Sam's roles" }).textContent).toContain("moderator · War Dogs"); });

    fireEvent.click(within(sam).getByRole("button", { name: "Revoke operator · Counter-Strike 2 from Sam" }));
    expect(await within(sam).findByText("Sam is no longer operator · Counter-Strike 2.")).toBeTruthy();
    expect(actions.map((a) => a.method)).toEqual(["setUserRole", "setUserRole"]);
    expect(actions[0]?.req).toMatchObject({ userId: "sam", role: "moderator", gameId: "wardogs", granted: true });
    expect(actions[1]?.req).toMatchObject({ userId: "sam", role: "operator", gameId: "cs2", granted: false });
    // Nobody changes their own grants: the owner's row has no form.
    expect(within(await row("Jo")).queryByRole("form")).toBeNull();
  });

  it("keeps admin grants to the owner, and a game admin to their game", async () => {
    renderShell({ me: admin, path: "/people", users, games });
    const form = within(await row("Sam")).getByRole("form", { name: "Grant Sam a role" });
    expect(within(form).getAllByRole("option").map((o) => o.textContent)).toEqual(["operator", "moderator", "Every game", "Counter-Strike 2", "War Dogs"]);
    // Ana's own admin grant can't be revoked by Ana; nobody's admin grant by a non-owner.
    expect(within(await row("Ana")).queryByRole("button", { name: /Revoke/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Hand the hub/ })).toBeNull();
  });

  it("offers a game's admin that game only", async () => {
    const cs2Admin = member({ id: "lee", displayName: "Lee", capabilities: ["identity:read"], grants: [{ role: "admin", gameId: "cs2" }] });
    renderShell({ me: cs2Admin, path: "/people", users: [...users, cs2Admin], games });
    const sam = await row("Sam");
    const form = within(sam).getByRole("form", { name: "Grant Sam a role" });
    expect(within(form).getAllByRole("option").map((o) => o.textContent)).toEqual(["operator", "moderator", "Counter-Strike 2"]);
    expect(within(sam).getByRole("button", { name: "Revoke operator · Counter-Strike 2 from Sam" })).toBeTruthy();
    // No members:manage: no accounts, links or logouts.
    expect(screen.queryByRole("heading", { name: "Add a local account" })).toBeNull();
    expect(within(sam).queryByRole("button", { name: "Password reset link" })).toBeNull();
    expect(within(sam).queryByRole("button", { name: "Log out everywhere" })).toBeNull();
  });

  it("shows a new account's password link once, absolute, until Done", async () => {
    const { actions } = renderShell({ me: owner, path: "/people", users, games });
    const add = (await screen.findByRole("heading", { name: "Add a local account" })).closest("section") as HTMLElement;
    fireEvent.change(within(add).getByLabelText("Username"), { target: { value: " mo " } });
    fireEvent.click(within(add).getByRole("button", { name: "Make the account" }));
    const shown = await within(add).findByText(/^http:\/\/localhost(:\d+)?\/auth\/local\/reset\/set-mo$/);
    expect(within(add).getByText(/shown once and works once, until 2026-10-15 12:00 UTC/)).toBeTruthy();
    expect(actions[0]).toEqual({ method: "createLocalAccount", req: expect.objectContaining({ username: "mo", email: "" }) as unknown });
    expect(await screen.findByRole("link", { name: "mo" })).toBeTruthy();
    fireEvent.click(within(add).getByRole("button", { name: "Done" }));
    await waitFor(() => { expect(shown.isConnected).toBe(false); });
    expect(within(add).queryByText(/set-mo/)).toBeNull();
    expect(within(add).getByLabelText("Username")).toHaveProperty("value", "");
  });

  it("issues a reset link for a local account only", async () => {
    const { actions } = renderShell({ me: owner, path: "/people", users, games });
    expect(within(await row("Kim")).queryByRole("button", { name: "Password reset link" })).toBeNull();
    const sam = await row("Sam");
    fireEvent.click(within(sam).getByRole("button", { name: "Password reset link" }));
    expect(await within(sam).findByText("https://hub.example.org/auth/local/reset/reset-sam")).toBeTruthy();
    expect(within(sam).getByText(/their earlier links no longer work/)).toBeTruthy();
    expect(actions).toEqual([{ method: "issuePasswordReset", req: expect.objectContaining({ username: "sam" }) as unknown }]);
    fireEvent.click(within(sam).getByRole("button", { name: "Done" }));
    expect(within(sam).queryByText(/reset-sam/)).toBeNull();
  });

  it("asks before logging a member out everywhere", async () => {
    const { actions } = renderShell({ me: owner, path: "/people", users, games });
    const kim = await row("Kim");
    fireEvent.click(within(kim).getByRole("button", { name: "Log out everywhere" }));
    fireEvent.click(within(kim).getByRole("button", { name: "Cancel" }));
    expect(actions).toEqual([]);
    fireEvent.click(within(kim).getByRole("button", { name: "Log out everywhere" }));
    expect(within(kim).getByText(/End every session Kim has/)).toBeTruthy();
    fireEvent.click(within(kim).getByRole("button", { name: "Yes, log out everywhere" }));
    expect(await within(kim).findByText("Kim is logged out everywhere (3 sessions ended).")).toBeTruthy();
    expect(actions).toEqual([{ method: "revokeUserSessions", req: expect.objectContaining({ userId: "kim" }) as unknown }]);
  });

  it("says what the hub refused", async () => {
    renderShell({ me: owner, path: "/people", users, games, refuse: ["setUserRole"] });
    const sam = await row("Sam");
    fireEvent.click(within(sam).getByRole("button", { name: "Grant" }));
    expect((await within(sam).findByRole("alert")).textContent).toBe("The hub refused: not allowed to change that grant: only the owner changes an admin grant");
  });

  it("pages through the members", async () => {
    renderShell({ me: owner, path: "/people", users, games, pageSize: 2 });
    await row("Ana");
    expect(screen.queryByRole("link", { name: "Kim" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "More members" }));
    await row("Kim");
    expect(screen.queryByRole("button", { name: "More members" })).toBeNull();
  });
});

describe("handing the hub over", () => {
  it("lets the owner nominate, see the nomination, and cancel it", async () => {
    const { actions, organization } = renderShell({ me: owner, path: "/people", users, games });
    const sam = await row("Sam");
    fireEvent.click(within(sam).getByRole("button", { name: "Hand the hub to Sam" }));
    fireEvent.click(within(sam).getByRole("button", { name: "Yes, hand the hub to Sam" }));
    const banner = (await screen.findByRole("heading", { name: "Handing the hub over" })).closest("section") as HTMLElement;
    expect(within(banner).getByText("Sam may take the hub over until 2026-10-21 12:00 UTC. Until they accept, you own it.")).toBeTruthy();
    expect(organization.pendingOwnerUserId).toBe("sam");
    fireEvent.click(within(banner).getByRole("button", { name: "Cancel the nomination" }));
    await waitFor(() => { expect(screen.queryByRole("heading", { name: "Handing the hub over" })).toBeNull(); });
    expect(actions.map((a) => a.method)).toEqual(["nominateOwner", "cancelOwnershipTransfer"]);
  });

  it("brings a nominee who reads no members here, to accept or decline", async () => {
    const sam = member({ id: "sam", displayName: "Sam" });
    const pending = { pendingOwnerUserId: "sam", pendingOwnerName: "Sam", pendingOwnerExpiresAt: timestampFromDate(new Date("2026-10-21T12:00:00Z")) };
    const { actions, container } = renderShell({ me: sam, path: "/people", users, games, org: pending, refuse: ["listUsers"] });
    expect(await screen.findByText("The owner offers you Hidden Token Gaming, until 2026-10-21 12:00 UTC. If you accept, you own it and they stay an admin.")).toBeTruthy();
    // The nav shows People to the nominee; nothing else is offered, and no refusal is shown.
    expect(screen.getByRole("link", { name: "People" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Members" })).toBeNull();
    expect(screen.queryByText(/do not include reading the members/)).toBeNull();
    expect(await axe(container)).toHaveNoViolations();
    fireEvent.click(screen.getByRole("button", { name: "Accept the hub" }));
    await waitFor(() => { expect(actions.map((a) => a.method)).toEqual(["acceptOwnership"]); });
  });

  it("tells a member without identity:read why the list is not there", async () => {
    renderShell({ me: member({ id: "kim", displayName: "Kim" }), path: "/people", users, games });
    expect(await screen.findByText(/do not include reading the members \(identity:read\)/)).toBeTruthy();
    expect(screen.queryByRole("link", { name: "People" })).toBeNull();
  });
});
