# One image serves any content repo; configure it with environment variables.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /trunkcms ./cmd/trunkcms

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /trunkcms /trunkcms
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/trunkcms"]
