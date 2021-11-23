FROM golang:1.16 AS builder
WORKDIR /workspace
ADD . .
RUN CGO_ENABLED=0 go build -trimpath -o /app ./cmd/server

FROM gcr.io/distroless/static:nonroot
LABEL maintainer="Jian Zeng <anonymousknight96@gmail.com>"
COPY --from=builder /app /app
ENTRYPOINT ["/app"]
