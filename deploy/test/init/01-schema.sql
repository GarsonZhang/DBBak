CREATE DATABASE IF NOT EXISTS vault_restore CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
CREATE TABLE IF NOT EXISTS vault_source.sample_records (
  id BIGINT PRIMARY KEY,
  name VARCHAR(100) NOT NULL,
  amount DECIMAL(12,2) NOT NULL,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB;
INSERT INTO vault_source.sample_records(id,name,amount) VALUES
  (1,'隔离验证-A',100.50),
  (2,'隔离验证-B',200.75),
  (3,'隔离验证-C',300.00);
