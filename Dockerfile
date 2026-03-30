FROM gcr.io/distroless/static:nonroot
COPY hyperping-exporter /hyperping-exporter
EXPOSE 9312
USER nonroot:nonroot
ENTRYPOINT ["/hyperping-exporter"]
CMD ["--listen-address=:9312", "--metrics-path=/metrics"]
