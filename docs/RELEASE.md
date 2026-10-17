# 发布流程

打一个 `v*` tag 触发 [`.github/workflows/release.yml`](../.github/workflows/release.yml)，
一次产出三样东西：

| 产物 | 位置 |
|---|---|
| 固件 zip（bootloader / 分区表 / otadata / app / flasher_args / OTA manifest） | Release 附件 `stickbox-firmware.zip` |
| OTA manifest 片段 | Release 附件 `stickbox-ota.json`（也在 zip 里） |
| `stickboxd` 镜像 | `ghcr.io/<owner>/stickboxd:<tag>`（首次推送后把 GHCR 包设为 public，或给部署机配认证） |
| `stickbox-agent` 六平台二进制 | Release 附件 + `SHA256SUMS` |

发布构建依赖 T9/M0：`rom/sdkconfig.release`、带 `ota_0/ota_1` 的
`rom/partitions.csv`、OTA 升级代码。M0 合并前工作流会在「检查发布配置」
一步直接失败，不会产出半成品。

---

## 1. 需要配置的仓库设置

### Secrets

| 名称 | 内容 |
|---|---|
| `OTA_SIGNING_KEY` | OTA 签名私钥 PEM 全文（见下） |

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

- 私钥只放进 GitHub secret `OTA_SIGNING_KEY`，不要入库、不要贴聊天、
  不要放进构建缓存。本地手动构建会在 `rom/ota_key.pem` 临时落一份，
  `release_build.sh` 用 trap 删除；构建后确认 `git status` 干净。
  （本分支尚未把 `rom/ota_key.pem` 加进 `.gitignore`，M0 合并时一并补。）
- 建议离线再存一份加密备份（密码管理器 / 硬件密钥），并记录生成日期。
- 公钥由 ESP-IDF 在构建时从私钥推导并编进 bootloader；不需要单独上传。
- 轮换密钥意味着已售设备无法验证新固件，除非同时推一版带新公钥的固件；
  本项目按「一把密钥用到产品生命周期结束」处理。

工作流里的处理：私钥只写入 runner 的临时文件（`mktemp`，`chmod 600`），
只读挂进容器；容器内 `install -m 600` 成 `rom/ota_key.pem`，`trap` 在
退出时删除；外层 `trap` 删除宿主临时文件。日志里不会出现私钥内容。

---

## 3. 发一版

```bash
git tag v0.1.0
git push origin v0.1.0
```

工作流会：

1. 在 `espressif/idf:v5.5` 容器里用**独立**的
   `-DSDKCONFIG=build-release/sdkconfig` 和
   `-DSDKCONFIG_DEFAULTS="sdkconfig.defaults;sdkconfig.release"` 构建。
   不能复用开发机的 `rom/sdkconfig`：`SDKCONFIG_DEFAULTS` 只补全缺失项，
   已有的 `sdkconfig` 会把签名开关盖掉，构建成功但 OTA 校验是关的。
2. 构建后断言 `build-release/sdkconfig` 里两个签名开关为 `y`，
   用临时导出的公钥对 `stickbox.bin` 做一次签名验证，
   并断言 `app_desc.version == tag`。
3. 生成 `ota-manifest.json`（`version` / `sha256` / `size`）。
4. 交叉编译 agent、构建并推送 `stickboxd` 镜像。
5. 创建 Release 并上传附件。

发布后检查：

```bash
gh release view v0.1.0
gh release download v0.1.0 -p stickbox-firmware.zip -D /tmp/fw
unzip -l /tmp/fw/stickbox-firmware.zip
```

zip 里应有 `flasher_args.json`、`app.bin`、`stickbox.bin`、
`bootloader/bootloader.bin`、`partition_table/partition-table.bin`、
`ota_data_initial.bin`、`ota-manifest.json`。

> ESP-IDF 的 app 产物名来自 `project(stickbox)`，实际是 `stickbox.bin`，
> `flasher_args.json` 指向它。发布脚本另存一份同字节的 `app.bin`，
> 两个都在 zip 里；OTA manifest 的 sha256/size 对两者都成立。

然后：

- 把仓库变量 `FIRMWARE_RELEASE_TAG` 改成 `v0.1.0`，T15a 的 Pages 工作流
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

## 6. 本地/容器手动跑一遍

不需要打 tag：

```bash
# 1) 生成一次性测试密钥
espsecure.py generate_signing_key --version 2 /tmp/ota_key.pem

# 2) 在 IDF 容器里跑构建（项目目录是 rom/）
docker run --rm \
  -v "$PWD/rom:/project" \
  -v /tmp/ota_key.pem:/run/secrets/ota_key.pem:ro \
  -e TAG=v0.0.0-test \
  -w /project \
  espressif/idf:v5.5 \
  bash tools/release_build.sh v0.0.0-test /run/secrets/ota_key.pem

# 3) 打包
cd rom/build-release
zip -r /tmp/stickbox-firmware.zip \
  bootloader/bootloader.bin partition_table/partition-table.bin \
  ota_data_initial.bin app.bin stickbox.bin flasher_args.json ota-manifest.json
```

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

## 8. 设计说明

- **所有 action 钉 commit SHA**：`actions/checkout` v4.4.0、
  `actions/setup-go` v5.6.0。第三方 Docker 登录/推送不用 action，直接
  `docker login` / `docker push`，减少供应链面。
- **固件容器固定 `espressif/idf:v5.5`**；镜像 tag 变更要单独评审。
- **发布构建与开发构建物理隔离**：`build-release/` 独立，构建前 `rm -rf`，
  不读开发机的 `sdkconfig`。
- **固定依赖版本**：`export IDF_COMPONENT_CHECK_NEW_VERSION=0`，只用
  `dependencies.lock` 的版本。否则组件管理器会在空 sdkconfig 上求值
  新版组件的可选依赖条件，触发 `MissingKconfigError: LV_USE_FREETYPE`。
- **版本一致性**：`-DPROJECT_VER=<tag>`，构建后从 `app.bin` 的
  `esp_app_desc_t` 反读并断言。
- **Release 附件名**：`stickbox-firmware.zip` 对齐 T15a Pages 的
  `FIRMWARE_ASSET`；tag 对齐 `FIRMWARE_RELEASE_TAG`。
