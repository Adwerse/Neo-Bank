DROP INDEX IF EXISTS idx_entries_reference;
ALTER TABLE entries DROP COLUMN IF EXISTS reference;
