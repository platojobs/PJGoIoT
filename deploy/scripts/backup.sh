#!/usr/bin/env bash
# 备份 PostgreSQL：pg_dump 自定义格式 → gzip，UTC 时间戳文件名，写入 SHA-256 校验和，
# 并按 RETENTION_DAYS 清理旧备份。任一步失败都会非零退出。
#
# 用法：
#   DATABASE_URL=postgres://user:pass@host:5432/db ./deploy/scripts/backup.sh
#   BACKUP_MODE=docker ./deploy/scripts/backup.sh   # 通过 docker compose exec postgres 执行
#
# 环境变量：
#   DATABASE_URL     host 模式数据库连接串（必填或使用默认）
#   BACKUP_DIR       备份目录，默认 ./backups
#   RETENTION_DAYS   保留天数，默认 14
#   BACKUP_MODE      host（默认）或 docker
#   POSTGRES_SERVICE docker 模式下的 compose 服务名，默认 postgres
#   POSTGRES_USER / POSTGRES_DB  docker 模式下的库账号与库名
set -euo pipefail

DATABASE_URL="${DATABASE_URL:-postgres://pjgoiot:pjgoiot-local-only@127.0.0.1:55432/pjgoiot?sslmode=disable}"
BACKUP_DIR="${BACKUP_DIR:-./backups}"
RETENTION_DAYS="${RETENTION_DAYS:-14}"
BACKUP_MODE="${BACKUP_MODE:-host}"
POSTGRES_SERVICE="${POSTGRES_SERVICE:-postgres}"
POSTGRES_USER="${POSTGRES_USER:-pjgoiot}"
POSTGRES_DB="${POSTGRES_DB:-pjgoiot}"

if ! [[ "$RETENTION_DAYS" =~ ^[0-9]+$ ]]; then
  echo "RETENTION_DAYS 必须是非负整数，当前为: $RETENTION_DAYS" >&2
  exit 1
fi

case "$BACKUP_MODE" in
  host)
    if ! command -v pg_dump >/dev/null 2>&1; then
      echo "未找到 pg_dump：请安装 postgresql-client，或改用 BACKUP_MODE=docker" >&2
      exit 1
    fi
    ;;
  docker)
    if ! command -v docker >/dev/null 2>&1; then
      echo "BACKUP_MODE=docker 但未找到 docker" >&2
      exit 1
    fi
    ;;
  *)
    echo "BACKUP_MODE 仅支持 host 或 docker，当前为: $BACKUP_MODE" >&2
    exit 1
    ;;
esac

if command -v sha256sum >/dev/null 2>&1; then
  SHA_CMD=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  SHA_CMD=(shasum -a 256)
else
  echo "缺少 sha256sum/shasum，无法生成校验和" >&2
  exit 1
fi

umask 077

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
base_name="pjgoiot-${timestamp}"
mkdir -p "$BACKUP_DIR"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

dump_tmp="$tmp_dir/${base_name}.dump"
final_gz="$BACKUP_DIR/${base_name}.dump.gz"

echo "开始备份 (mode=${BACKUP_MODE}, dir=${BACKUP_DIR})..."

if [ "$BACKUP_MODE" = "host" ]; then
  pg_dump --dbname="$DATABASE_URL" --format=custom --no-owner --no-privileges --file="$dump_tmp"
else
  docker compose exec -T "$POSTGRES_SERVICE" \
    pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --format=custom --no-owner --no-privileges \
    > "$dump_tmp"
fi

if [ ! -s "$dump_tmp" ]; then
  echo "pg_dump 产物为空，备份失败" >&2
  exit 1
fi

gzip -9 "$dump_tmp"
mv "$dump_tmp.gz" "$final_gz"

# 校验和文件记录 basename，便于 restore.sh 在同一目录校验。
(cd "$BACKUP_DIR" && "${SHA_CMD[@]}" "${base_name}.dump.gz" > "${base_name}.dump.gz.sha256")

# 仅删除本项目命名规则内的过期备份与校验和，避免误删其他文件。
find "$BACKUP_DIR" -maxdepth 1 -type f \
  \( -name 'pjgoiot-*.dump.gz' -o -name 'pjgoiot-*.dump.gz.sha256' \) \
  -mtime "+${RETENTION_DAYS}" -delete

size_bytes="$(wc -c < "$final_gz" | tr -d ' ')"
checksum="$(cut -d' ' -f1 "$BACKUP_DIR/${base_name}.dump.gz.sha256")"
echo "备份完成: $final_gz"
echo "大小: ${size_bytes} bytes"
echo "SHA-256: ${checksum}"
echo "保留策略: ${RETENTION_DAYS} 天（已清理超期文件）"
