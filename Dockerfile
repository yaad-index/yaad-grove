# Build stage: compile a static binary (CGO off, required for the distroless
# static runtime).
FROM golang:1.26 AS build
WORKDIR /src

# Module cache layer: only invalidated when the module graph changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/yaad-grove ./cmd/yaad-grove

# Stage an empty /data here: distroless has no shell to mkdir at runtime, so the
# writable data dir (for the default relative-path stores; see WORKDIR below) is
# created in the builder and copied in owned by the non-root user.
RUN mkdir -p /out/data

# Runtime stage: distroless static, non-root. No shell, no package manager —
# minimal attack surface for a network-facing bot.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/yaad-grove /usr/local/bin/yaad-grove
COPY --from=build --chown=65532:65532 /out/data /data
# The default stores are relative paths (budget.db / acl.db / callbacks.db), so
# default the working dir to the writable /data — they persist there without a -w
# flag. An absolute --store-path is unaffected.
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/yaad-grove"]
