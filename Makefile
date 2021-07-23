include ./Makefile.Common
VERSION=$(shell cat VERSION)

MOD_NAME=github.com/rockb1017/hec-receiver-mock

ALL_MODULES := $(shell find . -type f -name "go.mod" -exec dirname {} \; | sort | egrep  '^./' )

all-modules:
	@echo $(ALL_MODULES) | tr ' ' '\n' | sort


.PHONY: gotidy
gotidy:
	$(MAKE) for-all CMD="rm -fr go.sum"
	$(MAKE) for-all CMD="go mod tidy"

.PHONY: gomoddownload
gomoddownload:
	@$(MAKE) for-all CMD="go mod download"

.PHONY: gotestinstall
gotestinstall:
	@$(MAKE) for-all CMD="make test GOTEST_OPT=\"-i\""

.PHONY: gotest
gotest:
	$(MAKE) for-all CMD="make test"

.PHONY: gofmt
gofmt:
	$(MAKE) for-all CMD="make fmt"

.PHONY: golint
golint:
	$(MAKE) for-all CMD="make lint"

.PHONY: for-all
for-all:
	@echo "running $${CMD} in root"
	@$${CMD}
	@set -e; for dir in $(ALL_MODULES); do \
	  (cd "$${dir}" && \
	  	echo "running $${CMD} in $${dir}" && \
	 	$${CMD} ); \
	done

.PHONY: add-tag
add-tag:
	@[ "${TAG}" ] || ( echo ">> env var TAG is not set"; exit 1 )
	@echo "Adding tag ${TAG}"
	@git tag -a ${TAG} -s -m "Version ${TAG}"
	@set -e; for dir in $(ALL_MODULES); do \
	  (echo Adding tag "$${dir:2}/$${TAG}" && \
	 	git tag -a "$${dir:2}/$${TAG}" -s -m "Version ${dir:2}/${TAG}" ); \
	done

.PHONY: push-tag
push-tag:
	@[ "${TAG}" ] || ( echo ">> env var TAG is not set"; exit 1 )
	@echo "Pushing tag ${TAG}"
	@git push upstream ${TAG}
	@set -e; for dir in $(ALL_MODULES); do \
	  (echo Pushing tag "$${dir:2}/$${TAG}" && \
	 	git push upstream "$${dir:2}/$${TAG}"); \
	done

.PHONY: delete-tag
delete-tag:
	@[ "${TAG}" ] || ( echo ">> env var TAG is not set"; exit 1 )
	@echo "Deleting tag ${TAG}"
	@git tag -d ${TAG}
	@set -e; for dir in $(ALL_MODULES); do \
	  (echo Deleting tag "$${dir:2}/$${TAG}" && \
	 	git tag -d "$${dir:2}/$${TAG}" ); \
	done


GOMODULES = $(ALL_MODULES) $(PWD)
.PHONY: $(GOMODULES)
MODULEDIRS = $(GOMODULES:%=for-all-target-%)
for-all-target: $(MODULEDIRS)
$(MODULEDIRS):
	$(MAKE) -C $(@:for-all-target-%=%) $(TARGET)
.PHONY: for-all-target


.PHONY: docker-component # Not intended to be used directly
docker-component: check-component
	GOOS=linux GOARCH=amd64 $(MAKE) $(COMPONENT)
	cp ./bin/$(COMPONENT)_linux_amd64 ./cmd/$(COMPONENT)/$(COMPONENT)
	docker build -t $(COMPONENT) ./cmd/$(COMPONENT)/
	rm ./cmd/$(COMPONENT)/$(COMPONENT)


.PHONY: docker
docker:
	GOOS=linux GOARCH=amd64 $(MAKE) build
	cp ./bin/hec-receiver-mock_linux_amd64 ./cmd/hec-receiver-mock/hec-receiver-mock
	docker build -t hec-receiver-mock:$(VERSION) ./cmd/hec-receiver-mock/
	rm ./cmd/hec-receiver-mock/hec-receiver-mock

# Build the Collector executable.
.PHONY: build
build:
	GO111MODULE=on CGO_ENABLED=0 go build -trimpath -o ./bin/hec-receiver-mock_$(GOOS)_$(GOARCH)$(EXTENSION)