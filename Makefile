.PHONY: all build clean test tidy

PLUGIN_NAME = commandcode-go.so
VERSION ?= 0.1.0

all: build

build:
	CGO_ENABLED=1 go build -buildmode=c-shared -ldflags "-X main.pluginVersion=$(VERSION) -s -w" -o $(PLUGIN_NAME) .

tidy:
	go mod tidy

test:
	go test -v ./...

clean:
	rm -f $(PLUGIN_NAME) $(PLUGIN_NAME).* commandcode-go.h *.log
