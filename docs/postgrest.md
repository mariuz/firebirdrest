# Differences from PostgREST

FirebirdREST borrows PostgREST's URL grammar, `Prefer` headers, error shape and security model,
so PostgREST client libraries and habits mostly carry over. Firebird is not PostgreSQL, though.

[firebirdrest-js](https://github.com/mariuz/firebirdrest-js) wraps supabase-js's query builder
(`@supabase/postgrest-js`) for FirebirdREST; its test suite is the compatibility check for the
list below.

## Same

- `select` with aliases, casts, embedding, `!hint`, `!inner`, spread (`...`) and aggregates.
- Filters `eq`, `neq`, `gt`, `gte`, `lt`, `lte`, `like`, `ilike`, `in`, `is`, `isdistinct`,
  `not.`, and `or`/`and` trees.
- `order` (including `order=embed(column)` on to-one embeds), `limit`, `offset`, `Range`,
  `Prefer: count=exact`, `count=planned` and `count=estimated`.
- `POST`/`PATCH`/`PUT`/`DELETE` (`PATCH`/`DELETE` with `order` and `limit`), `Prefer: return=…`,
  `resolution=…`, `missing=default`,
  `tx=rollback`, `on_conflict`, `columns`.
- `/rpc/<name>` with `GET` (read-only) and `POST`, `Prefer: params=multiple-objects` and
  `params=single-object`.
- JWT role switching, an anonymous role, a pre-request hook, and claims available to SQL.
- `jwt-secret` as an HMAC secret (also base64 encoded), a PEM key, a JWK or a JWKS, or `@file`;
  a JWT validation cache.
- `server-timing-enabled` and the `Server-Timing` header (`jwt`, `parse`, `transaction`, `response`).
- `application/vnd.pgrst.object+json`, CSV, OpenAPI at `/`, `{code, message, details, hint}` errors.
- `Prefer: handling=strict`, `max-affected` and `timezone`; query plans with `Accept: application/vnd.pgrst.plan`
  (behind `db-plan-enabled`).

## Different

| Topic | PostgREST | FirebirdREST |
|---|---|---|
| Namespaces | exposed schemas, `Accept-Profile` | the same on Firebird 6 (`db-schemas`, `Accept-Profile`, `Content-Profile`); Firebird 3–5 have no schemas: one namespace. Packaged routines are `/rpc/package.name` |
| Relationships across schemas | foreign keys into other exposed schemas | the same on Firebird 6: foreign keys between exposed schemas embed both ways |
| Identifier case | as stored | unquoted (upper case) names shown in lower case |
| Claims in SQL | `current_setting('request.jwt.claims')::json` | `RDB$GET_CONTEXT('USER_TRANSACTION', 'request.jwt.claim.<name>')`, one variable per claim (Firebird has no JSON functions) |
| Computed fields / relationships | functions taking a table row | routines whose parameters are `TYPE OF COLUMN <table>.<col>` |
| To-one computed relationship | function returning one row | executable procedure |
| Extra operators | `fts`, `cs`, `cd`, `ov`, JSON `->`/`->>`, ranges, arrays | Firebird's `containing`, `starting`, `similar`; no JSON, array or range types in Firebird |
| Aggregates | off by default | on by default (`db-aggregates-enabled`) |
| Embedding | one SQL statement with lateral joins | one batched query per embed level |
| Roles | `SET ROLE` per transaction | a connection pool per role (Firebird sets the role at connect time) |
| Metrics | `pgrst_*` metrics on the admin server | `firebirdrest_*` metrics with their own names, at `/metrics` on the admin port ([list](references/observability.md#metrics)) |
| Tracing | OpenTelemetry through a separate setup | `otel-traces-endpoint`: a span per request, transaction and SQL statement, joining the caller's `traceparent` |
| JWT keys from a URL | not built in | `jwt-jwks-url` fetches a provider's JWKS and follows key rotation |
| JWT cache size | `jwt-cache-max-entries` | the same; entries also expire after 5 minutes and when the JWKS changes |
| Schema reload | `NOTIFY pgrst` | `POST_EVENT 'fbrest_reload'` |
| Realtime | not built in (Supabase Realtime is a separate service) | `GET /events/<name>` streams `POST_EVENT`s as Server-Sent Events ([events](api/events.md)) |
| Unfiltered `PATCH`/`DELETE` | allowed (unless pg-safeupdate) | refused unless `db-safe-updates = false` |
| Request context cost | always set | only when the database reads it (`db-request-context`) |
| `params=single-object` | the body goes to a single `json` parameter | the body goes to a single text parameter, as JSON text |
| `params=multiple-objects` on set-returning routines | no limit | at most 1024 objects per request, their arguments within 48 KB |
| `count=planned` | the planner's estimate of the query | the table's row count from index statistics; exact for filtered requests, views and procedures |
| Query plans | `EXPLAIN` output (text or JSON), also for writes | Firebird's explained plan per statement, as text or as a JSON tree, also for writes (rolled back) |
| `Prefer: timezone` | changes how `timestamptz` is displayed | `WITH TIME ZONE` values carry a zone in Firebird, so they are converted to the requested zone; ignored on Firebird 3 |

| Custom media types | handler functions per media type | any concrete `Accept` type returns a single value as is ([details](api/formats-errors.md#single-values-and-files)); a raw request body goes to a routine's only parameter |
| `application/geo+json` | built in | request it from a function that returns GeoJSON text |

Open an issue if you miss something.
