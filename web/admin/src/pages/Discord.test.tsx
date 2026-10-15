import { create, equals, type MessageInitShape } from "@bufbuild/protobuf";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";

import { DiscordSettingsSchema, type OrganizationSettings, OrganizationSettingsSchema } from "../gen/gravel/hub/v1/organization_pb";
import { isPlaced } from "../settings";
import { member, renderShell } from "../test/rig";

const owner = member({ id: "jo", displayName: "Jo", owner: true });

const servers = [
  { id: "htg-wardogs-1", name: "HTG WARDOGS #1", gameId: "wardogs" },
  { id: "htg-cs2-1", name: "HTG CS2 #1", gameId: "cs2" },
];
const games = [
  { id: "wardogs", name: "War Dogs" },
  { id: "cs2", name: "Counter-Strike 2" },
];

const id = (n: number) => "11500000000000000" + String(n).padStart(2, "0");

// discord is every Discord setting set, as HTG runs it.
const discord: MessageInitShape<typeof DiscordSettingsSchema> = {
  guildId: id(0),
  modLog: id(1),
  roles: { linked: id(2), providers: { steam: id(3) } },
  recognition: [{ role: id(4), rule: "first_members", count: 100 }],
  serverCards: [{ server: "htg-wardogs-1", channel: id(5), note: "Matches start at 20 players." }],
  seeding: [{ server: "htg-wardogs-1", channel: id(6), role: id(7), below: 20, cooldown: "2h", callAtStart: true, note: "Join from the server browser", hours: [{ days: ["wed"], from: "18:00", to: "23:00" }] }],
  recaps: [{ server: "htg-wardogs-1", channel: id(8), title: "Wardogs Wednesday", floor: 20, note: "Thanks for coming", hours: [{ days: ["wed", "sat"], from: "22:00", to: "02:00" }] }],
  presence: { channel: id(9), note: "Counts only", games: [{ game: "wardogs", label: "War Dogs", activities: ["WARDOGS"], applicationIds: [id(10)] }] },
  streams: { channel: id(11), role: id(12), note: "Go say hi", streamers: [{ platform: "twitch", login: "hiddentoken", label: "HTG" }, { platform: "kick", login: "hidden-token" }] },
  news: { feeds: [{ appId: 730, channel: id(13), label: "Counter-Strike 2", feeds: ["steam_community_announcements"], color: 0xde9b35 }] },
  matchResults: { channel: id(14), servers: ["htg-cs2-1", "htg-old-1"] },
};

const full: MessageInitShape<typeof OrganizationSettingsSchema> = { version: 1, stats: { timezone: "America/Chicago" }, discord };

async function form(): Promise<HTMLElement> {
  return screen.findByRole("form", { name: "Settings" });
}

function input(sel: string): HTMLInputElement {
  const el = document.querySelector<HTMLInputElement>("#" + sel);
  if (!el) {
    throw new Error("no #" + sel);
  }
  return el;
}

function saved(actions: { method: string; req: unknown }[]): OrganizationSettings {
  const a = actions.filter((x) => x.method === "updateOrganizationSettings").at(-1);
  return (a?.req as { settings: OrganizationSettings }).settings;
}

