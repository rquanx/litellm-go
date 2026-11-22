FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/minato-runtime ./cmd/minato-runtime

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/minato-runtime /minato-runtime
EXPOSE 4300
ENTRYPOINT ["/minato-runtime"]
