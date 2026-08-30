import { isApiError } from '../../shared/api-client/ApiError'

// Never fall back to showing a 0 balance here — an honest "unavailable"
// beats a fake number in a banking product.
export function getAccountErrorMessage(error: unknown): string {
  if (isApiError(error) && error.status === 503) {
    return 'Balance temporarily unavailable. Please try again in a minute.'
  }
  if (isApiError(error) && error.status === 404) {
    return "Your account is still being created — this usually takes a few seconds. Please refresh."
  }
  return 'Could not load your account, please try again.'
}
