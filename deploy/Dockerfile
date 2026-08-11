FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -p=1 -trimpath -ldflags='-s -w' -o /out/pasta . && mkdir /out/data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --chown=nonroot:nonroot --from=build /out/data /data
COPY --from=build /out/pasta /pasta
VOLUME ["/data"]
ENV PASTA_ADDR=:8080 PASTA_DATA_DIR=/data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/pasta"]
