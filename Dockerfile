FROM ubuntu:mantic
RUN apt update && apt install -y ca-certificates
COPY mqtt_blackbox_exporter /bin/mqtt_blackbox_exporter
EXPOSE 9214
ENTRYPOINT ["/bin/mqtt_blackbox_exporter"]
CMD ["-config.file /config.yaml"]
