GOBIN := $(CURDIR)/bin

.PHONY: build test vet clean install

build:
	go build -o bin/krakoad ./cmd/krakoad
	go build -o bin/krakoactl ./cmd/krakoactl

test:
	go test ./...

vet:
	go vet ./...

install:
	go install ./cmd/krakoad ./cmd/krakoactl

clean:
	rm -rf bin
