#pragma once
#include <stddef.h>
#include <stdint.h>

/* 照片存储用的 SHA-256。自带实现（不依赖 mbedtls），host 测试与固件共用。 */
typedef struct {
    uint32_t state[8];
    uint64_t bits;
    uint8_t  buf[64];
    size_t   fill;
} photosha_ctx;

void photosha_init(photosha_ctx *c);
void photosha_update(photosha_ctx *c, const void *data, size_t len);
void photosha_final(photosha_ctx *c, uint8_t out[32]);
void photosha(const void *data, size_t len, uint8_t out[32]);
