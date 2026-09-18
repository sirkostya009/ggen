#!/usr/bin/env bash
# Downloads Go, Node, a JRE, kotlinc, kotlinx.serialization and Swift into a
# cache directory, then runs the gen tests and the differential tests with
# every toolchain-gated test enabled. Needs only bash, curl, tar and unzip (or
# python3) on Linux x86_64 or aarch64.
#
#   gen/test-toolchains.sh              set up and run everything
#   gen/test-toolchains.sh -run Swift   extra arguments go to `go test`
#
# GGEN_TOOLCHAINS picks the cache directory (default ~/.cache/ggen-toolchains).
set -euo pipefail

GO_VERSION=1.27.0
NODE_VERSION=24.16.0
JRE_VERSION=21.0.12.1+1
KOTLIN_VERSION=2.4.20
KOTLINX_VERSION=1.11.0
SWIFT_VERSION=6.3.3

UA='Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36'
root=$(cd "$(dirname "$0")/.." && pwd)
cache=${GGEN_TOOLCHAINS:-$HOME/.cache/ggen-toolchains}
downloads=$cache/downloads
mkdir -p "$downloads"

[[ $(uname -s) == Linux ]] || { echo "only Linux is supported" >&2; exit 1; }
case $(uname -m) in
x86_64) goarch=amd64 nodearch=x64 jrearch=x64 swiftsuffix= ;;
aarch64 | arm64) goarch=arm64 nodearch=arm64 jrearch=aarch64 swiftsuffix=-aarch64 ;;
*) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
for tool in curl tar; do
	command -v $tool >/dev/null || { echo "$tool is required" >&2; exit 1; }
done

# fetch URL NAME downloads URL into the downloads directory once and prints
# the file path.
fetch() {
	local file=$downloads/$2
	if [[ ! -s $file ]]; then
		echo "downloading $2" >&2
		curl -fsSL --retry 3 -A "$UA" -o "$file.part" "$1"
		mv "$file.part" "$file"
	fi
	echo "$file"
}

unpack_zip() {
	if command -v unzip >/dev/null; then
		unzip -q "$1" -d "$2"
	elif command -v python3 >/dev/null; then
		python3 -m zipfile -e "$1" "$2"
	else
		echo "unzip or python3 is required" >&2
		exit 1
	fi
}

# Go
goroot=$cache/go-$GO_VERSION
if [[ ! -x $goroot/bin/go ]]; then
	archive=$(fetch "https://go.dev/dl/go$GO_VERSION.linux-$goarch.tar.gz" "go-$GO_VERSION.tar.gz")
	mkdir -p "$goroot.part" && tar xzf "$archive" -C "$goroot.part" --strip-components=1 && mv "$goroot.part" "$goroot"
fi

# Node, then the pinned zod, valibot and typescript
node=$cache/node-$NODE_VERSION
if [[ ! -x $node/bin/node ]]; then
	archive=$(fetch "https://nodejs.org/dist/v$NODE_VERSION/node-v$NODE_VERSION-linux-$nodearch.tar.gz" "node-$NODE_VERSION.tar.gz")
	mkdir -p "$node.part" && tar xzf "$archive" -C "$node.part" --strip-components=1 && mv "$node.part" "$node"
fi
export PATH=$goroot/bin:$node/bin:$PATH
(cd "$root" && npm ci --no-audit --no-fund)

