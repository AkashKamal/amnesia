BIN := amnesia
TLDR ?= /tmp/tldr

.PHONY: build test fmt vet corpus clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(BIN) ./cmd/amnesia

test:
	go test ./... -race

fmt:
	gofmt -w .

vet:
	go vet ./...

# Regenerate the command corpus from upstream tldr-pages. The result is
# committed on purpose: the build must not need the network, and a bad
# generator run should be visible as a diff in review.
corpus:
	@test -d $(TLDR) || git clone --depth 1 https://github.com/tldr-pages/tldr $(TLDR)
	@git -C $(TLDR) pull --ff-only --quiet || true
	go run ./tools/corpus-gen -tldr $(TLDR) -out internal/corpus/data/commands.tsv
	go test ./internal/corpus/

clean:
	rm -f $(BIN) $(BIN).exe
	rm -rf dist/
