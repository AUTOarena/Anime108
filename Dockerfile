FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go scraper.go hls_proxy.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /anime108 .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /anime108 /usr/local/bin/anime108
COPY templates ./templates
EXPOSE 5000
ENTRYPOINT ["anime108"]
