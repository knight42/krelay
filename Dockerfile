FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /krelay-server ./cmd/krelay-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /krelay-server /krelay-server
ENTRYPOINT ["/krelay-server"]
