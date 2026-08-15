FROM golang:1.25.13-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/workspace-brain ./cmd/workspace-brain

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/workspace-brain /workspace-brain
USER nonroot:nonroot
EXPOSE 8080
ENV ADDR=:8080
# WB-12: self-probe healthcheck using the binary's own "healthcheck" subcommand.
# The binary reads ADDR from the environment and probes the /readyz endpoint.
# distroless/static has no shell, so EXEC form is required (no shell expansion).
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD ["/workspace-brain", "healthcheck"]
ENTRYPOINT ["/workspace-brain"]
