// Shared machinery for the three load profiles. Everything that is the
// same regardless of who is sending money to whom lives here: fixtures,
// the request itself, how a response is classified, and what gets written
// out at the end.
//
// The three profiles differ in exactly one thing — which sender/recipient
// pair each iteration picks — and that is the point. Same VU ladder, same
// amount, same request, same measurement; only the shape of the contention
// changes. Anything else that differed between them would be a confound.

import http from 'k6/http';
import { Counter, Rate, Trend } from 'k6/metrics';

// Fixtures are read once per VU at init time. open() is init-context only
// in k6, which is exactly the constraint you want here: the file is read
// off disk before the clock starts, never during a measured iteration.
const FIXTURES = JSON.parse(open(__ENV.FIXTURES || '/fixtures/fixtures.json'));

export const users = FIXTURES.users;
export const gateway = __ENV.GATEWAY || FIXTURES.gateway_url;
export const runPrefix = FIXTURES.run_prefix;

// AMOUNT is small on purpose, and it is a correctness parameter rather
// than a cosmetic one. Each account is funded with FundedPerAccount minor
// units; if a profile can drain an account within the run, the tail of
// that run measures how fast ledger-svc can reject overdrafts instead of
// how fast it can post transfers. 100 minor units against a default
// funding of 100_000_000 leaves a million transfers of headroom per
// account, which no local run comes close to.
export const AMOUNT = Number(__ENV.AMOUNT || 100);

// RUN_ID namespaces idempotency keys per invocation. Without it, the
// second run at a higher VU level would reuse the first run's keys and
// every request would take the replay path — a very fast, completely
// meaningless result. The runner passes a distinct value per (profile,
// VU level) invocation.
export const runId = __ENV.RUN_ID || `${Date.now()}`;

export const PROFILE = __ENV.PROFILE || 'unnamed';
export const VUS = Number(__ENV.VUS || 10);
export const DURATION = __ENV.DURATION || '60s';

// One counter per outcome, because "error rate" alone would flatten
// distinctions that matter enormously here. A fraud rejection, an
// idempotent replay and a 500 are all "not a completed transfer" and are
// three entirely different pieces of news:
//
//   completed          — the money moved; the outcome being measured
//   rejected           — fraud-svc said no; the system working as designed
//   failed             — ledger-svc said no (overdraft, missing account)
//   replayed           — a duplicate key hit an existing transfer (HTTP 200)
//   uncertain          — 202: fraud-svc or ledger-svc did not answer, the
//                        transfer is parked pending for reconciliation.
//                        Under load this is the interesting degradation.
//   unauthorized       — 401: the fixture tokens expired mid-run. Its own
//                        bucket rather than part of client_error because
//                        it invalidates the entire result and has to be
//                        impossible to miss. See below.
//   client_error       — 4xx other than the above; a bug in the script or
//                        a genuine validation rejection
//   server_error       — 5xx; the only unambiguous "the system broke"
//   transport_error    — no response at all: timeout, refused, reset
export const outcomes = {
  completed: new Counter('outcome_completed'),
  rejected: new Counter('outcome_rejected'),
  failed: new Counter('outcome_failed'),
  replayed: new Counter('outcome_replayed'),
  uncertain: new Counter('outcome_uncertain'),
  unauthorized: new Counter('outcome_unauthorized'),
  client_error: new Counter('outcome_client_error'),
  server_error: new Counter('outcome_server_error'),
  transport_error: new Counter('outcome_transport_error'),
};

// errorRate counts only the outcomes that mean the system failed to
// answer properly — 5xx and no-response. A fraud rejection is not an
// error, and folding it into an error rate is how a load test ends up
// reporting a 90% failure rate for a system that is behaving perfectly.
export const errorRate = new Rate('request_errors');

// transferDuration duplicates http_req_duration on purpose: it is scoped
// to the POST /transfers call alone, so it stays comparable across
// profiles even if a profile ever adds a second request per iteration.
export const transferDuration = new Trend('transfer_duration', true);

