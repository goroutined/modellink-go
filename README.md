# modellink-go

ModelLink 官方 Go 客户端，用于获取、校验、缓存和解析版本化的中国 AI 模型与服务商数据。

数据来自 [`@modellink/data`](https://www.npmjs.com/package/@modellink/data)，运行时默认访问国内的 npmmirror。客户端不依赖 Node.js，也不会执行 npm 包中的任何脚本。

## 安装

```bash
go get github.com/goroutined/modellink-go
```

## 快速开始

```go
package main

import (
    "context"
    "fmt"
    "log"

    modellink "github.com/goroutined/modellink-go"
)

func main() {
    client, err := modellink.New(modellink.Options{})
    if err != nil {
        log.Fatal(err)
    }

    // 本地已有已校验版本时不访问网络；首次使用时下载最新版。
    snapshot, err := client.Load(context.Background())
    if err != nil {
        log.Fatal(err)
    }

    model, ok := snapshot.Offering("deepseek", "deepseek-v4-pro")
    if !ok {
        log.Fatal("model not found")
    }
    fmt.Printf("%s: context=%d\n", model.Name, model.Limit.Context)
}
```

`Snapshot` 还提供：

```go
snapshot.Model("deepseek/deepseek-v4-pro")
snapshot.Provider("deepseek")
snapshot.Offering("deepseek", "deepseek-v4-pro")
```

也可以直接访问完整的强类型目录：

```go
snapshot.Catalog.Models
snapshot.Catalog.Providers
snapshot.Manifest
snapshot.Schema()
snapshot.File(modellink.FileCatalog) // npm 包内经过哈希校验的原始字节
```

返回的 Snapshot 会在进程内共享，请将其视为只读数据。

## 检查和更新

```go
status, err := client.CheckLatest(ctx)
if err != nil {
    return err
}
if status.UpdateAvailable {
    snapshot, err = client.LoadLatest(ctx)
}
```

几个方法的网络行为是明确分开的：

| 方法 | 行为 |
| --- | --- |
| `Load` | 立即读取当前缓存；没有缓存时才下载最新版 |
| `Latest` | 只查询 Registry 中的最新版本元数据 |
| `CheckLatest` | 比较当前版本和 Registry 最新版本，不下载数据包 |
| `LoadLatest` | 查询并加载最新版，成功后原子切换当前缓存 |
| `LoadVersion` | 加载指定版本，但不改变 `Load` 使用的当前版本 |

固定版本示例：

```go
snapshot, err := client.LoadVersion(ctx, "0.1.3")
```

## 缓存与并发

默认缓存目录由 `os.UserCacheDir()` 决定：

| 系统 | 默认目录 |
| --- | --- |
| Linux | `$XDG_CACHE_HOME/modellink` 或 `~/.cache/modellink` |
| macOS | `~/Library/Caches/modellink` |
| Windows | `%LocalAppData%\\modellink` |

需要指定共享目录时，创建文件缓存并传给 Client：

```go
cache, err := modellink.NewFileCache(modellink.FileCacheOptions{
    Directory: "/var/cache/my-service/modellink",
})
if err != nil {
    return err
}

client, err := modellink.New(modellink.Options{
    Cache: cache,
})
```

文件缓存默认保留当前版本和最近一个其他版本，总计最多两个版本。只修改保留数量、
继续使用系统默认目录：

```go
cache, err := modellink.NewFileCache(modellink.FileCacheOptions{
    MaxVersions: 5,
})
```

同时修改目录和保留数量：

```go
cache, err := modellink.NewFileCache(modellink.FileCacheOptions{
    Directory:   "/var/cache/my-service/modellink",
    MaxVersions: 5,
})
```

`MaxVersions` 至少为 `2`；设置为 `-1` 可以保留全部历史版本。自动清理只在下载或切换版本时执行，
不会启动后台 goroutine；当前版本和本次操作使用的版本不会被删除。

- 多个 goroutine 同时请求同一个版本时，只下载一次，其余调用等待同一结果。
- 多个进程使用同一文件缓存时，通过带超时的系统文件锁共享下载与更新结果。
- 已有可用缓存时，`Load` 不等待正在进行的更新。
- 下载先写入独立临时目录；npm integrity、Manifest SHA-256、Schema 版本和 JSON 解析全部通过后才原子安装。
- 更新失败不会覆盖当前可用版本。
- 文件锁覆盖查询、下载、校验和激活的完整流程；进程退出后由操作系统自动释放。
- 调用方取消等待不会取消其他 goroutine 已经共享的下载任务。

### 自定义缓存

`Options` 只接收一个 `Cache`。它同时负责版本化数据存储和共享更新互斥：

```go
type Cache interface {
    CacheStore
    Locker
}
```

因此 Redis 实现通常只需要配置一次：

```go
client, err := modellink.New(modellink.Options{
    Cache: redisCache,
})
```

自定义缓存需要实现：

```go
type CacheStore interface {
    Current(ctx context.Context) (string, error)
    Get(ctx context.Context, version string) (*modellink.CacheEntry, error)
    Put(ctx context.Context, entry *modellink.CacheEntry) error
    SetCurrent(ctx context.Context, version string) error
}

type Locker interface {
    Lock(ctx context.Context, key string) (modellink.Lock, error)
}
```

`Put` 必须原子发布完整版本，不能让读取方看到部分文件；找不到数据时返回
`modellink.ErrNoCachedData`。客户端仍会重新校验 Manifest、文件哈希、Schema
版本并解析 JSON，缓存实现不能绕过数据完整性检查。

如果数据和锁位于不同系统，例如对象存储配 Redis 锁，可以组合为一个缓存：

```go
cache, err := modellink.NewCache(objectStore, redisLocker)
if err != nil {
    return err
}
client, err := modellink.New(modellink.Options{Cache: cache})
```

Redis 锁实现应使用唯一持有者 token 安全解锁，并处理锁租期和续租；不能仅使用
一个无法确认持有者的 `SETNX` 键。等待锁时必须遵守传入的 `context`。

默认 Registry：

```text
https://registry.npmmirror.com
```

可以切换到 npm 官方源或内部镜像：

```go
client, err := modellink.New(modellink.Options{
    Registry: "https://registry.npmjs.org",
})
```

## 可选字段

生成类型会保留公开 Schema 的可选性。例如：

```go
model.Temperature == nil                         // 未知
model.Temperature != nil && !*model.Temperature // 明确不支持或固定不可调
model.Temperature != nil && *model.Temperature  // 明确支持调节
```

不要把缺失字段自动当成 `false`，也不要根据上下文窗口推导缺失的最大输入或输出长度。

## Schema 与代码生成

仓库保存了生成依据：

```text
schema/schema.json
schema/schema.lock.json
types_generated.go
```

`schema.lock.json` 记录 npm 包版本、Schema 版本、SHA-256 和 ModelLink 源 commit。

更新过程是手动触发的：

```bash
# 从 npm 官方源同步最新 Schema 和锁文件
go run ./internal/cmd/schemasync

# 只根据仓库内 Schema 重新生成 Go 类型，不拉取 ModelLink 数据
go generate ./...

go test ./...
```

也可以同步指定版本或指定 Registry：

```bash
go run ./internal/cmd/schemasync -version 0.1.3
go run ./internal/cmd/schemasync -registry https://registry.npmmirror.com
```

CI 会强制检查已提交 Schema 与生成代码是否一致，但 npm 出现更新时只给出提示，不阻止 Pull Request：

```bash
go run ./internal/cmd/schemacheck
```

当前客户端支持的破坏性 Schema 版本由 `modellink.SupportedSchemaVersion` 表示。遇到更高版本时客户端会返回 `modellink.ErrUnsupportedSchema`，并继续保留旧缓存。

## 数据来源

- [ModelLink](https://github.com/goroutined/modellink)
- [数据格式与迁移指南](https://github.com/goroutined/modellink/blob/main/DATA_FORMAT.md)
- [在线目录](https://goroutined.github.io/modellink/)

## License

[MIT](./LICENSE)
