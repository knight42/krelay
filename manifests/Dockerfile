FROM golang:1.19 AS builder
WORKDIR /workspace
ADD . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-w -s' -o krelay-server ./cmd/server

FROM gcr.io/distroless/static:nonroot
LABEL maintainer="Jian Zeng <anonymousknight96@gmail.com>"
COPY --from=builder /workspace/krelay-server /server
ENTRYPOINT ["/server"]
