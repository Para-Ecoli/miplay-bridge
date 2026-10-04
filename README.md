# MiPlay Bridge（妙播桥 · 小米妙播 / DLNA 接收直出）

<p align="center">把 NAS 或小主机变成小米妙播（MiPlay）与 DLNA 双协议接收器：手机一键投送，宿主声卡实时直出。</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-CC%20BY--NC--SA%204.0-orange.svg" alt="License: CC BY-NC-SA 4.0" /></a>
  <a href="#docker-镜像"><img src="https://img.shields.io/badge/Platform-Docker-4f46e5" alt="Platforms" /></a>
  <a href="#docker-compose-部署"><img src="https://img.shields.io/badge/Docker-Compose-2496ED?logo=docker&logoColor=white" alt="Docker Compose" /></a>
</p>

> **项目说明**：MiPlay Bridge 通过 Docker 容器部署，面向拥有物理音频输出口（3.5mm 音频口、主板音频口、USB DAC 等）的 NAS 或小主机。它负责接收局域网内的「投送」——小米手机妙播协议与标准 DLNA 投音，两条链路汇聚到同一块声卡直出，把闲置的小主机变成一台随取随用的网络音箱。

当前稳定版本：`0.1.0`

---

## 0.1.0 首发特性

- **小米妙播原生接收**：完整实现米连（mi-connect）私有协议栈——mDNS 发现应答、TCP 8899 二进制控制通道、挑战应答鉴权、现代 MIUI 的 Safety 加密通道（AES-128-CBC 状态化）、反向 WFD（Wi-Fi Display）RTSP 协商与 RTP/MPEG-TS 媒体接收。手机在「妙播」设备列表中直接看到本机，点击即投。
- **DLNA / UPnP 标准渲染器**：SSDP 发现、SOAP 控制（AVTransport / RenderingControl / ConnectionManager）、GENA 事件订阅全实现。任何支持 DLNA 投音的手机、电脑或 App 均可投送，`FLAC/WAV` 等格式可位完美（bit-perfect）输出。
- **双引擎 + 声卡仲裁**：妙播实时流走 `ffmpeg → aplay` 低延迟管线；DLNA 与 HTTP API 走 MPD 播放内核（位置/拖动/元数据能力完备）。二者共用同一块独占声卡（无 dmix），当前音源持有声卡，冲突请求返回 `409 device_busy`——不会出现两个音源互相争抢爆音。
- **一键清理与内嵌控制台**：内置网页控制台（`http://NAS_IP:8092`），一键「清除妙播连接」「清除 DLNA 连接」「测试声卡」，实时显示声卡/混音器/内核/双协议状态。
- **硬件自动就绪**：启动自检强制修复 `Auto-Mute Mode` 静音陷阱（ALC269VB 等编解码器的出厂默认会静默静音两个输出口），完成 Master/Headphone 解静音与硬件音量校准，失败拒绝启动——避免“接口全绿但没声音”。
- **全平台 Docker 原生支持**：镜像由仓库内 Dockerfile 本地多阶段构建（纯 Go 静态二进制 + Alpine 运行层），不锁定架构，`linux/amd64`、`linux/arm64` 等 Docker 宿主均可直接构建运行，覆盖飞牛 fnOS、群晖 DSM、铁威马 TOS、威联通 QNAP、绿联 UGnas 及一切支持 Docker 的环境。

---

## 功能简介

- **手机投歌，音箱发声**：小米手机控制中心「妙播」一键投送至 NAS 物理音箱；支持播放/暂停/恢复/关闭/音量联动，手机端音量与硬件混音器全量对齐。
- **DLNA 全兼容**：Windows「投放」、各类音乐 App 的 DLNA 投音均可识别本机；支持 Seek 拖动、元数据（标题/艺术家/专辑）展示与传输状态事件推送。
- **HTTP API 与健康探测**：提供一套稳定的 REST API、测试播放路由（`/api/test-play`）与健康检查端点（`/healthz`），便于外部面板或运维脚本接入。
- **轻量常驻**：纯 Go 外壳 + 独立 MPD/ffmpeg 进程，内存上限 256MB，CPU 占用接近于 0。

---

## 工作原理（一张图）

