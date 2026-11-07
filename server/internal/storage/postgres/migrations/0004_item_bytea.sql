-- dynamoat migration 0004. items.item becomes raw JSON bytes instead of jsonb:
-- jsonb cannot represent \u0000, which a DynamoDB string attribute may contain,
-- so the exact JSON text is kept as bytea. The store never queries the item
-- JSON with SQL, so nothing is lost.
ALTER TABLE items ALTER COLUMN item TYPE bytea USING item::text::bytea;
