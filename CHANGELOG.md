# Changelog

All notable changes to FirebirdREST are listed here. The project follows
[semantic versioning](https://semver.org); until 1.0, minor versions may change the API.

## [Unreleased]

### Added
- Ordering by a column of a to-one embed, as in PostgREST: `order=authors(name).desc`. It works at
  any level (`books.order=authors(name)`), by alias and on spread embeds. To-many embeds are
  refused with `FBRST126`.
- `PATCH` and `DELETE` with `limit` and `offset`, as in PostgREST: `?order=created_at&limit=10`
  changes only the first 10 matching rows. A limit needs an `order` (`FBRST127`), and the primary
  key is added to it as a tie-breaker. `db-safe-updates` accepts a limit instead of a filter.
- CI runs the integration suite against the Firebird 6 snapshot (`firebirdsql/firebird:6-snapshot`)
  on every push and weekly. The job may fail without failing the build until Firebird 6 is released.

### Fixed
- A `PATCH` with `?limit=` and `return=representation` updated every matching row but returned
  only `limit` of them; the limit now applies to the update itself.
- Schema loading reports errors that happen while reading the system tables, instead of
  silently loading an incomplete schema (on Firebird 6 this hid every foreign key).
- Foreign keys and unique constraints load on Firebird 6.
- On Firebird 6, the `hint` of a PSQL exception names the exception without the `"PUBLIC".`
  schema prefix, as on earlier versions.

## [0.1.0] - 2026-10-18

First release. Supports Firebird 3, 4 and 5.

### Reading
- Tables and views at `/<name>`, with PostgREST's URL grammar: `select` (aliases, casts),
  filters (`eq`, `neq`, `gt`, `gte`, `lt`, `lte`, `like`, `ilike`, `in`, `is`, `isdistinct`, `not.`),
  plus Firebird's `containing`, `starting` and `similar`; nested `or`/`and` trees; `order`;
  `limit`/`offset` and the `Range` header; `Prefer: count=exact`.
- Resource embedding through foreign keys: many-to-one, one-to-many, many-to-many through junction
  tables, nested embeds, `!hint` disambiguation, `!inner`, `embed=is.null`, and per-embed
  filters, order and limits.
- Spread embeds (`...rel(cols)`): to-one values are merged into the parent; to-many columns
  become arrays.
- Aggregates `count()`, `sum()`, `avg()`, `min()`, `max()` with automatic grouping, inside embeds
  too (`authors?select=name,books(count)`).
- Computed fields (PSQL functions) and computed relationships (selectable and executable
  procedures) declared with `TYPE OF COLUMN` parameters.
- JSON, CSV and single-object (`application/vnd.pgrst.object+json`) responses.

### Writing
- `POST` (single or bulk), upserts with `Prefer: resolution=merge-duplicates|ignore-duplicates`
  and `on_conflict`, `PATCH`, `PUT`, `DELETE`, `Prefer: return=representation` with `select`
  and embeds, `?columns=`, `Prefer: missing=default`, `Prefer: tx=rollback`.
- `PATCH`/`DELETE` without filters are refused unless `db-safe-updates = false`.

### Routines
- `/rpc/<name>` for selectable procedures (filterable like tables), executable procedures and
  PSQL functions, plus public routines of packages at `/rpc/<package>.<name>`.
- `GET` requests run in read-only transactions.

### Performance
- Prepared statement cache per role (`db-prepared-statements`, default 256), with idle
  expiry (`db-prepared-statements-idle`, default 30 s) so `DROP` statements are not blocked.
- The `request.*` context variables are only set when the database reads them
  (`db-request-context = auto|always|never`), in a single `EXECUTE BLOCK` together with
  `db-pre-request`.
- Faster JSON encoding of result rows; bulk inserts reuse one prepared statement.
- See `docs/benchmarks.md` for numbers and the load tool.

### Security and operations
- JWT (HS256/384/512 secret, or RS*/ES* public key) mapped to Firebird roles; anonymous role;
  requests are refused if the role isn't granted to the database user.
- JWT claims, role, method, path and selected headers exposed to PSQL through
  `RDB$GET_CONTEXT('USER_TRANSACTION', ...)`; optional `db-pre-request` procedure.
- OpenAPI 3 document at `/`.
- Schema cache reload with `POST_EVENT 'fbrest_reload'`, `SIGUSR1` or `POST /reload` on the
  admin server, which also serves `/live` and `/ready`.
- Config file or `FBREST_*` environment variables; CORS; Docker image and docker-compose example.
- Documentation site at <https://mariuz.github.io/firebirdrest/> (built from `docs/`).
- `bench/` load tool with a data seeder.
- `admin-pprof` serves Go profiling endpoints on the admin port.
- Release workflow can be started from GitHub (Actions, then Release, then Run workflow).

[Unreleased]: https://github.com/mariuz/firebirdrest/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/mariuz/firebirdrest/releases/tag/v0.1.0
