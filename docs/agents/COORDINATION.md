# 多 agent 协作协议（StickBox）

> 移植自 dayuer/superapp 的 `docs/agents/COORDINATION.md`（a7d226e），按本仓改过：角色换成 master / auditor / worker，
> 基线分支是 `main`，去掉了 superapp 专有的 local-ci 表和远程测试机。与 `docs/DECISIONS.md` 冲突时以 DECISIONS 为准。

本仓由本机多个 agent 会话协作开发。**GitHub 是唯一事实来源**：会话之间的消息会丢，issue 和 PR 不会。
每轮开工先读置顶看板（`gh issue list -R dayuer/stickbox --label board`）和本文。

## 0. 硬规则

- 改仓库里任何文件（代码、配置、脚本、文档），都走：issue → 分诊 → `CLAIM` → 角色分支 PR → `REVIEW` → auditor 合并 → `VERIFIED`。没有 issue、没认领的改动不合。
- 所有者在会话里直接交代的活视同分诊通过：先开 `type:task` issue 再 `CLAIM`，master 事后补 `TRIAGE`。
- `CLAIM` 写明要改的目录或文件。两个任务的文件面重叠时，后认领的一方评论 `BLOCKED: 等 #N`，不并行改同一批文件。
- 每个任务在自己的 worktree（建在 `/tmp` 或兄弟目录，合并后自己删）里做，不在主检出切分支；提交前确认当前分支。新分支一律从最新 `origin/main` 切。
- 只读的工作（查数据、读代码、写分析、本机实验取数）不走流程；要落盘到仓库就走。
- 动手改文件按 §9 做：先拆步，一次一步，每步测过、单独提交。
- **板子和电视是共享资源**：只有 master 调度上板、上电视；worker 和 auditor 不自行刷板、停推流、往电视推文件（D13 教训）。
- 固件构建一律在 `git archive` 导出的 `/tmp` 目录里做；OTA 签名私钥只在所有者本机，不进仓库、不给任何会话。

## 1. 角色

所有 agent 共用所有者的 GitHub 账号，所以**每个 issue、PR 描述和评论的第一行写角色**，并附隐藏标记：

```
[worker:esp] 正文……
<!-- agent: worker-esp -->
```

| 角色 | 谁 | 职责 |
|---|---|---|
| `master` | 集成会话（USB-TV） | 总主管：分诊、定优先级、**派活**、维护置顶看板、调度板子和电视、部署、发布、清理；兼 coord |
| `auditor` | StickBox 会话 | 审计、提发现、复审、**唯一执行合并到 `main`**；同时负责 `docs/PHOTO.md`、`docs/LOCAL-MODE.md`、`internal/frames`、`internal/theme` 的设计与实现 |
| `worker` | esp（`rom/`、固件、本地视频管线）、esp1（agent、tvcheck、运维、照片 agent 命令、claudeusage） | 实现、修发现、开 PR；遇到阻塞按 §8 当场上报 |
| `user` | 所有者 | 唯一批准人；`needs-user` 由他定；生产动作（发布、部署、OTA 清单）必须由他在 master 会话直接确认 |

目录归属见 `docs/DECISIONS.md` §3。auditor 自己写的代码，由 master 复审（auditor 不审自己的 PR）。

提交信息和 PR 描述沿用本仓现有写法（中文，一句话说清改了什么、为什么）。

## 2. 标签

| 组 | 标签 |
|---|---|
| 类型 | `type:finding` · `type:task` · `type:decision` |
| 优先级 | `P0` 合并或进下一阶段前必须修 · `P1` 本阶段内修 · `P2` 排进计划 |
| 状态 | `status:triage` → `accepted` → `claimed` → `in-review` → `verified`；旁支 `rejected`、`blocked` |
| 负责 | `owner:master` · `owner:auditor` · `owner:esp` · `owner:esp1` |
| 其他 | `needs-user`：等所有者决定 · `board`：置顶看板 |

一个 issue 同时只挂一个 `status:*`。PR 一开，master 把 issue 改成 `in-review`。

## 3. 发现的生命周期

