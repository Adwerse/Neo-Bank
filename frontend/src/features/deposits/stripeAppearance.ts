import type { Appearance } from '@stripe/stripe-js'

// Stripe Elements' `appearance` only accepts literal values (colors,
// pixel sizes) — it renders inside a cross-origin iframe that can't read
// this app's CSS custom properties, so the two palettes below are a
// literal snapshot of shared/ui/tokens.css's dark ('nocturne-dark') and
// light ('nocturne-light') themes. Keep in sync by hand if tokens.css's
// --color-accent/-bg/-surface/-text/-danger or radii change.
const DARK: Appearance = {
  theme: 'night',
  variables: {
    colorPrimary: '#9184d9',
    colorBackground: '#232532',
    colorText: '#e9e9ed',
    colorTextSecondary: '#b7b7bd',
    colorDanger: '#f87171',
    colorTextPlaceholder: '#75798c',
    fontFamily: "'Inter', system-ui, 'Segoe UI', Roboto, sans-serif",
    borderRadius: '8px',
    spacingUnit: '4px',
  },
  rules: {
    '.Input': {
      border: '1px solid color-mix(in srgb, #e9e9ed 12%, transparent)',
      boxShadow: 'none',
    },
    '.Input:focus': {
      border: '1px solid #9184d9',
      boxShadow: '0 0 0 1px #9184d9',
    },
    '.Tab': {
      border: '1px solid color-mix(in srgb, #e9e9ed 12%, transparent)',
    },
    '.Tab--selected': {
      border: '1px solid #9184d9',
    },
  },
}

const LIGHT: Appearance = {
  theme: 'stripe',
  variables: {
    colorPrimary: '#5d5294',
    colorBackground: '#f3f5fe',
    colorText: '#292b31',
    colorTextSecondary: '#595d6c',
    colorDanger: '#b91c1c',
    colorTextPlaceholder: '#9397ab',
    fontFamily: "'Inter', system-ui, 'Segoe UI', Roboto, sans-serif",
    borderRadius: '8px',
    spacingUnit: '4px',
  },
  rules: {
    '.Input': {
      border: '1px solid color-mix(in srgb, #292b31 12%, transparent)',
      boxShadow: 'none',
    },
    '.Input:focus': {
      border: '1px solid #5d5294',
      boxShadow: '0 0 0 1px #5d5294',
    },
  },
}

// Layout.tsx sets document.documentElement.dataset.theme to
// 'nocturne-light' on desktop widths and 'nocturne-dark' (the default
// ground, tokens.css's un-suffixed :root block) everywhere else — same
// switch this mirrors.
export function getStripeAppearance(isDesktop: boolean): Appearance {
  return isDesktop ? LIGHT : DARK
}
