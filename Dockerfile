# Pin digest for reproducible builds. Update via: docker pull gcr.io/distroless/static:nonroot && docker inspect gcr.io/distroless/static:nonroot --format='{{index .RepoDigests 0}}'
FROM gcr.io/distroless/static:nonroot@sha256:d29e660cc75a5b6b1334e03c5c81ccf9bc0884a002c6000dbf0fb96034814478
COPY hyperping-exporter /hyperping-exporter
EXPOSE 9312
USER nonroot:nonroot
ENTRYPOINT ["/hyperping-exporter"]
CMD ["--listen-address=:9312", "--metrics-path=/metrics"]
