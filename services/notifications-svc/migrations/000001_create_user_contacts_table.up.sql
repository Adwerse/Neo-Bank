-- user_contacts is notifications-svc's own local projection of
-- user_id/account_id -> email, built entirely from Kafka events
-- (UserActivated, AccountCreated) — never a synchronous call to auth-svc
-- or accounts-svc. account_id starts NULL: UserActivated (which carries
-- email but not account_id) is what creates the row, and AccountCreated
-- (published later, once accounts-svc creates the account) fills
-- account_id in on a follow-up UPDATE. See kafka.go for the ordering.
CREATE TABLE user_contacts (
    user_id UUID PRIMARY KEY,
    account_id UUID UNIQUE,
    email TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
