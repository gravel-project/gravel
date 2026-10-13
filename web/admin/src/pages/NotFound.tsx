import { Link } from "wouter";

export function NotFound() {
  return (
    <>
      <h1 className="mb-4 text-2xl font-bold">Not found</h1>
      <p>
        There is no such page in the dashboard. <Link href="/">Back to the overview</Link>.
      </p>
    </>
  );
}
