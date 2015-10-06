#!/bin/bash

set -eo pipefail

export GOPATH="$(pwd)/go"
export PATH="${GOPATH}/bin:/usr/local/go/bin:/usr/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

make test test-pps-extra docker-build
