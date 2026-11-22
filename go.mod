module github.com/rquanx/minato-runtime

go 1.24.0

toolchain go1.24.13

require (
	github.com/aws/aws-sdk-go-v2 v1.41.1
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.5
	github.com/coder/websocket v1.8.14
	github.com/lib/pq v1.10.9
	go.yaml.in/yaml/v3 v3.0.4
	golang.org/x/crypto v0.45.0
)

require github.com/aws/smithy-go v1.24.1 // indirect
