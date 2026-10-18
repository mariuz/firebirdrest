// Command bench seeds a larger bookstore database and load-tests a running
// FirebirdREST server.
//
//	go run ./bench seed -dsn SYSDBA:masterkey@localhost:3050/var/lib/firebird/data/bench.fdb
//	go run ./bench run  -url http://localhost:3000 -c 16 -d 10s
//
// The database must already contain the schema from examples/bookstore.sql.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/nakagami/firebirdsql"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: bench seed|run [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "seed":
		seed(os.Args[2:])
	case "run":
		run(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: bench seed|run [flags]")
		os.Exit(2)
	}
}

// ---------------------------------------------------------------- seed

func seed(args []string) {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	dsn := fs.String("dsn", "", "DSN of a user allowed to insert (e.g. SYSDBA)")
	authors := fs.Int("authors", 2000, "number of authors")
	books := fs.Int("books", 100000, "number of books")
	customers := fs.Int("customers", 5000, "number of customers")
	orders := fs.Int("orders", 50000, "number of orders")
	fs.Parse(args)
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "-dsn is required")
		os.Exit(2)
	}
	db, err := sql.Open("firebirdsql", *dsn)
	check(err)
	defer db.Close()
	ctx := context.Background()

	steps := []struct{ name, sql string }{
		{"authors", fmt.Sprintf(`EXECUTE BLOCK AS DECLARE i INT = 0; BEGIN
  WHILE (i < %d) DO BEGIN
    INSERT INTO authors (name, country, born)
    VALUES ('Author ' || :i,
            DECODE(MOD(:i, 6), 0, 'UK', 1, 'USA', 2, 'Poland', 3, 'France', 4, 'Romania', 'Japan'),
            DATEADD(MOD(:i * 37, 25000) DAY TO DATE '1920-01-01'));
    i = i + 1;
  END
END`, *authors)},
		{"books", fmt.Sprintf(`EXECUTE BLOCK AS DECLARE i INT = 0; DECLARE a INT; BEGIN
  SELECT MIN(id) FROM authors INTO :a;
  WHILE (i < %d) DO BEGIN
    INSERT INTO books (title, author_id, price, published, in_stock, summary)
    VALUES ('Book ' || :i || ' of the ' || DECODE(MOD(:i, 4), 0, 'dragon', 1, 'stars', 2, 'sea', 'city'),
            :a + MOD(:i * 7919, %d), MOD(:i * 31, 5000) / 100.0 + 1, 1900 + MOD(:i, 125),
            MOD(:i, 10) <> 0, 'Summary of book ' || :i);
    i = i + 1;
  END
END`, *books, *authors)},
		{"tags", `EXECUTE BLOCK AS DECLARE i INT = 0; BEGIN
  WHILE (i < 50) DO BEGIN
    INSERT INTO tags (name) VALUES ('tag-' || :i);
    i = i + 1;
  END
END`},
		{"book_tags", `EXECUTE BLOCK AS DECLARE b INT; DECLARE t0 INT; BEGIN
  SELECT MIN(id) FROM tags WHERE name STARTING 'tag-' INTO :t0;
  FOR SELECT id FROM books WHERE title STARTING 'Book ' INTO :b DO BEGIN
    INSERT INTO book_tags (book_id, tag_id) VALUES (:b, :t0 + MOD(:b, 50));
    INSERT INTO book_tags (book_id, tag_id) VALUES (:b, :t0 + MOD(:b * 7 + 3, 50));
  END
END`},
		{"customers", fmt.Sprintf(`EXECUTE BLOCK AS DECLARE i INT = 0; BEGIN
  WHILE (i < %d) DO BEGIN
    INSERT INTO customers (email, name) VALUES ('c' || :i || '@example.com', 'Customer ' || :i);
    i = i + 1;
  END
END`, *customers)},
		{"orders", fmt.Sprintf(`EXECUTE BLOCK AS DECLARE i INT = 0; DECLARE c INT; DECLARE b INT; DECLARE o INT; BEGIN
  SELECT MIN(id) FROM customers WHERE email STARTING 'c' INTO :c;
  SELECT MIN(id) FROM books WHERE title STARTING 'Book ' INTO :b;
  WHILE (i < %d) DO BEGIN
    INSERT INTO orders (customer_id, status) VALUES (:c + MOD(:i * 13, %d), DECODE(MOD(:i, 3), 0, 'new', 1, 'paid', 'shipped'))
      RETURNING id INTO :o;
    INSERT INTO order_items (order_id, book_id, qty) VALUES (:o, :b + MOD(:i * 101, %d), 1 + MOD(:i, 3));
    INSERT INTO order_items (order_id, book_id, qty) VALUES (:o, :b + MOD(:i * 103 + 1, %d), 1);
    i = i + 1;
  END
END`, *orders, *customers, *books, *books)},
	}
	for _, s := range steps {
		start := time.Now()
		tx, err := db.BeginTx(ctx, nil)
		check(err)
		_, err = tx.ExecContext(ctx, s.sql)
		if err != nil {
			tx.Rollback()
			check(fmt.Errorf("%s: %w", s.name, err))
		}
		check(tx.Commit())
		fmt.Printf("seeded %-10s in %s\n", s.name, time.Since(start).Round(time.Millisecond))
	}
	// fresh index statistics so the optimizer picks the indexes
	rows, err := db.QueryContext(ctx, `SELECT TRIM(RDB$INDEX_NAME) FROM RDB$INDICES WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0`)
	check(err)
	var idx []string
	for rows.Next() {
		var n string
		check(rows.Scan(&n))
		idx = append(idx, n)
	}
	rows.Close()
	for _, n := range idx {
		_, err := db.ExecContext(ctx, `SET STATISTICS INDEX "`+n+`"`)
		check(err)
	}
	fmt.Printf("updated statistics of %d indexes\n", len(idx))
}

