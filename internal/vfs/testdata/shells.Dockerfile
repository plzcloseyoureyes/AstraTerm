FROM alpine:3.22
RUN apk add --no-cache openssh bash zsh fish mksh sudo shadow && ssh-keygen -A && \
    sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config && \
    echo 'Subsystem sftp internal-sftp' >> /etc/ssh/sshd_config && \
    for u in ubash:/bin/bash uzsh:/bin/zsh ufish:/usr/bin/fish umksh:/bin/mksh uash:/bin/ash sglob:/bin/bash; do \
      n=${u%%:*}; sh=${u#*:}; grep -qx "$sh" /etc/shells || echo "$sh" >> /etc/shells; \
      adduser -D -s "$sh" "$n" && echo "$n:test" | chpasswd; done && \
    printf 'Defaults:sglob timestamp_type=global\nsglob ALL=(ALL) ALL\n' > /etc/sudoers.d/sglob && chmod 440 /etc/sudoers.d/sglob
EXPOSE 22
CMD ["/usr/sbin/sshd","-D","-e"]
