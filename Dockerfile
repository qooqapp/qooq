# 多階段建置：--target qooqd 或 --target qooq-watcher 選擇要出的 image。
#   docker build --target qooqd -t qooq/qooqd .
#   docker build --target qooq-watcher -t qooq/qooq-watcher .
#
# 注意：qooqd 容器化只對 docker runtime 有意義（需掛 /var/run/docker.sock，
# 它建的容器是 host 上的兄弟容器）；process runtime 管的是 host 程序，
# 容器化後只能管到自己容器內，通常應直接跑 binary（見 docs/USAGE.md）。

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 版本從外面傳進來：build context 裡沒有 .git（.dockerignore 排除），
# 容器內問不到 git，不傳就是 "dev"。
#   docker build --build-arg VERSION=$(git describe --tags --always --dirty) ...
ARG VERSION=dev
ARG COMMIT=
ENV VERSION_PKG=github.com/qooqapp/qooq/internal/version
# 全部純 Go（watcher 的 sqlite 走 glebarez/modernc，無 CGO）
RUN LDFLAGS="-X ${VERSION_PKG}.Version=${VERSION} -X ${VERSION_PKG}.Commit=${COMMIT}" \
 && CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/qooqd ./cmd/qooqd \
 && CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/qooq-watcher ./cmd/qooq-watcher

# ---- qooqd（orchestrator daemon；CLI 在獨立 repo qooq-cli，image 不帶） ----
FROM alpine:3.22 AS qooqd
RUN apk add --no-cache ca-certificates
COPY --from=build /out/qooqd /usr/local/bin/
VOLUME /data
EXPOSE 7777
ENTRYPOINT ["qooqd"]
# 容器內必須 bind 0.0.0.0（預設 127.0.0.1 外面連不到）；務必配 QOOQ_TOKEN
CMD ["-listen", "0.0.0.0:7777", "-data", "/data/qooq.db", "-backup-dir", "/data/backups"]

# ---- qooq-watcher（GitOps service） ----
FROM alpine:3.22 AS qooq-watcher
# git：go-git 的 https/ssh 是純 Go，但 file:// 協定會 shell out 到 git binary
# （本機 bare repo 做 e2e 會用到，見 docker-compose.e2e.yml）
RUN apk add --no-cache ca-certificates git
COPY --from=build /out/qooq-watcher /usr/local/bin/
ENV QOOQ_WATCHER_HTTP_HOST=0.0.0.0 \
    QOOQ_WATCHER_SQLITE_PATH=/data/watcher.db \
    QOOQ_WATCHER_ARTIFACTS_DIR=/data/artifacts \
    QOOQ_WATCHER_BACKUP_DIR=/data/backups
VOLUME /data
EXPOSE 7778
ENTRYPOINT ["qooq-watcher"]
