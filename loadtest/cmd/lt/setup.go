package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

const (
	// genesisAccountID / externalWorldAccountID are the same two sentinel
	// accounts ledger-svc's cmd/seed and cmd/devtopup already use. They are
	// re-declared rather than imported because those live in package main
	// of a different module — the same duplication devtopup itself
	// documents.
	genesisAccountID       = "00000000-0000-0000-0000-000000000001"
	externalWorldAccountID = "00000000-0000-0000-0000-000000000000"

	// genesisMintChunk matches devtopup's: mint far more than one run
	// needs, so minting is a rare event rather than a per-account write
	// that would itself serialize on the genesis row.
	genesisMintChunk int64 = 1_000_000_000_000

	// loadTestPassword is fixed and public on purpose. These are throwaway
	// @loadtest.local users on a dev stack; a random password would only
	// mean it had to be persisted somewhere anyway.
	loadTestPassword = "loadtest-password"

	// setupConcurrency bounds how many users are provisioned at once.
	// Registration hashes a password with argon2id at 19 MiB per call
	// (auth-svc's parameters), so an unbounded fan-out here would put the
	// stack under a memory spike before the actual measurement even
	// starts.
	setupConcurrency = 8
)

// verificationCodePattern matches the body auth-svc's sendVerificationEmail
// writes ("Your verification code is: 123456"). Coupled to that format on
// purpose and cheaply: if the wording changes, this fails loudly at setup
// rather than producing a subtly wrong fixture.
var verificationCodePattern = regexp.MustCompile(`code is:\s*([0-9]{4,10})`)

func runSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	users := fs.Int("users", 40, "number of load-test users to provision")
	fund := fs.Int64("fund", 100_000_000, "minor units each account is topped up to")
	gateway := fs.String("gateway", "http://localhost:8080", "gateway base URL as seen from this machine")
	gatewayForK6 := fs.String("gateway-in-network", "http://gateway:8080", "gateway base URL as seen from the k6 container; recorded in fixtures.json")
	mailpit := fs.String("mailpit", "http://localhost:8025", "Mailpit base URL, used to read verification codes")
	ledgerAddr := fs.String("ledger", "localhost:8083", "ledger-svc gRPC address, used for funding")
	databaseURL := fs.String("database-url", envOr("DATABASE_URL", defaultDatabaseURL), "Postgres URL of the current leader")
	prefix := fs.String("prefix", "lt", "run prefix; namespaces user emails and, via k6, idempotency keys")
	out := fs.String("out", "loadtest/fixtures/fixtures.json", "where to write fixtures.json")
	refresh := fs.Bool("refresh", false, "reissue access tokens for the users already in -out and exit; no registration, no funding")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *users < 2 {
		return fmt.Errorf("-users must be at least 2 (a transfer needs two distinct accounts)")
	}

	client := &http.Client{Timeout: 30 * time.Second}

	if *refresh {
		return refreshTokens(ctx, client, *gateway, *out)
	}

	// Provisioning goes through the real public API — register, verify,
	// login — rather than writing users and accounts straight into
	// Postgres. That costs a Mailpit round trip per user and a wait for
	// the asynchronous account-creation pipeline, and it buys the one
	// thing hand-written rows cannot: the fixture accounts are, byte for
	// byte, what the system produces for a real signup. A load test whose
	// fixtures differ from production data is testing a different system.
	log.Printf("setup: provisioning %d users via %s", *users, *gateway)
	provisioned := make([]FixtureUser, *users)
	errs := make([]error, *users)
	var wg sync.WaitGroup
	sem := make(chan struct{}, setupConcurrency)
	for i := range *users {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			email := fmt.Sprintf("%s-user-%03d@loadtest.local", *prefix, i)
			u, err := provisionUser(ctx, client, *gateway, *mailpit, email)
			if err != nil {
				errs[i] = fmt.Errorf("user %s: %w", email, err)
				return
			}
			provisioned[i] = u
		}(i)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	log.Printf("setup: %d users ready", len(provisioned))

	if err := fundAccounts(ctx, *databaseURL, *ledgerAddr, provisioned, *fund); err != nil {
		return err
	}

	f := Fixtures{
		RunPrefix:        *prefix,
		GatewayURL:       *gatewayForK6,
		CreatedAt:        time.Now().UTC(),
		FundedPerAccount: *fund,
		Users:            provisioned,
	}
	if err := writeFixtures(*out, f); err != nil {
		return err
	}
	log.Printf("setup: wrote %s (%d users, %d minor units each)", *out, len(provisioned), *fund)
	return nil
}

