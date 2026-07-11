# Build stage: compile a static binary (CGO off, required for the distroless
# static runtime).
FROM golang:1.26 AS build
WORKDIR /src

# Module cache layer: only invalidated when the module graph changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/yaad-grove ./cmd/yaad-grove

# Runtime stage: distroless static, non-root. No shell, no package manager —
# minimal attack surface for a network-facing bot.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/yaad-grove /usr/local/bin/yaad-grove
ENTRYPOINT ["/usr/local/bin/yaad-grove"]
