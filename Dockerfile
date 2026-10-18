# Dell SC Series CSP for the HPE CSI Driver (Go). Static binary on distroless, runs as nonroot (65532).
# Build: docker build --build-arg VERSION=<v> -t ghcr.io/kube-dc/scseries-csp:<v> .
# Without a container engine: hack/build-image.sh (go build + crane) produces the same image.
FROM docker.io/library/golang:1.27.2 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /scseries-csp ./cmd/scseries-csp

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /scseries-csp /scseries-csp
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/scseries-csp"]
CMD ["serve"]
