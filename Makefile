BINARY  := nimdeploy
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint install uninstall clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

test:
	go test -race ./...

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...
	shellcheck install.sh deploy/examples/*.sh

install: build
	sudo ./install.sh install

uninstall:
	sudo ./install.sh uninstall

clean:
	rm -rf $(BINARY) dist
