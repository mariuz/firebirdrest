# Differences from PostgREST

FirebirdREST borrows PostgREST's URL grammar, `Prefer` headers, error shape and security model,
so PostgREST client libraries and habits mostly carry over. Firebird is not PostgreSQL, though.

## Same

- `select` with aliases, casts, embedding, `!hint`, `!inner`, spread (`...`) and aggregates.
- Filters `eq`, `neq`, `gt`, `gte`, `lt`, `lte`, `like`, `ilike`, `in`, `is`, `isdistinct`,
  `not.`, and `or`/`and` trees.
- `order` (including `order=embed(column)` on to-one embeds), `limit`, `offset`, `Range`,
  `Prefer: count=exact`.
- `POST`/`PATCH`/`PUT`/`DELETE`, `Prefer: return=…`, `resolution=…`, `missing=default`,
  `tx=rollback`, `on_conflict`, `columns`.
- `/rpc/<name>` with `GET` (read-only) and `POST`.
- JWT role switching, an anonymous role, a pre-request hook, and claims available to SQL.
- `application/vnd.pgrst.object+json`, CSV, OpenAPI at `/`, `{code, message, details, hint}` errors.

## Different

| Topic | PostgREST | FirebirdREST |
|---|---|---|
| Namespaces | exposed schemas, `Accept-Profile` | Firebird has no schemas: one namespace. Packaged routines are `/rpc/package.name` |
| Identifier case | as stored | unquoted (upper case) names shown in lower case |
| Claims in SQL | `current_setting('request.jwt.claims')::json` | `RDB$GET_CONTEXT('USER_TRANSACTION', 'request.jwt.claim.<name>')`, one variable per claim (Firebird has no JSON functions) |
| Computed fields / relationships | functions taking a table row | routines whose parameters are `TYPE OF COLUMN <table>.<col>` |
| To-one computed relationship | function returning one row | executable procedure |
| Extra operators | `fts`, `cs`, `cd`, `ov`, JSON `->`/`->>`, ranges, arrays | Firebird's `containing`, `starting`, `similar`; no JSON, array or range types in Firebird |
| Aggregates | off by default | on by default (`db-aggregates-enabled`) |
| Embedding | one SQL statement with lateral joins | one batched query per embed level |
| Roles | `SET ROLE` per transaction | a connection pool per role (Firebird sets the role at connect time) |
| Schema reload | `NOTIFY pgrst` | `POST_EVENT 'fbrest_reload'` |
| Unfiltered `PATCH`/`DELETE` | allowed (unless pg-safeupdate) | refused unless `db-safe-updates = false` |
| Request context cost | always set | only when the database reads it (`db-request-context`) |

## Not implemented yet

- `Prefer: count=planned/estimated` (treated as `exact`).
- `Accept: application/geo+json`, custom media type handlers.
- `PATCH`/`DELETE` with `limit`.
- RPC calls with an array body (`Prefer: params=multiple-objects`) or a single unnamed argument
  (`params=single-object`).

Open an issue if you need one of these.
