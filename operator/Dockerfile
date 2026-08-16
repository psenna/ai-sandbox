# Cross-compiled: the build stage always runs natively on BUILDPLATFORM and
# emits a static binary for TARGETOS/TARGETARCH, so linux/arm64 needs no QEMU
# (the runtime stage has no RUN instruction at all).
ARG GOPROXY=https://proxy.golang.org,direct
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG TARGETOS
ARG TARGETARCH
ARG GOPROXY
ENV GOPROXY=${GOPROXY} GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags "-s -w" -o /out/manager ./cmd

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
