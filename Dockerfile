# --- build stage ---
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
# No external deps yet; if added, COPY go.sum and run `go mod download`.
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/llmux ./cmd/llmux

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/llmux /llmux
EXPOSE 4000
ENV LLMUX_ADDR=:4000
ENTRYPOINT ["/llmux"]
