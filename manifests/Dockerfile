FROM golang:1.17 AS builder
WORKDIR /workspace
ADD . .
RUN make krelay-server

FROM gcr.io/distroless/static:nonroot
LABEL maintainer="Jian Zeng <anonymousknight96@gmail.com>"
COPY --from=builder /workspace/krelay-server /server
ENTRYPOINT ["/server"]
