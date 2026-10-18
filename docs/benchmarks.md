# Benchmarks

FirebirdREST ships a load tool in [`bench/`](https://github.com/mariuz/firebirdrest/tree/main/bench)
so you can measure it against your own hardware and data.

## Running it

```sh
# 1. a database with the example schema
isql -user SYSDBA -password masterkey -i examples/bookstore.sql localhost:/data/bench.fdb

# 2. ~400k rows: 2,000 authors, 100,000 books, 200,000 book/tag links,
#    5,000 customers, 50,000 orders with 100,000 items (takes a few seconds)
go run ./bench seed -dsn SYSDBA:masterkey@localhost:3050/data/bench.fdb

# 3. start FirebirdREST against it, then
go run ./bench run -url http://localhost:3000 -c 16 -d 10s -token "$JWT_FOR_WEB_USER"
```

`run` prints a Markdown table with requests per second and p50/p95/p99 latency for each
scenario. `-only <text>` runs the matching scenarios only. Scenarios marked "rollback" send
`Prefer: tx=rollback`, so the database is unchanged afterwards.

!!! tip "Measure on a quiet machine"
    The client, FirebirdREST and Firebird all compete for CPU. With Docker, connect to the
    container's IP rather than a published port: Docker's userland port proxy cost about 30% of
    throughput in our runs.

## Results

The current code compared with the code before the performance work (commit `712a7fb`), on a
4-vCPU Intel Xeon 2.1 GHz VM with 16 GB RAM, Firebird 5.0.4 in Docker on the same machine,
16 concurrent clients, pool of 32 connections. The two versions ran side by side and each
scenario alternated between them (median of two 4-second runs) so that drift in the shared VM
affects both equally. Absolute numbers depend heavily on hardware; compare the change column.

### Database reads the request context

The example database has a function that reads `request.jwt.claim.sub`, so the request context
is set on every request.

| Scenario | before req/s | after req/s | change | after p95 |
|---|---:|---:|---:|---:|
| row by primary key | 2118 | 3444 | +63% | 8.11 ms |
| page of 100 rows | 1244 | 1731 | +39% | 14.42 ms |
| filter + order + limit 25 | 91 | 102 | +12% | 200.71 ms |
| ilike search, limit 25 | 1070 | 1468 | +37% | 17.82 ms |
| embed many-to-one (25) | 1296 | 1896 | +46% | 14.88 ms |
| embed one-to-many (10) | 413 | 540 | +31% | 43.98 ms |
| embed many-to-many (25) | 566 | 666 | +18% | 39.26 ms |
| nested 3 levels (5) | 431 | 685 | +59% | 31.50 ms |
| aggregate group by | 108 | 116 | +8% | 200.73 ms |
| count=exact, 25 rows | 157 | 148 | -5% | 153.16 ms |
| computed field + order | 1592 | 2252 | +41% | 12.68 ms |
| rpc function | 2465 | 3942 | +60% | 7.03 ms |
| insert 1 row (rollback) | 1139 | 1334 | +17% | 17.54 ms |
| insert 100 rows (rollback) | 62 | 98 | +57% | 208.06 ms |

### Nothing reads the request context

This is what `db-request-context = auto` does when no view, trigger or routine mentions
`request.` (measured with `never`, which skips it unconditionally).

| Scenario | before req/s | after req/s | change | after p95 |
|---|---:|---:|---:|---:|
| row by primary key | 2036 | 4377 | +115% | 6.45 ms |
| page of 100 rows | 1214 | 1995 | +64% | 12.64 ms |
| filter + order + limit 25 | 94 | 97 | +4% | 228.31 ms |
| ilike search, limit 25 | 1072 | 1626 | +52% | 19.67 ms |
| embed many-to-one (25) | 1234 | 2253 | +83% | 12.38 ms |
| embed one-to-many (10) | 410 | 569 | +39% | 40.95 ms |
| embed many-to-many (25) | 569 | 718 | +26% | 36.09 ms |
| nested 3 levels (5) | 419 | 734 | +75% | 32.51 ms |
| aggregate group by | 104 | 113 | +9% | 196.07 ms |
| count=exact, 25 rows | 144 | 156 | +8% | 169.78 ms |
| computed field + order | 1592 | 2557 | +61% | 11.88 ms |
| rpc function | 2266 | 5108 | +125% | 5.58 ms |
| insert 1 row (rollback) | 1146 | 1494 | +30% | 14.72 ms |
| insert 100 rows (rollback) | 68 | 101 | +50% | 190.25 ms |

The scenarios that barely changed are limited by Firebird itself: sorting 100,000 rows on an
unindexed column (`filter + order`), grouping the whole table (`aggregate`) and counting it
(`count=exact`). An index on `books(price)` or on the filtered columns helps them far more than
anything in the API layer.

## What was optimized

Profiling (`admin-pprof = true`) showed that time went into database round trips and Firebird
compiling statements, not into Go code:

1. **Prepared statement cache.** Firebird compiles every statement on prepare, which costs about
   as much as running a small query. FirebirdREST now keeps up to `db-prepared-statements`
   (default 256) compiled statements per role and reuses them across requests. A cache miss runs
   the query directly and prepares it in the background, so a busy pool can't deadlock.
2. **Request context only when used.** Setting the `request.*` variables was a separate round
   trip on every request, taking 27% of CPU in the profile. At schema load FirebirdREST checks
   whether any view, trigger, procedure, function, package or default expression mentions
   `request.`. If none does, the step is skipped. When it is needed, all variables (and the
   `db-pre-request` procedure) go in one `EXECUTE BLOCK`.
3. **Faster JSON encoding.** Responses are written by an encoder specialised for result rows
   (no reflection, 2 allocations for a 100-row page) instead of `encoding/json`, which was 25% of
   CPU on embed-heavy requests.
4. **Bulk inserts reuse one prepared `INSERT`** for all rows with the same columns.

### Prepared statements and DDL

Firebird keeps a lock on every table a prepared statement uses. `ALTER TABLE` and
`CREATE OR ALTER VIEW` work while statements are cached, but `DROP TABLE` (or dropping a view,
procedure, ...) **waits** until the statements using it are closed. Cached statements unused for
`db-prepared-statements-idle` seconds (default 30) are closed automatically, and a schema reload
closes all of them. Before dropping an object that is in active use, reload first:

```sh
curl -X POST localhost:3001/reload     # admin server, or: kill -USR1 <pid>
```

or turn the cache off with `db-prepared-statements = 0`.
