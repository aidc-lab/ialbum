# 配置与运维

ialbum 通过环境变量配置。除特别说明外，配置在启动时读取。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `IALBUM_DATA_DIR` | 系统用户配置目录下的 `ialbum` | SQLite、主密钥、缓存和临时文件目录 |
| `IALBUM_LISTEN` | `127.0.0.1:8080` | HTTP 监听地址 |
| `IALBUM_PUBLIC_URL` | 空 | 反向代理后的 HTTPS 外部地址，也决定 Cookie 的 Secure 标志 |
| `IALBUM_ALLOW_INSECURE_LAN` | `false` | 明确允许非回环 HTTP，仅适用于可信局域网 |
| `IALBUM_TRUSTED_PROXIES` | 空 | 可信反向代理地址列表（为未来代理头支持保留） |
| `IALBUM_MASTER_KEY` | 自动生成文件 | Base64 编码的 32 字节密钥 |
| `IALBUM_MAX_UPLOAD_BYTES` | 20 GiB | 单文件上传上限 |
| `IALBUM_CACHE_MAX_BYTES` | 5 GiB | 派生缩略图缓存上限 |
| `IALBUM_MAX_VIDEO_STAGING_BYTES` | 2 GiB | 远程视频封面暂存上限 |
| `IALBUM_FFMPEG` / `IALBUM_FFPROBE` | 从 PATH 查找 | 可选可执行文件的绝对路径 |

## 反向代理

默认监听仅允许本机访问。若监听非回环地址，必须满足以下之一：

1. 设置 HTTPS `IALBUM_PUBLIC_URL`，由可信反向代理终止 TLS；
2. 对完全可信的家庭局域网显式设置 `IALBUM_ALLOW_INSECURE_LAN=true`。

生产环境应使用第一种方式。ialbum 不启用 CORS，浏览器页面和 API 必须同源。

## 备份数据

应备份整个数据目录，尤其是 `ialbum.db` 和 `master.key`。数据库有密文但主密钥丢失时，存储凭据无法恢复。缓存和 `tmp` 可以在服务停止后删除并重新生成。

SQLite 运行时可能存在 `ialbum.db-wal` 和 `ialbum.db-shm`。在线备份应使用 SQLite 备份机制或确保数据库、WAL 和 SHM 一致；最简单的可靠方式是正常停止 ialbum 后复制数据目录。

## 管理员密码重置

在 ialbum 停止时执行：

```bash
./ialbum admin reset-password --data-dir /path/to/data --username admin --password 'new-password-at-least-12'
```

也可通过 `IALBUM_NEW_PASSWORD` 提供新密码，避免密码出现在 shell 历史中。重置后所有现有会话会被撤销。
