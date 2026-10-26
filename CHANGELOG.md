# Changelog

All notable changes to FirebirdREST are listed here. The project follows
[semantic versioning](https://semver.org); until 1.0, minor versions may change the API.

## [Unreleased]

### Added
- OpenTelemetry traces over OTLP/HTTP: `otel-traces-endpoint` (or the standard `OTEL_EXPORTER_OTLP_*`
  variables) exports a span per request with children for the JWT check, the transaction and
  each SQL statement; W3C `traceparent` joins the caller's trace and decides sampling,
  `otel-traces-sample-ratio` samples the rest. No new dependencies: the exporter is written for
  FirebirdREST, like the metrics. Checked against Jaeger
  ([traces](https://mariuz.github.io/firebirdrest/references/observability/#traces)).
- A fuzz test of the URL grammar, run in CI: arbitrary query strings must produce SQL whose
  values are all parameters and whose identifiers all come from the schema, and must be refused
  with a 4xx error rather than a panic or a 500.

### Fixed
- Many-to-many embedding found no relationship when the junction table's foreign keys were not
  linked to a loaded schema; only schemas built in memory (tests, tools) were affected.

## [0.5.0] - 2026-10-26

### Added
- Realtime events: `GET /events/<name>[,<name>...]` streams Firebird `POST_EVENT` notifications
  as Server-Sent Events, for the names listed in `db-events` and the roles in `db-events-roles`.
  One database attachment serves every stream; keepalives every 15 s; metrics for streams and
  notifications ([events](https://mariuz.github.io/firebirdrest/api/events/)). The example
  database posts `order_placed` from `place_order` and has a `notify(name)` procedure.
- Query plans for writes: `Accept: application/vnd.pgrst.plan` on `POST`, `PATCH`, `PUT` and
  `DELETE` runs the request in a transaction that is always rolled back and returns the plans
  of its statements, as PostgREST does.
- `application/vnd.pgrst.plan+json` adds `tree`, the explained plan parsed into
  `{node, children}` nodes, next to the text in `plan`.

### Changed
- Plain JSON reads (no embedded resources) encode each row as it is read instead of keeping
  every row and then the JSON: a 200,000-row, 106 MB response peaks at 216 MB of process memory
  instead of 536 MB and takes 1.7 s instead of 4.5 s.
- `Prefer: params=multiple-objects` on a selectable procedure takes up to 1024 objects (their
  arguments within 48 KB) instead of 200: the calls are driven by a counting CTE in one
  statement, so the procedure is one context however many objects there are.

## [0.4.0] - 2026-10-24

### Changed
- Reads cost 4 round trips to Firebird instead of 7: `GET`/`HEAD` requests reuse a long-lived
  read-only `READ COMMITTED` transaction per connection (same results as a fresh one, see
  [Transactions](https://mariuz.github.io/firebirdrest/references/security/#transactions);
  `db-read-tx-reuse = false` restores the old behaviour), and the request context block no
  longer triggers a row-count request. On the benchmark database: a row by key +30%, a
  function call +60%, a computed field with order +25%, a 25-row embed +30%.
- Multi-row inserts no longer run one statement per row. On Firebird 4+, a plain insert with
  `return=minimal` goes through the wire protocol's batch API (one batch per column set);
  everything else (Firebird 3, upserts, `return=representation`, BLOB values) runs in
  `EXECUTE BLOCK`s of up to 100 rows that return every row's keys. A 100-row insert takes a
  third of the time it took, a 1000-row one less. Rows of a view are still inserted one by
  one. Transactions are now bound to one pooled connection each, which the batch API needs.

### Added
- Relationships across SQL schemas on Firebird 6: a foreign key between tables of two exposed
  schemas (`db-schemas`) is an embeddable relationship in both directions, as in PostgREST,
  also when the request's schema has a table of the same name. Foreign keys into schemas that
  aren't exposed stay out ([schemas](https://mariuz.github.io/firebirdrest/references/schemas/#relationships-across-schemas)).
- Benchmarks against PostgREST on identical data, with the PostgreSQL schema and seed in
  `bench/postgrest/` and the results and their interpretation on the Benchmarks page.
- A [stability promise](https://mariuz.github.io/firebirdrest/references/stability/) saying
  which interfaces are stable, which are experimental, and how deprecations and behaviour
  changes are announced, and an [upgrade guide](https://mariuz.github.io/firebirdrest/upgrading/)
  with the changes to check for each version.
- `firebirdrest -check [config-file]` validates the configuration and prints warnings without
  connecting; the warnings are also logged at startup. Deprecated setting names, when there
  are any, keep working for a minor version with a warning.

## [0.3.0] - 2026-10-23

Production readiness: Prometheus metrics, `Server-Timing`, JWKS keys from identity providers
with a token cache, single values and files in any media type, raw bodies for RPC, and the fixes
of a security review. A JavaScript client with the supabase-js API,
[firebirdrest-js](https://github.com/mariuz/firebirdrest-js), checks compatibility in CI.

Behaviour changes to check before upgrading:

- `server-cors-allowed-origins = "*"` no longer sends `Access-Control-Allow-Credentials`; list
  the origins explicitly if browsers must send credentials.
- `GET /` describes only what the caller's role is granted; set
  `openapi-mode = "ignore-privileges"` for the old document.
- Requests are limited to 60 seconds and 16 MB bodies (`server-request-timeout`,
  `server-max-body-size`), and `and=`/`or=` nesting to 16 levels, `in.()` to 1000 values.
- Permission errors no longer include Firebird's "Effective user is ..." line, and unexpected
  errors return a generic message (the cause is logged).
- Pool and statement cache metrics label roles in upper case.
- Responses carry `X-Content-Type-Options: nosniff` and a sandboxing `Content-Security-Policy`.

### Security

A review of the SQL generation, authentication and HTTP surface found no injection or
authentication bypass, and these weaknesses, all fixed here:

- A deeply nested `and=`/`or=` filter or a long `in.()` list could take the Firebird connection
  down or burn CPU for tens of seconds: nesting is limited to 16 levels and `in.()` to 1000
  values (`400` beyond).
- A `Range` header with a huge end overflowed the limit arithmetic and removed `db-max-rows`;
  such ranges are refused with `416`.
- A single value requested as `text/html` (or SVG, XML, script) was served as a page of the
  API's origin: stored script in a column could run there. Every response now carries
  `X-Content-Type-Options: nosniff` and a sandboxing `Content-Security-Policy`, and those types
  are sent as downloads.
- With the default `server-cors-allowed-origins = "*"`, any origin was reflected together with
  `Access-Control-Allow-Credentials: true`; the wildcard now answers `*` without credentials, and
  only listed origins get credentials (with `Vary: Origin`).
- `GET /` described every table and routine to anonymous callers. The document now follows the
  caller's privileges (`openapi-mode`, default `follow-privileges`).
- Permission errors named the database login ("Effective user is ..."), and unexpected errors
  returned their internal text; both are now generic for the client and logged on the server.
- Each case spelling of a role name (`web_user`, `WEB_USER`...) opened its own connection pool;
  names are folded to upper case, as Firebird does.
- The HTTP server had no request timeout and read request bodies inside the transaction:
  `server-request-timeout` (60 s), `server-max-body-size` (16 MB) and header limits are
  enforced, and bodies are read before the transaction starts.
- An empty boolean in the config file (`admin-pprof =`) counted as true; it now keeps the
  default.
- `admin-server-host` binds the admin server to its own address, e.g. `127.0.0.1`.

### Added
- Metrics in the Prometheus format at `GET /metrics` on the admin port: requests by method and
  status, a request duration histogram, error codes, connection pool and statement cache usage
  per role, schema cache loads, JWT cache and JWKS fetches, and Go runtime figures. See
  [Monitoring](https://mariuz.github.io/firebirdrest/references/observability/).
- `server-timing-enabled` adds a `Server-Timing` header (`jwt`, `parse`, `transaction`,
  `response`), as in PostgREST.
- `jwt-secret` accepts a JWK or a JWKS as JSON, PEM certificates and PKCS#1 RSA keys, several PEM
  keys at once, Ed25519 keys (`EdDSA`) and `@/path/to/file`; `PS256/384/512` tokens are accepted
  with RSA keys. Tokens with a `kid` are checked against the matching key.
- `jwt-secret-is-base64` for base64 encoded HMAC secrets.
- `jwt-jwks-url` fetches the signing keys of an identity provider (Auth0, Keycloak, Okta, Azure
  AD, ...) at startup and every `jwt-jwks-refresh` seconds, and again when a token names an
  unknown `kid`, so rotated keys work right away.
- A JWT validation cache (`jwt-cache-max-entries`, default 1000) skips the signature check for
  tokens already accepted, until they expire or for 5 minutes.

- Single values as is: any concrete `Accept` type that isn't a built-in format (`text/plain`,
  `application/octet-stream`, `image/png`, `application/geo+json`...) returns the one selected
  value of the one matching row as the body, with that `Content-Type`. Binary columns are sent
  as their bytes, so files can be served straight from the database. Works for reads, writes
  with `return=representation` and RPC.
- Raw request bodies for RPC: a `POST /rpc/<name>` whose `Content-Type` isn't JSON or a form puts
  the body into the routine's only parameter, as text or bytes (`FBRST133` otherwise).
- The example database has a `covers` table with a binary BLOB and a `blob_size` function.
- A raw single-value response sends a binary value as the driver returns it, without a base64
  encode/decode round trip, so serving a file holds one copy of it in memory.

### Fixed
- `Accept` entries with a parameter that has no value, such as the
  `application/vnd.pgrst.plan+text; for="application/json"; options=;` that postgrest-js sends
  for `explain()`, were rejected with `406`. The media type is now read even when a parameter
  is malformed.

- Responses are sent only after the transaction commits. A failed `COMMIT` now returns an error
  instead of a success response, and headers set before an error (such as `Content-Range` on a
  `416`) no longer appear on the error response.
- Graceful shutdown waits for requests in flight (up to 10 seconds) before closing the database
  connections; before, requests running at `SIGTERM` could fail with `500`.

## [0.2.0] - 2026-10-20

SQL schemas for Firebird 6, chosen per request with `Accept-Profile` and `Content-Profile`.
Schema support is **experimental**: it is tested against the Firebird 6 snapshot and may change
if Firebird 6 changes before its release. Existing setups keep working as before: Firebird 3-5
are unaffected, and Firebird 6 without `db-schemas` serves `PUBLIC` as it did.

### Added
- SQL schemas (Firebird 6, experimental): `db-schemas` lists the schemas to expose, the first
  being the default (`public` when unset). Requests pick one with `Accept-Profile` (reads) or `Content-Profile`
  (writes), as in PostgREST; responses carry `Content-Profile`, and unexposed schemas get `406`
  (`FBRST106`). Each schema has its own cache and OpenAPI description, statements use
  schema-qualified names, and embedding follows foreign keys within the schema.
  `examples/bookstore-fb6.sql` adds an `inventory` schema. `db-pre-request` accepts `schema.proc`.

### Fixed
- On Firebird 6 the schema cache matches system-table rows by schema as well as by name (domains,
  constraints, indexes, routine parameters), instead of relying on names being unique.

## [0.1.2] - 2026-10-19

Write guards with `max-affected`, query plans, and per-request time zones.

### Added
- `Prefer: handling=strict` refuses unknown or invalid preferences (`FBRST130`); the default
  `lenient` ignores them, as before.
- `Prefer: max-affected=n` (with `handling=strict`, as in PostgREST): a `PATCH` or `DELETE` that
  would change more than `n` rows fails with `FBRST131` and is rolled back.
- Query plans: with `db-plan-enabled = true`, `Accept: application/vnd.pgrst.plan` (`+text` or
  `+json`) returns Firebird's explained plan of every statement a `GET`/`HEAD` request runs,
  including embeds and counts, instead of the data.
- `Prefer: timezone=<IANA name>` (Firebird 4+): `TIME`/`TIMESTAMP WITH TIME ZONE` values are
  returned in that zone, and the Firebird session runs in it (`SET TIME ZONE`, reset before the
  connection is reused). Unknown zones are ignored, or refused with `handling=strict`.
- `examples/bookstore-fb4.sql` adds a `meetings` table with time zone columns for Firebird 4+.
- The release workflow can also publish the Docker image to Docker Hub, a Homebrew cask and a
  Scoop manifest, each once its secret is configured (see `RELEASING.md`).

### Changed
- Release notes are taken from the version's section of `CHANGELOG.md`, also for a release first
  drafted on GitHub. `RELEASING.md` describes the release steps.

## [0.1.1] - 2026-10-19

Ordering by embedded columns, limited `PATCH`/`DELETE`, bulk RPC calls, estimated counts, and
Firebird 6 support (tested against the 6.0 snapshot).

### Added
- Ordering by a column of a to-one embed, as in PostgREST: `order=authors(name).desc`. It works at
  any level (`books.order=authors(name)`), by alias and on spread embeds. To-many embeds are
  refused with `FBRST126`.
- `PATCH` and `DELETE` with `limit` and `offset`, as in PostgREST: `?order=created_at&limit=10`
  changes only the first 10 matching rows. A limit needs an `order` (`FBRST127`), and the primary
  key is added to it as a tie-breaker. `db-safe-updates` accepts a limit instead of a filter.
- `Prefer: params=multiple-objects` on `/rpc`: an array body calls the routine once per object in
  one transaction. Selectable procedures return the combined rows as one result (one `UNION ALL`
  query, so `select`, filters, `order` and `limit` apply to all of them; at most 200 objects),
  executable procedures and functions an array of results.
- `Prefer: params=single-object` on `/rpc`: the raw JSON body is passed to a routine's only text
  parameter. The example database has `log_request` for this.
- `Prefer: count=planned` and `count=estimated`. `planned` estimates a table's rows from the
  statistics of its unique indexes without reading the table; `estimated` counts exactly up to
  `db-max-rows` (or 1000) and uses the estimate above. Where no estimate applies (filters, views,
  procedures, no statistics) the count is exact and `Preference-Applied` says `count=exact`.
  Unknown `count=` values are ignored instead of being treated as `exact`. The example database
  computes its index statistics after loading data.
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

[Unreleased]: https://github.com/mariuz/firebirdrest/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/mariuz/firebirdrest/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/mariuz/firebirdrest/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/mariuz/firebirdrest/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/mariuz/firebirdrest/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/mariuz/firebirdrest/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/mariuz/firebirdrest/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/mariuz/firebirdrest/releases/tag/v0.1.0
