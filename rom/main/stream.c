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
#include "framepack.h"
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

/* 起播 burst：电视刚连上时先不按 fps 节拍，连发这么多帧再回到稳态。
 *
 * 为什么要 burst：稳态是 2fps，电视的 MJPEG 解码器拿到第一帧并不等于
 * 就出画——它要先攒够内部缓冲才开始送显。按 2fps 喂，攒缓冲这段时间
 * 全花在等下一帧上，「连上到出画」被节拍白白拖长好几秒。burst 把这段
 * 等待压成一次连续传输，之后再回到 2fps 省带宽。
 *
 * 为什么是 8 而不是更多：帧约 180KB，8 帧就是 1.4MB，按实测吞吐大约
 * 0.4~1 秒发完。再多只是多占 Wi-Fi，电视那边早就开始显示了；而 burst
 * 期间这个连接任务是不睡的，会一直占着发送路径。 */
#define BURST_FRAMES 8

static const frame_t *s_frames;
static jb_page_t s_pages[4];
static int s_n, s_fps, s_row0, s_rows;

/* 带的双缓冲。s_front 是已发布的那块；s_busy[i] = 正在发送它的连接数，
 * 写端只会去写 busy 为 0 的那块（stream_band_begin 会等）。 */
static SemaphoreHandle_t s_mx;
static uint8_t *s_band[2];
static size_t s_band_len[2];
static int s_band_page[2] = { -1, -1 };

/* 帧包模式（T13b）。底帧来自 framepack 的 flash 映射地址，推流时直接从
 * 那里发，整帧从不在 RAM 里拼起来。 */
static bool             s_fp_mode;
static const fp_index_t *s_fp_ix;
static const uint8_t    *s_fp_base;
static size_t            s_fp_base_len;
/* 双缓冲里每一块的行布局，和 s_band[] 一一对应。 */
static uint32_t s_row_off[2][FP_BAND_ROWS];
static uint32_t s_row_len[2][FP_BAND_ROWS];
static int s_front = -1;
static int s_busy[2];
/* 正在发送某一帧、且手里的段指针指向 s_fp_base 的连接数。
 * 换帧包时要等它归零再换，否则会把旧的 flash 映射从正在发送的段底下
 * 抽走。详见 fp_wait_idle 的注释。 */
static int s_fp_hold;
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

/* 等到没有任何连接的段指针还指着 s_fp_base。
 *
 * 为什么非等不可：发送循环在每帧开头把 s_fp_base 快照进 segs[]，之后的
 * send_all 不再回头看共享状态——这正是「换帧包不打断发送」的实现方式。
 * 代价是这段时间里 base 指向的 flash 映射必须一直有效，而帧包换代时要
 * 去释放旧槽的映射，抽走的正是这块地址。
 *
 * 等待有上界：连接的 SO_SNDTIMEO 是 15 秒，一帧要么发完要么超时退出，
 * 两种情况都会把 hold 放掉。等待发生在加载帧包的任务里，不在发送路径
 * 上，所以等的这几百毫秒不会让任何一条流停顿。 */
static void fp_wait_idle(void)
{
    int64_t t0 = esp_timer_get_time();
    while (1) {
        xSemaphoreTake(s_mx, portMAX_DELAY);
        bool idle = s_fp_hold == 0;
        xSemaphoreGive(s_mx);
        if (idle) break;
        vTaskDelay(pdMS_TO_TICKS(20));
    }
    int64_t ms = (esp_timer_get_time() - t0) / 1000;
    if (ms > 1000) ESP_LOGW(TAG, "换底帧前等在途的帧发完，等了 %lld 毫秒", ms);
}

/* 切模式只改「下一帧要发哪些字节」，绝不碰正在进行的 HTTP 响应：不关
 * socket、不改已经发出去的 AVI 头、不重发响应头，调用方也不需要重新
 * SetAVTransportURI。电视看到的只是 movi 里下一个 00dc 块的内容变了——
 * AVI 每块自带长度，内容换了不需要它重新握手，所以不进断流检查。 */
void stream_use_framepack(const struct fp_index *ix, const uint8_t *base, size_t base_len)
{
    fp_wait_idle();
    xSemaphoreTake(s_mx, portMAX_DELAY);
    s_fp_ix = ix; s_fp_base = base; s_fp_base_len = base_len;
    s_fp_mode = true;
    /* 旧的带是给预置页拼的，在帧包模式下毫无意义。不作废的话，第一帧
     * 会拿预置页的带去配底帧，发出去是花屏。
     *
     * 作废只是把 s_front 置 -1，不动两块缓冲里的字节——正在发送其中一块
     * 的连接已经靠 s_busy 占住了它，这里置位不会让它读到被改写的内容。 */
    s_front = -1;
    s_band_page[0] = s_band_page[1] = -1;
    xSemaphoreGive(s_mx);
    ESP_LOGI(TAG, "切到帧包模式，底帧 %u 字节", (unsigned)base_len);
}

