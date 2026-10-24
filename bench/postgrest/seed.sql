-- The same rows as `bench seed` creates on Firebird: 2,000 authors, 100,000
-- books, 50 tags with 200,000 links, 5,000 customers, 50,000 orders with
-- 100,000 items. The formulas are the Firebird ones, so the two databases
-- hold identical data.
--
--   psql -U postgres -d bench -f seed.sql

INSERT INTO authors (name, country, born)
SELECT 'Author ' || i,
       (ARRAY['UK', 'USA', 'Poland', 'France', 'Romania', 'Japan'])[1 + i % 6],
       DATE '1920-01-01' + (i * 37) % 25000
FROM generate_series(0, 1999) AS i;

INSERT INTO books (title, author_id, price, published, in_stock, summary)
SELECT 'Book ' || i || ' of the ' || (ARRAY['dragon', 'stars', 'sea', 'city'])[1 + i % 4],
       (SELECT min(id) FROM authors) + (i * 7919) % 2000,
       (i * 31) % 5000 / 100.0 + 1,
       1900 + i % 125,
       i % 10 <> 0,
       'Summary of book ' || i
FROM generate_series(0, 99999) AS i;

INSERT INTO tags (name) SELECT 'tag-' || i FROM generate_series(0, 49) AS i;

INSERT INTO book_tags (book_id, tag_id)
SELECT b.id, t0 + b.id % 50
FROM books b, (SELECT min(id) AS t0 FROM tags WHERE name LIKE 'tag-%') t
WHERE b.title LIKE 'Book %'
UNION
SELECT b.id, t0 + (b.id * 7 + 3) % 50
FROM books b, (SELECT min(id) AS t0 FROM tags WHERE name LIKE 'tag-%') t
WHERE b.title LIKE 'Book %';

INSERT INTO customers (email, name)
SELECT 'c' || i || '@example.com', 'Customer ' || i FROM generate_series(0, 4999) AS i;

-- orders get consecutive ids, so the items can be generated from the same i
INSERT INTO orders (customer_id, status)
SELECT (SELECT min(id) FROM customers WHERE email LIKE 'c%') + (i * 13) % 5000,
       (ARRAY['new', 'paid', 'shipped'])[1 + i % 3]
FROM generate_series(0, 49999) AS i;

INSERT INTO order_items (order_id, book_id, qty)
SELECT o0 + i, b0 + (i * 101) % 100000, 1 + i % 3
FROM generate_series(0, 49999) AS i,
     (SELECT min(id) AS o0 FROM orders WHERE status IN ('new', 'paid', 'shipped') AND id > 1) o,
     (SELECT min(id) AS b0 FROM books WHERE title LIKE 'Book %') b
UNION ALL
SELECT o0 + i, b0 + (i * 103 + 1) % 100000, 1
FROM generate_series(0, 49999) AS i,
     (SELECT min(id) AS o0 FROM orders WHERE status IN ('new', 'paid', 'shipped') AND id > 1) o,
     (SELECT min(id) AS b0 FROM books WHERE title LIKE 'Book %') b
ON CONFLICT DO NOTHING;

ANALYZE;
