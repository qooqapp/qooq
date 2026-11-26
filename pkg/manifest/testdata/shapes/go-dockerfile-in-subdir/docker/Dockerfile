FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
RUN go build -o /app ./...

FROM alpine:3.20
COPY --from=build /app /app
EXPOSE 9090
ENTRYPOINT ["/app"]