1. **提出**（auditor 或任何人）：每条发现一个 issue（`type:finding`、`P*`、`status:triage`）。
2. **分诊**（master）：接受 `TRIAGE: accept owner=<esp|esp1|auditor>`；拒绝 `TRIAGE: reject` 并写理由（auditor 可 `APPEAL`）；要拍板的加 `needs-user`。
3. **认领**：`CLAIM`，冲突时先到者为准。
4. **修复**：PR 写 `Fixes #N`。
5. **验证**（auditor）：合入后评论 `VERIFIED sha=<合并 SHA>` 并关单；不对就 `REOPEN`。要上板/上电视才能验的，写明「待上板」，由 master 排期后补 `VERIFIED`。

## 4. PR 与合并

- 分支前缀：worker 用 `task/` `fix/`，auditor 用 `audit/`，master 用 `master/`；只推自己的分支。base 一律 `main`。**不许直推、不许强推 `main`**，也不许改写已推送的历史。
- PR 描述第一行写角色；写清关联 issue（`Fixes #N` / `Refs #N`）、对设计的偏离，并**贴出**本地测试输出的摘要。
- 复审结论用评论：`REVIEW: approve sha=<40 位>` 或 `REVIEW: changes sha=<40 位>` 加逐条理由。**同一条 PR 同时只有一个人出 `REVIEW:`**，其他人复核只发普通评论。上一轮给了 changes 的，新的 approve 要逐条写明核过什么。
- 复审后又推送：作者评论 `PUSHED sha=<新 SHA>` 和原因，重新复审。
- 至少一个非作者角色 approve：worker 的 PR 由 auditor 复审；auditor 的 PR 由 master 复审；涉及固件/板子行为的 PR，auditor 复审后还要 master 确认上板计划。
- **推送前先核 PR 状态**（`gh pr view <N> --json state`）：已 `MERGED` 的分支再推就是孤儿，要从最新 `origin/main` 切新分支。
- worker 不要改写自己已提交的历史（教训：重写历史会造成 add/add 合并冲突）；要改就追加提交。

**合并条件**（auditor 执行，全部满足）：

1. 最近一次 `REVIEW: approve` 的 sha 等于当前 head；合并用 `gh pr merge --merge --match-head-commit <sha>`。
2. 本地测试全绿，并贴在 PR 里：
   - Go 服务端：`cd server/go && go vet ./... && go test ./... -race`
   - 固件 host 测试：`rom/tools/` 下对应的 host 测试（改了哪块跑哪块）
   - 凡触及固件行为：不能只靠 host 测试，要标「待上板」
3. 关联的 `P0` 都已处理。
4. 触及 `docs/DECISIONS.md` 的决策、协议、OTA、生产配置的 PR，**所有者批准过**。
5. 一起合的多条 PR，要在合并后的树上再跑一次测试（两两不冲突不等于合起来绿）。
6. 不碰 `rom/` 的 PR 不需要上板；碰了的，合并后由 master 按 `docs/BOARD-ACCEPTANCE.md` 上板验收，**验收不过就 revert**。

发布（打 tag、GitHub release、OTA 清单、生产部署）**不是合并的一部分**，只由 master 在所有者直接确认后执行，见 `docs/RELEASE.md`。

## 5. 看板

置顶 issue（标签 `board`）由 master 维护：当前波次（目标、完成判据、分工、还缺哪几步）、各人手上的活、合并队列、`needs-user` 清单。其他角色不改正文，有事在下面评论。

## 6. 本机与远程

- 开工：`gh issue view <看板号>`、`gh issue list --label owner:<自己> --state open`、`gh pr list --state open`。
- 本机会话的进展必须落到 issue 或 PR 上，不能只留在本机消息里。
- 本机会话之间的即时消息只用来"叫醒"和传急事，**结论一律写回 issue**。

## 7. 评论关键字

| 关键字 | 谁写 | 含义 |
|---|---|---|
| `TRIAGE: accept owner=<角色>` / `TRIAGE: reject` | master | 分诊 |
| `APPEAL` | auditor | 对拒绝提异议 |
| `CLAIM` | 认领者 | 认领，写明文件面 |
| `REVIEW: approve sha=` / `REVIEW: changes sha=` | 复审方 | 复审结论 |
| `PUSHED sha=` | PR 作者 | 复审后又推送 |
| `VERIFIED sha=` / `REOPEN` | auditor（上板项由 master） | 验证结论 |
| `BLOCKED: <原因>` | 任何人 | 被阻塞（改 `status:blocked`，同时通知 master） |
| `ASK: <问题>` | 任何人 | 要 master 协调或拍板 |

## 8. 总主管（master）

