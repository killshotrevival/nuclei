# Build
FROM --platform=linux/amd64 golang:1.20.3-alpine AS build-env
RUN apk add build-base
WORKDIR /app
COPY . /app
WORKDIR /app/v2
RUN go mod download
RUN CGO_ENABLED=0 go build ./cmd/nuclei

# Release
FROM --platform=linux/amd64 alpine:3.17.3
RUN apk -U upgrade --no-cache \
    && apk add --no-cache bind-tools chromium ca-certificates
COPY --from=build-env /app/v2/nuclei /usr/local/bin/

ENTRYPOINT ["nuclei"]