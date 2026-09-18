#!/bin/sh
set -eu
testdir=$(mktemp -d)
trap 'rm -rf "$testdir"' EXIT HUP INT TERM
mkdir "$testdir/bin"
printf 'original PBS command\n' > "$testdir/bin/qsub"
if ./scripts/install.sh --bin-dir "$testdir/bin"; then
    echo 'Installer replaced an existing PBS command.' >&2
    exit 1
fi
test "$(cat "$testdir/bin/qsub")" = 'original PBS command'
./scripts/install.sh --bin-dir "$testdir/bin" --no-aliases
test "$(cat "$testdir/bin/qsub")" = 'original PBS command'
rm "$testdir/bin/qsub"
./scripts/install.sh --bin-dir "$testdir/bin"
test "$(readlink "$testdir/bin/qsub")" = pbotd
"$testdir/bin/pbotd" --version
"$testdir/bin/qsub" --help
