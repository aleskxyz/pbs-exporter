NAME        ?= pbs-exporter
BUILD_DATE  ?= $(shell date -Iseconds)
VERSION     ?= $(shell git describe --tags --abbrev=0 2>/dev/null || git rev-parse --short HEAD)-local
# Adds "-dirty" suffix if there are uncommitted changes in the git repository
COMMIT_REF  ?= $(shell git describe --dirty --always)
GOOS        ?= $(shell go env GOOS)
GOARCH      ?= $(shell go env GOARCH)
LICENSE     ?= GPL-3
# <owner>/<name> of the GitHub repository from the "origin" remote, so that forks label their own images
GIT_REPOSITORY ?= $(shell git remote get-url origin 2>/dev/null | sed -nE 's,^(git@github\.com:|(https|ssh)://([^@/]+@)?github\.com/)([^/]+/[^/]+)$$,\4,p' | sed 's,\.git$$,,')
REPO_URL    ?= $(if $(GIT_REPOSITORY),https://github.com/$(GIT_REPOSITORY))
MAIL        ?= info@natron.io
VENDOR      ?= $(firstword $(subst /, ,$(GIT_REPOSITORY)))
DESCRIPTION ?= Export Proxmox Backup Server metrics for Prometheus

#########
# Go    #
#########

.PHONY: go-tidy
go-tidy:
	go mod tidy -compat=1.27
	@echo "Go modules tidied."

.PHONY: go-update
go-update:
	go get -u ./...
	make go-tidy
	@echo "Go modules updated."

.PHONY: go-build
go-build:
	go build -o $(NAME) -trimpath -tags="netgo" -ldflags "-s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT_REF) -X main.BuildTime=$(BUILD_DATE)" .
	@echo "Go build completed."

#########
# TOOLS #
#########

KO_VERSION  = v0.19.1
KO = $(shell pwd)/bin/ko

ko:
	$(call go-install-tool,$(KO),github.com/google/ko@$(KO_VERSION))

#########
# Ko    #
#########

KO_PLATFORM  ?= linux/$(GOARCH)
KOCACHE      ?= /tmp/ko-cache
KO_TAGS      := $(VERSION)

# Function to check if VERSION contains "-local" or "-rc*"
ifeq ($(findstring -local,$(VERSION)),)
  ifeq ($(findstring -rc,$(VERSION)),)
    # If VERSION does not contain "-local" or "-rc*", add 'latest' to KO_TAGS
    KO_TAGS := $(VERSION),latest
  endif
endif

REGISTRY        ?= ghcr.io
# ghcr.io requires lowercase names
REPO            ?= $(shell echo '$(VENDOR)' | tr '[:upper:]' '[:lower:]')
KO_REPOSITORY   := $(REGISTRY)/$(REPO)/$(NAME)

REGISTRY_PASSWORD  ?= dummy
REGISTRY_USERNAME  ?= dummy

LD_FLAGS        := "-s \
					-w \
					-X main.Version=$(VERSION) \
					-X main.Commit=$(COMMIT_REF) \
					-X main.BuildTime=$(BUILD_DATE)"

LABELS		    := "--image-label=org.opencontainers.image.created=$(BUILD_DATE),$\
						org.opencontainers.image.authors=$(MAIL),$\
						org.opencontainers.image.url=$(REPO_URL),$\
						org.opencontainers.image.documentation=$(REPO_URL),$\
						org.opencontainers.image.source=$(REPO_URL),$\
						org.opencontainers.image.version=$(VERSION),$\
						org.opencontainers.image.revision=$(COMMIT_REF),$\
						org.opencontainers.image.vendor=$(VENDOR),$\
						org.opencontainers.image.licenses=$(LICENSE),$\
						org.opencontainers.image.title=$(NAME),$\
						org.opencontainers.image.description=$(DESCRIPTION),$\
						org.opencontainers.image.base.name=cgr.dev/chainguard/static"

# REPO_URL, VENDOR and REPO have no fallback: fail if they could not be detected from the origin remote
.PHONY: check-repo
check-repo:
	@[ -n "$(REPO_URL)" ] && [ -n "$(VENDOR)" ] && [ -n "$(REPO)" ] || { \
		echo "ERROR: unable to detect the GitHub repository from the origin remote, set REPO_URL, VENDOR and REPO" >&2; exit 1; }

# Local ko build
.PHONY: ko-build-local
ko-build-local: ko check-repo
	@echo Building $(NAME) $(KO_TAGS) for $(KO_PLATFORM) >&2
	@LD_FLAGS=$(LD_FLAGS) KOCACHE=$(KOCACHE) KO_DOCKER_REPO=$(KO_REPOSITORY) \
		$(KO) build ./ --bare --tags=$(KO_TAGS) $(LABELS) --push=false --local --platform=$(KO_PLATFORM) --sbom=none

# Ko publish image
.PHONY: ko-login
ko-login: ko
	@$(KO) login $(REGISTRY) --username $(REGISTRY_USERNAME) --password $(REGISTRY_PASSWORD)

.PHONY: ko-publish
ko-publish: check-repo ko-login
	@LD_FLAGS=$(LD_FLAGS) KOCACHE=$(KOCACHE) KO_DOCKER_REPO=$(KO_REPOSITORY) \
		$(KO) build ./ --bare --tags=$(KO_TAGS) $(LABELS) --sbom=none

###########
# Helpers #
###########

# go-install-tool will 'go install' any package $2 and install it to $1.
PROJECT_DIR := $(shell dirname $(abspath $(lastword $(MAKEFILE_LIST))))
define go-install-tool
@[ -f $(1) ] || { \
set -e ;\
GOBIN=$(PROJECT_DIR)/bin go install $(2) ;\
}
endef
