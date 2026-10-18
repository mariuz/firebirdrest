# Deployment

FirebirdREST is one static binary with no dependencies. Run it close to the database: every
request makes a few round trips, so latency between the two adds up.

## Docker

```sh
docker run -d --name firebirdrest -p 3000:3000 -p 3001:3001 \
  -e FBREST_DB_URI='authenticator:secret@db.internal:3050/data/app.fdb' \
  -e FBREST_DB_ANON_ROLE=web_anon \
  -e FBREST_JWT_SECRET="$(cat /run/secrets/jwt_secret)" \
  -e FBREST_ADMIN_SERVER_PORT=3001 \
  ghcr.io/mariuz/firebirdrest:latest
```

Images are published for `linux/amd64` and `linux/arm64` with tags `latest`, `X.Y` and `X.Y.Z`.
They are based on distroless and run as a non-root user.

[`docker-compose.yml`](https://github.com/mariuz/firebirdrest/blob/main/docker-compose.yml) in the
repository runs Firebird, FirebirdREST and Swagger UI together.

## systemd

```ini
# /etc/systemd/system/firebirdrest.service
[Unit]
Description=FirebirdREST
After=network-online.target firebird.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/firebirdrest /etc/firebirdrest.conf
ExecReload=/bin/kill -USR1 $MAINPID
Restart=on-failure
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict

[Install]
WantedBy=multi-user.target
```

`systemctl reload firebirdrest` then reloads the schema cache.

## Behind a reverse proxy

Put TLS and rate limiting in front of FirebirdREST, for example with nginx:

```nginx
location /api/ {
    proxy_pass http://127.0.0.1:3000/;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
}
```

Set `openapi-server-proxy-uri = "https://example.com/api"` so the OpenAPI document shows the public
URL. `X-Forwarded-For` is available to PSQL as `request.header.x-forwarded-for`.

## Health checks

With `admin-server-port` set:

| Endpoint | |
|---|---|
| `GET /live` | `200` while the process runs |
| `GET /ready` | `200` when the database answers, `503` otherwise |
| `POST /reload` | reload the schema cache (`204`) |

```yaml
# Kubernetes
livenessProbe:  { httpGet: { path: /live,  port: 3001 } }
readinessProbe: { httpGet: { path: /ready, port: 3001 } }
```

## Sizing

- `db-pool` is per role. With several roles in use, the database sees up to
  roles × `db-pool` connections from each FirebirdREST instance.
- FirebirdREST keeps no state besides its caches, so you can run several instances behind a load
  balancer. Reload each one after DDL; the `POST_EVENT` route reaches all of them at once.
- See [Benchmarks](../benchmarks.md) for throughput figures and the load tool.

## Upgrading

Download the new binary or pull the new image and restart. Settings and behaviour changes are
listed in the [changelog](../changelog.md).

## Firebird 6

Firebird 6 is not released yet. CI runs the full integration suite against the nightly
`firebirdsql/firebird:6-snapshot` image on every push and once a week, so problems show up
before the release. Firebird 6 adds SQL schemas; FirebirdREST serves the `PUBLIC` schema.

Known issue with the current snapshot: text columns declared `CHARACTER SET NONE` (the default
in a database created without `DEFAULT CHARACTER SET`) fail with *string right truncation*
once a value is longer than a quarter of the column's size, when the client connects as UTF8
(FirebirdREST's default). Firebird 6 reads the row buffer the driver asks for as UTF8 characters
(4 bytes each) instead of bytes, as Firebird 5 does. Until the snapshot is
fixed, either create the database with a character set:

```sql
CREATE DATABASE 'app.fdb' DEFAULT CHARACTER SET UTF8;
```

or connect with the database's own character set, for example
`db-uri = "user:pass@host/app.fdb?charset=NONE"`.
