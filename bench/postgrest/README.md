# Comparing with PostgREST

The same scenarios as `bench run` uses, against PostgREST on PostgreSQL with identical data.
Results and interpretation are on the [Benchmarks](https://mariuz.github.io/firebirdrest/benchmarks/)
page.

```sh
# PostgreSQL with the schema and the same seed rows as `bench seed` creates on Firebird
docker run -d --name pgbench -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=bench postgres:17 -c shared_buffers=512MB
sleep 5
docker cp bench/postgrest/schema.sql pgbench:/tmp/ && docker cp bench/postgrest/seed.sql pgbench:/tmp/
docker exec pgbench psql -q -U postgres -d bench -v ON_ERROR_STOP=1 -f /tmp/schema.sql
docker exec pgbench psql -q -U postgres -d bench -v ON_ERROR_STOP=1 -f /tmp/seed.sql
docker exec pgbench psql -q -U postgres -d bench -c "VACUUM ANALYZE"

# PostgREST, configured like FirebirdREST in the comparison: pool of 32, anonymous role,
# the same JWT secret, aggregates on, and Prefer: tx=rollback honoured
PGIP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' pgbench)
docker run -d --name pgrest \
  -e PGRST_DB_URI="postgres://authenticator:authenticator@$PGIP:5432/bench" \
  -e PGRST_DB_SCHEMAS=public -e PGRST_DB_ANON_ROLE=web_anon \
  -e PGRST_JWT_SECRET=reallyreallyreallyreallyverysafe1234 \
  -e PGRST_DB_POOL=32 -e PGRST_DB_AGGREGATES_ENABLED=true \
  -e PGRST_DB_TX_END=commit-allow-override \
  postgrest/postgrest:latest
PRIP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' pgrest)

# the same tool, the same token (role web_user, signed with the secret above)
go run ./bench run -url http://$PRIP:3000 -c 16 -d 4s -token "$JWT"
go run ./bench run -url http://localhost:3000 -c 16 -d 4s -token "$JWT"   # FirebirdREST
```

Run each side at least twice, alternating, and compare medians: on a shared machine the
background load drifts, and alternating spreads it over both.

Without `PGRST_DB_TX_END=commit-allow-override` PostgREST ignores `Prefer: tx=rollback` and the
insert scenarios commit their rows, which changes later results. `schema.sql` adds indexes on
the foreign key columns, which Firebird creates by itself; without them the embed scenarios
would compare an indexed database with an unindexed one.
