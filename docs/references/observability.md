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
| `firebirdrest_event_stream_resets_total` | counter | reset messages sent: streams that may have missed notifications |
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

## Traces

With an OTLP endpoint configured, every request becomes an
[OpenTelemetry](https://opentelemetry.io) trace, exported over OTLP/HTTP (JSON) to Jaeger, Grafana
Tempo, Honeycomb, an OpenTelemetry Collector or anything else that accepts OTLP:

```ini
otel-traces-endpoint = "http://otel-collector:4318/v1/traces"
otel-service-name = "bookstore-api"       # default: OTEL_SERVICE_NAME, else firebirdrest
otel-traces-sample-ratio = 0.1            # share of requests traced when the caller doesn't decide
```

The standard variables work too: `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, or
`OTEL_EXPORTER_OTLP_ENDPOINT` with `/v1/traces` appended, and `OTEL_EXPORTER_OTLP_HEADERS` (or
`OTEL_EXPORTER_OTLP_TRACES_HEADERS`) for an API key: `x-honeycomb-team=...`.

A request that carries a W3C `traceparent` header joins the caller's trace, and the caller's
sampled flag decides whether it is recorded; other requests are sampled by
`otel-traces-sample-ratio` (default 1, every request). A traced request looks like this in Jaeger:

```text
GET /books                 25.6 ms  server   (child of the caller's span)
├─ jwt                      0.1 ms
└─ transaction             12.6 ms
   ├─ EXECUTE BLOCK         1.6 ms  client   the request context variables
   ├─ SELECT BOOKS          2.6 ms  client
   ├─ SELECT AUTHORS        4.7 ms  client   embedded authors(name)
   └─ SELECT TAGS           3.3 ms  client   embedded tags(name), through the junction
```

| Span | Attributes |
|---|---|
| request (`GET /books`, `POST /rpc/place_order`...) | `http.request.method`, `http.route`, `url.path`, `http.response.status_code`, `firebirdrest.role`, `firebirdrest.schema` (several schemas), `error.type` (the `FBRST` code or SQLSTATE); status error for 5xx |
| `jwt` | only when a token is sent |
| `transaction` | `firebirdrest.read_only`, `firebirdrest.tx_reused`, `firebirdrest.tx_outcome` (`commit`, `rollback`, `reuse`) |
| `SELECT BOOKS`, `INSERT TAGS`, `EXECUTE PROCEDURE`... | `db.system.name` = `firebirdsql`, `db.operation.name`, `db.collection.name`, `db.query.text` |

A statement's span lasts from its execution until the next statement starts or the transaction
ends, so it includes fetching its rows. Spans never carry values: `db.query.text` has
placeholders, and the query string and bodies are not recorded. The request log line gains a
`trace_id` field for traced requests.

Spans are exported in batches every 5 seconds; up to 4096 wait in memory, and more are dropped
(counted below) rather than slowing requests. On shutdown the queue is flushed.

| Metric | Type | |
|---|---|---|
| `firebirdrest_trace_spans_exported_total` | counter | spans accepted by the endpoint |
| `firebirdrest_trace_spans_dropped_total` | counter | spans dropped because the queue was full |
| `firebirdrest_trace_export_errors_total` | counter | failed exports (also logged) |

## Logs

Each request is logged at `info` level with method, path, status and duration, as text on
standard error:

```text
time=2026-10-20T18:12:01.123Z level=INFO msg=request method=GET path=/books status=200 took=1.84ms
```

`log-level = warn` keeps only problems, such as a failed schema reload or JWKS fetch.
