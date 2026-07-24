# Standalone image for the smoke suite's record/replay MITM proxy. Deliberately
# separate from the main router Dockerfile (no CGO/ONNX toolchain needed here).
FROM golang:1.25.9-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY smoke/mitmproxy/ ./smoke/mitmproxy/
RUN CGO_ENABLED=0 go build -o /mitmproxy ./smoke/mitmproxy/

FROM gcr.io/distroless/static-debian12
COPY --from=build /mitmproxy /mitmproxy
ENTRYPOINT ["/mitmproxy"]
