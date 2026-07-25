# syntax=docker/dockerfile:1

# Keep the default in sync with .go-version.
ARG GO_VERSION=1.26.5

FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
	-trimpath \
	-buildvcs=false \
	-ldflags="-s -w -X github.com/thevilledev/sonnetbox/internal/cli.releaseVersion=${VERSION}" \
	-o /out/sonnetbox \
	./cmd/sonnetbox \
	&& printf 'nobody:x:65534:65534:nobody:/:\n' > /out/passwd

FROM scratch

COPY --from=build /out/passwd /etc/passwd
COPY --from=build /out/sonnetbox /sonnetbox

USER 65534:65534
ENTRYPOINT ["/sonnetbox"]
