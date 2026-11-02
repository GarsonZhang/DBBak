# 阶段五生产镜像示例；应用与基础镜像均使用明确版本，不使用 latest。
FROM golang:1.27.3-bookworm AS build
# 只复制构建所需内容，避免把文档、测试数据和工具链带进镜像层。
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY embed.go ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web/ ./web/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath -buildvcs=false \
    -ldflags='-s -w -buildid=' -o /out/vault ./cmd/vault

FROM mysql:8.4.13
LABEL org.opencontainers.image.version="0.2.0-phase2"
RUN microdnf install -y curl && microdnf clean all \
    && groupadd --system --gid 10001 vault \
    && useradd --system --uid 10001 --gid 10001 --no-create-home --shell /sbin/nologin vault \
    && mkdir -p /var/lib/vault /backups /etc/vault-key \
    && chown -R 10001:10001 /var/lib/vault /backups /etc/vault-key
COPY --from=build --chown=10001:10001 /out/vault /usr/local/bin/vault
ENV VAULT_ADDR=0.0.0.0:8080 \
    VAULT_DATA_DIR=/var/lib/vault \
    VAULT_DATABASE=/var/lib/vault/vault.db \
    VAULT_BACKUP_ROOT=/backups \
    VAULT_MASTER_KEY_FILE=/etc/vault-key/master.key \
    VAULT_MYSQL=mysql \
    VAULT_MYSQLDUMP=mysqldump
VOLUME ["/var/lib/vault", "/backups", "/etc/vault-key"]
EXPOSE 8080
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/vault"]
CMD ["serve"]