// ---------------------------------------------------------------- run

type scenario struct {
	name    string
	method  string
	path    string
	body    string
	headers []string
	auth    bool
}

var scenarios = []scenario{
	{name: "row by primary key", method: "GET", path: "/books?id=eq.{n}"},
	{name: "page of 100 rows", method: "GET", path: "/books?select=id,title,price&limit=100&offset={n100}"},
	{name: "filter + order + limit 25", method: "GET", path: "/books?select=id,title&price=gt.20&in_stock=is.true&order=price.desc&limit=25"},
	{name: "ilike search, limit 25", method: "GET", path: "/books?select=id,title&title=ilike.*dragon*&limit=25&offset={n100}"},
	{name: "embed many-to-one (25)", method: "GET", path: "/books?select=title,authors(name,country)&limit=25&offset={n100}"},
	{name: "embed one-to-many (10)", method: "GET", path: "/authors?select=name,books(title,price)&limit=10&offset={n10}"},
	{name: "embed many-to-many (25)", method: "GET", path: "/books?select=title,tags(name)&limit=25&offset={n100}"},
	{name: "nested 3 levels (5)", method: "GET", path: "/customers?select=email,orders(status,order_items(qty,books(title)))&limit=5&offset={n10}", auth: true},
	{name: "aggregate group by", method: "GET", path: "/books?select=published,count(),price.avg()&published=gte.2000&order=published"},
	{name: "count=exact, 25 rows", method: "GET", path: "/books?select=id&in_stock=is.true&limit=25", headers: []string{"Prefer", "count=exact"}},
	{name: "computed field + order", method: "GET", path: "/books?select=id,price_with_vat&author_id=eq.{a}&order=price_with_vat.desc"},
	{name: "rpc function", method: "GET", path: "/rpc/add_vat?amount={n}"},
	{name: "insert 1 row (rollback)", method: "POST", path: "/tags", body: `{"name":"bench-{n}"}`,
		headers: []string{"Prefer", "tx=rollback"}, auth: true},
	{name: "insert 100 rows (rollback)", method: "POST", path: "/authors", body: "{bulk}",
		headers: []string{"Prefer", "tx=rollback"}, auth: true},
}

