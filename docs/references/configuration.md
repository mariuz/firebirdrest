# Configuration

Settings come from an optional config file passed as the only argument, and from `FBREST_*`
environment variables. Environment variables override the file.

```sh
firebirdrest /etc/firebirdrest.conf
FBREST_DB_POOL=20 firebirdrest /etc/firebirdrest.conf
```

The file holds `key = value` lines; values may be quoted, and `#` starts a comment. Unknown keys
are an error, so typos don't go unnoticed. A complete example is in
[`examples/firebirdrest.conf`](https://github.com/mariuz/firebirdrest/blob/main/examples/firebirdrest.conf).

```ini
db-uri = "authenticator:secret@localhost:3050/var/lib/firebird/data/app.fdb"
db-anon-role = "web_anon"
jwt-secret = "a long random string of at least 32 characters"
```

## Database

| Key | Environment | Default | |
|---|---|---|---|
| `db-uri` | `FBREST_DB_URI` | (required) | `user:password@host[:port]/path/to/db.fdb[?options]` |
| `db-password-file` | `FBREST_DB_PASSWORD_FILE` | none | read the database password from this file (a Docker or Kubernetes secret); `db-uri` then names the user without a password: `authenticator@db:3050/data/app.fdb` |
| `db-anon-role` | `FBREST_DB_ANON_ROLE` | none | role for requests without a JWT; empty means a token is required |
| `db-pool` | `FBREST_DB_POOL` | `10` | connections per role |
| `db-pool-acquisition-timeout` | `FBREST_DB_POOL_ACQUISITION_TIMEOUT` | `10` | seconds a request waits for a free connection before `504` (`FBRST003`); `0` waits up to `server-request-timeout` |
| `db-pool-max-lifetime` | `FBREST_DB_POOL_MAX_LIFETIME` | `1800` | seconds after which a connection is closed and replaced, once it is back in the pool; `0` keeps it |
| `db-pool-max-idletime` | `FBREST_DB_POOL_MAX_IDLETIME` | `300` | seconds an unused connection stays open; `0` keeps it |
| `db-max-rows` | `FBREST_DB_MAX_ROWS` | `0` | maximum rows per read (`0` = no limit); also where `Prefer: count=estimated` switches to the estimate (1000 when `0`) |
| `db-pre-request` | `FBREST_DB_PRE_REQUEST` | none | executable procedure run at the start of each request (`proc` or `schema.proc`) |
| `db-tx-end` | `FBREST_DB_TX_END` | `commit-allow-override` | how transactions end: `commit`, `commit-allow-override`, `rollback` or `rollback-allow-override`; only the `-allow-override` modes honour `Prefer: tx=` ([details](security.md#transactions)) |
| `db-root-spec` | `FBREST_DB_ROOT_SPEC` | none | a function or procedure whose result answers `GET /` instead of the generated OpenAPI document |
| `db-safe-updates` | `FBREST_DB_SAFE_UPDATES` | `true` | refuse `PATCH`/`DELETE` without filters or `limit` |
| `db-aggregates-enabled` | `FBREST_DB_AGGREGATES_ENABLED` | `true` | allow `count()`, `sum()`, … |
| `db-plan-enabled` | `FBREST_DB_PLAN_ENABLED` | `false` | allow `Accept: application/vnd.pgrst.plan` ([query plans](../api/formats-errors.md#query-plans)) |
| `db-schemas` | `FBREST_DB_SCHEMAS` | `public` | Firebird 6+, experimental: comma-separated SQL schemas to expose, the first is the default ([schemas](schemas.md)) |
| `db-request-context` | `FBREST_DB_REQUEST_CONTEXT` | `auto` | when to set `request.*` variables: `auto`, `always`, `never` ([details](security.md#when-the-variables-are-set)) |
| `db-prepared-statements` | `FBREST_DB_PREPARED_STATEMENTS` | `256` | prepared statements cached per role (`0` = off) |
| `db-prepared-statements-idle` | `FBREST_DB_PREPARED_STATEMENTS_IDLE` | `30` | seconds after which an unused cached statement is closed ([why](schema-cache.md#ddl-and-cached-statements)) |
| `db-events` | `FBREST_DB_EVENTS` | none | Firebird events clients may listen for at `GET /events/<name>` ([realtime events](../api/events.md)) |
| `db-events-roles` | `FBREST_DB_EVENTS_ROLES` | none | roles allowed to listen; empty means any authenticated role |
| `server-rate-limit` | `FBREST_SERVER_RATE_LIMIT` | `0` (off) | requests per second per client with a token (its role and `sub`); fractions allowed ([limits](security.md#rate-and-concurrency-limits)) |
| `server-rate-limit-anon` | `FBREST_SERVER_RATE_LIMIT_ANON` | `server-rate-limit` | requests per second per anonymous client IP |
| `server-rate-burst` | `FBREST_SERVER_RATE_BURST` | twice the rate | requests a client may make at once beyond its rate |
| `server-max-concurrent` | `FBREST_SERVER_MAX_CONCURRENT` | `0` (off) | requests a client may have in progress at once; event streams don't count |
| `server-client-ip-header` | `FBREST_SERVER_CLIENT_IP_HEADER` | none | behind a proxy, the header with the client address: `X-Real-IP`, or `X-Forwarded-For` (its last entry) |
| `server-tls-cert` | `FBREST_SERVER_TLS_CERT` | none | PEM certificate (with its chain) to serve HTTPS; reloaded on `SIGHUP` ([TLS](deployment.md#tls)) |
| `server-tls-key` | `FBREST_SERVER_TLS_KEY` | none | PEM private key of `server-tls-cert`; both or neither |
| `otel-traces-endpoint` | `FBREST_OTEL_TRACES_ENDPOINT` | none | OTLP/HTTP URL traces are posted to, e.g. `http://collector:4318/v1/traces`; also read from `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` or `OTEL_EXPORTER_OTLP_ENDPOINT` ([traces](observability.md#traces)) |
| `otel-traces-sample-ratio` | `FBREST_OTEL_TRACES_SAMPLE_RATIO` | `1` | share of requests traced when no `traceparent` header decides |
| `otel-service-name` | `FBREST_OTEL_SERVICE_NAME` | `firebirdrest` | `service.name` of the traces; also read from `OTEL_SERVICE_NAME` |
| `db-read-tx-reuse` | `FBREST_DB_READ_TX_REUSE` | `true` | `GET`/`HEAD` requests share long-lived read-only transactions, one per connection, instead of starting and committing one each ([details](security.md#transactions)) |
| `db-reload-event` | `FBREST_DB_RELOAD_EVENT` | `fbrest_reload` | `POST_EVENT` name that reloads the schema cache; empty disables it |

### `db-uri` options

The URI is passed to the [nakagami/firebirdsql](https://github.com/nakagami/firebirdsql) driver.
Useful options:

| Option | Default | |
|---|---|---|
| `charset` | `UTF8` | connection character set |
| `wire_crypt` | `true` | encrypt the connection (Firebird 3+); `false` for servers with `WireCrypt = Disabled` |
| `auth_plugin_name` | `Srp256` | `Srp`, `Srp256` or `Legacy_Auth` |
| `timezone` | server's | time zone for `TIMESTAMP`/`TIME` without zone |
| `wire_compress` | `false` | compress the wire protocol, useful over slow links |

```ini
db-uri = "authenticator:secret@db.internal:3050/data/app.fdb?wire_crypt=false&charset=UTF8"
```

Don't add `role=` yourself: FirebirdREST sets the role per request.

## Authentication

| Key | Environment | Default | |
|---|---|---|---|
| `jwt-secret` | `FBREST_JWT_SECRET` | none | HMAC secret, PEM public key or certificate, JWK or JWKS; `@/path` reads it from a file ([formats](security.md#json-web-tokens)) |
| `jwt-secret-is-base64` | `FBREST_JWT_SECRET_IS_BASE64` | `false` | the HMAC secret is base64 encoded |
| `jwt-jwks-url` | `FBREST_JWT_JWKS_URL` | none | JWKS of an identity provider, fetched over HTTP(S) and refreshed ([details](security.md#keys-from-an-identity-provider)) |
| `jwt-jwks-refresh` | `FBREST_JWT_JWKS_REFRESH` | `3600` | seconds between fetches of `jwt-jwks-url` (minimum 30) |
| `jwt-cache-max-entries` | `FBREST_JWT_CACHE_MAX_ENTRIES` | `1000` | accepted tokens remembered to skip signature checks; `0` disables the cache |
| `jwt-aud` | `FBREST_JWT_AUD` | none | required `aud` claim |
| `jwt-iss` | `FBREST_JWT_ISS` | none | accepted `iss` claims, comma separated; tokens from any other issuer, or without one, are refused |
| `jwt-leeway` | `FBREST_JWT_LEEWAY` | `0` | seconds of clock difference tolerated on `exp`, `nbf` and `iat` (at most 300) |
| `jwt-role-claim-key` | `FBREST_JWT_ROLE_CLAIM_KEY` | `role` | claim holding the role: `app_metadata.role`, `realm_access.roles[0]`, `."https://example.com/role"` ([details](security.md#json-web-tokens)) |

## Server

| Key | Environment | Default | |
|---|---|---|---|
| `server-host` | `FBREST_SERVER_HOST` | `0.0.0.0` | listen address |
| `server-port` | `FBREST_SERVER_PORT` | `3000` | API port |
| `server-unix-socket` | `FBREST_SERVER_UNIX_SOCKET` | none | serve on this Unix socket instead of `server-host` and `server-port` ([behind a proxy](deployment.md#behind-a-reverse-proxy)) |
| `server-unix-socket-mode` | `FBREST_SERVER_UNIX_SOCKET_MODE` | `660` | permissions of the socket file, in octal |
| `server-trace-header` | `FBREST_SERVER_TRACE_HEADER` | none | a request header such as `X-Request-Id`, copied into the response and the request log, and visible to PSQL as `request.header.<name>` ([logs](observability.md#logs)) |
| `server-request-timeout` | `FBREST_SERVER_REQUEST_TIMEOUT` | `60` | seconds a request may take to arrive and to be handled; `0` disables ([details](security.md#request-limits)) |
| `server-max-body-size` | `FBREST_SERVER_MAX_BODY_SIZE` | `16` | largest request body, in MB (`413` above it) |
| `server-cors-allowed-origins` | `FBREST_SERVER_CORS_ALLOWED_ORIGINS` | `*` | comma-separated origins allowed by CORS; `*` allows any origin without credentials; empty disables CORS headers |
| `openapi-server-proxy-uri` | `FBREST_OPENAPI_SERVER_PROXY_URI` | request host | public URL shown in the OpenAPI document |
| `openapi-mode` | `FBREST_OPENAPI_MODE` | `follow-privileges` | what `GET /` describes: only what the caller's role is granted (`follow-privileges`), everything (`ignore-privileges`), or nothing (`disabled`, `404`) |
| `server-timing-enabled` | `FBREST_SERVER_TIMING_ENABLED` | `false` | add a `Server-Timing` header to responses ([details](observability.md#server-timing)) |
| `admin-server-port` | `FBREST_ADMIN_SERVER_PORT` | `0` (off) | admin server: `GET /live`, `GET /ready`, `POST /reload`, `GET /metrics` |
| `admin-server-host` | `FBREST_ADMIN_SERVER_HOST` | `127.0.0.1` (`0.0.0.0` in the Docker image) | listen address of the admin server |
| `admin-server-token` | `FBREST_ADMIN_SERVER_TOKEN` | none | bearer token required by `/reload`, `/metrics` and `/debug/pprof/`; at least 16 characters, or `@/path/to/file` |
| `admin-pprof` | `FBREST_ADMIN_PPROF` | `false` | serve Go profiling at `/debug/pprof/` on the admin port |
| `log-level` | `FBREST_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`; `info` logs every request |
| `log-query` | `FBREST_LOG_QUERY` | `disabled` | `all` logs the SQL text of every statement, without its parameter values ([logs](observability.md#logs)) |

The admin server listens on `127.0.0.1` by default, so only the same machine reaches it. To reach
it from elsewhere (a Prometheus server, a load balancer's health check, Kubernetes probes), set
`admin-server-host` and an `admin-server-token`: `/reload`, `/metrics` and `/debug/pprof/` then
need `Authorization: Bearer <token>`, while `/live` and `/ready` stay open for probes. Without a
token on a non-local address, anyone who can reach the port can reload the schema cache and read
the metrics; FirebirdREST warns about it at startup and in `-check`.

Boolean settings left empty in the config file (`admin-pprof =`) keep their default.

## Command line

```
firebirdrest [config-file]
firebirdrest -version
firebirdrest -check [config-file]
firebirdrest -dump-config [config-file]
firebirdrest -dump-schema [config-file]
```

`-check` loads the configuration, prints warnings (deprecated settings, a short `jwt-secret`,
the CORS wildcard, no way to authenticate) and exits with status 0, or with status 2 and the
error when a setting is unknown or invalid. Nothing is connected. Use it before a rollout; see
[Upgrading](../upgrading.md). The same warnings are logged at startup.

`-dump-config` prints every setting in use, defaults included, as a config file sorted by key.
The output combines the file and the environment, so it shows what a running server would see.
`jwt-secret`, `admin-server-token` and the password in `db-uri` are shown as `***`.

`-dump-schema` connects, loads the schema cache and prints it as JSON:
- relations with their columns, keys and computed fields
- foreign keys, including those inferred for views
- routines
- which objects may set `response.*`

Use it to see what FirebirdREST found in a database, or to compare two databases.

FirebirdREST retries the database connection with back-off until it succeeds, so it can start
before Firebird. It stops gracefully on `SIGINT`/`SIGTERM` and reloads the schema cache on
`SIGUSR1` (not on Windows).
