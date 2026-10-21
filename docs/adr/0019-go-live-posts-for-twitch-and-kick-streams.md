# ADR-0019: Go-live posts for Twitch and Kick streams

**Status:** accepted (2026-10-21, John; built in #144) · **Changes:** a stock bot module, a section of the Organization settings and of bot.yaml · **Tracked by:** hidden-token-gaming/htg#54 (the first consumer)

## Context

A community's streamers are one of its growth sources. Hidden Token Gaming's playbook puts John's Twitch and Kick streams first, and nothing tells the Discord when one goes live. Discord's own Twitch integration only syncs a subscriber role; Kick has none. htg#54 asks for one post per stream (the title, the game, the link), edited when the stream ends, pinging an opt-in role and never `@everyone`, and never a second post for the same stream.

htg#54 first placed this in HTG's half of the bot, but nothing about it is HTG's: any host with streamers wants it. Like moderation (ADR-0013's command) it belongs upstream as a stock module.

The platforms (verified 2026-10-11):

- **Twitch:** Helix `GET /helix/streams?user_login=…` (up to 100 logins) with an **app access token** (client credentials, `POST id.twitch.tv/oauth2/token`) and the `Client-Id` header. It answers the live streams only, each with a stream `id`, `title`, `game_name` and `started_at`. EventSub `stream.online` exists, but its webhook transport needs a public HTTPS route and a subscription to keep alive. Its WebSocket transport needs a user token.
- **Kick:** `GET api.kick.com/public/v1/channels?slug=…` (up to 50) with an app access token (`POST id.kick.com/oauth/token`). Each channel has `stream_title`, `category.name` and `stream {is_live, start_time}`. **There is no stream id.** Kick's webhooks need a public route too.

## Decision

1. **Poll both, with app tokens.** One call per platform per pass, every minute by default (at least 30 s). That is far inside both platforms' limits, and it needs no inbound route, no subscription state, and no user's consent. The bot never calls a viewer endpoint. The platform clients sit behind a `Platform` interface (`Live(ctx, logins) → []Stream`), so a third platform is one more implementation.
2. **A stream's identity.** Twitch's stream id. For Kick, the stream's start time as a Unix time. If Kick gives no start time, the id is `0`, which is one stream while it stays live and never the same stream as an earlier one.
3. **Settings, not config, for the host's choices.** The Organization settings' `discord.streams` hold `channel`, an optional `role` (never the guild's `@everyone`), a `note` and up to 10 `streamers` (`platform` twitch or kick, `login` as in the channel's URL, an optional `label`). bot.yaml's `streams {enabled, interval, twitch, kick}` holds the developer applications' credentials, since secrets never go in the settings. A platform without an application is skipped and logged once.
4. **One post per stream, found again by its marker.** The post shows who, the platform, the title, the game, the link (Discord shows the platform's preview), the note and the role's ping. It ends with `-# stream <platform> \`<login>\` · <id>`.
   - **When the stream ends:** the post is edited. Its first line becomes "was live · ended <relative time>", the ping line goes, the marker gains `· ended`, and embeds are suppressed. An edit pings nobody.
   - **After a restart:** the module reads the channel's 100 newest messages for its own markers, keeping each streamer's newest. A stream it already announced is not announced again. One that ended while it was away is marked ended.
   - **Dedup:** the dedup state is Discord's own message history, as with recaps (ADR-0018) and the server cards. It needs no table, no hub API and no volume, and a host can delete a post to have it made again.
5. **A stream that drops and comes back is one post.** A new stream from a streamer whose post is live, or ended less than 10 minutes ago, edits that post back to live with the new title and id instead of posting and pinging again. A dropped connection or a restarted encoder is common, and a second ping for it is noise.
6. **Failures leave posts as they are.** A platform that errs, refuses the application or rate-limits the bot leaves its streamers' posts unchanged, and a pass only ends a stream when the platform answered. A 429 is honoured: Kick's `Retry-After`, Twitch's `Ratelimit-Reset`, else a minute, at most 15. A refused token is fetched anew once in the same pass. A failed post is retried next pass. If the stream is over by then, nothing is posted.

## Consequences

- Stream metadata only: nothing about viewers is read, and nothing is stored beyond the posts themselves. The host's privacy policy needs no new data line, only that the bot posts the host's own streams.
- A go-live post can be up to one interval plus the platform's own delay late. htg#54's "within a minute" holds at the default interval.
- If the settings move the channel, posts left live in the old channel are not edited.
- A Kick stream with no start time and a restart while it runs: the found post (id `0`) is taken as the same stream. That is right while it stays live.
- Not done: EventSub/webhooks (a public route; revisit if a host has many streamers), YouTube, per-streamer channels or roles, a stream's viewer count.
