// A small fixed palette rather than a computed hue — deliberately
// theme-invariant (same reasoning as Tag's chips, see Tag.module.css):
// an identity color shouldn't shift when the viewport crosses the
// desktop/mobile theme breakpoint. Every entry is picked to stay legible
// under a white initial.
const PALETTE = [
  '#e05d5d',
  '#e08a3c',
  '#c99a2e',
  '#5aab5a',
  '#3f9e8f',
  '#4a90c4',
  '#6c6cd9',
  '#a15ac9',
  '#c15a95',
  '#7a7a86',
]

// djb2 — cheap and stable; the only property that matters here is
// determinism (same seed always maps to the same palette index, forever,
// independent of session or render order), not cryptographic strength.
function hashString(s: string): number {
  let hash = 5381
  for (let i = 0; i < s.length; i++) {
    hash = (hash * 33) ^ s.charCodeAt(i)
  }
  return hash >>> 0
}

// seed should be something stable per-account for the lifetime of the
// account — user_id, not email (which can change) or display name
// (which the user can edit at will and shouldn't see their color change
// as a side effect of).
export function avatarColor(seed: string): string {
  return PALETTE[hashString(seed) % PALETTE.length]
}
