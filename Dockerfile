FROM golang:1.24-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN GOPROXY=$GOPROXY CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/litellm-go ./cmd/minato-runtime

FROM gcr.io/distroless/static-debian12:nonroot AS binary
COPY --from=build /out/litellm-go /litellm-go
COPY upstream/LICENSE.litellm /licenses/LICENSE.litellm
EXPOSE 4000 4300
ENTRYPOINT ["/litellm-go"]

FROM ghcr.io/berriai/litellm:v1.85.0@sha256:2e8517e2bed423c50ab7e40fb1ac0a9cbe62a764e9d65161d871fb6a9bf75a2d AS dashboard-assets

FROM binary AS runtime

FROM runtime AS dashboard
COPY --from=dashboard-assets /app/.venv/lib/python3.13/site-packages/litellm/proxy/_experimental/out /dashboard
COPY --from=dashboard-assets /app/.venv/lib/python3.13/site-packages/litellm/proxy/logo.jpg /dashboard/logo.jpg
ENV LITELLM_GO_DASHBOARD_DIR=/dashboard
