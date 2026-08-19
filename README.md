# ialbum

ialbum 是一个自托管的私人相册管理程序。它以主存储为唯一事实源，将本地路径、WebDAV 或百度网盘中的照片和视频建立为可搜索的相册索引，并可异步复制到备份存储。

项目采用 Go 1.24、Vue 3/TypeScript 和 SQLite。Vue 生产资源会嵌入 Go 二进制，部署时只需一个进程和一个可写数据目录。

## 功能

- 本地路径、WebDAV、百度网盘设备码授权与分页文件浏览器
- 相册、标签、封面、递归扫描和拖放上传
- JPEG、PNG、WebP、GIF、MP4、MOV/M4V、WebM、MKV、AVI
- 图片缩略图、可选 FFmpeg 视频封面、Range 播放
- 单文件下载、所选媒体或整册流式 ZIP 下载
- 安全备份和带双扫描确认/24 小时宽限的镜像备份
- 持久任务队列、失败重试、授权暂停和崩溃租约回收
- 单管理员初始化、Argon2id 密码、CSRF 防护和加密存储凭据

## 快速开始

需要 Go 1.24+、Node.js 20+ 和 npm。

```bash
npm --prefix web ci
make build
IALBUM_DATA_DIR="$PWD/data" ./bin/ialbum
```

首次启动会在控制台打印一个 15 分钟有效的 `setupToken`。打开 <http://127.0.0.1:8080>，输入令牌并创建管理员账户。密码至少 12 个字符。

开发时分别运行：

```bash
make dev-api
make dev-web
```

Vite 地址为 <http://127.0.0.1:5173>，`/api` 会代理到 Go 服务。`make verify` 会执行 Go 测试和 race 检查、Vue 测试、类型检查及生产构建。

`make release` 会生成 Linux、macOS 和 Windows 的 amd64/arm64 纯 Go 二进制。

## 配置

默认仅监听 `127.0.0.1:8080`，数据写入系统用户配置目录中的 `ialbum`。常用环境变量与反向代理安全要求见 [配置文档](docs/configuration.md)，内部边界和同步语义见 [架构文档](docs/architecture.md)。产品需求原稿保留在 [docs/prd.md](docs/prd.md)。

## 百度网盘

每个连接使用用户自己的 AppKey/SecretKey，ialbum 不内置公共凭据。真实可用性取决于百度开放平台授予的网盘文件权限；当前自动测试使用模拟服务，不访问真实账号。

## 边界

首版是单管理员实例，不包含公开分享、人物识别、HEIC/RAW/AVIF、视频转码、主存储迁移或媒体删除 API。将相册移出 ialbum 只删除索引；只有管理员明确启用镜像模式后，ialbum 自己记录的备份副本才可能经过安全状态机删除。

Licensed under Apache-2.0.
