GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, LOCK TABLES, RELOAD ON *.* TO 'dbbak_test'@'%';
GRANT ALL PRIVILEGES ON dbbak_source.* TO 'dbbak_test'@'%';
GRANT ALL PRIVILEGES ON dbbak_restore.* TO 'dbbak_test'@'%';
FLUSH PRIVILEGES;
