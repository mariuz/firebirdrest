# Reading tables

Every table and view is exposed at `/<name>`. Global temporary tables are included; system tables
are not.

## Names

Firebird stores unquoted identifiers in upper case. The API shows those in lower case, so table
`BOOKS` is `/books` and column `AUTHOR_ID` is `author_id`. Identifiers created with quotes keep
their exact spelling: a table `"Events Log"` is `/Events%20Log`, with a column `"When"` as `When`.
Lookups also accept the database spelling (`/BOOKS`).

## Selecting columns

| Request | Meaning |
|---|---|
| `GET /books` | all columns (`select=*`) |
| `GET /books?select=id,title` | chosen columns, in this order |
| `GET /books?select=book:title` | rename: `alias:column` |
| `GET /books?select=price::text,published::int` | cast: `column::type` |
| `GET /books?select="Odd Name"` | quote names that contain special characters |

Casts: `text`, `varchar`, `integer`, `smallint`, `bigint`, `int128`, `numeric`, `double`,
`decfloat`, `boolean`, `date`, `time`, `timestamp`.

## Filtering

A query parameter named after a column filters on it: `column=operator.value`. Several filters
are combined with `AND`, and one column may be filtered several times
(`?price=gt.5&price=lt.10`).

| Operator | SQL | Example |
|---|---|---|
| `eq`, `neq` | `=`, `<>` | `?status=eq.new` |
| `gt`, `gte`, `lt`, `lte` | `>`, `>=`, `<`, `<=` | `?price=lt.10` |
| `like` | `LIKE`, with `*` as the wildcard | `?title=like.The*` |
| `ilike` | case-insensitive `LIKE` | `?title=ilike.*god*` |
| `similar` | `SIMILAR TO` | `?code=similar.[A-Z]{3}` |
| `containing` | `CONTAINING` (case-insensitive substring) | `?summary=containing.dragon` |
| `starting` | `STARTING WITH` | `?title=starting.The` |
| `in` | `IN (...)` | `?id=in.(1,2,3)`, `?name=in.("a,b",c)` |
| `is` | `IS NULL`, `IS NOT NULL` (`not_null`), `IS TRUE/FALSE/UNKNOWN` | `?born=is.null` |
| `isdistinct` | `IS DISTINCT FROM` | `?a=isdistinct.5` |
| `not.<op>` | `NOT (...)` | `?id=not.in.(1,2)` |

`containing`, `starting` and `similar` are Firebird operators that PostgREST does not have.
`starting` can use an index on the column; `like`/`ilike` patterns, `containing` and `similar`
scan the rows.

Values are converted to the column type: booleans accept `true/false/t/f/1/0`, dates and
timestamps accept ISO 8601 (`2024-05-06`, `2024-05-06T07:08:09Z`). Wrap values that contain
commas or parentheses in double quotes inside `in.(...)`.

### Logic trees

`or` and `and` take a parenthesised list of conditions written as `column.operator.value`. They
nest, and `not.` negates a group:

```
?or=(price.lt.8,and(published.gte.1990,in_stock.is.true))
?not.and=(id.gt.2,id.lt.5)
?or=(title.ilike.*sea*,title.ilike.*star*)
?or=(born.is.null,country.not.eq.UK)
```

Inside a tree, negate a single condition with `column.not.operator.value`.

## Ordering

```
?order=price.desc
?order=published.desc.nullslast,title
?order=price.asc.nullsfirst
```

Without `order`, rows come in whatever order Firebird returns them.

## Paging

```
?limit=25&offset=50
```

or the `Range` header (items, zero-based, inclusive):

```http
Range: 50-74
```

Firebird runs these as `OFFSET n ROWS FETCH NEXT m ROWS ONLY`. Large offsets get slower, because
Firebird has to skip those rows. For deep paging, filter on an indexed key instead
(`?id=gt.<last id>&order=id&limit=25`).

`db-max-rows` caps every read, whatever the request asks for.

## Counting

Send `Prefer: count=exact` to get the total in `Content-Range`:

```http
GET /books?select=id&limit=2&offset=1
Prefer: count=exact
```

```http
HTTP/1.1 206 Partial Content
Content-Range: 1-2/5
```

The status is `206 Partial Content` when the page doesn't contain every matching row, and `200`
otherwise. Counting runs an extra `SELECT COUNT(*)`, which reads every matching row; only ask for
it when you need it.

## HEAD

`HEAD` runs the same query and returns only the headers, so `HEAD` with `Prefer: count=exact`
gives the count without a body.

## Transactions

Every request runs in its own transaction. `GET` and `HEAD` use a **read-only** transaction, so
a view or selectable procedure that tries to write fails instead of changing data.
