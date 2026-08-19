# ialbum 架构

## 进程与目录

`cmd/ialbum` 组装配置、SQLite、认证、存储注册表、媒体服务、任务队列和 HTTP API。`internal` 包不依赖 Vue；`web` 在开发模式提供说明页，在 `production` build tag 下嵌入 Vite 的 `dist`。生产服务器同源提供 `/api/v1` 与 SPA，只有非 API 路径才回退到 `index.html`。

数据库保存元数据和任务，不保存原始媒体。原图和视频始终在 provider 中，缩略图是数据目录内可重建的缓存。SQLite 开启 foreign keys、WAL 和 busy timeout，所有远程 I/O 均在事务之外完成。

## 存储边界

相册领域只依赖 `storage.Provider`，其路径是 `/` 分隔的相对逻辑路径。各驱动在入口拒绝绝对路径、NUL 和 `..`。Local 通过 Go 受限根目录 API 操作文件且扫描时忽略符号链接；WebDAV 校验 TLS 并限制跨主机重定向；百度 dlink 与 token 只在服务端使用。

存储管理页可通过 provider 的分页 `List` 浏览目录。文件预览和下载仍经过登录保护的同源代理，并在驱动支持时转发 Range；前端不会获得本地绝对映射、WebDAV 凭据、百度 token 或 dlink。

百度驱动按官方 SDK 的同一套 REST 契约实现设备码、目录分页、Meta+dlink、三段分片上传和文件管理。实现时官方 SDK v0.1.0 的 `go.mod` 已要求 Go 1.26.2，与本项目固定的 Go 1.24 基线冲突，因此 provider 内暂时直接调用官方端点；边界已隔离，官方 SDK 发布兼容 Go 1.24 的版本后可在不改领域层的情况下替换。

同一存储中的相册根目录不能相同或互为祖先。一个相册有且仅有一个主绑定，并最多有一个备份绑定。主目录可以导入已有媒体；备份目录必须为空或含匹配相册 ID 的 `.ialbum-album.json` 标记。

## 索引与备份

SQLite 索引是页面数据源，主存储是唯一事实源。完整扫描成功后才更新相册的成功状态；文件修改会产生新的 `source_version`，使缩略图失效并创建幂等备份任务。

安全模式从不传播源端删除。镜像模式只删除 `media_replicas` 中由 ialbum 记录的对象：必须连续两次完整扫描确认缺失，等待至少 24 小时，并在执行前重新验证主存储健康且对象仍不存在。管理员取消后保持抑制状态，直到对象重新出现或显式恢复。

上传先受限落盘，再以 provider 原子提交；主存储成功并写入索引即向客户端成功，备份在持久队列中完成。队列通过去重键、租约、心跳、指数退避和最大尝试次数保证可恢复执行。

## 安全模型

实例只有一个管理员。首次启动的临时 setup token 只输出到控制台；创建管理员后永久关闭。密码采用 Argon2id，session cookie 为不透明随机值且数据库仅存哈希。所有写 API 校验会话级 CSRF token 以及 Origin/Referer。

WebDAV 密码、百度应用密钥及 token 使用 AES-256-GCM 加密。主密钥来自 `IALBUM_MASTER_KEY`，否则创建权限为 0600 的 `master.key`。密钥损坏时拒绝启动，不会覆盖原密钥。

## 媒体与下载

图片生成 320px 网格图和 1280px 预览图。FFmpeg/ffprobe 是可选依赖，只生成视频封面及探测元数据，不做转码。单文件通过后端代理并在 provider 支持时处理 Range。

批量下载 ticket 绑定创建它的 session，五分钟有效且只可消费一次。ZIP 使用 Store 与 Zip64 流式写入响应，不缓存整册；个别条目失败时保留成功条目，并附加 `ialbum-download-errors.txt`。
