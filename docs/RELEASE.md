# 发布流程

正式发布走**本机签名发布**：签名私钥只由用户保管，不放进 GitHub
secret，也不进仓库。流程是「干净目录构建 → 打包 → 交叉编译 agent →
创建 Release」，步骤与 [`.github/workflows/release.yml`](../.github/workflows/release.yml)
保持一致，便于互相核对。

[`.github/workflows/release.yml`](../.github/workflows/release.yml) 现在只支持
**手动触发**（`workflow_dispatch`），且只适用于自行配置了
`OTA_SIGNING_KEY` 的仓库/环境；没有该 secret 时手动运行会在第一步明确
报错。打 tag 不再自动触发任何发布。

一次发布产出：

| 产物 | 位置 |
|---|---|
| 固件 zip（bootloader / 分区表 / otadata / app / flasher_args / OTA manifest） | Release 附件 `stickbox-firmware.zip` |
| OTA manifest 片段 | Release 附件 `stickbox-ota.json`（也在 zip 里） |
| `stickboxd` 镜像 | `ghcr.io/<owner>/stickboxd:<tag>`（首次推送后把 GHCR 包设为 public，或给部署机配认证） |
| `stickbox-agent` 六平台二进制 | Release 附件 + `SHA256SUMS` |

发布构建依赖 T9/M0：`rom/sdkconfig.release`、带 `ota_0/ota_1` 的
`rom/partitions.csv`、OTA 升级代码。`release_build.sh` 会先检查这些
配置，缺失就直接失败，不会产出半成品。

---

## 1. 需要配置的仓库设置

### Secrets（仅 CI 路径，可选）

| 名称 | 内容 |
|---|---|
| `OTA_SIGNING_KEY` | OTA 签名私钥 PEM 全文；只有选择 CI 发布时才配置 |

**正式本机签名发布不需要这个 secret。** 私钥由用户保管，不放 GitHub、
不进仓库。`release.yml` 只在手动触发且配置了该 secret 的环境里才可用。

`GITHUB_TOKEN` 由 Actions 自动提供；工作流只申请 `contents: write` 和
`packages: write`。

### Variables（T15a 烧录页用）

| 名称 | 值 |
|---|---|
| `FIRMWARE_RELEASE_TAG` | 最新固件 tag，如 `v0.1.0` |
| `FIRMWARE_ASSET` | `stickbox-firmware.zip`（默认值） |
| `FIRMWARE_REPO` | 留空 = 当前仓库；分仓/自托管时填 |

---

## 2. 签名密钥

只生成一次，**丢了就没法再给已售设备发 OTA**；泄露则任何人都能签固件。

```bash
espsecure.py generate_signing_key --version 2 ota_key.pem
```

- **私钥由用户保管**，放在用户自己保管的位置；加密备份也放在用户保管的
  位置，并记录生成日期。
- **不要**把私钥放进 GitHub secret、仓库、构建缓存、聊天记录或发布产物。
- 本机构建时 `release_build.sh` 会把私钥临时拷成 `rom/ota_key.pem`
  （`chmod 600`），用 `trap` 在退出时删除；`rom/*.pem` 已在 `.gitignore`。
  构建后确认 `git status` 干净。
- CI 路径（可选）才使用 `OTA_SIGNING_KEY`：私钥只写入 runner 的临时
  文件，只读挂进容器，外层/内层 `trap` 都会删除；日志里不会出现私钥内容。
- 公钥由 ESP-IDF 在构建时从私钥推导并编进 bootloader；不需要单独上传。
- 轮换密钥意味着已售设备无法验证新固件，除非同时推一版带新公钥的固件；
  本项目按「一把密钥用到产品生命周期结束」处理。

本机构建时用环境变量指向私钥（不要写进脚本或文档的固定路径）：

```bash
export OTA_KEY="<你保管私钥的文件路径>"
```

下面的构建示例都通过 `"$OTA_KEY"` 引用它。

---

## 3. 发一版

### 3.1 发布前

> **必须先过 [`docs/BOARD-ACCEPTANCE.md`](BOARD-ACCEPTANCE.md) 的全部
> 11 项**，其中第 10、11 项专门验证签名：已签名的新版本能通过 OTA
> 升级成功，未签名固件会被设备拒绝。任何 FAIL 都不发版；串口日志、
> 上板记录和 Release 产物一并归档。

