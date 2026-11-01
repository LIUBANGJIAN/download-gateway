package dispatch

import (
	"context"
	"sync"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
)

// DefaultSaveFolderTTL 是节点"可用下载目录"的缓存时长。
//
// 目录确实会变（用户在节点上改下载目录、换挂载点），但不是高频事件；
// 5 分钟既能摊掉「每个任务都多一次往返」的开销，又不会让改动长期不生效。
const DefaultSaveFolderTTL = 5 * time.Minute

// SaveFolderCache 缓存每个节点的默认下载目录。
//
// # 为什么必须有这层缓存
//
// BitComet 2.22+ 的 `http/add` / `torrent_links/add` **要求显式 save_folder**，
// 不传就 `FATAL_ERROR: save_folder missing`（真机上实测踩到）。而可用目录只能
// 由节点自己通过 `new_task/get` 报出来，跨平台无法猜。
//
// 代价是每次投递多一次往返。tick 每秒最多 20 个任务，不缓存会让投递延迟翻倍，
// 所以在节点维度缓存 —— 同一节点上的所有任务共用一次查询结果。
type SaveFolderCache struct {
	mu  sync.Mutex
	m   map[int64]saveFolderEntry
	ttl time.Duration
	now func() time.Time
}

type saveFolderEntry struct {
	path string // 空串 = 节点明确表示没有可用目录（旧版 BitComet）
	ok   bool   // 是否查到了可用目录
	at   time.Time
}

// NewSaveFolderCache 构造缓存；ttl <= 0 时用 [DefaultSaveFolderTTL]。
func NewSaveFolderCache(ttl time.Duration) *SaveFolderCache {
	if ttl <= 0 {
		ttl = DefaultSaveFolderTTL
	}
	return &SaveFolderCache{m: make(map[int64]saveFolderEntry), ttl: ttl, now: time.Now}
}

// get 读缓存。第二个返回值是"是否命中缓存"，第三个是缓存值本身。
func (c *SaveFolderCache) get(nodeID int64) (saveFolderEntry, bool) {
	if c == nil {
		return saveFolderEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, hit := c.m[nodeID]
	if !hit || c.now().Sub(e.at) > c.ttl {
		return saveFolderEntry{}, false
	}
	return e, true
}

// put 写缓存。
func (c *SaveFolderCache) put(nodeID int64, path string, ok bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[nodeID] = saveFolderEntry{path: path, ok: ok, at: c.now()}
}

// invalidate 丢弃某个节点的缓存（投递因 save_folder 失败时用，让下次重新问）。
func (c *SaveFolderCache) invalidate(nodeID int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, nodeID)
}

// resolveSaveFolder 取节点的默认下载目录，优先用缓存。
//
// 查询失败**不**返回错误：调用方应退回"不传 save_folder"的旧行为，让节点
// 自己报错（旧版 BitComet 本来就不需要这个参数，不能因为查询失败就把
// 老节点判死）。
func resolveSaveFolder(ctx context.Context, d *Deps, nodeID int64, cl *protocol.Client, taskType string) (string, bool) {
	cache := d.SaveFolders
	if e, hit := cache.get(nodeID); hit {
		return e.path, e.ok
	}
	path, ok, err := cl.DefaultSaveFolder(ctx, taskType)
	if err != nil {
		// 不回填缓存：下一次投递应当重试，而不是把这个"问不到"锁 5 分钟。
		return "", false
	}
	cache.put(nodeID, path, ok)
	return path, ok
}
