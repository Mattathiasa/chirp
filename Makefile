.PHONY: build run demo test race fuzz vet fmt lint vuln clean

BIN     := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

build:            ## build chirpd and the demo into ./bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/chirpd ./cmd/chirpd
	go build -trimpath -o $(BIN)/chirp-demo ./cmd/demo

run: build        ## run the real daemon on http://127.0.0.1:7777
	$(BIN)/chirpd

demo: build       ## scripted peers, no network needed
	$(BIN)/chirp-demo

test:
	go test ./...

race:             ## what CI runs
	go test -race -count=1 ./...

fuzz:             ## 30 s on the wire-format decoder
	go test ./internal/proto -run '^$$' -fuzz FuzzDecode -fuzztime 30s

vet:
	go vet ./...

fmt:
	gofmt -l -w .

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

clean:
	rm -rf $(BIN)
