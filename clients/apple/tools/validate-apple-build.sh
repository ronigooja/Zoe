#!/usr/bin/env bash
set -u

usage() {
    cat <<'USAGE'
Usage:
  validate-apple-build.sh \
    --approved-root PATH \
    --output-root PATH \
    --package-cache-seed PATH

Create a clean source snapshot and run Apple checks with all generated files
under --output-root. Both --output-root and --package-cache-seed must be below
--approved-root. The seed must contain SwiftPM artifacts, checkouts, and
repositories for the pinned WebRTC dependency. The seed is read only; its
contents are copied into the new output directory. No Keychain access is
needed when the seed contains the pinned artifact.

The checks run Swift package tests and an unsigned macOS Xcode app build.
USAGE
}

fail() {
    printf 'ERROR: %s\n' "$*" >&2
    exit 2
}

APPROVED_ROOT=
OUTPUT_ROOT=
PACKAGE_CACHE_SEED=

while [ "$#" -gt 0 ]; do
    case "$1" in
        --approved-root)
            [ "$#" -ge 2 ] || fail "--approved-root needs a path"
            APPROVED_ROOT=$2
            shift 2
            ;;
        --output-root)
            [ "$#" -ge 2 ] || fail "--output-root needs a path"
            OUTPUT_ROOT=$2
            shift 2
            ;;
        --package-cache-seed)
            [ "$#" -ge 2 ] || fail "--package-cache-seed needs a path"
            PACKAGE_CACHE_SEED=$2
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            fail "unknown argument: $1"
            ;;
    esac
done

[ -n "$APPROVED_ROOT" ] || fail "--approved-root is required"
[ -n "$OUTPUT_ROOT" ] || fail "--output-root is required"
[ -n "$PACKAGE_CACHE_SEED" ] || fail "--package-cache-seed is required"
[ "$(uname -s)" = Darwin ] || fail "run this tool on macOS"
[ -d "$APPROVED_ROOT" ] || fail "approved root must already exist"
[ -d "$PACKAGE_CACHE_SEED" ] || fail "package-cache seed must already exist"

APPROVED_ROOT=$(cd "$APPROVED_ROOT" && pwd -P)
PACKAGE_CACHE_SEED=$(cd "$PACKAGE_CACHE_SEED" && pwd -P)
OUTPUT_PARENT=$(dirname "$OUTPUT_ROOT")
OUTPUT_NAME=$(basename "$OUTPUT_ROOT")
[ -d "$OUTPUT_PARENT" ] || fail "output parent must already exist"
OUTPUT_ROOT="$(cd "$OUTPUT_PARENT" && pwd -P)/$OUTPUT_NAME"

