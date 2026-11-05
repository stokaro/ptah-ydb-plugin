# Lint entry points. They follow the host's Makefile targets of the same names,
# so the provider is held to the host's lint contract.
#
# Every linter runs over two build contours: the default one, and the one with
# the integration tag. A file behind the tag is outside the default build, so a
# run without the tag never reads the live tests.

export GOWORK := off

BUILD_TAGS := integration

# lint-qtlint-fix runs the rules in separate passes. Applied together with -fix
# they can collide: one rule deletes a receiver declaration the other rule's
# rewrite still references (go-extras/qtlint#65).
QTLINT_RULES := -require-qt-c-receiver -require-data-rows -require-testing-run

.PHONY: lint lint-qtlint lint-qtlint-fix lint-nolintguard lint-golangci lint-teststyle lint-fix

lint: lint-qtlint lint-nolintguard lint-golangci lint-teststyle

lint-qtlint:
	go tool qtlint $(QTLINT_RULES) ./...
	go tool qtlint $(QTLINT_RULES) -tags $(BUILD_TAGS) ./...

lint-qtlint-fix:
	for rule in $(QTLINT_RULES); do \
		go tool qtlint $$rule -fix ./... || exit 1; \
		go tool qtlint $$rule -fix -tags $(BUILD_TAGS) ./... || exit 1; \
	done

lint-nolintguard:
	nolintguard="$$(go tool -n nolintguard)" && \
	go vet -vettool="$$nolintguard" -require-justification ./... && \
	go vet -tags $(BUILD_TAGS) -vettool="$$nolintguard" -require-justification ./...

lint-golangci:
	golangci-lint run ./...
	golangci-lint run --build-tags=$(BUILD_TAGS) ./...

lint-teststyle:
	scripts/check-test-style.sh

lint-fix:
	$(MAKE) lint-qtlint-fix
	golangci-lint run --fix ./...
	golangci-lint run --fix --build-tags=$(BUILD_TAGS) ./...
	$(MAKE) lint