void stream_use_presets(void)
{
    fp_wait_idle();
    xSemaphoreTake(s_mx, portMAX_DELAY);
    bool was = s_fp_mode;
    s_fp_mode = false;
    s_fp_ix = NULL; s_fp_base = NULL; s_fp_base_len = 0;
    s_front = -1;
    s_band_page[0] = s_band_page[1] = -1;
    xSemaphoreGive(s_mx);
    if (was) ESP_LOGW(TAG, "退回预置页");
}

bool stream_band_publish_rows(uint8_t *buf, size_t len,
                              const uint32_t row_off[FP_BAND_ROWS],
                              const uint32_t row_len[FP_BAND_ROWS])
{
    if (len > BAND_CAP) {
        ESP_LOGE(TAG, "带长度 %u 超过 BAND_CAP %u，拒绝发布（画面保持上一帧）",
                 (unsigned)len, (unsigned)BAND_CAP);
        return false;
    }
    int i = buf == s_band[0] ? 0 : 1;
    xSemaphoreTake(s_mx, portMAX_DELAY);
    memcpy(s_row_off[i], row_off, sizeof s_row_off[i]);
    memcpy(s_row_len[i], row_len, sizeof s_row_len[i]);
    s_band_len[i] = len;
    s_band_page[i] = 0;       /* 帧包模式只有一页 */
    s_front = i;
    xSemaphoreGive(s_mx);
    return true;
}

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
    /* 稳态节拍的原点。burst 结束时重置成「那一刻」，否则后面每一帧的
     * due 都比现在早，wait 恒为负，等于再也不睡——整条流会一直全速跑。 */
    int64_t  pace_t0 = t0;
    uint32_t pace_n0 = 0;
    bool ok = send_all(fd, s_hdr, s_hdr_len);
    while (ok) {
        /* 每帧只在这一个临界区里看一次共享状态，拿到的东西（模式、底帧
         * 地址、哪块带）全部快照进局部变量和 segs[]；往下的 send_all 再
         * 也不回头看。
         *
         * 这是「全程不断流」的全部机制：换帧包、换时钟带、从配对页切到
         * 真实看板，都只改共享状态，而共享状态只在帧与帧的边界被读到。
         * 所以这些切换最多影响**下一帧**的字节，不可能在一帧发到一半时
         * 改变段表、长度或 00dc 块头——那才会让电视读到错位的流并断开。
         *
         * 两个占用必须在本帧发完前一直有效：
         *   s_busy[bi]  占住带缓冲，publish 只会写另一块（stream_band_begin 等）
         *   s_fp_hold   占住底帧的 flash 映射，换帧包会等它归零（fp_wait_idle） */
        const int cur = s_fp_mode ? 0 : s_cur;
        int bi = -1;
        bool fp;
        bool held = false;
        const fp_index_t *ix;
        const uint8_t *fpbase;
        size_t fpbase_len;
        xSemaphoreTake(s_mx, portMAX_DELAY);
        if (s_front >= 0 && s_band_page[s_front] == cur) { bi = s_front; s_busy[bi]++; }
        fp = s_fp_mode; ix = s_fp_ix; fpbase = s_fp_base; fpbase_len = s_fp_base_len;
        if (fp && fpbase) { s_fp_hold++; held = true; }
        xSemaphoreGive(s_mx);

        size_t len = 0;
        fp_seg_t segs[FP_MAX_SEGS];
        int nseg = 0;
        const jb_page_t *pg = NULL;

        if (fp && ix && fpbase && bi >= 0) {
            /* 帧包模式：底帧那些段直接指向 flash 映射地址，整帧从不在 RAM
             * 里拼起来。这个编排和主机侧 bandtest 验的是同一个函数。 */
            nseg = fp_frame_segments(ix, fpbase, fpbase_len, s_band[bi],
                                     s_row_off[bi], s_row_len[bi],
                                     segs, FP_MAX_SEGS, &len);
            if (!nseg) {
                /* 索引不合法。绝不能把错位的字节发出去——电视上是花屏，
                 * 而且会一直花下去。下面会退回「只发纯底帧」：画面是带
                 * 空白卡片的看板，不好看但不是花屏，流也不断。 */
                ESP_LOGE(TAG, "帧段编排失败，本帧退回纯底帧");
            }
        }
        if (!nseg) {
            /* 预置页模式，或者帧包还没就绪：老的三段拼接。 */
            if (fp) {
                /* 帧包模式但带还没发布：发纯底帧，画面是纯色，不是花屏。 */
                if (fpbase) { segs[0].p = fpbase; segs[0].n = fpbase_len; nseg = 1; len = fpbase_len; }
            }
            if (!nseg) {
                pg = &s_pages[cur];
                size_t a = pg->row_off[s_row0], b = pg->row_off[s_row0 + s_rows];
                if (bi >= 0) {
                    segs[0].p = pg->jpg;                segs[0].n = a;
                    segs[1].p = s_band[bi];             segs[1].n = s_band_len[bi];
                    segs[2].p = pg->jpg + b;            segs[2].n = pg->len - b;
                    nseg = 3;
                    len = a + s_band_len[bi] + (pg->len - b);
                } else {
                    segs[0].p = pg->jpg; segs[0].n = pg->len;
                    nseg = 1; len = pg->len;
                }
            }
        }

        /* 这一帧凑不出任何字节（页面没解析成功、底帧还没就绪）。发一个
         * 长度 0 的 00dc 块等于告诉电视「这是一帧空画面」，它八成会判成
         * 坏流然后断开；而 break 是我们自己把流掐了。两个都比「跳过这一
         * 拍、下一拍再试」差——TCP 上什么都不发，电视只是少等半秒。 */
        if (!nseg || !len) {
            if (bi >= 0 || held) {
                xSemaphoreTake(s_mx, portMAX_DELAY);
                if (bi >= 0) s_busy[bi]--;
                if (held) s_fp_hold--;
                xSemaphoreGive(s_mx);
            }
            ESP_LOGE(TAG, "这一帧没有可发的字节，跳过本拍（不断流）");
            vTaskDelay(pdMS_TO_TICKS(1000 / s_fps));
            continue;
        }

        uint8_t ch[8];
        pcc(ch, "00dc"); p32(ch + 4, len);
        static const uint8_t pad = 0;
        ok = send_all(fd, ch, 8);
        for (int i = 0; ok && i < nseg; i++)
            ok = send_all(fd, segs[i].p, segs[i].n);
        ok = ok && (!(len & 1) || send_all(fd, &pad, 1));

        if (bi >= 0 || held) {
            xSemaphoreTake(s_mx, portMAX_DELAY);
            if (bi >= 0) s_busy[bi]--;
            if (held) s_fp_hold--;
            xSemaphoreGive(s_mx);
        }
        if (!ok) break;
        n++; s_sent++; s_last_us = esp_timer_get_time();

        if (n < BURST_FRAMES) continue;        /* burst 期间完全不睡 */
        if (n == BURST_FRAMES) {
            pace_t0 = esp_timer_get_time();
            pace_n0 = n;
            ESP_LOGW(TAG, "起播 burst 发完 %d 帧（用了 %.1f 秒），回到 %d fps 稳态",
                     BURST_FRAMES, (pace_t0 - t0) / 1e6, s_fps);
        }
        int64_t due = pace_t0 + (int64_t)(n - pace_n0) * 1000000 / s_fps;
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
    /* 这里必须按**帧包底帧**算上界，而不是按内置预置页算。
     *
     * AVI 头只在连接建立时发一次，而之后随时可能切进帧包模式——切换不
     * 重连（那正是「全程不断流」要的），所以头里的 dwSuggestedBufferSize
     * 和 dwMaxBytesPerSec 从第一秒起就得覆盖两种模式里最大的那一帧。
     * blank.jpg 只有 33KB，按它算出来是 50KB；帧包底帧约 180KB，是它的
     * 三倍多。按解码器拿这个值预分配缓冲的常见做法，报小了就是缓冲不够。
     *
     * 报大了只是让电视多分配一点，报小了是读到一半发现装不下。 */
    maxf = MAX(maxf, (uint32_t)(FP_MAX_PACK + BAND_CAP + 8));
    build_header(w, h, maxf);
    ESP_LOGI(TAG, "%d 页，%dx%d，%d fps，带 = MCU 行 %d..%d，头里报单帧上限 %u 字节",
             s_n, w, h, fps, band_row0, band_row0 + band_rows - 1, (unsigned)maxf);
    xTaskCreate(server_task, "avi_srv", 3072, NULL, 5, NULL);
}
