# Pin digest for reproducible builds. Update via: docker pull gcr.io/distroless/static:nonroot && docker inspect gcr.io/distroless/static:nonroot --format='{{index .RepoDigests 0}}'
FROM gcr.io/distroless/static:nonroot@sha256:e3f945647ffb95b5839c07038d64f9811adf17308b9121d8a2b87b6a22a80a39
COPY hyperping-exporter /hyperping-exporter
EXPOSE 9312
USER nonroot:nonroot
ENTRYPOINT ["/hyperping-exporter"]
CMD ["--listen-address=:9312", "--metrics-path=/metrics"]
