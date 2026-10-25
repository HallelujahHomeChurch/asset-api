FROM golang:1.26.10-bookworm@sha256:5b1f78db1f54cc8cb46e652c51d3f854441c4310a449161b6fe9fb553edab505 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN --mount=type=cache,id=asset-go-build,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /asset-api ./cmd/server \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /asset-migrate ./cmd/migrate \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /asset-derivative-worker ./cmd/derivative-worker \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /asset-scan-warmer ./cmd/scan-warmer \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /asset-retention-worker ./cmd/retention-worker

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /asset-api /asset-api
COPY --from=build /asset-migrate /asset-migrate
COPY --from=build /asset-derivative-worker /asset-derivative-worker
COPY --from=build /asset-scan-warmer /asset-scan-warmer
COPY --from=build /asset-retention-worker /asset-retention-worker
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/asset-api"]
