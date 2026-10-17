/*
 * 永不结束的 MJPEG-AVI 直播流：GET /live.avi。
 *
 * 帧就是 flash 里的 JPEG 原样字节——不解码、不编码，只是按 fps 重复发「当前帧」。
 * 换内容 = 下一帧换一块字节。这台电视（UA60DU7000）MJPEG 只到 1080p，实测：
 *   720p / 1080p 丝滑、无工具条；4K 报「格式不支持」，6 秒后断开。
 *
 * AVI 头把帧数和 movi 长度都写成「很大」，没有 idx1——播放器当它是一段很长的视频。
 * 用原始 socket 而不是 esp_http_server：后者是单任务，一个长连接会把控制面板堵死。
 */
#include "stream.h"

#include <stdlib.h>
#include <string.h>
#include <sys/param.h>

#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "lwip/sockets.h"

static const char *TAG = "stream";

#define BIG 0x7FFFFFF0u

static const frame_t *s_frames;
static jb_page_t s_pages[4];
static int s_n, s_fps, s_row0, s_rows;

/* 带的双缓冲。s_front 是已发布的那块；s_busy[i] = 正在发送它的连接数，
 * 写端只会去写 busy 为 0 的那块（stream_band_begin 会等）。 */
static SemaphoreHandle_t s_mx;
static uint8_t *s_band[2];
static size_t s_band_len[2];
static int s_band_page[2] = { -1, -1 };
static int s_front = -1;
static int s_busy[2];
static volatile int s_cur;
static volatile int s_clients;
static volatile uint32_t s_sent;
static volatile int64_t s_last_us;
static uint8_t s_hdr[224];
static size_t s_hdr_len;

int stream_frame_count(void) { return s_n; }
const jb_page_t *stream_page(int idx) { return &s_pages[idx]; }

uint8_t *stream_band_begin(void)
{
    while (1) {
        xSemaphoreTake(s_mx, portMAX_DELAY);
        int back = s_front == 0 ? 1 : 0;
        bool free_ = s_busy[back] == 0;
        xSemaphoreGive(s_mx);
        if (free_) return s_band[back];
        vTaskDelay(pdMS_TO_TICKS(20));
    }
}

size_t stream_band_cap(void) { return BAND_CAP; }

bool stream_band_publish(uint8_t *buf, size_t len, int page)
{
    /* 越界在写入的时候就已经发生了，这里拦不住它。但发布一个超长的 len
     * 会让推流循环每帧都读到缓冲区外的垃圾、并推给电视——那是第二个 bug，
     * 而且比第一个更难查。宁可画面停在上一版。 */
    if (len > BAND_CAP) {
        ESP_LOGE(TAG, "带长度 %u 超过 BAND_CAP %u，拒绝发布（画面保持上一版）",
                 (unsigned)len, (unsigned)BAND_CAP);
        return false;
    }
    int i = buf == s_band[0] ? 0 : 1;
    xSemaphoreTake(s_mx, portMAX_DELAY);
    s_band_len[i] = len;
    s_band_page[i] = page;
    s_front = i;
    xSemaphoreGive(s_mx);
    return true;
}
int stream_current(void) { return s_cur; }
void stream_set(int idx) { s_cur = ((idx % s_n) + s_n) % s_n; }
int stream_clients(void) { return s_clients; }
uint32_t stream_frames_sent(void) { return s_sent; }
int64_t stream_last_active_us(void) { return s_last_us; }

/* ── AVI 头 ── */
static uint8_t *p32(uint8_t *p, uint32_t v) { memcpy(p, &v, 4); return p + 4; }  /* S3 是小端 */
static uint8_t *p16(uint8_t *p, uint16_t v) { memcpy(p, &v, 2); return p + 2; }
static uint8_t *pcc(uint8_t *p, const char *s) { memcpy(p, s, 4); return p + 4; }

