# Build stage
FROM golang:1.27-alpine AS build
RUN apk add --no-cache make
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make build

# Runtime stage
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /src/bin/homelab-bot /homelab-bot
EXPOSE 8080
ENTRYPOINT ["/homelab-bot"]
