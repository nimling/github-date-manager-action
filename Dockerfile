FROM golang:1.24-alpine as builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o date-manager .

FROM alpine:3.21

RUN apk --no-cache add ca-certificates
WORKDIR /app

COPY --from=builder /app/date-manager .

RUN adduser -D appuser
USER appuser

ENTRYPOINT ["./date-manager"]