static void build_header(int w, int h, uint32_t maxframe)
{
    uint8_t *p = s_hdr;
    p = pcc(p, "RIFF"); p = p32(p, BIG); p = pcc(p, "AVI ");
    p = pcc(p, "LIST"); p = p32(p, 4 + (8 + 56) + (12 + (8 + 56) + (8 + 40))); p = pcc(p, "hdrl");

    p = pcc(p, "avih"); p = p32(p, 56);
    p = p32(p, 1000000 / s_fps);   /* dwMicroSecPerFrame */
    p = p32(p, maxframe * s_fps);  /* dwMaxBytesPerSec */
    p = p32(p, 0); p = p32(p, 0);  /* padding, flags（无索引） */
    p = p32(p, BIG);               /* dwTotalFrames */
    p = p32(p, 0); p = p32(p, 1);  /* initial frames, streams */
    p = p32(p, maxframe);          /* dwSuggestedBufferSize */
    p = p32(p, w); p = p32(p, h);
    p = p32(p, 0); p = p32(p, 0); p = p32(p, 0); p = p32(p, 0);

    p = pcc(p, "LIST"); p = p32(p, 4 + (8 + 56) + (8 + 40)); p = pcc(p, "strl");
    p = pcc(p, "strh"); p = p32(p, 56);
    p = pcc(p, "vids"); p = pcc(p, "MJPG");
    p = p32(p, 0); p = p16(p, 0); p = p16(p, 0);   /* flags, priority, language */
    p = p32(p, 0); p = p32(p, 1); p = p32(p, s_fps); /* initial frames, scale, rate */
    p = p32(p, 0); p = p32(p, BIG);                /* start, length */
    p = p32(p, maxframe); p = p32(p, 0xFFFFFFFF); p = p32(p, 0); /* buffer, quality, samplesize */
    p = p16(p, 0); p = p16(p, 0); p = p16(p, w); p = p16(p, h);

    p = pcc(p, "strf"); p = p32(p, 40);
    p = p32(p, 40); p = p32(p, w); p = p32(p, h);
    p = p16(p, 1); p = p16(p, 24); p = pcc(p, "MJPG");
    p = p32(p, w * h * 3); p = p32(p, 0); p = p32(p, 0); p = p32(p, 0); p = p32(p, 0);

    p = pcc(p, "LIST"); p = p32(p, BIG); p = pcc(p, "movi");
    s_hdr_len = p - s_hdr;
}

/* ── 连接 ── */
static bool send_all(int fd, const void *buf, size_t len)
{
    const uint8_t *b = buf;
    while (len) {
        int r = send(fd, b, MIN(len, 16384), 0);
        if (r <= 0) return false;
        b += r; len -= r;
    }
    return true;
}

static const char RESP[] =
    "HTTP/1.0 200 OK\r\n"
    "Content-Type: video/avi\r\n"
    "transferMode.dlna.org: Streaming\r\n"
    "contentFeatures.dlna.org: DLNA.ORG_OP=00;DLNA.ORG_CI=0;"
    "DLNA.ORG_FLAGS=01700000000000000000000000000000\r\n"
    "Connection: close\r\n\r\n";

static void client_task(void *arg)
{
    int fd = (int)(intptr_t)arg;
    char req[512];
    int got = 0;
    while (got < (int)sizeof req - 1) {
        int r = recv(fd, req + got, sizeof req - 1 - got, 0);
        if (r <= 0) break;
        got += r; req[got] = 0;
        if (strstr(req, "\r\n\r\n")) break;
    }
    req[got] = 0;
    bool head = strncmp(req, "HEAD ", 5) == 0;
    bool get = strncmp(req, "GET ", 4) == 0;
    const char *path = strchr(req, ' ');
    if ((!head && !get) || !path || strncmp(path + 1, "/live.avi", 9) != 0) {
        static const char nf[] = "HTTP/1.0 404 Not Found\r\nConnection: close\r\n\r\n";
        send_all(fd, nf, sizeof nf - 1);
        goto out;
    }
    if (!send_all(fd, RESP, sizeof RESP - 1) || head) goto out;

    s_clients++;
    ESP_LOGW(TAG, "电视开始拉流（连接数 %d）", s_clients);
    int64_t t0 = esp_timer_get_time();
    uint32_t n = 0;
    bool ok = send_all(fd, s_hdr, s_hdr_len);
    while (ok) {
        const int cur = s_cur;
        const jb_page_t *pg = &s_pages[cur];
        /* 有对应本页的带就拼进去，否则原样发页面（带里是纯背景色） */
        int bi = -1;
        xSemaphoreTake(s_mx, portMAX_DELAY);
        if (s_front >= 0 && s_band_page[s_front] == cur) { bi = s_front; s_busy[bi]++; }
        xSemaphoreGive(s_mx);

        size_t a = pg->row_off[s_row0], b = pg->row_off[s_row0 + s_rows];
        size_t len = bi >= 0 ? a + s_band_len[bi] + (pg->len - b) : pg->len;
        uint8_t ch[8];
        pcc(ch, "00dc"); p32(ch + 4, len);
        static const uint8_t pad = 0;
        if (bi >= 0)
            ok = send_all(fd, ch, 8) && send_all(fd, pg->jpg, a) &&
                 send_all(fd, s_band[bi], s_band_len[bi]) && send_all(fd, pg->jpg + b, pg->len - b);
        else
            ok = send_all(fd, ch, 8) && send_all(fd, pg->jpg, pg->len);
        ok = ok && (!(len & 1) || send_all(fd, &pad, 1));
        if (bi >= 0) {
            xSemaphoreTake(s_mx, portMAX_DELAY);
            s_busy[bi]--;
            xSemaphoreGive(s_mx);
        }
        if (!ok) break;
        n++; s_sent++; s_last_us = esp_timer_get_time();
        int64_t due = t0 + (int64_t)n * 1000000 / s_fps;
        int64_t wait = due - esp_timer_get_time();
        if (wait > 0) vTaskDelay(pdMS_TO_TICKS(wait / 1000));
    }
    s_clients--;
    ESP_LOGW(TAG, "流断开：发了 %lu 帧，%.0f 秒", (unsigned long)n,
             (esp_timer_get_time() - t0) / 1e6);
out:
    shutdown(fd, SHUT_RDWR);
    close(fd);
    vTaskDelete(NULL);
}

