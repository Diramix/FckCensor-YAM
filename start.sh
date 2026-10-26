#!/bin/sh
set -eu
DIR=$(CDPATH= cd "$(dirname "$0")" && pwd)
exec "$DIR/bin/fckcensor" -library "$DIR/lib/libfckcensor.so"
