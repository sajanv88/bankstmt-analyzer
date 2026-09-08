# Cross-compiling build stage: it runs on the builder's native platform and
# targets whatever buildx asks for, so building the arm64 image on an amd64
# runner needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

# Set by buildx for each target platform.
ARG TARGETOS
ARG TARGETARCH
# Stamped into the binary and reported by --version.
ARG VERSION=dev

WORKDIR /src

# Dependencies are copied first so the module cache layer survives any
# change that does not touch go.mod or go.sum.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off gives a wholly static binary, which is what the distroless static
# base can run. -trimpath keeps build paths out of it, and -s -w drop the
# symbol and DWARF tables.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/bankstmt-analyzer ./cmd/api

# distroless/static carries CA certificates and nothing else: no shell, no
# package manager, nothing for an attacker who reaches the container to
# work with. The nonroot variant runs as uid 65532.
FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/bankstmt-analyzer /usr/local/bin/bankstmt-analyzer

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/bankstmt-analyzer"]
