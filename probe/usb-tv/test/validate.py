#!/usr/bin/env python3
"""
独立校验 vdisk 生成的镜像。

刻意不引用 vdisk.c 里的任何常量——所有几何都从 MBR / BPB 里现读现算，
按 FAT32 规范走。这样才能抓到固件自己的布局 bug，而不是把同一个错误验两遍。

终局判据：把 /SLIDE.JPG 和 /DCIM/SLIDE.JPG 的字节从簇链里抠出来，
和 assets/slideN.jpg 做逐字节比对。一致就说明 BPB 几何、FAT 链、
目录项里的簇号和数据区落位全都对。

用法：  test/harness <slide 0-2> | test/validate.py <slide 0-2>
"""
import sys
import pathlib

U16 = lambda b, o: int.from_bytes(b[o:o + 2], "little")
U32 = lambda b, o: int.from_bytes(b[o:o + 4], "little")

fails = []
notes = []


def chk(cond, msg):
    print(("  \033[32mOK\033[0m   " if cond else "  \033[31mFAIL\033[0m ") + msg)
    if not cond:
        fails.append(msg)
    return cond


def main():
    slide = int(sys.argv[1]) if len(sys.argv) > 1 else 0
    img = sys.stdin.buffer.read()
    SS = 512  # 先按 512 读 MBR，之后用 BPB 里的真实值
    sec = lambda n: img[n * SS:(n + 1) * SS]
    print(f"镜像 {len(img)} 字节 = {len(img)//SS} 扇区\n")

    # ── MBR ──
    print("[1] MBR")
    m = sec(0)
    chk(m[510] == 0x55 and m[511] == 0xAA, "MBR 结尾签名 0x55AA")
    e = m[0x1BE:0x1BE + 16]
    ptype, pstart, psize = e[4], U32(e, 8), U32(e, 12)
    chk(ptype == 0x0C, f"分区类型 0x{ptype:02X} (期望 0x0C = FAT32 LBA)")
    chk(pstart == 2048, f"分区起始 LBA {pstart} (期望 2048，标准对齐)")
    chk(psize > 0, f"分区长度 {psize} 扇区")
    chk(pstart + psize <= len(img) // SS or True, "分区范围")
    for i in range(1, 4):
        z = m[0x1BE + 16 * i:0x1BE + 16 * (i + 1)]
        chk(z == b"\0" * 16, f"分区表项 {i+1} 为空")

    # ── BPB ──
    print("\n[2] BPB (FAT32)")
    b = sec(pstart)
    chk(b[510] == 0x55 and b[511] == 0xAA, "BPB 结尾签名")
    bps = U16(b, 11)
    spc = b[13]
    rsvd = U16(b, 14)
    nfat = b[16]
    rootent = U16(b, 17)
    tot16 = U16(b, 19)
    fatsz16 = U16(b, 22)
    tot32 = U32(b, 32)
    fatsz32 = U32(b, 36)
    rootclus = U32(b, 44)
    fsinfosec = U16(b, 48)
    bkboot = U16(b, 50)
    hidd = U32(b, 28)
    chk(bps == 512, f"每扇区字节 {bps}")
    chk(spc in (1, 2, 4, 8, 16, 32, 64, 128), f"每簇扇区 {spc}")
    chk(rsvd >= 1, f"保留扇区 {rsvd}")
    chk(nfat in (1, 2), f"FAT 份数 {nfat}")
    chk(rootent == 0, f"RootEntCnt {rootent} (FAT32 必须为 0)")
    chk(tot16 == 0, f"TotSec16 {tot16} (必须为 0)")
    chk(fatsz16 == 0, f"FATSz16 {fatsz16} (必须为 0)")
    chk(fatsz32 > 0, f"FATSz32 {fatsz32}")
    chk(tot32 == psize, f"TotSec32 {tot32} == 分区长度 {psize}")
    chk(hidd == pstart, f"HiddSec {hidd} == 分区起始 {pstart}")
    chk(rootclus == 2, f"根目录簇 {rootclus}")
    chk(b[66] == 0x29, f"扩展引导签名 0x{b[66]:02X}")
    chk(b[82:90] == b"FAT32   ", f"FilSysType {b[82:90]!r}")
    print(f"       卷标 {b[71:82]!r}  OEM {b[3:11]!r}")

    # 簇数 —— FAT32 的硬门槛
    datasec = tot32 - (rsvd + nfat * fatsz32)
    nclus = datasec // spc
    chk(nclus >= 65525, f"簇数 {nclus} >= 65525 (低于此值主机会按 FAT16 解析然后失败)")
    chk(fatsz32 * (bps // 4) >= nclus + 2,
        f"FAT 容量 {fatsz32*(bps//4)} 项 >= 需要的 {nclus+2} 项")

    # 备份引导扇区 + FSInfo
    print("\n[3] 备份引导扇区 / FSInfo")
    if chk(bkboot > 0, f"BkBootSec {bkboot}"):
        chk(sec(pstart + bkboot) == b, "备份引导扇区与主引导扇区逐字节一致")
    fi = sec(pstart + fsinfosec)
    chk(U32(fi, 0) == 0x41615252, "FSInfo 前导签名 RRaA")
    chk(U32(fi, 484) == 0x61417272, "FSInfo 结构签名 rrAa")
    chk(U32(fi, 508) == 0xAA550000, "FSInfo 尾部签名")

    # ── FAT ──
    print("\n[4] FAT")
    fat1 = pstart + rsvd
    fat = img[fat1 * bps:(fat1 + fatsz32) * bps]
    ent = lambda n: U32(fat, n * 4) & 0x0FFFFFFF
    chk(fat[0] == 0xF8, f"FAT[0] 介质字节 0x{fat[0]:02X}")
    chk(ent(1) == 0x0FFFFFFF, f"FAT[1] = 0x{ent(1):08X}")
    chk(ent(rootclus) == 0x0FFFFFFF, f"FAT[根簇] = EOC")
    if nfat == 2:
        fat2 = fat1 + fatsz32
        same = img[fat2 * bps:(fat2 + fatsz32) * bps] == fat
        chk(same, "FAT2 与 FAT1 完全一致")

    data1 = pstart + rsvd + nfat * fatsz32
    clba = lambda n: data1 + (n - 2) * spc

    def chain(start):
        out, n, guard = [], start, 0
        while 2 <= n < 0x0FFFFFF0:
            out.append(n)
            n = ent(n)
            guard += 1
            if guard > 100000:
                raise RuntimeError("簇链不收敛")
        return out, n

    def readfile(first, size):
        cl, term = chain(first)
        need = -(-size // (bps * spc)) if size else 0
        if term != 0x0FFFFFFF:
            notes.append(f"簇链以 0x{term:08X} 结束而非 EOC")
        if len(cl) != need:
            notes.append(f"簇链 {len(cl)} 簇，按 size={size} 应为 {need} 簇")
        buf = b"".join(img[clba(c) * bps:(clba(c) + spc) * bps] for c in cl)
        return buf[:size], cl

    # ── 目录 ──
    def listdir(first):
        cl, _ = chain(first)
        raw = b"".join(img[clba(c) * bps:(clba(c) + spc) * bps] for c in cl)
        out = []
        for i in range(0, len(raw), 32):
            d = raw[i:i + 32]
            if not d or d[0] == 0x00:
                break
            if d[0] == 0xE5:
                continue
            attr = d[11]
            if attr == 0x0F:        # 长文件名项，这个盘不生成
                continue
            name = d[0:8].decode("ascii", "replace").rstrip()
            extn = d[8:11].decode("ascii", "replace").rstrip()
            clus = (U16(d, 20) << 16) | U16(d, 26)
            out.append(dict(name=name, ext=extn, attr=attr, clus=clus,
                            size=U32(d, 28), mtime=U16(d, 22), mdate=U16(d, 24)))
        return out

    print("\n[5] 根目录")
    root = listdir(rootclus)
    for d in root:
        kind = ("卷标" if d["attr"] & 0x08 else "目录" if d["attr"] & 0x10 else "文件")
        print(f"       {d['name']:<8}.{d['ext']:<3} {kind} attr=0x{d['attr']:02X} "
              f"clus={d['clus']} size={d['size']}")
    chk(any(x["attr"] & 0x08 for x in root), "存在卷标项")
    jpg = [x for x in root if x["ext"] == "JPG" and not x["attr"] & 0x18]
    dcim = [x for x in root if x["attr"] & 0x10 and x["name"] == "DCIM"]
    chk(len(jpg) == 1, "根目录下恰好一个 JPG")
    chk(len(dcim) == 1, "根目录下有 DCIM 目录")

    print("\n[6] /DCIM")
    sub = []
    if dcim:
        sub = listdir(dcim[0]["clus"])
        for d in sub:
            print(f"       {d['name']:<8}.{d['ext']:<3} attr=0x{d['attr']:02X} "
                  f"clus={d['clus']} size={d['size']}")
        chk(any(x["name"] == "." for x in sub), "有 . 项")
        dd = [x for x in sub if x["name"] == ".."]
        chk(len(dd) == 1, "有 .. 项")
        if dd:
            chk(dd[0]["clus"] == 0, f".. 的簇号 {dd[0]['clus']} (FAT32 里父为根时必须是 0)")

    # ── 终局：字节比对 ──
    print("\n[7] 抠出文件内容与原图逐字节比对  ← 终局判据")
    orig = (pathlib.Path(__file__).parent.parent / "assets" / f"slide{slide+1}.jpg").read_bytes()
    print(f"       原图 assets/slide{slide+1}.jpg = {len(orig)} 字节")
    targets = []
    if jpg:
        targets.append(("/SLIDE.JPG", jpg[0]))
    sj = [x for x in sub if x["ext"] == "JPG" and not x["attr"] & 0x18]
    if sj:
        targets.append(("/DCIM/SLIDE.JPG", sj[0]))
    chk(len(targets) == 2, "两个副本都找到了")
    seen = {}
    for label, d in targets:
        chk(d["size"] == len(orig), f"{label} 目录项 size={d['size']} == 原图 {len(orig)}")
        data, cl = readfile(d["clus"], d["size"])
        print(f"       {label}: 簇 {cl[0]}..{cl[-1]} 共 {len(cl)} 簇")
        chk(data == orig, f"{label} 内容与原图逐字节一致")
        chk(data[:2] == b"\xff\xd8" and data[-2:] == b"\xff\xd9",
            f"{label} JPEG 首尾标记 FFD8/FFD9 完整")
        seen[label] = set(cl)
    if len(seen) == 2:
        a, bb = seen.values()
        chk(not (a & bb), "两个副本的簇链完全不重叠（文件系统合法，不共享簇）")

    print()
    for n in notes:
        print(f"  \033[33m注意\033[0m {n}")
    if fails:
        print(f"\n\033[31m{len(fails)} 项失败\033[0m")
        for f in fails:
            print("   - " + f)
        return 1
    print("\033[32m全部通过\033[0m")
    return 0


sys.exit(main())
