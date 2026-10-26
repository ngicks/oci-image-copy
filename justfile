# cstorage imports go.podman.io/storage, whose btrfs driver needs C headers unless excluded,
# and its tests use copy.Image, which needs gpgme unless openpgp is selected.
tags := "exclude_graphdriver_btrfs containers_image_openpgp"

# Build pkg into bin/ without cgo; cstorage behaves the same, it uses no cgo-only feature.
build-static pkg:
    CGO_ENABLED=0 go build -tags '{{tags}}' -o bin/ {{pkg}}

# Build pkg into bin/ with cgo, linking the system libc dynamically.
build-dynamic pkg:
    CGO_ENABLED=1 go build -tags '{{tags}}' -o bin/ {{pkg}}

test *args:
    go test -tags '{{tags}}' {{args}} ./...

vet:
    go vet -tags '{{tags}}' -vettool="$(command -v ngcheckers)" ./...

lint:
    golangci-lint run ./...
