module neobank/loadtest

go 1.25.0

require (
	github.com/jackc/pgx/v5 v5.10.0
	google.golang.org/grpc v1.83.0
	neobank/proto/gen/go v0.0.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace neobank/proto/gen/go => ../proto/gen/go
