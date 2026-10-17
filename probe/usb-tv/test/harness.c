/* 把 vdisk.c 编到主机上，导出镜像前若干扇区，交给 test/validate.py 独立校验。 */
#include <stdio.h>
#include <stdlib.h>
#include "vdisk.h"
int main(int argc, char **argv)
{
    int slide = argc > 1 ? atoi(argv[1]) : 0;
    unsigned nsec = argc > 2 ? strtoul(argv[2], 0, 0) : 8300;
    vd_slide_arm(slide);
    vd_slide_commit();
    unsigned char b[512];
    for (unsigned l = 0; l < nsec; l++) { vd_read(l, b, 1); fwrite(b, 1, 512, stdout); }
    fprintf(stderr, "slide%d: %u 字节, 导出 %u 扇区\n", slide + 1, (unsigned)vd_slide_size(), nsec);
    return 0;
}
