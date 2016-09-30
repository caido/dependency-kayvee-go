include golang.mk
.DEFAULT_GOAL := test # override default goal set in library makefile

.PHONY: test bump-major bump-minor bump-patch $(PKGS)
SHELL := /bin/bash
PKGS = $(shell go list ./...)
$(eval $(call golang-version-check,1.7))

export _DEPLOY_ENV=testing

define set-version
@echo $(VERS) > VERSION
@echo "// AUTOGENERATED: DO NOT EDIT" > version.go
@echo >> version.go
@echo "package kayvee" >> version.go
@echo >> version.go
@echo "// Version is a string containing the version of this library." >> version.go
@echo "var Version = \"$(VERS)\"" >> version.go
@git add VERSION version.go
@git commit -m "Bump to v$(VERS)"
@git tag v$(VERS)
endef

bump-major:
	$(eval VERS := $(shell cat VERSION | awk 'BEGIN{FS="."} {print $$1+1 "." $$2 "." $$3}'))
	$(eval MAJOR_VERS := $(firstword $(subst ., ,$(VERS))))
	@find . -exec sed -i 's/gopkg\.in\/Clever\/kayvee-go\.v[[:digit:]]\+/gopkg\.in\/Clever\/kayvee-go\.v$(MAJOR_VERS)/' {} \;
	$(call set-version)

bump-minor:
	$(eval VERS := $(shell cat VERSION | awk 'BEGIN{FS="."} {print $$1 "." $$2+1 "." $$3}'))
	$(call set-version)

bump-patch:
	$(eval VERS := $(shell cat VERSION | awk 'BEGIN{FS="."} {print $$1 "." $$2 "." $$3+1}'))
	$(call set-version)

test: tests.json $(PKGS)

$(PKGS): golang-test-all-strict-deps
	@go get -d -t $@
	$(call golang-test-all-strict,$@)

tests.json:
	cp tests.json test/tests.json
