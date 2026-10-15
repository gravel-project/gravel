import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";

import { member, renderShell } from "../test/rig";

const servers = [
  { id: "htg-cs2-1", name: "HTG CS2 #1", gameId: "cs2", driver: "cloudvm", status: { state: "online" } },
  { id: "htg-wardogs-1", name: "HTG WARDOGS #1", gameId: "wardogs", driver: "wardogs", status: { state: "online" } },
];
const sizes = [
  { slug: "c-2", vcpus: 2, memoryMib: 4096, priceHourlyUsd: 0.063 },
  { slug: "c-4", vcpus: 4, memoryMib: 8192, priceHourlyUsd: 0.125 },
];
const running = { state: "running", desired: "running", size: "c-2", sizes, address: "198.51.100.1", hourlyPriceUsd: 0.063, monthCostUsd: 12.5 };
const parked = { state: "parked", desired: "parked", size: "c-2", sizes };

const owner = member({ id: "jo", displayName: "Jo", owner: true });
const operator = member({ id: "op", displayName: "Op", roles: ["operator"], capabilities: ["servers:read", "servers:operate", "servers:lease"] });
const moderator = member({ id: "mod", displayName: "Mod", roles: ["moderator"], capabilities: ["servers:read", "servers:moderate", "servers:lease"] });

const path = "/servers/htg-cs2-1";

function buttons(): string[] {
  const s = screen.queryByRole("heading", { name: "Actions" })?.closest("section");
  return s ? within(s).getAllByRole("button").map((b) => b.textContent) : [];
}