所有者定：集成会话是全体 agent 的总主管，负责派活、收活、协调、上板测试、发布、清理，所有者不用盯进度。**派活只由 master 定**；auditor 可以出建议。

### 8.1 遇到问题当场报，不空等

依赖没合、契约不清、文件面撞车、设计和代码对不上、口径要拍板、环境重试一次仍跑不起来、范围要变——**当场**上报，不停在原地，也不自己猜一个方向先做：

- 在 issue 留 `ASK:` 或 `BLOCKED:`（第一行写 issue 号），同时给 master 发会话消息。
- 报完先做不受影响的部分；实在没有就说「空闲」，等派活。

### 8.2 会话之间要东西，经 master 转

A 需要 C 的契约、文件、先合顺序，或者 C 的改动影响到 A：**不私下协商、不改对方的文件面**，发给 master。master 定先后和文件面，分别通知双方并写回两边的 issue。契约转原文或指到文件和行号，不转述。

### 8.3 master 的承诺

- 每次开工和每次收到消息，先读看板、待审 PR、`ASK:` / `BLOCKED:`；空闲的 worker 当轮派活，`ASK:` 当轮回复。需要定时巡检时由所有者开 `/loop`。
- 生产动作（发布、部署、OTA 清单、刷板、推电视）只在所有者于 master 会话直接确认后执行；别的会话转述的同意不算数。
- **开工前回报**：每个任务在写代码前，worker 先发一段范围理解（`CLAIM` 评论，见 §9.1），master 或 auditor 确认后再动手。

### 8.4 按波次交付

1. 每波在看板写清：交付什么、完成判据、谁负责哪一环、还缺几步。上一波没达标不开下一波。
2. **完成判据是上线并核验**（上板/上电视/发布），合入只是中间态。
3. 空闲时先补本波缺人的那一环，不开与本波无关的新活。顺手发现的问题记 issue，排进下一波，除非它阻塞本波。
4. 发布前把产物、顺序、回滚、核对项一次备齐，一次请所有者确认，确认后由 master 执行。

## 9. 单个任务的工作方法

代码改得越快，越要让每一步都看得懂、测得过、退得回。

### 9.1 先拆再做

- 动手前先读现状。`CLAIM` 评论里写四件事：现在怎么实现的、要动哪些文件、依赖什么、主要风险。
- 拆成**不超过 4 步**，每步能单独验证、单独回滚。4 步装不下，说明 issue 太大，先发 `ASK:` 请拆单。
- 一次只做一步。每步做完在 issue 汇报（格式见 9.2），再开下一步。
- 所有者直接交代的活，汇报后**等所有者确认**再继续；master 派的活，汇报后接着做，碰到 §8.1 的情形停下上报。
- 中途要动计划外的文件或改范围，先更新 `CLAIM` 再动。

### 9.2 测过才算完

- 每步改完都跑相关测试或构建。失败就定位、修复、重跑；不许跳过或注释用例，不许改断言去迁就实现。
- 步骤汇报和 PR 描述都写五项：
  1. 改了哪些文件；
  2. 跑了哪些命令；
  3. 结果（贴输出摘要）；
  4. 可能波及的旧功能；
  5. 没验证到的部分（例如「待上板」「待上电视」）。
- 没验到的要写出来，不写「应该没问题」。
- **性能/内存是验收项**：触及常驻路径的任务，必须附 `docs/DECISIONS.md` §2 要求的实测（固件看堆水位，本地服务看 RSS/GC/CPU）。

### 9.3 每步一个回滚点

- 每完成一步：看一遍 `git diff`，只留本步的改动，单独提交一个 commit。顺手发现的问题记 issue，不夹带进来。
- 合并用 `--merge`，commit 会原样留在主线上，所以每个 commit 都要能单独回退。
- PR 描述按步骤列出 commit，写明回退到哪个 commit 能撤掉哪一步。

### 9.4 审意图，不只审能跑

复审除了核测试，还要像资深开发那样回答四个问题：

1. 有没有更简单的做法？
2. 是否破坏了原有架构或分层？比如绕开已有抽象、把已有逻辑复制了一份。
3. 有没有硬编码、临时绕路，或者为了让测试通过加的特判？
4. 规模变大以后（并发、设备数、用户数、长时间运行）哪里先出问题？对本仓还要加一问：**内存/性能预算有没有被突破**。

有设计问题就写进 `REVIEW: changes`，逐条给理由。取舍需要拍板的，发 `ASK:`。
