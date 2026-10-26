FROM golang:1.25.8-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY migrations migrations
RUN CGO_ENABLED=0 go build -trimpath -o /authorizer .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /authorizer /authorizer
EXPOSE 8080 8000
HEALTHCHECK --interval=3s --timeout=3s --retries=30 CMD ["/authorizer", "health"]
ENTRYPOINT ["/authorizer"]
