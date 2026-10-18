# Schema cache

At start-up FirebirdREST reads the Firebird system tables (`RDB$RELATIONS`,
`RDB$RELATION_FIELDS`, `RDB$RELATION_CONSTRAINTS`, `RDB$PROCEDURES`, `RDB$FUNCTIONS` and
friends) and keeps the tables, columns, keys, foreign keys, routines and computed relationships
in memory. Requests are validated and planned from this cache, so it has to be reloaded after
DDL.

```
level=INFO msg="schema cache loaded" firebird=5.0.4 relations=8 routines=10 foreign_keys=6 request_context=true took=39ms
```

## Reloading

Any of these reloads it without dropping requests in flight:

=== "From the database"

    ```sql
    -- in isql; other tools don't need SET TERM
    SET TERM ^;
    EXECUTE BLOCK AS BEGIN POST_EVENT 'fbrest_reload'; END^
    SET TERM ;^
    COMMIT;
    ```

    Firebird delivers the event when the transaction commits. The name is set by
    `db-reload-event`.

=== "Signal"

    ```sh
    kill -USR1 $(pidof firebirdrest)
    ```

=== "Admin server"

    ```sh
    curl -X POST localhost:3001/reload        # with admin-server-port = 3001
    ```

### Reloading automatically after DDL

A database-level DDL trigger (Firebird 3+) posts the event after every metadata change:

```sql
SET TERM ^;
CREATE TRIGGER fbrest_ddl_reload AFTER ANY DDL STATEMENT AS
BEGIN
  POST_EVENT 'fbrest_reload';
END^
SET TERM ;^
```

## DDL and cached statements

FirebirdREST also caches **prepared statements** (`db-prepared-statements`), which roughly
halves the cost of each query. Firebird keeps a lock on every table, view and routine a prepared
statement refers to:

| DDL | While statements are cached |
|---|---|
| `ALTER TABLE ... ADD / DROP column`, `CREATE OR ALTER VIEW / PROCEDURE` | works |
| `DROP TABLE`, `DROP VIEW`, `DROP PROCEDURE`, ... | **waits** until the statements using the object are closed |

Statements unused for `db-prepared-statements-idle` seconds (default 30) are closed, and every
reload closes all of them. To drop an object that is in active use, reload first and then run
the `DROP` straight away:

```sh
curl -X POST localhost:3001/reload && isql -i drop_things.sql ...
```

Or set `db-prepared-statements = 0` to turn the statement cache off.
