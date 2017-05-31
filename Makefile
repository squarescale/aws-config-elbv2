DOCKER=docker

GO_PACKAGE_DIR=github.com/squarescale/aws-config-elbv2

VERSION = $(shell git describe --always --dirty)
NAME=aws-config-elbv2
DOCKER_IMAGE:=aws-config-elbv2
ifdef ECR_ADDR
DOCKER_IMAGE:=$(ECR_ADDR)/$(DOCKER_IMAGE)
endif
ifdef SQSC_ENV
DOCKER_IMAGE:=$(DOCKER_IMAGE):$(SQSC_ENV)-latest
endif

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

build: ## Build
	rm -rf gopath
	mkdir -p gopath/src/$(dir $(GO_PACKAGE_DIR))
	ln -s "$$PWD" gopath/src/$(GO_PACKAGE_DIR)
	export GOPATH="$$PWD/gopath"; \
	cd gopath/src/$(GO_PACKAGE_DIR); \
	go build -ldflags "-X main.version=$(VERSION)" .

build-linux-static: ## Build for linux-static (docker)
	GOOS=linux CGO_ENABLED=0 go build -o $(NAME)-linux-static -installsuffix -linux-static -ldflags "-X main.version=$(VERSION)" .

docker: build-linux-static get-ca-certificates ## Create squarescale-status docker image (requires build)
	$(DOCKER) build -t $(DOCKER_IMAGE) .

docker-push:
	$(DOCKER) push $(DOCKER_IMAGE)

get-ca-certificates: 
	mkdir -p ca
	$(DOCKER) run --rm -v $$PWD/ca:/ca:Z alpine:latest sh -c "apk add --update ca-certificates ; cp /etc/ssl/certs/ca-certificates.crt /ca/"

stop start status journal destroy:
	printf '\033]0;%s\007' "sqsc-status $@"
	fleetctl $@ $(FLEET_ARGS) squarescale-status.service

restart: stop start

lint: ## Lint Docker
	docker run --rm -v $$PWD:/root/ projectatomic/dockerfile-lint dockerfile_lint
	docker run --rm -i sjourdan/hadolint < Dockerfile

.PHONY: help build build-linux-static docker docker-push ca-certificates.crt
.PHONY: stop start status journal destroy restart
