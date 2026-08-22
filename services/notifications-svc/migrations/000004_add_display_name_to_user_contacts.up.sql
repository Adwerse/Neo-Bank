-- Populated from ProfileUpdated (user.events, shares auth-svc's outbox
-- with UserActivated — see README's mini-ADR on where profile data
-- lives). Nullable: most rows will never get a display name, and NULL
-- means "render the recipient without one", the same convention
-- account_number already uses on this table (migration 000003).
ALTER TABLE user_contacts ADD COLUMN display_name TEXT;
