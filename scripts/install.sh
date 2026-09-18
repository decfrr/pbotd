#!/bin/sh
set -eu

bindir=${HOME:?}/.local/bin
aliases=true
while [ "$#" -gt 0 ]; do
    case "$1" in
        --bin-dir) [ "$#" -ge 2 ] || exit 2; bindir=$2; shift 2 ;;
        --no-aliases) aliases=false; shift ;;
        --help) echo 'Usage: scripts/install.sh [--bin-dir DIR] [--no-aliases]'; exit 0 ;;
        *) echo "Unknown option: $1" >&2; exit 2 ;;
    esac
done
[ "$(uname -s)" = Linux ] || { echo 'pbotd execution requires Linux or WSL.' >&2; exit 1; }
case "$bindir" in /*) ;; *) bindir=$PWD/$bindir ;; esac
commands='qsub qstat qdel qhold qrls qalter qrerun pbsnodes'
if "$aliases"; then
    for name in $commands; do
        path=$bindir/$name
        if [ -e "$path" ] || [ -L "$path" ]; then
            if [ ! -L "$path" ] || [ "$(readlink "$path")" != pbotd ]; then
                echo "Refusing to replace $path; use --no-aliases or a different --bin-dir." >&2
                exit 1
            fi
        fi
    done
fi
mkdir -p "$bindir"
stage=$(mktemp -d "$bindir/.pbotd-install.XXXXXX")
trap 'rm -rf "$stage"' EXIT HUP INT TERM
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
CGO_ENABLED=0 go build -trimpath -o "$stage/pbotd" ./cmd/pbotd
chmod 755 "$stage/pbotd"
mv -f "$stage/pbotd" "$bindir/pbotd"
if "$aliases"; then
    for name in $commands; do
        [ -L "$bindir/$name" ] || ln -s pbotd "$bindir/$name"
    done
fi
echo "Installed pbotd in $bindir. Add that directory to PATH."
