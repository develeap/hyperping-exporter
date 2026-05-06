# Pin digest for reproducible builds. Update via: docker pull gcr.io/distroless/static:nonroot && docker inspect gcr.io/distroless/static:nonroot --format='{{index .RepoDigests 0}}'
FROM gcr.io/distroless/static:nonroot@sha256:e3f945647ffb95b5839c07038d64f9811adf17308b9121d8a2b87b6a22a80a39
# TARGETPLATFORM is auto-set by Docker buildx during multi-arch builds (e.g. linux/amd64).
# GoReleaser stages binaries at <platform>/hyperping-exporter in the build context, so the
# default BIN_PATH below evaluates correctly under buildx without any extra build-args.
# Local `make docker-build` overrides BIN_PATH to point at the binary at the project root.
ARG TARGETPLATFORM
ARG BIN_PATH=${TARGETPLATFORM}/hyperping-exporter
COPY ${BIN_PATH} /hyperping-exporter
EXPOSE 9312
USER nonroot:nonroot
ENTRYPOINT ["/hyperping-exporter"]
CMD ["--listen-address=:9312", "--metrics-path=/metrics"]
