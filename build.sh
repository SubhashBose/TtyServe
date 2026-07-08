# Stamp the build date into the binary (shown by --version).
BUILD_DATE=$(date -u +%Y-%m-%d)
DATE_LDFLAG="-X main.buildDate=${BUILD_DATE}"

go build --ldflags "$DATE_LDFLAG" ./cmd/ttyserve


for GOOS in linux windows darwin freebsd; do
  for GOARCH in amd64 arm64; do
    # Skip unsupported combinations
    [[ "$GOOS" == "freebsd" && "$GOARCH" == "arm64" ]] && continue

    EXT=""
    [[ "$GOOS" == "windows" ]] && EXT=".exe"

    OUTPUT="ttyserve-${GOOS}-${GOARCH}${EXT}"

    CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build -o "$OUTPUT" -trimpath --ldflags "-s -w -buildid= ${DATE_LDFLAG}" ./cmd/ttyserve
    echo "Built: $OUTPUT"
  done
done
