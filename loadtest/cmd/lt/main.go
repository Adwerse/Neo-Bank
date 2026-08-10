// Command lt is the load-testing sidecar for the Neo-Bank stack: everything
// a k6 run needs around it that k6 itself is the wrong tool for.
//
// k6 generates load and measures latency. It cannot provision fixtures, it
// cannot look inside Postgres while the run is in flight, and — most
// importantly — it cannot answer the only question that makes a load test
// meaningful for a ledger: after all that traffic, do the books still
// balance? A load test that reports 200 RPS and never checks SUM(entries)
// has measured a number, not a system. Hence four subcommands:
//
//	setup   — provision N verified users with funded accounts, write
//	          fixtures.json for the k6 scripts to read
//	fraud   — raise (and restore) fraud-svc's velocity thresholds, see
//	          fraud.go for why this is necessary and what it does NOT change
//	probe   — sample Postgres's own view of itself once a second during a
//	          run: connections, lock waits, outbox backlog, replica lag
//	verify  — the invariant suite; the reason this whole directory exists
//	report  — collate k6 summaries + probe CSVs into one Markdown table
//
// Everything here is dev-only tooling. It talks to the local stack over
// published ports and, for the parts with no public API (funding an
// account, reading fraud_rules), directly to Postgres — the same latitude
// ledger-svc's own cmd/seed and cmd/devtopup already take, and for the same
// reason: there is deliberately no production endpoint that mints money.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = `lt — Neo-Bank load-test sidecar

usage: lt <command> [flags]

commands:
  setup    provision users/accounts/funds and write fixtures.json
  fraud    raise fraud thresholds for a run (-mode loadtest) or restore them (-mode restore)
  probe    sample Postgres state during a run and write a CSV
  verify   check ledger invariants after a run
  report   collate k6 summaries and probe CSVs into Markdown

run "lt <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	// Ctrl-C cancels the context rather than killing the process outright.
	// It matters for `probe` (which must still flush its CSV) and for
	// `setup` (which is interruptible mid-provisioning and safe to re-run).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "setup":
		err = runSetup(ctx, os.Args[2:])
	case "fraud":
		err = runFraud(ctx, os.Args[2:])
	case "probe":
		err = runProbe(ctx, os.Args[2:])
	case "verify":
		err = runVerify(ctx, os.Args[2:])
	case "report":
		err = runReport(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "lt: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "lt %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

// defaultDatabaseURL points at HAProxy's leader port, not at a specific
// pg-node: which node is leader changes at runtime (see infra/patroni), and
// every write these tools make has to land on whoever holds the role right
// now. Same address every service's DATABASE_URL resolves to.
const defaultDatabaseURL = "postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable"

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
