package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"dbbak/internal/backup"
	"dbbak/internal/metadata"
)

// 删除接口统一返回服务端重新评估后的范围：先 DELETE 预览（force=false）看阻塞项，
// 再把同一参数带上 ?force=1 与 X-DBBak-Confirm 执行。浏览器与外部脚本都走同一套校验。

// planJobRemoval 返回任务删除范围（任务配置 + 其下全部备份文件）。
func (s *Server) planJobRemoval(w http.ResponseWriter, r *http.Request) {
	s.planRemoval(w, r, backup.RemovalTargetJob, r.PathValue("id"))
}

// planJobFilesRemoval 只评估任务下的备份文件，保留任务配置。
func (s *Server) planJobFilesRemoval(w http.ResponseWriter, r *http.Request) {
	s.planRemoval(w, r, backup.RemovalTargetJobFiles, r.PathValue("id"))
}

func (s *Server) planArtifactRemoval(w http.ResponseWriter, r *http.Request) {
	s.planRemoval(w, r, backup.RemovalTargetArtifact, r.PathValue("id"))
}

func (s *Server) planRemoval(w http.ResponseWriter, r *http.Request, target, id string) {
	plan, err := s.removal.Plan(r.Context(), target, id, r.URL.Query().Get("force") == "1")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// deleteJob 删除任务配置及其备份文件。任务必须已暂停，备份文件逐个删除成功后才移除记录。
func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	s.executeRemoval(w, r, backup.RemovalTargetJob, r.PathValue("id"), "job.delete")
}

// deleteJobFiles 只删除任务产生的备份文件，任务保留为「无备份」状态。
func (s *Server) deleteJobFiles(w http.ResponseWriter, r *http.Request) {
	s.executeRemoval(w, r, backup.RemovalTargetJobFiles, r.PathValue("id"), "job.files.delete")
}

// deleteArtifact 删除单个备份对象。
func (s *Server) deleteArtifact(w http.ResponseWriter, r *http.Request) {
	s.executeRemoval(w, r, backup.RemovalTargetArtifact, r.PathValue("id"), "artifact.delete")
}

// removalConfirm 读取强制删除的确认名称。
// 浏览器 fetch 的请求头值只能是 ISO-8859-1，中文任务名或中文路径放不进去，
// 会直接抛 “String contains non ISO-8859-1 code point”，因此优先取查询参数 confirm；
// header X-DBBak-Confirm 保留给外部脚本，仅在值含百分号转义时尝试解码。
func removalConfirm(r *http.Request) string {
	if value := strings.TrimSpace(r.URL.Query().Get("confirm")); value != "" {
		return value
	}
	raw := strings.TrimSpace(r.Header.Get("X-DBBak-Confirm"))
	if strings.Contains(raw, "%") {
		if decoded, err := url.QueryUnescape(raw); err == nil {
			return decoded
		}
	}
	return raw
}

func (s *Server) executeRemoval(w http.ResponseWriter, r *http.Request, target, id, action string) {
	force := r.URL.Query().Get("force") == "1"
	confirm := removalConfirm(r)
	result, err := s.removal.Execute(r.Context(), target, id, confirm, force, actor(r.Context()))
	if err != nil {
		switch {
		case errors.Is(err, backup.ErrRemovalBlocked):
			s.fail(w, r, http.StatusConflict, "REMOVAL_BLOCKED", err.Error())
		case errors.Is(err, backup.ErrConfirmationRequired):
			s.fail(w, r, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", err.Error())
		case errors.Is(err, metadata.ErrNotFound):
			s.internalError(w, r, err)
		default:
			s.audit(r, action, id, "failed")
			s.internalError(w, r, err)
		}
		return
	}
	if result.FailedFiles > 0 {
		// 部分失败必须如实返回：文件保留、记录不改，且不能报“成功”。
		s.audit(r, action, id, "partial_failure")
		writeJSON(w, http.StatusConflict, map[string]any{
			"code": "PARTIAL_FAILURE", "message": result.Message, "field_errors": map[string]string{}, "retryable": true,
			"request_id": r.Header.Get("X-Request-ID"), "result": result,
		})
		return
	}
	s.audit(r, action, id, "success")
	writeJSON(w, http.StatusOK, result)
}
