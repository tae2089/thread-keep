GO_TAGS := sqlite_fts5 sqlite_omit_load_extension
GO_ENV := CGO_ENABLED=1

.PHONY: test test-pack vet build verify-no-stale-binaries build-pack release-build release-test fmt docker-build docker-build-server docker-build-coordinator docker-build-runner e2e

test:
	$(GO_ENV) go test -tags '$(GO_TAGS)' ./...
	$(MAKE) test-pack

test-pack:
	cd packs/typescript && go test ./...
	cd packs/javascript && go test ./...
	cd packs/python && go test ./...
	cd packs/java && go test ./...
	cd packs/kotlin && go test ./...
	cd packs/rust && go test ./...

vet:
	$(GO_ENV) go vet -tags '$(GO_TAGS)' ./...

build: verify-no-stale-binaries
	$(GO_ENV) go build -trimpath -tags '$(GO_TAGS)' -o bin/thread-keep ./cmd/thread-keep
	$(GO_ENV) go build -trimpath -tags '$(GO_TAGS)' -o bin/thread-keep-server ./cmd/thread-keep-server
	$(GO_ENV) go build -trimpath -tags '$(GO_TAGS)' -o bin/thread-keep-coordinator ./cmd/thread-keep-coordinator
	$(GO_ENV) go build -trimpath -tags '$(GO_TAGS)' -o bin/thread-keep-runner ./cmd/thread-keep-runner
	$(GO_ENV) go build -trimpath -tags '$(GO_TAGS)' -o bin/thread-keep-mcp ./cmd/thread-keep-mcp

verify-no-stale-binaries:
	test ! -e bin/thread-keep-planner-runner
	test ! -e bin/thread-keep-planner
	test ! -e bin/thread-keep-webhook

build-pack:
	cd packs/typescript && go build -trimpath -o ../../bin/thread-keep-index-typescript ./cmd/thread-keep-index-typescript
	cd packs/javascript && go build -trimpath -o ../../bin/thread-keep-index-javascript ./cmd/thread-keep-index-javascript
	cd packs/python && go build -trimpath -o ../../bin/thread-keep-index-python ./cmd/thread-keep-index-python
	cd packs/java && go build -trimpath -o ../../bin/thread-keep-index-java ./cmd/thread-keep-index-java
	cd packs/kotlin && go build -trimpath -o ../../bin/thread-keep-index-kotlin ./cmd/thread-keep-index-kotlin
	cd packs/rust && go build -trimpath -o ../../bin/thread-keep-index-rust ./cmd/thread-keep-index-rust

release-build:
	test -n "$(THREAD_KEEP_MANIFEST_PUBLIC_KEY_B64)"
	$(GO_ENV) go build -trimpath -tags '$(GO_TAGS)' -ldflags "-X github.com/tae2089/thread-keep/internal/indexing.officialManifestPublicKeyBase64=$(THREAD_KEEP_MANIFEST_PUBLIC_KEY_B64)" -o bin/thread-keep ./cmd/thread-keep

release-test:
	node --test scripts/release/*.test.mjs
	python3 -m unittest scripts/release/test_build_wheels.py scripts/release/test_pypi_launcher.py

fmt:
	gofmt -w cmd internal

docker-build: docker-build-server docker-build-coordinator docker-build-runner

docker-build-server:
	docker build --file Dockerfile.server --tag thread-keep-server:local .

docker-build-coordinator:
	docker build --file Dockerfile.coordinator --tag thread-keep-coordinator:local .

docker-build-runner:
	docker build --file Dockerfile.runner --tag thread-keep-runner:local .

e2e:
	docker build --file Dockerfile.e2e --tag thread-keep-e2e:local .
	docker run --rm thread-keep-e2e:local
