FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN go build -o /notify-bot .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /notify-bot /usr/local/bin/notify-bot
WORKDIR /data
EXPOSE 9119
ENTRYPOINT ["notify-bot"]