static void server_task(void *arg)
{
    (void)arg;
    int ls = socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    int one = 1;
    setsockopt(ls, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
    struct sockaddr_in a = { .sin_family = AF_INET, .sin_port = htons(STREAM_PORT) };
    if (bind(ls, (struct sockaddr *)&a, sizeof a) || listen(ls, 4)) {
        ESP_LOGE(TAG, "bind/listen :%d 失败", STREAM_PORT);
        vTaskDelete(NULL);
    }
    while (1) {
        int fd = accept(ls, NULL, NULL);
        if (fd < 0) continue;
        struct timeval tv = { .tv_sec = 15 };   /* 电视卡住不读就放弃，别让任务永远挂着 */
        setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof tv);
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv);
        setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof one);
        if (xTaskCreate(client_task, "avi", 4096, (void *)(intptr_t)fd, 5, NULL) != pdPASS) {
            ESP_LOGE(TAG, "建连接任务失败（堆不够？）");
            close(fd);
        }
    }
}

void stream_start(const frame_t *frames, int n, int fps, int band_row0, int band_rows)
{
    s_frames = frames; s_n = n < 4 ? n : 4; s_fps = fps;
    s_row0 = band_row0; s_rows = band_rows;
    s_mx = xSemaphoreCreateMutex();
    /* T3 只发预置页，不发布动态条带：band_rows=0 时省掉两块 24KB 缓冲，
     * 把堆留给 MQTT 的 TLS 长连接。T4 接看板渲染时再传非 0 值。 */
    if (band_rows > 0) {
        s_band[0] = malloc(BAND_CAP);
        s_band[1] = malloc(BAND_CAP);
        if (!s_band[0] || !s_band[1]) ESP_LOGE(TAG, "带缓冲分配失败");
    }

    int w = 0, h = 0;
    uint32_t maxf = 0;
    for (int i = 0; i < s_n; i++) {
        if (!jb_parse(&s_pages[i], frames[i].data, frames[i].len)) {
            ESP_LOGE(TAG, "页面 %d 解析失败（需要 render_pages.py 生成的 DRI JPEG）", i);
            continue;
        }
        if (i == 0) { w = s_pages[i].w; h = s_pages[i].h; }
        else if (s_pages[i].w != w || s_pages[i].h != h)
            ESP_LOGE(TAG, "页面 %d 分辨率与首页不同——电视会出错", i);
        maxf = MAX(maxf, frames[i].len + BAND_CAP + 8);
    }
    build_header(w, h, maxf);
    ESP_LOGI(TAG, "%d 页，%dx%d，%d fps，带 = MCU 行 %d..%d", s_n, w, h, fps, band_row0,
             band_row0 + band_rows - 1);
    xTaskCreate(server_task, "avi_srv", 3072, NULL, 5, NULL);
}