// refreshTokens logs every user in the existing fixtures back in and
// rewrites the file with fresh access tokens.
//
// This exists because of a failure that cost a whole profile's worth of
// results. auth-svc's access tokens live 15 minutes (accessTokenTTL); a
// full three-profile sweep at four VU levels each takes longer than that.
// The tokens therefore expire MID-SWEEP, and what that looks like is not
// an error anyone would recognise: the gateway starts answering 401 in
// under half a millisecond, k6 cheerfully reports 2700 requests/second —
// twenty times the real throughput — and the run looks like a triumph
// until you notice nothing settled. (This is exactly what happened: the
// duplicates profile posted 164k requests, of which 151k were 401s.)
//
// Reissuing is cheap (one login per user, no registration, no account
// polling, no funding), so run.sh calls this before every single VU level
// rather than trying to reason about how much TTL is left.
func refreshTokens(ctx context.Context, client *http.Client, gateway, path string) error {
	fixtures, err := readFixtures(path)
	if err != nil {
		return err
	}

	errs := make([]error, len(fixtures.Users))
	var wg sync.WaitGroup
	sem := make(chan struct{}, setupConcurrency)
	for i := range fixtures.Users {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			status, body, err := postJSON(ctx, client, gateway+"/auth/login", "", map[string]string{
				"email": fixtures.Users[i].Email, "password": loadTestPassword,
			})
			if err != nil {
				errs[i] = fmt.Errorf("login %s: %w", fixtures.Users[i].Email, err)
				return
			}
			if status != http.StatusOK {
				errs[i] = fmt.Errorf("login %s: status %d: %s", fixtures.Users[i].Email, status, body)
				return
			}
			var tokens struct {
				AccessToken string `json:"access_token"`
			}
			if err := json.Unmarshal(body, &tokens); err != nil {
				errs[i] = fmt.Errorf("login %s: parse token pair: %w", fixtures.Users[i].Email, err)
				return
			}
			fixtures.Users[i].AccessToken = tokens.AccessToken
		}(i)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}

	if err := writeFixtures(path, fixtures); err != nil {
		return err
	}
	log.Printf("setup: reissued access tokens for %d users", len(fixtures.Users))
	return nil
}

// provisionUser drives one account from nothing to "can make a transfer":
// register, read the emailed code out of Mailpit, verify, log in, then wait
// for the account to appear.
//
// That last wait is not incidental — it is the asynchronous seam this
// system is built around. verify-email only writes a UserActivated event to
// auth-svc's outbox; the relay publishes it to Kafka up to a second later,
// accounts-svc consumes it and creates the account, then publishes
// AccountCreated, which ledger-svc consumes to create the ledger account.
// Nothing about "the user is verified" implies "the account exists yet", so
// setup polls instead of assuming.
func provisionUser(ctx context.Context, client *http.Client, gateway, mailpit, email string) (FixtureUser, error) {
	status, _, err := postJSON(ctx, client, gateway+"/auth/register", "", map[string]string{
		"email": email, "password": loadTestPassword,
	})
	if err != nil {
		return FixtureUser{}, fmt.Errorf("register: %w", err)
	}
	switch status {
	case http.StatusCreated:
		// A fresh registration: the code is in Mailpit, verify it.
		code, err := waitForVerificationCode(ctx, client, mailpit, email)
		if err != nil {
			return FixtureUser{}, err
		}
		vstatus, vbody, err := postJSON(ctx, client, gateway+"/auth/verify-email", "", map[string]string{
			"email": email, "code": code,
		})
		if err != nil {
			return FixtureUser{}, fmt.Errorf("verify-email: %w", err)
		}
		if vstatus != http.StatusOK {
			return FixtureUser{}, fmt.Errorf("verify-email: status %d: %s", vstatus, vbody)
		}
	case http.StatusConflict:
		// Already active from an earlier setup run. This is what makes
		// `lt setup` cheap to re-run between profiles: the users and their
		// accounts persist, only the tokens are reissued below.
	default:
		return FixtureUser{}, fmt.Errorf("register: unexpected status %d", status)
	}

	lstatus, lbody, err := postJSON(ctx, client, gateway+"/auth/login", "", map[string]string{
		"email": email, "password": loadTestPassword,
	})
	if err != nil {
		return FixtureUser{}, fmt.Errorf("login: %w", err)
	}
	if lstatus != http.StatusOK {
		return FixtureUser{}, fmt.Errorf("login: status %d: %s", lstatus, lbody)
	}
	var tokens struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(lbody, &tokens); err != nil {
		return FixtureUser{}, fmt.Errorf("login: parse token pair: %w", err)
	}
	if tokens.AccessToken == "" {
		return FixtureUser{}, errors.New("login: empty access_token")
	}

	account, err := waitForAccount(ctx, client, gateway, tokens.AccessToken)
	if err != nil {
		return FixtureUser{}, err
	}

	return FixtureUser{
		Email:         email,
		UserID:        account.UserID,
		AccountID:     account.ID,
		AccountNumber: account.AccountNumber,
		AccessToken:   tokens.AccessToken,
	}, nil
}