// settledDuration is the latency of only those requests that actually
// moved money. It is the number the hot-account profile lives or dies by:
// when a run is mostly replays or rejections, the overall p99 gets
// cheerfully fast for the worst possible reason.
export const settledDuration = new Trend('settled_duration', true);

// The trailing slash on '/transfers/' is load-bearing, exactly as it is in
// the frontend's api.ts. The gateway mounts this route as a subtree pattern
// ("/transfers/"), so Go's ServeMux answers a bare "/transfers" with a 301
// to the slashed form — and a client that follows redirects turns the POST
// into a GET along the way. The failure is silent and spectacular: every
// request returns 200 with a history page, k6 reports thousands of
// successful "transfers" per second, and not one unit of money has moved.
// This cost an hour the first time; it is written down so it costs nobody
// else one.
export function transfer(sender, recipientAccountNumber, idempotencyKey, amount = AMOUNT) {
  const res = http.post(
    `${gateway}/transfers/`,
    JSON.stringify({ recipient_account_number: recipientAccountNumber, amount }),
    {
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${sender.access_token}`,
        'Idempotency-Key': idempotencyKey,
      },
      // Named so k6 aggregates every transfer under one URL instead of
      // one per unique path — irrelevant here (the path is constant) but
      // it also becomes the tag used in the summary.
      tags: { name: 'POST /transfers' },
      timeout: '30s',
      // Never follow a redirect. See the trailing-slash note above: a
      // followed redirect is the one failure mode here that looks like a
      // perfect result. With this set, a routing change that reintroduces
      // the 301 shows up immediately as a wall of client_error instead.
      redirects: 0,
    },
  );

  const outcome = classify(res);
  outcomes[outcome].add(1);
  errorRate.add(
    outcome === 'server_error' || outcome === 'transport_error' || outcome === 'unauthorized',
  );
  transferDuration.add(res.timings.duration);
  if (outcome === 'completed') {
    settledDuration.add(res.timings.duration);
  }
  return { res, outcome };
}

// classify maps an HTTP response onto one of the outcome buckets above.
//
// The status code alone is not enough, and that is a deliberate property
// of transfers-svc's API rather than an inconsistency: a fraud-rejected
// transfer returns 201 because the transfer resource really was created,
// with the actual news in the body's status field (see
// createTransferHandler). A classifier that only looked at status codes
// would count every rejection as a success.
function classify(res) {
  if (res.status === 0) return 'transport_error';
  if (res.status >= 500) return 'server_error';
  // 401 gets its own bucket because of how it fails: the gateway rejects
  // an expired token in well under a millisecond, so a run whose fixtures
  // went stale does not slow down or error in any familiar way — it gets
  // twenty times FASTER and reports a flawless zero error rate. Lumping
  // it in with client_error once turned a dead run into a plausible-
  // looking result. See refreshTokens in loadtest/cmd/lt/setup.go.
  if (res.status === 401) return 'unauthorized';
  if (res.status === 202) return 'uncertain';
  if (res.status === 200) return 'replayed';
  if (res.status === 201) {
    let body;
    try {
      body = res.json();
    } catch (e) {
      return 'server_error';
    }
    if (body.status === 'completed') return 'completed';
    if (body.status === 'rejected') return 'rejected';
    if (body.status === 'failed') return 'failed';
    // 'pending' with a 201 should not happen — every path that leaves a
    // transfer pending returns 202 — so treat it as a contract surprise
    // rather than silently bucketing it as a success.
    return 'server_error';
  }
  return 'client_error';
}

// pickTwo returns two distinct indices into users, uniformly at random.
// The "shift if it collides" trick keeps the distribution uniform over
// ordered pairs without a retry loop, which matters because a retry loop
// inside a measured iteration adds its own (tiny, but unnecessary) cost.
export function pickTwo(n) {
  const a = Math.floor(Math.random() * n);
  let b = Math.floor(Math.random() * (n - 1));
  if (b >= a) b += 1;
  return [a, b];
}

// handleSummary writes a small, stable JSON next to k6's own text output
// rather than dumping k6's full summary object.
//
// The full dump is large, its shape moves between k6 versions, and 95% of
// it is metrics no one reads. A fixed set of fields is what `lt report`
// consumes and what ends up in the README table, so pinning it here means
// a k6 upgrade cannot silently change the report.
export function handleSummary(data) {
  const m = data.metrics;
  const durationS = data.state.testRunDurationMs / 1000;
  const count = (name) => (m[name] ? m[name].values.count : 0);
  const trend = (name) =>
    m[name]
      ? {
          avg: round(m[name].values.avg),
          p50: round(m[name].values.med),
          p90: round(m[name].values['p(90)']),
          p95: round(m[name].values['p(95)']),
          p99: round(m[name].values['p(99)']),
          max: round(m[name].values.max),
        }
      : null;

  const summary = {
    profile: PROFILE,
    vus: VUS,
    duration_s: round(durationS),
    iterations: count('iterations'),
    http_reqs: count('http_reqs'),
    rps: round(count('http_reqs') / durationS),
    // Completed transfers per second — the only throughput number that
    // means "money moved this fast". For the hot-account profile it
    // diverges sharply from rps, which is the finding.
    settled_per_s: round(count('outcome_completed') / durationS),
    latency_ms: trend('transfer_duration'),
    settled_latency_ms: trend('settled_duration'),
    outcomes: Object.fromEntries(
      Object.keys(outcomes).map((k) => [k, count(`outcome_${k}`)]),
    ),
    error_rate: m.request_errors ? round(m.request_errors.values.rate, 4) : 0,
  };

  const out = {};
  out[`/results/${PROFILE}-vus${VUS}.summary.json`] = JSON.stringify(summary, null, 2);
  out.stdout = renderText(summary);
  return out;
}

function round(v, digits = 2) {
  if (v === undefined || v === null || Number.isNaN(v)) return null;
  const f = Math.pow(10, digits);
  return Math.round(v * f) / f;
}

function renderText(s) {
  const lines = [
    '',
    `  profile ${s.profile}   VUs ${s.vus}   ${s.duration_s}s`,
    `  requests ${s.http_reqs}  (${s.rps}/s)   settled ${s.outcomes.completed} (${s.settled_per_s}/s)`,
    s.latency_ms
      ? `  latency  p50 ${s.latency_ms.p50}ms  p95 ${s.latency_ms.p95}ms  p99 ${s.latency_ms.p99}ms  max ${s.latency_ms.max}ms`
      : '  latency  (no samples)',
    `  errors   ${(s.error_rate * 100).toFixed(2)}%`,
    '  outcomes ' +
      Object.entries(s.outcomes)
        .filter(([, v]) => v > 0)
        .map(([k, v]) => `${k}=${v}`)
        .join('  '),
    '',
  ];
  if (s.outcomes.unauthorized > 0) {
    lines.push(
      `  !! ${s.outcomes.unauthorized} requests got 401 — the fixture tokens expired mid-run.`,
      '  !! THIS RESULT IS INVALID. Reissue with: go run ./loadtest/cmd/lt setup -refresh',
      '',
    );
  }
  return lines.join('\n');
}

// scenarioOptions is the shared load shape: a fixed number of VUs for a
// fixed time, one level per invocation.
//
// A closed model (constant-vus) rather than an open one (constant-arrival-
// rate) is the right choice for the question being asked. An arrival-rate
// scenario holds the request rate fixed and lets the queue grow when the
// system cannot keep up, which measures how badly it collapses past its
// limit. Fixed VUs instead hold the CONCURRENCY fixed and let throughput
// find its own level — which is what "where is the ceiling, and what
// happens to latency as we approach it" actually needs. It is also the
// only shape under which the hot-account result is legible: the plateau
// shows up as throughput refusing to rise while VUs do.
export const scenarioOptions = {
  scenarios: {
    load: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      gracefulStop: '30s',
    },
  },
  // No thresholds that abort the run. A threshold's job is to fail a
  // build; this suite's job is to find out what the numbers are, and a
  // run that aborts at 5% errors destroys the very measurement that
  // matters most.
  thresholds: {},
  summaryTrendStats: ['avg', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  discardResponseBodies: false,
};
