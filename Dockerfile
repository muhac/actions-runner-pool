FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gharp ./cmd/gharp

FROM alpine:3.20
RUN apk add --no-cache ca-certificates docker-cli
WORKDIR /app
COPY --from=build /out/gharp /usr/local/bin/gharp
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gharp"]
