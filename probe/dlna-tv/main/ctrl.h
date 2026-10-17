#pragma once
#include "dlna.h"

/* 控制面板（:80）。tv / pushed 由 main 维护，这里只读 */
void ctrl_start(const dlna_tv_t *tv, const volatile bool *found, volatile bool *repush_req);
