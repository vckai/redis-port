.DEFAULT_GOAL := build-all

export GO15VENDOREXPERIMENT=1

build-all: redis-port

redis-port:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -i -o bin/redis-port ./cmd

clean:
	@rm -rf bin

distclean: clean

gotest:
	go test ./pkg/...
