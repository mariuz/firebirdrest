# Schemas

!!! warning "Experimental"
    Schema support is built and tested against the Firebird 6 snapshot, before Firebird 6 is
    released. The configuration and headers follow PostgREST and are expected to stay, but
    details may change if Firebird 6 changes before its release.

Firebird 6 adds SQL schemas: named namespaces for tables, views, procedures, functions and
packages, like PostgreSQL's. FirebirdREST can expose several of them and lets each request pick
one, with the same headers as PostgREST.

Firebird 3, 4 and 5 have no schemas: the whole database is one namespace, `db-schemas` must not
be set, and the profile headers below are answered with `406`.

## Exposing schemas

```ini
db-schemas = "public, inventory"
```

The first schema is the default. Without `db-schemas`, Firebird 6 exposes `PUBLIC` only, as
before. Names are matched like unquoted identifiers (`inventory` is `INVENTORY`); a schema created
with a quoted, mixed-case name is written as stored. FirebirdREST refuses to start if a schema
doesn't exist, and `SYSTEM` cannot be exposed.

Each schema has its own schema cache, reloaded together by `POST_EVENT 'fbrest_reload'` or
`POST /reload`.

## Choosing the schema of a request

| Request | Header |
|---|---|
| `GET`, `HEAD` | `Accept-Profile: inventory` |
| `POST`, `PATCH`, `PUT`, `DELETE` | `Content-Profile: inventory` |

```sh
curl 'localhost:3000/warehouses?select=name,stock(qty)' -H 'Accept-Profile: inventory'
curl -X POST 'localhost:3000/warehouses' -H 'Content-Profile: inventory' \
     -H 'Content-Type: application/json' -d '{"name":"Iasi"}'
curl 'localhost:3000/rpc/low_stock?below=5' -H 'Accept-Profile: inventory'
```

Without a header the request uses the default schema. With more than one schema exposed, every
response says which one it used in `Content-Profile`. A schema that isn't exposed is refused with
`406` (`FBRST106`), listing the exposed ones. The OpenAPI description at `/` follows
`Accept-Profile` too.

Everything in a request happens in its schema:

- tables, views and routines are looked up there, so two schemas can each have a `books` table;
- every statement uses schema-qualified names (`"INVENTORY"."STOCK"`), not the search path;
- embedding follows foreign keys within the schema and into the other exposed schemas (see
  below); computed fields and relationships only use routines and `TYPE OF COLUMN` parameters
  of the same schema;
- packaged routines are `/rpc/package.name` of the schema's packages.

## Relationships across schemas

A foreign key whose table and referenced table are in two exposed schemas is a relationship
like any other, in both directions, as in PostgREST. In the example, `inventory.stock.book_id`
references `public.books`:

```sh
# from inventory into public ...
curl 'localhost:3000/stock?select=qty,books(title)' -H 'Accept-Profile: inventory'
# ... and from public into inventory, nesting on within inventory
curl 'localhost:3000/books?select=title,stock(qty,warehouses(name))&id=eq.1'
```

The embedded table is named as in its own schema; it does not become a resource of the
request's schema (`GET /stock` without `Accept-Profile: inventory` is still `404`). When the
request's schema has a table of the same name, as `inventory` has its own `books`, the foreign
key decides which one is meant; with several candidate keys, disambiguate with
`books!<constraint or column>(...)` as usual. A foreign key into a schema that is not in
`db-schemas` is not a relationship: that table is not part of the API.

The role needs `USAGE` on both schemas and the usual `SELECT` on the embedded table.

## Permissions

Besides the grants on each object, a role needs `USAGE` on the schema (for `PUBLIC` this is
granted to everyone by default):

```sql
GRANT USAGE ON SCHEMA inventory TO ROLE web_anon;
GRANT SELECT ON inventory.warehouses TO ROLE web_anon;
```

Without it every request into the schema fails with *no permission for USAGE access to SCHEMA*,
answered as `401` for anonymous requests and `403` otherwise.

## Example

[`examples/bookstore-fb6.sql`](https://github.com/mariuz/firebirdrest/blob/main/examples/bookstore-fb6.sql)
adds an `inventory` schema to the example database, with warehouses, stock, its own `books`
table, a selectable procedure, a package and the grants above.

`db-pre-request` may name a procedure of any schema: `db-pre-request = "inventory.check_request"`.
