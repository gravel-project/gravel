import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";

import { OrganizationSettingsSchema } from "../gen/gravel/hub/v1/organization_pb";
import { member, renderShell } from "../test/rig";

const owner = member({ id: "jo", displayName: "Jo", owner: true });

async function form(): Promise<HTMLElement> {
  return screen.findByRole("form", { name: "Settings" });
}

describe("settings", () => {
  it("adds a notifier and tests it from the browser", async () => {
    const { container, actions } = renderShell({ me: owner, path: "/settings" });
    const f = await form();
    expect(within(f).getByText(/No notifier yet/)).toBeTruthy();
    expect(await axe(container)).toHaveNoViolations();
    fireEvent.click(within(f).getByRole("button", { name: "Add a notifier" }));
    const card = within(f).getByRole("group", { name: "A new notifier" });
    fireEvent.change(within(card).getByLabelText("Name"), { target: { value: "john-phone" } });
    fireEvent.change(within(card).getByLabelText("Topic"), { target: { value: "htg-john" } });
    expect(within(card).getByRole("button", { name: "Send a test" })).toHaveProperty("disabled", true);
    fireEvent.click(within(f).getByRole("button", { name: "Save the settings" }));
    expect(await screen.findByText("Saved. Everyone sees the change at their next page.")).toBeTruthy();
    const saved = actions.find((a) => a.method === "updateOrganizationSettings")?.req as { settings: { notifiers: { name: string; kind: string; topic: string; url: string }[] }; ifUpdatedAt?: unknown };
    expect(saved.settings.notifiers).toEqual([expect.objectContaining({ name: "john-phone", kind: "ntfy", topic: "htg-john", url: "https://ntfy.sh" })]);
    expect(saved.ifUpdatedAt).toBeDefined();

    const test = await screen.findByRole("button", { name: "Send a test" });
    await waitFor(() => { expect(test).toHaveProperty("disabled", false); });
    fireEvent.click(test);
    expect(await screen.findByText("Delivered: check that it arrived.")).toBeTruthy();
    expect(actions.at(-1)).toEqual({ method: "testNotifier", req: expect.objectContaining({ name: "john-phone" }) as unknown });
  });

  it("says why a test was not delivered", async () => {
    renderShell({ me: owner, path: "/settings", settings: { version: 1, notifiers: [{ name: "ops-mail", kind: "email", to: ["ops@example.org"] }] }, notifierErrors: { "ops-mail": "smtp: 550 mailbox unavailable" } });
    fireEvent.click(await screen.findByRole("button", { name: "Send a test" }));
    expect((await screen.findByRole("alert")).textContent).toBe("Not delivered: smtp: 550 mailbox unavailable");
  });

  it("puts each refused field's message under it", async () => {
    renderShell({ me: owner, path: "/settings", settings: { version: 1, notifiers: [{ name: "ops", kind: "ntfy", url: "https://ntfy.sh", topic: "x" }] } });
    const f = await form();
    const card = within(f).getByRole("group", { name: "ops" });
    fireEvent.change(within(card).getByLabelText("Name"), { target: { value: "John Phone" } });
    fireEvent.change(within(f).getByLabelText("Server (guild) id"), { target: { value: "htg" } });
    fireEvent.click(within(f).getByRole("button", { name: "Save the settings" }));
    expect(await within(card).findByText(/"John Phone" is not a name/)).toBeTruthy();
    const guild = within(f).getByLabelText("Server (guild) id").parentElement as HTMLElement;
    expect(within(guild).getByText(/"htg" is not a Discord id/)).toBeTruthy();
    expect(screen.queryByText(/The hub refused the settings/)).toBeNull();
  });

  it("refuses to overwrite someone else's save, and loads theirs on request", async () => {
    const { store } = renderShell({ me: owner, path: "/settings" });
    const f = await form();
    // Someone else saves while this page is open.
    store.settings = create(OrganizationSettingsSchema, { version: 1, stats: { public: true }, updatedAt: timestampFromDate(new Date(2_000_000)) });
    fireEvent.click(within(f).getByLabelText("Server (guild) id"));
    fireEvent.change(within(f).getByLabelText("Server (guild) id"), { target: { value: "123" } });
    fireEvent.click(within(f).getByRole("button", { name: "Save the settings" }));
    expect(await screen.findByText(/Someone changed the settings since you opened this page, so nothing was saved/)).toBeTruthy();
    expect(store.settings.discord).toBeUndefined();
    fireEvent.click(screen.getByRole("button", { name: "Load the current settings" }));
    await waitFor(() => { expect(screen.getByLabelText<HTMLInputElement>(/Anyone may see the leaderboards/).checked).toBe(true); });
    expect(screen.getByLabelText<HTMLInputElement>("Server (guild) id").value).toBe("");
  });

  it("says how to connect Discord", async () => {
    renderShell({ me: owner, path: "/settings" });
    expect(await screen.findByText(/Discord isn’t connected/)).toBeTruthy();
  });

  it("keeps a trailing comma while a list is typed", async () => {
    renderShell({ me: owner, path: "/settings", settings: { version: 1, notifiers: [{ name: "ops-mail", kind: "email", to: ["ops@example.org"] }] } });
    const to = await screen.findByLabelText<HTMLInputElement>("To (commas between addresses)");
    fireEvent.change(to, { target: { value: "ops@example.org," } });
    expect(to.value).toBe("ops@example.org,");
  });

  it("edits the whole document as YAML: check, what would change, save", async () => {
    const { actions, store } = renderShell({ me: owner, path: "/settings" });
    fireEvent.click(await screen.findByRole("button", { name: "YAML, the whole document" }));
    const text = await screen.findByLabelText<HTMLTextAreaElement>("Settings as YAML");
    await waitFor(() => { expect(text.value).toContain("version: 1"); });
    fireEvent.click(screen.getByRole("button", { name: "Check" }));
    expect((await screen.findByRole("status")).textContent).toBe("Valid: 0 nav links, theme default, discord unmapped.");
    fireEvent.change(text, { target: { value: "version: 1\nnav:\n  - {label: Rules, url: /rules}\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Show what would change" }));
    expect((await screen.findByRole("status")).textContent).toContain("Saving would change: nav.");
    const before = store.settings.updatedAt?.seconds;
    fireEvent.click(screen.getByRole("button", { name: "Save the document" }));
    expect((await screen.findByRole("status")).textContent).toBe("Saved: nav changed.");
    expect(store.settings.updatedAt?.seconds).not.toBe(before);
    expect(actions.filter((a) => a.method === "applySettings").map((a) => (a.req as { dryRun: boolean; ifUpdatedAt?: unknown }).dryRun)).toEqual([true, false]);
    fireEvent.change(screen.getByLabelText("Settings as YAML"), { target: { value: "bad: 1\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Check" }));
    expect((await screen.findByRole("alert")).textContent).toContain("bad: unknown field");
  });

  it("is in the nav for whoever writes the settings only", async () => {
    renderShell({ me: member({ capabilities: ["servers:read", "settings:write"] }) });
    expect(await screen.findByRole("link", { name: "Settings" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Apps" })).toBeNull();
  });
});