type meResponse struct {
	ID            string `json:"id"`
	UserID        string `json:"user_id"`
	AccountNumber string `json:"account_number"`
	Status        string `json:"status"`
	Balance       int64  `json:"balance"`
}

// waitForAccount polls GET /accounts/me until the Kafka pipeline described
// in provisionUser has caught up. 90 seconds is generous for a local stack
// (the observed time is a few seconds) and is there for the one case that
// genuinely takes longer: the very first run after `docker compose up`,
// where accounts-svc may still be joining its consumer group.
func waitForAccount(ctx context.Context, client *http.Client, gateway, token string) (meResponse, error) {
	deadline := time.Now().Add(90 * time.Second)
	var lastStatus int
	for time.Now().Before(deadline) {
		status, body, err := getJSON(ctx, client, gateway+"/accounts/me", token)
		if err != nil {
			return meResponse{}, fmt.Errorf("GET /accounts/me: %w", err)
		}
		lastStatus = status
		if status == http.StatusOK {
			var me meResponse
			if err := json.Unmarshal(body, &me); err != nil {
				return meResponse{}, fmt.Errorf("GET /accounts/me: parse: %w", err)
			}
			if me.ID != "" {
				return me, nil
			}
		}
		select {
		case <-ctx.Done():
			return meResponse{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return meResponse{}, fmt.Errorf("account never appeared (last GET /accounts/me status %d) — is accounts-svc consuming user.events?", lastStatus)
}

// waitForVerificationCode reads the code out of the email auth-svc just
// sent. It polls because SMTP delivery to Mailpit is not synchronous with
// the register response returning.
func waitForVerificationCode(ctx context.Context, client *http.Client, mailpit, email string) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		code, found, err := latestVerificationCode(ctx, client, mailpit, email)
		if err != nil {
			return "", err
		}
		if found {
			return code, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("no verification email for %s within 30s — is Mailpit reachable at %s?", email, mailpit)
}

func latestVerificationCode(ctx context.Context, client *http.Client, mailpit, email string) (string, bool, error) {
	status, body, err := getJSON(ctx, client, mailpit+"/api/v1/search?query=to%3A"+email, "")
	if err != nil {
		return "", false, fmt.Errorf("mailpit search: %w", err)
	}
	if status != http.StatusOK {
		return "", false, fmt.Errorf("mailpit search: status %d", status)
	}
	var search struct {
		Messages []struct {
			ID string `json:"ID"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &search); err != nil {
		return "", false, fmt.Errorf("mailpit search: parse: %w", err)
	}
	if len(search.Messages) == 0 {
		return "", false, nil
	}

	// Mailpit returns newest first, which is what we want: a re-registered
	// pending user gets a fresh code and only the newest one is valid.
	mstatus, mbody, err := getJSON(ctx, client, mailpit+"/api/v1/message/"+search.Messages[0].ID, "")
	if err != nil {
		return "", false, fmt.Errorf("mailpit message: %w", err)
	}
	if mstatus != http.StatusOK {
		return "", false, fmt.Errorf("mailpit message: status %d", mstatus)
	}
	var msg struct {
		Text string `json:"Text"`
	}
	if err := json.Unmarshal(mbody, &msg); err != nil {
		return "", false, fmt.Errorf("mailpit message: parse: %w", err)
	}
	m := verificationCodePattern.FindStringSubmatch(msg.Text)
	if m == nil {
		return "", false, fmt.Errorf("verification email did not match %q — did auth-svc's email wording change?", verificationCodePattern)
	}
	return m[1], true, nil
}

// fundAccounts tops every cohort account up to `target` minor units.
//
// The top-up itself goes through ledger-svc's ordinary ExecuteTransfer RPC
// — real row locks, real overdraft check, real balance-cache update —
// exactly as cmd/devtopup does, so the fixture balances are produced by the
// same code path the load test will then hammer. Only the one thing that
// genuinely has no API — issuing new money into genesis — is a direct
// database write.
//
// Funding transfers deliberately carry NO reference. That is what lets the
// verifier separate "money that was put here by setup" from "money that
// moved during the run": every entry the load test produces is tagged with
// its transfer's id (see transfers-svc's settleTransfer), and every entry
// setup produces is not.
func fundAccounts(ctx context.Context, databaseURL, ledgerAddr string, users []FixtureUser, target int64) error {
	conn, err := grpc.NewClient(ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("create ledger gRPC client for %s: %w", ledgerAddr, err)
	}
	defer conn.Close()
	ledger := ledgerv1.NewLedgerServiceClient(conn)

	db, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer db.Close(ctx)

	if _, err := ledger.CreateLedgerAccount(ctx, &ledgerv1.CreateLedgerAccountRequest{AccountId: genesisAccountID}); err != nil {
		return fmt.Errorf("ensure genesis ledger account: %w", err)
	}

	// One pass to work out the total shortfall, so genesis is minted into
	// once rather than once per account.
	shortfalls := make([]int64, len(users))
	var total int64
	for i, u := range users {
		bal, err := ledger.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: u.AccountID})
		if err != nil {
			return fmt.Errorf("GetBalance(%s): %w", u.AccountID, err)
		}
		if missing := target - bal.GetBalance(); missing > 0 {
			shortfalls[i] = missing
			total += missing
		}
	}
	if total == 0 {
		log.Printf("setup: all %d accounts already hold at least %d — no funding needed", len(users), target)
		return nil
	}
	if err := ensureGenesisFunded(ctx, db, total); err != nil {
		return err
	}

	funded := 0
	for i, u := range users {
		if shortfalls[i] == 0 {
			continue
		}
		if _, err := ledger.ExecuteTransfer(ctx, &ledgerv1.ExecuteTransferRequest{
			FromAccountId: genesisAccountID,
			ToAccountId:   u.AccountID,
			Amount:        shortfalls[i],
		}); err != nil {
			return fmt.Errorf("fund %s: %w", u.AccountID, err)
		}
		funded++
	}
	log.Printf("setup: funded %d accounts to %d minor units (%d total issued)", funded, target, total)
	return nil
}

// ensureGenesisFunded mints into genesis when its spendable balance — the
// SUM over its entries, the same quantity ExecuteTransfer's overdraft check
// reads — cannot cover `amount`. Issuance cannot go through ExecuteTransfer
// by definition (it is the source account going negative), so it is a
// direct balanced write against the external-world account, keeping
// SUM(entries) = 0 intact. Lifted from cmd/devtopup's function of the same
// name; see its doc comment for the full reasoning.
func ensureGenesisFunded(ctx context.Context, conn *pgx.Conn, amount int64) error {
	genesisLedgerID, err := ensureLedgerAccountRow(ctx, conn, genesisAccountID)
	if err != nil {
		return err
	}
	var spendable int64
	if err := conn.QueryRow(ctx,
		"SELECT COALESCE(SUM(amount), 0)::bigint FROM entries WHERE ledger_account_id = $1",
		genesisLedgerID,
	).Scan(&spendable); err != nil {
		return fmt.Errorf("read genesis spendable balance: %w", err)
	}
	if spendable >= amount {
		return nil
	}

	externalLedgerID, err := ensureLedgerAccountRow(ctx, conn, externalWorldAccountID)
	if err != nil {
		return err
	}

	mint := amount - spendable
	if mint < genesisMintChunk {
		mint = genesisMintChunk
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin mint transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`WITH txn AS (SELECT gen_random_uuid() AS id)
		 INSERT INTO entries (transaction_id, ledger_account_id, amount)
		 SELECT txn.id, $1::uuid, $2::bigint FROM txn
		 UNION ALL
		 SELECT txn.id, $3::uuid, $4::bigint FROM txn`,
		genesisLedgerID, mint, externalLedgerID, -mint,
	); err != nil {
		return fmt.Errorf("insert mint entries: %w", err)
	}
	for _, id := range []string{genesisLedgerID, externalLedgerID} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO account_balances (ledger_account_id, balance, updated_at)
			 VALUES ($1, (SELECT COALESCE(SUM(amount), 0)::bigint FROM entries WHERE ledger_account_id = $1), now())
			 ON CONFLICT (ledger_account_id)
			 DO UPDATE SET balance = EXCLUDED.balance, updated_at = EXCLUDED.updated_at`,
			id,
		); err != nil {
			return fmt.Errorf("recompute cached balance for %s: %w", id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit mint transaction: %w", err)
	}
	log.Printf("setup: minted %d into genesis (was %d, needed %d)", mint, spendable, amount)
	return nil
}

func ensureLedgerAccountRow(ctx context.Context, conn *pgx.Conn, accountID string) (string, error) {
	var ledgerAccountID string
	err := conn.QueryRow(ctx,
		`INSERT INTO ledger_accounts (account_id)
		 VALUES ($1)
		 ON CONFLICT (account_id) DO UPDATE SET account_id = EXCLUDED.account_id
		 RETURNING id`,
		accountID,
	).Scan(&ledgerAccountID)
	if err != nil {
		return "", fmt.Errorf("upsert ledger_accounts(account_id=%s): %w", accountID, err)
	}
	return ledgerAccountID, nil
}

func postJSON(ctx context.Context, client *http.Client, url, token string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return do(client, req)
}

func getJSON(ctx context.Context, client *http.Client, url, token string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return do(client, req)
}

func do(client *http.Client, req *http.Request) (int, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}
