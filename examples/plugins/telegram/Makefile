# Build the sample plugin and package it for import into glue2flow.
#
#   make go       standard Go toolchain (>= 1.24), ~3 MB
#   make tinygo   TinyGo (>= 0.34), ~0.5 MB
#   make js       Extism JS PDK (extism-js + binaryen on PATH)
#   make bundle   -> telegram.zip (plugin.yaml + plugin.wasm)

.PHONY: go tinygo js bundle clean

go:
	cd go && GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o ../plugin.wasm .

tinygo:
	cd go && tinygo build -target wasip1 -buildmode=c-shared -no-debug -o ../plugin.wasm .

js:
	cd js && extism-js index.js -i index.d.ts -o ../plugin.wasm

bundle: plugin.wasm
	rm -f telegram.zip
	zip -j telegram.zip plugin.yaml plugin.wasm

plugin.wasm:
	@echo "build plugin.wasm first: make go | make tinygo | make js" && exit 1

clean:
	rm -f plugin.wasm telegram.zip
