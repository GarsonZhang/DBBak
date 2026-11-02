GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, LOCK TABLES, RELOAD ON *.* TO 'vault_test'@'%';
GRANT ALL PRIVILEGES ON vault_source.* TO 'vault_test'@'%';
GRANT ALL PRIVILEGES ON vault_restore.* TO 'vault_test'@'%';
FLUSH PRIVILEGES;
