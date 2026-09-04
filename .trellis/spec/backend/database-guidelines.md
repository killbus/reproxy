# Database Guidelines

> Not applicable — reproxy is a stateless HTTP retry reverse proxy with no storage layer.

---

There is no database, no ORM, no migrations. State per request lives in the retry loop (attempt counters, budget clock, captured body in memory) and dies with the request.

If a future feature introduces persistence (e.g. metrics persistence), it must come with:

- an ADR documenting the choice (per ADR-0001's bar for new dependencies),
- a filled-in version of this file following the code-spec structure (signatures, migration policy, error matrix),
- test coverage for the migration path itself, not just the happy path.
