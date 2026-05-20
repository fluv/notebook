FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/notebook ./

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/notebook /notebook
USER nonroot:nonroot
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/notebook"]
