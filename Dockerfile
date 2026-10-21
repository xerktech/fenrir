FROM golang:1.26 AS builder
ARG CGO_ENABLED=0
ARG APP_NAME
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN go build -o /app/$APP_NAME ./cmd/$APP_NAME

# Second stage: minimal runtime
FROM alpine:3.24 AS output
WORKDIR /app

ARG APP_NAME
COPY --from=builder --chmod=755 /app/${APP_NAME} /app/${APP_NAME}

# The binary is fixed at build time. Never pick it from the environment at
# run time: wolf-agent's container takes env from a User's sidecar policy.
RUN ln -s /app/${APP_NAME} /app/entrypoint
ENTRYPOINT ["/app/entrypoint"]
CMD []