```text
小米手机（妙播）── mDNS 发现 + TCP8899 控制 + 反向 WFD(RTSP/RTP-TCP/MPEG-TS)
                                                      └─ ffmpeg 解码 → aplay ──┐
手机/电脑（DLNA 投音）── SSDP + SOAP + GENA ─ MPD（HTTP URL 拉流播放）─────────┼─▶ ALSA hw:X,Y 直出（独占）
浏览器 / 控制端 / 运维 ── HTTP API (8092) ─ 状态 / 音量 / 断连 / 声卡测试 ─────┘
    └─ 内嵌控制台（GET /）：一键清除妙播连接、一键清除 DLNA 连接、一键声卡测试
```

> [!NOTE]
> **音质实况说明**：妙播协议本体即 AAC 48kHz 立体声（发送端转码所致），桥接端不做二次降质；DLNA 直投 `FLAC/WAV` 等无损格式时可位完美输出（MPD 已配置 `auto_resample no`）。

---

## Docker 镜像

正式版本镜像由 GitHub Actions 在推送 `v*.*.*` tag 时自动构建并发布至 GitHub Container Registry（多架构 `linux/amd64` + `linux/arm64`）：

```text
ghcr.io/para-ecoli/miplay-bridge:latest     # 最新正式版
ghcr.io/para-ecoli/miplay-bridge:0.x.y      # 精确版本（生产环境建议固定此形式）
ghcr.io/para-ecoli/miplay-bridge:sha-xxxxx  # 按提交回溯，便于回滚
```

镜像内的版本号由 git tag 在构建期注入（`/api/status` 与控制台显示的即真实版本）；本地自行构建未传入版本时显示 `dev`。

同时保留本地构建路径：仓库自带 [`Dockerfile`](Dockerfile) 多阶段构建（`CGO_ENABLED=0` 静态编译纯 Go 外壳 + Alpine 运行层，`mpd` / `alsa-utils` / `ffmpeg` 均以独立进程运行），离线环境执行 `docker compose up -d --build` 即可，无需任何镜像仓库。

---

## Docker Compose 部署

> [!IMPORTANT]
> **两个必需项**：
> 1. **`network_mode: host` 必须开启**——妙播依赖 mDNS（5353/udp）、DLNA 依赖 SSDP（1900/udp）组播发现，组播无法穿透 Docker 默认桥接网络；
> 2. **`/dev/snd` 必须挂载**——本项目直接驱动硬件声卡。宿主没有声卡时容器按预期以退出码 `3` 退出。

根目录提供了标准的 [`docker-compose.yml`](docker-compose.yml)：

```yaml
services:
  miplay-bridge:
    image: ghcr.io/para-ecoli/miplay-bridge:latest   # 预构建镜像（docker compose pull）
    build:
      context: .                    # 镜像不可用/离线时回退本地构建
      dockerfile: Dockerfile
    container_name: miplay-bridge
    restart: unless-stopped
    network_mode: host              # 组播发现必需：mDNS 5353 / SSDP 1900
    devices:
      - "/dev/snd:/dev/snd"         # 声卡直出必需
    volumes:
      - "./data:/data:rw"           # 设备身份持久化（妙播 UUID / DLNA UDN）
    environment:
      BRIDGE_PORT: "8092"
      ALSA_CARD: "0"                # 声卡编号（cat /proc/asound/cards）
      ALSA_PCM_DEVICE: "0"
      MPD_MIXER_CONTROL: "Master"   # MPD 音量控制使用的 ALSA 混音器
      MIPLAY_ENABLED: "true"
      MIPLAY_NAME: "妙播桥"
      DLNA_ENABLED: "true"
      DLNA_FRIENDLY_NAME: "妙播桥"
      BRIDGE_LOG_LEVEL: "info"
      TZ: "Asia/Shanghai"
    mem_limit: 256m
    healthcheck:
      test: ["CMD", "/usr/local/bin/miplay-bridge", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 20s
```

> [!NOTE]
> 如需通过 API 播放宿主曲库（`/api/play` 传 `file://` 路径或库内相对路径），在 `volumes` 中追加 `- "./music:/music:ro"`，并将曲目放入该目录即可。

### 部署与启动命令

```bash
git clone https://github.com/Para-Ecoli/miplay-bridge.git
cd miplay-bridge
docker compose pull               # 拉取 ghcr 预构建镜像（首版发布前尚无镜像，可跳过）
docker compose up -d              # 镜像不可用时自动回退本地构建
docker compose logs -f            # 观察启动自检输出
curl http://127.0.0.1:8092/healthz
```

