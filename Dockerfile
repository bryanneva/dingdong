FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/dingdong .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dingdong /usr/local/bin/dingdong
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/dingdong"]
