FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/case-api ./cmd/server

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/case-api /case-api
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/case-api"]
