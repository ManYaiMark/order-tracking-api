FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -o /api .

FROM alpine:3.20
COPY --from=build /api /api
CMD ["/api"]