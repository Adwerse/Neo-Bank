const BASE_DELAY_MS = 500
const MAX_DELAY_MS = 30_000

// Full jitter (as opposed to "exponential ± small noise"): pick uniformly
// from [0, cap] rather than clustering around the exponential value
// itself. If every client backed off to the same ~cap delay after a
// Gateway restart, they'd all reconnect in the same instant anyway — just
// a few seconds later than a naive retry loop, but still a synchronized
// spike hitting a service that just came back up. Spreading uniformly
// across the whole window is what actually smooths that out.
export function nextReconnectDelayMs(attempt: number): number {
  const cap = Math.min(MAX_DELAY_MS, BASE_DELAY_MS * 2 ** attempt)
  return Math.random() * cap
}
