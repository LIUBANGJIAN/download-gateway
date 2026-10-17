// 管理口令：查询状态与修改。
//
// # 为什么不是「再加一个表单」那么简单
//
// 管理口令原先只能由部署时的环境变量注入，或由进程启动时随机生成；
// `config.AdminPolicy.Password` 是启动快照，运行时改不了。
// 用户要「自己设置口令」，缺的是三层：**存哪里 / 怎么校验 / 怎么立即生效**。
// 本文件负责第三层的前半段（HTTP 入口），另两层在 `internal/adminpass`：
//
//	存储 —— `app_config` 里的 `admin.password_hash`（argon2id PHC 串）+ `admin.password_updated_at`
//	校验 —— `adminpass.Credential`：有哈希走 argon2id，无哈希才与明文做定长比较
//	生效 —— 落库成功后 `SetHash()` 热替换内存凭据，**不重启即生效**
//
// # 为什么必须校验「当前口令」
//
// 只验会话是不够的：会话可能来自一台没锁屏的公用电脑，或一个被盗的浏览器。
// 要求旧口令，等于把「能操作管理台」提升到「知道口令」才允许改密。
//
// # 安全约束
//
//   - 响应**永不**包含口令或哈希（只回来源与修改时刻）；
//   - 失败路径也写审计（谁在什么时候试过改密）；
//   - 新口令不得与旧口令相同（否则「改密」等于没改，用户却以为改了）。
package admin

import (
	"errors"
	"net/http"

	"github.com/LIUBANGJIAN/download-gateway/internal/adminpass"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/session"
)

// changePasswordReq 是 `POST /api/admin/password` 的请求体。
type changePasswordReq struct {
	// CurrentPassword 当前口令（防会话被劫持后直接改密）。
	CurrentPassword string `json:"current_password"`
	// NewPassword 新口令。
	NewPassword string `json:"new_password"`
}

// passwordStatus 实现 `GET /api/admin/password`。
//
// 只回「来源 / 上次修改时刻 / 长度限制」——界面凭这些决定提示文案。
func (s *adminServer) passwordStatus(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardRead(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	source := "environment"
	if s.st.Credential != nil {
		source = s.st.Credential.Source()
	}
	updatedAt := adminpass.LoadUpdatedAt(r.Context(), s.st.Store)
	w.Header().Set("Cache-Control", "no-store")
	ok(w, map[string]any{
		"source":     source,
		"updated_at": updatedAt,
		"customized": source == "database",
		"min_length": adminpass.MinLen,
		"max_length": adminpass.MaxLen,
	})
	s.renewCookie(w, r, exp)
}

// changePassword 实现 `POST /api/admin/password`。
func (s *adminServer) changePassword(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardWrite(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	var req changePasswordReq
	if err := decodeBody(r, &req); err != nil {
		fail(w, http.StatusBadRequest, CodeValidationError, err.Error())
		return
	}
	now := ids.NowSecs()

	// ① 旧口令必须对。
	if s.st.Credential == nil || !s.st.Credential.Verify(req.CurrentPassword) {
		s.auditPassword(r, now, "修改管理口令失败：当前口令不正确", "warn")
		fail(w, http.StatusUnauthorized, CodeUnauthenticated, "当前口令不正确")
		return
	}

	// ② 新口令格式。
	if err := adminpass.ValidateNew(req.NewPassword, req.CurrentPassword); err != nil {
		msg := err.Error()
		if errors.Is(err, adminpass.ErrWeakPassword) {
			msg = trimErrPrefix(msg, adminpass.ErrWeakPassword.Error())
		}
		fail(w, http.StatusUnprocessableEntity, CodeValidationError, msg)
		return
	}

	// ③ 哈希 + 落库。
	phc, err := adminpass.Hash(req.NewPassword)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	if err := adminpass.SaveHash(r.Context(), s.st.Store, phc, now); err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}

	// ④ 立即生效。顺序很重要：**先落库再换内存**，
	// 反过来的话「换内存成功、落库失败」会让重启后口令悄悄回退到旧的。
	s.st.Credential.SetHash(phc)

	s.auditPassword(r, now, "管理口令已修改", "info")

	w.Header().Set("Cache-Control", "no-store")
	ok(w, map[string]any{
		"changed":          true,
		"source":           s.st.Credential.Source(),
		"updated_at":       now,
		"sessions_revoked": false,
	})
	s.renewCookie(w, r, exp)
}

// auditPassword 写一条口令相关审计（失败只记日志，不阻断主流程）。
func (s *adminServer) auditPassword(r *http.Request, now int64, message, level string) {
	if _, err := s.st.Store.Execute(r.Context(),
		"INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at) "+
			"VALUES (?1, 'security', NULL, NULL, ?2, NULL, ?3)",
		level, message, now,
	); err != nil {
		_ = err // 审计失败不阻断；此处无 logger 依赖，保持与 logSecurity 一致的处理方式
	}
}

// trimErrPrefix 去掉 `ValidateNew` 错误里重复的哨兵前缀
// （错误串形如「新口令不符合要求：至少 10 个字符」，直接回给界面读起来啰嗦）。
func trimErrPrefix(msg, prefix string) string {
	for _, sep := range []string{prefix + "：", prefix + ": "} {
		if len(msg) > len(sep) && msg[:len(sep)] == sep {
			return msg[len(sep):]
		}
	}
	return msg
}
