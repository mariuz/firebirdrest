# Upgrading

FirebirdREST keeps no state outside its configuration and the database, so an upgrade is a new
binary or image and a restart. What needs attention is listed per version below; the
[stability promise](references/stability.md) says what never changes without notice.

## Procedure

1. Read the changelog entries between your version and the new one, starting with each
   version's *behaviour changes to check before upgrading*.
2. Check the configuration with the new binary before rolling it out:
   ```sh
   firebirdrest -check /etc/firebirdrest.conf
   ```
   It loads the configuration (file and `FBREST_*` variables), reports unknown or invalid
   settings and deprecation warnings, and exits with status 0 when all is well. Nothing is
   connected or served.
3. Deploy. With several instances behind a load balancer, roll them one at a time; the admin
   endpoint `GET /ready` returns `200` once an instance has loaded its schema cache.
4. If something is wrong, roll back to the previous binary or image tag: nothing in the
   database or the configuration format changes between versions.

Client code written against the documented API keeps working. If you use
[firebirdrest-js](https://github.com/mariuz/firebirdrest-js), its suite skips features newer
than the server, so a newer client works with an older server.

## 0.2.x → 0.3.0

Six behaviour changes, all from the security review and the monitoring work:

| Change | Who is affected | What to do |
|---|---|---|
| The CORS wildcard (`server-cors-allowed-origins = "*"`, the default) answers `Access-Control-Allow-Origin: *` and no longer sends `Access-Control-Allow-Credentials: true`. | Browser clients that send cookies or HTTP auth from a proxy in front of FirebirdREST. Bearer tokens set by JavaScript are not affected. | List the origins explicitly: `server-cors-allowed-origins = "https://app.example"`. |
| `GET /` describes only what the caller's role is granted (`openapi-mode = "follow-privileges"`). | Tools that fetched the whole schema anonymously, e.g. a code generator run without a token. | Send a token with the role whose view you want, or set `openapi-mode = "ignore-privileges"`. |
| Requests are limited: 60 s (`server-request-timeout`), 16 MB bodies (`server-max-body-size`), `and=`/`or=` nested at most 16 levels, at most 1000 values in `in.()`. | Bulk inserts over 16 MB, uploads, long-running procedures, generated filters with huge `in` lists. | Raise `server-max-body-size` or `server-request-timeout` (`0` disables the timeout); split long `in` lists over requests or use a selectable procedure. |
| Permission errors no longer include Firebird's "Effective user is ..." line; unexpected errors return `internal server error` and the cause goes to the log. | Anything matching on those strings. | Match on `code` (SQLSTATE `28000`, `FBRST000`) instead. |
| Pool and statement cache metrics label roles in upper case (`role="WEB_USER"`). | Prometheus queries and dashboards. | Update the label values. |
| Every response carries `X-Content-Type-Options: nosniff` and `Content-Security-Policy: default-src 'none'; sandbox`; single values requested as HTML, SVG, XML, script or PDF are sent as downloads. | Pages that embedded API URLs directly as documents. | Serve such content through your application, or request it with `fetch` and render it there. |

New settings have defaults that keep the old behaviour otherwise: `jwt-secret-is-base64`,
`jwt-jwks-url`, `jwt-jwks-refresh`, `jwt-cache-max-entries`, `server-timing-enabled`,
`admin-server-host`.

## 0.1.x → 0.2.0

No behaviour changes. `db-schemas` is new and off by default; on Firebird 6 the `PUBLIC`
schema is served as before.

## 0.1.0 → 0.1.1 and 0.1.2

No behaviour changes. New preferences (`count=planned`, `count=estimated`, `handling=strict`,
`max-affected`, `timezone`, `params=...`) are off unless sent; query plans need
`db-plan-enabled`.
