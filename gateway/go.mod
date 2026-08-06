module neobank/gateway

go 1.23.0

require (
	neobank/pkg/health v0.0.0
	neobank/proto/gen/go v0.0.0
)

require github.com/golang-jwt/jwt/v5 v5.3.1

require (
	github.com/coder/websocket v1.8.15
	github.com/segmentio/kafka-go v0.4.51
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/klauspost/compress v1.15.9 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
)

replace neobank/pkg/health => ../pkg/health

replace neobank/proto/gen/go => ../proto/gen/go
