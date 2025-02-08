FROM golang:1.23.5-alpine as builder

WORKDIR /app

COPY . .

RUN go mod tidy && CGO_ENABLED=0 go build -a -o nodeWatcher main.go

FROM alpine:latest

WORKDIR /root/

COPY --from=builder /app/nodeWatcher ./