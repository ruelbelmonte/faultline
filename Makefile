GO      ?= go
BIN     := bin
VERSION ?= 0.1.0
LDFLAGS := -X faultline/internal/cli.Version=$(VERSION)
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2

.PHONY: build ui test test-integration test-race test-load test-ui lint demo docker clean

# Builds both binaries. The web UI is embedded from internal/ui/dist, which is
# committed, so this needs Go only. Run `make ui` first after changing ui/.
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/ ./cmd/faultline ./cmd/faultline-demo

ui:
	cd ui && npm ci && npm run build

test:
	$(GO) test ./...

# Real faultline and faultline-demo processes on free ports.
test-integration:
	$(GO) test -tags integration ./test/integration/ -count=1 -timeout 30m

# Needs cgo (a C compiler). The spawned binaries are built with -race too.
test-race:
	$(GO) test ./... -race
	FAULTLINE_ITEST_RACE=1 $(GO) test -race -tags integration ./test/integration/ -count=1 -timeout 30m

# Measures proxy latency overhead and throughput with and without an active run.
test-load:
	$(GO) test -tags "integration load" ./test/integration/ -run TestLoad -count=1 -v -timeout 10m

# Playwright smoke test and axe accessibility audit of the UI against real
# processes, in Chromium, Firefox and WebKit.
test-ui: build
	cd ui && npm ci && npx playwright install chromium firefox webkit && npx playwright test

lint:
	$(GO) vet ./...
	$(GO) vet -tags "integration load" ./test/...
	@out="$$(gofmt -l cmd internal test)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) run $(STATICCHECK) -tags "integration load" ./...

# Runs the demo system under test on its default ports.
demo: build
	$(BIN)/faultline-demo

docker:
	docker build -t faultline:$(VERSION) .
	docker build -t faultline-demo:$(VERSION) -f Dockerfile.demo .

clean:
	rm -rf $(BIN)