- **更新版本**：`git pull && docker compose pull && docker compose up -d`；
- **离线 / 自行构建**：`docker compose up -d --build`（首次需数分钟；后续更新用 `git pull && docker compose up -d --build`）。

启动后浏览器打开 `http://NAS_IP:8092` 即可看到内嵌控制台。

---

## 使用说明

### 1. 小米妙播投送

1. 确保手机与本机处于**同一局域网**（同一 Vlan / 未开启 AP 隔离），且宿主防火墙放行 `5353/udp` 与 `8899/tcp`；
2. 手机端打开「妙播」（控制中心 → 妙播，或系统设置内），在设备列表中选择 `妙播桥`；
3. 选择要投送的音频后即开始播放，手机端可暂停/恢复/调节音量，NAS 声卡实时出声。

> 设备名称由 `MIPLAY_NAME` 控制；修改后需重启容器，手机端可能需下拉刷新设备列表。

### 2. DLNA 投音

1. 手机 / 电脑上的任意 DLNA 投音入口（如系统「投放」、支持 DLNA 的音乐 App）中选择 `妙播桥`；
2. 支持播放 / 暂停 / 停止 / 拖动进度 / 音量 / 静音，曲目标题与艺术家会显示在控制台。

### 3. 内嵌控制台（`http://NAS_IP:8092`）

- **状态卡片**：服务与版本、声卡（编号 / 选中设备 `hw:X,Y` / Auto-Mute 修复态）、MPD 内核、当前音源（`miplay | dlna | api | idle`）、妙播（广播中 / 已连接 / 来源设备名）与 DLNA（友好名 / 订阅数 / 传输态 / 当前曲目）；
- **一键清除妙播连接** → `POST /api/miplay/disconnect`：关闭控制会话与反向 WFD 连接、终止实时管线、释放声卡；
- **一键清除 DLNA 连接** → `POST /api/dlna/disconnect`：停止播放、清空队列与元数据、向订阅者推送 STOPPED 并释放全部事件订阅；
- **测试声卡** → `POST /api/test-play`：经 MPD 走生产同路径播放 440Hz 测试音（默认 2 秒）；
- 音量滑块 / 静音与「全部停止」按钮；
- 设置了 `BRIDGE_TOKEN` 时，页面提供 token 输入（保存在浏览器 localStorage，经 `x-bridge-token` 头调用）。

### 4. HTTP API 速查

| 方法 | 路径 | 说明 |
| :--- | :--- | :--- |
| GET | `/healthz`、`/api/healthz` | 健康探测（免鉴权，容器 healthcheck 使用） |
| GET | `/api/status` | 全量状态：声卡/混音器/内核/playback/`active_source`/`miplay`/`dlna` |
| GET | `/api/devices` | 声卡列表与当前选择 |
| POST | `/api/play` | 播放 URL：`{"url": "http://...", "title": "...", "artist": "...", "album": "...", "volume": 60, "position": 30, "start_paused": false}` |
| POST | `/api/control` | 动作：`play / pause / resume / toggle / stop / stop_all / next / prev / seek / repeat / random / single / consume`（别名 `unpause`、`previous`、`shuffle`；`stop_all` 同时清除妙播与 DLNA 会话） |
| POST | `/api/volume` | `{"volume": 60}` 或 `{"mute": true}` |
| POST | `/api/test-play` | 一键声卡测试音 |
| POST | `/api/miplay/disconnect` | 一键清除妙播连接（幂等，返回 `was_connected`） |
| POST | `/api/dlna/disconnect` | 一键清除 DLNA 连接（幂等，返回 `was_active`） |
| GET | `/tone/{token}/tone.wav` | 测试音内部服务路径（短时效 token，供 MPD 回环拉取） |

设置 `BRIDGE_TOKEN` 后，所有 `/api/*` 请求需携带 `x-bridge-token: <token>`（兼容 `x-local-output-token` 头与 `Authorization: Bearer <token>`）；`/healthz`、`/` 控制台页面与 `/dlna/*` 设备路径按协议要求保持开放。

示例：

```bash
curl -X POST http://127.0.0.1:8092/api/test-play
curl -X POST http://127.0.0.1:8092/api/miplay/disconnect
curl -X POST http://127.0.0.1:8092/api/dlna/disconnect
curl http://127.0.0.1:8092/api/status | head -c 800
```

---

## 声卡探测与配置说明