另外确认：

```bash
git status --short          # 工作区干净
git fetch origin
git rev-parse HEAD origin/main   # 待发布的 commit 就是 main
```

### 3.2 本机签名发布（正式路径）

签名私钥不经过 GitHub：构建在自己的机器上完成，`$OTA_KEY` 指向
用户保管的私钥。

```bash
TAG=v1.0.0
export OTA_KEY="<你保管私钥的文件路径>"

# 1) 本机打 tag（先不推送，构建/验收失败可以删掉重来）
git tag "$TAG"

# 2) 从 tag 导出干净目录，避免带上开发机的 sdkconfig / cloud_creds.h
EXPORT=$(mktemp -d)
git archive --format=tar "$TAG" | tar -x -C "$EXPORT"

# 3) 用官方示例地址初始化 cloud_creds.h（该文件不入库）
cd "$EXPORT/rom"
cp main/cloud_creds.h.example main/cloud_creds.h

# 4) 发布构建（独立 build-release/sdkconfig，签名 + 验签 + 版本断言）
source ~/esp/esp-idf-v5.5/export.sh   # 已有 idf.py 可跳过
bash tools/release_build.sh "$TAG" "$OTA_KEY"
```

`release_build.sh` 会断言：两个签名开关为 `y`、`stickbox.bin` 签名有效、
`app_desc.version == tag`、内嵌主机名是 `mqtt.stickbox.ai`，并生成
`ota-manifest.json`。构建目录是 `rom/build-release/`，与开发构建物理隔离。

打包固件（与 CI 的 zip 内容一致）：

```bash
DIST="$EXPORT/dist"
mkdir -p "$DIST"
cd "$EXPORT/rom/build-release"
zip -q -r "$DIST/stickbox-firmware.zip" \
  bootloader/bootloader.bin partition_table/partition-table.bin \
  ota_data_initial.bin app.bin stickbox.bin flasher_args.json ota-manifest.json
cp ota-manifest.json "$DIST/stickbox-ota.json"
```

交叉编译 agent（与 CI 的循环一致）：

```bash
cd "$EXPORT/server/go"
for os in darwin linux windows; do
  for arch in amd64 arm64; do
    ext=""
    [ "$os" = windows ] && ext=".exe"
    GOOS=$os GOARCH=$arch CGO_ENABLED=0 \
      go build -trimpath -ldflags "-s -w" \
      -o "$DIST/stickbox-agent-${os}-${arch}${ext}" ./cmd/stickbox-agent
  done
done
cd "$DIST"
sha256sum stickbox-agent-* > SHA256SUMS
# macOS 没有 sha256sum 时用：shasum -a 256 stickbox-agent-* > SHA256SUMS
```

构建并推送服务端镜像（tag 与 Release 一致）：

```bash
cd "$EXPORT/server/go"
IMAGE="ghcr.io/<owner>/stickboxd:$TAG"
docker build --build-arg VERSION="$TAG" -t "$IMAGE" .
# 需要推送权限（CI 里是 GITHUB_TOKEN）：
#   docker login ghcr.io
docker push "$IMAGE"
```

推送 tag 并创建 Release（顺序与 CI 一致；tag 推送不会再触发自动发布）：

```bash
cd /path/to/your/repo
git push origin "$TAG"
gh release create "$TAG" \
  "$DIST/stickbox-firmware.zip" \
  "$DIST/stickbox-ota.json" \
  "$DIST"/stickbox-agent-* \
  "$DIST/SHA256SUMS" \
  --verify-tag --title "$TAG" --generate-notes
```

发布后检查：

```bash
gh release view "$TAG"
gh release download "$TAG" -p stickbox-firmware.zip -D "$EXPORT/check"
unzip -l "$EXPORT/check/stickbox-firmware.zip"
```

zip 里应有 `flasher_args.json`、`app.bin`、`stickbox.bin`、
`bootloader/bootloader.bin`、`partition_table/partition-table.bin`、
`ota_data_initial.bin`、`ota-manifest.json`。

