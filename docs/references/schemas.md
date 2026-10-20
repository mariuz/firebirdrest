# Schemas

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
- embedding follows foreign keys within the schema. A foreign key that references a table in
  another schema is not an embeddable relationship; computed fields and relationships likewise
  only use routines and `TYPE OF COLUMN` parameters of the same schema;
- packaged routines are `/rpc/package.name` of the schema's packages.

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
