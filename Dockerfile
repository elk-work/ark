# Builds the sync service (cmd/ark-server). Used by Cloud Run source deploys.

FROM golang:1.26 AS build
# Pass the release stamp with:
#   docker build --build-arg VERSION="$(git describe --tags --always --dirty)" .
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /ark-server ./cmd/ark-server

# Not distroless any more: Git hosting (elk-work/ark#125) runs the git binary
# for every clone, fetch and push — upload-pack, receive-pack, and the
# mirror's `git push --mirror` over HTTPS — so the runtime image carries git
# and the CA roots it verifies GitHub with. Still a non-root user, with the
# same uid distroless's nonroot had.
FROM alpine:3.20
RUN apk add --no-cache git ca-certificates \
 && adduser -D -u 65532 -h /home/nonroot nonroot
COPY --from=build /ark-server /ark-server
USER 65532:65532
# DATA_DIR and GIT_DATA_DIR default to paths relative to here, so it has to be
# somewhere the service may write.
WORKDIR /home/nonroot
ENTRYPOINT ["/ark-server"]
