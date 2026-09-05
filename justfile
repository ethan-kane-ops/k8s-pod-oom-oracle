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

e2e_cluster  := "oom-oracle-e2e"
e2e_image    := "oom-oracle:e2e"
e2e_ns       := "oom-oracle"
e2e_workload := "oom-oracle-e2e"

e2e_base_image := "alpine:3.20"
# The image the test workloads run. Must match workloadImage in
# test/e2e/kube_test.go, which pins imagePullPolicy: Never so a mismatch fails
# in seconds naming the image rather than timing out ninety seconds later.
e2e_workload_image := "oom-oracle-workload:e2e"

# Create the kind cluster used by the e2e suite
#
# The inotify check is not incidental. Below roughly 512 instances, containerd
# inside the kind node fails to start its CRI plugin with "too many open files",
# the kubelet then cannot talk to a runtime, and the cluster dies during
# kubeadm init with an error that names none of this.
e2e-up:
    #!/usr/bin/env bash
    set -euo pipefail
    instances=$(docker run --rm --privileged --pid=host {{e2e_base_image}}       sysctl -n fs.inotify.max_user_instances)
    if [ "$instances" -lt 512 ]; then
      echo "▶ raising fs.inotify limits in the Docker VM (was $instances)"
      docker run --rm --privileged --pid=host {{e2e_base_image}} sh -c         'sysctl -w fs.inotify.max_user_watches=524288 >/dev/null
         sysctl -w fs.inotify.max_user_instances=1024 >/dev/null'
    fi
    if kind get clusters 2>/dev/null | grep -qx {{e2e_cluster}}; then
      echo "✓ cluster {{e2e_cluster}} already exists"
    else
      kind create cluster --config test/e2e/kind.yaml --wait 180s
    fi

# Build the image, load it into kind, and roll out the daemon
#
# The workload image is preloaded so no test ever waits on a registry. The first
# test to run would otherwise pay the pull inside its own report timeout, and a
# slow one there is indistinguishable from the daemon missing a kill.
#
# It is rebuilt from the base rather than loaded directly, because a pulled
# image is a multi-platform index whose other platforms' blobs were never
# fetched, and `kind load` asks ctr to import --all-platforms. A local build has
# one platform and every blob. Retagging does not help: the index travels with
# the name.
e2e-deploy: e2e-up
    echo "FROM {{e2e_base_image}}" | docker build -q -t {{e2e_workload_image}} -
    kind load docker-image {{e2e_workload_image}} --name {{e2e_cluster}}
    docker build -q -t {{e2e_image}} .
    kind load docker-image {{e2e_image}} --name {{e2e_cluster}}
    kubectl --context kind-{{e2e_cluster}} apply -f deploy/
    kubectl --context kind-{{e2e_cluster}} create namespace {{e2e_workload}} \
      --dry-run=client -o yaml | kubectl --context kind-{{e2e_cluster}} apply -f -
    kubectl --context kind-{{e2e_cluster}} -n {{e2e_ns}} set image \
      daemonset/oom-oracle oom-oracle={{e2e_image}}
    kubectl --context kind-{{e2e_cluster}} -n {{e2e_ns}} rollout status \
      daemonset/oom-oracle --timeout=180s

# Run the end-to-end suite against the deployed daemon
e2e: e2e-deploy
    kubectl config use-context kind-{{e2e_cluster}}
    go test -tags e2e -v -count=1 -timeout 20m ./test/e2e/...

# Tear the cluster down
e2e-down:
    kind delete cluster --name {{e2e_cluster}}

# Print the deployed daemon's logs, for when an e2e run fails
e2e-logs:
    kubectl --context kind-{{e2e_cluster}} -n {{e2e_ns}} logs \
      -l app.kubernetes.io/name=oom-oracle --tail=200

# Run linters against both target platforms, including build-tagged code.
#
# Linting only the host's GOOS is how an errorlint failure in tracer_linux.go
# reached main: the eBPF detector sits behind "linux && (amd64 || arm64)", so a
# run on macOS never compiles it, and a run on CI never compiles the
# !linux fallback beside it. Neither pass alone covers this package.
#
# The e2e tag is the same blind spot one level up. Without it the whole of
# test/e2e is invisible to every linter here, which is how a dead helper sat
# there unnoticed.
lint:
    GOOS=linux go vet -tags e2e ./...
    GOOS=darwin go vet ./...
    GOOS=linux golangci-lint run --build-tags e2e
    GOOS=darwin golangci-lint run

# Fuzz every parser target for the given budget each (default 60s)
#
# go test -fuzz takes exactly one target per run, so this enumerates them
# rather than relying on a pattern. Every target runs even after one fails,
# because finding two crashers in a run is more useful than finding the first.
fuzz time="60s":
    #!/usr/bin/env bash
    set -uo pipefail
    failed=0
    for pkg in ./internal/cgroup ./internal/procfs ./internal/correlate; do
      for target in $(go test "$pkg" -list 'Fuzz.*' 2>/dev/null | grep '^Fuzz' || true); do
        echo "▶ $pkg $target ({{time}})"
        if ! go test "$pkg" -run "^${target}$" -fuzz "^${target}$" -fuzztime={{time}}; then
          failed=1
        fi
      done
    done
    if [ "$failed" -ne 0 ]; then echo "✗ a fuzz target failed"; exit 1; fi
    echo "✓ all fuzz targets survived {{time}} each"

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
    # Hand-written notes under [Unreleased] describe this release, not a future
    # one, and nothing below can move them: git-cliff appends a section of its
    # own and cannot know they belong inside it. Stop here rather than tag a
    # release whose breaking changes sit under a heading saying "unreleased".
    if awk '/^## \[Unreleased\]/{f=1;next} /^## \[/{f=0} f && NF && !/^<!--/' CHANGELOG.md | grep -q .; then
      echo "✗ CHANGELOG.md has notes under [Unreleased]"
      echo "  Rename that heading to the version you are cutting, then re-run."
      exit 1
    fi
    case "{{bump}}" in
      v[0-9]*)                       new_ver="{{bump}}" ;;
      auto|patch|minor|major)        new_ver=$(git cliff --bump {{bump}} --bumped-version) ;;
      *) echo "usage: just release [auto|patch|minor|major|vX.Y.Z]"; exit 1 ;;
    esac
    case "$new_ver" in v*) ;; *) new_ver="v$new_ver" ;; esac
    if git rev-parse "$new_ver" >/dev/null 2>&1; then echo "✗ tag $new_ver already exists"; exit 1; fi
    just check
    echo "▶ releasing $new_ver"
    # --prepend, not -o. `-o` regenerates the whole file from commit subjects,
    # silently deleting every hand-written breaking-change and migration note:
    # exactly the content a release exists to carry, and the content no commit
    # subject can reconstruct. --prepend inserts the generated section under the
    # header and leaves the rest of the file alone.
    git cliff --tag "$new_ver" --unreleased --prepend CHANGELOG.md
    git add CHANGELOG.md
    git diff --cached --quiet || git commit -m "chore(release): $new_ver"
    git tag -a "$new_ver" -m "Release $new_ver"
    git push
    git push origin "refs/tags/$new_ver"
    notes=$(git cliff --tag "$new_ver" --latest --strip header)
    gh release create "$new_ver" --title "$new_ver" --notes "$notes" --verify-tag

# Serve the docs site locally with live reload
docs-serve:
    uv run --with-requirements docs/requirements.txt mkdocs serve

# Build the static docs site into ./site (--strict fails on a broken link)
docs-build:
    uv run --with-requirements docs/requirements.txt mkdocs build --strict