# JRE, kotlinc and the kotlinx.serialization jars, laid out as GGEN_KOTLIN_HOME
kotlin=$cache/kotlin-$KOTLIN_VERSION
if [[ ! -d $kotlin/kotlinc ]]; then
	mkdir -p "$kotlin.part"
	archive=$(fetch "https://api.adoptium.net/v3/binary/version/jdk-${JRE_VERSION/+/%2B}/linux/$jrearch/jre/hotspot/normal/eclipse" "jre-$JRE_VERSION.tar.gz")
	mkdir -p "$kotlin.part/jdk-$JRE_VERSION-jre" && tar xzf "$archive" -C "$kotlin.part/jdk-$JRE_VERSION-jre" --strip-components=1
	for part in core json; do
		jar=$(fetch "https://repo1.maven.org/maven2/org/jetbrains/kotlinx/kotlinx-serialization-$part-jvm/$KOTLINX_VERSION/kotlinx-serialization-$part-jvm-$KOTLINX_VERSION.jar" "kotlinx-serialization-$part-jvm-$KOTLINX_VERSION.jar")
		cp "$jar" "$kotlin.part/kotlinx-serialization-$part-jvm.jar"
	done
	archive=$(fetch "https://github.com/JetBrains/kotlin/releases/download/v$KOTLIN_VERSION/kotlin-compiler-$KOTLIN_VERSION.zip" "kotlin-compiler-$KOTLIN_VERSION.zip")
	unpack_zip "$archive" "$kotlin.part"
	chmod +x "$kotlin.part"/kotlinc/bin/*
	mv "$kotlin.part" "$kotlin"
fi

# Swift. The toolchain targets Ubuntu 24.04; distributions that ship only
# the wide ncurses library get a libncurses.so.6 link to it.
swift=$cache/swift-$SWIFT_VERSION
if [[ ! -d $swift/usr ]]; then
	archive=$(fetch "https://download.swift.org/swift-$SWIFT_VERSION-release/ubuntu2404$swiftsuffix/swift-$SWIFT_VERSION-RELEASE/swift-$SWIFT_VERSION-RELEASE-ubuntu24.04$swiftsuffix.tar.gz" "swift-$SWIFT_VERSION.tar.gz")
	mkdir -p "$swift.part" && tar xzf "$archive" -C "$swift.part" --strip-components=1 && mv "$swift.part" "$swift"
fi
mkdir -p "$swift/compat"
if ! ldconfig -p 2>/dev/null | grep -q 'libncurses\.so\.6 '; then
	for dir in /usr/lib /usr/lib64 /lib/x86_64-linux-gnu /usr/lib/x86_64-linux-gnu /lib/aarch64-linux-gnu /usr/lib/aarch64-linux-gnu; do
		if [[ -e $dir/libncursesw.so.6 ]]; then
			ln -sfn "$dir/libncursesw.so.6" "$swift/compat/libncurses.so.6"
			break
		fi
	done
fi
export LD_LIBRARY_PATH=$swift/compat${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}
if ! "$swift/usr/bin/swiftc" --version >/dev/null 2>&1; then
	echo "swiftc does not start; missing libraries:" >&2
	ldd "$swift/usr/bin/swiftc" "$swift/usr/bin/swift-frontend" | grep 'not found' >&2 || true
	exit 1
fi

export GGEN_NODE_MODULES=$root/node_modules
export GGEN_KOTLIN_HOME=$kotlin
export GGEN_SWIFTC=$swift/usr/bin/swiftc
export PATH=$GGEN_NODE_MODULES/.bin:$PATH
unset GTK_IM_MODULE_FILE

go version
node --version
tsc --version
"$GGEN_SWIFTC" --version | head -1
JAVA_HOME=$(echo "$kotlin"/jdk*) "$kotlin/kotlinc/bin/kotlinc" -version 2>&1 | tail -1

# Every lane skips when its toolchain is missing, and this script installs them
# all, so a skip here means a lane silently stopped running.
extra=("$@")
log=$(mktemp)
trap 'rm -f "$log"' EXIT
status=0
for target in "$root/gen ./..." "$root/integrationtests ./gen/"; do
	set -- $target
	(cd "$1" && go test -count=1 -json "${extra[@]}" "$2" >"$log") || status=1
	python3 -c '
import json, sys

skipped = []
for line in open(sys.argv[1]):
    if not line.startswith("{"):
        sys.stdout.write(line)
        continue
    event = json.loads(line)
    if event["Action"] == "output":
        sys.stdout.write(event["Output"])
    elif event["Action"] == "skip" and event.get("Test"):
        skipped.append(event["Package"] + "." + event["Test"])
for s in skipped:
    print("skipped although every toolchain is installed:", s)
sys.exit(1 if skipped else 0)' "$log" || status=1
done
exit $status
