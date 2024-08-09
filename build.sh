#!/bin/bash

uname_output=$(uname -m)
if [[ $uname_output == "arm64" ]]; then
    arch="aarch64-linux"
elif [[ $uname_output == "x86_64" ]]; then
    arch="x86_64-linux"
else
    echo "Error: unknown architecture to build binary file:" ${uname_output}
    exit 1
fi

zig build-exe src/main.zig \
  -target ${arch} \
  --library c \
  -femit-bin=zig-out/linux/bin/ttun
