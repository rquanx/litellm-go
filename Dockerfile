FROM golang:1.24-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN GOPROXY=$GOPROXY CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/litellm-go ./cmd/minato-runtime

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/litellm-go /litellm-go
COPY upstream/LICENSE.litellm /licenses/LICENSE.litellm
EXPOSE 4000 4300
ENTRYPOINT ["/litellm-go"]
