IMG ?= docker.io/sathvikm2002/secretusage:latest
CHART ?= charts/secretusage-controller
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0

.PHONY: all
all: test

.PHONY: manifests
manifests:
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=secretusage-controller-manager-role paths="./internal/controller/..." output:rbac:artifacts:config=config/rbac
	cp config/crd/bases/usage.secretusage.io_secretusages.yaml $(CHART)/crds/usage.secretusage.io_secretusages.yaml

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test: fmt vet
	go test ./...

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push:
	docker push $(IMG)

.PHONY: helm-lint
helm-lint:
	helm lint $(CHART)

.PHONY: helm-template
helm-template:
	helm template secretusage-controller $(CHART)
