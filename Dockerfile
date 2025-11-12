FROM golang:1.25.2-alpine AS build
RUN apk add --no-cache curl libstdc++ libgcc npm

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN npm install && \
    go install github.com/a-h/templ/cmd/templ@latest

RUN templ generate && \
    curl -sL https://github.com/tailwindlabs/tailwindcss/releases/latest/download/tailwindcss-linux-x64-musl -o tailwindcss && \
    chmod +x tailwindcss && \
    ./tailwindcss -i tailwind.css -o internal/views/assets/css/output.css -m

RUN go build -o main cmd/api/main.go

FROM alpine:3.20.1 AS prod
WORKDIR /app
COPY --from=build /app/main /app/main
EXPOSE ${PORT}
CMD ["./main"]


