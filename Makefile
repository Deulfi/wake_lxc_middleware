.PHONY: lint test vendor clean yaegi_test

export GO111MODULE=on

default: lint test

lint:
	golangci-lint run

test:
	go test -v -cover ./...

yaegi_test:
	rm -rf ./tmp
	mkdir -p ./tmp/src/github.com/Deulfi/wake_lxc_middleware
	cp -r ./internal ./middleware ./*.go ./.traefik.yml ./go.mod ./tmp/src/github.com/Deulfi/wake_lxc_middleware/
	GOPATH=$(shell pwd)/tmp yaegi test github.com/Deulfi/wake_lxc_middleware/middleware github.com/Deulfi/wake_lxc_middleware/internal
	rm -rf ./tmp

vendor:
	go mod vendor

clean:
	rm -rf ./vendor
	rm -rf ./tmp
