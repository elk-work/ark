# Builds the sync service (cmd/ark-server). Used by Cloud Run source deploys.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /ark-server ./cmd/ark-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ark-server /ark-server
ENTRYPOINT ["/ark-server"]