case "$OUTPUT_ROOT" in
    "$APPROVED_ROOT"/*) ;;
    *) fail "output root must be below the approved root" ;;
esac
case "$PACKAGE_CACHE_SEED" in
    "$APPROVED_ROOT"/*) ;;
    *) fail "package-cache seed must be below the approved root" ;;
esac
[ ! -e "$OUTPUT_ROOT" ] && [ ! -L "$OUTPUT_ROOT" ] || fail "output root already exists; choose a new task directory"

for seed_item in artifacts checkouts repositories; do
    [ -d "$PACKAGE_CACHE_SEED/$seed_item" ] || fail "package-cache seed is missing $seed_item"
done
[ -d "$PACKAGE_CACHE_SEED/artifacts/webrtc/WebRTC/WebRTC.xcframework" ] || fail "package-cache seed is missing the pinned WebRTC artifact"

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd -P)
APPLE_ROOT=$(cd "$SCRIPT_DIR/.." && pwd -P)
REPOSITORY_ROOT=$(git -C "$APPLE_ROOT" rev-parse --show-toplevel 2>/dev/null) || fail "Apple source is not in a Git worktree"
[ -z "$(git -C "$REPOSITORY_ROOT" status --porcelain -- clients/apple)" ] || fail "clients/apple has uncommitted or untracked changes; commit the source before making a snapshot"
SOURCE_REVISION=$(git -C "$REPOSITORY_ROOT" rev-parse HEAD)

mkdir -p "$OUTPUT_ROOT"
mkdir -p "$OUTPUT_ROOT/source"
git -C "$REPOSITORY_ROOT" archive "$SOURCE_REVISION" clients/apple | tar -x -C "$OUTPUT_ROOT/source" || fail "could not create source snapshot"
SOURCE_APPLE_ROOT="$OUTPUT_ROOT/source/clients/apple"
PACKAGE_ROOT="$SOURCE_APPLE_ROOT/Packages/ZoeKit"
REPORT="$OUTPUT_ROOT/validation-report.txt"

record() {
    printf '%s\n' "$*" | tee -a "$REPORT"
}

run_logged() {
    step_name=$1
    log_path=$2
    shift 2
    record "RUN $step_name"
    if "$@" >"$log_path" 2>&1; then
        record "PASS $step_name"
        return 0
    else
        step_status=$?
        tail -n 60 "$log_path"
        record "COMMAND_EXIT_NONZERO $step_name (exit $step_status; log: $log_path)"
        return "$step_status"
    fi
}

seed_swiftpm() {
    seed_destination=$1
    mkdir -p "$seed_destination"
    for seed_item in artifacts checkouts repositories; do
        ditto "$PACKAGE_CACHE_SEED/$seed_item" "$seed_destination/$seed_item"
    done
    if [ -f "$PACKAGE_CACHE_SEED/workspace-state.json" ]; then
        cp "$PACKAGE_CACHE_SEED/workspace-state.json" "$seed_destination/workspace-state.json"
    fi
}

record "SOURCE_REVISION=$SOURCE_REVISION"
record "OUTPUT_ROOT=$OUTPUT_ROOT"
record "XCODE=$(xcodebuild -version | tr '\n' ' ')"
record "SWIFT=$(swift --version | head -n 1)"
SWIFT_TEST_ROOT="$OUTPUT_ROOT/swift-test"
seed_swiftpm "$SWIFT_TEST_ROOT"
mkdir -p "$OUTPUT_ROOT/swift-cache" "$OUTPUT_ROOT/swift-config" "$OUTPUT_ROOT/swift-security"
cd "$PACKAGE_ROOT" || fail "could not enter package snapshot"

if ! run_logged "swift package tests" "$OUTPUT_ROOT/swift-test.log" \
    swift test --skip-update \
    --scratch-path "$SWIFT_TEST_ROOT" \
    --cache-path "$OUTPUT_ROOT/swift-cache" \
    --config-path "$OUTPUT_ROOT/swift-config" \
    --security-path "$OUTPUT_ROOT/swift-security" \
    --manifest-cache local; then
    record "FAIL swift package tests"
    exit 1
fi

XCODE_PACKAGE_ROOT="$OUTPUT_ROOT/xcode-source-packages"
seed_swiftpm "$XCODE_PACKAGE_ROOT"
mkdir -p "$OUTPUT_ROOT/package-cache"
if ! run_logged "unsigned universal macOS app build" "$OUTPUT_ROOT/macos-xcodebuild.log" \
    xcodebuild -quiet \
    -project "$SOURCE_APPLE_ROOT/Zoe.xcodeproj" \
    -scheme ZoeMac \
    -destination "generic/platform=macOS" \
    -derivedDataPath "$OUTPUT_ROOT/macos-derived" \
    -clonedSourcePackagesDirPath "$XCODE_PACKAGE_ROOT" \
    -packageCachePath "$OUTPUT_ROOT/package-cache" \
    -resultBundlePath "$OUTPUT_ROOT/macos-app-target.xcresult" \
    CODE_SIGNING_ALLOWED=NO build; then
    record "FAIL unsigned universal macOS app build"
    exit 1
fi

record "COMPLETE shared tests and macOS app build passed"
record "REPORT=$REPORT"
