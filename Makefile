GOFLAGS ?= -mod=mod
BINARY := provider-unleash
DIST := dist

.PHONY: build test vet package clean

build:
	go build $(GOFLAGS) -o $(BINARY) .

test:
	go test $(GOFLAGS) ./...

vet:
	go vet $(GOFLAGS) ./...

package: build
	go run $(GOFLAGS) ./cmd/package -binary $(BINARY) -manifest provider.codefly.yaml -out $(DIST)

clean:
	rm -rf $(BINARY) $(DIST)
