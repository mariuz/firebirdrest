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

## Debian, Ubuntu, RHEL, Fedora: packages

Every release has `.deb` and `.rpm` packages for amd64 and arm64. They install the binary, a
hardened systemd service that runs as a `firebirdrest` system user, and a configuration in
`/etc/firebirdrest/` that upgrades leave alone:

```sh
# Debian, Ubuntu
curl -LO https://github.com/mariuz/firebirdrest/releases/download/v0.7.0/firebirdrest_0.7.0_amd64.deb
sudo apt install ./firebirdrest_0.7.0_amd64.deb
# RHEL, Rocky, Alma, Fedora, openSUSE
sudo dnf install https://github.com/mariuz/firebirdrest/releases/download/v0.7.0/firebirdrest-0.7.0.x86_64.rpm

sudoedit /etc/firebirdrest/firebirdrest.conf        # db-uri, jwt-secret, ...
sudo systemctl enable --now firebirdrest
```

| Path | |
|---|---|
| `/usr/bin/firebirdrest` | the binary |
| `/etc/firebirdrest/firebirdrest.conf` | the configuration, `root:firebirdrest`, mode `0640` (it holds the database password); kept on upgrade and on removal |
| `/usr/lib/systemd/system/firebirdrest.service` | the service; override settings with `systemctl edit firebirdrest` |
| `/usr/share/doc/firebirdrest/` | README, changelog, license and the example database scripts |

The service checks the configuration (`firebirdrest -check`) before starting, so a typo shows up
in `systemctl status firebirdrest` instead of a crash loop. `systemctl reload firebirdrest`
reloads the TLS certificate and the schema cache. The first install doesn't start the service;
an upgrade restarts it if it was running. Logs go to the journal:
`journalctl -u firebirdrest -f`.

## systemd

Without the packages, the same unit works for a binary in `/usr/local/bin`:

```ini
# /etc/systemd/system/firebirdrest.service
[Unit]
Description=FirebirdREST
After=network-online.target firebird.service
Wants=network-online.target

[Service]
ExecStartPre=/usr/local/bin/firebirdrest -check /etc/firebirdrest/firebirdrest.conf
ExecStart=/usr/local/bin/firebirdrest /etc/firebirdrest/firebirdrest.conf
# SIGHUP reloads the TLS certificate and the schema cache; SIGUSR1 only the schema cache
ExecReload=/bin/sh -c 'kill -HUP $MAINPID'
Restart=on-failure
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict

[Install]
WantedBy=multi-user.target
```

The packaged unit adds further hardening; see
[`packaging/firebirdrest.service`](https://github.com/mariuz/firebirdrest/blob/main/packaging/firebirdrest.service).

## TLS

FirebirdREST can serve HTTPS itself, without a reverse proxy:

```ini
server-tls-cert = "/etc/firebirdrest/tls/fullchain.pem"   # certificate and chain, PEM
server-tls-key = "/etc/firebirdrest/tls/privkey.pem"
```

It speaks TLS 1.2 and 1.3, and HTTP/2 when the client offers it (event streams included). The
certificate is read at startup, checked by `firebirdrest -check` (which prints its expiry), and
read again on `SIGHUP` (`systemctl reload firebirdrest`): new connections get the renewed
certificate, open ones keep theirs, and a renewal that fails to load keeps the current
certificate and logs an error. A certificate that expires within 14 days is logged as a warning
at startup and on reload. With Let's Encrypt and certbot, a deploy hook copies the files where the
service user can read them and reloads:

```sh
# /etc/letsencrypt/renewal-hooks/deploy/firebirdrest
#!/bin/sh
install -m 0640 -g firebirdrest "$RENEWED_LINEAGE/fullchain.pem" "$RENEWED_LINEAGE/privkey.pem" /etc/firebirdrest/tls/
systemctl reload firebirdrest
```

The admin server (`admin-server-port`: health checks, metrics) stays plain HTTP; keep it on a
local or private address. On Windows a renewed certificate needs a restart.

## Behind a reverse proxy

A reverse proxy is the place for rate limiting, and for TLS when it terminates several services;
for example with nginx:

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
| `GET /metrics` | metrics in the Prometheus format ([list](observability.md#metrics)) |

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
before the release. Firebird 6 adds SQL schemas, which FirebirdREST can expose with `db-schemas`
(see [Schemas](schemas.md)); by default it serves `PUBLIC`.

Known issue with the current snapshot: text columns declared `CHARACTER SET NONE` (the default
in a database created without `DEFAULT CHARACTER SET`) fail with *string right truncation*
once a value is longer than a quarter of the column's size, when the client connects as UTF8
(FirebirdREST's default). Firebird 6 reads the row buffer the driver asks for as UTF8 characters
(4 bytes each) instead of bytes, as Firebird 5 does. A fix in the Go driver FirebirdREST uses,
which sizes that buffer for the connection character set, is proposed in
[nakagami/firebirdsql#317](https://github.com/nakagami/firebirdsql/pull/317). The Python driver
`firebirdsql` has the same problem and the same fix, proposed in
[nakagami/pyfirebirdsql#142](https://github.com/nakagami/pyfirebirdsql/pull/142). Until a
driver release includes it, either create the database with a character set:

```sql
CREATE DATABASE 'app.fdb' DEFAULT CHARACTER SET UTF8;
```

or connect with the database's own character set, for example
`db-uri = "user:pass@host/app.fdb?charset=NONE"`.

A second issue of the same kind affects binary data: with the current driver, Firebird 6 refuses
to read `CHARACTER SET OCTETS` columns and to write binary values under 32 KB (into `BLOB
SUB_TYPE BINARY` or `OCTETS` columns) with *Malformed string*, because it validates them as
UTF8 text. `BLOB SUB_TYPE BINARY` reads, and writes of 32 KB and more, work. The driver fix is
on the branch [`fix-fb6-octets`](https://github.com/mariuz/firebirdsql/tree/fix-fb6-octets), being sent upstream.
