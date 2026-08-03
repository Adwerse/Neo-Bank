-- account_balances is a pure cache, safe to drop unconditionally for
-- genesis: even after every deposit's entries have been cleaned back to a
-- net-zero balance (e.g. between test runs), applyBalanceDelta's upsert
-- leaves a zero-balance row behind rather than deleting it, which would
-- otherwise block the DELETE below on its own FK regardless of whether
-- genesis has any real transaction history.
DELETE FROM account_balances
WHERE ledger_account_id = '00000000-0000-0000-0000-000000000001';

-- Only succeeds if genesis has never been the target of a real deposit/
-- seed/devtopup write: entries.ledger_account_id REFERENCES
-- ledger_accounts(id) rejects this DELETE with a foreign-key violation
-- once any entries reference this row. That's an intentional, safe
-- failure mode — this down migration is only realistically usable against
-- a pristine database (e.g. a fresh CI up/down cycle), not one with real
-- deposit history, and it fails loudly rather than silently orphaning
-- entries.
DELETE FROM ledger_accounts
WHERE id = '00000000-0000-0000-0000-000000000001'
  AND account_id = '00000000-0000-0000-0000-000000000001';
