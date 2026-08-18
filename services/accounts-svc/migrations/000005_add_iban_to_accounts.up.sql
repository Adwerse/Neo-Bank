-- iban is nullable at the schema level: existing rows predate this column
-- and are filled in by a Go backfill step at startup (backfillIBANs in
-- backfill.go), not by this migration. The ISO 7064 mod-97-10 check-digit
-- arithmetic has exactly one implementation, in Go (pkg/iban), shared by
-- both new-account creation and the backfill — not a second copy
-- reimplemented in SQL that could silently drift from it. By the time this
-- service accepts any request, the backfill step (run synchronously right
-- after migrations, before any listener starts) has already filled every
-- row, so application code can treat the column as effectively non-null.
ALTER TABLE accounts ADD COLUMN iban TEXT UNIQUE;
