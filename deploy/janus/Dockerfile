FROM mwalbeck/janus-gateway:1.4.1

USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*

USER janus:janus
