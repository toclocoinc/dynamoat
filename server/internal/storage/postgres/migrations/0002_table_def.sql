-- dynamoat migration 0002. Store the whole storage.Table in ddb_tables.def so
-- every field round-trips; the typed columns from 0001 keep the unique name
-- index and give Open a fast way to find tables left DELETING.
ALTER TABLE ddb_tables ADD COLUMN IF NOT EXISTS def jsonb NOT NULL DEFAULT '{}'::jsonb;
