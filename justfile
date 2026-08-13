binary     := "oom-oracle"
module     := "github.com/ethan-kane-ops/k8s-pod-oom-oracle"
version    := `git describe --tags --always --dirty 2>/dev/null || echo dev`
commit     := `git rev-parse --short HEAD 2>/dev/null || echo unknown`
build_date := `date -u +%Y-%m-%dT%H:%M:%SZ`

ldflags := "-s -w" \
    + " -X " + module + "/internal/version.version=" + version \
    + " -X " + module + "/internal/version.commit=" + commit \
    + " -X " + module + "/internal/version.date=" + build_date

default:
    @just --list

# Build the binary into ./bin/ (isolated — does not affect the installed binary)
build:
    go build -trimpath -ldflags="{{ldflags}}" -o bin/{{binary}} ./cmd/{{binary}}

# Run the locally built binary — safe during development, never touches the installed version
run *args: build
    ./bin/{{binary}} {{args}}

# Run all tests
test:
    go test ./...

# Run tests with race detector
test-race:
    go test -race ./...

# Run tests with coverage and print the per-package summary
test-cover:
    go test -race -coverprofile=coverage.out -covermode=atomic ./...
    go tool cover -func=coverage.out

# Open the HTML coverage report
cover-html: test-cover
    go tool cover -html=coverage.out

# Build the container image for the local platform
docker-build:
    docker build \
      --build-arg VERSION={{version}} \
      --build-arg COMMIT={{commit}} \
      --build-arg BUILD_DATE={{build_date}} \
      -t {{binary}}:{{version}} .

# Regenerate the eBPF objects in internal/detector/bpf.
#
# Runs in a container because macOS clang has no BPF backend. The generated .o
# and .go files are committed, so this is only needed after editing the C.
bpf-generate:
    docker build -q -t oom-oracle-bpf:local -f build/bpf/Dockerfile build/bpf
    docker run --rm -u "$(id -u):$(id -g)" \
      -v "$PWD:/src" -w /src/internal/detector/bpf \
      -e HOME=/tmp -e GOCACHE=/tmp/gocache -e GOMODCACHE=/tmp/gomodcache \
      oom-oracle-bpf:local go generate ./...

# Fail if the committed eBPF objects no longer match the C source.
bpf-verify: bpf-generate
    @git diff --exit-code -- internal/detector/bpf \
      || (echo "✗ committed eBPF objects are stale — commit the regenerated files"; exit 1)
    @echo "✓ eBPF objects match their source"

# Run linters
lint:
    go vet ./...
    golangci-lint run

# Tidy go modules
tidy:
    go mod tidy

# Tidy + lint + test. Uses the race detector so the local gate matches CI:
# concurrency bugs in this codebase are the ones that fail on a node, not a laptop.
check: tidy lint test-race

# Remove build artifacts
clean:
    rm -rf bin/ coverage.out

# Install binary via `go install` and reshim so mise exposes it immediately
install:
    go install -trimpath -ldflags="{{ldflags}}" ./cmd/{{binary}}
    mise reshim 2>/dev/null || true
    @echo "installed → $(which {{binary}} 2>/dev/null || go env GOBIN)/{{binary}}"

# Preview the next release without writing anything
release-preview bump="auto":
    @git cliff --bump {{bump}} --bumped-version | xargs -I{} echo "next: v{}"
    @echo "── changelog preview ──"
    @git cliff --bump {{bump}} --unreleased

# Cut a release: bump (auto/patch/minor/major) or explicit vX.Y.Z. Generates CHANGELOG.md, tags, pushes, gh release.
release bump="auto":
    #!/usr/bin/env bash
    set -euo pipefail
    if ! git diff-index --quiet HEAD --; then echo "✗ working tree dirty"; exit 1; fi
    case "{{bump}}" in
      v[0-9]*)                       new_ver="{{bump}}" ;;
      auto|patch|minor|major)        new_ver=$(git cliff --bump {{bump}} --bumped-version) ;;
      *) echo "usage: just release [auto|patch|minor|major|vX.Y.Z]"; exit 1 ;;
    esac
    case "$new_ver" in v*) ;; *) new_ver="v$new_ver" ;; esac
    if git rev-parse "$new_ver" >/dev/null 2>&1; then echo "✗ tag $new_ver already exists"; exit 1; fi
    just check
    echo "▶ releasing $new_ver"
    git cliff --tag "$new_ver" -o CHANGELOG.md
    git add CHANGELOG.md
    git diff --cached --quiet || git commit -m "chore(release): $new_ver"
    git tag -a "$new_ver" -m "Release $new_ver"
    git push
    git push origin "refs/tags/$new_ver"
    notes=$(git cliff --tag "$new_ver" --latest --strip header)
    gh release create "$new_ver" --title "$new_ver" --notes "$notes" --verify-tag
