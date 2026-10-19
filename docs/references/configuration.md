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
| `db-anon-role` | `FBREST_DB_ANON_ROLE` | none | role for requests without a JWT; empty means a token is required |
| `db-pool` | `FBREST_DB_POOL` | `10` | connections per role |
| `db-max-rows` | `FBREST_DB_MAX_ROWS` | `0` | maximum rows per read (`0` = no limit) |
| `db-pre-request` | `FBREST_DB_PRE_REQUEST` | none | executable procedure run at the start of each request |
| `db-safe-updates` | `FBREST_DB_SAFE_UPDATES` | `true` | refuse `PATCH`/`DELETE` without filters or `limit` |
| `db-aggregates-enabled` | `FBREST_DB_AGGREGATES_ENABLED` | `true` | allow `count()`, `sum()`, … |
| `db-request-context` | `FBREST_DB_REQUEST_CONTEXT` | `auto` | when to set `request.*` variables: `auto`, `always`, `never` ([details](security.md#when-the-variables-are-set)) |
| `db-prepared-statements` | `FBREST_DB_PREPARED_STATEMENTS` | `256` | prepared statements cached per role (`0` = off) |
| `db-prepared-statements-idle` | `FBREST_DB_PREPARED_STATEMENTS_IDLE` | `30` | seconds after which an unused cached statement is closed ([why](schema-cache.md#ddl-and-cached-statements)) |
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
| `jwt-secret` | `FBREST_JWT_SECRET` | none | HMAC secret, or a PEM public key for RSA/ECDSA tokens |
| `jwt-aud` | `FBREST_JWT_AUD` | none | required `aud` claim |
| `jwt-role-claim-key` | `FBREST_JWT_ROLE_CLAIM_KEY` | `role` | claim holding the role; dots for nested claims (`app_metadata.role`) |

## Server

| Key | Environment | Default | |
|---|---|---|---|
| `server-host` | `FBREST_SERVER_HOST` | `0.0.0.0` | listen address |
| `server-port` | `FBREST_SERVER_PORT` | `3000` | API port |
| `server-cors-allowed-origins` | `FBREST_SERVER_CORS_ALLOWED_ORIGINS` | `*` | comma-separated origins allowed by CORS; empty disables CORS headers |
| `openapi-server-proxy-uri` | `FBREST_OPENAPI_SERVER_PROXY_URI` | request host | public URL shown in the OpenAPI document |
| `admin-server-port` | `FBREST_ADMIN_SERVER_PORT` | `0` (off) | admin server: `GET /live`, `GET /ready`, `POST /reload` |
| `admin-pprof` | `FBREST_ADMIN_PPROF` | `false` | serve Go profiling at `/debug/pprof/` on the admin port |
| `log-level` | `FBREST_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`; `info` logs every request |

The admin server listens on `server-host` too. Don't expose it publicly: anyone who can reach it
can reload the schema cache, and with `admin-pprof` read profiling data.

## Command line

```
firebirdrest [config-file]
firebirdrest -version
```

FirebirdREST retries the database connection with back-off until it succeeds, so it can start
before Firebird. It stops gracefully on `SIGINT`/`SIGTERM` and reloads the schema cache on
`SIGUSR1` (not on Windows).
