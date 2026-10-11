# ADR-0017: "Who's playing now", a presence card with counts only

**Status:** proposed (2026-10-11; accepted when John merges it) · **Changes:** how a host shows which games its members are playing, and how a module asks for a gateway intent · **Tracked by:** hidden-token-gaming/htg#32 (its presence half); ADR-0016 §7 deferred it to a decision of its own

## Context

Hidden Token Gaming's plan pairs seeding with "who's playing now": how many members are in each game right now, so a member sees where the people are. ADR-0016 shipped the seeding half and set presence aside because it needs two things seeding didn't:

- **Discord's presence intent.** Discord shows what a member is playing only to a bot that asks the gateway for `GUILD_PRESENCES`, a privileged intent. The application's owner turns it on in the Developer Portal: a toggle for a bot in under 100 guilds, a review above that. A bot that asks for a privileged intent it isn't allowed is disconnected with close code 4014, and the session never comes up. The runtime asked for a fixed set (guilds and guild members), and a host's `cli.Program` had no way to add one.
- **A privacy line of its own.** What a member plays is personal. The host's policy has to say what the bot sees and keeps.

Nothing here is specific to HTG: any host with a Discord guild and several games has the question, so it is a stock module (htg's rule: generic belongs upstream).

## Decision

1. **A stock module, `discord/modules/presence`, off by default** (`presence.enabled` in bot.yaml, a pass every minute, at least 15 s). Where the card goes and which games it counts are the Organization settings' `discord.presence`, like the server cards and seeding (ADR-0007): a `channel`, an optional `note`, and up to 20 `games`, each a `game` key, a `label`, and how its Discord activity is recognized (`activities`, names compared without regard to case, and `application_ids`). No name or id may count for two games.
2. **One card, counts only.** One Components V2 message, edited in place when a count changes, found again after a restart by the marker in its footer (as the server cards). It shows a line per game ("**War Dogs** · 3 playing", "nobody right now"), the note, and when the counts last changed. It never names a member, allows no mention, and escapes the host's text.
3. **Nothing stored.** The module keeps a live view in memory: for each online member, their playing activities' names and application ids, nothing else. It is built from the guild's presences when the gateway delivers the guild, then each presence update and member leave. Offline members, and activities that aren't playing or competing (listening, streaming, a custom status), never enter it. Nothing goes to the hub or to disk, and the only exported numbers are the per-game counts and the view's size. A restart starts from the guild's presences again.
4. **Honest when blind.** Without the intent the card says the bot can't see what members are playing; it never shows counts of nobody. Before the gateway has delivered the guild, the card is left as it was.
5. **Modules declare their intents (`bot.IntentsModule`).** A module may implement `Intents() gateway.Intents`; the runtime asks for the union with its base. For a privileged intent a module adds, the runtime reads the application's flags first (one REST call at start, only then). If the application isn't allowed it, the intent is left out, `gravel_bot_gateway_intent_missing{intent}` is 1 and an error names the portal toggle. So one module's missing toggle costs that module, never the whole bot: role sync, cards and commands keep running. A module learns what it got from `Registry.HasIntent`. If the flags can't be read, the intents are asked for as configured and Discord decides. The base (`Options.Intents`, guilds and guild members) is the host's choice and isn't checked. This is also the hook a host module will use for voice states (Sea of Thieves' temporary voice channels).

## Consequences

- A host turns presence on in three steps: the portal toggle, `presence.enabled`, and a `discord.presence` card in the settings. In any other order the bot still runs; the card or the gauge says what is missing.
- Only members who share their activity on Discord are counted. The card says so in its footer, and the host's privacy policy has to say what the bot sees and that it keeps no record (HTG: hidden-token-gaming/site, the presence row).
- Matching is by what Discord shows, which the host has to look up: a Steam game appears under the name Discord detects (War Dogs: to be confirmed live), a verified game also by its application id.
- A guild above Discord's large threshold still gets every online member's presence when the guild is delivered, which is all the card needs.
- Not done: per-game voice channels, history of any kind, a slash command (the card is the surface), and presence on the hub's pages. Each would be a decision of its own, and history would need the privacy policy first.
