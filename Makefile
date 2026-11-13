.PHONY: all fmt vet test wasm-test bench benchcmp

# Format, vet and test with the race detector.
all: fmt vet test

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test -race ./...

# js/wasm. WASM_ENV runs go test under go_js_wasm_exec (Node): it puts the
# exec wrapper on PATH and starts from an empty environment, because Node
# refuses a command line plus environment over its size limit, which a
# large CI or container environment exceeds.
WASM_ENV = env -i HOME="$(HOME)" PATH="$(shell go env GOROOT)/lib/wasm:$(PATH)" \
	GOPATH="$(shell go env GOPATH)" GOMODCACHE="$(shell go env GOMODCACHE)" \
	GOCACHE="$(shell go env GOCACHE)" GOTOOLCHAIN="$(shell go env GOTOOLCHAIN)" \
	GOOS=js GOARCH=wasm

# All tests under js/wasm (needs Node).
wasm-test:
	@command -v node >/dev/null || { echo "wasm-test: node not found"; exit 1; }
	$(WASM_ENV) go vet ./...
	$(WASM_ENV) go test ./...

# Benchmarks into build/bench/<name>.txt, compared with benchstat:
# `make bench name=baseline`, change something, then
# `make bench name=new && make benchcmp new=new`.
name ?= current
new ?= current
count ?= 10

bench:
	mkdir -p build/bench
	go test ./... -run '^$$' -bench . -benchmem -count $(count) | tee build/bench/$(name).txt

benchcmp:
	@test -f build/bench/baseline.txt || { echo "benchcmp: no build/bench/baseline.txt, run make bench name=baseline first"; exit 1; }
	benchstat build/bench/baseline.txt build/bench/$(new).txt
