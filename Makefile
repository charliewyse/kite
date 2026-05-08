IMAGE   := kite-service
HELMDIR := helm/kite-service

.PHONY: test build release

# Run tests locally (mirrors CI)
test:
	cd app && go vet ./... && go test -race -count=1 ./...

# Build the image into minikube's Docker daemon for a specific version.
#   make build VERSION=1.2.3
build:
	$(if $(VERSION),,$(error VERSION is required — e.g. make build VERSION=1.2.3))
	eval $$(minikube docker-env) && \
	  docker build -t $(IMAGE):$(VERSION) ./app

# Build the image, bump all environment values files, commit, push, and tag.
#   make release VERSION=1.2.3
release: build
	$(if $(VERSION),,$(error VERSION is required — e.g. make release VERSION=1.2.3))
	sed -i 's/^  tag: .*/  tag: "$(VERSION)"/' $(HELMDIR)/values-dev.yaml
	sed -i 's/^  tag: .*/  tag: "$(VERSION)"/' $(HELMDIR)/values-staging.yaml
	sed -i 's/^  tag: .*/  tag: "$(VERSION)"/' $(HELMDIR)/values-prod.yaml
	git add $(HELMDIR)/values-dev.yaml \
	        $(HELMDIR)/values-staging.yaml \
	        $(HELMDIR)/values-prod.yaml
	git diff --staged --quiet || git commit -m "chore(release): $(VERSION)"
	git tag v$(VERSION)
	git push origin main
	git push origin v$(VERSION)
