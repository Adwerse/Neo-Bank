module neobank/infra/failover

go 1.25.0

require (
	github.com/jackc/pgx/v5 v5.10.0
	google.golang.org/grpc v1.75.0
	neobank/pkg/pgha v0.0.0
	neobank/proto/gen/go v0.0.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/net v0.41.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250707201910-8d1bb00bc6a7 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace neobank/proto/gen/go => ../../proto/gen/go

replace neobank/pkg/pgha => ../../pkg/pgha
