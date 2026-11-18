#!/bin/sh
# Package version from the last Git tag (v1.2.3 -> 1.2.3, afterwards 1.2.3.5.gabcdef).
v=$(git describe --tags --match 'v[0-9]*' 2>/dev/null) || { echo 0.1.0; exit 0; }
echo "$v" | sed -e 's/^v//' -e 's/-\([0-9]*\)-g/.\1.g/'
