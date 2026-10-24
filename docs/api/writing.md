# Writing

Send JSON bodies with `Content-Type: application/json`. A form-urlencoded body is also accepted
for a single row.

## Insert

`POST` takes one object or an array of objects:

```sh
curl -X POST localhost:3000/tags -H 'Content-Type: application/json' -d '{"name":"classic"}'
curl -X POST localhost:3000/tags -H 'Content-Type: application/json' -d '[{"name":"noir"},{"name":"poetry"}]'
```

The response is `201 Created`. For a single row with a primary key, a `Location` header points
to it (`/tags?id=eq.7`). Columns missing from an object get their default (or the identity
value), and unknown keys are rejected with `400`.

An array is inserted in blocks: the rows' statements go to Firebird in `EXECUTE BLOCK`s of up
to 100 rows (fewer for wide rows), each returning the keys of its rows, so a 100-row body costs
a couple of round trips instead of 100. Objects in one array may have different columns. Rows
of a view are inserted one by one. If any row fails, the whole request fails and nothing is
kept.

### Getting the rows back

`Prefer: return=representation` returns the inserted rows, shaped by `select`, with embeds:

```sh
curl -X POST 'localhost:3000/books?select=id,title,authors(name)' \
     -H 'Prefer: return=representation' -H 'Content-Type: application/json' \
     -d '[{"title":"Mort","author_id":1}]'
```

```json
[{"id":6,"title":"Mort","authors":{"name":"Terry Pratchett"}}]
```

### Choosing columns

`?columns=` lists the keys to insert and ignores the rest. Keys missing from a row become `NULL`.
Add `Prefer: missing=default` to use the column default instead:

```
POST /books?columns=title,author_id
Prefer: missing=default
```

## Upsert

| `Prefer` | Firebird statement | On conflict |
|---|---|---|
| `resolution=merge-duplicates` | `UPDATE OR INSERT ... MATCHING (...)` | update the existing row |
| `resolution=ignore-duplicates` | `MERGE ... WHEN NOT MATCHED THEN INSERT` | keep the existing row |

Conflicts are detected on the primary key, or on the columns listed in `on_conflict`:

```sh
curl -X POST 'localhost:3000/tags?on_conflict=name' \
     -H 'Prefer: resolution=ignore-duplicates,return=representation' \
     -H 'Content-Type: application/json' -d '[{"name":"fantasy"},{"name":"new-tag"}]'
```

```json
[{"id":5,"name":"new-tag"}]
```

With `ignore-duplicates`, only newly inserted rows are returned.

## Update

`PATCH` updates every row matching the filters:

```sh
curl -X PATCH 'localhost:3000/books?author_id=eq.1&select=id,price' \
     -H 'Prefer: return=representation' -H 'Content-Type: application/json' -d '{"price":11.50}'
```

## Replace one row

`PUT` with `eq` filters on every primary key column inserts or replaces that single row. The body
must contain the same key values:

```sh
curl -X PUT 'localhost:3000/tags?id=eq.10' -H 'Content-Type: application/json' -d '{"id":10,"name":"noir"}'
```

## Delete

```sh
curl -X DELETE 'localhost:3000/tags?id=eq.10' -H 'Prefer: return=representation'
```

## Limiting updates and deletes

`limit` (and `offset`) on `PATCH` or `DELETE` change only that many of the matching rows. A
limit needs an `order`, so it is clear which rows are picked:

```sh
# mark the 10 oldest pending orders as shipped
curl -X PATCH 'localhost:3000/orders?status=eq.pending&order=created_at&limit=10' \
     -H 'Content-Type: application/json' -d '{"status":"shipped"}'

# delete the 100 oldest log rows, skipping the first 1000
curl -X DELETE 'localhost:3000/audit_log?order=logged_at&offset=1000&limit=100'
```

- The order may only use the table's own columns, not an embed's.
- When the table has a primary key, it is added to the end of the order as a tie-breaker, so
  rows with equal order values are still picked the same way every time.
- `Prefer: return=representation` returns exactly the changed rows, in that order.
- A limit without an order, or an offset without a limit, is refused with `FBRST127`.

Firebird runs these as `UPDATE … ORDER BY … ROWS m TO n` and `DELETE … ORDER BY … ROWS m TO n`.

## Capping how many rows a write changes