如有多块声卡或外接 USB DAC，可在 NAS 终端查看声卡编号：

```bash
cat /proc/asound/cards
```

例如输出：

```text
 0 [PCH            ]: HDA-Intel - HDA Intel PCH
 1 [DAC            ]: USB-Audio - USB Audio DAC
```

若需使用声卡 1（USB DAC），在环境变量中设置 `ALSA_CARD=1` 并重启服务即可；`ALSA_PCM_DEVICE` 对应设备号码，默认为 `0`。
容器内**不读取 `/proc/asound`**（容器环境中不可见），自检完全走 `/dev/snd` 与 ALSA control API（`amixer`）。

### 延迟调节（妙播实时管线）

妙播链路为低延迟实时播放，主旋钮为 `MIPLAY_ALSA_BUFFER_US`（`aplay` 缓冲，单位微秒，默认 `200000` 即 200ms）：

- 家中 Wi-Fi 拥塞、偶发卡顿：可上调至 `300000~500000` 增强抗抖动；
- 追求更低延迟（如配合视频）：可下调至 `100000` 左右，但过小可能产生爆音。

---

## 环境变量一览

所有变量均有可用默认值，**空环境即可启动**。

| 变量 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `BRIDGE_PORT` | `8092` | HTTP API / 控制台 / DLNA 描述端口 |
| `BRIDGE_BIND` | `0.0.0.0` | HTTP 监听地址 |
| `BRIDGE_TOKEN` | 空 | 设置后开启 `/api/*` 鉴权 |
| `BRIDGE_DATA_DIR` | `/data` | 设备身份持久化目录（务必持久化） |
| `BRIDGE_LOG_LEVEL` | `info` | `debug / info / warn / error` |
| `BRIDGE_TEST_TONE_SECONDS` | `2` | 测试音时长（1~30 秒） |
| `BRIDGE_TEST_TONE_HZ` | `440` | 测试音频率（20~20000 Hz） |
| `ALSA_SOUND_DIRECTORY` | `/dev/snd` | ALSA 设备节点目录 |
| `ALSA_CARD` | `0` | 声卡编号 |
| `ALSA_PCM_DEVICE` | `0` | PCM 设备号 |
| `ALSA_DEFAULT_VOLUME` | `80` | 启动时应用的硬件音量（0~100） |
| `ALSA_FIX_AUTO_MUTE` | `true` | 启动强制关闭 Auto-Mute 静音陷阱 |
| `ALSA_AUTO_MUTE_CONTROL` | `Auto-Mute Mode` | Auto-Mute 控制名 |
| `ALSA_MASTER_CONTROL` / `ALSA_HEADPHONE_CONTROL` / `ALSA_SPEAKER_CONTROL` | `Master` / `Headphone` / `Speaker` | 启动解静音的控制名 |
| `AMIXER_PATH` | `amixer` | alsa-utils 混音器路径 |
| `MPD_BINARY` | `mpd` | 播放内核路径 |
| `MPD_CONF_PATH` | `/etc/mpd.conf` | 渲染后的 MPD 配置路径 |
| `MPD_TEMPLATE_PATH` | `/etc/miplay-bridge/mpd.conf.template` | MPD 配置模板路径 |
| `MPD_HOST` / `MPD_PORT` | `127.0.0.1:6600` | 内核回环控制地址（不对局域网暴露） |
| `MPD_MUSIC_DIRECTORY` | `/music` | MPD 曲库目录（只读挂载） |
| `MPD_DATA_DIRECTORY` | `/var/lib/mpd` | MPD 数据目录 |
| `MPD_MIXER_CONTROL` | `Headphone` | MPD 硬件混音器控制名（不存在时自动降级软件混音） |
| `MPD_START_TIMEOUT_SECONDS` | `20` | 内核启动等待上限 |
| `MPD_MAX_RESTARTS` | `5` | 内核自动重启次数上限 |
| `MPD_LOG_FILE` | 空 | MPD 日志文件路径；为空时随容器日志输出（stderr） |
| `MIPLAY_ENABLED` | `true` | 妙播接收总开关 |
| `MIPLAY_CONTROL_PORT` | `8899` | 妙播二进制控制端口 |
| `MIPLAY_NAME` | `妙播桥` | 手机妙播设备列表显示名 |
| `MIPLAY_ADVERTISE_ADDRESS` | 自动探测 | 指定对手机广播的 LAN IPv4 |
| `MIPLAY_HANDSHAKE_TIMEOUT_SECONDS` | `15` | 控制通道鉴权超时 |
| `MIPLAY_ALSA_BUFFER_US` | `200000` | `aplay` 缓冲（微秒），延迟主旋钮 |
| `FFMPEG_PATH` / `APLAY_PATH` | `ffmpeg` / `aplay` | 实时管线二进制路径 |
| `DLNA_ENABLED` | `true` | DLNA 渲染器总开关 |
| `DLNA_FRIENDLY_NAME` | `妙播桥` | DLNA 设备显示名 |
| `DLNA_ADVERTISE_ADDRESS` | 自动探测 | 指定 SSDP LOCATION 使用的 LAN IPv4 |
| `DLNA_SSDP_MAX_AGE` | `1800` | SSDP 通告有效期（秒） |

