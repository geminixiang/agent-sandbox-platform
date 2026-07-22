# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.25.1-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY apps/control-plane-go ./apps/control-plane-go
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/control-plane ./apps/control-plane-go/cmd/control-plane

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/control-plane /control-plane
USER 65532:65532
EXPOSE 8787
ENTRYPOINT ["/control-plane"]
