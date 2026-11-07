# Build the sample plugin and package it for import into glue2flow.
#
#   make go       standard Go toolchain (>= 1.24), ~3 MB
#   make tinygo   TinyGo (>= 0.34), ~0.5 MB
#   make bundle   -> mistral.zip (plugin.yaml + plugin.wasm)

.PHONY: go tinygo bundle clean

go:
	cd go && GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o ../plugin.wasm .

tinygo:
	cd go && tinygo build -target wasip1 -buildmode=c-shared -no-debug -o ../plugin.wasm .

bundle: plugin.wasm
	rm -f mistral.zip
	zip -j mistral.zip plugin.yaml plugin.wasm

plugin.wasm:
	@echo "build plugin.wasm first: make go | make tinygo" && exit 1

clean:
	rm -f plugin.wasm mistral.zip
