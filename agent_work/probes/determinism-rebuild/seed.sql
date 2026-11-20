-- Deterministic seed workload: schema + inserts/updates/deletes, no nondeterministic functions.
CREATE TABLE customer(id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT UNIQUE, balance REAL);
CREATE TABLE orders(id INTEGER PRIMARY KEY, customer_id INTEGER REFERENCES customer(id), note TEXT, amount INTEGER);
CREATE INDEX orders_cust ON orders(customer_id);
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<2000)
INSERT INTO customer SELECT i, 'name-'||i, 'user'||i||'@example.com', i*1.25 FROM n;
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<6000)
INSERT INTO orders SELECT i, (i*7)%2000+1, printf('%.*c', 50+(i%200), 'x'), i*3 FROM n;
UPDATE customer SET balance = balance*2 WHERE id % 3 = 0;
UPDATE orders SET note = note||note WHERE id % 11 = 0;
DELETE FROM orders WHERE id % 5 = 0;
DELETE FROM customer WHERE id > 1900;