type result struct {
	name               string
	reqs, errs         int64
	rps                float64
	p50, p95, p99, max time.Duration
	firstErr           string
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	base := fs.String("url", "http://localhost:3000", "server URL")
	conc := fs.Int("c", 16, "concurrent clients")
	dur := fs.Duration("d", 10*time.Second, "duration per scenario")
	warm := fs.Duration("warmup", 2*time.Second, "warm-up per scenario")
	token := fs.String("token", "", "JWT for scenarios that need an authenticated role")
	only := fs.String("only", "", "run only scenarios whose name contains this text")
	fs.Parse(args)

	tr := &http.Transport{MaxIdleConns: *conc * 2, MaxIdleConnsPerHost: *conc * 2, IdleConnTimeout: time.Minute}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	fmt.Printf("FirebirdREST benchmark: %d clients, %s per scenario, %s, %d CPUs\n\n", *conc, *dur, runtime.GOOS+"/"+runtime.GOARCH, runtime.NumCPU())
	fmt.Println("| Scenario | req/s | p50 | p95 | p99 | errors |")
	fmt.Println("|---|---:|---:|---:|---:|---:|")
	for _, sc := range scenarios {
		if *only != "" && !strings.Contains(sc.name, *only) {
			continue
		}
		if sc.auth && *token == "" {
			fmt.Printf("| %s | skipped (needs -token) | | | | |\n", sc.name)
			continue
		}
		runFor(client, *base, sc, *token, *conc, *warm)
		r := runFor(client, *base, sc, *token, *conc, *dur)
		fmt.Printf("| %s | %.0f | %s | %s | %s | %d |\n", r.name, r.rps, ms(r.p50), ms(r.p95), ms(r.p99), r.errs)
		if r.firstErr != "" {
			fmt.Fprintf(os.Stderr, "  first error in %q: %s\n", r.name, r.firstErr)
		}
	}
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000)
}

var bulk = func() string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < 100; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"name":"Bench author %d","country":"UK"}`, i)
	}
	sb.WriteByte(']')
	return sb.String()
}()

func expand(s string, i int64) string {
	r := strings.NewReplacer(
		"{n}", fmt.Sprint(1+i%50000),
		"{n100}", fmt.Sprint((i*25)%1000),
		"{n10}", fmt.Sprint((i*10)%1900),
		"{a}", fmt.Sprint(1+i%2000),
		"{bulk}", bulk,
	)
	return r.Replace(s)
}

func runFor(client *http.Client, base string, sc scenario, token string, conc int, d time.Duration) result {
	var counter, errs atomic.Int64
	var mu sync.Mutex
	var firstErr string
	lat := make([][]time.Duration, conc)
	deadline := time.Now().Add(d)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				i := counter.Add(1)
				var body io.Reader
				if sc.body != "" {
					body = bytes.NewBufferString(expand(sc.body, i))
				}
				req, _ := http.NewRequest(sc.method, base+expand(sc.path, i), body)
				if sc.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				for h := 0; h+1 < len(sc.headers); h += 2 {
					req.Header.Add(sc.headers[h], sc.headers[h+1])
				}
				if sc.auth {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				t := time.Now()
				resp, err := client.Do(req)
				el := time.Since(t)
				if err == nil {
					b, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if resp.StatusCode >= 400 {
						err = fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, b)
					}
				}
				if err != nil {
					errs.Add(1)
					mu.Lock()
					if firstErr == "" {
						firstErr = err.Error()
					}
					mu.Unlock()
					continue
				}
				lat[w] = append(lat[w], el)
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)
	var all []time.Duration
	for _, l := range lat {
		all = append(all, l...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	pct := func(p float64) time.Duration {
		if len(all) == 0 {
			return 0
		}
		return all[int(float64(len(all)-1)*p)]
	}
	r := result{name: sc.name, reqs: int64(len(all)), errs: errs.Load(), firstErr: firstErr,
		rps: float64(len(all)) / elapsed.Seconds(), p50: pct(0.50), p95: pct(0.95), p99: pct(0.99)}
	if len(all) > 0 {
		r.max = all[len(all)-1]
	}
	return r
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
