# 第三方资源

## esp-web-tools 10.4.0

Apache-2.0，见 `esp-web-tools-10.4.0/LICENSE`。

烧录页能把固件写进用户的板子，所以运行时不从 CDN 加载脚本。更新用：

```sh
web/vendor-update.sh 10.4.0
```

脚本会从 npm registry 拉 tarball，先对照 registry 公布的 sha512 integrity
校验，再解包 `dist/web/`（`install-button.js` 及它动态 import 的全部 chunk）。
不要手改这个目录。
