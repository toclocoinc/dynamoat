-- dynamoat migration 0009. Deletion protection.
--
-- DeletionProtectionEnabled is stored both in ddb_tables.def (the whole
-- storage.Table) and here as a typed column, so a protected table survives a
-- round trip through the catalog even when the definition is read on its own.
ALTER TABLE ddb_tables ADD COLUMN IF NOT EXISTS deletion_protection boolean NOT NULL DEFAULT false;
