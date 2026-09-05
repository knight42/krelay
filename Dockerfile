FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /krelay-server ./cmd/krelay-server

FROM alpine:3.21
RUN apk add --no-cache bash util-linux-misc && adduser -D -u 65532 nonroot
COPY --from=builder /krelay-server /krelay-server
USER nonroot
ENTRYPOINT ["/krelay-server"]
