# proto

Shared gRPC contracts for the neobank services.

## Convention
- One `.proto` file per service's public gRPC interface (e.g. `auth/v1/auth.proto` for auth-svc).
- A file is added in the sprint where the service actually needs a gRPC contract — not ahead of time.

## Code generation
Generated Go code lives in `proto/gen/go` — never hand-edited, regenerated via:
```
buf generate
```
Requires locally: `buf`, `protoc-gen-go`, `protoc-gen-go-grpc`.