describe("discord settings", () => {
  it("round-trips every section through the form unchanged", async () => {
    const { actions, container } = renderShell({ me: owner, path: "/settings", settings: full, servers, games });
    const f = await form();
    await within(f).findAllByRole("option", { name: "HTG WARDOGS #1 (htg-wardogs-1)", selected: true });
    expect(await axe(container)).toHaveNoViolations();
    fireEvent.click(within(f).getByRole("button", { name: "Save the settings" }));
    expect(await screen.findByText("Saved. Everyone sees the change at their next page.")).toBeTruthy();
    const want = create(OrganizationSettingsSchema, full);
    const got = saved(actions);
    got.updatedAt = undefined;
    expect(equals(OrganizationSettingsSchema, got, want)).toBe(true);
  });

  it("shows each setting where it is edited", async () => {
    renderShell({ me: owner, path: "/settings", settings: full, servers, games });
    await screen.findByLabelText("HTG WARDOGS #1 (htg-wardogs-1)");
    expect(input("discord-provider-steam").value).toBe(id(3));
    expect(input("recognition-0-count").value).toBe("100");
    expect(input("seeding-0-cooldown").value).toBe("2h");
    expect(input("seeding-0-start").checked).toBe(true);
    expect(input("seeding-0-hours-0-wed").checked).toBe(true);
    expect(input("recap-0-hours-0-from").value).toBe("22:00");
    expect(input("presence-0-activities").value).toBe("WARDOGS");
    expect(input("streamer-1-login").value).toBe("hidden-token");
    expect(input("news-0-color").value).toBe("#de9b35");
    expect(input("results-server-htg-cs2-1").checked).toBe(true);
    expect(input("results-server-htg-wardogs-1").checked).toBe(false);
    expect(screen.getByLabelText("htg-old-1 (not a server on this hub)")).toHaveProperty("checked", true);
    expect(screen.getAllByText("Weekly hours, in America/Chicago")).toHaveLength(2);
  });

  it("adds a seeding rule with a server from the hub's list", async () => {
    const { actions } = renderShell({ me: owner, path: "/settings", settings: { version: 1, discord: { guildId: id(0) } }, servers, games });
    const f = await form();
    fireEvent.click(within(f).getByRole("button", { name: "Add a seeding rule" }));
    const rule = within(f).getByRole("group", { name: "Seeding for a server" });
    await within(rule).findByRole("option", { name: "HTG WARDOGS #1 (htg-wardogs-1)" });
    fireEvent.change(within(rule).getByLabelText("Server"), { target: { value: "htg-wardogs-1" } });
    fireEvent.change(within(rule).getByLabelText("Channel id"), { target: { value: id(6) } });
    fireEvent.change(within(rule).getByLabelText("Seeder role id"), { target: { value: id(7) } });
    fireEvent.click(within(rule).getByRole("button", { name: "Add a window" }));
    fireEvent.click(input("seeding-0-hours-0-fri"));
    fireEvent.click(input("seeding-0-hours-0-mon"));
    fireEvent.change(input("seeding-0-hours-0-to"), { target: { value: "02:00" } });
    fireEvent.click(within(f).getByRole("button", { name: "Save the settings" }));
    await screen.findByText("Saved. Everyone sees the change at their next page.");
    const s = saved(actions).discord?.seeding[0];
    expect(s).toMatchObject({ server: "htg-wardogs-1", channel: id(6), role: id(7), below: 20, hours: [{ days: ["mon", "fri"], from: "18:00", to: "02:00" }] });
  });

  it("edits the single sections and drops an emptied provider role", async () => {
    const { actions } = renderShell({ me: owner, path: "/settings", settings: full, servers, games });
    const f = await form();
    fireEvent.change(input("discord-provider-steam"), { target: { value: "" } });
    fireEvent.change(input("discord-provider-discord"), { target: { value: id(20) } });
    fireEvent.click(input("results-server-htg-wardogs-1"));
    fireEvent.click(input("results-server-htg-cs2-1"));
    fireEvent.change(input("streamer-0-platform"), { target: { value: "kick" } });
    fireEvent.click(input("news-0-color-own"));
    fireEvent.click(within(f).getByRole("button", { name: "Add a streamer" }));
    fireEvent.change(input("streamer-2-login"), { target: { value: "jomkz" } });
    fireEvent.click(within(f).getByRole("button", { name: "Save the settings" }));
    await screen.findByText("Saved. Everyone sees the change at their next page.");
    const d = saved(actions).discord;
    expect(d?.roles?.providers).toEqual({ discord: id(20) });
    expect(d?.matchResults?.servers).toEqual(["htg-old-1", "htg-wardogs-1"]);
    expect(d?.streams?.streamers.map((x) => [x.platform, x.login])).toEqual([["kick", "hiddentoken"], ["kick", "hidden-token"], ["twitch", "jomkz"]]);
    expect(d?.news?.feeds[0]?.color).toBe(0);
  });

  it("keeps each entry's typed text when an earlier one is removed", async () => {
    const two: MessageInitShape<typeof OrganizationSettingsSchema> = { ...full, discord: { ...discord, presence: { channel: id(9), games: [{ game: "wardogs", label: "War Dogs", activities: ["WARDOGS"] }, { game: "cs2", label: "Counter-Strike 2", activities: ["Counter-Strike 2", "CS2"] }] } } };
    const { actions } = renderShell({ me: owner, path: "/settings", settings: two, servers, games });
    const f = await form();
    const first = within(f).getByRole("group", { name: "War Dogs" });
    fireEvent.click(within(first).getByRole("button", { name: "Remove this game" }));
    await waitFor(() => { expect(input("presence-0-activities").value).toBe("Counter-Strike 2, CS2"); });
    fireEvent.click(within(f).getByRole("button", { name: "Save the settings" }));
    await screen.findByText("Saved. Everyone sees the change at their next page.");
    expect(saved(actions).discord?.presence?.games.map((g) => g.game)).toEqual(["cs2"]);
  });

  it("puts every message about Discord in the form, under its field", async () => {
    const paths = [
      "discord.guild_id", "discord.mod_log", "discord.roles.linked", "discord.roles.providers.steam", "discord.roles.providers.discord",
      "discord.recognition", "discord.recognition[0]", "discord.recognition[0].role", "discord.recognition[0].rule", "discord.recognition[0].count",
      "discord.server_cards", "discord.server_cards[0].server", "discord.server_cards[0].channel", "discord.server_cards[0].note",
      "discord.seeding", "discord.seeding[0].server", "discord.seeding[0].channel", "discord.seeding[0].role", "discord.seeding[0].below",
      "discord.seeding[0].cooldown", "discord.seeding[0].note", "discord.seeding[0].hours", "discord.seeding[0].hours[0]",
      "discord.seeding[0].hours[0].days", "discord.seeding[0].hours[0].from", "discord.seeding[0].hours[0].to",
      "discord.recaps", "discord.recaps[0].server", "discord.recaps[0].channel", "discord.recaps[0].title", "discord.recaps[0].floor", "discord.recaps[0].note",
      "discord.recaps[0].hours[0].days",
      "discord.presence.channel", "discord.presence.note", "discord.presence.games", "discord.presence.games[0]", "discord.presence.games[0].game",
      "discord.presence.games[0].label", "discord.presence.games[0].activities[0]", "discord.presence.games[0].application_ids[0]",
      "discord.streams.channel", "discord.streams.role", "discord.streams.note", "discord.streams.streamers", "discord.streams.streamers[1]",
      "discord.streams.streamers[0].platform", "discord.streams.streamers[0].login", "discord.streams.streamers[0].label",
      "discord.news.feeds", "discord.news.feeds[0]", "discord.news.feeds[0].app_id", "discord.news.feeds[0].channel", "discord.news.feeds[0].label",
      "discord.news.feeds[0].feeds[0]", "discord.news.feeds[0].color",
      "discord.match_results.channel", "discord.match_results.servers", "discord.match_results.servers[1]",
    ];
    for (const p of paths) {
      expect(isPlaced(p), p).toBe(true);
    }
    expect(isPlaced("discord.roles.providers.xbox")).toBe(false);
    renderShell({ me: owner, path: "/settings", settings: full, servers, games, violations: paths.map((p) => ({ path: p, message: "about " + p })) });
    const f = await form();
    fireEvent.click(within(f).getByRole("button", { name: "Save the settings" }));
    await within(f).findByText("about discord.guild_id");
    expect(screen.queryByText(/The hub refused the settings/)).toBeNull();
    for (const p of paths) {
      expect(within(f).getByText(new RegExp("about " + p.replace(/[.[\]]/g, "\\$&") + "(;|$)")), p).toBeTruthy();
    }
    // A field's own message sits under it.
    expect(within(input("seeding-0-cooldown").parentElement as HTMLElement).getByText("about discord.seeding[0].cooldown")).toBeTruthy();
    expect(within(input("presence-0-activities").parentElement as HTMLElement).getByText("about discord.presence.games[0].activities[0]")).toBeTruthy();
  });

  it("lists a message the form has no place for at the top", async () => {
    renderShell({ me: owner, path: "/settings", settings: full, servers, games, violations: [{ path: "theme.dark.accent", message: "not a colour" }] });
    const f = await form();
    fireEvent.click(within(f).getByRole("button", { name: "Save the settings" }));
    expect((await within(f).findByText(/The hub refused the settings/)).parentElement?.textContent).toContain("theme.dark.accent: not a colour");
  });
});
