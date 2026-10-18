# Resource embedding

Foreign keys connect tables. Name a related table in `select`, with its own column list in
parentheses, and its rows are nested into the response.

## Relationship kinds

```
GET /books?select=title,authors(name)              # many-to-one: an object (or null)
GET /authors?select=name,books(title,price)        # one-to-many: an array
GET /books?select=title,tags(name)                 # many-to-many: an array
```

```json
[{"title":"Guards! Guards!","authors":{"name":"Terry Pratchett"}}]
[{"name":"Terry Pratchett","books":[{"title":"Guards! Guards!","price":9.99},{"title":"Small Gods","price":8.50}]}]
[{"title":"Guards! Guards!","tags":[{"name":"fantasy"},{"name":"humour"}]}]
```

**Many-to-many** relationships go through a junction table whose primary key consists of foreign
keys to both sides, like `book_tags (book_id, tag_id)`. A one-to-many relationship whose foreign
key is also the child's primary key is treated as **one-to-one** and returns an object.

Embeds nest to any depth:

```
GET /customers?select=email,orders(status,order_items(qty,books(title,authors(name))))
```

## Naming and disambiguation

```
GET /books?select=title,writer:authors(name)        # alias the key
GET /books?select=title,author_id(name)             # embed by the foreign key column
```

When two foreign keys connect the same tables, FirebirdREST answers `300 Multiple Choices` and
lists them. Pick one with `!` followed by the constraint name or the column:

```
GET /orders?select=*,customers!fk_orders_customer(name)
GET /orders?select=*,billing:customers!billing_customer_id(name),shipping:customers!shipping_customer_id(name)
```

## Filtering, ordering and limiting embeds

Prefix a parameter with the embed's name (or alias) to apply it to the embedded rows:

```
GET /authors?select=name,books(title,price)&books.price=gt.9&books.order=price.desc&books.limit=1
GET /authors?select=name,books(title)&books.or=(title.ilike.*sea*,title.ilike.*star*)
GET /customers?select=email,orders(order_items(qty))&orders.order_items.qty=gt.1
```

These filters only remove embedded rows. Parents are always returned, with an empty array or
`null` when nothing matches, like a `LEFT JOIN`.

## Filtering parents by their embeds

`!inner` keeps only the parents that have at least one matching embedded row:

```
GET /books?select=title,authors!inner(name)&authors.country=eq.Poland
GET /books?select=title,tags!inner(name)&tags.name=eq.humour
```

`<embed>=is.null` and `<embed>=not.is.null` keep the parents without, or with, related rows:

```
GET /books?select=title,tags(name)&tags=is.null        # books without tags
GET /authors?select=name,books(title)&books=not.is.null
```

These turn into `EXISTS (...)` conditions on the parent query, so they also work with `limit`,
`order` and `Prefer: count=exact`.

## Spread

`...` merges an embed's columns into the parent instead of nesting them.

**To-one** (many-to-one, one-to-one, executable computed relationships): the values themselves.

```
GET /books?select=title,...authors(author:name,country)
```

```json
[{"title":"Guards! Guards!","author":"Terry Pratchett","country":"UK"}]
```

**To-many** (one-to-many, many-to-many, selectable computed relationships): one array per column,
in the embed's order.

```
GET /authors?select=name,...books(title,price)&books.order=price.desc
GET /books?select=title,...tags(tag:name)
```

```json
[{"name":"Terry Pratchett","title":["Guards! Guards!","Small Gods"],"price":[9.99,8.50]}]
[{"title":"Guards! Guards!","tag":["fantasy","humour"]}]
```

A spread of **aggregates only** gives plain values, because there is one row per parent:

```
GET /authors?select=name,...books(count(),total:price.sum())
```

```json
[{"name":"Terry Pratchett","count":2,"total":18.49}]
```

Spread embeds can't have an alias. If a spread column would collide with another key (both
tables have a `name`), the request fails with a hint; rename one with an alias, as in
`...tags(tag:name)`.

## Key order

Keys appear in the order of `select`, embeds included: `select=authors(name),title` puts
`authors` first.

## How it runs

Each embed level is one extra query, batched for all parents with `IN (...)`. `select=a,b(c(d))`
over 25 rows makes three queries, not 1 + 25 + 25×n. Per-parent `limit`/`offset` on embeds are
applied after fetching, so `books.limit=1` still reads every book of the authors on the page.
