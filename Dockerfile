FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tracker ./cmd/tracker

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /tracker /tracker
ENV LISTEN_ADDRESS=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["/tracker"]
CMD ["serve"]
