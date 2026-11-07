# homelab-bot - Discord bot for a self-hosted media server
# See LICENSE file for copyright and license details.

include config.mk

all: build

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP) ./cmd/$(APP)

run:
	$(GO) run ./cmd/$(APP)

test:
	$(GO) test -v ./...

fmt:
	$(GO) fmt ./...

lint:
	$(GO) vet ./...
	$(GO) tool staticcheck ./...

clean:
	rm -rf bin/

install: build
	mkdir -p $(DESTDIR)$(PREFIX)/bin
	cp -f bin/$(APP) $(DESTDIR)$(PREFIX)/bin
	chmod 755 $(DESTDIR)$(PREFIX)/bin/$(APP)

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(APP)

.PHONY: all build run test fmt lint clean install uninstall
