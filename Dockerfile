# ============================================================
# MiPlay Bridge（妙播桥）伴生容器
# ============================================================
# 自研 Go 外壳（cmd/miplay-bridge） + MPD 播放内核 + ffmpeg 实时解码管线。
# MPD / ffmpeg 均为独立进程，不链接、不修改、不内嵌其源码。
#
# 关键点：
#   · 基础镜像固定 Alpine 3.20；该版本的 mpd 0.23.15 实测包含 alsa 输出插件，
#     并自带 flac/mp3/aac/ogg/opus/wavpack/DSD 解码器与 ffmpeg 兜底解码；
#   · alsa-utils 提供 amixer 与 aplay —— amixer 用于 Auto-Mute 强制解静音与
#     硬件音量；aplay 是妙播实时管线（ffmpeg 解码后）的 ALSA 输出端；
#   · ffmpeg 用于把妙播的 MPEG-TS/AAC 实时流解码为 48kHz 立体声 PCM；
#   · 容器内读不到 /proc/asound，所有自检必须走 /dev/snd + ALSA control API；
#   · mDNS(5353/udp) 与 SSDP(1900/udp) 组播发现要求 host 网络模式，
#     docker-compose.yml 已固化 network_mode: host。

FROM golang:1.24-alpine AS build
# 版本号在构建期注入：CI 发布流水线传入 git tag（如 0.1.0），
# 本地构建不传时回落为 dev，避免与正式发布版本混淆。
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X github.com/miplay-bridge/miplay-bridge/internal/api.Version=${VERSION}" \
    -o /out/miplay-bridge ./cmd/miplay-bridge

FROM alpine:3.20
# 最终阶段重新声明 VERSION，用于 OCI 标签（与二进制内注入的版本同源）。
ARG VERSION=dev
LABEL org.opencontainers.image.title="MiPlay Bridge" \
      org.opencontainers.image.description="Xiaomi MiPlay (妙播) and DLNA bridge: receives LAN audio casts and plays them through the host sound card via ALSA/MPD" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/Para-Ecoli/miplay-bridge" \
      org.opencontainers.image.licenses="CC-BY-NC-SA-4.0 AND GPL-2.0-only"

# mpd        : 播放内核（独立进程，GPL-2.0，不修改其代码）
# alsa-utils : amixer（Auto-Mute 初始化/硬件音量）与 aplay（妙播实时输出端）
# ffmpeg     : 妙播实时流 MPEG-TS/AAC -> PCM 解码（独立进程，不修改其代码）
RUN apk add --no-cache mpd alsa-utils ffmpeg ca-certificates tzdata \
    && mkdir -p /var/lib/mpd/playlists /music /data

COPY --from=build /out/miplay-bridge /usr/local/bin/miplay-bridge
COPY assets/mpd.conf.template /etc/miplay-bridge/mpd.conf.template

ENV BRIDGE_PORT=8092 \
    ALSA_CARD=0 \
    ALSA_PCM_DEVICE=0 \
    MPD_MIXER_CONTROL=Headphone \
    MIPLAY_CONTROL_PORT=8899 \
    TZ=Asia/Shanghai

# 8092/tcp HTTP（API/控制台/DLNA 描述），8899/tcp 妙播控制，1900/udp SSDP
# 与 5353/udp mDNS 仅在 host 网络模式下有效。
EXPOSE 8092 8899

HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=20s \
    CMD ["/usr/local/bin/miplay-bridge", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/miplay-bridge"]
