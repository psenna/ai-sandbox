# The doubles module has zero third-party dependencies (see go.mod), so this
# build runs with GOPROXY=off -- no DependaProxy involvement at all. Never
# pushed anywhere; only `kind load docker-image`ed by hack/e2e-up.sh.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN GOPROXY=off CGO_ENABLED=0 go build -trimpath -o /out/doubles .

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/doubles /doubles
USER 65532:65532
ENTRYPOINT ["/doubles"]