`Prefer: max-affected=n` makes a `PATCH` or `DELETE` fail, changing nothing, when it would affect
more than `n` rows. As in PostgREST it only applies together with `handling=strict`:

```sh
curl -X DELETE 'localhost:3000/orders?status=eq.cancelled' \
     -H 'Prefer: handling=strict, max-affected=100'
```

Over the limit the response is `400` with `FBRST131` and the number of rows that would have
changed; the transaction is rolled back. Use it to guard writes whose filters come from user input.

## Safety

`PATCH` and `DELETE` without any filter are refused with `400` so a missing parameter can't
change a whole table. A `limit` counts as a restriction: `?order=id&limit=10` is allowed without
a filter. Set `db-safe-updates = false` to allow unrestricted writes.

## Prefer reference

| `Prefer` | Effect |
|---|---|
| `return=minimal` (default) | empty body (`201` for POST, `204` for PATCH/PUT/DELETE) |
| `return=representation` | the affected rows, shaped by `select` |
| `count=exact` | `Content-Range` ending in `/<n>`, the number of affected rows |
| `resolution=merge-duplicates` / `ignore-duplicates` | upsert behaviour of `POST` |
| `missing=default` | with `?columns=`, missing keys use column defaults |
| `tx=rollback` | run the request, then roll back: nothing is saved |
| `params=multiple-objects` | `/rpc`: call the routine once per object of an array body ([details](rpc.md#calling-a-routine-many-times)) |
| `params=single-object` | `/rpc`: pass the whole JSON body to the routine's only text parameter ([details](rpc.md#passing-the-whole-body)) |
| `handling=strict` | refuse the request (`FBRST130`) if any preference is unknown or invalid; the default `lenient` ignores them |
| `max-affected=n` | with `handling=strict`: fail a `PATCH`/`DELETE` that would change more than `n` rows ([details](#capping-how-many-rows-a-write-changes)) |
| `timezone=Europe/Berlin` | run the request in that time zone (Firebird 4+, [details](#time-zones)) |

Applied preferences are echoed in `Preference-Applied`. Combine them with commas or several
`Prefer` headers.

## Value formats

| Firebird type | JSON |
|---|---|
| `SMALLINT`, `INTEGER`, `BIGINT` | number |
| `INT128`, `NUMERIC`, `DECIMAL`, `DECFLOAT` | number, printed exactly as stored (`9.99`, not `9.9900000001`) |
| `FLOAT`, `DOUBLE PRECISION` | number |
| `BOOLEAN` | `true` / `false` |
| `DATE` | `"2024-05-06"` |
| `TIME` | `"13:14:15"` |
| `TIMESTAMP` | `"2024-05-06T13:14:15.1234"` |
| `... WITH TIME ZONE` | ISO 8601 with offset |
| `CHAR`, `VARCHAR`, `BLOB SUB_TYPE TEXT` | string |
| `BLOB SUB_TYPE BINARY`, `OCTETS` | base64 string (also accepted on input) |

On input, ISO 8601 timestamps (`2026-10-18T10:00:00+03:00`) and JSON numbers or strings for
numeric columns are accepted. JSON objects and arrays are stored as their text.

### Time zones

`Prefer: timezone=<IANA name>` runs the request in that time zone (Firebird 4+), for reads and
writes:

```sh
curl 'localhost:3000/meetings?select=title,starts_at' -H 'Prefer: timezone=Asia/Tokyo'
# [{"title":"Release planning","starts_at":"2026-10-20T15:00:00+09:00"}, ...]
```

- `TIME`/`TIMESTAMP WITH TIME ZONE` values are returned in that zone: the same instant, with the
  zone's offset. (Firebird stores a zone in each value; without the preference you get the zone
  it was stored with.)
- The Firebird session uses the zone too (`SET TIME ZONE`): `CURRENT_TIMESTAMP`, column defaults,
  and timestamps without an offset in filters or bodies (`starts_at=eq.2026-10-20T15:00:00`) are
  in that zone. The session zone is reset before the connection goes back to the pool.
- Plain `TIME` and `TIMESTAMP` columns have no zone and are not converted.
- An unknown zone is ignored (it is missing from `Preference-Applied`); with `handling=strict` it
  is refused with `FBRST130`. Firebird 3 has no time zones, so there the preference is ignored.
