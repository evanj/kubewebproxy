FROM golang:1.18-bullseye AS cached_modules
COPY go.mod go.sum /go/src/kubewebproxy/
WORKDIR /go/src/kubewebproxy
# TODO: Figure out how to compile cached modules in a smarter way?
RUN go mod download && go build -v \
    github.com/evanj/googlesignin/iap \
    golang.org/x/net/html \
    k8s.io/client-go/kubernetes \
    k8s.io/client-go/rest


FROM cached_modules AS builder
COPY kubewebproxy.go /go/src/kubewebproxy/
RUN go build -v kubewebproxy.go


FROM gcr.io/distroless/base-debian11:nonroot AS run
COPY --from=builder /go/src/kubewebproxy/kubewebproxy /kubewebproxy

# Use a non-root user: slightly more secure (defense in depth)
USER nobody
WORKDIR /
EXPOSE 8080
ENTRYPOINT ["/kubewebproxy"]
