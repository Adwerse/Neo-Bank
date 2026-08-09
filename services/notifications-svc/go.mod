module neobank/services/notifications-svc

go 1.25.0

require (
	github.com/golang-migrate/migrate/v4 v4.18.2
	github.com/jackc/pgx/v5 v5.10.0
	github.com/segmentio/kafka-go v0.4.51
	google.golang.org/protobuf v1.36.11
	neobank/pkg/pgha v0.0.0
	neobank/proto/gen/go v0.0.0
)

require (
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.15.11 // indirect
	github.com/lib/pq v1.10.9 // indirect
	github.com/pierrec/lz4/v4 v4.1.16 // indirect
	go.uber.org/atomic v1.7.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.40.0 // indirect
)

replace neobank/pkg/health => ../../pkg/health

replace neobank/proto/gen/go => ../../proto/gen/go

replace neobank/pkg/pgha => ../../pkg/pgha
