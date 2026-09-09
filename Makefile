.PHONY: build test fmt vet generate docker

build:
	go build -o _out/omni-infra-provider-morpheus ./cmd/omni-infra-provider-morpheus

test:
	go test ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

vet:
	go vet ./...

# Regenerates api/specs from the .proto. Needs protoc, protoc-gen-go and
# protoc-gen-go-vtproto on PATH.
generate:
	protoc -I api \
		--go_out=paths=source_relative:api \
		--go-vtproto_out=paths=source_relative:api \
		--go-vtproto_opt=features=marshal+unmarshal+size+equal \
		specs/specs.proto

docker:
	docker build -t omni-infra-provider-morpheus:local .
