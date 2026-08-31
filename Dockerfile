FROM golang@sha256:1e0126852075c9c60731c8ba49088448b91f63e2aed97ca9d1a9791622a05946 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /asset-api ./cmd/server \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /asset-migrate ./cmd/migrate \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /asset-derivative-worker ./cmd/derivative-worker \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /asset-retention-worker ./cmd/retention-worker

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /asset-api /asset-api
COPY --from=build /asset-migrate /asset-migrate
COPY --from=build /asset-derivative-worker /asset-derivative-worker
COPY --from=build /asset-retention-worker /asset-retention-worker
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/asset-api"]
