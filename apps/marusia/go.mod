module capsulad/apps/marusia

go 1.22.5

require (
	capsulad/sdk v0.0.0
	google.golang.org/grpc v1.67.1
	google.golang.org/protobuf v1.35.1
)

replace capsulad/sdk => ../../sdk/go

require (
	golang.org/x/net v0.28.0 // indirect
	golang.org/x/sys v0.27.0 // indirect
	golang.org/x/text v0.18.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240814211410-ddb44dafa142 // indirect
)
