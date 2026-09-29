# Throwaway SSH target for the term-transfer module tests: sshd + lrzsz (rz/sz) + trzsz-go (trz/tsz) + tmux.
FROM debian:bookworm-slim
ARG TRZSZ_VERSION=1.2.0
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server lrzsz tmux ca-certificates curl bash coreutils procps \
 && arch="$(uname -m)"; case "$arch" in aarch64|arm64) a=aarch64;; x86_64|amd64) a=x86_64;; *) a="$arch";; esac \
 && curl -fsSL "https://github.com/trzsz/trzsz-go/releases/download/v${TRZSZ_VERSION}/trzsz_${TRZSZ_VERSION}_linux_${a}.tar.gz" -o /tmp/trzsz.tgz \
 && mkdir -p /tmp/trzsz && tar -xzf /tmp/trzsz.tgz -C /tmp/trzsz \
 && find /tmp/trzsz -type f \( -name trz -o -name tsz -o -name trzsz \) -exec install -m 0755 {} /usr/local/bin/ \; \
 && rm -rf /tmp/trzsz /tmp/trzsz.tgz /var/lib/apt/lists/* \
 && useradd -m -s /bin/bash test && echo 'test:test' | chpasswd \
 && mkdir -p /run/sshd \
 && printf 'PasswordAuthentication yes\nKbdInteractiveAuthentication no\nUsePAM yes\nPermitRootLogin no\n' > /etc/ssh/sshd_config.d/test.conf
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
