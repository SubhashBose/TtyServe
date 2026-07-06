go build ./cmd/ttyserve


for GOOS in linux windows darwin freebsd; do
  for GOARCH in amd64 arm64; do
    # Skip unsupported combinations
    [[ "$GOOS" == "freebsd" && "$GOARCH" == "arm64" ]] && continue

    EXT=""
    [[ "$GOOS" == "windows" ]] && EXT=".exe"

    OUTPUT="ttyserve-${GOOS}-${GOARCH}${EXT}"

    GOOS=$GOOS GOARCH=$GOARCH go build -o "$OUTPUT" -trimpath --ldflags "-s -w -buildid=" ./cmd/ttyserve
    echo "Built: $OUTPUT"
  done
done