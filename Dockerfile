FROM golang:1.26-alpine AS build
ARG VERSION=devel
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/muhammetsafak/egresszero/internal/version.version=${VERSION}" \
    -o /out/egresszero ./cmd/egresszero

# distroless/static instead of scratch: ships the CA bundle (required
# for TLS to S3), tzdata and a nonroot user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/egresszero /egresszero
EXPOSE 8080
ENTRYPOINT ["/egresszero"]