> ESP-IDF 的 app 产物名来自 `project(stickbox)`，实际是 `stickbox.bin`，
> `flasher_args.json` 指向它。发布脚本另存一份同字节的 `app.bin`，
> 两个都在 zip 里；OTA manifest 的 sha256/size 对两者都成立。

### 3.3 CI 路径（可选）

只适用于自行配置了 `OTA_SIGNING_KEY` 的环境；正式发布不要用：

```bash
gh workflow run release.yml -f tag=v1.0.0
```

工作流在 `espressif/idf:v5.5` 容器里执行同一套 `release_build.sh`，
再打包、交叉编译 agent、构建镜像并创建 Release。

为什么改成手动触发、而不是「配置了 secret 才跑」：正式路径是本机签名，
仓库按约定不保存私钥；tag 推送若自动触发，每次都会产生一次必然失败
（或半成品）的发布。手动触发让发布动作显式、可重跑，也避免误触发；
需要 CI 的环境仍然保留完整能力。

### 3.4 发布之后

- 把仓库变量 `FIRMWARE_RELEASE_TAG` 改成新 tag，T15a 的 Pages 工作流
  会重新生成烧录页。
- 服务端 `config.json` 配 OTA（字段见第 4 节），先小比例放量。

---

## 4. 服务端 OTA manifest

`stickbox-ota.json` 是片段：

```json
{
  "version": "v0.1.0",
  "sha256": "<app.bin 的 SHA-256>",
  "size": 2179072
}
```

服务端配置补上 `url`（指向 `app.bin`）和 `rollout_percent`：

```json
{
  "firmware": {
    "version": "v0.1.0",
    "url": "https://mqtt.stickbox.ai/fw/app-v0.1.0.bin",
    "sha256": "<同上>",
    "size": 2179072,
    "rollout_percent": 5
  }
}
```

- `version` 必须与 `app_desc.version` 完全一致；设备只比较「不同就升级」。
- 放量按设备 ID 哈希分桶，先 5%，观察崩溃回传（T10）后加到 100%。
- `rollout_percent: 0` 是紧急刹车：所有设备都收到 204，继续用当前版本。

---

## 5. 回滚

### 停止继续放量

把服务端 `firmware.rollout_percent` 改成 `0`，重载配置。已升级的设备不会
自动降级——OTA 只在服务端版本与设备当前版本不同时触发。

### 已升级设备回滚

设备侧有 `CONFIG_BOOTLOADER_APP_ROLLBACK_ENABLE`：新固件启动后健康检查
（Wi-Fi + MQTT + 推流，或超时）不过就自动回滚上一槽。已经通过健康检查的
设备不会自动回退。

要主动回退一版：把服务端 manifest 的 `version` 改回上一版、`url`/`sha256`/`size`
指向上一版 `app.bin`，并保持小比例。设备会把「服务端版本不同」当成一次升级，
走同一套签名校验和回滚逻辑。失败过的版本会被设备记住并跳过，避免反复刷砖。

### 坏 Release

- 立刻把 `rollout_percent` 置 0。
- 不要删 Release 附件——已经拿到 URL 的设备可能还在下载；留着至少一个版本周期。
- 修好后发一个更高的 tag（如 `v0.1.1`），不要重用 `v0.1.0`。

---

## 6. 容器里复现 CI 构建（仅测试工具链）

正式发布用第 3.2 节的本机构建。只想验证 CI 的容器链路时，用**临时
测试密钥**（不是正式私钥），步骤与 `release.yml` 完全一致：

```bash
TEST_KEY="$(mktemp)"          # 一次性测试密钥，用完删除
export TEST_KEY

# 1) 生成一次性测试密钥
espsecure.py generate_signing_key --version 2 "$TEST_KEY"

# 2) 在 IDF 容器里跑构建（项目目录是 rom/）
docker run --rm \
  -v "$PWD/rom:/project" \
  -v "$TEST_KEY:/run/secrets/ota_key.pem:ro" \
  -e TAG=v0.0.0-test \
  -w /project \
  espressif/idf:v5.5 \
  bash tools/release_build.sh v0.0.0-test /run/secrets/ota_key.pem

# 3) 打包
cd rom/build-release
zip -r "$PWD/../../stickbox-firmware.zip" \
  bootloader/bootloader.bin partition_table/partition-table.bin \
  ota_data_initial.bin app.bin stickbox.bin flasher_args.json ota-manifest.json
```

