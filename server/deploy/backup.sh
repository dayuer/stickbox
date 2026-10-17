#!/usr/bin/env bash
# stickboxd 每日在线备份：sqlite 快照 → age 加密 → 保留最近 N 份。
#
# 服务器上：
#   sudo cp server/deploy/backup.sh /opt/stickbox/backup.sh
#   sudo cp server/deploy/age.pub   /opt/stickbox/age.pub
#   sudo chmod 700 /opt/stickbox/backup.sh
#   # cron（避开整点）：
#   17 3 * * * /opt/stickbox/backup.sh >> /var/log/stickbox-backup.log 2>&1
#
# 私钥不在服务器上；恢复见 docs/DEPLOY.md。
set -euo pipefail
umask 077

STICKBOX_DIR="${STICKBOX_DIR:-/opt/stickbox}"
BACKUP_DIR="${BACKUP_DIR:-$STICKBOX_DIR/backups}"
CONTAINER="${CONTAINER:-stickboxd}"
AGE_PUB="${AGE_PUB:-$STICKBOX_DIR/age.pub}"
KEEP="${KEEP:-14}"

command -v age >/dev/null || { echo "缺少 age：apt install age" >&2; exit 1; }
command -v docker >/dev/null || { echo "缺少 docker" >&2; exit 1; }
test -s "$AGE_PUB" || { echo "缺少 age 公钥: $AGE_PUB" >&2; exit 1; }
test "$(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null || echo false)" = "true" \
    || { echo "容器 $CONTAINER 没在运行" >&2; exit 1; }

mkdir -p "$BACKUP_DIR" "$STICKBOX_DIR/data/backup"

# 便携锁：服务器和本地都能跑。超过 2 小时的锁视为上次崩溃残留。
LOCKDIR="$BACKUP_DIR/.lock"
if ! mkdir "$LOCKDIR" 2>/dev/null; then
    if [ -n "$(find "$LOCKDIR" -mmin +120 -print -quit 2>/dev/null)" ]; then
        rmdir "$LOCKDIR" 2>/dev/null || true
        mkdir "$LOCKDIR" 2>/dev/null || { echo "已有备份在跑，跳过"; exit 0; }
    else
        echo "已有备份在跑，跳过"
        exit 0
    fi
fi

TS="$(date -u +%Y%m%dT%H%M%SZ)"
PLAIN="$STICKBOX_DIR/data/backup/jobs-$TS.db"
ENC="$BACKUP_DIR/jobs-$TS.db.age"
trap 'rm -f "$PLAIN" "$ENC.tmp"; rmdir "$LOCKDIR" 2>/dev/null || true' EXIT

# 容器内做 VACUUM INTO（镜像里没有 sqlite3），写到 /data 挂载点。
docker exec "$CONTAINER" /stickboxd -conf /etc/stickbox/config.json backup \
    -out "/data/backup/jobs-$TS.db"

# 先写 .tmp 再 rename，避免中断留下半个加密文件。
age -r "$(cat "$AGE_PUB")" -o "$ENC.tmp" "$PLAIN"
mv "$ENC.tmp" "$ENC"
rm -f "$PLAIN"

# 只保留最近 KEEP 份。
ls -1t "$BACKUP_DIR"/jobs-*.db.age 2>/dev/null \
    | tail -n +$((KEEP + 1)) \
    | xargs -r rm -f --

echo "backup: $ENC"
