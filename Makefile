WORKER ?= $(CURDIR)/bin/qjsbox-worker

.PHONY: worker test selftest vet example clean

worker:
	./worker/build.sh "$(WORKER)"

# Runs the full suite, including the native sandbox proof for this platform.
test: worker
	QJSBOX_WORKER="$(WORKER)" go test -race -count=1 ./...

# The worker sets RLIMIT_FSIZE=0, so its stdout must be a pipe, never a file.
selftest: worker
	@proof=$$("$(WORKER)" --selftest-isolation) && echo "$$proof"

vet:
	go vet ./...

example: worker
	go run ./examples/basic -worker "$(WORKER)"

clean:
	rm -rf bin
