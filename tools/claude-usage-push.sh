#!/bin/bash
# 定时拉 Claude 订阅用量，推到 StickBox 屏幕（POST /api/usage）。
# 由 launchd 定时调用（见 tools/com.stickbox.claude-usage.plist），桌面版用户不需要 CLI 会话。
#
# 原理：`claude -p "/usage"` 是本地命令，不走模型、不耗额度；stream-json 输出里
# 那条 assistant 消息带结构化的 usage_report.rate_limits.limits。
# 文档：https://code.claude.com/docs/en/agent-sdk/typescript#sdkusagereport
#
# 几个必须的参数：
#   --setting-sources project   跳过 ~/.claude/settings.json：那里的 env 把 CLI 指向了
#                               别的接口（ANTHROPIC_BASE_URL），那样拿不到订阅用量
#   --no-session-persistence    每次跑不落会话记录，否则每天几百个垃圾会话
# 用量接口本身有频率限制（429 后约 1 小时内不再给数据），所以间隔别低于 10 分钟。
#
# 用法：STICKBOX_HOST=192.168.3.14 tools/claude-usage-push.sh

set -u
HOST="${STICKBOX_HOST:-192.168.3.14}"
CLAUDE="${CLAUDE_BIN:-$HOME/.local/bin/claude}"
WORKDIR="$HOME/.config/stickbox"      # 空目录：不让任何项目级配置掺进来
mkdir -p "$WORKDIR"
# 重放文件按调用时的目录解析，下面要 cd 走
REPLAY_SRC="${STICKBOX_REPLAY_FILE:-$WORKDIR/last-good-payload.json}"
case "$REPLAY_SRC" in /*) ;; *) REPLAY_SRC="$PWD/$REPLAY_SRC" ;; esac
cd "$WORKDIR" || exit 1

log() { echo "$(date '+%F %T') $*"; }

# 重放模式：把缓存的最近一次数据再推一遍（now 换成当前时间，倒计时照常走）
if [ -n "${STICKBOX_REPLAY:-}" ]; then
    src="$REPLAY_SRC"
    payload=$(jq -c '.now = (now | floor)' "$src") || exit 1
    code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -X POST \
           -H 'Content-Type: application/json' -d "$payload" "http://$HOST/api/usage")
    log "replayed $src -> HTTP $code"
    [ "$code" = 204 ]; exit
fi

out=$(env -i HOME="$HOME" USER="$USER" PATH="$(dirname "$CLAUDE"):/usr/bin:/bin" \
      "$CLAUDE" -p "/usage" --setting-sources project --no-session-persistence \
      --output-format stream-json --verbose 2>/dev/null)

# 只取 limits 里的两行：kind=session → 5 小时窗口，kind=weekly_all → 7 天窗口。
# resets_at 是 ISO 8601，去掉小数秒、把 +00:00 换成 Z 后 jq 才认。
payload=$(jq -c -s '
  def epoch: sub("\\.[0-9]+"; "") | sub("\\+00:00$"; "Z") | fromdateiso8601;
  def win(k): (map(select(.kind == k)) | first
               | if . then {used_percentage: .percent, resets_at: (.resets_at | epoch)} else null end);
  [ .[] | select(.type == "assistant") | .usage_report.rate_limits.limits // empty ] | first
  | if . == null then empty
    else { now: (now | floor), five_hour: win("session"), seven_day: win("weekly_all") }
         | with_entries(select(.value != null))
    end
' <<<"$out" 2>/dev/null)

if [ -z "$payload" ]; then
    # 最常见是用量接口 429；屏幕上「更新 x分前」会变旧，等下一轮。
    # 原始输出留一份，排查是 429 还是登录态问题（看 init 里的 apiKeySource / model）
    printf '%s\n' "$out" > "$WORKDIR/last-usage-output.jsonl"
    log "no rate_limits in /usage output (likely 429 from usage endpoint); raw: $WORKDIR/last-usage-output.jsonl"
    exit 1
fi

# 缓存最近一次成功的数据：调界面时用 STICKBOX_REPLAY=1 重放，不用再打用量接口（会 429）
printf '%s\n' "$out"     > "$WORKDIR/last-good-output.jsonl"
printf '%s\n' "$payload" > "$WORKDIR/last-good-payload.json"

code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -X POST \
       -H 'Content-Type: application/json' -d "$payload" "http://$HOST/api/usage")
log "pushed $payload -> HTTP $code"
[ "$code" = 204 ]
