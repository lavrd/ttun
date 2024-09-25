#!/bin/bash

goarch=""
uname_arch=$(uname -m)
if [[ $uname_arch == "arm64" ]]; then
    goarch="arm64"
elif [[ $uname_arch == "x86_64" ]]; then
    goarch="amd64"
else
    echo "Error: unknown architecture to build binary file:" ${uname_arch}
    exit 1
fi

CGO_ENABLED=0 GOOS="linux" GOARCH=${goarch} \
    go build -o target/ttun
