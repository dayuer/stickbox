# 渲染用字体

全部来自 Google Fonts，SIL Open Font License 1.1，许可原文见同目录 `OFL-*.txt`。

| 文件 | 原字体 | 用途 |
|---|---|---|
| `BarlowCondensed-SemiBold.ttf` | Barlow Condensed 600 | 数字、小标 |
| `Barlow-Bold.ttf` | Barlow 700 | 翻页时钟 |
| `NotoSansSC-Medium.ttf` | Noto Sans SC 500 | 中文正文 |
| `NotoSansSC-Bold.ttf` | Noto Sans SC 700 | 结论句、预测 |

## 子集

为了控制仓库体积，字体已做子集化：

- 两个 Barlow 只保留 `U+0020-007E`；
- Noto Sans SC 从可变字体实例化出 500 / 700 两个字重，只保留设计稿与
  状态文案用到的汉字（见下）。

重新生成（需要 `fonttools`）：

```bash
fonttools varLib.instancer NotoSansSC[wght].ttf wght=500 -o Medium-full.ttf
fonttools varLib.instancer NotoSansSC[wght].ttf wght=700 -o Bold-full.ttf
pyftsubset Medium-full.ttf --text-file=chars.txt --unicodes='U+0020-007E,U+00B7' \
  --layout-features='' --no-hinting --desubroutinize --output-file=NotoSansSC-Medium.ttf
pyftsubset Bold-full.ttf --text-file=chars.txt --unicodes='U+0020-007E,U+00B7' \
  --layout-features='' --no-hinting --desubroutinize --output-file=NotoSansSC-Bold.ttf
pyftsubset BarlowCondensed-SemiBold.ttf --unicodes='U+0020-007E' \
  --layout-features='' --no-hinting --output-file=BarlowCondensed-SemiBold.ttf
pyftsubset Barlow-Bold.ttf --unicodes='U+0020-007E' \
  --layout-features='' --no-hinting --output-file=Barlow-Bold.ttf
```

`chars.txt` 当前内容（新增文案时同步加字，否则会画成空白）：

```
用量月日周一二三四五六两个窗口都够等待电脑上报数据小时额度已满快完了偏天时间进度节奏建议上限慢剩分秒约用完重置新每钟预测按当前平均速度尚未收到怎么读彩色弧白色刻度越过得比更
```
