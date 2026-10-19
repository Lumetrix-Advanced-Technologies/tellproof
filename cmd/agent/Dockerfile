# The evidence agent as a container (S-5). Build from the repository root:
#   docker build -f cmd/agent/Dockerfile -t tellproof-agent .
# Run on its own, with a dashboard on http://127.0.0.1:4319:
#   docker run -d --name tellproof-agent -p 127.0.0.1:4318:4318 -p 127.0.0.1:4319:4319 \
#     -v tellproof-agent:/data tellproof-agent run --local
# Run (the Tellproof app shows this line with the token filled in):
#   docker run -d --name tellproof-agent --restart unless-stopped -p 127.0.0.1:4318:4318 \
#     -v tellproof-agent:/data -e TELLPROOF_SERVER=https://<server> tellproof-agent connect <token>
# The build stage runs on the builder's platform and cross-compiles, so a
# multi-platform build (--platform linux/amd64,linux/arm64) needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS build
ARG VERSION=dev
ARG TAGS=
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -tags "${TAGS}" -ldflags "-s -w -X main.version=${VERSION}" -o /out/tellproof-agent ./cmd/agent \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/tellproof-agent /tellproof-agent
COPY --from=build --chown=65532:65532 /out/data /data
# Inside the container the receiver (4318) and, in local mode, the dashboard
# (4319) listen on all interfaces; publish them on the host's loopback only
# (-p 127.0.0.1:4318:4318 -p 127.0.0.1:4319:4319) unless the app runs elsewhere.
ENV TELLPROOF_DATA_DIR=/data TELLPROOF_OTLP_LISTEN=0.0.0.0:4318 TELLPROOF_DASHBOARD_LISTEN=0.0.0.0:4319 TELLPROOF_OTLP_ALLOW_REMOTE=1
VOLUME /data
EXPOSE 4318 4319
USER nonroot:nonroot
ENTRYPOINT ["/tellproof-agent"]
CMD ["run"]
