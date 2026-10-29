FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /catalogue ./cmd/catalogue

FROM golang:1.26-alpine AS test
RUN apk add --no-cache gcc musl-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
CMD ["go", "test", "-race", "-count=1", "./..."]

FROM alpine:3.23 AS runtime
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /catalogue /app/catalogue
COPY web /app/web
USER app
EXPOSE 8080 8081
ENTRYPOINT ["/app/catalogue"]