---

## 常见问题（FAQ）

**Q1：手机妙播里找不到「妙播桥」？**

- 确认容器运行在 `network_mode: host`（bridge 网络收不到 mDNS 组播）；
- 确认宿主防火墙放行 `5353/udp`（并允许组播地址 `224.0.0.251`）与 `8899/tcp`；
- 确认手机与 NAS 同网段且未开启 AP 隔离；多网卡主机可用 `MIPLAY_ADVERTISE_ADDRESS` 指定广播网卡 IP；
- 与宿主 avahi 共存冲突时：本服务使用 Go 的多播地址复用绑定，一般可与 avahi 并行；若路由器启用了 IGMP 嗅探异常，可在宿主临时停用 avahi 验证（`systemctl stop avahi-daemon`）。

**Q2：DLNA 设备搜不到？**

- 同类检查 `1900/udp` 组播；播放器需支持「DLNA 投音 / 投放到此设备」；
- 修改 `DLNA_FRIENDLY_NAME` 后需重启容器，部分控制端有设备缓存。

**Q3：控制台一切正常，但完全没有声音？**

按顺序排查：

1. 控制台「测试声卡」是否出声？不出声先检查音箱接线与宿主声卡选择（`ALSA_CARD`）；
2. 看状态里的 `auto_mute_fixed` 是否为 `true`；为 `false` 时说明 Auto-Mute 未修复成功，多为控制名不匹配（宿主执行 `amixer -c 0 scontrols` 对照设置 `ALSA_AUTO_MUTE_CONTROL`）；
3. 查看 `docker compose logs` 是否有 mixer 告警；
4. 妙播链路额外确认 `MIPLAY_ADVERTISE_ADDRESS` 与 Safety 密钥相关报错。

**Q4：接口返回 `409 device_busy` 是什么？**

本机声卡为独占模式（无 dmix），同一时刻只允许一个音源。当妙播正在投送时再发起 DLNA 投音（或反之）会返回 `409`，这是**预期行为**：在控制台点击「清除妙播连接」或「清除 DLNA 连接」释放声卡后再投送即可。

**Q5：妙播播放正常但延迟偏高 / 偶发卡顿？**

调节 `MIPLAY_ALSA_BUFFER_US`（见上文「延迟调节」）。

**Q6：容器启动失败，如何判断原因？**

按退出码对照：

| 退出码 | 含义 | 处理 |
| :--- | :--- | :--- |
| `0` | 正常运行 / 收到停止信号 | — |
| `2` | 配置无效 | 检查环境变量格式（端口范围、布尔值拼写等） |
| `3` | 未检测到声卡 | 检查 `/dev/snd` 挂载与宿主声卡 |
| `4` | 混音器初始化失败 | 检查 `ALSA_*` 控制名与 `amixer` 可用性 |
| `5` | MPD 内核启动失败 | 查看日志内 MPD 尾部输出（目录权限/配置） |
| `6` | 妙播接收端或 DLNA 渲染器启动失败 | 检查 5353/1900 组播绑定与端口占用 |

**Q7：重启容器后手机里出现多个同名设备？**

`/data` 目录未持久化。妙播设备 UUID（`/data/miplay-device-id`）与 DLNA UDN（`/data/dlna-udn`）必须跨容器重建保持不变，请确保 `BRIDGE_DATA_DIR` 指向持久卷。

---

> **免责声明**：本项目为协议互操作性研究与实践，与小米公司无任何关联；「小米」「妙播」等商标归其各自权利人所有。
