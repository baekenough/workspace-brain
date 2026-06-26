FROM golang:1.25.11-bookworm AS build
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
# TODO(WB-12-followup): HEALTHCHECK via self-probe subcommand
ENTRYPOINT ["/workspace-brain"]
