# ADR-0013: Hub roles: the owner grants moderator, and the hub is its authority

**Status:** accepted (2026-10-10; John chose it) · **Changes:** who may moderate servers, and what an app's `on_behalf_of` must be · **Tracked by:** #18 (moderation from the web); hidden-token-gaming/htg#57 (the same procedures from Discord)

## Context

Moderation (ADR-0010, step 3) admits the owner and apps with `servers:moderate`. A community has more than one person keeping its servers in order, and the web UI is the authoritative admin surface (principle 9), so the hub needs a role it can check itself. Three sources were on the table (gravel#18, 2026-10-10): a role the owner grants in the hub; a Discord role the bot reports; or owner-only for now.

## Decision

1. **A role is on the member, and the hub is its authority.** `users.roles` (migration 12) holds the roles the owner granted; today only `moderator`, and the column refuses anything else. The owner holds every power already and needs no role.
2. **The owner grants and revokes** with `IdentityService.SetUserRole`, from the web UI's Members page. Every change that changes something is appended to `role_changes` (member, role, granted or revoked, by whom, when). A trigger refuses updates and deletes.
3. **A moderator moderates as the owner does.** `ModerationService` (kick, ban, unban, message, broadcast, move, the ban list, the audit log) admits the owner, a moderator, or an app with `servers:moderate`. The audit row names the moderator.
4. **An app acts only for someone who could act themselves.** When an app sends `on_behalf_of`, that identity must resolve to the owner or a moderator, or the call is refused (`permission_denied`). A bot can't lend its scope to anyone who asks it. An app acting for itself (no `on_behalf_of`: an automation, a scheduled broadcast) is admitted on its scope.
5. **Discord follows the hub,** never the other way round: role sync can mirror `moderator` to a Discord role later, the same direction as every other mapping (ADR-0008).

## Consequences

- HTG's staff link their Discord account to the hub and are granted the role before htg#57's `/wd` commands work for them. That's intended: the hub decides.
- Configuration (`ServerConfigService`) stays the owner's or `servers:configure`; moderators don't change server settings.
- A new role is a migration (the CHECK) plus a constant, and every role is a decision of this kind, not a setting.
