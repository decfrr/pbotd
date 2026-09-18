FROM golang:1.27.1
RUN apt-get update && apt-get install -y --no-install-recommends systemd systemd-sysv dbus dbus-user-session \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 --shell /bin/bash pbotd-test
ENV container=docker
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
