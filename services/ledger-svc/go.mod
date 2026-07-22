module neobank/services/ledger-svc

go 1.25.0

require (
	github.com/golang-migrate/migrate/v4 v4.18.2
	github.com/jackc/pgx/v5 v5.10.0
	google.golang.org/grpc v1.75.0
	google.golang.org/protobuf v1.36.11
	neobank/proto/gen/go v0.0.0
)

require (
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/lib/pq v1.10.9 // indirect
	go.uber.org/atomic v1.7.0 // indirect
	golang.org/x/net v0.41.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.33.0 // indirect
	golang.org/x/text v0.29.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250707201910-8d1bb00bc6a7 // indirect
)

replace neobank/proto/gen/go => ../../proto/gen/go