> 用临时密钥构建出来的固件，不能被已售设备接受（它们的 bootloader 里
> 是正式公钥）；只用于验证构建/签名/打包链路，不要发布。

`release_build.sh` 会先检查 `sdkconfig.release` 和 `ota_0/ota_1`，缺失就
直接报错（见开头的 T9/M0 依赖）。

agent 与镜像可以单独验：

```bash
cd server/go
for os in darwin linux windows; do
  for arch in amd64 arm64; do
    GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -o /tmp/agent-$os-$arch ./cmd/stickbox-agent
  done
done
docker build --build-arg VERSION=v0.0.0-test -t stickboxd:test .
```

---

## 7. 验收记录（2026-10-17）

在 `espressif/idf:v5.5` 容器里，用 M0 分支的 `rom` 副本（含
`sdkconfig.release`、OTA 分区表和 `ota.c`）跑通了完整构建：

```
app_desc.version=v0.0.0-test ✓
build-release/ota-manifest.json:
  {"version":"v0.0.0-test","sha256":"b663cebd...","size":2101248}
stickbox.bin 0x201000 bytes，3M 槽余 33%
```

产物齐全：`bootloader/bootloader.bin`、`partition_table/partition-table.bin`、
`ota_data_initial.bin`、`stickbox.bin`、`app.bin`、`flasher_args.json`、
`ota-manifest.json`。签名验证输出 `Signature block 0 is valid (RSA)`。zip 解压后 `gen_web_manifest.py --copy-bins` 能正常生成
T15a 的 `manifest.json`（偏移 0 / 0x8000 / 0x10000 / 0x20000）。

同时验证：六个 agent 目标（darwin/linux/windows × amd64/arm64）都能
`CGO_ENABLED=0 go build`。`stickboxd` 镜像构建见工作流。

### 升级 IDF 镜像 digest

```bash
docker pull espressif/idf:v5.5
docker inspect --format '{{index .RepoDigests 0}}' espressif/idf:v5.5
```

把输出的 `@sha256:...` 替换到 `.github/workflows/release.yml` 的
`docker run` 那一行；先用 `rom/tools/release_build.sh` 在本地跑通一次完整
构建（含验签和版本断言），再提交。不要只改 tag。

## 8. 设计说明

- **签名私钥只在用户手里**：正式发布在本机完成，GitHub 不保存私钥；
  本机构建从 `git archive` 的干净目录开始，用
  `cloud_creds.h.example` 的官方地址，避免把开发机状态带进发布物。
- **Release 只手动触发**：`release.yml` 需要 `OTA_SIGNING_KEY`，而正式
  路径不配置它；tag 推送自动触发只会制造必然失败的运行。手动触发让
  发布显式、可重跑，同时保留 CI 能力（配置了 secret 的环境）。
- **本地与 CI 用同一条构建脚本**：两边都调用
  `rom/tools/release_build.sh`，打包、agent 交叉编译和 `gh release create`
  的产物列表保持一致，便于互相对账。
- **所有 action 钉 commit SHA**：`actions/checkout` v4.4.0、
  `actions/setup-go` v5.6.0。第三方 Docker 登录/推送不用 action，直接
  `docker login` / `docker push`，减少供应链面。
- **固件容器固定到 digest**：`espressif/idf:v5.5@sha256:00e94c...`。
  只写 tag 不够——tag 会漂移，digest 才能复现同一套工具链。
- **发布构建与开发构建物理隔离**：`build-release/` 独立，构建前 `rm -rf`，
  不读开发机的 `sdkconfig`。
- **固定依赖版本**：`export IDF_COMPONENT_CHECK_NEW_VERSION=0`，只用
  `dependencies.lock` 的版本。否则组件管理器会在空 sdkconfig 上求值
  新版组件的可选依赖条件，触发 `MissingKconfigError: LV_USE_FREETYPE`。
- **版本一致性**：`-DPROJECT_VER=<tag>`，构建后从 `app.bin` 的
  `esp_app_desc_t` 反读并断言。
- **Release 附件名**：`stickbox-firmware.zip` 对齐 T15a Pages 的
  `FIRMWARE_ASSET`；tag 对齐 `FIRMWARE_RELEASE_TAG`。
