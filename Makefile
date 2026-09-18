BIN := wa-bridge
PKG := ./cmd/wa-bridge

# Static, no cgo, so the binary drops onto a fresh Debian with nothing installed.
GOFLAGS := CGO_ENABLED=0 GOOS=linux

.PHONY: test vet check arm64 amd64 clean

check: vet test

test:
	go test ./...

vet:
	go vet ./...

# Match the box: `uname -m` reports aarch64 for arm64, x86_64 for amd64.
arm64:
	$(GOFLAGS) GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o $(BIN).arm64 $(PKG)

amd64:
	$(GOFLAGS) GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o $(BIN).amd64 $(PKG)

clean:
	rm -f $(BIN).arm64 $(BIN).amd64
