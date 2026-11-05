#!/usr/bin/env bash
# 从 backup.sh 的产物恢复数据库。必须显式 --confirm；先校验 SHA-256，再恢复，
# 恢复后做迁移版本检查并输出关键表行数摘要。
#
# 用法：
#   ./deploy/scripts/restore.sh --confirm --file ./backups/pjgoiot-20261105T010000Z.dump.gz
#   ./deploy/scripts/restore.sh --confirm            # 使用 BACKUP_DIR 中最新备份
#
# 环境变量：
#   DATABASE_URL      host 模式目标库（必填或使用默认）
#   BACKUP_DIR        备份目录，默认 ./backups
#   RESTORE_MODE      host（默认）或 docker
#   POSTGRES_SERVICE  docker 模式下的 compose 服务名，默认 postgres
#   POSTGRES_USER / POSTGRES_DB  docker 模式下的库账号与库名
#
# 注意：pg_restore --clean --if-exists 会先删除目标库中的同名对象，属于破坏性操作。
set -euo pipefail

DATABASE_URL="${DATABASE_URL:-postgres://pjgoiot:pjgoiot-local-only@127.0.0.1:55432/pjgoiot?sslmode=disable}"
BACKUP_DIR="${BACKUP_DIR:-./backups}"
RESTORE_MODE="${RESTORE_MODE:-host}"
POSTGRES_SERVICE="${POSTGRES_SERVICE:-postgres}"
POSTGRES_USER="${POSTGRES_USER:-pjgoiot}"
POSTGRES_DB="${POSTGRES_DB:-pjgoiot}"

confirm=false
file=""
while [ $# -gt 0 ]; do
  case "$1" in
    --confirm)
      confirm=true
      shift
      ;;
    --file)
      if [ $# -lt 2 ]; then
        echo "--file 需要参数" >&2
        exit 2
      fi
      file="$2"
      shift 2
      ;;
    --file=*)
      file="${1#--file=}"
      shift
      ;;
    -h|--help)
      sed -n '2,20p' "$0"
      exit 0
      ;;
    *)
      echo "未知参数: $1" >&2
      exit 2
      ;;
  esac
done

if [ "$confirm" != true ]; then
  echo "拒绝执行：恢复会覆盖目标库，必须显式传入 --confirm。" >&2
  echo "示例: $0 --confirm --file <backup.dump.gz>" >&2
  exit 2
fi

if [ -z "$file" ]; then
  # 按文件名中的 UTC 时间戳排序，取最新备份。
  file="$(find "$BACKUP_DIR" -maxdepth 1 -type f -name 'pjgoiot-*.dump.gz' | sort | tail -n 1)"
  if [ -z "$file" ]; then
    echo "在 $BACKUP_DIR 未找到任何备份文件" >&2
    exit 1
  fi
  echo "未指定 --file，使用最新备份: $file"
fi

if [ ! -f "$file" ]; then
  echo "备份文件不存在: $file" >&2
  exit 1
fi

checksum_file="${file}.sha256"
if [ ! -f "$checksum_file" ]; then
  echo "缺少校验和文件: $checksum_file" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  sha_cmd=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  sha_cmd=(shasum -a 256)
else
  echo "缺少 sha256sum/shasum，无法校验备份" >&2
  exit 1
fi

echo "校验备份完整性..."
(cd "$(dirname "$file")" && "${sha_cmd[@]}" -c "$(basename "$checksum_file")")

case "$RESTORE_MODE" in
  host)
    if ! command -v pg_restore >/dev/null 2>&1; then
      echo "未找到 pg_restore：请安装 postgresql-client，或改用 RESTORE_MODE=docker" >&2
      exit 1
    fi
    ;;
  docker)
    if ! command -v docker >/dev/null 2>&1; then
      echo "RESTORE_MODE=docker 但未找到 docker" >&2
      exit 1
    fi
    ;;
  *)
    echo "RESTORE_MODE 仅支持 host 或 docker" >&2
    exit 1
    ;;
esac

echo "开始恢复: $file"
if [ "$RESTORE_MODE" = "host" ]; then
  gunzip -c "$file" | pg_restore --clean --if-exists --no-owner --no-privileges --dbname="$DATABASE_URL"
else
  gunzip -c "$file" | docker compose exec -T "$POSTGRES_SERVICE" \
    pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean --if-exists --no-owner --no-privileges
fi
echo "恢复完成"

run_psql() {
  local sql="$1"
  if [ "$RESTORE_MODE" = "host" ]; then
    psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c "$sql"
  else
    docker compose exec -T "$POSTGRES_SERVICE" \
      psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -c "$sql"
  fi
}

echo "迁移版本检查:"
run_psql "SELECT version, dirty FROM schema_migrations;"

echo "关键表行数摘要:"
run_psql "SELECT
  (SELECT count(*) FROM users) AS users,
  (SELECT count(*) FROM user_devices) AS user_devices,
  (SELECT count(*) FROM readings) AS readings,
  (SELECT count(*) FROM change_log) AS change_log,
  (SELECT count(*) FROM pairing_codes) AS pairing_codes;"
