// Carries the real HTTP status and parsed body so callers can distinguish
// 401 from 409 from 503 instead of catching a single generic failure —
// the backend already returns meaningful codes; collapsing them here would
// throw that away.
export class ApiError extends Error {
  readonly status: number
  readonly body: unknown

  constructor(status: number, body: unknown, message?: string) {
    super(message ?? `Request failed with status ${status}`)
    this.name = 'ApiError'
    this.status = status
    this.body = body
  }
}

export function isApiError(err: unknown): err is ApiError {
  return err instanceof ApiError
}
