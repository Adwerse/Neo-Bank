// The backend returns a stable snake_case error code for every 4xx/5xx it
// can produce — never a sentence (see gateway/openapi.yaml's Error
// schema) — so this is the one place a code becomes the text a signed-in
// user actually reads. Every call site looks a code up here instead of
// keeping its own copy, which is what makes adding a new locale later
// nearly free: the lookup already exists, only its values would need
// translating.
const ERROR_MESSAGES: Record<string, string> = {
  // Generic / shared across services
  invalid_request_body: 'Something went wrong with that request, please try again',
  internal_error: 'Something went wrong on our end, please try again',
  missing_user_id_header: 'Your session looks invalid, please log in again',
  user_not_found: 'Account not found',
  account_not_found: 'Account not found',
  account_not_active: "This account isn't active right now",

  // auth-svc
  email_already_registered: 'This email is already registered',
  email_already_verified: 'This email is already verified',
  email_not_verified: 'Email not verified yet',
  verification_email_send_failed: 'Could not send the verification email, please try again',
  invalid_credentials: 'Incorrect email or password',
  invalid_email: 'Enter a valid email address',
  invalid_refresh_token: 'Your session has expired, please log in again',
  invalid_reset_code: 'This code is invalid or has expired. Request a new one on the password recovery page.',
  invalid_verification_code: 'Incorrect code, please try again',
  no_active_verification_code: 'No active code found. Request a new one.',
  password_too_short: 'Password must be at least 8 characters',
  verification_code_cooldown: 'Please wait a moment before requesting another code',
  too_many_verification_attempts: 'Too many attempts. Request a new code.',
  verification_code_expired: 'This code has expired. Request a new one.',

  // profile / avatar (auth-svc)
  invalid_key: 'Something went wrong, please try uploading the photo again',
  avatar_upload_not_found: 'Upload not found — it may have expired. Please try again.',
  avatar_upload_rate_limited: 'Too many attempts, please try again later',
  avatar_too_large: 'That image is too large (max 8 MB)',
  avatar_unsupported_type: 'Only JPEG or PNG images are supported',
  avatar_too_many_pixels: 'That image is too large to process — try a smaller photo',
  avatar_decode_failed: "Couldn't read that image — try a different file",
  display_name_too_long: 'Name must be 50 characters or fewer',
  display_name_invalid_characters: "Name contains characters that aren't allowed",

  // transfers-svc
  self_transfer_not_allowed: "You can't transfer to your own account",
  deposit_not_found: 'Deposit not found',
  idempotency_key_conflict: 'Something went wrong, please start a new transfer',
  invalid_iban: "Check the recipient's IBAN — invalid format or check digits",
  invalid_amount: 'Enter an amount greater than zero',
  invalid_cursor: 'Could not load more — please refresh and try again',
  missing_idempotency_key_header: 'Something went wrong, please try again',
  unsupported_bank: 'Transfers are only supported within this bank',
  recipient_account_closed: "The recipient's account is closed",
  recipient_not_found: 'No recipient found with this IBAN',
  sender_account_not_active: "Your account can't send transfers right now",
  too_many_resolve_attempts: 'Too many attempts, please try again later',

  // ledger failure_reason (transfer status "failed")
  insufficient_funds: 'Insufficient funds',
  ledger_internal_error: 'A temporary error occurred, please try again later',

  // gateway
  missing_bearer_token: 'Please log in to continue',
  invalid_token: 'Your session has expired, please log in again',
}

// Never shows the raw code as a fallback — a code is a stable identifier
// for this lookup, not something a user should ever see verbatim.
export function errorMessage(code: string, fallback = 'Something went wrong, please try again'): string {
  return ERROR_MESSAGES[code] ?? fallback
}

// Lowercase clause fragments for the "This transfer was blocked... Reason:
// {clause}." template on a rejected transfer — a deliberately different
// shape from ERROR_MESSAGES above (a fraud rejection reason is never
// shown as its own sentence, only appended to one), so it's its own table
// rather than mixed into the same one.
export const REJECTED_REASON_CLAUSES: Record<string, string> = {
  amount_threshold: 'the amount exceeds the limit for a single transfer',
  velocity_count: 'too many transfers in a short time',
  velocity_sum: 'the total amount transferred in a short time is too high',
}
