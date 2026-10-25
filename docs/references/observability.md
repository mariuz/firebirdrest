# Monitoring

FirebirdREST reports what it's doing in three ways: metrics in the Prometheus format on the
admin port, an optional `Server-Timing` header on each response, and a log line per request.

## Metrics

With `admin-server-port` set, `GET /metrics` on the admin port returns metrics in the
[Prometheus text format](https://prometheus.io/docs/instrumenting/exposition_formats/):

```sh
curl -s localhost:3001/metrics | grep requests_total
# firebirdrest_requests_total{method="GET",status="200"} 1832
# firebirdrest_requests_total{method="POST",status="201"} 41
```

A Prometheus scrape job:

```yaml
scrape_configs:
  - job_name: firebirdrest
    static_configs:
      - targets: ["firebirdrest:3001"]
```

In Kubernetes, scrape the admin port of each pod; don't route it through the public service.

### Requests

| Metric | Type | Labels | |
|---|---|---|---|
| `firebirdrest_requests_total` | counter | `method`, `status` | requests served |
| `firebirdrest_request_duration_seconds` | histogram | `method` | time to serve a request, from 1 ms to 10 s buckets |
| `firebirdrest_errors_total` | counter | `code` | error responses by code: `FBRST...` or the Firebird SQLSTATE (see [errors](../api/formats-errors.md)) |

`method` is one of `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS` or `OTHER`.

### Connection pools and statement caches

Each Firebird role has its own pool and [statement cache](configuration.md#database); these
metrics have a `role` label, with the role name in upper case as Firebird stores it. The pool with an empty `role` is the connection without a role that
reads the schema cache.

| Metric | Type | |
|---|---|---|
| `firebirdrest_db_pool_max_connections` | gauge | `db-pool` |
| `firebirdrest_db_pool_open_connections` | gauge | open connections, in use or idle |
| `firebirdrest_db_pool_in_use_connections` | gauge | connections running a request |
| `firebirdrest_db_pool_idle_connections` | gauge | idle connections |
| `firebirdrest_db_pool_waits_total` | counter | times a request waited for a free connection |
| `firebirdrest_db_pool_wait_seconds_total` | counter | total time spent waiting |
| `firebirdrest_stmt_cache_hits_total` | counter | statements run from the prepared statement cache |
| `firebirdrest_stmt_cache_misses_total` | counter | statements that had to be prepared |
| `firebirdrest_stmt_cache_entries` | gauge | statements in the cache |

A rising `firebirdrest_db_pool_waits_total` means requests queue for connections: raise
`db-pool`, or check for slow queries. The statement cache metrics are absent with
`db-prepared-statements = 0`.

### Schema cache

| Metric | Type | |
|---|---|---|
| `firebirdrest_schema_reloads_total` | counter | schema cache loads, the first one included |
| `firebirdrest_schema_reload_errors_total` | counter | failed loads (the previous cache stays in use) |
| `firebirdrest_schema_load_duration_seconds` | gauge | how long the last successful load took |
| `firebirdrest_schema_load_time_seconds` | gauge | when it happened, in unix seconds |
| `firebirdrest_schema_relations` | gauge | tables and views, by `schema` |
| `firebirdrest_schema_routines` | gauge | procedures and functions, by `schema` |

### Authentication

| Metric | Type | |
|---|---|---|
| `firebirdrest_jwt_cache_hits_total` | counter | tokens found in the [validation cache](security.md#validation-cache) |
| `firebirdrest_jwt_cache_misses_total` | counter | tokens whose signature was checked |
| `firebirdrest_jwks_fetches_total` | counter | fetches of `jwt-jwks-url` (only with it set) |
| `firebirdrest_jwks_fetch_errors_total` | counter | failed fetches |

### Events

Only with `db-events` set ([realtime events](../api/events.md)).

| Metric | Type | |
|---|---|---|
| `firebirdrest_event_streams` | gauge | open `/events` streams |
| `firebirdrest_events_received_total{event}` | counter | notifications received from the database, by event |
| `firebirdrest_events_delivered_total` | counter | notifications written to streams |
| `firebirdrest_event_subscription_errors_total` | counter | times the subscription to the database was lost and renewed |

### Process

| Metric | Type | |
|---|---|---|
| `firebirdrest_build_info` | gauge | always 1; `version` and `firebird` (server version) labels |
| `firebirdrest_start_time_seconds` | gauge | process start, in unix seconds |
| `go_goroutines` | gauge | goroutines |
| `go_memstats_heap_alloc_bytes` | gauge | heap in use |
| `go_memstats_sys_bytes` | gauge | memory obtained from the operating system |
| `go_gc_cycles_total` | counter | garbage collections |

### Example alerts

```yaml
groups:
  - name: firebirdrest
    rules:
      - alert: FirebirdRESTErrors
        expr: sum(rate(firebirdrest_requests_total{status=~"5.."}[5m])) > 0.1
      - alert: FirebirdRESTSlow
        expr: histogram_quantile(0.95, sum by (le) (rate(firebirdrest_request_duration_seconds_bucket[5m]))) > 0.5
      - alert: FirebirdRESTPoolSaturated
        expr: rate(firebirdrest_db_pool_waits_total[5m]) > 1
      - alert: FirebirdRESTSchemaReloadFailed
        expr: increase(firebirdrest_schema_reload_errors_total[15m]) > 0
```

## Server-Timing

With `server-timing-enabled = true`, responses carry a
[`Server-Timing`](https://developer.mozilla.org/docs/Web/HTTP/Headers/Server-Timing) header, as
in PostgREST. Browser developer tools show it in the network panel's Timing tab.

```http
Server-Timing: jwt;dur=0.1, parse;dur=0.2, transaction;dur=3.4, response;dur=0.3
```

| Name | Time spent (milliseconds) |
|---|---|
| `jwt` | verifying the bearer token |
| `parse` | parsing the URL and the body, and building the SQL |
| `transaction` | in the database: from `BEGIN` to `COMMIT`, less `parse` and `response` |
| `response` | rendering the result as JSON, CSV or text |

Error responses have no `Server-Timing` header. The header shows how your API is built, so
leave it off on public APIs unless you want clients to see it.

## Logs

Each request is logged at `info` level with method, path, status and duration, as text on
standard error:

```text
time=2026-10-20T18:12:01.123Z level=INFO msg=request method=GET path=/books status=200 took=1.84ms
```

`log-level = warn` keeps only problems, such as a failed schema reload or JWKS fetch.
