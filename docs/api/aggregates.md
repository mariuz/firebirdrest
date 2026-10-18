# Aggregates

`select` accepts `count()`, `sum()`, `avg()`, `min()` and `max()`. Every other selected column
becomes part of the `GROUP BY`.

```
GET /books?select=count()
[{"count":5}]

GET /books?select=author_id,count(),total:price.sum(),price.avg()&order=author_id
[{"author_id":1,"count":2,"total":18.49,"avg":9.24}, ...]

GET /books?select=in_stock,count()&order=in_stock
[{"in_stock":false,"count":1},{"in_stock":true,"count":4}]

GET /books?select=published.min(),published.max()
[{"min":1961,"max":1992}]
```

- The key is the function name unless you give an alias (`total:price.sum()`).
- `count()` counts rows; `column.count()` counts non-null values.
- Casts apply to the result: `price.sum()::text`.
- Filters apply before grouping (SQL `WHERE`).
- `order` may use the grouped columns.
- `Prefer: count=exact` counts the groups.

Firebird's typing applies: `avg()` of an integer column returns an integer (1976.8 becomes
1976). Cast the column to `numeric` or `double` in a view, or use a computed field, if you need
the fraction.

## Inside embeds

Aggregates in an embed are computed per parent row:

```
GET /authors?select=name,books(count)
[{"name":"Terry Pratchett","books":[{"count":2}]}, ...]

GET /authors?select=name,books(count(),price.sum())
```

Inside an embed, plain `count` is short for `count()`. A parent without related rows gets
`[{"count":0}]`. Spread the embed to get flat values:

```
GET /authors?select=name,...books(count(),total:price.sum())
[{"name":"Terry Pratchett","count":2,"total":18.49}, ...]
```

## Turning them off

Aggregates over large tables can be expensive. Set `db-aggregates-enabled = false` to reject
them with `400`.
