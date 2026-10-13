// Overview: who the dashboard thinks you are here, and what you may do. The fleet overview
// (gravel#190's summary) takes this page over; until then it is the place to check a grant.
import type { User } from "../gen/gravel/hub/v1/identity_pb";
import type { Organization } from "../gen/gravel/hub/v1/organization_pb";

export function Overview({ me, org }: { me: User; org: Organization | undefined }) {
  const roles = me.owner ? ["owner", ...me.roles] : me.roles;
  const scoped = me.grants.filter((g) => g.gameId !== "");
  return (
    <>
      <h1 className="mb-4 text-2xl font-bold">Overview</h1>
      <p className="mb-6">
        You are signed in to {org?.name ?? "this hub"} as <strong>{me.displayName}</strong>.
      </p>
      <section aria-labelledby="roles" className="mb-6">
        <h2 id="roles" className="mb-2 text-xl font-bold">
          Your roles
        </h2>
        {roles.length === 0 && scoped.length === 0 ? (
          <p>You are a member: no role is granted to you. An owner or admin grants roles on the hub.</p>
        ) : (
          <ul className="list-disc pl-6">
            {roles.map((r) => (
              <li key={r}>{r}, everywhere</li>
            ))}
            {scoped.map((g) => (
              <li key={g.role + "/" + g.gameId}>
                {g.role}, for {g.gameId}
              </li>
            ))}
          </ul>
        )}
      </section>
      <section aria-labelledby="capabilities">
        <h2 id="capabilities" className="mb-2 text-xl font-bold">
          What you may do everywhere
        </h2>
        {me.capabilities.length === 0 ? (
          <p>Nothing beyond what every visitor may: the pages here show only what your roles allow.</p>
        ) : (
          <ul className="flex flex-wrap gap-2">
            {me.capabilities.map((c) => (
              <li key={c} className="rounded border border-line px-2 py-0.5 font-mono text-sm">
                {c}
              </li>
            ))}
          </ul>
        )}
      </section>
    </>
  );
}
