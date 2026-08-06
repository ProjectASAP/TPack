SHELL := /bin/bash

PROTO_DIR := pkg/tpackmodel/proto
GO_OUT := pkg/tpackmodel/proto

.PHONY: proto-gen build test clean tidy

proto-gen:
	# tpack.proto → Go
	protoc \
		--go_out=$(GO_OUT) --go_opt=paths=source_relative \
		-I $(PROTO_DIR) \
		$(PROTO_DIR)/tpack.proto
	# model_service.proto → Go
	protoc \
		--go_out=$(PROTO_DIR) --go_opt=paths=source_relative \
		--go-grpc_out=$(PROTO_DIR) --go-grpc_opt=paths=source_relative \
		-I $(PROTO_DIR) \
		$(PROTO_DIR)/model_service.proto

tidy:
	cd pkg/tpackmodel && go mod tidy
	cd exporter/tpackexporter && go mod tidy
	cd receiver/tpackreceiver && go mod tidy
	cd cmd/otelcol-tpack && go mod tidy
	cd cmd/tpack-eval && go mod tidy

# `build` deliberately does not depend on `tidy`: `go mod tidy` needs network
# access and rewrites go.mod/go.sum, which would leave a fresh checkout dirty.
# Run `make tidy` explicitly when adding or removing dependencies.
build:
	cd pkg/tpackmodel && go build ./...
	cd exporter/tpackexporter && go build ./...
	cd receiver/tpackreceiver && go build ./...
	cd cmd/tpack-eval && go build ./...

test:
	cd pkg/tpackmodel && go test -v -count=1 ./...
	cd exporter/tpackexporter && go test -v -count=1 ./...
	cd receiver/tpackreceiver && go test -v -count=1 ./...
	cd cmd/tpack-eval && go test -v -count=1 ./...

clean:
	rm -f $(GO_OUT)/*.pb.go
	cd pkg/tpackmodel && go clean
	cd exporter/tpackexporter && go clean
	cd receiver/tpackreceiver && go clean
	cd cmd/tpack-eval && go clean
