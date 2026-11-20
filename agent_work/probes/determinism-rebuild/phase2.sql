-- Second-phase workload (several transactions) used by the WAL probes.
BEGIN; UPDATE customer SET balance = balance + 1 WHERE id % 7 = 0; COMMIT;
BEGIN; DELETE FROM orders WHERE id % 13 = 0; COMMIT;
BEGIN; WITH RECURSIVE n(i) AS (SELECT 6001 UNION ALL SELECT i+1 FROM n WHERE i<6500)
  INSERT INTO orders SELECT i, i%1900+1, printf('%.*c', 100, 'y'), i FROM n; COMMIT;
BEGIN; UPDATE orders SET note='short' WHERE id BETWEEN 100 AND 400; COMMIT;
BEGIN; DELETE FROM orders WHERE id > 3000; COMMIT;
