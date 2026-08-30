import { loadStripe } from '@stripe/stripe-js'

// loadStripe must be called exactly once at module scope (Stripe's own
// documented convention) — calling it per-render would reinitialize
// Stripe.js and remount every Elements-connected component unnecessarily.
// The key is public by nature (Stripe.js ships it to the browser to
// tokenize card details before they ever reach this app's own code) but
// still comes from a Vite env var, never hardcoded — see .env.example.
const publishableKey = import.meta.env.VITE_STRIPE_PUBLISHABLE_KEY as string | undefined

if (!publishableKey) {
  // Fails loudly at startup rather than letting Stripe.js reject a
  // confusingly-undefined key deep inside a payment attempt.
  console.error('VITE_STRIPE_PUBLISHABLE_KEY is not set — see frontend/.env.example')
}

// locale: 'en' matches the rest of this app's UI language for
// PaymentElement's own labels/placeholders and Stripe's built-in
// validation messages.
export const stripePromise = loadStripe(publishableKey ?? '', { locale: 'en' })