describe("a server's controls", () => {
  it("lets an operator park a running server, and names the event", async () => {
    const { container, actions } = renderShell({ me: operator, path, servers, hosts: { "htg-cs2-1": running } });
    expect(await screen.findByRole("heading", { name: "HTG CS2 #1" })).toBeTruthy();
    await screen.findByRole("heading", { name: "Actions" });
    expect(buttons()).toEqual(["Park", "Reboot", "Expect players"]);
    expect(await axe(container)).toHaveNoViolations();

    fireEvent.click(screen.getByRole("button", { name: "Park" }));
    const form = screen.getByRole("form", { name: "Park" });
    fireEvent.change(within(form).getByLabelText("Reason (kept in the audit log)"), { target: { value: "nobody tonight" } });
    fireEvent.click(within(form).getByRole("button", { name: "Park" }));
    const done = await screen.findByRole("status");
    expect(done.textContent).toBe("Parking: the players are warned, then the machine stops. Event #101.");
    expect(within(done).getByRole("link", { name: "Event #101" }).getAttribute("href")).toBe("/admin/events?server=htg-cs2-1");
    expect(actions).toEqual([{ method: "setServerHostDesired", req: expect.objectContaining({ serverId: "htg-cs2-1", desired: "parked", reason: "nobody tonight" }) as unknown }]);
    // The host moved: no actions until it settles.
    expect(await screen.findByText("The host is parking: actions return when it settles.")).toBeTruthy();
  });

  it("gives a moderator no buttons", async () => {
    renderShell({ me: moderator, path, servers, hosts: { "htg-cs2-1": running } });
    expect(await screen.findByText("Your roles give you no actions on this server.")).toBeTruthy();
    expect(buttons()).toEqual([]);
  });

  it("gates what costs or removes behind the server's id, typed", async () => {
    const { actions } = renderShell({ me: owner, path, servers, hosts: { "htg-cs2-1": parked } });
    await screen.findByRole("heading", { name: "Actions" });
    expect(buttons()).toEqual(["Start", "Snapshot the volume", "Destroy"]);
    fireEvent.click(screen.getByRole("button", { name: "Destroy" }));
    const form = screen.getByRole("form", { name: "Destroy" });
    const go = within(form).getByRole("button", { name: "Destroy" });
    expect(go).toHaveProperty("disabled", true);
    fireEvent.change(within(form).getByLabelText(/to confirm/), { target: { value: "htg-cs2-2" } });
    expect(go).toHaveProperty("disabled", true);
    fireEvent.submit(form);
    expect(actions).toEqual([]);
    fireEvent.click(within(form).getByLabelText(/Delete the data volume too/));
    fireEvent.change(within(form).getByLabelText(/to confirm/), { target: { value: "htg-cs2-1" } });
    expect(go).toHaveProperty("disabled", false);
    fireEvent.click(go);
    await waitFor(() => { expect(actions).toHaveLength(1); });
    expect(actions[0]).toEqual({ method: "destroyServerHost", req: expect.objectContaining({ deleteVolume: true, releaseAddress: false }) as unknown });
  });

  it("resizes and pins, typed, and unpins a pinned size", async () => {
    const { actions } = renderShell({ me: owner, path, servers, hosts: { "htg-cs2-1": running } });
    await screen.findByRole("heading", { name: "Actions" });
    expect(buttons()).toEqual(["Park", "Reboot", "Expect players", "Resize"]);
    fireEvent.click(screen.getByRole("button", { name: "Resize" }));
    const form = screen.getByRole("form", { name: "Resize" });
    expect(within(form).getByLabelText<HTMLSelectElement>("Size").value).toBe("c-4");
    fireEvent.click(within(form).getByLabelText(/Pin it/));
    fireEvent.change(within(form).getByLabelText(/to confirm/), { target: { value: "htg-cs2-1" } });
    fireEvent.click(within(form).getByRole("button", { name: "Resize" }));
    expect((await screen.findByRole("status")).textContent).toMatch(/^Resizing to c-4, pinned\. Event #101\.$/);
    expect(actions[0]).toEqual({ method: "resizeServerHost", req: expect.objectContaining({ size: "c-4", pin: true, force: false }) as unknown });
    await waitFor(() => { expect(buttons()).toEqual(["Park", "Reboot", "Expect players", "Resize", "Unpin the size"]); });
  });

  it("reboots with players on when asked", async () => {
    const { actions } = renderShell({ me: operator, path, servers, hosts: { "htg-cs2-1": running } });
    await screen.findByRole("heading", { name: "Actions" });
    fireEvent.click(screen.getByRole("button", { name: "Reboot" }));
    const form = screen.getByRole("form", { name: "Reboot" });
    fireEvent.click(within(form).getByLabelText(/Even with players on/));
    fireEvent.click(within(form).getByRole("button", { name: "Reboot" }));
    expect((await screen.findByRole("status")).textContent).toMatch(/^Rebooting: .* Event #101\.$/);
    expect(actions[0]).toEqual({ method: "rebootServerHost", req: expect.objectContaining({ force: true }) as unknown });
  });

  it("shows a refusal in the hub's words, and keeps the form", async () => {
    renderShell({ me: operator, path, servers, hosts: { "htg-cs2-1": running }, refuse: ["rebootServerHost"] });
    await screen.findByRole("heading", { name: "Actions" });
    fireEvent.click(screen.getByRole("button", { name: "Reboot" }));
    const form = screen.getByRole("form", { name: "Reboot" });
    fireEvent.click(within(form).getByRole("button", { name: "Reboot" }));
    expect((await screen.findByRole("alert")).textContent).toBe("The hub refused: not allowed to change that grant: only the owner changes an admin grant");
    expect(screen.getByRole("form", { name: "Reboot" })).toBeTruthy();
  });

  it("releases a live lease, asked first", async () => {
    const { actions } = renderShell({ me: operator, path, servers, hosts: { "htg-cs2-1": running }, leases: { "htg-cs2-1": { id: "l-1", holder: "muster", pool: "pug", state: "ready" } } });
    expect(await screen.findByText(/Held by muster from pool pug · ready/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Release the lease" }));
    fireEvent.click(screen.getByRole("button", { name: "Yes, release it" }));
    expect((await screen.findByRole("status")).textContent).toMatch(/^The lease is released: .* Event #101\.$/);
    expect(actions).toEqual([{ method: "releaseServerLease", req: expect.objectContaining({ leaseId: "l-1" }) as unknown }]);
    await waitFor(() => { expect(screen.queryByRole("heading", { name: "Lease" })).toBeNull(); });
  });

  it("says a server outside the cloud-VM driver has no machine to act on", async () => {
    renderShell({ me: owner, path: "/servers/htg-wardogs-1", servers });
    expect(await screen.findByText(/is not a cloud-VM server/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "member page and moderation" }).getAttribute("href")).toBe("/servers/htg-wardogs-1");
  });

  it("links Manage on the fleet for whoever may act on the game", async () => {
    const fleet = { totals: { servers: 1, up: 1 }, servers: [{ id: "htg-cs2-1", name: "HTG CS2 #1", gameId: "cs2", health: "up", reason: "answering" }] };
    const view = renderShell({ me: operator, fleet });
    expect((await screen.findByRole("link", { name: "Manage HTG CS2 #1" })).getAttribute("href")).toBe("/admin/servers/htg-cs2-1");
    view.unmount();
    renderShell({ me: member({ capabilities: ["servers:read"] }), fleet });
    await screen.findByRole("link", { name: "HTG CS2 #1" });
    expect(screen.queryByRole("link", { name: /Manage/ })).toBeNull();
  });
});
