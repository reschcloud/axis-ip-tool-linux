#!/bin/sh
# Paketversion aus dem letzten Git-Tag (v1.2.3 -> 1.2.3, danach 1.2.3.5.gabcdef).
v=$(git describe --tags --match 'v[0-9]*' 2>/dev/null) || { echo 0.1.0; exit 0; }
echo "$v" | sed -e 's/^v//' -e 's/-\([0-9]*\)-g/.\1.g/'
