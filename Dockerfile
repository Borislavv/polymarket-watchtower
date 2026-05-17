FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/watchtower ./cmd/app

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/watchtower /watchtower
USER nonroot:nonroot
EXPOSE 9090
ENTRYPOINT ["/watchtower"]
