ALTER TABLE entries ADD COLUMN reference UUID;

CREATE INDEX idx_entries_reference ON entries (reference);
