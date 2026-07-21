BINARY := katana
PKG := ./cmd/katana

.PHONY: build run test tidy fmt vet clean launch-test-katana

build:
	go build -o $(BINARY) $(PKG)

run: build
	./$(BINARY) --config config.yaml

test:
	go test ./...

tidy:
	go mod tidy

fmt:
	go fmt ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
	rm -rf proposals

# make launch-test-katana DRY_RUN=true   - evaluate + notify, never broadcast
DRY_RUN ?= false

launch-test-katana:
	DRY_RUN=$(DRY_RUN) ./scripts/launch-test-katana.sh
